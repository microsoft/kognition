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

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type concurrentTrustProfileAdjustmentStore struct {
	*memStore
	conflicted bool
	updates    int
}

type persistentTrustProfileConflictStore struct {
	*memStore
	updates int
}

type trustProfilePromotionCreateFaultStore struct {
	*memStore
}

func (s *trustProfilePromotionCreateFaultStore) Create(
	_ context.Context, kind, _, _ string, _ []byte,
) error {
	if kind == autonomyReviewKind {
		return fmt.Errorf("wrapped concurrent promotion review: %w", store.ErrAlreadyExists)
	}
	return nil
}

func (s *persistentTrustProfileConflictStore) Update(
	_ context.Context, kind, _, _ string, _ []byte, _ int64,
) error {
	if kind == trustProfileKind {
		s.updates++
		return fmt.Errorf("wrapped TrustProfile conflict: %w", store.ErrConflict)
	}
	return nil
}

func (s *concurrentTrustProfileAdjustmentStore) Update(
	ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64,
) error {
	if kind == trustProfileKind {
		s.updates++
		if !s.conflicted {
			s.conflicted = true
			raw, err := s.memStore.Get(ctx, kind, namespace, name)
			if err != nil {
				return err
			}
			var latest v1alpha1.TrustProfile
			if err := json.Unmarshal(raw, &latest); err != nil {
				return err
			}
			latest.Spec.MaxClass = ClassAutonomous
			latest.Status.CurrentScore += 10
			latest.Status.TotalEvaluations++
			latest.Status.CurrentClass = ClassAssisted
			latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.StatusCondition{
				Type:   "ConcurrentEvaluation",
				Status: "True",
			})
			updated, err := json.Marshal(latest)
			if err != nil {
				return err
			}
			if err := s.memStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
				return err
			}
			return store.ErrConflict
		}
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func TestCognitiveQuotaReconciler_TrackUsage(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewCognitiveQuotaReconciler(ms, logger)

	ctx := context.Background()
	ns := "default"

	// Create a quota.
	quota := v1alpha1.CognitiveQuota{}
	quota.TypeMeta.Kind = "CognitiveQuota"
	quota.ObjectMeta.Name = "test-quota"
	quota.ObjectMeta.Namespace = ns
	quota.Spec.Hard = map[string]int64{"agents": 5}
	data, _ := json.Marshal(quota)
	ms.Create(ctx, "CognitiveQuota", ns, "test-quota", data)

	// Create some agents.
	for _, name := range []string{"a1", "a2", "a3"} {
		agentData, _ := json.Marshal(map[string]interface{}{
			"kind":     "Agent",
			"metadata": map[string]string{"name": name, "namespace": ns},
		})
		ms.Create(ctx, "Agent", ns, name, agentData)
	}

	// Reconcile should update usage.
	result, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "test-quota", Kind: "CognitiveQuota"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("unexpected requeue")
	}

	// Verify usage was updated.
	raw, _ := ms.Get(ctx, "CognitiveQuota", ns, "test-quota")
	var updated v1alpha1.CognitiveQuota
	json.Unmarshal(raw, &updated)
	if updated.Status.Used["agents"] != 3 {
		t.Errorf("expected agents usage 3, got %d", updated.Status.Used["agents"])
	}
}

