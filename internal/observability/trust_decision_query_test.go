// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

type trustDecisionQueryStore struct {
	entries []AuditEntry
	filter  AuditFilter
	err     error
}

type boundedTrustDecisionQueryStore struct {
	entries []AuditEntry
}

func (*trustDecisionQueryStore) Append(context.Context, AuditEntry) error        { return nil }
func (*trustDecisionQueryStore) AppendBatch(context.Context, []AuditEntry) error { return nil }
func (s *trustDecisionQueryStore) Query(_ context.Context, filter AuditFilter) ([]AuditEntry, error) {
	s.filter = filter
	return append([]AuditEntry(nil), s.entries...), s.err
}

func (*boundedTrustDecisionQueryStore) Append(context.Context, AuditEntry) error        { return nil }
func (*boundedTrustDecisionQueryStore) AppendBatch(context.Context, []AuditEntry) error { return nil }
func (s *boundedTrustDecisionQueryStore) Query(_ context.Context, filter AuditFilter) ([]AuditEntry, error) {
	entries := make([]AuditEntry, 0, filter.Limit)
	for _, entry := range s.entries {
		var fields struct {
			SchemaVersion int `json:"schemaVersion"`
		}
		if err := json.Unmarshal(entry.Details, &fields); err != nil {
			return nil, err
		}
		if filter.SchemaVersion > 0 && fields.SchemaVersion != filter.SchemaVersion {
			continue
		}
		entries = append(entries, entry)
		if len(entries) == filter.Limit {
			break
		}
	}
	return entries, nil
}

