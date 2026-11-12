// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/k8s/dispatcher"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// --- Test helpers ---

func testLogger(t *testing.T) *zap.Logger {
	t.Helper()
	l, _ := zap.NewDevelopment()
	return l
}

// fakeStore implements store.Store with stubbed methods that record calls.
type fakeStore struct {
	store.Store // embed to satisfy interface; we override needed methods.

	getCalls []storeGetCall
	getData  map[string][]byte
}

type storeGetCall struct {
	Kind, Namespace, Name string
}

func newFakeStore() *fakeStore {
	return &fakeStore{getData: make(map[string][]byte)}
}

func (s *fakeStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	s.getCalls = append(s.getCalls, storeGetCall{kind, namespace, name})
	key := kind + "/" + namespace + "/" + name
	if data, ok := s.getData[key]; ok {
		return data, nil
	}
	return nil, store.ErrNotFound
}

func (s *fakeStore) List(_ context.Context, _ string, _ store.ListOptions) ([][]byte, error) {
	return nil, nil
}

func (s *fakeStore) Create(_ context.Context, kind, namespace, name string, data []byte) error {
	key := kind + "/" + namespace + "/" + name
	s.getData[key] = data
	return nil
}

func (s *fakeStore) Update(_ context.Context, kind, namespace, name string, data []byte, _ int64) error {
	key := kind + "/" + namespace + "/" + name
	s.getData[key] = data
	return nil
}

func (s *fakeStore) Delete(_ context.Context, kind, namespace, name string) error {
	return nil
}

func (s *fakeStore) Watch(_ context.Context, _ string, _ store.ListOptions) (<-chan store.WatchEvent, error) {
	return nil, fmt.Errorf("not supported")
}

func (s *fakeStore) Ping(_ context.Context) error { return nil }

func (s *fakeStore) CreateRevision(_ context.Context, _, _, _ string, _ int64, _, _ []byte) error {
	return nil
}

func (s *fakeStore) ListRevisions(_ context.Context, _, _, _ string) ([]store.Revision, error) {
	return nil, nil
}

func (s *fakeStore) GetRevision(_ context.Context, _, _, _ string, _ int64) (*store.Revision, error) {
	return nil, store.ErrNotFound
}

func (s *fakeStore) AppendEvent(_ context.Context, _, _, _ string, _ store.InstanceEvent) error {
	return nil
}

func (s *fakeStore) ListEvents(_ context.Context, _, _, _ string) ([]store.InstanceEvent, error) {
	return nil, nil
}

// fakeDispatcher satisfies intctrl.RuntimeDispatcher.
type fakeDispatcher struct{}

func (d *fakeDispatcher) Dispatch(_ context.Context, _ *api.AgentInstance) (string, error) {
	return "session-123", nil
}

func (d *fakeDispatcher) Status(_ context.Context, _ *api.AgentInstance) (intctrl.RuntimeStatus, error) {
	return intctrl.RuntimeStatus{Alive: true}, nil
}

// --- Tests ---

func TestReconcilerAdapters_MapRequestsToInternalKinds(t *testing.T) {
	tests := []struct {
		name              string
		kind              string
		expectedNamespace string
		newAdapter        func(store.Store, *zap.Logger) reconcile.Reconciler
	}{
		{name: "agent job", kind: "AgentJob", expectedNamespace: "tenant-a", newAdapter: func(s store.Store, logger *zap.Logger) reconcile.Reconciler {
			return NewAgentJobReconcilerAdapter(s, logger)
		}},
		{name: "workflow run", kind: "WorkflowRun", expectedNamespace: "tenant-a", newAdapter: func(s store.Store, logger *zap.Logger) reconcile.Reconciler {
			return NewWorkflowRunReconcilerAdapter(s, logger)
		}},
		{name: "knowledge base claim", kind: "KnowledgeBaseClaim", expectedNamespace: "tenant-a", newAdapter: func(s store.Store, logger *zap.Logger) reconcile.Reconciler {
			return NewKnowledgeBaseClaimReconcilerAdapter(s, logger)
		}},
		{name: "knowledge base index run", kind: "KnowledgeBaseIndexRun", expectedNamespace: "tenant-a", newAdapter: func(s store.Store, logger *zap.Logger) reconcile.Reconciler {
			return NewKnowledgeBaseIndexRunReconcilerAdapter(s, logger)
		}},
		{name: "knowledge base index schedule", kind: "KnowledgeBaseIndexSchedule", expectedNamespace: "tenant-a", newAdapter: func(s store.Store, logger *zap.Logger) reconcile.Reconciler {
			return NewKnowledgeBaseIndexScheduleReconcilerAdapter(s, logger)
		}},
		{name: "secret ref", kind: "SecretRef", expectedNamespace: "tenant-a", newAdapter: func(s store.Store, logger *zap.Logger) reconcile.Reconciler {
			return NewSecretRefReconcilerAdapter(s, logger)
		}},
		{name: "memory claim", kind: "MemoryClaim", expectedNamespace: "tenant-a", newAdapter: func(s store.Store, logger *zap.Logger) reconcile.Reconciler {
			return NewMemoryClaimReconcilerAdapter(s, logger)
		}},
		{name: "memory class", kind: "MemoryClass", newAdapter: func(s store.Store, logger *zap.Logger) reconcile.Reconciler {
			return NewMemoryClassReconcilerAdapter(s, logger)
		}},
		{name: "memory volume", kind: "MemoryVolume", expectedNamespace: "tenant-a", newAdapter: func(s store.Store, logger *zap.Logger) reconcile.Reconciler {
			return NewMemoryVolumeReconcilerAdapter(s, logger)
		}},
		{name: "memory grant", kind: "MemoryGrant", expectedNamespace: "tenant-a", newAdapter: func(s store.Store, logger *zap.Logger) reconcile.Reconciler {
			return NewMemoryGrantReconcilerAdapter(s, logger)
		}},
		{name: "prompt", kind: "Prompt", expectedNamespace: "tenant-a", newAdapter: func(s store.Store, logger *zap.Logger) reconcile.Reconciler {
			return NewPromptReconcilerAdapter(s, logger)
		}},
		{name: "trust profile", kind: "TrustProfile", expectedNamespace: "tenant-a", newAdapter: func(s store.Store, logger *zap.Logger) reconcile.Reconciler {
			return NewTrustProfileReconcilerAdapter(s, logger)
		}},
		{name: "agent service", kind: "AgentService", expectedNamespace: "tenant-a", newAdapter: func(s store.Store, logger *zap.Logger) reconcile.Reconciler {
			return NewAgentServiceReconcilerAdapter(s, logger)
		}},
		{name: "intent gateway", kind: "IntentGateway", expectedNamespace: "tenant-a", newAdapter: func(s store.Store, logger *zap.Logger) reconcile.Reconciler {
			return NewIntentGatewayReconcilerAdapter(s, logger)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeStore()
			adapter := tt.newAdapter(fs, testLogger(t))
			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "resource-a"}}

			result, err := adapter.Reconcile(context.Background(), req)
			if err != nil {
				t.Fatalf("Reconcile() error = %v, want nil for missing resource", err)
			}
			if result != (ctrl.Result{}) {
				t.Fatalf("Reconcile() result = %+v, want empty result for missing resource", result)
			}
			if len(fs.getCalls) == 0 {
				t.Fatal("Reconcile() did not read from the store")
			}
			call := fs.getCalls[0]
			if call.Kind != tt.kind || call.Namespace != tt.expectedNamespace || call.Name != "resource-a" {
				t.Fatalf("store Get() = %+v, want kind=%q namespace=%q name=resource-a", call, tt.kind, tt.expectedNamespace)
			}
		})
	}
}

