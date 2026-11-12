// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type conflictingCognitiveQuotaUsageStore struct {
	*memStore
	conflictName       string
	conflictsRemaining int
	onConflict         func(*v1alpha1.CognitiveQuota)
	updates            int
}

type cognitiveQuotaFaultStore struct {
	*memStore
	getErr      error
	listErrKind string
}

func (s *cognitiveQuotaFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.memStore.Get(ctx, kind, namespace, name)
}

func (s *cognitiveQuotaFaultStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if kind == s.listErrKind {
		return nil, errors.New("list unavailable")
	}
	return s.memStore.List(ctx, kind, opts)
}

func (s *conflictingCognitiveQuotaUsageStore) Update(
	ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64,
) error {
	if kind == cognitiveQuotaKind {
		s.updates++
		if s.conflictsRemaining > 0 && (s.conflictName == "" || s.conflictName == name) {
			s.conflictsRemaining--
			if s.onConflict == nil {
				return store.ErrConflict
			}
			raw, err := s.memStore.Get(ctx, kind, namespace, name)
			if err != nil {
				return err
			}
			var latest v1alpha1.CognitiveQuota
			if err := json.Unmarshal(raw, &latest); err != nil {
				return err
			}
			s.onConflict(&latest)
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

func createQuota(t *testing.T, s store.Store, quota v1alpha1.CognitiveQuota) {
	t.Helper()
	data, err := json.Marshal(quota)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), cognitiveQuotaKind, quota.ObjectMeta.Namespace, quota.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
}

func getQuota(t *testing.T, s store.Store, ns, name string) v1alpha1.CognitiveQuota {
	t.Helper()
	data, err := s.Get(context.Background(), cognitiveQuotaKind, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	var quota v1alpha1.CognitiveQuota
	if err := json.Unmarshal(data, &quota); err != nil {
		t.Fatal(err)
	}
	return quota
}

func TestCognitiveQuota_ShouldReset_NoInterval(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	quota := v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "default", Name: "q1"},
		Spec:       v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": 5}},
	}
	// No ResetInterval → shouldReset returns false.
	if r.shouldReset(context.Background(), &quota) {
		t.Fatal("expected no reset when ResetInterval is empty")
	}
}

func TestCognitiveQuota_ShouldReset_ElapsedInterval(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	quota := v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "default", Name: "q1"},
		Spec:       v1alpha1.CognitiveQuotaSpec{ResetInterval: "1h", Hard: map[string]int64{"agents": 5}},
		Status:     v1alpha1.CognitiveQuotaStatus{LastResetTime: past},
	}
	if !r.shouldReset(context.Background(), &quota) {
		t.Fatal("expected reset when interval has elapsed")
	}
}

func TestCognitiveQuota_ShouldReset_NotYet(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	recent := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	quota := v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "default", Name: "q1"},
		Spec:       v1alpha1.CognitiveQuotaSpec{ResetInterval: "1h", Hard: map[string]int64{"agents": 5}},
		Status:     v1alpha1.CognitiveQuotaStatus{LastResetTime: recent},
	}
	if r.shouldReset(context.Background(), &quota) {
		t.Fatal("expected no reset when interval has not elapsed")
	}
}

func TestCognitiveQuota_NonPositiveResetIntervalNeverClearsUsage(t *testing.T) {
	r := NewCognitiveQuotaReconciler(newMemStore(), zaptest.NewLogger(t))
	for _, interval := range []string{"0s", "-1h"} {
		quota := v1alpha1.CognitiveQuota{
			Spec:   v1alpha1.CognitiveQuotaSpec{ResetInterval: interval},
			Status: v1alpha1.CognitiveQuotaStatus{LastResetTime: time.Now().Add(-time.Hour).Format(time.RFC3339Nano), CostUsage: &v1alpha1.QuotaCostUsage{TotalCost: 10}},
		}
		if r.shouldReset(context.Background(), &quota) {
			t.Fatalf("ResetInterval %q cleared usage", interval)
		}
		if got := r.resetRequeueAfter(&quota); got != 0 {
			t.Fatalf("ResetInterval %q RequeueAfter = %s, want 0", interval, got)
		}
	}
}

func TestCognitiveQuota_ReconcileSchedulesNextReset(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
	fixedNow := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	previousNow := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() { nowFunc = previousNow })
	quota := v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "default", Name: "q1", ResourceVersion: 1},
		Spec:       v1alpha1.CognitiveQuotaSpec{ResetInterval: "1h", Hard: map[string]int64{}},
		Status:     v1alpha1.CognitiveQuotaStatus{LastResetTime: fixedNow.Add(-10 * time.Minute).Format(time.RFC3339Nano)},
	}
	createQuota(t, s, quota)
	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "q1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 50*time.Minute {
		t.Fatalf("RequeueAfter = %s, want 50m", result.RequeueAfter)
	}
}