func TestQueryTrustDecisionsDefaultsFiltersOrdersAndTruncates(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	store := &trustDecisionQueryStore{entries: []AuditEntry{
		trustDecisionAuditEntry(t, "b", now.Add(-time.Minute), "tool", "allowed", "agent-a", "session-a", "demo-a"),
		trustDecisionAuditEntry(t, "c", now.Add(-time.Minute), "tool", "allowed", "agent-a", "session-a", "demo-a"),
		trustDecisionAuditEntry(t, "aa", now.Add(-2*time.Hour), "tool", "allowed", "agent-a", "session-a", "demo-a"),
		trustDecisionAuditEntry(t, "a", now.Add(-time.Hour), "tool", "allowed", "agent-a", "session-a", "demo-b"),
		{ID: "unrelated", Timestamp: now, Workspace: "team-a", EventType: AuditSpecChange, Details: json.RawMessage(`{}`)},
	}}
	result, err := QueryTrustDecisions(context.Background(), store, TrustDecisionQuery{Namespace: "team-a", RunID: "demo-a", AgentIdentity: "agent-a", SessionID: "session-a", Plane: "tool", Outcome: "allowed", Limit: 2}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.APIVersion != TrustDecisionAPIVersion || len(result.Items) != 2 || !result.Truncated || result.Items[0].ID != "c" || result.Items[1].ID != "b" {
		t.Fatalf("result = %#v", result)
	}
	if !store.filter.Since.IsZero() || !store.filter.Until.Equal(now) || store.filter.Limit != 3 || !store.filter.Descending || len(store.filter.EventTypes) != 4 || store.filter.Plane != "tool" || store.filter.Outcome != "allowed" {
		t.Fatalf("store filter = %#v", store.filter)
	}
}

func TestQueryTrustDecisionsRunIDIncludesRetainedEvidenceByDefault(t *testing.T) {
	now := time.Date(2026, 9, 10, 20, 0, 0, 0, time.UTC)
	store := &trustDecisionQueryStore{entries: []AuditEntry{
		trustDecisionAuditEntry(t, "retained-model", now.Add(-25*time.Hour), "model", "completed", "agent-a", "session-a", "retained-run"),
	}}

	result, err := QueryTrustDecisions(context.Background(), store, TrustDecisionQuery{Namespace: "team-a", RunID: "retained-run"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "retained-model" {
		t.Fatalf("result = %#v, want retained run evidence", result)
	}
	if !store.filter.Since.IsZero() || !store.filter.Until.Equal(now) {
		t.Fatalf("store filter = %#v, want unbounded retained history through now", store.filter)
	}
}

// TC-0308: Model and Communication decisions use the shared four-plane query contract.
func TestQueryTrustDecisionsReturnsAllFourPlanes(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	store := &trustDecisionQueryStore{}
	for index, plane := range []string{"tool", "memory", "model", "communication"} {
		entry := trustDecisionAuditEntry(t, plane, now.Add(-time.Duration(index)*time.Minute), plane, "allowed", "agent-a", "session-a")
		entry.EventType = map[string]AuditEventType{"tool": AuditToolCall, "memory": AuditMemoryOp, "model": AuditModelCall, "communication": AuditCommunication}[plane]
		store.entries = append(store.entries, entry)
	}
	result, err := QueryTrustDecisions(context.Background(), store, TrustDecisionQuery{Namespace: "team-a"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 4 {
		t.Fatalf("items=%d, want all four planes", len(result.Items))
	}
	for _, plane := range []string{"tool", "memory", "model", "communication"} {
		found := false
		for _, decision := range result.Items {
			found = found || decision.Plane == plane
		}
		if !found {
			t.Fatalf("plane %q missing from result: %#v", plane, result.Items)
		}
	}
}

func TestQueryTrustDecisionsAllowsAllNamespaces(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	store := &trustDecisionQueryStore{entries: []AuditEntry{
		trustDecisionAuditEntryForWorkspace(t, "decision-a", "team-a", now.Add(-time.Minute), "tool", "allowed", "agent-a", "session-a"),
		trustDecisionAuditEntryForWorkspace(t, "decision-b", "team-b", now.Add(-2*time.Minute), "tool", "allowed", "agent-b", "session-b"),
	}}

	result, err := QueryTrustDecisions(context.Background(), store, TrustDecisionQuery{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 2 || result.Items[0].Workspace != "team-a" || result.Items[1].Workspace != "team-b" {
		t.Fatalf("result = %#v", result)
	}
	if store.filter.Workspace != "" {
		t.Fatalf("workspace filter = %q, want all namespaces", store.filter.Workspace)
	}
}

func TestQueryTrustDecisionsIgnoresNonContractAuditEntries(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	store := &trustDecisionQueryStore{entries: []AuditEntry{
		{ID: "general-memory-audit", Timestamp: now, Workspace: "team-a", EventType: AuditMemoryOp, Details: json.RawMessage(`{"recordID":"continuing-record","volumeHandle":"in-memory-volume"}`)},
		trustDecisionAuditEntry(t, "decision", now.Add(-time.Minute), "tool", "allowed", "agent-a", "session-a", "demo-a"),
	}}

	result, err := QueryTrustDecisions(context.Background(), store, TrustDecisionQuery{Namespace: "team-a"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "decision" {
		t.Fatalf("result = %#v", result)
	}
}

func TestQueryTrustDecisionsFiltersContractBeforeApplyingLimit(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	store := &boundedTrustDecisionQueryStore{entries: []AuditEntry{
		{ID: "general-memory-audit", Timestamp: now, Workspace: "team-a", EventType: AuditMemoryOp, Details: json.RawMessage(`{"recordID":"continuing-record"}`)},
		trustDecisionAuditEntry(t, "newest-decision", now.Add(-time.Minute), "tool", "allowed", "agent-a", "session-a", "demo-a"),
		trustDecisionAuditEntry(t, "older-decision", now.Add(-2*time.Minute), "tool", "allowed", "agent-a", "session-a", "demo-a"),
	}}

	result, err := QueryTrustDecisions(context.Background(), store, TrustDecisionQuery{Namespace: "team-a", Limit: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "newest-decision" || !result.Truncated {
		t.Fatalf("result = %#v", result)
	}
}

func TestQueryTrustDecisionsRejectsInvalidQueriesAndStoredPayloads(t *testing.T) {
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	for name, query := range map[string]TrustDecisionQuery{
		"limit":   {Namespace: "team-a", Limit: 501},
		"plane":   {Namespace: "team-a", Plane: "database"},
		"outcome": {Namespace: "team-a", Outcome: "maybe"},
		"range":   {Namespace: "team-a", Since: now, Until: now.Add(-time.Minute)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := QueryTrustDecisions(context.Background(), &trustDecisionQueryStore{}, query, now); !errors.Is(err, ErrInvalidTrustDecisionQuery) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	store := &trustDecisionQueryStore{entries: []AuditEntry{{ID: "broken", Timestamp: now, Workspace: "team-a", EventType: AuditToolCall, Details: json.RawMessage(`{`)}}}
	if _, err := QueryTrustDecisions(context.Background(), store, TrustDecisionQuery{Namespace: "team-a"}, now); err == nil || !strings.Contains(err.Error(), `entry "broken"`) {
		t.Fatalf("decode error = %v", err)
	}
	store = &trustDecisionQueryStore{err: errors.New("bounded page overflow")}
	if _, err := QueryTrustDecisions(context.Background(), store, TrustDecisionQuery{Namespace: "team-a"}, now); err == nil || !strings.Contains(err.Error(), "bounded page overflow") {
		t.Fatalf("store error = %v", err)
	}
}

func TestParseTrustDecisionQuery(t *testing.T) {
	values := url.Values{
		"runId": {"demo-a"}, "agentIdentity": {"agent-a"}, "sessionId": {"session-a"}, "plane": {"memory"}, "outcome": {"revoked"},
		"since": {"2026-08-26T10:00:00Z"}, "until": {"2026-08-27T10:00:00Z"}, "limit": {"25"},
	}
	query, err := ParseTrustDecisionQuery("team-a", values)
	if err != nil {
		t.Fatal(err)
	}
	if query.Namespace != "team-a" || query.RunID != "demo-a" || query.AgentIdentity != "agent-a" || query.SessionID != "session-a" || query.Plane != "memory" || query.Outcome != "revoked" || query.Limit != 25 || query.Since.IsZero() || query.Until.IsZero() {
		t.Fatalf("query = %#v", query)
	}
	for _, values := range []url.Values{{"since": {"yesterday"}}, {"until": {"tomorrow"}}, {"limit": {"many"}}} {
		if _, err := ParseTrustDecisionQuery("team-a", values); !errors.Is(err, ErrInvalidTrustDecisionQuery) {
			t.Fatalf("ParseTrustDecisionQuery(%v) error = %v", values, err)
		}
	}
}

func trustDecisionAuditEntry(t *testing.T, id string, timestamp time.Time, plane, outcome, identity, session string, runIDs ...string) AuditEntry {
	return trustDecisionAuditEntryForWorkspace(t, id, "team-a", timestamp, plane, outcome, identity, session, runIDs...)
}

func trustDecisionAuditEntryForWorkspace(t *testing.T, id, workspace string, timestamp time.Time, plane, outcome, identity, session string, runIDs ...string) AuditEntry {
	t.Helper()
	runID := ""
	if len(runIDs) > 0 {
		runID = runIDs[0]
	}
	decision := TrustDecisionV1{
		SchemaVersion: 1, ID: id, Timestamp: timestamp, Plane: plane, Outcome: outcome, ReasonCode: "authorized", Workspace: workspace,
		RunID:         runID,
		Request:       TrustDecisionRequest{Operation: "invoke", TargetRef: TrustDecisionResourceRef{Kind: "Tool", Name: "target"}},
		Authorization: TrustDecisionAuthorization{Identity: "allowed", Role: "allowed", Grant: "allowed", Policy: "allowed"},
		Execution:     TrustDecisionExecution{Status: "not_started"},
	}
	details, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	return AuditEntry{ID: id, Timestamp: timestamp, Workspace: workspace, AgentIdentity: identity, SessionID: session, EventType: AuditToolCall, Details: details}
}
