// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type usageEventWriterStub struct {
	applied bool
	err     error
}

func (s *usageEventWriterStub) ApplyUsageEvent(context.Context, CostRecord) (bool, error) {
	return s.applied, s.err
}

func TestModelUsageMetrics_RecordOnlyNewlyAppliedEvents(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewModelUsageMetrics(registry)
	store := &usageEventWriterStub{applied: true}
	writer := metrics.Wrap(store)
	event := CostRecord{
		Workspace:              "team-a",
		Agent:                  "planner",
		Workflow:               "research-flow",
		Step:                   "plan",
		OperationType:          CostOpModelInference,
		ModelCostTier:          "standard",
		PricingConfigured:      true,
		UsageReported:          true,
		PromptTokens:           13,
		CompletionTokens:       8,
		CostUnits:              21,
		ProviderDurationMillis: 34,
	}

	applied, err := writer.ApplyUsageEvent(context.Background(), event)
	if err != nil || !applied {
		t.Fatalf("ApplyUsageEvent() = (%t, %v), want (true, nil)", applied, err)
	}
	store.applied = false
	if applied, err = writer.ApplyUsageEvent(context.Background(), event); err != nil || applied {
		t.Fatalf("duplicate ApplyUsageEvent() = (%t, %v), want (false, nil)", applied, err)
	}

	labels := prometheus.Labels{
		"namespace": "team-a", "agent": "planner", "workflow": "research-flow", "step": "plan",
		"purpose": "runtime", "pricing_state": "priced", "cost_tier": "standard",
		"attribution_state": "attributed", "usage_state": "reported",
	}
	if got := testutil.ToFloat64(metrics.operations.With(labels)); got != 1 {
		t.Fatalf("operations = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.tokens.With(withTokenType(labels, "prompt"))); got != 13 {
		t.Fatalf("prompt tokens = %v, want 13", got)
	}
	if got := testutil.ToFloat64(metrics.tokens.With(withTokenType(labels, "completion"))); got != 8 {
		t.Fatalf("completion tokens = %v, want 8", got)
	}
	if got := testutil.ToFloat64(metrics.costUnits.With(labels)); got != 21 {
		t.Fatalf("cost units = %v, want 21", got)
	}
	if got := testutil.ToFloat64(metrics.providerDuration.With(labels)); got != 34 {
		t.Fatalf("provider duration = %v, want 34", got)
	}
}

func TestModelUsageMetrics_PreserveUnknownEconomicStates(t *testing.T) {
	metrics := NewModelUsageMetrics(prometheus.NewRegistry())
	writer := metrics.Wrap(&usageEventWriterStub{applied: true})
	event := CostRecord{Workspace: "team-a", OperationType: CostOpModelInference, UsageEstimated: true}

	if _, err := writer.ApplyUsageEvent(context.Background(), event); err != nil {
		t.Fatalf("ApplyUsageEvent(): %v", err)
	}

	labels := prometheus.Labels{
		"namespace": "team-a", "agent": "not_attributed", "workflow": "not_attributed", "step": "not_attributed",
		"purpose": "runtime", "pricing_state": "unpriced", "cost_tier": "not_reported",
		"attribution_state": "unattributed", "usage_state": "estimated",
	}
	if got := testutil.ToFloat64(metrics.operations.With(labels)); got != 1 {
		t.Fatalf("operations = %v, want 1", got)
	}
}

func TestModelUsageMetrics_PreserveExplicitUsageStates(t *testing.T) {
	for _, test := range []struct {
		name  string
		event CostRecord
		want  string
	}{
		{name: "reported zero", event: CostRecord{UsageReported: true}, want: "reported"},
		{name: "estimated", event: CostRecord{UsageEstimated: true}, want: "estimated"},
		{name: "not reported", event: CostRecord{}, want: "not_reported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			metrics := NewModelUsageMetrics(prometheus.NewRegistry())
			writer := metrics.Wrap(&usageEventWriterStub{applied: true})
			test.event.Workspace = "team-a"
			test.event.OperationType = CostOpModelInference

			if _, err := writer.ApplyUsageEvent(context.Background(), test.event); err != nil {
				t.Fatalf("ApplyUsageEvent(): %v", err)
			}

			labels := prometheus.Labels{
				"namespace": "team-a", "agent": "not_attributed", "workflow": "not_attributed", "step": "not_attributed",
				"purpose": "runtime", "pricing_state": "unpriced", "cost_tier": "not_reported",
				"attribution_state": "unattributed", "usage_state": test.want,
			}
			if got := testutil.ToFloat64(metrics.operations.With(labels)); got != 1 {
				t.Fatalf("operations = %v, want 1", got)
			}
		})
	}
}

func TestModelUsageMetrics_SkipsFailedAndNonModelEvents(t *testing.T) {
	metrics := NewModelUsageMetrics(prometheus.NewRegistry())
	failed := metrics.Wrap(&usageEventWriterStub{applied: true, err: errors.New("write failed")})
	event := CostRecord{Workspace: "team-a", OperationType: CostOpModelInference, UsageReported: true}
	if applied, err := failed.ApplyUsageEvent(context.Background(), event); !applied || err == nil {
		t.Fatalf("failed ApplyUsageEvent() = (%t, %v), want (true, error)", applied, err)
	}

	nonModel := metrics.Wrap(&usageEventWriterStub{applied: true})
	event.OperationType = CostOpToolExecution
	if applied, err := nonModel.ApplyUsageEvent(context.Background(), event); !applied || err != nil {
		t.Fatalf("non-model ApplyUsageEvent() = (%t, %v), want (true, nil)", applied, err)
	}

	labels := modelUsageLabels(CostRecord{Workspace: "team-a", UsageReported: true})
	if got := testutil.ToFloat64(metrics.operations.With(labels)); got != 0 {
		t.Fatalf("operations = %v, want 0", got)
	}
}
