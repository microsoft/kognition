// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/store"
)

type concurrentSolutionReadyStore struct {
	store.Store
	conflicted   bool
	agentCreates int
}

type persistentSolutionReadyConflictStore struct {
	store.Store
	solutionUpdates int
	agentCreates    int
}

type solutionRevisionFaultStore struct {
	store.Store
	revisionCreates int
}

type solutionLabelCleanupDeleteFailureStore struct {
	store.Store
}

type solutionLabelCleanupListFailureStore struct {
	store.Store
}

type solutionLabelCleanupMalformedStore struct {
	store.Store
}

type solutionQuotaFaultStore struct {
	store.Store
	listErr error
	items   [][]byte
}

func (s *solutionQuotaFaultStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if kind == "CognitiveQuota" {
		if s.listErr != nil {
			return nil, s.listErr
		}
		if s.items != nil {
			return s.items, nil
		}
	}
	return s.Store.List(ctx, kind, opts)
}

func (s *solutionLabelCleanupDeleteFailureStore) Delete(ctx context.Context, kind, namespace, name string) error {
	if kind == "Agent" && name == "owned-agent" {
		return errors.New("storage unavailable")
	}
	return s.Store.Delete(ctx, kind, namespace, name)
}

func (s *solutionLabelCleanupListFailureStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if kind == "Agent" {
		return nil, errors.New("storage unavailable")
	}
	return s.Store.List(ctx, kind, opts)
}

func (s *solutionLabelCleanupMalformedStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if kind == "Agent" {
		return [][]byte{[]byte(`{"metadata":`)}, nil
	}
	return s.Store.List(ctx, kind, opts)
}

func (s *solutionRevisionFaultStore) CreateRevision(context.Context, string, string, string, int64, []byte, []byte) error {
	s.revisionCreates++
	return errors.New("revision storage unavailable")
}

func (s *persistentSolutionReadyConflictStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if kind == "Agent" {
		s.agentCreates++
	}
	return s.Store.Create(ctx, kind, namespace, name, data)
}

func (s *persistentSolutionReadyConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == solutionKind {
		var desired v1alpha1.Solution
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase == v1alpha1.PhaseReady {
			s.solutionUpdates++
			return store.ErrConflict
		}
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *concurrentSolutionReadyStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if kind == "Agent" {
		s.agentCreates++
	}
	return s.Store.Create(ctx, kind, namespace, name, data)
}

func (s *concurrentSolutionReadyStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == solutionKind && !s.conflicted {
		var desired v1alpha1.Solution
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase == v1alpha1.PhaseReady {
			s.conflicted = true
			raw, err := s.Store.Get(ctx, kind, namespace, name)
			if err != nil {
				return err
			}
			var latest v1alpha1.Solution
			if err := json.Unmarshal(raw, &latest); err != nil {
				return err
			}
			latest.Spec.Preferences = map[string]string{"review": "required"}
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
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makeSolution(ns, name, goal string) v1alpha1.Solution {
	return v1alpha1.Solution{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Solution"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: ns, UID: "sol-uid-1", CreationTimestamp: time.Now().UTC()},
		Spec:       v1alpha1.SolutionSpec{Goal: goal},
	}
}

func storeSolution(t *testing.T, s *memStore, sol v1alpha1.Solution) {
	t.Helper()
	data, _ := json.Marshal(sol)
	if err := s.Create(context.Background(), "Solution", sol.ObjectMeta.Namespace, sol.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store solution: %v", err)
	}
}

func getSolution(t *testing.T, s *memStore, ns, name string) v1alpha1.Solution {
	t.Helper()
	data, err := s.Get(context.Background(), "Solution", ns, name)
	if err != nil {
		t.Fatalf("get solution: %v", err)
	}
	var sol v1alpha1.Solution
	if err := json.Unmarshal(data, &sol); err != nil {
		t.Fatalf("unmarshal solution: %v", err)
	}
	return sol
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestSolutionReconciler_HappyPath(t *testing.T) {
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))

	sol := makeSolution("default", "my-sol", "summarize documents")
	sol.Spec.ModelRef = &v1alpha1.LocalObjectReference{Name: "gpt-4"}
	storeSolution(t, ms, sol)

	// First reconcile adds finalizer and requeues.
	res, err := r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "my-sol"})
	if err != nil {
		t.Fatalf("reconcile (finalizer): %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after finalizer add")
	}

	// Second reconcile does actual work.
	res, err = r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "my-sol"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Requeue {
		t.Fatal("unexpected requeue")
	}

	// Verify child Agent was created.
	agentData, err := ms.Get(context.Background(), "Agent", "default", "my-sol-agent")
	if err != nil {
		t.Fatalf("expected child agent to be created: %v", err)
	}
	var agent v1alpha1.Agent
	if err := json.Unmarshal(agentData, &agent); err != nil {
		t.Fatalf("unmarshal agent: %v", err)
	}
	if agent.Spec.Instructions.Inline != "summarize documents" {
		t.Errorf("expected instructions inline = goal, got %q", agent.Spec.Instructions.Inline)
	}
	if agent.Spec.ModelRef.Name != "gpt-4" {
		t.Errorf("expected modelRef gpt-4, got %q", agent.Spec.ModelRef.Name)
	}
	// Check label.
	if agent.ObjectMeta.Labels["kognition.dev/solution"] != "my-sol" {
		t.Errorf("expected solution label, got labels %v", agent.ObjectMeta.Labels)
	}

	// Verify solution status updated to Ready.
	updated := getSolution(t, ms, "default", "my-sol")
	if updated.Status.Phase != v1alpha1.PhaseReady {
		t.Errorf("expected phase Ready, got %v", updated.Status.Phase)
	}
	if updated.Status.ActiveRevision != 1 {
		t.Errorf("expected activeRevision=1, got %d", updated.Status.ActiveRevision)
	}
}

// TC-0293: a failed Evaluation gate blocks promotion with an actionable condition.
func TestSolutionReconciler_PromotionGateBlocksAndThenActivatesCandidateRevision(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))
	sol := makeSolution("default", "gated-sol", "summarize documents")
	sol.Spec.Target = &v1alpha1.SolutionTarget{Kind: "Agent", Ref: v1alpha1.LocalObjectReference{Name: "assistant"}}
	sol.Spec.PromotionGates = []v1alpha1.SolutionPromotionGate{{EvaluationRef: v1alpha1.LocalObjectReference{Name: "quality"}}}
	storeAgent(t, ms, v1alpha1.Agent{TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"}, ObjectMeta: v1alpha1.ObjectMeta{Name: "assistant", Namespace: "default"}})
	storeSolution(t, ms, sol)
	storeEvaluation(t, ms, v1alpha1.Evaluation{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Evaluation"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "quality", Namespace: "default", UID: "eval-uid"},
		Spec:       v1alpha1.EvaluationSpec{DatasetRef: v1alpha1.EvaluationDatasetRef{Name: "smoke"}},
	})

	if _, err := r.Reconcile(ctx, Request{Kind: "Solution", Namespace: "default", Name: "gated-sol"}); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	result, err := r.Reconcile(ctx, Request{Kind: "Solution", Namespace: "default", Name: "gated-sol"})
	if err != nil {
		t.Fatalf("create gated candidate: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("blocked candidate was not requeued")
	}
	blocked := getSolution(t, ms, "default", "gated-sol")
	if blocked.Status.ActiveRevision != 0 || blocked.Status.CandidateRevision != 1 {
		t.Fatalf("blocked revisions = active %d candidate %d", blocked.Status.ActiveRevision, blocked.Status.CandidateRevision)
	}
	if _, err := ms.Get(ctx, "AgentJob", "default", "gated-sol-agent-job"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("candidate AgentJob deployed before gate passed: %v", err)
	}
	evaluation := getEvaluation(t, ms, "default", "quality")
	runName, err := solutionPromotionRunName("gated-sol", 1, &evaluation)
	if err != nil {
		t.Fatalf("promotion run name: %v", err)
	}
	run := getEvaluationRun(t, ms, "default", runName)
	if run.Spec.TargetRef.Kind != "Solution" || run.Spec.TargetRef.Revision == nil || *run.Spec.TargetRef.Revision != 1 {
		t.Fatalf("promotion run target = %#v", run.Spec.TargetRef)
	}
	run.Status.Phase = v1alpha1.PhaseFailed
	run.Status.Degraded = true
	run.Status.Message = "task_success below minimum 0.9"
	data, _ := json.Marshal(run)
	if err := ms.Update(ctx, evaluationRunKind, "default", runName, data, run.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("fail promotion run: %v", err)
	}
	if _, err := r.Reconcile(ctx, Request{Kind: "Solution", Namespace: "default", Name: "gated-sol"}); err != nil {
		t.Fatalf("reconcile failed gate: %v", err)
	}
	blocked = getSolution(t, ms, "default", "gated-sol")
	if condition := restartLadderCondition(blocked.Status.Conditions, string(lifecycle.ConditionReady)); condition == nil || condition.Reason != "PromotionBlocked" || !strings.Contains(condition.Message, "task_success") {
		t.Fatalf("blocked condition = %#v", condition)
	}

	run = getEvaluationRun(t, ms, "default", runName)
	run.Status.Phase = v1alpha1.PhaseReady
	run.Status.Passed = true
	run.Status.Degraded = false
	run.Status.Message = "all thresholds passed"
	data, _ = json.Marshal(run)
	if err := ms.Update(ctx, evaluationRunKind, "default", runName, data, run.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("pass promotion run: %v", err)
	}
	if _, err := r.Reconcile(ctx, Request{Kind: "Solution", Namespace: "default", Name: "gated-sol"}); err != nil {
		t.Fatalf("promote candidate: %v", err)
	}
	ready := getSolution(t, ms, "default", "gated-sol")
	if ready.Status.ActiveRevision != 1 || ready.Status.CandidateRevision != 0 || ready.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("promoted status = %#v", ready.Status)
	}
	if _, err := ms.Get(ctx, "AgentJob", "default", "gated-sol-agent-job"); err != nil {
		t.Fatalf("promoted AgentJob missing: %v", err)
	}
}

func legacyPromotionUpgradeFixture() (v1alpha1.Solution, v1alpha1.Evaluation, v1alpha1.EvaluationRun) {
	evaluation, run := legacyEvaluationUpgradeFixture()
	evaluation.Name = "quality"
	solution := v1alpha1.Solution{
		ObjectMeta: v1alpha1.ObjectMeta{Name: strings.Repeat("s", 63), Namespace: "default", UID: "solution-owner"},
		Spec: v1alpha1.SolutionSpec{
			PromotionGates: []v1alpha1.SolutionPromotionGate{{EvaluationRef: v1alpha1.LocalObjectReference{Name: evaluation.Name}}},
		},
	}
	revision := int64(7)
	// Fixed pre-upgrade name for this Solution, revision, and Evaluation spec.
	run.Name = strings.Repeat("s", 54) + "-f6fe2003"
	run.OwnerReferences = []v1alpha1.OwnerReference{{Kind: solutionKind, Name: solution.Name, UID: solution.UID}}
	run.Labels = map[string]string{"kognition.dev/solution": solution.Name, "kognition.dev/evaluation": evaluation.Name}
	run.Spec.EvaluationRef.Name = evaluation.Name
	run.Spec.TargetRef = v1alpha1.EvaluationTargetRef{Kind: solutionKind, Name: solution.Name, Revision: &revision}
	evaluation.Status.LastRunRef = &v1alpha1.LocalObjectReference{Name: run.Name}
	return solution, evaluation, run
}

func TestSolutionReconcilerReusesLegacyLongNamedPromotionRun(t *testing.T) {
	for _, test := range []struct {
		name     string
		phase    v1alpha1.Phase
		passed   bool
		degraded bool
		reason   string
	}{
		{"passed", v1alpha1.PhaseReady, true, false, ""},
		{"failed", v1alpha1.PhaseFailed, false, true, "PromotionBlocked"},
		{"threshold failed", v1alpha1.PhaseReady, false, false, "PromotionBlocked"},
		{"degraded", v1alpha1.PhaseReady, true, true, "PromotionGatesPending"},
		{"pending", v1alpha1.PhasePending, false, false, "PromotionGatesPending"},
		{"running", v1alpha1.PhaseRunning, false, false, "PromotionGatesPending"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			memory := newMemStore()
			solution, evaluation, run := legacyPromotionUpgradeFixture()
			run.Status.Phase, run.Status.Passed, run.Status.Degraded = test.phase, test.passed, test.degraded
			storeEvaluation(t, memory, evaluation)
			storeEvaluationRun(t, memory, run)
			r := NewSolutionReconciler(memory, zaptest.NewLogger(t))
			for range 2 {
				passed, reason, message, err := r.reconcilePromotionGates(ctx, &solution, 7)
				if err != nil || passed != (test.reason == "") || reason != test.reason {
					t.Fatalf("gate = %v, %q, %q, %v", passed, reason, message, err)
				}
				if reason != "" && !strings.Contains(message, run.Name) {
					t.Fatalf("gate no longer references the legacy run: %q", message)
				}
			}
			runs, err := memory.List(ctx, evaluationRunKind, store.ListOptions{Namespace: solution.Namespace})
			if err != nil || len(runs) != 1 {
				t.Fatalf("unexpected duplicate runs: %d, error = %v", len(runs), err)
			}
			got := getEvaluationRun(t, memory, solution.Namespace, run.Name)
			if !reflect.DeepEqual(got.Spec, run.Spec) || !reflect.DeepEqual(got.Status, run.Status) {
				t.Fatalf("historical promotion run changed: %#v", got)
			}
			jobs, err := memory.List(ctx, agentJobKind, store.ListOptions{Namespace: solution.Namespace})
			if err != nil || len(jobs) != 0 {
				t.Fatalf("unexpected model jobs: %d, error = %v", len(jobs), err)
			}
		})
	}
}

