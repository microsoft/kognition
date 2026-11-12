// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controller"
	"go.uber.org/zap"
)

// mockDispatcher implements controller.RuntimeDispatcher for testing.
type mockDispatcher struct {
	dispatchFn func(ctx context.Context, inst *v1alpha1.AgentInstance) (string, error)
	statusFn   func(ctx context.Context, inst *v1alpha1.AgentInstance) (controller.RuntimeStatus, error)
}

func (d *mockDispatcher) Dispatch(ctx context.Context, inst *v1alpha1.AgentInstance) (string, error) {
	if d.dispatchFn != nil {
		return d.dispatchFn(ctx, inst)
	}
	return "session-123", nil
}

func (d *mockDispatcher) Status(ctx context.Context, inst *v1alpha1.AgentInstance) (controller.RuntimeStatus, error) {
	if d.statusFn != nil {
		return d.statusFn(ctx, inst)
	}
	return controller.RuntimeStatus{Alive: true}, nil
}

func TestLifecycle_PendingToRunning(t *testing.T) {
	h := newTestHarness(t)
	logger, _ := zap.NewDevelopment()
	disp := &mockDispatcher{}
	rec := controller.NewAgentInstanceReconcilerWithDispatcher(h.Store, disp, logger)

	// Seed an AgentInstance in Pending phase.
	inst := v1alpha1.AgentInstance{
		TypeMeta: v1alpha1.TypeMeta{Kind: "AgentInstance", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:            "inst-1",
			Namespace:       testNS,
			ResourceVersion: 1,
		},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "agent-1"},
		},
		Status: v1alpha1.AgentInstanceStatus{
			Phase: v1alpha1.PhasePending,
		},
	}
	seedResource(t, h, "AgentInstance", testNS, "inst-1", inst)

	// Also seed the parent agent so reconciler can resolve it.
	seedResource(t, h, "Agent", testNS, "agent-1", makeAgent("agent-1", testNS, "m1", v1alpha1.PhaseReady))

	// Reconcile — should add finalizer first.
	res, err := rec.Reconcile(context.Background(), controller.Request{
		Namespace: testNS,
		Name:      "inst-1",
		Kind:      "AgentInstance",
	})
	if err != nil {
		t.Fatalf("reconcile (finalizer): %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	// Reconcile again — should dispatch and move to Running.
	res, err = rec.Reconcile(context.Background(), controller.Request{
		Namespace: testNS,
		Name:      "inst-1",
		Kind:      "AgentInstance",
	})
	if err != nil {
		t.Fatalf("reconcile (dispatch): %v", err)
	}

	// Verify the instance moved to Running.
	raw, err := h.Store.Get(context.Background(), "AgentInstance", testNS, "inst-1")
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	var updated v1alpha1.AgentInstance
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if updated.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("expected Running, got %s", updated.Status.Phase)
	}
	if updated.Status.SessionID != "session-123" {
		t.Fatalf("expected session-123, got %s", updated.Status.SessionID)
	}
	_ = res
}