func TestCognitiveQuota_ResetUsage_ZeroesCounters(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	quota := v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "default", Name: "q1"},
		Status: v1alpha1.CognitiveQuotaStatus{
			TokenUsage: &v1alpha1.QuotaTokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
			CostUsage:  &v1alpha1.QuotaCostUsage{TotalCost: 500, ToolCalls: 10},
		},
	}
	r.resetUsage(&quota)
	if quota.Status.TokenUsage != nil {
		t.Fatalf("expected TokenUsage nil after reset, got %+v", quota.Status.TokenUsage)
	}
	if quota.Status.CostUsage != nil {
		t.Fatalf("expected CostUsage nil after reset, got %+v", quota.Status.CostUsage)
	}
	if quota.Status.LastResetTime == "" {
		t.Fatal("expected LastResetTime to be set after reset")
	}
}

func TestCognitiveQuota_IncrementTokenUsage(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	quota := v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1", ResourceVersion: 1},
		Spec:       v1alpha1.CognitiveQuotaSpec{TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 1000}, Hard: map[string]int64{}},
	}
	createQuota(t, s, quota)

	if err := r.IncrementTokenUsage(context.Background(), "ns1", 100, 50); err != nil {
		t.Fatal(err)
	}

	got := getQuota(t, s, "ns1", "q1")
	if got.Status.TokenUsage == nil {
		t.Fatal("expected token usage to be set")
	}
	if got.Status.TokenUsage.InputTokens != 100 {
		t.Fatalf("expected input 100, got %d", got.Status.TokenUsage.InputTokens)
	}
	if got.Status.TokenUsage.OutputTokens != 50 {
		t.Fatalf("expected output 50, got %d", got.Status.TokenUsage.OutputTokens)
	}
	if got.Status.TokenUsage.TotalTokens != 150 {
		t.Fatalf("expected total 150, got %d", got.Status.TokenUsage.TotalTokens)
	}
}

func TestCognitiveQuota_IncrementTokenUsageRetriesConcurrentConsumption(t *testing.T) {
	baseStore := newMemStore()
	s := &conflictingCognitiveQuotaUsageStore{
		memStore:           baseStore,
		conflictsRemaining: 1,
		onConflict: func(latest *v1alpha1.CognitiveQuota) {
			latest.Status.TokenUsage = &v1alpha1.QuotaTokenUsage{
				InputTokens: 30, OutputTokens: 20, TotalTokens: 50,
			}
			latest.Status.LastResetTime = "2026-01-02T03:04:05Z"
		},
	}
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
	quota := v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1"},
		Spec:       v1alpha1.CognitiveQuotaSpec{TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 1000}},
	}
	createQuota(t, baseStore, quota)

	if err := r.IncrementTokenUsage(context.Background(), "ns1", 100, 50); err != nil {
		t.Fatalf("IncrementTokenUsage() error = %v", err)
	}
	if s.updates != 2 {
		t.Fatalf("CognitiveQuota update attempts = %d, want 2", s.updates)
	}
	got := getQuota(t, baseStore, "ns1", "q1")
	if got.Status.TokenUsage == nil {
		t.Fatal("expected token usage to be set")
	}
	if got.Status.TokenUsage.InputTokens != 130 || got.Status.TokenUsage.OutputTokens != 70 || got.Status.TokenUsage.TotalTokens != 200 {
		t.Fatalf("token usage = %+v, want input/output/total 130/70/200", got.Status.TokenUsage)
	}
	if got.Status.LastResetTime != "2026-01-02T03:04:05Z" {
		t.Fatalf("LastResetTime = %q, want concurrent value", got.Status.LastResetTime)
	}
}

