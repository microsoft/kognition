// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package invocation_test

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/kognition-ai/kognition/internal/invocation"
	"github.com/kognition-ai/kognition/internal/observability"
)

// TC-0169
// TestTC0169_InvocationEmitsSpanAndAuditEntry verifies that invoking a tool
// through the Invoker produces both an OTel span with correct attributes and
// a CognitiveAuditLog entry with cost attribution.
func TestTC0169_InvocationEmitsSpanAndAuditEntry(t *testing.T) {
	rec := invocation.NewRecorder()
	inv := &invocation.Invoker{Tracer: rec}

	params := invocation.InvokeParams{
		ToolName:       "web-search",
		ToolDigest:     "sha256:abc123",
		ToolRef:        "default/web-search",
		GrantName:      "researcher-web-search",
		GrantRef:       "default/researcher-web-search",
		GrantVerbs:     []string{"invoke"},
		Workspace:      "research",
		AgentNamespace: "research",
		AgentIdentity:  "spiffe://cluster/agent/researcher",
		Solution:       "research-assistant",
		SolutionRev:    7,
		SessionID:      "session-123",
		SandboxTier:    "process",
		TraceID:        "trace-001",
		Arguments:      `{"query":"test"}`,
		CostUnits:      1,
		TokenCost:      42,
	}

	fn := func(_ context.Context, args string) (string, error) {
		return `{"results":["r1"]}`, nil
	}

	payload, err := inv.Invoke(context.Background(), params, fn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if payload != `{"results":["r1"]}` {
		t.Fatalf("unexpected payload: %s", payload)
	}

	// Verify span
	if len(rec.Spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(rec.Spans))
	}
	span := rec.Spans[0]
	if span.ToolName != "web-search" {
		t.Errorf("span.ToolName = %q, want %q", span.ToolName, "web-search")
	}
	if span.ToolDigest != "sha256:abc123" {
		t.Errorf("span.ToolDigest = %q", span.ToolDigest)
	}
	if span.GrantName != "researcher-web-search" {
		t.Errorf("span.GrantName = %q", span.GrantName)
	}
	if span.ResultStatus != invocation.ResultSuccess {
		t.Errorf("span.ResultStatus = %q, want success", span.ResultStatus)
	}
	if span.SandboxTier != "process" {
		t.Errorf("span.SandboxTier = %q", span.SandboxTier)
	}
	if span.CostUnits != 1 {
		t.Errorf("span.CostUnits = %d", span.CostUnits)
	}

	// Verify audit entry
	if len(rec.Entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(rec.Entries))
	}
	entry := rec.Entries[0]
	if entry.AgentIdentity != "spiffe://cluster/agent/researcher" {
		t.Errorf("entry.AgentIdentity = %q", entry.AgentIdentity)
	}
	if entry.ToolRef != "default/web-search" {
		t.Errorf("entry.ToolRef = %q", entry.ToolRef)
	}
	if entry.GrantRef != "default/researcher-web-search" {
		t.Errorf("entry.GrantRef = %q", entry.GrantRef)
	}
	if entry.Verb != "invoke" {
		t.Errorf("entry.Verb = %q", entry.Verb)
	}
	if entry.Result != invocation.ResultSuccess {
		t.Errorf("entry.Result = %q", entry.Result)
	}
	if entry.TokenCost != 42 {
		t.Errorf("entry.TokenCost = %d", entry.TokenCost)
	}
	if entry.TraceID != "trace-001" {
		t.Errorf("entry.TraceID = %q", entry.TraceID)
	}
	if entry.LatencyMs < 0 {
		t.Errorf("entry.LatencyMs = %d", entry.LatencyMs)
	}
}

// TestTC0169_InvocationError verifies error status is recorded correctly.
func TestTC0169_InvocationError(t *testing.T) {
	rec := invocation.NewRecorder()
	inv := &invocation.Invoker{Tracer: rec}

	params := invocation.InvokeParams{
		ToolName:      "failing-tool",
		ToolRef:       "default/failing-tool",
		AgentIdentity: "spiffe://cluster/agent/test",
		TraceID:       "trace-002",
	}

	fn := func(_ context.Context, _ string) (string, error) {
		return "", errors.New("connection refused")
	}

	_, err := inv.Invoke(context.Background(), params, fn)
	if err == nil {
		t.Fatal("expected error")
	}

	if len(rec.Spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(rec.Spans))
	}
	if rec.Spans[0].ResultStatus != invocation.ResultError {
		t.Errorf("expected error status, got %q", rec.Spans[0].ResultStatus)
	}
	if rec.Entries[0].Result != invocation.ResultError {
		t.Errorf("expected error result, got %q", rec.Entries[0].Result)
	}
}

func TestInvokerInvokeAllowsNilTracer(t *testing.T) {
	inv := &invocation.Invoker{}
	called := false

	payload, err := inv.Invoke(context.Background(), invocation.InvokeParams{Arguments: `{"q":"test"}`}, func(_ context.Context, args string) (string, error) {
		called = true
		if args != `{"q":"test"}` {
			t.Fatalf("args = %q, want request payload", args)
		}
		return `{"ok":true}`, nil
	})
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if !called {
		t.Fatal("tool function was not called")
	}
	if payload != `{"ok":true}` {
		t.Fatalf("payload = %q, want tool result", payload)
	}
}

