// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controller"
)

// advancingProbeClock models health probes that are at least one interval apart.
func advancingProbeClock(step time.Duration) func() time.Time {
	current := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		current = current.Add(step)
		return current
	}
}

// FR-0312: Browser tools use the remote HTTP executor, while Shell Tools
// remain fail-closed unless a class-approved sandbox target is available.
func TestToolsMCP_BrowserReadyAndUnconfiguredShellFailsClosed(t *testing.T) {
	tests := []struct {
		name            string
		protocol        v1alpha1.ToolProtocol
		endpoint        string
		wantPhase       v1alpha1.Phase
		wantAvailable   bool
		wantReadyReason string
	}{
		{name: "browser", protocol: v1alpha1.ToolProtocolBrowser, endpoint: "https://browser.internal", wantPhase: v1alpha1.PhaseReady, wantAvailable: true, wantReadyReason: "Validated"},
		{name: "shell", protocol: v1alpha1.ToolProtocolShell, wantPhase: v1alpha1.PhaseFailed, wantAvailable: false, wantReadyReason: "ExecutorUnavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHarness(t)
			tool := v1alpha1.Tool{
				TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
				ObjectMeta: v1alpha1.ObjectMeta{Name: tc.name + "-protocol", Namespace: testNS},
				Spec:       v1alpha1.ToolSpec{Protocol: tc.protocol, Endpoint: tc.endpoint},
			}
			created := h.doReq(t, http.MethodPost, apiBase+"/tools", tool)
			if created.Code != http.StatusCreated {
				t.Fatalf("create Tool status = %d, body = %s", created.Code, created.Body.String())
			}

			reconciler := controller.NewToolReconciler(h.Store, h.Logger)
			request := controller.Request{Kind: "Tool", Namespace: testNS, Name: tool.Name}
			if result, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("register Tool: %v", err)
			} else if !result.Requeue {
				t.Fatal("registration did not request reconciliation")
			}
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatalf("evaluate Tool: %v", err)
			}

			var got v1alpha1.Tool
			h.getResource(t, "Tool", testNS, tool.Name, &got)
			if got.Status.Phase != tc.wantPhase || got.Status.Available != tc.wantAvailable {
				t.Fatalf("status = phase %s available %t, want %s and %t", got.Status.Phase, got.Status.Available, tc.wantPhase, tc.wantAvailable)
			}
			var ready *v1alpha1.Condition
			for index := range got.Status.Conditions {
				if got.Status.Conditions[index].Type == "Ready" {
					ready = &got.Status.Conditions[index]
					break
				}
			}
			if ready == nil || ready.Reason != tc.wantReadyReason {
				t.Fatalf("Ready condition = %+v, want %s", ready, tc.wantReadyReason)
			}
		})
	}
}

