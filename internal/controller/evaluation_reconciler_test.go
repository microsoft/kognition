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
	"github.com/kognition-ai/kognition/internal/lifecycle"
	storepkg "github.com/kognition-ai/kognition/internal/store"
)

type evaluationSentinelStore struct {
	*memStore
	getKind    string
	createKind string
}

func (s *evaluationSentinelStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == s.getKind {
		return nil, fmt.Errorf("wrapped missing %s: %w", kind, storepkg.ErrNotFound)
	}
	return s.memStore.Get(ctx, kind, namespace, name)
}

func (s *evaluationSentinelStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if kind == s.createKind {
		return fmt.Errorf("wrapped existing %s: %w", kind, storepkg.ErrAlreadyExists)
	}
	return s.memStore.Create(ctx, kind, namespace, name, data)
}

func TestEvaluationReconcilersHandleWrappedNotFound(t *testing.T) {
	t.Run("evaluation", func(t *testing.T) {
		faults := &evaluationSentinelStore{memStore: newMemStore(), getKind: evaluationKind}
		result, err := NewEvaluationReconciler(faults, zaptest.NewLogger(t)).Reconcile(context.Background(), Request{Namespace: "default", Name: "missing"})
		if err != nil || result != (Result{}) {
			t.Fatalf("result = %+v, error = %v, want completed reconciliation", result, err)
		}
	})

	t.Run("evaluation run", func(t *testing.T) {
		faults := &evaluationSentinelStore{memStore: newMemStore(), getKind: evaluationRunKind}
		result, err := NewEvaluationRunReconciler(faults, zaptest.NewLogger(t)).Reconcile(context.Background(), Request{Namespace: "default", Name: "missing"})
		if err != nil || result != (Result{}) {
			t.Fatalf("result = %+v, error = %v, want completed reconciliation", result, err)
		}
	})

	t.Run("missing run is created", func(t *testing.T) {
		memory := newMemStore()
		evaluation := v1alpha1.Evaluation{ObjectMeta: v1alpha1.ObjectMeta{Name: "quality", Namespace: "default", Generation: 1}}
		storeEvaluation(t, memory, evaluation)
		faults := &evaluationSentinelStore{memStore: memory, getKind: evaluationRunKind}
		if _, err := NewEvaluationReconciler(faults, zaptest.NewLogger(t)).Reconcile(context.Background(), Request{Namespace: "default", Name: evaluation.ObjectMeta.Name}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	})

	t.Run("dataset", func(t *testing.T) {
		memory := newMemStore()
		run := v1alpha1.EvaluationRun{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "quality-g1", Namespace: "default"},
			Spec: v1alpha1.EvaluationRunSpec{
				TargetRef:  v1alpha1.EvaluationTargetRef{Kind: "Agent", Name: "assistant"},
				DatasetRef: v1alpha1.EvaluationDatasetRef{Name: "missing"},
			},
		}
		storeEvaluationRun(t, memory, run)
		faults := &evaluationSentinelStore{memStore: memory, getKind: evaluationDatasetKind}
		if _, err := NewEvaluationRunReconciler(faults, zaptest.NewLogger(t)).Reconcile(context.Background(), Request{Namespace: "default", Name: run.ObjectMeta.Name}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if got := getEvaluationRun(t, memory, "default", run.ObjectMeta.Name); got.Status.Phase != v1alpha1.PhaseFailed {
			t.Fatalf("phase = %q, want %q", got.Status.Phase, v1alpha1.PhaseFailed)
		}
	})

	t.Run("parent evaluation", func(t *testing.T) {
		faults := &evaluationSentinelStore{memStore: newMemStore(), getKind: evaluationKind}
		run := &v1alpha1.EvaluationRun{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "default"}, Spec: v1alpha1.EvaluationRunSpec{EvaluationRef: v1alpha1.LocalObjectReference{Name: "missing"}}}
		if err := NewEvaluationRunReconciler(faults, zaptest.NewLogger(t)).reflectParentEvaluationStatus(context.Background(), run); err != nil {
			t.Fatalf("reflectParentEvaluationStatus: %v", err)
		}
	})
}

func TestEvaluationReconcilersHandleWrappedAlreadyExists(t *testing.T) {
	evaluation := &v1alpha1.Evaluation{ObjectMeta: v1alpha1.ObjectMeta{Name: "quality", Namespace: "default"}}
	run := &v1alpha1.EvaluationRun{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "quality-g1", Namespace: "default"},
		Spec: v1alpha1.EvaluationRunSpec{
			EvaluationRef: v1alpha1.LocalObjectReference{Name: evaluation.ObjectMeta.Name},
			TargetRef:     v1alpha1.EvaluationTargetRef{Kind: "Agent", Name: "assistant"},
		},
	}

	t.Run("evaluation run", func(t *testing.T) {
		faults := &evaluationSentinelStore{memStore: newMemStore(), createKind: evaluationRunKind}
		if err := NewEvaluationReconciler(faults, zaptest.NewLogger(t)).createEvaluationRun(context.Background(), evaluation, run.ObjectMeta.Name); err != nil {
			t.Fatalf("createEvaluationRun: %v", err)
		}
	})

	t.Run("target job", func(t *testing.T) {
		faults := &evaluationSentinelStore{memStore: newMemStore(), createKind: agentJobKind}
		if err := NewEvaluationRunReconciler(faults, zaptest.NewLogger(t)).createAgentJob(context.Background(), run, "assistant", "target", v1alpha1.EvaluationDatasetCase{ID: "case"}); err != nil {
			t.Fatalf("createAgentJob: %v", err)
		}
	})

	t.Run("judge job", func(t *testing.T) {
		faults := &evaluationSentinelStore{memStore: newMemStore(), createKind: agentJobKind}
		judge := llmJudgeGrader{grader: v1alpha1.EvaluationGrader{Name: "judge", Parameters: map[string]string{"agentRef": "judge-agent"}}}
		if err := NewEvaluationRunReconciler(faults, zaptest.NewLogger(t)).createLLMJudgeJob(context.Background(), run, &v1alpha1.AgentJob{}, "judge", v1alpha1.EvaluationDatasetCase{ID: "case"}, judge); err != nil {
			t.Fatalf("createLLMJudgeJob: %v", err)
		}
	})
}

