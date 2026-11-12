// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// Integration tests exercise the full CognitiveQuota reconciler lifecycle
// using the in-memory store: quota creation → resource creation → reconcile →
// enforcement → token/cost accumulation → budget enforcement → reset cycle.

// TC-0087: Full quota lifecycle - allocation, consumption, exceeded, reset.
func TestIntegration_CognitiveQuota_FullLifecycle(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
	ctx := context.Background()
	ns := "team-alpha"

	// --- Phase 1: Create quota with hard limits and token budget ---
	quota := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: "team-quota"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			Hard:          map[string]int64{"agents": 5, "tools": 10, "sessions": 20},
			TokenBudget:   &v1alpha1.TokenBudget{MaxInputTokens: 1000, MaxOutputTokens: 500, MaxTotalTokens: 1500},
			CostBudget:    &v1alpha1.CostBudget{MaxCostPerPeriod: 10000, MaxToolCallsPerPeriod: 50},
			ResetInterval: "24h",
		},
	}
	createQuota(t, s, quota)

	// --- Phase 2: CheckQuota should pass when no resources exist ---
	if err := r.CheckQuota(ctx, ns, "agents", 1); err != nil {
		t.Fatalf("Phase 2: expected CheckQuota to pass on empty namespace: %v", err)
	}

	// --- Phase 3: Create some resources ---
	for _, name := range []string{"agent-1", "agent-2", "agent-3"} {
		data, _ := json.Marshal(map[string]interface{}{
			"kind":     "Agent",
			"metadata": map[string]string{"name": name, "namespace": ns},
		})
		if err := s.Create(ctx, "Agent", ns, name, data); err != nil {
			t.Fatalf("Phase 3: creating agent %s: %v", name, err)
		}
	}
	for _, name := range []string{"tool-1", "tool-2"} {
		data, _ := json.Marshal(map[string]interface{}{
			"kind":     "Tool",
			"metadata": map[string]string{"name": name, "namespace": ns},
		})
		if err := s.Create(ctx, "Tool", ns, name, data); err != nil {
			t.Fatalf("Phase 3: creating tool %s: %v", name, err)
		}
	}

	// --- Phase 4: Reconcile should detect resource counts ---
	result, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "team-quota", Kind: "CognitiveQuota"})
	if err != nil {
		t.Fatalf("Phase 4: Reconcile error: %v", err)
	}
	if result.Requeue {
		t.Fatal("Phase 4: unexpected requeue")
	}

	got := getQuota(t, s, ns, "team-quota")
	if got.Status.Used["agents"] != 3 {
		t.Errorf("Phase 4: expected agents=3, got %d", got.Status.Used["agents"])
	}
	if got.Status.Used["tools"] != 2 {
		t.Errorf("Phase 4: expected tools=2, got %d", got.Status.Used["tools"])
	}
	if got.Status.Used["sessions"] != 0 {
		t.Errorf("Phase 4: expected sessions=0, got %d", got.Status.Used["sessions"])
	}

	// --- Phase 5: CheckQuota should enforce after reconcile ---
	// agents: 3 used, limit 5 → adding 2 should be ok
	if err := r.CheckQuota(ctx, ns, "agents", 2); err != nil {
		t.Fatalf("Phase 5: expected adding 2 agents to pass: %v", err)
	}
	// agents: 3 used, limit 5 → adding 3 should fail
	if err := r.CheckQuota(ctx, ns, "agents", 3); err == nil {
		t.Fatal("Phase 5: expected adding 3 agents to be denied")
	}

	// --- Phase 6: Token budget check before any usage ---
	if err := r.CheckTokenQuota(ctx, ns, 500, 200); err != nil {
		t.Fatalf("Phase 6: expected token check to pass with no prior usage: %v", err)
	}

	// --- Phase 7: Increment token usage ---
	if err := r.IncrementTokenUsage(ctx, ns, 400, 200); err != nil {
		t.Fatalf("Phase 7: IncrementTokenUsage: %v", err)
	}

	got = getQuota(t, s, ns, "team-quota")
	if got.Status.TokenUsage == nil {
		t.Fatal("Phase 7: expected TokenUsage to be non-nil")
	}
	if got.Status.TokenUsage.InputTokens != 400 {
		t.Errorf("Phase 7: expected input=400, got %d", got.Status.TokenUsage.InputTokens)
	}
	if got.Status.TokenUsage.OutputTokens != 200 {
		t.Errorf("Phase 7: expected output=200, got %d", got.Status.TokenUsage.OutputTokens)
	}
	if got.Status.TokenUsage.TotalTokens != 600 {
		t.Errorf("Phase 7: expected total=600, got %d", got.Status.TokenUsage.TotalTokens)
	}

	// --- Phase 8: Token budget enforcement after usage ---
	// Input: 400 used, limit 1000 → adding 700 exceeds (400+700=1100 > 1000)
	if err := r.CheckTokenQuota(ctx, ns, 700, 0); err == nil {
		t.Fatal("Phase 8: expected input token quota exceeded")
	}
	// Output: 200 used, limit 500 → adding 400 exceeds (200+400=600 > 500)
	if err := r.CheckTokenQuota(ctx, ns, 0, 400); err == nil {
		t.Fatal("Phase 8: expected output token quota exceeded")
	}
	// Total: 600 used, limit 1500 → adding 500+500=1000 would be 1600 > 1500
	if err := r.CheckTokenQuota(ctx, ns, 500, 500); err == nil {
		t.Fatal("Phase 8: expected total token quota exceeded")
	}
	// Smaller request should pass: 200+100=300, total goes to 900 < 1500
	if err := r.CheckTokenQuota(ctx, ns, 200, 100); err != nil {
		t.Fatalf("Phase 8: expected smaller token request to pass: %v", err)
	}

	// --- Phase 9: Cost budget ---
	if err := r.IncrementCostUsage(ctx, ns, 5000, 20); err != nil {
		t.Fatalf("Phase 9: IncrementCostUsage: %v", err)
	}
	// Cost: 5000 used, limit 10000 → adding 6000 exceeds
	if err := r.CheckCostQuota(ctx, ns, 6000, 0); err == nil {
		t.Fatal("Phase 9: expected cost quota exceeded")
	}
	// Tool calls: 20 used, limit 50 → adding 31 exceeds
	if err := r.CheckCostQuota(ctx, ns, 0, 31); err == nil {
		t.Fatal("Phase 9: expected tool calls quota exceeded")
	}
	// Within limits
	if err := r.CheckCostQuota(ctx, ns, 4000, 25); err != nil {
		t.Fatalf("Phase 9: expected within-limit cost check to pass: %v", err)
	}

	// --- Phase 10: Accumulation (second increment) ---
	if err := r.IncrementTokenUsage(ctx, ns, 300, 100); err != nil {
		t.Fatalf("Phase 10: second IncrementTokenUsage: %v", err)
	}
	got = getQuota(t, s, ns, "team-quota")
	if got.Status.TokenUsage.InputTokens != 700 {
		t.Errorf("Phase 10: expected input=700, got %d", got.Status.TokenUsage.InputTokens)
	}
	if got.Status.TokenUsage.TotalTokens != 1000 {
		t.Errorf("Phase 10: expected total=1000, got %d", got.Status.TokenUsage.TotalTokens)
	}

	// --- Phase 11: Reconcile preserves token/cost usage ---
	// Add another agent then reconcile - token/cost should be preserved.
	agentData, _ := json.Marshal(map[string]interface{}{
		"kind":     "Agent",
		"metadata": map[string]string{"name": "agent-4", "namespace": ns},
	})
	s.Create(ctx, "Agent", ns, "agent-4", agentData)

	_, err = r.Reconcile(ctx, Request{Namespace: ns, Name: "team-quota", Kind: "CognitiveQuota"})
	if err != nil {
		t.Fatalf("Phase 11: Reconcile error: %v", err)
	}
	got = getQuota(t, s, ns, "team-quota")
	if got.Status.Used["agents"] != 4 {
		t.Errorf("Phase 11: expected agents=4, got %d", got.Status.Used["agents"])
	}
	// Token/cost usage preserved through reconcile (no reset interval elapsed).
	if got.Status.TokenUsage == nil || got.Status.TokenUsage.TotalTokens != 1000 {
		t.Errorf("Phase 11: expected token usage preserved at 1000, got %+v", got.Status.TokenUsage)
	}
}

