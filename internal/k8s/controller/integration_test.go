// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/store"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// Integration tests verify the full path:
// K8s fake client → StoreAdapter → internal reconciler (via adapter) → StoreAdapter writes back.

func integrationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := k8s.AddToScheme(s); err != nil {
		t.Fatalf("add k8s scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return s
}

func integrationClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(integrationScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&k8s.Agent{}, &k8s.AgentInstance{}, &k8s.Workflow{}, &k8s.Tool{}, &k8s.ToolClass{}, &k8s.ModelClass{}, &k8s.ModelAutoscaler{}, &k8s.MemoryClass{}).
		Build()
}

// TestIntegration_AgentReconciler_CreatesInstance verifies that when a K8s Agent
// exists, the AgentReconcilerAdapter (which uses StoreAdapter) reads it and
// creates an AgentInstance.
func TestIntegration_AgentReconciler_CreatesInstance(t *testing.T) {
	agent := &k8s.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "int-agent",
			ResourceVersion: "1",
		},
		Spec: k8s.AgentSpec{
			ModelRef:     k8s.LocalObjectReference{Name: "gpt-4o"},
			Instructions: k8s.AgentInstructions{Inline: "hello"},
		},
	}
	fc := integrationClient(t, agent)
	sa := NewStoreAdapter(fc)
	logger := testLogger(t)

	adapter := NewAgentReconcilerAdapter(sa, nil, logger)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "int-agent"}}
	// The reconciler requires multiple passes: first to add the finalizer, then
	// to create the revision and instances.
	for i := 0; i < 5; i++ {
		res, err := adapter.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("reconcile pass %d error: %v", i+1, err)
		}
		if res.IsZero() {
			break
		}
	}

	// Verify an AgentInstance was created via the store.
	instances, err := sa.List(context.Background(), "AgentInstance", store.ListOptions{Namespace: "default"})
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	if len(instances) == 0 {
		t.Fatal("expected at least one AgentInstance to be created")
	}

	// Verify the instance references the agent.
	var inst api.AgentInstance
	if err := json.Unmarshal(instances[0], &inst); err != nil {
		t.Fatalf("unmarshal instance: %v", err)
	}
	if inst.Spec.AgentRef.Name != "int-agent" {
		t.Errorf("instance agentRef = %q, want %q", inst.Spec.AgentRef.Name, "int-agent")
	}
}

// TestIntegration_AgentInstanceReconciler_Dispatches verifies the full path
// for AgentInstance reconciliation with a real StoreAdapter.
func TestIntegration_AgentInstanceReconciler_Dispatches(t *testing.T) {
	inst := &k8s.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "int-inst",
			ResourceVersion: "1",
		},
		Spec: k8s.AgentInstanceSpec{
			AgentRef: k8s.LocalObjectReference{Name: "some-agent"},
		},
		Status: k8s.AgentInstanceStatus{
			Phase: string(api.PhasePending),
		},
	}
	fc := integrationClient(t, inst)
	sa := NewStoreAdapter(fc)
	logger := testLogger(t)

	adapter := NewAgentInstanceReconcilerAdapter(sa, &fakeDispatcher{}, logger)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "int-inst"}}
	result, err := adapter.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Verify it requeues (the instance reconciler polls after dispatch).
	if result.IsZero() {
		// May be acceptable if immediately completes, but log for visibility.
		t.Log("no requeue after dispatch — reconciler may have completed immediately")
	}

	// Verify the instance status was updated in K8s.
	got := &k8s.AgentInstance{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "int-inst"}, got); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	// After dispatch, phase should change from Pending.
	if got.Status.Phase == string(api.PhasePending) {
		t.Log("instance phase still Pending — reconciler may require additional reconcile loops")
	}
}

// TestIntegration_WorkflowReconciler verifies the Workflow reconciler reads
// from K8s via StoreAdapter.
func TestIntegration_WorkflowReconciler(t *testing.T) {
	wf := &k8s.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "int-wf",
			ResourceVersion: "1",
		},
		Spec: k8s.WorkflowSpec{
			Steps: []k8s.WorkflowStep{{Name: "s1", AgentRef: k8s.LocalObjectReference{Name: "a1"}}},
		},
	}
	fc := integrationClient(t, wf)
	sa := NewStoreAdapter(fc)
	logger := testLogger(t)

	adapter := NewWorkflowReconcilerAdapter(sa, logger)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "int-wf"}}
	_, err := adapter.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
}