// TC-0290: an Agent Evaluation creates a run and reaches a passing terminal state.
func TestEvaluationReconcilerCreatesRunAndEvaluationRunCompletesAgentTarget(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	minScore := 1.0
	evaluation := v1alpha1.Evaluation{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Evaluation"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "agent-quality",
			Namespace:  "default",
			UID:        "eval-uid-001",
			Generation: 7,
		},
		Spec: v1alpha1.EvaluationSpec{
			TargetRef:  v1alpha1.EvaluationTargetRef{Kind: "Agent", Name: "assistant"},
			DatasetRef: v1alpha1.EvaluationDatasetRef{Name: "smoke-dataset", Version: "v1"},
			Graders: []v1alpha1.EvaluationGrader{{
				Name: "answer-quality",
				Type: "builtin",
				Parameters: map[string]string{
					"mode": "strict",
				},
			}},
			MetricThresholds: []v1alpha1.EvaluationMetricThreshold{{Metric: "task_success", Min: &minScore, Required: true}},
		},
	}
	storeEvaluation(t, store, evaluation)
	storeEvaluationDataset(t, store, v1alpha1.EvaluationDataset{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationDataset"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "smoke-dataset", Namespace: "default", UID: "dataset-uid-001"},
		Spec: v1alpha1.EvaluationDatasetSpec{Version: "v1", Cases: []v1alpha1.EvaluationDatasetCase{
			{ID: "hello", Input: "Say hello to the evaluator.", Expected: "hello"},
			{ID: "status", Input: "Return the service status.", Expected: "ready"},
		}},
	})

	evaluationReconciler := NewEvaluationReconciler(store, zaptest.NewLogger(t))
	runReconciler := NewEvaluationRunReconciler(store, zaptest.NewLogger(t))
	if _, err := evaluationReconciler.Reconcile(ctx, Request{Kind: "Evaluation", Namespace: "default", Name: "agent-quality"}); err != nil {
		t.Fatalf("reconcile evaluation: %v", err)
	}

	runName := evaluationRunName("agent-quality", 7)
	run := getEvaluationRun(t, store, "default", runName)
	if run.Spec.EvaluationRef.Name != "agent-quality" || run.Spec.TargetRef.Name != "assistant" || run.Spec.DatasetRef.Name != "smoke-dataset" {
		t.Fatalf("evaluationRun snapshot = %#v", run.Spec)
	}
	if run.Spec.Graders[0].Parameters["mode"] != "strict" || run.Spec.MetricThresholds[0].Min == nil || *run.Spec.MetricThresholds[0].Min != minScore {
		t.Fatalf("evaluationRun graders/thresholds were not snapshotted: %#v", run.Spec)
	}

	parent := getEvaluation(t, store, "default", "agent-quality")
	if parent.Status.Phase != v1alpha1.PhaseRunning || parent.Status.LastRunRef == nil || parent.Status.LastRunRef.Name != runName {
		t.Fatalf("evaluation status after run create = %#v", parent.Status)
	}

	if _, err := runReconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: runName}); err != nil {
		t.Fatalf("reconcile evaluationRun: %v", err)
	}
	jobName := evaluationCaseJobName(runName, "hello")
	job := getJob(t, store, "default", jobName)
	if job.Spec.AgentRef == nil || job.Spec.AgentRef.Name != "assistant" {
		t.Fatalf("target AgentJob agent ref = %#v", job.Spec.AgentRef)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != evaluationTargetJobBackoffLimit {
		t.Fatalf("target AgentJob backoffLimit = %#v, want %d", job.Spec.BackoffLimit, evaluationTargetJobBackoffLimit)
	}
	if job.Spec.Input != "Say hello to the evaluator." || strings.Contains(job.Spec.Input, "Evaluate dataset") {
		t.Fatalf("target AgentJob input = %q", job.Spec.Input)
	}
	if !ownedBy(job.ObjectMeta.OwnerReferences, "EvaluationRun", runName, run.ObjectMeta.UID) {
		t.Fatalf("target AgentJob ownerRefs = %#v", job.ObjectMeta.OwnerReferences)
	}
	secondJobName := evaluationCaseJobName(runName, "status")
	secondJob := getJob(t, store, "default", secondJobName)
	if secondJob.Spec.Input != "Return the service status." || strings.Contains(secondJob.Spec.Input, "Evaluate dataset") {
		t.Fatalf("second target AgentJob input = %q", secondJob.Spec.Input)
	}

	job.Status.Phase = v1alpha1.PhaseReady
	job.Status.Succeeded = 1
	job.Status.Output = "hello evaluator"
	updateEvaluationTargetJob(t, store, job)
	secondJob.Status.Phase = v1alpha1.PhaseReady
	secondJob.Status.Succeeded = 1
	secondJob.Status.Output = "service ready"
	updateEvaluationTargetJob(t, store, secondJob)
	if _, err := runReconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: runName}); err != nil {
		t.Fatalf("reconcile completed evaluationRun: %v", err)
	}

	completed := getEvaluationRun(t, store, "default", runName)
	if completed.Status.Phase != v1alpha1.PhaseReady || !completed.Status.Passed || completed.Status.Degraded {
		t.Fatalf("completed evaluationRun status = %#v", completed.Status)
	}
	if completed.Status.TargetOutput != "hello evaluator" || completed.Status.TargetJobRef == nil || completed.Status.TargetJobRef.Name != jobName {
		t.Fatalf("completed evaluationRun target status = %#v", completed.Status)
	}
	if completed.Status.CaseCount != 2 || completed.Status.CompletedCases != 2 || completed.Status.FailedCases != 0 || len(completed.Status.CaseResults) != 2 {
		t.Fatalf("completed evaluationRun case summary = %#v", completed.Status)
	}
	if completed.Status.CaseResults[0].ID != "hello" || completed.Status.CaseResults[0].TargetOutput != "hello evaluator" || !completed.Status.CaseResults[0].Passed {
		t.Fatalf("first case result = %#v", completed.Status.CaseResults[0])
	}
	if completed.Status.CaseResults[1].ID != "status" || completed.Status.CaseResults[1].TargetOutput != "service ready" || !completed.Status.CaseResults[1].Passed {
		t.Fatalf("second case result = %#v", completed.Status.CaseResults[1])
	}
	if completed.Status.Score == nil || *completed.Status.Score != 1 || len(completed.Status.Metrics) != 1 || !completed.Status.Metrics[0].Passed {
		t.Fatalf("completed evaluationRun metrics = %#v", completed.Status)
	}

	parent = getEvaluation(t, store, "default", "agent-quality")
	if parent.Status.Phase != v1alpha1.PhaseReady || parent.Status.LastRunTime == nil || len(parent.Status.RunHistory) != 1 {
		t.Fatalf("completed evaluation status = %#v", parent.Status)
	}
	if parent.Status.RunHistory[0].Name != runName || !parent.Status.RunHistory[0].Passed || parent.Status.RunHistory[0].Score == nil || *parent.Status.RunHistory[0].Score != 1 {
		t.Fatalf("evaluation run history = %#v", parent.Status.RunHistory)
	}
	if parent.Status.RunHistory[0].TargetRef.Kind != "Agent" || parent.Status.RunHistory[0].TargetRef.Name != "assistant" || parent.Status.RunHistory[0].DatasetRef.Name != "smoke-dataset" {
		t.Fatalf("evaluation run history traceability = %#v", parent.Status.RunHistory[0])
	}
}