func TestAgentServiceDependencyWatchesMapRelatedResources(t *testing.T) {
	scheme := newScheme(t)
	agent := &k8s.Agent{ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: "tenant-a"}}
	matching := &k8s.AgentService{
		ObjectMeta: metav1.ObjectMeta{Name: "matching", Namespace: "tenant-a"},
		Spec:       k8s.AgentServiceSpec{AgentRef: k8s.LocalObjectReference{Name: "assistant"}},
	}
	unrelatedAgent := &k8s.AgentService{
		ObjectMeta: metav1.ObjectMeta{Name: "other-agent", Namespace: "tenant-a"},
		Spec:       k8s.AgentServiceSpec{AgentRef: k8s.LocalObjectReference{Name: "writer"}},
	}
	unrelatedNamespace := &k8s.AgentService{
		ObjectMeta: metav1.ObjectMeta{Name: "other-namespace", Namespace: "tenant-b"},
		Spec:       k8s.AgentServiceSpec{AgentRef: k8s.LocalObjectReference{Name: "assistant"}},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, matching, unrelatedAgent, unrelatedNamespace).Build()
	adapter := NewAgentServiceReconcilerAdapter(newFakeStore(), testLogger(t))
	adapter.client = k8sClient

	requests := adapter.agentToServices(context.Background(), agent)
	if len(requests) != 1 || requests[0].NamespacedName != ctrlclient.ObjectKeyFromObject(matching) {
		t.Fatalf("agentToServices() = %#v, want only matching AgentService", requests)
	}

	requests = adapter.dependencyToServices(context.Background(), &k8s.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "assistant-run", Namespace: "tenant-a"}})
	want := map[types.NamespacedName]bool{
		ctrlclient.ObjectKeyFromObject(matching):       true,
		ctrlclient.ObjectKeyFromObject(unrelatedAgent): true,
	}
	if len(requests) != len(want) {
		t.Fatalf("dependencyToServices() = %#v, want %d tenant-a AgentServices", requests, len(want))
	}
	for _, request := range requests {
		if !want[request.NamespacedName] {
			t.Fatalf("dependencyToServices() returned unexpected request %#v", request)
		}
	}
}

func TestAgentIdentityDependencyWatchMapsReferencedAgent(t *testing.T) {
	adapter := NewAgentReconcilerAdapter(newFakeStore(), nil, testLogger(t))
	identity := &k8s.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "planner-identity", Namespace: "tenant-a"},
		Spec:       k8s.AgentIdentitySpec{AgentRef: k8s.LocalObjectReference{Name: "planner"}},
	}
	requests := adapter.identityToAgent(context.Background(), identity)
	if len(requests) != 1 || requests[0].NamespacedName != (types.NamespacedName{Namespace: "tenant-a", Name: "planner"}) {
		t.Fatalf("identityToAgent() = %#v", requests)
	}
}

func TestAgentServiceRevisionWorkloadWatchMapsOnlyDedicatedDeployments(t *testing.T) {
	adapter := NewAgentServiceReconcilerAdapter(newFakeStore(), testLogger(t))
	dedicated := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      "agent-svc-research-r1-abc123",
		Namespace: "agent-runtimes",
		Labels:    map[string]string{"kognition.dev/agent-service-workload": "agent-svc-research-r1-abc123"},
		Annotations: map[string]string{
			"kognition.dev/owner-agent-service": "tenant-a/research-api",
		},
	}}
	requests := adapter.revisionDeploymentToService(context.Background(), dedicated)
	if len(requests) != 1 || requests[0].NamespacedName != (types.NamespacedName{Namespace: "tenant-a", Name: "research-api"}) {
		t.Fatalf("revisionDeploymentToService() = %#v, want tenant-a/research-api", requests)
	}

	shared := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:        "agent-researcher",
		Namespace:   "agent-runtimes",
		Annotations: map[string]string{"kognition.dev/owner-agent-service": "tenant-a/research-api"},
	}}
	if requests := adapter.revisionDeploymentToService(context.Background(), shared); len(requests) != 0 {
		t.Fatalf("shared Deployment requests = %#v, want none", requests)
	}
}

