// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func deployedIsolationProfile() string {
	if os.Getenv("KOGNITION_ADMISSION_CNI") == "cilium" {
		return "cilium"
	}
	return "network-policy"
}

type deployedCiliumProfile struct {
	image            string
	agentDigest      string
	operatorFixtures bool
	remoteDedicated  bool
	settings         map[string]string
}

// deployedTargetEvidence is everything the reviewed-target gate needs to decide whether an endpoint may be tested.
type deployedTargetEvidence struct {
	Profile             string
	Host                string
	InsecureTLS         bool
	RemoteEndpoint      string
	DedicatedClusterUID string
	ClusterUID          string
	Namespaces          []string
	Permitted           []string
}

// permittedDeployedNamespaces joins the reviewed fixed namespaces with operator-named extras, ignoring blanks.
func permittedDeployedNamespaces(fixed []string, extras string) []string {
	var permitted []string
	for _, name := range fixed {
		if name != "" {
			permitted = append(permitted, name)
		}
	}
	for _, name := range strings.Split(extras, ",") {
		if name = strings.TrimSpace(name); name != "" {
			permitted = append(permitted, name)
		}
	}
	return permitted
}

// validateDeployedTarget keeps loopback targets as before and admits a remote API endpoint only for a profile that
// declares remote qualification, when the operator pinned that exact endpoint, attested the exact cluster UID as
// dedicated, and the cluster holds no namespace outside the reviewed installation.
func validateDeployedTarget(evidence deployedTargetEvidence) error {
	endpoint, err := url.Parse(evidence.Host)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" {
		return fmt.Errorf("deployed acceptance requires an https API endpoint")
	}
	if endpoint.Hostname() == "127.0.0.1" {
		return nil
	}
	profile, err := resolveDeployedCiliumProfile(evidence.Profile, true, true, true)
	if err != nil {
		return err
	}
	if !profile.remoteDedicated {
		return fmt.Errorf("profile %q qualifies loopback targets only", evidence.Profile)
	}
	if evidence.InsecureTLS {
		return fmt.Errorf("remote qualification forbids insecure TLS")
	}
	if evidence.RemoteEndpoint == "" || evidence.RemoteEndpoint != evidence.Host {
		return fmt.Errorf("remote endpoint %q must equal the endpoint pinned in the reviewed plan", evidence.Host)
	}
	if evidence.ClusterUID == "" || evidence.DedicatedClusterUID != evidence.ClusterUID {
		return fmt.Errorf("dedicated-cluster attestation must name the exact observed cluster UID")
	}
	if len(evidence.Namespaces) == 0 {
		return fmt.Errorf("remote qualification requires the observed namespace inventory")
	}
	for _, namespace := range evidence.Namespaces {
		if namespace == "default" || strings.HasPrefix(namespace, "kube-") || slices.Contains(evidence.Permitted, namespace) {
			continue
		}
		return fmt.Errorf("dedicated cluster holds foreign namespace %q", namespace)
	}
	return nil
}

var tunnelCiliumSettings = map[string]string{
	"enable-policy": "default", "enable-k8s-networkpolicy": "true", "enable-ipv4": "true", "enable-ipv6": "false",
	"ipam": "kubernetes", "kube-proxy-replacement": "false", "routing-mode": "tunnel", "tunnel-protocol": "vxlan",
}

// AKS-managed Azure CNI powered by Cilium: delegated IPAM, native routing and kube-proxy replacement.
var aksCiliumSettings = map[string]string{
	"enable-policy": "default", "enable-k8s-networkpolicy": "true", "enable-ipv4": "true", "enable-ipv6": "false",
	"ipam": "delegated-plugin", "kube-proxy-replacement": "true", "routing-mode": "native", "datapath-mode": "veth",
	"enable-endpoint-routes": "true", "identity-allocation-mode": "crd",
}

