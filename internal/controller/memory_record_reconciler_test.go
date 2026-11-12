// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type memoryRecordGetErrorStore struct {
	store.Store
	err error
}

func (s *memoryRecordGetErrorStore) Get(context.Context, string, string, string) ([]byte, error) {
	return nil, s.err
}

func TestMemoryRecordReconcilerNotFoundIsComplete(t *testing.T) {
	r := NewMemoryRecordReconciler(&memoryRecordGetErrorStore{
		Store: newMemStore(),
		err:   fmt.Errorf("wrapped missing record: %w", store.ErrNotFound),
	}, zaptest.NewLogger(t))
	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "deleted"})
	if err != nil || result != (Result{}) {
		t.Fatalf("result = %+v, error = %v, want completed not-found reconciliation", result, err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// makeMemoryRecord builds a MemoryRecord with the supplied provenance fields.
func makeMemoryRecord(ns, name string, prov *v1alpha1.MemoryProvenance) v1alpha1.MemoryRecord {
	return v1alpha1.MemoryRecord{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: memoryRecordKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "mr-uid-" + name,
		},
		Spec: v1alpha1.MemoryRecordSpec{
			MemoryRef:  v1alpha1.LocalObjectReference{Name: "mem-" + name},
			Content:    "test content for " + name,
			Provenance: prov,
		},
	}
}

// fullProvenance returns a MemoryProvenance with all required fields set.
func fullProvenance(sessionID, reasoningStepID string) *v1alpha1.MemoryProvenance {
	return &v1alpha1.MemoryProvenance{
		SourceAgentIdentity: "agent-identity-01",
		SessionID:           sessionID,
		ReasoningStepID:     reasoningStepID,
		GrantID:             "grant-01",
	}
}

func storeMemoryRecord(t *testing.T, s *memStore, rec v1alpha1.MemoryRecord) {
	t.Helper()
	data, _ := json.Marshal(rec)
	if err := s.Create(context.Background(), memoryRecordKind, rec.Namespace, rec.Name, data); err != nil {
		t.Fatalf("store memory record: %v", err)
	}
}

func reconcileRecord(t *testing.T, r *MemoryRecordReconciler, ns, name string) {
	t.Helper()
	req := Request{Namespace: ns, Name: name, Kind: memoryRecordKind}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile(%s/%s): %v", ns, name, err)
	}
}

// ---------------------------------------------------------------------------
// TC-0041 — Session read-your-writes
// ---------------------------------------------------------------------------

// TestMemoryRecord_TC0041_SessionReadYourWrites verifies that a MemoryRecord
// written with a specific SessionID is immediately visible via
// ListRecordsBySession within the same session (synchronous store guarantee).
func TestMemoryRecord_TC0041_SessionReadYourWrites(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemoryRecordReconciler(s, logger)

	const (
		ns        = "default"
		name      = "rec-session-01"
		sessionID = "sess-abc"
	)

	rec := makeMemoryRecord(ns, name, fullProvenance(sessionID, "step-01"))
	storeMemoryRecord(t, s, rec)
	reconcileRecord(t, r, ns, name)

	results, err := ListRecordsBySession(ctx, s, ns, sessionID)
	if err != nil {
		t.Fatalf("ListRecordsBySession: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 record, got %d", len(results))
	}
	if results[0].Name != name {
		t.Errorf("expected record %q, got %q", name, results[0].Name)
	}
}

// TestMemoryRecord_TC0041_ListBySessionFiltersCorrectly ensures that
// ListRecordsBySession does not return records belonging to a different session.
func TestMemoryRecord_TC0041_ListBySessionFiltersCorrectly(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemoryRecordReconciler(s, logger)

	const ns = "default"

	recA := makeMemoryRecord(ns, "rec-a", fullProvenance("sess-A", "step-1"))
	recB := makeMemoryRecord(ns, "rec-b", fullProvenance("sess-B", "step-2"))
	storeMemoryRecord(t, s, recA)
	storeMemoryRecord(t, s, recB)
	reconcileRecord(t, r, ns, "rec-a")
	reconcileRecord(t, r, ns, "rec-b")

	results, err := ListRecordsBySession(ctx, s, ns, "sess-A")
	if err != nil {
		t.Fatalf("ListRecordsBySession: %v", err)
	}
	if len(results) != 1 || results[0].Name != "rec-a" {
		t.Errorf("expected only rec-a; got %+v", results)
	}
}

// ---------------------------------------------------------------------------
// TC-0042 — Provenance metadata populated and validated
// ---------------------------------------------------------------------------