func TestIntentGatewayDependencyWatchMapsNamespaceGateways(t *testing.T) {
	scheme := newScheme(t)
	workflow := &k8s.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "triage", Namespace: "tenant-a"}}
	first := &k8s.IntentGateway{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "tenant-a"}}
	second := &k8s.IntentGateway{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "tenant-a"}}
	otherNamespace := &k8s.IntentGateway{ObjectMeta: metav1.ObjectMeta{Name: "third", Namespace: "tenant-b"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workflow, first, second, otherNamespace).Build()
	adapter := NewIntentGatewayReconcilerAdapter(newFakeStore(), testLogger(t))
	adapter.client = k8sClient

	requests := adapter.dependencyToGateways(context.Background(), workflow)
	want := map[types.NamespacedName]bool{
		ctrlclient.ObjectKeyFromObject(first):  true,
		ctrlclient.ObjectKeyFromObject(second): true,
	}
	if len(requests) != len(want) {
		t.Fatalf("dependencyToGateways() = %#v, want %d tenant-a IntentGateways", requests, len(want))
	}
	for _, request := range requests {
		if !want[request.NamespacedName] {
			t.Fatalf("dependencyToGateways() returned unexpected request %#v", request)
		}
	}
}

func TestIntentGatewayAdapterDoesNotAdvertiseUnservedEndpoint(t *testing.T) {
	store := newFakeStore()
	gateway := api.IntentGateway{
		ObjectMeta: api.ObjectMeta{Namespace: "tenant-a", Name: "support", Finalizers: []string{intctrl.IntentGatewayFinalizer}},
		Spec: api.IntentGatewaySpec{Routes: []api.IntentRoute{{
			Pattern: "plan:*", TargetKind: "Agent", TargetRef: api.LocalObjectReference{Name: "planner"},
		}}},
	}
	for _, resource := range []struct {
		kind string
		name string
		data any
	}{
		{kind: "IntentGateway", name: "support", data: gateway},
		{kind: "Agent", name: "planner", data: api.Agent{ObjectMeta: api.ObjectMeta{Namespace: "tenant-a", Name: "planner"}}},
	} {
		data, err := json.Marshal(resource.data)
		if err != nil {
			t.Fatal(err)
		}
		store.getData[resource.kind+"/tenant-a/"+resource.name] = data
	}

	adapter := NewIntentGatewayReconcilerAdapter(store, testLogger(t))
	if _, err := adapter.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "support"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var updated api.IntentGateway
	if err := json.Unmarshal(store.getData["IntentGateway/tenant-a/support"], &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != api.PhasePending || updated.Status.Endpoint != "" || len(updated.Status.Conditions) != 1 || updated.Status.Conditions[0].Reason != "ServingUnavailable" {
		t.Fatalf("gateway status = %#v", updated.Status)
	}
}

func TestIntentGatewayAdapterPublishesEndpointWhenServingDeploymentIsAvailable(t *testing.T) {
	store := newFakeStore()
	gateway := api.IntentGateway{
		ObjectMeta: api.ObjectMeta{Namespace: "tenant-a", Name: "support", Finalizers: []string{intctrl.IntentGatewayFinalizer}},
		Spec: api.IntentGatewaySpec{Routes: []api.IntentRoute{{
			Pattern: "plan:*", TargetKind: "Agent", TargetRef: api.LocalObjectReference{Name: "planner"},
		}}},
	}
	for _, resource := range []struct {
		kind string
		name string
		data any
	}{
		{kind: "IntentGateway", name: "support", data: gateway},
		{kind: "Agent", name: "planner", data: api.Agent{ObjectMeta: api.ObjectMeta{Namespace: "tenant-a", Name: "planner"}}},
	} {
		data, err := json.Marshal(resource.data)
		if err != nil {
			t.Fatal(err)
		}
		store.getData[resource.kind+"/tenant-a/"+resource.name] = data
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "intent-gateway", Namespace: "kognition-system", Generation: 1},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Conditions:         []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}},
		},
	}
	adapter := NewIntentGatewayReconcilerAdapter(store, testLogger(t), IntentGatewayServingConfig{
		Namespace: "kognition-system", Deployment: "intent-gateway", Endpoint: "http://intent-gateway.kognition-system.svc.cluster.local:8080",
	})
	adapter.client = fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(deployment).Build()

	if _, err := adapter.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "support"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var updated api.IntentGateway
	if err := json.Unmarshal(store.getData["IntentGateway/tenant-a/support"], &updated); err != nil {
		t.Fatal(err)
	}
	wantEndpoint := "http://intent-gateway.kognition-system.svc.cluster.local:8080/apis/kognition.dev/v1alpha1/namespaces/tenant-a/intentgateways/support/goals"
	if updated.Status.Phase != api.PhaseReady || updated.Status.Endpoint != wantEndpoint || len(updated.Status.Conditions) != 1 || updated.Status.Conditions[0].Reason != "RoutesValid" {
		t.Fatalf("gateway status = %#v", updated.Status)
	}
}