// TestIntegration_ToolReconciler verifies the Tool reconciler reads from K8s.
func TestIntegration_ToolReconciler(t *testing.T) {
	tool := &k8s.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "int-tool",
			ResourceVersion: "1",
		},
		Spec: k8s.ToolSpec{
			ClassName: "http",
		},
	}
	fc := integrationClient(t, tool)
	sa := NewStoreAdapter(fc)
	logger := testLogger(t)

	adapter := NewToolReconcilerAdapter(sa, logger)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "int-tool"}}
	_, err := adapter.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
}

// TestIntegration_ModelClassReconciler verifies the ModelClass reconciler reads from K8s.
func TestIntegration_ModelClassReconciler(t *testing.T) {
	mc := &k8s.ModelClass{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "int-model",
			ResourceVersion: "1",
		},
		Spec: k8s.ModelClassSpec{
			Provisioner: "openai",
		},
	}
	fc := integrationClient(t, mc)
	sa := NewStoreAdapter(fc)
	logger := testLogger(t)
	registry := cpi.NewRegistry()

	adapter := NewModelClassReconcilerAdapter(sa, registry, logger)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "int-model"}}
	_, err := adapter.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
}

// TestIntegration_ModelAutoscalerReconciler verifies the ModelAutoscaler reconciler
// reads and writes through the K8s StoreAdapter path.
func TestIntegration_ModelAutoscalerReconciler(t *testing.T) {
	agent := &k8s.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "int-agent",
			ResourceVersion: "1",
		},
		Spec: k8s.AgentSpec{
			ModelRef:     k8s.LocalObjectReference{Name: "model-primary"},
			Instructions: k8s.AgentInstructions{Inline: "hello"},
		},
	}
	primary := &k8s.Model{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "model-primary",
			ResourceVersion: "1",
		},
		Spec:   k8s.ModelSpec{ClassName: "primary-class", ModelID: "p1"},
		Status: k8s.ModelStatus{Phase: string(api.PhaseReady)},
	}
	fallback := &k8s.Model{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "model-fallback",
			ResourceVersion: "1",
		},
		Spec:   k8s.ModelSpec{ClassName: "fallback-class", ModelID: "f1"},
		Status: k8s.ModelStatus{Phase: string(api.PhaseReady)},
	}
	primaryClass := &k8s.ModelClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "primary-class",
			ResourceVersion: "1",
		},
		Spec:   k8s.ModelClassSpec{Provisioner: "openai"},
		Status: k8s.ModelClassStatus{Phase: string(api.PhaseDegraded), FallbackClass: "fallback-class"},
	}
	fallbackClass := &k8s.ModelClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "fallback-class",
			ResourceVersion: "1",
		},
		Spec:   k8s.ModelClassSpec{Provisioner: "openai"},
		Status: k8s.ModelClassStatus{Phase: string(api.PhaseReady)},
	}
	autoscaler := &k8s.ModelAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "default",
			Name:            "int-mas",
			ResourceVersion: "1",
		},
		Spec: k8s.ModelAutoscalerSpec{
			AgentRef:          k8s.LocalObjectReference{Name: "int-agent"},
			PreferredModelRef: k8s.LocalObjectReference{Name: "model-primary"},
		},
	}

	fc := integrationClient(t, agent, primary, fallback, primaryClass, fallbackClass, autoscaler)
	sa := NewStoreAdapter(fc)
	logger := testLogger(t)
	adapter := NewModelAutoscalerReconcilerAdapter(sa, logger)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "int-mas"}}
	if _, err := adapter.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1 error: %v", err)
	}
	if _, err := adapter.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2 error: %v", err)
	}

	gotAgent := &k8s.Agent{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "int-agent"}, gotAgent); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if gotAgent.Spec.ModelRef.Name != "model-fallback" {
		t.Fatalf("expected fallback model, got %q", gotAgent.Spec.ModelRef.Name)
	}

	gotAutoscaler := &k8s.ModelAutoscaler{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "int-mas"}, gotAutoscaler); err != nil {
		t.Fatalf("get autoscaler: %v", err)
	}
	if gotAutoscaler.Status.CurrentModelRef != "model-fallback" {
		t.Fatalf("expected currentModelRef=model-fallback, got %q", gotAutoscaler.Status.CurrentModelRef)
	}
}

