// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type quotaRefundFaultStore struct {
	*memStore
	failNextQuotaUpdate bool
}

type durableReservationTestStore struct {
	*memStore
	mu           sync.Mutex
	reservations map[string]store.TokenReservation
	settled      int
	reapBefore   time.Time
	reapLimit    int
}

func (s *durableReservationTestStore) CreateTokenReservation(
	_ context.Context, reservation store.TokenReservation,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.reservations[reservation.ID]; ok {
		if existing == reservation {
			return false, nil
		}
		return false, store.ErrConflict
	}
	s.reservations[reservation.ID] = reservation
	return true, nil
}

func (s *durableReservationTestStore) SettleTokenReservation(
	_ context.Context, reservationID string, _, _ int64,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.reservations[reservationID]; !ok {
		return false, nil
	}
	delete(s.reservations, reservationID)
	s.settled++
	return true, nil
}

func (s *durableReservationTestStore) ReapExpiredTokenReservations(
	_ context.Context, expiredBefore time.Time, limit int,
) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapBefore = expiredBefore
	s.reapLimit = limit
	return 0, nil
}

func (s *quotaRefundFaultStore) Update(
	ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64,
) error {
	if kind == cognitiveQuotaKind && s.failNextQuotaUpdate {
		s.failNextQuotaUpdate = false
		return errors.New("quota storage unavailable")
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func createLimitRange(t *testing.T, s store.Store, namespace, name string, tokensPerSession int64) {
	t.Helper()
	lr := v1alpha1.CognitiveLimitRange{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveLimitRange", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1alpha1.CognitiveLimitRangeSpec{
			Limits: []v1alpha1.LimitRangeItem{
				{Type: "Agent", TokensPerSession: tokensPerSession},
			},
		},
	}
	data, err := json.Marshal(lr)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), "CognitiveLimitRange", namespace, name, data); err != nil {
		t.Fatal(err)
	}
}

// TC-0090: CheckSchedulerQuota rejects when session quota is exhausted.
func TestQuotaEnforcer_CheckSchedulerQuota_Exhausted(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()

	// Create a quota allowing 2 sessions.
	createQuota(t, s, v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "default"},
		Spec:       v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"sessions": 2}},
		Status:     v1alpha1.CognitiveQuotaStatus{Used: make(map[string]int64)},
	})

	// Create 2 sessions to exhaust the quota.
	for i := range 2 {
		data, _ := json.Marshal(struct {
			v1alpha1.TypeMeta   `json:",inline"`
			v1alpha1.ObjectMeta `json:"metadata"`
		}{
			TypeMeta:   v1alpha1.TypeMeta{Kind: "Session", APIVersion: "v1alpha1"},
			ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: fmt.Sprintf("s%d", i)},
		})
		if err := s.Create(ctx, "Session", "team-a", fmt.Sprintf("s%d", i), data); err != nil {
			t.Fatal(err)
		}
	}

	// Reconcile so the usage count is updated.
	_, err := reconciler.Reconcile(ctx, Request{Namespace: "team-a", Name: "default"})
	if err != nil {
		t.Fatal(err)
	}

	// Now scheduler-time check should fail.
	err = enforcer.CheckSchedulerQuota(ctx, "team-a")
	if err == nil {
		t.Fatal("expected quota exhausted error")
	}
	if !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("expected ErrQuotaExhausted, got: %v", err)
	}
}

// TC-0090 variant: CheckSchedulerQuota passes with capacity.
func TestQuotaEnforcer_CheckSchedulerQuota_Passes(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()

	// Quota allows 5 sessions, none used.
	createQuota(t, s, v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-b", Name: "default"},
		Spec:       v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"sessions": 5}},
		Status:     v1alpha1.CognitiveQuotaStatus{Used: make(map[string]int64)},
	})

	if err := enforcer.CheckSchedulerQuota(ctx, "team-b"); err != nil {
		t.Fatalf("expected pass, got: %v", err)
	}
}

// TC-0091: CheckRuntimeBudget rejects when per-agent token limit is breached.
func TestQuotaEnforcer_CheckRuntimeBudget_Exceeded(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()

	// CognitiveLimitRange with 1000 tokens per session.
	createLimitRange(t, s, "team-a", "default", 1000)

	// Agent used 1000 tokens → should be rejected.
	err := enforcer.CheckRuntimeBudget(ctx, "team-a", "agent-1", 1000)
	if err == nil {
		t.Fatal("expected budget exceeded error")
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got: %v", err)
	}
}