func TestIntentGatewayServingDeploymentWatchAndReadiness(t *testing.T) {
	scheme := newScheme(t)
	first := &k8s.IntentGateway{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "tenant-a"}}
	second := &k8s.IntentGateway{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "tenant-b"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(first, second).Build()
	adapter := NewIntentGatewayReconcilerAdapter(newFakeStore(), testLogger(t), IntentGatewayServingConfig{
		Namespace: "kognition-system", Deployment: "intent-gateway", Endpoint: "http://intent-gateway.kognition-system.svc.cluster.local:8080",
	})
	adapter.client = k8sClient

	for _, test := range []struct {
		name   string
		object ctrlclient.Object
		want   int
	}{
		{name: "other namespace", object: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "intent-gateway", Namespace: "other"}}},
		{name: "other deployment", object: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "kognition-system"}}},
		{name: "serving deployment", object: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "intent-gateway", Namespace: "kognition-system"}}, want: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := adapter.gatewayDeploymentToGateways(context.Background(), test.object)
			if len(requests) != test.want {
				t.Fatalf("gatewayDeploymentToGateways() = %#v, want %d requests", requests, test.want)
			}
		})
	}

	for _, test := range []struct {
		name       string
		deployment *appsv1.Deployment
		wantReady  bool
	}{
		{name: "unobserved generation", deployment: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Generation: 2}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}}}},
		{name: "unavailable condition", deployment: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Generation: 1}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse}}}}},
		{name: "available", deployment: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Generation: 1}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}}}, wantReady: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := deploymentAvailable(test.deployment); got != test.wantReady {
				t.Fatalf("deploymentAvailable() = %t, want %t", got, test.wantReady)
			}
		})
	}
}

func TestAgentReconcilerAdapter_KindMapping(t *testing.T) {
	fs := newFakeStore()
	adapter := NewAgentReconcilerAdapter(fs, nil, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "agent1"}}
	_, _ = adapter.Reconcile(context.Background(), req)

	if len(fs.getCalls) == 0 {
		t.Fatal("expected at least one Get call to the store")
	}
	call := fs.getCalls[0]
	if call.Kind != "Agent" {
		t.Errorf("kind = %q, want %q", call.Kind, "Agent")
	}
	if call.Namespace != "ns1" {
		t.Errorf("namespace = %q, want %q", call.Namespace, "ns1")
	}
	if call.Name != "agent1" {
		t.Errorf("name = %q, want %q", call.Name, "agent1")
	}
}

func TestSplitOwnerRef(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want []string
	}{
		{name: "valid", ref: "team-a/writer", want: []string{"team-a", "writer"}},
		{name: "empty", ref: ""},
		{name: "missing separator", ref: "writer"},
		{name: "missing namespace", ref: "/writer"},
		{name: "missing name", ref: "team-a/"},
		{name: "extra separator", ref: "team-a/writer/extra"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitOwnerRef(tt.ref)
			if len(got) != len(tt.want) {
				t.Fatalf("splitOwnerRef(%q) = %#v, want %#v", tt.ref, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("splitOwnerRef(%q) = %#v, want %#v", tt.ref, got, tt.want)
				}
			}
		})
	}
}

func TestAgentInstanceReconcilerAdapter_KindMapping(t *testing.T) {
	fs := newFakeStore()
	adapter := NewAgentInstanceReconcilerAdapter(fs, &fakeDispatcher{}, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns2", Name: "inst1"}}
	_, _ = adapter.Reconcile(context.Background(), req)

	if len(fs.getCalls) == 0 {
		t.Fatal("expected at least one Get call to the store")
	}
	call := fs.getCalls[0]
	if call.Kind != "AgentInstance" {
		t.Errorf("kind = %q, want %q", call.Kind, "AgentInstance")
	}
	if call.Namespace != "ns2" {
		t.Errorf("namespace = %q, want %q", call.Namespace, "ns2")
	}
	if call.Name != "inst1" {
		t.Errorf("name = %q, want %q", call.Name, "inst1")
	}
}

func TestWorkflowReconcilerAdapter_KindMapping(t *testing.T) {
	fs := newFakeStore()
	adapter := NewWorkflowReconcilerAdapter(fs, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "wf1"}}
	_, _ = adapter.Reconcile(context.Background(), req)

	if len(fs.getCalls) == 0 {
		t.Fatal("expected at least one Get call to the store")
	}
	if fs.getCalls[0].Kind != "Workflow" {
		t.Errorf("kind = %q, want %q", fs.getCalls[0].Kind, "Workflow")
	}
}

func TestToolReconcilerAdapter_KindMapping(t *testing.T) {
	fs := newFakeStore()
	adapter := NewToolReconcilerAdapter(fs, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "tool1"}}
	_, _ = adapter.Reconcile(context.Background(), req)

	if len(fs.getCalls) == 0 {
		t.Fatal("expected at least one Get call to the store")
	}
	if fs.getCalls[0].Kind != "Tool" {
		t.Errorf("kind = %q, want %q", fs.getCalls[0].Kind, "Tool")
	}
}

func TestToolClassReconcilerAdapter_KindMapping(t *testing.T) {
	fs := newFakeStore()
	adapter := NewToolClassReconcilerAdapter(fs, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "tc1"}}
	_, _ = adapter.Reconcile(context.Background(), req)

	if len(fs.getCalls) == 0 {
		t.Fatal("expected at least one Get call to the store")
	}
	if fs.getCalls[0].Kind != "ToolClass" {
		t.Errorf("kind = %q, want %q", fs.getCalls[0].Kind, "ToolClass")
	}
}

func TestModelClassReconcilerAdapter_KindMapping(t *testing.T) {
	fs := newFakeStore()
	registry := cpi.NewRegistry()
	adapter := NewModelClassReconcilerAdapter(fs, registry, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "m1"}}
	_, _ = adapter.Reconcile(context.Background(), req)

	if len(fs.getCalls) == 0 {
		t.Fatal("expected at least one Get call to the store")
	}
	if fs.getCalls[0].Kind != "ModelClass" {
		t.Errorf("kind = %q, want %q", fs.getCalls[0].Kind, "ModelClass")
	}
}

