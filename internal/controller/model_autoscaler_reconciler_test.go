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

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type orderedModelListStore struct {
	*memStore
	models [][]byte
}

type modelAutoscalerGetFaultStore struct {
	*memStore
	kind string
}

func (s *modelAutoscalerGetFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == s.kind {
		return nil, fmt.Errorf("wrapped missing %s: %w", kind, store.ErrNotFound)
	}
	return s.memStore.Get(ctx, kind, namespace, name)
}

type concurrentAgentEditStore struct {
	*memStore
	conflicted bool
}

type conflictingAgentStore struct {
	*memStore
	updates int
}

type conflictingModelAutoscalerStore struct {
	*memStore
	updates int
}

func (s *conflictingModelAutoscalerStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == modelAutoscalerKind {
		s.updates++
		return fmt.Errorf("wrapped ModelAutoscaler conflict: %w", store.ErrConflict)
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

type concurrentModelAutoscalerEditStore struct {
	*memStore
	conflicted   bool
	agentUpdates int
}

func (s *concurrentModelAutoscalerEditStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "Agent" {
		s.agentUpdates++
	}
	if kind == modelAutoscalerKind && !s.conflicted {
		s.conflicted = true
		raw, err := s.memStore.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var latest v1alpha1.ModelAutoscaler
		if err := json.Unmarshal(raw, &latest); err != nil {
			return err
		}
		latest.ObjectMeta.Annotations = map[string]string{"example.com/audit": "concurrent"}
		latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{
			Type: "Audited", Status: "True", Reason: "ConcurrentAudit",
		})
		updated, err := json.Marshal(latest)
		if err != nil {
			return err
		}
		if err := s.memStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
			return err
		}
		return fmt.Errorf("wrapped concurrent ModelAutoscaler conflict: %w", store.ErrConflict)
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *conflictingAgentStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "Agent" {
		s.updates++
		return fmt.Errorf("wrapped Agent conflict: %w", store.ErrConflict)
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *concurrentAgentEditStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "Agent" && !s.conflicted {
		s.conflicted = true
		raw, err := s.memStore.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var latest v1alpha1.Agent
		if err := json.Unmarshal(raw, &latest); err != nil {
			return err
		}
		latest.Spec.Instructions.Inline = "concurrently updated instructions"
		updated, err := json.Marshal(latest)
		if err != nil {
			return err
		}
		if err := s.memStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
			return err
		}
		return fmt.Errorf("wrapped concurrent Agent conflict: %w", store.ErrConflict)
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *orderedModelListStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if kind == "Model" {
		return s.models, nil
	}
	return s.memStore.List(ctx, kind, opts)
}

func storeModelAutoscalerFixture(t *testing.T, s *memStore, mas v1alpha1.ModelAutoscaler) {
	t.Helper()
	data, err := json.Marshal(mas)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), modelAutoscalerKind, mas.ObjectMeta.Namespace, mas.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
}

func getModelAutoscalerFixture(t *testing.T, s *memStore, ns, name string) v1alpha1.ModelAutoscaler {
	t.Helper()
	raw, err := s.Get(context.Background(), modelAutoscalerKind, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	var mas v1alpha1.ModelAutoscaler
	if err := json.Unmarshal(raw, &mas); err != nil {
		t.Fatal(err)
	}
	return mas
}

func storeModelFixture(t *testing.T, s *memStore, model v1alpha1.Model) {
	t.Helper()
	data, err := json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), "Model", model.ObjectMeta.Namespace, model.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
}

func getAutoscalerAgent(t *testing.T, s *memStore, ns, name string) v1alpha1.Agent {
	t.Helper()
	raw, err := s.Get(context.Background(), "Agent", ns, name)
	if err != nil {
		t.Fatal(err)
	}
	var agent v1alpha1.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		t.Fatal(err)
	}
	return agent
}

func makeModel(ns, name, className string, phase v1alpha1.Phase) v1alpha1.Model {
	return v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.ModelSpec{
			ClassName: className,
			ModelID:   name,
			Provider:  "openai-compatible",
		},
		Status: v1alpha1.ModelStatus{Phase: phase},
	}
}

