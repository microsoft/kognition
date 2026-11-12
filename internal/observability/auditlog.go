// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"encoding/json"
	"time"
)

// AuditEventType classifies audit log entries.
type AuditEventType string

const (
	AuditSpecChange     AuditEventType = "spec_change"
	AuditReasoningStep  AuditEventType = "reasoning_step"
	AuditToolCall       AuditEventType = "tool_call"
	AuditMemoryOp       AuditEventType = "memory_op"
	AuditModelCall      AuditEventType = "model_call"
	AuditKnowledgeQuery AuditEventType = "knowledge_query"
	AuditCommunication  AuditEventType = "communication"
	AuditGrantChange    AuditEventType = "grant_change"
	AuditSessionStart   AuditEventType = "session_start"
	AuditSessionEnd     AuditEventType = "session_end"
	AuditAutonomyReview AuditEventType = "autonomy_review"
	AuditTrustChange    AuditEventType = "trust_change"
	// AuditAdmissionAttempt records an admission request that may later be
	// denied by another validating webhook. It must not be interpreted as a
	// committed resource mutation.
	AuditAdmissionAttempt AuditEventType = "admission_attempt"
)

// AuditEntry represents a single CognitiveAuditLog entry.
type AuditEntry struct {
	ID            string          `json:"id"`
	Timestamp     time.Time       `json:"timestamp"`
	AgentIdentity string          `json:"agentIdentity"`
	Workspace     string          `json:"workspace"`
	Solution      string          `json:"solution,omitempty"`
	SolutionRev   int64           `json:"solutionRevision,omitempty"`
	SessionID     string          `json:"sessionId,omitempty"`
	EventType     AuditEventType  `json:"eventType"`
	SubjectKind   string          `json:"subjectKind,omitempty"`
	SubjectRef    string          `json:"subjectRef,omitempty"`
	Verb          string          `json:"verb,omitempty"`
	Details       json.RawMessage `json:"details,omitempty"`
	TraceID       string          `json:"traceId,omitempty"`
	SpanID        string          `json:"spanId,omitempty"`
}

// AuditStore is the persistence interface for CognitiveAuditLog entries.
// Implementations must guarantee that Append returns only after the entry is
// durably written (write-acknowledged), satisfying the requirement that
// audit records are persisted before any side-effect they describe executes.
type AuditQueryStore interface {
	// Query returns audit entries matching the given filter.
	Query(ctx context.Context, filter AuditFilter) ([]AuditEntry, error)
}

type AuditStore interface {
	AuditQueryStore

	// Append durably writes a single audit entry.
	// It blocks until the entry is confirmed written.
	Append(ctx context.Context, entry AuditEntry) error

	// AppendBatch durably writes multiple audit entries atomically.
	AppendBatch(ctx context.Context, entries []AuditEntry) error
}

// AuditApplyStore reports which immutable audit entries were newly applied.
// Idempotent duplicates return false without weakening AuditStore semantics.
type AuditApplyStore interface {
	AuditStore

	ApplyAuditEntry(ctx context.Context, entry AuditEntry) (bool, error)
	ApplyAuditBatch(ctx context.Context, entries []AuditEntry) ([]bool, error)
}

// AuditFilter specifies query criteria for audit log entries.
type AuditFilter struct {
	Workspace     string
	AgentIdentity string
	SessionID     string
	EventType     AuditEventType
	EventTypes    []AuditEventType
	SchemaVersion int
	Plane         string
	Outcome       string
	Since         time.Time
	Until         time.Time
	Limit         int
	Descending    bool
}

// CognitiveAuditLog provides an append-only log of all agent operations.
// It wraps an AuditStore and enforces the write-ahead durability guarantee:
// no tool/memory/grant side-effect may execute until the corresponding audit
// entry is write-acknowledged.
type CognitiveAuditLog struct {
	store AuditStore
}

// NewCognitiveAuditLog creates a new audit log backed by the given store.
func NewCognitiveAuditLog(store AuditStore) *CognitiveAuditLog {
	return &CognitiveAuditLog{store: store}
}

// RecordBeforeExec writes the audit entry and returns only after it is durably
// stored. The caller must NOT execute the described side-effect until this
// method returns nil. This enforces the T-0182 durability guarantee.
func (l *CognitiveAuditLog) RecordBeforeExec(ctx context.Context, entry AuditEntry) error {
	entry = correlateAuditEntry(ctx, entry)
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	return l.store.Append(ctx, entry)
}

// Record writes an audit entry for informational events (session start/end,
// reasoning steps) that do not gate a side-effect.
func (l *CognitiveAuditLog) Record(ctx context.Context, entry AuditEntry) error {
	entry = correlateAuditEntry(ctx, entry)
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	return l.store.Append(ctx, entry)
}

// RecordBatch atomically writes a batch of audit entries.
func (l *CognitiveAuditLog) RecordBatch(ctx context.Context, entries []AuditEntry) error {
	now := time.Now().UTC()
	for i := range entries {
		entries[i] = correlateAuditEntry(ctx, entries[i])
		if entries[i].Timestamp.IsZero() {
			entries[i].Timestamp = now
		}
	}
	return l.store.AppendBatch(ctx, entries)
}

// Query searches audit entries matching the filter criteria.
func (l *CognitiveAuditLog) Query(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	return l.store.Query(ctx, filter)
}

func correlateAuditEntry(ctx context.Context, entry AuditEntry) AuditEntry {
	if span, ok := SpanContextFromContext(ctx); ok && entry.TraceID == "" {
		traceID, spanID := HexTraceID(span.TraceID), HexSpanID(span.SpanID)
		if IsValidTraceID(traceID) && IsValidSpanID(spanID) {
			entry.TraceID, entry.SpanID = traceID, spanID
		}
	}
	if !isTrustDecisionEventType(entry.EventType) || !IsValidTraceID(entry.TraceID) {
		return entry
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(entry.Details, &fields) != nil || string(fields["schemaVersion"]) != "1" {
		return entry
	}
	if existing := fields["trace"]; len(existing) > 0 && string(existing) != "null" {
		return entry
	}
	correlation := TrustDecisionTrace{TraceID: entry.TraceID}
	if IsValidSpanID(entry.SpanID) {
		correlation.SpanID = entry.SpanID
	}
	fields["trace"], _ = json.Marshal(correlation)
	if details, err := json.Marshal(fields); err == nil {
		entry.Details = details
	}
	return entry
}
