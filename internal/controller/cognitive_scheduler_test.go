// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
	"testing"

	"go.uber.org/zap/zaptest"
)

func runtimeReplicas(agentName string, names ...string) []RuntimeReplica {
	replicas := make([]RuntimeReplica, 0, len(names))
	for _, name := range names {
		replicas = append(replicas, RuntimeReplica{Name: name, AgentName: agentName, Ready: true})
	}
	return replicas
}

func TestRoundRobinScheduler_SelectsOnlyReadySameAgentReplicas(t *testing.T) {
	sched := NewRoundRobinScheduler(nil, zaptest.NewLogger(t), NewSchedulerRegistry())
	req := ScheduleRequest{
		Namespace: "default",
		AgentName: "agent-a",
		Replicas: []RuntimeReplica{
			{Name: "agent-b-ready", AgentName: "agent-b", Ready: true},
			{Name: "agent-a-not-ready", AgentName: "agent-a", Ready: false},
			{Name: "agent-a-ready-2", AgentName: "agent-a", Ready: true},
			{Name: "agent-a-ready-1", AgentName: "agent-a", Ready: true},
		},
	}

	var got []string
	for i := 0; i < 4; i++ {
		result, err := sched.Schedule(context.Background(), req)
		if err != nil {
			t.Fatalf("Schedule() error = %v", err)
		}
		if result.AgentName != req.AgentName {
			t.Fatalf("result AgentName = %q, want immutable %q", result.AgentName, req.AgentName)
		}
		got = append(got, result.PreferredReplica)
	}

	want := []string{"agent-a-ready-1", "agent-a-ready-2", "agent-a-ready-1", "agent-a-ready-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("placements = %v, want deterministic same-Agent round robin %v", got, want)
	}
}

func TestRoundRobinScheduler_NoEligibleReplicaRequestsServiceFallback(t *testing.T) {
	sched := NewRoundRobinScheduler(nil, zaptest.NewLogger(t), NewSchedulerRegistry())
	result, err := sched.Schedule(context.Background(), ScheduleRequest{
		Namespace: "default",
		AgentName: "agent-a",
		Replicas: []RuntimeReplica{
			{Name: "other-ready", AgentName: "agent-b", Ready: true},
			{Name: "same-not-ready", AgentName: "agent-a", Ready: false},
		},
	})
	if err != nil {
		t.Fatalf("Schedule() error = %v", err)
	}
	if result.Assigned || result.PreferredReplica != "" {
		t.Fatalf("Schedule() = %+v, want no pod preference", result)
	}
	if result.AgentName != "agent-a" {
		t.Fatalf("result AgentName = %q, want agent-a", result.AgentName)
	}
}

// TC-0093: Schedule a cognitive task; verify round-robin distributes across workers.
func TestRoundRobinScheduler_DistributesAcrossWorkers(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	// Create 3 agents as available workers.
	for _, name := range []string{"agent-a", "agent-b", "agent-c"} {
		data := makeResource("Agent", "default", name)
		if err := ms.Create(ctx, "Agent", "default", name, data); err != nil {
			t.Fatalf("create agent %s: %v", name, err)
		}
	}

	registry := NewSchedulerRegistry()
	sched := NewRoundRobinScheduler(ms, logger, registry)

	// Schedule 6 tasks and verify round-robin distribution.
	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		result, err := sched.Schedule(ctx, ScheduleRequest{
			Namespace: "default",
			AgentName: "test-agent",
			SessionID: "session-1",
			Priority:  0,
			Replicas:  runtimeReplicas("test-agent", "agent-a", "agent-b", "agent-c"),
		})
		if err != nil {
			t.Fatalf("schedule #%d: %v", i, err)
		}
		if !result.Assigned {
			t.Fatalf("schedule #%d: expected assignment, got: %s", i, result.Reason)
		}
		counts[result.Worker]++
	}

	// Each worker should get exactly 2 tasks.
	for _, name := range []string{"agent-a", "agent-b", "agent-c"} {
		if counts[name] != 2 {
			t.Errorf("expected worker %s to get 2 tasks, got %d", name, counts[name])
		}
	}
}

