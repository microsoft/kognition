// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type appendEventFailStore struct {
	*memStore
}

type appendEventFailOnceStore struct {
	*memStore
	failures int
}

type rtbfConflictOnceStore struct {
	store.Store
	target     string
	conflicted bool
}

type rtbfPersistentConflictStore struct {
	store.Store
	updates int
}

func (f *appendEventFailStore) AppendEvent(_ context.Context, _, _, _ string, _ store.InstanceEvent) error {
	return errors.New("append event failure")
}

func (f *appendEventFailOnceStore) AppendEvent(ctx context.Context, kind, namespace, name string, event store.InstanceEvent) error {
	if f.failures > 0 {
		f.failures--
		return errors.New("transient append event failure")
	}
	return f.memStore.AppendEvent(ctx, kind, namespace, name, event)
}

func (s *rtbfConflictOnceStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == memoryRecordKind && name == s.target && !s.conflicted {
		s.conflicted = true
		raw, err := s.Store.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var latest v1alpha1.MemoryRecord
		if err := json.Unmarshal(raw, &latest); err != nil {
			return err
		}
		if latest.Spec.Metadata == nil {
			latest.Spec.Metadata = map[string]string{}
		}
		latest.Spec.Metadata["concurrent"] = "preserved"
		updated, err := json.Marshal(latest)
		if err != nil {
			return err
		}
		if err := s.Store.Update(ctx, kind, namespace, name, updated, latest.ResourceVersion); err != nil {
			return err
		}
		return fmt.Errorf("injected concurrent conflict: %w", store.ErrConflict)
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *rtbfPersistentConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == memoryRecordKind {
		s.updates++
		return fmt.Errorf("injected persistent conflict: %w", store.ErrConflict)
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

// TC-0046: A -> B -> C chain should be tombstoned by RTBF(A).
func TestMemoryRTBF_TC0046_CascadesAcrossDerivedChain(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()

	const (
		ns        = "default"
		memName   = "rtbf-mem"
		claimName = "rtbf-claim"
		className = "rtbf-class"
	)

	storeMemoryClass(t, s, makeMemoryClassWithSummarization(className, 99))
	storeMemoryClaim(t, s, makeMemoryClaim(ns, claimName, className, v1alpha1.MemoryTypeEpisodic))
	storeMemory(t, s, makeMemory(ns, memName, claimName, v1alpha1.MemoryTypeEpisodic))

	base := time.Date(2024, 10, 1, 0, 0, 0, 0, time.UTC)
	recA := makeMemoryRecordForMemory(ns, "rec-a", memName, "seed", base)
	recB := makeMemoryRecordForMemory(ns, "rec-b", memName, "derived from a", base.Add(time.Minute))
	recC := makeMemoryRecordForMemory(ns, "rec-c", memName, "derived from b", base.Add(2*time.Minute))
	recB.Spec.Provenance.DerivedFromRecordNames = []string{"rec-a"}
	recC.Spec.Provenance.DerivedFromRecordNames = []string{"rec-b"}

	storeMemoryRecord(t, s, recA)
	storeMemoryRecord(t, s, recB)
	storeMemoryRecord(t, s, recC)

	tombstoned, err := RightToBeForgotten(ctx, s, ns, "rec-a")
	if err != nil {
		t.Fatalf("RightToBeForgotten: %v", err)
	}
	if len(tombstoned) != 3 {
		t.Fatalf("expected 3 tombstoned records, got %d (%v)", len(tombstoned), tombstoned)
	}

	for _, name := range []string{"rec-a", "rec-b", "rec-c"} {
		raw, err := s.Get(ctx, memoryRecordKind, ns, name)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		var rec v1alpha1.MemoryRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		if rec.Status.Phase != v1alpha1.PhaseDeleted {
			t.Fatalf("expected %s phase Deleted, got %s", name, rec.Status.Phase)
		}
		if rec.Spec.Content != "[TOMBSTONED]" {
			t.Fatalf("expected %s content to be tombstoned, got %q", name, rec.Spec.Content)
		}
		if rec.Spec.Metadata["rtbfTombstoned"] != "true" {
			t.Fatalf("expected %s rtbfTombstoned=true", name)
		}
		evts, err := s.ListEvents(ctx, memoryRecordKind, ns, name)
		if err != nil {
			t.Fatalf("ListEvents %s: %v", name, err)
		}
		if len(evts) == 0 || evts[0].Type != "RTBF_TOMBSTONED" {
			t.Fatalf("expected RTBF audit event for %s, got %+v", name, evts)
		}
		if _, err := time.Parse(time.RFC3339, evts[0].Timestamp); err != nil {
			t.Fatalf("expected RFC3339 RTBF audit timestamp for %s, got %q: %v", name, evts[0].Timestamp, err)
		}
	}
}

func TestMemoryRTBF_CascadesAcrossLegacyMetadataLinks(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()

	const (
		ns        = "default"
		memName   = "rtbf-legacy-mem"
		claimName = "rtbf-legacy-claim"
		className = "rtbf-legacy-class"
	)

	storeMemoryClass(t, s, makeMemoryClassWithSummarization(className, 99))
	storeMemoryClaim(t, s, makeMemoryClaim(ns, claimName, className, v1alpha1.MemoryTypeEpisodic))
	storeMemory(t, s, makeMemory(ns, memName, claimName, v1alpha1.MemoryTypeEpisodic))

	base := time.Date(2024, 10, 2, 0, 0, 0, 0, time.UTC)
	records := []v1alpha1.MemoryRecord{
		makeMemoryRecordForMemory(ns, "source", memName, "seed", base),
		makeMemoryRecordForMemory(ns, "single", memName, "single parent", base.Add(time.Minute)),
		makeMemoryRecordForMemory(ns, "multiple", memName, "multiple parents", base.Add(2*time.Minute)),
		makeMemoryRecordForMemory(ns, "sources", memName, "source list", base.Add(3*time.Minute)),
		makeMemoryRecordForMemory(ns, "cycle", memName, "cycle", base.Add(4*time.Minute)),
		makeMemoryRecordForMemory(ns, "partial", memName, "not derived", base.Add(5*time.Minute)),
	}
	records[1].Spec.Metadata = map[string]string{"derivedFromRecord": " source "}
	records[2].Spec.Metadata = map[string]string{"derivedFromRecords": "other, single "}
	records[3].Spec.Metadata = map[string]string{"sourceRecords": " multiple, another"}
	records[4].Spec.Metadata = map[string]string{"sourceRecords": "sources, cycle"}
	records[5].Spec.Metadata = map[string]string{"derivedFromRecords": "source-copy"}
	for _, record := range records {
		storeMemoryRecord(t, s, record)
	}

	tombstoned, err := RightToBeForgotten(ctx, s, ns, "source")
	if err != nil {
		t.Fatalf("RightToBeForgotten: %v", err)
	}
	want := map[string]bool{"source": true, "single": true, "multiple": true, "sources": true, "cycle": true}
	if len(tombstoned) != len(want) {
		t.Fatalf("tombstoned records = %v, want exactly %v", tombstoned, want)
	}
	for _, name := range tombstoned {
		if !want[name] {
			t.Fatalf("unexpected tombstoned record %q in %v", name, tombstoned)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("missing tombstoned records: %v", want)
	}

	raw, err := s.Get(ctx, memoryRecordKind, ns, "partial")
	if err != nil {
		t.Fatalf("get partial-match record: %v", err)
	}
	var partial v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &partial); err != nil {
		t.Fatalf("unmarshal partial-match record: %v", err)
	}
	if partial.Status.Phase == v1alpha1.PhaseDeleted {
		t.Fatal("partial metadata token must not tombstone an unrelated record")
	}
}

func TestMemoryRTBF_RetriesConflictAndPreservesConcurrentUpdate(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	s := &rtbfConflictOnceStore{Store: baseStore, target: "derived"}

	const (
		ns        = "default"
		memName   = "rtbf-conflict-mem"
		claimName = "rtbf-conflict-claim"
		className = "rtbf-conflict-class"
	)

	storeMemoryClass(t, baseStore, makeMemoryClassWithSummarization(className, 99))
	storeMemoryClaim(t, baseStore, makeMemoryClaim(ns, claimName, className, v1alpha1.MemoryTypeEpisodic))
	storeMemory(t, baseStore, makeMemory(ns, memName, claimName, v1alpha1.MemoryTypeEpisodic))

	base := time.Date(2024, 10, 3, 0, 0, 0, 0, time.UTC)
	source := makeMemoryRecordForMemory(ns, "source", memName, "seed", base)
	derived := makeMemoryRecordForMemory(ns, "derived", memName, "derived", base.Add(time.Minute))
	derived.Spec.Provenance.DerivedFromRecordNames = []string{"source"}
	storeMemoryRecord(t, baseStore, source)
	storeMemoryRecord(t, baseStore, derived)

	tombstoned, err := RightToBeForgotten(ctx, s, ns, source.Name)
	if err != nil {
		t.Fatalf("RightToBeForgotten: %v", err)
	}
	if !s.conflicted {
		t.Fatal("expected injected update conflict")
	}
	if len(tombstoned) != 2 {
		t.Fatalf("tombstoned records = %v, want source and derived", tombstoned)
	}

	raw, err := baseStore.Get(ctx, memoryRecordKind, ns, derived.Name)
	if err != nil {
		t.Fatalf("get derived record: %v", err)
	}
	var persisted v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("unmarshal derived record: %v", err)
	}
	if persisted.Status.Phase != v1alpha1.PhaseDeleted {
		t.Fatalf("derived phase = %s, want Deleted", persisted.Status.Phase)
	}
	if persisted.Spec.Metadata["concurrent"] != "preserved" {
		t.Fatalf("concurrent metadata = %q, want preserved", persisted.Spec.Metadata["concurrent"])
	}
}

func TestMemoryRTBF_StopsAfterPersistentConflicts(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	s := &rtbfPersistentConflictStore{Store: baseStore}

	record := makeMemoryRecordForMemory("default", "source", "rtbf-conflict-mem", "seed", time.Now().UTC())
	storeMemoryRecord(t, baseStore, record)

	_, err := RightToBeForgotten(ctx, s, record.Namespace, record.Name)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("RightToBeForgotten error = %v, want resource version conflict", err)
	}
	if s.updates != 5 {
		t.Fatalf("update attempts = %d, want 5", s.updates)
	}
}

func TestMemoryRTBF_FailsWhenAuditAppendFails(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	s := &appendEventFailStore{memStore: baseStore}

	const (
		ns        = "default"
		memName   = "rtbf-audit-fail-mem"
		claimName = "rtbf-audit-fail-claim"
		className = "rtbf-audit-fail-class"
	)

	storeMemoryClass(t, baseStore, makeMemoryClassWithSummarization(className, 99))
	storeMemoryClaim(t, baseStore, makeMemoryClaim(ns, claimName, className, v1alpha1.MemoryTypeEpisodic))
	storeMemory(t, baseStore, makeMemory(ns, memName, claimName, v1alpha1.MemoryTypeEpisodic))

	rec := makeMemoryRecordForMemory(ns, "rec-audit", memName, "seed", time.Now().UTC())
	storeMemoryRecord(t, baseStore, rec)

	if _, err := RightToBeForgotten(ctx, s, ns, "rec-audit"); err == nil {
		t.Fatalf("expected RTBF to fail when audit append fails")
	}
	raw, err := baseStore.Get(ctx, memoryRecordKind, ns, rec.Name)
	if err != nil {
		t.Fatalf("get tombstoned record: %v", err)
	}
	var persisted v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("unmarshal tombstoned record: %v", err)
	}
	if persisted.Status.Phase != v1alpha1.PhaseDeleted {
		t.Fatalf("record phase = %s, want Deleted despite audit failure", persisted.Status.Phase)
	}
}

