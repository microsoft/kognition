// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap/zaptest"
)

type terminalMetricsFaultStore struct {
	store.Store
	runs            map[string][]byte
	updateErr       error
	failReadback    bool
	readbackPending bool
}

func (storage *terminalMetricsFaultStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	if storage.readbackPending {
		storage.readbackPending = false
		return nil, errors.New("readback unavailable after successful write")
	}
	if data, ok := storage.runs[kind+"/"+namespace+"/"+name]; ok {
		return data, nil
	}
	return nil, store.ErrNotFound
}

func (storage *terminalMetricsFaultStore) List(_ context.Context, kind string, options store.ListOptions) ([][]byte, error) {
	var result [][]byte
	if kind != "WorkflowRun" {
		return result, nil
	}
	for _, data := range storage.runs {
		var run v1alpha1.WorkflowRun
		if err := json.Unmarshal(data, &run); err != nil {
			return nil, err
		}
		if options.Namespace == "" || run.ObjectMeta.Namespace == options.Namespace {
			result = append(result, data)
		}
	}
	return result, nil
}

func (storage *terminalMetricsFaultStore) Update(ctx context.Context, kind, namespace, name string, data []byte, version int64) error {
	if storage.updateErr != nil {
		return storage.updateErr
	}
	storage.runs[kind+"/"+namespace+"/"+name] = data
	storage.readbackPending = storage.failReadback
	return nil
}

func TestWorkflowRunTerminalMetricsRecovery(t *testing.T) {
	for _, phase := range []v1alpha1.Phase{v1alpha1.PhaseReady, v1alpha1.PhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			storage := &terminalMetricsFaultStore{runs: map[string][]byte{}}
			workflowName := "terminal-recovery-" + string(phase)
			run := v1alpha1.WorkflowRun{}
			run.ObjectMeta.Name = "finished"
			run.ObjectMeta.Namespace = "metrics-recovery"
			run.ObjectMeta.Annotations = map[string]string{observability.AnnotationMetricsStarted: "true"}
			run.Spec.WorkflowRef.Name = workflowName
			run.Status.Phase = phase
			encoded, err := json.Marshal(run)
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.Update(ctx, "WorkflowRun", run.ObjectMeta.Namespace, run.ObjectMeta.Name, encoded, 0); err != nil {
				t.Fatal(err)
			}
			registry := prometheus.NewRegistry()
			observability.NewWorkflowMetrics(registry, storage)
			reconciler := NewWorkflowRunReconciler(storage, zaptest.NewLogger(t))
			request := Request{Namespace: run.ObjectMeta.Namespace, Name: run.ObjectMeta.Name}
			storage.updateErr = errors.New("metrics write unavailable")
			if _, err := reconciler.Reconcile(ctx, request); !errors.Is(err, storage.updateErr) {
				t.Fatalf("reconcile error = %v, want retriable metrics write failure", err)
			}
			storage.updateErr = nil
			storage.failReadback = true
			if _, err := reconciler.Reconcile(ctx, request); err == nil {
				t.Fatal("expected readback failure after durable completion write")
			}
			storage.failReadback = false
			for attempt := 0; attempt < 3; attempt++ {
				if _, err := reconciler.Reconcile(ctx, request); err != nil {
					t.Fatal(err)
				}
			}
			stored, err := storage.Get(ctx, "WorkflowRun", request.Namespace, request.Name)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(stored, &run); err != nil {
				t.Fatal(err)
			}
			if run.ObjectMeta.Annotations[observability.AnnotationMetricsCompleted] != "true" {
				t.Fatal("terminal retry did not persist completion marker")
			}
			families, err := registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			foundGauge, foundCounter := false, false
			for _, family := range families {
				for _, metric := range family.Metric {
					matches := false
					for _, label := range metric.Label {
						if label.GetName() == "workflow" && label.GetValue() == workflowName {
							matches = true
						}
					}
					if !matches {
						continue
					}
					switch family.GetName() {
					case "kognition_workflow_active_runs":
						foundGauge = true
						if metric.GetGauge().GetValue() != 0 {
							t.Fatalf("active gauge = %v, want zero after restart recovery", metric.GetGauge().GetValue())
						}
					case "kognition_workflow_retained_runs":
						foundCounter = true
						if metric.GetGauge().GetValue() != 1 {
							t.Fatalf("retained gauge = %v, want one despite retries", metric.GetGauge().GetValue())
						}
					}
				}
			}
			if !foundGauge || !foundCounter {
				t.Fatal("expected active gauge and completion counter")
			}
		})
	}
}
