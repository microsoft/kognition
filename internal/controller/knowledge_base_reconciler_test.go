// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/store"
)

type knowledgeBaseAgentConflictStore struct {
	*memStore
	injectConflict bool
	conflicted     bool
}

type refreshScheduleConflictStore struct {
	*memStore
	conflicted bool
}

type knowledgeBaseClassLookupFailureStore struct {
	store.Store
	err error
}

func (s *knowledgeBaseClassLookupFailureStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == knowledgeBaseClassKind {
		return nil, s.err
	}
	return s.Store.Get(ctx, kind, namespace, name)
}

func (s *knowledgeBaseAgentConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "Agent" && s.injectConflict && !s.conflicted {
		raw, err := s.memStore.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var latest v1alpha1.Agent
		if err := json.Unmarshal(raw, &latest); err != nil {
			return err
		}
		replicas := int32(5)
		latest.ObjectMeta.Labels = map[string]string{"concurrent": "preserved"}
		latest.Spec.Replicas = &replicas
		latest.Status.Phase = v1alpha1.PhaseRunning
		latest.Status.Replicas = 5
		latest.Status.ReadyReplicas = 4
		latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{
			Type: "ConcurrentReady", Status: "True", Reason: "ConcurrentUpdate",
		})
		updated, err := json.Marshal(latest)
		if err != nil {
			return err
		}
		if err := s.memStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
			return err
		}
		s.conflicted = true
		return fmt.Errorf("injected agent conflict: %w", store.ErrConflict)
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *refreshScheduleConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == knowledgeBaseIndexScheduleKind && !s.conflicted {
		var desired v1alpha1.KnowledgeBaseIndexSchedule
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Spec.Schedule == "@every 15m" {
			s.conflicted = true
			raw, err := s.memStore.Get(ctx, kind, namespace, name)
			if err != nil {
				return err
			}
			var latest v1alpha1.KnowledgeBaseIndexSchedule
			if err := json.Unmarshal(raw, &latest); err != nil {
				return err
			}
			latest.Status.Phase = v1alpha1.PhaseReady
			latest.Status.NextScheduleTime = "2026-08-14T22:30:00Z"
			latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{Type: "Audited", Status: "True", Reason: "ConcurrentStatusUpdate"})
			updated, err := json.Marshal(latest)
			if err != nil {
				return err
			}
			if err := s.memStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
				return err
			}
			return fmt.Errorf("injected refresh schedule conflict: %w", store.ErrConflict)
		}
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func makeKBClass(name, provisioner, indexMode string, policy v1alpha1.ReclaimPolicy) v1alpha1.KnowledgeBaseClass {
	return v1alpha1.KnowledgeBaseClass{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "KnowledgeBaseClass"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name: name,
			UID:  "kbc-uid-" + name,
		},
		Spec: v1alpha1.KnowledgeBaseClassSpec{
			Provisioner:   provisioner,
			IndexMode:     indexMode,
			ReclaimPolicy: policy,
		},
	}
}

func makeKB(ns, name, className string, sources ...v1alpha1.KBSource) v1alpha1.KnowledgeBase {
	return v1alpha1.KnowledgeBase{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "KnowledgeBase"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "kb-uid-" + name,
		},
		Spec: v1alpha1.KnowledgeBaseSpec{
			KnowledgeBaseClassName: className,
			Sources:                sources,
		},
	}
}

func providerBindingFor(kb v1alpha1.KnowledgeBase, class v1alpha1.KnowledgeBaseClass) *v1alpha1.KnowledgeBackendBinding {
	configurationAPIGroup, configurationKind, configurationName := "knowledge.kognition.dev", "TestBackend", "test"
	if class.Spec.BackendRef != nil {
		configurationAPIGroup = class.Spec.BackendRef.APIGroup
		configurationKind = class.Spec.BackendRef.Kind
		configurationName = class.Spec.BackendRef.Name
	}
	return &v1alpha1.KnowledgeBackendBinding{
		Generation:            kb.Status.Generation + 1,
		ProviderName:          class.Spec.Provisioner,
		ReclaimPolicy:         effectiveKnowledgeReclaimPolicy(class.Spec.ReclaimPolicy),
		ClassUID:              class.ObjectMeta.UID,
		ClassGeneration:       class.ObjectMeta.Generation,
		ConfigurationAPIGroup: configurationAPIGroup,
		ConfigurationKind:     configurationKind,
		ConfigurationName:     configurationName,
		ConfigurationUID:      "configuration-uid",
		ConfigurationDigest:   strings.Repeat("a", 64),
		OperationID:           "default/kb-index-5",
		BackendOperationID:    "backend-operation",
	}
}

func inlineSource(data string) v1alpha1.KBSource {
	return v1alpha1.KBSource{Type: "inline", Data: data}
}

func urlSource(uri string) v1alpha1.KBSource {
	return v1alpha1.KBSource{Type: "url", URI: uri}
}

func fileSource(uri string) v1alpha1.KBSource {
	return v1alpha1.KBSource{Type: "file", URI: uri}
}

func dirSource(dir string) v1alpha1.KBSource {
	return v1alpha1.KBSource{Type: "directory", Directory: dir}
}

func gitSource(url, branch string) v1alpha1.KBSource {
	return v1alpha1.KBSource{Type: "git", Git: &v1alpha1.KBGitSource{URL: url, Branch: branch}}
}

func configMapSource(name string) v1alpha1.KBSource {
	return v1alpha1.KBSource{Type: "configMap", ConfigMap: &v1alpha1.LocalObjectReference{Name: name}}
}

func storeKBClass(t *testing.T, s *memStore, kbc v1alpha1.KnowledgeBaseClass) {
	t.Helper()
	data, _ := json.Marshal(kbc)
	if err := s.Create(context.Background(), "KnowledgeBaseClass", "", kbc.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store KBClass: %v", err)
	}
}

func storeKB(t *testing.T, s *memStore, kb v1alpha1.KnowledgeBase) {
	t.Helper()
	data, _ := json.Marshal(kb)
	if err := s.Create(context.Background(), "KnowledgeBase", kb.ObjectMeta.Namespace, kb.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store KB: %v", err)
	}
}

func getKB(t *testing.T, s *memStore, ns, name string) v1alpha1.KnowledgeBase {
	t.Helper()
	raw, err := s.Get(context.Background(), "KnowledgeBase", ns, name)
	if err != nil {
		t.Fatalf("get KB %s/%s: %v", ns, name, err)
	}
	var kb v1alpha1.KnowledgeBase
	if err := json.Unmarshal(raw, &kb); err != nil {
		t.Fatalf("unmarshal KB: %v", err)
	}
	return kb
}

func getAgent(t *testing.T, s *memStore, ns, name string) v1alpha1.Agent {
	t.Helper()
	raw, err := s.Get(context.Background(), "Agent", ns, name)
	if err != nil {
		t.Fatalf("get Agent %s/%s: %v", ns, name, err)
	}
	var agent v1alpha1.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		t.Fatalf("unmarshal Agent: %v", err)
	}
	return agent
}

func storeAgent(t *testing.T, s *memStore, agent v1alpha1.Agent) {
	t.Helper()
	data, _ := json.Marshal(agent)
	if err := s.Create(context.Background(), "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store Agent: %v", err)
	}
}

func makeTestAgent(ns, name, claimName string, required bool) v1alpha1.Agent {
	return v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "agent-uid-" + name,
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef: v1alpha1.LocalObjectReference{Name: "gpt-4o"},
			KnowledgeBaseClaims: []v1alpha1.KnowledgeBaseClaimRef{
				{
					Name:     claimName,
					Required: required,
				},
			},
		},
	}
}

// makeTestKBClaim builds a KnowledgeBaseClaim binding to the given KB.
func makeTestKBClaim(ns, name, kbName string) v1alpha1.KnowledgeBaseClaim {
	return v1alpha1.KnowledgeBaseClaim{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "KnowledgeBaseClaim"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "kbc-uid-" + name,
		},
		Spec: v1alpha1.KnowledgeBaseClaimSpec{
			KnowledgeBaseName: kbName,
		},
	}
}

func storeKBClaim(t *testing.T, s *memStore, claim v1alpha1.KnowledgeBaseClaim) {
	t.Helper()
	data, _ := json.Marshal(claim)
	if err := s.Create(context.Background(), "KnowledgeBaseClaim", claim.ObjectMeta.Namespace, claim.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store KBClaim: %v", err)
	}
}

// reconcileKB drives the KB reconciler to a stable state (up to 10 loops).
func reconcileKB(t *testing.T, r *KnowledgeBaseReconciler, ns, name string) {
	t.Helper()
	ctx := context.Background()
	req := Request{Namespace: ns, Name: name, Kind: "KnowledgeBase"}
	for i := 0; i < 10; i++ {
		res, err := r.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("reconcile loop %d: %v", i+1, err)
		}
		if !res.Requeue && res.RequeueAfter == 0 {
			return
		}
	}
}

// findCondition returns the condition with the given type, or nil if not found.
func findKBCondition(kb v1alpha1.KnowledgeBase, condType string) *v1alpha1.Condition {
	for i := range kb.Status.Conditions {
		if kb.Status.Conditions[i].Type == condType {
			return &kb.Status.Conditions[i]
		}
	}
	return nil
}

func findAgentCondition(agent v1alpha1.Agent, condType string) *v1alpha1.Condition {
	for i := range agent.Status.Conditions {
		if agent.Status.Conditions[i].Type == condType {
			return &agent.Status.Conditions[i]
		}
	}
	return nil
}

// fakeIngestLauncher records Reset calls and returns a fixed Ensure result.
type fakeIngestLauncher struct {
	ensureState IngestState
	ensureStats IngestStats
	ensureErr   error
	ensureCalls int
	resetCalls  int
}

func (f *fakeIngestLauncher) Ensure(context.Context, *v1alpha1.KnowledgeBase, *v1alpha1.KnowledgeBaseClass) (IngestState, IngestStats, error) {
	f.ensureCalls++
	return f.ensureState, f.ensureStats, f.ensureErr
}

func (f *fakeIngestLauncher) Reset(context.Context, *v1alpha1.KnowledgeBase) error {
	f.resetCalls++
	return nil
}

// failedKB builds a KB already in the Failed phase with the given Ready-condition
// reason and failure age, plus the finalizer so reconcile goes straight to the
// phase switch.
func failedKB(reason string, failedAgo time.Duration) v1alpha1.KnowledgeBase {
	kb := makeKB("default", "kb-failed", "cls", inlineSource("x"))
	kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer}
	kb.Status.Phase = v1alpha1.PhaseFailed
	kb.Status.Conditions = []v1alpha1.Condition{{
		Type:               "Ready",
		Status:             "False",
		Reason:             reason,
		LastTransitionTime: time.Now().Add(-failedAgo),
	}}
	return kb
}

