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

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

type workflowRunConflictStore struct {
	*memStore
	conflicted bool
}

type workflowRunCompletionConflictStore struct {
	store.Store
	conflicted bool
	completed  v1alpha1.WorkflowRunStatus
}

type workflowRunMemoryClaimDeleteStore struct {
	*eventMemStore
	failKind string
}

type workflowRunJobListFaultStore struct {
	*eventMemStore
	err error
}

type workflowRunConcurrencyFaultStore struct {
	*eventMemStore
	listErr   error
	updateErr error
}

func (s *workflowRunConcurrencyFaultStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if kind == "WorkflowRun" && s.listErr != nil {
		return nil, s.listErr
	}
	return s.eventMemStore.List(ctx, kind, opts)
}

func (s *workflowRunConcurrencyFaultStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "WorkflowRun" && s.updateErr != nil {
		return s.updateErr
	}
	return s.eventMemStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *workflowRunJobListFaultStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if kind == "AgentJob" {
		return nil, s.err
	}
	return s.eventMemStore.List(ctx, kind, opts)
}

func (s *workflowRunMemoryClaimDeleteStore) Delete(ctx context.Context, kind, namespace, name string) error {
	if kind == s.failKind {
		return fmt.Errorf("injected %s delete failure", kind)
	}
	return s.eventMemStore.Delete(ctx, kind, namespace, name)
}

func TestWorkflowRunReconciler_StoreBoundaries(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		reconciler := NewWorkflowRunReconciler(newMemStore(), zaptest.NewLogger(t))
		result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: "missing"})
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if result.Requeue || result.RequeueAfter != 0 {
			t.Fatalf("result = %+v, want no requeue", result)
		}
	})

	t.Run("get failure", func(t *testing.T) {
		wantErr := errors.New("storage unavailable")
		reconciler := NewWorkflowRunReconciler(&faultStore{memStore: newMemStore(), getErr: wantErr}, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "get workflow run") {
			t.Fatalf("error = %v, want wrapped get failure", err)
		}
	})

	t.Run("malformed workflow run", func(t *testing.T) {
		s := newMemStore()
		if err := s.Create(context.Background(), "WorkflowRun", "default", "run", []byte("{")); err != nil {
			t.Fatalf("create malformed run: %v", err)
		}
		reconciler := NewWorkflowRunReconciler(s, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal workflow run") {
			t.Fatalf("error = %v, want unmarshal failure", err)
		}
	})

	t.Run("finalizer update failure", func(t *testing.T) {
		base := newMemStore()
		run := makeWorkflowRun("default", "run", "workflow")
		storeWorkflowRun(t, base, run)
		wantErr := errors.New("update unavailable")
		reconciler := NewWorkflowRunReconciler(&faultStore{memStore: base, updateErr: wantErr}, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "add finalizer") {
			t.Fatalf("error = %v, want wrapped finalizer update failure", err)
		}
		stored := getWorkflowRun(t, base, "default", "run")
		if hasString(stored.ObjectMeta.Finalizers, WorkflowRunFinalizer) {
			t.Fatal("finalizer persisted despite injected update failure")
		}
	})
}

func TestWorkflowRunReconciler_ProtectedRunCannotEnterLegacyGraph(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained snapshot=%v", running), func(t *testing.T) {
			ctx := context.Background()
			storage := newEventMemStore()
			run := makeWorkflowRun("team-a", "protected-run", "workflow")
			run.ObjectMeta.Annotations = map[string]string{runtimeapproval.WorkflowRunAcceptanceRequiredAnnotation: "true"}
			if running {
				workflow := makeWorkflow("team-a", "workflow")
				run.ObjectMeta.Finalizers = []string{WorkflowRunFinalizer}
				run.Status.SnapshotSpec = &workflow.Spec
				run.Status.Phase = v1alpha1.PhaseRunning
			}
			storeWorkflowRun(t, storage, run)
			before := getWorkflowRun(t, storage, "team-a", "protected-run")
			reconciler := NewWorkflowRunReconciler(storage, zaptest.NewLogger(t))
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := reconciler.Reconcile(ctx, Request{Namespace: "team-a", Name: "protected-run"}); !errors.Is(err, runtimeapproval.ErrWorkflowDispatchUnsupported) {
					t.Fatalf("protected run entered legacy reconciliation: %v", err)
				}
			}
			after := getWorkflowRun(t, storage, "team-a", "protected-run")
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("legacy reconciliation mutated protected run: before=%#v after=%#v", before, after)
			}
			jobs, err := storage.List(ctx, "AgentJob", store.ListOptions{Namespace: "team-a"})
			if err != nil || len(jobs) != 0 {
				t.Fatalf("protected run produced legacy AgentJobs: count=%d error=%v", len(jobs), err)
			}
		})
	}
}

func (s *workflowRunConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "WorkflowRun" && !s.conflicted {
		s.conflicted = true
		raw, err := s.memStore.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var latest v1alpha1.WorkflowRun
		if err := json.Unmarshal(raw, &latest); err != nil {
			return err
		}
		latest.ObjectMeta.Finalizers = append(latest.ObjectMeta.Finalizers, "example.com/foreign")
		latest.ObjectMeta.Annotations = map[string]string{"example.com/revision": "2"}
		latest.Status.ApprovalRecords = append(latest.Status.ApprovalRecords, v1alpha1.WorkflowApprovalRecord{
			Step: "review", Decision: "Approved", Approver: "concurrent-user",
		})
		latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{
			Type: "Audited", Status: "True", Reason: "ConcurrentObserver",
		})
		updated, err := json.Marshal(latest)
		if err != nil {
			return err
		}
		if err := s.memStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
			return err
		}
		return store.ErrConflict
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *workflowRunCompletionConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "WorkflowRun" && !s.conflicted {
		var desired v1alpha1.WorkflowRun
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase == v1alpha1.PhaseRunning {
			s.conflicted = true
			raw, err := s.Store.Get(ctx, kind, namespace, name)
			if err != nil {
				return err
			}
			var latest v1alpha1.WorkflowRun
			if err := json.Unmarshal(raw, &latest); err != nil {
				return err
			}
			latest.Status = s.completed
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

func makeWorkflowRun(ns, name, workflowName string) v1alpha1.WorkflowRun {
	return v1alpha1.WorkflowRun{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "WorkflowRun"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			UID:               "run-uid-" + name,
			CreationTimestamp: time.Now(),
		},
		Spec: v1alpha1.WorkflowRunSpec{
			WorkflowRef: v1alpha1.LocalObjectReference{Name: workflowName},
		},
	}
}

func storeWorkflowRun(t *testing.T, s store.Store, run v1alpha1.WorkflowRun) {
	t.Helper()
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal workflow run: %v", err)
	}
	if err := s.Create(context.Background(), "WorkflowRun", run.ObjectMeta.Namespace, run.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store workflow run: %v", err)
	}
}

