// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	metricsOnce sync.Once
	invocations *prometheus.CounterVec
	durations   *prometheus.HistogramVec
	resilience  *prometheus.CounterVec
)

// InitMetrics registers Tool Gateway metrics on the shared controller-runtime
// registry. It is safe for standalone and Kubernetes gateway construction.
func InitMetrics() {
	metricsOnce.Do(func() {
		invocations = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kognition_tool_gateway_invocations_total", Help: "Completed Tool Gateway invocations by outcome."}, []string{"outcome"})
		durations = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "kognition_tool_gateway_invocation_duration_seconds", Help: "Tool Gateway target execution duration.", Buckets: prometheus.ExponentialBuckets(0.01, 2, 12)}, []string{"outcome"})
		resilience = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "kognition_tool_gateway_resilience_events_total", Help: "Tool Gateway resilience decisions by reason."}, []string{"reason"})
		ctrlmetrics.Registry.MustRegister(invocations, durations, resilience)
	})
}

func recordInvocation(outcome string, duration time.Duration) {
	InitMetrics()
	invocations.WithLabelValues(outcome).Inc()
	durations.WithLabelValues(outcome).Observe(duration.Seconds())
}

func recordResilience(reason string) {
	InitMetrics()
	resilience.WithLabelValues(reason).Inc()
}
