// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// makeMemoryClassWithSummarization builds a MemoryClass with summarization
// enabled at the given threshold (number of active records that triggers a
// merge).
func makeMemoryClassWithSummarization(name string, threshold int32) v1alpha1.MemoryClass {
	return v1alpha1.MemoryClass{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "MemoryClass"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name: name,
			UID:  "mc-sum-uid-" + name,
		},
		Spec: v1alpha1.MemoryClassSpec{
			BackendType:   "pgvector",
			ReclaimPolicy: v1alpha1.ReclaimDelete,
			SummarizationConfig: &v1alpha1.MemorySummarizationConfig{
				Enabled:          true,
				ThresholdRecords: threshold,
			},
		},
	}
}

// storeMemory persists a Memory resource into the test store.
func storeMemory(t *testing.T, s *memStore, mem v1alpha1.Memory) {
	t.Helper()
	data, _ := json.Marshal(mem)
	if err := s.Create(context.Background(), memoryKind, mem.ObjectMeta.Namespace, mem.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store Memory %s/%s: %v", mem.Namespace, mem.Name, err)
	}
}

// makeMemoryRecordForMemory builds a MemoryRecord referencing the named Memory
// with the given content and a full provenance timestamp.
func makeMemoryRecordForMemory(ns, name, memoryName, content string, ts time.Time) v1alpha1.MemoryRecord {
	return v1alpha1.MemoryRecord{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: memoryRecordKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "mrm-uid-" + name,
		},
		Spec: v1alpha1.MemoryRecordSpec{
			MemoryRef: v1alpha1.LocalObjectReference{Name: memoryName},
			Content:   content,
			Provenance: &v1alpha1.MemoryProvenance{
				SourceAgentIdentity: "agent-identity-01",
				SessionID:           "sess-summarization",
				ReasoningStepID:     "step-" + name,
				Timestamp:           ts,
			},
		},
	}
}

// getMemory fetches and unmarshals a Memory from the test store.
func getMemory(t *testing.T, s *memStore, ns, name string) v1alpha1.Memory {
	t.Helper()
	raw, err := s.Get(context.Background(), memoryKind, ns, name)
	if err != nil {
		t.Fatalf("get Memory %s/%s: %v", ns, name, err)
	}
	var mem v1alpha1.Memory
	if err := json.Unmarshal(raw, &mem); err != nil {
		t.Fatalf("unmarshal Memory: %v", err)
	}
	return mem
}

// reconcileMemory invokes the MemorySummarizationReconciler for a single Memory.
func reconcileMemory(t *testing.T, r *MemorySummarizationReconciler, ns, name string) {
	t.Helper()
	req := Request{Namespace: ns, Name: name, Kind: memoryKind}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile Memory %s/%s: %v", ns, name, err)
	}
}

// ---------------------------------------------------------------------------
// TC-0043 — Summarization triggered when threshold is exceeded
// ---------------------------------------------------------------------------

// TestMemorySummarization_TC0043_TriggersWhenThresholdExceeded fills an
// episodic Memory past the summarization threshold and verifies that the
// controller merges the active records into a single summary MemoryRecord
// while retaining the original content within the summary.
func TestMemorySummarization_TC0043_TriggersWhenThresholdExceeded(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemorySummarizationReconciler(s, logger)

	const (
		ns        = "default"
		memName   = "episodic-mem"
		claimName = "episodic-claim"
		className = "episodic-class"
	)

	// MemoryClass: threshold = 3, enabled = true.
	storeMemoryClass(t, s, makeMemoryClassWithSummarization(className, 3))

	// MemoryClaim references the class.
	storeMemoryClaim(t, s, makeMemoryClaim(ns, claimName, className, v1alpha1.MemoryTypeEpisodic))

	// Memory references the claim.
	storeMemory(t, s, makeMemory(ns, memName, claimName, v1alpha1.MemoryTypeEpisodic))

	// 4 records — one above the threshold of 3.
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, content := range []string{"fact-alpha", "fact-beta", "fact-gamma", "fact-delta"} {
		rec := makeMemoryRecordForMemory(ns, content, memName, content, base.Add(time.Duration(i)*time.Minute))
		storeMemoryRecord(t, s, rec)
	}

	reconcileMemory(t, r, ns, memName)

	// After summarization: exactly 1 active record (the summary).
	active, err := ListActiveMemoryRecords(ctx, s, ns, memName)
	if err != nil {
		t.Fatalf("ListActiveMemoryRecords: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active record after summarization, got %d", len(active))
	}

	// The single active record is the summary.
	summary := active[0]
	if summary.Spec.Metadata["summary"] != "true" {
		t.Errorf("expected summary metadata flag, got %v", summary.Spec.Metadata)
	}

	// Summary content must contain all original facts.
	for _, fact := range []string{"fact-alpha", "fact-beta", "fact-gamma", "fact-delta"} {
		if !strings.Contains(summary.Spec.Content, fact) {
			t.Errorf("summary content missing %q; got: %s", fact, summary.Spec.Content)
		}
	}

	// Memory.Status.RecordCount updated to 1.
	mem := getMemory(t, s, ns, memName)
	if mem.Status.RecordCount != 1 {
		t.Errorf("expected Memory.Status.RecordCount=1, got %d", mem.Status.RecordCount)
	}
}