func TestIntegration_CognitiveQuota_ResetCycle(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
	ctx := context.Background()
	ns := "team-beta"

	// Create quota with a short reset interval and pre-existing usage.
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	quota := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: "reset-quota"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			Hard:          map[string]int64{"agents": 10},
			TokenBudget:   &v1alpha1.TokenBudget{MaxTotalTokens: 5000},
			CostBudget:    &v1alpha1.CostBudget{MaxCostPerPeriod: 10000},
			ResetInterval: "1h",
		},
		Status: v1alpha1.CognitiveQuotaStatus{
			Used:          map[string]int64{"agents": 3},
			LastResetTime: past,
			TokenUsage:    &v1alpha1.QuotaTokenUsage{InputTokens: 2000, OutputTokens: 1000, TotalTokens: 3000},
			CostUsage:     &v1alpha1.QuotaCostUsage{TotalCost: 8000, ToolCalls: 40},
		},
	}
	createQuota(t, s, quota)

	// Before reconcile: token quota nearly exhausted.
	if err := r.CheckTokenQuota(ctx, ns, 1500, 1000); err == nil {
		t.Fatal("expected token quota exceeded before reset")
	}
	if err := r.CheckCostQuota(ctx, ns, 3000, 0); err == nil {
		t.Fatal("expected cost quota exceeded before reset")
	}

	// Reconcile triggers reset (interval elapsed).
	_, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "reset-quota", Kind: "CognitiveQuota"})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	got := getQuota(t, s, ns, "reset-quota")
	// Token and cost usage should be cleared.
	if got.Status.TokenUsage != nil {
		t.Fatalf("expected TokenUsage nil after reset, got %+v", got.Status.TokenUsage)
	}
	if got.Status.CostUsage != nil {
		t.Fatalf("expected CostUsage nil after reset, got %+v", got.Status.CostUsage)
	}
	// LastResetTime updated.
	if got.Status.LastResetTime == past {
		t.Fatal("expected LastResetTime to be updated after reset")
	}
	// Hard resource counts recalculated (no agents in store → 0, not preserved from status).
	if got.Status.Used["agents"] != 0 {
		t.Errorf("expected agents=0 after reconcile (no agents in store), got %d", got.Status.Used["agents"])
	}

	// After reset: same requests should now pass.
	if err := r.CheckTokenQuota(ctx, ns, 1500, 1000); err != nil {
		t.Fatalf("expected token quota to pass after reset: %v", err)
	}
	if err := r.CheckCostQuota(ctx, ns, 3000, 0); err != nil {
		t.Fatalf("expected cost quota to pass after reset: %v", err)
	}

	// Accumulate again after reset.
	if err := r.IncrementTokenUsage(ctx, ns, 1000, 500); err != nil {
		t.Fatalf("IncrementTokenUsage after reset: %v", err)
	}
	got = getQuota(t, s, ns, "reset-quota")
	if got.Status.TokenUsage.TotalTokens != 1500 {
		t.Errorf("expected total tokens 1500 after fresh increment, got %d", got.Status.TokenUsage.TotalTokens)
	}
}

