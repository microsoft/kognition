// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// TC-0298: Tool Gateway metrics underpin Prometheus scrape and dashboard validation.
func TestMetricsRecordInvocationAndResilience(t *testing.T) {
	InitMetrics()
	recordInvocation("success", time.Millisecond)
	recordResilience("circuit_open")
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seenInvocation, seenLatency, seenResilience := false, false, false
	for _, family := range families {
		switch family.GetName() {
		case "kognition_tool_gateway_invocations_total":
			seenInvocation = metricFamilyHasLabel(family.GetMetric(), "outcome", "success")
		case "kognition_tool_gateway_invocation_duration_seconds":
			seenLatency = metricFamilyHasLabel(family.GetMetric(), "outcome", "success")
		case "kognition_tool_gateway_resilience_events_total":
			seenResilience = metricFamilyHasLabel(family.GetMetric(), "reason", "circuit_open")
		}
	}
	if !seenInvocation || !seenLatency || !seenResilience {
		t.Fatalf("gateway metric families missing invocation=%t latency=%t resilience=%t", seenInvocation, seenLatency, seenResilience)
	}
}

func metricFamilyHasLabel(metrics []*dto.Metric, name, value string) bool {
	for _, metric := range metrics {
		for _, label := range metric.GetLabel() {
			if label.GetName() == name && label.GetValue() == value {
				return true
			}
		}
	}
	return false
}
