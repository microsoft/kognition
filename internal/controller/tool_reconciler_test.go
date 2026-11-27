// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type toolClassUpdateCountingStore struct {
	*memStore
	updates atomic.Int32
}

type concurrentAgentUpdateStore struct {
	*memStore
	conflicted bool
	wrapError  bool
}

type wrappedToolGetErrorStore struct {
	*memStore
	err error
}

func (s *wrappedToolGetErrorStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == toolKind {
		return nil, fmt.Errorf("wrapped get error: %w", s.err)
	}
	return s.memStore.Get(ctx, kind, namespace, name)
}

func (s *concurrentAgentUpdateStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "Agent" && !s.conflicted {
		raw, err := s.memStore.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var latest v1alpha1.Agent
		if err := json.Unmarshal(raw, &latest); err != nil {
			return err
		}
		latest.ObjectMeta.Labels = map[string]string{"concurrent": "preserved"}
		latest.Spec.Instructions.Inline = "concurrently updated"
		latest.Status.Phase = v1alpha1.PhaseRunning
		latest.Status.Replicas = 3
		latest.Status.ReadyReplicas = 2
		latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{
			Type: "ConcurrentReady", Status: "True", Reason: "ConcurrentUpdate",
		})
		updated, err := json.Marshal(latest)
		if err != nil {
			return err
		}
		if err := s.memStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
			return err
		}
		s.conflicted = true
		if s.wrapError {
			return fmt.Errorf("wrapped update conflict: %w", store.ErrConflict)
		}
		return store.ErrConflict
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *toolClassUpdateCountingStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == toolClassKind {
		s.updates.Add(1)
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func TestToolProbeAnnotationIntegerBounds(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int32
	}{
		{value: "0", want: 0}, {value: "42", want: 42},
		{value: "2147483647", want: 2147483647}, {value: "-2147483648", want: -2147483648},
		{value: "2147483648", want: 0}, {value: "-2147483649", want: 0},
		{value: "4294967297", want: 0}, {value: "9223372036854775808", want: 0},
		{value: "invalid", want: 0}, {value: "", want: 0},
	} {
		t.Run(test.value, func(t *testing.T) {
			if got := getAnnotationInt32(map[string]string{"counter": test.value}, "counter"); got != test.want {
				t.Fatalf("parsed counter = %d, want %d", got, test.want)
			}
		})
	}
	if got := getAnnotationInt32(nil, "counter"); got != 0 {
		t.Fatalf("missing counter = %d, want 0", got)
	}
}

func TestToolProbeCountersDoNotOverflow(t *testing.T) {
	for _, passes := range []bool{false, true} {
		t.Run(fmt.Sprintf("passes-%t", passes), func(t *testing.T) {
			tool := v1alpha1.Tool{
				ObjectMeta: v1alpha1.ObjectMeta{Annotations: map[string]string{
					annotationProbeFailures: "2147483647", annotationProbeSuccesses: "2147483647",
				}},
				Spec: v1alpha1.ToolSpec{HealthProbe: &v1alpha1.ToolHealthProbe{
					FailureThreshold: 2147483647, SuccessThreshold: 2147483647,
				}},
				Status: v1alpha1.ToolStatus{Phase: v1alpha1.PhaseDegraded},
			}
			NewToolReconciler(newMemStore(), zaptest.NewLogger(t)).applyHealthProbe(&tool, passes, v1alpha1.PhaseDegraded, false)
			counter, reset := annotationProbeFailures, annotationProbeSuccesses
			wantPhase := v1alpha1.PhaseDegraded
			if passes {
				counter, reset = reset, counter
				wantPhase = v1alpha1.PhaseReady
			}
			if tool.ObjectMeta.Annotations[counter] != "2147483647" || tool.ObjectMeta.Annotations[reset] != "0" ||
				tool.Status.Phase != wantPhase || tool.Status.Available != passes {
				t.Fatalf("counter overflow changed probe thresholds: annotations=%v status=%#v", tool.ObjectMeta.Annotations, tool.Status)
			}
		})
	}
}

func storeTool(t *testing.T, s *memStore, tool v1alpha1.Tool) {
	t.Helper()
	if parsed, err := url.Parse(tool.Spec.Endpoint); err == nil {
		host := parsed.Hostname()
		if host == "localhost" || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".test") || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback() {
			if tool.Spec.Metadata == nil {
				tool.Spec.Metadata = map[string]string{}
			}
			tool.Spec.Metadata["allowPrivateNetwork"] = "true"
		}
	}
	data, _ := json.Marshal(tool)
	if err := s.Create(context.Background(), "Tool", tool.ObjectMeta.Namespace, tool.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store Tool: %v", err)
	}
}

func storeToolClass(t *testing.T, s *memStore, class v1alpha1.ToolClass) {
	t.Helper()
	data, err := json.Marshal(class)
	if err != nil {
		t.Fatalf("marshal ToolClass: %v", err)
	}
	if err := s.Create(context.Background(), toolClassKind, "", class.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store ToolClass: %v", err)
	}
}

func getTool(t *testing.T, s *memStore, ns, name string) v1alpha1.Tool {
	t.Helper()
	raw, err := s.Get(context.Background(), "Tool", ns, name)
	if err != nil {
		t.Fatalf("get Tool %s/%s: %v", ns, name, err)
	}
	var tool v1alpha1.Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		t.Fatalf("unmarshal Tool: %v", err)
	}
	return tool
}

func reconcileTool(t *testing.T, r *ToolReconciler, ns, name string) {
	t.Helper()
	// First reconcile may add finalizer and requeue; reconcile again to complete lifecycle.
	res, err := r.Reconcile(context.Background(), Request{Namespace: ns, Name: name, Kind: "Tool"})
	if err != nil {
		t.Fatalf("reconcile Tool: %v", err)
	}
	if res.Requeue {
		if _, err := r.Reconcile(context.Background(), Request{Namespace: ns, Name: name, Kind: "Tool"}); err != nil {
			t.Fatalf("reconcile Tool (requeue): %v", err)
		}
	}
}

func findToolCondition(tool v1alpha1.Tool, condType string) *v1alpha1.Condition {
	for i := range tool.Status.Conditions {
		if tool.Status.Conditions[i].Type == condType {
			return &tool.Status.Conditions[i]
		}
	}
	return nil
}

func updateTool(t *testing.T, s *memStore, tool v1alpha1.Tool) {
	t.Helper()
	data, _ := json.Marshal(tool)
	if err := s.Update(context.Background(), "Tool", tool.ObjectMeta.Namespace, tool.ObjectMeta.Name, data, tool.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update Tool: %v", err)
	}
}

// TC-0047: MCP tool health and discoverability state.
func TestToolReconciler_TC0047_MCPToolBecomesAvailable(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "web-search", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: "https://tools.internal/web-search",
			Schema:   map[string]any{"type": "object"},
		},
	}

	storeTool(t, s, tool)
	reconcileTool(t, r, "default", "web-search")

	got := getTool(t, s, "default", "web-search")
	if !got.Status.Available {
		t.Fatal("expected Tool to be available")
	}
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.ResolvedProtocol != v1alpha1.ToolProtocolMCP {
		t.Fatalf("resolvedProtocol = %q, want MCP", got.Status.ResolvedProtocol)
	}
	if got.Status.LastHealthCheck == "" {
		t.Fatal("expected LastHealthCheck to be set")
	}
	if _, err := time.Parse(time.RFC3339, got.Status.LastHealthCheck); err != nil {
		t.Fatalf("LastHealthCheck is not RFC3339: %v", err)
	}
	cond := findToolCondition(got, "Ready")
	if cond == nil || cond.Status != "True" {
		t.Fatalf("expected Ready=True condition, got %+v", cond)
	}
	// Verify MCP compilation produced a definition.
	if got.Status.CompiledMCP == nil {
		t.Fatal("expected CompiledMCP to be populated after successful reconciliation")
	}
	if got.Status.CompiledMCP.Name != "web-search" {
		t.Errorf("CompiledMCP.Name = %q, want %q", got.Status.CompiledMCP.Name, "web-search")
	}
	if got.Status.CompiledMCP.Transport != v1alpha1.TransportHTTP {
		t.Errorf("CompiledMCP.Transport = %q, want %q", got.Status.CompiledMCP.Transport, v1alpha1.TransportHTTP)
	}
	if got.Status.CompiledMCP.Endpoint != "https://tools.internal/web-search" {
		t.Errorf("CompiledMCP.Endpoint = %q, want %q", got.Status.CompiledMCP.Endpoint, "https://tools.internal/web-search")
	}
}

