// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/store"
)

type indexRunLaunchResult struct {
	state   IngestState
	stats   IngestStats
	jobName string
	err     error
}

type fakeIndexRunLauncher struct {
	results []indexRunLaunchResult
	classes []*v1alpha1.KnowledgeBaseClass
	calls   int
}

type indexRunUpdateFaultStore struct {
	*memStore
	failKnowledgeBaseUpdates int
}

type indexRunDeleteFaultStore struct {
	*memStore
	failDeletes int
}

type indexRunDependencyFaultStore struct {
	store.Store
	kind string
	err  error
}

func (s *indexRunDependencyFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == s.kind {
		return nil, s.err
	}
	return s.Store.Get(ctx, kind, namespace, name)
}

func (s *indexRunDeleteFaultStore) Delete(ctx context.Context, kind, namespace, name string) error {
	if kind == knowledgeBaseIndexRunKind && s.failDeletes > 0 {
		s.failDeletes--
		return errors.New("injected IndexRun delete failure")
	}
	return s.memStore.Delete(ctx, kind, namespace, name)
}

type concurrentIndexRunStatusStore struct {
	store.Store
	conflicted bool
}

type concurrentIndexRunStartStore struct {
	store.Store
	conflicted bool
	startedAt  string
}

type concurrentIndexRunCompletionStore struct {
	store.Store
	conflicted bool
	completed  v1alpha1.KnowledgeBaseIndexRunStatus
}

type persistentIndexRunStatusConflictStore struct {
	store.Store
	updates int
}

func (s *persistentIndexRunStatusConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == knowledgeBaseIndexRunKind {
		var desired v1alpha1.KnowledgeBaseIndexRun
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase == v1alpha1.PhaseRunning {
			s.updates++
			return fmt.Errorf("injected persistent conflict: %w", store.ErrConflict)
		}
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *concurrentIndexRunStatusStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == knowledgeBaseIndexRunKind && !s.conflicted {
		var desired v1alpha1.KnowledgeBaseIndexRun
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase == v1alpha1.PhaseRunning {
			s.conflicted = true
			raw, err := s.Store.Get(ctx, kind, namespace, name)
			if err != nil {
				return err
			}
			var latest v1alpha1.KnowledgeBaseIndexRun
			if err := json.Unmarshal(raw, &latest); err != nil {
				return err
			}
			ttl := int32(90)
			latest.Spec.TTLSecondsAfterFinished = &ttl
			latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{
				Type: "Audited", Status: "True", Reason: "ConcurrentAudit",
			})
			updated, err := json.Marshal(latest)
			if err != nil {
				return err
			}
			if err := s.Store.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
				return err
			}
			return fmt.Errorf("injected concurrent conflict: %w", store.ErrConflict)
		}
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *concurrentIndexRunStartStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == knowledgeBaseIndexRunKind && !s.conflicted {
		var desired v1alpha1.KnowledgeBaseIndexRun
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase == v1alpha1.PhaseRunning {
			s.conflicted = true
			raw, err := s.Store.Get(ctx, kind, namespace, name)
			if err != nil {
				return err
			}
			var latest v1alpha1.KnowledgeBaseIndexRun
			if err := json.Unmarshal(raw, &latest); err != nil {
				return err
			}
			latest.Status.Phase = v1alpha1.PhaseRunning
			latest.Status.JobName = "concurrent-job"
			latest.Status.StartedAt = s.startedAt
			updated, err := json.Marshal(latest)
			if err != nil {
				return err
			}
			if err := s.Store.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
				return err
			}
			return fmt.Errorf("injected concurrent start conflict: %w", store.ErrConflict)
		}
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *concurrentIndexRunCompletionStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == knowledgeBaseIndexRunKind && !s.conflicted {
		var desired v1alpha1.KnowledgeBaseIndexRun
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase == v1alpha1.PhaseRunning {
			s.conflicted = true
			raw, err := s.Store.Get(ctx, kind, namespace, name)
			if err != nil {
				return err
			}
			var latest v1alpha1.KnowledgeBaseIndexRun
			if err := json.Unmarshal(raw, &latest); err != nil {
				return err
			}
			latest.Status = snapshotKnowledgeBaseIndexRunStatus(s.completed)
			updated, err := json.Marshal(latest)
			if err != nil {
				return err
			}
			if err := s.Store.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
				return err
			}
			return fmt.Errorf("injected concurrent completion conflict: %w", store.ErrConflict)
		}
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *indexRunUpdateFaultStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == knowledgeBaseKind && s.failKnowledgeBaseUpdates > 0 {
		s.failKnowledgeBaseUpdates--
		return errors.New("injected KnowledgeBase update failure")
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (l *fakeIndexRunLauncher) LaunchRun(
	_ context.Context,
	_ *v1alpha1.KnowledgeBase,
	class *v1alpha1.KnowledgeBaseClass,
	_ *v1alpha1.KnowledgeBaseIndexRun,
) (IngestState, IngestStats, string, error) {
	l.classes = append(l.classes, class)
	result := l.results[l.calls]
	l.calls++
	return result.state, result.stats, result.jobName, result.err
}

func makeIndexRun(namespace, name, knowledgeBaseName string) v1alpha1.KnowledgeBaseIndexRun {
	return v1alpha1.KnowledgeBaseIndexRun{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: knowledgeBaseIndexRunKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: v1alpha1.KnowledgeBaseIndexRunSpec{KnowledgeBaseName: knowledgeBaseName},
	}
}

func TestKnowledgeBaseIndexRunReconciler_WrappedNotFoundIsNoOp(t *testing.T) {
	base := newMemStore()
	s := &indexRunDependencyFaultStore{
		Store: base,
		kind:  knowledgeBaseIndexRunKind,
		err:   fmt.Errorf("wrapped missing run: %w", store.ErrNotFound),
	}
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t))

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "missing"})
	if err != nil {
		t.Fatalf("Reconcile() error = %v, want nil", err)
	}
	if result != (Result{}) {
		t.Fatalf("Reconcile() result = %#v, want zero result", result)
	}
}

func storeIndexRun(t *testing.T, s *memStore, run v1alpha1.KnowledgeBaseIndexRun) {
	t.Helper()
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal IndexRun: %v", err)
	}
	if err := s.Create(context.Background(), knowledgeBaseIndexRunKind, run.ObjectMeta.Namespace, run.ObjectMeta.Name, raw); err != nil {
		t.Fatalf("store IndexRun: %v", err)
	}
}

