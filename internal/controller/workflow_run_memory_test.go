// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

type workflowRunMemoryClaimCreateStore struct {
	store.Store
	err error
}

func (s *workflowRunMemoryClaimCreateStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if kind == "MemoryClaim" {
		return s.err
	}
	return s.Store.Create(ctx, kind, namespace, name, data)
}

func memoryClaimExists(t *testing.T, s *memStore, ns, name string) bool {
	t.Helper()
	_, err := s.Get(context.Background(), "MemoryClaim", ns, name)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("get memory claim %s: %v", name, err)
	}
	return true
}

func findJobByStep(jobs []v1alpha1.AgentJob, runName, stepName string) (v1alpha1.AgentJob, bool) {
	for _, j := range jobs {
		if j.ObjectMeta.Labels[workflowStepLabel] != stepName {
			continue
		}
		for _, ref := range j.ObjectMeta.OwnerReferences {
			if ref.Name == runName {
				return j, true
			}
		}
	}
	return v1alpha1.AgentJob{}, false
}

// TC-0235: Workflow-level MemoryClaim is materialized once per run and every
// step's AgentJob carries memoryClaimRef pointing to the shared claim.
func TestWorkflowRunReconciler_TC0235_WorkflowMemoryClaimSharedAcrossSteps(t *testing.T) {
	es := newEventMemStore()
	logger := zaptest.NewLogger(t)

	wf := makeWorkflow("default", "mem-wf")
	wf.Spec.MemoryClaim = &v1alpha1.WorkflowMemoryClaim{
		MemoryClassName: "shared-class",
		AccessMode:      "ReadWriteMany",
	}
	wf.Spec.MemoryRetentionPolicy = v1alpha1.WorkflowMemoryRetentionRetain
	storeWorkflow(t, es.memStore, wf)

	run := makeWorkflowRun("default", "mem-run", "mem-wf")
	storeWorkflowRun(t, es, run)

	rec := NewWorkflowRunReconciler(es, logger)
	driveRunToCompletion(t, rec, es, "default", "mem-run")

	if !memoryClaimExists(t, es.memStore, "default", "mem-run-memory") {
		t.Fatalf("expected workflow-level MemoryClaim mem-run-memory to be materialized")
	}
	mc := getMemoryClaim(t, es.memStore, "default", "mem-run-memory")
	if mc.Spec.MemoryClassName != "shared-class" {
		t.Errorf("memoryClassName = %q, want %q", mc.Spec.MemoryClassName, "shared-class")
	}
	if mc.Spec.Type != v1alpha1.MemoryTypeShared {
		t.Errorf("memory type = %q, want %q", mc.Spec.Type, v1alpha1.MemoryTypeShared)
	}

	jobs := listWorkflowJobs(t, es.memStore, "default")
	if len(jobs) == 0 {
		t.Fatalf("no agent jobs created")
	}
	for _, j := range jobs {
		if j.Spec.MemoryClaimRef == nil {
			t.Errorf("job %s: expected memoryClaimRef to be set", j.ObjectMeta.Name)
			continue
		}
		if j.Spec.MemoryClaimRef.Name != "mem-run-memory" {
			t.Errorf("job %s: memoryClaimRef = %q, want %q",
				j.ObjectMeta.Name, j.Spec.MemoryClaimRef.Name, "mem-run-memory")
		}
	}
}

// TC-0236: MemoryRetentionPolicy controls per-run MemoryClaim teardown after
// the run reaches a terminal phase.
func TestWorkflowRunReconciler_TC0236_MemoryRetentionPolicy(t *testing.T) {
	cases := []struct {
		name        string
		policy      v1alpha1.WorkflowMemoryRetentionPolicy
		expectExist bool
	}{
		{"default-deletes", "", false},
		{"explicit-delete", v1alpha1.WorkflowMemoryRetentionDelete, false},
		{"retain-survives", v1alpha1.WorkflowMemoryRetentionRetain, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			es := newEventMemStore()
			logger := zaptest.NewLogger(t)

			wf := makeWorkflow("default", "ret-wf")
			wf.Spec.MemoryClaim = &v1alpha1.WorkflowMemoryClaim{MemoryClassName: "c"}
			wf.Spec.MemoryRetentionPolicy = tc.policy
			storeWorkflow(t, es.memStore, wf)

			run := makeWorkflowRun("default", "ret-run", "ret-wf")
			storeWorkflowRun(t, es, run)

			rec := NewWorkflowRunReconciler(es, logger)
			driveRunToCompletion(t, rec, es, "default", "ret-run")

			final := getWorkflowRun(t, es, "default", "ret-run")
			if final.Status.Phase != v1alpha1.PhaseReady && final.Status.Phase != v1alpha1.PhaseFailed {
				t.Fatalf("expected terminal phase, got %s", final.Status.Phase)
			}

			ok := memoryClaimExists(t, es.memStore, "default", "ret-run-memory")
			if ok != tc.expectExist {
				t.Errorf("MemoryClaim exists=%v, want %v (policy=%q)", ok, tc.expectExist, tc.policy)
			}
		})
	}
}

