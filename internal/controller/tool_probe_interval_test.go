// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// A Tool status write wakes the controller again. Non-exec probes must honor the
// configured interval too, or each probe write immediately triggers another
// probe and bumps the Tool resourceVersion that dispatched Agents embed.
func TestToolReconciler_NetworkProbesHonorIntervalOnSelfTriggeredReconcile(t *testing.T) {
	for _, probeType := range []v1alpha1.ProbeType{v1alpha1.ProbeTypeHTTPGet} {
		t.Run(string(probeType), func(t *testing.T) {
			var hits atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			store := newMemStore()
			tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "interval-" + string(probeType), Namespace: "default"}}
			tool.Spec.Protocol = v1alpha1.ToolProtocolMCP
			tool.Spec.Endpoint = server.URL
			tool.Spec.HealthProbe = &v1alpha1.ToolHealthProbe{Type: probeType, Interval: "30s", Timeout: "2s"}
			storeTool(t, store, tool)

			now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
			reconciler := NewToolReconciler(store, zaptest.NewLogger(t), WithToolClock(func() time.Time { return now }))
			reconcile := func() Result {
				t.Helper()
				result, err := reconciler.Reconcile(context.Background(), Request{Kind: "Tool", Namespace: "default", Name: tool.Name})
				if err != nil {
					t.Fatal(err)
				}
				if result.Requeue {
					return reconcile2(t, reconciler, tool.Name)
				}
				return result
			}

			reconcile()
			first := getTool(t, store, "default", tool.Name)
			if hits.Load() == 0 || first.Status.Phase != v1alpha1.PhaseReady {
				t.Fatalf("initial probe hits=%d phase=%s", hits.Load(), first.Status.Phase)
			}
			probed := hits.Load()
			before := first.ObjectMeta.ResourceVersion

			now = now.Add(5 * time.Second)
			result := reconcile()
			if hits.Load() != probed || result.RequeueAfter <= 0 || result.RequeueAfter > 30*time.Second {
				t.Fatalf("self-triggered reconcile probed again: hits=%d (was %d) requeue=%s", hits.Load(), probed, result.RequeueAfter)
			}
			if after := getTool(t, store, "default", tool.Name).ObjectMeta.ResourceVersion; after != before {
				t.Fatalf("debounced reconcile rewrote the Tool: resourceVersion %d -> %d", before, after)
			}

			now = now.Add(30 * time.Second)
			reconcile()
			if hits.Load() <= probed {
				t.Fatal("probe was not repeated after the configured interval elapsed")
			}
		})
	}
}

// advancingClock moves forward by step on every reading, modeling probes that
// are always at least one interval apart.
func advancingClock(step time.Duration) func() time.Time {
	current := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		current = current.Add(step)
		return current
	}
}

func reconcile2(t *testing.T, reconciler *ToolReconciler, name string) Result {
	t.Helper()
	result, err := reconciler.Reconcile(context.Background(), Request{Kind: "Tool", Namespace: "default", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