func getIndexRun(t *testing.T, s *memStore, namespace, name string) v1alpha1.KnowledgeBaseIndexRun {
	t.Helper()
	raw, err := s.Get(context.Background(), knowledgeBaseIndexRunKind, namespace, name)
	if err != nil {
		t.Fatalf("get IndexRun: %v", err)
	}
	var run v1alpha1.KnowledgeBaseIndexRun
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatalf("unmarshal IndexRun: %v", err)
	}
	return run
}

func TestKnowledgeBaseIndexRunReconciler_MissingKnowledgeBaseFailsRun(t *testing.T) {
	s := newMemStore()
	storeIndexRun(t, s, makeIndexRun("default", "run", "missing"))
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t))

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	run := getIndexRun(t, s, "default", "run")
	if run.Status.Phase != v1alpha1.PhaseFailed || run.Status.Message == "" || run.Status.CompletedAt == "" {
		t.Fatalf("run status = %+v, want terminal KnowledgeBase-not-found failure", run.Status)
	}
}

func TestKnowledgeBaseIndexRunReconciler_StoreAndDependencyBoundaries(t *testing.T) {
	t.Run("run not found", func(t *testing.T) {
		r := NewKnowledgeBaseIndexRunReconciler(newMemStore(), zaptest.NewLogger(t))
		result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "missing"})
		if err != nil || result.Requeue || result.RequeueAfter != 0 {
			t.Fatalf("result = %+v, error = %v; want no-op", result, err)
		}
	})

	t.Run("run read failure", func(t *testing.T) {
		wantErr := errors.New("run store unavailable")
		r := NewKnowledgeBaseIndexRunReconciler(&faultStore{memStore: newMemStore(), getErr: wantErr}, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "get knowledgeBaseIndexRun") {
			t.Fatalf("error = %v, want wrapped run read failure", err)
		}
	})

	t.Run("malformed run", func(t *testing.T) {
		memory := newMemStore()
		if err := memory.Create(context.Background(), knowledgeBaseIndexRunKind, "default", "run", []byte("{")); err != nil {
			t.Fatalf("store malformed run: %v", err)
		}
		r := NewKnowledgeBaseIndexRunReconciler(memory, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal knowledgeBaseIndexRun") {
			t.Fatalf("error = %v, want malformed run failure", err)
		}
	})

	t.Run("deleting run", func(t *testing.T) {
		memory := newMemStore()
		run := makeIndexRun("default", "run", "docs")
		now := time.Now().UTC()
		run.ObjectMeta.DeletionTimestamp = &now
		storeIndexRun(t, memory, run)
		r := NewKnowledgeBaseIndexRunReconciler(memory, zaptest.NewLogger(t))
		result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
		if err != nil || result.Requeue || result.RequeueAfter != 0 {
			t.Fatalf("result = %+v, error = %v; want deletion no-op", result, err)
		}
	})

	for _, tc := range []struct {
		name        string
		kind        string
		setup       func(*memStore)
		wantMessage string
	}{
		{
			name:        "knowledge base read failure",
			kind:        knowledgeBaseKind,
			wantMessage: "get knowledgeBase",
		},
		{
			name: "class read failure",
			kind: knowledgeBaseClassKind,
			setup: func(memory *memStore) {
				storeKB(t, memory, makeKB("default", "docs", "vector"))
			},
			wantMessage: "get knowledgeBaseClass",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := newMemStore()
			storeIndexRun(t, memory, makeIndexRun("default", "run", "docs"))
			if tc.setup != nil {
				tc.setup(memory)
			}
			wantErr := errors.New("dependency unavailable")
			r := NewKnowledgeBaseIndexRunReconciler(&indexRunDependencyFaultStore{Store: memory, kind: tc.kind, err: wantErr}, zaptest.NewLogger(t))
			_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
			if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("error = %v, want wrapped dependency failure containing %q", err, tc.wantMessage)
			}
		})
	}

	t.Run("malformed knowledge base", func(t *testing.T) {
		memory := newMemStore()
		storeIndexRun(t, memory, makeIndexRun("default", "run", "docs"))
		if err := memory.Create(context.Background(), knowledgeBaseKind, "default", "docs", []byte("{")); err != nil {
			t.Fatalf("store malformed KnowledgeBase: %v", err)
		}
		r := NewKnowledgeBaseIndexRunReconciler(memory, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal knowledgeBase") {
			t.Fatalf("error = %v, want malformed KnowledgeBase failure", err)
		}
	})

	t.Run("malformed class", func(t *testing.T) {
		memory := newMemStore()
		storeKB(t, memory, makeKB("default", "docs", "vector"))
		storeIndexRun(t, memory, makeIndexRun("default", "run", "docs"))
		if err := memory.Create(context.Background(), knowledgeBaseClassKind, "", "vector", []byte("{")); err != nil {
			t.Fatalf("store malformed KnowledgeBaseClass: %v", err)
		}
		r := NewKnowledgeBaseIndexRunReconciler(memory, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal knowledgeBaseClass") {
			t.Fatalf("error = %v, want malformed KnowledgeBaseClass failure", err)
		}
	})

	t.Run("unknown phase reset failure", func(t *testing.T) {
		memory := newMemStore()
		run := makeIndexRun("default", "run", "docs")
		run.Status.Phase = v1alpha1.Phase("Unknown")
		storeIndexRun(t, memory, run)
		wantErr := errors.New("update unavailable")
		r := NewKnowledgeBaseIndexRunReconciler(&faultStore{memStore: memory, updateErr: wantErr}, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "reset indexRun with unknown phase") {
			t.Fatalf("error = %v, want wrapped reset failure", err)
		}
	})
}

