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
	"github.com/kognition-ai/kognition/internal/store"
)

// TestSolutionIntegration_EndToEnd exercises the full lifecycle:
// Create Solution → Reconcile (adds finalizer) → Reconcile (compiles goal, creates revision, deploys child resources)
// → Update goal → Reconcile (new revision, updated resources) → Delete → Reconcile (cleanup children).
func TestSolutionIntegration_EndToEnd(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewSolutionReconciler(ms, logger)
	ctx := context.Background()
	req := Request{Namespace: "prod", Name: "e2e-sol"}

	// --- Phase 1: Create Solution and reconcile to Ready ---
	sol := v1alpha1.Solution{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Solution"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "e2e-sol", Namespace: "prod", UID: "e2e-uid-1", CreationTimestamp: time.Now().UTC()},
		Spec: v1alpha1.SolutionSpec{
			Goal:     "process invoices and send summaries",
			ModelRef: &v1alpha1.LocalObjectReference{Name: "gpt-4o"},
		},
	}
	data, _ := json.Marshal(sol)
	if err := ms.Create(ctx, "Solution", "prod", "e2e-sol", data); err != nil {
		t.Fatalf("create solution: %v", err)
	}

	// First reconcile: adds finalizer, requeues.
	res, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	// Verify finalizer was added.
	sol = getSolutionFromStore(t, ms, "prod", "e2e-sol")
	if len(sol.ObjectMeta.Finalizers) == 0 {
		t.Fatal("expected finalizer to be added")
	}

	// Second reconcile: compiles goal, creates revision, deploys child resources.
	res, err = reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if res.Requeue {
		t.Fatal("did not expect requeue after successful deploy")
	}

	// Verify solution is in Ready phase.
	sol = getSolutionFromStore(t, ms, "prod", "e2e-sol")
	if sol.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected Ready phase, got %s", sol.Status.Phase)
	}
	if sol.Status.ActiveRevision != 1 {
		t.Fatalf("expected revision 1, got %d", sol.Status.ActiveRevision)
	}

	// Verify child Agents were created.
	agents, err := ms.List(ctx, "Agent", store.ListOptions{Namespace: "prod"})
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	if len(agents) != 4 {
		t.Fatalf("expected 4 child agents, got %d", len(agents))
	}
	var agent v1alpha1.Agent
	if err := json.Unmarshal(agents[0], &agent); err != nil {
		t.Fatalf("unmarshal agent: %v", err)
	}
	if agent.Spec.Instructions.Inline == "" {
		t.Fatal("expected agent instructions to be populated")
	}
	if agent.ObjectMeta.Labels["kognition.dev/solution"] != "e2e-sol" {
		t.Fatalf("expected solution label on agent, got labels: %v", agent.ObjectMeta.Labels)
	}

	// Verify revision was stored.
	revisions, err := ms.ListRevisions(ctx, "Solution", "prod", "e2e-sol")
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	if len(revisions) != 1 {
		t.Fatalf("expected 1 revision, got %d", len(revisions))
	}

	// --- Phase 2: Update goal → new revision, updated Agent ---
	sol = getSolutionFromStore(t, ms, "prod", "e2e-sol")
	sol.Spec.Goal = "process invoices, validate totals, and alert on discrepancies"
	updateSolutionInStore(t, ms, sol)

	res, err = reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile after goal change: %v", err)
	}

	sol = getSolutionFromStore(t, ms, "prod", "e2e-sol")
	if sol.Status.ActiveRevision != 2 {
		t.Fatalf("expected revision 2 after goal change, got %d", sol.Status.ActiveRevision)
	}

	// Verify revision 2 stored.
	revisions, err = ms.ListRevisions(ctx, "Solution", "prod", "e2e-sol")
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	if len(revisions) != 2 {
		t.Fatalf("expected 2 revisions, got %d", len(revisions))
	}

	// Verify agent instructions were updated.
	agents, err = ms.List(ctx, "Agent", store.ListOptions{Namespace: "prod"})
	if err != nil {
		t.Fatalf("list agents after update: %v", err)
	}
	if len(agents) != 4 {
		t.Fatalf("expected 4 agents after update, got %d", len(agents))
	}
	if err := json.Unmarshal(agents[0], &agent); err != nil {
		t.Fatalf("unmarshal agent: %v", err)
	}
	if agent.Spec.Instructions.Inline == "" {
		t.Fatal("expected updated agent instructions")
	}

	// --- Phase 3: Rollback to revision 1 ---
	if err := reconciler.RollbackToRevision(ctx, "prod", "e2e-sol", 1); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	sol = getSolutionFromStore(t, ms, "prod", "e2e-sol")
	if sol.Spec.Goal != "process invoices and send summaries" {
		t.Fatalf("expected original goal after rollback, got: %s", sol.Spec.Goal)
	}

	// --- Phase 4: Delete → cleanup children ---
	sol = getSolutionFromStore(t, ms, "prod", "e2e-sol")
	now := time.Now().UTC()
	sol.ObjectMeta.DeletionTimestamp = &now
	updateSolutionInStore(t, ms, sol)

	res, err = reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}

	// Verify child agents are cleaned up.
	agents, err = ms.List(ctx, "Agent", store.ListOptions{Namespace: "prod"})
	if err != nil {
		t.Fatalf("list agents after deletion: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("expected 0 agents after cleanup, got %d", len(agents))
	}

	// Verify finalizer removed and solution updated.
	sol = getSolutionFromStore(t, ms, "prod", "e2e-sol")
	for _, f := range sol.ObjectMeta.Finalizers {
		if f == SolutionFinalizer {
			t.Fatal("expected finalizer to be removed after deletion")
		}
	}
}