// A KB that failed to ingest transiently should self-heal: after the backoff,
// the reconciler resets the terminal Job and re-enters Indexing.
func TestKnowledgeBaseReconciler_SelfHealsFailedIngest(t *testing.T) {
	s := newMemStore()
	launcher := &fakeIngestLauncher{}
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t)).WithIngesterLauncher(launcher)
	storeKB(t, s, failedKB("IngestFailed", 5*time.Minute))

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "kb-failed", Kind: "KnowledgeBase"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if launcher.resetCalls != 1 {
		t.Fatalf("expected Reset called once, got %d", launcher.resetCalls)
	}
	if got := getKB(t, s, "default", "kb-failed"); got.Status.Phase != v1alpha1.PhaseIndexing {
		t.Fatalf("expected phase Indexing after retry, got %s", got.Status.Phase)
	}
}

func TestKnowledgeBaseReconciler_SynthesizesPeriodicRefreshSchedule(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	kb := makeKB("default", "docs", "cls", inlineSource("content"))
	kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{Mode: "periodic", Interval: "6h"}
	storeKB(t, s, kb)
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "docs", Kind: knowledgeBaseKind}); err != nil {
			t.Fatalf("initial reconcile %d: %v", i+1, err)
		}
	}

	schedule := getIndexSchedule(t, s, "default", "docs-refresh")
	if schedule.Spec.KnowledgeBaseName != "docs" || schedule.Spec.Schedule != "@every 6h" {
		t.Fatalf("synthesized schedule spec = %#v", schedule.Spec)
	}
	kb = getKB(t, s, "default", "docs")
	owned := false
	for _, owner := range schedule.ObjectMeta.OwnerReferences {
		if owner.Kind == knowledgeBaseKind && owner.Name == kb.ObjectMeta.Name && owner.UID == kb.ObjectMeta.UID {
			owned = true
			break
		}
	}
	if !owned {
		t.Fatalf("synthesized schedule ownerRefs = %#v", schedule.ObjectMeta.OwnerReferences)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "docs", Kind: knowledgeBaseKind}); err != nil {
		t.Fatalf("reconcile periodic refresh schedule idempotently: %v", err)
	}
	if schedules, err := s.List(ctx, knowledgeBaseIndexScheduleKind, store.ListOptions{Namespace: "default"}); err != nil || len(schedules) != 1 {
		t.Fatalf("periodic schedules = %d, %v; want exactly one", len(schedules), err)
	}
}

func TestKnowledgeBaseReconciler_PeriodicScheduleRefreshesKnowledgeBase(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	kb := makeKB("default", "docs", "vector", inlineSource("content"))
	kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{Mode: "periodic", Interval: "15m"}
	storeKB(t, s, kb)
	kbc := makeKBClass("vector", "pgvector", "dense", v1alpha1.ReclaimDelete)
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
	storeKBClass(t, s, kbc)
	kbReconciler := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t)).WithIngesterLauncher(&fakeIngestLauncher{
		ensureState: IngestSucceeded,
		ensureStats: IngestStats{Documents: 1, Chunks: 10},
	})

	for i := 0; i < 3; i++ {
		if _, err := kbReconciler.Reconcile(ctx, Request{Namespace: "default", Name: "docs", Kind: knowledgeBaseKind}); err != nil {
			t.Fatalf("KnowledgeBase reconcile %d: %v", i+1, err)
		}
	}
	before := getKB(t, s, "default", "docs")
	if before.Status.Phase != v1alpha1.PhaseReady || before.Status.Generation != 1 {
		t.Fatalf("KnowledgeBase before periodic run = %#v", before.Status)
	}

	now := time.Date(2026, time.August, 14, 22, 30, 0, 0, time.UTC)
	schedule := getIndexSchedule(t, s, "default", "docs-refresh")
	schedule.Status.LastScheduleTime = now.Add(-16 * time.Minute).Format(time.RFC3339)
	updateIndexSchedule(t, s, schedule)
	scheduleReconciler := NewKnowledgeBaseIndexScheduleReconciler(s, zaptest.NewLogger(t))
	scheduleReconciler.now = func() time.Time { return now }
	if _, err := scheduleReconciler.Reconcile(ctx, Request{Namespace: "default", Name: schedule.ObjectMeta.Name, Kind: knowledgeBaseIndexScheduleKind}); err != nil {
		t.Fatalf("schedule reconcile: %v", err)
	}
	runs := listIndexRuns(t, s, "default")
	if len(runs) != 1 || !scheduleOwnsRun(&schedule, &runs[0]) {
		t.Fatalf("periodic IndexRuns = %#v, want one run owned by generated schedule", runs)
	}

	launcher := &fakeIndexRunLauncher{results: []indexRunLaunchResult{{
		state:   IngestSucceeded,
		stats:   IngestStats{Documents: 4, Chunks: 12, SourceDigest: "sha256:periodic"},
		jobName: "periodic-ingest",
	}}}
	runReconciler := NewKnowledgeBaseIndexRunReconciler(s, zaptest.NewLogger(t)).WithIndexRunLauncher(launcher)
	if _, err := runReconciler.Reconcile(ctx, Request{Namespace: "default", Name: runs[0].ObjectMeta.Name, Kind: knowledgeBaseIndexRunKind}); err != nil {
		t.Fatalf("IndexRun reconcile: %v", err)
	}
	after := getKB(t, s, "default", "docs")
	if after.Status.Phase != v1alpha1.PhaseReady || after.Status.Generation != 2 || after.Status.DocumentCount != 4 || after.Status.IndexedChunks != 12 {
		t.Fatalf("KnowledgeBase after periodic run = %#v", after.Status)
	}
}

func TestKnowledgeBaseReconciler_ReconcilesPeriodicRefreshScheduleLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	kb := makeKB("default", "docs", "cls", inlineSource("content"))
	kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{Mode: "periodic", Interval: "6h"}
	storeKB(t, s, kb)
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "docs", Kind: knowledgeBaseKind}); err != nil {
			t.Fatalf("initial reconcile %d: %v", i+1, err)
		}
	}

	kb = getKB(t, s, "default", "docs")
	kb.Spec.RefreshPolicy.Interval = "15m"
	data, err := json.Marshal(kb)
	if err != nil {
		t.Fatalf("marshal updated KnowledgeBase: %v", err)
	}
	if err := s.Update(ctx, knowledgeBaseKind, kb.ObjectMeta.Namespace, kb.ObjectMeta.Name, data, kb.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update periodic KnowledgeBase: %v", err)
	}
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "docs", Kind: knowledgeBaseKind}); err != nil {
		t.Fatalf("reconcile updated interval: %v", err)
	}
	if got := getIndexSchedule(t, s, "default", "docs-refresh").Spec.Schedule; got != "@every 15m" {
		t.Fatalf("updated refresh schedule = %q, want @every 15m", got)
	}

	kb = getKB(t, s, "default", "docs")
	kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{Mode: "onDemand"}
	data, err = json.Marshal(kb)
	if err != nil {
		t.Fatalf("marshal disabled periodic KnowledgeBase: %v", err)
	}
	if err := s.Update(ctx, knowledgeBaseKind, kb.ObjectMeta.Namespace, kb.ObjectMeta.Name, data, kb.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update disabled periodic KnowledgeBase: %v", err)
	}
	storeScheduleRun(t, s, getIndexSchedule(t, s, "default", "docs-refresh"), "docs-refresh-child", v1alpha1.PhaseRunning, time.Now().UTC(), "")
	storeIndexRun(t, s, makeIndexRun("default", "unrelated-run", "other"))
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "docs", Kind: knowledgeBaseKind}); err != nil {
		t.Fatalf("reconcile disabled periodic refresh: %v", err)
	}
	if _, err := s.Get(ctx, knowledgeBaseIndexScheduleKind, "default", "docs-refresh"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refresh schedule after disable get error = %v, want not found", err)
	}
	if runs := listIndexRuns(t, s, "default"); len(runs) != 1 || runs[0].ObjectMeta.Name != "unrelated-run" {
		t.Fatalf("refresh schedule cleanup runs after disable = %#v, want only unrelated-run", runs)
	}
}

func TestKnowledgeBaseReconciler_RetriesPeriodicRefreshScheduleUpdateConflict(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	storeKBClass(t, baseStore, makeKBClass("cls", "pgvector", "dense", v1alpha1.ReclaimDelete))
	s := &refreshScheduleConflictStore{memStore: baseStore}
	kb := makeKB("default", "docs", "cls", inlineSource("content"))
	kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{Mode: "periodic", Interval: "6h"}
	storeKB(t, baseStore, kb)
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "docs", Kind: knowledgeBaseKind}); err != nil {
			t.Fatalf("initial reconcile %d: %v", i+1, err)
		}
	}
	kb = getKB(t, baseStore, "default", "docs")
	kb.Spec.RefreshPolicy.Interval = "15m"
	data, err := json.Marshal(kb)
	if err != nil {
		t.Fatalf("marshal updated KnowledgeBase: %v", err)
	}
	if err := baseStore.Update(ctx, knowledgeBaseKind, kb.ObjectMeta.Namespace, kb.ObjectMeta.Name, data, kb.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update periodic KnowledgeBase: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "docs", Kind: knowledgeBaseKind}); err != nil {
		t.Fatalf("reconcile schedule update conflict: %v", err)
	}
	updated := getIndexSchedule(t, baseStore, "default", "docs-refresh")
	if !s.conflicted || updated.Spec.Schedule != "@every 15m" {
		t.Fatalf("updated refresh schedule = %#v, conflict=%t", updated, s.conflicted)
	}
	if updated.Status.NextScheduleTime != "2026-08-14T22:30:00Z" || len(updated.Status.Conditions) != 1 || updated.Status.Conditions[0].Type != "Audited" {
		t.Fatalf("concurrent schedule status was not preserved: %#v", updated.Status)
	}
	if current := getKB(t, baseStore, "default", "docs"); current.Status.Phase == v1alpha1.PhaseFailed {
		t.Fatalf("KnowledgeBase failed after transient schedule update conflict: %#v", current.Status)
	}
}