func getWorkflowRun(t *testing.T, s store.Store, ns, name string) v1alpha1.WorkflowRun {
	t.Helper()
	raw, err := s.Get(context.Background(), "WorkflowRun", ns, name)
	if err != nil {
		t.Fatalf("get workflow run: %v", err)
	}
	var run v1alpha1.WorkflowRun
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatalf("unmarshal workflow run: %v", err)
	}
	return run
}

// reconcileN calls Reconcile repeatedly until Requeue is false or maxIter is reached.
func reconcileRunN(t *testing.T, r *WorkflowRunReconciler, ns, name string, maxIter int) {
	t.Helper()
	for i := 0; i < maxIter; i++ {
		res, err := r.Reconcile(context.Background(), Request{Namespace: ns, Name: name})
		if err != nil {
			t.Fatalf("reconcile iter %d: %v", i, err)
		}
		if !res.Requeue {
			return
		}
	}
}

func TestWorkflowRunReconciler_FinalizerConflictPreservesConcurrentState(t *testing.T) {
	baseStore := newMemStore()
	s := &workflowRunConflictStore{memStore: baseStore}
	r := NewWorkflowRunReconciler(s, zaptest.NewLogger(t))
	run := makeWorkflowRun("default", "run-conflict", "workflow")
	storeWorkflowRun(t, baseStore, run)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run-conflict"})
	if err != nil {
		t.Fatalf("reconcile through finalizer conflict: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	updated := getWorkflowRun(t, baseStore, "default", "run-conflict")
	if !hasString(updated.ObjectMeta.Finalizers, WorkflowRunFinalizer) || !hasString(updated.ObjectMeta.Finalizers, "example.com/foreign") {
		t.Fatalf("finalizers = %v, want owned and foreign finalizers", updated.ObjectMeta.Finalizers)
	}
	if updated.ObjectMeta.Annotations["example.com/revision"] != "2" {
		t.Fatalf("concurrent annotation was lost: %v", updated.ObjectMeta.Annotations)
	}
	if len(updated.Status.ApprovalRecords) != 1 || updated.Status.ApprovalRecords[0].Approver != "concurrent-user" {
		t.Fatalf("concurrent approval record was lost: %#v", updated.Status.ApprovalRecords)
	}
	if condition := findCondition(updated.Status.Conditions, "Audited"); condition == nil || condition.Reason != "ConcurrentObserver" {
		t.Fatalf("concurrent condition was lost: %#v", condition)
	}
}

func TestWorkflowRunReconciler_RunningConflictPreservesConcurrentCompletion(t *testing.T) {
	es := newEventMemStore()
	wf := makeWorkflow("default", "workflow")
	run := makeWorkflowRun("default", "run-conflict", "workflow")
	run.ObjectMeta.Finalizers = []string{WorkflowRunFinalizer}
	run.ObjectMeta.Annotations = map[string]string{observability.AnnotationMetricsStarted: "true"}
	run.Status.SnapshotSpec = &wf.Spec
	storeWorkflowRun(t, es, run)
	completed := run.Status
	completed.Phase = v1alpha1.PhaseReady
	completed.CompletedSteps = []string{"step1", "step2"}
	completed.Conditions = []v1alpha1.Condition{{Type: "Ready", Status: "True", Reason: "Completed"}}
	conflictStore := &workflowRunCompletionConflictStore{Store: es, completed: completed}
	r := NewWorkflowRunReconciler(conflictStore, zaptest.NewLogger(t))

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run-conflict"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	stored := getWorkflowRun(t, es, "default", "run-conflict")
	if !reflect.DeepEqual(stored.Status, completed) {
		t.Fatalf("status after stale Running retry = %+v, want concurrent completion", stored.Status)
	}
	if condition := findCondition(stored.Status.Conditions, "Ready"); condition == nil || condition.Status != "True" || condition.Reason != "Completed" {
		t.Fatalf("Ready condition = %#v, want concurrent terminal condition", condition)
	}
}

func TestWorkflowRunReconciler_OutputPublishedOnlyAfterCompletion(t *testing.T) {
	es := newEventMemStore()
	storeWorkflow(t, es.memStore, makeWorkflow("default", "workflow-output"))
	run := makeWorkflowRun("default", "run-output", "workflow-output")
	storeWorkflowRun(t, es, run)
	reconciler := NewWorkflowRunReconciler(es, zaptest.NewLogger(t))

	reconcileRunN(t, reconciler, "default", "run-output", 3)
	markWorkflowRunStepReady(t, es, "default", "run-output", "plan", "partial plan output")
	reconcileRunN(t, reconciler, "default", "run-output", 5)

	stored := getWorkflowRun(t, es, "default", "run-output")
	if stored.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("phase after first step = %q, want Running", stored.Status.Phase)
	}
	if stored.Status.Output != "" {
		t.Fatalf("output while run is still active = %q, want empty", stored.Status.Output)
	}

	markWorkflowRunStepReady(t, es, "default", "run-output", "execute", "final workflow output")
	reconcileRunN(t, reconciler, "default", "run-output", 5)
	stored = getWorkflowRun(t, es, "default", "run-output")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase after final step = %q, want Ready", stored.Status.Phase)
	}
	if stored.Status.Output != "final workflow output" {
		t.Fatalf("final output = %q, want terminal step output", stored.Status.Output)
	}
}