func TestEvaluationRunReconcilerCreatesJudgeJobAndCompletesFromJudgeScore(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	minJudgeScore := 0.8
	run := v1alpha1.EvaluationRun{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationRun"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "agent-quality-g1",
			Namespace: "default",
			UID:       "run-uid-judge",
		},
		Spec: v1alpha1.EvaluationRunSpec{
			EvaluationRef: v1alpha1.LocalObjectReference{Name: "agent-quality"},
			TargetRef:     v1alpha1.EvaluationTargetRef{Kind: "Agent", Name: "assistant"},
			DatasetRef:    v1alpha1.EvaluationDatasetRef{Name: "smoke-dataset"},
			Graders: []v1alpha1.EvaluationGrader{{
				Name: "rubric-judge",
				Type: v1alpha1.EvaluationGraderTypeLLMJudge,
				Parameters: map[string]string{
					"agentRef":      "judge-agent",
					"metric":        "rubric_score",
					"passingScore":  "0.8",
					"rubric":        "Prefer concise answers grounded in the reference.",
					"customIgnored": "left alone",
				},
			}},
			MetricThresholds: []v1alpha1.EvaluationMetricThreshold{{Metric: "rubric_score", Min: &minJudgeScore, Required: true}},
		},
	}
	storeEvaluationRun(t, store, run)
	storeEvaluationDataset(t, store, v1alpha1.EvaluationDataset{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationDataset"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "smoke-dataset", Namespace: "default"},
		Spec: v1alpha1.EvaluationDatasetSpec{Cases: []v1alpha1.EvaluationDatasetCase{{
			ID:        "judge-rubric",
			Input:     "Say hello and explain what you can do.",
			Expected:  "friendly greeting with a capability summary",
			Reference: "The assistant should greet the user and summarize its capabilities.",
			Rubric:    "Score high when the response is friendly, concise, and grounded.",
		}}},
	})

	reconciler := NewEvaluationRunReconciler(store, zaptest.NewLogger(t))
	if _, err := reconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: run.ObjectMeta.Name}); err != nil {
		t.Fatalf("reconcile evaluationRun create target job: %v", err)
	}
	targetJobName := evaluationCaseJobName(run.ObjectMeta.Name, "judge-rubric")
	targetJob := getJob(t, store, "default", targetJobName)
	targetJob.Status.Phase = v1alpha1.PhaseReady
	targetJob.Status.Output = "Hello! I can answer questions and help evaluate workflows."
	updateEvaluationTargetJob(t, store, targetJob)

	result, err := reconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: run.ObjectMeta.Name})
	if err != nil {
		t.Fatalf("reconcile evaluationRun create judge job: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatalf("expected requeue while judge AgentJob is running, got %#v", result)
	}
	judgeJobName := evaluationJudgeJobName(run.ObjectMeta.Name, "judge-rubric", 0, run.Spec.Graders[0])
	judgeJob := getJob(t, store, "default", judgeJobName)
	if judgeJob.Spec.AgentRef == nil || judgeJob.Spec.AgentRef.Name != "judge-agent" {
		t.Fatalf("judge AgentJob agent ref = %#v", judgeJob.Spec.AgentRef)
	}
	if !strings.Contains(judgeJob.Spec.Input, "Case input:") || !strings.Contains(judgeJob.Spec.Input, targetJob.Status.Output) || !strings.Contains(judgeJob.Spec.Input, "Prefer concise answers") || !strings.Contains(judgeJob.Spec.Input, "untrusted data") {
		t.Fatalf("judge AgentJob prompt = %q", judgeJob.Spec.Input)
	}
	if !ownedBy(judgeJob.ObjectMeta.OwnerReferences, evaluationRunKind, run.ObjectMeta.Name, run.ObjectMeta.UID) {
		t.Fatalf("judge AgentJob ownerRefs = %#v", judgeJob.ObjectMeta.OwnerReferences)
	}

	judgeJob.Status.Phase = v1alpha1.PhaseReady
	judgeJob.Status.Output = "```json\n{\"score\":0.91,\"passed\":true,\"reason\":\"friendly and grounded\"}\n```"
	updateEvaluationTargetJob(t, store, judgeJob)
	if _, err := reconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: run.ObjectMeta.Name}); err != nil {
		t.Fatalf("reconcile evaluationRun complete judge result: %v", err)
	}

	completed := getEvaluationRun(t, store, "default", run.ObjectMeta.Name)
	if completed.Status.Phase != v1alpha1.PhaseReady || !completed.Status.Passed || completed.Status.FailedCases != 0 || len(completed.Status.CaseResults) != 1 {
		t.Fatalf("completed judge evaluationRun status = %#v", completed.Status)
	}
	metrics := metricsByName(completed.Status.CaseResults[0].Metrics)
	if len(metrics) != 1 || metrics["rubric_score"].Value != 0.91 || !metrics["rubric_score"].Passed {
		t.Fatalf("judge case metrics = %#v, want only passing rubric_score", completed.Status.CaseResults[0].Metrics)
	}
	if completed.Status.Score == nil || *completed.Status.Score != 1 || completed.Status.Metrics[0].Metric != "rubric_score" {
		t.Fatalf("completed judge metrics = %#v", completed.Status)
	}
}

func TestEvaluationRunReconcilerFailsSlashQualifiedLLMJudgeAgentRef(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	run := v1alpha1.EvaluationRun{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationRun"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "agent-quality-g1",
			Namespace: "default",
			UID:       "run-uid-judge-invalid-ref",
		},
		Spec: v1alpha1.EvaluationRunSpec{
			EvaluationRef: v1alpha1.LocalObjectReference{Name: "agent-quality"},
			TargetRef:     v1alpha1.EvaluationTargetRef{Kind: "Agent", Name: "assistant"},
			DatasetRef:    v1alpha1.EvaluationDatasetRef{Name: "smoke-dataset"},
			Graders: []v1alpha1.EvaluationGrader{{
				Name: "rubric-judge",
				Type: v1alpha1.EvaluationGraderTypeLLMJudge,
				Parameters: map[string]string{
					"agentRef": "other-ns/judge-agent",
				},
			}},
		},
	}
	storeEvaluationRun(t, store, run)
	storeEvaluationDataset(t, store, v1alpha1.EvaluationDataset{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationDataset"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "smoke-dataset", Namespace: "default"},
		Spec: v1alpha1.EvaluationDatasetSpec{Cases: []v1alpha1.EvaluationDatasetCase{{
			ID:    "judge-rubric",
			Input: "Say hello and explain what you can do.",
		}}},
	})

	reconciler := NewEvaluationRunReconciler(store, zaptest.NewLogger(t))
	if _, err := reconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: run.ObjectMeta.Name}); err != nil {
		t.Fatalf("reconcile evaluationRun create target job: %v", err)
	}
	targetJobName := evaluationCaseJobName(run.ObjectMeta.Name, "judge-rubric")
	targetJob := getJob(t, store, "default", targetJobName)
	targetJob.Status.Phase = v1alpha1.PhaseReady
	targetJob.Status.Output = "Hello!"
	updateEvaluationTargetJob(t, store, targetJob)

	if _, err := reconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: run.ObjectMeta.Name}); err != nil {
		t.Fatalf("reconcile evaluationRun invalid judge ref: %v", err)
	}
	failed := getEvaluationRun(t, store, "default", run.ObjectMeta.Name)
	if failed.Status.Phase != v1alpha1.PhaseFailed || !failed.Status.Degraded || !strings.Contains(failed.Status.Message, "must reference a judge Agent") {
		t.Fatalf("invalid judge ref status = %#v", failed.Status)
	}
	if len(failed.Status.Conditions) == 0 || failed.Status.Conditions[0].Reason != "LLMJudgeInvalid" {
		t.Fatalf("invalid judge ref conditions = %#v", failed.Status.Conditions)
	}
	jobs, err := store.List(ctx, agentJobKind, storepkg.ListOptions{Namespace: "default", LabelSelector: map[string]string{
		"kognition.dev/evaluationRun": run.ObjectMeta.Name,
		"kognition.dev/evaluationJob": "judge",
	}})
	if err != nil {
		t.Fatalf("list judge jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("created %d judge AgentJobs for invalid judge ref", len(jobs))
	}
}

