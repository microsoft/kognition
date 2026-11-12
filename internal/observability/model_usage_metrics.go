// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	notAttributedLabel  = "not_attributed"
	notReportedLabel    = "not_reported"
	runtimePurposeLabel = "runtime"
)

var modelUsageLabelNames = []string{
	"namespace",
	"agent",
	"workflow",
	"step",
	"purpose",
	"pricing_state",
	"cost_tier",
	"attribution_state",
	"usage_state",
}

// ModelUsageMetrics records economic totals only after an immutable model
// usage event has been durably applied.
type ModelUsageMetrics struct {
	operations       *prometheus.CounterVec
	tokens           *prometheus.CounterVec
	costUnits        *prometheus.CounterVec
	providerDuration *prometheus.CounterVec
}

// NewModelUsageMetrics registers durable model economic counters.
func NewModelUsageMetrics(registerer prometheus.Registerer) *ModelUsageMetrics {
	metrics := &ModelUsageMetrics{
		operations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kognition_model_usage_operations_total",
			Help: "Durably applied model usage operations.",
		}, modelUsageLabelNames),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kognition_model_usage_tokens_total",
			Help: "Durably applied model usage tokens by token type.",
		}, append(modelUsageLabelNames, "token_type")),
		costUnits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kognition_model_usage_cost_units_total",
			Help: "Durably applied model usage cost in normalized micro-dollar units.",
		}, modelUsageLabelNames),
		providerDuration: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kognition_model_usage_provider_duration_milliseconds_total",
			Help: "Durably applied model provider duration in milliseconds.",
		}, modelUsageLabelNames),
	}
	registerer.MustRegister(metrics.operations, metrics.tokens, metrics.costUnits, metrics.providerDuration)
	return metrics
}

// Wrap records metrics for newly applied events written by writer.
func (m *ModelUsageMetrics) Wrap(writer UsageEventWriter) UsageEventWriter {
	return &meteredUsageEventWriter{writer: writer, metrics: m}
}

type meteredUsageEventWriter struct {
	writer  UsageEventWriter
	metrics *ModelUsageMetrics
}

func (w *meteredUsageEventWriter) ApplyUsageEvent(ctx context.Context, event CostRecord) (bool, error) {
	applied, err := w.writer.ApplyUsageEvent(ctx, event)
	if err != nil || !applied || event.OperationType != CostOpModelInference {
		return applied, err
	}

	labels := modelUsageLabels(event)
	w.metrics.operations.With(labels).Inc()
	w.metrics.tokens.With(withTokenType(labels, "prompt")).Add(float64(event.PromptTokens))
	w.metrics.tokens.With(withTokenType(labels, "completion")).Add(float64(event.CompletionTokens))
	w.metrics.costUnits.With(labels).Add(float64(event.CostUnits))
	w.metrics.providerDuration.With(labels).Add(float64(event.ProviderDurationMillis))
	return true, nil
}

func modelUsageLabels(event CostRecord) prometheus.Labels {
	pricingState := "unpriced"
	if event.PricingConfigured {
		pricingState = "priced"
	}
	attributionState := "unattributed"
	if event.Agent != "" {
		attributionState = "attributed"
	}
	usageState := notReportedLabel
	if event.UsageReported {
		usageState = "reported"
	}
	if event.UsageEstimated {
		usageState = "estimated"
	}
	purpose := string(event.Purpose)
	if purpose == "" {
		purpose = runtimePurposeLabel
	}
	return prometheus.Labels{
		"namespace":         event.Workspace,
		"agent":             labelOr(event.Agent, notAttributedLabel),
		"workflow":          labelOr(event.Workflow, notAttributedLabel),
		"step":              labelOr(event.Step, notAttributedLabel),
		"purpose":           purpose,
		"pricing_state":     pricingState,
		"cost_tier":         labelOr(event.ModelCostTier, notReportedLabel),
		"attribution_state": attributionState,
		"usage_state":       usageState,
	}
}

func withTokenType(labels prometheus.Labels, tokenType string) prometheus.Labels {
	withType := make(prometheus.Labels, len(labels)+1)
	for name, value := range labels {
		withType[name] = value
	}
	withType["token_type"] = tokenType
	return withType
}

func labelOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