func makeModelClass(ns, name, fallback string, phase v1alpha1.Phase) v1alpha1.ModelClass {
	return v1alpha1.ModelClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ModelClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.ModelClassSpec{Provisioner: "openai-compatible", LatencyClass: "low", CostTier: "standard"},
		Status:     v1alpha1.ModelClassStatus{Phase: phase, FallbackClass: fallback, Health: "Healthy"},
	}
}

func makeModelAutoscaler(ns, name, agentName, preferredModel string) v1alpha1.ModelAutoscaler {
	return v1alpha1.ModelAutoscaler{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ModelAutoscaler"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.ModelAutoscalerSpec{
			AgentRef:                   v1alpha1.LocalObjectReference{Name: agentName},
			PreferredModelRef:          v1alpha1.LocalObjectReference{Name: preferredModel},
			StabilizationWindowSeconds: 0,
		},
	}
}

func TestModelAutoscalerReconciler_SwitchesToFallbackOnDegradedPreferredClass(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewModelAutoscalerReconciler(s, zaptest.NewLogger(t))

	agent := makeAgent("default", "writer", 1)
	agent.Spec.ModelRef = v1alpha1.LocalObjectReference{Name: "model-primary"}
	agentData, _ := json.Marshal(agent)
	if err := s.Create(ctx, "Agent", "default", "writer", agentData); err != nil {
		t.Fatal(err)
	}
	storeModelFixture(t, s, makeModel("default", "model-primary", "primary-class", v1alpha1.PhaseReady))
	storeModelFixture(t, s, makeModel("default", "model-fallback", "fallback-class", v1alpha1.PhaseReady))
	storeModelClass(t, s, makeModelClass("default", "primary-class", "fallback-class", v1alpha1.PhaseDegraded))
	storeModelClass(t, s, makeModelClass("default", "fallback-class", "", v1alpha1.PhaseReady))
	storeModelAutoscalerFixture(t, s, makeModelAutoscaler("default", "writer-model-scale", "writer", "model-primary"))

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "writer-model-scale", Kind: modelAutoscalerKind}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "writer-model-scale", Kind: modelAutoscalerKind}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	gotAgent := getAutoscalerAgent(t, s, "default", "writer")
	if gotAgent.Spec.ModelRef.Name != "model-fallback" {
		t.Fatalf("expected fallback model, got %q", gotAgent.Spec.ModelRef.Name)
	}
	gotMAS := getModelAutoscalerFixture(t, s, "default", "writer-model-scale")
	if gotMAS.Status.CurrentModelRef != "model-fallback" {
		t.Fatalf("expected currentModelRef=model-fallback, got %q", gotMAS.Status.CurrentModelRef)
	}
}

func TestModelAutoscalerReconciler_ConflictRetryPreservesConcurrentAgentChanges(t *testing.T) {
	ctx := context.Background()
	base := newMemStore()
	s := &concurrentAgentEditStore{memStore: base}
	r := NewModelAutoscalerReconciler(s, zaptest.NewLogger(t))

	agent := makeAgent("default", "writer", 1)
	agent.Spec.ModelRef = v1alpha1.LocalObjectReference{Name: "model-primary"}
	agentData, _ := json.Marshal(agent)
	if err := base.Create(ctx, "Agent", "default", "writer", agentData); err != nil {
		t.Fatal(err)
	}

	storeModelFixture(t, base, makeModel("default", "model-primary", "primary-class", v1alpha1.PhaseReady))
	storeModelFixture(t, base, makeModel("default", "model-fallback", "fallback-class", v1alpha1.PhaseReady))
	storeModelClass(t, base, makeModelClass("default", "primary-class", "fallback-class", v1alpha1.PhaseDegraded))
	storeModelAutoscalerFixture(t, base, makeModelAutoscaler("default", "writer-model-scale", "writer", "model-primary"))

	request := Request{Namespace: "default", Name: "writer-model-scale", Kind: modelAutoscalerKind}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("switch model after conflict: %v", err)
	}

	got := getAutoscalerAgent(t, base, "default", "writer")
	if got.Spec.ModelRef.Name != "model-fallback" {
		t.Fatalf("agent model = %q, want model-fallback", got.Spec.ModelRef.Name)
	}
	if got.Spec.Instructions.Inline != "concurrently updated instructions" {
		t.Fatalf("agent instructions = %q, want concurrent edit preserved", got.Spec.Instructions.Inline)
	}
}