func TestKnowledgeBaseReconciler_FailsWhenPeriodicRefreshScheduleNameCollides(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	kb := makeKB("default", "docs", "cls", inlineSource("content"))
	kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{Mode: "periodic", Interval: "6h"}
	storeKB(t, s, kb)
	storeIndexSchedule(t, s, v1alpha1.KnowledgeBaseIndexSchedule{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: knowledgeBaseIndexScheduleKind},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "docs-refresh", Namespace: "default", UID: "foreign-schedule"},
		Spec:       v1alpha1.KnowledgeBaseIndexScheduleSpec{KnowledgeBaseName: "other", Schedule: "@every 1h"},
	})
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "docs", Kind: knowledgeBaseKind}); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "docs", Kind: knowledgeBaseKind}); err != nil {
		t.Fatalf("reconcile colliding refresh schedule: %v", err)
	}
	failed := getKB(t, s, "default", "docs")
	if failed.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("colliding KnowledgeBase phase = %q, want Failed", failed.Status.Phase)
	}
	ready := findKBCondition(failed, string(lifecycle.ConditionReady))
	if ready == nil || ready.Reason != "RefreshScheduleCollision" {
		t.Fatalf("colliding KnowledgeBase Ready condition = %#v, want RefreshScheduleCollision", ready)
	}
	if schedule := getIndexSchedule(t, s, "default", "docs-refresh"); schedule.Spec.KnowledgeBaseName != "other" {
		t.Fatalf("foreign schedule was modified: %#v", schedule.Spec)
	}
	if err := s.Delete(ctx, knowledgeBaseIndexScheduleKind, "default", "docs-refresh"); err != nil {
		t.Fatalf("delete foreign schedule: %v", err)
	}
	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "docs", Kind: knowledgeBaseKind})
	if err != nil {
		t.Fatalf("reconcile repaired refresh schedule: %v", err)
	}
	if !result.Requeue {
		t.Fatalf("reconcile repaired refresh schedule result = %#v, want requeue", result)
	}
	recovered := getKB(t, s, "default", "docs")
	if recovered.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("repaired refresh schedule KnowledgeBase phase = %q, want Pending", recovered.Status.Phase)
	}
	if schedule := getIndexSchedule(t, s, "default", "docs-refresh"); schedule.Spec.Schedule != "@every 6h" {
		t.Fatalf("repaired refresh schedule = %#v", schedule.Spec)
	}
}

func TestKnowledgeBaseReconciler_DeletesPeriodicRefreshScheduleWithKnowledgeBase(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	kb := makeKB("default", "docs", "cls", inlineSource("content"))
	kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{Mode: "periodic", Interval: "6h"}
	storeKB(t, s, kb)
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "docs", Kind: knowledgeBaseKind}); err != nil {
			t.Fatalf("initial reconcile %d: %v", i+1, err)
		}
	}
	kb = getKB(t, s, "default", "docs")
	now := time.Now().UTC()
	kb.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(kb)
	if err != nil {
		t.Fatalf("marshal deleting KnowledgeBase: %v", err)
	}
	if err := s.Update(ctx, knowledgeBaseKind, kb.ObjectMeta.Namespace, kb.ObjectMeta.Name, data, kb.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update deleting KnowledgeBase: %v", err)
	}
	storeScheduleRun(t, s, getIndexSchedule(t, s, "default", "docs-refresh"), "docs-refresh-child", v1alpha1.PhaseRunning, time.Now().UTC(), "")
	storeIndexRun(t, s, makeIndexRun("default", "unrelated-run", "other"))
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "docs", Kind: knowledgeBaseKind}); err != nil {
		t.Fatalf("reconcile deleting KnowledgeBase: %v", err)
	}
	if _, err := s.Get(ctx, knowledgeBaseIndexScheduleKind, "default", "docs-refresh"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("refresh schedule after KnowledgeBase deletion get error = %v, want not found", err)
	}
	if runs := listIndexRuns(t, s, "default"); len(runs) != 1 || runs[0].ObjectMeta.Name != "unrelated-run" {
		t.Fatalf("refresh schedule cleanup runs after KnowledgeBase deletion = %#v, want only unrelated-run", runs)
	}
}

func TestKnowledgeBaseRefreshScheduleNameBoundsLongNames(t *testing.T) {
	name := knowledgeBaseRefreshScheduleName(strings.Repeat("a", 253))
	if len(name) > 253 || !strings.Contains(name, "-refresh-") {
		t.Fatalf("long refresh schedule name = %q (length %d), want bounded hashed refresh name", name, len(name))
	}
	if name != knowledgeBaseRefreshScheduleName(strings.Repeat("a", 253)) {
		t.Fatalf("long refresh schedule name is not deterministic: %q", name)
	}
}

func TestKnowledgeBaseRefreshScheduleOwnershipRequiresExactUID(t *testing.T) {
	kb := makeKB("default", "docs", "cls", inlineSource("content"))
	kb.ObjectMeta.UID = ""
	foreign := &v1alpha1.KnowledgeBaseIndexSchedule{ObjectMeta: v1alpha1.ObjectMeta{OwnerReferences: []v1alpha1.OwnerReference{{
		APIVersion: v1alpha1.APIVersion,
		Kind:       knowledgeBaseKind,
		Name:       kb.ObjectMeta.Name,
		UID:        "different-kb",
	}}}}
	if knowledgeBaseOwnsRefreshSchedule(&kb, foreign) {
		t.Fatal("empty-UID KnowledgeBase adopted a schedule owned by a different UID")
	}
	foreign.ObjectMeta.OwnerReferences[0].UID = ""
	if knowledgeBaseOwnsRefreshSchedule(&kb, foreign) {
		t.Fatal("empty-UID KnowledgeBase adopted a schedule without immutable ownership")
	}
}

func TestKnowledgeBaseRefreshScheduleRejectsInvalidPolicies(t *testing.T) {
	for _, policy := range []*v1alpha1.KBRefreshPolicy{
		{Mode: "periodic", Interval: "0s"},
		{Mode: "periodic", Interval: "-5m"},
		{Mode: "manual"},
	} {
		t.Run(policy.Mode+policy.Interval, func(t *testing.T) {
			kb := makeKB("default", "docs", "cls", inlineSource("content"))
			kb.Spec.RefreshPolicy = policy
			if _, _, err := knowledgeBaseRefreshSchedule(&kb); err == nil {
				t.Fatalf("knowledgeBaseRefreshSchedule(%#v) error = nil, want error", policy)
			}
		})
	}
}

// A non-ingest failure (e.g. the class was removed) must NOT be auto-retried.
func TestKnowledgeBaseReconciler_DoesNotRetryNonIngestFailure(t *testing.T) {
	s := newMemStore()
	launcher := &fakeIngestLauncher{}
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t)).WithIngesterLauncher(launcher)
	storeKB(t, s, failedKB("ClassRemoved", 5*time.Minute))

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "kb-failed", Kind: "KnowledgeBase"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if launcher.resetCalls != 0 {
		t.Fatalf("expected no Reset for non-ingest failure, got %d", launcher.resetCalls)
	}
	if got := getKB(t, s, "default", "kb-failed"); got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected phase to stay Failed, got %s", got.Status.Phase)
	}
}

// A recent ingest failure should back off (not retry immediately).
func TestKnowledgeBaseReconciler_BacksOffBeforeRetry(t *testing.T) {
	s := newMemStore()
	launcher := &fakeIngestLauncher{}
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t)).WithIngesterLauncher(launcher)
	storeKB(t, s, failedKB("IngestFailed", 5*time.Second))

	res, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "kb-failed", Kind: "KnowledgeBase"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if launcher.resetCalls != 0 {
		t.Fatalf("expected no Reset during backoff, got %d", launcher.resetCalls)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("expected a requeue-after during backoff, got %v", res.RequeueAfter)
	}
	if got := getKB(t, s, "default", "kb-failed"); got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected phase to stay Failed during backoff, got %s", got.Status.Phase)
	}
}

func TestKnowledgeBaseReconciler_MissingClassFailsExternalIngest(t *testing.T) {
	for _, phase := range []v1alpha1.Phase{v1alpha1.PhaseIndexing, v1alpha1.PhaseRefreshing} {
		t.Run(string(phase), func(t *testing.T) {
			store := newMemStore()
			launcher := &fakeIngestLauncher{}
			reconciler := NewKnowledgeBaseReconciler(store, zaptest.NewLogger(t)).WithIngesterLauncher(launcher)
			kb := makeKB("default", "kb-missing-class", "missing-class", inlineSource("data"))
			kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer}
			kb.Status.Phase = phase
			storeKB(t, store, kb)

			result, err := reconciler.Reconcile(context.Background(), Request{
				Namespace: "default", Name: kb.ObjectMeta.Name, Kind: knowledgeBaseKind,
			})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if result.RequeueAfter != 30*time.Second {
				t.Fatalf("RequeueAfter = %v, want 30s", result.RequeueAfter)
			}
			stored := getKB(t, store, "default", kb.ObjectMeta.Name)
			if stored.Status.Phase != v1alpha1.PhaseFailed {
				t.Fatalf("phase = %q, want Failed", stored.Status.Phase)
			}
			condition := findKBCondition(stored, string(lifecycle.ConditionReady))
			if condition == nil || condition.Status != string(lifecycle.ConditionFalse) || condition.Reason != "ClassNotFound" {
				t.Fatalf("Ready condition = %#v, want False/ClassNotFound", condition)
			}
		})
	}
}