// TC-0168: HTTP health probes apply failure and recovery thresholds to persisted Tool status.
func TestToolsMCP_HTTPHealthProbeDegradesAndRecovers(t *testing.T) {
	h := newTestHarness(t)
	var endpointStatus atomic.Int64
	endpointStatus.Store(http.StatusOK)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			t.Errorf("probe method = %s, want GET", req.Method)
		}
		w.WriteHeader(int(endpointStatus.Load()))
	}))
	defer endpoint.Close()

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "health-probed-tool", Namespace: testNS},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: endpoint.URL,
			Metadata: map[string]string{"allowPrivateNetwork": "true"},
			HealthProbe: &v1alpha1.ToolHealthProbe{
				Type:             v1alpha1.ProbeTypeHTTPGet,
				Interval:         "10s",
				Timeout:          "1s",
				FailureThreshold: 2,
				SuccessThreshold: 2,
			},
		},
	}
	created := h.doReq(t, http.MethodPost, apiBase+"/tools", tool)
	if created.Code != http.StatusCreated {
		t.Fatalf("create Tool status = %d, body = %s", created.Code, created.Body.String())
	}

	reconciler := controller.NewToolReconciler(h.Store, h.Logger, controller.WithToolClock(advancingProbeClock(time.Minute)))
	reconcile := func() controller.Result {
		t.Helper()
		result, err := reconciler.Reconcile(context.Background(), controller.Request{Kind: "Tool", Namespace: testNS, Name: tool.Name})
		if err != nil {
			t.Fatalf("reconcile Tool: %v", err)
		}
		if result.Requeue {
			result, err = reconciler.Reconcile(context.Background(), controller.Request{Kind: "Tool", Namespace: testNS, Name: tool.Name})
			if err != nil {
				t.Fatalf("reconcile Tool after registration: %v", err)
			}
		}
		return result
	}
	getTool := func() v1alpha1.Tool {
		t.Helper()
		var got v1alpha1.Tool
		h.getResource(t, "Tool", testNS, tool.Name, &got)
		return got
	}

	if result := reconcile(); result.RequeueAfter != 10*time.Second {
		t.Fatalf("initial requeueAfter = %s, want 10s", result.RequeueAfter)
	}
	if got := getTool(); got.Status.Phase != v1alpha1.PhaseReady || !got.Status.Available {
		t.Fatalf("initial status = phase %s available %t, want Ready and available", got.Status.Phase, got.Status.Available)
	}

	endpointStatus.Store(http.StatusServiceUnavailable)
	if result := reconcile(); result.RequeueAfter != 10*time.Second {
		t.Fatalf("requeueAfter below failure threshold = %s, want 10s", result.RequeueAfter)
	}
	if got := getTool(); got.Status.Phase != v1alpha1.PhaseReady || !got.Status.Available {
		t.Fatalf("status below failure threshold = phase %s available %t, want Ready and available", got.Status.Phase, got.Status.Available)
	}
	if result := reconcile(); result.RequeueAfter != 10*time.Second {
		t.Fatalf("requeueAfter below recovery threshold = %s, want 10s", result.RequeueAfter)
	}
	degraded := getTool()
	if degraded.Status.Phase != v1alpha1.PhaseDegraded || degraded.Status.Available {
		t.Fatalf("degraded status = phase %s available %t, want Degraded and unavailable", degraded.Status.Phase, degraded.Status.Available)
	}
	if degraded.Annotations["kognition.dev/probe-failures"] != "2" {
		t.Fatalf("persisted probe failures = %q, want 2", degraded.Annotations["kognition.dev/probe-failures"])
	}

	endpointStatus.Store(http.StatusNoContent)
	if result := reconcile(); result.RequeueAfter != 10*time.Second {
		t.Fatalf("requeueAfter after recovery = %s, want 10s", result.RequeueAfter)
	}
	if got := getTool(); got.Status.Phase != v1alpha1.PhaseDegraded || got.Status.Available {
		t.Fatalf("status below recovery threshold = phase %s available %t, want Degraded and unavailable", got.Status.Phase, got.Status.Available)
	}
	reconcile()
	recovered := getTool()
	if recovered.Status.Phase != v1alpha1.PhaseReady || !recovered.Status.Available {
		t.Fatalf("recovered status = phase %s available %t, want Ready and available", recovered.Status.Phase, recovered.Status.Available)
	}
	if _, err := json.Marshal(recovered.Status.CompiledMCP); err != nil {
		t.Fatalf("marshal persisted compiled MCP definition: %v", err)
	}
}

// TC-0168: TCP health probes persist reachability loss through the production reconciler.
func TestToolsMCP_TCPHealthProbeDegradesWhenEndpointCloses(t *testing.T) {
	h := newTestHarness(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "tcp-health-probed-tool", Namespace: testNS},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: "tcp://" + listener.Addr().String(),
			Metadata: map[string]string{"allowPrivateNetwork": "true"},
			HealthProbe: &v1alpha1.ToolHealthProbe{
				Type:             v1alpha1.ProbeTypeTCP,
				Interval:         "10s",
				Timeout:          "1s",
				FailureThreshold: 1,
				SuccessThreshold: 1,
			},
		},
	}
	created := h.doReq(t, http.MethodPost, apiBase+"/tools", tool)
	if created.Code != http.StatusCreated {
		t.Fatalf("create Tool status = %d, body = %s", created.Code, created.Body.String())
	}

	reconciler := controller.NewToolReconciler(h.Store, h.Logger, controller.WithToolClock(advancingProbeClock(time.Minute)))
	reconcile := func() controller.Result {
		t.Helper()
		result, err := reconciler.Reconcile(context.Background(), controller.Request{Kind: "Tool", Namespace: testNS, Name: tool.Name})
		if err != nil {
			t.Fatalf("reconcile Tool: %v", err)
		}
		if result.Requeue {
			result, err = reconciler.Reconcile(context.Background(), controller.Request{Kind: "Tool", Namespace: testNS, Name: tool.Name})
			if err != nil {
				t.Fatalf("reconcile Tool after registration: %v", err)
			}
		}
		return result
	}
	getTool := func() v1alpha1.Tool {
		t.Helper()
		var got v1alpha1.Tool
		h.getResource(t, "Tool", testNS, tool.Name, &got)
		return got
	}

	if result := reconcile(); result.RequeueAfter != 10*time.Second {
		t.Fatalf("initial requeueAfter = %s, want 10s", result.RequeueAfter)
	}
	if got := getTool(); got.Status.Phase != v1alpha1.PhaseReady || !got.Status.Available {
		t.Fatalf("initial status = phase %s available %t, want Ready and available", got.Status.Phase, got.Status.Available)
	}

	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	if result := reconcile(); result.RequeueAfter != 10*time.Second {
		t.Fatalf("failure requeueAfter = %s, want 10s", result.RequeueAfter)
	}
	degraded := getTool()
	if degraded.Status.Phase != v1alpha1.PhaseDegraded || degraded.Status.Available {
		t.Fatalf("degraded status = phase %s available %t, want Degraded and unavailable", degraded.Status.Phase, degraded.Status.Available)
	}
	if degraded.Annotations["kognition.dev/probe-failures"] != "1" {
		t.Fatalf("persisted probe failures = %q, want 1", degraded.Annotations["kognition.dev/probe-failures"])
	}
}