func TestEvaluationRunReconcilerBackfillsTargetJobBackoffLimit(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	run := v1alpha1.EvaluationRun{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationRun"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "agent-quality-g1",
			Namespace: "default",
			UID:       "run-uid-001",
		},
		Spec: v1alpha1.EvaluationRunSpec{
			EvaluationRef: v1alpha1.LocalObjectReference{Name: "agent-quality"},
			TargetRef:     v1alpha1.EvaluationTargetRef{Kind: "Agent", Name: "assistant"},
			DatasetRef:    v1alpha1.EvaluationDatasetRef{Name: "smoke-dataset", Version: "v1"},
		},
	}
	storeEvaluationRun(t, store, run)
	storeEvaluationDataset(t, store, v1alpha1.EvaluationDataset{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationDataset"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "smoke-dataset", Namespace: "default", UID: "dataset-uid-001"},
		Spec: v1alpha1.EvaluationDatasetSpec{Version: "v1", Cases: []v1alpha1.EvaluationDatasetCase{
			{ID: "greeting", Input: "Say hello."},
		}},
	})
	job := v1alpha1.AgentJob{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentJob"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      evaluationCaseJobName(run.ObjectMeta.Name, "greeting"),
			Namespace: "default",
			UID:       "job-uid-001",
			Labels: map[string]string{
				"kognition.dev/evaluation":     "agent-quality",
				"kognition.dev/evaluationRun":  run.ObjectMeta.Name,
				"kognition.dev/evaluationCase": "greeting",
			},
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       evaluationRunKind,
				Name:       run.ObjectMeta.Name,
				UID:        run.ObjectMeta.UID,
			}},
		},
		Spec: v1alpha1.AgentJobSpec{
			AgentRef: &v1alpha1.LocalObjectReference{Name: "assistant"},
			Input:    "Say hello.",
		},
		Status: v1alpha1.AgentJobStatus{Phase: v1alpha1.PhaseRunning},
	}
	storeJob(t, store, job)

	reconciler := NewEvaluationRunReconciler(store, zaptest.NewLogger(t))
	result, err := reconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: run.ObjectMeta.Name})
	if err != nil {
		t.Fatalf("reconcile evaluationRun: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected requeue after target AgentJob retry repair")
	}
	updated := getJob(t, store, "default", job.ObjectMeta.Name)
	if updated.Spec.BackoffLimit == nil || *updated.Spec.BackoffLimit != evaluationTargetJobBackoffLimit {
		t.Fatalf("target AgentJob backoffLimit = %#v, want %d", updated.Spec.BackoffLimit, evaluationTargetJobBackoffLimit)
	}
}

func TestEvaluationRunReconcilerFailsWhenTargetAgentJobFails(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	run := v1alpha1.EvaluationRun{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationRun"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "agent-quality-g1",
			Namespace: "default",
			UID:       "run-uid-failed-job",
		},
		Spec: v1alpha1.EvaluationRunSpec{
			EvaluationRef: v1alpha1.LocalObjectReference{Name: "agent-quality"},
			TargetRef:     v1alpha1.EvaluationTargetRef{Kind: "Agent", Name: "assistant"},
			DatasetRef:    v1alpha1.EvaluationDatasetRef{Name: "smoke-dataset"},
		},
	}
	storeEvaluationRun(t, store, run)
	storeEvaluationDataset(t, store, v1alpha1.EvaluationDataset{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationDataset"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "smoke-dataset", Namespace: "default"},
		Spec:       v1alpha1.EvaluationDatasetSpec{Cases: []v1alpha1.EvaluationDatasetCase{{ID: "quota", Input: "Trigger quota path"}}},
	})

	reconciler := NewEvaluationRunReconciler(store, zaptest.NewLogger(t))
	if _, err := reconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: "agent-quality-g1"}); err != nil {
		t.Fatalf("reconcile evaluationRun create target job: %v", err)
	}

	jobName := evaluationCaseJobName("agent-quality-g1", "quota")
	job := getJob(t, store, "default", jobName)
	job.Status.Phase = v1alpha1.PhaseFailed
	job.Status.Failed = 1
	job.Status.Output = "provider quota exhausted"
	updateEvaluationTargetJob(t, store, job)

	if _, err := reconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: "agent-quality-g1"}); err != nil {
		t.Fatalf("reconcile failed target AgentJob: %v", err)
	}
	failed := getEvaluationRun(t, store, "default", "agent-quality-g1")
	if failed.Status.Phase != v1alpha1.PhaseFailed || !failed.Status.Degraded || failed.Status.Passed {
		t.Fatalf("failed target job status = %#v", failed.Status)
	}
	if failed.Status.Message != "provider quota exhausted" {
		t.Fatalf("failed target job message = %q", failed.Status.Message)
	}
	if failed.Status.TargetJobRef == nil || failed.Status.TargetJobRef.Name != jobName || failed.Status.CompletedAt == nil {
		t.Fatalf("failed target job references/timestamps = %#v", failed.Status)
	}
}

func TestEvaluationReconcilerFailsWhenTargetAgentInstanceHasTerminalPendingCondition(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	evaluation := v1alpha1.Evaluation{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Evaluation"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "agent-quality-denied",
			Namespace:  "default",
			UID:        "eval-uid-denied",
			Generation: 1,
		},
		Spec: v1alpha1.EvaluationSpec{
			TargetRef:  v1alpha1.EvaluationTargetRef{Kind: "Agent", Name: "assistant"},
			DatasetRef: v1alpha1.EvaluationDatasetRef{Name: "smoke-dataset"},
		},
	}
	storeEvaluation(t, store, evaluation)
	storeEvaluationDataset(t, store, v1alpha1.EvaluationDataset{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationDataset"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "smoke-dataset", Namespace: "default"},
		Spec:       v1alpha1.EvaluationDatasetSpec{Cases: []v1alpha1.EvaluationDatasetCase{{ID: "denied", Input: "Trigger policy denial"}}},
	})

	evaluationReconciler := NewEvaluationReconciler(store, zaptest.NewLogger(t))
	runReconciler := NewEvaluationRunReconciler(store, zaptest.NewLogger(t))
	jobReconciler := NewAgentJobReconciler(store, zaptest.NewLogger(t))
	if _, err := evaluationReconciler.Reconcile(ctx, Request{Kind: "Evaluation", Namespace: "default", Name: "agent-quality-denied"}); err != nil {
		t.Fatalf("reconcile evaluation: %v", err)
	}

	runName := evaluationRunName("agent-quality-denied", 1)
	if _, err := runReconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: runName}); err != nil {
		t.Fatalf("reconcile evaluationRun create target job: %v", err)
	}
	jobName := evaluationCaseJobName(runName, "denied")
	if _, err := jobReconciler.Reconcile(ctx, Request{Kind: "AgentJob", Namespace: "default", Name: jobName}); err != nil {
		t.Fatalf("reconcile AgentJob finalizer: %v", err)
	}
	if _, err := jobReconciler.Reconcile(ctx, Request{Kind: "AgentJob", Namespace: "default", Name: jobName}); err != nil {
		t.Fatalf("reconcile AgentJob create instance: %v", err)
	}

	instances := listJobInstances(t, store, "default")
	if len(instances) != 1 {
		t.Fatalf("target AgentJob instances = %d, want 1", len(instances))
	}
	denial := "authorize model: no ModelGrant permits this agent"
	inst := instances[0]
	inst.Status.Phase = v1alpha1.PhasePending
	inst.Status.Conditions = []v1alpha1.Condition{{
		Type:    "Ready",
		Status:  "False",
		Reason:  "ModelAccessDenied",
		Message: denial,
	}}
	data, _ := json.Marshal(inst)
	if err := store.Update(ctx, "AgentInstance", inst.ObjectMeta.Namespace, inst.ObjectMeta.Name, data, inst.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update instance to terminal pending condition: %v", err)
	}
	if _, err := jobReconciler.Reconcile(ctx, Request{Kind: "AgentJob", Namespace: "default", Name: jobName}); err != nil {
		t.Fatalf("reconcile AgentJob terminal pending condition: %v", err)
	}

	job := getJob(t, store, "default", jobName)
	if job.Status.Phase != v1alpha1.PhaseFailed || job.Status.Output != denial {
		t.Fatalf("target AgentJob status = %#v", job.Status)
	}
	if _, err := runReconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: runName}); err != nil {
		t.Fatalf("reconcile failed target job EvaluationRun: %v", err)
	}

	failedRun := getEvaluationRun(t, store, "default", runName)
	if failedRun.Status.Phase != v1alpha1.PhaseFailed || !failedRun.Status.Degraded || failedRun.Status.Passed || failedRun.Status.CompletedAt == nil {
		t.Fatalf("failed evaluationRun status = %#v", failedRun.Status)
	}
	if failedRun.Status.Message != denial || failedRun.Status.TargetJobRef == nil || failedRun.Status.TargetJobRef.Name != jobName {
		t.Fatalf("failed evaluationRun target status = %#v", failedRun.Status)
	}

	parent := getEvaluation(t, store, "default", "agent-quality-denied")
	if parent.Status.Phase != v1alpha1.PhaseFailed || len(parent.Status.RunHistory) != 1 || parent.Status.RunHistory[0].Passed {
		t.Fatalf("failed evaluation status = %#v", parent.Status)
	}
	if parent.Status.RunHistory[0].Name != runName || parent.Status.RunHistory[0].Message != denial {
		t.Fatalf("failed evaluation run history = %#v", parent.Status.RunHistory)
	}
}

