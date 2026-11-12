// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
)

// Integration tests exercise the full ModelClass reconciler lifecycle
// using the in-memory store: creation → Registered → Validating → Ready →
// version pin change → Degraded → recovery → deletion with drain.

// TC-INT-001: Full ModelClass lifecycle from creation through deletion.
func TestIntegration_ModelClass_FullLifecycle(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := testRegistry()
	r := NewModelClassReconciler(s, reg, logger)
	ctx := context.Background()
	ns := "production"

	// --- Phase 1: Create ModelClass with initial version pin ---
	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "gpt4-production",
			Namespace: ns,
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "low",
			CostTier:     "premium",
			VersionPin:   "gpt-4-0613",
			HealthProbe: &v1alpha1.HealthProbeSpec{
				Endpoint:         "https://api.openai.com/v1/health",
				IntervalSeconds:  30,
				TimeoutSeconds:   5,
				SuccessThreshold: 1,
			},
		},
	}
	storeModelClass(t, s, mc)

	// --- Phase 2: First reconcile → adds finalizer, sets Registered ---
	res := reconcileModelClass(t, r, ns, "gpt4-production")
	if !res.Requeue {
		t.Fatal("Phase 2: expected requeue after adding finalizer")
	}
	got := getModelClass(t, s, ns, "gpt4-production")
	if got.Status.Phase != v1alpha1.PhaseRegistered {
		t.Fatalf("Phase 2: expected Registered, got %s", got.Status.Phase)
	}
	assertHasFinalizer(t, got.ObjectMeta.Finalizers, modelClassFinalizer)

	// --- Phase 3: Second reconcile → validates config → Validating ---
	res = reconcileModelClass(t, r, ns, "gpt4-production")
	if !res.Requeue {
		t.Fatal("Phase 3: expected requeue after advancing to Validating")
	}
	got = getModelClass(t, s, ns, "gpt4-production")
	if got.Status.Phase != v1alpha1.PhaseValidating {
		t.Fatalf("Phase 3: expected Validating, got %s", got.Status.Phase)
	}

	// --- Phase 4: Third reconcile → driver found → Ready ---
	res = reconcileModelClass(t, r, ns, "gpt4-production")
	if res.Requeue || res.RequeueAfter != 0 {
		t.Fatal("Phase 4: expected no requeue when Ready")
	}
	got = getModelClass(t, s, ns, "gpt4-production")
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("Phase 4: expected Ready, got %s", got.Status.Phase)
	}
	if !got.Status.Provisioned {
		t.Fatal("Phase 4: expected Provisioned=true")
	}
	if got.Status.ActiveVersion != "gpt-4-0613" {
		t.Fatalf("Phase 4: expected ActiveVersion gpt-4-0613, got %s", got.Status.ActiveVersion)
	}
	if got.Status.Health != "Healthy" {
		t.Fatalf("Phase 4: expected Health Healthy, got %s", got.Status.Health)
	}
	assertCondition(t, got.Status.Conditions, "Ready", "True", "Provisioned")

	// --- Phase 5: Version pin change (canary promotion) ---
	got.Spec.VersionPin = "gpt-4-0125-preview"
	updateModelClassInStore(t, s, got)

	res = reconcileModelClass(t, r, ns, "gpt4-production")
	if res.Requeue || res.RequeueAfter != 0 {
		t.Fatal("Phase 5: expected no requeue after successful promotion")
	}
	got = getModelClass(t, s, ns, "gpt4-production")
	if got.Status.ActiveVersion != "gpt-4-0125-preview" {
		t.Fatalf("Phase 5: expected ActiveVersion gpt-4-0125-preview, got %s", got.Status.ActiveVersion)
	}
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("Phase 5: expected phase Ready after promotion, got %s", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, "Ready", "True", "VersionPromoted")

	// --- Phase 6: Version pin change with driver unavailable → rollback to Degraded ---
	emptyReg := cpi.NewRegistry()
	rNoDriver := NewModelClassReconciler(s, emptyReg, logger)

	got = getModelClass(t, s, ns, "gpt4-production")
	got.Spec.VersionPin = "gpt-4-turbo-2024-04-09"
	updateModelClassInStore(t, s, got)

	res = reconcileModelClass(t, rNoDriver, ns, "gpt4-production")
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("Phase 6: expected 30s requeue after rollback, got %v", res.RequeueAfter)
	}
	got = getModelClass(t, s, ns, "gpt4-production")
	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("Phase 6: expected Degraded, got %s", got.Status.Phase)
	}
	if got.Status.ActiveVersion != "gpt-4-0125-preview" {
		t.Fatalf("Phase 6: expected rollback to gpt-4-0125-preview, got %s", got.Status.ActiveVersion)
	}
	if got.Status.Health != "Degraded" {
		t.Fatalf("Phase 6: expected Health Degraded, got %s", got.Status.Health)
	}

	// --- Phase 7: Still degraded (driver unavailable) → requeue ---
	res = reconcileModelClass(t, rNoDriver, ns, "gpt4-production")
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("Phase 7: expected 30s requeue while still degraded, got %v", res.RequeueAfter)
	}
	got = getModelClass(t, s, ns, "gpt4-production")
	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("Phase 7: expected to remain Degraded, got %s", got.Status.Phase)
	}

	// --- Phase 8: Recovery when driver comes back → Ready ---
	res = reconcileModelClass(t, r, ns, "gpt4-production")
	if res.Requeue || res.RequeueAfter != 0 {
		t.Fatal("Phase 8: expected no requeue after recovery")
	}
	got = getModelClass(t, s, ns, "gpt4-production")
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("Phase 8: expected Ready after recovery, got %s", got.Status.Phase)
	}
	if got.Status.Health != "Healthy" {
		t.Fatalf("Phase 8: expected Health Healthy after recovery, got %s", got.Status.Health)
	}
	assertCondition(t, got.Status.Conditions, "Ready", "True", "Recovered")

	// --- Phase 9: Initiate deletion with active model (drain) ---
	// Create a dependent Model
	model := v1alpha1.Model{
		TypeMeta: v1alpha1.TypeMeta{Kind: "Model", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "gpt4-turbo-instance",
			Namespace: ns,
		},
		Spec: v1alpha1.ModelSpec{
			ClassName: "gpt4-production",
			Provider:  "openai",
			Endpoint:  "https://api.openai.com/v1",
		},
		Status: v1alpha1.ModelStatus{
			Phase: v1alpha1.PhaseReady,
		},
	}
	storeModel(t, s, model)

	// Set deletion timestamp on the ModelClass
	got = getModelClass(t, s, ns, "gpt4-production")
	now := time.Now()
	got.ObjectMeta.DeletionTimestamp = &now
	updateModelClassInStore(t, s, got)

	// Reconcile → should drain (requeue because model is active)
	res = reconcileModelClass(t, r, ns, "gpt4-production")
	if res.RequeueAfter != 5*time.Second {
		t.Fatalf("Phase 9: expected 5s requeue while draining, got %v", res.RequeueAfter)
	}
	got = getModelClass(t, s, ns, "gpt4-production")
	if got.Status.Phase != v1alpha1.PhaseTerminated {
		t.Fatalf("Phase 9: expected Terminated during drain, got %s", got.Status.Phase)
	}
	assertHasFinalizer(t, got.ObjectMeta.Finalizers, modelClassFinalizer)
	assertCondition(t, got.Status.Conditions, "Ready", "False", "Draining")

	// --- Phase 10: Model terminates → finalizer removed ---
	currentModel := getModel(t, s, ns, "gpt4-turbo-instance")
	currentModel.Status.Phase = v1alpha1.PhaseTerminated
	data, _ := json.Marshal(currentModel)
	_ = s.Update(ctx, "Model", ns, "gpt4-turbo-instance", data, currentModel.ObjectMeta.ResourceVersion)

	res = reconcileModelClass(t, r, ns, "gpt4-production")
	if res.Requeue || res.RequeueAfter != 0 {
		t.Fatal("Phase 10: expected no requeue after drain completes")
	}
	got = getModelClass(t, s, ns, "gpt4-production")
	assertNoFinalizer(t, got.ObjectMeta.Finalizers, modelClassFinalizer)
	assertCondition(t, got.Status.Conditions, "Ready", "False", "Terminated")
}