func TestWorkflowRunReconciler_ApprovalGatePausesUntilApproved(t *testing.T) {
	es := newEventMemStore()
	wf := makeWorkflow("default", "workflow-approval")
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "plan", To: "execute", RequiredApprovers: 1}}
	storeWorkflow(t, es.memStore, wf)
	run := makeWorkflowRun("default", "run-approval", "workflow-approval")
	storeWorkflowRun(t, es, run)
	reconciler := NewWorkflowRunReconciler(es, zaptest.NewLogger(t))

	reconcileRunN(t, reconciler, "default", "run-approval", 5)
	markWorkflowRunStepReady(t, es, "default", "run-approval", "plan", "approved plan")
	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: "run-approval"}); err != nil {
		t.Fatalf("reconcile waiting approval: %v", err)
	}

	stored := getWorkflowRun(t, es, "default", "run-approval")
	if stored.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("phase = %q, want Pending", stored.Status.Phase)
	}
	if stored.Status.ActiveStep != "execute" {
		t.Fatalf("active step = %q, want execute", stored.Status.ActiveStep)
	}
	if condition := findCondition(stored.Status.Conditions, "Ready"); condition == nil || condition.Reason != "WaitingApproval" {
		t.Fatalf("Ready condition = %#v, want WaitingApproval", condition)
	}
	reconcileRunN(t, reconciler, "default", "run-approval", 3)
	if jobs := listWorkflowJobs(t, es.memStore, "default"); len(jobs) != 1 {
		t.Fatalf("jobs before approval = %d, want only the prepared plan job", len(jobs))
	}

	storeApprovalDecision(t, es.memStore, makeApprovalDecision("default", "approve-execute", "run-approval", "execute", "reviewer", v1alpha1.ApprovalDecisionApprove))
	reconciler = NewWorkflowRunReconciler(es, zaptest.NewLogger(t))
	reconcileRunN(t, reconciler, "default", "run-approval", 5)
	jobs := listWorkflowJobs(t, es.memStore, "default")
	dispatched := 0
	for _, job := range jobs {
		if job.ObjectMeta.Labels[workflowStepLabel] == "execute" && agentJobOwnedBy(job, "run-approval") {
			dispatched++
		}
	}
	if len(jobs) != 2 || dispatched != 1 {
		t.Fatalf("jobs after approval = %d, execute jobs = %d; want 2 and 1", len(jobs), dispatched)
	}

	reconciler = NewWorkflowRunReconciler(es, zaptest.NewLogger(t))
	reconcileRunN(t, reconciler, "default", "run-approval", 5)
	if jobs := listWorkflowJobs(t, es.memStore, "default"); len(jobs) != 2 {
		t.Fatalf("jobs after reconciler restart = %d, want no duplicate dispatch", len(jobs))
	}
}

func TestWorkflowRunReconciler_ApprovalGatedToolExecutesOnce(t *testing.T) {
	for _, decision := range []string{v1alpha1.ApprovalDecisionApprove, v1alpha1.ApprovalDecisionDeny} {
		t.Run(string(decision), func(t *testing.T) {
			es := newEventMemStore()
			workflow := makeToolStepWorkflow("default", "publication")
			workflow.Spec.Steps = append(workflow.Spec.Steps, v1alpha1.WorkflowStep{
				Name: "publish", Type: v1alpha1.WorkflowStepTypeTool,
				ToolRef: v1alpha1.LocalObjectReference{Name: "publish"}, Input: `{"runID":"publication-run"}`,
			})
			workflow.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "fetch", To: "publish"}}
			workflow.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "fetch", To: "publish", RequiredApprovers: 1}}
			storeWorkflow(t, es.memStore, workflow)
			for _, name := range []string{"http-get", "publish"} {
				storeTool(t, es.memStore, v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "default", Name: name}})
			}
			storeWorkflowRun(t, es, makeWorkflowRun("default", "publication-run", "publication"))
			executor := &workflowRecordingExecutor{result: runtime.ToolExecutionResult{Output: map[string]any{"published": true}}}
			newReconciler := func() *WorkflowRunReconciler {
				reconciler := NewWorkflowRunReconciler(es, zaptest.NewLogger(t))
				reconciler.SetToolInvoker(NewDefaultWorkflowToolInvoker(es, nil, runtime.NewToolDispatcher(executor, nil), zaptest.NewLogger(t)))
				return reconciler
			}
			reconcileRunN(t, newReconciler(), "default", "publication-run", 8)
			pending := getWorkflowRun(t, es, "default", "publication-run")
			if executor.calls != 1 || executor.request.Tool.ObjectMeta.Name != "http-get" {
				t.Fatalf("before approval: calls=%d, last tool=%q; want preparation only", executor.calls, executor.request.Tool.ObjectMeta.Name)
			}
			if condition := findCondition(pending.Status.Conditions, "Ready"); condition == nil || condition.Reason != "WaitingApproval" {
				t.Fatalf("before approval: condition=%+v, want WaitingApproval", condition)
			}
			reconcileRunN(t, newReconciler(), "default", "publication-run", 5)
			if executor.calls != 1 {
				t.Fatal("restart bypassed pending approval")
			}
			storeApprovalDecision(t, es.memStore, makeApprovalDecision("default", "publish-decision", "publication-run", "publish", "reviewer", decision))
			reconcileRunN(t, newReconciler(), "default", "publication-run", 8)
			completed := getWorkflowRun(t, es, "default", "publication-run")
			wantCalls := 1
			if decision == v1alpha1.ApprovalDecisionApprove {
				wantCalls = 2
				if completed.Status.Phase != v1alpha1.PhaseReady || executor.request.Tool.ObjectMeta.Name != "publish" || executor.request.Input["runID"] != "publication-run" {
					t.Fatalf("approved publication: phase=%q, request=%+v", completed.Status.Phase, executor.request)
				}
			} else if completed.Status.Phase != v1alpha1.PhaseFailed {
				t.Fatalf("rejected publication phase=%q, want Failed", completed.Status.Phase)
			}
			reconcileRunN(t, newReconciler(), "default", "publication-run", 8)
			if executor.calls != wantCalls {
				t.Fatalf("calls after decision and restart=%d, want %d", executor.calls, wantCalls)
			}
			if jobs := listWorkflowJobs(t, es.memStore, "default"); len(jobs) != 0 {
				t.Fatalf("Tool workflow created %d AgentJobs", len(jobs))
			}
		})
	}
}