// TC-0168: MCP ping probes persist JSON-RPC health failures through the production reconciler.
func TestToolsMCP_PingHealthProbeDegradesOnRPCError(t *testing.T) {
	h := newTestHarness(t)
	var fail atomic.Bool
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var rpcReq struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(req.Body).Decode(&rpcReq); err != nil {
			t.Errorf("decode MCP ping: %v", err)
		}
		switch rpcReq.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "integration-session")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      rpcReq.ID,
				"result":  map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}},
			})
			return
		case "notifications/initialized":
			if req.Header.Get("Mcp-Session-Id") != "integration-session" {
				t.Errorf("initialized session = %q, want integration-session", req.Header.Get("Mcp-Session-Id"))
			}
			w.WriteHeader(http.StatusAccepted)
			return
		case "ping":
			if req.Header.Get("Mcp-Session-Id") != "integration-session" {
				t.Errorf("ping session = %q, want integration-session", req.Header.Get("Mcp-Session-Id"))
			}
		default:
			t.Errorf("unexpected MCP method %q", rpcReq.Method)
		}
		response := map[string]any{"jsonrpc": "2.0", "id": rpcReq.ID, "result": map[string]any{}}
		if fail.Load() {
			response = map[string]any{"jsonrpc": "2.0", "id": rpcReq.ID, "error": map[string]any{"code": -32603, "message": "unhealthy"}}
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer endpoint.Close()

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "mcp-ping-health-probed-tool", Namespace: testNS},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: endpoint.URL,
			Metadata: map[string]string{"allowPrivateNetwork": "true"},
			HealthProbe: &v1alpha1.ToolHealthProbe{
				Type:             v1alpha1.ProbeTypeMCPPing,
				Interval:         "10s",
				Timeout:          "1s",
				FailureThreshold: 1,
			},
		},
	}
	created := h.doReq(t, http.MethodPost, apiBase+"/tools", tool)
	if created.Code != http.StatusCreated {
		t.Fatalf("create Tool status = %d, body = %s", created.Code, created.Body.String())
	}

	reconciler := controller.NewToolReconciler(h.Store, h.Logger, controller.WithToolClock(advancingProbeClock(time.Minute)))
	reconcile := func() controller.Result {
		t.Helper()
		result, err := reconciler.Reconcile(context.Background(), controller.Request{Kind: "Tool", Namespace: testNS, Name: tool.Name})
		if err != nil {
			t.Fatalf("reconcile Tool: %v", err)
		}
		if result.Requeue {
			result, err = reconciler.Reconcile(context.Background(), controller.Request{Kind: "Tool", Namespace: testNS, Name: tool.Name})
			if err != nil {
				t.Fatalf("reconcile Tool after registration: %v", err)
			}
		}
		return result
	}
	getTool := func() v1alpha1.Tool {
		t.Helper()
		var got v1alpha1.Tool
		h.getResource(t, "Tool", testNS, tool.Name, &got)
		return got
	}

	if result := reconcile(); result.RequeueAfter != 10*time.Second {
		t.Fatalf("initial requeueAfter = %s, want 10s", result.RequeueAfter)
	}
	if got := getTool(); got.Status.Phase != v1alpha1.PhaseReady || !got.Status.Available {
		t.Fatalf("initial status = phase %s available %t, want Ready and available", got.Status.Phase, got.Status.Available)
	}

	fail.Store(true)
	if result := reconcile(); result.RequeueAfter != 10*time.Second {
		t.Fatalf("failure requeueAfter = %s, want 10s", result.RequeueAfter)
	}
	degraded := getTool()
	if degraded.Status.Phase != v1alpha1.PhaseDegraded || degraded.Status.Available {
		t.Fatalf("degraded status = phase %s available %t, want Degraded and unavailable", degraded.Status.Phase, degraded.Status.Available)
	}
	if degraded.Annotations["kognition.dev/probe-failures"] != "1" {
		t.Fatalf("persisted probe failures = %q, want 1", degraded.Annotations["kognition.dev/probe-failures"])
	}
}