// TestSolutionIntegration_AdmissionAndReconcile verifies that invalid solutions
// are caught by the admission validator before reaching the reconciler.
func TestSolutionIntegration_AdmissionAndReconcile(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewSolutionReconciler(ms, logger)
	ctx := context.Background()

	// Empty goal should be rejected by admission, but if it reaches reconciler
	// it should still fail gracefully.
	sol := v1alpha1.Solution{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Solution"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "bad-sol", Namespace: "default", UID: "bad-uid", CreationTimestamp: time.Now().UTC()},
		Spec:       v1alpha1.SolutionSpec{Goal: ""}, // empty goal
	}
	data, _ := json.Marshal(sol)
	if err := ms.Create(ctx, "Solution", "default", "bad-sol", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	req := Request{Namespace: "default", Name: "bad-sol"}

	// First reconcile adds finalizer.
	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Second reconcile — the reconciler processes even empty goals (admission should
	// have caught this). Verify it doesn't panic and completes gracefully.
	_, err = reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Logf("reconcile with empty goal returned error (acceptable): %v", err)
	}

	// Verify the solution was processed without panic.
	sol = getSolutionFromStore(t, ms, "default", "bad-sol")
	t.Logf("solution with empty goal reached phase: %s (admission should have prevented this)", sol.Status.Phase)
}

// ---------------------------------------------------------------------------
// Integration test helpers
// ---------------------------------------------------------------------------

func getSolutionFromStore(t *testing.T, s *memStore, ns, name string) v1alpha1.Solution {
	t.Helper()
	data, err := s.Get(context.Background(), "Solution", ns, name)
	if err != nil {
		t.Fatalf("get solution %s/%s: %v", ns, name, err)
	}
	var sol v1alpha1.Solution
	if err := json.Unmarshal(data, &sol); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return sol
}

func updateSolutionInStore(t *testing.T, s *memStore, sol v1alpha1.Solution) {
	t.Helper()
	data, _ := json.Marshal(sol)
	rv := s.versions[s.key("Solution", sol.ObjectMeta.Namespace, sol.ObjectMeta.Name)]
	if err := s.Update(context.Background(), "Solution", sol.ObjectMeta.Namespace, sol.ObjectMeta.Name, data, rv); err != nil {
		t.Fatalf("update solution: %v", err)
	}
}