// TestMemoryRecord_TC0042_ProvenanceRoundTrip verifies that all provenance
// fields survive a store → reconcile → GetRecordProvenance round-trip.
func TestMemoryRecord_TC0042_ProvenanceRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemoryRecordReconciler(s, logger)

	const (
		ns              = "default"
		name            = "rec-provenance-01"
		sessionID       = "sess-roundtrip"
		reasoningStepID = "step-roundtrip"
	)

	prov := fullProvenance(sessionID, reasoningStepID)
	rec := makeMemoryRecord(ns, name, prov)
	storeMemoryRecord(t, s, rec)
	reconcileRecord(t, r, ns, name)

	got, err := GetRecordProvenance(ctx, s, ns, name)
	if err != nil {
		t.Fatalf("GetRecordProvenance: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil provenance")
	}
	if got.SourceAgentIdentity != prov.SourceAgentIdentity {
		t.Errorf("SourceAgentIdentity: want %q, got %q", prov.SourceAgentIdentity, got.SourceAgentIdentity)
	}
	if got.SessionID != sessionID {
		t.Errorf("SessionID: want %q, got %q", sessionID, got.SessionID)
	}
	if got.ReasoningStepID != reasoningStepID {
		t.Errorf("ReasoningStepID: want %q, got %q", reasoningStepID, got.ReasoningStepID)
	}
	if got.GrantID != prov.GrantID {
		t.Errorf("GrantID: want %q, got %q", prov.GrantID, got.GrantID)
	}
	// Timestamp must have been auto-stamped by the reconciler.
	if got.Timestamp.IsZero() {
		t.Error("expected Timestamp to be auto-stamped, got zero value")
	}
}

// TestMemoryRecord_TC0042_MissingProvenanceReadyFalse verifies that a
// MemoryRecord with no provenance set gets ConditionReady=False.
func TestMemoryRecord_TC0042_MissingProvenanceReadyFalse(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemoryRecordReconciler(s, logger)

	const (
		ns   = "default"
		name = "rec-no-prov"
	)

	rec := makeMemoryRecord(ns, name, nil)
	storeMemoryRecord(t, s, rec)
	reconcileRecord(t, r, ns, name)

	raw, err := s.Get(ctx, memoryRecordKind, ns, name)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	var updated v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cond := findConditionInSlice(updated.Status.Conditions, "Ready")
	if cond == nil {
		t.Fatal("expected ConditionReady to be set, got nil")
	}
	if cond.Status != "False" {
		t.Errorf("expected ConditionReady=False, got %q", cond.Status)
	}
}

// TestMemoryRecord_TC0042_MissingSessionIDReadyFalse verifies that a record
// with partial provenance (missing SessionID) also gets ConditionReady=False.
func TestMemoryRecord_TC0042_MissingSessionIDReadyFalse(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemoryRecordReconciler(s, logger)

	const (
		ns   = "default"
		name = "rec-no-session"
	)

	prov := &v1alpha1.MemoryProvenance{
		SourceAgentIdentity: "agent-01",
		// SessionID intentionally absent
		ReasoningStepID: "step-01",
	}
	rec := makeMemoryRecord(ns, name, prov)
	storeMemoryRecord(t, s, rec)
	reconcileRecord(t, r, ns, name)

	raw, err := s.Get(ctx, memoryRecordKind, ns, name)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	var updated v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cond := findConditionInSlice(updated.Status.Conditions, "Ready")
	if cond == nil {
		t.Fatal("expected ConditionReady to be set, got nil")
	}
	if cond.Status != "False" {
		t.Errorf("expected ConditionReady=False, got %q", cond.Status)
	}
}

// TestMemoryRecord_TC0042_ListByReasoningStep verifies the reasoning-step
// query API (T-0054).
func TestMemoryRecord_TC0042_ListByReasoningStep(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemoryRecordReconciler(s, logger)

	const ns = "default"

	recA := makeMemoryRecord(ns, "rs-a", fullProvenance("sess-x", "step-TARGET"))
	recB := makeMemoryRecord(ns, "rs-b", fullProvenance("sess-y", "step-OTHER"))
	storeMemoryRecord(t, s, recA)
	storeMemoryRecord(t, s, recB)
	reconcileRecord(t, r, ns, "rs-a")
	reconcileRecord(t, r, ns, "rs-b")

	results, err := ListRecordsByReasoningStep(ctx, s, ns, "step-TARGET")
	if err != nil {
		t.Fatalf("ListRecordsByReasoningStep: %v", err)
	}
	if len(results) != 1 || results[0].Name != "rs-a" {
		t.Errorf("expected only rs-a; got %+v", results)
	}
}