func markWorkflowRunStepReady(t *testing.T, es *eventMemStore, ns, runName, stepName, output string) {
	t.Helper()
	jobs := listWorkflowJobs(t, es.memStore, ns)
	for _, job := range jobs {
		if job.ObjectMeta.Labels[workflowStepLabel] != stepName || !agentJobOwnedBy(job, runName) {
			continue
		}
		job.Status.Phase = v1alpha1.PhaseReady
		job.Status.Output = output
		updateJob(t, es.memStore, job)
		return
	}
	t.Fatalf("AgentJob for workflow run %s step %s not found", runName, stepName)
}

// driveRunToCompletion completes all pending/running jobs for a run and reconciles in a loop
// until the run reaches a terminal phase or maxRounds is exhausted.
func driveRunToCompletion(t *testing.T, rec *WorkflowRunReconciler, es *eventMemStore, ns, name string) {
	t.Helper()
	for round := 0; round < 20; round++ {
		run := getWorkflowRun(t, es, ns, name)
		if run.Status.Phase == v1alpha1.PhaseReady || run.Status.Phase == v1alpha1.PhaseFailed {
			return
		}
		// Complete all pending/running jobs owned by this run.
		jobs := listWorkflowJobs(t, es.memStore, ns)
		for _, j := range jobs {
			if j.Status.Phase == v1alpha1.PhaseReady || j.Status.Phase == v1alpha1.PhaseFailed {
				continue
			}
			for _, ref := range j.ObjectMeta.OwnerReferences {
				if ref.Name == name {
					j.Status.Phase = v1alpha1.PhaseReady
					j.Status.Output = "done"
					updateJob(t, es.memStore, j)
					break
				}
			}
		}
		reconcileRunN(t, rec, ns, name, 5)
	}
	t.Fatalf("run %s did not reach terminal phase after 20 rounds", name)
}

// TC-0224: Multiple WorkflowRuns from same Workflow with different params run independently.
func TestWorkflowRunReconciler_TC0224_IndependentRunsWithParams(t *testing.T) {
	es := newEventMemStore()
	logger := zaptest.NewLogger(t)

	// Create a template workflow with a parameter and two steps.
	wf := makeWorkflow("default", "my-wf")
	wf.Spec.Parameters = map[string]string{"env": "staging"}
	storeWorkflow(t, es.memStore, wf)

	// Create 3 runs with different parameter overrides.
	for i, env := range []string{"dev", "qa", "prod"} {
		run := makeWorkflowRun("default", fmt.Sprintf("run-%d", i), "my-wf")
		run.Spec.Parameters = map[string]string{"env": env}
		storeWorkflowRun(t, es, run)
	}

	rec := NewWorkflowRunReconciler(es, logger)

	// Reconcile each run through finalizer addition.
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("run-%d", i)
		reconcileRunN(t, rec, "default", name, 3)
	}

	// Each run should now be running with its own jobs.
	for i, env := range []string{"dev", "qa", "prod"} {
		name := fmt.Sprintf("run-%d", i)
		run := getWorkflowRun(t, es, "default", name)

		if run.Status.Phase != v1alpha1.PhaseRunning {
			t.Errorf("run-%d: expected PhaseRunning, got %s", i, run.Status.Phase)
		}

		// Verify snapshot captured the merged params.
		if run.Status.SnapshotSpec == nil {
			t.Fatalf("run-%d: expected snapshot spec", i)
		}
		if got := run.Status.SnapshotSpec.Parameters["env"]; got != env {
			t.Errorf("run-%d: expected param env=%s, got %s", i, env, got)
		}
	}
}

// TC-0225: MaxConcurrentRuns queues excess runs.
func TestWorkflowRunReconciler_TC0225_MaxConcurrentRuns(t *testing.T) {
	es := newEventMemStore()
	logger := zaptest.NewLogger(t)

	wf := makeWorkflow("default", "limited-wf")
	wf.Spec.MaxConcurrentRuns = 1
	storeWorkflow(t, es.memStore, wf)

	// Create two runs.
	run1 := makeWorkflowRun("default", "run-1", "limited-wf")
	run1.ObjectMeta.CreationTimestamp = time.Now().Add(-time.Minute)
	storeWorkflowRun(t, es, run1)

	run2 := makeWorkflowRun("default", "run-2", "limited-wf")
	storeWorkflowRun(t, es, run2)

	rec := NewWorkflowRunReconciler(es, logger)

	// Reconcile run-1 to Running state.
	reconcileRunN(t, rec, "default", "run-1", 3)
	r1 := getWorkflowRun(t, es, "default", "run-1")
	if r1.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("run-1: expected PhaseRunning, got %s", r1.Status.Phase)
	}

	// Reconcile run-2 — should be queued (Pending).
	reconcileRunN(t, rec, "default", "run-2", 3)
	r2 := getWorkflowRun(t, es, "default", "run-2")
	if r2.Status.Phase != v1alpha1.PhasePending {
		t.Errorf("run-2: expected PhasePending, got %s", r2.Status.Phase)
	}

	// Complete run-1 by driving it through all steps.
	driveRunToCompletion(t, rec, es, "default", "run-1")
	r1 = getWorkflowRun(t, es, "default", "run-1")
	if r1.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("run-1: expected PhaseReady, got %s", r1.Status.Phase)
	}

	// Now reconcile run-2 — should proceed.
	reconcileRunN(t, rec, "default", "run-2", 3)
	r2 = getWorkflowRun(t, es, "default", "run-2")
	if r2.Status.Phase == v1alpha1.PhasePending {
		t.Errorf("run-2: should no longer be Pending after run-1 completed, got %s", r2.Status.Phase)
	}
}

