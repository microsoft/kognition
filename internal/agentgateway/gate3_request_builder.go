// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentgateway

import (
	"context"
	"encoding/json"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

// NewGate3RequestBuilder returns a production Gate3RequestBuilder that maps
// an Agent Communication Gateway exact-capability exchange onto an
// ADR-0014 Gate 3 request. The target AgentService reference (kind, scope,
// namespace, name, uid, resource version) is stamped so a security reviewer
// approves the same (source agent, target AgentService, invocation) tuple
// that will actually run. The source AgentIdentity is provided by the
// gateway after ready-state validation; the source AgentInstance is loaded
// from the shared store under the request context so the durable
// RuntimeApproval binds to the current running instance.
//
// The builder returns ok=false and skips Gate 3 when the store is nil,
// when the AgentInstance cannot be resolved, when the identity is nil, or
// when the canonical payload cannot be produced: those cases are already
// denied by upstream capability validation, so downgrading them to a
// Gate 3 skip preserves the ADR-0014 fail-closed contract without emitting
// a spurious unavailable audit record.
func NewGate3RequestBuilder(st store.Store) Gate3RequestBuilder {
	return func(ctx context.Context, claims *runtimeauth.AgentDelegationSessionCapabilityClaims, identity *v1alpha1.AgentIdentity, invocationID, targetNamespace, targetService, targetAgent, targetUID string, targetResourceVersion int64, message string) (runtimeapproval.Gate3Request, bool) {
		if st == nil || claims == nil || identity == nil {
			return runtimeapproval.Gate3Request{}, false
		}
		instance, err := loadResource[v1alpha1.AgentInstance](ctx, st, "AgentInstance", claims.Namespace, claims.InstanceName)
		if err != nil {
			return runtimeapproval.Gate3Request{}, false
		}
		canonical, err := canonicalizeDelegationPayload(invocationID, targetNamespace, targetService, targetAgent, message)
		if err != nil {
			return runtimeapproval.Gate3Request{}, false
		}
		return runtimeapproval.Gate3Request{
			Workspace:     claims.Namespace,
			Namespace:     claims.Namespace,
			InvocationID:  invocationID,
			Plane:         v1alpha1.RuntimeApprovalPlaneCommunication,
			Operation:     "issue-capability",
			Action:        runtimeapproval.ActionExternalCommunication,
			AgentName:     claims.AgentName,
			SessionID:     claims.SessionID,
			AgentIdentity: *identity,
			AgentInstance: *instance,
			Target: v1alpha1.RuntimeApprovalReference{
				Kind:            "AgentService",
				Scope:           "Namespaced",
				Namespace:       targetNamespace,
				Name:            targetService,
				UID:             targetUID,
				ResourceVersion: targetResourceVersion,
			},
			CanonicalPayload: canonical,
		}, true
	}
}

// canonicalizeDelegationPayload produces a stable JSON digest input for the
// delegation invocation so the resulting RuntimeApproval binds to a
// deterministic payload independent of map iteration or field ordering.
func canonicalizeDelegationPayload(invocationID, targetNamespace, targetService, targetAgent, message string) ([]byte, error) {
	raw, err := json.Marshal(struct {
		InvocationID       string `json:"invocation_id"`
		TargetNamespace    string `json:"target_namespace"`
		TargetAgentService string `json:"target_agent_service"`
		TargetAgent        string `json:"target_agent"`
		Message            string `json:"message"`
	}{invocationID, targetNamespace, targetService, targetAgent, message})
	if err != nil {
		return nil, err
	}
	return runtimeapproval.CanonicalizeJSON(raw)
}