func TestKnowledgeBaseIndexRunReconciler_RunningThenSucceededReflectsKnowledgeBase(t *testing.T) {
	s := newMemStore()
	kb := makeKB("default", "docs", "vector")
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKB(t, s, kb)
	storeKBClass(t, s, kbc)
	storeIndexRun(t, s, makeIndexRun("default", "run", "docs"))
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{
		{state: IngestRunning, jobName: "ingest-run"},
		{state: IngestSucceeded, jobName: "ingest-run", stats: IngestStats{Documents: 3, Chunks: 8, SourceDigest: "sha256:abc"}},
	}}
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
	if err != nil {
		t.Fatalf("start Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Fatalf("start RequeueAfter = %v, want 10s", result.RequeueAfter)
	}
	run := getIndexRun(t, s, "default", "run")
	if run.Status.Phase != v1alpha1.PhaseRunning || run.Status.JobName != "ingest-run" || run.Status.StartedAt == "" {
		t.Fatalf("running status = %+v", run.Status)
	}

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"}); err != nil {
		t.Fatalf("poll Reconcile() error = %v", err)
	}
	run = getIndexRun(t, s, "default", "run")
	if run.Status.Phase != v1alpha1.PhaseSucceeded || run.Status.DocumentCount != 3 || run.Status.IndexedChunks != 8 || run.Status.SourceDigest != "sha256:abc" {
		t.Fatalf("succeeded status = %+v", run.Status)
	}
	kb = getKB(t, s, "default", "docs")
	if kb.Status.Phase != v1alpha1.PhaseReady || kb.Status.DocumentCount != 3 || kb.Status.IndexedChunks != 8 || kb.Status.Generation != 1 {
		t.Fatalf("reflected KnowledgeBase status = %+v", kb.Status)
	}
	if launcher.calls != 2 {
		t.Fatalf("launcher calls = %d, want 2", launcher.calls)
	}
}

func TestKnowledgeBaseIndexRunReconciler_RetriesProviderReflectionFromDurableStatus(t *testing.T) {
	base := newMemStore()
	kb := makeKB("default", "docs", "search")
	kbc := makeKBClass("search", "azuresearch", "hybrid", v1alpha1.ReclaimDelete)
	kbc.Spec.BackendRef = &v1alpha1.KnowledgeBackendReference{APIGroup: "azuresearch.knowledge.kognition.dev", Kind: "AzureSearchBackend", Name: "shared"}
	storeKB(t, base, kb)
	storeKBClass(t, base, kbc)
	storeIndexRun(t, base, makeIndexRun("default", "run", "docs"))
	binding := providerBindingFor(kb, kbc)
	s := &indexRunUpdateFaultStore{memStore: base, failKnowledgeBaseUpdates: 1}
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{
		state: IngestSucceeded,
		stats: IngestStats{Documents: 2, Chunks: 4, SourceDigest: "sha256:provider", BackendBinding: binding},
	}}}
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)
	req := Request{Namespace: "default", Name: "run"}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil || result.RequeueAfter != 10*time.Second {
		t.Fatalf("first Reconcile() = %+v, %v; want reflection retry", result, err)
	}
	durableRun := getIndexRun(t, base, "default", "run")
	if durableRun.Status.Phase != v1alpha1.PhaseSucceeded || !reflect.DeepEqual(durableRun.Status.BackendBinding, binding) {
		t.Fatalf("durable run status = %+v, want provider binding %+v", durableRun.Status, binding)
	}
	if status := getKB(t, base, "default", "docs").Status; status.Generation != 0 || status.BackendBinding != nil {
		t.Fatalf("KB changed before atomic reflection: %+v", status)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("retry Reconcile() error = %v", err)
	}
	reflected := getKB(t, base, "default", "docs")
	if reflected.Status.Generation != 1 || reflected.Status.DocumentCount != 2 || reflected.Status.IndexedChunks != 4 || !reflect.DeepEqual(reflected.Status.BackendBinding, binding) {
		t.Fatalf("reflected KB status = %+v, want exact durable provider result", reflected.Status)
	}
	if launcher.calls != 1 {
		t.Fatalf("launcher calls = %d, want 1", launcher.calls)
	}
}

func TestKnowledgeBaseIndexRunReconciler_RunningConflictPreservesLaunchAndConcurrentState(t *testing.T) {
	base := newMemStore()
	kb := makeKB("default", "docs", "vector")
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKB(t, base, kb)
	storeKBClass(t, base, kbc)
	storeIndexRun(t, base, makeIndexRun("default", "run", "docs"))
	conflictStore := &concurrentIndexRunStatusStore{Store: base}
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{state: IngestRunning, jobName: "ingest-run"}}}
	r := NewKnowledgeBaseIndexRunReconciler(conflictStore, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Fatalf("RequeueAfter = %v, want 10s", result.RequeueAfter)
	}
	if !conflictStore.conflicted {
		t.Fatal("expected Running conflict fixture to execute")
	}
	if launcher.calls != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1", launcher.calls)
	}
	run := getIndexRun(t, base, "default", "run")
	if run.Status.Phase != v1alpha1.PhaseRunning || run.Status.JobName != "ingest-run" {
		t.Fatalf("running status = %+v", run.Status)
	}
	if run.Spec.TTLSecondsAfterFinished == nil || *run.Spec.TTLSecondsAfterFinished != 90 {
		t.Fatalf("concurrent TTL = %v, want 90", run.Spec.TTLSecondsAfterFinished)
	}
	foundAudit := false
	for _, condition := range run.Status.Conditions {
		if condition.Type == "Audited" && condition.Reason == "ConcurrentAudit" {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Fatalf("concurrent Audited condition was lost: %+v", run.Status.Conditions)
	}
}

func TestKnowledgeBaseIndexRunReconciler_RunningConflictPreservesConcurrentExecutionIdentity(t *testing.T) {
	base := newMemStore()
	kb := makeKB("default", "docs", "vector")
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKB(t, base, kb)
	storeKBClass(t, base, kbc)
	storeIndexRun(t, base, makeIndexRun("default", "run", "docs"))
	concurrentStartedAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	conflictStore := &concurrentIndexRunStartStore{Store: base, startedAt: concurrentStartedAt}
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{state: IngestRunning, jobName: "stale-job"}}}
	r := NewKnowledgeBaseIndexRunReconciler(conflictStore, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Fatalf("RequeueAfter = %v, want 10s", result.RequeueAfter)
	}
	run := getIndexRun(t, base, "default", "run")
	if run.Status.JobName != "concurrent-job" || run.Status.StartedAt != concurrentStartedAt {
		t.Fatalf("execution identity = (%q, %q), want concurrent tuple (%q, %q)", run.Status.JobName, run.Status.StartedAt, "concurrent-job", concurrentStartedAt)
	}
	if launcher.calls != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1", launcher.calls)
	}
}

