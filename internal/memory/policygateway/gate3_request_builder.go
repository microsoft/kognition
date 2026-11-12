// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package policygateway

import (
	"bytes"
	"context"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

// NewGate3RequestBuilder returns a production Gate3RequestBuilder that maps
// a Memory Policy Gateway invocation onto an ADR-0014 Gate 3 request. The
// MemoryClaim carries the persisted namespaced binding used as the exact
// approval target so a security reviewer approves the same
// (agent, memory claim, operation) tuple that will actually run. The
// invocation payload is canonicalized so the resulting RuntimeApproval binds
// to a stable digest.
//
// The builder returns ok=false when the invocation cannot be projected or
// canonicalized. The configured Gate 3 caller treats that result as
// unavailable so malformed writes cannot bypass approval and execution claim.
func NewGate3RequestBuilder() Gate3RequestBuilder {
	return func(ctx context.Context, invocation Gate3Invocation) (runtimeapproval.Gate3Request, bool) {
		if invocation.Claims == nil {
			return runtimeapproval.Gate3Request{}, false
		}
		canonical, err := canonicalizeInvocationPayload(invocation.CanonicalPayload)
		if err != nil {
			return runtimeapproval.Gate3Request{}, false
		}
		return runtimeapproval.Gate3Request{
			Workspace:     invocation.Namespace,
			Namespace:     invocation.Namespace,
			InvocationID:  invocation.InvocationID,
			Plane:         v1alpha1.RuntimeApprovalPlaneMemory,
			Operation:     invocation.Operation,
			Action:        runtimeapproval.ActionMemoryWrite,
			AgentName:     invocation.Claims.AgentName,
			AgentIdentity: invocation.Identity,
			AgentInstance: invocation.Instance,
			SessionID:     invocation.Claims.SessionID,
			Target: v1alpha1.RuntimeApprovalReference{
				Kind:            "MemoryClaim",
				Scope:           "Namespaced",
				Namespace:       invocation.Claim.Namespace,
				Name:            invocation.Claim.Name,
				UID:             invocation.Claim.UID,
				ResourceVersion: invocation.Claim.ResourceVersion,
			},
			CanonicalPayload: canonical,
		}, true
	}
}

// canonicalizeInvocationPayload canonicalizes the memory operation payload so
// the resulting RuntimeApproval binds to a stable digest. A nil or empty
// payload is treated as an empty JSON object rather than a null so two
// invocations that carry no explicit body still resolve to the same
// canonical payload.
func canonicalizeInvocationPayload(payload []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		trimmed = []byte("{}")
	}
	return runtimeapproval.CanonicalizeJSON(trimmed)
}
