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

type knowledgeBaseClaimConflictStore struct {
	*memStore
	conflicted bool
}

type knowledgeBaseClaimRetargetConflictStore struct {
	*memStore
	conflicted bool
	newTarget  string
}

type knowledgeBaseClaimDependencyFaultStore struct {
	*memStore
	err error
}

type knowledgeBaseClaimPersistentConflictStore struct {
	*memStore
	afterConflictErr error
	afterConflictRaw []byte
	claimGets        int
	updates          int
}

func (s *knowledgeBaseClaimDependencyFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == knowledgeBaseKind {
		return nil, s.err
	}
	return s.memStore.Get(ctx, kind, namespace, name)
}

func (s *knowledgeBaseClaimPersistentConflictStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == knowledgeBaseClaimKind {
		s.claimGets++
		if s.claimGets > 1 {
			if s.afterConflictErr != nil {
				return nil, s.afterConflictErr
			}
			if s.afterConflictRaw != nil {
				return s.afterConflictRaw, nil
			}
		}
	}
	return s.memStore.Get(ctx, kind, namespace, name)
}

func (s *knowledgeBaseClaimPersistentConflictStore) Update(context.Context, string, string, string, []byte, int64) error {
	s.updates++
	return fmt.Errorf("injected persistent conflict: %w", store.ErrConflict)
}

func (s *knowledgeBaseClaimRetargetConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == knowledgeBaseClaimKind && !s.conflicted {
		s.conflicted = true
		raw, err := s.memStore.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var latest v1alpha1.KnowledgeBaseClaim
		if err := json.Unmarshal(raw, &latest); err != nil {
			return err
		}
		latest.Spec.KnowledgeBaseName = s.newTarget
		latest.ObjectMeta.Generation++
		updated, err := json.Marshal(latest)
		if err != nil {
			return err
		}
		if err := s.memStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
			return err
		}
		return fmt.Errorf("injected retarget conflict: %w", store.ErrConflict)
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *knowledgeBaseClaimConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == knowledgeBaseClaimKind && !s.conflicted {
		s.conflicted = true
		raw, err := s.memStore.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var latest v1alpha1.KnowledgeBaseClaim
		if err := json.Unmarshal(raw, &latest); err != nil {
			return err
		}
		latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{
			Type: "Audited", Status: "True", Reason: "ConcurrentObserver",
		})
		updated, err := json.Marshal(latest)
		if err != nil {
			return err
		}
		if err := s.memStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
			return err
		}
		return fmt.Errorf("injected concurrent conflict: %w", store.ErrConflict)
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func makeKnowledgeBase(ns, name string, phase v1alpha1.Phase) v1alpha1.KnowledgeBase {
	return v1alpha1.KnowledgeBase{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "KnowledgeBase"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "kb-uid-" + name,
		},
		Spec: v1alpha1.KnowledgeBaseSpec{
			KnowledgeBaseClassName: "in-memory-bm25",
		},
		Status: v1alpha1.KnowledgeBaseStatus{
			Phase:      phase,
			Generation: 1,
		},
	}
}

func makeKnowledgeBaseClaim(ns, name, kbName string) v1alpha1.KnowledgeBaseClaim {
	return v1alpha1.KnowledgeBaseClaim{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "KnowledgeBaseClaim"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       name,
			Namespace:  ns,
			UID:        "kbclaim-uid-" + name,
			Generation: 1,
		},
		Spec: v1alpha1.KnowledgeBaseClaimSpec{
			KnowledgeBaseName: kbName,
		},
	}
}

func storeKnowledgeBase(t *testing.T, s *memStore, kb v1alpha1.KnowledgeBase) {
	t.Helper()
	data, _ := json.Marshal(kb)
	if err := s.Create(context.Background(), "KnowledgeBase", kb.ObjectMeta.Namespace, kb.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store knowledge base: %v", err)
	}
}