func TestKnowledgeBaseIndexRunReconciler_RunningConflictPreservesConcurrentCompletion(t *testing.T) {
	base := newMemStore()
	kb := makeKB("default", "docs", "vector")
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKB(t, base, kb)
	storeKBClass(t, base, kbc)
	storeIndexRun(t, base, makeIndexRun("default", "run", "docs"))
	completed := v1alpha1.KnowledgeBaseIndexRunStatus{
		Phase:         v1alpha1.PhaseSucceeded,
		JobName:       "completed-job",
		StartedAt:     "2026-01-02T03:04:05Z",
		CompletedAt:   "2026-01-02T03:05:05Z",
		DocumentCount: 3,
		IndexedChunks: 8,
		Conditions: []v1alpha1.Condition{
			{Type: string(conditionKBRunReady), Status: "True", Reason: "IndexRunComplete"},
			{Type: "Progressing", Status: "False", Reason: "IndexRunComplete"},
		},
	}
	conflictStore := &concurrentIndexRunCompletionStore{Store: base, completed: completed}
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{state: IngestRunning, jobName: "stale-job"}}}
	r := NewKnowledgeBaseIndexRunReconciler(conflictStore, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got := getIndexRun(t, base, "default", "run")
	if !reflect.DeepEqual(got.Status, completed) {
		t.Fatalf("status after stale Running retry = %+v, want concurrent completion %+v", got.Status, completed)
	}
	if launcher.calls != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1", launcher.calls)
	}
}

func TestKnowledgeBaseIndexRunReconciler_UnknownPhaseResetsToPending(t *testing.T) {
	s := newMemStore()
	run := makeIndexRun("default", "unknown-phase", "kb")
	run.Status.Phase = v1alpha1.Phase("Corrupted")
	run.Status.JobName = "existing-job"
	storeIndexRun(t, s, run)
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t))

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: run.ObjectMeta.Name, Kind: knowledgeBaseIndexRunKind})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !result.Requeue {
		t.Fatal("Reconcile() did not requeue after resetting unknown phase")
	}
	stored := getIndexRun(t, s, "default", run.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("phase = %q, want Pending", stored.Status.Phase)
	}
	if stored.Status.JobName != "existing-job" {
		t.Fatalf("jobName = %q, want existing execution identity preserved", stored.Status.JobName)
	}
}

func TestKnowledgeBaseIndexRunReconciler_RunningConflictRetriesAreBounded(t *testing.T) {
	base := newMemStore()
	kb := makeKB("default", "docs", "vector")
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKB(t, base, kb)
	storeKBClass(t, base, kbc)
	storeIndexRun(t, base, makeIndexRun("default", "run", "docs"))
	conflictStore := &persistentIndexRunStatusConflictStore{Store: base}
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{state: IngestRunning, jobName: "ingest-run"}}}
	r := NewKnowledgeBaseIndexRunReconciler(conflictStore, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Reconcile() error = %v, want ErrConflict", err)
	}
	if conflictStore.updates != 5 {
		t.Fatalf("IndexRun update attempts = %d, want 5", conflictStore.updates)
	}
	if launcher.calls != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1", launcher.calls)
	}
}

func TestKnowledgeBaseIndexRunReconciler_LauncherErrorRequeuesWithoutTransition(t *testing.T) {
	s := newMemStore()
	kb := makeKB("default", "docs", "vector")
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKB(t, s, kb)
	storeKBClass(t, s, kbc)
	storeIndexRun(t, s, makeIndexRun("default", "run", "docs"))
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{err: errors.New("job API unavailable")}}}
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Fatalf("RequeueAfter = %v, want 15s", result.RequeueAfter)
	}
	if phase := getIndexRun(t, s, "default", "run").Status.Phase; phase != "" {
		t.Fatalf("phase = %q, want unchanged", phase)
	}
}

func TestKnowledgeBaseIndexRunReconciler_MapsTerminalLauncherStates(t *testing.T) {
	tests := []struct {
		name        string
		result      indexRunLaunchResult
		wantPhase   v1alpha1.Phase
		wantMessage string
	}{
		{
			name:        "failed",
			result:      indexRunLaunchResult{state: IngestFailed, stats: IngestStats{Error: "embedding rejected"}},
			wantPhase:   v1alpha1.PhaseFailed,
			wantMessage: "embedding rejected",
		},
		{
			name:        "skipped",
			result:      indexRunLaunchResult{state: IngestSkipped},
			wantPhase:   v1alpha1.PhaseSucceeded,
			wantMessage: "ingester reported the run as skipped",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newMemStore()
			kb := makeKB("default", "docs", "vector")
			kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
			kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
			storeKB(t, s, kb)
			storeKBClass(t, s, kbc)
			storeIndexRun(t, s, makeIndexRun("default", "run", "docs"))
			launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{test.result}}
			r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)

			if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			run := getIndexRun(t, s, "default", "run")
			if run.Status.Phase != test.wantPhase || run.Status.Message != test.wantMessage {
				t.Fatalf("status = %+v, want phase %q and message %q", run.Status, test.wantPhase, test.wantMessage)
			}
		})
	}
}

func TestKnowledgeBaseIndexRunReconciler_RunningRunFailsWhenKnowledgeBaseDeleted(t *testing.T) {
	s := newMemStore()
	run := makeIndexRun("default", "run", "deleted")
	run.Status.Phase = v1alpha1.PhaseRunning
	storeIndexRun(t, s, run)
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t))

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	run = getIndexRun(t, s, "default", "run")
	if run.Status.Phase != v1alpha1.PhaseFailed || run.Status.Message != "KnowledgeBase disappeared while the run was in progress" {
		t.Fatalf("status = %+v, want deleted-KnowledgeBase failure", run.Status)
	}
}

func TestKnowledgeBaseIndexRunReconciler_RunningRunWithoutLauncherFailsClosedAfterClassDeleted(t *testing.T) {
	s := newMemStore()
	storeKB(t, s, makeKB("default", "docs", "deleted-class"))
	run := makeIndexRun("default", "run", "docs")
	run.Status.Phase = v1alpha1.PhaseRunning
	storeIndexRun(t, s, run)
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t))

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	run = getIndexRun(t, s, "default", "run")
	if run.Status.Phase != v1alpha1.PhaseFailed || run.Status.Message != "configured knowledge backend requires an ingester" {
		t.Fatalf("status = %+v, want fail-closed missing-ingester failure", run.Status)
	}
}