func TestSolutionReconcilerRejectsUnverifiedLegacyPromotionRun(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*v1alpha1.Solution, *v1alpha1.EvaluationRun)
	}{
		{"different revision", func(_ *v1alpha1.Solution, r *v1alpha1.EvaluationRun) { *r.Spec.TargetRef.Revision = 6 }},
		{"missing revision", func(_ *v1alpha1.Solution, r *v1alpha1.EvaluationRun) { r.Spec.TargetRef.Revision = nil }},
		{"missing owner UID", func(s *v1alpha1.Solution, _ *v1alpha1.EvaluationRun) { s.UID = "" }},
		{"different owner UID", func(_ *v1alpha1.Solution, r *v1alpha1.EvaluationRun) { r.OwnerReferences[0].UID = "previous-owner" }},
		{"different owner kind", func(_ *v1alpha1.Solution, r *v1alpha1.EvaluationRun) { r.OwnerReferences[0].Kind = evaluationKind }},
		{"different owner name", func(_ *v1alpha1.Solution, r *v1alpha1.EvaluationRun) { r.OwnerReferences[0].Name = "other" }},
		{"different evaluation", func(_ *v1alpha1.Solution, r *v1alpha1.EvaluationRun) { r.Spec.EvaluationRef.Name = "other" }},
		{"different target", func(_ *v1alpha1.Solution, r *v1alpha1.EvaluationRun) { r.Spec.TargetRef.Name = "other" }},
		{"different namespace", func(_ *v1alpha1.Solution, r *v1alpha1.EvaluationRun) { r.Spec.TargetRef.Namespace = "other" }},
		{"different dataset", func(_ *v1alpha1.Solution, r *v1alpha1.EvaluationRun) { r.Spec.DatasetRef.Version = "v2" }},
		{"different grader", func(_ *v1alpha1.Solution, r *v1alpha1.EvaluationRun) { r.Spec.Graders[0].Name = "other" }},
		{"different threshold", func(_ *v1alpha1.Solution, r *v1alpha1.EvaluationRun) { *r.Spec.MetricThresholds[0].Min = 0.99 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			memory := newMemStore()
			solution, evaluation, run := legacyPromotionUpgradeFixture()
			test.change(&solution, &run)
			storeEvaluation(t, memory, evaluation)
			storeEvaluationRun(t, memory, run)
			passed, reason, _, err := NewSolutionReconciler(memory, zaptest.NewLogger(t)).reconcilePromotionGates(ctx, &solution, 7)
			if err != nil || passed || reason != "PromotionGatesPending" {
				t.Fatalf("unverified run reused: gate = %v, %q, %v", passed, reason, err)
			}
			runName, err := solutionPromotionRunName(solution.Name, 7, &evaluation)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := memory.Get(ctx, evaluationRunKind, solution.Namespace, runName); err != nil {
				t.Fatalf("new promotion run missing: %v", err)
			}
		})
	}
}

func TestSolutionReconcilerLegacyPromotionLookupErrorsDoNotCreateRuns(t *testing.T) {
	unavailable := errors.New("storage unavailable")
	for _, test := range []struct {
		name      string
		listErr   error
		malformed bool
		ambiguous bool
	}{
		{"unavailable", unavailable, false, false},
		{"malformed", nil, true, false},
		{"ambiguous", nil, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			memory := newMemStore()
			solution, evaluation, run := legacyPromotionUpgradeFixture()
			storeEvaluation(t, memory, evaluation)
			storeEvaluationRun(t, memory, run)
			faults := &evaluationUpgradeFaultStore{memStore: memory, listErr: test.listErr}
			if test.malformed {
				faults.listItems = [][]byte{[]byte("{")}
			}
			if test.ambiguous {
				first, err := json.Marshal(run)
				if err != nil {
					t.Fatal(err)
				}
				run.Name = strings.Repeat("s", 54) + "-deadbeef"
				second, err := json.Marshal(run)
				if err != nil {
					t.Fatal(err)
				}
				faults.listItems = [][]byte{first, second}
			}
			_, _, _, err := NewSolutionReconciler(faults, zaptest.NewLogger(t)).reconcilePromotionGates(ctx, &solution, 7)
			if err == nil || (test.listErr != nil && !errors.Is(err, unavailable)) {
				t.Fatalf("lookup error not propagated: %v", err)
			}
			runs, listErr := memory.List(ctx, evaluationRunKind, store.ListOptions{Namespace: solution.Namespace})
			if listErr != nil || len(runs) != 1 {
				t.Fatalf("lookup failure created a run: %d, error = %v", len(runs), listErr)
			}
		})
	}
}