func getKBClaim(t *testing.T, s *memStore, ns, name string) v1alpha1.KnowledgeBaseClaim {
	t.Helper()
	raw, err := s.Get(context.Background(), "KnowledgeBaseClaim", ns, name)
	if err != nil {
		t.Fatalf("get knowledge base claim: %v", err)
	}
	var claim v1alpha1.KnowledgeBaseClaim
	if err := json.Unmarshal(raw, &claim); err != nil {
		t.Fatalf("unmarshal knowledge base claim: %v", err)
	}
	return claim
}

func reconcileKBClaim(t *testing.T, r *KnowledgeBaseClaimReconciler, ns, name string) {
	t.Helper()
	ctx := context.Background()
	req := Request{Namespace: ns, Name: name, Kind: "KnowledgeBaseClaim"}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func findBoundCondition(conds []v1alpha1.Condition) *v1alpha1.Condition {
	for i := range conds {
		if conds[i].Type == string(conditionBound) {
			return &conds[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// TC-0170: KnowledgeBase missing → Pending, condition Bound=False
// ---------------------------------------------------------------------------

func TestKnowledgeBaseClaimReconciler_TC0170_Pending_KBMissing(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseClaimReconciler(s, zaptest.NewLogger(t))

	storeKBClaim(t, s, makeKnowledgeBaseClaim("default", "claim-a", "kb-missing"))

	reconcileKBClaim(t, r, "default", "claim-a")

	claim := getKBClaim(t, s, "default", "claim-a")
	if claim.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("phase = %q, want Pending", claim.Status.Phase)
	}
	if claim.Status.BoundKnowledgeBase != "" {
		t.Fatalf("BoundKnowledgeBase = %q, want empty", claim.Status.BoundKnowledgeBase)
	}
	cond := findBoundCondition(claim.Status.Conditions)
	if cond == nil {
		t.Fatal("expected Bound condition")
	}
	if cond.Status != string(lifecycle.ConditionFalse) {
		t.Fatalf("Bound.Status = %q, want %q", cond.Status, lifecycle.ConditionFalse)
	}
	if cond.Reason != "KnowledgeBaseNotFound" {
		t.Fatalf("Bound.Reason = %q, want KnowledgeBaseNotFound", cond.Reason)
	}
	if claim.Status.ObservedGeneration != 1 {
		t.Fatalf("ObservedGeneration = %d, want 1", claim.Status.ObservedGeneration)
	}
}

func TestKnowledgeBaseClaimReconciler_PendingReconcileRequeuesWithoutWriting(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseClaimReconciler(s, zaptest.NewLogger(t))
	storeKBClaim(t, s, makeKnowledgeBaseClaim("default", "claim-pending", "kb-missing"))

	reconcileKBClaim(t, r, "default", "claim-pending")
	before := getKBClaim(t, s, "default", "claim-pending")

	result, err := r.Reconcile(context.Background(), Request{
		Namespace: "default", Name: "claim-pending", Kind: knowledgeBaseClaimKind,
	})
	if err != nil {
		t.Fatalf("reconcile stable pending claim: %v", err)
	}
	if result.RequeueAfter != kbClaimRequeueUnbound {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, kbClaimRequeueUnbound)
	}
	after := getKBClaim(t, s, "default", "claim-pending")
	if after.ObjectMeta.ResourceVersion != before.ObjectMeta.ResourceVersion {
		t.Fatalf("resource version changed from %d to %d on a no-op reconcile", before.ObjectMeta.ResourceVersion, after.ObjectMeta.ResourceVersion)
	}
}

func TestKnowledgeBaseClaimReconciler_BindsWhenKnowledgeBaseReady(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseClaimReconciler(s, zaptest.NewLogger(t))

	storeKnowledgeBase(t, s, makeKnowledgeBase("default", "kb-docs", v1alpha1.PhaseReady))
	storeKBClaim(t, s, makeKnowledgeBaseClaim("default", "claim-b", "kb-docs"))

	reconcileKBClaim(t, r, "default", "claim-b")

	claim := getKBClaim(t, s, "default", "claim-b")
	if claim.Status.Phase != v1alpha1.PhaseBound {
		t.Fatalf("phase = %q, want Bound", claim.Status.Phase)
	}
	if claim.Status.BoundKnowledgeBase != "kb-docs" {
		t.Fatalf("BoundKnowledgeBase = %q, want kb-docs", claim.Status.BoundKnowledgeBase)
	}
	cond := findBoundCondition(claim.Status.Conditions)
	if cond == nil {
		t.Fatal("expected Bound condition")
	}
	if cond.Status != string(lifecycle.ConditionTrue) {
		t.Fatalf("Bound.Status = %q, want %q", cond.Status, lifecycle.ConditionTrue)
	}
	if cond.Reason != "KnowledgeBaseReady" {
		t.Fatalf("Bound.Reason = %q, want KnowledgeBaseReady", cond.Reason)
	}
	if claim.Status.ObservedGeneration != 1 {
		t.Fatalf("ObservedGeneration = %d, want 1", claim.Status.ObservedGeneration)
	}
}

func TestKnowledgeBaseClaimReconciler_BoundReconcileIsIdempotent(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseClaimReconciler(s, zaptest.NewLogger(t))

	storeKnowledgeBase(t, s, makeKnowledgeBase("default", "kb-stable", v1alpha1.PhaseReady))
	storeKBClaim(t, s, makeKnowledgeBaseClaim("default", "claim-stable", "kb-stable"))

	reconcileKBClaim(t, r, "default", "claim-stable")
	before := getKBClaim(t, s, "default", "claim-stable")

	reconcileKBClaim(t, r, "default", "claim-stable")
	after := getKBClaim(t, s, "default", "claim-stable")

	if after.ObjectMeta.ResourceVersion != before.ObjectMeta.ResourceVersion {
		t.Fatalf("resource version changed from %d to %d on a no-op reconcile", before.ObjectMeta.ResourceVersion, after.ObjectMeta.ResourceVersion)
	}
}

func TestKnowledgeBaseClaimReconciler_BoundConflictPreservesForeignCondition(t *testing.T) {
	baseStore := newMemStore()
	s := &knowledgeBaseClaimConflictStore{memStore: baseStore}
	r := NewKnowledgeBaseClaimReconciler(s, zaptest.NewLogger(t))

	storeKnowledgeBase(t, baseStore, makeKnowledgeBase("default", "kb-conflict", v1alpha1.PhaseReady))
	storeKBClaim(t, baseStore, makeKnowledgeBaseClaim("default", "claim-conflict", "kb-conflict"))

	reconcileKBClaim(t, r, "default", "claim-conflict")

	claim := getKBClaim(t, baseStore, "default", "claim-conflict")
	if claim.Status.Phase != v1alpha1.PhaseBound {
		t.Fatalf("phase = %q, want Bound", claim.Status.Phase)
	}
	if condition := findCondition(claim.Status.Conditions, "Audited"); condition == nil || condition.Reason != "ConcurrentObserver" {
		t.Fatalf("concurrent condition was lost: %#v", condition)
	}
	if condition := findBoundCondition(claim.Status.Conditions); condition == nil || condition.Status != string(lifecycle.ConditionTrue) {
		t.Fatalf("Bound condition = %#v, want True", condition)
	}
}

func TestKnowledgeBaseClaimReconciler_BoundConflictRejectsStaleRetargetedBinding(t *testing.T) {
	baseStore := newMemStore()
	s := &knowledgeBaseClaimRetargetConflictStore{memStore: baseStore, newTarget: "kb-new"}
	r := NewKnowledgeBaseClaimReconciler(s, zaptest.NewLogger(t))

	storeKnowledgeBase(t, baseStore, makeKnowledgeBase("default", "kb-old", v1alpha1.PhaseReady))
	storeKnowledgeBase(t, baseStore, makeKnowledgeBase("default", "kb-new", v1alpha1.PhaseReady))
	storeKBClaim(t, baseStore, makeKnowledgeBaseClaim("default", "claim-retargeted", "kb-old"))

	req := Request{Namespace: "default", Name: "claim-retargeted", Kind: knowledgeBaseClaimKind}
	if _, err := r.Reconcile(context.Background(), req); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("reconcile error = %v, want ErrConflict after concurrent retarget", err)
	}
	stale := getKBClaim(t, baseStore, "default", "claim-retargeted")
	if stale.Spec.KnowledgeBaseName != "kb-new" {
		t.Fatalf("target = %q, want kb-new", stale.Spec.KnowledgeBaseName)
	}
	if stale.Status.Phase == v1alpha1.PhaseBound || stale.Status.BoundKnowledgeBase != "" {
		t.Fatalf("stale binding was published after retarget: %#v", stale.Status)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile new target: %v", err)
	}
	bound := getKBClaim(t, baseStore, "default", "claim-retargeted")
	if bound.Status.Phase != v1alpha1.PhaseBound || bound.Status.BoundKnowledgeBase != "kb-new" {
		t.Fatalf("binding after retry = %#v, want Bound to kb-new", bound.Status)
	}
	if bound.Status.ObservedGeneration != bound.ObjectMeta.Generation {
		t.Fatalf("observed generation = %d, want %d", bound.Status.ObservedGeneration, bound.ObjectMeta.Generation)
	}
}

func TestKnowledgeBaseClaimReconciler_RemainsPendingWhenKnowledgeBaseNotReady(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseClaimReconciler(s, zaptest.NewLogger(t))

	storeKnowledgeBase(t, s, makeKnowledgeBase("default", "kb-slow", v1alpha1.PhaseIndexing))
	storeKBClaim(t, s, makeKnowledgeBaseClaim("default", "claim-c", "kb-slow"))

	reconcileKBClaim(t, r, "default", "claim-c")

	claim := getKBClaim(t, s, "default", "claim-c")
	if claim.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("phase = %q, want Pending", claim.Status.Phase)
	}
	if claim.Status.BoundKnowledgeBase != "" {
		t.Fatalf("BoundKnowledgeBase = %q, want empty", claim.Status.BoundKnowledgeBase)
	}
	cond := findBoundCondition(claim.Status.Conditions)
	if cond == nil {
		t.Fatal("expected Bound condition")
	}
	if cond.Reason != "KnowledgeBaseNotReady" {
		t.Fatalf("Bound.Reason = %q, want KnowledgeBaseNotReady", cond.Reason)
	}
}

// ---------------------------------------------------------------------------
// TC-0173: Bound → Lost when KnowledgeBase disappears
// ---------------------------------------------------------------------------

func TestKnowledgeBaseClaimReconciler_TC0173_Lost_KBDeleted(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseClaimReconciler(s, zaptest.NewLogger(t))

	// Seed a claim that was already Bound previously.
	claim := makeKnowledgeBaseClaim("default", "claim-d", "kb-gone")
	claim.Status.Phase = v1alpha1.PhaseBound
	claim.Status.BoundKnowledgeBase = "kb-gone"
	claim.Status.ObservedGeneration = 1
	storeKBClaim(t, s, claim)

	// KnowledgeBase is intentionally absent.
	reconcileKBClaim(t, r, "default", "claim-d")

	got := getKBClaim(t, s, "default", "claim-d")
	if got.Status.Phase != v1alpha1.PhaseLost {
		t.Fatalf("phase = %q, want Lost", got.Status.Phase)
	}
	if got.Status.BoundKnowledgeBase != "" {
		t.Fatalf("BoundKnowledgeBase = %q, want cleared", got.Status.BoundKnowledgeBase)
	}
	cond := findBoundCondition(got.Status.Conditions)
	if cond == nil || cond.Reason != "KnowledgeBaseNotFound" {
		t.Fatalf("Bound condition = %+v, want reason KnowledgeBaseNotFound", cond)
	}
}

func TestKnowledgeBaseClaimReconciler_LostClaimRebindsWhenKnowledgeBaseRecovers(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseClaimReconciler(s, zaptest.NewLogger(t))

	storeKnowledgeBase(t, s, makeKnowledgeBase("default", "kb-recovered", v1alpha1.PhaseReady))
	claim := makeKnowledgeBaseClaim("default", "claim-recovered", "kb-recovered")
	claim.Status.Phase = v1alpha1.PhaseLost
	claim.Status.ObservedGeneration = 1
	claim.Status.Conditions = []v1alpha1.Condition{{
		Type: string(conditionBound), Status: string(lifecycle.ConditionFalse), Reason: "KnowledgeBaseNotFound",
	}}
	storeKBClaim(t, s, claim)

	reconcileKBClaim(t, r, "default", "claim-recovered")

	got := getKBClaim(t, s, "default", "claim-recovered")
	if got.Status.Phase != v1alpha1.PhaseBound {
		t.Fatalf("phase = %q, want Bound", got.Status.Phase)
	}
	if got.Status.BoundKnowledgeBase != "kb-recovered" {
		t.Fatalf("BoundKnowledgeBase = %q, want kb-recovered", got.Status.BoundKnowledgeBase)
	}
	condition := findBoundCondition(got.Status.Conditions)
	if condition == nil || condition.Status != string(lifecycle.ConditionTrue) || condition.Reason != "KnowledgeBaseReady" {
		t.Fatalf("Bound condition = %+v, want True/KnowledgeBaseReady", condition)
	}
}

// ---------------------------------------------------------------------------
// TC-0174: ObservedGeneration tracks spec Generation
// ---------------------------------------------------------------------------

func TestKnowledgeBaseClaimReconciler_TC0174_ObservedGeneration(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseClaimReconciler(s, zaptest.NewLogger(t))

	storeKnowledgeBase(t, s, makeKnowledgeBase("default", "kb-e", v1alpha1.PhaseReady))
	claim := makeKnowledgeBaseClaim("default", "claim-e", "kb-e")
	claim.ObjectMeta.Generation = 3
	storeKBClaim(t, s, claim)

	reconcileKBClaim(t, r, "default", "claim-e")

	got := getKBClaim(t, s, "default", "claim-e")
	if got.Status.ObservedGeneration != 3 {
		t.Fatalf("ObservedGeneration = %d, want 3", got.Status.ObservedGeneration)
	}
}

// ---------------------------------------------------------------------------
// TC-0175: Deletion timestamp is a no-op (no finalizer)
// ---------------------------------------------------------------------------

func TestKnowledgeBaseClaimReconciler_TC0175_DeletionNoOp(t *testing.T) {
	s := newMemStore()
	r := NewKnowledgeBaseClaimReconciler(s, zaptest.NewLogger(t))

	storeKnowledgeBase(t, s, makeKnowledgeBase("default", "kb-f", v1alpha1.PhaseReady))
	claim := makeKnowledgeBaseClaim("default", "claim-f", "kb-f")
	now := time.Now().UTC()
	claim.ObjectMeta.DeletionTimestamp = &now
	storeKBClaim(t, s, claim)

	reconcileKBClaim(t, r, "default", "claim-f")

	got := getKBClaim(t, s, "default", "claim-f")
	// No status write should have happened.
	if got.Status.Phase != "" {
		t.Fatalf("phase = %q, want empty (deletion no-op)", got.Status.Phase)
	}
	if len(got.ObjectMeta.Finalizers) != 0 {
		t.Fatalf("finalizers = %v, want none", got.ObjectMeta.Finalizers)
	}
}

func TestKnowledgeBaseClaimReconciler_StoreAndDependencyBoundaries(t *testing.T) {
	t.Run("claim not found", func(t *testing.T) {
		r := NewKnowledgeBaseClaimReconciler(newMemStore(), zaptest.NewLogger(t))
		result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "missing"})
		if err != nil || result.Requeue || result.RequeueAfter != 0 {
			t.Fatalf("result = %+v, error = %v; want no-op", result, err)
		}
	})

	t.Run("claim read failure", func(t *testing.T) {
		wantErr := errors.New("claim store unavailable")
		r := NewKnowledgeBaseClaimReconciler(&faultStore{memStore: newMemStore(), getErr: wantErr}, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "claim"})
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "get knowledgebaseclaim") {
			t.Fatalf("error = %v, want wrapped claim read failure", err)
		}
	})

	t.Run("malformed claim", func(t *testing.T) {
		memory := newMemStore()
		if err := memory.Create(context.Background(), knowledgeBaseClaimKind, "default", "claim", []byte("{")); err != nil {
			t.Fatalf("store malformed claim: %v", err)
		}
		r := NewKnowledgeBaseClaimReconciler(memory, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "claim"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal knowledgebaseclaim") {
			t.Fatalf("error = %v, want malformed claim failure", err)
		}
	})

	t.Run("knowledge base read failure", func(t *testing.T) {
		memory := newMemStore()
		storeKBClaim(t, memory, makeKnowledgeBaseClaim("default", "claim", "docs"))
		wantErr := errors.New("knowledge base store unavailable")
		r := NewKnowledgeBaseClaimReconciler(&knowledgeBaseClaimDependencyFaultStore{memStore: memory, err: wantErr}, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "claim"})
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "get knowledge base") {
			t.Fatalf("error = %v, want wrapped KnowledgeBase read failure", err)
		}
	})

	t.Run("malformed knowledge base", func(t *testing.T) {
		memory := newMemStore()
		storeKBClaim(t, memory, makeKnowledgeBaseClaim("default", "claim", "docs"))
		if err := memory.Create(context.Background(), knowledgeBaseKind, "default", "docs", []byte("{")); err != nil {
			t.Fatalf("store malformed KnowledgeBase: %v", err)
		}
		r := NewKnowledgeBaseClaimReconciler(memory, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "claim"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal knowledge base") {
			t.Fatalf("error = %v, want malformed KnowledgeBase failure", err)
		}
	})

	for _, tc := range []struct {
		name        string
		ready       bool
		wantMessage string
	}{
		{name: "bound update failure", ready: true, wantMessage: "update claim to Bound"},
		{name: "pending update failure", ready: false, wantMessage: "update claim to Pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := newMemStore()
			storeKBClaim(t, memory, makeKnowledgeBaseClaim("default", "claim", "docs"))
			if tc.ready {
				storeKnowledgeBase(t, memory, makeKnowledgeBase("default", "docs", v1alpha1.PhaseReady))
			}
			wantErr := errors.New("update unavailable")
			r := NewKnowledgeBaseClaimReconciler(&faultStore{memStore: memory, updateErr: wantErr}, zaptest.NewLogger(t))
			_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "claim"})
			if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("error = %v, want wrapped update failure containing %q", err, tc.wantMessage)
			}
		})
	}
}