// TC-0048: Digest pin mismatch after endpoint drift marks tool unhealthy.
func TestToolReconciler_TC0048_DigestPinMismatchAfterEndpointChange(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))
	var probeCalls atomic.Int64
	r.probeClient = probeClientFunc(func(*http.Request) (*http.Response, error) {
		probeCalls.Add(1)
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})

	initialEndpoint := "https://tools.internal/weather/v1"
	newEndpoint := "https://tools.internal/weather/v2"
	pin := computeObservedDigest(v1alpha1.ToolProtocolMCP, initialEndpoint, map[string]any{"type": "object"})

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "weather", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol:  v1alpha1.ToolProtocolMCP,
			Endpoint:  initialEndpoint,
			Schema:    map[string]any{"type": "object"},
			DigestPin: pin,
			HealthProbe: &v1alpha1.ToolHealthProbe{
				Type:             v1alpha1.ProbeTypeHTTPGet,
				FailureThreshold: 2,
			},
		},
	}

	storeTool(t, s, tool)
	reconcileTool(t, r, "default", "weather")

	stable := getTool(t, s, "default", "weather")
	if !stable.Status.Available {
		t.Fatal("expected initial reconcile to be available")
	}
	if probeCalls.Load() != 1 {
		t.Fatalf("probe calls after valid digest = %d, want 1", probeCalls.Load())
	}

	stable.Spec.Endpoint = newEndpoint
	data, _ := json.Marshal(stable)
	if err := s.Update(context.Background(), "Tool", "default", "weather", data, stable.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update tool endpoint: %v", err)
	}

	reconcileTool(t, r, "default", "weather")
	changed := getTool(t, s, "default", "weather")
	if changed.Status.Available {
		t.Fatal("expected tool to be unavailable after digest mismatch")
	}
	if changed.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", changed.Status.Phase)
	}
	cond := findToolCondition(changed, "Ready")
	if cond == nil {
		t.Fatal("expected Ready condition")
	}
	if cond.Reason != "DigestMismatch" {
		t.Fatalf("ready condition reason = %q, want DigestMismatch", cond.Reason)
	}
	if probeCalls.Load() != 1 {
		t.Fatalf("probe calls after digest mismatch = %d, want validation to skip the probe", probeCalls.Load())
	}
}

// TC-0049: OpenAPI tools are adapted to MCP internally.
func TestToolReconciler_TC0049_OpenAPIAdaptsToMCP(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "ticketing", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolOpenAPI,
			Endpoint: "https://tools.internal/ticketing",
			Schema: map[string]any{
				"openapi": "3.1.0",
				"paths": map[string]any{
					"/tickets": map[string]any{"get": map[string]any{}},
				},
			},
		},
	}

	storeTool(t, s, tool)
	reconcileTool(t, r, "default", "ticketing")

	got := getTool(t, s, "default", "ticketing")
	if !got.Status.Available {
		t.Fatal("expected OpenAPI tool to be available after adaptation")
	}
	if got.Status.ResolvedProtocol != v1alpha1.ToolProtocolMCP {
		t.Fatalf("resolvedProtocol = %q, want MCP", got.Status.ResolvedProtocol)
	}
	cond := findToolCondition(got, "Ready")
	if cond == nil {
		t.Fatal("expected Ready condition")
	}
	if cond.Reason != "AdaptedToMCP" {
		t.Fatalf("ready condition reason = %q, want AdaptedToMCP", cond.Reason)
	}
}

// TC-0163: ToolClass with executionTier=sidecar validates correctly, Tool references it.
func TestToolReconciler_TC0163_ToolClassSidecarValidation(t *testing.T) {
	tc := v1alpha1.ToolClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "mcp-sidecar", Namespace: "default"},
		Spec: v1alpha1.ToolClassSpec{
			Provisioner:   "mcp",
			ExecutionTier: v1alpha1.ExecutionTierSidecar,
			Transport:     v1alpha1.TransportSSE,
			SandboxDefaults: &v1alpha1.ToolSandboxSpec{
				Timeout:  "30s",
				CPUMilli: 500,
				MemoryMB: 256,
			},
			ResourceLimits: &v1alpha1.ToolClassResourceLimits{
				MaxCPUMilli: 1000,
				MaxMemoryMB: 512,
				MaxTimeout:  "60s",
			},
		},
	}

	// Validate ToolClass spec
	if err := tc.Spec.Validate(); err != nil {
		t.Fatalf("ToolClassSpec.Validate() unexpected error: %v", err)
	}

	// Validate invalid executionTier
	badTC := tc
	badTC.Spec.ExecutionTier = "invalid"
	if err := badTC.Spec.Validate(); err == nil {
		t.Fatal("expected error for invalid executionTier")
	}

	// Validate invalid transport
	badTC2 := tc
	badTC2.Spec.ExecutionTier = v1alpha1.ExecutionTierSidecar
	badTC2.Spec.Transport = "grpc"
	if err := badTC2.Spec.Validate(); err == nil {
		t.Fatal("expected error for invalid transport")
	}

	// Tool referencing this ToolClass reconciles to Ready
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "search-tool", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol:  v1alpha1.ToolProtocolMCP,
			ClassName: "mcp-sidecar",
			Endpoint:  "https://tools.internal/search",
			Schema:    map[string]any{"type": "object"},
		},
	}

	storeTool(t, s, tool)
	reconcileTool(t, r, "default", "search-tool")

	got := getTool(t, s, "default", "search-tool")
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", got.Status.Phase)
	}
	if !got.Status.Available {
		t.Fatal("expected tool to be available")
	}
}

func TestToolReconciler_ToolClassMissingProvisionerFails(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))
	tc := v1alpha1.ToolClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "missing-provisioner"},
		Spec:       v1alpha1.ToolClassSpec{Provisioner: " "},
	}
	data, _ := json.Marshal(tc)
	if err := s.Create(context.Background(), "ToolClass", "", tc.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store ToolClass: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), Request{Name: tc.ObjectMeta.Name, Kind: "ToolClass"}); err != nil {
		t.Fatalf("reconcile ToolClass: %v", err)
	}
	raw, err := s.Get(context.Background(), "ToolClass", "", tc.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get ToolClass: %v", err)
	}
	var stored v1alpha1.ToolClass
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal ToolClass: %v", err)
	}
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want %q", stored.Status.Phase, v1alpha1.PhaseFailed)
	}
	if len(stored.Status.Conditions) != 1 || stored.Status.Conditions[0].Type != "Ready" ||
		stored.Status.Conditions[0].Status != "False" || stored.Status.Conditions[0].Reason != "ValidationFailed" {
		t.Fatalf("conditions = %#v, want Ready=False/ValidationFailed", stored.Status.Conditions)
	}
}

func TestToolReconciler_ValidToolClassBecomesReadyOnce(t *testing.T) {
	baseStore := newMemStore()
	s := &toolClassUpdateCountingStore{memStore: baseStore}
	r := NewToolReconciler(s, zaptest.NewLogger(t))
	tc := v1alpha1.ToolClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "mcp-sidecar"},
		Spec: v1alpha1.ToolClassSpec{
			Provisioner:   "mcp",
			ExecutionTier: v1alpha1.ExecutionTierSidecar,
			Transport:     v1alpha1.TransportSSE,
		},
	}
	data, _ := json.Marshal(tc)
	if err := baseStore.Create(context.Background(), toolClassKind, "", tc.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store ToolClass: %v", err)
	}
	req := Request{Name: tc.ObjectMeta.Name, Kind: toolClassKind}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile ToolClass: %v", err)
	}
	raw, err := baseStore.Get(context.Background(), toolClassKind, "", tc.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get ToolClass: %v", err)
	}
	var stored v1alpha1.ToolClass
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal ToolClass: %v", err)
	}
	if stored.Status.Phase != v1alpha1.PhaseReady || stored.Status.Message != "tool class spec validated" {
		t.Fatalf("status = %#v, want Ready with validation message", stored.Status)
	}
	if len(stored.Status.Conditions) != 1 || stored.Status.Conditions[0].Type != "Ready" ||
		stored.Status.Conditions[0].Status != "True" || stored.Status.Conditions[0].Reason != "Validated" {
		t.Fatalf("conditions = %#v, want Ready=True/Validated", stored.Status.Conditions)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile unchanged ToolClass: %v", err)
	}
	if updates := s.updates.Load(); updates != 1 {
		t.Fatalf("ToolClass update attempts = %d, want 1", updates)
	}
}