func TestModelAutoscalerReconciler_FailedPreferredModelDoesNotTriggerClassFallback(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewModelAutoscalerReconciler(s, zaptest.NewLogger(t))
	agent := makeAgent("default", "writer", 1)
	agent.Spec.ModelRef = v1alpha1.LocalObjectReference{Name: "model-primary"}
	agentData, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, "Agent", "default", "writer", agentData); err != nil {
		t.Fatal(err)
	}
	preferred := makeModel("default", "model-primary", "primary-class", v1alpha1.PhaseFailed)
	preferred.Status.Conditions = []v1alpha1.Condition{{Type: "Ready", Status: "False", Reason: "QuotaOrBillingError", Message: "passive health: recent model calls are failing with QuotaOrBillingError"}}
	storeModelFixture(t, s, preferred)
	storeModelFixture(t, s, makeModel("default", "model-fallback", "fallback-class", v1alpha1.PhaseReady))
	storeModelClass(t, s, makeModelClass("default", "primary-class", "fallback-class", v1alpha1.PhaseReady))
	storeModelClass(t, s, makeModelClass("default", "fallback-class", "", v1alpha1.PhaseReady))
	storeModelAutoscalerFixture(t, s, makeModelAutoscaler("default", "writer-model-scale", "writer", "model-primary"))
	for range 2 {
		if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "writer-model-scale", Kind: modelAutoscalerKind}); err != nil {
			t.Fatal(err)
		}
	}
	if got := getAutoscalerAgent(t, s, "default", "writer"); got.Spec.ModelRef.Name != "model-primary" {
		t.Fatalf("modelRef = %q, want a healthy class to retain the preferred Model", got.Spec.ModelRef.Name)
	}
	if got := getModelAutoscalerFixture(t, s, "default", "writer-model-scale"); got.Status.CurrentModelRef != "model-primary" {
		t.Fatalf("autoscaler status = %+v, want no class-triggered fallback", got.Status)
	}
}

func TestModelAutoscalerReconciler_StatusConflictPreservesScaleTransitionAndConcurrentState(t *testing.T) {
	ctx := context.Background()
	base := newMemStore()
	s := &concurrentModelAutoscalerEditStore{memStore: base}
	r := NewModelAutoscalerReconciler(s, zaptest.NewLogger(t))

	agent := makeAgent("default", "writer", 1)
	agent.Spec.ModelRef = v1alpha1.LocalObjectReference{Name: "model-primary"}
	agentData, _ := json.Marshal(agent)
	if err := base.Create(ctx, "Agent", "default", "writer", agentData); err != nil {
		t.Fatal(err)
	}
	storeModelFixture(t, base, makeModel("default", "model-primary", "primary-class", v1alpha1.PhaseReady))
	storeModelFixture(t, base, makeModel("default", "model-fallback", "fallback-class", v1alpha1.PhaseReady))
	storeModelClass(t, base, makeModelClass("default", "primary-class", "fallback-class", v1alpha1.PhaseDegraded))
	mas := makeModelAutoscaler("default", "writer-model-scale", "writer", "model-primary")
	mas.ObjectMeta.Finalizers = []string{ModelAutoscalerFinalizer}
	storeModelAutoscalerFixture(t, base, mas)

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: mas.ObjectMeta.Name, Kind: modelAutoscalerKind}); err != nil {
		t.Fatalf("switch model after autoscaler conflict: %v", err)
	}
	if !s.conflicted {
		t.Fatal("expected autoscaler conflict fixture to execute")
	}
	if s.agentUpdates != 1 {
		t.Fatalf("agent update attempts = %d, want exactly 1", s.agentUpdates)
	}

	got := getModelAutoscalerFixture(t, base, "default", mas.ObjectMeta.Name)
	if got.Status.LastScaleTime == nil || got.Status.CurrentModelRef != "model-fallback" || got.Status.DesiredModelRef != "model-fallback" {
		t.Fatalf("scale transition was not persisted: %+v", got.Status)
	}
	if got.ObjectMeta.Annotations["example.com/audit"] != "concurrent" {
		t.Fatalf("concurrent annotations were lost: %+v", got.ObjectMeta.Annotations)
	}
	if condition := findCondition(got.Status.Conditions, "Audited"); condition == nil || condition.Reason != "ConcurrentAudit" {
		t.Fatalf("concurrent condition was lost: %+v", condition)
	}
}

