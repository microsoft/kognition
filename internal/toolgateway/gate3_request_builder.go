// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

// NewGate3RequestBuilder returns a production Gate3RequestBuilder that maps a
// Tool Gateway Request onto an ADR-0014 Gate 3 request by loading the current
// AgentIdentity and AgentInstance from the shared store. The Tool argument
// carries the persisted Tool custom resource identity used as the exact
// approval target. The builder canonicalizes the invocation input so the
// resulting RuntimeApproval binds to a stable payload digest.
//
// The builder returns ok=false and skips Gate 3 when the AgentIdentity or
// AgentInstance cannot be resolved from the request context or when the
// invocation input cannot be canonicalized: those cases are already denied by
// upstream capability validation, so downgrading them to a Gate 3 skip
// preserves the ADR-0014 fail-closed contract without producing a spurious
// unavailable audit record.
func NewGate3RequestBuilder(st store.Store) Gate3RequestBuilder {
	return func(ctx context.Context, request Request, tool *v1alpha1.Tool) (runtimeapproval.Gate3Request, bool) {
		if st == nil || tool == nil {
			return runtimeapproval.Gate3Request{}, false
		}
		identityName := autoAgentIdentityName(request.Principal)
		identity, err := loadResource[v1alpha1.AgentIdentity](ctx, st, "AgentIdentity", request.Namespace, identityName)
		if err != nil {
			return runtimeapproval.Gate3Request{}, false
		}
		instance, err := loadResource[v1alpha1.AgentInstance](ctx, st, "AgentInstance", request.Namespace, request.SubjectName)
		if err != nil {
			return runtimeapproval.Gate3Request{}, false
		}
		canonical, err := canonicalizeInvocationInput(request.Input)
		if err != nil {
			return runtimeapproval.Gate3Request{}, false
		}
		return runtimeapproval.Gate3Request{
			Workspace:     request.Workspace,
			Namespace:     request.Namespace,
			InvocationID:  request.InvocationID,
			Plane:         v1alpha1.RuntimeApprovalPlaneTool,
			Operation:     "invoke",
			Action:        runtimeapproval.ActionToolInvocation,
			AgentName:     request.Principal,
			AgentIdentity: *identity,
			AgentInstance: *instance,
			SessionID:     request.SessionID,
			Target: v1alpha1.RuntimeApprovalReference{
				Kind:            "Tool",
				Scope:           "Namespaced",
				Namespace:       tool.Namespace,
				Name:            tool.Name,
				UID:             tool.UID,
				ResourceVersion: tool.ResourceVersion,
			},
			CanonicalPayload: canonical,
		}, true
	}
}

// canonicalizeInvocationInput marshals the invocation input to JSON and
// canonicalizes it so the resulting RuntimeApproval binds to a stable digest.
// A nil input map is treated as an empty JSON object rather than a null so
// two invocations that carry no explicit arguments still resolve to the same
// canonical payload.
func canonicalizeInvocationInput(input map[string]any) ([]byte, error) {
	if input == nil {
		input = map[string]any{}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	return runtimeapproval.CanonicalizeJSON(raw)
}

// autoAgentIdentityName mirrors the naming convention used by the Agent
// controller when it auto-provisions an AgentIdentity for an Agent. The Tool
// Gateway must resolve the same name so Gate 3 sees the identity that a
// running Agent instance is bound to. See
// internal/controller/agent_reconciler.go for the authoritative rule.
func autoAgentIdentityName(agentName string) string {
	const prefix = "agent-identity-"
	if len(prefix)+len(agentName) <= 63 {
		return prefix + agentName
	}
	digest := sha256.Sum256([]byte(agentName))
	const suffixLength = 1 + 8
	maxNameLength := 63 - len(prefix) - suffixLength
	trimmed := strings.TrimRight(agentName[:maxNameLength], "-.")
	if trimmed == "" {
		trimmed = "agent"
	}
	return fmt.Sprintf("%s%s-%x", prefix, trimmed, digest[:4])
}
