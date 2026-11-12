// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/store"
	"go.uber.org/zap/zaptest"
)

type conflictOnceAgentStore struct {
	store.Store
	conflicted bool
}

type persistentAgentConflictStore struct {
	store.Store
	updates int
}

type concurrentAgentAutoscalerEditStore struct {
	store.Store
	conflicted   bool
	agentUpdates int
}

type persistentAgentAutoscalerConflictStore struct {
	store.Store
	updates int
}

func (s *persistentAgentAutoscalerConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == agentAutoscalerKind {
		s.updates++
		return store.ErrConflict
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *concurrentAgentAutoscalerEditStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "Agent" {
		s.agentUpdates++
	}
	if kind == agentAutoscalerKind && !s.conflicted {
		s.conflicted = true
		raw, err := s.Store.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var latest v1alpha1.AgentAutoscaler
		if err := json.Unmarshal(raw, &latest); err != nil {
			return err
		}
		latest.ObjectMeta.Annotations = map[string]string{"example.test/audit": "concurrent"}
		latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{
			Type: "Audited", Status: string(lifecycle.ConditionTrue), Reason: "ConcurrentAudit",
		})
		updated, err := json.Marshal(latest)
		if err != nil {
			return err
		}
		if err := s.Store.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
			return err
		}
		return store.ErrConflict
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *persistentAgentConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "Agent" {
		s.updates++
		return store.ErrConflict
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *conflictOnceAgentStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "Agent" && !s.conflicted {
		s.conflicted = true
		raw, err := s.Store.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var concurrent v1alpha1.Agent
		if err := json.Unmarshal(raw, &concurrent); err != nil {
			return err
		}
		concurrent.ObjectMeta.Annotations = map[string]string{"example.test/owner": "concurrent"}
		concurrent.Status.Conditions = []v1alpha1.Condition{{
			Type:   "Audited",
			Status: string(lifecycle.ConditionTrue),
			Reason: "ConcurrentUpdate",
		}}
		concurrentData, err := json.Marshal(concurrent)
		if err != nil {
			return err
		}
		if err := s.Store.Update(ctx, kind, namespace, name, concurrentData, concurrent.ObjectMeta.ResourceVersion); err != nil {
			return err
		}
		return store.ErrConflict
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func makeAgentAutoscaler(ns, name, agentName string, min, max int32, metrics []v1alpha1.AutoscalerMetric) v1alpha1.AgentAutoscaler {
	return v1alpha1.AgentAutoscaler{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentAutoscaler"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.AgentAutoscalerSpec{
			AgentRef:    v1alpha1.LocalObjectReference{Name: agentName},
			MinReplicas: min,
			MaxReplicas: max,
			Metrics:     metrics,
		},
	}
}

func seedAgentForAutoscaler(t *testing.T, ctx context.Context, s *memStore, ns, name string, replicas int32, instanceCount int) {
	t.Helper()
	agent := makeAgent(ns, name, replicas)
	agent.ObjectMeta.UID = "agent-uid-" + name
	data, _ := json.Marshal(agent)
	if err := s.Create(ctx, "Agent", ns, name, data); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Create instances owned by the agent.
	for i := 0; i < instanceCount; i++ {
		inst := v1alpha1.AgentInstance{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentInstance"},
			ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: fmt.Sprintf("%s-inst-%d", name, i)},
			Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: name}},
			Status:     v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhaseRunning},
		}
		inst.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{
			{Kind: "Agent", Name: name, UID: "agent-uid-" + name},
		}
		instData, _ := json.Marshal(inst)
		if err := s.Create(ctx, "AgentInstance", ns, inst.ObjectMeta.Name, instData); err != nil {
			t.Fatalf("seed instance: %v", err)
		}
	}
}

