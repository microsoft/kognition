// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	toolGrantKind  = "ToolGrant"
	modelGrantKind = "ModelGrant"
)

// ToolGrantEngine enforces ToolGrant and ModelGrant policies with rate limiting.
type ToolGrantEngine struct {
	store  store.Store
	logger *zap.Logger

	// rateLimiter tracks per-agent per-tool invocation counts.
	mu       sync.Mutex
	counters map[string]*rateLimitCounter
}

// rateLimitCounter tracks invocation counts for rate limiting.
type rateLimitCounter struct {
	minuteCount int64
	hourCount   int64
	dayCount    int64
	minuteReset time.Time
	hourReset   time.Time
	dayReset    time.Time
}

// NewToolGrantEngine creates a new ToolGrant enforcement engine.
func NewToolGrantEngine(s store.Store, logger *zap.Logger) *ToolGrantEngine {
	return &ToolGrantEngine{
		store:    s,
		logger:   logger.With(zap.String("component", "tool-grant-engine")),
		counters: make(map[string]*rateLimitCounter),
	}
}

// EvaluateToolAccess checks if an agent is permitted to invoke a tool with
// the given verb, applying rate-limit checks when configured.
func (e *ToolGrantEngine) EvaluateToolAccess(ctx context.Context, namespace, agentName, toolName string, verb v1alpha1.ToolGrantVerb) error {
	_, err := e.EvaluateToolAccessDecision(ctx, namespace, agentName, toolName, verb)
	return err
}

// ToolGrantDecision identifies the persisted grant or AgentPolicy projection
// that allowed a tool operation. It is used for gateway audit provenance.
type ToolGrantDecision struct {
	GrantRef  string
	PolicyRef string
}

// EvaluateToolAccessDecision checks access and returns authorization provenance.
func (e *ToolGrantEngine) EvaluateToolAccessDecision(ctx context.Context, namespace, agentName, toolName string, verb v1alpha1.ToolGrantVerb) (ToolGrantDecision, error) {
	return e.evaluateToolAccessDecision(ctx, namespace, agentName, toolName, verb, nil, false)
}

// EvaluateToolInvocationDecision checks invoke access, including argument constraints.
func (e *ToolGrantEngine) EvaluateToolInvocationDecision(ctx context.Context, namespace, agentName, toolName string, input map[string]any) (ToolGrantDecision, error) {
	return e.evaluateToolAccessDecision(ctx, namespace, agentName, toolName, v1alpha1.ToolGrantVerbInvoke, input, true)
}

func (e *ToolGrantEngine) evaluateToolAccessDecision(ctx context.Context, namespace, agentName, toolName string, verb v1alpha1.ToolGrantVerb, input map[string]any, validateArguments bool) (ToolGrantDecision, error) {
	grants, err := e.store.List(ctx, toolGrantKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return ToolGrantDecision{}, fmt.Errorf("list tool grants: %w", err)
	}

	var candidateDecision ToolGrantDecision
	var candidateErr error
	for _, raw := range grants {
		var grant v1alpha1.ToolGrant
		if err := json.Unmarshal(raw, &grant); err != nil {
			continue
		}

		grantRef := fmt.Sprintf("%s/%s@%d", namespace, grant.ObjectMeta.Name, grant.ObjectMeta.ResourceVersion)
		allowed, err := e.evaluateToolGrant(ctx, namespace, agentName, toolName, verb, input, validateArguments, &grant)
		if err != nil {
			candidateDecision = ToolGrantDecision{GrantRef: grantRef}
			candidateErr = err
			continue
		}
		if allowed {
			return ToolGrantDecision{GrantRef: grantRef}, nil
		}
	}

	allowed, policyRef, err := e.evaluateAgentPolicyToolGrants(ctx, namespace, agentName, toolName, verb)
	if err != nil {
		return ToolGrantDecision{}, err
	}
	if allowed {
		return ToolGrantDecision{PolicyRef: policyRef}, nil
	}
	if candidateErr != nil {
		return candidateDecision, candidateErr
	}

	return ToolGrantDecision{}, fmt.Errorf("no ToolGrant permits agent %q to %s tool %q in namespace %q", agentName, verb, toolName, namespace)
}