func TestKnowledgeBaseReconciler_ConfiguredBackendCannotSimulateSuccess(t *testing.T) {
	for _, phase := range []v1alpha1.Phase{v1alpha1.PhaseIndexing, v1alpha1.PhaseRefreshing} {
		for _, scenario := range []struct {
			name       string
			launcher   IngesterLauncher
			referenced bool
		}{
			{name: "missing launcher"},
			{name: "unsupported backend", launcher: &fakeIngestLauncher{ensureState: IngestSkipped}},
			{name: "invalid response", launcher: &fakeIngestLauncher{ensureState: IngestState(99)}},
			{name: "reference missing launcher", referenced: true},
			{name: "reference unsupported backend", referenced: true, launcher: &fakeIngestLauncher{ensureState: IngestSkipped}},
			{name: "reference invalid response", referenced: true, launcher: &fakeIngestLauncher{ensureState: IngestState(99)}},
		} {
			t.Run(string(phase)+"/"+scenario.name, func(t *testing.T) {
				storage := newMemStore()
				class := makeKBClass("external-class", "pgvector", "dense", v1alpha1.ReclaimDelete)
				class.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
				if scenario.referenced {
					class.Spec.VectorStore = nil
					class.Spec.BackendRef = &v1alpha1.KnowledgeBackendReference{APIGroup: "pgvector.knowledge.kognition.dev", Kind: "PgvectorBackend", Name: "shared"}
				}
				storeKBClass(t, storage, class)
				kb := makeKB("default", "kb-external", class.ObjectMeta.Name, inlineSource("data"))
				kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer}
				kb.Status.Phase = phase
				kb.Status.Generation = 4
				kb.Status.DocumentCount = 7
				kb.Status.IndexedChunks = 23
				kb.Status.LastRefresh = "2026-09-18T00:00:00Z"
				storeKB(t, storage, kb)
				reconciler := NewKnowledgeBaseReconciler(storage, zaptest.NewLogger(t)).WithIngesterLauncher(scenario.launcher)
				result, err := reconciler.Reconcile(context.Background(), Request{
					Namespace: "default", Name: kb.ObjectMeta.Name, Kind: knowledgeBaseKind,
				})
				if err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				stored := getKB(t, storage, "default", kb.ObjectMeta.Name)
				if stored.Status.Phase != v1alpha1.PhaseFailed || result.RequeueAfter <= 0 {
					t.Fatalf("phase/requeue = %s/%v, want Failed with retry", stored.Status.Phase, result.RequeueAfter)
				}
				if stored.Status.Generation != 4 || stored.Status.DocumentCount != 7 || stored.Status.IndexedChunks != 23 || stored.Status.LastRefresh != kb.Status.LastRefresh {
					t.Fatalf("unsuccessful ingestion changed committed status: %#v", stored.Status)
				}
				condition := findKBCondition(stored, string(lifecycle.ConditionReady))
				if condition == nil || condition.Status != string(lifecycle.ConditionFalse) || condition.Message == "" {
					t.Fatalf("Ready condition = %#v, want False with actionable message", condition)
				}
				for index := range stored.Status.Conditions {
					stored.Status.Conditions[index].LastTransitionTime = time.Now().Add(-5 * time.Minute)
				}
				data, err := json.Marshal(stored)
				if err != nil {
					t.Fatalf("marshal failure status: %v", err)
				}
				if err := storage.Update(context.Background(), knowledgeBaseKind, "default", kb.ObjectMeta.Name, data, stored.ObjectMeta.ResourceVersion); err != nil {
					t.Fatalf("advance retry age: %v", err)
				}
				reconciler.WithIngesterLauncher(&fakeIngestLauncher{
					ensureState: IngestSucceeded,
					ensureStats: IngestStats{Documents: 8, Chunks: 29, BackendBinding: func() *v1alpha1.KnowledgeBackendBinding {
						if !scenario.referenced {
							return nil
						}
						return providerBindingFor(kb, class)
					}()},
				})
				for attempt := 0; attempt < 2; attempt++ {
					if _, err := reconciler.Reconcile(context.Background(), Request{
						Namespace: "default", Name: kb.ObjectMeta.Name, Kind: knowledgeBaseKind,
					}); err != nil {
						t.Fatalf("reconcile restored provider: %v", err)
					}
				}
				recovered := getKB(t, storage, "default", kb.ObjectMeta.Name)
				if recovered.Status.Phase != v1alpha1.PhaseReady || recovered.Status.Generation != 5 || recovered.Status.DocumentCount != 8 || recovered.Status.IndexedChunks != 29 {
					t.Fatalf("provider restoration did not commit real ingestion: %#v", recovered.Status)
				}
				if scenario.referenced && (recovered.Status.BackendBinding == nil || recovered.Status.BackendBinding.Generation != 5 || recovered.Status.BackendBinding.ClassUID != class.ObjectMeta.UID) {
					t.Fatalf("provider restoration binding = %#v, want exact generation/class confirmation", recovered.Status.BackendBinding)
				}
			})
		}
	}
}

func TestKnowledgeBaseReconciler_ProviderConfirmationMustMatchPendingBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*v1alpha1.KnowledgeBackendBinding)
	}{
		{name: "missing", mutate: func(binding *v1alpha1.KnowledgeBackendBinding) { *binding = v1alpha1.KnowledgeBackendBinding{} }},
		{name: "stale generation", mutate: func(binding *v1alpha1.KnowledgeBackendBinding) { binding.Generation-- }},
		{name: "wrong provider", mutate: func(binding *v1alpha1.KnowledgeBackendBinding) { binding.ProviderName = "other" }},
		{name: "recreated class", mutate: func(binding *v1alpha1.KnowledgeBackendBinding) { binding.ClassUID = "replacement" }},
		{name: "changed class", mutate: func(binding *v1alpha1.KnowledgeBackendBinding) { binding.ClassGeneration++ }},
		{name: "missing configuration uid", mutate: func(binding *v1alpha1.KnowledgeBackendBinding) { binding.ConfigurationUID = "" }},
		{name: "missing configuration digest", mutate: func(binding *v1alpha1.KnowledgeBackendBinding) { binding.ConfigurationDigest = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := newMemStore()
			class := makeKBClass("external-class", "azuresearch", "dense", v1alpha1.ReclaimDelete)
			class.ObjectMeta.Generation = 3
			class.Spec.BackendRef = &v1alpha1.KnowledgeBackendReference{
				APIGroup: "search.kognition.dev", Kind: "AzureSearchBackend", Name: "shared",
			}
			storeKBClass(t, storage, class)

			kb := makeKB("default", "kb-external", class.ObjectMeta.Name, inlineSource("data"))
			kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer}
			kb.Status.Phase = v1alpha1.PhaseRefreshing
			kb.Status.Generation = 4
			kb.Status.DocumentCount = 7
			kb.Status.IndexedChunks = 23
			kb.Status.BackendBinding = &v1alpha1.KnowledgeBackendBinding{
				Generation: 4, ProviderName: "azuresearch", ClassUID: class.ObjectMeta.UID, ClassGeneration: 2,
				ConfigurationUID: "old-configuration", ConfigurationDigest: strings.Repeat("b", 64),
			}
			committed := *kb.Status.BackendBinding
			confirmation := providerBindingFor(kb, class)
			if tc.name == "missing" {
				confirmation = nil
			} else {
				tc.mutate(confirmation)
			}
			storeKB(t, storage, kb)

			reconciler := NewKnowledgeBaseReconciler(storage, zaptest.NewLogger(t)).WithIngesterLauncher(&fakeIngestLauncher{
				ensureState: IngestSucceeded,
				ensureStats: IngestStats{Documents: 8, Chunks: 29, BackendBinding: confirmation},
			})
			result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: kb.ObjectMeta.Name, Kind: knowledgeBaseKind})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			stored := getKB(t, storage, "default", kb.ObjectMeta.Name)
			if stored.Status.Phase != v1alpha1.PhaseFailed || result.RequeueAfter <= 0 {
				t.Fatalf("phase/requeue = %s/%v, want Failed with retry", stored.Status.Phase, result.RequeueAfter)
			}
			if stored.Status.Generation != 4 || stored.Status.DocumentCount != 7 || stored.Status.IndexedChunks != 23 {
				t.Fatalf("invalid confirmation changed committed generation: %#v", stored.Status)
			}
			if stored.Status.BackendBinding == nil || *stored.Status.BackendBinding != committed {
				t.Fatalf("invalid confirmation replaced committed binding: %#v", stored.Status.BackendBinding)
			}
		})
	}
}