func resolveDeployedCiliumProfile(profile string, reviewed, isolated, frozen bool) (deployedCiliumProfile, error) {
	switch profile {
	case "", "local-cilium-1.17.8":
		return deployedCiliumProfile{image: "quay.io/cilium/cilium:v1.17.8@sha256:6d7ea72ed311eeca4c75a1f17617a3d596fb6038d30d00799090679f82a01636", settings: tunnelCiliumSettings}, nil
	case "reviewed-cilium-1.20.2":
		if !reviewed || !isolated || !frozen {
			return deployedCiliumProfile{}, fmt.Errorf("current Cilium qualification requires an explicit reviewed target, isolated cluster and frozen writers")
		}
		return deployedCiliumProfile{
			image:            "quay.io/cilium/cilium:v1.20.2@sha256:2939231d0d3e3ebddcd80fffa168b7ddcc78fdf0dc864d1c8c126ff523c54f01",
			operatorFixtures: true,
			settings:         tunnelCiliumSettings,
		}, nil
	case "reviewed-aks-cilium-1.18.15":
		if !reviewed || !isolated || !frozen {
			return deployedCiliumProfile{}, fmt.Errorf("AKS Cilium qualification requires an explicit reviewed target, isolated cluster and frozen writers")
		}
		return deployedCiliumProfile{
			image:            "mcr.microsoft.com/containernetworking/cilium/cilium-distroless:v1.18.15-261022",
			agentDigest:      "sha256:c7fd124f9b0c01ac4ae2fec56047e22e2860fc112535dc52aac7cfa2878575db",
			operatorFixtures: true,
			remoteDedicated:  true,
			settings:         aksCiliumSettings,
		}, nil
	default:
		return deployedCiliumProfile{}, fmt.Errorf("unsupported deployed network profile %q", profile)
	}
}

// deployedFixturePullPolicyFor keeps fixtures offline on local clusters, where images are preloaded, and lets only the
// dedicated remote profile pull the same digest-pinned images, because a remote node may not hold them yet.
func deployedFixturePullPolicyFor(profile string) corev1.PullPolicy {
	if resolved, err := resolveDeployedCiliumProfile(profile, true, true, true); err == nil && resolved.remoteDedicated {
		return corev1.PullIfNotPresent
	}
	return corev1.PullNever
}

func deployedFixturePullPolicy() corev1.PullPolicy {
	return deployedFixturePullPolicyFor(os.Getenv("KOGNITION_ADMISSION_NETWORK_PROFILE"))
}

// deployedPositiveControlLabels returns the intended labels of the positive-control client. Policies must select on
// these, never on the labels read back from the created Pod: managed clusters such as AKS add topology.kubernetes.io
// labels that Cilium leaves out of endpoint identities, so a selector copied from the live object matches nothing and
// the policy is silently not enforced.
func deployedPositiveControlLabels(suffix string) map[string]string {
	return map[string]string{"sg07-positive": suffix}
}

// deployedRealizationBudgetFor bounds how long a NetworkPolicy change may take to reach a pod before the gate fails.
// Remote clusters add API and exec round trips to every probe, so only the remote profile gets the longer budget;
// the blocked and reachable assertions themselves are identical.
func deployedRealizationBudgetFor(profile string) time.Duration {
	if resolved, err := resolveDeployedCiliumProfile(profile, true, true, true); err == nil && resolved.remoteDedicated {
		return 90 * time.Second
	}
	return 15 * time.Second
}

func validateDeployedCiliumConfiguration(profile deployedCiliumProfile, data map[string]string) error {
	for key, expected := range profile.settings {
		if data[key] != expected {
			return fmt.Errorf("uncertified Cilium setting %s=%q, expected %q", key, data[key], expected)
		}
	}
	return nil
}