func TestSolutionReconcilerPrefersCurrentNamedPromotionRun(t *testing.T) {
	ctx := context.Background()
	memory := newMemStore()
	solution, evaluation, legacy := legacyPromotionUpgradeFixture()
	current := legacy
	var err error
	current.Name, err = solutionPromotionRunName(solution.Name, 7, &evaluation)
	if err != nil {
		t.Fatal(err)
	}
	current.Status.Phase = v1alpha1.PhaseFailed
	storeEvaluation(t, memory, evaluation)
	storeEvaluationRun(t, memory, legacy)
	storeEvaluationRun(t, memory, current)
	passed, reason, message, err := NewSolutionReconciler(memory, zaptest.NewLogger(t)).reconcilePromotionGates(ctx, &solution, 7)
	if err != nil || passed || reason != "PromotionBlocked" || !strings.Contains(message, current.Name) {
		t.Fatalf("canonical failure overridden by legacy success: %v, %q, %q, %v", passed, reason, message, err)
	}
}

func TestSolutionReconcilerDoesNotReuseAlternateUnboundedPromotionName(t *testing.T) {
	ctx := context.Background()
	memory := newMemStore()
	solution, evaluation, run := legacyPromotionUpgradeFixture()
	solution.Name = strings.Repeat("s", 43)
	run.OwnerReferences[0].Name = solution.Name
	run.Labels["kognition.dev/solution"] = solution.Name
	run.Spec.TargetRef.Name = solution.Name
	name, err := solutionPromotionRunName(solution.Name, 7, &evaluation)
	if err != nil || len(name) != 63 {
		t.Fatalf("boundary name = %q, error = %v, want exactly 63 characters", name, err)
	}
	run.Name = name[:len(name)-8] + "deadbeef"
	storeEvaluation(t, memory, evaluation)
	storeEvaluationRun(t, memory, run)
	passed, reason, _, err := NewSolutionReconciler(memory, zaptest.NewLogger(t)).reconcilePromotionGates(ctx, &solution, 7)
	if err != nil || passed || reason != "PromotionGatesPending" {
		t.Fatalf("non-legacy alternate run reused: %v, %q, %v", passed, reason, err)
	}
	if _, err := memory.Get(ctx, evaluationRunKind, solution.Namespace, name); err != nil {
		t.Fatalf("current promotion run missing: %v", err)
	}
}

func TestSolutionReconciler_ExecutesRoutedTarget(t *testing.T) {
	for _, test := range []struct {
		name       string
		kind       string
		targetName string
		wantKind   string
		wantName   string
	}{
		{name: "agent", kind: "Agent", targetName: "dispatch", wantKind: "AgentJob", wantName: "routed-agent-job"},
		{name: "workflow", kind: "Workflow", targetName: "triage", wantKind: "WorkflowRun", wantName: "routed-workflow-run"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ms := newMemStore()
			r := NewSolutionReconciler(ms, zaptest.NewLogger(t))
			targetData, err := json.Marshal(map[string]any{"metadata": map[string]any{"name": test.targetName, "namespace": "default"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := ms.Create(context.Background(), test.kind, "default", test.targetName, targetData); err != nil {
				t.Fatalf("store target: %v", err)
			}
			solution := makeSolution("default", "routed", "handle this request")
			solution.Spec.Target = &v1alpha1.SolutionTarget{Kind: test.kind, Ref: v1alpha1.LocalObjectReference{Name: test.targetName}}
			storeSolution(t, ms, solution)
			request := Request{Kind: solutionKind, Namespace: "default", Name: "routed"}
			if _, err := r.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("reconcile finalizer: %v", err)
			}
			if _, err := r.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("reconcile target execution: %v", err)
			}
			data, err := ms.Get(context.Background(), test.wantKind, "default", test.wantName)
			if err != nil {
				t.Fatalf("get %s: %v", test.wantKind, err)
			}
			if test.wantKind == "AgentJob" {
				var job v1alpha1.AgentJob
				if err := json.Unmarshal(data, &job); err != nil {
					t.Fatal(err)
				}
				if job.Spec.AgentRef == nil || job.Spec.AgentRef.Name != test.targetName || job.Spec.Input != solution.Spec.Goal {
					t.Fatalf("agent job spec = %#v", job.Spec)
				}
			} else {
				var run v1alpha1.WorkflowRun
				if err := json.Unmarshal(data, &run); err != nil {
					t.Fatal(err)
				}
				if run.Spec.WorkflowRef.Name != test.targetName || run.Spec.Input != solution.Spec.Goal {
					t.Fatalf("workflow run spec = %#v", run.Spec)
				}
			}
			if _, err := ms.Get(context.Background(), "Agent", "default", "routed-agent"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("generic planner agent should not be created, err = %v", err)
			}
		})
	}
}

func TestSolutionReconciler_ExecutesAgentServicePinnedRevision(t *testing.T) {
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))
	pinnedRevision := int64(1)
	liveAgent := v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "default"},
		Spec:       v1alpha1.AgentSpec{Instructions: v1alpha1.AgentInstructions{Inline: "current instructions"}},
	}
	pinnedAgent := liveAgent
	pinnedAgent.Spec.Instructions.Inline = "pinned instructions"
	liveData, err := json.Marshal(liveAgent)
	if err != nil {
		t.Fatal(err)
	}
	pinnedData, err := json.Marshal(pinnedAgent)
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.Create(context.Background(), "Agent", "default", "planner", liveData); err != nil {
		t.Fatal(err)
	}
	if err := ms.CreateRevision(context.Background(), "Agent", "default", "planner", pinnedRevision, nil, pinnedData); err != nil {
		t.Fatal(err)
	}
	service := v1alpha1.AgentService{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-service", Namespace: "default"},
		Spec:       v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}, RevisionPin: &pinnedRevision},
		Status:     v1alpha1.AgentServiceStatus{Phase: v1alpha1.PhaseReady, ReadyInstances: 1},
	}
	serviceData, err := json.Marshal(service)
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.Create(context.Background(), "AgentService", "default", "planner-service", serviceData); err != nil {
		t.Fatal(err)
	}
	solution := makeSolution("default", "routed", "handle this request")
	solution.Spec.Target = &v1alpha1.SolutionTarget{Kind: "AgentService", Ref: v1alpha1.LocalObjectReference{Name: "planner-service"}}
	storeSolution(t, ms, solution)
	request := Request{Kind: solutionKind, Namespace: "default", Name: "routed"}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("reconcile AgentService execution: %v", err)
	}
	data, err := ms.Get(context.Background(), "AgentJob", "default", "routed-agent-service-job")
	if err != nil {
		t.Fatal(err)
	}
	var job v1alpha1.AgentJob
	if err := json.Unmarshal(data, &job); err != nil {
		t.Fatal(err)
	}
	if job.Spec.AgentRef == nil || job.Spec.AgentRef.Name != "planner" || job.Spec.AgentSpec != nil || job.Spec.AgentRevision == nil || *job.Spec.AgentRevision != pinnedRevision {
		t.Fatalf("AgentService job spec = %#v, want planner at pinned revision %d", job.Spec, pinnedRevision)
	}
	if job.ObjectMeta.Labels["kognition.dev/agent-service"] != "planner-service" || job.Spec.Input != solution.Spec.Goal {
		t.Fatalf("AgentService job metadata/spec = %#v/%#v", job.ObjectMeta, job.Spec)
	}
}