func TestWorkflowRunReconciler_ConcurrencyFailuresDoNotQueueRun(t *testing.T) {
	tests := []struct {
		name        string
		listErr     error
		updateErr   error
		wantMessage string
	}{
		{name: "list failure", listErr: errors.New("list unavailable"), wantMessage: "list workflow runs for concurrency check"},
		{name: "pending update failure", updateErr: errors.New("update unavailable"), wantMessage: "update run to pending"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseStore := newEventMemStore()
			workflow := makeWorkflow("default", "limited-failure-wf")
			workflow.Spec.MaxConcurrentRuns = 1

			active := makeWorkflowRun("default", "active-run", workflow.ObjectMeta.Name)
			active.Status.Phase = v1alpha1.PhaseRunning
			storeWorkflowRun(t, baseStore, active)

			queued := makeWorkflowRun("default", "queued-run", workflow.ObjectMeta.Name)
			queued.ObjectMeta.Finalizers = []string{WorkflowRunFinalizer}
			queued.ObjectMeta.Annotations = map[string]string{observability.AnnotationMetricsStarted: "true"}
			queued.Status.SnapshotSpec = &workflow.Spec
			storeWorkflowRun(t, baseStore, queued)

			faults := &workflowRunConcurrencyFaultStore{
				eventMemStore: baseStore,
				listErr:       test.listErr,
				updateErr:     test.updateErr,
			}
			reconciler := NewWorkflowRunReconciler(faults, zaptest.NewLogger(t))
			_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: queued.ObjectMeta.Name})
			if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("error = %v, want %q", err, test.wantMessage)
			}

			stored := getWorkflowRun(t, baseStore, "default", queued.ObjectMeta.Name)
			if stored.Status.Phase == v1alpha1.PhasePending {
				t.Fatal("run was persisted as Pending despite failed concurrency decision")
			}
		})
	}
}

// TC-0226: HistoryLimit garbage-collects oldest completed runs.
func TestWorkflowRunReconciler_TC0226_HistoryLimit(t *testing.T) {
	es := newEventMemStore()
	logger := zaptest.NewLogger(t)

	wf := makeWorkflow("default", "gc-wf")
	wf.Spec.HistoryLimit = 2
	storeWorkflow(t, es.memStore, wf)

	rec := NewWorkflowRunReconciler(es, logger)

	// Create and complete 4 runs.
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("gc-run-%d", i)
		run := makeWorkflowRun("default", name, "gc-wf")
		run.ObjectMeta.CreationTimestamp = time.Now().Add(time.Duration(i) * time.Minute)
		storeWorkflowRun(t, es, run)

		driveRunToCompletion(t, rec, es, "default", name)
	}

	// After gc-run-3 completes, history GC should have deleted the 2 oldest.
	// Remaining: gc-run-2 and gc-run-3.
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("gc-run-%d", i)
		_, err := es.Get(context.Background(), "WorkflowRun", "default", name)
		if err == nil {
			t.Errorf("expected %s to be garbage-collected", name)
		}
	}
	for i := 2; i < 4; i++ {
		name := fmt.Sprintf("gc-run-%d", i)
		_, err := es.Get(context.Background(), "WorkflowRun", "default", name)
		if err != nil {
			t.Errorf("expected %s to still exist, got %v", name, err)
		}
	}
}

func TestWorkflowRunReconciler_HistoryLimitUsesCompletionOrder(t *testing.T) {
	es := newEventMemStore()
	baseTime := time.Now().Add(-time.Hour)
	runs := []v1alpha1.WorkflowRun{
		makeWorkflowRun("default", "created-first-completed-last", "gc-wf"),
		makeWorkflowRun("default", "created-last-completed-first", "gc-wf"),
		makeWorkflowRun("default", "current", "gc-wf"),
	}
	creationOffsets := []time.Duration{0, 2 * time.Minute, time.Minute}
	completionOffsets := []time.Duration{20 * time.Minute, 10 * time.Minute, 15 * time.Minute}
	for index := range runs {
		runs[index].ObjectMeta.CreationTimestamp = baseTime.Add(creationOffsets[index])
		runs[index].Status.Phase = v1alpha1.PhaseReady
		runs[index].Status.Conditions = []v1alpha1.Condition{{
			Type:               "Ready",
			Status:             "True",
			LastTransitionTime: baseTime.Add(completionOffsets[index]),
		}}
		storeWorkflowRun(t, es, runs[index])
	}

	reconciler := NewWorkflowRunReconciler(es, zaptest.NewLogger(t))
	reconciler.enforceHistoryLimit(context.Background(), &runs[2], 2, zaptest.NewLogger(t))

	if _, err := es.Get(context.Background(), "WorkflowRun", "default", runs[1].ObjectMeta.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("oldest completed run lookup error = %v, want ErrNotFound", err)
	}
	for _, name := range []string{runs[0].ObjectMeta.Name, runs[2].ObjectMeta.Name} {
		if _, err := es.Get(context.Background(), "WorkflowRun", "default", name); err != nil {
			t.Fatalf("expected later-completed run %s to survive: %v", name, err)
		}
	}
}

// TC-0227: WorkflowRun continues when Workflow template is deleted (uses SnapshotSpec).
func TestWorkflowRunReconciler_TC0227_TemplateDeletedUsesSnapshot(t *testing.T) {
	es := newEventMemStore()
	logger := zaptest.NewLogger(t)

	wf := makeWorkflow("default", "ephemeral-wf")
	storeWorkflow(t, es.memStore, wf)

	run := makeWorkflowRun("default", "survivor-run", "ephemeral-wf")
	storeWorkflowRun(t, es, run)

	rec := NewWorkflowRunReconciler(es, logger)

	// Reconcile to snapshot the spec and start running.
	reconcileRunN(t, rec, "default", "survivor-run", 3)
	r := getWorkflowRun(t, es, "default", "survivor-run")
	if r.Status.SnapshotSpec == nil {
		t.Fatal("expected snapshot spec after first reconcile")
	}
	if r.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("expected PhaseRunning, got %s", r.Status.Phase)
	}

	// Delete the template workflow.
	if err := es.Delete(context.Background(), "Workflow", "default", "ephemeral-wf"); err != nil {
		t.Fatalf("delete workflow: %v", err)
	}

	// Complete the first step job.
	jobs := listWorkflowJobs(t, es.memStore, "default")
	for i := range jobs {
		if jobs[i].Status.Phase == "" || jobs[i].Status.Phase == v1alpha1.PhasePending {
			jobs[i].Status.Phase = v1alpha1.PhaseReady
			jobs[i].Status.Output = "step done"
			updateJob(t, es.memStore, jobs[i])
		}
	}

	// Reconcile again — should continue using snapshot.
	reconcileRunN(t, rec, "default", "survivor-run", 5)
	r = getWorkflowRun(t, es, "default", "survivor-run")

	// It should have progressed (created job for second step or completed).
	// At minimum, it should NOT be Failed with TemplateNotFound.
	if r.Status.Phase == v1alpha1.PhaseFailed {
		for _, c := range r.Status.Conditions {
			if c.Reason == "TemplateNotFound" {
				t.Fatal("run should not fail with TemplateNotFound when snapshot exists")
			}
		}
	}
}