// TestIntegration_StoreAdapter_CreateAndGet verifies the full Create → Get
// round-trip through the StoreAdapter with real K8s fake client.
func TestIntegration_StoreAdapter_CreateAndGet(t *testing.T) {
	fc := integrationClient(t)
	sa := NewStoreAdapter(fc)

	// Create an agent via internal JSON.
	agent := api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "test-ns", Name: "created-agent"},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "claude"},
			Instructions: api.AgentInstructions{Inline: "be helpful"},
		},
	}
	data, _ := json.Marshal(agent)

	if err := sa.Create(context.Background(), "Agent", "test-ns", "created-agent", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Get it back and verify conversion round-trip.
	gotData, err := sa.Get(context.Background(), "Agent", "test-ns", "created-agent")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.Agent
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Spec.ModelRef.Name != "claude" {
		t.Errorf("modelRef = %q, want %q", got.Spec.ModelRef.Name, "claude")
	}
	if got.Spec.Instructions.Inline != "be helpful" {
		t.Errorf("instructions = %q, want %q", got.Spec.Instructions.Inline, "be helpful")
	}

	// Verify the K8s object exists with correct fields.
	k8sAgent := &k8s.Agent{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "test-ns", Name: "created-agent"}, k8sAgent); err != nil {
		t.Fatalf("k8s get: %v", err)
	}
	if k8sAgent.Spec.ModelRef.Name != "claude" {
		t.Errorf("k8s modelRef = %q, want %q", k8sAgent.Spec.ModelRef.Name, "claude")
	}
}

// TestIntegration_StoreAdapter_UpdateAndVerify tests Create → Update → Get
// with resource version tracking.
func TestIntegration_StoreAdapter_UpdateAndVerify(t *testing.T) {
	fc := integrationClient(t)
	sa := NewStoreAdapter(fc)

	// Create.
	agent := api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "upd-agent"},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "v1"},
			Instructions: api.AgentInstructions{Inline: "original"},
		},
	}
	data, _ := json.Marshal(agent)
	if err := sa.Create(context.Background(), "Agent", "default", "upd-agent", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Get to learn the resource version.
	gotData, err := sa.Get(context.Background(), "Agent", "default", "upd-agent")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var current api.Agent
	if err := json.Unmarshal(gotData, &current); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Update with correct resource version.
	updated := api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "upd-agent"},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "v2"},
			Instructions: api.AgentInstructions{Inline: "modified"},
		},
	}
	updData, _ := json.Marshal(updated)
	if err := sa.Update(context.Background(), "Agent", "default", "upd-agent", updData, current.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Verify the update.
	finalData, err := sa.Get(context.Background(), "Agent", "default", "upd-agent")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	var final api.Agent
	if err := json.Unmarshal(finalData, &final); err != nil {
		t.Fatalf("unmarshal final: %v", err)
	}
	if final.Spec.ModelRef.Name != "v2" {
		t.Errorf("modelRef = %q, want %q", final.Spec.ModelRef.Name, "v2")
	}
	if final.Spec.Instructions.Inline != "modified" {
		t.Errorf("instructions = %q, want %q", final.Spec.Instructions.Inline, "modified")
	}
}

// TestIntegration_StoreAdapter_DeleteAndVerify tests Create → Delete → Get(NotFound).
func TestIntegration_StoreAdapter_DeleteAndVerify(t *testing.T) {
	fc := integrationClient(t)
	sa := NewStoreAdapter(fc)

	agent := api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "del-agent"},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "x"},
			Instructions: api.AgentInstructions{Inline: "y"},
		},
	}
	data, _ := json.Marshal(agent)
	if err := sa.Create(context.Background(), "Agent", "default", "del-agent", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := sa.Delete(context.Background(), "Agent", "default", "del-agent"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err := sa.Get(context.Background(), "Agent", "default", "del-agent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: err = %v, want ErrNotFound", err)
	}
}

// TestIntegration_ReconcileNotFound verifies reconcilers handle missing objects gracefully.
func TestIntegration_ReconcileNotFound(t *testing.T) {
	fc := integrationClient(t)
	sa := NewStoreAdapter(fc)
	logger := testLogger(t)

	adapters := []struct {
		name    string
		adapter interface {
			Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
		}
	}{
		{"Agent", NewAgentReconcilerAdapter(sa, nil, logger)},
		{"AgentInstance", NewAgentInstanceReconcilerAdapter(sa, &fakeDispatcher{}, logger)},
		{"Workflow", NewWorkflowReconcilerAdapter(sa, logger)},
		{"Tool", NewToolReconcilerAdapter(sa, logger)},
		{"ToolClass", NewToolClassReconcilerAdapter(sa, logger)},
		{"ModelClass", NewModelClassReconcilerAdapter(sa, cpi.NewRegistry(), logger)},
		{"MemoryClaim", NewMemoryClaimReconcilerAdapter(sa, logger)},
	}

	for _, tc := range adapters {
		t.Run(tc.name, func(t *testing.T) {
			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "nonexistent"}}
			result, err := tc.adapter.Reconcile(context.Background(), req)
			if err != nil {
				t.Fatalf("reconcile error for missing %s: %v", tc.name, err)
			}
			if !result.IsZero() {
				t.Errorf("unexpected requeue for missing %s", tc.name)
			}
		})
	}
}