func TestSolutionReconciler_CompilesMultiAgentWorkflow(t *testing.T) {
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))

	sol := makeSolution("default", "launch", "research a market and prepare a launch brief")
	sol.Spec.ModelRef = &v1alpha1.LocalObjectReference{Name: "gpt-4"}
	storeSolution(t, ms, sol)
	req := Request{Kind: solutionKind, Namespace: "default", Name: sol.ObjectMeta.Name}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile compile: %v", err)
	}

	updated := getSolution(t, ms, "default", sol.ObjectMeta.Name)
	wantCompiled := []struct {
		kind string
		name string
	}{
		{kind: "MemoryClaim", name: "launch-memory"},
		{kind: "Tool", name: "launch-research-tool"},
		{kind: "AgentPolicy", name: "launch-policy"},
		{kind: "Agent", name: "launch-agent"},
		{kind: "Agent", name: "launch-researcher"},
		{kind: "Agent", name: "launch-writer"},
		{kind: "Agent", name: "launch-publisher"},
		{kind: "Workflow", name: "launch-workflow"},
	}
	if len(updated.Status.CompiledResources) != len(wantCompiled) {
		t.Fatalf("compiled resources = %#v, want %d entries", updated.Status.CompiledResources, len(wantCompiled))
	}
	for i, want := range wantCompiled {
		got := updated.Status.CompiledResources[i]
		if got.Kind != want.kind || got.Name != want.name || got.Namespace != "default" {
			t.Fatalf("compiled[%d] = %s/%s ns=%s, want %s/%s ns=default", i, got.Kind, got.Name, got.Namespace, want.kind, want.name)
		}
	}

	memoryData, err := ms.Get(context.Background(), "MemoryClaim", "default", "launch-memory")
	if err != nil {
		t.Fatalf("get compiled MemoryClaim: %v", err)
	}
	var memoryClaim v1alpha1.MemoryClaim
	if err := json.Unmarshal(memoryData, &memoryClaim); err != nil {
		t.Fatalf("unmarshal MemoryClaim: %v", err)
	}
	if memoryClaim.Spec.MemoryClassName != "default" || memoryClaim.Spec.Type != v1alpha1.MemoryTypeShared || memoryClaim.Spec.Capacity != "64Mi" {
		t.Fatalf("MemoryClaim spec = %#v, want default/shared/64Mi", memoryClaim.Spec)
	}
	if memoryClaim.Spec.ConcurrencyMode != v1alpha1.ConcurrencyLWW {
		t.Fatalf("MemoryClaim concurrencyMode = %q, want lww", memoryClaim.Spec.ConcurrencyMode)
	}
	if !reflect.DeepEqual(memoryClaim.Spec.AccessModes, []string{"ReadWrite"}) {
		t.Fatalf("MemoryClaim accessModes = %#v, want ReadWrite", memoryClaim.Spec.AccessModes)
	}
	if memoryClaim.ObjectMeta.Labels["kognition.dev/solution"] != sol.ObjectMeta.Name {
		t.Fatalf("MemoryClaim labels = %v", memoryClaim.ObjectMeta.Labels)
	}
	if !solutionOwnsResource(&sol, memoryClaim.ObjectMeta.OwnerReferences) {
		t.Fatalf("MemoryClaim ownerReferences = %#v, want Solution owner", memoryClaim.ObjectMeta.OwnerReferences)
	}

	toolData, err := ms.Get(context.Background(), "Tool", "default", "launch-research-tool")
	if err != nil {
		t.Fatalf("get compiled Tool: %v", err)
	}
	var tool v1alpha1.Tool
	if err := json.Unmarshal(toolData, &tool); err != nil {
		t.Fatalf("unmarshal Tool: %v", err)
	}
	if tool.Spec.Protocol != v1alpha1.ToolProtocolOpenAPI || tool.Spec.ClassName != "sandbox-openapi" {
		t.Fatalf("Tool spec protocol/class = %s/%s, want OpenAPI/sandbox-openapi", tool.Spec.Protocol, tool.Spec.ClassName)
	}
	if tool.Spec.Endpoint != "http://tool-sandbox.samples-tool-sandbox.svc.cluster.local:8080/knowledge-search" {
		t.Fatalf("Tool endpoint = %q", tool.Spec.Endpoint)
	}
	if tool.Spec.Metadata["allowPrivateNetwork"] != "true" || tool.Spec.Metadata["kognition.dev/purpose"] != "solution-research" {
		t.Fatalf("Tool metadata = %v", tool.Spec.Metadata)
	}
	properties, ok := tool.Spec.Schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Tool schema properties = %#v", tool.Spec.Schema["properties"])
	}
	query, ok := properties["query"].(map[string]any)
	if !ok || query["type"] != "string" {
		t.Fatalf("Tool schema query property = %#v", properties["query"])
	}
	required, ok := tool.Spec.Schema["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "query" {
		t.Fatalf("Tool schema required = %#v", tool.Spec.Schema["required"])
	}
	if tool.ObjectMeta.Labels["kognition.dev/solution"] != sol.ObjectMeta.Name {
		t.Fatalf("Tool labels = %v", tool.ObjectMeta.Labels)
	}
	if !solutionOwnsResource(&sol, tool.ObjectMeta.OwnerReferences) {
		t.Fatalf("Tool ownerReferences = %#v, want Solution owner", tool.ObjectMeta.OwnerReferences)
	}

	policyData, err := ms.Get(context.Background(), "AgentPolicy", "default", "launch-policy")
	if err != nil {
		t.Fatalf("get compiled AgentPolicy: %v", err)
	}
	var policy v1alpha1.AgentPolicy
	if err := json.Unmarshal(policyData, &policy); err != nil {
		t.Fatalf("unmarshal AgentPolicy: %v", err)
	}
	if !policy.Spec.Enforce || policy.Spec.Scope != "agent" {
		t.Fatalf("AgentPolicy enforce/scope = %t/%q, want true/agent", policy.Spec.Enforce, policy.Spec.Scope)
	}
	if !reflect.DeepEqual(policy.Spec.TargetAgents, []string{"launch-researcher", "launch-writer"}) {
		t.Fatalf("AgentPolicy targetAgents = %#v, want researcher/writer", policy.Spec.TargetAgents)
	}
	if len(policy.Spec.ToolGrants) != 0 {
		t.Fatalf("AgentPolicy toolGrants = %#v, want no Solution-generated authorization", policy.Spec.ToolGrants)
	}
	if policy.ObjectMeta.Labels["kognition.dev/solution"] != sol.ObjectMeta.Name {
		t.Fatalf("AgentPolicy labels = %v", policy.ObjectMeta.Labels)
	}
	if !solutionOwnsResource(&sol, policy.ObjectMeta.OwnerReferences) {
		t.Fatalf("AgentPolicy ownerReferences = %#v, want Solution owner", policy.ObjectMeta.OwnerReferences)
	}

	for _, name := range []string{"launch-agent", "launch-researcher", "launch-writer", "launch-publisher"} {
		raw, err := ms.Get(context.Background(), "Agent", "default", name)
		if err != nil {
			t.Fatalf("get compiled Agent %s: %v", name, err)
		}
		var agent v1alpha1.Agent
		if err := json.Unmarshal(raw, &agent); err != nil {
			t.Fatalf("unmarshal Agent %s: %v", name, err)
		}
		if agent.Spec.ModelRef.Name != "gpt-4" {
			t.Fatalf("Agent %s modelRef = %q, want gpt-4", name, agent.Spec.ModelRef.Name)
		}
		if agent.ObjectMeta.Labels["kognition.dev/solution"] != sol.ObjectMeta.Name {
			t.Fatalf("Agent %s labels = %v", name, agent.ObjectMeta.Labels)
		}
		if agent.Spec.Memory == nil || len(agent.Spec.Memory.Claims) != 1 || agent.Spec.Memory.Claims[0].Name != "launch-memory" {
			t.Fatalf("Agent %s memory = %#v, want launch-memory claim", name, agent.Spec.Memory)
		}
		if name == "launch-researcher" || name == "launch-writer" {
			if len(agent.Spec.Tools) != 1 || agent.Spec.Tools[0].Ref.Name != "launch-research-tool" || !agent.Spec.Tools[0].Required {
				t.Fatalf("Agent %s tools = %#v, want required launch-research-tool", name, agent.Spec.Tools)
			}
		} else if len(agent.Spec.Tools) != 0 {
			t.Fatalf("Agent %s tools = %#v, want none", name, agent.Spec.Tools)
		}
	}

	workflowData, err := ms.Get(context.Background(), "Workflow", "default", "launch-workflow")
	if err != nil {
		t.Fatalf("get compiled Workflow: %v", err)
	}
	var workflow v1alpha1.Workflow
	if err := json.Unmarshal(workflowData, &workflow); err != nil {
		t.Fatalf("unmarshal Workflow: %v", err)
	}
	if workflow.Spec.EntryPoint != "plan" {
		t.Fatalf("workflow entryPoint = %q, want plan", workflow.Spec.EntryPoint)
	}
	if len(workflow.Spec.Steps) != 4 || len(workflow.Spec.Edges) != 3 {
		t.Fatalf("workflow topology = %d steps/%d edges, want 4/3", len(workflow.Spec.Steps), len(workflow.Spec.Edges))
	}
	if workflow.Spec.Steps[0].AgentRef.Name != "launch-agent" || workflow.Spec.Steps[3].AgentRef.Name != "launch-publisher" {
		t.Fatalf("workflow steps = %#v", workflow.Spec.Steps)
	}
	if len(workflow.Spec.ApprovalGates) != 1 || workflow.Spec.ApprovalGates[0].From != "write" || workflow.Spec.ApprovalGates[0].To != "publish" {
		t.Fatalf("workflow approval gates = %#v", workflow.Spec.ApprovalGates)
	}
}

