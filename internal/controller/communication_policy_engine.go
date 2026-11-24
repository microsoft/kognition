// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	agentCommunicationPolicyKind   = "AgentCommunicationPolicy"
	crossNamespaceGrantKind        = "CrossNamespaceGrant"
	maxCommunicationPolicies       = 512
	maxCommunicationPolicyBytes    = 1 << 20
	maxCommunicationPolicySetBytes = 8 << 20
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
	return e.evaluateQualifiedDispatch(ctx, namespace, sourceAgent, namespace, targetAgent)
}

func (e *CommunicationPolicyEngine) evaluateQualifiedDispatch(ctx context.Context, sourceNamespace, sourceAgent, namespace, targetAgent string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if sourceNamespace == "" || sourceAgent == "" || namespace == "" || targetAgent == "" {
		return false, fmt.Errorf("exact source and target identities are required")
	}
	rawPolicies, err := e.store.List(ctx, agentCommunicationPolicyKind, store.ListOptions{Namespace: namespace, Limit: maxCommunicationPolicies + 1})
	if err != nil {
		return false, fmt.Errorf("list communication policies: %w", err)
	}
	if len(rawPolicies) > maxCommunicationPolicies {
		return false, fmt.Errorf("communication policy count exceeds its finite bound")
	}
	policies := make([]v1alpha1.AgentCommunicationPolicy, 0, len(rawPolicies))
	totalBytes := 0
	for _, raw := range rawPolicies {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		totalBytes += len(raw)
		if len(raw) > maxCommunicationPolicyBytes || totalBytes > maxCommunicationPolicySetBytes {
			return false, fmt.Errorf("communication policy data exceeds its finite bound")
		}
		if _, err := runtimeapproval.CanonicalizeJSON(raw); err != nil {
			return false, fmt.Errorf("invalid communication policy: %w", err)
		}
		var policy v1alpha1.AgentCommunicationPolicy
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&policy); err != nil {
			return false, fmt.Errorf("invalid communication policy: %w", err)
		}
		if policy.Namespace != namespace || policy.Name == "" {
			return false, fmt.Errorf("communication policy identity does not match the requested namespace")
		}
		for _, reference := range append(append([]v1alpha1.LocalObjectReference(nil), policy.Spec.AllowFrom...), policy.Spec.AllowTo...) {
			if reference.Name != "*" && len(validation.IsDNS1123Subdomain(reference.Name)) != 0 {
				return false, fmt.Errorf("communication policy has an invalid Agent reference")
			}
		}
		for _, reference := range policy.Spec.AllowFromQualified {
			if len(validation.IsDNS1123Label(reference.Namespace)) != 0 || reference.Name != "*" && len(validation.IsDNS1123Subdomain(reference.Name)) != 0 {
				return false, fmt.Errorf("communication policy has an invalid namespace-qualified Agent reference")
			}
		}
		policies = append(policies, policy)
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
	for _, policy := range policies {
		if policy.Spec.DefaultDeny {
			hasDefaultDeny = true
		}

		// Check if this policy explicitly allows the communication.
		if policy.Namespace == namespace && policy.DeletionTimestamp == nil && policyAllowsQualified(&policy, sourceNamespace, sourceAgent, targetAgent) {
			if err := ctx.Err(); err != nil {
				return false, err
			}
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
	allowed, err := e.evaluateQualifiedDispatch(ctx, sourceNamespace, sourceAgent, targetNamespace, targetAgent)
	if err != nil {
		return false, err
	}

	return allowed, nil
}

// policyAllows checks if a specific policy explicitly allows communication.
func policyAllows(policy *v1alpha1.AgentCommunicationPolicy, sourceAgent, targetAgent string) bool {
	return policyAllowsQualified(policy, policy.Namespace, sourceAgent, targetAgent)
}

func policyAllowsQualified(policy *v1alpha1.AgentCommunicationPolicy, sourceNamespace, sourceAgent, targetAgent string) bool {
	// A policy with no allow rules does not grant access.
	if len(policy.Spec.AllowFrom) == 0 && len(policy.Spec.AllowFromQualified) == 0 && len(policy.Spec.AllowTo) == 0 {
		return false
	}

	sourceAllowed := false
	if sourceNamespace == policy.Namespace {
		sourceAllowed = len(policy.Spec.AllowFrom) == 0 && len(policy.Spec.AllowFromQualified) == 0
		for _, from := range policy.Spec.AllowFrom {
			if from.Name == sourceAgent || from.Name == "*" {
				sourceAllowed = true
				break
			}
		}
	}
	for _, from := range policy.Spec.AllowFromQualified {
		if from.Namespace != "" && from.Namespace == sourceNamespace && (from.Name == sourceAgent || from.Name == "*") {
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