func TestKnowledgeBaseIndexRunReconciler_MissingClassFailsRun(t *testing.T) {
	s := newMemStore()
	storeKB(t, s, makeKB("default", "docs", "missing-class"))
	storeIndexRun(t, s, makeIndexRun("default", "run", "docs"))
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t))

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	run := getIndexRun(t, s, "default", "run")
	if run.Status.Phase != v1alpha1.PhaseFailed || run.Status.Message != `KnowledgeBaseClass "missing-class" not found` {
		t.Fatalf("status = %+v, want missing-class failure", run.Status)
	}
}

func TestKnowledgeBaseIndexRunReconciler_PersistsJobDiscoveredWhilePolling(t *testing.T) {
	s := newMemStore()
	kb := makeKB("default", "docs", "vector")
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKB(t, s, kb)
	storeKBClass(t, s, kbc)
	run := makeIndexRun("default", "run", "docs")
	run.Status.Phase = v1alpha1.PhaseRunning
	storeIndexRun(t, s, run)
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{state: IngestRunning, jobName: "recovered-job"}}}
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if jobName := getIndexRun(t, s, "default", "run").Status.JobName; jobName != "recovered-job" {
		t.Fatalf("jobName = %q, want recovered-job", jobName)
	}
}

func TestKnowledgeBaseIndexRunReconciler_ProviderBackedRunSucceeds(t *testing.T) {
	for _, phase := range []v1alpha1.Phase{"", v1alpha1.PhaseRunning} {
		t.Run(string(phase), func(t *testing.T) {
			s := newMemStore()
			kb := makeKB("default", "docs", "search")
			kbc := makeKBClass("search", "azuresearch", "hybrid", v1alpha1.ReclaimDelete)
			kbc.Spec.BackendRef = &v1alpha1.KnowledgeBackendReference{
				APIGroup: "azuresearch.knowledge.kognition.dev",
				Kind:     "AzureSearchBackend",
				Name:     "shared",
			}
			storeKB(t, s, kb)
			storeKBClass(t, s, kbc)
			run := makeIndexRun("default", "run", "docs")
			run.Status.Phase = phase
			storeIndexRun(t, s, run)
			binding := providerBindingFor(kb, kbc)
			launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{
				state:   IngestSucceeded,
				stats:   IngestStats{Documents: 3, Chunks: 8, SourceDigest: "sha256:provider", BackendBinding: binding},
				jobName: "provider-run",
			}}}

			r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)
			if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			stored := getIndexRun(t, s, "default", "run")
			if stored.Status.Phase != v1alpha1.PhaseSucceeded || !reflect.DeepEqual(stored.Status.BackendBinding, binding) {
				t.Fatalf("status = %+v, want successful provider binding %+v", stored.Status, binding)
			}
			reflected := getKB(t, s, "default", "docs")
			if reflected.Status.Generation != 1 || reflected.Status.DocumentCount != 3 || reflected.Status.IndexedChunks != 8 || !reflect.DeepEqual(reflected.Status.BackendBinding, binding) {
				t.Fatalf("reflected KB status = %+v, want atomic provider result", reflected.Status)
			}
		})
	}
}

func TestKnowledgeBaseIndexRunReconciler_ProviderCommitAfterClassDeletionPreservesBinding(t *testing.T) {
	s := newMemStore()
	kb := makeKB("default", "docs", "search")
	kbc := makeKBClass("search", "azuresearch", "hybrid", v1alpha1.ReclaimDelete)
	kbc.ObjectMeta.Generation = 1
	kbc.Spec.BackendRef = &v1alpha1.KnowledgeBackendReference{
		APIGroup: "azuresearch.knowledge.kognition.dev",
		Kind:     "AzureSearchBackend",
		Name:     "shared",
	}
	storeKB(t, s, kb)
	storeKBClass(t, s, kbc)
	run := makeIndexRun("default", "run", "docs")
	run.Status.Phase = v1alpha1.PhaseRunning
	storeIndexRun(t, s, run)
	binding := providerBindingFor(kb, kbc)
	if err := s.Delete(context.Background(), knowledgeBaseClassKind, "", kbc.ObjectMeta.Name); err != nil {
		t.Fatalf("delete KnowledgeBaseClass: %v", err)
	}
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{
		state: IngestSucceeded,
		stats: IngestStats{Documents: 3, Chunks: 8, SourceDigest: "sha256:provider", BackendBinding: binding},
	}}}
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if launcher.calls != 1 || len(launcher.classes) != 1 || launcher.classes[0] != nil {
		t.Fatalf("launcher calls = %d with classes %+v, want one recovery inspection with nil class", launcher.calls, launcher.classes)
	}

	storedRun := getIndexRun(t, s, "default", "run")
	if storedRun.Status.Phase != v1alpha1.PhaseSucceeded || !reflect.DeepEqual(storedRun.Status.BackendBinding, binding) {
		t.Fatalf("run status = %+v, want successful provider binding %+v", storedRun.Status, binding)
	}
	reflected := getKB(t, s, "default", "docs")
	if !reflect.DeepEqual(reflected.Status.BackendBinding, binding) {
		t.Fatalf("KnowledgeBase binding = %+v, want %+v", reflected.Status.BackendBinding, binding)
	}
}

func TestKnowledgeBaseIndexRunReconciler_ProviderConfirmationFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		binding func(v1alpha1.KnowledgeBase, v1alpha1.KnowledgeBaseClass) *v1alpha1.KnowledgeBackendBinding
	}{
		{name: "missing"},
		{name: "mismatched", binding: func(kb v1alpha1.KnowledgeBase, kbc v1alpha1.KnowledgeBaseClass) *v1alpha1.KnowledgeBackendBinding {
			binding := providerBindingFor(kb, kbc)
			binding.ClassUID = "untrusted-class"
			return binding
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newMemStore()
			kb := makeKB("default", "docs", "search")
			kbc := makeKBClass("search", "azuresearch", "hybrid", v1alpha1.ReclaimDelete)
			kbc.Spec.BackendRef = &v1alpha1.KnowledgeBackendReference{Kind: "AzureSearchBackend", Name: "shared"}
			storeKB(t, s, kb)
			storeKBClass(t, s, kbc)
			storeIndexRun(t, s, makeIndexRun("default", "run", "docs"))
			var binding *v1alpha1.KnowledgeBackendBinding
			if tc.binding != nil {
				binding = tc.binding(kb, kbc)
			}
			launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{
				state: IngestSucceeded, stats: IngestStats{Documents: 3, Chunks: 8, BackendBinding: binding},
			}}}
			r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)

			if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			stored := getIndexRun(t, s, "default", "run")
			condition := lifecycle.GetCondition(apiToLifecycleConditions(stored.Status.Conditions), conditionKBRunReady)
			if stored.Status.Phase != v1alpha1.PhaseFailed || condition == nil || condition.Reason != "ProviderConfirmationInvalid" {
				t.Fatalf("status = %+v, want invalid provider confirmation failure", stored.Status)
			}
			if generation := getKB(t, s, "default", "docs").Status.Generation; generation != 0 {
				t.Fatalf("KB generation = %d, want unchanged", generation)
			}
		})
	}
}