// TestMemoryRecord_PreservesSummarizedPhase ensures provenance reconciliation
// does not revert a record that was already summarized back to PhaseReady.
func TestMemoryRecord_PreservesSummarizedPhase(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemoryRecordReconciler(s, logger)

	const (
		ns   = "default"
		name = "rec-summarized"
	)

	rec := makeMemoryRecord(ns, name, fullProvenance("sess-keep", "step-keep"))
	rec.Status.Phase = v1alpha1.PhaseSummarized
	storeMemoryRecord(t, s, rec)

	reconcileRecord(t, r, ns, name)

	raw, err := s.Get(ctx, memoryRecordKind, ns, name)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	var updated v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if updated.Status.Phase != v1alpha1.PhaseSummarized {
		t.Fatalf("expected phase %q, got %q", v1alpha1.PhaseSummarized, updated.Status.Phase)
	}
}

// TC-0045: memory write path redacts PII according to policy.
// TestMemoryRecord_TC0045_RedactsPII verifies that email and phone patterns
// are redacted at memory-write reconcile time.
func TestMemoryRecord_TC0045_RedactsPII(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemoryRecordReconciler(s, logger)

	const (
		ns   = "default"
		name = "rec-pii"
	)

	rec := makeMemoryRecord(ns, name, fullProvenance("sess-pii", "step-pii"))
	rec.Spec.Content = "Reach me at alice@example.com or +1 (555) 123-4567"
	storeMemoryRecord(t, s, rec)

	reconcileRecord(t, r, ns, name)

	raw, err := s.Get(ctx, memoryRecordKind, ns, name)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	var updated v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if updated.Spec.Content == rec.Spec.Content {
		t.Fatalf("expected PII content to be redacted, got unchanged content")
	}
	if updated.Spec.Content != "Reach me at [REDACTED_EMAIL] or [REDACTED_PHONE]" {
		t.Fatalf("unexpected redacted content: %q", updated.Spec.Content)
	}
	if updated.Spec.Metadata["piiRedacted"] != "true" {
		t.Fatalf("expected piiRedacted metadata=true, got %q", updated.Spec.Metadata["piiRedacted"])
	}
}

func TestMemoryRecord_TC0045_WorkspaceRulesDisableRedaction(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemoryRecordReconciler(s, logger)

	const (
		ns   = "default"
		name = "rec-pii-none"
	)

	ws := v1alpha1.Workspace{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: workspaceKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name: ns,
			Annotations: map[string]string{
				"kognition.dev/memory-pii-rules": "none",
			},
		},
	}
	wsData, _ := json.Marshal(ws)
	if err := s.Create(ctx, workspaceKind, "", ns, wsData); err != nil {
		t.Fatalf("store Workspace: %v", err)
	}

	rec := makeMemoryRecord(ns, name, fullProvenance("sess-pii-none", "step-pii-none"))
	rec.Spec.Content = "Contact bob@example.com or +1 555 222 3333"
	storeMemoryRecord(t, s, rec)

	reconcileRecord(t, r, ns, name)

	raw, err := s.Get(ctx, memoryRecordKind, ns, name)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	var updated v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if updated.Spec.Content != rec.Spec.Content {
		t.Fatalf("expected content unchanged with workspace pii rules=none, got %q", updated.Spec.Content)
	}
}

func TestMemoryRecord_TC0045_WorkspaceRulesEmailOnly(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemoryRecordReconciler(s, logger)

	const (
		ns   = "default"
		name = "rec-pii-email-only"
	)

	ws := v1alpha1.Workspace{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: workspaceKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name: ns,
			Annotations: map[string]string{
				"kognition.dev/memory-pii-rules": "email",
			},
		},
	}
	wsData, _ := json.Marshal(ws)
	if err := s.Create(ctx, workspaceKind, "", ns, wsData); err != nil {
		t.Fatalf("store Workspace: %v", err)
	}

	rec := makeMemoryRecord(ns, name, fullProvenance("sess-pii-email", "step-pii-email"))
	rec.Spec.Content = "Contact bob@example.com or +1 555 222 3333"
	storeMemoryRecord(t, s, rec)

	reconcileRecord(t, r, ns, name)

	raw, err := s.Get(ctx, memoryRecordKind, ns, name)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	var updated v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if updated.Spec.Content != "Contact [REDACTED_EMAIL] or +1 555 222 3333" {
		t.Fatalf("unexpected content with email-only redaction: %q", updated.Spec.Content)
	}
}

// findConditionInSlice returns the condition with the given type, or nil.
func findConditionInSlice(conditions []v1alpha1.Condition, condType string) *v1alpha1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