// validateDeployedCiliumAgentDigest binds tag-pinned managed images to the exact digest running on every node.
func validateDeployedCiliumAgentDigest(profile deployedCiliumProfile, agents []corev1.Pod) error {
	if profile.agentDigest == "" {
		return nil
	}
	if len(agents) == 0 {
		return fmt.Errorf("no Cilium agent Pods found to verify the pinned image digest")
	}
	for _, pod := range agents {
		var found bool
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != "cilium-agent" {
				continue
			}
			found = true
			if !strings.HasSuffix(status.ImageID, "@"+profile.agentDigest) {
				return fmt.Errorf("Cilium agent on Pod %s runs %q, expected digest %s", pod.Name, status.ImageID, profile.agentDigest)
			}
		}
		if !found {
			return fmt.Errorf("Cilium agent container missing from Pod %s", pod.Name)
		}
	}
	return nil
}

func validateDeployedNetworkFixtureNamespace(namespace *corev1.Namespace, uid, run, authority string, manifest activation.Manifest) error {
	if namespace == nil || namespace.Name == "" || uid == "" || run == "" || authority == "" ||
		string(namespace.UID) != uid || namespace.DeletionTimestamp != nil {
		return fmt.Errorf("network fixtures require an existing exact-UID operator namespace, recording run and authority boundary")
	}
	if namespace.Name == manifest.Namespace || namespace.Name == authority || slices.Contains(manifest.Namespaces, namespace.Name) ||
		namespace.Name == "default" || strings.HasPrefix(namespace.Name, "kube-") {
		return fmt.Errorf("network fixtures must remain outside installation, workload, authority and system namespaces")
	}
	if namespace.Labels["kognition.dev/acceptance-operator"] != "true" || namespace.Labels["kognition.dev/recording-run"] != run ||
		namespace.Labels["pod-security.kubernetes.io/enforce"] != "privileged" {
		return fmt.Errorf("network fixture namespace must explicitly bind operator scope, recording run and host-network permission")
	}
	return nil
}