func TestCognitiveQuotaReconciler_CheckQuota(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewCognitiveQuotaReconciler(ms, logger)

	ctx := context.Background()
	ns := "default"

	// Create a quota with low limit.
	quota := v1alpha1.CognitiveQuota{}
	quota.TypeMeta.Kind = "CognitiveQuota"
	quota.ObjectMeta.Name = "test-quota"
	quota.ObjectMeta.Namespace = ns
	quota.Spec.Hard = map[string]int64{"agents": 2}
	quota.Status.Used = map[string]int64{"agents": 2}
	data, _ := json.Marshal(quota)
	ms.Create(ctx, "CognitiveQuota", ns, "test-quota", data)

	// Should be denied.
	err := r.CheckQuota(ctx, ns, "agents", 1)
	if err == nil {
		t.Fatal("expected quota exceeded error")
	}

	// Different resource should pass.
	err = r.CheckQuota(ctx, ns, "tools", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTrustProfileReconciler_TierPromotion(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewTrustProfileReconciler(ms, logger)

	ctx := context.Background()
	ns := "default"

	// Create a trust profile with score triggering "standard" tier.
	tp := v1alpha1.TrustProfile{}
	tp.TypeMeta.Kind = "TrustProfile"
	tp.ObjectMeta.Name = "agent-trust"
	tp.ObjectMeta.Namespace = ns
	tp.Spec.AgentRef.Name = "test-agent"
	tp.Spec.InitialScore = 55
	tp.Status.CurrentScore = 55
	tp.Status.CurrentTier = "restricted" // out of date tier
	data, _ := json.Marshal(tp)
	ms.Create(ctx, "TrustProfile", ns, "agent-trust", data)

	// Reconcile should update tier to "standard".
	_, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "agent-trust", Kind: "TrustProfile"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	raw, _ := ms.Get(ctx, "TrustProfile", ns, "agent-trust")
	var updated v1alpha1.TrustProfile
	json.Unmarshal(raw, &updated)
	if updated.Status.CurrentTier != TierStandard {
		t.Errorf("expected tier %q, got %q", TierStandard, updated.Status.CurrentTier)
	}
}

func TestTrustProfileReconciler_ReconcileStoreOutcomes(t *testing.T) {
	t.Run("missing profile is already reconciled", func(t *testing.T) {
		r := NewTrustProfileReconciler(&faultStore{
			memStore: newMemStore(),
			getErr:   fmt.Errorf("wrapped missing TrustProfile: %w", store.ErrNotFound),
		}, zaptest.NewLogger(t))
		result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "deleted"})
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if result.Requeue {
			t.Fatal("Reconcile() requested requeue for missing profile")
		}
	})

	t.Run("backend read failure is surfaced", func(t *testing.T) {
		getErr := errors.New("store unavailable")
		r := NewTrustProfileReconciler(&faultStore{memStore: newMemStore(), getErr: getErr}, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "agent-trust"})
		if !errors.Is(err, getErr) || !strings.Contains(err.Error(), "get TrustProfile:") {
			t.Fatalf("Reconcile() error = %v, want wrapped get TrustProfile error", err)
		}
	})

	t.Run("malformed profile surfaces persisted state error", func(t *testing.T) {
		ms := newMemStore()
		if err := ms.Create(context.Background(), trustProfileKind, "default", "malformed", []byte(`{`)); err != nil {
			t.Fatalf("create malformed TrustProfile: %v", err)
		}
		r := NewTrustProfileReconciler(ms, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "malformed"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal TrustProfile:") {
			t.Fatalf("Reconcile() error = %v, want unmarshal TrustProfile error", err)
		}
	})

	t.Run("update conflict requests requeue", func(t *testing.T) {
		ms := newMemStore()
		tp := v1alpha1.TrustProfile{
			ObjectMeta: v1alpha1.ObjectMeta{Namespace: "default", Name: "stale-tier"},
			Status:     v1alpha1.TrustProfileStatus{CurrentScore: 75, CurrentTier: TierStandard},
		}
		data, err := json.Marshal(tp)
		if err != nil {
			t.Fatalf("marshal TrustProfile: %v", err)
		}
		if err := ms.Create(context.Background(), trustProfileKind, "default", tp.ObjectMeta.Name, data); err != nil {
			t.Fatalf("create TrustProfile: %v", err)
		}
		conflictStore := &persistentTrustProfileConflictStore{memStore: ms}
		r := NewTrustProfileReconciler(conflictStore, zaptest.NewLogger(t))
		result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: tp.ObjectMeta.Name})
		if err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		if !result.Requeue {
			t.Fatal("Reconcile() did not request requeue after update conflict")
		}
		if conflictStore.updates != 1 {
			t.Fatalf("update attempts = %d, want 1", conflictStore.updates)
		}
	})

	t.Run("backend update failure is surfaced", func(t *testing.T) {
		ms := newMemStore()
		tp := v1alpha1.TrustProfile{
			ObjectMeta: v1alpha1.ObjectMeta{Namespace: "default", Name: "stale-tier"},
			Status:     v1alpha1.TrustProfileStatus{CurrentScore: 75, CurrentTier: TierStandard},
		}
		data, err := json.Marshal(tp)
		if err != nil {
			t.Fatalf("marshal TrustProfile: %v", err)
		}
		if err := ms.Create(context.Background(), trustProfileKind, "default", tp.ObjectMeta.Name, data); err != nil {
			t.Fatalf("create TrustProfile: %v", err)
		}
		updateErr := errors.New("write unavailable")
		r := NewTrustProfileReconciler(&faultStore{memStore: ms, updateErr: updateErr}, zaptest.NewLogger(t))
		result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: tp.ObjectMeta.Name})
		if !errors.Is(err, updateErr) || !strings.Contains(err.Error(), "update TrustProfile:") {
			t.Fatalf("Reconcile() error = %v, want wrapped update TrustProfile error", err)
		}
		if result.Requeue {
			t.Fatal("Reconcile() requested requeue for non-conflict update failure")
		}
	})
}

