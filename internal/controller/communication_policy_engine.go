// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	agentCommunicationPolicyKind = "AgentCommunicationPolicy"
	crossNamespaceGrantKind      = "CrossNamespaceGrant"
)

// CommunicationPolicyEngine enforces agent-to-agent communication policies.
type CommunicationPolicyEngine struct {
	store  store.Store
	logger *zap.Logger
}

// NewCommunicationPolicyEngine creates a new communication policy enforcement engine.
func NewCommunicationPolicyEngine(s store.Store, logger *zap.Logger) *CommunicationPolicyEngine {
	return &CommunicationPolicyEngine{
		store:  s,
		logger: logger.With(zap.String("component", "comm-policy-engine")),
	}
}

// EvaluateDispatch checks whether sourceAgent is permitted to send a message
// to targetAgent within the given namespace.
func (e *CommunicationPolicyEngine) EvaluateDispatch(ctx context.Context, namespace, sourceAgent, targetAgent string) (bool, error) {
	rawPolicies, err := e.store.List(ctx, agentCommunicationPolicyKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return false, fmt.Errorf("list communication policies: %w", err)
	}

	// Communication is a default-deny isolation plane. Workspace reconciliation
	// normally creates an explicit default-deny policy, but callers must remain
	// denied during creation races and after accidental policy deletion.
	if len(rawPolicies) == 0 {
		e.logger.Debug("communication denied without policy",
			zap.String("from", sourceAgent),
			zap.String("to", targetAgent),
			zap.String("namespace", namespace))
		return false, nil
	}

	// Check each policy.
	hasDefaultDeny := false
	for _, raw := range rawPolicies {
		var policy v1alpha1.AgentCommunicationPolicy
		if err := json.Unmarshal(raw, &policy); err != nil {
			continue
		}

		if policy.Spec.DefaultDeny {
			hasDefaultDeny = true
		}

		// Check if this policy explicitly allows the communication.
		if policyAllows(&policy, sourceAgent, targetAgent) {
			e.logger.Debug("communication allowed by policy",
				zap.String("policy", policy.ObjectMeta.Name),
				zap.String("from", sourceAgent),
				zap.String("to", targetAgent))
			return true, nil
		}
	}

	// Communication remains default-deny even when a malformed or legacy policy
	// omits defaultDeny. Only a matching explicit allow can authorize delivery.
	e.logger.Debug("communication denied without matching allow policy",
		zap.Bool("defaultDeny", hasDefaultDeny),
		zap.String("from", sourceAgent),
		zap.String("to", targetAgent),
		zap.String("namespace", namespace))
	return false, nil
}

// EvaluateCrossWorkspaceDispatch checks whether cross-workspace communication
// is allowed via CrossNamespaceGrants.
func (e *CommunicationPolicyEngine) EvaluateCrossWorkspaceDispatch(ctx context.Context, sourceNamespace, sourceAgent, targetNamespace, targetAgent string) (bool, error) {
	// First check if cross-namespace grant exists.
	if err := ValidateCrossWorkspaceAccess(ctx, e.store, sourceNamespace, targetNamespace, "Agent", targetAgent); err != nil {
		e.logger.Debug("cross-workspace communication denied",
			zap.String("from", sourceNamespace+"/"+sourceAgent),
			zap.String("to", targetNamespace+"/"+targetAgent),
			zap.Error(err))
		return false, nil
	}

	// Then check the target namespace policy allows the source.
	allowed, err := e.EvaluateDispatch(ctx, targetNamespace, sourceAgent, targetAgent)
	if err != nil {
		return false, err
	}

	return allowed, nil
}

// policyAllows checks if a specific policy explicitly allows communication.
func policyAllows(policy *v1alpha1.AgentCommunicationPolicy, sourceAgent, targetAgent string) bool {
	// A policy with no allow rules does not grant access.
	if len(policy.Spec.AllowFrom) == 0 && len(policy.Spec.AllowTo) == 0 {
		return false
	}

	sourceAllowed := len(policy.Spec.AllowFrom) == 0
	for _, from := range policy.Spec.AllowFrom {
		if from.Name == sourceAgent || from.Name == "*" {
			sourceAllowed = true
			break
		}
	}

	targetAllowed := len(policy.Spec.AllowTo) == 0
	for _, to := range policy.Spec.AllowTo {
		if to.Name == targetAgent || to.Name == "*" {
			targetAllowed = true
			break
		}
	}

	return sourceAllowed && targetAllowed
}