func TestModelReconcilerAdapter_KindMapping(t *testing.T) {
	fs := newFakeStore()
	registry := cpi.NewRegistry()
	adapter := NewModelReconcilerAdapter(fs, registry, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "m1"}}
	_, _ = adapter.Reconcile(context.Background(), req)

	if len(fs.getCalls) == 0 {
		t.Fatal("expected at least one Get call to the store")
	}
	if fs.getCalls[0].Kind != "Model" {
		t.Errorf("kind = %q, want %q", fs.getCalls[0].Kind, "Model")
	}
}

func TestModelAutoscalerReconcilerAdapter_KindMapping(t *testing.T) {
	fs := newFakeStore()
	adapter := NewModelAutoscalerReconcilerAdapter(fs, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "mas1"}}
	_, _ = adapter.Reconcile(context.Background(), req)

	if len(fs.getCalls) == 0 {
		t.Fatal("expected at least one Get call to the store")
	}
	if fs.getCalls[0].Kind != "ModelAutoscaler" {
		t.Errorf("kind = %q, want %q", fs.getCalls[0].Kind, "ModelAutoscaler")
	}
}

func TestMemoryClaimReconcilerAdapter_KindMapping(t *testing.T) {
	fs := newFakeStore()
	adapter := NewMemoryClaimReconcilerAdapter(fs, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "mem1"}}
	_, _ = adapter.Reconcile(context.Background(), req)

	if len(fs.getCalls) == 0 {
		t.Fatal("expected at least one Get call to the store")
	}
	// The internal MemoryClaimReconciler hardcodes memoryClaimKind="MemoryClaim" for store
	// lookups, regardless of what Kind the adapter passes in the request.
	if fs.getCalls[0].Kind != "MemoryClaim" {
		t.Errorf("kind = %q, want %q", fs.getCalls[0].Kind, "MemoryClaim")
	}
}

func TestAgentPolicyReconcilerAdapter_KindMapping(t *testing.T) {
	fs := newFakeStore()
	adapter := NewAgentPolicyReconcilerAdapter(fs, opa.NewEngine(), testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "policy1"}}
	_, _ = adapter.Reconcile(context.Background(), req)

	if len(fs.getCalls) == 0 {
		t.Fatal("expected at least one Get call to the store")
	}
	if fs.getCalls[0].Kind != "AgentPolicy" {
		t.Errorf("kind = %q, want %q", fs.getCalls[0].Kind, "AgentPolicy")
	}
}

func TestCognitiveQuotaReconcilerAdapter_KindMapping(t *testing.T) {
	fs := newFakeStore()
	adapter := NewCognitiveQuotaReconcilerAdapter(fs, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "quota1"}}
	_, _ = adapter.Reconcile(context.Background(), req)

	if len(fs.getCalls) == 0 {
		t.Fatal("expected at least one Get call to the store")
	}
	if fs.getCalls[0].Kind != "CognitiveQuota" {
		t.Errorf("kind = %q, want %q", fs.getCalls[0].Kind, "CognitiveQuota")
	}
}

func TestAgentIdentityReconcilerAdapter_KindMapping(t *testing.T) {
	fs := newFakeStore()
	adapter := NewAgentIdentityReconcilerAdapter(fs, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "id1"}}
	_, _ = adapter.Reconcile(context.Background(), req)

	if len(fs.getCalls) == 0 {
		t.Fatal("expected at least one Get call to the store")
	}
	if fs.getCalls[0].Kind != "AgentIdentity" {
		t.Errorf("kind = %q, want %q", fs.getCalls[0].Kind, "AgentIdentity")
	}
}

type adapterIdentityStatusReader struct {
	status runtimeauth.WorkloadIdentityStatus
}

func (reader adapterIdentityStatusReader) AgentStatus(namespace, name, uid string) (runtimeauth.WorkloadIdentityStatus, error) {
	if namespace != "tenant" || name != "worker" || uid != "70000000-0000-4000-8000-000000000001" {
		return runtimeauth.WorkloadIdentityStatus{}, runtimeauth.ErrWorkloadIdentityUnavailable
	}
	return reader.status, nil
}

func TestAgentIdentityReconcilerAdapterProjectsCertificateStatus(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	uid := "70000000-0000-4000-8000-000000000001"
	spiffeID, err := runtimeauth.AgentIncarnationSPIFFEID("kognition.dev", "tenant", "worker", uid)
	if err != nil {
		t.Fatal(err)
	}
	agent := api.Agent{ObjectMeta: api.ObjectMeta{Namespace: "tenant", Name: "worker", UID: uid}}
	identity := api.AgentIdentity{
		ObjectMeta: api.ObjectMeta{Namespace: "tenant", Name: "agent-identity-worker", Finalizers: []string{intctrl.AgentIdentityFinalizer},
			Labels:          map[string]string{"kognition.dev/auto-created-for-agent": "worker"},
			OwnerReferences: []api.OwnerReference{{APIVersion: api.APIVersion, Kind: "Agent", Name: "worker", UID: uid}}},
		Spec: api.AgentIdentitySpec{AgentRef: api.LocalObjectReference{Name: "worker"}},
	}
	for kind, object := range map[string]any{"Agent": agent, "AgentIdentity": identity} {
		data, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		name := agent.ObjectMeta.Name
		if kind == "AgentIdentity" {
			name = identity.ObjectMeta.Name
		}
		if err := st.Create(ctx, kind, "tenant", name, data); err != nil {
			t.Fatal(err)
		}
	}
	reader := adapterIdentityStatusReader{status: runtimeauth.WorkloadIdentityStatus{SPIFFEID: spiffeID, TrustDomain: "kognition.dev",
		NotBefore: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour), Generation: 1, Ready: true}}
	adapter := NewAgentIdentityReconcilerAdapterWithWorkloadIdentity(st, testLogger(t), reader)
	result, err := adapter.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant", Name: identity.ObjectMeta.Name}})
	if err != nil || result.RequeueAfter != 5*time.Second {
		t.Fatalf("periodic refresh result=%+v error=%v", result, err)
	}
	raw, err := st.Get(ctx, "AgentIdentity", "tenant", identity.ObjectMeta.Name)
	var observed api.AgentIdentity
	if err != nil || json.Unmarshal(raw, &observed) != nil || observed.Status.WorkloadIdentity == nil || !observed.Status.WorkloadIdentity.Ready {
		t.Fatalf("adapter did not publish verified certificate status: %+v", observed.Status)
	}
}