// TC-0105: scale up when metric exceeds target
func TestAgentAutoscalerReconciler_ScaleUp(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAgentAutoscalerReconciler(s, logger)

	// Seed agent with 1 replica and 3 active instances (simulating high load).
	seedAgentForAutoscaler(t, ctx, s, "default", "my-agent", 1, 3)

	// Create autoscaler targeting 1 instance per replica.
	as := makeAgentAutoscaler("default", "my-as", "my-agent", 1, 5,
		[]v1alpha1.AutoscalerMetric{{Type: v1alpha1.MetricQueueDepth, TargetValue: 1}})
	data, _ := json.Marshal(as)
	if err := s.Create(ctx, agentAutoscalerKind, "default", "my-as", data); err != nil {
		t.Fatal(err)
	}

	// First reconcile: adds finalizer, requeues.
	res, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "my-as", Kind: agentAutoscalerKind})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	// Second reconcile: computes desired and scales.
	res, err = r.Reconcile(ctx, Request{Namespace: "default", Name: "my-as", Kind: agentAutoscalerKind})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	// Verify Agent.Spec.Replicas was updated.
	agentRaw, err := s.Get(ctx, "Agent", "default", "my-agent")
	if err != nil {
		t.Fatal(err)
	}
	var agent v1alpha1.Agent
	json.Unmarshal(agentRaw, &agent)
	if agent.Spec.Replicas == nil || *agent.Spec.Replicas < 2 {
		t.Fatalf("expected scale up, got replicas=%v", agent.Spec.Replicas)
	}
	if *agent.Spec.Replicas > 5 {
		t.Fatalf("exceeded max replicas: %d", *agent.Spec.Replicas)
	}

	// Verify status was updated.
	asRaw, _ := s.Get(ctx, agentAutoscalerKind, "default", "my-as")
	var updatedAS v1alpha1.AgentAutoscaler
	json.Unmarshal(asRaw, &updatedAS)
	if updatedAS.Status.DesiredReplicas <= 1 {
		t.Fatalf("expected desired > 1, got %d", updatedAS.Status.DesiredReplicas)
	}
	if updatedAS.Status.LastScaleTime == nil {
		t.Fatal("expected LastScaleTime to be set")
	}
	_ = res
}

func TestAgentAutoscalerReconciler_ScaleUpPreservesConcurrentStateAfterConflict(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	conflictStore := &conflictOnceAgentStore{Store: baseStore}
	r := NewAgentAutoscalerReconciler(conflictStore, zaptest.NewLogger(t))

	seedAgentForAutoscaler(t, ctx, baseStore, "default", "conflict-agent", 1, 3)
	as := makeAgentAutoscaler("default", "conflict-as", "conflict-agent", 1, 5,
		[]v1alpha1.AutoscalerMetric{{Type: v1alpha1.MetricQueueDepth, TargetValue: 1}})
	data, _ := json.Marshal(as)
	if err := baseStore.Create(ctx, agentAutoscalerKind, "default", "conflict-as", data); err != nil {
		t.Fatal(err)
	}

	request := Request{Namespace: "default", Name: "conflict-as", Kind: agentAutoscalerKind}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("scale after conflict: %v", err)
	}

	agentRaw, err := baseStore.Get(ctx, "Agent", "default", "conflict-agent")
	if err != nil {
		t.Fatal(err)
	}
	var agent v1alpha1.Agent
	if err := json.Unmarshal(agentRaw, &agent); err != nil {
		t.Fatal(err)
	}
	if agent.Spec.Replicas == nil || *agent.Spec.Replicas != 3 {
		t.Fatalf("expected desired replicas=3 after retry, got %v", agent.Spec.Replicas)
	}
	if agent.ObjectMeta.Annotations["example.test/owner"] != "concurrent" {
		t.Fatalf("annotations = %v, want concurrent annotation preserved", agent.ObjectMeta.Annotations)
	}
	condition := lifecycle.GetCondition(apiToLifecycleConditions(agent.Status.Conditions), lifecycle.ConditionType("Audited"))
	if condition == nil || condition.Reason != "ConcurrentUpdate" {
		t.Fatalf("Audited condition = %#v, want concurrent condition preserved", condition)
	}
}