func TestWorkflowRunReconciler_ResolveSpecIsNamespaceScoped(t *testing.T) {
	es := newEventMemStore()
	logger := zaptest.NewLogger(t)
	storeWorkflow(t, es.memStore, makeWorkflow("other", "shared-name"))
	run := makeWorkflowRun("tenant-a", "isolated-run", "shared-name")
	storeWorkflowRun(t, es, run)
	run = getWorkflowRun(t, es, "tenant-a", "isolated-run")

	reconciler := NewWorkflowRunReconciler(es, logger)
	spec, err := reconciler.resolveSpec(context.Background(), &run, logger)
	if err == nil {
		t.Fatal("resolveSpec succeeded with a template from another namespace")
	}
	if spec != nil {
		t.Fatalf("resolveSpec returned spec from another namespace: %#v", spec)
	}

	stored := getWorkflowRun(t, es, "tenant-a", "isolated-run")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want %q", stored.Status.Phase, v1alpha1.PhaseFailed)
	}
	if len(stored.Status.Conditions) != 1 || stored.Status.Conditions[0].Reason != "TemplateNotFound" {
		t.Fatalf("conditions = %#v, want TemplateNotFound", stored.Status.Conditions)
	}
}

// TC-0245: WorkflowRun reconciler emits lifecycle metrics exactly once per run
// across multiple reconciles: gauge increments on Start, decrements on terminal,
// runs_total{phase} increments once, and step duration histogram is observed.
func TestWorkflowRunReconciler_TC0245_EmitsLifecycleMetricsOnce(t *testing.T) {

	es := newEventMemStore()
	registry := prometheus.NewRegistry()
	observability.NewWorkflowMetrics(registry, es)
	logger := zaptest.NewLogger(t)
	wf := makeWorkflow("default", "metric-wf")
	storeWorkflow(t, es.memStore, wf)
	run := makeWorkflowRun("default", "metric-run", "metric-wf")
	storeWorkflowRun(t, es, run)

	rec := NewWorkflowRunReconciler(es, logger)
	// Drive run to completion while also stamping CompletionTime so the step
	// duration histogram observes a sample (driveRunToCompletion only sets phase).
	for round := 0; round < 20; round++ {
		cur := getWorkflowRun(t, es, "default", "metric-run")
		if cur.Status.Phase == v1alpha1.PhaseReady || cur.Status.Phase == v1alpha1.PhaseFailed {
			break
		}
		for _, j := range listWorkflowJobs(t, es.memStore, "default") {
			if j.Status.Phase == v1alpha1.PhaseReady || j.Status.Phase == v1alpha1.PhaseFailed {
				continue
			}
			owned := false
			for _, ref := range j.ObjectMeta.OwnerReferences {
				if ref.Name == "metric-run" {
					owned = true
					break
				}
			}
			if !owned {
				continue
			}
			j.Status.Phase = v1alpha1.PhaseReady
			j.Status.Output = "done"
			ct := time.Now()
			j.Status.CompletionTime = &ct
			updateJob(t, es.memStore, j)
		}
		reconcileRunN(t, rec, "default", "metric-run", 5)
	}

	// Reconcile a few more times after terminal phase to assert idempotency.
	reconcileRunN(t, rec, "default", "metric-run", 3)

	final := getWorkflowRun(t, es, "default", "metric-run")
	if final.Status.Phase != v1alpha1.PhaseReady && final.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("run phase = %q, want terminal", final.Status.Phase)
	}
	if final.ObjectMeta.Annotations[observability.AnnotationMetricsStarted] != "true" {
		t.Fatalf("metrics-started annotation missing")
	}
	if final.ObjectMeta.Annotations[observability.AnnotationMetricsCompleted] != "true" {
		t.Fatalf("metrics-completed annotation missing")
	}

	phase := string(final.Status.Phase)
	assertRetainedWorkflowGauge(t, registry, "kognition_workflow_active_runs", map[string]string{"workflow": "metric-wf"}, 0)
	assertRetainedWorkflowGauge(t, registry, "kognition_workflow_retained_runs", map[string]string{"workflow": "metric-wf", "phase": phase}, 1)
	assertRetainedWorkflowGauge(t, registry, "kognition_workflow_retained_step_duration_observations", map[string]string{"workflow": "metric-wf", "step": "plan"}, 1)
}