func TestModelAutoscalerReconciler_StatusConflictRetryIsBounded(t *testing.T) {
	ctx := context.Background()
	base := newMemStore()
	s := &conflictingModelAutoscalerStore{memStore: base}
	r := NewModelAutoscalerReconciler(s, zaptest.NewLogger(t))
	mas := makeModelAutoscaler("default", "writer-model-scale", "writer", "model-primary")
	storeModelAutoscalerFixture(t, base, mas)

	_, err := r.Reconcile(ctx, Request{Namespace: "default", Name: mas.ObjectMeta.Name, Kind: modelAutoscalerKind})
	if err == nil || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("reconcile error = %v, want conflict", err)
	}
	if s.updates != 5 {
		t.Fatalf("autoscaler update attempts = %d, want 5", s.updates)
	}
	if got := getModelAutoscalerFixture(t, base, "default", mas.ObjectMeta.Name); len(got.ObjectMeta.Finalizers) != 0 {
		t.Fatalf("finalizers = %v, want unchanged persisted state", got.ObjectMeta.Finalizers)
	}
}

func TestModelAutoscalerReconciler_ConflictRetryIsBounded(t *testing.T) {
	ctx := context.Background()
	base := newMemStore()
	s := &conflictingAgentStore{memStore: base}
	r := NewModelAutoscalerReconciler(s, zaptest.NewLogger(t))

	agent := makeAgent("default", "writer", 1)
	agent.Spec.ModelRef = v1alpha1.LocalObjectReference{Name: "model-primary"}
	agentData, _ := json.Marshal(agent)
	if err := base.Create(ctx, "Agent", "default", "writer", agentData); err != nil {
		t.Fatal(err)
	}
	storeModelFixture(t, base, makeModel("default", "model-primary", "primary-class", v1alpha1.PhaseReady))
	storeModelFixture(t, base, makeModel("default", "model-fallback", "fallback-class", v1alpha1.PhaseReady))
	storeModelClass(t, base, makeModelClass("default", "primary-class", "fallback-class", v1alpha1.PhaseDegraded))
	mas := makeModelAutoscaler("default", "writer-model-scale", "writer", "model-primary")
	mas.ObjectMeta.Finalizers = []string{ModelAutoscalerFinalizer}
	storeModelAutoscalerFixture(t, base, mas)

	_, err := r.Reconcile(ctx, Request{Namespace: "default", Name: mas.ObjectMeta.Name, Kind: modelAutoscalerKind})
	if err == nil || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("reconcile error = %v, want conflict", err)
	}
	if s.updates != 5 {
		t.Fatalf("agent update attempts = %d, want 5", s.updates)
	}
	if got := getAutoscalerAgent(t, base, "default", "writer").Spec.ModelRef.Name; got != "model-primary" {
		t.Fatalf("agent model = %q, want unchanged model-primary", got)
	}
}