func TestAgentAutoscalerReconciler_StatusConflictPreservesScaleTransitionAndConcurrentState(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	conflictStore := &concurrentAgentAutoscalerEditStore{Store: baseStore}
	r := NewAgentAutoscalerReconciler(conflictStore, zaptest.NewLogger(t))

	seedAgentForAutoscaler(t, ctx, baseStore, "default", "status-conflict-agent", 1, 3)
	as := makeAgentAutoscaler("default", "status-conflict-as", "status-conflict-agent", 1, 5,
		[]v1alpha1.AutoscalerMetric{{Type: v1alpha1.MetricQueueDepth, TargetValue: 1}})
	as.ObjectMeta.Finalizers = []string{AgentAutoscalerFinalizer}
	data, _ := json.Marshal(as)
	if err := baseStore.Create(ctx, agentAutoscalerKind, "default", as.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: as.ObjectMeta.Name, Kind: agentAutoscalerKind}); err != nil {
		t.Fatalf("scale after autoscaler conflict: %v", err)
	}
	if !conflictStore.conflicted {
		t.Fatal("expected autoscaler conflict fixture to execute")
	}
	if conflictStore.agentUpdates != 1 {
		t.Fatalf("agent update attempts = %d, want exactly 1", conflictStore.agentUpdates)
	}

	raw, err := baseStore.Get(ctx, agentAutoscalerKind, "default", as.ObjectMeta.Name)
	if err != nil {
		t.Fatal(err)
	}
	var stored v1alpha1.AgentAutoscaler
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.LastScaleTime == nil || stored.Status.CurrentReplicas != 1 || stored.Status.DesiredReplicas != 3 {
		t.Fatalf("scale transition was not persisted: %+v", stored.Status)
	}
	if stored.ObjectMeta.Annotations["example.test/audit"] != "concurrent" {
		t.Fatalf("concurrent annotations were lost: %+v", stored.ObjectMeta.Annotations)
	}
	condition := lifecycle.GetCondition(apiToLifecycleConditions(stored.Status.Conditions), lifecycle.ConditionType("Audited"))
	if condition == nil || condition.Reason != "ConcurrentAudit" {
		t.Fatalf("Audited condition = %#v, want concurrent condition preserved", condition)
	}
}

func TestAgentAutoscalerReconciler_StatusConflictRetryIsBounded(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	conflictStore := &persistentAgentAutoscalerConflictStore{Store: baseStore}
	r := NewAgentAutoscalerReconciler(conflictStore, zaptest.NewLogger(t))
	as := makeAgentAutoscaler("default", "persistent-conflict-as", "unused-agent", 1, 5, nil)
	data, _ := json.Marshal(as)
	if err := baseStore.Create(ctx, agentAutoscalerKind, "default", as.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}

	_, err := r.Reconcile(ctx, Request{Namespace: "default", Name: as.ObjectMeta.Name, Kind: agentAutoscalerKind})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("reconcile error = %v, want conflict", err)
	}
	if conflictStore.updates != 5 {
		t.Fatalf("autoscaler update attempts = %d, want 5", conflictStore.updates)
	}
	raw, err := baseStore.Get(ctx, agentAutoscalerKind, "default", as.ObjectMeta.Name)
	if err != nil {
		t.Fatal(err)
	}
	var stored v1alpha1.AgentAutoscaler
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.ObjectMeta.Finalizers) != 0 {
		t.Fatalf("finalizers = %v, want unchanged persisted state", stored.ObjectMeta.Finalizers)
	}
}

