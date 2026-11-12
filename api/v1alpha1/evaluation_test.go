// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEvaluationJSONRoundTripPreservesEvaluationContract(t *testing.T) {
	revision := int64(7)
	minScore := 0.85
	maxLatency := 2500.0
	score := 0.91
	startedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(2 * time.Minute)
	evaluation := &Evaluation{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: "Evaluation"},
		ObjectMeta: ObjectMeta{
			Name:      "writer-regression",
			Namespace: "default",
		},
		Spec: EvaluationSpec{
			TargetRef: EvaluationTargetRef{Kind: "Solution", Name: "launch", Revision: &revision},
			DatasetRef: EvaluationDatasetRef{
				Name:    "launch-dataset",
				Version: "2026-08-12",
			},
			Graders: []EvaluationGrader{{
				Name:       "task-success",
				Type:       "task-success",
				Parameters: map[string]string{"rubric": "must answer the launch brief"},
			}},
			MetricThresholds: []EvaluationMetricThreshold{
				{Metric: "task_success", Min: &minScore, Required: true},
				{Metric: "latency_ms", Max: &maxLatency},
			},
			Schedule: &EvaluationSchedule{
				Cron:                      "0 */6 * * *",
				Timezone:                  "UTC",
				ConcurrencyPolicy:         EvaluationConcurrencyForbid,
				SuccessfulRunHistoryLimit: 5,
				FailedRunHistoryLimit:     3,
			},
		},
		Status: EvaluationStatus{
			Phase:              PhaseReady,
			ObservedGeneration: 3,
			LastRunRef:         &LocalObjectReference{Name: "writer-regression-202608120900"},
			LastRunTime:        &completedAt,
			RunHistory: []EvaluationRunSummary{{
				Name:        "writer-regression-202608120900",
				Phase:       PhaseReady,
				StartedAt:   &startedAt,
				CompletedAt: &completedAt,
				Score:       &score,
				Passed:      true,
				Metrics: []EvaluationMetricResult{{
					Metric: "task_success",
					Value:  0.91,
					Passed: true,
				}},
			}},
		},
	}

	data, err := json.Marshal(evaluation)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got Evaluation
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if got.Spec.TargetRef.Kind != "Solution" || got.Spec.TargetRef.Name != "launch" || got.Spec.TargetRef.Revision == nil || *got.Spec.TargetRef.Revision != revision {
		t.Fatalf("targetRef = %+v, want Solution launch revision %d", got.Spec.TargetRef, revision)
	}
	if got.Spec.DatasetRef.Name != "launch-dataset" || got.Spec.DatasetRef.Version != "2026-08-12" {
		t.Fatalf("datasetRef = %+v, want named versioned dataset", got.Spec.DatasetRef)
	}
	if len(got.Spec.Graders) != 1 || got.Spec.Graders[0].Parameters["rubric"] == "" {
		t.Fatalf("graders = %+v, want grader parameters", got.Spec.Graders)
	}
	if len(got.Spec.MetricThresholds) != 2 || got.Spec.MetricThresholds[0].Min == nil || *got.Spec.MetricThresholds[0].Min != minScore || got.Spec.MetricThresholds[1].Max == nil || *got.Spec.MetricThresholds[1].Max != maxLatency {
		t.Fatalf("metricThresholds = %+v, want min score and max latency", got.Spec.MetricThresholds)
	}
	if got.Spec.Schedule == nil || got.Spec.Schedule.ConcurrencyPolicy != EvaluationConcurrencyForbid || got.Spec.Schedule.SuccessfulRunHistoryLimit != 5 {
		t.Fatalf("schedule = %+v, want scheduled forbid policy", got.Spec.Schedule)
	}
	if got.Status.LastRunRef == nil || got.Status.LastRunRef.Name != "writer-regression-202608120900" || len(got.Status.RunHistory) != 1 {
		t.Fatalf("status = %+v, want last run history", got.Status)
	}
	if got.Status.RunHistory[0].Score == nil || *got.Status.RunHistory[0].Score != score || len(got.Status.RunHistory[0].Metrics) != 1 {
		t.Fatalf("runHistory = %+v, want score and metrics", got.Status.RunHistory)
	}
}

func TestEvaluationRunJSONRoundTripPreservesRunContract(t *testing.T) {
	revision := int64(11)
	minScore := 0.8
	score := 1.0
	startedAt := time.Date(2026, time.August, 12, 10, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(30 * time.Second)
	run := &EvaluationRun{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: "EvaluationRun"},
		ObjectMeta: ObjectMeta{
			Name:      "writer-regression-g3",
			Namespace: "default",
			OwnerReferences: []OwnerReference{{
				APIVersion: APIVersion,
				Kind:       "Evaluation",
				Name:       "writer-regression",
				UID:        "eval-uid",
			}},
		},
		Spec: EvaluationRunSpec{
			EvaluationRef: LocalObjectReference{Name: "writer-regression"},
			TargetRef:     EvaluationTargetRef{Kind: "Agent", Name: "writer", Revision: &revision},
			DatasetRef:    EvaluationDatasetRef{Name: "writer-dataset", Version: "v1"},
			Graders: []EvaluationGrader{{
				Name:       "task-success",
				Type:       "task-success",
				Parameters: map[string]string{"expected": "ship brief"},
			}},
			MetricThresholds: []EvaluationMetricThreshold{{Metric: "task_success", Min: &minScore, Required: true}},
		},
		Status: EvaluationRunStatus{
			Phase:        PhaseReady,
			StartedAt:    &startedAt,
			CompletedAt:  &completedAt,
			TargetJobRef: &LocalObjectReference{Name: "writer-regression-g3-target"},
			TargetOutput: "ship brief complete",
			Score:        &score,
			Passed:       true,
			Metrics: []EvaluationMetricResult{{
				Metric:  "task_success",
				Value:   1,
				Passed:  true,
				Message: "target completed successfully",
			}},
			Message: "evaluation passed",
		},
	}

	data, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got EvaluationRun
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if got.Spec.EvaluationRef.Name != "writer-regression" || got.Spec.TargetRef.Kind != "Agent" || got.Spec.TargetRef.Revision == nil || *got.Spec.TargetRef.Revision != revision {
		t.Fatalf("spec = %+v, want parent Evaluation and Agent revision", got.Spec)
	}
	if len(got.Spec.Graders) != 1 || got.Spec.Graders[0].Parameters["expected"] != "ship brief" {
		t.Fatalf("graders = %+v, want grader parameters", got.Spec.Graders)
	}
	if got.Status.TargetJobRef == nil || got.Status.TargetJobRef.Name != "writer-regression-g3-target" {
		t.Fatalf("targetJobRef = %+v, want target job reference", got.Status.TargetJobRef)
	}
	if got.Status.Score == nil || *got.Status.Score != score || !got.Status.Passed || got.Status.Degraded {
		t.Fatalf("status = %+v, want passed score", got.Status)
	}
	if len(got.Status.Metrics) != 1 || got.Status.Metrics[0].Metric != "task_success" || !got.Status.Metrics[0].Passed {
		t.Fatalf("metrics = %+v, want passing task_success metric", got.Status.Metrics)
	}
}
