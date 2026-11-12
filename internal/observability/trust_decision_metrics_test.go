// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type trustDecisionAuditStoreStub struct {
	appendErr      error
	appendBatchErr error
	applied        bool
	batchApplied   []bool
}

func (s *trustDecisionAuditStoreStub) Append(context.Context, AuditEntry) error {
	return s.appendErr
}

func (s *trustDecisionAuditStoreStub) AppendBatch(context.Context, []AuditEntry) error {
	return s.appendBatchErr
}

func (*trustDecisionAuditStoreStub) Query(context.Context, AuditFilter) ([]AuditEntry, error) {
	return nil, nil
}

func (s *trustDecisionAuditStoreStub) ApplyAuditEntry(context.Context, AuditEntry) (bool, error) {
	return s.applied, s.appendErr
}

func (s *trustDecisionAuditStoreStub) ApplyAuditBatch(_ context.Context, entries []AuditEntry) ([]bool, error) {
	if s.appendBatchErr != nil {
		return nil, s.appendBatchErr
	}
	if s.batchApplied != nil {
		return s.batchApplied, nil
	}
	applied := make([]bool, len(entries))
	for index := range applied {
		applied[index] = s.applied
	}
	return applied, nil
}

func TestTrustDecisionMetrics_RecordOnlyDurablyAppendedNormalizedDecisions(t *testing.T) {
	metrics := NewTrustDecisionMetrics(prometheus.NewRegistry())
	store := &trustDecisionAuditStoreStub{applied: true}
	metered := metrics.Wrap(store)
	decision := TrustDecisionV1{
		SchemaVersion: TrustDecisionSchemaVersion,
		ID:            "decision-1",
		Timestamp:     time.Now().UTC(),
		Plane:         "tool",
		Outcome:       "denied",
		ReasonCode:    "grant_missing",
		Workspace:     "team-a",
		Request: TrustDecisionRequest{
			Operation: "invoke",
			TargetRef: TrustDecisionResourceRef{Kind: "Tool", Name: "weather"},
		},
		Authorization: TrustDecisionAuthorization{
			Identity: "allowed",
			Role:     "allowed",
			Grant:    "denied",
			Policy:   "not_evaluated",
		},
		Approval:  &TrustDecisionApproval{Status: "not_evaluated"},
		Execution: TrustDecisionExecution{Status: "blocked", TargetReached: trustDecisionBoolPointer(false)},
	}

	if err := metered.Append(context.Background(), trustDecisionMetricsAuditEntry(t, decision)); err != nil {
		t.Fatalf("Append(): %v", err)
	}

	labels := prometheus.Labels{
		"namespace": "team-a", "plane": "tool", "outcome": "denied", "reason_code": "grant_missing",
		"identity_state": "allowed", "role_state": "allowed", "grant_state": "denied", "policy_state": "not_evaluated",
		"approval_state": "not_evaluated", "execution_state": "blocked", "target_reached_state": "false",
	}
	if got := testutil.ToFloat64(metrics.decisions.With(labels)); got != 1 {
		t.Fatalf("decisions = %v, want 1", got)
	}

	store.applied = false
	if err := metered.Append(context.Background(), trustDecisionMetricsAuditEntry(t, decision)); err != nil {
		t.Fatalf("duplicate Append(): %v", err)
	}
	if got := testutil.ToFloat64(metrics.decisions.With(labels)); got != 1 {
		t.Fatalf("decisions after failed append = %v, want 1", got)
	}
	store.appendErr = errors.New("store unavailable")
	if err := metered.Append(context.Background(), trustDecisionMetricsAuditEntry(t, decision)); err == nil {
		t.Fatal("failed Append() error = nil, want error")
	}
}