func TestEvaluationRunReconcilerFailsUnsupportedTargets(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	run := v1alpha1.EvaluationRun{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationRun"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "workflow-eval-g1",
			Namespace: "default",
			UID:       "run-uid-001",
		},
		Spec: v1alpha1.EvaluationRunSpec{
			EvaluationRef: v1alpha1.LocalObjectReference{Name: "workflow-eval"},
			TargetRef:     v1alpha1.EvaluationTargetRef{Kind: "Workflow", Name: "flow"},
			DatasetRef:    v1alpha1.EvaluationDatasetRef{Name: "dataset"},
		},
	}
	storeEvaluationRun(t, store, run)

	reconciler := NewEvaluationRunReconciler(store, zaptest.NewLogger(t))
	if _, err := reconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: "workflow-eval-g1"}); err != nil {
		t.Fatalf("reconcile unsupported evaluationRun: %v", err)
	}
	failed := getEvaluationRun(t, store, "default", "workflow-eval-g1")
	if failed.Status.Phase != v1alpha1.PhaseFailed || !failed.Status.Degraded || failed.Status.Passed {
		t.Fatalf("unsupported target status = %#v", failed.Status)
	}
	if !strings.Contains(failed.Status.Message, "Workflow") {
		t.Fatalf("unsupported target message = %q", failed.Status.Message)
	}
}

func TestEvaluationRunReconcilerExecutesPinnedSolutionRevision(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	revision := int64(2)
	solution := v1alpha1.Solution{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Solution"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "support-solution", Namespace: "default", UID: "solution-uid"},
		Status:     v1alpha1.SolutionStatus{ActiveRevision: 1, CandidateRevision: revision},
	}
	data, _ := json.Marshal(solution)
	if err := store.Create(ctx, solutionKind, "default", solution.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store Solution: %v", err)
	}
	revisionSpec := v1alpha1.SolutionSpec{Goal: "answer support questions", Target: &v1alpha1.SolutionTarget{Kind: "Agent", Ref: v1alpha1.LocalObjectReference{Name: "support-agent"}}}
	specData, _ := json.Marshal(revisionSpec)
	if err := store.CreateRevision(ctx, solutionKind, "default", solution.ObjectMeta.Name, revision, specData, nil); err != nil {
		t.Fatalf("store Solution revision: %v", err)
	}
	storeEvaluationDataset(t, store, v1alpha1.EvaluationDataset{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationDataset"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "regression", Namespace: "default"},
		Spec:       v1alpha1.EvaluationDatasetSpec{Version: "v2", Cases: []v1alpha1.EvaluationDatasetCase{{ID: "case-1", Input: "Help me"}}},
	})
	run := v1alpha1.EvaluationRun{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationRun"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "support-r2", Namespace: "default", UID: "run-uid"},
		Spec: v1alpha1.EvaluationRunSpec{
			EvaluationRef: v1alpha1.LocalObjectReference{Name: "quality"},
			TargetRef:     v1alpha1.EvaluationTargetRef{Kind: "Solution", Name: solution.ObjectMeta.Name, Revision: &revision},
			DatasetRef:    v1alpha1.EvaluationDatasetRef{Name: "regression", Version: "v2"},
		},
	}
	storeEvaluationRun(t, store, run)

	if _, err := NewEvaluationRunReconciler(store, zaptest.NewLogger(t)).Reconcile(ctx, Request{Kind: evaluationRunKind, Namespace: "default", Name: run.ObjectMeta.Name}); err != nil {
		t.Fatalf("reconcile Solution EvaluationRun: %v", err)
	}
	job := getJob(t, store, "default", evaluationCaseJobName(run.ObjectMeta.Name, "case-1"))
	if job.Spec.AgentRef == nil || job.Spec.AgentRef.Name != "support-agent" {
		t.Fatalf("Solution evaluation AgentJob target = %#v", job.Spec.AgentRef)
	}
	started := getEvaluationRun(t, store, "default", run.ObjectMeta.Name)
	if started.Spec.TargetRef.Revision == nil || *started.Spec.TargetRef.Revision != revision || started.Spec.DatasetRef.Version != "v2" || started.Status.DatasetDigest == "" {
		t.Fatalf("Solution evaluation traceability = spec %#v status %#v", started.Spec, started.Status)
	}
}

// TC-0292: Solution regression history retains the exact target revision.
func TestEvaluationReconcilerKeepsSolutionRevisionRegressionHistory(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	revision1 := int64(1)
	evaluation := v1alpha1.Evaluation{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Evaluation"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "solution-regression", Namespace: "default", UID: "eval-regression", Generation: 1},
		Spec: v1alpha1.EvaluationSpec{
			TargetRef:  v1alpha1.EvaluationTargetRef{Kind: "Solution", Name: "support-solution", Revision: &revision1},
			DatasetRef: v1alpha1.EvaluationDatasetRef{Name: "regression", Version: "v1"},
		},
	}
	storeEvaluation(t, store, evaluation)
	reconciler := NewEvaluationReconciler(store, zaptest.NewLogger(t))

	completeGeneration := func(generation int64, score float64, passed bool) {
		t.Helper()
		if _, err := reconciler.Reconcile(ctx, Request{Kind: evaluationKind, Namespace: "default", Name: evaluation.ObjectMeta.Name}); err != nil {
			t.Fatalf("create generation %d run: %v", generation, err)
		}
		runName := evaluationRunName(evaluation.ObjectMeta.Name, generation)
		run := getEvaluationRun(t, store, "default", runName)
		run.Status.Phase = v1alpha1.PhaseReady
		run.Status.Score = &score
		run.Status.Passed = passed
		now := time.Now().UTC()
		run.Status.CompletedAt = &now
		data, _ := json.Marshal(run)
		if err := store.Update(ctx, evaluationRunKind, "default", runName, data, run.ObjectMeta.ResourceVersion); err != nil {
			t.Fatalf("complete generation %d run: %v", generation, err)
		}
		if _, err := reconciler.Reconcile(ctx, Request{Kind: evaluationKind, Namespace: "default", Name: evaluation.ObjectMeta.Name}); err != nil {
			t.Fatalf("reflect generation %d run: %v", generation, err)
		}
	}

	completeGeneration(1, 0.72, false)
	stored := getEvaluation(t, store, "default", evaluation.ObjectMeta.Name)
	revision2 := int64(2)
	stored.ObjectMeta.Generation = 2
	stored.Spec.TargetRef.Revision = &revision2
	data, _ := json.Marshal(stored)
	if err := store.Update(ctx, evaluationKind, "default", stored.ObjectMeta.Name, data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update Evaluation to revision 2: %v", err)
	}
	completeGeneration(2, 0.96, true)

	stored = getEvaluation(t, store, "default", evaluation.ObjectMeta.Name)
	if len(stored.Status.RunHistory) != 2 {
		t.Fatalf("regression history = %#v", stored.Status.RunHistory)
	}
	if got := stored.Status.RunHistory[0].TargetRef.Revision; got == nil || *got != 2 || !stored.Status.RunHistory[0].Passed {
		t.Fatalf("newest history target = %#v", stored.Status.RunHistory[0])
	}
	if got := stored.Status.RunHistory[1].TargetRef.Revision; got == nil || *got != 1 || stored.Status.RunHistory[1].Passed {
		t.Fatalf("older history target = %#v", stored.Status.RunHistory[1])
	}
}

