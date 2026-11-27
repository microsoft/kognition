// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	"github.com/kognition-ai/kognition/internal/k8s/installation"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

type freshOptions struct {
	kubeconfig, context, chart, values, release, namespace, authority, workloads, state string
	replicas                                                                            int
	exclusive                                                                           bool
}

func runFresh(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fresh", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := freshOptions{}
	flags.StringVar(&options.kubeconfig, "kubeconfig", "", "Target kubeconfig; its selected context is captured without changing global configuration")
	flags.StringVar(&options.context, "context", "", "Context from the supplied kubeconfig")
	flags.StringVar(&options.chart, "chart", "", "Local packaged Kognition .tgz chart")
	flags.StringVar(&options.values, "values", "", "Kubernetes installation values, including image digests and network configuration")
	flags.StringVar(&options.release, "release", "kognition", "New Helm release")
	flags.StringVar(&options.namespace, "namespace", "kognition-system", "Dedicated installation namespace")
	flags.StringVar(&options.authority, "authority-namespace", "kognition-installation-authority", "Separate namespace for installation records")
	flags.StringVar(&options.workloads, "workload-namespaces", "", "Explicit comma-separated dedicated Agent namespaces")
	flags.StringVar(&options.state, "state-dir", ".local/kognition-install", "New private directory retained for diagnostics and recovery")
	flags.IntVar(&options.replicas, "replicas", 1, "Controller replicas to start after checks pass")
	flags.BoolVar(&options.exclusive, "exclusive", false, "Confirm no other installer, GitOps or workload writer targets these namespaces during installation")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "fresh accepts named options only")
		return 2
	}
	if err := options.validate(); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	path, err := prepareFresh(options, stdout, stderr)
	if err == nil {
		err = execute("fresh", path, "", "", "", stdout, stderr)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "Fresh installation failed: %v\nPreserve any state created in %s; do not retry over a partial installation or remove its state.\n", err, options.state)
		return 1
	}
	return 0
}

func (options freshOptions) validate() error {
	if options.kubeconfig == "" || options.chart == "" || options.values == "" || !strings.HasSuffix(options.chart, ".tgz") ||
		options.state == "" || !options.exclusive || options.replicas < 1 || options.replicas > 100 {
		return errors.New("fresh requires --kubeconfig, --chart <local.tgz>, --values, --workload-namespaces and --exclusive; use a new --state-dir and 1-100 replicas")
	}
	if len(validation.IsDNS1123Subdomain(options.release)) != 0 || len(options.release) > 53 {
		return errors.New("invalid Helm release name")
	}
	seen := map[string]bool{}
	for _, name := range append([]string{options.namespace, options.authority}, strings.Split(options.workloads, ",")...) {
		if name == "" || name == "default" || strings.HasPrefix(name, "kube-") || len(validation.IsDNS1123Label(name)) != 0 || seen[name] {
			return errors.New("installation, authority and workload namespaces must be valid, distinct, dedicated names")
		}
		seen[name] = true
	}
	return nil
}