func TestKnowledgeBaseReconciler_ExternalIngesterOutcomes(t *testing.T) {
	for _, phase := range []v1alpha1.Phase{v1alpha1.PhaseIndexing, v1alpha1.PhaseRefreshing} {
		for _, tc := range []struct {
			name           string
			state          IngestState
			stats          IngestStats
			err            error
			wantPhase      v1alpha1.Phase
			wantGeneration int64
			wantRequeue    time.Duration
		}{
			{name: "running", state: IngestRunning, wantPhase: phase, wantRequeue: 10 * time.Second},
			{name: "transient error", err: errors.New("launcher unavailable"), wantPhase: phase, wantRequeue: 15 * time.Second},
			{name: "transient not found", err: store.ErrNotFound, wantPhase: phase, wantRequeue: 15 * time.Second},
			{name: "succeeded", state: IngestSucceeded, stats: IngestStats{Documents: 7, Chunks: 23}, wantPhase: v1alpha1.PhaseReady, wantGeneration: 1},
			{name: "failed", state: IngestFailed, stats: IngestStats{Error: "source unavailable"}, wantPhase: v1alpha1.PhaseFailed, wantRequeue: 30 * time.Second},
		} {
			t.Run(string(phase)+"/"+tc.name, func(t *testing.T) {
				store := newMemStore()
				class := makeKBClass("external-class", "pgvector", "dense", v1alpha1.ReclaimDelete)
				class.Spec.VectorStore = &v1alpha1.KBVectorStore{Type: "pgvector"}
				storeKBClass(t, store, class)
				launcher := &fakeIngestLauncher{ensureState: tc.state, ensureStats: tc.stats, ensureErr: tc.err}
				reconciler := NewKnowledgeBaseReconciler(store, zaptest.NewLogger(t)).WithIngesterLauncher(launcher)
				kb := makeKB("default", "kb-external", class.ObjectMeta.Name, inlineSource("data"))
				kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer}
				kb.Status.Phase = phase
				storeKB(t, store, kb)

				result, err := reconciler.Reconcile(context.Background(), Request{
					Namespace: "default", Name: kb.ObjectMeta.Name, Kind: knowledgeBaseKind,
				})
				if err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				if result.RequeueAfter != tc.wantRequeue {
					t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, tc.wantRequeue)
				}
				if launcher.ensureCalls != 1 {
					t.Fatalf("Ensure calls = %d, want 1", launcher.ensureCalls)
				}
				stored := getKB(t, store, "default", kb.ObjectMeta.Name)
				if stored.Status.Phase != tc.wantPhase || stored.Status.Generation != tc.wantGeneration {
					t.Fatalf("status phase/generation = %q/%d, want %q/%d", stored.Status.Phase, stored.Status.Generation, tc.wantPhase, tc.wantGeneration)
				}
				if tc.state == IngestSucceeded && (stored.Status.DocumentCount != 7 || stored.Status.IndexedChunks != 23) {
					t.Fatalf("ingest stats = %d/%d, want 7/23", stored.Status.DocumentCount, stored.Status.IndexedChunks)
				}
				if tc.state == IngestFailed {
					wantReason := "IngestFailed"
					if phase == v1alpha1.PhaseRefreshing {
						wantReason = "RefreshFailed"
					}
					condition := findKBCondition(stored, string(lifecycle.ConditionReady))
					if condition == nil || condition.Reason != wantReason || condition.Message != tc.stats.Error {
						t.Fatalf("Ready condition = %#v, want %s/%q", condition, wantReason, tc.stats.Error)
					}
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// TC-0038: Create KB with inline sources → Pending→Indexing→Ready
// ---------------------------------------------------------------------------

func TestKnowledgeBaseReconciler_TC0038_InlineSources(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	storeKBClass(t, s, makeKBClass("default-class", "pgvector", "dense", v1alpha1.ReclaimDelete))
	storeKB(t, s, makeKB("default", "kb-docs", "default-class",
		inlineSource("document one"),
		inlineSource("document two"),
	))

	reconcileKB(t, r, "default", "kb-docs")

	kb := getKB(t, s, "default", "kb-docs")
	if kb.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", kb.Status.Phase)
	}
	if kb.Status.DocumentCount == 0 {
		t.Fatal("DocumentCount should be > 0 after indexing")
	}
}

// ---------------------------------------------------------------------------
// TC-0039: Ready KB status fields are populated correctly
// ---------------------------------------------------------------------------

func TestKnowledgeBaseReconciler_TC0039_ReadyStatusFields(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	storeKBClass(t, s, makeKBClass("status-class", "qdrant", "hybrid", v1alpha1.ReclaimRetain))
	storeKB(t, s, makeKB("default", "kb-status", "status-class",
		inlineSource("doc-a"),
		inlineSource("doc-b"),
		inlineSource("doc-c"),
	))

	reconcileKB(t, r, "default", "kb-status")

	kb := getKB(t, s, "default", "kb-status")

	if kb.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", kb.Status.Phase)
	}
	if kb.Status.DocumentCount == 0 {
		t.Error("DocumentCount should be > 0")
	}
	if kb.Status.IndexedChunks == 0 {
		t.Error("IndexedChunks should be > 0")
	}
	if kb.Status.Generation != 1 {
		t.Errorf("Generation = %d, want 1", kb.Status.Generation)
	}
	if kb.Status.LastRefresh == "" {
		t.Error("LastRefresh should not be empty")
	}
	if _, err := time.Parse(time.RFC3339, kb.Status.LastRefresh); err != nil {
		t.Errorf("LastRefresh %q is not a valid RFC3339 timestamp: %v", kb.Status.LastRefresh, err)
	}

	cond := findKBCondition(kb, "Ready")
	if cond == nil {
		t.Fatal("Ready condition not found")
	}
	if cond.Status != "True" {
		t.Errorf("Ready condition status = %q, want True", cond.Status)
	}
}

// ---------------------------------------------------------------------------
// TC-0040: KnowledgeBase type is read-only at the spec level after creation
// (verified by confirming Spec is unchanged after reconcile loops).
// ---------------------------------------------------------------------------

func TestKnowledgeBaseReconciler_TC0040_SpecUnchangedAfterReconcile(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	originalSpec := v1alpha1.KnowledgeBaseSpec{
		KnowledgeBaseClassName: "ro-class",
		Sources:                []v1alpha1.KBSource{inlineSource("unchanged")},
	}
	storeKBClass(t, s, makeKBClass("ro-class", "memory", "", v1alpha1.ReclaimDelete))
	kb := makeKB("default", "kb-ro", "ro-class", inlineSource("unchanged"))
	storeKB(t, s, kb)

	reconcileKB(t, r, "default", "kb-ro")

	got := getKB(t, s, "default", "kb-ro")
	if got.Spec.KnowledgeBaseClassName != originalSpec.KnowledgeBaseClassName {
		t.Errorf("Spec.KnowledgeBaseClassName mutated: got %q", got.Spec.KnowledgeBaseClassName)
	}
	if len(got.Spec.Sources) != len(originalSpec.Sources) {
		t.Errorf("Spec.Sources length changed: got %d, want %d", len(got.Spec.Sources), len(originalSpec.Sources))
	}
	if got.Spec.Sources[0].Data != originalSpec.Sources[0].Data {
		t.Errorf("Spec.Sources[0].Data mutated: got %q", got.Spec.Sources[0].Data)
	}
}

// ---------------------------------------------------------------------------
// TC-0155: KBClass + KB → full lifecycle, generation=1, conditions[Ready]=True
// ---------------------------------------------------------------------------

func TestKnowledgeBaseReconciler_TC0155_FullLifecycle(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	storeKBClass(t, s, makeKBClass("prod-class", "pgvector", "dense", v1alpha1.ReclaimDelete))
	storeKB(t, s, makeKB("prod", "kb-main", "prod-class",
		inlineSource("architecture docs"),
		inlineSource("api reference"),
	))

	reconcileKB(t, r, "prod", "kb-main")

	kb := getKB(t, s, "prod", "kb-main")

	if kb.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", kb.Status.Phase)
	}
	if kb.Status.Generation != 1 {
		t.Errorf("Generation = %d, want 1", kb.Status.Generation)
	}
	if kb.Status.DocumentCount != 2 {
		t.Errorf("DocumentCount = %d, want 2", kb.Status.DocumentCount)
	}
	if kb.Status.IndexedChunks != 20 {
		t.Errorf("IndexedChunks = %d, want 20 (2 docs * 10 chunks)", kb.Status.IndexedChunks)
	}

	cond := findKBCondition(kb, "Ready")
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Status != "True" {
		t.Errorf("Ready condition status = %q, want True", cond.Status)
	}
}

// ---------------------------------------------------------------------------
// TC-0158: Refreshing phase → generation stays at prior value during refresh,
// increments to gen+1 after Ready.
// ---------------------------------------------------------------------------

func TestKnowledgeBaseReconciler_TC0158_RefreshIncreasesGeneration(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	storeKBClass(t, s, makeKBClass("refresh-class", "pgvector", "dense", v1alpha1.ReclaimRetain))
	storeKB(t, s, makeKB("default", "kb-refresh", "refresh-class", inlineSource("initial")))

	// Bring to Ready (generation=1).
	reconcileKB(t, r, "default", "kb-refresh")
	kb := getKB(t, s, "default", "kb-refresh")
	if kb.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected Ready after initial indexing, got %s", kb.Status.Phase)
	}
	if kb.Status.Generation != 1 {
		t.Fatalf("expected generation=1 after indexing, got %d", kb.Status.Generation)
	}

	// Manually transition to Refreshing to simulate an on-demand refresh trigger.
	kb.Status.Phase = v1alpha1.PhaseRefreshing
	data, _ := json.Marshal(kb)
	if err := s.Update(context.Background(), "KnowledgeBase", "default", "kb-refresh", data, kb.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("set Refreshing phase: %v", err)
	}

	// Verify generation is still 1 while in Refreshing (before reconcile).
	kbMid := getKB(t, s, "default", "kb-refresh")
	if kbMid.Status.Phase != v1alpha1.PhaseRefreshing {
		t.Fatalf("phase = %q, want Refreshing", kbMid.Status.Phase)
	}
	if kbMid.Status.Generation != 1 {
		t.Errorf("Generation during refresh = %d, want 1", kbMid.Status.Generation)
	}

	// Complete the refresh.
	reconcileKB(t, r, "default", "kb-refresh")
	kbFinal := getKB(t, s, "default", "kb-refresh")
	if kbFinal.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase after refresh = %q, want Ready", kbFinal.Status.Phase)
	}
	if kbFinal.Status.Generation != 2 {
		t.Errorf("Generation after refresh = %d, want 2", kbFinal.Status.Generation)
	}
}

// ---------------------------------------------------------------------------
// TC-0160: KB deleted with agent required=true → agent not-ready;
//          agent required=false → agent condition unaffected.
// ---------------------------------------------------------------------------

func TestKnowledgeBaseReconciler_TC0160_AgentReadinessOnDeletion(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	storeKBClass(t, s, makeKBClass("del-class", "pgvector", "dense", v1alpha1.ReclaimDelete))
	storeKB(t, s, makeKB("default", "kb-del", "del-class", inlineSource("data")))

	// Claims binding to the KB (agents reference claims, never KBs directly).
	storeKBClaim(t, s, makeTestKBClaim("default", "claim-required", "kb-del"))
	storeKBClaim(t, s, makeTestKBClaim("default", "claim-optional", "kb-del"))

	// Agent with required=true ref.
	storeAgent(t, s, makeTestAgent("default", "agent-required", "claim-required", true))
	// Agent with required=false ref.
	storeAgent(t, s, makeTestAgent("default", "agent-optional", "claim-optional", false))

	// Bring KB to Ready.
	reconcileKB(t, r, "default", "kb-del")
	kb := getKB(t, s, "default", "kb-del")
	if kb.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected Ready, got %s", kb.Status.Phase)
	}

	// Trigger deletion by setting DeletionTimestamp.
	now := time.Now()
	kb.ObjectMeta.DeletionTimestamp = &now
	// Ensure KB has the finalizer so handleDeletion triggers the notify.
	if !contains(kb.ObjectMeta.Finalizers, KnowledgeBaseFinalizer) {
		kb.ObjectMeta.Finalizers = append(kb.ObjectMeta.Finalizers, KnowledgeBaseFinalizer)
	}
	kb.ObjectMeta.Finalizers = append(kb.ObjectMeta.Finalizers, "example.dev/external-cleanup")
	data, _ := json.Marshal(kb)
	if err := s.Update(context.Background(), "KnowledgeBase", "default", "kb-del", data, kb.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("set DeletionTimestamp: %v", err)
	}

	// Reconcile deletion.
	ctx := context.Background()
	req := Request{Namespace: "default", Name: "kb-del", Kind: "KnowledgeBase"}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}
	deletedKB := getKB(t, s, "default", "kb-del")
	if contains(deletedKB.ObjectMeta.Finalizers, KnowledgeBaseFinalizer) {
		t.Fatalf("Kognition finalizer was not removed: %v", deletedKB.ObjectMeta.Finalizers)
	}
	if !contains(deletedKB.ObjectMeta.Finalizers, "example.dev/external-cleanup") {
		t.Fatalf("external finalizer was removed: %v", deletedKB.ObjectMeta.Finalizers)
	}

	// Agent with required=true must have KnowledgeBaseReady=False.
	agentReq := getAgent(t, s, "default", "agent-required")
	condReq := findAgentCondition(agentReq, "KnowledgeBaseReady")
	if condReq == nil {
		t.Fatal("agent-required: KnowledgeBaseReady condition not set")
	}
	if condReq.Status != "False" {
		t.Errorf("agent-required KnowledgeBaseReady = %q, want False", condReq.Status)
	}

	// Agent with required=false must NOT have KnowledgeBaseReady condition set to False.
	agentOpt := getAgent(t, s, "default", "agent-optional")
	condOpt := findAgentCondition(agentOpt, "KnowledgeBaseReady")
	if condOpt != nil && condOpt.Status == "False" {
		t.Errorf("agent-optional KnowledgeBaseReady should not be False, got %q", condOpt.Status)
	}
}