// TC-0106: stabilization window blocks scale down
func TestAgentAutoscalerReconciler_StabilizationWindow(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAgentAutoscalerReconciler(s, logger)

	// Seed agent with 3 replicas and 1 instance (low load → should want to scale down).
	seedAgentForAutoscaler(t, ctx, s, "default", "my-agent", 3, 1)

	// Create autoscaler with a 60s scale-down stabilization window.
	scaleDownWindow := int32(60)
	as := makeAgentAutoscaler("default", "my-as", "my-agent", 1, 5,
		[]v1alpha1.AutoscalerMetric{{Type: v1alpha1.MetricQueueDepth, TargetValue: 1}})
	as.Spec.ScaleDownPolicy = &v1alpha1.ScalingPolicy{StabilizationWindowSeconds: scaleDownWindow}
	// Pretend we just scaled recently.
	now := time.Now().UTC()
	as.Status.LastScaleTime = &now

	data, _ := json.Marshal(as)
	if err := s.Create(ctx, agentAutoscalerKind, "default", "my-as", data); err != nil {
		t.Fatal(err)
	}

	// First reconcile: adds finalizer.
	r.Reconcile(ctx, Request{Namespace: "default", Name: "my-as", Kind: agentAutoscalerKind})

	// Second reconcile: should be blocked by stabilization window.
	res, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "my-as", Kind: agentAutoscalerKind})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Agent should NOT have been scaled down.
	agentRaw, _ := s.Get(ctx, "Agent", "default", "my-agent")
	var agent v1alpha1.Agent
	json.Unmarshal(agentRaw, &agent)
	if agent.Spec.Replicas != nil && *agent.Spec.Replicas < 3 {
		t.Fatalf("scale-down should be blocked, got replicas=%d", *agent.Spec.Replicas)
	}

	// Should have a RequeueAfter set.
	if res.RequeueAfter <= 0 {
		t.Fatal("expected RequeueAfter > 0 during stabilization")
	}
}

// TC-0107: min/max bounds are never violated
func TestAgentAutoscalerReconciler_MinMaxBounds(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAgentAutoscalerReconciler(s, logger)

	// Seed agent with 1 replica and 100 instances (extreme load).
	seedAgentForAutoscaler(t, ctx, s, "default", "my-agent", 1, 100)

	// Create autoscaler with max=3.
	as := makeAgentAutoscaler("default", "my-as", "my-agent", 2, 3,
		[]v1alpha1.AutoscalerMetric{{Type: v1alpha1.MetricQueueDepth, TargetValue: 1}})
	data, _ := json.Marshal(as)
	if err := s.Create(ctx, agentAutoscalerKind, "default", "my-as", data); err != nil {
		t.Fatal(err)
	}

	// Add finalizer.
	r.Reconcile(ctx, Request{Namespace: "default", Name: "my-as", Kind: agentAutoscalerKind})
	// Scale.
	r.Reconcile(ctx, Request{Namespace: "default", Name: "my-as", Kind: agentAutoscalerKind})

	// Verify clamped to max.
	agentRaw, _ := s.Get(ctx, "Agent", "default", "my-agent")
	var agent v1alpha1.Agent
	json.Unmarshal(agentRaw, &agent)
	if agent.Spec.Replicas == nil || *agent.Spec.Replicas != 3 {
		t.Fatalf("expected replicas clamped to max=3, got %v", agent.Spec.Replicas)
	}

	// Also verify min bound: seed new scenario with 0 load.
	s2 := newMemStore()
	r2 := NewAgentAutoscalerReconciler(s2, logger)
	seedAgentForAutoscaler(t, ctx, s2, "default", "agent2", 5, 0)
	as2 := makeAgentAutoscaler("default", "as2", "agent2", 2, 10,
		[]v1alpha1.AutoscalerMetric{{Type: v1alpha1.MetricQueueDepth, TargetValue: 1}})
	data2, _ := json.Marshal(as2)
	s2.Create(ctx, agentAutoscalerKind, "default", "as2", data2)

	r2.Reconcile(ctx, Request{Namespace: "default", Name: "as2", Kind: agentAutoscalerKind})
	r2.Reconcile(ctx, Request{Namespace: "default", Name: "as2", Kind: agentAutoscalerKind})

	agentRaw2, _ := s2.Get(ctx, "Agent", "default", "agent2")
	var agent2 v1alpha1.Agent
	json.Unmarshal(agentRaw2, &agent2)
	if agent2.Spec.Replicas == nil || *agent2.Spec.Replicas < 2 {
		t.Fatalf("expected replicas clamped to min=2, got %v", agent2.Spec.Replicas)
	}
}