// TC-0164: Tool lifecycle state machine: Registered → Validating → Ready, then delete → Terminated with finalizer.
func TestToolReconciler_TC0164_ToolLifecycleStateMachine(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "lifecycle-tool", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: "https://tools.internal/lifecycle",
			Schema:   map[string]any{"type": "object"},
		},
	}

	storeTool(t, s, tool)

	// First reconcile: adds finalizer, sets Registered phase.
	res, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "lifecycle-tool", Kind: "Tool"})
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected Requeue after adding finalizer")
	}

	registered := getTool(t, s, "default", "lifecycle-tool")
	if registered.Status.Phase != v1alpha1.PhaseRegistered {
		t.Fatalf("phase = %q, want Registered", registered.Status.Phase)
	}
	if len(registered.ObjectMeta.Finalizers) == 0 {
		t.Fatal("expected finalizer to be added")
	}
	if registered.ObjectMeta.Finalizers[0] != "kognition.dev/tool-cleanup" {
		t.Fatalf("finalizer = %q, want kognition.dev/tool-cleanup", registered.ObjectMeta.Finalizers[0])
	}

	// Second reconcile: validates and sets Ready.
	_, err = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "lifecycle-tool", Kind: "Tool"})
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	ready := getTool(t, s, "default", "lifecycle-tool")
	if ready.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", ready.Status.Phase)
	}
	if !ready.Status.Available {
		t.Fatal("expected tool to be available")
	}

	// Simulate deletion by setting DeletionTimestamp.
	now := time.Now()
	ready.ObjectMeta.DeletionTimestamp = &now
	data, _ := json.Marshal(ready)
	if err := s.Update(context.Background(), "Tool", "default", "lifecycle-tool", data, ready.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update tool for deletion: %v", err)
	}

	// Third reconcile: handles deletion → Terminated, removes finalizer.
	_, err = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "lifecycle-tool", Kind: "Tool"})
	if err != nil {
		t.Fatalf("delete reconcile: %v", err)
	}

	terminated := getTool(t, s, "default", "lifecycle-tool")
	if terminated.Status.Phase != v1alpha1.PhaseTerminated {
		t.Fatalf("phase = %q, want Terminated", terminated.Status.Phase)
	}
	if len(terminated.ObjectMeta.Finalizers) != 0 {
		t.Fatalf("expected finalizers to be removed, got %v", terminated.ObjectMeta.Finalizers)
	}
	cond := findToolCondition(terminated, "Ready")
	if cond == nil || cond.Status != "False" || cond.Reason != "Terminated" {
		t.Fatalf("expected Ready=False/Terminated condition, got %+v", cond)
	}
}

// Test: Empty endpoint results in PhaseFailed with EndpointMissing.
func TestToolReconciler_EndpointMissing(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "no-endpoint", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: "",
			Schema:   map[string]any{"type": "object"},
		},
	}

	storeTool(t, s, tool)
	reconcileTool(t, r, "default", "no-endpoint")

	got := getTool(t, s, "default", "no-endpoint")
	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	cond := findToolCondition(got, "Ready")
	if cond == nil || cond.Reason != "EndpointMissing" {
		t.Fatalf("expected Ready condition with Reason=EndpointMissing, got %+v", cond)
	}
}

// Test: Shell protocol remains unavailable until a production stdio executor is wired.
func TestToolReconciler_ShellStdioFailsClosedWithoutExecutor(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "shell-tool", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolShell,
			Endpoint: "",
		},
	}

	storeTool(t, s, tool)
	reconcileTool(t, r, "default", "shell-tool")

	got := getTool(t, s, "default", "shell-tool")
	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if got.Status.Available {
		t.Fatal("expected shell tool to be unavailable")
	}
	cond := findToolCondition(got, "Ready")
	if cond == nil || cond.Reason != "ExecutorUnavailable" {
		t.Fatalf("expected Ready=False/ExecutorUnavailable, got %+v", cond)
	}
}

func TestToolReconciler_ShellWithClassOwnedStdioLauncherIsReady(t *testing.T) {
	store := newMemStore()
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "default"}, Spec: v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolShell, ClassName: "shell-class"}}
	storeTool(t, store, tool)
	storeToolClass(t, store, v1alpha1.ToolClass{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell-class"}, Spec: v1alpha1.ToolClassSpec{ExecutionTier: v1alpha1.ExecutionTierPod, SandboxImage: "registry.example/shell:1", StdioCommand: []string{"/srv/mcp", "--stdio"}}, Status: v1alpha1.ToolClassStatus{Phase: v1alpha1.PhaseReady}})
	reconcileTool(t, NewToolReconciler(store, zaptest.NewLogger(t)), "default", "shell")
	got := getTool(t, store, "default", "shell")
	if got.Status.Phase != v1alpha1.PhaseReady || !got.Status.Available || got.Status.CompiledMCP == nil || got.Status.CompiledMCP.Transport != v1alpha1.TransportStdio {
		t.Fatalf("class-backed shell status = %#v", got.Status)
	}
}

func TestToolReconciler_BrowserEndpointIsReadyForRemoteExecutor(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))
	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "browser-tool", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolBrowser,
			Endpoint: "https://browser.internal",
		},
	}

	storeTool(t, s, tool)
	reconcileTool(t, r, "default", tool.Name)

	got := getTool(t, s, "default", tool.Name)
	if got.Status.Phase != v1alpha1.PhaseReady || !got.Status.Available {
		t.Fatalf("browser status = phase %q available %t, want Ready and available", got.Status.Phase, got.Status.Available)
	}
	if got.Status.CompiledMCP == nil || got.Status.CompiledMCP.Transport != v1alpha1.TransportHTTP {
		t.Fatalf("browser compiled definition = %#v, want HTTP transport", got.Status.CompiledMCP)
	}
}

func TestToolReconciler_UnsupportedProtocolClearsCompiledDefinition(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))
	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "protocol-change", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: "https://tools.internal/mcp",
		},
	}

	storeTool(t, s, tool)
	reconcileTool(t, r, "default", tool.Name)
	ready := getTool(t, s, "default", tool.Name)
	if ready.Status.CompiledMCP == nil {
		t.Fatal("expected ready MCP tool to have a compiled definition")
	}

	ready.Spec.Protocol = v1alpha1.ToolProtocolShell
	ready.Spec.Endpoint = ""
	updateTool(t, s, ready)
	reconcileTool(t, r, "default", tool.Name)

	failed := getTool(t, s, "default", tool.Name)
	if failed.Status.Phase != v1alpha1.PhaseFailed || failed.Status.Available {
		t.Fatalf("status = phase %q available %t, want Failed and unavailable", failed.Status.Phase, failed.Status.Available)
	}
	if failed.Status.CompiledMCP != nil {
		t.Fatalf("CompiledMCP = %+v, want nil after protocol becomes unavailable", failed.Status.CompiledMCP)
	}
	if failed.Status.ObservedSpecDigest == "" {
		t.Fatal("expected failed terminal state to record the complete observed spec digest")
	}

	failedVersion := failed.ObjectMeta.ResourceVersion
	reconcileTool(t, r, "default", tool.Name)
	stable := getTool(t, s, "default", tool.Name)
	if stable.ObjectMeta.ResourceVersion != failedVersion {
		t.Fatalf("resourceVersion = %d after stable reconcile, want %d", stable.ObjectMeta.ResourceVersion, failedVersion)
	}
}

