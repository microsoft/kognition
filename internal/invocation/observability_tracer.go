// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package invocation

import (
	"context"
	"sync"

	"github.com/kognition-ai/kognition/internal/observability"
)

// ObservabilityTracer records invocation events as first-class observability spans
// while retaining immutable audit entries for invocation-specific consumers.
type ObservabilityTracer struct {
	lineage *observability.Tracer

	mu      sync.RWMutex
	entries []AuditEntry
}

// NewObservabilityTracer adapts the shared observability tracer to invocation.Tracer.
func NewObservabilityTracer(lineage *observability.Tracer) *ObservabilityTracer {
	return &ObservabilityTracer{lineage: lineage}
}

func (t *ObservabilityTracer) RecordInvocation(ctx context.Context, spanAttrs SpanAttributes, audit AuditEntry) {
	if t == nil || t.lineage == nil {
		return
	}

	attrs := map[string]interface{}{
		observability.AttrInvocationID:    spanAttrs.InvocationID,
		observability.AttrWorkspace:       audit.Workspace,
		observability.AttrAgentNamespace:  audit.AgentNamespace,
		observability.AttrToolName:        spanAttrs.ToolName,
		observability.AttrToolDigest:      spanAttrs.ToolDigest,
		observability.AttrToolGrantRef:    audit.GrantRef,
		observability.AttrToolSandboxTier: spanAttrs.SandboxTier,
		observability.AttrToolArgs:        spanAttrs.Arguments,
		observability.AttrToolResult:      spanAttrs.ResultPayload,
		observability.AttrToolLatencyMs:   spanAttrs.LatencyMs,
		observability.AttrToolCostUnits:   spanAttrs.CostUnits,
		observability.AttrAgentIdentity:   audit.AgentIdentity,
		observability.AttrSolution:        audit.Solution,
		observability.AttrSolutionRev:     audit.SolutionRev,
		observability.AttrSessionID:       audit.SessionID,
		observability.AttrStatus:          string(spanAttrs.ResultStatus),
	}

	toolSpan := t.lineage.StartSpan(ctx, observability.SpanNameToolCall, observability.SpanKindClient, attrs)
	if !audit.Timestamp.IsZero() {
		toolSpan.StartTime = audit.Timestamp.UTC()
	}
	status := observability.StatusOK
	if spanAttrs.ResultStatus != ResultSuccess {
		status = observability.StatusError
	}
	_ = t.lineage.EndSpan(toolSpan, status, string(spanAttrs.ResultStatus))

	audit.TraceID = observability.HexTraceID(toolSpan.Context.TraceID)
	audit.SpanID = observability.HexSpanID(toolSpan.Context.SpanID)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = append(t.entries, audit)
}

// AuditEntries returns a snapshot of invocation audit entries recorded by this tracer.
func (t *ObservabilityTracer) AuditEntries() []AuditEntry {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	entries := make([]AuditEntry, len(t.entries))
	copy(entries, t.entries)
	return entries
}