func TestKnowledgeBaseIndexRunReconciler_PollMapsTerminalAndSkippedStates(t *testing.T) {
	for _, tc := range []struct {
		name        string
		launcher    *fakeIndexRunLauncher
		wantPhase   v1alpha1.Phase
		wantMessage string
	}{
		{
			name:        "no launcher",
			wantPhase:   v1alpha1.PhaseFailed,
			wantMessage: "configured knowledge backend requires an ingester",
		},
		{
			name: "launcher failed",
			launcher: &fakeIndexRunLauncher{results: []indexRunLaunchResult{{
				state: IngestFailed,
				stats: IngestStats{Error: "embedding rejected"},
			}}},
			wantPhase:   v1alpha1.PhaseFailed,
			wantMessage: "embedding rejected",
		},
		{
			name:        "launcher skipped",
			launcher:    &fakeIndexRunLauncher{results: []indexRunLaunchResult{{state: IngestSkipped}}},
			wantPhase:   v1alpha1.PhaseSucceeded,
			wantMessage: "ingester reported the run as skipped",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := newMemStore()
			kb := makeKB("default", "docs", "vector")
			kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
			kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
			storeKB(t, memory, kb)
			storeKBClass(t, memory, kbc)
			run := makeIndexRun("default", "run", "docs")
			run.Status.Phase = v1alpha1.PhaseRunning
			run.Status.JobName = "ingest-run"
			storeIndexRun(t, memory, run)
			r := NewKnowledgeBaseIndexRunReconciler(memory, zaptest.NewLogger(t))
			if tc.launcher != nil {
				r.WithIndexRunLauncher(tc.launcher)
			}

			result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
			if err != nil || result.Requeue || result.RequeueAfter != 0 {
				t.Fatalf("result = %+v, error = %v; want terminal result", result, err)
			}
			stored := getIndexRun(t, memory, "default", "run")
			if stored.Status.Phase != tc.wantPhase || stored.Status.Message != tc.wantMessage {
				t.Fatalf("status = %+v, want phase %q and message %q", stored.Status, tc.wantPhase, tc.wantMessage)
			}
		})
	}
}

func TestKnowledgeBaseIndexRunReconciler_PollFailsWhenDiscoveredJobNameCannotPersist(t *testing.T) {
	memory := newMemStore()
	kb := makeKB("default", "docs", "vector")
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKB(t, memory, kb)
	storeKBClass(t, memory, kbc)
	run := makeIndexRun("default", "run", "docs")
	run.Status.Phase = v1alpha1.PhaseRunning
	storeIndexRun(t, memory, run)
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{state: IngestRunning, jobName: "recovered-job"}}}
	wantErr := errors.New("update unavailable")
	r := NewKnowledgeBaseIndexRunReconciler(&faultStore{memStore: memory, updateErr: wantErr}, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "persist discovered Job name") {
		t.Fatalf("error = %v, want wrapped persistence failure", err)
	}
}

func TestKnowledgeBaseIndexRunReconciler_PollRecoversAfterLauncherFailure(t *testing.T) {
	s := newMemStore()
	kb := makeKB("default", "docs", "vector")
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKB(t, s, kb)
	storeKBClass(t, s, kbc)
	run := makeIndexRun("default", "run", "docs")
	run.Status.Phase = v1alpha1.PhaseRunning
	run.Status.JobName = "ingest-run"
	run.Status.StartedAt = "2026-01-02T03:04:05Z"
	run.Status.Message = "ingestion in progress"
	run.Status.Conditions = []v1alpha1.Condition{{Type: "Audited", Status: "True", Reason: "RunningAudit"}}
	wantRunningStatus := snapshotKnowledgeBaseIndexRunStatus(run.Status)
	storeIndexRun(t, s, run)
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{
		{err: errors.New("job API unavailable")},
		{state: IngestSucceeded, stats: IngestStats{Documents: 3, Chunks: 8, SourceDigest: "sha256:done"}},
	}}
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)
	req := Request{Namespace: "default", Name: "run"}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("failed poll Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Fatalf("failed poll RequeueAfter = %v, want 15s", result.RequeueAfter)
	}
	afterFailure := getIndexRun(t, s, "default", "run")
	if !reflect.DeepEqual(afterFailure.Status, wantRunningStatus) {
		t.Fatalf("status after failed poll = %+v, want unchanged %+v", afterFailure.Status, wantRunningStatus)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("recovery poll Reconcile() error = %v", err)
	}
	completed := getIndexRun(t, s, "default", "run")
	if completed.Status.Phase != v1alpha1.PhaseSucceeded || completed.Status.DocumentCount != 3 || completed.Status.IndexedChunks != 8 {
		t.Fatalf("status after recovery = %+v, want succeeded with 3 documents and 8 chunks", completed.Status)
	}
	reflected := getKB(t, s, "default", "docs")
	if reflected.Status.Generation != 1 || reflected.Status.IndexedChunks != 8 {
		t.Fatalf("reflected KB status = %+v, want one successful reflection with 8 chunks", reflected.Status)
	}
	if launcher.calls != 2 {
		t.Fatalf("launcher calls = %d, want 2", launcher.calls)
	}
}