func TestSolutionReconciler_TC0128_MVP6SolutionWorkflowProducesAnswer(t *testing.T) {
	es := newEventMemStore()
	logger := zaptest.NewLogger(t)
	solutionReconciler := NewSolutionReconciler(es, logger)
	workflowRunReconciler := NewWorkflowRunReconciler(es, logger)
	ctx := context.Background()

	sol := makeSolution("default", "launch", "research the market and prepare a launch brief")
	storeSolution(t, es.memStore, sol)
	solutionReq := Request{Kind: solutionKind, Namespace: "default", Name: sol.ObjectMeta.Name}

	if _, err := solutionReconciler.Reconcile(ctx, solutionReq); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}
	if _, err := solutionReconciler.Reconcile(ctx, solutionReq); err != nil {
		t.Fatalf("reconcile compile: %v", err)
	}

	run := makeWorkflowRun("default", "launch-run", "launch-workflow")
	run.Spec.Input = sol.Spec.Goal
	storeWorkflowRun(t, es, run)
	driveSolutionWorkflowRunToCompletion(t, workflowRunReconciler, es, "default", run.ObjectMeta.Name, map[string]string{
		"plan":     "plan: identify audience, evidence, and launch recommendation",
		"research": "research: market evidence collected from generated knowledge search",
		"write":    "write: launch brief drafted with recommendation and risks",
		"publish":  "final answer: launch brief ready for review",
	})

	storedRun := getWorkflowRun(t, es, "default", run.ObjectMeta.Name)
	if storedRun.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("workflow run phase = %q, want Ready", storedRun.Status.Phase)
	}
	if storedRun.Status.Output != "final answer: launch brief ready for review" {
		t.Fatalf("workflow run output = %q", storedRun.Status.Output)
	}
	if !reflect.DeepEqual(storedRun.Status.CompletedSteps, []string{"plan", "publish", "research", "write"}) {
		t.Fatalf("completed steps = %#v, want all generated steps", storedRun.Status.CompletedSteps)
	}
	if len(storedRun.Status.StepStatuses) != 4 {
		t.Fatalf("step statuses = %#v, want 4 entries", storedRun.Status.StepStatuses)
	}
}

func driveSolutionWorkflowRunToCompletion(t *testing.T, rec *WorkflowRunReconciler, es *eventMemStore, ns, name string, stepOutputs map[string]string) {
	t.Helper()
	approvedPublish := false
	for round := 0; round < 20; round++ {
		run := getWorkflowRun(t, es, ns, name)
		if run.Status.Phase == v1alpha1.PhaseReady || run.Status.Phase == v1alpha1.PhaseFailed {
			return
		}
		jobs := listWorkflowJobs(t, es.memStore, ns)
		for _, job := range jobs {
			if job.Status.Phase == v1alpha1.PhaseReady || job.Status.Phase == v1alpha1.PhaseFailed || !agentJobOwnedBy(job, name) {
				continue
			}
			step := job.ObjectMeta.Labels[workflowStepLabel]
			output, ok := stepOutputs[step]
			if !ok {
				t.Fatalf("no output fixture for workflow step %q", step)
			}
			job.Status.Phase = v1alpha1.PhaseReady
			job.Status.Output = output
			updateJob(t, es.memStore, job)
		}
		run = getWorkflowRun(t, es, ns, name)
		if run.Status.ActiveStep == "publish" && !approvedPublish {
			storeApprovalDecision(t, es.memStore, makeApprovalDecision(ns, fmt.Sprintf("%s-publish-approval", name), name, "publish", "demo-approver", v1alpha1.ApprovalDecisionApprove))
			approvedPublish = true
		}
		if _, err := rec.Reconcile(context.Background(), Request{Kind: "WorkflowRun", Namespace: ns, Name: name}); err != nil {
			t.Fatalf("reconcile workflow run round %d: %v", round, err)
		}
	}
	t.Fatalf("workflow run %s/%s did not reach a terminal phase", ns, name)
}

func agentJobOwnedBy(job v1alpha1.AgentJob, ownerName string) bool {
	for _, ref := range job.ObjectMeta.OwnerReferences {
		if ref.Kind == "WorkflowRun" && ref.Name == ownerName {
			return true
		}
	}
	return false
}

func TestSolutionReconciler_ReadyConflictPreservesDeploymentAndConcurrentState(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	conflictStore := &concurrentSolutionReadyStore{Store: baseStore}
	r := NewSolutionReconciler(conflictStore, zaptest.NewLogger(t))
	sol := makeSolution("default", "conflict-sol", "summarize documents")
	sol.Spec.ModelRef = &v1alpha1.LocalObjectReference{Name: "gpt-4"}
	sol.ObjectMeta.Finalizers = []string{SolutionFinalizer}
	storeSolution(t, baseStore, sol)

	if _, err := r.Reconcile(ctx, Request{Kind: solutionKind, Namespace: "default", Name: sol.ObjectMeta.Name}); err != nil {
		t.Fatalf("reconcile after ready conflict: %v", err)
	}
	if !conflictStore.conflicted {
		t.Fatal("expected ready conflict fixture to execute")
	}
	if conflictStore.agentCreates != 4 {
		t.Fatalf("Agent creates = %d, want exactly 4", conflictStore.agentCreates)
	}

	stored := getSolution(t, baseStore, "default", sol.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseReady || stored.Status.ActiveRevision != 1 {
		t.Fatalf("ready status was not persisted: %+v", stored.Status)
	}
	if stored.Spec.Preferences["review"] != "required" {
		t.Fatalf("concurrent spec change was lost: %+v", stored.Spec)
	}
	condition := lifecycle.GetCondition(apiToLifecycleConditions(stored.Status.Conditions), lifecycle.ConditionType("Audited"))
	if condition == nil || condition.Reason != "ConcurrentAudit" {
		t.Fatalf("Audited condition = %#v, want concurrent condition preserved", condition)
	}
}

func TestSolutionReconciler_ReadyConflictRetriesAreBounded(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	conflictStore := &persistentSolutionReadyConflictStore{Store: baseStore}
	r := NewSolutionReconciler(conflictStore, zaptest.NewLogger(t))
	sol := makeSolution("default", "persistent-conflict-sol", "summarize documents")
	sol.ObjectMeta.Finalizers = []string{SolutionFinalizer}
	storeSolution(t, baseStore, sol)

	_, err := r.Reconcile(ctx, Request{Kind: solutionKind, Namespace: "default", Name: sol.ObjectMeta.Name})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Reconcile() error = %v, want ErrConflict", err)
	}
	if conflictStore.solutionUpdates != 5 {
		t.Fatalf("Solution update attempts = %d, want 5", conflictStore.solutionUpdates)
	}
	if conflictStore.agentCreates != 4 {
		t.Fatalf("Agent creates = %d, want exactly 4", conflictStore.agentCreates)
	}
}

func TestSolutionReconciler_NotFound(t *testing.T) {
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))

	res, err := r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "missing"})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.Requeue {
		t.Fatal("unexpected requeue")
	}
}

func TestSolutionReconciler_RevisionFailurePersistsFailedStatus(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	s := &solutionRevisionFaultStore{Store: baseStore}
	r := NewSolutionReconciler(s, zaptest.NewLogger(t))
	sol := makeSolution("default", "revision-failure", "summarize documents")
	sol.ObjectMeta.Finalizers = []string{SolutionFinalizer}
	storeSolution(t, baseStore, sol)

	result, err := r.Reconcile(ctx, Request{Kind: solutionKind, Namespace: "default", Name: sol.ObjectMeta.Name})
	if err != nil {
		t.Fatalf("reconcile revision failure: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %v, want 30s", result.RequeueAfter)
	}
	if s.revisionCreates != 1 {
		t.Fatalf("revision create calls = %d, want 1", s.revisionCreates)
	}
	updated := getSolution(t, baseStore, "default", sol.ObjectMeta.Name)
	if updated.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	condition := findCondition(updated.Status.Conditions, string(lifecycle.ConditionReady))
	if condition == nil || condition.Status != string(lifecycle.ConditionFalse) || condition.Reason != "RevisionFailed" {
		t.Fatalf("Ready condition = %#v, want False/RevisionFailed", condition)
	}
	if condition.Message != "create revision 1: revision storage unavailable" {
		t.Fatalf("Ready condition message = %q, want revision storage error", condition.Message)
	}
	if updated.Status.ActiveRevision != 0 {
		t.Fatalf("active revision = %d, want 0", updated.Status.ActiveRevision)
	}
	if !hasFinalizer(updated.ObjectMeta.Finalizers, SolutionFinalizer) {
		t.Fatal("Solution finalizer was removed after revision failure")
	}
	if _, err := baseStore.Get(ctx, "Agent", "default", "revision-failure-agent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("child Agent Get() error = %v, want ErrNotFound", err)
	}
}