func TestCognitiveQuota_AccountingRetriesConcurrentConsumption(t *testing.T) {
	tests := []struct {
		name       string
		quota      v1alpha1.CognitiveQuota
		increment  func(*CognitiveQuotaReconciler) error
		concurrent func(*v1alpha1.CognitiveQuota)
		assert     func(*testing.T, v1alpha1.CognitiveQuota)
	}{
		{
			name:  "resource",
			quota: v1alpha1.CognitiveQuota{Spec: v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": 10}}},
			increment: func(r *CognitiveQuotaReconciler) error {
				return r.IncrementUsage(context.Background(), "ns1", "agents", 2)
			},
			concurrent: func(quota *v1alpha1.CognitiveQuota) {
				quota.Status.Used = map[string]int64{"agents": 3}
				quota.Status.LastResetTime = "concurrent"
			},
			assert: func(t *testing.T, quota v1alpha1.CognitiveQuota) {
				if quota.Status.Used["agents"] != 5 {
					t.Fatalf("agents usage = %d, want 5", quota.Status.Used["agents"])
				}
			},
		},
		{
			name:  "cost",
			quota: v1alpha1.CognitiveQuota{Spec: v1alpha1.CognitiveQuotaSpec{CostBudget: &v1alpha1.CostBudget{MaxCostPerPeriod: 1000}}},
			increment: func(r *CognitiveQuotaReconciler) error {
				return r.IncrementCostUsage(context.Background(), "ns1", 200, 3)
			},
			concurrent: func(quota *v1alpha1.CognitiveQuota) {
				quota.Status.CostUsage = &v1alpha1.QuotaCostUsage{TotalCost: 50, ToolCalls: 2}
				quota.Status.LastResetTime = "concurrent"
			},
			assert: func(t *testing.T, quota v1alpha1.CognitiveQuota) {
				if quota.Status.CostUsage == nil || quota.Status.CostUsage.TotalCost != 250 || quota.Status.CostUsage.ToolCalls != 5 {
					t.Fatalf("cost usage = %+v, want cost/tool calls 250/5", quota.Status.CostUsage)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseStore := newMemStore()
			s := &conflictingCognitiveQuotaUsageStore{
				memStore: baseStore, conflictsRemaining: 1, onConflict: tt.concurrent,
			}
			r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
			tt.quota.ObjectMeta = v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1"}
			createQuota(t, baseStore, tt.quota)

			if err := tt.increment(r); err != nil {
				t.Fatalf("increment error = %v", err)
			}
			if s.updates != 2 {
				t.Fatalf("update attempts = %d, want 2", s.updates)
			}
			got := getQuota(t, baseStore, "ns1", "q1")
			tt.assert(t, got)
			if got.Status.LastResetTime != "concurrent" {
				t.Fatalf("LastResetTime = %q, want concurrent value", got.Status.LastResetTime)
			}
		})
	}
}

func TestCognitiveQuota_AccountingPersistentConflictStopsAfterFiveAttempts(t *testing.T) {
	tests := []struct {
		name      string
		quota     v1alpha1.CognitiveQuota
		increment func(*CognitiveQuotaReconciler) error
	}{
		{
			name:  "resource",
			quota: v1alpha1.CognitiveQuota{Spec: v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": 10}}},
			increment: func(r *CognitiveQuotaReconciler) error {
				return r.IncrementUsage(context.Background(), "ns1", "agents", 1)
			},
		},
		{
			name:  "token",
			quota: v1alpha1.CognitiveQuota{Spec: v1alpha1.CognitiveQuotaSpec{TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 1000}}},
			increment: func(r *CognitiveQuotaReconciler) error {
				return r.IncrementTokenUsage(context.Background(), "ns1", 1, 1)
			},
		},
		{
			name:  "cost",
			quota: v1alpha1.CognitiveQuota{Spec: v1alpha1.CognitiveQuotaSpec{CostBudget: &v1alpha1.CostBudget{MaxCostPerPeriod: 1000}}},
			increment: func(r *CognitiveQuotaReconciler) error {
				return r.IncrementCostUsage(context.Background(), "ns1", 1, 1)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseStore := newMemStore()
			s := &conflictingCognitiveQuotaUsageStore{memStore: baseStore, conflictsRemaining: 5}
			r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
			tt.quota.ObjectMeta = v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1"}
			createQuota(t, baseStore, tt.quota)

			err := tt.increment(r)
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("increment error = %v, want conflict", err)
			}
			if s.updates != 5 {
				t.Fatalf("update attempts = %d, want 5", s.updates)
			}
		})
	}
}

func TestCognitiveQuota_IncrementTokenUsageRetriesOnlyConflictingQuota(t *testing.T) {
	baseStore := newMemStore()
	s := &conflictingCognitiveQuotaUsageStore{
		memStore: baseStore, conflictName: "q2", conflictsRemaining: 1,
		onConflict: func(quota *v1alpha1.CognitiveQuota) {
			quota.Status.TokenUsage = &v1alpha1.QuotaTokenUsage{TotalTokens: 10}
		},
	}
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
	for _, name := range []string{"q1", "q2"} {
		createQuota(t, baseStore, v1alpha1.CognitiveQuota{
			ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: name},
			Spec:       v1alpha1.CognitiveQuotaSpec{TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 1000}},
		})
	}

	if err := r.IncrementTokenUsage(context.Background(), "ns1", 6, 4); err != nil {
		t.Fatalf("IncrementTokenUsage() error = %v", err)
	}
	if s.updates != 3 {
		t.Fatalf("update attempts = %d, want 3", s.updates)
	}
	if got := getQuota(t, baseStore, "ns1", "q1").Status.TokenUsage; got == nil || got.TotalTokens != 10 {
		t.Fatalf("q1 token usage = %+v, want total 10", got)
	}
	if got := getQuota(t, baseStore, "ns1", "q2").Status.TokenUsage; got == nil || got.TotalTokens != 20 {
		t.Fatalf("q2 token usage = %+v, want total 20", got)
	}
}