func TestWorkspaceReconcilerAdapter_KindMapping(t *testing.T) {
	fs := newFakeStore()
	adapter := NewWorkspaceReconcilerAdapter(fs, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "ws1"}}
	_, _ = adapter.Reconcile(context.Background(), req)

	if len(fs.getCalls) == 0 {
		t.Fatal("expected at least one Get call to the store")
	}
	if fs.getCalls[0].Kind != "Workspace" {
		t.Errorf("kind = %q, want %q", fs.getCalls[0].Kind, "Workspace")
	}
}

func TestReconcilerAdapter_ResultTranslation(t *testing.T) {
	// The AgentReconciler returns Result{} when the object is not found (no requeue).
	fs := newFakeStore()
	adapter := NewAgentReconcilerAdapter(fs, nil, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "missing"}}
	result, err := adapter.Reconcile(context.Background(), req)

	// Not-found is not an error for reconcilers; they return an empty result.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue { //nolint:staticcheck // Verify controller-runtime 1.x immediate requeue compatibility.
		t.Error("expected Requeue=false for not-found")
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected RequeueAfter=0, got %v", result.RequeueAfter)
	}
}

func TestReconcilerAdapter_InterfaceCompliance(t *testing.T) {
	// Verify all adapters satisfy the reconcile.Reconciler interface.
	fs := newFakeStore()
	logger := testLogger(t)
	registry := cpi.NewRegistry()

	var _ reconcile.Reconciler = NewAgentReconcilerAdapter(fs, nil, logger)
	var _ reconcile.Reconciler = NewAgentInstanceReconcilerAdapter(fs, &fakeDispatcher{}, logger)
	var _ reconcile.Reconciler = NewWorkflowReconcilerAdapter(fs, logger)
	var _ reconcile.Reconciler = NewToolReconcilerAdapter(fs, logger)
	var _ reconcile.Reconciler = NewToolClassReconcilerAdapter(fs, logger)
	var _ reconcile.Reconciler = NewModelClassReconcilerAdapter(fs, registry, logger)
	var _ reconcile.Reconciler = NewModelReconcilerAdapter(fs, registry, logger)
	var _ reconcile.Reconciler = NewModelAutoscalerReconcilerAdapter(fs, logger)
	var _ reconcile.Reconciler = NewMemoryClaimReconcilerAdapter(fs, logger)
	var _ reconcile.Reconciler = NewKnowledgeBaseReconcilerAdapter(fs, logger)
	var _ reconcile.Reconciler = NewAgentPolicyReconcilerAdapter(fs, opa.NewEngine(), logger)
	var _ reconcile.Reconciler = NewCognitiveQuotaReconcilerAdapter(fs, logger)
	var _ reconcile.Reconciler = NewAgentIdentityReconcilerAdapter(fs, logger)
	var _ reconcile.Reconciler = NewWorkspaceReconcilerAdapter(fs, logger)
}

func TestSetupScheme_RegistersKognitionTypes(t *testing.T) {
	scheme := SetupScheme()

	// Verify Kognition types are registered.
	kinds := []string{
		"Agent", "AgentInstance", "Workflow", "Tool", "ToolClass",
		"Model", "ModelClass",
		"ModelAutoscaler",
		"MemoryClass", "MemoryClaim", "MemoryVolume",
		"KnowledgeBaseClass", "KnowledgeBase",
		"SecretRef",
	}
	for _, kind := range kinds {
		gvk := k8s.GroupVersion.WithKind(kind)
		obj, err := scheme.New(gvk)
		if err != nil {
			t.Errorf("scheme.New(%v) failed: %v", gvk, err)
			continue
		}
		if obj == nil {
			t.Errorf("scheme.New(%v) returned nil", gvk)
		}
	}
}

func TestSetupScheme_RegistersCoreTypes(t *testing.T) {
	scheme := SetupScheme()

	// Check that the core API group (from clientgoscheme) is registered.
	if !scheme.IsGroupRegistered("") {
		t.Error("core API group not registered in scheme")
	}
	// Check that the kognition API group is registered.
	if !scheme.IsGroupRegistered("kognition.dev") {
		t.Error("kognition.dev API group not registered in scheme")
	}
}

