// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// seedAgent registers an Agent so it appears as an available worker.
func seedAgent(t *testing.T, ms *memStore, ns, name, modelRef string) {
	t.Helper()
	agent := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: modelRef}},
	}
	data, _ := json.Marshal(agent)
	if err := ms.Create(context.Background(), "Agent", ns, name, data); err != nil {
		t.Fatalf("seed agent %s: %v", name, err)
	}
}

// seedModelChain seeds the Agent → Model → ModelClass chain for a worker.
func seedModelChain(t *testing.T, ms *memStore, ns, agent, model, class, costTier string, contextWindow int64) {
	t.Helper()
	seedAgent(t, ms, ns, agent, model)

	m := v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: model},
		Spec:       v1alpha1.ModelSpec{ClassName: class, ModelID: "test-model"},
	}
	mData, _ := json.Marshal(m)
	if err := ms.Create(context.Background(), "Model", ns, model, mData); err != nil {
		t.Fatalf("seed model %s: %v", model, err)
	}

	mc := v1alpha1.ModelClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ModelClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: class},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:   "openai-compatible",
			CostTier:      costTier,
			ContextWindow: contextWindow,
		},
	}
	mcData, _ := json.Marshal(mc)
	if err := ms.Create(context.Background(), "ModelClass", "", class, mcData); err != nil {
		t.Fatalf("seed model class %s: %v", class, err)
	}
}

// --- SessionLocalityPlugin ---------------------------------------------------

func TestSessionLocality_NoAssociationScoresZero(t *testing.T) {
	p := NewSessionLocalityPlugin(SessionLocalityConfig{})
	scores, err := p.Score(context.Background(),
		ScheduleRequest{Namespace: "default", SessionID: "s1"},
		[]string{"w1", "w2"})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if len(scores) != 0 {
		t.Fatalf("expected no scores before any assignment, got %v", scores)
	}
}

func TestSessionLocality_StickyAfterAssignment(t *testing.T) {
	p := NewSessionLocalityPlugin(SessionLocalityConfig{Bonus: 100})
	req := ScheduleRequest{Namespace: "default", SessionID: "s1"}
	p.OnAssigned(req, "w2")

	scores, err := p.Score(context.Background(), req, []string{"w1", "w2", "w3"})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if scores["w2"] != 100 {
		t.Fatalf("expected sticky bonus 100 on w2, got %v", scores)
	}
	if scores["w1"] != 0 || scores["w3"] != 0 {
		t.Fatalf("expected only w2 scored, got %v", scores)
	}
}

func TestSessionLocality_StickyWorkerGoneScoresZero(t *testing.T) {
	p := NewSessionLocalityPlugin(SessionLocalityConfig{})
	req := ScheduleRequest{Namespace: "default", SessionID: "s1"}
	p.OnAssigned(req, "w2")

	// w2 no longer among candidates.
	scores, _ := p.Score(context.Background(), req, []string{"w1", "w3"})
	if len(scores) != 0 {
		t.Fatalf("expected no bonus when sticky worker absent, got %v", scores)
	}
}

func TestSessionLocality_EmptySessionScoresZero(t *testing.T) {
	p := NewSessionLocalityPlugin(SessionLocalityConfig{})
	p.OnAssigned(ScheduleRequest{SessionID: "s1"}, "w1")
	scores, _ := p.Score(context.Background(),
		ScheduleRequest{Namespace: "default", SessionID: ""},
		[]string{"w1"})
	if len(scores) != 0 {
		t.Fatalf("expected no scores for empty session, got %v", scores)
	}
}

func TestSessionLocality_BoundedEviction(t *testing.T) {
	p := NewSessionLocalityPlugin(SessionLocalityConfig{MaxSessions: 2})
	p.OnAssigned(ScheduleRequest{SessionID: "s1"}, "w1")
	p.OnAssigned(ScheduleRequest{SessionID: "s2"}, "w2")
	p.OnAssigned(ScheduleRequest{SessionID: "s3"}, "w3") // evicts s1

	if s, _ := p.Score(context.Background(), ScheduleRequest{SessionID: "s1"}, []string{"w1"}); len(s) != 0 {
		t.Fatalf("expected s1 evicted, got %v", s)
	}
	if s, _ := p.Score(context.Background(), ScheduleRequest{SessionID: "s3"}, []string{"w3"}); s["w3"] == 0 {
		t.Fatalf("expected s3 retained, got %v", s)
	}
}