func TestCognitiveQuota_IncrementTokenUsageDoesNotRestoreRemovedBudget(t *testing.T) {
	baseStore := newMemStore()
	s := &conflictingCognitiveQuotaUsageStore{
		memStore: baseStore, conflictsRemaining: 1,
		onConflict: func(quota *v1alpha1.CognitiveQuota) {
			quota.Spec.TokenBudget = nil
			quota.Status.LastResetTime = "concurrent"
		},
	}
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
	createQuota(t, baseStore, v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1"},
		Spec:       v1alpha1.CognitiveQuotaSpec{TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 1000}},
	})

	if err := r.IncrementTokenUsage(context.Background(), "ns1", 6, 4); err != nil {
		t.Fatalf("IncrementTokenUsage() error = %v", err)
	}
	if s.updates != 1 {
		t.Fatalf("update attempts = %d, want 1 after policy removal", s.updates)
	}
	got := getQuota(t, baseStore, "ns1", "q1")
	if got.Spec.TokenBudget != nil {
		t.Fatalf("TokenBudget = %+v, want removed budget preserved", got.Spec.TokenBudget)
	}
	if got.Status.TokenUsage != nil {
		t.Fatalf("TokenUsage = %+v, want no accounting for removed budget", got.Status.TokenUsage)
	}
	if got.Status.LastResetTime != "concurrent" {
		t.Fatalf("LastResetTime = %q, want concurrent value", got.Status.LastResetTime)
	}
}

func TestCognitiveQuota_IncrementCostUsage(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	quota := v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1", ResourceVersion: 1},
		Spec:       v1alpha1.CognitiveQuotaSpec{CostBudget: &v1alpha1.CostBudget{MaxCostPerPeriod: 1000}, Hard: map[string]int64{}},
	}
	createQuota(t, s, quota)

	if err := r.IncrementCostUsage(context.Background(), "ns1", 200, 3); err != nil {
		t.Fatal(err)
	}

	got := getQuota(t, s, "ns1", "q1")
	if got.Status.CostUsage == nil {
		t.Fatal("expected cost usage to be set")
	}
	if got.Status.CostUsage.TotalCost != 200 {
		t.Fatalf("expected cost 200, got %d", got.Status.CostUsage.TotalCost)
	}
	if got.Status.CostUsage.ToolCalls != 3 {
		t.Fatalf("expected tool calls 3, got %d", got.Status.CostUsage.ToolCalls)
	}
}

