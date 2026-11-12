// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/store"
)

// RuntimeReplica is an invocation target belonging to one immutable Agent.
type RuntimeReplica struct {
	Name      string
	AgentName string
	Ready     bool
}

// ScheduleRequest represents a request to schedule a cognitive task.
type ScheduleRequest struct {
	Namespace string
	AgentName string
	SessionID string
	Priority  int
	Replicas  []RuntimeReplica
}

// ScheduleResult represents the outcome of a scheduling decision.
type ScheduleResult struct {
	Assigned         bool
	AgentName        string
	PreferredReplica string
	// Worker is retained as a compatibility alias for PreferredReplica.
	Worker string
	Reason string
}

// CognitiveScheduler decides how cognitive tasks are assigned to workers.
type CognitiveScheduler interface {
	Schedule(ctx context.Context, req ScheduleRequest) (ScheduleResult, error)
}

// SchedulerPlugin represents a pluggable scheduling strategy.
type SchedulerPlugin interface {
	Name() string
	Score(ctx context.Context, req ScheduleRequest, workers []string) (map[string]int, error)
}

// AssignmentObserver is an optional interface a SchedulerPlugin may implement to
// be notified of the final placement decision. Stateful strategies (e.g. session
// locality) use this to record which worker served a request so future requests
// for the same session can be routed back to it.
type AssignmentObserver interface {
	OnAssigned(req ScheduleRequest, worker string)
}

// SchedulerRegistry manages pluggable scheduling strategies.
type SchedulerRegistry struct {
	mu      sync.RWMutex
	plugins map[string]SchedulerPlugin
}

// NewSchedulerRegistry creates a new SchedulerRegistry.
func NewSchedulerRegistry() *SchedulerRegistry {
	return &SchedulerRegistry{
		plugins: make(map[string]SchedulerPlugin),
	}
}

// Register adds a plugin to the registry.
func (r *SchedulerRegistry) Register(plugin SchedulerPlugin) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plugins[plugin.Name()] = plugin
}

// List returns all registered plugins.
func (r *SchedulerRegistry) List() []SchedulerPlugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	plugins := make([]SchedulerPlugin, 0, len(r.plugins))
	for _, p := range r.plugins {
		plugins = append(plugins, p)
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].Name() < plugins[j].Name() })
	return plugins
}

// NewConfiguredScheduler builds the production scheduler from selected plugin
// names. Unknown names are ignored so placement configuration cannot block the
// existing Agent Service fallback.
func NewConfiguredScheduler(s store.Store, logger *zap.Logger, pluginNames []string) *RoundRobinScheduler {
	registry := NewSchedulerRegistry()
	for _, configuredName := range pluginNames {
		switch name := strings.ToLower(strings.TrimSpace(configuredName)); name {
		case "":
			continue
		case "session-locality":
			registry.Register(NewSessionLocalityPlugin(SessionLocalityConfig{}))
		case "cost-aware":
			registry.Register(NewCostAwarePlugin(s, nil))
		case "context-capacity":
			registry.Register(NewContextCapacityPlugin(s, ContextCapacityConfig{}))
		default:
			logger.Warn("ignoring unknown scheduler plugin", zap.String("plugin", name))
		}
	}
	return NewRoundRobinScheduler(s, logger, registry)
}

// RoundRobinScheduler implements CognitiveScheduler with simple round-robin assignment.
type RoundRobinScheduler struct {
	logger   *zap.Logger
	registry *SchedulerRegistry
	mu       sync.Mutex
	index    int
}

// NewRoundRobinScheduler creates a new RoundRobinScheduler.
func NewRoundRobinScheduler(_ store.Store, logger *zap.Logger, registry *SchedulerRegistry) *RoundRobinScheduler {
	return &RoundRobinScheduler{
		logger:   logger.With(zap.String("component", "cognitive-scheduler")),
		registry: registry,
	}
}

// Schedule prefers the next ready replica belonging to the requested Agent.
func (s *RoundRobinScheduler) Schedule(ctx context.Context, req ScheduleRequest) (ScheduleResult, error) {
	workers := eligibleReplicaNames(req)

	if len(workers) == 0 {
		return ScheduleResult{
			Assigned:  false,
			AgentName: req.AgentName,
			Reason:    "no ready same-agent replicas available; use service fallback",
		}, nil
	}

	// If plugins are registered, use their scores to pick the best worker.
	if plugins := s.registry.List(); len(plugins) > 0 {
		return s.scheduleWithPlugins(ctx, req, workers, plugins)
	}

	// Default round-robin.
	s.mu.Lock()
	worker := workers[s.index%len(workers)]
	s.index++
	s.mu.Unlock()

	s.logger.Debug("scheduled task",
		zap.String("agent", req.AgentName),
		zap.String("worker", worker))

	return ScheduleResult{
		Assigned:         true,
		AgentName:        req.AgentName,
		PreferredReplica: worker,
		Worker:           worker,
	}, nil
}

func (s *RoundRobinScheduler) scheduleWithPlugins(ctx context.Context, req ScheduleRequest, workers []string, plugins []SchedulerPlugin) (ScheduleResult, error) {
	scores := make(map[string]int)
	for _, w := range workers {
		scores[w] = 0
	}

	for _, plugin := range plugins {
		pluginScores, err := plugin.Score(ctx, req, workers)
		if err != nil {
			s.logger.Warn("plugin scoring failed",
				zap.String("plugin", plugin.Name()),
				zap.Error(err))
			continue
		}
		for w, score := range pluginScores {
			scores[w] += score
		}
	}

	// Determine the highest summed score.
	bestScore := math.MinInt
	for _, w := range workers {
		if scores[w] > bestScore {
			bestScore = scores[w]
		}
	}

	// Collect all workers tied at the best score, preserving the sorted input
	// order for determinism.
	var top []string
	for _, w := range workers {
		if scores[w] == bestScore {
			top = append(top, w)
		}
	}
	if len(top) == 0 {
		return ScheduleResult{Assigned: false, Reason: "no suitable worker"}, nil
	}

	// Round-robin among the tied top workers so plugin scoring narrows the
	// candidate set while fairness is preserved when scores are equal (e.g.
	// session-less requests, or unresolved cost/context lookups).
	s.mu.Lock()
	worker := top[s.index%len(top)]
	s.index++
	s.mu.Unlock()

	// Notify stateful plugins of the placement so strategies like session
	// locality can remember it for subsequent requests.
	for _, plugin := range plugins {
		if obs, ok := plugin.(AssignmentObserver); ok {
			obs.OnAssigned(req, worker)
		}
	}

	return ScheduleResult{Assigned: true, AgentName: req.AgentName, PreferredReplica: worker, Worker: worker}, nil
}

func eligibleReplicaNames(req ScheduleRequest) []string {
	seen := make(map[string]struct{}, len(req.Replicas))
	workers := make([]string, 0, len(req.Replicas))
	for _, replica := range req.Replicas {
		if !replica.Ready || replica.AgentName != req.AgentName || replica.Name == "" {
			continue
		}
		if _, exists := seen[replica.Name]; exists {
			continue
		}
		seen[replica.Name] = struct{}{}
		workers = append(workers, replica.Name)
	}
	sort.Strings(workers)
	return workers
}