func freshValues(content []byte, options freshOptions) ([]byte, error) {
	var values map[string]interface{}
	if err := yaml.UnmarshalStrict(content, &values); err != nil {
		return nil, err
	}
	if values == nil {
		return nil, errors.New("installation values must contain a YAML object")
	}
	if mode, found := values["mode"]; found && mode != "kubernetes" {
		return nil, errors.New("fresh installs Kubernetes mode; the values select another mode")
	}
	values["mode"] = "kubernetes"
	section := func(key string) (map[string]interface{}, error) {
		if value, found := values[key]; found {
			object, ok := value.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("values.%s must be an object", key)
			}
			return object, nil
		}
		object := map[string]interface{}{}
		values[key] = object
		return object, nil
	}
	controller, err := section("k8sController")
	if err != nil {
		return nil, err
	}
	image, ok := controller["image"].(map[string]interface{})
	if !ok {
		return nil, errors.New("k8sController.image.digest must pin the controller image before installation")
	}
	digest, ok := image["digest"].(string)
	encoded, hasPrefix := strings.CutPrefix(digest, "sha256:")
	digestBytes, digestErr := hex.DecodeString(encoded)
	if !ok || !hasPrefix || digestErr != nil || len(digestBytes) != sha256.Size || encoded != strings.ToLower(encoded) {
		return nil, errors.New("k8sController.image.digest must be a lowercase sha256 image digest")
	}
	controller["replicas"], controller["replicaCount"], controller["activated"] = 0, options.replicas, false
	controller["managedNamespaces"] = strings.Split(options.workloads, ",")
	guard, err := section("admissionGuard")
	if err != nil {
		return nil, err
	}
	if profile, found := guard["stdioIsolationProfile"]; found && profile != "cilium" {
		return nil, errors.New("fresh installation requires admissionGuard.stdioIsolationProfile=cilium")
	}
	guard["stdioIsolationProfile"] = "cilium"
	network, err := section("networkPolicy")
	if err != nil {
		return nil, err
	}
	if enabled, found := network["enabled"]; found && enabled != true {
		return nil, errors.New("fresh installation requires networkPolicy.enabled=true")
	}
	network["enabled"] = true
	if enabled, found := network["ciliumHostProbeIngress"]; found && enabled != true {
		return nil, errors.New("fresh installation requires Cilium node-health probe ingress")
	}
	network["ciliumHostProbeIngress"] = true
	cidrs, ok := network["kubeletProbeIngressCIDRs"].([]interface{})
	if _, found := network["kubeletProbeIngressCIDRs"]; found && !ok {
		return nil, errors.New("networkPolicy.kubeletProbeIngressCIDRs must be a list")
	}
	for _, value := range cidrs {
		text, ok := value.(string)
		if !ok {
			return nil, errors.New("kubelet probe CIDRs must be strings")
		}
		prefix, err := netip.ParsePrefix(text)
		if err != nil || prefix.Bits() == 0 {
			return nil, errors.New("kubelet probe CIDRs must be explicit, valid node network prefixes")
		}
	}
	if enabled, found := network["ciliumApiServerEgress"]; found && enabled != true {
		return nil, errors.New("fresh installation requires the scoped Cilium Kubernetes API egress policy")
	}
	network["ciliumApiServerEgress"] = true
	migrations, err := section("migrations")
	if err != nil {
		return nil, err
	}
	migrations["retainCompletedJobs"] = true
	runtimeAuth, err := section("runtimeAuth")
	if err != nil {
		return nil, err
	}
	if existing, configured := runtimeAuth["existingSecret"]; configured {
		if _, ok := existing.(string); !ok {
			return nil, errors.New("runtimeAuth.existingSecret must be a string")
		}
	}
	if create, configured := runtimeAuth["create"]; configured {
		if _, ok := create.(bool); !ok {
			return nil, errors.New("runtimeAuth.create must be a boolean")
		}
	} else {
		existing, _ := runtimeAuth["existingSecret"].(string)
		runtimeAuth["create"] = existing == ""
	}
	postgres, err := section("postgres")
	if err != nil {
		return nil, err
	}
	if postgres["enabled"] != false {
		auth, found := postgres["auth"]
		if !found {
			auth = map[string]interface{}{}
			postgres["auth"] = auth
		}
		settings, ok := auth.(map[string]interface{})
		if !ok {
			return nil, errors.New("postgres.auth must be an object")
		}
		if settings["existingSecret"] == nil || settings["existingSecret"] == "" {
			if password := settings["password"]; password == nil || password == "" || password == "kognition" {
				material := make([]byte, 32)
				if _, err := rand.Read(material); err != nil {
					return nil, err
				}
				settings["password"] = base64.RawURLEncoding.EncodeToString(material)
			}
		}
	}
	return json.MarshalIndent(values, "", "  ")
}