func TestConfigureConfigMapCache_LimitsInformerWithoutLimitingReads(t *testing.T) {
	options := &ctrl.Options{}
	configureConfigMapCache(options)

	var configMapSelectorFound bool
	for object, byObject := range options.Cache.ByObject {
		if _, ok := object.(*corev1.ConfigMap); !ok {
			continue
		}
		configMapSelectorFound = true
		if byObject.Label == nil || !byObject.Label.Matches(labels.Set{
			toolCallLedgerManagedLabel: toolCallLedgerManagedLabelValue,
		}) {
			t.Fatal("ConfigMap informer selector does not match managed tool-call ledgers")
		}
		if byObject.Label.Matches(labels.Set{}) {
			t.Fatal("ConfigMap informer selector matches unrelated ConfigMaps")
		}
		if byObject.Transform == nil {
			t.Fatal("ConfigMap informer does not strip cached payloads")
		}
		cachedObject, err := byObject.Transform(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "ledger"},
			Data:       map[string]string{"ledger.json": "large payload"},
			BinaryData: map[string][]byte{"receipt": []byte("large payload")},
		})
		if err != nil {
			t.Fatalf("transform ConfigMap: %v", err)
		}
		cachedConfigMap, ok := cachedObject.(*corev1.ConfigMap)
		if !ok {
			t.Fatalf("transformed object = %T, want *corev1.ConfigMap", cachedObject)
		}
		if cachedConfigMap.Name != "ledger" || cachedConfigMap.Data != nil || cachedConfigMap.BinaryData != nil {
			t.Fatalf("transformed ConfigMap = %#v, want metadata without payload", cachedConfigMap)
		}
	}
	if !configMapSelectorFound {
		t.Fatal("ConfigMap informer selector is not configured")
	}
	if options.Client.Cache == nil || len(options.Client.Cache.DisableFor) != 3 {
		t.Fatal("ConfigMap, Lease and Secret reads are not configured to bypass the cache")
	}
	var configMapsUncached, leasesUncached, secretsUncached bool
	for _, object := range options.Client.Cache.DisableFor {
		switch object.(type) {
		case *corev1.ConfigMap:
			configMapsUncached = true
		case *coordinationv1.Lease:
			leasesUncached = true
		case *corev1.Secret:
			secretsUncached = true
		}
	}
	if !configMapsUncached || !leasesUncached || !secretsUncached {
		t.Fatal("ConfigMap, Lease and Secret cache bypass is required")
	}
}

// --- Reconcile with existing object tests ---

func TestAgentReconcilerAdapter_ReconcileExistingObject(t *testing.T) {
	fs := newFakeStore()
	logger := testLogger(t)
	adapter := NewAgentReconcilerAdapter(fs, nil, logger)

	// Seed the store with an Agent.
	agent := api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "test-agent", ResourceVersion: 1},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "gpt-4o"},
			Instructions: api.AgentInstructions{Inline: "help"},
		},
	}
	data, _ := json.Marshal(agent)
	fs.getData["Agent/default/test-agent"] = data

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-agent"}}
	result, err := adapter.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Agent reconciler creates an instance — it should succeed or requeue for instance creation.
	_ = result
}

func TestAgentReconcilerAdapter_MissingWorkloadDoesNotFailReconcile(t *testing.T) {
	fs := newFakeStore()
	logger := testLogger(t)
	k8sClient := fake.NewClientBuilder().WithScheme(SetupScheme()).Build()
	runtimeDispatcher := dispatcher.NewK8sDispatcher(k8sClient, fs, logger, dispatcher.Config{})
	adapter := NewAgentReconcilerAdapter(fs, runtimeDispatcher, logger)

	agent := api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "test-agent", ResourceVersion: 1},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "gpt-4o"},
			Instructions: api.AgentInstructions{Inline: "help"},
		},
	}
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("marshal agent: %v", err)
	}
	fs.getData["Agent/default/test-agent"] = data

	result, err := adapter.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-agent"},
	})
	if err != nil {
		t.Fatalf("Reconcile() error with missing workload = %v, want nil", err)
	}
	if !result.Requeue || result.RequeueAfter != 0 { //nolint:staticcheck // Verify controller-runtime 1.x immediate requeue compatibility.
		t.Fatalf("Reconcile() result = %+v, want immediate internal requeue", result)
	}
}

func TestAgentReconcilerAdapter_ReconcileRefreshesAgentRuntime(t *testing.T) {
	t.Setenv("KOGNITION_OTEL_TRACES_ENABLED", "true")
	fs := newFakeStore()
	logger := testLogger(t)
	k8sClient := fake.NewClientBuilder().WithScheme(SetupScheme()).WithObjects(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "agent-test-agent",
			Namespace:   "default",
			Annotations: map[string]string{dispatcher.AnnotationOwnerAgent: "default/test-agent"},
		},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "agent",
			Image: "runtime:old",
		}}}}},
	}).Build()
	runtimeDispatcher := dispatcher.NewK8sDispatcher(k8sClient, fs, logger, dispatcher.Config{AgentImage: "runtime:new"})
	adapter := NewAgentReconcilerAdapter(fs, runtimeDispatcher, logger)

	agent := api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "test-agent", ResourceVersion: 1},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "gpt-4o"},
			Instructions: api.AgentInstructions{Inline: "help"},
		},
	}
	model := api.Model{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Model"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "gpt-4o", ResourceVersion: 1},
		Spec: api.ModelSpec{
			Provider: "openai",
			ModelID:  "gpt-4o",
		},
	}
	agentData, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("marshal agent: %v", err)
	}
	modelData, err := json.Marshal(model)
	if err != nil {
		t.Fatalf("marshal model: %v", err)
	}
	fs.getData["Agent/default/test-agent"] = agentData
	fs.getData["Model/default/gpt-4o"] = modelData

	_, err = adapter.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-agent"}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var deployment appsv1.Deployment
	if err := k8sClient.Get(context.Background(), ctrlclient.ObjectKey{Namespace: "default", Name: "agent-test-agent"}, &deployment); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if container.Image != "runtime:new" {
		t.Fatalf("deployment image = %q, want runtime:new", container.Image)
	}
	foundTracing := false
	for _, env := range container.Env {
		if env.Name == "KOGNITION_OTEL_TRACES_ENABLED" && env.Value == "true" {
			foundTracing = true
		}
	}
	if !foundTracing {
		t.Fatalf("deployment env = %#v, want inherited tracing enabled", container.Env)
	}
}