func TestEvaluationRunReconcilerFailsWhenDatasetIsMissing(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	run := v1alpha1.EvaluationRun{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationRun"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "agent-quality-g1",
			Namespace: "default",
			UID:       "run-uid-missing-dataset",
		},
		Spec: v1alpha1.EvaluationRunSpec{
			EvaluationRef: v1alpha1.LocalObjectReference{Name: "agent-quality"},
			TargetRef:     v1alpha1.EvaluationTargetRef{Kind: "Agent", Name: "assistant"},
			DatasetRef:    v1alpha1.EvaluationDatasetRef{Name: "missing-dataset"},
		},
	}
	storeEvaluationRun(t, store, run)

	reconciler := NewEvaluationRunReconciler(store, zaptest.NewLogger(t))
	if _, err := reconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: "agent-quality-g1"}); err != nil {
		t.Fatalf("reconcile missing dataset evaluationRun: %v", err)
	}
	failed := getEvaluationRun(t, store, "default", "agent-quality-g1")
	if failed.Status.Phase != v1alpha1.PhaseFailed || !failed.Status.Degraded || !strings.Contains(failed.Status.Message, "missing-dataset") {
		t.Fatalf("missing dataset status = %#v", failed.Status)
	}
}

func TestEvaluationRunReconcilerRejectsCrossNamespaceAndUnversionedPinnedDatasets(t *testing.T) {
	t.Run("cross namespace", func(t *testing.T) {
		store := newMemStore()
		run := v1alpha1.EvaluationRun{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: evaluationRunKind},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "cross-namespace", Namespace: "default", UID: "run-cross"},
			Spec: v1alpha1.EvaluationRunSpec{
				TargetRef:  v1alpha1.EvaluationTargetRef{Kind: "Agent", Name: "assistant"},
				DatasetRef: v1alpha1.EvaluationDatasetRef{Name: "private", Namespace: "other"},
			},
		}
		storeEvaluationRun(t, store, run)
		if _, err := NewEvaluationRunReconciler(store, zaptest.NewLogger(t)).Reconcile(context.Background(), Request{Kind: evaluationRunKind, Namespace: "default", Name: run.ObjectMeta.Name}); err != nil {
			t.Fatalf("reconcile cross-namespace dataset: %v", err)
		}
		failed := getEvaluationRun(t, store, "default", run.ObjectMeta.Name)
		if failed.Status.Phase != v1alpha1.PhaseFailed || !strings.Contains(failed.Status.Message, "EvaluationRun namespace") {
			t.Fatalf("cross-namespace status = %#v", failed.Status)
		}
	})

	t.Run("pinned version requires exact dataset version", func(t *testing.T) {
		dataset := &v1alpha1.EvaluationDataset{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "unversioned"},
			Spec:       v1alpha1.EvaluationDatasetSpec{Cases: []v1alpha1.EvaluationDatasetCase{{ID: "case", Input: "hello"}}},
		}
		if _, _, err := evaluationDatasetSnapshot(dataset, "v1"); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("unversioned pinned dataset error = %v", err)
		}
	})
}

func TestEvaluationRunReconcilerFailsWhenDatasetChangesAfterStarting(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	run := v1alpha1.EvaluationRun{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationRun"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-quality-g1", Namespace: "default", UID: "run-uid-dataset-change"},
		Spec: v1alpha1.EvaluationRunSpec{
			EvaluationRef: v1alpha1.LocalObjectReference{Name: "agent-quality"},
			TargetRef:     v1alpha1.EvaluationTargetRef{Kind: "Agent", Name: "assistant"},
			DatasetRef:    v1alpha1.EvaluationDatasetRef{Name: "smoke-dataset"},
		},
	}
	storeEvaluationRun(t, store, run)
	dataset := v1alpha1.EvaluationDataset{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationDataset"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "smoke-dataset", Namespace: "default"},
		Spec:       v1alpha1.EvaluationDatasetSpec{Cases: []v1alpha1.EvaluationDatasetCase{{ID: "first", Input: "First prompt"}}},
	}
	storeEvaluationDataset(t, store, dataset)

	reconciler := NewEvaluationRunReconciler(store, zaptest.NewLogger(t))
	if _, err := reconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: run.ObjectMeta.Name}); err != nil {
		t.Fatalf("reconcile evaluationRun start: %v", err)
	}
	started := getEvaluationRun(t, store, "default", run.ObjectMeta.Name)
	if started.Status.DatasetDigest == "" {
		t.Fatal("started EvaluationRun did not record dataset digest")
	}

	raw, err := store.Get(ctx, evaluationDatasetKind, "default", dataset.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get stored dataset: %v", err)
	}
	if err := json.Unmarshal(raw, &dataset); err != nil {
		t.Fatalf("unmarshal stored dataset: %v", err)
	}
	dataset.Spec.Cases = append(dataset.Spec.Cases, v1alpha1.EvaluationDatasetCase{ID: "added", Input: "Added prompt"})
	data, err := json.Marshal(dataset)
	if err != nil {
		t.Fatalf("marshal changed dataset: %v", err)
	}
	if err := store.Update(ctx, evaluationDatasetKind, dataset.ObjectMeta.Namespace, dataset.ObjectMeta.Name, data, dataset.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update changed dataset: %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: run.ObjectMeta.Name}); err != nil {
		t.Fatalf("reconcile changed dataset: %v", err)
	}
	failed := getEvaluationRun(t, store, "default", run.ObjectMeta.Name)
	if failed.Status.Phase != v1alpha1.PhaseFailed || !failed.Status.Degraded || failed.Status.Message != "EvaluationDataset changed after this EvaluationRun started" {
		t.Fatalf("changed dataset status = %#v", failed.Status)
	}
	if failed.Status.DatasetDigest != started.Status.DatasetDigest {
		t.Fatalf("changed dataset digest = %q, want original %q", failed.Status.DatasetDigest, started.Status.DatasetDigest)
	}
	for _, condition := range failed.Status.Conditions {
		if condition.Type == string(lifecycle.ConditionProgressing) && condition.Status != string(lifecycle.ConditionFalse) {
			t.Fatalf("changed dataset progressing condition = %#v, want terminal false", condition)
		}
	}
	if _, err := store.Get(ctx, agentJobKind, "default", evaluationCaseJobName(run.ObjectMeta.Name, "added")); !errors.Is(err, storepkg.ErrNotFound) {
		t.Fatalf("added dataset case created AgentJob, get error = %v", err)
	}
}