func TestIntegration_CognitiveQuota_MultiNamespaceIsolation(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
	ctx := context.Background()

	// Quota in ns-a.
	qa := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns-a", Name: "qa"},
		Spec:       v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": 2}},
	}
	// Quota in ns-b with different limit.
	qb := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns-b", Name: "qb"},
		Spec:       v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": 5}},
	}
	createQuota(t, s, qa)
	createQuota(t, s, qb)

	// Create agents in ns-a.
	for _, name := range []string{"a1", "a2"} {
		data, _ := json.Marshal(map[string]interface{}{
			"kind":     "Agent",
			"metadata": map[string]string{"name": name, "namespace": "ns-a"},
		})
		s.Create(ctx, "Agent", "ns-a", name, data)
	}

	// Reconcile ns-a.
	r.Reconcile(ctx, Request{Namespace: "ns-a", Name: "qa", Kind: "CognitiveQuota"})

	// ns-a is at limit.
	if err := r.CheckQuota(ctx, "ns-a", "agents", 1); err == nil {
		t.Fatal("expected ns-a quota exceeded")
	}

	// ns-b should be unaffected.
	if err := r.CheckQuota(ctx, "ns-b", "agents", 1); err != nil {
		t.Fatalf("expected ns-b to allow agents: %v", err)
	}

	// Reconcile ns-b should show 0 agents (none created there).
	r.Reconcile(ctx, Request{Namespace: "ns-b", Name: "qb", Kind: "CognitiveQuota"})
	got := getQuota(t, s, "ns-b", "qb")
	if got.Status.Used["agents"] != 0 {
		t.Errorf("expected ns-b agents=0, got %d", got.Status.Used["agents"])
	}
}

