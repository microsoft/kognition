// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// fakeCollaborationRunner replays scripted progress per Advance call.
type fakeCollaborationRunner struct {
	script   []WorkflowCollaborationProgress
	err      error
	advances []WorkflowCollaborationRun
	stops    []string
}

func (f *fakeCollaborationRunner) Advance(_ context.Context, run WorkflowCollaborationRun) (WorkflowCollaborationProgress, error) {
	f.advances = append(f.advances, run)
	if f.err != nil {
		return WorkflowCollaborationProgress{}, f.err
	}
	idx := len(f.advances) - 1
	if idx >= len(f.script) {
		idx = len(f.script) - 1
	}
	return f.script[idx], nil
}

func (f *fakeCollaborationRunner) Stop(_ context.Context, run WorkflowCollaborationRun, reason string) error {
	f.stops = append(f.stops, run.Step+":"+reason)
	return nil
}

func makeCollaborationWorkflow(name string) v1alpha1.Workflow {
	return v1alpha1.Workflow{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: "default", UID: "wf-collab-uid"},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{{
				Name:  "debate",
				Type:  v1alpha1.WorkflowStepTypeCollaboration,
				Input: "review the proposal",
				Collaboration: &v1alpha1.WorkflowCollaboration{
					Strategy: v1alpha1.CollaborationStrategySupervisor,
					Protocol: v1alpha1.CollaborationProtocolA2A,
					Limits:   v1alpha1.CollaborationLimits{MaxContentBytes: 8192},
				},
			}},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
}

func TestWorkflowRunReconciler_RejectsInvalidResolvedCollaborationInput(t *testing.T) {
	tests := map[string]string{
		"blank":     " \n ",
		"oversized": strings.Repeat("x", 8193),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			s := newMemStore()
			r := NewWorkflowRunReconciler(s, zaptest.NewLogger(t))
			runner := &fakeCollaborationRunner{err: errors.New("unexpected member dispatch")}
			r.SetCollaborationRunner(runner)

			wf := makeCollaborationWorkflow("wf-invalid-input")
			wf.Spec.Steps[0].Input = "{{ .Input }}"
			wf.Spec.Steps[0].Timeout = "1m"
			storeWorkflow(t, s, wf)
			run := makeWorkflowRun("default", "invalid-input-run", wf.ObjectMeta.Name)
			run.Spec.Input = input
			storeWorkflowRun(t, s, run)
			reconcileRunN(t, r, run.ObjectMeta.Namespace, run.ObjectMeta.Name, 3)

			got := getWorkflowRun(t, s, run.ObjectMeta.Namespace, run.ObjectMeta.Name)
			if got.Status.Phase != v1alpha1.PhaseFailed ||
				len(got.Status.StepStatuses) != 1 || got.Status.StepStatuses[0].Phase != v1alpha1.PhaseFailed {
				t.Fatalf("expected a terminal step and run failure, got %+v", got.Status)
			}
			if len(runner.advances) != 0 {
				t.Fatalf("invalid input dispatched to the runner: %d calls", len(runner.advances))
			}
		})
	}
}

func TestWorkflowRunReconciler_PersistsCollaborationStartOnRunnerError(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowRunReconciler(s, zaptest.NewLogger(t))
	runner := &fakeCollaborationRunner{err: errors.New("runner unavailable")}
	r.SetCollaborationRunner(runner)
	wf := makeCollaborationWorkflow("wf-runner-error")
	wf.Spec.Steps[0].Timeout = "1m"
	storeWorkflow(t, s, wf)
	run := makeWorkflowRun("default", "runner-error-run", wf.ObjectMeta.Name)
	storeWorkflowRun(t, s, run)
	req := Request{Namespace: run.ObjectMeta.Namespace, Name: run.ObjectMeta.Name}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); !errors.Is(err, runner.err) {
		t.Fatalf("expected a runner error, got %v", err)
	}

	got := getWorkflowRun(t, s, req.Namespace, req.Name)
	if got.Status.Phase != v1alpha1.PhaseRunning || len(got.Status.StepStatuses) != 1 {
		t.Fatalf("the active attempt was not persisted: %+v", got.Status)
	}
	step := got.Status.StepStatuses[0]
	if step.StartedAt == nil || step.Phase != v1alpha1.PhaseRunning || step.Attempts != 1 {
		t.Fatalf("expected running attempt 1 with a start time, got %+v", step)
	}
}