// TC-0091 variant: CheckRuntimeBudget passes under limit.
func TestQuotaEnforcer_CheckRuntimeBudget_Passes(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()

	createLimitRange(t, s, "team-a", "default", 1000)

	// Agent used 500 tokens → should pass.
	if err := enforcer.CheckRuntimeBudget(ctx, "team-a", "agent-1", 500); err != nil {
		t.Fatalf("expected pass, got: %v", err)
	}
}

// TC-0091: No limit range → no enforcement.
func TestQuotaEnforcer_CheckRuntimeBudget_NoLimit(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()

	// No CognitiveLimitRange → unlimited.
	if err := enforcer.CheckRuntimeBudget(ctx, "team-a", "agent-1", 999999); err != nil {
		t.Fatalf("expected pass with no limit, got: %v", err)
	}
}

// TC-0092: Reserve and Settle refunds unused tokens.
func TestQuotaEnforcer_ReserveAndSettle(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()

	// Quota with token budget: 2000 total.
	createQuota(t, s, v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "default"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			TokenBudget: &v1alpha1.TokenBudget{
				MaxInputTokens:  1000,
				MaxOutputTokens: 1000,
				MaxTotalTokens:  2000,
			},
		},
		Status: v1alpha1.CognitiveQuotaStatus{Used: make(map[string]int64)},
	})

	// Reserve 800 input + 800 output.
	err := enforcer.ReserveTokens(ctx, "team-a", "session-1", 800, 800)
	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}

	// Verify quota was charged.
	quota := getQuota(t, s, "team-a", "default")
	if quota.Status.TokenUsage.InputTokens != 800 {
		t.Fatalf("expected 800 input charged, got %d", quota.Status.TokenUsage.InputTokens)
	}
	if quota.Status.TokenUsage.OutputTokens != 800 {
		t.Fatalf("expected 800 output charged, got %d", quota.Status.TokenUsage.OutputTokens)
	}

	// Settle: actually used 300 input + 200 output.
	err = enforcer.Settle(ctx, "session-1", 300, 200)
	if err != nil {
		t.Fatalf("settle failed: %v", err)
	}

	// Verify refund: 800-300=500 input refunded, 800-200=600 output refunded.
	quota = getQuota(t, s, "team-a", "default")
	if quota.Status.TokenUsage.InputTokens != 300 {
		t.Fatalf("expected 300 input after refund, got %d", quota.Status.TokenUsage.InputTokens)
	}
	if quota.Status.TokenUsage.OutputTokens != 200 {
		t.Fatalf("expected 200 output after refund, got %d", quota.Status.TokenUsage.OutputTokens)
	}
}

func TestQuotaEnforcer_DurableReservationSettlesAfterRestart(t *testing.T) {
	s := &durableReservationTestStore{
		memStore:     newMemStore(),
		reservations: make(map[string]store.TokenReservation),
	}
	logger := zaptest.NewLogger(t)
	ctx := context.Background()

	first := NewQuotaEnforcer(s, NewCognitiveQuotaReconciler(s, logger), logger)
	startedAt := nowFunc()
	if err := first.ReserveTokens(ctx, "team-a", "run-1", 400, 300); err != nil {
		t.Fatalf("ReserveTokens() error = %v", err)
	}
	if s.reapLimit != tokenReservationReapLimit || s.reapBefore.Before(startedAt.Add(-tokenReservationMaxAge)) {
		t.Fatalf("reaper called with before=%v limit=%d, want cutoff at or after %v and limit %d",
			s.reapBefore, s.reapLimit, startedAt.Add(-tokenReservationMaxAge), tokenReservationReapLimit)
	}

	second := NewQuotaEnforcer(s, NewCognitiveQuotaReconciler(s, logger), logger)
	if err := second.Settle(ctx, "run-1", 125, 75); err != nil {
		t.Fatalf("Settle() after restart error = %v", err)
	}
	if s.settled != 1 || len(s.reservations) != 0 {
		t.Fatalf("durable store settlement = %d, reservations = %d; want 1, 0", s.settled, len(s.reservations))
	}
}