func TestKnowledgeBaseIndexRunReconciler_ZeroResultClearsStaleCounts(t *testing.T) {
	s := newMemStore()
	kb := makeKB("default", "docs", "vector")
	kb.Status.DocumentCount = 5
	kb.Status.IndexedChunks = 12
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKB(t, s, kb)
	storeKBClass(t, s, kbc)
	run := makeIndexRun("default", "run", "docs")
	run.Status.DocumentCount = 5
	run.Status.IndexedChunks = 12
	storeIndexRun(t, s, run)
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{state: IngestSucceeded}}}
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	run = getIndexRun(t, s, "default", "run")
	kb = getKB(t, s, "default", "docs")
	if run.Status.DocumentCount != 0 || run.Status.IndexedChunks != 0 || kb.Status.DocumentCount != 0 || kb.Status.IndexedChunks != 0 {
		t.Fatalf("stale counts remain: run=%+v KB=%+v", run.Status, kb.Status)
	}
}

func TestKnowledgeBaseIndexRunReconciler_RetriesKnowledgeBaseReflectionExactlyOnce(t *testing.T) {
	base := newMemStore()
	kb := makeKB("default", "docs", "vector")
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKB(t, base, kb)
	storeKBClass(t, base, kbc)
	storeIndexRun(t, base, makeIndexRun("default", "run", "docs"))
	s := &indexRunUpdateFaultStore{memStore: base, failKnowledgeBaseUpdates: 1}
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{state: IngestSucceeded, stats: IngestStats{Documents: 2, Chunks: 4}}}}
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)
	req := Request{Namespace: "default", Name: "run"}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("completion Reconcile() error = %v", err)
	}
	if generation := getKB(t, base, "default", "docs").Status.Generation; generation != 0 {
		t.Fatalf("generation after failed reflection = %d, want 0", generation)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("retry Reconcile() error = %v", err)
	}
	reflected := getKB(t, base, "default", "docs")
	if reflected.Status.Generation != 1 || reflected.ObjectMeta.Annotations[lastIndexRunAnnotation] == "" {
		t.Fatalf("reflected KnowledgeBase = %+v", reflected)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("idempotence Reconcile() error = %v", err)
	}
	if generation := getKB(t, base, "default", "docs").Status.Generation; generation != 1 {
		t.Fatalf("generation after repeated terminal reconcile = %d, want 1", generation)
	}
}

func TestKnowledgeBaseIndexRunReconciler_ReflectsRecreatedRunWithSameName(t *testing.T) {
	s := newMemStore()
	kb := makeKB("default", "docs", "vector")
	storeKB(t, s, kb)
	completedAt := time.Now().Add(-time.Minute).UTC()
	first := completedIndexRun("default", "run", "run-uid-1", "docs", completedAt, 2)
	storeIndexRun(t, s, first)
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t))
	req := Request{Namespace: "default", Name: "run"}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if err := s.Delete(context.Background(), knowledgeBaseIndexRunKind, "default", "run"); err != nil {
		t.Fatalf("delete first run: %v", err)
	}
	second := completedIndexRun("default", "run", "run-uid-2", "docs", completedAt.Add(time.Second), 7)
	storeIndexRun(t, s, second)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("recreated Reconcile() error = %v", err)
	}
	reflected := getKB(t, s, "default", "docs")
	if reflected.Status.Generation != 2 || reflected.Status.IndexedChunks != 7 {
		t.Fatalf("reflected recreated run = %+v", reflected.Status)
	}
}

func TestKnowledgeBaseIndexRunReconciler_UnexpectedStartStateRequeuesWithoutTransition(t *testing.T) {
	s := newMemStore()
	kb := makeKB("default", "docs", "vector")
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKB(t, s, kb)
	storeKBClass(t, s, kbc)
	run := makeIndexRun("default", "run", "docs")
	wantStatus := snapshotKnowledgeBaseIndexRunStatus(run.Status)
	storeIndexRun(t, s, run)
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{state: IngestState(99), jobName: "unexpected-job"}}}
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Fatalf("RequeueAfter = %v, want 15s", result.RequeueAfter)
	}
	if launcher.calls != 1 {
		t.Fatalf("launcher calls = %d, want 1", launcher.calls)
	}
	got := getIndexRun(t, s, "default", "run")
	if !reflect.DeepEqual(got.Status, wantStatus) {
		t.Fatalf("status = %+v, want unchanged %+v", got.Status, wantStatus)
	}
}

func TestKnowledgeBaseIndexRunReconciler_UnexpectedPollStateRequeuesWithoutTransition(t *testing.T) {
	s := newMemStore()
	kb := makeKB("default", "docs", "vector")
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKB(t, s, kb)
	storeKBClass(t, s, kbc)
	run := makeIndexRun("default", "run", "docs")
	run.Status.Phase = v1alpha1.PhaseRunning
	run.Status.JobName = "ingest-run"
	run.Status.StartedAt = "2026-01-02T03:04:05Z"
	wantStatus := snapshotKnowledgeBaseIndexRunStatus(run.Status)
	storeIndexRun(t, s, run)
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{state: IngestState(99)}}}
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "run"})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Fatalf("RequeueAfter = %v, want 15s", result.RequeueAfter)
	}
	if launcher.calls != 1 {
		t.Fatalf("launcher calls = %d, want 1", launcher.calls)
	}
	got := getIndexRun(t, s, "default", "run")
	if !reflect.DeepEqual(got.Status, wantStatus) {
		t.Fatalf("status = %+v, want unchanged %+v", got.Status, wantStatus)
	}
}

func TestKnowledgeBaseIndexRunReconciler_OlderRetryDoesNotOverwriteNewerRun(t *testing.T) {
	s := newMemStore()
	storeKB(t, s, makeKB("default", "docs", "vector"))
	completedAt := time.Now().Add(-time.Minute).UTC()
	older := completedIndexRun("default", "older", "run-uid-older", "docs", completedAt, 2)
	newer := completedIndexRun("default", "newer", "run-uid-newer", "docs", completedAt.Add(time.Second), 7)
	storeIndexRun(t, s, older)
	storeIndexRun(t, s, newer)
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t))

	for _, name := range []string{"older", "newer", "older"} {
		if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: name}); err != nil {
			t.Fatalf("Reconcile(%q) error = %v", name, err)
		}
	}
	reflected := getKB(t, s, "default", "docs")
	if reflected.Status.Generation != 2 || reflected.Status.IndexedChunks != 7 {
		t.Fatalf("reflected concurrent runs = %+v", reflected.Status)
	}
}