// TC-0246: Deleting a WorkflowRun mid-flight decrements the active gauge and
// emits a Failed terminal counter so the gauge does not leak.
func TestWorkflowRunReconciler_TC0246_DeletionDecrementsActiveGauge(t *testing.T) {
	es := newEventMemStore()
	registry := prometheus.NewRegistry()
	observability.NewWorkflowMetrics(registry, es)
	logger := zaptest.NewLogger(t)
	wf := makeWorkflow("default", "del-wf")
	wf.Spec.MemoryClaim = &v1alpha1.WorkflowMemoryClaim{MemoryClassName: "shared-class"}
	wf.Spec.Steps[0].MemoryClaim = &v1alpha1.WorkflowMemoryClaim{MemoryClassName: "step-class"}
	storeWorkflow(t, es.memStore, wf)
	run := makeWorkflowRun("default", "del-run", "del-wf")
	storeWorkflowRun(t, es, run)

	rec := NewWorkflowRunReconciler(es, logger)
	// Add finalizer + start (gauge +1).
	reconcileRunN(t, rec, "default", "del-run", 3)

	stored := getWorkflowRun(t, es, "default", "del-run")
	if stored.ObjectMeta.Annotations[observability.AnnotationMetricsStarted] != "true" {
		t.Fatalf("expected MetricsStarted annotation after first reconciles")
	}
	assertRetainedWorkflowGauge(t, registry, "kognition_workflow_active_runs", map[string]string{"workflow": "del-wf"}, 1)
	if !memoryClaimExists(t, es.memStore, "default", "del-run-memory") ||
		!memoryClaimExists(t, es.memStore, "default", "del-run-plan-memory") {
		t.Fatal("expected workflow and step memory claims before deletion")
	}

	// Mark for deletion mid-flight (before terminal phase).
	now := time.Now()
	stored.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := es.Update(context.Background(), "WorkflowRun", "default", "del-run", data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update with deletion timestamp: %v", err)
	}

	// Drive deletion handling (handleDeletion → removeFinalizer → store.Delete).
	reconcileRunN(t, rec, "default", "del-run", 5)

	restarted := prometheus.NewRegistry()
	observability.NewWorkflowMetrics(restarted, es)
	assertRetainedWorkflowGauge(t, restarted, "kognition_workflow_active_runs", map[string]string{"workflow": "del-wf"}, 0)
	assertRetainedWorkflowGauge(t, restarted, "kognition_workflow_retained_runs", map[string]string{"workflow": "del-wf", "phase": "Failed"}, 1)
	if memoryClaimExists(t, es.memStore, "default", "del-run-memory") ||
		memoryClaimExists(t, es.memStore, "default", "del-run-plan-memory") {
		t.Fatal("memory claims survived mid-flight WorkflowRun deletion")
	}
}

func TestWorkflowRunReconciler_DeletionRetriesMemoryClaimCleanup(t *testing.T) {
	baseStore := newEventMemStore()
	s := &workflowRunMemoryClaimDeleteStore{eventMemStore: baseStore}
	registry := prometheus.NewRegistry()
	observability.NewWorkflowMetrics(registry, s)
	logger := zaptest.NewLogger(t)
	wf := makeWorkflow("default", "cleanup-wf")
	wf.Spec.MemoryClaim = &v1alpha1.WorkflowMemoryClaim{MemoryClassName: "shared-class"}
	storeWorkflow(t, baseStore.memStore, wf)
	run := makeWorkflowRun("default", "cleanup-run", "cleanup-wf")
	storeWorkflowRun(t, s, run)

	rec := NewWorkflowRunReconciler(s, logger)
	reconcileRunN(t, rec, "default", "cleanup-run", 3)
	stored := getWorkflowRun(t, s, "default", "cleanup-run")
	now := time.Now()
	stored.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := s.Update(context.Background(), "WorkflowRun", "default", "cleanup-run", data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark run for deletion: %v", err)
	}

	s.failKind = "MemoryClaim"
	_, err = rec.Reconcile(context.Background(), Request{Namespace: "default", Name: "cleanup-run"})
	if err == nil {
		t.Fatal("expected memory claim cleanup error")
	}
	stored = getWorkflowRun(t, s, "default", "cleanup-run")
	if !hasString(stored.ObjectMeta.Finalizers, WorkflowRunFinalizer) {
		t.Fatal("WorkflowRun finalizer removed before memory claim cleanup succeeded")
	}
	if !memoryClaimExists(t, baseStore.memStore, "default", "cleanup-run-memory") {
		t.Fatal("memory claim missing after injected delete failure")
	}

	s.failKind = ""
	if _, err := rec.Reconcile(context.Background(), Request{Namespace: "default", Name: "cleanup-run"}); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	stored = getWorkflowRun(t, s, "default", "cleanup-run")
	if hasString(stored.ObjectMeta.Finalizers, WorkflowRunFinalizer) {
		t.Fatal("WorkflowRun finalizer retained after memory claim cleanup succeeded")
	}
	if memoryClaimExists(t, baseStore.memStore, "default", "cleanup-run-memory") {
		t.Fatal("memory claim survived successful cleanup retry")
	}
	assertRetainedWorkflowGauge(t, registry, "kognition_workflow_active_runs", map[string]string{"workflow": "cleanup-wf"}, 0)
	assertRetainedWorkflowGauge(t, registry, "kognition_workflow_retained_runs", map[string]string{"workflow": "cleanup-wf", "phase": "Failed"}, 1)
}

func TestWorkflowRunReconciler_DeletionRetriesOwnedJobCleanup(t *testing.T) {
	baseStore := newEventMemStore()
	s := &workflowRunMemoryClaimDeleteStore{eventMemStore: baseStore}
	wf := makeWorkflow("default", "job-cleanup-wf")
	storeWorkflow(t, baseStore.memStore, wf)
	run := makeWorkflowRun("default", "job-cleanup-run", "job-cleanup-wf")
	storeWorkflowRun(t, s, run)
	rec := NewWorkflowRunReconciler(s, zaptest.NewLogger(t))
	reconcileRunN(t, rec, "default", "job-cleanup-run", 3)
	if jobs := listWorkflowJobs(t, baseStore.memStore, "default"); len(jobs) == 0 {
		t.Fatal("expected an owned AgentJob before deletion")
	}

	stored := getWorkflowRun(t, s, "default", "job-cleanup-run")
	now := time.Now()
	stored.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := s.Update(context.Background(), "WorkflowRun", "default", "job-cleanup-run", data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark run for deletion: %v", err)
	}

	s.failKind = "AgentJob"
	if _, err := rec.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-cleanup-run"}); err == nil {
		t.Fatal("expected owned AgentJob cleanup error")
	}
	stored = getWorkflowRun(t, s, "default", "job-cleanup-run")
	if !hasString(stored.ObjectMeta.Finalizers, WorkflowRunFinalizer) {
		t.Fatal("WorkflowRun finalizer removed before owned AgentJob cleanup succeeded")
	}
	if jobs := listWorkflowJobs(t, baseStore.memStore, "default"); len(jobs) == 0 {
		t.Fatal("owned AgentJob missing after injected delete failure")
	}

	s.failKind = ""
	if _, err := rec.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-cleanup-run"}); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	stored = getWorkflowRun(t, s, "default", "job-cleanup-run")
	if hasString(stored.ObjectMeta.Finalizers, WorkflowRunFinalizer) {
		t.Fatal("WorkflowRun finalizer retained after owned AgentJob cleanup succeeded")
	}
	if jobs := listWorkflowJobs(t, baseStore.memStore, "default"); len(jobs) != 0 {
		t.Fatalf("owned AgentJobs survived cleanup retry: %d", len(jobs))
	}
}