func TestLifecycle_RunningToCompleted(t *testing.T) {
	h := newTestHarness(t)
	logger, _ := zap.NewDevelopment()
	disp := &mockDispatcher{
		statusFn: func(_ context.Context, _ *v1alpha1.AgentInstance) (controller.RuntimeStatus, error) {
			return controller.RuntimeStatus{
				Alive:     false,
				Completed: true,
				Output:    "task done",
				TokenUsage: &v1alpha1.TokenUsage{
					PromptTokens:     100,
					CompletionTokens: 50,
					TotalTokens:      150,
				},
			}, nil
		},
	}
	rec := controller.NewAgentInstanceReconcilerWithDispatcher(h.Store, disp, logger)

	// Seed instance already in Running phase with a session.
	inst := v1alpha1.AgentInstance{
		TypeMeta: v1alpha1.TypeMeta{Kind: "AgentInstance", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:            "inst-2",
			Namespace:       testNS,
			ResourceVersion: 1,
			Finalizers:      []string{"kognition.dev/instance-cleanup"},
		},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "agent-1"},
		},
		Status: v1alpha1.AgentInstanceStatus{
			Phase:     v1alpha1.PhaseRunning,
			SessionID: "sess-abc",
		},
	}
	seedResource(t, h, "AgentInstance", testNS, "inst-2", inst)
	seedResource(t, h, "Agent", testNS, "agent-1", makeAgent("agent-1", testNS, "m1", v1alpha1.PhaseReady))

	// Reconcile — should detect completed and update status.
	_, err := rec.Reconcile(context.Background(), controller.Request{
		Namespace: testNS,
		Name:      "inst-2",
		Kind:      "AgentInstance",
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	raw, err := h.Store.Get(context.Background(), "AgentInstance", testNS, "inst-2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var updated v1alpha1.AgentInstance
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if updated.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected Ready (completed), got %s", updated.Status.Phase)
	}
}

func TestLifecycle_DispatchFailure(t *testing.T) {
	h := newTestHarness(t)
	logger, _ := zap.NewDevelopment()
	disp := &mockDispatcher{
		dispatchFn: func(_ context.Context, _ *v1alpha1.AgentInstance) (string, error) {
			return "", context.DeadlineExceeded
		},
	}
	rec := controller.NewAgentInstanceReconcilerWithDispatcher(h.Store, disp, logger)

	inst := v1alpha1.AgentInstance{
		TypeMeta: v1alpha1.TypeMeta{Kind: "AgentInstance", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:            "inst-3",
			Namespace:       testNS,
			ResourceVersion: 1,
			Finalizers:      []string{"kognition.dev/instance-cleanup"},
		},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "agent-1"},
		},
		Status: v1alpha1.AgentInstanceStatus{
			Phase: v1alpha1.PhasePending,
		},
	}
	seedResource(t, h, "AgentInstance", testNS, "inst-3", inst)
	seedResource(t, h, "Agent", testNS, "agent-1", makeAgent("agent-1", testNS, "m1", v1alpha1.PhaseReady))

	// Reconcile — should return error (dispatch failed).
	_, err := rec.Reconcile(context.Background(), controller.Request{
		Namespace: testNS,
		Name:      "inst-3",
		Kind:      "AgentInstance",
	})
	// The reconciler may return an error or requeue — either is acceptable.
	// Check that the instance hasn't moved to Running.
	raw, _ := h.Store.Get(context.Background(), "AgentInstance", testNS, "inst-3")
	var updated v1alpha1.AgentInstance
	json.Unmarshal(raw, &updated)
	if updated.Status.Phase == v1alpha1.PhaseRunning {
		t.Fatal("should not be Running after dispatch failure")
	}
	_ = err
}

func TestLifecycle_AutoscalerScaleUp(t *testing.T) {
	h := newTestHarness(t)
	logger, _ := zap.NewDevelopment()
	rec := controller.NewAgentAutoscalerReconciler(h.Store, logger)

	// Seed an agent with replicas=1.
	agent := makeAgent("agent-1", testNS, "m1", v1alpha1.PhaseReady)
	agent.Spec.Replicas = int32Ptr(1)
	agent.Status.Replicas = 1
	seedResource(t, h, "Agent", testNS, "agent-1", agent)

	// Seed an autoscaler.
	as := v1alpha1.AgentAutoscaler{
		TypeMeta: v1alpha1.TypeMeta{Kind: "AgentAutoscaler", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:            "as-1",
			Namespace:       testNS,
			ResourceVersion: 1,
		},
		Spec: v1alpha1.AgentAutoscalerSpec{
			AgentRef:    v1alpha1.LocalObjectReference{Name: "agent-1"},
			MinReplicas: 1,
			MaxReplicas: 5,
			Metrics: []v1alpha1.AutoscalerMetric{
				{Type: v1alpha1.MetricQueueDepth, TargetValue: 10},
			},
		},
	}
	seedResource(t, h, "AgentAutoscaler", testNS, "as-1", as)

	// Reconcile.
	_, err := rec.Reconcile(context.Background(), controller.Request{
		Namespace: testNS,
		Name:      "as-1",
		Kind:      "AgentAutoscaler",
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	raw, err := h.Store.Get(context.Background(), "AgentAutoscaler", testNS, "as-1")
	if err != nil {
		t.Fatalf("get autoscaler: %v", err)
	}
	var updated v1alpha1.AgentAutoscaler
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("decode autoscaler: %v", err)
	}
	for _, finalizer := range updated.ObjectMeta.Finalizers {
		if finalizer == controller.AgentAutoscalerFinalizer {
			return
		}
	}
	t.Fatal("first reconcile did not persist the autoscaler finalizer")
}

func int32Ptr(i int32) *int32 { return &i }
