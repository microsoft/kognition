// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentgateway

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	metricsOnce sync.Once
	requests    *prometheus.CounterVec
	durations   *prometheus.HistogramVec
)

// InitMetrics registers Agent Communication Gateway metrics on the shared
// controller-runtime registry.
func InitMetrics() {
	metricsOnce.Do(func() {
		requests = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kognition_agent_communication_gateway_requests_total",
			Help: "Completed Agent Communication Gateway requests by operation and outcome.",
		}, []string{"operation", "outcome"})
		durations = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kognition_agent_communication_gateway_request_duration_seconds",
			Help:    "Agent Communication Gateway request duration by operation and outcome.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
		}, []string{"operation", "outcome"})
		ctrlmetrics.Registry.MustRegister(requests, durations)
	})
}

func recordRequest(operation, outcome string, duration time.Duration) {
	InitMetrics()
	requests.WithLabelValues(operation, outcome).Inc()
	durations.WithLabelValues(operation, outcome).Observe(duration.Seconds())
}

func requestOutcome(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "success"
	case status == 401 || status == 403:
		return "denied"
	case status >= 500:
		return "failed"
	default:
		return "rejected"
	}
}