func TestTrustProfileReconciler_TierBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		score    float64
		wantTier string
	}{
		{name: "below restricted", score: 24.99, wantTier: TierUntrusted},
		{name: "at restricted", score: 25, wantTier: TierRestricted},
		{name: "below standard", score: 49.99, wantTier: TierRestricted},
		{name: "at standard", score: 50, wantTier: TierStandard},
		{name: "below trusted", score: 69.99, wantTier: TierStandard},
		{name: "at trusted", score: 70, wantTier: TierTrusted},
		{name: "below privileged", score: 89.99, wantTier: TierTrusted},
		{name: "at privileged", score: 90, wantTier: TierPrivileged},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ms := newMemStore()
			r := NewTrustProfileReconciler(ms, zaptest.NewLogger(t))
			tp := v1alpha1.TrustProfile{
				ObjectMeta: v1alpha1.ObjectMeta{Namespace: "default", Name: "agent-trust"},
				Status: v1alpha1.TrustProfileStatus{
					CurrentScore: test.score,
					CurrentTier:  "stale",
				},
			}
			data, err := json.Marshal(tp)
			if err != nil {
				t.Fatalf("marshal TrustProfile: %v", err)
			}
			if err := ms.Create(context.Background(), trustProfileKind, "default", tp.ObjectMeta.Name, data); err != nil {
				t.Fatalf("create TrustProfile: %v", err)
			}

			if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: tp.ObjectMeta.Name}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			raw, err := ms.Get(context.Background(), trustProfileKind, "default", tp.ObjectMeta.Name)
			if err != nil {
				t.Fatalf("get TrustProfile: %v", err)
			}
			var updated v1alpha1.TrustProfile
			if err := json.Unmarshal(raw, &updated); err != nil {
				t.Fatalf("unmarshal TrustProfile: %v", err)
			}
			if updated.Status.CurrentTier != test.wantTier {
				t.Fatalf("score %v tier = %q, want %q", test.score, updated.Status.CurrentTier, test.wantTier)
			}
		})
	}
}