func TestEvaluationRunReconcilerFailsWhenDatasetCaseIDsNormalizeToSameJobName(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	run := v1alpha1.EvaluationRun{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationRun"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "agent-quality-g1",
			Namespace: "default",
			UID:       "run-uid-colliding-cases",
		},
		Spec: v1alpha1.EvaluationRunSpec{
			EvaluationRef: v1alpha1.LocalObjectReference{Name: "agent-quality"},
			TargetRef:     v1alpha1.EvaluationTargetRef{Kind: "Agent", Name: "assistant"},
			DatasetRef:    v1alpha1.EvaluationDatasetRef{Name: "smoke-dataset"},
		},
	}
	storeEvaluationRun(t, store, run)
	storeEvaluationDataset(t, store, v1alpha1.EvaluationDataset{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "EvaluationDataset"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "smoke-dataset", Namespace: "default"},
		Spec: v1alpha1.EvaluationDatasetSpec{Cases: []v1alpha1.EvaluationDatasetCase{
			{ID: "hello world", Input: "Say hello."},
			{ID: "hello-world", Input: "Say hello again."},
		}},
	})

	reconciler := NewEvaluationRunReconciler(store, zaptest.NewLogger(t))
	if _, err := reconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: "agent-quality-g1"}); err != nil {
		t.Fatalf("reconcile colliding dataset evaluationRun: %v", err)
	}
	failed := getEvaluationRun(t, store, "default", "agent-quality-g1")
	if failed.Status.Phase != v1alpha1.PhaseFailed || !failed.Status.Degraded || !strings.Contains(failed.Status.Message, "collides") {
		t.Fatalf("colliding dataset status = %#v", failed.Status)
	}
}

func TestEvaluationReconcilerReflectsPinnedAgentRevisionFailure(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	revision := int64(42)
	evaluation := v1alpha1.Evaluation{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Evaluation"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "pinned-agent-quality",
			Namespace:  "default",
			UID:        "eval-uid-002",
			Generation: 3,
		},
		Spec: v1alpha1.EvaluationSpec{
			TargetRef:  v1alpha1.EvaluationTargetRef{Kind: "Agent", Name: "assistant", Revision: &revision},
			DatasetRef: v1alpha1.EvaluationDatasetRef{Name: "smoke-dataset"},
		},
	}
	storeEvaluation(t, store, evaluation)

	evaluationReconciler := NewEvaluationReconciler(store, zaptest.NewLogger(t))
	runReconciler := NewEvaluationRunReconciler(store, zaptest.NewLogger(t))
	if _, err := evaluationReconciler.Reconcile(ctx, Request{Kind: "Evaluation", Namespace: "default", Name: "pinned-agent-quality"}); err != nil {
		t.Fatalf("reconcile evaluation: %v", err)
	}
	runName := evaluationRunName("pinned-agent-quality", 3)
	run := getEvaluationRun(t, store, "default", runName)
	if run.Spec.TargetRef.Revision == nil || *run.Spec.TargetRef.Revision != revision {
		t.Fatalf("evaluationRun target revision = %#v", run.Spec.TargetRef.Revision)
	}

	if _, err := runReconciler.Reconcile(ctx, Request{Kind: "EvaluationRun", Namespace: "default", Name: runName}); err != nil {
		t.Fatalf("reconcile pinned revision evaluationRun: %v", err)
	}
	run = getEvaluationRun(t, store, "default", runName)
	if run.Status.Phase != v1alpha1.PhaseFailed || !run.Status.Degraded || !strings.Contains(run.Status.Message, "pinned Agent revisions") {
		t.Fatalf("pinned revision run status = %#v", run.Status)
	}
	if _, err := evaluationReconciler.Reconcile(ctx, Request{Kind: "Evaluation", Namespace: "default", Name: "pinned-agent-quality"}); err != nil {
		t.Fatalf("reconcile failed evaluation: %v", err)
	}
	parent := getEvaluation(t, store, "default", "pinned-agent-quality")
	if parent.Status.Phase != v1alpha1.PhaseFailed || len(parent.Status.RunHistory) != 1 || parent.Status.RunHistory[0].Passed {
		t.Fatalf("failed evaluation status = %#v", parent.Status)
	}
}

// TC-0291: required metric thresholds distinguish passing and degraded results.
func TestEvaluateAgentJobOutputUsesBuiltInGraders(t *testing.T) {
	maxLatency := 750.0
	maxCost := 150.0
	minReliability := 0.9
	minMemory := 0.8
	startTime := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	completionTime := startTime.Add(620 * time.Millisecond)
	jobStatus := v1alpha1.AgentJobStatus{
		StartTime:      &startTime,
		CompletionTime: &completionTime,
		TokenUsage:     &v1alpha1.TokenUsage{PromptTokens: 90, CompletionTokens: 30, TotalTokens: 120},
		ToolCalls:      20,
		Output:         "answer cites source A and used memory",
	}
	metrics, score, passed := evaluateAgentJobOutput(jobStatus, []v1alpha1.EvaluationMetricThreshold{
		{Metric: "latency_ms", Max: &maxLatency, Required: true},
		{Metric: "cost_units", Max: &maxCost, Required: true},
		{Metric: "tool_reliability", Min: &minReliability, Required: true},
		{Metric: "memory_quality", Min: &minMemory, Required: true},
	}, []v1alpha1.EvaluationGrader{
		{Type: v1alpha1.EvaluationGraderTypeTaskSuccess, Parameters: map[string]string{"expected": "source A"}},
		{Type: v1alpha1.EvaluationGraderTypeHallucination, Parameters: map[string]string{"forbidden": "unsupported claim"}},
		{Type: v1alpha1.EvaluationGraderTypeSafety, Parameters: map[string]string{"forbidden": "unsafe"}},
		{Type: v1alpha1.EvaluationGraderTypeLatency},
		{Type: v1alpha1.EvaluationGraderTypeCost},
		{Type: v1alpha1.EvaluationGraderTypeToolReliability},
		{Type: v1alpha1.EvaluationGraderTypeMemoryQuality, Parameters: map[string]string{"score": "0.92"}},
	})

	if !passed || score != 1 {
		t.Fatalf("built-in grader result score=%v passed=%v metrics=%#v, want complete pass", score, passed, metrics)
	}
	got := metricsByName(metrics)
	for _, metric := range []string{"task_success", "hallucination_score", "safety_score", "latency_ms", "cost_units", "tool_reliability", "memory_quality"} {
		if result, ok := got[metric]; !ok || !result.Passed {
			t.Fatalf("metric %q = %#v, want passing built-in metric", metric, result)
		}
	}
	if got["latency_ms"].Value != 620 || got["cost_units"].Value != 120 || got["tool_reliability"].Value != 1 || got["memory_quality"].Value != 0.92 {
		t.Fatalf("numeric metrics = %#v, want telemetry-backed values", got)
	}

	metrics, score, passed = evaluateAgentJobOutput(v1alpha1.AgentJobStatus{Output: "answer includes unsupported claim"}, nil, []v1alpha1.EvaluationGrader{
		{Type: v1alpha1.EvaluationGraderTypeTaskSuccess},
		{Type: v1alpha1.EvaluationGraderTypeHallucination, Parameters: map[string]string{"forbidden": "unsupported claim"}},
	})
	if passed || score != 0.5 || metricsByName(metrics)["hallucination_score"].Passed {
		t.Fatalf("hallucination failure score=%v passed=%v metrics=%#v, want degraded result", score, passed, metrics)
	}

	metrics, _, passed = evaluateAgentJobOutput(v1alpha1.AgentJobStatus{Output: "answer"}, []v1alpha1.EvaluationMetricThreshold{
		{Metric: "latency_ms", Max: &maxLatency, Required: true},
	}, []v1alpha1.EvaluationGrader{{Type: v1alpha1.EvaluationGraderTypeLatency}})
	if passed || metricsByName(metrics)["latency_ms"].Passed {
		t.Fatalf("missing latency sample metrics=%#v, want required numeric grader failure", metrics)
	}

	metrics, _, passed = evaluateAgentJobOutput(v1alpha1.AgentJobStatus{Output: "answer"}, nil, []v1alpha1.EvaluationGrader{
		{Type: v1alpha1.EvaluationGraderTypeMemoryQuality, Parameters: map[string]string{"score": "not-a-number"}},
	})
	if passed || metricsByName(metrics)["memory_quality"].Passed {
		t.Fatalf("invalid memory quality sample metrics=%#v, want grader failure", metrics)
	}
}