func freshReplicaCount(artifact installation.Artifact) (int32, error) {
	content, err := installation.ReadArtifact(artifact)
	if err != nil {
		return 0, err
	}
	var values struct {
		Controller struct {
			Replicas int32 `json:"replicaCount"`
		} `json:"k8sController"`
	}
	if err := json.Unmarshal(content, &values); err != nil {
		return 0, err
	}
	if values.Controller.Replicas < 1 || values.Controller.Replicas > 100 {
		return 0, errors.New("fresh installation values require 1-100 controller replicas")
	}
	return values.Controller.Replicas, nil
}

func prepareFresh(options freshOptions, stdout, stderr io.Writer) (string, error) {
	chart, err := os.ReadFile(options.chart)
	if err != nil {
		return "", err
	}
	sourceValues, err := os.ReadFile(options.values)
	if err != nil {
		return "", err
	}
	values, err := freshValues(sourceValues, options)
	if err != nil {
		return "", err
	}
	kubeconfig, err := clientcmd.LoadFromFile(options.kubeconfig)
	if err != nil {
		return "", err
	}
	if options.context != "" {
		kubeconfig.CurrentContext = options.context
	}
	if err := clientcmdapi.MinifyConfig(kubeconfig); err != nil {
		return "", err
	}
	for _, auth := range kubeconfig.AuthInfos {
		if auth.Exec != nil && strings.ContainsAny(auth.Exec.Command, `/\`) && !filepath.IsAbs(auth.Exec.Command) {
			base, err := filepath.Abs(filepath.Dir(options.kubeconfig))
			if err != nil {
				return "", err
			}
			auth.Exec.Command = filepath.Join(base, auth.Exec.Command)
		}
	}
	if err := clientcmdapi.FlattenConfig(kubeconfig); err != nil {
		return "", err
	}
	config, err := clientcmd.NewDefaultClientConfig(*kubeconfig, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return "", err
	}
	endpoint, err := url.Parse(config.Host)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		endpoint.Path != "" && endpoint.Path != "/" || config.TLSClientConfig.Insecure || config.Impersonate.UserName != "" ||
		config.Impersonate.UID != "" || len(config.Impersonate.Groups) != 0 || len(config.Impersonate.Extra) != 0 {
		return "", errors.New("fresh installation requires an explicit HTTPS cluster with verified TLS and no impersonation")
	}
	config.Timeout = 15 * time.Second
	config.QPS, config.Burst = 50, 100
	admin, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return "", err
	}
	discover, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return "", err
	}
	for _, group := range []string{"kognition.dev/v1alpha1", "cert-manager.io/v1", "cilium.io/v2", "admissionregistration.k8s.io/v1"} {
		if _, err := discover.ServerResourcesForGroupVersion(group); err != nil {
			return "", fmt.Errorf("required Kubernetes API %s is unavailable: %w", group, err)
		}
	}
	version, err := discover.ServerVersion()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := &corev1.Namespace{}
	if err := admin.Get(ctx, client.ObjectKey{Name: "kube-system"}, root); err != nil {
		return "", err
	}
	if root.UID == "" || root.DeletionTimestamp != nil {
		return "", errors.New("cluster identity is unavailable or deleting")
	}
	helm, err := exec.LookPath("helm")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(options.state), 0700); err != nil {
		return "", err
	}
	if err := os.Mkdir(options.state, 0700); err != nil {
		return "", fmt.Errorf("a new private state directory is required: %w", err)
	}
	state, err := filepath.Abs(options.state)
	if err != nil {
		return "", err
	}
	artifact := func(name string, content []byte) (installation.Artifact, error) {
		path := filepath.Join(state, name)
		if err := os.WriteFile(path, content, 0600); err != nil {
			return installation.Artifact{}, err
		}
		return installation.Artifact{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(content))}, nil
	}
	chartArtifact, err := artifact("chart.tgz", chart)
	if err != nil {
		return "", err
	}
	valuesArtifact, err := artifact("values.json", values)
	if err != nil {
		return "", err
	}
	configContent, err := clientcmd.Write(*kubeconfig)
	if err != nil {
		return "", err
	}
	configArtifact, err := artifact("kubeconfig", configContent)
	if err != nil {
		return "", err
	}
	command := func(arguments ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, helm, arguments...)
		cmd.Env = helmEnvironment(os.Environ(), "", "")
		cmd.Stderr = stderr
		return cmd.Output()
	}
	releases, err := command("list", "--deployed", "--failed", "--pending", "--superseded", "--uninstalled", "--uninstalling", "--kubeconfig", configArtifact.Path, "--namespace", options.namespace, "--output", "json")
	if err != nil {
		return "", err
	}
	var releaseList []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(releases, &releaseList); err != nil {
		return "", err
	}
	for _, release := range releaseList {
		if release.Name == options.release {
			return "", errors.New("release already exists, including failed or pending history; use the upgrade/recovery workflow")
		}
	}
	rendered, err := command("template", options.release, chartArtifact.Path, "--kubeconfig", configArtifact.Path,
		"--namespace", options.namespace, "--values", valuesArtifact.Path, "--no-hooks", "--kube-version", version.GitVersion)
	if err != nil {
		return "", err
	}
	if _, err := activation.ParseCandidate(rendered, options.release); err != nil {
		return "", err
	}
	manifest, err := activation.Parse(strings.NewReader(string(rendered)))
	if err != nil {
		return "", err
	}
	expectedNamespaces := strings.Split(options.workloads, ",")
	actualNamespaces := slices.Clone(manifest.Namespaces)
	sort.Strings(expectedNamespaces)
	sort.Strings(actualNamespaces)
	if manifest.Namespace != options.namespace || !slices.Equal(actualNamespaces, expectedNamespaces) {
		return "", errors.New("rendered installation or workload namespaces differ from the selected target")
	}
	manifestArtifact, err := artifact("bootstrap.yaml", rendered)
	if err != nil {
		return "", err
	}
	plan := installation.Plan{
		Version: 1, Kubeconfig: configArtifact.Path, Endpoint: config.Host, ClusterUID: string(root.UID),
		Release: options.release, AuthorityNamespace: options.authority,
		Chart: chartArtifact, Values: valuesArtifact, Manifest: manifestArtifact,
		Controllers: []installation.Controller{}, Grants: []installation.ObjectRef{}, LegacyPolicies: []installation.ObjectRef{},
		WorkloadNamespaceUIDs: map[string]string{},
	}
	operations := &installation.Kubernetes{Plan: plan, Manifest: manifest, Admin: admin, Discovery: discover}
	if err := operations.FreshWorkloadsEmpty(ctx, rendered, nil, "", true); err != nil {
		return "", err
	}
	if err := operations.WorkloadsDrained(ctx); err != nil {
		return "", err
	}
	if err := operations.VerifyReleaseObjects(ctx); err != nil {
		return "", err
	}
	for _, name := range append([]string{options.namespace, options.authority}, manifest.Namespaces...) {
		namespace := &corev1.Namespace{}
		err := admin.Get(ctx, client.ObjectKey{Name: name}, namespace)
		if apierrors.IsNotFound(err) {
			namespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
			err = admin.Create(ctx, namespace)
		}
		if err != nil {
			return "", err
		}
		if namespace.UID == "" || namespace.DeletionTimestamp != nil {
			return "", fmt.Errorf("namespace identity is unavailable or deleting: %s", name)
		}
		switch name {
		case options.namespace:
			plan.NamespaceUID = string(namespace.UID)
		case options.authority:
			plan.AuthorityNamespaceUID = string(namespace.UID)
		default:
			plan.WorkloadNamespaceUIDs[name] = string(namespace.UID)
		}
	}
	encoded, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return "", err
	}
	planArtifact, err := artifact("install.json", encoded)
	if err != nil {
		return "", err
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(state, filepath.Base(executable)), binary, 0700); err != nil {
		return "", err
	}
	_, err = fmt.Fprintf(stdout, "Installing new release %s into %s; recovery artifacts retained in %s. No providers or samples are provisioned.\n", options.release, options.namespace, state)
	return planArtifact.Path, err
}