func seedTerminalInstancesForLatency(t *testing.T, ctx context.Context, s *memStore, ns, agentName string, latencies []time.Duration) {
	t.Helper()
	for i, latency := range latencies {
		start := time.Now().UTC().Add(-10 * time.Minute).Add(time.Duration(i) * time.Second)
		end := start.Add(latency)
		inst := v1alpha1.AgentInstance{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentInstance"},
			ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: fmt.Sprintf("%s-lat-%d", agentName, i)},
			Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: agentName}},
			Status: v1alpha1.AgentInstanceStatus{
				Phase:     v1alpha1.PhaseReady,
				StartTime: &start,
				Conditions: []v1alpha1.Condition{{
					Type:               "Ready",
					Status:             "True",
					Reason:             "Completed",
					LastTransitionTime: end,
				}},
			},
		}
		inst.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{Kind: "Agent", Name: agentName, UID: "agent-uid-" + agentName}}
		instData, _ := json.Marshal(inst)
		if err := s.Create(ctx, "AgentInstance", ns, inst.ObjectMeta.Name, instData); err != nil {
			t.Fatalf("seed latency instance: %v", err)
		}
	}
}

// TC-0108: p95 latency metric drives scale up when p95 exceeds target.
func TestAgentAutoscalerReconciler_P95LatencyScalesUp(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAgentAutoscalerReconciler(s, logger)

	seedAgentForAutoscaler(t, ctx, s, "default", "lat-agent", 2, 0)
	seedTerminalInstancesForLatency(t, ctx, s, "default", "lat-agent", []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		300 * time.Millisecond,
		400 * time.Millisecond,
		500 * time.Millisecond,
		600 * time.Millisecond,
		700 * time.Millisecond,
		800 * time.Millisecond,
		900 * time.Millisecond,
		1800 * time.Millisecond,
	})

	as := makeAgentAutoscaler("default", "lat-as", "lat-agent", 1, 6,
		[]v1alpha1.AutoscalerMetric{{Type: v1alpha1.MetricP95Latency, TargetValue: 1000}})
	data, _ := json.Marshal(as)
	if err := s.Create(ctx, agentAutoscalerKind, "default", "lat-as", data); err != nil {
		t.Fatal(err)
	}

	_, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "lat-as", Kind: agentAutoscalerKind})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	_, err = r.Reconcile(ctx, Request{Namespace: "default", Name: "lat-as", Kind: agentAutoscalerKind})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	agentRaw, err := s.Get(ctx, "Agent", "default", "lat-agent")
	if err != nil {
		t.Fatal(err)
	}
	var agent v1alpha1.Agent
	_ = json.Unmarshal(agentRaw, &agent)
	if agent.Spec.Replicas == nil || *agent.Spec.Replicas < 4 {
		t.Fatalf("expected p95-based scale up to at least 4 replicas, got %v", agent.Spec.Replicas)
	}
}

// TC-0109: p95 latency metric with no terminal samples does not force scaling.
func TestAgentAutoscalerReconciler_P95LatencyNoSamplesNoScale(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAgentAutoscalerReconciler(s, logger)

	seedAgentForAutoscaler(t, ctx, s, "default", "lat-nosample-agent", 3, 0)

	as := makeAgentAutoscaler("default", "lat-nosample-as", "lat-nosample-agent", 1, 6,
		[]v1alpha1.AutoscalerMetric{{Type: v1alpha1.MetricP95Latency, TargetValue: 1000}})
	data, _ := json.Marshal(as)
	if err := s.Create(ctx, agentAutoscalerKind, "default", "lat-nosample-as", data); err != nil {
		t.Fatal(err)
	}

	_, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "lat-nosample-as", Kind: agentAutoscalerKind})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	_, err = r.Reconcile(ctx, Request{Namespace: "default", Name: "lat-nosample-as", Kind: agentAutoscalerKind})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	agentRaw, err := s.Get(ctx, "Agent", "default", "lat-nosample-agent")
	if err != nil {
		t.Fatal(err)
	}
	var agent v1alpha1.Agent
	_ = json.Unmarshal(agentRaw, &agent)
	if agent.Spec.Replicas == nil || *agent.Spec.Replicas != 3 {
		t.Fatalf("expected no scale change with no p95 samples, got %v", agent.Spec.Replicas)
	}
}

