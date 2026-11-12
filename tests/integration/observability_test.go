// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/observability"
)

// --- FR-0700: PII Redaction ---

func TestObs_PIIRedaction_Email(t *testing.T) {
	r := observability.NewRedactor(observability.DefaultRedactionConfig())
	input := "Contact john.doe@example.com for details"
	got := r.Redact(observability.FieldToolResult, input)
	if got == input {
		t.Fatal("email was not redacted")
	}
	if !contains(got, "[REDACTED:email]") {
		t.Fatalf("expected [REDACTED:email], got: %s", got)
	}
}

func TestObs_PIIRedaction_SSN(t *testing.T) {
	r := observability.NewRedactor(observability.DefaultRedactionConfig())
	input := "SSN: 123-45-6789"
	got := r.Redact(observability.FieldMemoryValue, input)
	if !contains(got, "[REDACTED:ssn]") {
		t.Fatalf("expected [REDACTED:ssn], got: %s", got)
	}
}

func TestObs_PIIRedaction_CreditCard(t *testing.T) {
	r := observability.NewRedactor(observability.DefaultRedactionConfig())
	input := "Card: 4111-1111-1111-1111"
	got := r.Redact(observability.FieldToolArgs, input)
	if !contains(got, "[REDACTED:credit_card]") {
		t.Fatalf("expected [REDACTED:credit_card], got: %s", got)
	}
}

func TestObs_PIIRedaction_StrictMode(t *testing.T) {
	cfg := &observability.RedactionConfig{
		Level:          observability.RedactionStrict,
		Rules:          observability.DefaultRules(),
		FieldOverrides: make(map[observability.RedactableField]observability.RedactionLevel),
	}
	r := observability.NewRedactor(cfg)
	got := r.Redact(observability.FieldReasoningContent, "Some sensitive text")
	if got != "[REDACTED]" {
		t.Fatalf("expected [REDACTED], got: %s", got)
	}
}

func TestObs_PIIRedaction_NoneMode(t *testing.T) {
	cfg := &observability.RedactionConfig{
		Level:          observability.RedactionNone,
		Rules:          observability.DefaultRules(),
		FieldOverrides: make(map[observability.RedactableField]observability.RedactionLevel),
	}
	r := observability.NewRedactor(cfg)
	input := "john.doe@example.com"
	got := r.Redact(observability.FieldToolResult, input)
	if got != input {
		t.Fatalf("expected no redaction, got: %s", got)
	}
}

// --- FR-0701: Cost Attribution ---

func TestObs_CostAttribution(t *testing.T) {
	rec := observability.CostRecord{
		Workspace:        "ws-1",
		AgentIdentity:    "agent-1",
		AgentNamespace:   "default",
		SessionID:        "sess-1",
		OperationType:    observability.CostOpModelInference,
		ModelName:        "gpt-4o",
		ModelProvider:    "openai",
		PromptTokens:     1000,
		CompletionTokens: 500,
		TotalTokens:      1500,
		CostUnits:        observability.CostUnit(4500), // 4.5 millicents
		Timestamp:        time.Now(),
		TraceID:          "trace-abc",
	}

	// Verify all attribution fields are populated.
	if rec.Workspace == "" || rec.AgentIdentity == "" || rec.SessionID == "" {
		t.Fatal("cost record missing attribution fields")
	}
	if rec.CostUnits <= 0 {
		t.Fatal("cost units should be positive")
	}

	// Verify JSON marshaling preserves fields.
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded observability.CostRecord
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Workspace != "ws-1" || decoded.ModelName != "gpt-4o" {
		t.Fatalf("decoded record mismatch: %+v", decoded)
	}
}

// --- FR-0702: Audit Log ---

func TestObs_AuditLog_AppendAndQuery(t *testing.T) {
	store := &memAuditStore{entries: make([]observability.AuditEntry, 0)}
	log := observability.NewCognitiveAuditLog(store)

	entry := observability.AuditEntry{
		ID:            "e1",
		Timestamp:     time.Now(),
		AgentIdentity: "agent-1",
		Workspace:     "ws-1",
		EventType:     observability.AuditToolCall,
		Verb:          "invoke",
		SubjectKind:   "Tool",
		SubjectRef:    "http-tool",
	}
	if err := log.RecordBeforeExec(context.Background(), entry); err != nil {
		t.Fatalf("record: %v", err)
	}

	results, err := store.Query(context.Background(), observability.AuditFilter{
		Workspace: "ws-1",
		EventType: observability.AuditToolCall,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].SubjectRef != "http-tool" {
		t.Fatalf("unexpected subject: %s", results[0].SubjectRef)
	}
}

// --- FR-0703: CloudEvents ---

func TestObs_CloudEvents_Format(t *testing.T) {
	data := map[string]string{"key": "value"}
	ev, err := observability.NewCloudEvent(observability.EventTypeToolCall, "kognition/test", data)
	if err != nil {
		t.Fatalf("create cloud event: %v", err)
	}

	if ev.SpecVersion != "1.0" {
		t.Fatalf("expected specversion 1.0, got %s", ev.SpecVersion)
	}
	if ev.Type != observability.EventTypeToolCall {
		t.Fatalf("expected type %s, got %s", observability.EventTypeToolCall, ev.Type)
	}
	if ev.Source != "kognition/test" {
		t.Fatalf("expected source kognition/test, got %s", ev.Source)
	}
	if ev.ID == "" {
		t.Fatal("event ID should not be empty")
	}
	if ev.Time.IsZero() {
		t.Fatal("event time should not be zero")
	}
	if ev.DataContentType != "application/json" {
		t.Fatalf("expected application/json, got %s", ev.DataContentType)
	}

	// Verify data payload.
	var parsed map[string]string
	if err := json.Unmarshal(ev.Data, &parsed); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if parsed["key"] != "value" {
		t.Fatalf("data mismatch: %+v", parsed)
	}
}

// --- FR-0704: Retention ---

func TestObs_Retention_SweepResult(t *testing.T) {
	// Verify SweepResult structure.
	res := observability.SweepResult{
		SpansDeleted:       10,
		AuditLogsDeleted:   5,
		CostRecordsDeleted: 3,
		EventsDeleted:      2,
	}
	total := res.SpansDeleted + res.AuditLogsDeleted + res.CostRecordsDeleted + res.EventsDeleted
	if total != 20 {
		t.Fatalf("expected 20 total deleted, got %d", total)
	}
}

// --- helpers ---

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// memAuditStore is an in-memory AuditStore for testing.
type memAuditStore struct {
	entries []observability.AuditEntry
}

func (s *memAuditStore) Append(_ context.Context, entry observability.AuditEntry) error {
	s.entries = append(s.entries, entry)
	return nil
}

func (s *memAuditStore) AppendBatch(_ context.Context, entries []observability.AuditEntry) error {
	s.entries = append(s.entries, entries...)
	return nil
}

func (s *memAuditStore) Query(_ context.Context, filter observability.AuditFilter) ([]observability.AuditEntry, error) {
	var result []observability.AuditEntry
	for _, e := range s.entries {
		if filter.Workspace != "" && e.Workspace != filter.Workspace {
			continue
		}
		if filter.EventType != "" && e.EventType != filter.EventType {
			continue
		}
		if filter.AgentIdentity != "" && e.AgentIdentity != filter.AgentIdentity {
			continue
		}
		result = append(result, e)
	}
	return result, nil
}
