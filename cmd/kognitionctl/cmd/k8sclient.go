// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// k8sDynClient is the shared dynamic client for direct CRD operations.
// Initialized when mode == "kubernetes".
var k8sDynClient dynamic.Interface

// k8sCoreClient is the typed client used for core/v1 reads (e.g. Events).
var k8sCoreClient kubernetes.Interface

// k8sRESTConfig is the resolved rest.Config (used for port-forward/logs too).
var k8sRESTConfig *rest.Config

// kognitionGV is the GroupVersion for Kognition CRDs.
var kognitionGV = schema.GroupVersion{Group: "kognition.dev", Version: "v1alpha1"}

// initK8sClient builds a dynamic client from kubeconfig.
func initK8sClient() error {
	cfg, err := buildRESTConfig()
	if err != nil {
		return err
	}
	k8sRESTConfig = cfg

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}
	k8sDynClient = dyn

	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create core client: %w", err)
	}
	k8sCoreClient = core
	return nil
}

func k8sCurrentUsername(ctx context.Context) (string, error) {
	if k8sCoreClient == nil {
		return "", fmt.Errorf("kube client not initialized")
	}
	review, err := k8sCoreClient.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("resolve current Kubernetes user: %w", err)
	}
	if review.Status.UserInfo.Username == "" {
		return "", fmt.Errorf("resolve current Kubernetes user: API returned an empty username")
	}
	return review.Status.UserInfo.Username, nil
}

// buildRESTConfig resolves kubeconfig using standard client-go loading rules.
func buildRESTConfig() (*rest.Config, error) {
	loadingRules := &clientcmd.ClientConfigLoadingRules{
		ExplicitPath: kubeconfigPath(),
	}
	overrides := &clientcmd.ConfigOverrides{}
	if kubeContext != "" {
		overrides.CurrentContext = kubeContext
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("build kube config: %w", err)
	}
	return cfg, nil
}

// kubeconfigPath returns the kubeconfig file path.
func kubeconfigPath() string {
	if v := os.Getenv("KUBECONFIG"); v != "" {
		sep := ":"
		if runtime.GOOS == "windows" {
			sep = ";"
		}
		if strings.Contains(v, sep) {
			return strings.SplitN(v, sep, 2)[0]
		}
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kube", "config")
}

// k8sGet retrieves a single CRD resource and returns it as JSON bytes.
func k8sGet(ns, resource, name string) ([]byte, error) {
	plural := pluralize(resource)
	gvr := schema.GroupVersionResource{Group: kognitionGV.Group, Version: kognitionGV.Version, Resource: plural}
	obj, err := nsClientFor(ns, gvr).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s/%s: %w", plural, name, err)
	}
	return json.Marshal(obj.Object)
}

// k8sList lists CRD resources and returns them as JSON bytes (in standard list form).
func k8sList(ns, resource string) ([]byte, error) {
	plural := pluralize(resource)
	gvr := schema.GroupVersionResource{Group: kognitionGV.Group, Version: kognitionGV.Version, Resource: plural}
	list, err := nsClientFor(ns, gvr).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", plural, err)
	}
	return json.Marshal(list)
}

// k8sCreate creates a CRD resource from an unstructured map.
func k8sCreate(ns string, obj map[string]any) ([]byte, error) {
	u := &unstructured.Unstructured{Object: obj}
	gvr := gvrFromObject(u)
	// Cluster-scoped resources must not carry a metadata.namespace; strip it.
	if !k8sIsNamespaced(gvr.Resource) {
		if md, ok := obj["metadata"].(map[string]any); ok {
			delete(md, "namespace")
		}
	}
	result, err := nsClientFor(ns, gvr).Create(context.TODO(), u, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create: %w", err)
	}
	return json.Marshal(result.Object)
}

// k8sUpdate updates a CRD resource from an unstructured map (must have resourceVersion).
func k8sUpdate(ns string, obj map[string]any) ([]byte, error) {
	u := &unstructured.Unstructured{Object: obj}
	gvr := gvrFromObject(u)
	if !k8sIsNamespaced(gvr.Resource) {
		if md, ok := obj["metadata"].(map[string]any); ok {
			delete(md, "namespace")
		}
	}
	result, err := nsClientFor(ns, gvr).Update(context.TODO(), u, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	return json.Marshal(result.Object)
}

// k8sDelete deletes a named CRD resource.
func k8sDelete(ns, resource, name string) error {
	plural := pluralize(resource)
	gvr := schema.GroupVersionResource{Group: kognitionGV.Group, Version: kognitionGV.Version, Resource: plural}
	return nsClientFor(ns, gvr).Delete(context.TODO(), name, metav1.DeleteOptions{})
}

// resolveAgentBaseURL returns a base URL (like "http://localhost:PORT") that can
// reach the agent runtime pod via port-forwarding.
// If the cached port-forward is stale (pod replaced), it invalidates and retries once.
func resolveAgentBaseURL(ctx context.Context, namespace, agentName string) (string, error) {
	pf, err := getOrCreatePortForward(ctx, namespace, agentName)
	if err != nil {
		return "", fmt.Errorf("port-forward to agent %s/%s: %w", namespace, agentName, err)
	}
	// Quick health-check: dial the local port to verify the tunnel is alive.
	addr := fmt.Sprintf("localhost:%d", pf.LocalPort)
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		// Tunnel is dead (pod was likely replaced). Invalidate and retry once.
		invalidatePortForward(namespace, agentName)
		pf, err = getOrCreatePortForward(ctx, namespace, agentName)
		if err != nil {
			return "", fmt.Errorf("port-forward to agent %s/%s (retry): %w", namespace, agentName, err)
		}
	} else {
		_ = conn.Close()
	}
	return fmt.Sprintf("http://localhost:%d", pf.LocalPort), nil
}