func TestTrustProfileReconciler_ZeroScoreAfterEvaluationIsNotReinitialized(t *testing.T) {
	ms := newMemStore()
	r := NewTrustProfileReconciler(ms, zaptest.NewLogger(t))
	ctx := context.Background()
	tp := v1alpha1.TrustProfile{}
	tp.TypeMeta.Kind = trustProfileKind
	tp.ObjectMeta.Name = "agent-trust"
	tp.ObjectMeta.Namespace = "default"
	tp.Spec.InitialScore = 80
	tp.Status.CurrentScore = 80
	tp.Status.CurrentTier = TierTrusted
	data, err := json.Marshal(tp)
	if err != nil {
		t.Fatalf("marshal TrustProfile: %v", err)
	}
	if err := ms.Create(ctx, trustProfileKind, "default", tp.ObjectMeta.Name, data); err != nil {
		t.Fatalf("create TrustProfile: %v", err)
	}

	if err := r.AdjustScore(ctx, "default", tp.ObjectMeta.Name, -100); err != nil {
		t.Fatalf("AdjustScore() error = %v", err)
	}
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: tp.ObjectMeta.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	raw, err := ms.Get(ctx, trustProfileKind, "default", tp.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get TrustProfile: %v", err)
	}
	var updated v1alpha1.TrustProfile
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal TrustProfile: %v", err)
	}
	if updated.Status.CurrentScore != 0 {
		t.Fatalf("current score = %v, want 0 after penalty", updated.Status.CurrentScore)
	}
	if updated.Status.CurrentTier != TierUntrusted {
		t.Fatalf("current tier = %q, want %q", updated.Status.CurrentTier, TierUntrusted)
	}
	if updated.Status.TotalEvaluations != 1 {
		t.Fatalf("total evaluations = %d, want 1", updated.Status.TotalEvaluations)
	}

	if err := r.AdjustScore(ctx, "default", tp.ObjectMeta.Name, 75); err != nil {
		t.Fatalf("positive AdjustScore() error = %v", err)
	}
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: tp.ObjectMeta.Name}); err != nil {
		t.Fatalf("recovery Reconcile() error = %v", err)
	}
	raw, err = ms.Get(ctx, trustProfileKind, "default", tp.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get recovered TrustProfile: %v", err)
	}
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal recovered TrustProfile: %v", err)
	}
	if updated.Status.CurrentScore != 75 || updated.Status.CurrentTier != TierTrusted {
		t.Fatalf("recovered status = %+v, want score 75 and tier %q", updated.Status, TierTrusted)
	}
	if updated.Status.TotalEvaluations != 2 {
		t.Fatalf("recovered total evaluations = %d, want 2", updated.Status.TotalEvaluations)
	}
}

func TestTrustProfileReconciler_InitializesScoreBeforeFirstEvaluation(t *testing.T) {
	ms := newMemStore()
	r := NewTrustProfileReconciler(ms, zaptest.NewLogger(t))
	ctx := context.Background()
	tp := v1alpha1.TrustProfile{}
	tp.TypeMeta.Kind = trustProfileKind
	tp.ObjectMeta.Name = "new-agent-trust"
	tp.ObjectMeta.Namespace = "default"
	tp.Spec.InitialScore = 80
	data, err := json.Marshal(tp)
	if err != nil {
		t.Fatalf("marshal TrustProfile: %v", err)
	}
	if err := ms.Create(ctx, trustProfileKind, "default", tp.ObjectMeta.Name, data); err != nil {
		t.Fatalf("create TrustProfile: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: tp.ObjectMeta.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	raw, err := ms.Get(ctx, trustProfileKind, "default", tp.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get TrustProfile: %v", err)
	}
	var updated v1alpha1.TrustProfile
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal TrustProfile: %v", err)
	}
	if updated.Status.CurrentScore != 80 || updated.Status.CurrentTier != TierTrusted {
		t.Fatalf("initialized status = %+v, want score 80 and tier %q", updated.Status, TierTrusted)
	}
	if updated.Status.TotalEvaluations != 0 {
		t.Fatalf("total evaluations = %d, want 0", updated.Status.TotalEvaluations)
	}
}

func TestTrustProfileReconciler_PersistsInitialScoreWhenTierAlreadyMatches(t *testing.T) {
	ms := newMemStore()
	r := NewTrustProfileReconciler(ms, zaptest.NewLogger(t))
	ctx := context.Background()
	tp := v1alpha1.TrustProfile{}
	tp.TypeMeta.Kind = trustProfileKind
	tp.ObjectMeta.Name = "partially-initialized-trust"
	tp.ObjectMeta.Namespace = "default"
	tp.Spec.InitialScore = 80
	tp.Status.CurrentTier = TierTrusted
	data, err := json.Marshal(tp)
	if err != nil {
		t.Fatalf("marshal TrustProfile: %v", err)
	}
	if err := ms.Create(ctx, trustProfileKind, "default", tp.ObjectMeta.Name, data); err != nil {
		t.Fatalf("create TrustProfile: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: tp.ObjectMeta.Name}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	raw, err := ms.Get(ctx, trustProfileKind, "default", tp.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get TrustProfile: %v", err)
	}
	var updated v1alpha1.TrustProfile
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal TrustProfile: %v", err)
	}
	if updated.Status.CurrentScore != 80 {
		t.Fatalf("initialized score = %v, want 80", updated.Status.CurrentScore)
	}
}