func TestToolReconciler_DescriptionChangeRebuildsCompiledDefinition(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))
	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "description-change", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol:    v1alpha1.ToolProtocolMCP,
			Description: "old description",
			Endpoint:    "https://tools.internal/mcp",
		},
	}

	storeTool(t, s, tool)
	reconcileTool(t, r, "default", tool.Name)
	ready := getTool(t, s, "default", tool.Name)
	ready.Spec.Description = "new description"
	updateTool(t, s, ready)
	reconcileTool(t, r, "default", tool.Name)

	updated := getTool(t, s, "default", tool.Name)
	if updated.Status.CompiledMCP == nil || updated.Status.CompiledMCP.Description != "new description" {
		t.Fatalf("CompiledMCP = %+v, want rebuilt definition with new description", updated.Status.CompiledMCP)
	}
	if updated.Status.ObservedSpecDigest == ready.Status.ObservedSpecDigest {
		t.Fatal("expected complete observed spec digest to change")
	}
}

func TestToolReconciler_ValidationFailureIsIdempotent(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))
	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "invalid-stable", Namespace: "default"},
		Spec:       v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolMCP},
	}

	storeTool(t, s, tool)
	reconcileTool(t, r, "default", tool.Name)
	failed := getTool(t, s, "default", tool.Name)
	if failed.Status.Phase != v1alpha1.PhaseFailed || failed.Status.ObservedSpecDigest == "" {
		t.Fatalf("failed status = %+v, want terminal state with observed spec digest", failed.Status)
	}
	failedVersion := failed.ObjectMeta.ResourceVersion
	reconcileTool(t, r, "default", tool.Name)
	stable := getTool(t, s, "default", tool.Name)
	if stable.ObjectMeta.ResourceVersion != failedVersion {
		t.Fatalf("resourceVersion = %d after stable reconcile, want %d", stable.ObjectMeta.ResourceVersion, failedVersion)
	}
}

// Test: Invalid URL endpoint results in PhaseFailed with EndpointInvalid.
func TestToolReconciler_EndpointInvalid(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "bad-url", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: "not-a-url",
			Schema:   map[string]any{"type": "object"},
		},
	}

	storeTool(t, s, tool)
	reconcileTool(t, r, "default", "bad-url")

	got := getTool(t, s, "default", "bad-url")
	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	cond := findToolCondition(got, "Ready")
	if cond == nil || cond.Reason != "EndpointInvalid" {
		t.Fatalf("expected Ready condition with Reason=EndpointInvalid, got %+v", cond)
	}
}

// Test: OpenAPI tool without schema results in PhaseFailed with OpenAPISchemaMissing.
func TestToolReconciler_OpenAPISchemaMissing(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "no-schema", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolOpenAPI,
			Endpoint: "https://tools.internal/no-schema",
		},
	}

	storeTool(t, s, tool)
	reconcileTool(t, r, "default", "no-schema")

	got := getTool(t, s, "default", "no-schema")
	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	cond := findToolCondition(got, "Ready")
	if cond == nil || cond.Reason != "OpenAPISchemaMissing" {
		t.Fatalf("expected Ready condition with Reason=OpenAPISchemaMissing, got %+v", cond)
	}
}

// Test: Progressing condition is True during registration and False after Ready.
func TestToolReconciler_ProgressingConditionLifecycle(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "progressing-test", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: "https://tools.internal/progressing",
			Schema:   map[string]any{"type": "object"},
		},
	}

	storeTool(t, s, tool)

	// First reconcile: adds finalizer, sets Registered + Progressing=True.
	res, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "progressing-test", Kind: "Tool"})
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after registration")
	}

	registered := getTool(t, s, "default", "progressing-test")
	progCond := findToolCondition(registered, "Progressing")
	if progCond == nil || progCond.Status != "True" {
		t.Fatalf("expected Progressing=True after registration, got %+v", progCond)
	}

	// Second reconcile: evaluates → Ready, Progressing should be False.
	_, err = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "progressing-test", Kind: "Tool"})
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	ready := getTool(t, s, "default", "progressing-test")
	if ready.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", ready.Status.Phase)
	}
	progCond = findToolCondition(ready, "Progressing")
	if progCond == nil || progCond.Status != "False" {
		t.Fatalf("expected Progressing=False after Ready, got %+v", progCond)
	}
}

// Test: Re-reconcile an already-Ready tool is idempotent.
func TestToolReconciler_IdempotentReReconcile(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "idempotent", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: "https://tools.internal/idempotent",
			Schema:   map[string]any{"type": "object"},
		},
	}

	storeTool(t, s, tool)
	reconcileTool(t, r, "default", "idempotent")

	first := getTool(t, s, "default", "idempotent")
	if first.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", first.Status.Phase)
	}
	firstDigest := first.Status.ObservedDigest

	// Reconcile again — should stay Ready with same digest.
	reconcileTool(t, r, "default", "idempotent")

	second := getTool(t, s, "default", "idempotent")
	if second.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase after re-reconcile = %q, want Ready", second.Status.Phase)
	}
	if second.Status.ObservedDigest != firstDigest {
		t.Fatalf("digest changed: %q → %q", firstDigest, second.Status.ObservedDigest)
	}
	if !second.Status.Available {
		t.Fatal("expected tool to remain available")
	}
}

// TC-0167: Agent with required tool ref gets ToolsReady condition from tool reconciler.
func TestToolReconciler_TC0167_AgentToolConditionGating(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))

	// Create an agent that references a tool with required=true.
	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "my-agent",
			Namespace: "default",
			UID:       "agent-uid-001",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{Inline: "You are a helpful agent."},
			Tools: []v1alpha1.AgentToolRef{
				{Ref: v1alpha1.LocalObjectReference{Name: "search"}, Required: true},
			},
		},
	}
	agentData, _ := json.Marshal(agent)
	if err := s.Create(context.Background(), "Agent", "default", "my-agent", agentData); err != nil {
		t.Fatalf("store agent: %v", err)
	}

	// Create a tool that will initially be Ready.
	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "search", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: "https://tools.internal/search",
			Schema:   map[string]any{"type": "object"},
		},
	}
	storeTool(t, s, tool)
	reconcileTool(t, r, "default", "search")

	// Tool should be Ready.
	gotTool := getTool(t, s, "default", "search")
	if gotTool.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("tool phase = %q, want Ready", gotTool.Status.Phase)
	}

	// Agent should have ToolsReady=True condition.
	raw, _ := s.Get(context.Background(), "Agent", "default", "my-agent")
	var gotAgent v1alpha1.Agent
	json.Unmarshal(raw, &gotAgent)

	var toolsReadyCond *v1alpha1.Condition
	for i := range gotAgent.Status.Conditions {
		if gotAgent.Status.Conditions[i].Type == "ToolsReady" {
			toolsReadyCond = &gotAgent.Status.Conditions[i]
			break
		}
	}
	if toolsReadyCond == nil {
		t.Fatal("expected ToolsReady condition on agent")
	}
	if toolsReadyCond.Status != "True" {
		t.Fatalf("ToolsReady status = %q, want True", toolsReadyCond.Status)
	}
	if toolsReadyCond.Reason != "ToolReady" {
		t.Fatalf("ToolsReady reason = %q, want ToolReady", toolsReadyCond.Reason)
	}

	// Now break the tool by removing its endpoint and reconcile → agent gets ToolsReady=False.
	gotTool.Spec.Endpoint = ""
	data, _ := json.Marshal(gotTool)
	_ = s.Update(context.Background(), "Tool", "default", "search", data, gotTool.ObjectMeta.ResourceVersion)
	reconcileTool(t, r, "default", "search")

	// Tool should be Failed.
	gotTool = getTool(t, s, "default", "search")
	if gotTool.Status.Phase == v1alpha1.PhaseReady {
		t.Fatal("expected tool to no longer be Ready")
	}

	// Agent should now have ToolsReady=False.
	raw, _ = s.Get(context.Background(), "Agent", "default", "my-agent")
	json.Unmarshal(raw, &gotAgent)

	toolsReadyCond = nil
	for i := range gotAgent.Status.Conditions {
		if gotAgent.Status.Conditions[i].Type == "ToolsReady" {
			toolsReadyCond = &gotAgent.Status.Conditions[i]
			break
		}
	}
	if toolsReadyCond == nil {
		t.Fatal("expected ToolsReady condition on agent after tool failure")
	}
	if toolsReadyCond.Status != "False" {
		t.Fatalf("ToolsReady status = %q, want False", toolsReadyCond.Status)
	}
	if toolsReadyCond.Reason != "ToolNotReady" {
		t.Fatalf("ToolsReady reason = %q, want ToolNotReady", toolsReadyCond.Reason)
	}
}

