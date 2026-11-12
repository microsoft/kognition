// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/toolgateway"
)

// ToolGatewayAuthorizer adapts the existing invocation policy engine to the
// shared Tool Gateway contract without duplicating grants, RBAC, or policy.
type ToolGatewayAuthorizer struct {
	policies *InvocationPolicyEngine
}

// NewToolGatewayAuthorizer creates a Tool Gateway authorization adapter.
func NewToolGatewayAuthorizer(policies *InvocationPolicyEngine) *ToolGatewayAuthorizer {
	return &ToolGatewayAuthorizer{policies: policies}
}

// AuthorizeTool evaluates live ToolGrant, AgentPolicy, and AgentRole policy
// and exposes the persisted decision references for durable gateway audit.
func (a *ToolGatewayAuthorizer) AuthorizeTool(ctx context.Context, namespace, principal, toolName string, verb v1alpha1.ToolGrantVerb) (toolgateway.AuthorizationDecision, error) {
	decision, err := a.policies.AuthorizeTool(ctx, namespace, principal, toolName, verb)
	if err != nil {
		return toolgateway.AuthorizationDecision{}, err
	}
	return toolgateway.AuthorizationDecision{GrantRef: decision.GrantRef, RoleRefs: decision.RoleRefs, PolicyRefs: decision.PolicyRefs}, nil
}

// AuthorizeToolInvocation evaluates live authorization against invocation arguments.
func (a *ToolGatewayAuthorizer) AuthorizeToolInvocation(ctx context.Context, namespace, principal, toolName string, input map[string]any) (toolgateway.AuthorizationDecision, error) {
	decision, err := a.policies.AuthorizeToolInvocation(ctx, namespace, principal, toolName, input)
	gatewayDecision := toolgateway.AuthorizationDecision{GrantRef: decision.GrantRef, RoleRefs: decision.RoleRefs, PolicyRefs: decision.PolicyRefs}
	if err != nil {
		var constraintErr *ToolGrantArgumentConstraintError
		if errors.As(err, &constraintErr) {
			return gatewayDecision, fmt.Errorf("%w: %w", toolgateway.ErrToolArgumentConstraintsDenied, constraintErr)
		}
		return gatewayDecision, err
	}
	return gatewayDecision, nil
}
