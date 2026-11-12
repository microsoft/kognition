// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const agentPolicyKind = "AgentPolicy"

// AgentPolicyReconciler compiles aggregate policies for agents based on
// workspace-level and agent-level AgentPolicy resources.
type AgentPolicyReconciler struct {
	store  store.Store
	engine *opa.Engine
	logger *zap.Logger
}

// NewAgentPolicyReconciler creates a new AgentPolicyReconciler.
func NewAgentPolicyReconciler(s store.Store, engine *opa.Engine, logger *zap.Logger) *AgentPolicyReconciler {
	return &AgentPolicyReconciler{
		store:  s,
		engine: engine,
		logger: logger.With(zap.String("controller", "agent-policy")),
	}
}

// Reconcile processes an AgentPolicy resource, ensuring its rules are compiled
// and ready for evaluation by the admission pipeline.
func (r *AgentPolicyReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	raw, err := r.store.Get(ctx, agentPolicyKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			r.logger.Debug("AgentPolicy deleted, removing compiled policies", zap.String("name", req.Name), requestid.ZapField(ctx))
			r.engine.RemovePolicy(policyKey(req.Namespace, req.Name))
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get AgentPolicy: %w", err)
	}

	var policy v1alpha1.AgentPolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return Result{}, fmt.Errorf("unmarshal AgentPolicy: %w", err)
	}

	oldPhase := policy.Status.Phase
	oldMessage := policy.Status.Message
	oldGen := policy.Status.ObservedGeneration

	// Build aggregate Rego from all rule sources.
	aggregateRego := r.buildAggregateRego(&policy)

	var (
		newPhase   v1alpha1.Phase
		newMessage string
		readyState lifecycle.ConditionStatus
		reason     string
	)

	switch {
	case aggregateRego == "":
		r.logger.Warn("AgentPolicy has no evaluatable rules",
			zap.String("namespace", req.Namespace),
			zap.String("name", req.Name),
			requestid.ZapField(ctx))
		r.engine.RemovePolicy(policyKey(req.Namespace, req.Name))
		newPhase = v1alpha1.PhaseReady
		newMessage = "policy has no evaluatable rules"
		readyState = lifecycle.ConditionTrue
		reason = "NoRules"
	default:
		key := policyKey(req.Namespace, req.Name)
		if loadErr := r.engine.LoadPolicy(key, aggregateRego); loadErr != nil {
			newPhase = v1alpha1.PhaseFailed
			newMessage = loadErr.Error()
			readyState = lifecycle.ConditionFalse
			reason = "CompileFailed"
			r.logger.Error("AgentPolicy compile failed",
				zap.String("namespace", req.Namespace),
				zap.String("name", req.Name),
				zap.Error(loadErr),
				requestid.ZapField(ctx))
		} else {
			newPhase = v1alpha1.PhaseReady
			newMessage = "policy compiled successfully"
			readyState = lifecycle.ConditionTrue
			reason = "PolicyCompiled"
			r.logger.Info("AgentPolicy compiled",
				zap.String("namespace", req.Namespace),
				zap.String("name", req.Name),
				zap.String("scope", policy.Spec.Scope),
				zap.Bool("enforce", policy.Spec.Enforce),
				zap.Int("safetyRules", len(policy.Spec.SafetyRules)),
				zap.Int("complianceRules", len(policy.Spec.ComplianceRules)),
				zap.Int("toolGrants", len(policy.Spec.ToolGrants)),
				requestid.ZapField(ctx))
		}
	}

	policy.Status.Phase = newPhase
	policy.Status.Message = newMessage
	policy.Status.ObservedGeneration = policy.ObjectMeta.Generation
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(policy.Status.Conditions),
		lifecycle.ConditionReady,
		readyState,
		reason,
		newMessage,
	)
	policy.Status.Conditions = lifecycleToAPIConditions(lc)

	// Skip persist if nothing material changed (avoid pg_notify loops).
	if oldPhase == newPhase && oldMessage == newMessage && oldGen == policy.Status.ObservedGeneration {
		return Result{}, nil
	}

	data, err := json.Marshal(policy)
	if err != nil {
		return Result{}, fmt.Errorf("marshal AgentPolicy: %w", err)
	}
	if err := r.store.Update(ctx, agentPolicyKind, req.Namespace, req.Name, data, policy.ObjectMeta.ResourceVersion); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return Result{Requeue: true}, nil
		}
		return Result{}, fmt.Errorf("update AgentPolicy status: %w", err)
	}

	return Result{}, nil
}