func TestToolReconciler_AgentConflictPreservesConcurrentState(t *testing.T) {
	baseStore := newMemStore()
	s := &concurrentAgentUpdateStore{memStore: baseStore, wrapError: true}
	r := NewToolReconciler(s, zaptest.NewLogger(t))
	agent := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "conflict-agent", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			Instructions: v1alpha1.AgentInstructions{Inline: "original"},
			Tools: []v1alpha1.AgentToolRef{
				{Ref: v1alpha1.LocalObjectReference{Name: "search"}, Required: true},
			},
		},
	}
	agentData, _ := json.Marshal(agent)
	if err := baseStore.Create(context.Background(), "Agent", "default", agent.ObjectMeta.Name, agentData); err != nil {
		t.Fatalf("store Agent: %v", err)
	}
	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "search", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: "https://tools.internal/search",
			Schema:   map[string]any{"type": "object"},
		},
	}
	storeTool(t, baseStore, tool)

	reconcileTool(t, r, "default", tool.ObjectMeta.Name)

	raw, err := baseStore.Get(context.Background(), "Agent", "default", agent.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get Agent: %v", err)
	}
	var stored v1alpha1.Agent
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal Agent: %v", err)
	}
	if !s.conflicted {
		t.Fatal("expected Agent update conflict")
	}
	if stored.ObjectMeta.Labels["concurrent"] != "preserved" || stored.Spec.Instructions.Inline != "concurrently updated" {
		t.Fatalf("concurrent metadata or spec lost: metadata=%#v spec=%#v", stored.ObjectMeta, stored.Spec)
	}
	if stored.Status.Phase != v1alpha1.PhaseRunning || stored.Status.Replicas != 3 || stored.Status.ReadyReplicas != 2 {
		t.Fatalf("concurrent status lost: %#v", stored.Status)
	}
	var toolsReady, concurrentReady bool
	for _, condition := range stored.Status.Conditions {
		toolsReady = toolsReady || condition.Type == string(conditionToolsReady) && condition.Status == "True"
		concurrentReady = concurrentReady || condition.Type == "ConcurrentReady" && condition.Status == "True"
	}
	if !toolsReady || !concurrentReady {
		t.Fatalf("merged conditions = %#v, want ToolsReady and ConcurrentReady", stored.Status.Conditions)
	}
}

func TestToolReconciler_WrappedNotFoundIsNoOp(t *testing.T) {
	s := &wrappedToolGetErrorStore{memStore: newMemStore(), err: store.ErrNotFound}
	r := NewToolReconciler(s, zaptest.NewLogger(t))

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "missing"})

	if err != nil {
		t.Fatalf("Reconcile() error = %v, want nil", err)
	}
	if result != (Result{}) {
		t.Fatalf("Reconcile() result = %#v, want zero result", result)
	}
}

// TC-0167 sub-case: Agent with required=false tool ref does NOT get ToolsReady condition.
func TestToolReconciler_TC0167_NonRequiredToolSkipped(t *testing.T) {
	s := newMemStore()
	r := NewToolReconciler(s, zaptest.NewLogger(t))

	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "optional-agent",
			Namespace: "default",
			UID:       "agent-uid-002",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{Inline: "Agent with optional tool."},
			Tools: []v1alpha1.AgentToolRef{
				{Ref: v1alpha1.LocalObjectReference{Name: "optional-tool"}, Required: false},
			},
		},
	}
	agentData, _ := json.Marshal(agent)
	if err := s.Create(context.Background(), "Agent", "default", "optional-agent", agentData); err != nil {
		t.Fatalf("store agent: %v", err)
	}

	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "optional-tool", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: "https://tools.internal/optional",
			Schema:   map[string]any{"type": "object"},
		},
	}
	storeTool(t, s, tool)
	reconcileTool(t, r, "default", "optional-tool")

	// Agent should NOT have ToolsReady condition since required=false.
	raw, _ := s.Get(context.Background(), "Agent", "default", "optional-agent")
	var gotAgent v1alpha1.Agent
	json.Unmarshal(raw, &gotAgent)

	for _, c := range gotAgent.Status.Conditions {
		if c.Type == "ToolsReady" {
			t.Fatalf("unexpected ToolsReady condition on agent with optional tool ref")
		}
	}
}

// TC-0168
// TestToolReconciler_TC0168_HealthProbeDegradedRecovery verifies that a tool
// with a configured HealthProbe transitions to Degraded after consecutive
// failures reach the threshold, and recovers to Ready on subsequent successes.
func TestToolReconciler_TC0168_HealthProbeDegradedRecovery(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	rec := NewToolReconciler(ms, logger, WithToolClock(advancingClock(time.Minute)))
	var status atomic.Int64
	var methodMismatch atomic.Bool
	status.Store(http.StatusOK)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			methodMismatch.Store(true)
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer endpoint.Close()

	// Create tool with two-result thresholds in both directions.
	tool := v1alpha1.Tool{}
	tool.ObjectMeta.Namespace = "default"
	tool.ObjectMeta.Name = "probe-tool"
	tool.Spec.Protocol = v1alpha1.ToolProtocolMCP
	tool.Spec.Endpoint = endpoint.URL
	tool.Spec.HealthProbe = &v1alpha1.ToolHealthProbe{
		Type:             v1alpha1.ProbeTypeHTTPGet,
		Interval:         "10s",
		Timeout:          "5s",
		FailureThreshold: 2,
		SuccessThreshold: 2,
	}
	storeTool(t, ms, tool)

	// reconcileToolResult is a helper that returns Result for probe tests.
	reconcileResult := func() Result {
		t.Helper()
		res, err := rec.Reconcile(context.Background(), Request{Namespace: "default", Name: "probe-tool", Kind: "Tool"})
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		// If requeue (finalizer added), reconcile again.
		if res.Requeue {
			res, err = rec.Reconcile(context.Background(), Request{Namespace: "default", Name: "probe-tool", Kind: "Tool"})
			if err != nil {
				t.Fatalf("reconcile (requeue): %v", err)
			}
		}
		return res
	}

	// First reconcile: tool is valid, should become Ready with RequeueAfter.
	res := reconcileResult()
	if res.RequeueAfter != 10*time.Second {
		t.Fatalf("expected RequeueAfter=10s, got %v", res.RequeueAfter)
	}
	got := getTool(t, ms, "default", "probe-tool")
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected Ready, got %s", got.Status.Phase)
	}
	if methodMismatch.Load() {
		t.Fatal("expected health probe to use GET")
	}

	// Make the real endpoint unhealthy without changing the Tool spec.
	status.Store(http.StatusServiceUnavailable)

	// First failure remains Ready because it is below the failure threshold.
	res = reconcileResult()
	if res.RequeueAfter != 10*time.Second {
		t.Fatalf("expected RequeueAfter=10s below failure threshold, got %v", res.RequeueAfter)
	}
	got = getTool(t, ms, "default", "probe-tool")
	if got.Status.Phase != v1alpha1.PhaseReady || !got.Status.Available {
		t.Fatalf("expected Ready and available after 1 probe failure, got phase=%s available=%t", got.Status.Phase, got.Status.Available)
	}

	// Second failure: hits threshold → Degraded.
	res = reconcileResult()
	if res.RequeueAfter != 10*time.Second {
		t.Fatalf("expected RequeueAfter=10s during recovery, got %v", res.RequeueAfter)
	}
	got = getTool(t, ms, "default", "probe-tool")
	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded after 2 probe failures, got %s", got.Status.Phase)
	}
	if got.Status.Available {
		t.Fatal("expected Available=false when Degraded")
	}
	if res.RequeueAfter != 10*time.Second {
		t.Fatalf("expected RequeueAfter=10s in degraded state, got %v", res.RequeueAfter)
	}

	// Healthy condition should be False.
	hc := findToolCondition(got, "Healthy")
	if hc == nil || hc.Status != "False" {
		t.Fatal("expected Healthy=False condition")
	}

	// A first success remains Degraded until the recovery threshold is reached.
	status.Store(http.StatusNoContent)

	res = reconcileResult()
	if res.RequeueAfter != 10*time.Second {
		t.Fatalf("expected RequeueAfter=10s after recovery, got %v", res.RequeueAfter)
	}
	got = getTool(t, ms, "default", "probe-tool")
	if got.Status.Phase != v1alpha1.PhaseDegraded || got.Status.Available {
		t.Fatalf("expected Degraded and unavailable after 1 recovery success, got phase=%s available=%t", got.Status.Phase, got.Status.Available)
	}

	// A failed recovery probe keeps the tool Degraded and resets its successes.
	status.Store(http.StatusServiceUnavailable)
	reconcileResult()
	got = getTool(t, ms, "default", "probe-tool")
	if got.Status.Phase != v1alpha1.PhaseDegraded || got.Status.Available {
		t.Fatalf("expected Degraded and unavailable after interrupted recovery, got phase=%s available=%t", got.Status.Phase, got.Status.Available)
	}
	if got.Annotations[annotationProbeSuccesses] != "0" {
		t.Fatalf("expected recovery successes to reset, got %q", got.Annotations[annotationProbeSuccesses])
	}

	status.Store(http.StatusNoContent)
	reconcileResult()
	got = getTool(t, ms, "default", "probe-tool")
	if got.Status.Phase != v1alpha1.PhaseDegraded || got.Status.Available {
		t.Fatalf("expected Degraded after first new recovery success, got phase=%s available=%t", got.Status.Phase, got.Status.Available)
	}

	// A second consecutive success recovers the tool.
	res = reconcileResult()
	got = getTool(t, ms, "default", "probe-tool")
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected Ready after recovery, got %s", got.Status.Phase)
	}
	if !got.Status.Available {
		t.Fatal("expected Available=true after recovery")
	}
	hc = findToolCondition(got, "Healthy")
	if hc == nil || hc.Status != "True" {
		t.Fatal("expected Healthy=True after recovery")
	}
}