func TestMemoryRTBF_RetryRepairsMissingAuditWithoutDuplicateTombstone(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	s := &appendEventFailOnceStore{memStore: baseStore, failures: 1}
	record := makeMemoryRecordForMemory("default", "source", "rtbf-audit-retry-mem", "seed", time.Now().UTC())
	storeMemoryRecord(t, baseStore, record)

	if _, err := RightToBeForgotten(ctx, s, record.Namespace, record.Name); err == nil {
		t.Fatal("first RTBF call should fail while appending its audit event")
	}
	raw, err := baseStore.Get(ctx, memoryRecordKind, record.Namespace, record.Name)
	if err != nil {
		t.Fatalf("get record after first RTBF call: %v", err)
	}
	var afterFirst v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &afterFirst); err != nil {
		t.Fatalf("unmarshal record after first RTBF call: %v", err)
	}
	firstVersion := afterFirst.ResourceVersion

	for attempt := 0; attempt < 2; attempt++ {
		if _, err := RightToBeForgotten(ctx, s, record.Namespace, record.Name); err != nil {
			t.Fatalf("RTBF retry %d: %v", attempt+1, err)
		}
	}
	raw, err = baseStore.Get(ctx, memoryRecordKind, record.Namespace, record.Name)
	if err != nil {
		t.Fatalf("get record after retries: %v", err)
	}
	var persisted v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("unmarshal record after retries: %v", err)
	}
	if persisted.ResourceVersion != firstVersion {
		t.Fatalf("resource version changed from %d to %d while repairing audit", firstVersion, persisted.ResourceVersion)
	}
	events, err := baseStore.ListEvents(ctx, memoryRecordKind, record.Namespace, record.Name)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 || events[0].Type != "RTBF_TOMBSTONED" {
		t.Fatalf("audit events = %+v, want one RTBF_TOMBSTONED event", events)
	}
}