func (e *ToolGrantEngine) evaluateToolGrant(ctx context.Context, namespace, agentName, toolName string, verb v1alpha1.ToolGrantVerb, input map[string]any, validateArguments bool, grant *v1alpha1.ToolGrant) (bool, error) {
	matchesAgent, err := e.toolGrantMatchesAgent(ctx, namespace, grant, agentName)
	if err != nil || !matchesAgent {
		return false, err
	}

	if grant.Spec.ToolRef.Name != toolName && grant.Spec.ToolRef.Name != "*" {
		return false, nil
	}

	if !toolGrantHasVerb(grant, verb) {
		return false, nil
	}
	if matches, err := e.toolGrantConditionsMatch(ctx, namespace, agentName, grant.Spec.Conditions); err != nil || !matches {
		return false, err
	}
	if validateArguments && len(grant.Spec.ArgumentConstraints) > 0 {
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("toolgrant-argument-constraints.json", grant.Spec.ArgumentConstraints); err != nil {
			return false, fmt.Errorf("load argument constraints for ToolGrant %q: %w", grant.ObjectMeta.Name, err)
		}
		schema, err := compiler.Compile("toolgrant-argument-constraints.json")
		if err != nil {
			return false, fmt.Errorf("compile argument constraints for ToolGrant %q: %w", grant.ObjectMeta.Name, err)
		}
		if err := schema.Validate(input); err != nil {
			return false, &ToolGrantArgumentConstraintError{GrantName: grant.ObjectMeta.Name, Err: err}
		}
	}

	if validateArguments && grant.Spec.RateLimits != nil {
		if err := e.reserveInvocation(ctx, namespace, agentName, toolName, grant, grant.Spec.RateLimits); err != nil {
			return false, err
		}
	}
	return true, nil
}

// ToolGrantArgumentConstraintError identifies an invocation rejected by a
// matching ToolGrant's JSON Schema without exposing the invocation arguments.
type ToolGrantArgumentConstraintError struct {
	GrantName string
	Err       error
}

func (e *ToolGrantArgumentConstraintError) Error() string {
	return fmt.Sprintf("ToolGrant %q argument constraints denied invocation: %v", e.GrantName, e.Err)
}

func (e *ToolGrantArgumentConstraintError) Unwrap() error { return e.Err }

func (e *ToolGrantEngine) toolGrantConditionsMatch(ctx context.Context, namespace, agentName string, conditions *v1alpha1.ToolGrantConditions) (bool, error) {
	if conditions == nil {
		return true, nil
	}
	now := time.Now()
	if conditions.ValidFrom != "" {
		validFrom, err := time.Parse(time.RFC3339, conditions.ValidFrom)
		if err != nil {
			return false, fmt.Errorf("parse ToolGrant validFrom: %w", err)
		}
		if now.Before(validFrom) {
			return false, nil
		}
	}
	if conditions.ValidUntil != "" {
		validUntil, err := time.Parse(time.RFC3339, conditions.ValidUntil)
		if err != nil {
			return false, fmt.Errorf("parse ToolGrant validUntil: %w", err)
		}
		if !now.Before(validUntil) {
			return false, nil
		}
	}
	if len(conditions.RequiredAgentLabels) == 0 {
		return true, nil
	}
	raw, err := e.store.Get(ctx, "Agent", namespace, agentName)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load agent for ToolGrant conditions: %w", err)
	}
	var agent v1alpha1.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		return false, fmt.Errorf("unmarshal agent for ToolGrant conditions: %w", err)
	}
	for key, value := range conditions.RequiredAgentLabels {
		if agent.ObjectMeta.Labels[key] != value {
			return false, nil
		}
	}
	return true, nil
}