func TestWorkflowRunReconciler_MemoryRetentionRetriesAfterDeleteFailure(t *testing.T) {
	eventStore := newEventMemStore()
	faultStore := &workflowRunMemoryClaimDeleteStore{
		eventMemStore: eventStore,
		failKind:      "MemoryClaim",
	}

	wf := makeWorkflow("default", "ret-retry-wf")
	wf.Spec.MemoryClaim = &v1alpha1.WorkflowMemoryClaim{MemoryClassName: "c"}
	storeWorkflow(t, eventStore.memStore, wf)
	storeWorkflowRun(t, faultStore, makeWorkflowRun("default", "ret-retry-run", "ret-retry-wf"))

	rec := NewWorkflowRunReconciler(faultStore, zaptest.NewLogger(t))
	driveRunToCompletion(t, rec, eventStore, "default", "ret-retry-run")
	terminal := getWorkflowRun(t, eventStore, "default", "ret-retry-run")
	if terminal.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase after best-effort cleanup failure = %s, want Ready", terminal.Status.Phase)
	}
	if !memoryClaimExists(t, eventStore.memStore, "default", "ret-retry-run-memory") {
		t.Fatal("memory claim missing after injected retention cleanup failure")
	}
	if terminal.Status.StartTime == nil || terminal.Status.CompletionTime == nil {
		t.Fatalf("terminal run timestamps = start %v, completion %v; want both", terminal.Status.StartTime, terminal.Status.CompletionTime)
	}
	startedAt := *terminal.Status.StartTime
	completedAt := *terminal.Status.CompletionTime
	registry := prometheus.NewRegistry()
	observability.NewWorkflowMetrics(registry, faultStore)
	assertRetainedWorkflowGauge(t, registry, "kognition_workflow_retained_runs", map[string]string{
		"namespace": "default", "workflow": "ret-retry-wf", "phase": string(v1alpha1.PhaseReady),
	}, 1)
	faultStore.failKind = ""
	if _, err := rec.Reconcile(context.Background(), Request{Namespace: "default", Name: "ret-retry-run"}); err != nil {
		t.Fatalf("retry retention cleanup: %v", err)
	}
	if memoryClaimExists(t, eventStore.memStore, "default", "ret-retry-run-memory") {
		t.Fatal("memory claim survived successful retention cleanup retry")
	}
	terminal = getWorkflowRun(t, eventStore, "default", "ret-retry-run")
	if terminal.Status.StartTime == nil || !terminal.Status.StartTime.Equal(startedAt) {
		t.Fatalf("start time changed on cleanup retry: got %v, want %v", terminal.Status.StartTime, startedAt)
	}
	if terminal.Status.CompletionTime == nil || !terminal.Status.CompletionTime.Equal(completedAt) {
		t.Fatalf("completion time changed on cleanup retry: got %v, want %v", terminal.Status.CompletionTime, completedAt)
	}
	assertRetainedWorkflowGauge(t, registry, "kognition_workflow_retained_runs", map[string]string{
		"namespace": "default", "workflow": "ret-retry-wf", "phase": string(v1alpha1.PhaseReady),
	}, 1)
}