func TestTrustProfileReconciler_FirstMutationUsesInitialScore(t *testing.T) {
	tests := []struct {
		name      string
		wantScore float64
		mutate    func(*TrustProfileReconciler, context.Context, string) error
	}{
		{
			name:      "AdjustScore",
			wantScore: 70,
			mutate: func(r *TrustProfileReconciler, ctx context.Context, name string) error {
				return r.AdjustScore(ctx, "default", name, -10)
			},
		},
		{
			name:      "ApplyScoring",
			wantScore: 81,
			mutate: func(r *TrustProfileReconciler, ctx context.Context, name string) error {
				return r.ApplyScoring(ctx, "default", name, []TrustScoringEvent{{Type: EventSuccess}})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := newMemStore()
			r := NewTrustProfileReconciler(ms, zaptest.NewLogger(t))
			ctx := context.Background()
			tp := v1alpha1.TrustProfile{}
			tp.TypeMeta.Kind = trustProfileKind
			tp.ObjectMeta.Name = "unevaluated-trust"
			tp.ObjectMeta.Namespace = "default"
			tp.Spec.InitialScore = 80
			data, err := json.Marshal(tp)
			if err != nil {
				t.Fatalf("marshal TrustProfile: %v", err)
			}
			if err := ms.Create(ctx, trustProfileKind, "default", tp.ObjectMeta.Name, data); err != nil {
				t.Fatalf("create TrustProfile: %v", err)
			}

			if err := tt.mutate(r, ctx, tp.ObjectMeta.Name); err != nil {
				t.Fatalf("mutation error = %v", err)
			}
			raw, err := ms.Get(ctx, trustProfileKind, "default", tp.ObjectMeta.Name)
			if err != nil {
				t.Fatalf("get TrustProfile: %v", err)
			}
			var updated v1alpha1.TrustProfile
			if err := json.Unmarshal(raw, &updated); err != nil {
				t.Fatalf("unmarshal TrustProfile: %v", err)
			}
			if updated.Status.CurrentScore != tt.wantScore || updated.Status.TotalEvaluations != 1 {
				t.Fatalf("score/evaluations = %v/%d, want %v/1", updated.Status.CurrentScore, updated.Status.TotalEvaluations, tt.wantScore)
			}
		})
	}
}

func TestTrustProfileReconciler_AdjustScore(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewTrustProfileReconciler(ms, logger)

	ctx := context.Background()
	ns := "default"

	tp := v1alpha1.TrustProfile{}
	tp.TypeMeta.Kind = "TrustProfile"
	tp.ObjectMeta.Name = "agent-trust"
	tp.ObjectMeta.Namespace = ns
	tp.Status.CurrentScore = 50
	data, _ := json.Marshal(tp)
	ms.Create(ctx, "TrustProfile", ns, "agent-trust", data)

	// Adjust upward.
	if err := r.AdjustScore(ctx, ns, "agent-trust", 25); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	raw, _ := ms.Get(ctx, "TrustProfile", ns, "agent-trust")
	var updated v1alpha1.TrustProfile
	json.Unmarshal(raw, &updated)
	if updated.Status.CurrentScore != 75 {
		t.Errorf("expected score 75, got %f", updated.Status.CurrentScore)
	}
	if updated.Status.TotalEvaluations != 1 {
		t.Errorf("expected 1 evaluation, got %d", updated.Status.TotalEvaluations)
	}
}