// TestToolReconciler_TC0168_NoProbeNoRequeue ensures tools without a health
// probe do not get RequeueAfter set.
func TestToolReconciler_TC0168_NoProbeNoRequeue(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	rec := NewToolReconciler(ms, logger)

	tool := v1alpha1.Tool{}
	tool.ObjectMeta.Namespace = "default"
	tool.ObjectMeta.Name = "no-probe-tool"
	tool.Spec.Protocol = v1alpha1.ToolProtocolMCP
	tool.Spec.Endpoint = "https://valid.example.com/mcp"
	storeTool(t, ms, tool)

	res, err := rec.Reconcile(context.Background(), Request{Namespace: "default", Name: "no-probe-tool", Kind: "Tool"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Requeue {
		res, err = rec.Reconcile(context.Background(), Request{Namespace: "default", Name: "no-probe-tool", Kind: "Tool"})
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no RequeueAfter without health probe, got %v", res.RequeueAfter)
	}
}

func TestToolReconciler_NonPositiveProbeIntervalUsesDefault(t *testing.T) {
	for _, interval := range []string{"0s", "-1s"} {
		t.Run(interval, func(t *testing.T) {
			probe := &v1alpha1.ToolHealthProbe{Interval: interval}
			if got := probeInterval(probe); got != defaultProbeInterval {
				t.Fatalf("probeInterval(%q) = %s, want %s", interval, got, defaultProbeInterval)
			}
		})
	}
}

func TestToolReconciler_TCPProbeConnectsAndReportsFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	rec := NewToolReconciler(newMemStore(), zaptest.NewLogger(t))
	endpoint := "tcp://" + listener.Addr().String()

	if err := rec.probeTCP(context.Background(), endpoint, "1s", true); err != nil {
		t.Fatalf("probe open listener: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	if err := rec.probeTCP(context.Background(), endpoint, "1s", true); err == nil {
		t.Fatal("expected closed listener probe to fail")
	}
}

func TestToolReconciler_HealthProbesRejectPrivateEndpointsWithoutOptIn(t *testing.T) {
	reconciler := NewToolReconciler(newMemStore(), zaptest.NewLogger(t))
	tool := &v1alpha1.Tool{Spec: v1alpha1.ToolSpec{Endpoint: "http://127.0.0.1:8080/health"}}
	if err := reconciler.probeHTTP(context.Background(), tool, "1s"); err == nil || !strings.Contains(err.Error(), "endpoint denied") {
		t.Fatalf("HTTP probe error = %v, want endpoint denial", err)
	}
	tool.Spec.Protocol = v1alpha1.ToolProtocolMCP
	if err := reconciler.probeMCP(context.Background(), tool, "1s"); err == nil || !strings.Contains(err.Error(), "endpoint denied") {
		t.Fatalf("MCP probe error = %v, want endpoint denial", err)
	}
	if err := reconciler.probeTCP(context.Background(), "tcp://169.254.169.254:80", "1s"); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("TCP probe error = %v, want endpoint denial", err)
	}
}

func TestToolReconciler_InitialProbeFailuresHonorThreshold(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer endpoint.Close()

	ms := newMemStore()
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "initially-unhealthy", Namespace: "default"}}
	tool.Spec.Protocol = v1alpha1.ToolProtocolMCP
	tool.Spec.Endpoint = endpoint.URL
	tool.Spec.HealthProbe = &v1alpha1.ToolHealthProbe{
		Type:             v1alpha1.ProbeTypeHTTPGet,
		FailureThreshold: 2,
	}
	storeTool(t, ms, tool)
	rec := NewToolReconciler(ms, zaptest.NewLogger(t), WithToolClock(advancingClock(time.Minute)))
	reconcile := func() v1alpha1.Tool {
		t.Helper()
		result, err := rec.Reconcile(context.Background(), Request{Kind: "Tool", Namespace: "default", Name: tool.Name})
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if result.Requeue {
			if _, err := rec.Reconcile(context.Background(), Request{Kind: "Tool", Namespace: "default", Name: tool.Name}); err != nil {
				t.Fatalf("reconcile after registration: %v", err)
			}
		}
		return getTool(t, ms, "default", tool.Name)
	}

	first := reconcile()
	if first.Status.Phase != v1alpha1.PhaseValidating || first.Status.Available {
		t.Fatalf("first failure status = phase %s available %t, want Validating and unavailable", first.Status.Phase, first.Status.Available)
	}
	second := reconcile()
	if second.Status.Phase != v1alpha1.PhaseDegraded || second.Status.Available {
		t.Fatalf("second failure status = phase %s available %t, want Degraded and unavailable", second.Status.Phase, second.Status.Available)
	}
}

func TestToolReconciler_MCPPingUsesJSONRPCAndThresholds(t *testing.T) {
	var fail atomic.Bool
	var methodMismatch atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var rpcReq struct {
			JSONRPC string `json:"jsonrpc"`
			ID      int64  `json:"id"`
			Method  string `json:"method"`
		}
		if req.Method != http.MethodPost || json.NewDecoder(req.Body).Decode(&rpcReq) != nil || rpcReq.JSONRPC != "2.0" || rpcReq.Method != "ping" {
			if rpcReq.Method != "initialize" && rpcReq.Method != "notifications/initialized" {
				methodMismatch.Store(true)
			}
		}
		switch rpcReq.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "probe-session")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      rpcReq.ID,
				"result":  map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}},
			})
			return
		case "notifications/initialized":
			if req.Header.Get("Mcp-Session-Id") != "probe-session" {
				methodMismatch.Store(true)
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Header.Get("Mcp-Session-Id") != "probe-session" {
			methodMismatch.Store(true)
		}
		if fail.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpcReq.ID, "result": map[string]any{}})
	}))
	defer server.Close()

	ms := newMemStore()
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "mcp-ping", Namespace: "default"}}
	tool.Spec.Protocol = v1alpha1.ToolProtocolMCP
	tool.Spec.Endpoint = server.URL
	tool.Spec.HealthProbe = &v1alpha1.ToolHealthProbe{
		Type:             v1alpha1.ProbeTypeMCPPing,
		Interval:         "10s",
		Timeout:          "1s",
		FailureThreshold: 1,
	}
	storeTool(t, ms, tool)
	rec := NewToolReconciler(ms, zaptest.NewLogger(t), WithToolClock(advancingClock(time.Minute)))
	reconcile := func() (v1alpha1.Tool, Result) {
		t.Helper()
		result, err := rec.Reconcile(context.Background(), Request{Kind: "Tool", Namespace: "default", Name: tool.Name})
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if result.Requeue {
			result, err = rec.Reconcile(context.Background(), Request{Kind: "Tool", Namespace: "default", Name: tool.Name})
			if err != nil {
				t.Fatalf("reconcile after registration: %v", err)
			}
		}
		return getTool(t, ms, "default", tool.Name), result
	}

	ready, result := reconcile()
	if ready.Status.Phase != v1alpha1.PhaseReady || !ready.Status.Available || result.RequeueAfter != 10*time.Second {
		t.Fatalf("successful ping = phase %s available %t requeue %s", ready.Status.Phase, ready.Status.Available, result.RequeueAfter)
	}
	if methodMismatch.Load() {
		t.Fatal("MCP ping did not use the expected JSON-RPC request")
	}

	fail.Store(true)
	degraded, result := reconcile()
	if degraded.Status.Phase != v1alpha1.PhaseDegraded || degraded.Status.Available || result.RequeueAfter != 10*time.Second {
		t.Fatalf("failed ping = phase %s available %t requeue %s", degraded.Status.Phase, degraded.Status.Available, result.RequeueAfter)
	}
}