func TestModelAutoscalerReconciler_StabilizationWindowBlocksModelSwitch(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewModelAutoscalerReconciler(s, zaptest.NewLogger(t))

	agent := makeAgent("default", "writer", 1)
	agent.Spec.ModelRef = v1alpha1.LocalObjectReference{Name: "model-primary"}
	agentData, _ := json.Marshal(agent)
	if err := s.Create(ctx, "Agent", "default", "writer", agentData); err != nil {
		t.Fatal(err)
	}
	storeModelFixture(t, s, makeModel("default", "model-primary", "primary-class", v1alpha1.PhaseReady))
	storeModelFixture(t, s, makeModel("default", "model-fallback", "fallback-class", v1alpha1.PhaseReady))
	storeModelClass(t, s, makeModelClass("default", "primary-class", "fallback-class", v1alpha1.PhaseDegraded))
	mas := makeModelAutoscaler("default", "writer-model-scale", "writer", "model-primary")
	mas.ObjectMeta.Finalizers = []string{ModelAutoscalerFinalizer}
	mas.Spec.StabilizationWindowSeconds = 120
	lastScaleTime := time.Now().UTC()
	mas.Status.LastScaleTime = &lastScaleTime
	storeModelAutoscalerFixture(t, s, mas)

	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: mas.ObjectMeta.Name, Kind: modelAutoscalerKind})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > 120*time.Second {
		t.Fatalf("requeue after = %s, want within stabilization window", result.RequeueAfter)
	}
	if got := getAutoscalerAgent(t, s, "default", "writer").Spec.ModelRef.Name; got != "model-primary" {
		t.Fatalf("agent model = %q, want model-primary while stabilization is active", got)
	}
	gotMAS := getModelAutoscalerFixture(t, s, "default", mas.ObjectMeta.Name)
	if gotMAS.Status.CurrentModelRef != "model-primary" || gotMAS.Status.DesiredModelRef != "model-fallback" {
		t.Fatalf("model status = current %q desired %q, want model-primary and model-fallback", gotMAS.Status.CurrentModelRef, gotMAS.Status.DesiredModelRef)
	}
	if len(gotMAS.Status.Conditions) == 0 || gotMAS.Status.Conditions[0].Reason != "ScalingBlocked" {
		t.Fatalf("conditions = %+v, want ScalingBlocked", gotMAS.Status.Conditions)
	}
}

func TestModelAutoscalerReconciler_RevertsToPreferredWhenRecovered(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewModelAutoscalerReconciler(s, zaptest.NewLogger(t))

	agent := makeAgent("default", "writer", 1)
	agent.Spec.ModelRef = v1alpha1.LocalObjectReference{Name: "model-fallback"}
	agentData, _ := json.Marshal(agent)
	if err := s.Create(ctx, "Agent", "default", "writer", agentData); err != nil {
		t.Fatal(err)
	}
	storeModelFixture(t, s, makeModel("default", "model-primary", "primary-class", v1alpha1.PhaseReady))
	storeModelFixture(t, s, makeModel("default", "model-fallback", "fallback-class", v1alpha1.PhaseReady))
	storeModelClass(t, s, makeModelClass("default", "primary-class", "fallback-class", v1alpha1.PhaseReady))
	storeModelClass(t, s, makeModelClass("default", "fallback-class", "", v1alpha1.PhaseReady))
	mas := makeModelAutoscaler("default", "writer-model-scale", "writer", "model-primary")
	now := time.Now().UTC().Add(-time.Minute)
	mas.Status.LastScaleTime = &now
	storeModelAutoscalerFixture(t, s, mas)

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "writer-model-scale", Kind: modelAutoscalerKind}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "writer-model-scale", Kind: modelAutoscalerKind}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	gotAgent := getAutoscalerAgent(t, s, "default", "writer")
	if gotAgent.Spec.ModelRef.Name != "model-primary" {
		t.Fatalf("expected preferred model after recovery, got %q", gotAgent.Spec.ModelRef.Name)
	}
}