func TestWorkflowRunReconciler_StopsCollaborationOnSiblingTemplateFailure(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowRunReconciler(s, zaptest.NewLogger(t))
	runner := &fakeCollaborationRunner{script: []WorkflowCollaborationProgress{{}}}
	r.SetCollaborationRunner(runner)
	wf := makeCollaborationWorkflow("wf-sibling")
	wf.Spec.Steps[0].Timeout = "1m"
	wf.Spec.Steps = append(wf.Spec.Steps, v1alpha1.WorkflowStep{
		Name: "summarize", AgentRef: v1alpha1.LocalObjectReference{Name: "summarizer"},
		Input: "{{ .Parameters.missing }}",
	})
	storeWorkflow(t, s, wf)
	run := makeWorkflowRun("default", "sibling-run", wf.ObjectMeta.Name)
	storeWorkflowRun(t, s, run)
	reconcileRunN(t, r, run.ObjectMeta.Namespace, run.ObjectMeta.Name, 3)

	got := getWorkflowRun(t, s, run.ObjectMeta.Namespace, run.ObjectMeta.Name)
	if got.Status.Phase != v1alpha1.PhaseFailed || len(runner.advances) != 1 {
		t.Fatalf("expected a failed run after a member was dispatched: phase=%s, advances=%d", got.Status.Phase, len(runner.advances))
	}
	if len(runner.stops) != 1 || runner.stops[0] != "debate:WorkflowFailed" {
		t.Fatalf("stops = %v, want [debate:WorkflowFailed]", runner.stops)
	}
}

func collaborationEvents(t *testing.T, s store.Store, name string) []string {
	t.Helper()
	evts, err := s.ListEvents(context.Background(), "Workflow", "default", name)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var reasons []string
	for _, ev := range evts {
		reasons = append(reasons, ev.Reason)
	}
	return reasons
}

func TestWorkflowReconciler_CollaborationStep_RunsThenSucceedsAndFeedsDownstream(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	runner := &fakeCollaborationRunner{script: []WorkflowCollaborationProgress{
		{},
		{Done: true, Succeeded: true, Output: "consensus reached"},
	}}
	r.SetCollaborationRunner(runner)

	wf := makeCollaborationWorkflow("wf-collab-ok")
	wf.Spec.Steps = append(wf.Spec.Steps, v1alpha1.WorkflowStep{
		Name:     "summarize",
		AgentRef: v1alpha1.LocalObjectReference{Name: "summarizer"},
		Input:    "result={{ .Steps.debate.Output }}",
	})
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "debate", To: "summarize"}}
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}
	mustReconcile(t, r, req) // finalizer
	mustReconcile(t, r, req) // dispatch, still running

	stored := getWorkflow(t, s, "default", wf.ObjectMeta.Name)
	if ss := findStepStatus(&stored, "debate"); ss == nil || ss.Phase != v1alpha1.PhaseRunning || ss.Attempts != 1 {
		t.Fatalf("step status after first pass = %+v, want Running attempt 1", ss)
	}

	if got := len(listWorkflowJobs(t, s, "default")); got != 0 {
		t.Fatalf("jobs before collaboration completes = %d, want 0", got)
	}

	for i := 0; i < 5 && len(listWorkflowJobs(t, s, "default")) == 0; i++ {
		mustReconcile(t, r, req)
	}
	jobs := listWorkflowJobs(t, s, "default")
	if len(jobs) != 1 || !strings.Contains(jobs[0].Spec.Input, "consensus reached") {
		t.Fatalf("downstream jobs = %+v, want one carrying the collaboration output", jobs)
	}
	stored = getWorkflow(t, s, "default", wf.ObjectMeta.Name)
	if ss := findStepStatus(&stored, "debate"); ss == nil || ss.Phase != v1alpha1.PhaseReady || ss.Attempts != 1 {
		t.Fatalf("step status = %+v, want Ready attempt 1", ss)
	}
	for _, run := range runner.advances {
		if run.Step != "debate" || run.Attempt != 1 || run.RunUID != "wf-collab-uid" || run.Spec == nil {
			t.Fatalf("advance run = %+v", run)
		}
	}
	if len(runner.stops) != 0 {
		t.Fatalf("unexpected stops: %v", runner.stops)
	}
}