type probeClientFunc func(*http.Request) (*http.Response, error)

func (fn probeClientFunc) Do(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type execProbeRunnerFunc func(context.Context, *v1alpha1.Tool, ExecProbeTarget, []string) error

func (fn execProbeRunnerFunc) RunExecProbe(ctx context.Context, tool *v1alpha1.Tool, target ExecProbeTarget, command []string) error {
	return fn(ctx, tool, target, command)
}

func TestToolReconciler_ExecProbeUsesSandboxRunnerAndThresholds(t *testing.T) {
	ms := newMemStore()
	tool := v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "exec-probe", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			ClassName: "exec-pod",
			Protocol:  v1alpha1.ToolProtocolMCP,
			Endpoint:  "https://tools.internal/mcp",
			HealthProbe: &v1alpha1.ToolHealthProbe{
				Type:             v1alpha1.ProbeTypeExec,
				Command:          []string{"healthcheck", "--ready"},
				Timeout:          "1s",
				FailureThreshold: 1,
			},
		},
	}
	storeTool(t, ms, tool)
	storeToolClass(t, ms, v1alpha1.ToolClass{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "exec-pod"},
		Spec: v1alpha1.ToolClassSpec{
			Provisioner:   "mcp",
			ExecutionTier: v1alpha1.ExecutionTierPod,
			SandboxImage:  "registry.example/tool-probe:1.0",
			SandboxDefaults: &v1alpha1.ToolSandboxSpec{
				CPUMilli: 250,
				MemoryMB: 128,
			},
		},
		Status: v1alpha1.ToolClassStatus{Phase: v1alpha1.PhaseReady},
	})

	var probeErr error
	var calls int
	rec := NewToolReconciler(ms, zaptest.NewLogger(t), WithExecProbeRunner(execProbeRunnerFunc(func(ctx context.Context, gotTool *v1alpha1.Tool, target ExecProbeTarget, command []string) error {
		calls++
		if gotTool.Name != tool.Name {
			t.Errorf("tool name = %q, want %q", gotTool.Name, tool.Name)
		}
		if len(command) != 2 || command[0] != "healthcheck" || command[1] != "--ready" {
			t.Errorf("command = %v, want exact argv", command)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("exec probe context has no deadline")
		}
		if target.Image != "registry.example/tool-probe:1.0" || target.CPUMilli != 250 || target.MemoryMB != 128 || target.Timeout != time.Second {
			t.Errorf("exec probe target = %#v", target)
		}
		command[0] = "mutated"
		return probeErr
	})))

	reconcileTool(t, rec, "default", tool.Name)
	ready := getTool(t, ms, "default", tool.Name)
	if ready.Status.Phase != v1alpha1.PhaseReady || !ready.Status.Available {
		t.Fatalf("successful exec probe = phase %s available %t, want Ready and available", ready.Status.Phase, ready.Status.Available)
	}
	if ready.Spec.HealthProbe.Command[0] != "healthcheck" {
		t.Fatalf("runner mutated stored command: %v", ready.Spec.HealthProbe.Command)
	}
	setToolLastHealthCheck(t, ms, "default", tool.Name, time.Now().Add(-time.Minute))

	probeErr = fmt.Errorf("exit status 1")
	reconcileTool(t, rec, "default", tool.Name)
	degraded := getTool(t, ms, "default", tool.Name)
	if degraded.Status.Phase != v1alpha1.PhaseDegraded || degraded.Status.Available {
		t.Fatalf("failed exec probe = phase %s available %t, want Degraded and unavailable", degraded.Status.Phase, degraded.Status.Available)
	}
	if calls != 2 {
		t.Fatalf("exec probe calls = %d, want 2", calls)
	}
}

func TestToolReconciler_ExecProbeResolvesSandboxTimeouts(t *testing.T) {
	baseTool := &v1alpha1.Tool{Spec: v1alpha1.ToolSpec{HealthProbe: &v1alpha1.ToolHealthProbe{Timeout: "10s"}}}
	cases := []struct {
		name     string
		defaults *v1alpha1.ToolSandboxSpec
		sandbox  *v1alpha1.ToolSandboxSpec
		limits   *v1alpha1.ToolClassResourceLimits
		want     time.Duration
		wantErr  string
	}{
		{name: "probe timeout without sandbox override", want: 10 * time.Second},
		{name: "class sandbox default bounds probe", defaults: &v1alpha1.ToolSandboxSpec{Timeout: "8s"}, want: 8 * time.Second},
		{name: "tool sandbox overrides class default", defaults: &v1alpha1.ToolSandboxSpec{Timeout: "8s"}, sandbox: &v1alpha1.ToolSandboxSpec{Timeout: "6s"}, want: 6 * time.Second},
		{name: "probe timeout remains upper bound", sandbox: &v1alpha1.ToolSandboxSpec{Timeout: "30s"}, want: 10 * time.Second},
		{name: "class maximum rejects effective timeout", limits: &v1alpha1.ToolClassResourceLimits{MaxTimeout: "5s"}, wantErr: "exceeds ToolClass limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveExecProbeTimeout(baseTool.Spec.HealthProbe, tc.defaults, tc.sandbox, tc.limits)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolveExecProbeTimeout error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveExecProbeTimeout: %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolveExecProbeTimeout = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestToolReconciler_ExecProbePreservesDefaultResourcesForPartialClassDefaults(t *testing.T) {
	ms := newMemStore()
	tool := v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "exec-default-resources", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			ClassName: "exec-pod", Protocol: v1alpha1.ToolProtocolMCP, Endpoint: "https://tools.internal/mcp",
			HealthProbe: &v1alpha1.ToolHealthProbe{Type: v1alpha1.ProbeTypeExec, Command: []string{"healthcheck"}},
		},
	}
	storeTool(t, ms, tool)
	storeToolClass(t, ms, v1alpha1.ToolClass{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "exec-pod"},
		Spec: v1alpha1.ToolClassSpec{
			Provisioner: "mcp", ExecutionTier: v1alpha1.ExecutionTierPod, SandboxImage: "registry.example/tool-probe:1.0",
			SandboxDefaults: &v1alpha1.ToolSandboxSpec{CPUMilli: 100},
		},
		Status: v1alpha1.ToolClassStatus{Phase: v1alpha1.PhaseReady},
	})
	target, err := NewToolReconciler(ms, zaptest.NewLogger(t)).resolveExecProbeTarget(context.Background(), &tool)
	if err != nil {
		t.Fatalf("resolveExecProbeTarget: %v", err)
	}
	if target.CPUMilli != 100 || target.MemoryMB != defaultExecProbeMemoryMB {
		t.Fatalf("partial sandbox defaults target = %#v", target)
	}
}