// buildAggregateRego assembles a combined Rego policy from the AgentPolicy spec.
func (r *AgentPolicyReconciler) buildAggregateRego(policy *v1alpha1.AgentPolicy) string {
	var parts []string

	// Include the raw Rego rules if present.
	if policy.Spec.Rules != "" {
		parts = append(parts, policy.Spec.Rules)
	}

	// Compile safety rules into deny statements.
	for _, rule := range policy.Spec.SafetyRules {
		if rule.Rego != "" {
			parts = append(parts, rule.Rego)
		}
	}

	// Compile compliance rules into deny statements.
	for _, rule := range policy.Spec.ComplianceRules {
		if rule.Rego != "" {
			parts = append(parts, rule.Rego)
		}
	}

	// Compile tool grant conditions into deny statements.
	for _, grant := range policy.Spec.ToolGrants {
		if !grant.Allow {
			// Denied tool: generate a deny rule.
			denyRego := fmt.Sprintf(`deny[msg] { input.tool == %q; msg := "tool %s is denied by policy" }`, grant.ToolRef, grant.ToolRef)
			parts = append(parts, denyRego)
		} else if grant.Conditions != "" {
			// Conditionally allowed tool: include the condition Rego.
			parts = append(parts, grant.Conditions)
		}
	}

	return strings.Join(parts, "\n")
}

// EvaluateAgent evaluates all policies applicable to a specific agent.
func (r *AgentPolicyReconciler) EvaluateAgent(ctx context.Context, namespace, agentName string, input map[string]interface{}) (*opa.EvalResult, error) {
	// List all policies in the namespace.
	raws, err := r.store.List(ctx, agentPolicyKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return nil, fmt.Errorf("list AgentPolicies: %w", err)
	}

	// Add agent identity to input.
	input["agent"] = agentName
	input["namespace"] = namespace

	for _, raw := range raws {
		var policy v1alpha1.AgentPolicy
		if err := json.Unmarshal(raw, &policy); err != nil {
			continue
		}

		// Skip policies not applicable to this agent.
		if !r.policyAppliesToAgent(&policy, agentName) {
			continue
		}

		key := policyKey(namespace, policy.ObjectMeta.Name)
		aggregateRego := r.buildAggregateRego(&policy)
		if aggregateRego == "" {
			continue
		}
		if err := r.engine.LoadPolicy(key, aggregateRego); err != nil {
			return nil, fmt.Errorf("load AgentPolicy %q for runtime evaluation: %w", policy.ObjectMeta.Name, err)
		}
		result, err := r.engine.Evaluate(ctx, key, input)
		if err != nil {
			return nil, fmt.Errorf("evaluate AgentPolicy %q: %w", policy.ObjectMeta.Name, err)
		}
		if !result.Allowed {
			if policy.Spec.Enforce {
				return result, nil
			}
			r.logger.Warn("warn-only AgentPolicy matched",
				zap.String("policy", policy.ObjectMeta.Name),
				zap.String("agent", agentName),
				zap.String("reason", result.Reason),
				requestid.ZapField(ctx),
			)
		}
	}

	return &opa.EvalResult{Allowed: true}, nil
}

// policyAppliesToAgent checks if a policy targets the given agent.
func (r *AgentPolicyReconciler) policyAppliesToAgent(policy *v1alpha1.AgentPolicy, agentName string) bool {
	// Workspace-scoped policies apply to all agents.
	if policy.Spec.Scope == "" || policy.Spec.Scope == "workspace" {
		return true
	}

	// Agent-scoped policies only apply to listed targets.
	for _, target := range policy.Spec.TargetAgents {
		if target == agentName {
			return true
		}
	}
	return false
}

// policyKey generates a unique key for a policy in the OPA engine.
func policyKey(namespace, name string) string {
	return namespace + "/" + name
}