func TestCognitiveQuota_RejectsInvalidAccounting(t *testing.T) {
	tests := []struct {
		name      string
		quota     v1alpha1.CognitiveQuota
		operation func(*CognitiveQuotaReconciler) error
	}{
		{
			name:  "negative resource check",
			quota: v1alpha1.CognitiveQuota{Spec: v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": 10}}},
			operation: func(r *CognitiveQuotaReconciler) error {
				return r.CheckQuota(context.Background(), "ns1", "agents", -1)
			},
		},
		{
			name:  "resource check overflow",
			quota: v1alpha1.CognitiveQuota{Spec: v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": math.MaxInt64}}, Status: v1alpha1.CognitiveQuotaStatus{Used: map[string]int64{"agents": math.MaxInt64}}},
			operation: func(r *CognitiveQuotaReconciler) error {
				return r.CheckQuota(context.Background(), "ns1", "agents", 1)
			},
		},
		{
			name:  "negative resource increment",
			quota: v1alpha1.CognitiveQuota{Spec: v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": 10}}},
			operation: func(r *CognitiveQuotaReconciler) error {
				return r.IncrementUsage(context.Background(), "ns1", "agents", -1)
			},
		},
		{
			name:  "resource increment overflow",
			quota: v1alpha1.CognitiveQuota{Spec: v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": math.MaxInt64}}, Status: v1alpha1.CognitiveQuotaStatus{Used: map[string]int64{"agents": math.MaxInt64}}},
			operation: func(r *CognitiveQuotaReconciler) error {
				return r.IncrementUsage(context.Background(), "ns1", "agents", 1)
			},
		},
		{
			name:  "negative token increment",
			quota: v1alpha1.CognitiveQuota{Spec: v1alpha1.CognitiveQuotaSpec{TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: math.MaxInt64}}},
			operation: func(r *CognitiveQuotaReconciler) error {
				return r.IncrementTokenUsage(context.Background(), "ns1", -1, 0)
			},
		},
		{
			name: "token check overflow",
			quota: v1alpha1.CognitiveQuota{
				Spec:   v1alpha1.CognitiveQuotaSpec{TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: math.MaxInt64}},
				Status: v1alpha1.CognitiveQuotaStatus{TokenUsage: &v1alpha1.QuotaTokenUsage{TotalTokens: math.MaxInt64}},
			},
			operation: func(r *CognitiveQuotaReconciler) error {
				return r.CheckTokenQuota(context.Background(), "ns1", 1, 0)
			},
		},
		{
			name: "token increment overflow",
			quota: v1alpha1.CognitiveQuota{
				Spec:   v1alpha1.CognitiveQuotaSpec{TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: math.MaxInt64}},
				Status: v1alpha1.CognitiveQuotaStatus{TokenUsage: &v1alpha1.QuotaTokenUsage{TotalTokens: math.MaxInt64}},
			},
			operation: func(r *CognitiveQuotaReconciler) error {
				return r.IncrementTokenUsage(context.Background(), "ns1", 1, 0)
			},
		},
		{
			name:  "negative cost increment",
			quota: v1alpha1.CognitiveQuota{Spec: v1alpha1.CognitiveQuotaSpec{CostBudget: &v1alpha1.CostBudget{MaxCostPerPeriod: math.MaxInt64}}},
			operation: func(r *CognitiveQuotaReconciler) error {
				return r.IncrementCostUsage(context.Background(), "ns1", -1, 0)
			},
		},
		{
			name: "cost check overflow",
			quota: v1alpha1.CognitiveQuota{
				Spec:   v1alpha1.CognitiveQuotaSpec{CostBudget: &v1alpha1.CostBudget{MaxCostPerPeriod: math.MaxInt64}},
				Status: v1alpha1.CognitiveQuotaStatus{CostUsage: &v1alpha1.QuotaCostUsage{TotalCost: math.MaxInt64}},
			},
			operation: func(r *CognitiveQuotaReconciler) error {
				return r.CheckCostQuota(context.Background(), "ns1", 1, 0)
			},
		},
		{
			name: "cost increment overflow",
			quota: v1alpha1.CognitiveQuota{
				Spec:   v1alpha1.CognitiveQuotaSpec{CostBudget: &v1alpha1.CostBudget{MaxCostPerPeriod: math.MaxInt64}},
				Status: v1alpha1.CognitiveQuotaStatus{CostUsage: &v1alpha1.QuotaCostUsage{TotalCost: math.MaxInt64}},
			},
			operation: func(r *CognitiveQuotaReconciler) error {
				return r.IncrementCostUsage(context.Background(), "ns1", 1, 0)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newMemStore()
			r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
			tt.quota.ObjectMeta = v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1"}
			storeExactQuota(t, s, tt.quota)

			if err := tt.operation(r); err == nil {
				t.Fatal("operation error = nil, want invalid accounting rejected")
			}
			got := getQuota(t, s, "ns1", "q1")
			if !quotaUsageEqual(got.Status, tt.quota.Status) {
				t.Fatalf("quota usage changed: got %+v, want %+v", got.Status, tt.quota.Status)
			}
		})
	}
}

func quotaUsageEqual(got, want v1alpha1.CognitiveQuotaStatus) bool {
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	return string(gotJSON) == string(wantJSON)
}

func storeExactQuota(t *testing.T, s *memStore, quota v1alpha1.CognitiveQuota) {
	t.Helper()
	data, err := json.Marshal(quota)
	if err != nil {
		t.Fatal(err)
	}
	s.data[s.key(cognitiveQuotaKind, quota.ObjectMeta.Namespace, quota.ObjectMeta.Name)] = data
}

func TestCognitiveQuota_ReconcileCountsObjectsAndReportsOverage(t *testing.T) {
	s := newMemStore()
	createQuota(t, s, v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1"},
		Spec:       v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": 1}},
	})
	for _, name := range []string{"agent-a", "agent-b"} {
		if err := s.Create(context.Background(), "Agent", "ns1", name, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}

	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
	if _, err := r.Reconcile(context.Background(), Request{Namespace: "ns1", Name: "q1"}); err != nil {
		t.Fatal(err)
	}
	got := getQuota(t, s, "ns1", "q1")
	if got.Status.Used["agents"] != 2 || got.Status.Phase != v1alpha1.PhaseDegraded || got.Status.Message != "quota exceeded for agents" {
		t.Fatalf("status = %+v, want two agents and degraded phase", got.Status)
	}
}

func TestCognitiveQuota_FailsClosedOnStoreErrors(t *testing.T) {
	backendErr := errors.New("backend unavailable")
	tests := []struct {
		name string
		run  func(*CognitiveQuotaReconciler) error
	}{
		{name: "reconcile get", run: func(r *CognitiveQuotaReconciler) error {
			_, err := r.Reconcile(context.Background(), Request{Namespace: "ns1", Name: "q1"})
			return err
		}},
		{name: "quota list", run: func(r *CognitiveQuotaReconciler) error {
			return r.CheckQuota(context.Background(), "ns1", "agents", 1)
		}},
		{name: "token list", run: func(r *CognitiveQuotaReconciler) error {
			return r.CheckTokenQuota(context.Background(), "ns1", 1, 1)
		}},
		{name: "cost list", run: func(r *CognitiveQuotaReconciler) error {
			return r.CheckCostQuota(context.Background(), "ns1", 1, 1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := &cognitiveQuotaFaultStore{memStore: newMemStore(), listErrKind: cognitiveQuotaKind}
			if test.name == "reconcile get" {
				s.getErr = backendErr
			}
			if err := test.run(NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))); err == nil {
				t.Fatal("error = nil, want backend failure")
			}
		})
	}
}