func TestInvocationObservabilityTracerSharesIDsAcrossSpanAuditAndCost(t *testing.T) {
	lineage := observability.NewTracer(observability.TracerConfig{
		SampleRate: 1.0,
		CostTagger: observability.NewCostTagger(),
	})
	var costs []observability.CostRecord
	lineage.SetCostSink(func(record observability.CostRecord) {
		costs = append(costs, record)
	})
	tracer := invocation.NewObservabilityTracer(lineage)
	inv := &invocation.Invoker{Tracer: tracer}

	params := invocation.InvokeParams{
		ToolName:       "web-search",
		ToolDigest:     "sha256:abc123",
		ToolRef:        "default/web-search",
		GrantName:      "researcher-web-search",
		GrantRef:       "default/researcher-web-search",
		GrantVerbs:     []string{"invoke"},
		Workspace:      "research",
		AgentNamespace: "research",
		AgentIdentity:  "spiffe://cluster/agent/researcher",
		Solution:       "research-assistant",
		SolutionRev:    7,
		SessionID:      "session-123",
		SandboxTier:    "process",
		Arguments:      `{"query":"test"}`,
		CostUnits:      7,
		TokenCost:      42,
	}

	_, err := inv.Invoke(context.Background(), params, func(_ context.Context, _ string) (string, error) {
		return `{"results":["r1"]}`, nil
	})
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}

	spans := lineage.Spans()
	if len(spans) != 1 {
		t.Fatalf("span count = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Name != observability.SpanNameToolCall {
		t.Fatalf("span name = %q, want %q", span.Name, observability.SpanNameToolCall)
	}
	if span.Attributes[observability.AttrToolName] != "web-search" {
		t.Fatalf("tool name attr = %v, want web-search", span.Attributes[observability.AttrToolName])
	}
	if span.Attributes[observability.AttrToolCostUnits] != int64(7) {
		t.Fatalf("tool cost attr = %v, want 7", span.Attributes[observability.AttrToolCostUnits])
	}
	if span.Attributes[observability.AttrWorkspace] != "research" || span.Attributes[observability.AttrAgentNamespace] != "research" || span.Attributes[observability.AttrAgentIdentity] != "spiffe://cluster/agent/researcher" || span.Attributes[observability.AttrSolution] != "research-assistant" || span.Attributes[observability.AttrSolutionRev] != int64(7) || span.Attributes[observability.AttrSessionID] != "session-123" {
		t.Fatalf("attribution span attrs = %#v", span.Attributes)
	}

	spanID := hex.EncodeToString(span.Context.SpanID[:])
	entries := tracer.AuditEntries()
	if len(entries) != 1 {
		t.Fatalf("audit entry count = %d, want 1", len(entries))
	}
	if entries[0].TraceID != lineage.TraceIDHex() {
		t.Fatalf("audit trace ID = %q, want %q", entries[0].TraceID, lineage.TraceIDHex())
	}
	if entries[0].SpanID != spanID {
		t.Fatalf("audit span ID = %q, want %q", entries[0].SpanID, spanID)
	}

	if len(costs) != 1 {
		t.Fatalf("cost count = %d, want 1", len(costs))
	}
	if costs[0].OperationType != observability.CostOpToolExecution {
		t.Fatalf("cost operation = %q, want %q", costs[0].OperationType, observability.CostOpToolExecution)
	}
	if costs[0].CostUnits != 7 {
		t.Fatalf("cost units = %d, want 7", costs[0].CostUnits)
	}
	if costs[0].TraceID != lineage.TraceIDHex() {
		t.Fatalf("cost trace ID = %q, want %q", costs[0].TraceID, lineage.TraceIDHex())
	}
	if costs[0].SpanID != spanID {
		t.Fatalf("cost span ID = %q, want %q", costs[0].SpanID, spanID)
	}
	if costs[0].Workspace != "research" || costs[0].AgentNamespace != "research" || costs[0].AgentIdentity != "spiffe://cluster/agent/researcher" || costs[0].Solution != "research-assistant" || costs[0].SolutionRev != 7 || costs[0].SessionID != "session-123" {
		t.Fatalf("cost attribution = %#v", costs[0])
	}
}

func TestInvocationObservabilityTracerAuditUsesParentedSpanTrace(t *testing.T) {
	lineage := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0})
	tracer := invocation.NewObservabilityTracer(lineage)
	inv := &invocation.Invoker{Tracer: tracer}
	parent := lineage.StartRootSpan("parent", observability.SpanKindInternal, nil)
	ctx := observability.ContextWithSpanContext(context.Background(), parent.Context)

	_, err := inv.Invoke(ctx, invocation.InvokeParams{
		ToolName:      "web-search",
		ToolRef:       "default/web-search",
		AgentIdentity: "spiffe://cluster/agent/researcher",
	}, func(_ context.Context, _ string) (string, error) {
		return `{"results":["r1"]}`, nil
	})
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}

	spans := lineage.Spans()
	if len(spans) != 1 {
		t.Fatalf("span count = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Context.TraceID != parent.Context.TraceID {
		t.Fatalf("span trace ID = %q, want parent trace %q", observability.HexTraceID(span.Context.TraceID), observability.HexTraceID(parent.Context.TraceID))
	}
	entries := tracer.AuditEntries()
	if len(entries) != 1 {
		t.Fatalf("audit entry count = %d, want 1", len(entries))
	}
	if entries[0].TraceID != observability.HexTraceID(span.Context.TraceID) {
		t.Fatalf("audit trace ID = %q, want span trace %q", entries[0].TraceID, observability.HexTraceID(span.Context.TraceID))
	}
	if entries[0].TraceID == lineage.TraceIDHex() {
		t.Fatalf("audit trace ID = lineage trace %q, want parented span trace", entries[0].TraceID)
	}
}