// TestMemorySummarization_TC0043_NoTriggerBelowThreshold verifies that no
// summarization occurs when the active-record count is within the threshold.
func TestMemorySummarization_TC0043_NoTriggerBelowThreshold(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemorySummarizationReconciler(s, logger)

	const (
		ns        = "default"
		memName   = "small-mem"
		claimName = "small-claim"
		className = "episodic-class-small"
	)

	// Threshold = 3; only 2 records → no summarization.
	storeMemoryClass(t, s, makeMemoryClassWithSummarization(className, 3))
	storeMemoryClaim(t, s, makeMemoryClaim(ns, claimName, className, v1alpha1.MemoryTypeEpisodic))
	storeMemory(t, s, makeMemory(ns, memName, claimName, v1alpha1.MemoryTypeEpisodic))

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, content := range []string{"rec-one", "rec-two"} {
		rec := makeMemoryRecordForMemory(ns, content, memName, content, base.Add(time.Duration(i)*time.Minute))
		storeMemoryRecord(t, s, rec)
	}

	reconcileMemory(t, r, ns, memName)

	// Both original records remain active — no summary created.
	active, err := ListActiveMemoryRecords(ctx, s, ns, memName)
	if err != nil {
		t.Fatalf("ListActiveMemoryRecords: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("expected 2 active records (no summarization), got %d", len(active))
	}

	// No record should have the summary metadata flag.
	for _, rec := range active {
		if rec.Spec.Metadata["summary"] == "true" {
			t.Errorf("unexpected summary record found when below threshold: %s", rec.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// TC-0044 — Raw history accessible after summarization
// ---------------------------------------------------------------------------

// TestMemorySummarization_TC0044_RawHistoryAccessible verifies that after
// summarization the original (now PhaseSummarized) records are still
// retrievable via ListAllMemoryRecords, satisfying the raw-history requirement.
func TestMemorySummarization_TC0044_RawHistoryAccessible(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemorySummarizationReconciler(s, logger)

	const (
		ns        = "default"
		memName   = "history-mem"
		claimName = "history-claim"
		className = "episodic-class-history"
	)

	storeMemoryClass(t, s, makeMemoryClassWithSummarization(className, 3))
	storeMemoryClaim(t, s, makeMemoryClaim(ns, claimName, className, v1alpha1.MemoryTypeEpisodic))
	storeMemory(t, s, makeMemory(ns, memName, claimName, v1alpha1.MemoryTypeEpisodic))

	// 4 records → triggers summarization (threshold = 3).
	base := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	for i, content := range []string{"history-a", "history-b", "history-c", "history-d"} {
		rec := makeMemoryRecordForMemory(ns, content, memName, content, base.Add(time.Duration(i)*time.Minute))
		storeMemoryRecord(t, s, rec)
	}

	reconcileMemory(t, r, ns, memName)

	// ListActiveMemoryRecords → 1 record (the summary).
	active, err := ListActiveMemoryRecords(ctx, s, ns, memName)
	if err != nil {
		t.Fatalf("ListActiveMemoryRecords: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active record after summarization, got %d", len(active))
	}

	// ListAllMemoryRecords → 5 records (4 original + 1 summary).
	all, err := ListAllMemoryRecords(ctx, s, ns, memName)
	if err != nil {
		t.Fatalf("ListAllMemoryRecords: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("expected 5 total records (4 originals + 1 summary), got %d", len(all))
	}

	// Exactly 4 records should have PhaseSummarized.
	summarizedCount := 0
	for _, rec := range all {
		if rec.Status.Phase == v1alpha1.PhaseSummarized {
			summarizedCount++
		}
	}
	if summarizedCount != 4 {
		t.Errorf("expected 4 records with PhaseSummarized, got %d", summarizedCount)
	}
}

// TestMemorySummarization_ReconcileFromMemoryRecordEvent verifies that the
// summarization reconciler can be invoked from a MemoryRecord event and still
// summarize the owning Memory.
func TestMemorySummarization_ReconcileFromMemoryRecordEvent(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemorySummarizationReconciler(s, logger)

	const (
		ns        = "default"
		memName   = "record-trigger-mem"
		claimName = "record-trigger-claim"
		className = "record-trigger-class"
	)

	storeMemoryClass(t, s, makeMemoryClassWithSummarization(className, 2))
	storeMemoryClaim(t, s, makeMemoryClaim(ns, claimName, className, v1alpha1.MemoryTypeEpisodic))
	storeMemory(t, s, makeMemory(ns, memName, claimName, v1alpha1.MemoryTypeEpisodic))

	base := time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, name := range []string{"r-a", "r-b", "r-c"} {
		rec := makeMemoryRecordForMemory(ns, name, memName, name, base.Add(time.Duration(i)*time.Minute))
		storeMemoryRecord(t, s, rec)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "r-c", Kind: memoryRecordKind}); err != nil {
		t.Fatalf("Reconcile from MemoryRecord event: %v", err)
	}

	active, err := ListActiveMemoryRecords(ctx, s, ns, memName)
	if err != nil {
		t.Fatalf("ListActiveMemoryRecords: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active record after record-triggered summarization, got %d", len(active))
	}
	if active[0].Spec.Metadata["summary"] != "true" {
		t.Fatalf("expected active record to be summary, metadata=%v", active[0].Spec.Metadata)
	}
}

// TestMemorySummarization_NoOpStatusUpdateAvoidsWrite verifies that a second
// identical reconciliation does not update Memory resourceVersion.
func TestMemorySummarization_NoOpStatusUpdateAvoidsWrite(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemorySummarizationReconciler(s, logger)

	const (
		ns        = "default"
		memName   = "no-op-mem"
		claimName = "no-op-claim"
		className = "no-op-class"
	)

	storeMemoryClass(t, s, makeMemoryClassWithSummarization(className, 10))
	storeMemoryClaim(t, s, makeMemoryClaim(ns, claimName, className, v1alpha1.MemoryTypeEpisodic))
	storeMemory(t, s, makeMemory(ns, memName, claimName, v1alpha1.MemoryTypeEpisodic))

	reconcileMemory(t, r, ns, memName)
	first := getMemory(t, s, ns, memName)

	reconcileMemory(t, r, ns, memName)
	second := getMemory(t, s, ns, memName)

	if second.ResourceVersion != first.ResourceVersion {
		t.Fatalf("expected no-op reconcile to keep resourceVersion %d, got %d", first.ResourceVersion, second.ResourceVersion)
	}
}

// TestMemorySummarization_DeletedMemoryRecordTriggerIsIgnored ensures a
// MemoryRecord delete event (record no longer exists) is treated as no-op.
func TestMemorySummarization_DeletedMemoryRecordTriggerIsIgnored(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemorySummarizationReconciler(s, logger)

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "missing-record", Kind: memoryRecordKind}); err != nil {
		t.Fatalf("expected no error for deleted/missing MemoryRecord trigger, got %v", err)
	}
}

func TestMemorySummarization_RTBFTombstonesAreNotActive(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewMemorySummarizationReconciler(s, zaptest.NewLogger(t))

	const (
		ns        = "default"
		memName   = "rtbf-filter-mem"
		claimName = "rtbf-filter-claim"
		className = "rtbf-filter-class"
	)

	storeMemoryClass(t, s, makeMemoryClassWithSummarization(className, 1))
	storeMemoryClaim(t, s, makeMemoryClaim(ns, claimName, className, v1alpha1.MemoryTypeEpisodic))
	storeMemory(t, s, makeMemory(ns, memName, claimName, v1alpha1.MemoryTypeEpisodic))

	base := time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC)
	activeRecord := makeMemoryRecordForMemory(ns, "active-record", memName, "retain me", base)
	storeMemoryRecord(t, s, activeRecord)

	deletedRecord := makeMemoryRecordForMemory(ns, "deleted-record", memName, "[TOMBSTONED]", base.Add(time.Minute))
	deletedRecord.Status.Phase = v1alpha1.PhaseDeleted
	deletedRecord.Spec.Metadata = map[string]string{"rtbfTombstoned": "true"}
	storeMemoryRecord(t, s, deletedRecord)

	reconcileMemory(t, r, ns, memName)

	active, err := ListActiveMemoryRecords(ctx, s, ns, memName)
	if err != nil {
		t.Fatalf("ListActiveMemoryRecords: %v", err)
	}
	if len(active) != 1 || active[0].Name != activeRecord.Name {
		t.Fatalf("expected only %q to remain active, got %+v", activeRecord.Name, active)
	}
	if active[0].Status.Phase == v1alpha1.PhaseSummarized {
		t.Fatal("expected active record not to be summarized by an RTBF tombstone")
	}
	if got := getMemory(t, s, ns, memName).Status.RecordCount; got != 1 {
		t.Fatalf("expected Memory.Status.RecordCount=1, got %d", got)
	}
}

// TestMemorySummarization_MissingClaimSkipsWithStatus verifies that when
// MemoryClaim cannot be resolved, reconciliation does not fail and still
// updates Memory.Status.RecordCount from the active set.
func TestMemorySummarization_MissingClaimSkipsWithStatus(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemorySummarizationReconciler(s, logger)

	const (
		ns      = "default"
		memName = "missing-claim-mem"
	)

	// Memory references a claim that does not exist.
	storeMemory(t, s, makeMemory(ns, memName, "missing-claim", v1alpha1.MemoryTypeEpisodic))

	base := time.Date(2024, 8, 1, 0, 0, 0, 0, time.UTC)
	for i, name := range []string{"mc-a", "mc-b"} {
		rec := makeMemoryRecordForMemory(ns, name, memName, name, base.Add(time.Duration(i)*time.Minute))
		storeMemoryRecord(t, s, rec)
	}

	reconcileMemory(t, r, ns, memName)

	active, err := ListActiveMemoryRecords(ctx, s, ns, memName)
	if err != nil {
		t.Fatalf("ListActiveMemoryRecords: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("expected active records unchanged when claim missing, got %d", len(active))
	}

	mem := getMemory(t, s, ns, memName)
	if mem.Status.RecordCount != 2 {
		t.Fatalf("expected Memory.Status.RecordCount=2, got %d", mem.Status.RecordCount)
	}
}

// TestMemorySummarization_DisabledConfigDoesNotSummarize verifies that an
// explicitly disabled SummarizationConfig prevents compaction even above threshold.
func TestMemorySummarization_DisabledConfigDoesNotSummarize(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewMemorySummarizationReconciler(s, logger)

	const (
		ns        = "default"
		memName   = "disabled-config-mem"
		claimName = "disabled-config-claim"
		className = "disabled-config-class"
	)

	mc := makeMemoryClassWithSummarization(className, 1)
	mc.Spec.SummarizationConfig.Enabled = false
	storeMemoryClass(t, s, mc)
	storeMemoryClaim(t, s, makeMemoryClaim(ns, claimName, className, v1alpha1.MemoryTypeEpisodic))
	storeMemory(t, s, makeMemory(ns, memName, claimName, v1alpha1.MemoryTypeEpisodic))

	base := time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC)
	for i, name := range []string{"d-a", "d-b", "d-c"} {
		rec := makeMemoryRecordForMemory(ns, name, memName, name, base.Add(time.Duration(i)*time.Minute))
		storeMemoryRecord(t, s, rec)
	}

	reconcileMemory(t, r, ns, memName)

	active, err := ListActiveMemoryRecords(ctx, s, ns, memName)
	if err != nil {
		t.Fatalf("ListActiveMemoryRecords: %v", err)
	}
	if len(active) != 3 {
		t.Fatalf("expected all records to remain active when summarization disabled, got %d", len(active))
	}
}