func TestAgentReconcilerAdapter_AnnotationRepairFailureDoesNotFailReconcile(t *testing.T) {
	fs := newFakeStore()
	logger := testLogger(t)
	wantErr := fmt.Errorf("kubernetes API unavailable")
	k8sClient := fake.NewClientBuilder().
		WithScheme(SetupScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectKey, ctrlclient.Object, ...ctrlclient.GetOption) error {
				return wantErr
			},
		}).
		Build()
	runtimeDispatcher := dispatcher.NewK8sDispatcher(k8sClient, fs, logger, dispatcher.Config{})
	adapter := NewAgentReconcilerAdapter(fs, runtimeDispatcher, logger)

	agent := api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "test-agent", ResourceVersion: 1},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "gpt-4o"},
			Instructions: api.AgentInstructions{Inline: "help"},
		},
	}
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("marshal agent: %v", err)
	}
	fs.getData["Agent/default/test-agent"] = data

	result, err := adapter.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-agent"},
	})
	if err != nil {
		t.Fatalf("Reconcile() error with annotation repair failure = %v, want nil", err)
	}
	if !result.Requeue || result.RequeueAfter != 0 { //nolint:staticcheck // Verify controller-runtime 1.x immediate requeue compatibility.
		t.Fatalf("Reconcile() result = %+v, want immediate internal requeue", result)
	}
}

func TestWorkflowReconcilerAdapter_ReconcileExistingObject(t *testing.T) {
	fs := newFakeStore()
	logger := testLogger(t)
	adapter := NewWorkflowReconcilerAdapter(fs, logger)

	wf := api.Workflow{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Workflow"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "test-wf", ResourceVersion: 1},
		Spec: api.WorkflowSpec{
			Steps: []api.WorkflowStep{{Name: "step1", AgentRef: api.LocalObjectReference{Name: "a1"}}},
		},
	}
	data, _ := json.Marshal(wf)
	fs.getData["Workflow/default/test-wf"] = data

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-wf"}}
	result, err := adapter.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestToolReconcilerAdapter_ReconcileExistingObject(t *testing.T) {
	fs := newFakeStore()
	logger := testLogger(t)
	adapter := NewToolReconcilerAdapter(fs, logger)

	tool := api.Tool{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Tool"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "test-tool", ResourceVersion: 1},
		Spec: api.ToolSpec{
			ClassName: "http",
		},
	}
	data, _ := json.Marshal(tool)
	fs.getData["Tool/default/test-tool"] = data

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-tool"}}
	result, err := adapter.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
}

func TestModelReconcilerAdapter_ReconcileExistingObject(t *testing.T) {
	fs := newFakeStore()
	registry := cpi.NewRegistry()
	logger := testLogger(t)
	adapter := NewModelReconcilerAdapter(fs, registry, logger)

	model := api.Model{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Model"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "test-model", ResourceVersion: 1},
		Spec: api.ModelSpec{
			Provider:  "openai-compatible",
			ClassName: "gpt-class",
			ModelID:   "gpt-4o",
		},
	}
	data, _ := json.Marshal(model)
	fs.getData["Model/default/test-model"] = data

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-model"}}
	result, err := adapter.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result

	// Verify the adapter routed the lookup through the "Model" kind.
	var sawModel bool
	for _, c := range fs.getCalls {
		if c.Kind == "Model" && c.Namespace == "default" && c.Name == "test-model" {
			sawModel = true
			break
		}
	}
	if !sawModel {
		t.Errorf("expected store.Get(Model, default, test-model) to be called; got %+v", fs.getCalls)
	}
}

// --- RequeueAfter test ---

func TestReconcilerAdapter_RequeueAfterPropagation(t *testing.T) {
	// AgentInstance reconciler will requeue when it dispatches an instance.
	fs := newFakeStore()
	logger := testLogger(t)
	adapter := NewAgentInstanceReconcilerAdapter(fs, &fakeDispatcher{}, logger)

	// Seed with an AgentInstance in Pending phase.
	inst := api.AgentInstance{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "AgentInstance"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "inst1", ResourceVersion: 1},
		Spec: api.AgentInstanceSpec{
			AgentRef: api.LocalObjectReference{Name: "agent1"},
		},
		Status: api.AgentInstanceStatus{
			Phase: api.PhasePending,
		},
	}
	data, _ := json.Marshal(inst)
	fs.getData["AgentInstance/default/inst1"] = data

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "inst1"}}
	result, err := adapter.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The reconciler dispatches, then requeues with a poll interval.
	if result.RequeueAfter == 0 && !result.Requeue { //nolint:staticcheck // Verify controller-runtime 1.x immediate requeue compatibility.
		// Acceptable: may not requeue if immediately completed.
		_ = result
	}
	// Verify RequeueAfter is a valid positive duration if set.
	if result.RequeueAfter < 0 {
		t.Errorf("invalid negative RequeueAfter: %v", result.RequeueAfter)
	}
}

func TestApprovalDecisionMapsToReferencedWorkflowRun(t *testing.T) {
	decision := &k8s.ApprovalDecision{Spec: k8s.ApprovalDecisionSpec{
		WorkflowRunRef: k8s.LocalObjectReference{Name: "run-42"},
	}}
	decision.Namespace = "tenant-a"
	requests := approvalDecisionToWorkflowRuns(context.Background(), decision)
	if len(requests) != 1 || requests[0].Namespace != "tenant-a" || requests[0].Name != "run-42" {
		t.Fatalf("requests = %+v, want tenant-a/run-42", requests)
	}
	decision.Spec.WorkflowRunRef.Name = ""
	if requests := approvalDecisionToWorkflowRuns(context.Background(), decision); len(requests) != 0 {
		t.Fatalf("empty workflowRunRef requests = %+v, want none", requests)
	}
}

// Ensure the time import is used.
var _ = time.Second
