// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/store"
)

// AgentPolicyEvaluator evaluates policies applicable to an invocation principal.
type AgentPolicyEvaluator interface {
	EvaluateAgent(context.Context, string, string, map[string]interface{}) (*opa.EvalResult, error)
}

// InvocationPolicyEngine applies grants and AgentPolicy rules at dispatch time.
type InvocationPolicyEngine struct {
	grants   *ToolGrantEngine
	policies AgentPolicyEvaluator
	store    store.Store
	rbac     *RBACEngine
}

// ToolAuthorizationDecision records the live policy resources that allowed a
// Tool invocation. The Agent Gateway persists it with invocation events.
type ToolAuthorizationDecision struct {
	GrantRef   string
	RoleRefs   []string
	PolicyRefs []string
}

// NewInvocationPolicyEngine creates a runtime invocation policy boundary.
func NewInvocationPolicyEngine(grants *ToolGrantEngine, policies AgentPolicyEvaluator) *InvocationPolicyEngine {
	engine := &InvocationPolicyEngine{grants: grants, policies: policies}
	if grants != nil {
		engine.store = grants.store
	}
	return engine
}

// WithAgentRBAC enforces AgentIdentity and AgentRole permissions for agent principals.
func (e *InvocationPolicyEngine) WithAgentRBAC(s store.Store, rbac *RBACEngine) *InvocationPolicyEngine {
	e.store = s
	e.rbac = rbac
	return e
}

// EvaluateToolAccess checks ToolGrant and applicable AgentPolicy rules.
func (e *InvocationPolicyEngine) EvaluateToolAccess(ctx context.Context, namespace, principal, toolName string, verb v1alpha1.ToolGrantVerb) error {
	_, err := e.AuthorizeTool(ctx, namespace, principal, toolName, verb)
	return err
}

// AuthorizeTool checks ToolGrant, enforced AgentPolicy, and AgentRole while
// returning the matching resource references for durable invocation audit.
func (e *InvocationPolicyEngine) AuthorizeTool(ctx context.Context, namespace, principal, toolName string, verb v1alpha1.ToolGrantVerb) (ToolAuthorizationDecision, error) {
	return e.authorizeTool(ctx, namespace, principal, toolName, verb, nil, false)
}

// AuthorizeToolInvocation evaluates an invocation including ToolGrant argument constraints.
func (e *InvocationPolicyEngine) AuthorizeToolInvocation(ctx context.Context, namespace, principal, toolName string, input map[string]any) (ToolAuthorizationDecision, error) {
	return e.authorizeTool(ctx, namespace, principal, toolName, v1alpha1.ToolGrantVerbInvoke, input, true)
}

func (e *InvocationPolicyEngine) authorizeTool(ctx context.Context, namespace, principal, toolName string, verb v1alpha1.ToolGrantVerb, invocationInput map[string]any, validateArguments bool) (ToolAuthorizationDecision, error) {
	decision := ToolAuthorizationDecision{}
	if e.grants != nil {
		var grantDecision ToolGrantDecision
		var err error
		if validateArguments {
			grantDecision, err = e.grants.EvaluateToolInvocationDecision(ctx, namespace, principal, toolName, invocationInput)
		} else {
			grantDecision, err = e.grants.EvaluateToolAccessDecision(ctx, namespace, principal, toolName, verb)
		}
		decision.GrantRef = grantDecision.GrantRef
		if grantDecision.PolicyRef != "" {
			decision.PolicyRefs = append(decision.PolicyRefs, grantDecision.PolicyRef)
		}
		if err != nil {
			return decision, err
		}
	}
	if e.policies == nil {
		roles, err := e.evaluateToolRBAC(ctx, namespace, principal)
		if err != nil {
			return decision, err
		}
		decision.RoleRefs = roles
		return decision, nil
	}
	input := map[string]interface{}{
		"action": "tool_call",
		"tool":   toolName,
		"verb":   string(verb),
	}
	if e.store != nil && !strings.Contains(principal, ":") {
		agent, err := getStoredResource[v1alpha1.Agent](ctx, e.store, "Agent", namespace, principal)
		if err != nil {
			return ToolAuthorizationDecision{}, fmt.Errorf("load agent for tool policy: %w", err)
		}
		input["metadata"] = map[string]interface{}{
			"name":        agent.ObjectMeta.Name,
			"namespace":   agent.ObjectMeta.Namespace,
			"labels":      agent.ObjectMeta.Labels,
			"annotations": agent.ObjectMeta.Annotations,
		}
		spec, err := agentSpecPolicyInput(agent.Spec)
		if err != nil {
			return ToolAuthorizationDecision{}, fmt.Errorf("build agent policy input: %w", err)
		}
		input["spec"] = spec
	}
	decision.PolicyRefs = append(decision.PolicyRefs, e.appliedPolicyRefs(ctx, namespace, principal)...)
	result, err := e.policies.EvaluateAgent(ctx, namespace, principal, input)
	if err != nil {
		return decision, fmt.Errorf("evaluate AgentPolicy: %w", err)
	}
	if !result.Allowed {
		return decision, fmt.Errorf("AgentPolicy denied tool %q: %s", toolName, result.Reason)
	}
	roles, err := e.evaluateToolRBAC(ctx, namespace, principal)
	if err != nil {
		return decision, err
	}
	decision.RoleRefs = roles
	return decision, nil
}

