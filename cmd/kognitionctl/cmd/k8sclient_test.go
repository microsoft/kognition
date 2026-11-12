// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

// resetDiscoveryCache restores the package-level discovery state to zero values
// after the test, so subsequent tests are not contaminated by primed values.
func resetDiscoveryCache(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		k8sAvailableOnce = sync.Once{}
		k8sAvailablePlural = nil
		k8sAvailableScope = nil
		k8sAvailableErr = nil
		k8sDynClient = nil
		k8sCoreClient = nil
		k8sRESTConfig = nil
	})
}

// primeDiscoveryCache pre-populates the discovery cache so k8sIsNamespaced
// returns deterministic values without contacting a real cluster.
func primeDiscoveryCache(scope map[string]bool, err error) {
	k8sAvailableScope = scope
	k8sAvailablePlural = make(map[string]bool, len(scope))
	for k := range scope {
		k8sAvailablePlural[k] = true
	}
	k8sAvailableErr = err
	// Mark the sync.Once as consumed so k8sAvailableKognitionResources
	// returns the primed values instead of attempting real discovery.
	k8sAvailableOnce.Do(func() {})
}

func writeTestKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	contents := `apiVersion: v1
kind: Config
clusters:
- name: primary
  cluster:
    server: https://primary.example.test
    insecure-skip-tls-verify: true
- name: secondary
  cluster:
    server: https://secondary.example.test
    insecure-skip-tls-verify: true
contexts:
- name: primary
  context:
    cluster: primary
    user: test
- name: secondary
  context:
    cluster: secondary
    user: test
current-context: primary
users:
- name: test
  user:
    token: test-token
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func TestKubeconfigPath_UsesFirstExplicitPath(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first-config")
	second := filepath.Join(t.TempDir(), "second-config")
	separator := string(os.PathListSeparator)
	t.Setenv("KUBECONFIG", first+separator+second)

	if got := kubeconfigPath(); got != first {
		t.Fatalf("kubeconfigPath() = %q, want first path %q", got, first)
	}
}

func TestKubeconfigPath_DefaultsToUserHome(t *testing.T) {
	t.Setenv("KUBECONFIG", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home: %v", err)
	}
	if got, want := kubeconfigPath(), filepath.Join(home, ".kube", "config"); got != want {
		t.Fatalf("kubeconfigPath() = %q, want %q", got, want)
	}
}

func TestBuildRESTConfig_ContextOverrideAndErrors(t *testing.T) {
	path := writeTestKubeconfig(t)
	t.Setenv("KUBECONFIG", path)
	previousContext := kubeContext
	t.Cleanup(func() { kubeContext = previousContext })

	kubeContext = "secondary"
	config, err := buildRESTConfig()
	if err != nil {
		t.Fatalf("buildRESTConfig(): %v", err)
	}
	if config.Host != "https://secondary.example.test" || config.BearerToken != "test-token" {
		t.Fatalf("config = {Host:%q BearerToken:%q}, want secondary context", config.Host, config.BearerToken)
	}

	kubeContext = "missing"
	if _, err := buildRESTConfig(); err == nil || !strings.Contains(err.Error(), "build kube config") {
		t.Fatalf("buildRESTConfig() error = %v, want wrapped missing-context failure", err)
	}
}

func TestInitK8sClient_BuildsSharedClients(t *testing.T) {
	resetDiscoveryCache(t)
	path := writeTestKubeconfig(t)
	t.Setenv("KUBECONFIG", path)
	previousContext := kubeContext
	kubeContext = "primary"
	t.Cleanup(func() { kubeContext = previousContext })

	if err := initK8sClient(); err != nil {
		t.Fatalf("initK8sClient(): %v", err)
	}
	if k8sRESTConfig == nil || k8sRESTConfig.Host != "https://primary.example.test" {
		t.Fatalf("REST config = %#v, want primary cluster", k8sRESTConfig)
	}
	if k8sDynClient == nil || k8sCoreClient == nil {
		t.Fatalf("clients = dynamic:%v core:%v, want initialized", k8sDynClient, k8sCoreClient)
	}
}

func TestInitK8sClient_PropagatesKubeconfigFailure(t *testing.T) {
	resetDiscoveryCache(t)
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing-config"))
	previousContext := kubeContext
	kubeContext = ""
	t.Cleanup(func() { kubeContext = previousContext })

	if err := initK8sClient(); err == nil || !strings.Contains(err.Error(), "build kube config") {
		t.Fatalf("initK8sClient() error = %v, want wrapped kubeconfig failure", err)
	}
	if k8sRESTConfig != nil || k8sDynClient != nil || k8sCoreClient != nil {
		t.Fatalf("clients initialized after failure: config:%v dynamic:%v core:%v", k8sRESTConfig, k8sDynClient, k8sCoreClient)
	}
}

func TestK8sIsNamespaced_NamespacedResource(t *testing.T) {
	resetDiscoveryCache(t)
	primeDiscoveryCache(map[string]bool{"agents": true}, nil)

	if !k8sIsNamespaced("agents") {
		t.Errorf("k8sIsNamespaced(agents) = false, want true")
	}
}

func TestK8sIsNamespaced_ClusterScopedResource(t *testing.T) {
	resetDiscoveryCache(t)
	primeDiscoveryCache(map[string]bool{"modelclasses": false}, nil)

	if k8sIsNamespaced("modelclasses") {
		t.Errorf("k8sIsNamespaced(modelclasses) = true, want false")
	}
}

func TestK8sIsNamespaced_UnknownResourceDefaultsToNamespaced(t *testing.T) {
	resetDiscoveryCache(t)
	primeDiscoveryCache(map[string]bool{"agents": true}, nil)

	if !k8sIsNamespaced("nonexistent") {
		t.Errorf("k8sIsNamespaced(nonexistent) = false, want true (default)")
	}
}

func TestK8sIsNamespaced_DiscoveryErrorDefaultsToNamespaced(t *testing.T) {
	resetDiscoveryCache(t)
	// Even when the cached scope says cluster-scoped, a discovery error
	// forces the safe default of namespaced.
	primeDiscoveryCache(map[string]bool{"modelclasses": false}, fmt.Errorf("discovery offline"))

	if !k8sIsNamespaced("modelclasses") {
		t.Errorf("k8sIsNamespaced(modelclasses) with discovery error = false, want true")
	}
}

func TestNsClientFor_NamespacedResource(t *testing.T) {
	resetDiscoveryCache(t)
	primeDiscoveryCache(map[string]bool{"agents": true}, nil)

	agentsGVR := schema.GroupVersionResource{Group: "kognition.dev", Version: "v1alpha1", Resource: "agents"}
	agent := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Agent",
		"metadata": map[string]any{
			"namespace": "team-a",
			"name":      "my-agent",
		},
	}}

	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(agentsGVR.GroupVersion().WithKind("Agent"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(agentsGVR.GroupVersion().WithKind("AgentList"), &unstructured.UnstructuredList{})

	listKinds := map[schema.GroupVersionResource]string{agentsGVR: "AgentList"}
	k8sDynClient = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, agent)

	got, err := nsClientFor("team-a", agentsGVR).Get(context.TODO(), "my-agent", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get from correct namespace: %v", err)
	}
	if got.GetName() != "my-agent" {
		t.Errorf("got name = %q, want my-agent", got.GetName())
	}

	// A different namespace must not see the object — proves the client is namespace-scoped.
	if _, err := nsClientFor("team-b", agentsGVR).Get(context.TODO(), "my-agent", metav1.GetOptions{}); err == nil {
		t.Errorf("get from wrong namespace: want NotFound error, got nil")
	}
}

func TestNsClientFor_ClusterScopedResource(t *testing.T) {
	resetDiscoveryCache(t)
	primeDiscoveryCache(map[string]bool{"modelclasses": false}, nil)

	mcGVR := schema.GroupVersionResource{Group: "kognition.dev", Version: "v1alpha1", Resource: "modelclasses"}
	mc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "ModelClass",
		"metadata": map[string]any{
			"name": "openai-gpt",
		},
	}}

	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(mcGVR.GroupVersion().WithKind("ModelClass"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(mcGVR.GroupVersion().WithKind("ModelClassList"), &unstructured.UnstructuredList{})

	listKinds := map[schema.GroupVersionResource]string{mcGVR: "ModelClassList"}
	k8sDynClient = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, mc)

	// Cluster-scoped resources ignore the namespace argument.
	got, err := nsClientFor("ignored-ns", mcGVR).Get(context.TODO(), "openai-gpt", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster-scoped get: %v", err)
	}
	if got.GetName() != "openai-gpt" {
		t.Errorf("got name = %q, want openai-gpt", got.GetName())
	}

	// And a different "namespace" string still finds the same cluster-scoped object.
	got2, err := nsClientFor("another-ns", mcGVR).Get(context.TODO(), "openai-gpt", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("cluster-scoped get with different ns arg: %v", err)
	}
	if got2.GetName() != "openai-gpt" {
		t.Errorf("got name = %q, want openai-gpt", got2.GetName())
	}
}

func TestK8sCRUD_NamespacedResourceLifecycle(t *testing.T) {
	resetDiscoveryCache(t)
	primeDiscoveryCache(map[string]bool{"agents": true}, nil)

	agentsGVR := schema.GroupVersionResource{Group: "kognition.dev", Version: "v1alpha1", Resource: "agents"}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(agentsGVR.GroupVersion().WithKind("Agent"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(agentsGVR.GroupVersion().WithKind("AgentList"), &unstructured.UnstructuredList{})
	k8sDynClient = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{agentsGVR: "AgentList"},
	)

	agent := map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Agent",
		"metadata": map[string]any{
			"name":      "assistant",
			"namespace": "team-a",
		},
		"spec": map[string]any{"instructions": "be concise"},
	}
	createdJSON, err := k8sCreate("team-a", agent)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	var created unstructured.Unstructured
	if err := json.Unmarshal(createdJSON, &created.Object); err != nil {
		t.Fatal(err)
	}
	if created.GetName() != "assistant" || created.GetNamespace() != "team-a" {
		t.Errorf("created metadata = %s/%s", created.GetNamespace(), created.GetName())
	}

	getJSON, err := k8sGet("team-a", "Agent", "assistant")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if !strings.Contains(string(getJSON), `"instructions":"be concise"`) {
		t.Errorf("get response = %s", getJSON)
	}

	listJSON, err := k8sList("team-a", "Agent")
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	var list unstructured.UnstructuredList
	if err := json.Unmarshal(listJSON, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].GetName() != "assistant" {
		t.Errorf("listed agents = %#v", list.Items)
	}

	agent["spec"].(map[string]any)["instructions"] = "be precise"
	updatedJSON, err := k8sUpdate("team-a", agent)
	if err != nil {
		t.Fatalf("update agent: %v", err)
	}
	if !strings.Contains(string(updatedJSON), `"instructions":"be precise"`) {
		t.Errorf("update response = %s", updatedJSON)
	}

	if err := k8sDelete("team-a", "Agent", "assistant"); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	if _, err := k8sGet("team-a", "Agent", "assistant"); err == nil {
		t.Fatal("get deleted agent: want error, got nil")
	}
}

func TestK8sCreateAndUpdate_StripNamespaceForClusterScopedResource(t *testing.T) {
	resetDiscoveryCache(t)
	primeDiscoveryCache(map[string]bool{"modelclasses": false}, nil)

	modelClassesGVR := schema.GroupVersionResource{Group: "kognition.dev", Version: "v1alpha1", Resource: "modelclasses"}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(modelClassesGVR.GroupVersion().WithKind("ModelClass"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(modelClassesGVR.GroupVersion().WithKind("ModelClassList"), &unstructured.UnstructuredList{})
	k8sDynClient = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{modelClassesGVR: "ModelClassList"},
	)

	modelClass := map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "ModelClass",
		"metadata": map[string]any{
			"name":            "fast-model",
			"namespace":       "must-be-removed",
			"resourceVersion": "1",
		},
	}
	createdJSON, err := k8sCreate("ignored", modelClass)
	if err != nil {
		t.Fatalf("create model class: %v", err)
	}
	if strings.Contains(string(createdJSON), "namespace") {
		t.Errorf("create response retained namespace: %s", createdJSON)
	}
	modelClass["metadata"].(map[string]any)["namespace"] = "must-also-be-removed"
	updatedJSON, err := k8sUpdate("ignored", modelClass)
	if err != nil {
		t.Fatalf("update model class: %v", err)
	}
	if strings.Contains(string(updatedJSON), "namespace") {
		t.Errorf("update response retained namespace: %s", updatedJSON)
	}
}

func TestK8sCRUD_ErrorsIncludeOperationContext(t *testing.T) {
	resetDiscoveryCache(t)
	primeDiscoveryCache(map[string]bool{"agents": true}, nil)
	agentsGVR := schema.GroupVersionResource{Group: "kognition.dev", Version: "v1alpha1", Resource: "agents"}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(agentsGVR.GroupVersion().WithKind("Agent"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(agentsGVR.GroupVersion().WithKind("AgentList"), &unstructured.UnstructuredList{})
	k8sDynClient = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{agentsGVR: "AgentList"},
	)

	if _, err := k8sGet("team-a", "Agent", "missing"); err == nil || !strings.Contains(err.Error(), "get agents/missing") || !apierrors.IsNotFound(err) {
		t.Errorf("get error = %v", err)
	}

	fakeClient := k8sDynClient.(*dynamicfake.FakeDynamicClient)
	fakeClient.PrependReactor("list", "agents", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("backend offline")
	})
	if _, err := k8sList("team-a", "Agent"); err == nil || !strings.Contains(err.Error(), "list agents: backend offline") {
		t.Errorf("list error = %v", err)
	}

	invalid := map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Agent",
		"metadata":   map[string]any{"name": "broken", "namespace": "team-a"},
	}
	fakeClient.PrependReactor("create", "agents", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("create denied")
	})
	if _, err := k8sCreate("team-a", invalid); err == nil || err.Error() != "create: create denied" {
		t.Errorf("create error = %v", err)
	}
	fakeClient.PrependReactor("update", "agents", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("conflict")
	})
	if _, err := k8sUpdate("team-a", invalid); err == nil || err.Error() != "update: conflict" {
		t.Errorf("update error = %v", err)
	}
}

func TestK8sListNamespaces_InitializationAndResults(t *testing.T) {
	resetDiscoveryCache(t)
	if _, err := k8sListNamespaces(); err == nil || err.Error() != "kube client not initialized" {
		t.Errorf("uninitialized error = %v", err)
	}

	namespacesGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(namespacesGVR.GroupVersion().WithKind("Namespace"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(namespacesGVR.GroupVersion().WithKind("NamespaceList"), &unstructured.UnstructuredList{})
	one := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "team-a"}}}
	two := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "team-b"}}}
	k8sDynClient = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{namespacesGVR: "NamespaceList"},
		one,
		two,
	)
	names, err := k8sListNamespaces()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "team-a,team-b" {
		t.Errorf("namespaces = %v", names)
	}

	fakeClient := k8sDynClient.(*dynamicfake.FakeDynamicClient)
	fakeClient.PrependReactor("list", "namespaces", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("permission denied")
	})
	if _, err := k8sListNamespaces(); err == nil || err.Error() != "list namespaces: permission denied" {
		t.Errorf("list error = %v", err)
	}
}

func TestK8sListEvents_SortsNewestAndUsesTimestampPrecedence(t *testing.T) {
	oldest := metav1.NewTime(time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC))
	middle := metav1.NewMicroTime(time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC))
	newest := metav1.NewTime(time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC))
	k8sCoreClient = fake.NewSimpleClientset(
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "created", Namespace: "team-a", CreationTimestamp: oldest}, InvolvedObject: corev1.ObjectReference{Kind: "Agent", Name: "assistant"}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "event-time", Namespace: "team-a"}, EventTime: middle, InvolvedObject: corev1.ObjectReference{Kind: "Agent", Name: "assistant"}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "last-time", Namespace: "team-a"}, LastTimestamp: newest, EventTime: metav1.NewMicroTime(oldest.Time), InvolvedObject: corev1.ObjectReference{Kind: "Agent", Name: "assistant"}},
	)
	t.Cleanup(func() { k8sCoreClient = nil })

	events, err := k8sListEvents(context.Background(), "team-a", "Agent", "assistant")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{events[0].Name, events[1].Name, events[2].Name}
	if strings.Join(got, ",") != "last-time,event-time,created" {
		t.Errorf("events = %v", got)
	}
	if !eventTime(events[0]).Equal(newest.Time) {
		t.Errorf("eventTime(last-time) = %s", eventTime(events[0]))
	}

	k8sCoreClient.(*fake.Clientset).PrependReactor("list", "events", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("events unavailable")
	})
	if _, err := k8sListEvents(context.Background(), "team-a", "Agent", "assistant"); err == nil || err.Error() != "list events: events unavailable" {
		t.Errorf("list error = %v", err)
	}
}

func TestK8sListEvents_RequiresInitializedClient(t *testing.T) {
	previous := k8sCoreClient
	k8sCoreClient = nil
	t.Cleanup(func() { k8sCoreClient = previous })

	if _, err := k8sListEvents(context.Background(), "team-a", "Agent", "assistant"); err == nil || err.Error() != "kube client not initialized" {
		t.Errorf("error = %v", err)
	}
}

func TestK8sAvailableKognitionResources_DiscoversScopeAndSkipsSubresources(t *testing.T) {
	resetDiscoveryCache(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "kind":"APIResourceList",
  "apiVersion":"v1",
  "groupVersion":"kognition.dev/v1alpha1",
  "resources":[
    {"name":"agents","singularName":"agent","namespaced":true,"kind":"Agent","verbs":["get","list"]},
    {"name":"agents/status","singularName":"","namespaced":true,"kind":"Agent","verbs":["get","update"]},
    {"name":"modelclasses","singularName":"modelclass","namespaced":false,"kind":"ModelClass","verbs":["get","list"]}
  ]
}`))
	}))
	defer server.Close()
	k8sRESTConfig = &rest.Config{Host: server.URL}

	resources, err := k8sAvailableKognitionResources()
	if err != nil {
		t.Fatal(err)
	}
	if !resources["agents"] || !resources["modelclasses"] || resources["agents/status"] {
		t.Errorf("resources = %#v", resources)
	}
	if !k8sAvailableScope["agents"] || k8sAvailableScope["modelclasses"] {
		t.Errorf("scope = %#v", k8sAvailableScope)
	}
}