func TestKnowledgeBaseReconciler_DeletionSkipsMalformedClaimsAndAgents(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))
	storeKBClass(t, s, makeKBClass("del-class", "pgvector", "dense", v1alpha1.ReclaimDelete))

	now := time.Now()
	kb := makeKB("default", "kb-del-mixed", "del-class", inlineSource("data"))
	kb.ObjectMeta.DeletionTimestamp = &now
	kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer, "example.dev/external-cleanup"}
	kb.Status.Phase = v1alpha1.PhaseReady
	storeKB(t, s, kb)

	storeKBClaim(t, s, makeTestKBClaim("default", "valid-claim", kb.ObjectMeta.Name))
	if err := s.Create(context.Background(), "KnowledgeBaseClaim", "default", "malformed-claim", []byte("{")); err != nil {
		t.Fatalf("store malformed claim: %v", err)
	}
	storeAgent(t, s, makeTestAgent("default", "referencing-agent", "valid-claim", true))
	storeAgent(t, s, makeTestAgent("default", "unrelated-agent", "other-claim", true))
	if err := s.Create(context.Background(), "Agent", "default", "malformed-agent", []byte("{")); err != nil {
		t.Fatalf("store malformed agent: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: kb.ObjectMeta.Name, Kind: "KnowledgeBase"}); err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}

	updatedKB := getKB(t, s, "default", kb.ObjectMeta.Name)
	if contains(updatedKB.ObjectMeta.Finalizers, KnowledgeBaseFinalizer) {
		t.Fatalf("Kognition finalizer was not removed: %v", updatedKB.ObjectMeta.Finalizers)
	}
	if !contains(updatedKB.ObjectMeta.Finalizers, "example.dev/external-cleanup") {
		t.Fatalf("external finalizer was removed: %v", updatedKB.ObjectMeta.Finalizers)
	}

	referencing := getAgent(t, s, "default", "referencing-agent")
	condition := findAgentCondition(referencing, "KnowledgeBaseReady")
	if condition == nil || condition.Status != "False" {
		t.Fatalf("referencing agent condition = %#v, want KnowledgeBaseReady=False", condition)
	}
	unrelated := getAgent(t, s, "default", "unrelated-agent")
	if condition := findAgentCondition(unrelated, "KnowledgeBaseReady"); condition != nil {
		t.Fatalf("unrelated agent condition = %#v, want none", condition)
	}

	for _, record := range []struct {
		kind string
		name string
	}{
		{kind: "KnowledgeBaseClaim", name: "malformed-claim"},
		{kind: "Agent", name: "malformed-agent"},
	} {
		raw, err := s.Get(context.Background(), record.kind, "default", record.name)
		if err != nil {
			t.Fatalf("get %s %s: %v", record.kind, record.name, err)
		}
		if string(raw) != "{" {
			t.Fatalf("%s %s changed to %q, want malformed record preserved", record.kind, record.name, raw)
		}
	}
}

func TestKnowledgeBaseReconciler_DeletionStopsWhenReclaimPolicyLookupFails(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	now := time.Now().UTC()
	kb := makeKB("default", "kb-policy-error", "retain-class", inlineSource("data"))
	kb.ObjectMeta.DeletionTimestamp = &now
	kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer}
	storeKB(t, baseStore, kb)

	lookupErr := errors.New("knowledge base class store unavailable")
	r := NewKnowledgeBaseReconciler(&knowledgeBaseClassLookupFailureStore{Store: baseStore, err: lookupErr}, zaptest.NewLogger(t))
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: kb.ObjectMeta.Name, Kind: knowledgeBaseKind}); !errors.Is(err, lookupErr) {
		t.Fatalf("reconcile error = %v, want wrapped lookup error", err)
	}
	updated := getKB(t, baseStore, "default", kb.ObjectMeta.Name)
	if !contains(updated.ObjectMeta.Finalizers, KnowledgeBaseFinalizer) {
		t.Fatal("knowledge base finalizer was removed after policy lookup failure")
	}
}

func TestKnowledgeBaseReconciler_ProviderDeletionRequiresConfirmedCleanup(t *testing.T) {
	for _, policy := range []v1alpha1.ReclaimPolicy{v1alpha1.ReclaimDelete, v1alpha1.ReclaimRetain, v1alpha1.ReclaimArchive} {
		for _, missingClass := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/missing-class=%t", policy, missingClass), func(t *testing.T) {
				storage := newMemStore()
				class := makeKBClass("provider-class", "pgvector", "dense", policy)
				class.Spec.BackendRef = &v1alpha1.KnowledgeBackendReference{APIGroup: "pgvector.knowledge.kognition.dev", Kind: "PgvectorBackend", Name: "shared"}
				if !missingClass {
					storeKBClass(t, storage, class)
				}
				kb := makeKB("default", "provider-kb", "provider-class", inlineSource("data"))
				now := time.Now().UTC()
				kb.ObjectMeta.DeletionTimestamp = &now
				kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer, "example.dev/cleanup"}
				kb.Status.Generation = 3
				if missingClass {
					kb.Status.BackendBinding = &v1alpha1.KnowledgeBackendBinding{Generation: 3, ProviderName: "pgvector"}
				}
				storeKB(t, storage, kb)
				reconciler := NewKnowledgeBaseReconciler(storage, zaptest.NewLogger(t))
				result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: "provider-kb"})
				if err != nil || result.RequeueAfter <= 0 {
					t.Fatalf("unconfirmed cleanup must remain retryable: %+v, %v", result, err)
				}
				updated := getKB(t, storage, "default", "provider-kb")
				condition := findConditionByType(updated.Status.Conditions, string(lifecycle.ConditionReady))
				if !contains(updated.ObjectMeta.Finalizers, KnowledgeBaseFinalizer) || !contains(updated.ObjectMeta.Finalizers, "example.dev/cleanup") || updated.Status.Generation != 3 || condition == nil || condition.Reason != "ProviderCleanupRequired" {
					t.Fatalf("unconfirmed cleanup lost ownership or recovery state: %+v", updated)
				}
			})
		}
	}
}

type recordingKnowledgeCleaner struct {
	calls  int
	policy v1alpha1.ReclaimPolicy
	kb     *v1alpha1.KnowledgeBase
	err    error
	events *[]string
}

type recordingRetainedBindingRecorder struct {
	calls  int
	err    error
	events *[]string
}

func (recorder *recordingRetainedBindingRecorder) Record(_ context.Context, _ *v1alpha1.KnowledgeBase) error {
	recorder.calls++
	if recorder.events != nil {
		*recorder.events = append(*recorder.events, "record")
	}
	return recorder.err
}

func (cleaner *recordingKnowledgeCleaner) Cleanup(_ context.Context, kb *v1alpha1.KnowledgeBase, policy v1alpha1.ReclaimPolicy) error {
	cleaner.calls++
	cleaner.policy = policy
	cleaner.kb = kb
	if cleaner.events != nil {
		*cleaner.events = append(*cleaner.events, "cleanup")
	}
	return cleaner.err
}

func TestKnowledgeBaseReconciler_ProviderDeletionRemovesFinalizerAfterConfirmedCleanup(t *testing.T) {
	storage := newMemStore()
	class := makeKBClass("provider-class", "azuresearch", "dense", v1alpha1.ReclaimDelete)
	class.Spec.BackendRef = &v1alpha1.KnowledgeBackendReference{APIGroup: "search.knowledge.kognition.dev", Kind: "AzureSearchBackend", Name: "shared"}
	storeKBClass(t, storage, class)
	kb := makeKB("default", "provider-kb", "provider-class", inlineSource("data"))
	now := time.Now().UTC()
	kb.ObjectMeta.DeletionTimestamp = &now
	kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer, "example.dev/cleanup"}
	kb.Status.Generation = 3
	kb.Status.BackendBinding = providerBindingFor(kb, class)
	kb.Status.BackendBinding.Generation = kb.Status.Generation
	storeKB(t, storage, kb)
	cleaner := &recordingKnowledgeCleaner{}
	recorder := &recordingRetainedBindingRecorder{}
	reconciler := NewKnowledgeBaseReconciler(storage, zaptest.NewLogger(t)).
		WithBackendCleaner(cleaner).
		WithRetainedBindingRecorder(recorder)

	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: "provider-kb"}); err != nil {
		t.Fatal(err)
	}
	updated := getKB(t, storage, "default", "provider-kb")
	if cleaner.calls != 1 || cleaner.policy != v1alpha1.ReclaimDelete || cleaner.kb.Status.BackendBinding.BackendOperationID == "" {
		t.Fatalf("cleanup call = count %d policy %q kb %+v", cleaner.calls, cleaner.policy, cleaner.kb)
	}
	if recorder.calls != 0 {
		t.Fatalf("Delete invoked retained binding recorder %d times", recorder.calls)
	}
	if contains(updated.ObjectMeta.Finalizers, KnowledgeBaseFinalizer) || !contains(updated.ObjectMeta.Finalizers, "example.dev/cleanup") {
		t.Fatalf("finalizers after confirmed cleanup = %v", updated.ObjectMeta.Finalizers)
	}
}

func TestKnowledgeBaseReconciler_ArchiveDoesNotRecordRetainedBinding(t *testing.T) {
	storage := newMemStore()
	class := makeKBClass("provider-class", "azuresearch", "dense", v1alpha1.ReclaimArchive)
	class.Spec.BackendRef = &v1alpha1.KnowledgeBackendReference{APIGroup: "search.knowledge.kognition.dev", Kind: "AzureSearchBackend", Name: "shared"}
	storeKBClass(t, storage, class)
	kb := makeKB("default", "provider-kb", "provider-class", inlineSource("data"))
	now := time.Now().UTC()
	kb.ObjectMeta.DeletionTimestamp = &now
	kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer}
	kb.Status.Generation = 3
	kb.Status.BackendBinding = providerBindingFor(kb, class)
	kb.Status.BackendBinding.Generation = kb.Status.Generation
	storeKB(t, storage, kb)
	recorder := &recordingRetainedBindingRecorder{}
	reconciler := NewKnowledgeBaseReconciler(storage, zaptest.NewLogger(t)).
		WithBackendCleaner(&recordingKnowledgeCleaner{}).
		WithRetainedBindingRecorder(recorder)

	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: "provider-kb"}); err != nil {
		t.Fatal(err)
	}
	if recorder.calls != 0 {
		t.Fatalf("Archive invoked retained binding recorder %d times", recorder.calls)
	}
	if contains(getKB(t, storage, "default", "provider-kb").ObjectMeta.Finalizers, KnowledgeBaseFinalizer) {
		t.Fatal("Archive kept the finalizer after confirmed cleanup")
	}
}