// TC-0151: summarized record should be tombstoned when one of its source
// records is RTBF-deleted via provenance chain metadata.
func TestMemoryRTBF_TC0151_SummaryRecordTombstonedFromSource(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	summarizer := NewMemorySummarizationReconciler(s, logger)

	const (
		ns        = "default"
		memName   = "summary-rtbf-mem"
		claimName = "summary-rtbf-claim"
		className = "summary-rtbf-class"
	)

	storeMemoryClass(t, s, makeMemoryClassWithSummarization(className, 2))
	storeMemoryClaim(t, s, makeMemoryClaim(ns, claimName, className, v1alpha1.MemoryTypeEpisodic))
	storeMemory(t, s, makeMemory(ns, memName, claimName, v1alpha1.MemoryTypeEpisodic))

	base := time.Date(2024, 11, 1, 0, 0, 0, 0, time.UTC)
	for i, name := range []string{"src-a", "src-b", "src-c"} {
		rec := makeMemoryRecordForMemory(ns, name, memName, name, base.Add(time.Duration(i)*time.Minute))
		storeMemoryRecord(t, s, rec)
	}

	if _, err := summarizer.Reconcile(ctx, Request{Namespace: ns, Name: memName, Kind: memoryKind}); err != nil {
		t.Fatalf("summarizer reconcile: %v", err)
	}

	activeBefore, err := ListActiveMemoryRecords(ctx, s, ns, memName)
	if err != nil {
		t.Fatalf("ListActiveMemoryRecords: %v", err)
	}
	if len(activeBefore) != 1 || activeBefore[0].Spec.Metadata["summary"] != "true" {
		t.Fatalf("expected exactly one active summary record before RTBF, got %+v", activeBefore)
	}
	if got := getMemory(t, s, ns, memName).Status.RecordCount; got != 1 {
		t.Fatalf("expected Memory.Status.RecordCount=1 before RTBF, got %d", got)
	}
	summaryName := activeBefore[0].Name

	tombstoned, err := RightToBeForgotten(ctx, s, ns, "src-a")
	if err != nil {
		t.Fatalf("RightToBeForgotten: %v", err)
	}
	if len(tombstoned) < 2 {
		t.Fatalf("expected at least source+summary tombstoned, got %v", tombstoned)
	}

	rawSummary, err := s.Get(ctx, memoryRecordKind, ns, summaryName)
	if err != nil {
		t.Fatalf("get summary: %v", err)
	}
	var summaryRec v1alpha1.MemoryRecord
	if err := json.Unmarshal(rawSummary, &summaryRec); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	if summaryRec.Status.Phase != v1alpha1.PhaseDeleted {
		t.Fatalf("expected summary phase Deleted after RTBF, got %s", summaryRec.Status.Phase)
	}

	activeAfter, err := ListActiveMemoryRecords(ctx, s, ns, memName)
	if err != nil {
		t.Fatalf("ListActiveMemoryRecords after RTBF: %v", err)
	}
	if len(activeAfter) != 0 {
		t.Fatalf("expected no active records after RTBF, got %+v", activeAfter)
	}
	if _, err := summarizer.Reconcile(ctx, Request{Namespace: ns, Name: memName, Kind: memoryKind}); err != nil {
		t.Fatalf("summarizer reconcile after RTBF: %v", err)
	}
	if got := getMemory(t, s, ns, memName).Status.RecordCount; got != 0 {
		t.Fatalf("expected Memory.Status.RecordCount=0 after RTBF reconciliation, got %d", got)
	}
}