func deployedNetworkControls(test *testing.T, ctx context.Context, config *rest.Config, admin client.Client, create func(client.Object), sandbox *corev1.Pod, manifest activation.Manifest, suffix string) func(string, ...*corev1.Pod) {
	test.Helper()
	if os.Getenv("KOGNITION_ADMISSION_CNI") != "cilium" {
		if os.Getenv("KOGNITION_ADMISSION_NETWORK_PROFILE") != "" {
			test.Fatal("an explicit network qualification profile requires Cilium enforcement")
		}
		test.Log("network acceptance not requested; no CNI enforcement claim")
		return func(string, ...*corev1.Pod) {}
	}
	profile, err := resolveDeployedCiliumProfile(
		os.Getenv("KOGNITION_ADMISSION_NETWORK_PROFILE"),
		os.Getenv("KOGNITION_ADMISSION_TARGET_MANIFEST") != "",
		os.Getenv("KOGNITION_ADMISSION_ISOLATED") == "true",
		os.Getenv("KOGNITION_ADMISSION_WRITERS_FROZEN") == "true",
	)
	if err != nil {
		test.Fatal(err)
	}
	daemon, err := kubernetes.NewForConfig(config)
	if err != nil {
		test.Fatal(err)
	}
	cilium, err := daemon.AppsV1().DaemonSets("kube-system").Get(ctx, "cilium", metav1.GetOptions{})
	if err != nil || cilium.Status.DesiredNumberScheduled == 0 || cilium.Status.NumberReady != cilium.Status.DesiredNumberScheduled {
		test.Fatalf("Cilium readiness: daemon=%+v err=%v", cilium, err)
	}
	test.Logf("CNI daemon UID=%s image=%s ready=%d", cilium.UID, cilium.Spec.Template.Spec.Containers[0].Image, cilium.Status.NumberReady)
	if cilium.Spec.Template.Spec.Containers[0].Image != profile.image {
		test.Fatalf("deployed network qualification requires the exact profile image %s", profile.image)
	}
	configuration, err := daemon.CoreV1().ConfigMaps("kube-system").Get(ctx, "cilium-config", metav1.GetOptions{})
	if err != nil {
		test.Fatal(err)
	}
	if err := validateDeployedCiliumConfiguration(profile, configuration.Data); err != nil {
		test.Fatal(err)
	}
	agents, err := daemon.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "k8s-app=cilium"})
	if err != nil {
		test.Fatal(err)
	}
	if err := validateDeployedCiliumAgentDigest(profile, agents.Items); err != nil {
		test.Fatal(err)
	}
	configurationJSON, err := json.Marshal(configuration.Data)
	if err != nil {
		test.Fatal(err)
	}
	test.Logf("CNI configuration UID=%s data=%s", configuration.UID, configurationJSON)
	installation := manifest.Namespace
	fixtureNamespace := installation
	if profile.operatorFixtures {
		name := os.Getenv("KOGNITION_ADMISSION_NETWORK_FIXTURE_NAMESPACE")
		if name == "" {
			test.Fatal("current network qualification requires an explicit operator fixture namespace")
		}
		namespace, err := daemon.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			test.Fatal(err)
		}
		if err := validateDeployedNetworkFixtureNamespace(namespace,
			os.Getenv("KOGNITION_ADMISSION_NETWORK_FIXTURE_NAMESPACE_UID"),
			os.Getenv("KOGNITION_ADMISSION_RECORDING_RUN"),
			os.Getenv("KOGNITION_ADMISSION_AUTHORITY_NAMESPACE"), manifest); err != nil {
			test.Fatal(err)
		}
		fixtureNamespace = namespace.Name
		test.Logf("operator network fixtures namespace=%s UID=%s; production namespace security remains unchanged", namespace.Name, namespace.UID)
	}
	clientset := daemon
	realizationBudget := deployedRealizationBudgetFor(os.Getenv("KOGNITION_ADMISSION_NETWORK_PROFILE"))
	waitRunning := func(pod *corev1.Pod) {
		test.Helper()
		if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
			if err := admin.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
				return false, err
			}
			return pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "", nil
		}); err != nil {
			test.Fatalf("network control %s: %v", pod.Name, err)
		}
	}
	var targets []string
	for _, hostNetwork := range []bool{false, true} {
		server := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("network-control-%t-%s", hostNetwork, suffix), Namespace: fixtureNamespace}, Spec: *sandbox.Spec.DeepCopy()}
		server.Spec.HostNetwork = hostNetwork
		server.Spec.NodeName = sandbox.Spec.NodeName
		server.Spec.Containers[0].Command = []string{"httpd", "-f", "-p", "18080", "-h", "/etc"}
		server.Spec.Containers[0].Stdin, server.Spec.Containers[0].StdinOnce = false, false
		create(server)
		waitRunning(server)
		targets = append(targets, "http://"+server.Status.PodIP+":18080/hostname")
		test.Logf("reachable-control candidate hostNetwork=%t Pod=%s/%s UID=%s IP=%s", hostNetwork, server.Namespace, server.Name, server.UID, server.Status.PodIP)
	}
	control := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "network-client-" + suffix, Namespace: sandbox.Namespace, Labels: deployedPositiveControlLabels(suffix)}, Spec: *sandbox.Spec.DeepCopy()}
	control.Spec.Containers[0].Command = []string{"sleep", "600"}
	control.Spec.Containers[0].Stdin, control.Spec.Containers[0].StdinOnce = false, false
	create(control)
	waitRunning(control)
	execute := func(pod *corev1.Pod, target string) (string, error) {
		command := []string{"sh", "-c", `if wget -q -T 2 -O /dev/null "$1"; then printf reachable; else printf blocked; fi`, "probe", target}
		request := clientset.CoreV1().RESTClient().Post().Namespace(pod.Namespace).Resource("pods").Name(pod.Name).SubResource("exec").VersionedParams(&corev1.PodExecOptions{Container: "exec-probe", Command: command, Stdout: true, Stderr: true}, scheme.ParameterCodec)
		executor, err := remotecommand.NewSPDYExecutor(config, http.MethodPost, request.URL())
		if err != nil {
			return "", err
		}
		probe, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		var stdout, stderr bytes.Buffer
		if err := executor.StreamWithContext(probe, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
			return "", fmt.Errorf("exec transport: %w; stderr=%s", err, stderr.String())
		}
		return stdout.String(), nil
	}
	check := func(phase string, additional ...*corev1.Pod) {
		test.Helper()
		for _, target := range targets {
			var positive string
			if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, realizationBudget, true, func(context.Context) (bool, error) {
				var err error
				positive, err = execute(control, target)
				return positive == "reachable", err
			}); err != nil {
				test.Fatalf("%s positive network control %s: result=%q err=%v", phase, target, positive, err)
			}
			for _, current := range append([]*corev1.Pod{sandbox}, additional...) {
				result, err := execute(current, target)
				if err != nil || result != "blocked" {
					test.Fatalf("%s sandbox %s egress %s: result=%q err=%v; positive control reachable", phase, current.Name, target, result, err)
				}
				test.Logf("%s Pod=%s target=%s positive=reachable sandbox=blocked", phase, current.Name, target)
			}
		}
	}
	check("before-invocation-policy")
	startupLabels := map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/managed": "stdio-invocation", "kognition.dev/stdio-invocation": suffix}
	startup := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "kognition-stdio-invocation-startup-" + suffix, Namespace: sandbox.Namespace, Labels: startupLabels}, Spec: *sandbox.Spec.DeepCopy()}
	startup.Spec.Containers[0].Command = []string{"sh", "-c", `for target; do if wget -q -T 2 -O /dev/null "$target" 2>/dev/null; then echo leaked; exit 1; fi; done; echo blocked`, "startup"}
	startup.Spec.Containers[0].Command = append(startup.Spec.Containers[0].Command, targets...)
	create(startup)
	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := admin.Get(ctx, client.ObjectKeyFromObject(startup), startup); err != nil {
			return false, err
		}
		if startup.Status.Phase == corev1.PodFailed {
			return false, fmt.Errorf("startup probe leaked traffic or failed: %+v", startup.Status.ContainerStatuses)
		}
		return startup.Status.Phase == corev1.PodSucceeded, nil
	}); err != nil {
		test.Fatal(err)
	}
	logs, err := clientset.CoreV1().Pods(startup.Namespace).GetLogs(startup.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(logs)), "blocked") {
		test.Fatalf("startup probe logs=%q err=%v", logs, err)
	}
	test.Logf("first-command egress blocked before invocation policy: Pod=%s UID=%s", startup.Name, startup.UID)
	additiveCreated := false
	return func(phase string, additional ...*corev1.Pod) {
		check(phase, additional...)
		if additiveCreated {
			return
		}
		create(&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "control-deny-" + suffix, Namespace: sandbox.Namespace}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: deployedPositiveControlLabels(suffix)}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}}})
		for _, target := range targets {
			var result string
			var probeErr error
			attempts := 0
			if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, realizationBudget, true, func(context.Context) (bool, error) {
				attempts++
				result, probeErr = execute(control, target)
				return result == "blocked", probeErr
			}); err != nil {
				test.Fatalf("positive client deny policy not realized for %s after %d probes within %s: last result=%q probe error=%v: %v", target, attempts, realizationBudget, result, probeErr, err)
			}
			test.Logf("positive client deny policy realized for %s after %d probes", target, attempts)
		}
		allow := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "additive-allow-" + suffix, Namespace: sandbox.Namespace},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress:      []networkingv1.NetworkPolicyEgressRule{{}},
			},
		}
		create(allow)
		additiveCreated = true
		test.Logf("injected additive allow UID=%s selecting sandbox and positive control", allow.UID)
		for attempt := 0; attempt < 3; attempt++ {
			check("additive-allow-present")
		}
	}
}