func TestCognitiveQuota_ChecksSpecificBudgetDimensions(t *testing.T) {
	tests := []struct {
		name  string
		quota v1alpha1.CognitiveQuota
		check func(*CognitiveQuotaReconciler) error
	}{
		{
			name:  "input tokens",
			quota: v1alpha1.CognitiveQuota{Spec: v1alpha1.CognitiveQuotaSpec{TokenBudget: &v1alpha1.TokenBudget{MaxInputTokens: 10}}, Status: v1alpha1.CognitiveQuotaStatus{TokenUsage: &v1alpha1.QuotaTokenUsage{InputTokens: 9}}},
			check: func(r *CognitiveQuotaReconciler) error { return r.CheckTokenQuota(context.Background(), "ns1", 2, 0) },
		},
		{
			name:  "output tokens",
			quota: v1alpha1.CognitiveQuota{Spec: v1alpha1.CognitiveQuotaSpec{TokenBudget: &v1alpha1.TokenBudget{MaxOutputTokens: 10}}, Status: v1alpha1.CognitiveQuotaStatus{TokenUsage: &v1alpha1.QuotaTokenUsage{OutputTokens: 9}}},
			check: func(r *CognitiveQuotaReconciler) error { return r.CheckTokenQuota(context.Background(), "ns1", 0, 2) },
		},
		{
			name:  "period cost",
			quota: v1alpha1.CognitiveQuota{Spec: v1alpha1.CognitiveQuotaSpec{CostBudget: &v1alpha1.CostBudget{MaxCostPerPeriod: 10}}, Status: v1alpha1.CognitiveQuotaStatus{CostUsage: &v1alpha1.QuotaCostUsage{TotalCost: 9}}},
			check: func(r *CognitiveQuotaReconciler) error { return r.CheckCostQuota(context.Background(), "ns1", 2, 0) },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newMemStore()
			test.quota.ObjectMeta = v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1"}
			storeExactQuota(t, s, test.quota)
			if err := test.check(NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))); err == nil {
				t.Fatal("error = nil, want budget rejection")
			}
		})
	}
}

func TestQuotaArithmeticRejectsInvalidDeltas(t *testing.T) {
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "negative current", run: func() error { _, err := addQuotaUsage(-1, 0); return err }},
		{name: "delta overflow", run: func() error { _, err := addQuotaDelta(math.MaxInt64, 1); return err }},
		{name: "delta underflow", run: func() error { _, err := addQuotaDelta(math.MinInt64, -1); return err }},
		{name: "negative stored usage", run: func() error { _, err := applyQuotaDelta(-1, 1); return err }},
		{name: "negative result", run: func() error { _, err := applyQuotaDelta(1, -2); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil {
				t.Fatal("error = nil, want invalid arithmetic rejected")
			}
		})
	}
}