func resolveAgentRuntimeAuthorization(ctx context.Context, namespace, agentName string) (string, error) {
	if k8sCoreClient == nil {
		return "", nil
	}
	selector := fmt.Sprintf("kognition.dev/agent-name=%s,kognition.dev/agent-namespace=%s,app.kubernetes.io/managed-by=kognition", agentName, namespace)
	pods, err := k8sCoreClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", fmt.Errorf("list agent pods: %w", err)
	}
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			for _, env := range container.Env {
				if env.Name != "KOGNITION_RUNTIME_AUTH_TOKEN" || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
					continue
				}
				ref := env.ValueFrom.SecretKeyRef
				secret, err := k8sCoreClient.CoreV1().Secrets(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
				if err != nil {
					return "", fmt.Errorf("read agent runtime authorization: %w", err)
				}
				token := secret.Data[ref.Key]
				if len(token) == 0 {
					return "", fmt.Errorf("agent runtime authorization is empty")
				}
				return "Bearer " + string(token), nil
			}
		}
	}
	return "", nil
}

// gvrFromObject extracts the GVR from an unstructured object's apiVersion+kind.
func gvrFromObject(u *unstructured.Unstructured) schema.GroupVersionResource {
	kind := strings.ToLower(u.GetKind())
	plural := pluralize(kind)
	return schema.GroupVersionResource{
		Group:    kognitionGV.Group,
		Version:  kognitionGV.Version,
		Resource: plural,
	}
}

var (
	k8sAvailableOnce   sync.Once
	k8sAvailablePlural map[string]bool
	k8sAvailableScope  map[string]bool // plural -> namespaced (true) / cluster-scoped (false)
	k8sAvailableErr    error
)

// k8sIsNamespaced reports whether the given plural Kognition resource is
// namespaced. Defaults to true when discovery is unavailable.
func k8sIsNamespaced(plural string) bool {
	if _, err := k8sAvailableKognitionResources(); err != nil {
		return true
	}
	if v, ok := k8sAvailableScope[plural]; ok {
		return v
	}
	return true
}

// nsClientFor returns the correct dynamic client for a GVR, honoring scope.
func nsClientFor(ns string, gvr schema.GroupVersionResource) dynamic.ResourceInterface {
	if k8sIsNamespaced(gvr.Resource) {
		return k8sDynClient.Resource(gvr).Namespace(ns)
	}
	return k8sDynClient.Resource(gvr)
}

// k8sListEvents returns Events for the given involved object, newest first.
func k8sListEvents(ctx context.Context, ns, kind, name string) ([]corev1.Event, error) {
	if k8sCoreClient == nil {
		return nil, fmt.Errorf("kube client not initialized")
	}
	fs := fmt.Sprintf("involvedObject.kind=%s,involvedObject.name=%s", kind, name)
	list, err := k8sCoreClient.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: fs})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool {
		return eventTime(items[i]).After(eventTime(items[j]))
	})
	return items, nil
}

// eventTime picks the most recent timestamp available on an Event.
func eventTime(e corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

// k8sListNamespaces returns the names of all namespaces in the cluster.
func k8sListNamespaces() ([]string, error) {
	if k8sDynClient == nil {
		return nil, fmt.Errorf("kube client not initialized")
	}
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
	list, err := k8sDynClient.Resource(gvr).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	out := make([]string, 0, len(list.Items))
	for _, ns := range list.Items {
		out = append(out, ns.GetName())
	}
	return out, nil
}

// k8sAvailableKognitionResources returns the set of plural resource names served
// by the Kognition API group in the current cluster (i.e. installed CRDs).
// Result is cached for the lifetime of the process. Returns nil + error if
// discovery fails.
func k8sAvailableKognitionResources() (map[string]bool, error) {
	k8sAvailableOnce.Do(func() {
		if k8sRESTConfig == nil {
			k8sAvailableErr = fmt.Errorf("kube client not initialized")
			return
		}
		dc, err := discovery.NewDiscoveryClientForConfig(k8sRESTConfig)
		if err != nil {
			k8sAvailableErr = fmt.Errorf("create discovery client: %w", err)
			return
		}
		rl, err := dc.ServerResourcesForGroupVersion(kognitionGV.String())
		if err != nil {
			k8sAvailableErr = fmt.Errorf("discover %s: %w", kognitionGV.String(), err)
			return
		}
		out := make(map[string]bool, len(rl.APIResources))
		scope := make(map[string]bool, len(rl.APIResources))
		for _, r := range rl.APIResources {
			// Skip subresources like "agents/status".
			if strings.Contains(r.Name, "/") {
				continue
			}
			out[r.Name] = true
			scope[r.Name] = r.Namespaced
		}
		k8sAvailablePlural = out
		k8sAvailableScope = scope
	})
	return k8sAvailablePlural, k8sAvailableErr
}