func (e *InvocationPolicyEngine) appliedPolicyRefs(ctx context.Context, namespace, principal string) []string {
	if e.store == nil {
		return nil
	}
	rawPolicies, err := e.store.List(ctx, agentPolicyKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return nil
	}
	references := make([]string, 0, len(rawPolicies))
	for _, raw := range rawPolicies {
		var policy v1alpha1.AgentPolicy
		if json.Unmarshal(raw, &policy) != nil || !policy.Spec.Enforce || !agentPolicyToolGrantsTargetAgent(&policy, principal) {
			continue
		}
		references = append(references, fmt.Sprintf("%s/%s@%d", namespace, policy.ObjectMeta.Name, policy.ObjectMeta.ResourceVersion))
	}
	return references
}

func (e *InvocationPolicyEngine) evaluateToolRBAC(ctx context.Context, namespace, principal string) ([]string, error) {
	if e.rbac == nil || e.store == nil || strings.Contains(principal, ":") {
		return nil, nil
	}
	identity, err := ResolveAuthorizationAgentIdentity(ctx, e.store, namespace, principal)
	if err != nil {
		return nil, fmt.Errorf("resolve identity for agent %q: %w", principal, err)
	}
	roles, err := e.rbac.EvaluateRoles(ctx, identity, "invoke", "Tool")
	if err != nil {
		return nil, fmt.Errorf("evaluate AgentRole for agent %q: %w", principal, err)
	}
	if len(roles) == 0 {
		return nil, fmt.Errorf("AgentRole denies agent %q invoke access to Tool resources", principal)
	}
	return roles, nil
}

// EvaluateModelAccess checks ModelGrant and applicable AgentPolicy rules.
func (e *InvocationPolicyEngine) EvaluateModelAccess(ctx context.Context, namespace, agentName, modelName string) error {
	if e.grants != nil {
		if err := e.grants.EvaluateModelAccess(ctx, namespace, agentName, modelName); err != nil {
			return err
		}
	}
	if e.policies == nil {
		return nil
	}
	input := map[string]interface{}{
		"action": "model_call",
		"model":  modelName,
	}
	if e.store != nil {
		agent, err := getStoredResource[v1alpha1.Agent](ctx, e.store, "Agent", namespace, agentName)
		if err != nil {
			return fmt.Errorf("load agent for model policy: %w", err)
		}
		input["metadata"] = map[string]interface{}{
			"name":        agent.ObjectMeta.Name,
			"namespace":   agent.ObjectMeta.Namespace,
			"labels":      agent.ObjectMeta.Labels,
			"annotations": agent.ObjectMeta.Annotations,
		}
		spec, err := agentSpecPolicyInput(agent.Spec)
		if err != nil {
			return fmt.Errorf("build agent policy input: %w", err)
		}
		input["spec"] = spec
	}
	result, err := e.policies.EvaluateAgent(ctx, namespace, agentName, input)
	if err != nil {
		return fmt.Errorf("evaluate AgentPolicy: %w", err)
	}
	if !result.Allowed {
		return fmt.Errorf("AgentPolicy denied model %q: %s", modelName, result.Reason)
	}
	return nil
}

func agentSpecPolicyInput(spec v1alpha1.AgentSpec) (map[string]interface{}, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}
