// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package invocation implements tool invocation traceability per §10A.8.
// It records OTel-style spans and CognitiveAuditLog entries for each tool call.
package invocation

import (
	"context"
	"sync"
	"time"
)

// ResultStatus is the outcome of a tool invocation.
type ResultStatus string

const (
	ResultSuccess ResultStatus = "success"
	ResultError   ResultStatus = "error"
	ResultTimeout ResultStatus = "timeout"
)

// SpanAttributes captures OTel span attributes for a tool invocation.
type SpanAttributes struct {
	InvocationID   string
	Workspace      string
	AgentNamespace string
	AgentIdentity  string
	Solution       string
	SolutionRev    int64
	SessionID      string
	ToolName       string
	ToolDigest     string
	GrantName      string
	GrantVerbs     []string
	Arguments      string // redacted per policy
	ResultStatus   ResultStatus
	ResultPayload  string // redacted per policy
	SandboxTier    string
	LatencyMs      int64
	CostUnits      int64
}

// AuditEntry represents an immutable CognitiveAuditLog record per tool call.
type AuditEntry struct {
	InvocationID   string
	Timestamp      time.Time
	Workspace      string
	AgentNamespace string
	AgentIdentity  string
	Solution       string
	SolutionRev    int64
	SessionID      string
	ToolRef        string // namespace/tool-name
	ToolDigest     string
	GrantRef       string // namespace/toolGrant-name
	Verb           string
	Arguments      string // redacted
	Result         ResultStatus
	LatencyMs      int64
	TokenCost      int64
	TraceID        string
	SpanID         string
}

// Tracer records tool invocation spans and audit entries.
type Tracer interface {
	// RecordInvocation records a completed tool invocation.
	RecordInvocation(ctx context.Context, span SpanAttributes, audit AuditEntry)
}

// Recorder is an in-memory Tracer implementation for testing and development.
type Recorder struct {
	mu      sync.Mutex
	Spans   []SpanAttributes
	Entries []AuditEntry
}

// NewRecorder creates a new in-memory Recorder.
func NewRecorder() *Recorder {
	return &Recorder{}
}

func (r *Recorder) RecordInvocation(_ context.Context, span SpanAttributes, audit AuditEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Spans = append(r.Spans, span)
	r.Entries = append(r.Entries, audit)
}