func TestWorkflowRunReconciler_DeletionRetainsFinalizerWhenJobListingFails(t *testing.T) {
	baseStore := newEventMemStore()
	s := &workflowRunJobListFaultStore{eventMemStore: baseStore, err: fmt.Errorf("injected AgentJob list failure")}
	run := makeWorkflowRun("default", "job-list-failure-run", "job-list-failure-wf")
	run.ObjectMeta.Finalizers = []string{WorkflowRunFinalizer}
	now := time.Now()
	run.ObjectMeta.DeletionTimestamp = &now
	storeWorkflowRun(t, s, run)

	reconciler := NewWorkflowRunReconciler(s, zaptest.NewLogger(t))
	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: run.ObjectMeta.Name}); err == nil {
		t.Fatal("expected owned AgentJob list failure")
	}

	stored := getWorkflowRun(t, s, "default", run.ObjectMeta.Name)
	if !hasString(stored.ObjectMeta.Finalizers, WorkflowRunFinalizer) {
		t.Fatal("WorkflowRun finalizer removed before owned AgentJobs could be listed")
	}
}

func TestWorkflowRunReconciler_DeletionSkipsMalformedJobs(t *testing.T) {
	es := newEventMemStore()
	run := makeWorkflowRun("default", "malformed-job-run", "workflow")
	run.ObjectMeta.Finalizers = []string{WorkflowRunFinalizer}
	now := time.Now()
	run.ObjectMeta.DeletionTimestamp = &now
	storeWorkflowRun(t, es, run)

	ownedJob := v1alpha1.AgentJob{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentJob"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "owned-job",
			Namespace: run.ObjectMeta.Namespace,
			UID:       "owned-job-uid",
			OwnerReferences: []v1alpha1.OwnerReference{{
				Kind: "WorkflowRun", Name: run.ObjectMeta.Name, UID: run.ObjectMeta.UID,
			}},
		},
	}
	storeJob(t, es.memStore, ownedJob)
	if err := es.Create(context.Background(), "AgentJob", run.ObjectMeta.Namespace, "malformed-job", []byte(`{"metadata":`)); err != nil {
		t.Fatalf("store malformed AgentJob: %v", err)
	}

	reconciler := NewWorkflowRunReconciler(es, zaptest.NewLogger(t))
	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: run.ObjectMeta.Namespace, Name: run.ObjectMeta.Name}); err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}

	stored := getWorkflowRun(t, es, run.ObjectMeta.Namespace, run.ObjectMeta.Name)
	if hasString(stored.ObjectMeta.Finalizers, WorkflowRunFinalizer) {
		t.Fatal("WorkflowRun finalizer retained after valid owned jobs were cleaned up")
	}
	if _, err := es.Get(context.Background(), "AgentJob", ownedJob.ObjectMeta.Namespace, ownedJob.ObjectMeta.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("owned AgentJob lookup error = %v, want ErrNotFound", err)
	}
	if _, err := es.Get(context.Background(), "AgentJob", run.ObjectMeta.Namespace, "malformed-job"); err != nil {
		t.Fatalf("malformed AgentJob was removed: %v", err)
	}
}

func TestWorkflowRunReconciler_DeletionPreservesSameNameForeignResources(t *testing.T) {
	es := newEventMemStore()
	wf := makeWorkflow("default", "foreign-cleanup-wf")
	storeWorkflow(t, es.memStore, wf)
	run := makeWorkflowRun("default", "foreign-cleanup-run", "foreign-cleanup-wf")
	storeWorkflowRun(t, es, run)
	rec := NewWorkflowRunReconciler(es, zaptest.NewLogger(t))
	reconcileRunN(t, rec, "default", "foreign-cleanup-run", 3)

	foreignOwner := []v1alpha1.OwnerReference{{
		Kind: "WorkflowRun", Name: "foreign-cleanup-run", UID: "different-run-uid",
	}}
	foreignJob := v1alpha1.AgentJob{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentJob"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name: "foreign-job", Namespace: "default", UID: "foreign-job-uid", OwnerReferences: foreignOwner,
		},
	}
	storeJob(t, es.memStore, foreignJob)
	foreignClaim := v1alpha1.MemoryClaim{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "MemoryClaim"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name: "foreign-cleanup-run-memory", Namespace: "default", UID: "foreign-claim-uid", OwnerReferences: foreignOwner,
		},
	}
	claimData, err := json.Marshal(foreignClaim)
	if err != nil {
		t.Fatalf("marshal foreign claim: %v", err)
	}
	if err := es.Create(context.Background(), "MemoryClaim", "default", foreignClaim.ObjectMeta.Name, claimData); err != nil {
		t.Fatalf("store foreign claim: %v", err)
	}

	stored := getWorkflowRun(t, es, "default", "foreign-cleanup-run")
	now := time.Now()
	stored.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	if err := es.Update(context.Background(), "WorkflowRun", "default", "foreign-cleanup-run", data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark run for deletion: %v", err)
	}
	if _, err := rec.Reconcile(context.Background(), Request{Namespace: "default", Name: "foreign-cleanup-run"}); err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}

	stored = getWorkflowRun(t, es, "default", "foreign-cleanup-run")
	if hasString(stored.ObjectMeta.Finalizers, WorkflowRunFinalizer) {
		t.Fatal("WorkflowRun finalizer retained after owned cleanup completed")
	}
	if _, err := es.Get(context.Background(), "AgentJob", "default", foreignJob.ObjectMeta.Name); err != nil {
		t.Fatalf("foreign AgentJob was removed: %v", err)
	}
	if !memoryClaimExists(t, es.memStore, "default", foreignClaim.ObjectMeta.Name) {
		t.Fatal("foreign MemoryClaim was removed")
	}
}