func TestKnowledgeBaseClaimReconciler_ConflictBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name             string
		afterConflictErr error
		afterConflictRaw []byte
		wantErr          error
		wantMessage      string
		wantUpdates      int
	}{
		{
			name:             "reread failure",
			afterConflictErr: errors.New("read unavailable"),
			wantMessage:      "re-read after conflict",
			wantUpdates:      1,
		},
		{
			name:             "malformed concurrent claim",
			afterConflictRaw: []byte("{"),
			wantMessage:      "unmarshal after conflict",
			wantUpdates:      1,
		},
		{
			name:        "retry exhaustion",
			wantErr:     store.ErrConflict,
			wantMessage: "update claim to Bound",
			wantUpdates: 5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := newMemStore()
			storeKnowledgeBase(t, memory, makeKnowledgeBase("default", "docs", v1alpha1.PhaseReady))
			storeKBClaim(t, memory, makeKnowledgeBaseClaim("default", "claim", "docs"))
			faults := &knowledgeBaseClaimPersistentConflictStore{
				memStore:         memory,
				afterConflictErr: tc.afterConflictErr,
				afterConflictRaw: tc.afterConflictRaw,
			}
			r := NewKnowledgeBaseClaimReconciler(faults, zaptest.NewLogger(t))

			_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "claim"})
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("error = %v, want message containing %q", err, tc.wantMessage)
			}
			if faults.updates != tc.wantUpdates {
				t.Fatalf("update attempts = %d, want %d", faults.updates, tc.wantUpdates)
			}
		})
	}
}