func TestEvaluateAgentJobOutputFallsBackToSuppliedMetricSamples(t *testing.T) {
	metrics, _, passed := evaluateAgentJobOutput(v1alpha1.AgentJobStatus{Output: "tool timeout but acceptable answer"}, nil, []v1alpha1.EvaluationGrader{
		{Type: v1alpha1.EvaluationGraderTypeLatency, Parameters: map[string]string{"latencyMs": "410"}},
		{Type: v1alpha1.EvaluationGraderTypeCost, Parameters: map[string]string{"costUnits": "0.25"}},
		{Type: v1alpha1.EvaluationGraderTypeToolReliability, Parameters: map[string]string{"toolCalls": "4", "toolFailures": "1"}},
		{Type: v1alpha1.EvaluationGraderTypeMemoryQuality},
		{Type: "custom_quality"},
	})
	if !passed {
		t.Fatalf("fallback metric samples passed=false metrics=%#v", metrics)
	}
	got := metricsByName(metrics)
	if got["latency_ms"].Value != 410 || got["cost_units"].Value != 0.25 || got["tool_reliability"].Value != 0.75 {
		t.Fatalf("fallback metrics = %#v, want supplied sample values", got)
	}
	if got["memory_quality"].Value != 1 || got["custom_quality"].Metric != "custom_quality" {
		t.Fatalf("heuristic/default metrics = %#v", got)
	}

	maxCost := 10.0
	metrics, _, passed = evaluateAgentJobOutput(v1alpha1.AgentJobStatus{Output: "answer"}, []v1alpha1.EvaluationMetricThreshold{
		{Metric: "cost_units", Max: &maxCost, Required: true},
	}, []v1alpha1.EvaluationGrader{{Type: v1alpha1.EvaluationGraderTypeCost}})
	if passed || metricsByName(metrics)["cost_units"].Passed {
		t.Fatalf("missing cost telemetry metrics=%#v, want required metric failure", metrics)
	}
}

func TestEvaluateAgentJobOutputRejectsNonFiniteMetricSamples(t *testing.T) {
	tests := []struct {
		name   string
		grader v1alpha1.EvaluationGrader
		metric string
	}{
		{name: "latency NaN", grader: v1alpha1.EvaluationGrader{Type: v1alpha1.EvaluationGraderTypeLatency, Parameters: map[string]string{"latencyMs": "NaN"}}, metric: "latency_ms"},
		{name: "cost infinity", grader: v1alpha1.EvaluationGrader{Type: v1alpha1.EvaluationGraderTypeCost, Parameters: map[string]string{"cost": "+Inf"}}, metric: "cost_units"},
		{name: "tool reliability NaN", grader: v1alpha1.EvaluationGrader{Type: v1alpha1.EvaluationGraderTypeToolReliability, Parameters: map[string]string{"toolCalls": "NaN", "toolFailures": "0"}}, metric: "tool_reliability"},
		{name: "memory quality NaN", grader: v1alpha1.EvaluationGrader{Type: v1alpha1.EvaluationGraderTypeMemoryQuality, Parameters: map[string]string{"score": "NaN"}}, metric: "memory_quality"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics, _, passed := evaluateAgentJobOutput(v1alpha1.AgentJobStatus{Output: "answer"}, nil, []v1alpha1.EvaluationGrader{tt.grader})
			metric, ok := metricsByName(metrics)[tt.metric]
			if passed || !ok || metric.Passed || metric.Value != 0 {
				t.Fatalf("non-finite metric result = %#v, passed=%v; want a failed zero-valued metric", metrics, passed)
			}
		})
	}
}

func TestGradeLLMJudgeOutputParsesJSONAndRejectsInvalidScores(t *testing.T) {
	grader := v1alpha1.EvaluationGrader{Type: v1alpha1.EvaluationGraderTypeLLMJudge, Parameters: map[string]string{"metricName": "judge-score", "passingScore": "0.75"}}
	metric := gradeLLMJudgeOutput("{\"score\":0.8,\"reason\":\"good\"}", grader)
	if metric.Metric != "judge_score" || metric.Value != 0.8 || !metric.Passed || metric.Message != "good" {
		t.Fatalf("json judge metric = %#v", metric)
	}

	metric = gradeLLMJudgeOutput("```json\n{\"score\":0.6,\"passed\":true,\"message\":\"manual pass\"}\n```", grader)
	if metric.Value != 0.6 || !metric.Passed || metric.Message != "manual pass" {
		t.Fatalf("fenced judge metric = %#v", metric)
	}

	metric = gradeLLMJudgeOutput("{\"score\":1.2}", grader)
	if metric.Passed || !strings.Contains(metric.Message, "between 0 and 1") {
		t.Fatalf("out-of-range judge metric = %#v", metric)
	}

	metric = gradeLLMJudgeOutput("not-json", grader)
	if metric.Passed || !strings.Contains(metric.Message, "must be JSON") {
		t.Fatalf("invalid judge metric = %#v", metric)
	}
}

func metricsByName(metrics []v1alpha1.EvaluationMetricResult) map[string]v1alpha1.EvaluationMetricResult {
	out := make(map[string]v1alpha1.EvaluationMetricResult, len(metrics))
	for _, metric := range metrics {
		out[metric.Metric] = metric
	}
	return out
}

func storeEvaluation(t *testing.T, s *memStore, evaluation v1alpha1.Evaluation) {
	t.Helper()
	data, err := json.Marshal(evaluation)
	if err != nil {
		t.Fatalf("marshal evaluation: %v", err)
	}
	if err := s.Create(context.Background(), "Evaluation", evaluation.ObjectMeta.Namespace, evaluation.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store evaluation: %v", err)
	}
}

func getEvaluation(t *testing.T, s *memStore, namespace, name string) v1alpha1.Evaluation {
	t.Helper()
	raw, err := s.Get(context.Background(), "Evaluation", namespace, name)
	if err != nil {
		t.Fatalf("get evaluation: %v", err)
	}
	var evaluation v1alpha1.Evaluation
	if err := json.Unmarshal(raw, &evaluation); err != nil {
		t.Fatalf("unmarshal evaluation: %v", err)
	}
	return evaluation
}

func storeEvaluationRun(t *testing.T, s *memStore, run v1alpha1.EvaluationRun) {
	t.Helper()
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal evaluationRun: %v", err)
	}
	if err := s.Create(context.Background(), "EvaluationRun", run.ObjectMeta.Namespace, run.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store evaluationRun: %v", err)
	}
}

func storeEvaluationDataset(t *testing.T, s *memStore, dataset v1alpha1.EvaluationDataset) {
	t.Helper()
	data, err := json.Marshal(dataset)
	if err != nil {
		t.Fatalf("marshal evaluationDataset: %v", err)
	}
	if err := s.Create(context.Background(), "EvaluationDataset", dataset.ObjectMeta.Namespace, dataset.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store evaluationDataset: %v", err)
	}
}

func getEvaluationRun(t *testing.T, s *memStore, namespace, name string) v1alpha1.EvaluationRun {
	t.Helper()
	raw, err := s.Get(context.Background(), "EvaluationRun", namespace, name)
	if err != nil {
		t.Fatalf("get evaluationRun: %v", err)
	}
	var run v1alpha1.EvaluationRun
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatalf("unmarshal evaluationRun: %v", err)
	}
	return run
}

func updateEvaluationTargetJob(t *testing.T, s *memStore, job v1alpha1.AgentJob) {
	t.Helper()
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	if err := s.Update(context.Background(), "AgentJob", job.ObjectMeta.Namespace, job.ObjectMeta.Name, data, job.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update job: %v", err)
	}
}