func TestToolReconciler_ExecProbeWithoutSandboxRunnerFailsClosed(t *testing.T) {
	ms := newMemStore()
	tool := v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "exec-unavailable", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			ClassName: "exec-pod",
			Protocol:  v1alpha1.ToolProtocolMCP,
			Endpoint:  "https://tools.internal/mcp",
			HealthProbe: &v1alpha1.ToolHealthProbe{
				Type:             v1alpha1.ProbeTypeExec,
				Command:          []string{"healthcheck"},
				FailureThreshold: 1,
			},
		},
	}
	storeTool(t, ms, tool)
	storeToolClass(t, ms, v1alpha1.ToolClass{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "exec-pod"},
		Spec:       v1alpha1.ToolClassSpec{Provisioner: "mcp", ExecutionTier: v1alpha1.ExecutionTierPod, SandboxImage: "registry.example/tool-probe:1.0"},
		Status:     v1alpha1.ToolClassStatus{Phase: v1alpha1.PhaseReady},
	})

	rec := NewToolReconciler(ms, zaptest.NewLogger(t))
	reconcileTool(t, rec, "default", tool.Name)

	got := getTool(t, ms, "default", tool.Name)
	if got.Status.Phase != v1alpha1.PhaseDegraded || got.Status.Available {
		t.Fatalf("unavailable exec runner = phase %s available %t, want Degraded and unavailable", got.Status.Phase, got.Status.Available)
	}
}

func TestToolReconciler_ExecProbeHonorsInterval(t *testing.T) {
	ms := newMemStore()
	tool := v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "exec-interval", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			ClassName: "exec-pod",
			Protocol:  v1alpha1.ToolProtocolMCP,
			Endpoint:  "https://tools.internal/mcp",
			HealthProbe: &v1alpha1.ToolHealthProbe{
				Type:             v1alpha1.ProbeTypeExec,
				Command:          []string{"healthcheck"},
				Interval:         "1m",
				FailureThreshold: 1,
			},
		},
	}
	storeTool(t, ms, tool)
	storeToolClass(t, ms, v1alpha1.ToolClass{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "exec-pod"},
		Spec:       v1alpha1.ToolClassSpec{Provisioner: "mcp", ExecutionTier: v1alpha1.ExecutionTierPod, SandboxImage: "registry.example/tool-probe:1.0"},
		Status:     v1alpha1.ToolClassStatus{Phase: v1alpha1.PhaseReady},
	})
	var calls int
	reconciler := NewToolReconciler(ms, zaptest.NewLogger(t), WithExecProbeRunner(execProbeRunnerFunc(func(context.Context, *v1alpha1.Tool, ExecProbeTarget, []string) error {
		calls++
		return nil
	})))
	reconcileTool(t, reconciler, "default", tool.Name)
	if calls != 1 {
		t.Fatalf("initial exec probe calls = %d, want 1", calls)
	}
	result, err := reconciler.Reconcile(context.Background(), Request{Kind: "Tool", Namespace: "default", Name: tool.Name})
	if err != nil {
		t.Fatalf("reconcile unchanged exec probe: %v", err)
	}
	if calls != 1 || result.RequeueAfter <= 0 || result.RequeueAfter > time.Minute {
		t.Fatalf("unchanged exec probe calls=%d requeue=%s, want one call and remaining interval", calls, result.RequeueAfter)
	}
}

func TestToolReconciler_ExecProbeRequiresApprovedPodTarget(t *testing.T) {
	tests := []struct {
		name    string
		class   *v1alpha1.ToolClass
		tool    v1alpha1.Tool
		wantErr string
	}{
		{
			name:    "missing class",
			tool:    v1alpha1.Tool{Spec: v1alpha1.ToolSpec{ClassName: "missing", HealthProbe: &v1alpha1.ToolHealthProbe{Type: v1alpha1.ProbeTypeExec}}},
			wantErr: "was not found",
		},
		{
			name:    "wrong tier",
			class:   &v1alpha1.ToolClass{ObjectMeta: v1alpha1.ObjectMeta{Name: "sidecar"}, Spec: v1alpha1.ToolClassSpec{ExecutionTier: v1alpha1.ExecutionTierSidecar, SandboxImage: "registry.example/probe:1.0"}, Status: v1alpha1.ToolClassStatus{Phase: v1alpha1.PhaseReady}},
			tool:    v1alpha1.Tool{Spec: v1alpha1.ToolSpec{ClassName: "sidecar", HealthProbe: &v1alpha1.ToolHealthProbe{Type: v1alpha1.ProbeTypeExec}}},
			wantErr: "must use executionTier pod",
		},
		{
			name:    "missing image",
			class:   &v1alpha1.ToolClass{ObjectMeta: v1alpha1.ObjectMeta{Name: "pod"}, Spec: v1alpha1.ToolClassSpec{ExecutionTier: v1alpha1.ExecutionTierPod}, Status: v1alpha1.ToolClassStatus{Phase: v1alpha1.PhaseReady}},
			tool:    v1alpha1.Tool{Spec: v1alpha1.ToolSpec{ClassName: "pod", HealthProbe: &v1alpha1.ToolHealthProbe{Type: v1alpha1.ProbeTypeExec}}},
			wantErr: "must define sandboxImage",
		},
		{
			name:    "class not ready",
			class:   &v1alpha1.ToolClass{ObjectMeta: v1alpha1.ObjectMeta{Name: "pending"}, Spec: v1alpha1.ToolClassSpec{ExecutionTier: v1alpha1.ExecutionTierPod, SandboxImage: "registry.example/probe:1.0"}},
			tool:    v1alpha1.Tool{Spec: v1alpha1.ToolSpec{ClassName: "pending", HealthProbe: &v1alpha1.ToolHealthProbe{Type: v1alpha1.ProbeTypeExec}}},
			wantErr: "is not Ready",
		},
		{
			name:    "resource limit exceeded",
			class:   &v1alpha1.ToolClass{ObjectMeta: v1alpha1.ObjectMeta{Name: "limited"}, Spec: v1alpha1.ToolClassSpec{ExecutionTier: v1alpha1.ExecutionTierPod, SandboxImage: "registry.example/probe:1.0", ResourceLimits: &v1alpha1.ToolClassResourceLimits{MaxCPUMilli: 100}}, Status: v1alpha1.ToolClassStatus{Phase: v1alpha1.PhaseReady}},
			tool:    v1alpha1.Tool{Spec: v1alpha1.ToolSpec{ClassName: "limited", Sandbox: &v1alpha1.ToolSandboxSpec{CPUMilli: 101}, HealthProbe: &v1alpha1.ToolHealthProbe{Type: v1alpha1.ProbeTypeExec}}},
			wantErr: "exceeds ToolClass limit",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newMemStore()
			if test.class != nil {
				storeToolClass(t, store, *test.class)
			}
			reconciler := NewToolReconciler(store, zaptest.NewLogger(t))
			_, err := reconciler.resolveExecProbeTarget(context.Background(), &test.tool)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("resolveExecProbeTarget error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func setToolLastHealthCheck(t *testing.T, s *memStore, namespace, name string, when time.Time) {
	t.Helper()
	tool := getTool(t, s, namespace, name)
	tool.Status.LastHealthCheck = when.UTC().Format(time.RFC3339)
	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal Tool: %v", err)
	}
	if err := s.Update(context.Background(), toolKind, namespace, name, data, tool.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update Tool health check: %v", err)
	}
}

func TestToolReconciler_HTTPProbeHonorsConfiguredTimeout(t *testing.T) {
	ms := newMemStore()
	rec := NewToolReconciler(ms, zaptest.NewLogger(t))
	rec.probeClient = probeClientFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	tool := v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "timeout-tool", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: "https://tools.internal/health",
			HealthProbe: &v1alpha1.ToolHealthProbe{
				Type:             v1alpha1.ProbeTypeHTTPGet,
				Timeout:          "1ms",
				FailureThreshold: 1,
			},
		},
	}
	storeTool(t, ms, tool)
	reconcileTool(t, rec, "default", tool.Name)

	got := getTool(t, ms, "default", tool.Name)
	if got.Status.Phase != v1alpha1.PhaseDegraded || got.Status.Available {
		t.Fatalf("timeout status = phase %s available %t, want Degraded and unavailable", got.Status.Phase, got.Status.Available)
	}
	if got.Annotations[annotationProbeFailures] != "1" {
		t.Fatalf("timeout probe failures = %q, want 1", got.Annotations[annotationProbeFailures])
	}
}