// TC-0093: Schedule with no workers available; verify graceful rejection.
func TestRoundRobinScheduler_NoWorkers(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	registry := NewSchedulerRegistry()
	sched := NewRoundRobinScheduler(ms, logger, registry)

	result, err := sched.Schedule(ctx, ScheduleRequest{
		Namespace: "empty-ns",
		AgentName: "test-agent",
		SessionID: "session-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Assigned {
		t.Fatal("expected no assignment when no workers available")
	}
	if result.Reason == "" {
		t.Fatal("expected a reason for non-assignment")
	}
}

// TC-0094: Register a scheduler plugin; verify plugin scoring overrides round-robin.
func TestRoundRobinScheduler_PluginScoring(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	// Create workers.
	for _, name := range []string{"worker-low", "worker-high"} {
		data := makeResource("Agent", "default", name)
		if err := ms.Create(ctx, "Agent", "default", name, data); err != nil {
			t.Fatalf("create agent %s: %v", name, err)
		}
	}

	registry := NewSchedulerRegistry()
	// Register a plugin that always scores "worker-high" higher.
	registry.Register(&fixedScorePlugin{
		name: "prefer-high",
		scores: map[string]int{
			"worker-high": 100,
			"worker-low":  10,
		},
	})

	sched := NewRoundRobinScheduler(ms, logger, registry)

	// All tasks should go to worker-high.
	for i := 0; i < 5; i++ {
		result, err := sched.Schedule(ctx, ScheduleRequest{
			Namespace: "default",
			AgentName: "test-agent",
			SessionID: "session-1",
			Replicas:  runtimeReplicas("test-agent", "worker-low", "worker-high"),
		})
		if err != nil {
			t.Fatalf("schedule #%d: %v", i, err)
		}
		if result.Worker != "worker-high" {
			t.Errorf("schedule #%d: expected worker-high, got %s", i, result.Worker)
		}
	}
}

// TC-0094: Plugin that returns an error should be skipped gracefully.
func TestRoundRobinScheduler_PluginError(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	for _, name := range []string{"worker-a", "worker-b"} {
		data := makeResource("Agent", "default", name)
		if err := ms.Create(ctx, "Agent", "default", name, data); err != nil {
			t.Fatalf("create agent %s: %v", name, err)
		}
	}

	registry := NewSchedulerRegistry()
	registry.Register(&errorPlugin{name: "broken"})
	// Also register a working plugin.
	registry.Register(&fixedScorePlugin{
		name:   "fallback",
		scores: map[string]int{"worker-a": 50, "worker-b": 10},
	})

	sched := NewRoundRobinScheduler(ms, logger, registry)

	result, err := sched.Schedule(ctx, ScheduleRequest{
		Namespace: "default",
		AgentName: "test-agent",
		SessionID: "session-1",
		Replicas:  runtimeReplicas("test-agent", "worker-a", "worker-b"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Worker != "worker-a" {
		t.Errorf("expected worker-a (highest fallback score), got %s", result.Worker)
	}
}

func TestSchedulerRegistry_RegisterAndList(t *testing.T) {
	registry := NewSchedulerRegistry()

	if len(registry.List()) != 0 {
		t.Fatal("expected empty registry")
	}

	registry.Register(&fixedScorePlugin{name: "a"})
	registry.Register(&fixedScorePlugin{name: "b"})

	plugins := registry.List()
	if len(plugins) != 2 {
		t.Fatalf("expected 2 plugins, got %d", len(plugins))
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type fixedScorePlugin struct {
	name   string
	scores map[string]int
}

func (p *fixedScorePlugin) Name() string { return p.name }

func (p *fixedScorePlugin) Score(_ context.Context, _ ScheduleRequest, workers []string) (map[string]int, error) {
	result := make(map[string]int)
	for _, w := range workers {
		if s, ok := p.scores[w]; ok {
			result[w] = s
		}
	}
	return result, nil
}

type errorPlugin struct {
	name string
}

func (p *errorPlugin) Name() string { return p.name }

func (p *errorPlugin) Score(_ context.Context, _ ScheduleRequest, _ []string) (map[string]int, error) {
	return nil, context.DeadlineExceeded
}