func TestTrustProfileReconciler_AdjustScoreRetriesConcurrentEvaluation(t *testing.T) {
	baseStore := newMemStore()
	s := &concurrentTrustProfileAdjustmentStore{memStore: baseStore}
	r := NewTrustProfileReconciler(s, zaptest.NewLogger(t))
	ctx := context.Background()
	tp := v1alpha1.TrustProfile{}
	tp.TypeMeta.Kind = trustProfileKind
	tp.ObjectMeta.Name = "agent-trust-conflict"
	tp.ObjectMeta.Namespace = "default"
	tp.Status.CurrentScore = 50
	data, _ := json.Marshal(tp)
	if err := baseStore.Create(ctx, trustProfileKind, "default", tp.ObjectMeta.Name, data); err != nil {
		t.Fatalf("create TrustProfile: %v", err)
	}

	if err := r.AdjustScore(ctx, "default", tp.ObjectMeta.Name, 25); err != nil {
		t.Fatalf("AdjustScore() error = %v", err)
	}
	if !s.conflicted {
		t.Fatal("expected conflict fixture to execute")
	}
	if s.updates != 2 {
		t.Fatalf("TrustProfile update attempts = %d, want 2", s.updates)
	}
	raw, err := baseStore.Get(ctx, trustProfileKind, "default", tp.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get TrustProfile: %v", err)
	}
	var updated v1alpha1.TrustProfile
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal TrustProfile: %v", err)
	}
	if updated.Status.CurrentScore != 85 || updated.Status.TotalEvaluations != 2 {
		t.Fatalf("score/evaluations = %v/%d, want 85/2", updated.Status.CurrentScore, updated.Status.TotalEvaluations)
	}
	if updated.Spec.MaxClass != ClassAutonomous || updated.Status.CurrentClass != ClassAssisted {
		t.Fatalf("concurrent state was lost: spec=%+v status=%+v", updated.Spec, updated.Status)
	}
	if len(updated.Status.Conditions) != 1 || updated.Status.Conditions[0].Type != "ConcurrentEvaluation" {
		t.Fatalf("conditions = %+v, want one concurrent condition", updated.Status.Conditions)
	}
}

func TestTrustProfileReconciler_ApplyScoringRetriesConcurrentEvaluation(t *testing.T) {
	baseStore := newMemStore()
	s := &concurrentTrustProfileAdjustmentStore{memStore: baseStore}
	r := NewTrustProfileReconciler(s, zaptest.NewLogger(t))
	ctx := context.Background()
	tp := v1alpha1.TrustProfile{}
	tp.TypeMeta.Kind = trustProfileKind
	tp.ObjectMeta.Name = "agent-scoring-conflict"
	tp.ObjectMeta.Namespace = "default"
	tp.Status.CurrentScore = 50
	tp.Status.CurrentClass = ClassAssisted
	data, _ := json.Marshal(tp)
	if err := baseStore.Create(ctx, trustProfileKind, "default", tp.ObjectMeta.Name, data); err != nil {
		t.Fatalf("create TrustProfile: %v", err)
	}

	if err := r.ApplyScoring(ctx, "default", tp.ObjectMeta.Name, []TrustScoringEvent{{Type: EventSuccess}}); err != nil {
		t.Fatalf("ApplyScoring() error = %v", err)
	}
	if !s.conflicted {
		t.Fatal("expected conflict fixture to execute")
	}
	if s.updates != 2 {
		t.Fatalf("TrustProfile update attempts = %d, want 2", s.updates)
	}
	raw, err := baseStore.Get(ctx, trustProfileKind, "default", tp.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get TrustProfile: %v", err)
	}
	var updated v1alpha1.TrustProfile
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal TrustProfile: %v", err)
	}
	if updated.Status.CurrentScore != 61 || updated.Status.TotalEvaluations != 2 {
		t.Fatalf("score/evaluations = %v/%d, want 61/2", updated.Status.CurrentScore, updated.Status.TotalEvaluations)
	}
	if len(updated.Status.ScoreHistory) != 1 || updated.Status.ScoreHistory[0].Score != 61 {
		t.Fatalf("score history = %+v, want one entry at 61", updated.Status.ScoreHistory)
	}
	if updated.Spec.MaxClass != ClassAutonomous {
		t.Fatalf("concurrent MaxClass = %q, want %q", updated.Spec.MaxClass, ClassAutonomous)
	}
	if len(updated.Status.Conditions) != 1 || updated.Status.Conditions[0].Type != "ConcurrentEvaluation" {
		t.Fatalf("conditions = %+v, want one concurrent condition", updated.Status.Conditions)
	}
}