func (e *ToolGrantEngine) evaluateAgentPolicyToolGrants(ctx context.Context, namespace, agentName, toolName string, verb v1alpha1.ToolGrantVerb) (bool, string, error) {
	policies, err := e.store.List(ctx, agentPolicyKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return false, "", fmt.Errorf("list agent policies for tool grants: %w", err)
	}

	for _, raw := range policies {
		var policy v1alpha1.AgentPolicy
		if err := json.Unmarshal(raw, &policy); err != nil {
			continue
		}
		if !policy.Spec.Enforce || !agentPolicyToolGrantsTargetAgent(&policy, agentName) {
			continue
		}
		for _, policyGrant := range policy.Spec.ToolGrants {
			if verb != v1alpha1.ToolGrantVerbInvoke || !policyGrant.Allow || (policyGrant.ToolRef != toolName && policyGrant.ToolRef != "*") {
				continue
			}
			if policyGrant.Conditions != "" {
				conditionEngine := opa.NewEngine()
				if err := conditionEngine.LoadPolicy("tool-grant-condition", policyGrant.Conditions); err != nil {
					return false, "", fmt.Errorf("compile AgentPolicy %q tool grant condition: %w", policy.ObjectMeta.Name, err)
				}
				conditionInput := map[string]interface{}{
					"agent":     agentName,
					"namespace": namespace,
					"tool":      toolName,
					"verb":      string(verb),
				}
				if agent, err := getStoredResource[v1alpha1.Agent](ctx, e.store, "Agent", namespace, agentName); err == nil {
					conditionInput["metadata"] = map[string]interface{}{
						"name":        agent.ObjectMeta.Name,
						"namespace":   agent.ObjectMeta.Namespace,
						"labels":      agent.ObjectMeta.Labels,
						"annotations": agent.ObjectMeta.Annotations,
					}
				}
				result, err := conditionEngine.Evaluate(ctx, "tool-grant-condition", conditionInput)
				if err != nil {
					return false, "", fmt.Errorf("evaluate AgentPolicy %q tool grant condition: %w", policy.ObjectMeta.Name, err)
				}
				if !result.Allowed {
					continue
				}
			}
			grant := v1alpha1.ToolGrant{
				Spec: v1alpha1.ToolGrantSpec{
					AgentRef: v1alpha1.LocalObjectReference{Name: agentName},
					ToolRef:  v1alpha1.LocalObjectReference{Name: policyGrant.ToolRef},
					Verbs:    []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
				},
			}
			allowed, err := e.evaluateToolGrant(ctx, namespace, agentName, toolName, verb, nil, false, &grant)
			if err != nil {
				return false, "", err
			}
			if allowed {
				return true, fmt.Sprintf("%s/%s@%d", namespace, policy.ObjectMeta.Name, policy.ObjectMeta.ResourceVersion), nil
			}
		}
	}
	return false, "", nil
}

func agentPolicyToolGrantsTargetAgent(policy *v1alpha1.AgentPolicy, agentName string) bool {
	if policy.Spec.Scope == "" || policy.Spec.Scope == "workspace" {
		return true
	}
	for _, target := range policy.Spec.TargetAgents {
		if target == agentName {
			return true
		}
	}
	return false
}

// EvaluateModelAccess checks if an agent has a ModelGrant for the given model.
func (e *ToolGrantEngine) EvaluateModelAccess(ctx context.Context, namespace, agentName, modelName string) error {
	grants, err := e.store.List(ctx, modelGrantKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return fmt.Errorf("list model grants: %w", err)
	}

	for _, raw := range grants {
		var grant v1alpha1.ModelGrant
		if err := json.Unmarshal(raw, &grant); err != nil {
			continue
		}

		if grant.Spec.AgentRef.Name == agentName && (grant.Spec.ModelRef.Name == modelName || grant.Spec.ModelRef.Name == "*") {
			return nil
		}
	}

	return fmt.Errorf("no ModelGrant permits agent %q to use model %q in namespace %q", agentName, modelName, namespace)
}