func TestKnowledgeBaseReconciler_ProviderDeletionRetainsFinalizerWhenCleanupFails(t *testing.T) {
	storage := newMemStore()
	class := makeKBClass("provider-class", "azuresearch", "dense", v1alpha1.ReclaimRetain)
	class.Spec.BackendRef = &v1alpha1.KnowledgeBackendReference{APIGroup: "search.knowledge.kognition.dev", Kind: "AzureSearchBackend", Name: "shared"}
	storeKBClass(t, storage, class)
	kb := makeKB("default", "provider-kb", "provider-class", inlineSource("data"))
	now := time.Now().UTC()
	kb.ObjectMeta.DeletionTimestamp = &now
	kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer}
	kb.Status.Generation = 3
	kb.Status.BackendBinding = providerBindingFor(kb, class)
	kb.Status.BackendBinding.Generation = kb.Status.Generation
	storeKB(t, storage, kb)
	events := []string{}
	cleaner := &recordingKnowledgeCleaner{err: errors.New("provider unavailable"), events: &events}
	recorder := &recordingRetainedBindingRecorder{events: &events}
	reconciler := NewKnowledgeBaseReconciler(storage, zaptest.NewLogger(t)).
		WithBackendCleaner(cleaner).
		WithRetainedBindingRecorder(recorder)

	result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: "provider-kb"})
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("cleanup failure must remain retryable: %+v, %v", result, err)
	}
	updated := getKB(t, storage, "default", "provider-kb")
	condition := findConditionByType(updated.Status.Conditions, string(lifecycle.ConditionReady))
	if cleaner.calls != 1 || cleaner.policy != v1alpha1.ReclaimRetain || !contains(updated.ObjectMeta.Finalizers, KnowledgeBaseFinalizer) || condition == nil || condition.Reason != "ProviderCleanupFailed" {
		t.Fatalf("failed cleanup lost ownership or recovery state: cleaner %+v, kb %+v", cleaner, updated)
	}
	if recorder.calls != 0 || len(events) != 1 || events[0] != "cleanup" {
		t.Fatalf("failed cleanup must stop before recording: recorder calls %d events %v", recorder.calls, events)
	}
}

func TestKnowledgeBaseReconciler_RetainKeepsFinalizerWhenBindingRecordFails(t *testing.T) {
	storage := newMemStore()
	class := makeKBClass("provider-class", "azuresearch", "dense", v1alpha1.ReclaimRetain)
	class.Spec.BackendRef = &v1alpha1.KnowledgeBackendReference{APIGroup: "search.knowledge.kognition.dev", Kind: "AzureSearchBackend", Name: "shared"}
	storeKBClass(t, storage, class)
	kb := makeKB("default", "provider-kb", "provider-class", inlineSource("data"))
	now := time.Now().UTC()
	kb.ObjectMeta.DeletionTimestamp = &now
	kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer}
	kb.Status.Generation = 3
	kb.Status.BackendBinding = providerBindingFor(kb, class)
	kb.Status.BackendBinding.Generation = kb.Status.Generation
	storeKB(t, storage, kb)
	recorder := &recordingRetainedBindingRecorder{err: errors.New("record unavailable")}
	reconciler := NewKnowledgeBaseReconciler(storage, zaptest.NewLogger(t)).
		WithBackendCleaner(&recordingKnowledgeCleaner{}).
		WithRetainedBindingRecorder(recorder)

	result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: "provider-kb"})
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("record failure must remain retryable: %+v, %v", result, err)
	}
	updated := getKB(t, storage, "default", "provider-kb")
	condition := findConditionByType(updated.Status.Conditions, string(lifecycle.ConditionReady))
	if recorder.calls != 1 || !contains(updated.ObjectMeta.Finalizers, KnowledgeBaseFinalizer) || condition == nil || condition.Reason != "RetainedBindingRecordFailed" {
		t.Fatalf("failed retained record lost ownership: recorder %+v, kb %+v", recorder, updated)
	}
}

func TestKnowledgeBaseReconciler_ProviderDeletionUsesDurableReclaimPolicy(t *testing.T) {
	for _, test := range []struct {
		name        string
		storeClass  bool
		classPolicy v1alpha1.ReclaimPolicy
	}{
		{name: "class deleted", storeClass: false, classPolicy: v1alpha1.ReclaimRetain},
		{name: "live class changed", storeClass: true, classPolicy: v1alpha1.ReclaimDelete},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage := newMemStore()
			class := makeKBClass("provider-class", "azuresearch", "dense", test.classPolicy)
			class.Spec.BackendRef = &v1alpha1.KnowledgeBackendReference{APIGroup: "search.knowledge.kognition.dev", Kind: "AzureSearchBackend", Name: "shared"}
			if test.storeClass {
				storeKBClass(t, storage, class)
			}
			kb := makeKB("default", "provider-kb", class.ObjectMeta.Name, inlineSource("data"))
			now := time.Now().UTC()
			kb.ObjectMeta.DeletionTimestamp = &now
			kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer}
			kb.Status.Generation = 3
			kb.Status.BackendBinding = providerBindingFor(kb, class)
			kb.Status.BackendBinding.Generation = kb.Status.Generation
			kb.Status.BackendBinding.ReclaimPolicy = v1alpha1.ReclaimRetain
			storeKB(t, storage, kb)
			events := []string{}
			cleaner := &recordingKnowledgeCleaner{events: &events}
			recorder := &recordingRetainedBindingRecorder{events: &events}
			reconciler := NewKnowledgeBaseReconciler(storage, zaptest.NewLogger(t)).
				WithBackendCleaner(cleaner).
				WithRetainedBindingRecorder(recorder)

			if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: kb.ObjectMeta.Name}); err != nil {
				t.Fatal(err)
			}
			if cleaner.calls != 1 || cleaner.policy != v1alpha1.ReclaimRetain {
				t.Fatalf("cleanup call = count %d policy %q, want durable Retain", cleaner.calls, cleaner.policy)
			}
			if recorder.calls != 1 || len(events) != 2 || events[0] != "cleanup" || events[1] != "record" {
				t.Fatalf("Retain lifecycle = recorder calls %d events %v, want cleanup then durable record", recorder.calls, events)
			}
			if contains(getKB(t, storage, "default", kb.ObjectMeta.Name).ObjectMeta.Finalizers, KnowledgeBaseFinalizer) {
				t.Fatal("Retain kept finalizer after cleanup and durable record succeeded")
			}
		})
	}
}

func TestKnowledgeBaseReconciler_DeletionContinuesWhenClassIsMissing(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	now := time.Now().UTC()
	kb := makeKB("default", "kb-missing-class", "deleted-class", inlineSource("data"))
	kb.ObjectMeta.DeletionTimestamp = &now
	kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer, "example.dev/external-cleanup"}
	storeKB(t, s, kb)

	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: kb.ObjectMeta.Name, Kind: knowledgeBaseKind}); err != nil {
		t.Fatalf("reconcile deletion with missing class: %v", err)
	}

	updated := getKB(t, s, "default", kb.ObjectMeta.Name)
	if contains(updated.ObjectMeta.Finalizers, KnowledgeBaseFinalizer) {
		t.Fatalf("Kognition finalizer remained after class deletion: %v", updated.ObjectMeta.Finalizers)
	}
	if !contains(updated.ObjectMeta.Finalizers, "example.dev/external-cleanup") {
		t.Fatalf("external finalizer was removed: %v", updated.ObjectMeta.Finalizers)
	}
}

func TestKnowledgeBaseReconciler_AgentConflictPreservesConcurrentState(t *testing.T) {
	baseStore := newMemStore()
	s := &knowledgeBaseAgentConflictStore{memStore: baseStore}
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	storeKBClass(t, baseStore, makeKBClass("del-class", "pgvector", "dense", v1alpha1.ReclaimDelete))
	storeKB(t, baseStore, makeKB("default", "kb-del", "del-class", inlineSource("data")))
	storeKBClaim(t, baseStore, makeTestKBClaim("default", "claim-required", "kb-del"))
	storeAgent(t, baseStore, makeTestAgent("default", "agent-required", "claim-required", true))
	reconcileKB(t, r, "default", "kb-del")

	kb := getKB(t, baseStore, "default", "kb-del")
	now := time.Now()
	kb.ObjectMeta.DeletionTimestamp = &now
	data, _ := json.Marshal(kb)
	if err := baseStore.Update(context.Background(), "KnowledgeBase", "default", "kb-del", data, kb.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("set DeletionTimestamp: %v", err)
	}
	s.injectConflict = true

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "kb-del", Kind: "KnowledgeBase"}); err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}
	stored := getAgent(t, baseStore, "default", "agent-required")
	if !s.conflicted {
		t.Fatal("expected Agent update conflict")
	}
	if stored.ObjectMeta.Labels["concurrent"] != "preserved" || stored.Spec.Replicas == nil || *stored.Spec.Replicas != 5 {
		t.Fatalf("concurrent metadata or spec lost: metadata=%#v spec=%#v", stored.ObjectMeta, stored.Spec)
	}
	if stored.Status.Phase != v1alpha1.PhaseRunning || stored.Status.Replicas != 5 || stored.Status.ReadyReplicas != 4 {
		t.Fatalf("concurrent status lost: %#v", stored.Status)
	}
	kbReady := findAgentCondition(stored, string(conditionKBReady))
	concurrentReady := findAgentCondition(stored, "ConcurrentReady")
	if kbReady == nil || kbReady.Status != "False" || concurrentReady == nil || concurrentReady.Status != "True" {
		t.Fatalf("merged conditions = %#v, want KnowledgeBaseReady=False and ConcurrentReady=True", stored.Status.Conditions)
	}
}

// contains is a helper checking if a string is in a slice.
func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// TC-0162: KBClass with provisioner=pgvector, indexMode=hybrid → KB Ready
// ---------------------------------------------------------------------------

func TestKnowledgeBaseReconciler_TC0162_PgvectorHybrid(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	storeKBClass(t, s, makeKBClass("pgvector-hybrid", "pgvector", "hybrid", v1alpha1.ReclaimDelete))
	storeKB(t, s, makeKB("default", "kb-hybrid", "pgvector-hybrid",
		inlineSource("hybrid indexed doc"),
	))

	reconcileKB(t, r, "default", "kb-hybrid")

	kb := getKB(t, s, "default", "kb-hybrid")
	if kb.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", kb.Status.Phase)
	}
	if kb.Status.DocumentCount == 0 {
		t.Error("DocumentCount should be > 0")
	}
	if kb.Status.IndexedChunks == 0 {
		t.Error("IndexedChunks should be > 0")
	}
}

