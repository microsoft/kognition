// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"sync"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// This file implements the cognitive scheduling strategies described in
// PROJECT.md (token/context, cost, and semantic-locality aware placement) as
// pluggable SchedulerPlugins. They score candidate workers; the scheduler sums
// plugin scores and picks the highest. None of them change placement on their
// own unless registered.

// --- Session (semantic) locality ---------------------------------------------

// SessionLocalityPlugin routes requests that share a SessionID back to the
// worker that previously served that session, preserving warm context (KV cache,
// loaded memory) and reducing cold-start cost. It remembers a bounded number of
// recent session→worker assignments.
type SessionLocalityPlugin struct {
	bonus   int
	maxSize int

	mu    sync.Mutex
	assoc map[string]string // sessionID -> worker
	order []string          // insertion order for FIFO eviction
}

// SessionLocalityConfig configures a SessionLocalityPlugin.
type SessionLocalityConfig struct {
	// Bonus is the score added to the sticky worker. Defaults to 100.
	Bonus int
	// MaxSessions caps how many session associations are retained. Defaults to 4096.
	MaxSessions int
}

// NewSessionLocalityPlugin creates a SessionLocalityPlugin.
func NewSessionLocalityPlugin(cfg SessionLocalityConfig) *SessionLocalityPlugin {
	bonus := cfg.Bonus
	if bonus <= 0 {
		bonus = 100
	}
	maxSize := cfg.MaxSessions
	if maxSize <= 0 {
		maxSize = 4096
	}
	return &SessionLocalityPlugin{
		bonus:   bonus,
		maxSize: maxSize,
		assoc:   make(map[string]string),
	}
}

func (p *SessionLocalityPlugin) Name() string { return "session-locality" }

// Score gives the previously-assigned worker for this session a bonus, provided
// that worker is still among the available candidates.
func (p *SessionLocalityPlugin) Score(_ context.Context, req ScheduleRequest, workers []string) (map[string]int, error) {
	scores := make(map[string]int, len(workers))
	if req.SessionID == "" {
		return scores, nil
	}

	p.mu.Lock()
	sticky, ok := p.assoc[req.SessionID]
	p.mu.Unlock()
	if !ok {
		return scores, nil
	}

	for _, w := range workers {
		if w == sticky {
			scores[w] = p.bonus
			break
		}
	}
	return scores, nil
}

// OnAssigned records the session→worker association for future stickiness.
func (p *SessionLocalityPlugin) OnAssigned(req ScheduleRequest, worker string) {
	if req.SessionID == "" || worker == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.assoc[req.SessionID]; !exists {
		if len(p.order) >= p.maxSize {
			oldest := p.order[0]
			p.order[0] = "" // release the evicted key for GC promptly
			p.order = p.order[1:]
			delete(p.assoc, oldest)
		}
		p.order = append(p.order, req.SessionID)
	}
	p.assoc[req.SessionID] = worker
}

// --- Cost awareness ----------------------------------------------------------

// CostAwarePlugin prefers workers whose model resolves to a cheaper ModelClass
// cost tier, so routine work lands on lower-cost models when several are viable.
type CostAwarePlugin struct {
	store      store.Store
	tierScores map[string]int
}

// NewCostAwarePlugin creates a CostAwarePlugin. When tierScores is nil or empty a
// default mapping of free=100, standard=50, premium=10 is used; unknown tiers
// score 0. The provided map is copied so later external mutation cannot affect
// scoring.
func NewCostAwarePlugin(s store.Store, tierScores map[string]int) *CostAwarePlugin {
	resolved := make(map[string]int, len(tierScores))
	if len(tierScores) == 0 {
		resolved = map[string]int{
			"free":     100,
			"standard": 50,
			"premium":  10,
		}
	} else {
		for tier, score := range tierScores {
			resolved[tier] = score
		}
	}
	return &CostAwarePlugin{store: s, tierScores: resolved}
}

func (p *CostAwarePlugin) Name() string { return "cost-aware" }

// Score applies the requested Agent's ModelClass cost score uniformly to its
// eligible replicas. Cost never authorizes switching to another Agent.
func (p *CostAwarePlugin) Score(ctx context.Context, req ScheduleRequest, workers []string) (map[string]int, error) {
	scores := make(map[string]int, len(workers))
	mc, err := resolveAgentModelClass(ctx, p.store, req.Namespace, req.AgentName)
	if err != nil {
		return scores, nil
	}
	score, ok := p.tierScores[mc.Spec.CostTier]
	if !ok {
		return scores, nil
	}
	for _, worker := range workers {
		scores[worker] = score
	}
	return scores, nil
}

// --- Context / token capacity ------------------------------------------------

// ContextCapacityPlugin prefers workers backed by a model with a larger context
// window, which better accommodates long sessions and large prompts.
type ContextCapacityPlugin struct {
	store   store.Store
	divisor int64
	cap     int
}

// ContextCapacityConfig configures a ContextCapacityPlugin.
type ContextCapacityConfig struct {
	// Divisor converts a context window (in tokens) into a score. Defaults to 2048.
	Divisor int64
	// Cap bounds the maximum score so this signal does not dwarf others. Defaults to 100.
	Cap int
}

// NewContextCapacityPlugin creates a ContextCapacityPlugin.
func NewContextCapacityPlugin(s store.Store, cfg ContextCapacityConfig) *ContextCapacityPlugin {
	divisor := cfg.Divisor
	if divisor <= 0 {
		divisor = 2048
	}
	capScore := cfg.Cap
	if capScore <= 0 {
		capScore = 100
	}
	return &ContextCapacityPlugin{store: s, divisor: divisor, cap: capScore}
}

func (p *ContextCapacityPlugin) Name() string { return "context-capacity" }

// Score applies the requested Agent's context-window score uniformly to its
// eligible replicas. Context capacity never authorizes Agent substitution.
func (p *ContextCapacityPlugin) Score(ctx context.Context, req ScheduleRequest, workers []string) (map[string]int, error) {
	scores := make(map[string]int, len(workers))
	mc, err := resolveAgentModelClass(ctx, p.store, req.Namespace, req.AgentName)
	if err != nil || mc.Spec.ContextWindow <= 0 {
		return scores, nil
	}
	score := int(mc.Spec.ContextWindow / p.divisor)
	if score > p.cap {
		score = p.cap
	}
	for _, worker := range workers {
		scores[worker] = score
	}
	return scores, nil
}

// --- Shared resolution -------------------------------------------------------

// resolveAgentModelClass resolves the immutable requested Agent through the
// Agent → Model → ModelClass chain within a namespace.
func resolveAgentModelClass(ctx context.Context, s store.Store, namespace, agentName string) (*v1alpha1.ModelClass, error) {
	agent, err := getStoredResource[v1alpha1.Agent](ctx, s, "Agent", namespace, agentName)
	if err != nil {
		return nil, err
	}
	model, err := getStoredResource[v1alpha1.Model](ctx, s, "Model", namespace, agent.Spec.ModelRef.Name)
	if err != nil {
		return nil, err
	}
	return getStoredResource[v1alpha1.ModelClass](ctx, s, "ModelClass", "", model.Spec.ClassName)
}