// TC-INT-002: Invalid config prevents reaching Ready.
func TestIntegration_ModelClass_InvalidConfig(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := testRegistry()
	r := NewModelClassReconciler(s, reg, logger)
	ns := "default"

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "bad-config",
			Namespace: ns,
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "ultra-fast", // invalid
		},
	}
	storeModelClass(t, s, mc)

	// Three reconciles: Registered → config validation → Failed
	reconcileModelClass(t, r, ns, "bad-config")
	reconcileModelClass(t, r, ns, "bad-config")
	got := getModelClass(t, s, ns, "bad-config")

	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected Failed for invalid config, got %s", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, "Ready", "False", "LatencyClassInvalid")

	// Subsequent reconcile is no-op (terminal)
	res := reconcileModelClass(t, r, ns, "bad-config")
	if res.Requeue || res.RequeueAfter != 0 {
		t.Fatal("expected no requeue for Failed state")
	}
}

// TC-INT-003: Idempotent reconcile on Ready with no drift.
func TestIntegration_ModelClass_ReadyIdempotent(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := testRegistry()
	r := NewModelClassReconciler(s, reg, logger)
	ns := "default"

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "stable-class",
			Namespace:  ns,
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "openai",
			VersionPin:  "gpt-4-0613",
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:         v1alpha1.PhaseReady,
			Provisioned:   true,
			ActiveVersion: "gpt-4-0613",
			Health:        "Healthy",
		},
	}
	storeModelClass(t, s, mc)

	// Multiple reconciles should be no-ops
	for i := 0; i < 5; i++ {
		res := reconcileModelClass(t, r, ns, "stable-class")
		if res.Requeue || res.RequeueAfter != 0 {
			t.Fatalf("reconcile %d: expected no requeue for stable Ready state", i+1)
		}
	}

	got := getModelClass(t, s, ns, "stable-class")
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected phase to remain Ready, got %s", got.Status.Phase)
	}
}