func TestIntegration_CognitiveQuota_ConcurrentIncrements(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
	ctx := context.Background()
	ns := "ns-concurrent"

	quota := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: "q1"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			Hard:        map[string]int64{"agents": 100},
			TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 100000},
		},
	}
	createQuota(t, s, quota)

	// Sequential increments should accumulate correctly.
	for i := 0; i < 10; i++ {
		if err := r.IncrementTokenUsage(ctx, ns, 100, 50); err != nil {
			t.Fatalf("increment %d: %v", i, err)
		}
	}

	got := getQuota(t, s, ns, "q1")
	if got.Status.TokenUsage.InputTokens != 1000 {
		t.Errorf("expected input tokens 1000, got %d", got.Status.TokenUsage.InputTokens)
	}
	if got.Status.TokenUsage.OutputTokens != 500 {
		t.Errorf("expected output tokens 500, got %d", got.Status.TokenUsage.OutputTokens)
	}
	if got.Status.TokenUsage.TotalTokens != 1500 {
		t.Errorf("expected total tokens 1500, got %d", got.Status.TokenUsage.TotalTokens)
	}
}

func TestIntegration_CognitiveQuota_NoResetWithoutInterval(t *testing.T) {
	s := newMemStore()
	r := NewCognitiveQuotaReconciler(s, zaptest.NewLogger(t))
	ctx := context.Background()
	ns := "ns-noreset"

	// Quota without ResetInterval — usage should never be reset.
	quota := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: "q1"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			Hard:        map[string]int64{"agents": 10},
			TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 5000},
		},
		Status: v1alpha1.CognitiveQuotaStatus{
			TokenUsage: &v1alpha1.QuotaTokenUsage{InputTokens: 2000, OutputTokens: 1000, TotalTokens: 3000},
		},
	}
	createQuota(t, s, quota)

	// Reconcile — no reset should happen.
	_, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "q1", Kind: "CognitiveQuota"})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	got := getQuota(t, s, ns, "q1")
	if got.Status.TokenUsage == nil {
		t.Fatal("expected TokenUsage to be preserved (no reset)")
	}
	if got.Status.TokenUsage.TotalTokens != 3000 {
		t.Errorf("expected total tokens preserved at 3000, got %d", got.Status.TokenUsage.TotalTokens)
	}
}