func TestModelAutoscalerReconciler_DegradedWithoutFallbackModelStaysNotReady(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewModelAutoscalerReconciler(s, zaptest.NewLogger(t))

	agent := makeAgent("default", "writer", 1)
	agent.Spec.ModelRef = v1alpha1.LocalObjectReference{Name: "model-primary"}
	agentData, _ := json.Marshal(agent)
	if err := s.Create(ctx, "Agent", "default", "writer", agentData); err != nil {
		t.Fatal(err)
	}
	storeModelFixture(t, s, makeModel("default", "model-primary", "primary-class", v1alpha1.PhaseReady))
	storeModelClass(t, s, makeModelClass("default", "primary-class", "fallback-class", v1alpha1.PhaseDegraded))
	storeModelAutoscalerFixture(t, s, makeModelAutoscaler("default", "writer-model-scale", "writer", "model-primary"))

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "writer-model-scale", Kind: modelAutoscalerKind}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "writer-model-scale", Kind: modelAutoscalerKind}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	gotAgent := getAutoscalerAgent(t, s, "default", "writer")
	if gotAgent.Spec.ModelRef.Name != "model-primary" {
		t.Fatalf("expected agent to stay on preferred model, got %q", gotAgent.Spec.ModelRef.Name)
	}
	gotMAS := getModelAutoscalerFixture(t, s, "default", "writer-model-scale")
	if len(gotMAS.Status.Conditions) == 0 || gotMAS.Status.Conditions[0].Status != "False" {
		t.Fatalf("expected not-ready condition when fallback model is unavailable, got %+v", gotMAS.Status.Conditions)
	}
}

func TestModelAutoscalerReconciler_FallbackRequiresExplicitReadyPhase(t *testing.T) {
	base := newMemStore()
	model := makeModel("default", "uninitialized", "fallback-class", "")
	raw, _ := json.Marshal(model)
	r := NewModelAutoscalerReconciler(&orderedModelListStore{memStore: base, models: [][]byte{raw}}, zaptest.NewLogger(t))

	if _, err := r.findFallbackModel(context.Background(), "default", "fallback-class", "primary"); err == nil {
		t.Fatal("expected an uninitialized fallback model to be rejected")
	}
}

func TestModelAutoscalerReconciler_SelectsFallbackDeterministically(t *testing.T) {
	base := newMemStore()
	zulu, _ := json.Marshal(makeModel("default", "zulu", "fallback-class", v1alpha1.PhaseReady))
	alpha, _ := json.Marshal(makeModel("default", "alpha", "fallback-class", v1alpha1.PhaseReady))
	r := NewModelAutoscalerReconciler(&orderedModelListStore{memStore: base, models: [][]byte{zulu, alpha}}, zaptest.NewLogger(t))

	model, err := r.findFallbackModel(context.Background(), "default", "fallback-class", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if model.ObjectMeta.Name != "alpha" {
		t.Fatalf("selected fallback = %q, want deterministic first name alpha", model.ObjectMeta.Name)
	}
}

func TestModelAutoscalerReconciler_RestoresPreferredModelBeforeDeletion(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewModelAutoscalerReconciler(s, zaptest.NewLogger(t))

	agent := makeAgent("default", "writer", 1)
	agent.Spec.ModelRef.Name = "model-fallback"
	agentData, _ := json.Marshal(agent)
	if err := s.Create(ctx, "Agent", "default", "writer", agentData); err != nil {
		t.Fatal(err)
	}
	mas := makeModelAutoscaler("default", "writer-model-scale", "writer", "model-primary")
	now := time.Now().UTC()
	mas.ObjectMeta.DeletionTimestamp = &now
	mas.ObjectMeta.Finalizers = []string{ModelAutoscalerFinalizer}
	storeModelAutoscalerFixture(t, s, mas)

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: mas.ObjectMeta.Name}); err != nil {
		t.Fatal(err)
	}
	if got := getAutoscalerAgent(t, s, "default", "writer").Spec.ModelRef.Name; got != "model-primary" {
		t.Fatalf("agent model after autoscaler deletion = %q, want model-primary", got)
	}
}

func TestModelAutoscalerReconciler_DeletionCompletesWhenAgentIsMissing(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewModelAutoscalerReconciler(s, zaptest.NewLogger(t))

	mas := makeModelAutoscaler("default", "writer-model-scale", "missing-agent", "model-primary")
	now := time.Now().UTC()
	mas.ObjectMeta.DeletionTimestamp = &now
	mas.ObjectMeta.Finalizers = []string{ModelAutoscalerFinalizer}
	storeModelAutoscalerFixture(t, s, mas)

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: mas.ObjectMeta.Name, Kind: modelAutoscalerKind}); err != nil {
		t.Fatal(err)
	}
	got := getModelAutoscalerFixture(t, s, "default", mas.ObjectMeta.Name)
	if len(got.ObjectMeta.Finalizers) != 0 {
		t.Fatalf("finalizers = %v, want cleanup finalizer removed", got.ObjectMeta.Finalizers)
	}
}