func TestSolutionReconciler_DoesNotOverwriteForeignChild(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))
	sol := makeSolution("default", "isolated-sol", "solution instructions")
	sol.ObjectMeta.Finalizers = []string{SolutionFinalizer}
	storeSolution(t, ms, sol)

	foreign := makeAgent("default", "isolated-sol-agent", 1)
	foreign.ObjectMeta.UID = "foreign-agent-uid"
	foreign.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{Kind: solutionKind, Name: "other-solution", UID: "other-solution-uid"}}
	foreign.Spec.Instructions.Inline = "foreign instructions"
	foreignData, _ := json.Marshal(foreign)
	if err := ms.Create(ctx, "Agent", "default", foreign.ObjectMeta.Name, foreignData); err != nil {
		t.Fatalf("seed foreign Agent: %v", err)
	}

	result, err := r.Reconcile(ctx, Request{Kind: solutionKind, Namespace: "default", Name: sol.ObjectMeta.Name})
	if err != nil {
		t.Fatalf("reconcile child ownership collision: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %v, want 30s", result.RequeueAfter)
	}

	updated := getSolution(t, ms, "default", sol.ObjectMeta.Name)
	condition := findCondition(updated.Status.Conditions, string(lifecycle.ConditionReady))
	if updated.Status.Phase != v1alpha1.PhaseFailed || condition == nil || condition.Reason != "DeployFailed" {
		t.Fatalf("status = %+v, want Failed with DeployFailed condition", updated.Status)
	}
	if !strings.Contains(condition.Message, "existing Agent/isolated-sol-agent is not owned by Solution default/isolated-sol") {
		t.Fatalf("condition message = %q, want ownership collision", condition.Message)
	}

	storedData, err := ms.Get(ctx, "Agent", "default", foreign.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get foreign Agent: %v", err)
	}
	var stored v1alpha1.Agent
	if err := json.Unmarshal(storedData, &stored); err != nil {
		t.Fatalf("unmarshal foreign Agent: %v", err)
	}
	if stored.ObjectMeta.UID != foreign.ObjectMeta.UID || stored.Spec.Instructions.Inline != "foreign instructions" {
		t.Fatalf("foreign Agent was overwritten: %+v", stored)
	}
}

func TestSolutionReconciler_Deletion(t *testing.T) {
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))

	sol := makeSolution("default", "del-sol", "do things")
	now := time.Now().UTC()
	sol.ObjectMeta.DeletionTimestamp = &now
	sol.ObjectMeta.Finalizers = []string{SolutionFinalizer}
	sol.Status.CompiledResources = []v1alpha1.CompiledResource{
		{Kind: "Agent", Name: "del-sol-agent", Namespace: "default"},
	}
	storeSolution(t, ms, sol)

	// Create the child agent so deletion can remove it.
	childAgent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "del-sol-agent",
			Namespace: "default",
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       solutionKind,
				Name:       sol.ObjectMeta.Name,
				UID:        sol.ObjectMeta.UID,
			}},
		},
	}
	childData, _ := json.Marshal(childAgent)
	_ = ms.Create(context.Background(), "Agent", "default", "del-sol-agent", childData)

	res, err := r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "del-sol"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	_ = res

	// Verify child deleted.
	_, err = ms.Get(context.Background(), "Agent", "default", "del-sol-agent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected child agent deleted, got err=%v", err)
	}

	// Verify finalizer removed.
	updated := getSolution(t, ms, "default", "del-sol")
	for _, f := range updated.ObjectMeta.Finalizers {
		if f == SolutionFinalizer {
			t.Error("expected finalizer to be removed")
		}
	}
}

func TestSolutionReconciler_NoChangeNoNewRevision(t *testing.T) {
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))

	sol := makeSolution("default", "stable", "summarize")
	storeSolution(t, ms, sol)

	// First reconcile adds finalizer.
	_, err := r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "stable"})
	if err != nil {
		t.Fatalf("reconcile (finalizer): %v", err)
	}

	// Second reconcile creates revision 1.
	_, err = r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "stable"})
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	compiledAfterFirstReady := getSolution(t, ms, "default", "stable").Status.CompiledResources

	// Third reconcile should not increment revision.
	_, err = r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "stable"})
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	updated := getSolution(t, ms, "default", "stable")
	if updated.Status.ActiveRevision != 1 {
		t.Errorf("expected revision still 1, got %d", updated.Status.ActiveRevision)
	}
	if !reflect.DeepEqual(updated.Status.CompiledResources, compiledAfterFirstReady) {
		t.Fatalf("compiled resources changed on no-op reconcile")
	}

	// Verify exactly one revision exists in the store.
	revisions, err := ms.ListRevisions(context.Background(), "Solution", "default", "stable")
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	if len(revisions) != 1 {
		t.Errorf("expected exactly 1 revision stored, got %d", len(revisions))
	}
}

func TestSolutionReconciler_WorkspaceQuotaInheritance(t *testing.T) {
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))

	// Create a CognitiveQuota in the namespace.
	quota := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "CognitiveQuota"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "ns-quota", Namespace: "default"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 5000},
		},
	}
	qdata, _ := json.Marshal(quota)
	_ = ms.Create(context.Background(), "CognitiveQuota", "default", "ns-quota", qdata)

	sol := makeSolution("default", "quota-sol", "plan things")
	sol.Spec.Overrides = &v1alpha1.SolutionOverrides{
		TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 3000},
	}
	storeSolution(t, ms, sol)

	// Finalizer reconcile.
	_, _ = r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "quota-sol"})

	_, err := r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "quota-sol"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Check the child agent has capped MaxTokens.
	agentData, err := ms.Get(context.Background(), "Agent", "default", "quota-sol-agent")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	var agent v1alpha1.Agent
	_ = json.Unmarshal(agentData, &agent)
	if agent.Spec.Runtime == nil || agent.Spec.Runtime.MaxTokens == nil {
		t.Fatal("expected runtime.maxTokens to be set")
	}
	if *agent.Spec.Runtime.MaxTokens != 3000 {
		t.Errorf("expected maxTokens=3000, got %d", *agent.Spec.Runtime.MaxTokens)
	}
}

func TestSolutionReconciler_OverrideExceedsWorkspaceQuota(t *testing.T) {
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))

	// Workspace quota caps at 2000.
	quota := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "CognitiveQuota"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "ns-quota", Namespace: "default"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 2000},
		},
	}
	qdata, _ := json.Marshal(quota)
	_ = ms.Create(context.Background(), "CognitiveQuota", "default", "ns-quota", qdata)

	sol := makeSolution("default", "over-sol", "do things")
	sol.Spec.Overrides = &v1alpha1.SolutionOverrides{
		TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 9000}, // exceeds workspace
	}
	storeSolution(t, ms, sol)

	// Finalizer reconcile.
	_, _ = r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "over-sol"})

	_, err := r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "over-sol"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	agentData, _ := ms.Get(context.Background(), "Agent", "default", "over-sol-agent")
	var agent v1alpha1.Agent
	_ = json.Unmarshal(agentData, &agent)
	if agent.Spec.Runtime == nil || agent.Spec.Runtime.MaxTokens == nil {
		t.Fatal("expected runtime.maxTokens to be set")
	}
	// Should be capped at workspace maximum of 2000.
	if *agent.Spec.Runtime.MaxTokens != 2000 {
		t.Errorf("expected maxTokens=2000 (capped), got %d", *agent.Spec.Runtime.MaxTokens)
	}
}

func TestSolutionReconciler_CompiledAgentCarriesDeterminismAndQuotaMetadata(t *testing.T) {
	memory := newMemStore()
	r := NewSolutionReconciler(memory, zaptest.NewLogger(t))
	quota := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "CognitiveQuota"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "workspace-quota", Namespace: "default"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			Hard: map[string]int64{"agents": 4, "requestsPerMinute": 60},
			TokenBudget: &v1alpha1.TokenBudget{
				MaxInputTokens:  4000,
				MaxOutputTokens: 2000,
				MaxTotalTokens:  5000,
			},
		},
	}
	quotaData, err := json.Marshal(quota)
	if err != nil {
		t.Fatalf("marshal quota: %v", err)
	}
	if err := memory.Create(context.Background(), "CognitiveQuota", "default", quota.ObjectMeta.Name, quotaData); err != nil {
		t.Fatalf("store quota: %v", err)
	}
	seed := int64(42)
	solution := makeSolution("default", "governed", "review compliance evidence")
	solution.Spec.Seed = &seed
	solution.Spec.Constraints = []string{"read-only", "cite sources"}
	solution.Spec.Overrides = &v1alpha1.SolutionOverrides{TokenBudget: &v1alpha1.TokenBudget{
		MaxInputTokens:  3000,
		MaxOutputTokens: 9000,
		MaxTotalTokens:  4500,
	}}
	storeSolution(t, memory, solution)
	request := Request{Kind: solutionKind, Namespace: "default", Name: solution.ObjectMeta.Name}

	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("compile solution: %v", err)
	}

	agentData, err := memory.Get(context.Background(), "Agent", "default", "governed-agent")
	if err != nil {
		t.Fatalf("get compiled Agent: %v", err)
	}
	var agent v1alpha1.Agent
	if err := json.Unmarshal(agentData, &agent); err != nil {
		t.Fatalf("unmarshal compiled Agent: %v", err)
	}
	if agent.Spec.ModelRef.Name != "" {
		t.Fatalf("modelRef = %q, want empty fallback", agent.Spec.ModelRef.Name)
	}
	if agent.ObjectMeta.Annotations["kognition.dev/seed"] != "42" {
		t.Fatalf("seed annotation = %q, want 42", agent.ObjectMeta.Annotations["kognition.dev/seed"])
	}
	var constraints []string
	if err := json.Unmarshal([]byte(agent.ObjectMeta.Annotations["kognition.dev/constraints"]), &constraints); err != nil {
		t.Fatalf("decode constraints annotation: %v", err)
	}
	if len(constraints) != 2 || constraints[0] != "read-only" || constraints[1] != "cite sources" {
		t.Fatalf("constraints = %v", constraints)
	}
	var hard map[string]int64
	if err := json.Unmarshal([]byte(agent.ObjectMeta.Annotations["kognition.dev/workspace-quota"]), &hard); err != nil {
		t.Fatalf("decode quota annotation: %v", err)
	}
	if hard["agents"] != 4 || hard["requestsPerMinute"] != 60 {
		t.Fatalf("hard quota = %v", hard)
	}
	if agent.Spec.Runtime == nil || agent.Spec.Runtime.MaxTokens == nil || *agent.Spec.Runtime.MaxTokens != 4500 {
		t.Fatalf("runtime = %#v, want maxTokens 4500", agent.Spec.Runtime)
	}
	if len(agent.ObjectMeta.OwnerReferences) != 1 || agent.ObjectMeta.OwnerReferences[0].UID != solution.ObjectMeta.UID {
		t.Fatalf("owner references = %#v, want Solution UID %q", agent.ObjectMeta.OwnerReferences, solution.ObjectMeta.UID)
	}
}