func TestQuotaEnforcer_ReserveToolCallChargesAndEnforcesCostBudget(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	enforcer := NewQuotaEnforcer(s, NewCognitiveQuotaReconciler(s, logger), logger)
	createQuota(t, s, v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "default"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			CostBudget: &v1alpha1.CostBudget{MaxToolCallsPerPeriod: 1},
		},
	})

	if err := enforcer.ReserveToolCall(context.Background(), "team-a"); err != nil {
		t.Fatalf("first ReserveToolCall() error = %v", err)
	}
	quota := getQuota(t, s, "team-a", "default")
	if quota.Status.CostUsage == nil || quota.Status.CostUsage.ToolCalls != 1 {
		t.Fatalf("cost usage = %+v, want one tool call", quota.Status.CostUsage)
	}
	if err := enforcer.ReserveToolCall(context.Background(), "team-a"); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("second ReserveToolCall() error = %v, want ErrQuotaExhausted", err)
	}
	quota = getQuota(t, s, "team-a", "default")
	if quota.Status.CostUsage.ToolCalls != 1 {
		t.Fatalf("tool calls after denial = %d, want 1", quota.Status.CostUsage.ToolCalls)
	}
}

func TestQuotaEnforcer_SettleRetainsReservationWhenRefundFails(t *testing.T) {
	baseStore := newMemStore()
	s := &quotaRefundFaultStore{memStore: baseStore}
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()
	createQuota(t, baseStore, v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "default"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 2000},
		},
	})

	if err := enforcer.ReserveTokens(ctx, "team-a", "session-1", 800, 800); err != nil {
		t.Fatalf("ReserveTokens() error = %v", err)
	}
	s.failNextQuotaUpdate = true
	if err := enforcer.Settle(ctx, "session-1", 300, 200); err == nil {
		t.Fatal("Settle() error = nil, want storage failure")
	}
	charged := getQuota(t, baseStore, "team-a", "default")
	if charged.Status.TokenUsage == nil || charged.Status.TokenUsage.InputTokens != 800 || charged.Status.TokenUsage.OutputTokens != 800 {
		t.Fatalf("usage after failed refund = %+v, want 800/800", charged.Status.TokenUsage)
	}

	if err := enforcer.Settle(ctx, "session-1", 300, 200); err != nil {
		t.Fatalf("retry Settle() error = %v", err)
	}
	settled := getQuota(t, baseStore, "team-a", "default")
	if settled.Status.TokenUsage == nil || settled.Status.TokenUsage.InputTokens != 300 || settled.Status.TokenUsage.OutputTokens != 200 {
		t.Fatalf("usage after settlement retry = %+v, want 300/200", settled.Status.TokenUsage)
	}
}

func TestQuotaEnforcer_ReserveTokensIsIdempotentPerSession(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()
	createQuota(t, s, v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "default"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 2000},
		},
	})

	for attempt := 0; attempt < 2; attempt++ {
		if err := enforcer.ReserveTokens(ctx, "team-a", "session-1", 800, 800); err != nil {
			t.Fatalf("ReserveTokens() attempt %d error = %v", attempt+1, err)
		}
	}
	charged := getQuota(t, s, "team-a", "default")
	if charged.Status.TokenUsage == nil || charged.Status.TokenUsage.InputTokens != 800 || charged.Status.TokenUsage.OutputTokens != 800 {
		t.Fatalf("usage after duplicate reservation = %+v, want 800/800", charged.Status.TokenUsage)
	}
	if err := enforcer.ReserveTokens(ctx, "team-a", "session-1", 700, 800); err == nil {
		t.Fatal("ReserveTokens() with conflicting session reuse error = nil")
	}
	unchanged := getQuota(t, s, "team-a", "default")
	if unchanged.Status.TokenUsage.InputTokens != 800 || unchanged.Status.TokenUsage.OutputTokens != 800 {
		t.Fatalf("usage after conflicting reservation = %+v, want unchanged 800/800", unchanged.Status.TokenUsage)
	}
}

func TestQuotaEnforcer_ConcurrentDuplicateReservationChargesOnce(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()
	createQuota(t, s, v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "default"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 10000},
		},
	})

	const callers = 8
	errorsByCaller := make(chan error, callers)
	var start sync.WaitGroup
	start.Add(1)
	var callersDone sync.WaitGroup
	callersDone.Add(callers)
	for range callers {
		go func() {
			defer callersDone.Done()
			start.Wait()
			errorsByCaller <- enforcer.ReserveTokens(ctx, "team-a", "session-1", 800, 800)
		}()
	}
	start.Done()
	done := make(chan struct{})
	go func() {
		callersDone.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for concurrent reservations")
	}
	close(errorsByCaller)
	for err := range errorsByCaller {
		if err != nil {
			t.Fatalf("concurrent ReserveTokens() error = %v", err)
		}
	}
	charged := getQuota(t, s, "team-a", "default")
	if charged.Status.TokenUsage == nil || charged.Status.TokenUsage.InputTokens != 800 || charged.Status.TokenUsage.OutputTokens != 800 {
		t.Fatalf("usage after concurrent reservations = %+v, want 800/800", charged.Status.TokenUsage)
	}
}