func completedIndexRun(namespace, name, uid, knowledgeBaseName string, completedAt time.Time, chunks int64) v1alpha1.KnowledgeBaseIndexRun {
	run := makeIndexRun(namespace, name, knowledgeBaseName)
	run.ObjectMeta.UID = uid
	run.Status.Phase = v1alpha1.PhaseSucceeded
	run.Status.CompletedAt = completedAt.Format(time.RFC3339Nano)
	run.Status.IndexedChunks = chunks
	run.Status.Conditions = []v1alpha1.Condition{{
		Type:   string(conditionKBRunReady),
		Status: "True",
		Reason: "IndexRunComplete",
	}}
	return run
}

func TestReflectedRunIsCurrent_UsesStableUIDOrderForEqualTimestamps(t *testing.T) {
	completedAt := time.Now().UTC().Format(time.RFC3339Nano)
	current, err := json.Marshal(reflectedIndexRun{UID: "uid-b", Name: "run-b", CompletedAt: completedAt})
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	if !reflectedRunIsCurrent(string(current), reflectedIndexRun{UID: "uid-a", Name: "run-a", CompletedAt: completedAt}) {
		t.Fatal("higher current UID should win equal completion timestamp")
	}
	if reflectedRunIsCurrent(string(current), reflectedIndexRun{UID: "uid-c", Name: "run-c", CompletedAt: completedAt}) {
		t.Fatal("higher candidate UID should win equal completion timestamp")
	}
	if reflectedRunIsCurrent("run-from-legacy-format", reflectedIndexRun{Name: "run", CompletedAt: completedAt}) {
		t.Fatal("legacy marker should allow replacement with structured identity")
	}
}

func TestKnowledgeBaseIndexRunReconciler_RetriesReflectionBeforeImmediateTTLDeletion(t *testing.T) {
	base := newMemStore()
	kb := makeKB("default", "docs", "vector")
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKB(t, base, kb)
	storeKBClass(t, base, kbc)
	ttl := int32(0)
	run := makeIndexRun("default", "run", "docs")
	run.Spec.TTLSecondsAfterFinished = &ttl
	storeIndexRun(t, base, run)
	s := &indexRunUpdateFaultStore{memStore: base, failKnowledgeBaseUpdates: 1}
	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{state: IngestSucceeded, stats: IngestStats{Chunks: 4}}}}
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)
	req := Request{Namespace: "default", Name: "run"}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("completion Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Fatalf("RequeueAfter = %v, want reflection retry in 10s", result.RequeueAfter)
	}
	if _, err := base.Get(context.Background(), knowledgeBaseIndexRunKind, "default", "run"); err != nil {
		t.Fatalf("run deleted before reflection retry: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("retry Reconcile() error = %v", err)
	}
	if generation := getKB(t, base, "default", "docs").Status.Generation; generation != 1 {
		t.Fatalf("generation after retry = %d, want 1", generation)
	}
	if _, err := base.Get(context.Background(), knowledgeBaseIndexRunKind, "default", "run"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("run Get() error = %v, want deletion after successful reflection", err)
	}
}

func TestKnowledgeBaseIndexRunReconciler_DeletesExpiredTerminalRun(t *testing.T) {
	s := newMemStore()
	ttl := int32(1)
	run := makeIndexRun("default", "expired", "docs")
	run.Spec.TTLSecondsAfterFinished = &ttl
	run.Status.Phase = v1alpha1.PhaseSucceeded
	run.Status.CompletedAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	storeIndexRun(t, s, run)
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t))

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "expired"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, err := s.Get(context.Background(), knowledgeBaseIndexRunKind, "default", "expired"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestKnowledgeBaseIndexRunReconciler_TTLRequeuesAndRetriesDeleteFailure(t *testing.T) {
	base := newMemStore()
	ttl := int32(24 * 60 * 60)
	future := makeIndexRun("default", "future", "docs")
	future.Spec.TTLSecondsAfterFinished = &ttl
	future.Status.Phase = v1alpha1.PhaseFailed
	future.Status.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	storeIndexRun(t, base, future)

	faultStore := &indexRunDeleteFaultStore{memStore: base, failDeletes: 1}
	r := NewKnowledgeBaseIndexRunReconciler(faultStore, zaptest.NewLogger(t))
	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "future"})
	if err != nil {
		t.Fatalf("future TTL Reconcile() error = %v", err)
	}
	if result.RequeueAfter < 23*time.Hour || result.RequeueAfter > 24*time.Hour {
		t.Fatalf("future TTL RequeueAfter = %v, want between 23h and 24h", result.RequeueAfter)
	}
	if faultStore.failDeletes != 1 {
		t.Fatal("future TTL attempted deletion before the deadline")
	}

	expired := getIndexRun(t, base, "default", "future")
	expired.Status.CompletedAt = time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(expired)
	if err != nil {
		t.Fatalf("marshal expired run: %v", err)
	}
	if err := base.Update(context.Background(), knowledgeBaseIndexRunKind, "default", "future", raw, expired.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("expire run: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "future"}); err == nil {
		t.Fatal("expected TTL deletion error")
	}
	if _, err := base.Get(context.Background(), knowledgeBaseIndexRunKind, "default", "future"); err != nil {
		t.Fatalf("run missing after failed TTL deletion: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "future"}); err != nil {
		t.Fatalf("TTL deletion retry error = %v", err)
	}
	if _, err := base.Get(context.Background(), knowledgeBaseIndexRunKind, "default", "future"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("run Get() error after retry = %v, want ErrNotFound", err)
	}
}

func TestKnowledgeBaseIndexRunReconciler_InvalidCompletionTimeSkipsTTL(t *testing.T) {
	s := newMemStore()
	ttl := int32(0)
	run := makeIndexRun("default", "invalid-completion", "docs")
	run.Spec.TTLSecondsAfterFinished = &ttl
	run.Status.Phase = v1alpha1.PhaseSucceeded
	run.Status.CompletedAt = "not-a-timestamp"
	storeIndexRun(t, s, run)
	r := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t))

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "invalid-completion"})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want zero for invalid completion time", result.RequeueAfter)
	}
	if _, err := s.Get(context.Background(), knowledgeBaseIndexRunKind, "default", "invalid-completion"); err != nil {
		t.Fatalf("run removed for invalid completion time: %v", err)
	}
}
