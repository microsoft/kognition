// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kognition-ai/kognition/internal/cpi"
)

func TestCPICallMetricsCountsOutcomesByProviderOperationAndReason(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewCPICallMetrics(registry)

	metrics.RecordCall(cpi.CallOutcome{Provider: "anthropic", Operation: cpi.OperationComplete, Model: "claude", Duration: 120 * time.Millisecond})
	metrics.RecordCall(cpi.CallOutcome{Provider: "anthropic", Operation: cpi.OperationComplete, Model: "other-model", Duration: time.Second})
	metrics.RecordCall(cpi.CallOutcome{Provider: "anthropic", Operation: cpi.OperationComplete, Reason: cpi.ReasonRateLimited, ProviderFault: true})
	metrics.RecordCall(cpi.CallOutcome{Provider: "anthropic", Operation: cpi.OperationComplete, Reason: cpi.ReasonInvalidRequest})
	metrics.RecordCall(cpi.CallOutcome{Provider: "openai-compatible", Operation: cpi.OperationEmbed, Reason: cpi.ReasonUnreachable, ProviderFault: true})

	tests := []struct {
		provider, operation, outcome, reason string
		want                                 float64
	}{
		{"anthropic", "complete", "success", "none", 2},
		{"anthropic", "complete", "provider_failure", "RateLimited", 1},
		{"anthropic", "complete", "request_failure", "InvalidRequest", 1},
		{"openai-compatible", "embed", "provider_failure", "EndpointUnreachable", 1},
	}
	for _, test := range tests {
		got := testutil.ToFloat64(metrics.calls.WithLabelValues(test.provider, test.operation, test.outcome, test.reason))
		if got != test.want {
			t.Errorf("calls{%s,%s,%s,%s} = %v, want %v", test.provider, test.operation, test.outcome, test.reason, got, test.want)
		}
	}
	if got := testutil.CollectAndCount(metrics.calls); got != len(tests) {
		t.Errorf("calls has %d series, want %d: the model must not create series", got, len(tests))
	}
}

func TestCPICallMetricsObservesDurationPerOutcome(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewCPICallMetrics(registry)
	metrics.RecordCall(cpi.CallOutcome{Provider: "foundry", Operation: cpi.OperationComplete, Duration: 250 * time.Millisecond})
	metrics.RecordCall(cpi.CallOutcome{Provider: "foundry", Operation: cpi.OperationComplete, Duration: 750 * time.Millisecond})
	metrics.RecordCall(cpi.CallOutcome{Provider: "foundry", Operation: cpi.OperationComplete, Reason: cpi.ReasonTimeout, ProviderFault: true, Duration: 60 * time.Second})

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	counts := map[string]uint64{}
	sums := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "kognition_cpi_call_duration_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "outcome" {
					counts[label.GetValue()] = metric.GetHistogram().GetSampleCount()
					sums[label.GetValue()] = metric.GetHistogram().GetSampleSum()
				}
			}
		}
	}
	if counts["success"] != 2 || sums["success"] != 1.0 || counts["provider_failure"] != 1 || sums["provider_failure"] != 60 {
		t.Fatalf("durations = counts %v, sums %v; want 2 successes summing 1s and 1 provider failure of 60s", counts, sums)
	}
}

func TestCPICallMetricsBoundsOperationAndReasonLabels(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewCPICallMetrics(registry)

	metrics.RecordCall(cpi.CallOutcome{Provider: "p", Operation: "free-form-operation"})
	metrics.RecordCall(cpi.CallOutcome{Provider: "p", Operation: cpi.OperationComplete, Reason: "provider said: arbitrary text", ProviderFault: true})

	if got := testutil.ToFloat64(metrics.calls.WithLabelValues("p", "complete", "success", "none")); got != 1 {
		t.Errorf("unknown operation was not folded into complete, count = %v", got)
	}
	if got := testutil.ToFloat64(metrics.calls.WithLabelValues("p", "complete", "provider_failure", "Other")); got != 1 {
		t.Errorf("unknown reason was not folded into Other, count = %v", got)
	}
}

func TestCPICallMetricsExposeStableMetricNames(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewCPICallMetrics(registry)
	metrics.RecordCall(cpi.CallOutcome{Provider: "ollama", Operation: cpi.OperationComplete, Duration: time.Millisecond})

	expected := `
# HELP kognition_cpi_calls_total Completed model calls through CPI drivers by provider, operation, outcome and failure reason.
# TYPE kognition_cpi_calls_total counter
kognition_cpi_calls_total{operation="complete",outcome="success",provider="ollama",reason="none"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(expected), "kognition_cpi_calls_total"); err != nil {
		t.Fatal(err)
	}
}