func TestAgentAutoscalerP95UsesReadyTransitionOnly(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	reconciler := NewAgentAutoscalerReconciler(store, zaptest.NewLogger(t))
	seedAgentForAutoscaler(t, ctx, store, "default", "latency-agent", 1, 0)

	start := time.Now().UTC().Add(-time.Minute)
	instance := v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{
			Namespace: "default",
			Name:      "latency-instance",
			OwnerReferences: []v1alpha1.OwnerReference{{
				Kind: "Agent", Name: "latency-agent", UID: "agent-uid-latency-agent",
			}},
		},
		Status: v1alpha1.AgentInstanceStatus{
			Phase:     v1alpha1.PhaseFailed,
			StartTime: &start,
			Conditions: []v1alpha1.Condition{
				{Type: "Ready", Status: "True", LastTransitionTime: start.Add(500 * time.Millisecond)},
				{Type: "Ready", Status: "False", LastTransitionTime: start.Add(5 * time.Second)},
			},
		},
	}
	data, err := json.Marshal(instance)
	if err != nil {
		t.Fatalf("marshal instance: %v", err)
	}
	if err := store.Create(ctx, agentInstanceKind, "default", instance.ObjectMeta.Name, data); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	agentRaw, err := store.Get(ctx, "Agent", "default", "latency-agent")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	var agent v1alpha1.Agent
	if err := json.Unmarshal(agentRaw, &agent); err != nil {
		t.Fatalf("unmarshal agent: %v", err)
	}
	latency, observed, err := reconciler.getMetricValue(ctx, v1alpha1.MetricP95Latency, &agent)
	if err != nil {
		t.Fatalf("getMetricValue() error = %v", err)
	}
	if !observed || latency != 500 {
		t.Fatalf("P95 latency = %d, observed = %v; want 500, true", latency, observed)
	}
}

func TestAgentAutoscalerStabilizationWindows(t *testing.T) {
	now := time.Now().UTC()
	lastScale := now.Add(-10 * time.Second)
	autoscaler := &v1alpha1.AgentAutoscaler{
		Spec: v1alpha1.AgentAutoscalerSpec{
			ScaleUpPolicy:   &v1alpha1.ScalingPolicy{StabilizationWindowSeconds: 30},
			ScaleDownPolicy: &v1alpha1.ScalingPolicy{StabilizationWindowSeconds: 60},
		},
		Status: v1alpha1.AgentAutoscalerStatus{LastScaleTime: &lastScale},
	}
	reconciler := NewAgentAutoscalerReconciler(newMemStore(), zaptest.NewLogger(t))

	for _, tc := range []struct {
		name             string
		current, desired int32
		at               time.Time
		wantBlocked      bool
		wantRemaining    time.Duration
	}{
		{name: "scale up blocked", current: 2, desired: 3, at: now, wantBlocked: true, wantRemaining: 20 * time.Second},
		{name: "scale down blocked", current: 2, desired: 1, at: now, wantBlocked: true, wantRemaining: 50 * time.Second},
		{name: "scale up window expired", current: 2, desired: 3, at: lastScale.Add(30 * time.Second)},
		{name: "scale down window expired", current: 2, desired: 1, at: lastScale.Add(60 * time.Second)},
		{name: "unchanged replicas", current: 2, desired: 2, at: now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocked, remaining := reconciler.isStabilizationBlocked(autoscaler, tc.current, tc.desired, tc.at)
			if blocked != tc.wantBlocked || remaining != tc.wantRemaining {
				t.Fatalf("isStabilizationBlocked() = %v, %v; want %v, %v", blocked, remaining, tc.wantBlocked, tc.wantRemaining)
			}
		})
	}
}