func TestModelAutoscalerReconciler_WrappedNotFoundIsComplete(t *testing.T) {
	s := &modelAutoscalerGetFaultStore{memStore: newMemStore(), kind: modelAutoscalerKind}
	r := NewModelAutoscalerReconciler(s, zaptest.NewLogger(t))
	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "deleted"})
	if err != nil || result != (Result{}) {
		t.Fatalf("result = %+v, error = %v, want completed not-found reconciliation", result, err)
	}
}

func TestModelAutoscalerReconciler_UnresolvedDependenciesStayRecoverable(t *testing.T) {
	tests := []struct {
		name            string
		notFoundKind    string
		agentRef        string
		preferredModel  string
		storeAgent      bool
		storePreferred  bool
		preferredClass  string
		wantReason      string
		wantMessagePart string
	}{
		{name: "empty agent reference", preferredModel: "model-primary", wantReason: "AgentNotFound", wantMessagePart: "agentRef.name is required"},
		{name: "missing agent", notFoundKind: "Agent", agentRef: "writer", preferredModel: "model-primary", wantReason: "AgentNotFound", wantMessagePart: `agent "writer" not found`},
		{name: "empty preferred model reference", agentRef: "writer", storeAgent: true, wantReason: "PreferredModelNotFound", wantMessagePart: "modelRef.name is required"},
		{name: "missing preferred model", notFoundKind: "Model", agentRef: "writer", preferredModel: "model-primary", storeAgent: true, wantReason: "PreferredModelNotFound", wantMessagePart: `model "model-primary" not found`},
		{name: "empty preferred model class", agentRef: "writer", preferredModel: "model-primary", storeAgent: true, storePreferred: true, wantReason: "PreferredModelClassNotFound", wantMessagePart: "model class name is required"},
		{name: "missing preferred model class", notFoundKind: modelClassKind, agentRef: "writer", preferredModel: "model-primary", storeAgent: true, storePreferred: true, preferredClass: "primary-class", wantReason: "PreferredModelClassNotFound", wantMessagePart: `modelClass "primary-class" not found`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s := &modelAutoscalerGetFaultStore{memStore: newMemStore(), kind: tt.notFoundKind}
			r := NewModelAutoscalerReconciler(s, zaptest.NewLogger(t))

			if tt.storeAgent {
				agent := makeAgent("default", "writer", 1)
				agent.Spec.ModelRef.Name = "original-model"
				raw, err := json.Marshal(agent)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.memStore.Create(ctx, "Agent", "default", agent.ObjectMeta.Name, raw); err != nil {
					t.Fatal(err)
				}
			}
			if tt.storePreferred {
				storeModelFixture(t, s.memStore, makeModel("default", "model-primary", tt.preferredClass, v1alpha1.PhaseReady))
			}

			mas := makeModelAutoscaler("default", "writer-model-scale", tt.agentRef, tt.preferredModel)
			mas.ObjectMeta.Finalizers = []string{ModelAutoscalerFinalizer}
			storeModelAutoscalerFixture(t, s.memStore, mas)

			result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: mas.ObjectMeta.Name, Kind: modelAutoscalerKind})
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if result.RequeueAfter != modelAutoscalerRequeue {
				t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, modelAutoscalerRequeue)
			}

			got := getModelAutoscalerFixture(t, s.memStore, "default", mas.ObjectMeta.Name)
			condition := findCondition(got.Status.Conditions, "Ready")
			if condition == nil || condition.Status != "False" || condition.Reason != tt.wantReason || !strings.Contains(condition.Message, tt.wantMessagePart) {
				t.Fatalf("Ready condition = %+v, want False/%s containing %q", condition, tt.wantReason, tt.wantMessagePart)
			}
			if tt.storeAgent {
				if model := getAutoscalerAgent(t, s.memStore, "default", "writer").Spec.ModelRef.Name; model != "original-model" {
					t.Fatalf("agent model = %q, want original-model", model)
				}
			}
		})
	}
}