// TC-0087: Token quota exceeded; request rejected.
func TestCognitiveQuota_CheckTokenQuota_Exceeded(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	quota := v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1", ResourceVersion: 1},
		Spec:       v1alpha1.CognitiveQuotaSpec{TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 100}, Hard: map[string]int64{}},
		Status:     v1alpha1.CognitiveQuotaStatus{TokenUsage: &v1alpha1.QuotaTokenUsage{TotalTokens: 90}},
	}
	createQuota(t, s, quota)

	err := r.CheckTokenQuota(context.Background(), "ns1", 10, 5)
	if err == nil {
		t.Fatal("expected error for exceeded quota")
	}
}

func TestCognitiveQuota_CheckTokenQuota_OK(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	quota := v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1", ResourceVersion: 1},
		Spec:       v1alpha1.CognitiveQuotaSpec{TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 1000}, Hard: map[string]int64{}},
		Status:     v1alpha1.CognitiveQuotaStatus{TokenUsage: &v1alpha1.QuotaTokenUsage{TotalTokens: 50}},
	}
	createQuota(t, s, quota)

	if err := r.CheckTokenQuota(context.Background(), "ns1", 10, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCognitiveQuota_CheckCostQuota_Exceeded(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	quota := v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1", ResourceVersion: 1},
		Spec:       v1alpha1.CognitiveQuotaSpec{CostBudget: &v1alpha1.CostBudget{MaxToolCallsPerPeriod: 5}, Hard: map[string]int64{}},
		Status:     v1alpha1.CognitiveQuotaStatus{CostUsage: &v1alpha1.QuotaCostUsage{ToolCalls: 4}},
	}
	createQuota(t, s, quota)

	err := r.CheckCostQuota(context.Background(), "ns1", 0, 2)
	if err == nil {
		t.Fatal("expected error for exceeded tool calls quota")
	}
}

func TestCognitiveQuota_CheckModelTierAndExactCostQuota(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
	quota := v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1", ResourceVersion: 1},
		Spec: v1alpha1.CognitiveQuotaSpec{CostBudget: &v1alpha1.CostBudget{
			MaxCostPerPeriod: 100, MaxModelTierSeconds: map[string]int64{"premium": 60},
		}, Hard: map[string]int64{}},
		Status: v1alpha1.CognitiveQuotaStatus{CostUsage: &v1alpha1.QuotaCostUsage{
			TotalCost: 100, ModelTierSeconds: map[string]int64{"premium": 60},
		}},
	}
	createQuota(t, s, quota)
	if err := r.CheckCostQuota(context.Background(), "ns1", 0, 0); err == nil {
		t.Fatal("exactly exhausted monetary quota was accepted")
	}
	if err := r.CheckModelTierQuota(context.Background(), "ns1", "premium"); err == nil {
		t.Fatal("exactly exhausted model-tier quota was accepted")
	}
	if err := r.CheckModelTierQuota(context.Background(), "ns1", "standard"); err != nil {
		t.Fatalf("unlimited model tier was rejected: %v", err)
	}
}

func TestCognitiveQuota_ShouldReset_EmptyLastResetTime(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	// ResetInterval set but LastResetTime empty → first reset.
	quota := v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "default", Name: "q1"},
		Spec:       v1alpha1.CognitiveQuotaSpec{ResetInterval: "1h", Hard: map[string]int64{"agents": 5}},
		Status:     v1alpha1.CognitiveQuotaStatus{LastResetTime: ""},
	}
	if !r.shouldReset(context.Background(), &quota) {
		t.Fatal("expected reset when LastResetTime is empty with a valid ResetInterval")
	}
}

func TestCognitiveQuota_Reconcile_TriggersReset(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	quota := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1"},
		Spec:       v1alpha1.CognitiveQuotaSpec{ResetInterval: "1h", Hard: map[string]int64{}, TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 1000}},
		Status: v1alpha1.CognitiveQuotaStatus{
			LastResetTime: past,
			TokenUsage:    &v1alpha1.QuotaTokenUsage{InputTokens: 500, OutputTokens: 200, TotalTokens: 700},
			CostUsage:     &v1alpha1.QuotaCostUsage{TotalCost: 100, ToolCalls: 5},
		},
	}
	createQuota(t, s, quota)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "ns1", Name: "q1", Kind: "CognitiveQuota"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getQuota(t, s, "ns1", "q1")
	if got.Status.TokenUsage != nil {
		t.Fatalf("expected TokenUsage nil after reset, got %+v", got.Status.TokenUsage)
	}
	if got.Status.CostUsage != nil {
		t.Fatalf("expected CostUsage nil after reset, got %+v", got.Status.CostUsage)
	}
	if got.Status.LastResetTime == "" {
		t.Fatal("expected LastResetTime to be updated")
	}
	if got.Status.LastResetTime == past {
		t.Fatal("expected LastResetTime to differ from original past time")
	}
}