func TestAgentAutoscalerReconcilerDeletionRemovesFinalizer(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	reconciler := NewAgentAutoscalerReconciler(store, zaptest.NewLogger(t))
	autoscaler := makeAgentAutoscaler("default", "deleting-as", "agent", 1, 3, nil)
	deletedAt := time.Now().UTC()
	autoscaler.ObjectMeta.DeletionTimestamp = &deletedAt
	autoscaler.ObjectMeta.Finalizers = []string{AgentAutoscalerFinalizer, "example.test/other"}
	data, err := json.Marshal(autoscaler)
	if err != nil {
		t.Fatalf("marshal autoscaler: %v", err)
	}
	if err := store.Create(ctx, agentAutoscalerKind, "default", autoscaler.ObjectMeta.Name, data); err != nil {
		t.Fatalf("create autoscaler: %v", err)
	}

	result, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: autoscaler.ObjectMeta.Name, Kind: agentAutoscalerKind})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("Reconcile() result = %#v, want no requeue", result)
	}
	raw, err := store.Get(ctx, agentAutoscalerKind, "default", autoscaler.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get autoscaler: %v", err)
	}
	var stored v1alpha1.AgentAutoscaler
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal autoscaler: %v", err)
	}
	if lifecycle.HasFinalizer(stored.ObjectMeta.Finalizers, AgentAutoscalerFinalizer) {
		t.Fatalf("finalizers = %v, want autoscaler finalizer removed", stored.ObjectMeta.Finalizers)
	}
	if !lifecycle.HasFinalizer(stored.ObjectMeta.Finalizers, "example.test/other") {
		t.Fatalf("finalizers = %v, want unrelated finalizer preserved", stored.ObjectMeta.Finalizers)
	}
}

func TestAgentAutoscalerReconcilerMissingAgentSetsNotReady(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	reconciler := NewAgentAutoscalerReconciler(store, zaptest.NewLogger(t))
	autoscaler := makeAgentAutoscaler("default", "missing-agent-as", "missing-agent", 1, 3, nil)
	autoscaler.ObjectMeta.Finalizers = []string{AgentAutoscalerFinalizer}
	data, err := json.Marshal(autoscaler)
	if err != nil {
		t.Fatalf("marshal autoscaler: %v", err)
	}
	if err := store.Create(ctx, agentAutoscalerKind, "default", autoscaler.ObjectMeta.Name, data); err != nil {
		t.Fatalf("create autoscaler: %v", err)
	}

	result, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: autoscaler.ObjectMeta.Name, Kind: agentAutoscalerKind})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Fatalf("RequeueAfter = %v, want 10s", result.RequeueAfter)
	}
	raw, err := store.Get(ctx, agentAutoscalerKind, "default", autoscaler.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get autoscaler: %v", err)
	}
	var stored v1alpha1.AgentAutoscaler
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal autoscaler: %v", err)
	}
	condition := lifecycle.GetCondition(apiToLifecycleConditions(stored.Status.Conditions), lifecycle.ConditionReady)
	if condition == nil || condition.Status != string(lifecycle.ConditionFalse) || condition.Reason != "AgentNotFound" {
		t.Fatalf("Ready condition = %#v, want False/AgentNotFound", condition)
	}
	resourceVersion := stored.ObjectMeta.ResourceVersion
	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: autoscaler.ObjectMeta.Name, Kind: agentAutoscalerKind}); err != nil {
		t.Fatalf("repeat Reconcile() error = %v", err)
	}
	raw, err = store.Get(ctx, agentAutoscalerKind, "default", autoscaler.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get autoscaler after repeat reconcile: %v", err)
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal autoscaler after repeat reconcile: %v", err)
	}
	if stored.ObjectMeta.ResourceVersion != resourceVersion {
		t.Fatalf("resourceVersion = %d after unchanged missing-agent reconcile, want %d", stored.ObjectMeta.ResourceVersion, resourceVersion)
	}
}

