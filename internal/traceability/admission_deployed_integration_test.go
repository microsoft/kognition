// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kognition-ai/kognition/internal/k8s/activation"
	"github.com/kognition-ai/kognition/internal/k8s/installation"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDeployedReviewedAdmissionTarget(test *testing.T) {
	path := os.Getenv("KOGNITION_ADMISSION_TARGET_MANIFEST")
	if path == "" {
		test.Skip("explicit reviewed deployment manifest required")
	}
	if os.Getenv("KOGNITION_ADMISSION_WRITERS_FROZEN") != "true" || os.Getenv("KOGNITION_ADMISSION_ISOLATED") != "true" {
		test.Fatal("reviewed-target acceptance requires an isolated local target and frozen writers")
	}
	requireDeployedAcceptanceBudget(test)
	content, err := os.ReadFile(path)
	if err != nil {
		test.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	if digest != os.Getenv("KOGNITION_ADMISSION_TARGET_SHA256") {
		test.Fatal("reviewed target manifest digest differs")
	}
	manifest, err := activation.Parse(strings.NewReader(string(content)))
	if err != nil {
		test.Fatal(err)
	}
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KOGNITION_ADMISSION_KUBECONFIG"))
	if err != nil {
		test.Fatalf("reviewed target kubeconfig: %v", err)
	}
	config.Timeout = 15 * time.Second
	config.QPS = 50
	config.Burst = 100
	admin, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	root := &corev1.Namespace{}
	if err := admin.Get(ctx, client.ObjectKey{Name: "kube-system"}, root); err != nil || string(root.UID) != os.Getenv("KOGNITION_ADMISSION_EXPECTED_CLUSTER_UID") {
		test.Fatalf("reviewed cluster UID differs: %v", err)
	}
	namespaces := &corev1.NamespaceList{}
	if err := admin.List(ctx, namespaces); err != nil {
		test.Fatal(err)
	}
	observed := make([]string, 0, len(namespaces.Items))
	for _, namespace := range namespaces.Items {
		observed = append(observed, namespace.Name)
	}
	permitted := permittedDeployedNamespaces(append([]string{manifest.Namespace, os.Getenv("KOGNITION_ADMISSION_AUTHORITY_NAMESPACE"), os.Getenv("KOGNITION_ADMISSION_NETWORK_FIXTURE_NAMESPACE")}, manifest.Namespaces...), os.Getenv("KOGNITION_ADMISSION_PERMITTED_NAMESPACES"))
	if err := validateDeployedTarget(deployedTargetEvidence{
		Profile: os.Getenv("KOGNITION_ADMISSION_NETWORK_PROFILE"), Host: config.Host, InsecureTLS: config.Insecure,
		RemoteEndpoint:      os.Getenv("KOGNITION_ADMISSION_REMOTE_ENDPOINT"),
		DedicatedClusterUID: os.Getenv("KOGNITION_ADMISSION_DEDICATED_CLUSTER_UID"),
		ClusterUID:          string(root.UID), Namespaces: observed, Permitted: permitted,
	}); err != nil {
		test.Fatalf("reviewed target endpoint: %v", err)
	}
	if err := activation.Verify(ctx, admin, manifest); err != nil {
		test.Fatal(err)
	}
	image := os.Getenv("KOGNITION_ADMISSION_STREAM_IMAGE")
	if image == "" || manifest.StdioIsolationProfile != "cilium" || os.Getenv("KOGNITION_ADMISSION_CNI") != "cilium" {
		test.Fatal("reviewed Cilium profile and explicit stream image required")
	}
	test.Logf("reviewed target endpoint=%s clusterUID=%s manifestSHA256=%s controller=%s/%s namespaces=%v", config.Host, root.UID, digest, manifest.Namespace, manifest.Controller, manifest.Namespaces)
	test.Run("native-policies", func(test *testing.T) {
		probes, err := activation.VerifyPolicyBehavior(ctx, config, manifest)
		if err != nil || len(probes) != 6*len(manifest.Namespaces) {
			test.Fatalf("six policy probes per namespace required: count=%d namespaces=%d error=%v", len(probes), len(manifest.Namespaces), err)
		}
	})
	test.Run("deployed-attach", func(test *testing.T) {
		var retained []client.Object
		for _, expected := range manifest.Objects {
			if expected.GetKind() == "NetworkPolicy" {
				live := expected.DeepCopy()
				if err := admin.Get(ctx, client.ObjectKeyFromObject(expected), live); err != nil {
					test.Fatal(err)
				}
				retained = append(retained, live)
			}
		}
		deployedAttachmentAcceptance(test, config, manifest, image, uuid.NewString()[:8], &retained)
	})
	documents, err := installation.Documents(content)
	if err != nil {
		test.Fatal(err)
	}
	var roles []*unstructured.Unstructured
	suffix := uuid.NewString()[:8]
	for _, object := range documents {
		if (object.GetKind() == "Role" || object.GetKind() == "ClusterRole") && object.GetLabels()["app.kubernetes.io/component"] == "k8s-controller" {
			role := object.DeepCopy()
			role.SetName(fmt.Sprintf("target-probe-%s-%d", suffix, len(roles)))
			roles = append(roles, role)
		}
	}
	test.Run("production-stdio", func(test *testing.T) {
		deployedStdioProducerAcceptance(test, config, manifest, roles, image, suffix)
	})
	test.Run("production-ingestion", func(test *testing.T) {
		ingester, database := os.Getenv("KOGNITION_ADMISSION_INGESTER_IMAGE"), os.Getenv("KOGNITION_ADMISSION_DATABASE_IMAGE")
		if ingester == "" || database == "" || os.Getenv("KOGNITION_ADMISSION_AGENT_IMAGE") == "" || os.Getenv("KOGNITION_ADMISSION_TARGET_RUNTIME_SECRET") == "" {
			test.Fatal("reviewed producer images and runtime source Secret are required")
		}
		deployedIngestionProducerAcceptance(test, config, manifest, roles, ingester, database)
	})
	if err := activation.Verify(ctx, admin, manifest); err != nil {
		test.Fatal(err)
	}
	test.Log("reviewed target remains inactive; temporary producer grants and workloads absent")
}

func TestDeployedAdmissionPreflight(test *testing.T) {
	kubeconfig, image := os.Getenv("KOGNITION_ADMISSION_KUBECONFIG"), os.Getenv("KOGNITION_ADMISSION_GUARD_IMAGE")
	if kubeconfig == "" || image == "" {
		test.Skip("explicit isolated kubeconfig and KOGNITION_ADMISSION_GUARD_IMAGE required")
	}
	streamImage := os.Getenv("KOGNITION_ADMISSION_STREAM_IMAGE")
	if streamImage == "" {
		test.Fatal("KOGNITION_ADMISSION_STREAM_IMAGE must identify a preloaded BusyBox image")
	}
	separator := strings.LastIndex(image, ":")
	if separator <= strings.LastIndex(image, "/") || separator == len(image)-1 || strings.Contains(image, "@") {
		test.Fatal("guard image must have an explicit tag")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		test.Fatal(err)
	}
	config.Timeout = 10 * time.Second
	cluster, err := kubernetes.NewForConfig(config)
	if err != nil {
		test.Fatal(err)
	}
	version, err := cluster.Discovery().ServerVersion()
	if err != nil {
		test.Fatal(err)
	}
	if expected := os.Getenv("KOGNITION_ADMISSION_EXPECTED_VERSION"); expected == "" || expected != version.GitVersion {
		test.Fatalf("explicit expected version mismatch: expected=%q actual=%s", expected, version.GitVersion)
	}
	if !strings.HasPrefix(config.Host, "https://127.0.0.1:") || os.Getenv("KOGNITION_ADMISSION_ISOLATED") != "true" {
		test.Fatal("deployed acceptance requires explicit KOGNITION_ADMISSION_ISOLATED=true and a loopback control plane")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	admin, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		test.Fatal(err)
	}
	suffix := uuid.NewString()[:8]
	installation, workload := "sg03-control-"+suffix, "sg03-work-"+suffix
	test.Logf("server=%s endpoint=%s image=%s namespaces=%s,%s", version.GitVersion, config.Host, image, installation, workload)
	var created []client.Object
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for index := len(created) - 1; index >= 0; index-- {
			object := created[index]
			if object.GetUID() == "" {
				test.Errorf("cleanup missing UID: %T %s", object, object.GetName())
				continue
			}
			err := admin.Delete(cleanup, object, client.Preconditions{UID: ptr.To(object.GetUID())}, client.PropagationPolicy(metav1.DeletePropagationBackground))
			if err != nil && !apierrors.IsNotFound(err) {
				test.Errorf("cleanup %T %s: %v", object, object.GetName(), err)
				continue
			}
			if err := wait.PollUntilContextTimeout(cleanup, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
				current := object.DeepCopyObject().(client.Object)
				err := admin.Get(ctx, client.ObjectKeyFromObject(object), current)
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				if err == nil && current.GetUID() != object.GetUID() {
					return false, fmt.Errorf("replacement UID for %T %s", object, object.GetName())
				}
				return false, err
			}); err != nil {
				test.Errorf("cleanup confirmation %T %s: %v", object, object.GetName(), err)
			} else {
				test.Logf("cleanup confirmed %T %s/%s UID=%s", object, object.GetNamespace(), object.GetName(), object.GetUID())
			}
		}
	})
	create := func(object client.Object) {
		test.Helper()
		if err := createAdmissionFixture(ctx, admin, object, &created); err != nil {
			test.Fatal(err)
		}
	}
	for _, namespace := range []string{installation, workload} {
		create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
	}
	guard := "sg03-" + suffix + "-admission-guard"
	certificate, key := admissionServingCertificate(test, guard+"."+installation+".svc")
	create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: guard + "-tls", Namespace: installation}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": certificate, "tls.key": key}})
	var rendered strings.Builder
	var controller *unstructured.Unstructured
	var guardNetwork *unstructured.Unstructured
	var producerRoles []*unstructured.Unstructured
	chartArgs := []string{"--namespace", installation, "--set", "fullnameOverride=sg03-" + suffix,
		"--set", "networkPolicy.enabled=true",
		"--set", "runtimeAuth.existingSecret=" + deployedRuntimeSourceName,
		"--set", "admissionGuard.stdioIsolationProfile=" + deployedIsolationProfile(),
		"--set", "k8sController.managedNamespaces[0]=" + workload, "--set", "admissionGuard.tls.existingSecret=" + guard + "-tls",
		"--set", "admissionGuard.tls.caBundle=" + base64.StdEncoding.EncodeToString(certificate),
		"--set", "k8sController.image.repository=" + image[:separator], "--set", "k8sController.image.tag=" + image[separator+1:], "--set", "k8sController.image.pullPolicy=Never"}
	if os.Getenv("KOGNITION_ADMISSION_PROVIDER_IMAGE") != "" {
		address := "sg08-provider." + workload + ".svc:9443"
		chartArgs = append(chartArgs, "--set", "knowledgeGateway.enabled=true",
			"--set", "knowledgeGateway.provider.name=sg08-local", "--set", "knowledgeGateway.provider.address="+address,
			"--set", "knowledgeGateway.provider.serverName=knowledge.test", "--set", "knowledgeGateway.provider.peerURI=spiffe://kognition.test/providers/sg08",
			"--set", "knowledgeGateway.provider.kubernetesApiEgressCIDRs[0]=10.96.0.1/32",
			"--set", "knowledgeGateway.tls.secretName=sg08-provider-client", "--set", "knowledgeGateway.runtimeTLS.secretName=sg08-provider-runtime",
			"--set", "knowledgeGateway.runtimeTLS.serverName=knowledge.test")
	}
	for _, document := range renderControllerChart(test, chartArgs...) {
		object := &unstructured.Unstructured{}
		if err := object.UnmarshalJSON(document.Raw); err != nil {
			test.Fatal(err)
		}
		annotations := object.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["kognition.dev/admission-fixture-owner"] = uuid.NewString()
		object.SetAnnotations(annotations)
		if (object.GetKind() == "Role" || object.GetKind() == "ClusterRole") && object.GetLabels()["app.kubernetes.io/component"] == "k8s-controller" {
			producerRoles = append(producerRoles, object.DeepCopy())
		}
		raw, err := object.MarshalJSON()
		if err != nil {
			test.Fatal(err)
		}
		rendered.WriteString("---\n")
		rendered.Write(raw)
		rendered.WriteByte('\n')
		component, _, _ := unstructured.NestedString(object.Object, "spec", "template", "metadata", "labels", "app.kubernetes.io/component")
		if object.GetKind() == "Deployment" && component == "k8s-controller" {
			controller = object
		}
		if object.GetKind() == "NetworkPolicy" && object.GetNamespace() == installation && object.GetName() == guard {
			guardNetwork = object
		}
	}
	manifest, err := activation.Parse(strings.NewReader(rendered.String()))
	if err != nil {
		test.Fatal(err)
	}
	if controller == nil {
		test.Fatal("inactive controller missing")
	}
	if guardNetwork == nil {
		test.Fatal("rendered guard NetworkPolicy missing")
	}
	create(guardNetwork)
	create(controller)
	for _, object := range manifest.Objects {
		create(object.DeepCopy())
	}
	if err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		return activation.Verify(ctx, admin, manifest) == nil, nil
	}); err != nil {
		test.Fatalf("bootstrap readiness: %v; verification: %v", err, activation.Verify(ctx, admin, manifest))
	}
	test.Run("cilium-inventory-drift", func(test *testing.T) {
		if manifest.StdioIsolationProfile != "cilium" {
			test.Skip("Cilium isolation profile required")
		}
		for _, expected := range manifest.Objects {
			if expected.GetKind() != "CiliumNetworkPolicy" {
				continue
			}
			for _, scenario := range []string{"weakened-deny", "release-label"} {
				test.Run(scenario, func(test *testing.T) {
					current := expected.DeepCopy()
					if err := admin.Get(ctx, client.ObjectKeyFromObject(expected), current); err != nil {
						test.Fatal(err)
					}
					original := current.DeepCopy()
					defer func() {
						recovery, cancel := context.WithTimeout(context.Background(), 15*time.Second)
						defer cancel()
						if err := admin.Get(recovery, client.ObjectKeyFromObject(expected), current); err != nil {
							test.Error(err)
							return
						}
						if current.GetUID() != original.GetUID() {
							test.Error("Cilium policy replaced during drift check")
							return
						}
						current.Object["spec"] = original.Object["spec"]
						current.SetLabels(original.GetLabels())
						if err := admin.Update(recovery, current); err != nil {
							test.Error(err)
						}
					}()
					if scenario == "weakened-deny" {
						unstructured.RemoveNestedField(current.Object, "spec", "egressDeny")
						if err := unstructured.SetNestedSlice(current.Object, []interface{}{map[string]interface{}{}}, "spec", "egress"); err != nil {
							test.Fatal(err)
						}
					} else {
						labels := current.GetLabels()
						labels["app.kubernetes.io/instance"] = "foreign-release"
						current.SetLabels(labels)
					}
					if err := admin.Update(ctx, current); err != nil {
						test.Fatal(err)
					}
					if err := activation.Verify(ctx, admin, manifest); err == nil || !strings.Contains(err.Error(), expected.GetName()) {
						test.Fatalf("live Cilium drift must reject activation for policy %s: %v", expected.GetName(), err)
					} else {
						test.Logf("live Cilium drift rejected: UID=%s scenario=%s err=%v", current.GetUID(), scenario, err)
					}
				})
				if err := activation.Verify(ctx, admin, manifest); err != nil {
					test.Fatalf("Cilium inventory recovery: %v", err)
				}
			}
		}
	})
	test.Run("six-live-policies", func(test *testing.T) {
		results, err := activation.VerifyPolicyBehavior(ctx, config, manifest)
		if err != nil {
			test.Fatal(err)
		}
		if len(results) != 6 {
			test.Fatalf("expected six outcomes: %+v", results)
		}
		for _, result := range results {
			if !result.Allowed || !result.Denied || !result.CleanupConfirmed || result.UID == "" {
				test.Fatalf("incomplete probe: %+v", result)
			}
			test.Logf("policy=%s object=%s/%s UID=%s allowed=%t denied=%t cleanup=%t", result.Policy, result.Namespace, result.Name, result.UID, result.Allowed, result.Denied, result.CleanupConfirmed)
		}
	})
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "kognition-stdio-invocation-" + suffix, Namespace: workload, Labels: map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/stdio-invocation": suffix}}, Spec: corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false),
		SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Containers:      []corev1.Container{{Name: "exec-probe", Image: "registry.k8s.io/pause:3.9", Stdin: true, StdinOnce: true, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}},
	}}
	validPod := func() error { return admin.Create(ctx, pod.DeepCopy(), client.DryRunAll) }
	test.Run("service-tls-sandbox-allow-deny", func(test *testing.T) {
		if err := validPod(); err != nil {
			test.Fatalf("valid sandbox via deployed Service: %v", err)
		}
		unsafe := pod.DeepCopy()
		unsafe.Spec.HostPID = true
		if err := admin.Create(ctx, unsafe, client.DryRunAll); !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "sandbox.guard.kognition.dev") {
			test.Fatalf("sandbox guard denial required: %v", err)
		}
	})
	webhooks := &admissionv1.ValidatingWebhookConfiguration{}
	if err := admin.Get(ctx, client.ObjectKey{Name: guard}, webhooks); err != nil {
		test.Fatal(err)
	}
	service := &corev1.Service{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: installation, Name: guard}, service); err != nil {
		test.Fatal(err)
	}
	alias := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "wrong-" + suffix, Namespace: installation}, Spec: corev1.ServiceSpec{Selector: service.Spec.Selector, Ports: service.Spec.Ports}}
	create(alias)
	emptyService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "empty-" + suffix, Namespace: installation}, Spec: corev1.ServiceSpec{Selector: map[string]string{"sg03-no-endpoint": suffix}, Ports: service.Spec.Ports}}
	create(emptyService)
	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		endpoints := &discoveryv1.EndpointSliceList{}
		if err := admin.List(ctx, endpoints, client.InNamespace(emptyService.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: emptyService.Name}); err != nil {
			return false, err
		}
		for _, endpointSlice := range endpoints.Items {
			if len(endpointSlice.Endpoints) != 0 {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		test.Fatalf("empty Service endpoints: %v", err)
	}
	for _, fault := range []string{"wrong-ca", "wrong-dns", "no-endpoints"} {
		test.Run(fault, func(test *testing.T) {
			original := webhooks.DeepCopy()
			for index := range webhooks.Webhooks {
				if fault == "wrong-ca" {
					other, _ := admissionServingCertificate(test, "wrong.invalid")
					webhooks.Webhooks[index].ClientConfig.CABundle = other
				} else if fault == "no-endpoints" {
					webhooks.Webhooks[index].ClientConfig.Service.Name = emptyService.Name
				} else {
					webhooks.Webhooks[index].ClientConfig.Service.Name = alias.Name
				}
			}
			if err := admin.Update(ctx, webhooks); err != nil {
				test.Fatal(err)
			}
			defer func() {
				restored := webhooks.DeepCopy()
				restored.Webhooks = original.Webhooks
				if err := admin.Patch(ctx, restored, client.MergeFrom(webhooks)); err != nil {
					test.Error(err)
				}
				webhooks = restored
			}()
			var response error
			if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
				response = validPod()
				if response == nil || !strings.Contains(response.Error(), "failed calling webhook") || !strings.Contains(response.Error(), "sandbox.guard.kognition.dev") {
					return false, nil
				}
				if fault == "wrong-dns" {
					return strings.Contains(response.Error(), "x509") && strings.Contains(response.Error(), "not "+alias.Name+"."+installation+".svc"), nil
				}
				if fault == "wrong-ca" {
					return strings.Contains(response.Error(), "x509") && strings.Contains(response.Error(), "unknown authority"), nil
				}
				return strings.Contains(response.Error(), emptyService.Name+"."+installation+".svc") && !strings.Contains(response.Error(), "x509"), nil
			}); err != nil {
				test.Fatalf("fail-closed %s: %v; response=%v", fault, err, response)
			}
			test.Logf("%s failed closed: %v", fault, response)
		})
		if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 15*time.Second, true, func(context.Context) (bool, error) { return validPod() == nil, nil }); err != nil {
			test.Fatalf("guard recovery after %s: %v", fault, err)
		}
	}
	test.Run("deployed-attach", func(test *testing.T) {
		deployedAttachmentAcceptance(test, config, manifest, streamImage, suffix, &created)
	})
	test.Run("production-stdio", func(test *testing.T) {
		if os.Getenv("KOGNITION_ADMISSION_PRODUCERS") != "true" {
			test.Skip("KOGNITION_ADMISSION_PRODUCERS=true required for SG-08 production lifecycle acceptance")
		}
		deployedStdioProducerAcceptance(test, config, manifest, producerRoles, streamImage, suffix)
	})
	test.Run("production-ingestion", func(test *testing.T) {
		if os.Getenv("KOGNITION_ADMISSION_PRODUCERS") != "true" || os.Getenv("KOGNITION_ADMISSION_INGESTER_IMAGE") == "" || os.Getenv("KOGNITION_ADMISSION_DATABASE_IMAGE") == "" {
			test.Skip("production ingestion requires explicit local ingester and database images")
		}
		deployedIngestionProducerAcceptance(test, config, manifest, producerRoles, os.Getenv("KOGNITION_ADMISSION_INGESTER_IMAGE"), os.Getenv("KOGNITION_ADMISSION_DATABASE_IMAGE"))
	})
	if err := activation.Verify(ctx, admin, manifest); err != nil {
		test.Fatal(err)
	}
	deployment := &appsv1.Deployment{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(controller), deployment); err != nil {
		test.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
		test.Fatal("controller activated")
	}
	test.Logf("final bootstrap verified; controller=%s/%s replicas=0; grants unchanged", installation, manifest.Controller)
	guardPods := &corev1.PodList{}
	if err := admin.List(ctx, guardPods, client.InNamespace(installation), client.MatchingLabels(service.Spec.Selector)); err != nil {
		test.Fatal(err)
	}
	if len(guardPods.Items) == 0 {
		test.Fatal("no guard Pods for runtime image evidence")
	}
	for _, guardPod := range guardPods.Items {
		for _, status := range guardPod.Status.ContainerStatuses {
			if status.ImageID == "" {
				test.Fatal("missing guard runtime image ID")
			}
			test.Logf("guard Pod=%s UID=%s container=%s image=%s imageID=%s", guardPod.Name, guardPod.UID, status.Name, status.Image, status.ImageID)
		}
	}
}

func admissionServingCertificate(test *testing.T, hostname string) ([]byte, []byte) {
	test.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		test.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		test.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: hostname}, DNSNames: []string{hostname}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		test.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		test.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
}
