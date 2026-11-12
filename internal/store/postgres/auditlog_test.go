// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/observability"
)

func TestAuditAbsentDetailsPersistAndFenceDuplicates(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%t", batch), func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			apply := func(entry observability.AuditEntry) (bool, error) {
				if !batch {
					return store.ApplyAuditEntry(ctx, entry)
				}
				applied, err := store.ApplyAuditBatch(ctx, []observability.AuditEntry{entry})
				if err != nil {
					return false, err
				}
				if len(applied) != 1 {
					t.Fatalf("applied = %v, want one result", applied)
				}
				return applied[0], nil
			}
			entry := observability.AuditEntry{ID: "absent-details", Workspace: "cluster", EventType: observability.AuditSpecChange}
			if applied, err := apply(entry); err != nil || !applied {
				t.Fatalf("initial write: applied=%t error=%v", applied, err)
			}
			if applied, err := apply(entry); err != nil || applied {
				t.Fatalf("identical retry: applied=%t error=%v", applied, err)
			}
			if batch {
				if err := store.AppendBatch(ctx, []observability.AuditEntry{entry}); err != nil {
					t.Fatalf("append batch retry: %v", err)
				}
			} else if err := store.Append(ctx, entry); err != nil {
				t.Fatalf("append retry: %v", err)
			}
			entries, err := store.Query(ctx, observability.AuditFilter{Workspace: "cluster"})
			if err != nil || len(entries) != 1 {
				t.Fatalf("persisted entries=%v error=%v", entries, err)
			}
			if len(entries[0].Details) != 0 {
				t.Fatalf("absent details changed to %q", entries[0].Details)
			}
			var payload []byte
			if err := testPool.QueryRow(ctx, "SELECT canonical_payload FROM cognitive_audit_log WHERE id = $1", entry.ID).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			if payload == nil || len(payload) != 0 {
				t.Fatalf("canonical payload=%v, want non-null empty bytes", payload)
			}
			entry.Details = json.RawMessage(`{"changed":true}`)
			if _, err := apply(entry); err == nil || !strings.Contains(err.Error(), "different content") {
				t.Fatalf("conflicting retry error=%v", err)
			}
			if batch {
				pending := observability.AuditEntry{ID: "rolled-back", Workspace: "cluster", EventType: observability.AuditSpecChange}
				if _, err := store.ApplyAuditBatch(ctx, []observability.AuditEntry{pending, entry}); err == nil || !strings.Contains(err.Error(), "different content") {
					t.Fatalf("conflicting batch error=%v", err)
				}
			}
			entries, err = store.Query(ctx, observability.AuditFilter{Workspace: "cluster"})
			if err != nil || len(entries) != 1 || entries[0].ID != "absent-details" || len(entries[0].Details) != 0 {
				t.Fatalf("entries changed after conflict: entries=%v error=%v", entries, err)
			}
			entry.ID = "exact-bytes"
			if applied, err := apply(entry); err != nil || !applied {
				t.Fatalf("nonempty payload: applied=%t error=%v", applied, err)
			}
			if applied, err := apply(entry); err != nil || applied {
				t.Fatalf("nonempty retry: applied=%t error=%v", applied, err)
			}
			entry.Details = json.RawMessage(`{ "changed": true }`)
			if _, err := apply(entry); err == nil || !strings.Contains(err.Error(), "different content") {
				t.Fatalf("byte-different retry error=%v", err)
			}
		})
	}
}

func TestAuditAppendRejectsDuplicateIDWithDifferentCanonicalPayload(t *testing.T) {
	store := newTestStore(t)
	entry := observability.AuditEntry{ID: "reasoning-probe-stable", Workspace: "team-a", EventType: observability.AuditReasoningStep, Details: json.RawMessage(`{"outcome":"succeeded"}`)}
	if err := store.Append(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), entry); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	entry.Details = json.RawMessage(`{"outcome":"failed"}`)
	if err := store.Append(context.Background(), entry); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("conflicting retry error = %v, want different content", err)
	}
}

// TC-0307: PostgreSQL trust-decision queries implement the normalized dashboard contract.
func TestTrustDecisionQueryMatchesDashboardContract(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	for _, fixture := range []struct {
		id      string
		at      time.Time
		plane   string
		outcome string
	}{
		{id: "a", at: now.Add(-time.Hour), plane: "tool", outcome: "allowed"},
		{id: "b", at: now.Add(-time.Minute), plane: "tool", outcome: "allowed"},
		{id: "c", at: now.Add(-time.Minute), plane: "tool", outcome: "allowed"},
		{id: "memory", at: now, plane: "memory", outcome: "allowed"},
		{id: "model", at: now, plane: "model", outcome: "allowed"},
		{id: "communication", at: now, plane: "communication", outcome: "revoked"},
		{id: "denied", at: now, plane: "tool", outcome: "denied"},
	} {
		entry := postgresTrustDecisionEntry(t, fixture.id, fixture.at, fixture.plane, fixture.outcome)
		if err := store.Append(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	for index := range 3 {
		details := json.RawMessage(`{"plane":"tool","outcome":"allowed","recordID":"continuing-record"}`)
		entry := observability.AuditEntry{ID: fmt.Sprintf("generic-memory-%d", index), Timestamp: now.Add(time.Duration(index) * time.Second), Workspace: "team-a", AgentIdentity: "agent-a", SessionID: "session-a", EventType: observability.AuditMemoryOp, Details: details}
		if err := store.Append(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	result, err := observability.QueryTrustDecisions(context.Background(), store, observability.TrustDecisionQuery{Namespace: "team-a", AgentIdentity: "agent-a", SessionID: "session-a", Plane: "tool", Outcome: "allowed", Limit: 2}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 2 || result.Items[0].ID != "c" || result.Items[1].ID != "b" || !result.Truncated {
		t.Fatalf("result = %#v", result)
	}
	for _, fixture := range []struct{ plane, outcome, id string }{{"model", "allowed", "model"}, {"communication", "revoked", "communication"}} {
		result, err := observability.QueryTrustDecisions(context.Background(), store, observability.TrustDecisionQuery{Namespace: "team-a", Plane: fixture.plane, Outcome: fixture.outcome}, now)
		if err != nil || len(result.Items) != 1 || result.Items[0].ID != fixture.id {
			t.Fatalf("%s result=%#v error=%v", fixture.plane, result, err)
		}
	}
}

func postgresTrustDecisionEntry(t *testing.T, id string, timestamp time.Time, plane, outcome string) observability.AuditEntry {
	t.Helper()
	eventType, kind := observability.AuditToolCall, "Tool"
	switch plane {
	case "memory":
		eventType, kind = observability.AuditMemoryOp, "MemoryVolume"
	case "model":
		eventType, kind = observability.AuditModelCall, "Model"
	case "communication":
		eventType, kind = observability.AuditCommunication, "AgentService"
	}
	decision := observability.TrustDecisionV1{SchemaVersion: 1, ID: id, Timestamp: timestamp, Plane: plane, Outcome: outcome, ReasonCode: "authorized", Workspace: "team-a", Request: observability.TrustDecisionRequest{Operation: "invoke", TargetRef: observability.TrustDecisionResourceRef{Kind: kind, Name: "target"}}, Authorization: observability.TrustDecisionAuthorization{Identity: "allowed", Role: "allowed", Grant: "allowed", Policy: "allowed"}, Execution: observability.TrustDecisionExecution{Status: "not_started"}}
	details, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	return observability.AuditEntry{ID: id, Timestamp: timestamp, Workspace: "team-a", AgentIdentity: "agent-a", SessionID: "session-a", EventType: eventType, Details: details}
}