func TestAgentAutoscalerReconcilerMalformedResourceReturnsError(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	if err := store.Create(ctx, agentAutoscalerKind, "default", "malformed", []byte("{")); err != nil {
		t.Fatalf("create malformed autoscaler: %v", err)
	}

	reconciler := NewAgentAutoscalerReconciler(store, zaptest.NewLogger(t))
	_, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "malformed", Kind: agentAutoscalerKind})
	if err == nil || !strings.Contains(err.Error(), "unmarshal agent autoscaler") {
		t.Fatalf("Reconcile() error = %v, want unmarshal agent autoscaler error", err)
	}
}

func TestAgentAutoscalerReconcilerUnsupportedMetricSetsNotReady(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	seedAgentForAutoscaler(t, ctx, store, "default", "metric-agent", 1, 1)
	autoscaler := makeAgentAutoscaler("default", "metric-as", "metric-agent", 1, 3,
		[]v1alpha1.AutoscalerMetric{{Type: v1alpha1.MetricType("Unsupported"), TargetValue: 1}})
	autoscaler.ObjectMeta.Finalizers = []string{AgentAutoscalerFinalizer}
	data, err := json.Marshal(autoscaler)
	if err != nil {
		t.Fatalf("marshal autoscaler: %v", err)
	}
	if err := store.Create(ctx, agentAutoscalerKind, "default", autoscaler.ObjectMeta.Name, data); err != nil {
		t.Fatalf("create autoscaler: %v", err)
	}

	reconciler := NewAgentAutoscalerReconciler(store, zaptest.NewLogger(t))
	result, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: autoscaler.ObjectMeta.Name, Kind: agentAutoscalerKind})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Fatalf("RequeueAfter = %v, want 10s", result.RequeueAfter)
	}
	raw, err := store.Get(ctx, agentAutoscalerKind, "default", autoscaler.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get autoscaler: %v", err)
	}
	var stored v1alpha1.AgentAutoscaler
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal autoscaler: %v", err)
	}
	condition := lifecycle.GetCondition(apiToLifecycleConditions(stored.Status.Conditions), lifecycle.ConditionReady)
	if condition == nil || condition.Status != string(lifecycle.ConditionFalse) || condition.Reason != "MetricError" {
		t.Fatalf("Ready condition = %#v, want False/MetricError", condition)
	}
}

func TestAgentAutoscalerReconcilerExhaustedConflictsDoNotRecordScale(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	conflictStore := &persistentAgentConflictStore{Store: baseStore}
	seedAgentForAutoscaler(t, ctx, baseStore, "default", "blocked-agent", 1, 3)
	autoscaler := makeAgentAutoscaler("default", "blocked-as", "blocked-agent", 1, 5,
		[]v1alpha1.AutoscalerMetric{{Type: v1alpha1.MetricQueueDepth, TargetValue: 1}})
	autoscaler.ObjectMeta.Finalizers = []string{AgentAutoscalerFinalizer}
	data, err := json.Marshal(autoscaler)
	if err != nil {
		t.Fatalf("marshal autoscaler: %v", err)
	}
	if err := baseStore.Create(ctx, agentAutoscalerKind, "default", autoscaler.ObjectMeta.Name, data); err != nil {
		t.Fatalf("create autoscaler: %v", err)
	}

	reconciler := NewAgentAutoscalerReconciler(conflictStore, zaptest.NewLogger(t))
	_, err = reconciler.Reconcile(ctx, Request{Namespace: "default", Name: autoscaler.ObjectMeta.Name, Kind: agentAutoscalerKind})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Reconcile() error = %v, want ErrConflict", err)
	}
	if conflictStore.updates != 5 {
		t.Fatalf("Agent update attempts = %d, want 5", conflictStore.updates)
	}
	raw, err := baseStore.Get(ctx, agentAutoscalerKind, "default", autoscaler.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get autoscaler: %v", err)
	}
	var stored v1alpha1.AgentAutoscaler
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal autoscaler: %v", err)
	}
	if stored.Status.LastScaleTime != nil {
		t.Fatalf("LastScaleTime = %v, want nil after failed scale", stored.Status.LastScaleTime)
	}
}