func TestWorkflowReconciler_CollaborationPreservesConfiguredLargeOutput(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	output := strings.Repeat("\u00e9", 65536)
	runner := &fakeCollaborationRunner{script: []WorkflowCollaborationProgress{
		{Done: true, Succeeded: true, Output: output},
	}}
	r.SetCollaborationRunner(runner)
	wf := makeCollaborationWorkflow("wf-large-collaboration")
	wf.Spec.Steps[0].Collaboration.Limits.MaxContentBytes = 131072
	wf.Spec.Steps = append(wf.Spec.Steps, v1alpha1.WorkflowStep{
		Name: "summarize", AgentRef: v1alpha1.LocalObjectReference{Name: "summarizer"},
		Input: "{{ .Steps.debate.Output }}",
	})
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "debate", To: "summarize"}}
	storeWorkflow(t, s, wf)
	req := Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}
	for i := 0; i < 5 && len(listWorkflowJobs(t, s, "default")) == 0; i++ {
		mustReconcile(t, r, req)
	}
	stored := getWorkflow(t, s, "default", wf.ObjectMeta.Name)
	if ss := findStepStatus(&stored, "debate"); ss == nil || ss.Output != output {
		t.Fatal("configured collaboration output was truncated in workflow status")
	}
	jobs := listWorkflowJobs(t, s, "default")
	if len(jobs) != 1 || jobs[0].Spec.Input != output {
		t.Fatal("downstream step did not receive the complete collaboration result")
	}
}

func TestWorkflowReconciler_CollaborationStep_FailureRetriesWithNewAttempt(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	runner := &fakeCollaborationRunner{script: []WorkflowCollaborationProgress{
		{Done: true, Reason: "TurnLimit"},
		{Done: true, Succeeded: true, Output: "ok"},
	}}
	r.SetCollaborationRunner(runner)

	wf := makeCollaborationWorkflow("wf-collab-retry")
	wf.Spec.Steps[0].Retry = &v1alpha1.WorkflowStepRetry{MaxAttempts: 2, Backoff: "1ms"}
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}
	mustReconcile(t, r, req) // finalizer
	mustReconcile(t, r, req) // attempt 1 fails, retry scheduled
	stored := getWorkflow(t, s, "default", wf.ObjectMeta.Name)
	if stored.Status.Phase == v1alpha1.PhaseFailed {
		t.Fatalf("workflow failed during retry window: %+v", stored.Status)
	}
	if ss := findStepStatus(&stored, "debate"); ss == nil || ss.NextRetryAt == nil || ss.Phase != v1alpha1.PhasePending {
		t.Fatalf("step status = %+v, want Pending with retry scheduled", ss)
	}

	time.Sleep(5 * time.Millisecond)
	mustReconcile(t, r, req) // attempt 2 succeeds

	stored = getWorkflow(t, s, "default", wf.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", stored.Status.Phase)
	}
	if len(runner.advances) != 2 || runner.advances[0].Attempt != 1 || runner.advances[1].Attempt != 2 {
		t.Fatalf("advances = %+v, want attempts 1 then 2", runner.advances)
	}
}

func TestWorkflowReconciler_CollaborationStep_FailureWithoutRetryFailsWorkflow(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	runner := &fakeCollaborationRunner{script: []WorkflowCollaborationProgress{{Done: true, Reason: "AllowanceExhausted"}}}
	r.SetCollaborationRunner(runner)

	wf := makeCollaborationWorkflow("wf-collab-fail")
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}
	mustReconcile(t, r, req)
	mustReconcile(t, r, req)

	stored := getWorkflow(t, s, "default", wf.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", stored.Status.Phase)
	}
	ss := findStepStatus(&stored, "debate")
	if ss == nil || ss.Phase != v1alpha1.PhaseFailed || ss.Output != "" {
		t.Fatalf("step status = %+v, want Failed with empty output", ss)
	}
	found := false
	for _, reason := range collaborationEvents(t, s, wf.ObjectMeta.Name) {
		found = found || reason == "CollaborationStepFailed"
	}
	if !found {
		t.Fatalf("missing CollaborationStepFailed event")
	}
}

func TestWorkflowReconciler_CollaborationStep_RunnerErrorIsReturned(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	r.SetCollaborationRunner(&fakeCollaborationRunner{err: errors.New("ledger unavailable")})

	wf := makeCollaborationWorkflow("wf-collab-err")
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}
	mustReconcile(t, r, req)
	if _, err := r.reconcileDirect(context.Background(), req); err == nil || !strings.Contains(err.Error(), "ledger unavailable") {
		t.Fatalf("err = %v, want runner error", err)
	}
	stored := getWorkflow(t, s, "default", wf.ObjectMeta.Name)
	if stored.Status.Phase == v1alpha1.PhaseFailed {
		t.Fatalf("transient runner error must not fail the workflow")
	}
}