// --- Integration test helpers ---

func updateModelClassInStore(t *testing.T, s *memStore, mc v1alpha1.ModelClass) {
	t.Helper()
	data, err := json.Marshal(mc)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(context.Background(), modelClassKind, mc.ObjectMeta.Namespace, mc.ObjectMeta.Name, data, mc.ObjectMeta.ResourceVersion); err != nil {
		t.Fatal(err)
	}
}

func assertHasFinalizer(t *testing.T, finalizers []string, expected string) {
	t.Helper()
	for _, f := range finalizers {
		if f == expected {
			return
		}
	}
	t.Fatalf("expected finalizer %q to be present", expected)
}

func assertNoFinalizer(t *testing.T, finalizers []string, unexpected string) {
	t.Helper()
	for _, f := range finalizers {
		if f == unexpected {
			t.Fatalf("expected finalizer %q to be absent", unexpected)
		}
	}
}

func assertCondition(t *testing.T, conditions []v1alpha1.Condition, condType, status, reason string) {
	t.Helper()
	for _, c := range conditions {
		if c.Type == condType {
			if c.Status != status {
				t.Fatalf("condition %s: expected status %q, got %q", condType, status, c.Status)
			}
			if c.Reason != reason {
				t.Fatalf("condition %s: expected reason %q, got %q", condType, reason, c.Reason)
			}
			return
		}
	}
	t.Fatalf("condition %q not found in conditions", condType)
}
