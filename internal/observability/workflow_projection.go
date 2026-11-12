// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	AnnotationMetricsStarted   = "kognition.dev/metrics-started"
	AnnotationMetricsCompleted = "kognition.dev/metrics-completed"
)

type WorkflowMetrics struct {
	source       store.Store
	active       *prometheus.Desc
	runs         *prometheus.Desc
	duration     *prometheus.Desc
	observations *prometheus.Desc
}

func NewWorkflowMetrics(registry prometheus.Registerer, source store.Store) *WorkflowMetrics {
	collector := &WorkflowMetrics{
		source:       source,
		active:       prometheus.NewDesc("kognition_workflow_active_runs", "Number of retained WorkflowRuns currently executing.", []string{"namespace", "workflow"}, nil),
		runs:         prometheus.NewDesc("kognition_workflow_retained_runs", "Number of retained terminal WorkflowRuns; decreases under retention.", []string{"namespace", "workflow", "phase"}, nil),
		duration:     prometheus.NewDesc("kognition_workflow_retained_step_duration_seconds_sum", "Sum of retained workflow step durations with owned job evidence; decreases under retention.", []string{"namespace", "workflow", "step", "phase"}, nil),
		observations: prometheus.NewDesc("kognition_workflow_retained_step_duration_observations", "Number of retained workflow step duration observations; decreases under retention.", []string{"namespace", "workflow", "step", "phase"}, nil),
	}
	registry.MustRegister(collector)
	return collector
}

func (m *WorkflowMetrics) Describe(output chan<- *prometheus.Desc) {
	output <- m.active
	output <- m.runs
	output <- m.duration
	output <- m.observations
}

func (m *WorkflowMetrics) Collect(output chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rawRuns, err := m.source.List(ctx, "WorkflowRun", store.ListOptions{})
	if err != nil {
		output <- prometheus.NewInvalidMetric(m.active, fmt.Errorf("list workflow runs: %w", err))
		return
	}
	rawJobs, err := m.source.List(ctx, "AgentJob", store.ListOptions{})
	if err != nil {
		output <- prometheus.NewInvalidMetric(m.duration, fmt.Errorf("list workflow jobs: %w", err))
		return
	}
	jobs := make(map[[3]string][]v1alpha1.AgentJob)
	for _, raw := range rawJobs {
		var job v1alpha1.AgentJob
		if err := json.Unmarshal(raw, &job); err != nil {
			output <- prometheus.NewInvalidMetric(m.duration, fmt.Errorf("decode workflow job: %w", err))
			return
		}
		for _, owner := range job.ObjectMeta.OwnerReferences {
			if owner.Kind == "WorkflowRun" {
				key := [3]string{job.ObjectMeta.Namespace, owner.Name, owner.UID}
				jobs[key] = append(jobs[key], job)
				break
			}
		}
	}
	active := make(map[[2]string]float64)
	runs := make(map[[3]string]float64)
	durations := make(map[[4]string]float64)
	observations := make(map[[4]string]float64)
	for _, raw := range rawRuns {
		var run v1alpha1.WorkflowRun
		if err := json.Unmarshal(raw, &run); err != nil {
			output <- prometheus.NewInvalidMetric(m.active, fmt.Errorf("decode workflow run: %w", err))
			return
		}
		namespace, workflow := run.ObjectMeta.Namespace, run.Spec.WorkflowRef.Name
		key := [2]string{namespace, workflow}
		active[key] += 0
		terminal := run.Status.Phase == v1alpha1.PhaseReady || run.Status.Phase == v1alpha1.PhaseFailed
		if terminal {
			runs[[3]string{namespace, workflow, string(run.Status.Phase)}]++
		} else if run.ObjectMeta.DeletionTimestamp == nil && (run.Status.StartTime != nil || run.ObjectMeta.Annotations[AnnotationMetricsStarted] == "true") {
			active[key]++
		}
		owned := jobs[[3]string{namespace, run.ObjectMeta.Name, run.ObjectMeta.UID}]
		for _, step := range run.Status.StepStatuses {
			if step.StartedAt == nil {
				continue
			}
			var completed *time.Time
			for _, job := range owned {
				if job.ObjectMeta.Annotations[AnnotationStepName] != step.Name || job.Status.CompletionTime == nil || job.Status.CompletionTime.Before(*step.StartedAt) {
					continue
				}
				if completed == nil || job.Status.CompletionTime.After(*completed) {
					completed = job.Status.CompletionTime
				}
			}
			if completed != nil && (step.Phase == v1alpha1.PhaseReady || step.Phase == v1alpha1.PhaseFailed) {
				labels := [4]string{namespace, workflow, step.Name, string(step.Phase)}
				durations[labels] += completed.Sub(*step.StartedAt).Seconds()
				observations[labels]++
			}
		}
	}
	for labels, value := range active {
		output <- prometheus.MustNewConstMetric(m.active, prometheus.GaugeValue, value, labels[:]...)
	}
	for labels, value := range runs {
		output <- prometheus.MustNewConstMetric(m.runs, prometheus.GaugeValue, value, labels[:]...)
	}
	for labels, value := range durations {
		output <- prometheus.MustNewConstMetric(m.duration, prometheus.GaugeValue, value, labels[:]...)
		output <- prometheus.MustNewConstMetric(m.observations, prometheus.GaugeValue, observations[labels], labels[:]...)
	}
}