func TestTrustProfileReconciler_ScoreMutationsStopAfterPersistentConflicts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*TrustProfileReconciler, context.Context) error
	}{
		{
			name: "AdjustScore",
			mutate: func(r *TrustProfileReconciler, ctx context.Context) error {
				return r.AdjustScore(ctx, "default", "persistent-conflict", 25)
			},
		},
		{
			name: "ApplyScoring",
			mutate: func(r *TrustProfileReconciler, ctx context.Context) error {
				return r.ApplyScoring(ctx, "default", "persistent-conflict", []TrustScoringEvent{{Type: EventSuccess}})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseStore := newMemStore()
			s := &persistentTrustProfileConflictStore{memStore: baseStore}
			r := NewTrustProfileReconciler(s, zaptest.NewLogger(t))
			ctx := context.Background()
			tp := v1alpha1.TrustProfile{}
			tp.TypeMeta.Kind = trustProfileKind
			tp.ObjectMeta.Name = "persistent-conflict"
			tp.ObjectMeta.Namespace = "default"
			tp.Status.CurrentScore = 50
			data, _ := json.Marshal(tp)
			if err := baseStore.Create(ctx, trustProfileKind, "default", tp.ObjectMeta.Name, data); err != nil {
				t.Fatalf("create TrustProfile: %v", err)
			}

			err := tt.mutate(r, ctx)
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("mutation error = %v, want conflict", err)
			}
			if s.updates != 5 {
				t.Fatalf("TrustProfile update attempts = %d, want 5", s.updates)
			}
		})
	}
}

func TestTrustProfileReconciler_WrappedPromotionReviewAlreadyExistsIsIgnored(t *testing.T) {
	r := NewTrustProfileReconciler(&trustProfilePromotionCreateFaultStore{memStore: newMemStore()}, zaptest.NewLogger(t))
	profile := &v1alpha1.TrustProfile{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-trust", Namespace: "default"},
		Status:     v1alpha1.TrustProfileStatus{CurrentClass: ClassAssisted},
	}
	if err := r.ensurePromotionReview(context.Background(), profile, "event-1", ClassAutonomous); err != nil {
		t.Fatalf("ensurePromotionReview() error = %v, want concurrent create success", err)
	}
}

func TestAutonomyReviewReconciler_CreateAndExpire(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAutonomyReviewReconciler(ms, logger)

	ctx := context.Background()
	ns := "default"

	// Create a review.
	err := r.CreateReview(ctx, ns, "review-1", "agent-1", "delete-file", "session-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify it was created with Pending status.
	raw, err := ms.Get(ctx, "AutonomyReview", ns, "review-1")
	if err != nil {
		t.Fatalf("review not found: %v", err)
	}
	var review v1alpha1.AutonomyReview
	json.Unmarshal(raw, &review)
	if review.Status.Decision != DecisionPending {
		t.Errorf("expected Pending, got %q", review.Status.Decision)
	}
	if review.Spec.AgentRef.Name != "agent-1" {
		t.Errorf("expected agent-1, got %q", review.Spec.AgentRef.Name)
	}
}

func TestRoundRobinScheduler(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	registry := NewSchedulerRegistry()
	s := NewRoundRobinScheduler(ms, logger, registry)

	ctx := context.Background()
	ns := "default"

	// Schedule should round-robin.
	results := make([]string, 3)
	for i := 0; i < 3; i++ {
		res, err := s.Schedule(ctx, ScheduleRequest{
			Namespace: ns,
			AgentName: "agent-a",
			Replicas:  runtimeReplicas("agent-a", "worker-a", "worker-b", "worker-c"),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.Assigned {
			t.Fatal("expected assignment")
		}
		results[i] = res.Worker
	}

	// All three replicas should have been assigned.
	unique := make(map[string]bool)
	for _, w := range results {
		unique[w] = true
	}
	if len(unique) != 3 {
		t.Errorf("expected 3 unique workers, got %d: %v", len(unique), results)
	}
}
