// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type workflowProjectionStore struct {
	store.Store
	runs   [][]byte
	jobs   [][]byte
	err    error
	jobErr error
}

func (s *workflowProjectionStore) List(_ context.Context, kind string, _ store.ListOptions) ([][]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	if kind == "WorkflowRun" {
		return s.runs, nil
	}
	return s.jobs, s.jobErr
}

func TestWorkflowProjectionRecoversFromStoreFailureAndRestart(t *testing.T) {
	source := &workflowProjectionStore{runs: [][]byte{[]byte(`{"metadata":{"name":"run","namespace":"team","uid":"run-uid"},"spec":{"workflowRef":{"name":"workflow"}},"status":{"phase":"Ready"}}`)}}
	registry := prometheus.NewRegistry()
	NewWorkflowMetrics(registry, source)
	if count, err := testutil.GatherAndCount(registry, "kognition_workflow_retained_runs"); err != nil || count != 1 {
		t.Fatalf("retained run: count=%d, err=%v", count, err)
	}
	source.err = errors.New("temporary list failure")
	if _, err := registry.Gather(); err == nil {
		t.Fatal("failed source read must fail the scrape, not publish a misleading zero")
	}
	source.err = nil
	restarted := prometheus.NewRegistry()
	NewWorkflowMetrics(restarted, source)
	if count, err := testutil.GatherAndCount(restarted, "kognition_workflow_retained_runs"); err != nil || count != 1 {
		t.Fatalf("recovered retained run: count=%d, err=%v", count, err)
	}
}

func TestWorkflowProjectionDurationOwnershipAndRetention(t *testing.T) {
	started := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	completed := started.Add(3 * time.Second)
	run := v1alpha1.WorkflowRun{}
	run.ObjectMeta.Name, run.ObjectMeta.Namespace, run.ObjectMeta.UID = "run", "team", "uid"
	run.Spec.WorkflowRef.Name = "workflow"
	run.Status.Phase = v1alpha1.PhaseReady
	run.Status.StepStatuses = []v1alpha1.WorkflowStepStatus{{Name: "step", Phase: v1alpha1.PhaseReady, StartedAt: &started}}
	job := v1alpha1.AgentJob{}
	job.ObjectMeta.Namespace = "team"
	job.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{Kind: "WorkflowRun", Name: "run", UID: "uid"}}
	job.ObjectMeta.Annotations = map[string]string{AnnotationStepName: "step"}
	job.Status.CompletionTime = &completed
	encode := func(value any) []byte {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, scenario := range []string{"owned", "wrong uid", "wrong namespace", "wrong name", "missing completion", "negative duration", "missing start"} {
		t.Run(scenario, func(t *testing.T) {
			candidate := job
			candidate.ObjectMeta.OwnerReferences = append([]v1alpha1.OwnerReference(nil), job.ObjectMeta.OwnerReferences...)
			candidateRun := run
			candidateRun.Status.StepStatuses = append([]v1alpha1.WorkflowStepStatus(nil), run.Status.StepStatuses...)
			switch scenario {
			case "wrong uid":
				candidate.ObjectMeta.OwnerReferences[0].UID = "other"
			case "wrong namespace":
				candidate.ObjectMeta.Namespace = "other"
			case "wrong name":
				candidate.ObjectMeta.OwnerReferences[0].Name = "other"
			case "missing completion":
				candidate.Status.CompletionTime = nil
			case "negative duration":
				before := started.Add(-time.Second)
				candidate.Status.CompletionTime = &before
			case "missing start":
				candidateRun.Status.StepStatuses[0].StartedAt = nil
			}
			source := &workflowProjectionStore{runs: [][]byte{encode(candidateRun)}, jobs: [][]byte{encode(candidate)}}
			registry := prometheus.NewRegistry()
			NewWorkflowMetrics(registry, source)
			if err := testutil.GatherAndCompare(registry, strings.NewReader(`# HELP kognition_workflow_retained_runs Number of retained terminal WorkflowRuns; decreases under retention.
# TYPE kognition_workflow_retained_runs gauge
kognition_workflow_retained_runs{namespace="team",phase="Ready",workflow="workflow"} 1
`), "kognition_workflow_retained_runs"); err != nil {
				t.Fatal(err)
			}
			if scenario == "owned" {
				t.Run("TC-0243 retained metrics endpoint survives restart", func(t *testing.T) {
					scrape := func(gatherer prometheus.Gatherer) string {
						response := httptest.NewRecorder()
						promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
						if response.Code != http.StatusOK {
							t.Fatalf("metrics status = %d", response.Code)
						}
						return response.Body.String()
					}
					beforeRestart := scrape(registry)
					restarted := prometheus.NewRegistry()
					NewWorkflowMetrics(restarted, source)
					afterRestart := scrape(restarted)
					if afterRestart != beforeRestart {
						t.Fatal("retained metric exposition changed after collector restart")
					}
					for _, sample := range []string{
						`kognition_workflow_retained_runs{namespace="team",phase="Ready",workflow="workflow"} 1`,
						`kognition_workflow_retained_step_duration_seconds_sum{namespace="team",phase="Ready",step="step",workflow="workflow"} 3`,
						`kognition_workflow_retained_step_duration_observations{namespace="team",phase="Ready",step="step",workflow="workflow"} 1`,
					} {
						if !strings.Contains(afterRestart, sample) {
							t.Fatalf("restarted scrape missing %s", sample)
						}
					}
				})
				if err := testutil.GatherAndCompare(registry, strings.NewReader(`# HELP kognition_workflow_retained_step_duration_seconds_sum Sum of retained workflow step durations with owned job evidence; decreases under retention.
# TYPE kognition_workflow_retained_step_duration_seconds_sum gauge
kognition_workflow_retained_step_duration_seconds_sum{namespace="team",phase="Ready",step="step",workflow="workflow"} 3
# HELP kognition_workflow_retained_step_duration_observations Number of retained workflow step duration observations; decreases under retention.
# TYPE kognition_workflow_retained_step_duration_observations gauge
kognition_workflow_retained_step_duration_observations{namespace="team",phase="Ready",step="step",workflow="workflow"} 1
`), "kognition_workflow_retained_step_duration_seconds_sum", "kognition_workflow_retained_step_duration_observations"); err != nil {
					t.Fatal(err)
				}
			} else if count, err := testutil.GatherAndCount(registry, "kognition_workflow_retained_step_duration_observations"); err != nil || count != 0 {
				t.Fatalf("unproven duration emitted: %d, %v", count, err)
			}
			source.jobErr = errors.New("job list temporarily unavailable")
			if _, err := registry.Gather(); err == nil {
				t.Fatal("job read failure must fail scrape")
			}
			source.jobErr = nil
			source.runs = nil
			if count, err := testutil.GatherAndCount(registry); err != nil || count != 0 {
				t.Fatalf("retention left samples: %d, %v", count, err)
			}
		})
	}
}