func TestSessionLocality_ReassignmentUpdatesWorker(t *testing.T) {
	p := NewSessionLocalityPlugin(SessionLocalityConfig{Bonus: 50})
	req := ScheduleRequest{SessionID: "s1"}
	p.OnAssigned(req, "w1")
	p.OnAssigned(req, "w2") // session moved to w2

	scores, _ := p.Score(context.Background(), req, []string{"w1", "w2"})
	if scores["w2"] != 50 || scores["w1"] != 0 {
		t.Fatalf("expected stickiness to follow latest worker w2, got %v", scores)
	}
}

func TestSessionLocality_SchedulerRoutesSessionBack(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	// Pre-associate the session with replica-c, which is NOT the worker the
	// default round-robin would pick first. Only the locality bonus can make
	// the scheduler choose it, so this test fails if stickiness is a no-op.
	plugin := NewSessionLocalityPlugin(SessionLocalityConfig{})
	plugin.OnAssigned(ScheduleRequest{SessionID: "sticky"}, "replica-c")

	registry := NewSchedulerRegistry()
	registry.Register(plugin)
	sched := NewRoundRobinScheduler(ms, logger, registry)

	for i := 0; i < 5; i++ {
		got, err := sched.Schedule(ctx, ScheduleRequest{
			Namespace: "default",
			AgentName: "agent-a",
			SessionID: "sticky",
			Replicas:  runtimeReplicas("agent-a", "replica-a", "replica-b", "replica-c"),
		})
		if err != nil {
			t.Fatalf("schedule #%d: %v", i, err)
		}
		if got.Worker != "replica-c" {
			t.Fatalf("expected session pinned to replica-c, got %s", got.Worker)
		}
	}
}

// --- CostAwarePlugin ---------------------------------------------------------

func TestCostAware_AppliesRequestedAgentTierToReplicas(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	seedModelChain(t, ms, "default", "agent-a", "m-free", "c-free", "free", 8192)

	p := NewCostAwarePlugin(ms, nil)
	scores, err := p.Score(ctx, ScheduleRequest{Namespace: "default", AgentName: "agent-a"}, []string{"replica-a", "replica-b"})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if scores["replica-a"] != 100 || scores["replica-b"] != 100 {
		t.Fatalf("expected requested Agent tier applied uniformly, got %v", scores)
	}
}

func TestCostAware_UnresolvableRequestedAgentIsUnscored(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()

	p := NewCostAwarePlugin(ms, nil)
	scores, err := p.Score(ctx, ScheduleRequest{Namespace: "default", AgentName: "missing-agent"}, []string{"replica-a"})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if len(scores) != 0 {
		t.Fatalf("expected unresolvable requested Agent to be unscored, got %v", scores)
	}
}

func TestCostAware_SchedulerPreservesAgentIdentity(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	seedModelChain(t, ms, "default", "agent-a", "m-free", "c-free", "free", 8192)

	registry := NewSchedulerRegistry()
	registry.Register(NewCostAwarePlugin(ms, nil))
	sched := NewRoundRobinScheduler(ms, logger, registry)

	want := []string{"replica-a", "replica-b", "replica-a", "replica-b"}
	for i := 0; i < 4; i++ {
		got, err := sched.Schedule(ctx, ScheduleRequest{
			Namespace: "default",
			AgentName: "agent-a",
			Replicas:  runtimeReplicas("agent-a", "replica-b", "replica-a"),
		})
		if err != nil {
			t.Fatalf("schedule #%d: %v", i, err)
		}
		if got.AgentName != "agent-a" || got.Worker != want[i] {
			t.Fatalf("schedule #%d: got %+v, want Agent agent-a replica %s", i, got, want[i])
		}
	}
}

// --- ContextCapacityPlugin ---------------------------------------------------