func TestTrustDecisionMetrics_PreserveExplicitMissingAndUnknownStates(t *testing.T) {
	metrics := NewTrustDecisionMetrics(prometheus.NewRegistry())
	metered := metrics.Wrap(&trustDecisionAuditStoreStub{applied: true})
	decision := TrustDecisionV1{
		SchemaVersion: TrustDecisionSchemaVersion,
		ID:            "decision-2",
		Timestamp:     time.Now().UTC(),
		Plane:         "unexpected-plane",
		Outcome:       "unexpected-outcome",
		ReasonCode:    "unexpected-reason",
		Workspace:     "team-a",
		Request:       TrustDecisionRequest{Operation: "custom", TargetRef: TrustDecisionResourceRef{Kind: "Custom", Name: "target"}},
		Authorization: TrustDecisionAuthorization{Identity: "", Role: "unexpected", Grant: "", Policy: ""},
		Execution:     TrustDecisionExecution{Status: ""},
	}

	if err := metered.Append(context.Background(), trustDecisionMetricsAuditEntry(t, decision)); err != nil {
		t.Fatalf("Append(): %v", err)
	}

	labels := prometheus.Labels{
		"namespace": "team-a", "plane": "unknown", "outcome": "unknown", "reason_code": "unknown",
		"identity_state": "not_reported", "role_state": "unknown", "grant_state": "not_reported", "policy_state": "not_reported",
		"approval_state": "not_reported", "execution_state": "not_reported", "target_reached_state": "not_reported",
	}
	if got := testutil.ToFloat64(metrics.decisions.With(labels)); got != 1 {
		t.Fatalf("decisions = %v, want 1", got)
	}
}

func TestTrustDecisionMetrics_SkipUnrelatedAuditAndCountSuccessfulBatch(t *testing.T) {
	metrics := NewTrustDecisionMetrics(prometheus.NewRegistry())
	store := &trustDecisionAuditStoreStub{applied: true}
	metered := metrics.Wrap(store)
	nonContract := AuditEntry{ID: "ordinary", EventType: AuditSpecChange, Details: json.RawMessage(`{"schemaVersion":1,"message":"ordinary audit"}`)}
	decision := TrustDecisionV1{
		SchemaVersion: TrustDecisionSchemaVersion,
		ID:            "decision-3",
		Timestamp:     time.Now().UTC(),
		Plane:         "memory",
		Outcome:       "completed",
		ReasonCode:    "target_completed",
		Workspace:     "team-b",
		Request:       TrustDecisionRequest{Operation: "read", TargetRef: TrustDecisionResourceRef{Kind: "Memory", Name: "shared"}},
		Authorization: TrustDecisionAuthorization{Identity: "allowed", Role: "allowed", Grant: "allowed", Policy: "allowed"},
		Execution:     TrustDecisionExecution{Status: "completed", TargetReached: trustDecisionBoolPointer(true)},
	}
	batch := []AuditEntry{nonContract, trustDecisionMetricsAuditEntry(t, decision)}

	if err := metered.Append(context.Background(), nonContract); err != nil {
		t.Fatalf("Append(non-contract): %v", err)
	}
	if err := metered.AppendBatch(context.Background(), batch); err != nil {
		t.Fatalf("AppendBatch(): %v", err)
	}

	labels := prometheus.Labels{
		"namespace": "team-b", "plane": "memory", "outcome": "completed", "reason_code": "target_completed",
		"identity_state": "allowed", "role_state": "allowed", "grant_state": "allowed", "policy_state": "allowed",
		"approval_state": "not_reported", "execution_state": "completed", "target_reached_state": "true",
	}
	if got := testutil.ToFloat64(metrics.decisions.With(labels)); got != 1 {
		t.Fatalf("decisions = %v, want 1", got)
	}

	store.applied = false
	if err := metered.AppendBatch(context.Background(), batch); err != nil {
		t.Fatalf("duplicate AppendBatch(): %v", err)
	}
	if got := testutil.ToFloat64(metrics.decisions.With(labels)); got != 1 {
		t.Fatalf("decisions after duplicate batch = %v, want 1", got)
	}
	store.appendBatchErr = errors.New("batch failed")
	if err := metered.AppendBatch(context.Background(), batch); err == nil {
		t.Fatal("failed AppendBatch() error = nil, want error")
	}
}

func trustDecisionMetricsAuditEntry(t *testing.T, decision TrustDecisionV1) AuditEntry {
	t.Helper()
	details, err := json.Marshal(decision)
	if err != nil {
		t.Fatalf("marshal trust decision: %v", err)
	}
	eventTypes := map[string]AuditEventType{
		"tool": AuditToolCall, "memory": AuditMemoryOp, "model": AuditModelCall, "communication": AuditCommunication,
	}
	eventType := eventTypes[decision.Plane]
	if eventType == "" {
		eventType = AuditToolCall
	}
	return AuditEntry{ID: decision.ID, Workspace: decision.Workspace, EventType: eventType, Details: details}
}

func trustDecisionBoolPointer(value bool) *bool {
	return &value
}