func TestSolutionReconciler_QuotaLookupFailuresDoNotBlockDeployment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		listErr error
		items   [][]byte
	}{
		{name: "list failure", listErr: errors.New("quota store unavailable")},
		{name: "malformed quota", items: [][]byte{[]byte("{")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := newMemStore()
			faults := &solutionQuotaFaultStore{Store: memory, listErr: tc.listErr, items: tc.items}
			r := NewSolutionReconciler(faults, zaptest.NewLogger(t))
			solution := makeSolution("default", "quota-fallback", "continue without quota")
			storeSolution(t, memory, solution)
			request := Request{Kind: solutionKind, Namespace: "default", Name: solution.ObjectMeta.Name}

			if _, err := r.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("add finalizer: %v", err)
			}
			if _, err := r.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("deploy despite quota failure: %v", err)
			}

			stored := getSolution(t, memory, "default", solution.ObjectMeta.Name)
			if stored.Status.Phase != v1alpha1.PhaseReady {
				t.Fatalf("phase = %q, want Ready", stored.Status.Phase)
			}
			agentData, err := memory.Get(context.Background(), "Agent", "default", "quota-fallback-agent")
			if err != nil {
				t.Fatalf("get compiled Agent: %v", err)
			}
			var agent v1alpha1.Agent
			if err := json.Unmarshal(agentData, &agent); err != nil {
				t.Fatalf("unmarshal compiled Agent: %v", err)
			}
			if agent.Spec.Runtime != nil && agent.Spec.Runtime.MaxTokens != nil {
				t.Fatalf("runtime maxTokens = %d, want no invented quota", *agent.Spec.Runtime.MaxTokens)
			}
		})
	}
}

func TestSolutionReconciler_GoalChange(t *testing.T) {
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))

	sol := makeSolution("default", "evolving", "first goal")
	storeSolution(t, ms, sol)

	// First reconcile adds finalizer.
	_, err := r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "evolving"})
	if err != nil {
		t.Fatalf("reconcile (finalizer): %v", err)
	}

	// Second reconcile creates revision 1.
	_, err = r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "evolving"})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	updated := getSolution(t, ms, "default", "evolving")
	if updated.Status.ActiveRevision != 1 {
		t.Fatalf("expected revision 1 after first compile, got %d", updated.Status.ActiveRevision)
	}

	// Mutate the goal in the store (simulating a user update).
	updated.Spec.Goal = "second goal"
	data, _ := json.Marshal(updated)
	if err := ms.Update(context.Background(), "Solution", "default", "evolving", data, updated.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update solution goal: %v", err)
	}

	// Reconcile detects change and creates revision 2.
	_, err = r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "evolving"})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	updated = getSolution(t, ms, "default", "evolving")
	if updated.Status.ActiveRevision != 2 {
		t.Errorf("expected revision 2 after goal change, got %d", updated.Status.ActiveRevision)
	}

	// Verify two revisions exist.
	revisions, err := ms.ListRevisions(context.Background(), "Solution", "default", "evolving")
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	if len(revisions) != 2 {
		t.Errorf("expected 2 revisions, got %d", len(revisions))
	}

	// Verify child agent has updated instructions.
	agentData, err := ms.Get(context.Background(), "Agent", "default", "evolving-agent")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	var agent v1alpha1.Agent
	_ = json.Unmarshal(agentData, &agent)
	if agent.Spec.Instructions.Inline != "second goal" {
		t.Errorf("expected agent instructions = 'second goal', got %q", agent.Spec.Instructions.Inline)
	}
}

func TestSolutionReconciler_Rollback(t *testing.T) {
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))

	sol := makeSolution("default", "rollback-sol", "original goal")
	storeSolution(t, ms, sol)

	// Finalizer reconcile.
	_, _ = r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "rollback-sol"})
	// Compile revision 1.
	_, err := r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "rollback-sol"})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Update goal to create revision 2.
	updated := getSolution(t, ms, "default", "rollback-sol")
	updated.Spec.Goal = "new goal"
	data, _ := json.Marshal(updated)
	if err := ms.Update(context.Background(), "Solution", "default", "rollback-sol", data, updated.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update: %v", err)
	}
	_, err = r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "rollback-sol"})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	updated = getSolution(t, ms, "default", "rollback-sol")
	if updated.Status.ActiveRevision != 2 {
		t.Fatalf("expected revision 2, got %d", updated.Status.ActiveRevision)
	}

	// Rollback to revision 1.
	if err := r.RollbackToRevision(context.Background(), "default", "rollback-sol", 1); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Verify spec was restored.
	restored := getSolution(t, ms, "default", "rollback-sol")
	if restored.Spec.Goal != "original goal" {
		t.Errorf("expected goal='original goal' after rollback, got %q", restored.Spec.Goal)
	}
}

func TestSolutionReconciler_DeployChildUpdate(t *testing.T) {
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))

	sol := makeSolution("default", "update-sol", "some task")
	storeSolution(t, ms, sol)

	// Pre-create a conflicting agent resource (simulate prior deploy).
	preAgent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "update-sol-agent",
			Namespace: "default",
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       solutionKind,
				Name:       sol.ObjectMeta.Name,
				UID:        sol.ObjectMeta.UID,
			}},
		},
		Spec: v1alpha1.AgentSpec{
			Instructions: v1alpha1.AgentInstructions{Inline: "old instructions"},
		},
	}
	preData, _ := json.Marshal(preAgent)
	_ = ms.Create(context.Background(), "Agent", "default", "update-sol-agent", preData)

	// Finalizer reconcile.
	_, _ = r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "update-sol"})

	// Compile reconcile hits ErrAlreadyExists path and updates.
	_, err := r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "update-sol"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Verify agent was updated with new instructions.
	agentData, err := ms.Get(context.Background(), "Agent", "default", "update-sol-agent")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	var agent v1alpha1.Agent
	_ = json.Unmarshal(agentData, &agent)
	if agent.Spec.Instructions.Inline != "some task" {
		t.Errorf("expected updated instructions='some task', got %q", agent.Spec.Instructions.Inline)
	}

	// Verify solution is Ready.
	updated := getSolution(t, ms, "default", "update-sol")
	if updated.Status.Phase != v1alpha1.PhaseReady {
		t.Errorf("expected Ready, got %v", updated.Status.Phase)
	}
}

func TestSolutionReconciler_RecreatedSolutionRejectsStaleChild(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))

	sol := makeSolution("default", "recreated-sol", "new instructions")
	sol.ObjectMeta.UID = "new-solution-uid"
	sol.ObjectMeta.Finalizers = []string{SolutionFinalizer}
	storeSolution(t, ms, sol)

	staleAgent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "recreated-sol-agent",
			Namespace: "default",
			UID:       "stale-agent-uid",
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       solutionKind,
				Name:       sol.ObjectMeta.Name,
				UID:        "old-solution-uid",
			}},
		},
		Spec: v1alpha1.AgentSpec{
			Instructions: v1alpha1.AgentInstructions{Inline: "old instructions"},
		},
	}
	staleData, err := json.Marshal(staleAgent)
	if err != nil {
		t.Fatalf("marshal stale Agent: %v", err)
	}
	if err := ms.Create(ctx, "Agent", "default", staleAgent.ObjectMeta.Name, staleData); err != nil {
		t.Fatalf("store stale Agent: %v", err)
	}

	result, err := r.Reconcile(ctx, Request{Kind: solutionKind, Namespace: "default", Name: sol.ObjectMeta.Name})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %v, want 30s", result.RequeueAfter)
	}
	updated := getSolution(t, ms, "default", sol.ObjectMeta.Name)
	if updated.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("Solution phase = %s, want Failed", updated.Status.Phase)
	}
	condition := findCondition(updated.Status.Conditions, string(lifecycle.ConditionReady))
	if condition == nil || condition.Reason != "DeployFailed" {
		t.Fatalf("Ready condition = %#v, want DeployFailed", condition)
	}

	raw, err := ms.Get(ctx, "Agent", "default", staleAgent.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get stale Agent after collision: %v", err)
	}
	var preserved v1alpha1.Agent
	if err := json.Unmarshal(raw, &preserved); err != nil {
		t.Fatalf("unmarshal stale Agent after collision: %v", err)
	}
	if preserved.ObjectMeta.UID != staleAgent.ObjectMeta.UID || preserved.Spec.Instructions.Inline != "old instructions" {
		t.Fatalf("stale Agent was overwritten: %+v", preserved)
	}
	if len(preserved.ObjectMeta.OwnerReferences) != 1 || preserved.ObjectMeta.OwnerReferences[0].UID != "old-solution-uid" {
		t.Fatalf("stale Agent owner was changed: %+v", preserved.ObjectMeta.OwnerReferences)
	}
}