// ---------------------------------------------------------------------------
// TC-0181: All source types (inline, url, file, directory, git, configMap)
//          drive KB through Indexing → Ready
// ---------------------------------------------------------------------------

func TestKnowledgeBaseReconciler_TC0181_AllSourceTypes(t *testing.T) {
	sourceTests := []struct {
		name   string
		source v1alpha1.KBSource
	}{
		{"inline", inlineSource("some text")},
		{"url", urlSource("https://example.com/docs")},
		{"file", fileSource("/data/docs/manual.pdf")},
		{"directory", dirSource("/data/docs/")},
		{"git", gitSource("https://github.com/example/repo", "main")},
		{"configMap", configMapSource("my-configmap")},
	}

	for _, tc := range sourceTests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s := newMemStore()
			r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

			className := "class-" + tc.name
			kbName := "kb-" + tc.name

			storeKBClass(t, s, makeKBClass(className, "pgvector", "dense", v1alpha1.ReclaimDelete))
			storeKB(t, s, makeKB("default", kbName, className, tc.source))

			reconcileKB(t, r, "default", kbName)

			kb := getKB(t, s, "default", kbName)
			if kb.Status.Phase != v1alpha1.PhaseReady {
				t.Fatalf("source type %q: phase = %q, want Ready", tc.name, kb.Status.Phase)
			}
			if kb.Status.DocumentCount == 0 {
				t.Errorf("source type %q: DocumentCount should be > 0", tc.name)
			}
			if kb.Status.IndexedChunks == 0 {
				t.Errorf("source type %q: IndexedChunks should be > 0", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TC-extra: KB not found → no error (already deleted)
// ---------------------------------------------------------------------------

func TestKnowledgeBaseReconciler_NotFound(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	ctx := context.Background()
	req := Request{Namespace: "default", Name: "nonexistent", Kind: "KnowledgeBase"}
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Requeue {
		t.Error("should not requeue for not-found resource")
	}
}

func TestKnowledgeBaseReconciler_UnknownPhaseResetsToPending(t *testing.T) {
	s := newMemStore()
	kb := makeKB("default", "kb-unknown", "class")
	kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer}
	kb.Status.Phase = v1alpha1.Phase("Corrupted")
	kb.Status.Generation = 4
	kb.Status.DocumentCount = 12
	storeKB(t, s, kb)
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: kb.ObjectMeta.Name, Kind: knowledgeBaseKind})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !result.Requeue {
		t.Fatal("Reconcile() did not requeue after resetting unknown phase")
	}
	stored := getKB(t, s, "default", kb.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("phase = %q, want Pending", stored.Status.Phase)
	}
	if stored.Status.Generation != 4 || stored.Status.DocumentCount != 12 {
		t.Fatalf("status data changed during phase recovery: %+v", stored.Status)
	}
}

// ---------------------------------------------------------------------------
// TC-extra: KB with missing KBClass → condition reflects class-not-found
// ---------------------------------------------------------------------------

func TestKnowledgeBaseReconciler_MissingClass(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	// No KBClass stored — KB references a non-existent class.
	storeKB(t, s, makeKB("default", "kb-noclass", "missing-class", inlineSource("data")))

	// Should not error; KB should stay in Indexing with a Not-Ready condition.
	// The reconciler transitions to Indexing first (requeue), then on completeIndexing
	// the class is NOT checked (indexing is simulated). The class is checked only in verifyReady.
	// Let's just ensure no panic or error propagation.
	reconcileKB(t, r, "default", "kb-noclass")

	// The KB will be Ready (indexing is class-independent in the simulation).
	// Missing class is caught in verifyReady on the next reconcile cycle.
	// Running one extra reconcile to trigger verifyReady.
	ctx := context.Background()
	req := Request{Namespace: "default", Name: "kb-noclass", Kind: "KnowledgeBase"}
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error on verifyReady with missing class: %v", err)
	}

	kb := getKB(t, s, "default", "kb-noclass")
	if kb.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed when KBClass is missing during verifyReady", kb.Status.Phase)
	}

	cond := findKBCondition(kb, "Ready")
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Status != "False" {
		t.Errorf("Ready condition = %q, want False when class missing", cond.Status)
	}
}

// ---------------------------------------------------------------------------
// TC-extra: List all KBs in a namespace
// ---------------------------------------------------------------------------

func TestKnowledgeBase_ListInNamespace(t *testing.T) {
	s := newMemStore()

	storeKBClass(t, s, makeKBClass("list-class", "memory", "", v1alpha1.ReclaimDelete))
	storeKB(t, s, makeKB("ns-a", "kb-1", "list-class"))
	storeKB(t, s, makeKB("ns-a", "kb-2", "list-class"))
	storeKB(t, s, makeKB("ns-b", "kb-3", "list-class"))

	items, err := s.List(context.Background(), "KnowledgeBase", store.ListOptions{Namespace: "ns-a"})
	if err != nil {
		t.Fatalf("list KBs: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 KBs in ns-a, got %d", len(items))
	}

	allItems, err := s.List(context.Background(), "KnowledgeBase", store.ListOptions{})
	if err != nil {
		t.Fatalf("list all KBs: %v", err)
	}
	if len(allItems) != 3 {
		t.Fatalf("expected 3 KBs total, got %d", len(allItems))
	}
}

// ---------------------------------------------------------------------------
// TC-extra: Ready KB with KBClass still present → verifyReady returns cleanly
// (positive path for the PhaseReady case in the phase switch)
// ---------------------------------------------------------------------------

func TestKnowledgeBaseReconciler_VerifyReadyPositive(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	storeKBClass(t, s, makeKBClass("stable-class", "pgvector", "dense", v1alpha1.ReclaimDelete))
	storeKB(t, s, makeKB("default", "kb-stable", "stable-class", inlineSource("data")))

	// Bring KB to Ready.
	reconcileKB(t, r, "default", "kb-stable")
	kb := getKB(t, s, "default", "kb-stable")
	if kb.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected Ready after indexing, got %s", kb.Status.Phase)
	}

	// Reconcile again: phase=Ready → verifyReady → class exists → no error, no requeue.
	ctx := context.Background()
	req := Request{Namespace: "default", Name: "kb-stable", Kind: "KnowledgeBase"}
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("verifyReady with present class returned error: %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("verifyReady with present class should not requeue, got Requeue=%v RequeueAfter=%v",
			res.Requeue, res.RequeueAfter)
	}

	// Phase must remain Ready (verifyReady must not mutate status).
	kbAfter := getKB(t, s, "default", "kb-stable")
	if kbAfter.Status.Phase != v1alpha1.PhaseReady {
		t.Errorf("phase after verifyReady = %q, want Ready", kbAfter.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// TC-extra: KB in PhaseFailed → reconciler requeues after 30s (recovery loop)
// ---------------------------------------------------------------------------

func TestKnowledgeBaseReconciler_FailedPhaseRequeues(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	// Store a KB already in PhaseFailed (e.g. arrived there via MissingClass path).
	kb := makeKB("default", "kb-failed", "gone-class", inlineSource("data"))
	kb.ObjectMeta.Finalizers = []string{KnowledgeBaseFinalizer}
	kb.Status.Phase = v1alpha1.PhaseFailed
	storeKB(t, s, kb)

	ctx := context.Background()
	req := Request{Namespace: "default", Name: "kb-failed", Kind: "KnowledgeBase"}
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("PhaseFailed reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Errorf("expected RequeueAfter=30s for PhaseFailed, got %v", res.RequeueAfter)
	}

	// Phase must remain Failed (no accidental transition).
	kbAfter := getKB(t, s, "default", "kb-failed")
	if kbAfter.Status.Phase != v1alpha1.PhaseFailed {
		t.Errorf("phase = %q after Failed-state reconcile, want Failed", kbAfter.Status.Phase)
	}
}

// ---------------------------------------------------------------------------
// TC-extra: Progressing condition is True during indexing, False after Ready
// ---------------------------------------------------------------------------

func TestKnowledgeBaseReconciler_ProgressingConditionLifecycle(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseReconciler(s, zaptest.NewLogger(t))

	storeKBClass(t, s, makeKBClass("prog-class", "pgvector", "dense", v1alpha1.ReclaimDelete))
	storeKB(t, s, makeKB("default", "kb-prog", "prog-class", inlineSource("chunk")))

	ctx := context.Background()
	req := Request{Namespace: "default", Name: "kb-prog", Kind: "KnowledgeBase"}

	// Loop 1: adds finalizer → requeue.
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("loop1: %v", err)
	}
	if !res.Requeue {
		t.Fatal("loop1: expected Requeue after adding finalizer")
	}

	// Loop 2: startIndexing → PhaseIndexing, Progressing=True, Ready=False.
	res, err = r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("loop2: %v", err)
	}
	if !res.Requeue {
		t.Fatal("loop2: expected Requeue after startIndexing")
	}
	kb := getKB(t, s, "default", "kb-prog")
	if kb.Status.Phase != v1alpha1.PhaseIndexing {
		t.Fatalf("loop2: expected PhaseIndexing, got %s", kb.Status.Phase)
	}
	progCond := findKBCondition(kb, "Progressing")
	if progCond == nil {
		t.Fatal("loop2: Progressing condition not set")
	}
	if progCond.Status != "True" {
		t.Errorf("loop2: Progressing = %q, want True", progCond.Status)
	}
	readyCond := findKBCondition(kb, "Ready")
	if readyCond == nil {
		t.Fatal("loop2: Ready condition not set")
	}
	if readyCond.Status != "False" {
		t.Errorf("loop2: Ready = %q during indexing, want False", readyCond.Status)
	}

	// Loop 3: completeIndexing → PhaseReady, Progressing=False, Ready=True.
	res, err = r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("loop3: %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("loop3: unexpected requeue after completeIndexing")
	}
	kb = getKB(t, s, "default", "kb-prog")
	if kb.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("loop3: expected PhaseReady, got %s", kb.Status.Phase)
	}
	progCond = findKBCondition(kb, "Progressing")
	if progCond == nil {
		t.Fatal("loop3: Progressing condition not set")
	}
	if progCond.Status != "False" {
		t.Errorf("loop3: Progressing = %q after Ready, want False", progCond.Status)
	}
	readyCond = findKBCondition(kb, "Ready")
	if readyCond == nil {
		t.Fatal("loop3: Ready condition not set")
	}
	if readyCond.Status != "True" {
		t.Errorf("loop3: Ready = %q after indexing, want True", readyCond.Status)
	}
}