func TestQuotaEnforcer_SettleChargesUsageAboveReservation(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()
	createQuota(t, s, v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "default"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 2000},
		},
	})

	if err := enforcer.ReserveTokens(ctx, "team-a", "session-1", 100, 100); err != nil {
		t.Fatalf("ReserveTokens() error = %v", err)
	}
	if err := enforcer.Settle(ctx, "session-1", 150, 125); err != nil {
		t.Fatalf("Settle() error = %v", err)
	}
	settled := getQuota(t, s, "team-a", "default")
	if settled.Status.TokenUsage == nil || settled.Status.TokenUsage.InputTokens != 150 || settled.Status.TokenUsage.OutputTokens != 125 {
		t.Fatalf("usage after overage settlement = %+v, want 150/125", settled.Status.TokenUsage)
	}
}

func TestQuotaEnforcer_RejectsNegativeTokenAccounting(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()
	createQuota(t, s, v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "default"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 2000},
		},
	})

	if err := enforcer.ReserveTokens(ctx, "team-a", "invalid", -1, 0); err == nil {
		t.Fatal("ReserveTokens() with negative ceiling error = nil")
	}
	if err := enforcer.ReserveTokens(ctx, "team-a", "session-1", 800, 800); err != nil {
		t.Fatalf("ReserveTokens() error = %v", err)
	}
	if err := enforcer.Settle(ctx, "session-1", -1, 200); err == nil {
		t.Fatal("Settle() with negative usage error = nil")
	}
	charged := getQuota(t, s, "team-a", "default")
	if charged.Status.TokenUsage == nil || charged.Status.TokenUsage.InputTokens != 800 || charged.Status.TokenUsage.OutputTokens != 800 {
		t.Fatalf("usage after invalid accounting = %+v, want unchanged 800/800", charged.Status.TokenUsage)
	}
	if err := enforcer.Settle(ctx, "session-1", 300, 200); err != nil {
		t.Fatalf("valid settlement after rejection error = %v", err)
	}
}

// TC-0092 variant: Reserve fails when quota would be exceeded.
func TestQuotaEnforcer_Reserve_ExceedsQuota(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()

	createQuota(t, s, v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "default"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			TokenBudget: &v1alpha1.TokenBudget{
				MaxInputTokens:  500,
				MaxOutputTokens: 500,
				MaxTotalTokens:  1000,
			},
		},
		Status: v1alpha1.CognitiveQuotaStatus{Used: make(map[string]int64)},
	})

	// Reservation for 600 input exceeds 500 limit.
	err := enforcer.ReserveTokens(ctx, "team-a", "session-1", 600, 400)
	if err == nil {
		t.Fatal("expected quota exhausted error")
	}
	if !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("expected ErrQuotaExhausted, got: %v", err)
	}
}

// CheckCallBudget rejects a call that would exceed per-agent limit (T-0115).
func TestQuotaEnforcer_CheckCallBudget_Exceeded(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()

	createLimitRange(t, s, "team-a", "default", 1000)

	// Already used 900, call wants 200 → exceeds 1000.
	err := enforcer.CheckCallBudget(ctx, "team-a", "agent-1", 900, 200)
	if err == nil {
		t.Fatal("expected budget exceeded error")
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got: %v", err)
	}
}

// CheckCallBudget passes when within limits.
func TestQuotaEnforcer_CheckCallBudget_Passes(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()

	createLimitRange(t, s, "team-a", "default", 1000)

	// Create workspace quota so token check passes.
	createQuota(t, s, v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "default"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 10000},
		},
		Status: v1alpha1.CognitiveQuotaStatus{Used: make(map[string]int64)},
	})

	// Used 500, call wants 100 → under 1000 limit.
	if err := enforcer.CheckCallBudget(ctx, "team-a", "agent-1", 500, 100); err != nil {
		t.Fatalf("expected pass, got: %v", err)
	}
}

// Settle with no reservation is a no-op.
func TestQuotaEnforcer_Settle_NoReservation(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewCognitiveQuotaReconciler(s, logger)
	enforcer := NewQuotaEnforcer(s, reconciler, logger)
	ctx := context.Background()

	if err := enforcer.Settle(ctx, "nonexistent", 100, 100); err != nil {
		t.Fatalf("settle with no reservation should be no-op, got: %v", err)
	}
}