func TestSolutionReconciler_DeletionLabelCleanup(t *testing.T) {
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))

	sol := makeSolution("default", "cleanup-sol", "clean things")
	now := time.Now().UTC()
	sol.ObjectMeta.DeletionTimestamp = &now
	sol.ObjectMeta.Finalizers = []string{SolutionFinalizer}
	sol.Status.CompiledResources = []v1alpha1.CompiledResource{} // empty; rely on label cleanup
	storeSolution(t, ms, sol)

	// Create an owned agent (has the solution label).
	ownedAgent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "owned-agent",
			Namespace: "default",
			Labels:    map[string]string{"kognition.dev/solution": "cleanup-sol"},
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       solutionKind,
				Name:       sol.ObjectMeta.Name,
				UID:        sol.ObjectMeta.UID,
			}},
		},
	}
	ownedData, _ := json.Marshal(ownedAgent)
	_ = ms.Create(context.Background(), "Agent", "default", "owned-agent", ownedData)

	// Create an unrelated agent (no solution label).
	unrelatedAgent := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "unrelated-agent", Namespace: "default"},
	}
	unrelatedData, _ := json.Marshal(unrelatedAgent)
	_ = ms.Create(context.Background(), "Agent", "default", "unrelated-agent", unrelatedData)

	// Reconcile (deletion).
	_, err := r.Reconcile(context.Background(), Request{Kind: "Solution", Namespace: "default", Name: "cleanup-sol"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Owned agent should be deleted.
	_, err = ms.Get(context.Background(), "Agent", "default", "owned-agent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected owned agent deleted, got err=%v", err)
	}

	// Unrelated agent should still exist.
	_, err = ms.Get(context.Background(), "Agent", "default", "unrelated-agent")
	if err != nil {
		t.Errorf("expected unrelated agent to survive, got err=%v", err)
	}
}

func TestSolutionReconciler_DeletionLabelCleanupFailurePreservesFinalizer(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	sol := makeSolution("default", "cleanup-sol", "clean things")
	now := time.Now().UTC()
	sol.ObjectMeta.DeletionTimestamp = &now
	sol.ObjectMeta.Finalizers = []string{SolutionFinalizer}
	sol.Status.CompiledResources = []v1alpha1.CompiledResource{}
	storeSolution(t, ms, sol)

	ownedAgent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "owned-agent",
			Namespace: "default",
			Labels:    map[string]string{"kognition.dev/solution": sol.ObjectMeta.Name},
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       solutionKind,
				Name:       sol.ObjectMeta.Name,
				UID:        sol.ObjectMeta.UID,
			}},
		},
	}
	ownedData, err := json.Marshal(ownedAgent)
	if err != nil {
		t.Fatalf("marshal owned Agent: %v", err)
	}
	if err := ms.Create(ctx, "Agent", "default", ownedAgent.ObjectMeta.Name, ownedData); err != nil {
		t.Fatalf("store owned Agent: %v", err)
	}

	r := NewSolutionReconciler(&solutionLabelCleanupDeleteFailureStore{Store: ms}, zaptest.NewLogger(t))
	if _, err := r.Reconcile(ctx, Request{Kind: solutionKind, Namespace: "default", Name: sol.ObjectMeta.Name}); err == nil {
		t.Fatal("Reconcile() error = nil, want label cleanup delete failure")
	}
	stored := getSolution(t, ms, "default", sol.ObjectMeta.Name)
	if !lifecycle.HasFinalizer(stored.ObjectMeta.Finalizers, SolutionFinalizer) {
		t.Fatalf("finalizers = %v, want %q retained", stored.ObjectMeta.Finalizers, SolutionFinalizer)
	}
	if _, err := ms.Get(ctx, "Agent", "default", ownedAgent.ObjectMeta.Name); err != nil {
		t.Fatalf("owned Agent should remain after failed deletion: %v", err)
	}
}

func TestSolutionReconciler_DeletionLabelCleanupListFailurePreservesFinalizer(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	sol := makeSolution("default", "cleanup-sol", "clean things")
	now := time.Now().UTC()
	sol.ObjectMeta.DeletionTimestamp = &now
	sol.ObjectMeta.Finalizers = []string{SolutionFinalizer}
	sol.Status.CompiledResources = []v1alpha1.CompiledResource{}
	storeSolution(t, ms, sol)

	r := NewSolutionReconciler(&solutionLabelCleanupListFailureStore{Store: ms}, zaptest.NewLogger(t))
	if _, err := r.Reconcile(ctx, Request{Kind: solutionKind, Namespace: "default", Name: sol.ObjectMeta.Name}); err == nil {
		t.Fatal("Reconcile() error = nil, want label cleanup list failure")
	}
	stored := getSolution(t, ms, "default", sol.ObjectMeta.Name)
	if !lifecycle.HasFinalizer(stored.ObjectMeta.Finalizers, SolutionFinalizer) {
		t.Fatalf("finalizers = %v, want %q retained", stored.ObjectMeta.Finalizers, SolutionFinalizer)
	}
}

func TestSolutionReconciler_DeletionLabelCleanupMalformedItemPreservesFinalizer(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	sol := makeSolution("default", "cleanup-sol", "clean things")
	now := time.Now().UTC()
	sol.ObjectMeta.DeletionTimestamp = &now
	sol.ObjectMeta.Finalizers = []string{SolutionFinalizer}
	sol.Status.CompiledResources = []v1alpha1.CompiledResource{}
	storeSolution(t, ms, sol)

	r := NewSolutionReconciler(&solutionLabelCleanupMalformedStore{Store: ms}, zaptest.NewLogger(t))
	if _, err := r.Reconcile(ctx, Request{Kind: solutionKind, Namespace: "default", Name: sol.ObjectMeta.Name}); err == nil {
		t.Fatal("Reconcile() error = nil, want malformed label cleanup item error")
	}
	stored := getSolution(t, ms, "default", sol.ObjectMeta.Name)
	if !lifecycle.HasFinalizer(stored.ObjectMeta.Finalizers, SolutionFinalizer) {
		t.Fatalf("finalizers = %v, want %q retained", stored.ObjectMeta.Finalizers, SolutionFinalizer)
	}
}

func TestSolutionReconciler_RecreatedSolutionDeletionPreservesStaleChildren(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))

	sol := makeSolution("default", "recreated-delete", "clean things")
	sol.ObjectMeta.UID = "new-solution-uid"
	now := time.Now().UTC()
	sol.ObjectMeta.DeletionTimestamp = &now
	sol.ObjectMeta.Finalizers = []string{SolutionFinalizer}
	sol.Status.CompiledResources = []v1alpha1.CompiledResource{{
		Kind: "Agent", Name: "recreated-delete-agent", Namespace: "default",
	}}
	storeSolution(t, ms, sol)

	staleAgent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "recreated-delete-agent",
			Namespace: "default",
			Labels:    map[string]string{"kognition.dev/solution": sol.ObjectMeta.Name},
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       solutionKind,
				Name:       sol.ObjectMeta.Name,
				UID:        "old-solution-uid",
			}},
		},
	}
	staleData, err := json.Marshal(staleAgent)
	if err != nil {
		t.Fatalf("marshal stale Agent: %v", err)
	}
	if err := ms.Create(ctx, "Agent", "default", staleAgent.ObjectMeta.Name, staleData); err != nil {
		t.Fatalf("store stale Agent: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Kind: solutionKind, Namespace: "default", Name: sol.ObjectMeta.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, err := ms.Get(ctx, "Agent", "default", staleAgent.ObjectMeta.Name); err != nil {
		t.Fatalf("stale Agent was deleted: %v", err)
	}
	if updated := getSolution(t, ms, "default", sol.ObjectMeta.Name); hasFinalizer(updated.ObjectMeta.Finalizers, SolutionFinalizer) {
		t.Fatal("Solution cleanup finalizer was not removed")
	}
}

func TestSolutionReconciler_RollbackNonExistentRevision(t *testing.T) {
	ms := newMemStore()
	r := NewSolutionReconciler(ms, zaptest.NewLogger(t))

	sol := makeSolution("default", "norev-sol", "goal")
	storeSolution(t, ms, sol)

	// Rollback without any revisions should fail.
	err := r.RollbackToRevision(context.Background(), "default", "norev-sol", 99)
	if err == nil {
		t.Fatal("expected error for non-existent revision")
	}
}
