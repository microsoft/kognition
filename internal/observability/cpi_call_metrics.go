// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kognition-ai/kognition/internal/cpi"
)

// CPI call outcomes. provider_failure counts failures that reflect the health of
// the provider or its configuration (availability, credentials, quota, rate
// limits); request_failure counts caller-side failures such as invalid requests,
// cancellations and undecodable responses, which should not degrade a model.
const (
	cpiOutcomeSuccess         = "success"
	cpiOutcomeProviderFailure = "provider_failure"
	cpiOutcomeRequestFailure  = "request_failure"
	cpiReasonNone             = "none"
)

// CPICallMetrics counts and times real model calls made through CPI drivers. It
// implements cpi.CallRecorder. Labels are limited to bounded sets: the model is
// deliberately not a label, since Model resources are user-defined.
type CPICallMetrics struct {
	calls    *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

var _ cpi.CallRecorder = (*CPICallMetrics)(nil)

// NewCPICallMetrics registers CPI call counters and a duration histogram.
func NewCPICallMetrics(registerer prometheus.Registerer) *CPICallMetrics {
	metrics := &CPICallMetrics{
		calls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kognition_cpi_calls_total",
			Help: "Completed model calls through CPI drivers by provider, operation, outcome and failure reason.",
		}, []string{"provider", "operation", "outcome", "reason"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kognition_cpi_call_duration_seconds",
			Help:    "Duration of model calls through CPI drivers.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 12),
		}, []string{"provider", "operation", "outcome"}),
	}
	registerer.MustRegister(metrics.calls, metrics.duration)
	return metrics
}

// RecordCall records one finished driver call.
func (m *CPICallMetrics) RecordCall(call cpi.CallOutcome) {
	operation := call.Operation
	if operation != cpi.OperationEmbed {
		operation = cpi.OperationComplete
	}
	outcome, reason := cpiOutcomeSuccess, cpiReasonNone
	if !call.Succeeded() {
		reason = cpi.NormalizeReason(call.Reason)
		outcome = cpiOutcomeRequestFailure
		if call.ProviderFault {
			outcome = cpiOutcomeProviderFailure
		}
	}
	m.calls.WithLabelValues(call.Provider, operation, outcome, reason).Inc()
	m.duration.WithLabelValues(call.Provider, operation, outcome).Observe(call.Duration.Seconds())
}