func TestCognitiveQuota_Reconcile_InvalidResetState(t *testing.T) {
	tests := []struct {
		name             string
		interval         string
		lastReset        string
		wantUsageCleared bool
	}{
		{name: "invalid interval preserves usage", interval: "daily", lastReset: "2026-01-02T03:04:05Z"},
		{name: "invalid timestamp resets usage", interval: "1h", lastReset: "not-a-timestamp", wantUsageCleared: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newMemStore()
			r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
			createQuota(t, s, v1alpha1.CognitiveQuota{
				TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota"},
				ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1"},
				Spec: v1alpha1.CognitiveQuotaSpec{
					ResetInterval: test.interval,
					TokenBudget:   &v1alpha1.TokenBudget{MaxTotalTokens: 1000},
				},
				Status: v1alpha1.CognitiveQuotaStatus{
					LastResetTime: test.lastReset,
					TokenUsage:    &v1alpha1.QuotaTokenUsage{TotalTokens: 700},
				},
			})

			if _, err := r.Reconcile(context.Background(), Request{Namespace: "ns1", Name: "q1", Kind: cognitiveQuotaKind}); err != nil {
				t.Fatalf("reconcile quota: %v", err)
			}

			got := getQuota(t, s, "ns1", "q1")
			if test.wantUsageCleared {
				if got.Status.TokenUsage != nil {
					t.Fatalf("TokenUsage = %+v, want reset", got.Status.TokenUsage)
				}
				if got.Status.LastResetTime == test.lastReset {
					t.Fatalf("LastResetTime = %q, want repaired timestamp", got.Status.LastResetTime)
				}
				return
			}
			if got.Status.TokenUsage == nil || got.Status.TokenUsage.TotalTokens != 700 {
				t.Fatalf("TokenUsage = %+v, want preserved total of 700", got.Status.TokenUsage)
			}
			if got.Status.LastResetTime != test.lastReset {
				t.Fatalf("LastResetTime = %q, want preserved %q", got.Status.LastResetTime, test.lastReset)
			}
		})
	}
}

func TestCognitiveQuota_IncrementUsage(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	quota := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1"},
		Spec:       v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": 10}},
		Status:     v1alpha1.CognitiveQuotaStatus{Used: map[string]int64{"agents": 3}},
	}
	createQuota(t, s, quota)

	if err := r.IncrementUsage(context.Background(), "ns1", "agents", 2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getQuota(t, s, "ns1", "q1")
	if got.Status.Used["agents"] != 5 {
		t.Errorf("expected agents usage 5, got %d", got.Status.Used["agents"])
	}
}

func TestCognitiveQuota_IncrementUsage_NoMatchingQuota(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	// Quota has Hard limit for "agents" but we increment "tools" → no-op.
	quota := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "q1"},
		Spec:       v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": 10}},
		Status:     v1alpha1.CognitiveQuotaStatus{Used: map[string]int64{"agents": 3}},
	}
	createQuota(t, s, quota)

	if err := r.IncrementUsage(context.Background(), "ns1", "tools", 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Agents count unchanged.
	got := getQuota(t, s, "ns1", "q1")
	if got.Status.Used["agents"] != 3 {
		t.Errorf("expected agents usage unchanged at 3, got %d", got.Status.Used["agents"])
	}
}

func TestCognitiveQuota_Reconcile_NotFound(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	// Reconciling a deleted quota should succeed (skip).
	result, err := r.Reconcile(context.Background(), Request{Namespace: "ns1", Name: "nonexistent", Kind: "CognitiveQuota"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("unexpected requeue for deleted quota")
	}
}

func TestCognitiveQuota_MultipleQuotas_MostRestrictiveWins(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))

	// Two quotas in the same namespace with different limits.
	q1 := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "generous"},
		Spec:       v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": 100}},
		Status:     v1alpha1.CognitiveQuotaStatus{Used: map[string]int64{"agents": 5}},
	}
	q2 := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "strict"},
		Spec:       v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": 6}},
		Status:     v1alpha1.CognitiveQuotaStatus{Used: map[string]int64{"agents": 5}},
	}
	createQuota(t, s, q1)
	createQuota(t, s, q2)

	// Adding 2 agents: generous allows (5+2 <= 100) but strict denies (5+2 > 6).
	err := r.CheckQuota(context.Background(), "ns1", "agents", 2)
	if err == nil {
		t.Fatal("expected quota exceeded error from the stricter quota")
	}

	// Adding 1 agent: both allow (5+1 <= 100, 5+1 <= 6).
	err = r.CheckQuota(context.Background(), "ns1", "agents", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
