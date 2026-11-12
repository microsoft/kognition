// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

type metricsRunStore struct{ store.Store }

func (metricsRunStore) List(_ context.Context, kind string, _ store.ListOptions) ([][]byte, error) {
	if kind == "WorkflowRun" {
		return [][]byte{[]byte(`{"metadata":{"namespace":"metrics-test-workspace","name":"run"},"spec":{"workflowRef":{"name":"metrics-test-workflow"}},"status":{"phase":"Ready"}}`)}, nil
	}
	return nil, nil
}

func TestControllerMetricsEndpointIncludesWorkflowAndLocalMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	localCounter := prometheus.NewCounter(prometheus.CounterOpts{Name: "controller_local_test_total", Help: "Local registry test counter."})
	registry.MustRegister(localCounter)
	localCounter.Inc()

	observability.NewWorkflowMetrics(registry, metricsRunStore{})

	server := newControllerMetricsServer(registry)
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", response.Code, response.Body.String())
	}
	for _, sample := range []string{
		"controller_local_test_total 1",
		`kognition_workflow_active_runs{namespace="metrics-test-workspace",workflow="metrics-test-workflow"} 0`,
		`kognition_workflow_retained_runs{namespace="metrics-test-workspace",phase="Ready",workflow="metrics-test-workflow"} 1`,
	} {
		if !strings.Contains(response.Body.String(), sample) {
			t.Errorf("metrics endpoint missing %s", sample)
		}
	}
}