func TestWorkflowRunReconciler_PropagatesMemoryClaimCreationFailures(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(*v1alpha1.WorkflowSpec)
		wantContext string
	}{
		{
			name: "workflow claim",
			configure: func(spec *v1alpha1.WorkflowSpec) {
				spec.MemoryClaim = &v1alpha1.WorkflowMemoryClaim{MemoryClassName: "shared"}
			},
			wantContext: "ensure workflow memory claim",
		},
		{
			name: "step claim",
			configure: func(spec *v1alpha1.WorkflowSpec) {
				spec.Steps[0].MemoryClaim = &v1alpha1.WorkflowMemoryClaim{MemoryClassName: "private"}
			},
			wantContext: `ensure step memory claims: step "plan"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := newMemStore()
			workflow := makeWorkflow("default", "memory-failure-workflow")
			tc.configure(&workflow.Spec)

			run := makeWorkflowRun("default", "memory-failure-run", workflow.ObjectMeta.Name)
			run.ObjectMeta.Finalizers = []string{WorkflowRunFinalizer}
			startedAt := time.Now().UTC()
			run.Status.StartTime = &startedAt
			run.Status.SnapshotSpec = &workflow.Spec
			storeWorkflowRun(t, base, run)

			wantErr := errors.New("memory claim storage unavailable")
			reconciler := NewWorkflowRunReconciler(&workflowRunMemoryClaimCreateStore{Store: base, err: wantErr}, zaptest.NewLogger(t))
			_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: run.ObjectMeta.Name})
			if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), tc.wantContext) {
				t.Fatalf("error = %v, want wrapped failure containing %q", err, tc.wantContext)
			}
		})
	}
}

// TC-0237: Per-step MemoryClaim creates a step-scoped claim that overrides the
// workflow-level claim for that step's AgentJob, while other steps continue
// using the workflow-level claim.
func TestWorkflowRunReconciler_TC0237_PerStepMemoryClaimOverride(t *testing.T) {
	es := newEventMemStore()
	logger := zaptest.NewLogger(t)

	wf := makeWorkflow("default", "ovr-wf")
	wf.Spec.MemoryClaim = &v1alpha1.WorkflowMemoryClaim{MemoryClassName: "wf-class"}
	wf.Spec.MemoryRetentionPolicy = v1alpha1.WorkflowMemoryRetentionRetain
	// makeWorkflow gives us steps "plan" and "execute"; override "execute".
	for i := range wf.Spec.Steps {
		if wf.Spec.Steps[i].Name == "execute" {
			wf.Spec.Steps[i].MemoryClaim = &v1alpha1.WorkflowMemoryClaim{
				MemoryClassName: "execute-class",
			}
		}
	}
	storeWorkflow(t, es.memStore, wf)

	run := makeWorkflowRun("default", "ovr-run", "ovr-wf")
	storeWorkflowRun(t, es, run)

	rec := NewWorkflowRunReconciler(es, logger)
	driveRunToCompletion(t, rec, es, "default", "ovr-run")

	// Workflow-level claim should exist.
	if !memoryClaimExists(t, es.memStore, "default", "ovr-run-memory") {
		t.Fatalf("expected workflow-level MemoryClaim ovr-run-memory")
	}
	wfClaim := getMemoryClaim(t, es.memStore, "default", "ovr-run-memory")
	if wfClaim.Spec.MemoryClassName != "wf-class" {
		t.Errorf("workflow claim class = %q, want %q", wfClaim.Spec.MemoryClassName, "wf-class")
	}
	// Step-level claim should exist for "execute".
	if !memoryClaimExists(t, es.memStore, "default", "ovr-run-execute-memory") {
		t.Fatalf("expected step-level MemoryClaim ovr-run-execute-memory")
	}
	stepClaim := getMemoryClaim(t, es.memStore, "default", "ovr-run-execute-memory")
	if stepClaim.Spec.MemoryClassName != "execute-class" {
		t.Errorf("step claim class = %q, want %q", stepClaim.Spec.MemoryClassName, "execute-class")
	}
	// No claim should exist for the non-overridden "plan" step.
	if memoryClaimExists(t, es.memStore, "default", "ovr-run-plan-memory") {
		t.Errorf("did not expect step-level MemoryClaim ovr-run-plan-memory")
	}

	jobs := listWorkflowJobs(t, es.memStore, "default")
	planJob, ok := findJobByStep(jobs, "ovr-run", "plan")
	if !ok {
		t.Fatalf("plan job not found")
	}
	if planJob.Spec.MemoryClaimRef == nil || planJob.Spec.MemoryClaimRef.Name != "ovr-run-memory" {
		t.Errorf("plan job memoryClaimRef = %v, want ovr-run-memory", planJob.Spec.MemoryClaimRef)
	}
	execJob, ok := findJobByStep(jobs, "ovr-run", "execute")
	if !ok {
		t.Fatalf("execute job not found")
	}
	if execJob.Spec.MemoryClaimRef == nil || execJob.Spec.MemoryClaimRef.Name != "ovr-run-execute-memory" {
		t.Errorf("execute job memoryClaimRef = %v, want ovr-run-execute-memory", execJob.Spec.MemoryClaimRef)
	}
}