func TestWorkflowReconciler_CollaborationStep_TimeoutStopsRunner(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	runner := &fakeCollaborationRunner{script: []WorkflowCollaborationProgress{{}}}
	r.SetCollaborationRunner(runner)

	wf := makeCollaborationWorkflow("wf-collab-timeout")
	wf.Spec.Steps[0].Timeout = "5ms"
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}
	mustReconcile(t, r, req)
	mustReconcile(t, r, req) // running
	time.Sleep(10 * time.Millisecond)
	mustReconcile(t, r, req) // timed out

	stored := getWorkflow(t, s, "default", wf.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", stored.Status.Phase)
	}
	if len(runner.stops) != 1 || runner.stops[0] != "debate:StepTimedOut" {
		t.Fatalf("stops = %v, want [debate:StepTimedOut]", runner.stops)
	}
}

func TestWorkflowReconciler_CollaborationStep_SiblingFailureStopsRunner(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	runner := &fakeCollaborationRunner{script: []WorkflowCollaborationProgress{{}}}
	r.SetCollaborationRunner(runner)
	r.SetToolInvoker(&fakeWorkflowToolInvoker{err: errors.New("boom")})

	wf := makeCollaborationWorkflow("wf-collab-sibling")
	wf.Spec.Steps = append(wf.Spec.Steps, v1alpha1.WorkflowStep{
		Name:    "fetch",
		Type:    v1alpha1.WorkflowStepTypeTool,
		ToolRef: v1alpha1.LocalObjectReference{Name: "lookup"},
	})
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}
	for i := 0; i < 4; i++ {
		mustReconcile(t, r, req)
	}

	stored := getWorkflow(t, s, "default", wf.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", stored.Status.Phase)
	}
	if len(runner.stops) == 0 || runner.stops[0] != "debate:WorkflowFailed" {
		t.Fatalf("stops = %v, want debate:WorkflowFailed", runner.stops)
	}
}

func TestWorkflowReconciler_CollaborationStep_RejectedWithoutRunner(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))

	wf := makeCollaborationWorkflow("wf-collab-disabled")
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}
	mustReconcile(t, r, req)
	mustReconcile(t, r, req)

	stored := getWorkflow(t, s, "default", wf.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", stored.Status.Phase)
	}
	cond := findCondition(stored.Status.Conditions, "Ready")
	if cond == nil || cond.Reason != "InvalidDAG" {
		t.Fatalf("condition = %+v, want InvalidDAG", cond)
	}
}

func TestBuildWorkflowPlan_CollaborationRules(t *testing.T) {
	wf := makeCollaborationWorkflow("plan")
	if _, err := buildWorkflowPlan(&wf, true); err != nil {
		t.Fatalf("enabled acyclic collaboration rejected: %v", err)
	}
	if _, err := buildWorkflowPlan(&wf, false); err == nil {
		t.Fatalf("collaboration accepted while disabled")
	}
	missing := makeCollaborationWorkflow("plan-missing")
	missing.Spec.Steps[0].Collaboration = nil
	if _, err := buildWorkflowPlan(&missing, true); err == nil {
		t.Fatalf("collaboration step without a spec accepted")
	}
	cyclic := makeCollaborationWorkflow("plan-cyclic")
	cyclic.Spec.EntryPoint = "debate"
	cyclic.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "debate", To: "debate"}}
	cyclic.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 2}
	if _, err := buildWorkflowPlan(&cyclic, true); err == nil || !strings.Contains(err.Error(), "cyclic") {
		t.Fatalf("cyclic collaboration err = %v, want cyclic rejection", err)
	}
}

func TestStopWorkflowCollaborations_OnlyRunningCollaborationSteps(t *testing.T) {
	wf := makeCollaborationWorkflow("stop")
	wf.Spec.Steps = append(wf.Spec.Steps, v1alpha1.WorkflowStep{
		Name: "other", Type: v1alpha1.WorkflowStepTypeCollaboration,
		Collaboration: &v1alpha1.WorkflowCollaboration{},
	}, v1alpha1.WorkflowStep{Name: "agent", AgentRef: v1alpha1.LocalObjectReference{Name: "a"}})
	wf.Status.StepStatuses = []v1alpha1.WorkflowStepStatus{
		{Name: "debate", Phase: v1alpha1.PhaseRunning, Attempts: 2},
		{Name: "other", Phase: v1alpha1.PhaseReady, Attempts: 1},
		{Name: "agent", Phase: v1alpha1.PhaseRunning, Attempts: 1},
	}
	runner := &fakeCollaborationRunner{}
	if err := stopWorkflowCollaborations(context.Background(), runner, &wf, "WorkflowRun", "WorkflowRunDeleted"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(runner.stops) != 1 || runner.stops[0] != "debate:WorkflowRunDeleted" {
		t.Fatalf("stops = %v", runner.stops)
	}
}