// toolGrantMatchesAgent checks direct references and matchLabels selectors.
func (e *ToolGrantEngine) toolGrantMatchesAgent(ctx context.Context, namespace string, grant *v1alpha1.ToolGrant, agentName string) (bool, error) {
	if grant.Spec.AgentRef.Name == agentName {
		return true, nil
	}
	if len(grant.Spec.AgentSelector) == 0 {
		return false, nil
	}
	agent, err := getStoredResource[v1alpha1.Agent](ctx, e.store, "Agent", namespace, agentName)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load agent for ToolGrant selector: %w", err)
	}
	for key, value := range grant.Spec.AgentSelector {
		if agent.ObjectMeta.Labels[key] != value {
			return false, nil
		}
	}
	return true, nil
}

// toolGrantHasVerb checks if the grant includes the requested verb.
func toolGrantHasVerb(grant *v1alpha1.ToolGrant, verb v1alpha1.ToolGrantVerb) bool {
	for _, v := range grant.Spec.Verbs {
		if v == verb || v == "*" {
			return true
		}
	}
	return false
}

// reserveInvocation atomically checks and consumes one rate-limit unit.
func (e *ToolGrantEngine) reserveInvocation(ctx context.Context, namespace, agentName, toolName string, grant *v1alpha1.ToolGrant, limits *v1alpha1.ToolGrantRateLimits) error {
	key := fmt.Sprintf("%s/%s@%d/%s/%s", namespace, grant.ObjectMeta.Name, grant.ObjectMeta.ResourceVersion, agentName, toolName)
	now := time.Now()
	if reservationStore, ok := e.store.(store.ToolGrantRateReservationStore); ok {
		err := reservationStore.ReserveToolGrantInvocation(ctx, store.ToolGrantRateReservation{
			Key:            key,
			Namespace:      namespace,
			Now:            now,
			CallsPerMinute: limits.CallsPerMinute,
			CallsPerHour:   limits.CallsPerHour,
			CallsPerDay:    limits.CallsPerDay,
		})
		if errors.Is(err, store.ErrQuotaExceeded) {
			return fmt.Errorf("rate limit exceeded for agent %q invoking tool %q: %w", agentName, toolName, err)
		}
		if err != nil {
			return fmt.Errorf("reserve ToolGrant rate limit: %w", err)
		}
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	counter, exists := e.counters[key]
	if !exists {
		counter = &rateLimitCounter{}
		e.counters[key] = counter
	}
	if !now.Before(counter.minuteReset) {
		counter.minuteCount = 0
		counter.minuteReset = now.Add(time.Minute)
	}
	if !now.Before(counter.hourReset) {
		counter.hourCount = 0
		counter.hourReset = now.Add(time.Hour)
	}
	if !now.Before(counter.dayReset) {
		counter.dayCount = 0
		counter.dayReset = now.Add(24 * time.Hour)
	}

	if limits.CallsPerMinute > 0 && counter.minuteCount >= limits.CallsPerMinute {
		return fmt.Errorf("rate limit exceeded: agent %q exceeded %d calls/minute for tool %q", agentName, limits.CallsPerMinute, toolName)
	}

	if limits.CallsPerHour > 0 && counter.hourCount >= limits.CallsPerHour {
		return fmt.Errorf("rate limit exceeded: agent %q exceeded %d calls/hour for tool %q", agentName, limits.CallsPerHour, toolName)
	}

	if limits.CallsPerDay > 0 && counter.dayCount >= limits.CallsPerDay {
		return fmt.Errorf("rate limit exceeded: agent %q exceeded %d calls/day for tool %q", agentName, limits.CallsPerDay, toolName)
	}

	counter.minuteCount++
	counter.hourCount++
	counter.dayCount++
	return nil
}