func TestContextCapacity_AppliesRequestedAgentWindowToReplicas(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	seedModelChain(t, ms, "default", "agent-a", "m-large", "c-large", "standard", 200000)

	p := NewContextCapacityPlugin(ms, ContextCapacityConfig{})
	scores, err := p.Score(ctx, ScheduleRequest{Namespace: "default", AgentName: "agent-a"}, []string{"replica-a", "replica-b"})
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if scores["replica-a"] != 97 || scores["replica-b"] != 97 {
		t.Fatalf("expected requested Agent context score applied uniformly, got %v", scores)
	}
}

func TestContextCapacity_ScoreIsCapped(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	seedModelChain(t, ms, "default", "agent-huge", "m-huge", "c-huge", "standard", 100_000_000)

	p := NewContextCapacityPlugin(ms, ContextCapacityConfig{Divisor: 2048, Cap: 100})
	scores, _ := p.Score(ctx, ScheduleRequest{Namespace: "default", AgentName: "agent-huge"}, []string{"replica-a"})
	if scores["replica-a"] != 100 {
		t.Fatalf("expected score capped at 100, got %v", scores)
	}
}

func TestContextCapacity_ZeroWindowUnscored(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	seedModelChain(t, ms, "default", "agent-nowin", "m-nowin", "c-nowin", "standard", 0)

	p := NewContextCapacityPlugin(ms, ContextCapacityConfig{})
	scores, _ := p.Score(ctx, ScheduleRequest{Namespace: "default", AgentName: "agent-nowin"}, []string{"replica-a"})
	if _, present := scores["replica-a"]; present {
		t.Fatalf("expected zero-window worker to be unscored, got %v", scores)
	}
}

// --- Fair distribution + score combination ----------------------------------

func TestScheduler_TieBreakDistributesFairly(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	// All workers score equally: the scheduler must spread load round-robin
	// among the tied top workers rather than always picking the first.
	registry := NewSchedulerRegistry()
	registry.Register(&fixedScorePlugin{name: "flat", scores: map[string]int{
		"replica-a": 5, "replica-b": 5, "replica-c": 5,
	}})
	sched := NewRoundRobinScheduler(ms, logger, registry)

	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		got, err := sched.Schedule(ctx, ScheduleRequest{
			Namespace: "default",
			AgentName: "agent-a",
			Replicas:  runtimeReplicas("agent-a", "replica-b", "replica-a", "replica-c"),
		})
		if err != nil {
			t.Fatalf("schedule #%d: %v", i, err)
		}
		counts[got.Worker]++
	}
	for _, name := range []string{"replica-a", "replica-b", "replica-c"} {
		if counts[name] != 3 {
			t.Fatalf("expected fair distribution (3 each), got %v", counts)
		}
	}
}

func TestScheduler_CombinesPluginScores(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	// Two plugins disagree; the summed score must decide. replica-b wins 30+5
	// vs replica-a 10+20 = 35 vs 30.
	registry := NewSchedulerRegistry()
	registry.Register(&fixedScorePlugin{name: "p1", scores: map[string]int{"replica-a": 10, "replica-b": 30}})
	registry.Register(&fixedScorePlugin{name: "p2", scores: map[string]int{"replica-a": 20, "replica-b": 5}})
	sched := NewRoundRobinScheduler(ms, logger, registry)

	got, err := sched.Schedule(ctx, ScheduleRequest{
		Namespace: "default",
		AgentName: "agent-a",
		Replicas:  runtimeReplicas("agent-a", "replica-a", "replica-b"),
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if got.AgentName != "agent-a" || got.Worker != "replica-b" {
		t.Fatalf("expected Agent agent-a on replica-b (higher combined score), got %+v", got)
	}
}

func TestSessionLocality_ConcurrentAccess(t *testing.T) {
	p := NewSessionLocalityPlugin(SessionLocalityConfig{MaxSessions: 64})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			req := ScheduleRequest{SessionID: fmt.Sprintf("s%d", n%8)}
			p.OnAssigned(req, fmt.Sprintf("w%d", n%4))
			_, _ = p.Score(context.Background(), req, []string{"w0", "w1", "w2", "w3"})
		}(i)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for concurrent locality updates")
	}
}
