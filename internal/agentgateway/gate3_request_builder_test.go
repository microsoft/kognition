// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentgateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

// TestNewGate3RequestBuilderProjectsDelegationTarget discharges the
// production contract of NewGate3RequestBuilder used by
// cmd/kognition-agent-communication-gateway: given a validated session
// capability, a ready source AgentIdentity, and a persisted source
// AgentInstance, the builder MUST project capability issuance onto a
// Gate3Request whose Plane is Communication, whose Action is
// externalCommunication, whose Target is the AgentService reference with
// UID and ResourceVersion propagated from the resolved target, and whose
// CanonicalPayload is stable across map iteration order.
func TestNewGate3RequestBuilderProjectsDelegationTarget(t *testing.T) {
	st := &testStore{data: map[string][]byte{}}
	instance := v1alpha1.AgentInstance{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "AgentInstance", APIVersion: v1alpha1.APIVersion},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "planner-r1", UID: "instance-uid", ResourceVersion: 7},
		Status:     v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhaseRunning, SessionID: "session-1"},
	}
	storeObject(t, st, "AgentInstance", "team-a", "planner-r1", instance)

	identity := &v1alpha1.AgentIdentity{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "AgentIdentity", APIVersion: v1alpha1.APIVersion},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "agent-identity-planner", UID: "identity-uid", ResourceVersion: 4},
	}
	claims := &runtimeauth.AgentDelegationSessionCapabilityClaims{
		Namespace:         "team-a",
		AgentName:         "planner",
		AgentIdentityName: "agent-identity-planner",
		InstanceName:      "planner-r1",
		InstanceUID:       "instance-uid",
		SessionID:         "session-1",
	}

	builder := NewGate3RequestBuilder(st)
	request, ok := builder(context.Background(), claims, identity, "delegation-1", "team-a", "peer-service", "reviewer", "peer-service-uid", 3, "please review this")
	if !ok {
		t.Fatalf("builder returned ok=false; want true")
	}

	if request.Workspace != "team-a" || request.Namespace != "team-a" || request.InvocationID != "delegation-1" {
		t.Fatalf("workspace/namespace/invocation binding wrong: %#v", request)
	}
	if request.Plane != v1alpha1.RuntimeApprovalPlaneCommunication {
		t.Fatalf("Plane=%q, want %q", request.Plane, v1alpha1.RuntimeApprovalPlaneCommunication)
	}
	if request.Operation != "issue-capability" {
		t.Fatalf("Operation=%q, want issue-capability", request.Operation)
	}
	if request.Action != runtimeapproval.ActionExternalCommunication {
		t.Fatalf("Action=%q, want %q", request.Action, runtimeapproval.ActionExternalCommunication)
	}
	if request.AgentName != "planner" || request.SessionID != "session-1" {
		t.Fatalf("agent binding wrong: name=%q session=%q", request.AgentName, request.SessionID)
	}
	if request.Target.Kind != "AgentService" || request.Target.Scope != "Namespaced" ||
		request.Target.Namespace != "team-a" || request.Target.Name != "peer-service" ||
		request.Target.UID != "peer-service-uid" || request.Target.ResourceVersion != 3 {
		t.Fatalf("Target reference wrong: %#v", request.Target)
	}
	if request.AgentIdentity.ObjectMeta.UID != "identity-uid" {
		t.Fatalf("AgentIdentity.UID=%q, want identity-uid", request.AgentIdentity.ObjectMeta.UID)
	}
	if request.AgentInstance.ObjectMeta.UID != "instance-uid" || request.AgentInstance.ObjectMeta.ResourceVersion != 7 {
		t.Fatalf("AgentInstance binding wrong: uid=%q rv=%d", request.AgentInstance.ObjectMeta.UID, request.AgentInstance.ObjectMeta.ResourceVersion)
	}

	// The canonical payload MUST be a stable digest that includes the
	// invocation binding and message so a reviewer sees the exact intent
	// they are approving.
	var payload map[string]any
	if err := json.Unmarshal(request.CanonicalPayload, &payload); err != nil {
		t.Fatalf("decode canonical payload: %v; raw=%s", err, string(request.CanonicalPayload))
	}
	if payload["invocation_id"] != "delegation-1" || payload["target_namespace"] != "team-a" ||
		payload["target_agent_service"] != "peer-service" || payload["target_agent"] != "reviewer" ||
		payload["message"] != "please review this" {
		t.Fatalf("canonical payload fields wrong: %v", payload)
	}

	// Contract: the canonical payload is stable across independent
	// invocations with the same inputs. This is the property Gate 3
	// binding depends on.
	requestB, ok := builder(context.Background(), claims, identity, "delegation-1", "team-a", "peer-service", "reviewer", "peer-service-uid", 3, "please review this")
	if !ok {
		t.Fatalf("builder second call returned ok=false; want true")
	}
	if string(request.CanonicalPayload) != string(requestB.CanonicalPayload) {
		t.Fatalf("canonical payload not stable across invocations:\n  a=%s\n  b=%s", request.CanonicalPayload, requestB.CanonicalPayload)
	}
}

// TestNewGate3RequestBuilderSkipsWhenInstanceMissing discharges the
// fail-closed skip contract: when the source AgentInstance cannot be
// resolved from the store, the builder MUST return ok=false so upstream
// capability validation (which already denied the invocation) remains the
// single source of denial evidence.
func TestNewGate3RequestBuilderSkipsWhenInstanceMissing(t *testing.T) {
	st := &testStore{data: map[string][]byte{}}
	builder := NewGate3RequestBuilder(st)
	identity := &v1alpha1.AgentIdentity{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "agent-identity-planner"}}
	claims := &runtimeauth.AgentDelegationSessionCapabilityClaims{Namespace: "team-a", AgentName: "planner", InstanceName: "planner-r1"}
	if _, ok := builder(context.Background(), claims, identity, "delegation-1", "team-a", "peer-service", "reviewer", "peer-service-uid", 3, "msg"); ok {
		t.Fatalf("builder returned ok=true with missing AgentInstance; want ok=false")
	}
}

// TestNewGate3RequestBuilderSkipsWhenClaimsOrIdentityNil enforces the
// upstream-precondition rule: nil claims or a nil identity means the
// invocation was already denied elsewhere and Gate 3 MUST NOT synthesize
// a spurious approval.
func TestNewGate3RequestBuilderSkipsWhenClaimsOrIdentityNil(t *testing.T) {
	st := &testStore{data: map[string][]byte{}}
	builder := NewGate3RequestBuilder(st)
	if _, ok := builder(context.Background(), nil, &v1alpha1.AgentIdentity{}, "delegation-1", "team-a", "peer-service", "reviewer", "peer-service-uid", 3, "msg"); ok {
		t.Fatalf("builder returned ok=true with nil claims; want ok=false")
	}
	if _, ok := builder(context.Background(), &runtimeauth.AgentDelegationSessionCapabilityClaims{}, nil, "delegation-1", "team-a", "peer-service", "reviewer", "peer-service-uid", 3, "msg"); ok {
		t.Fatalf("builder returned ok=true with nil identity; want ok=false")
	}
}

// TestNewGate3RequestBuilderSkipsWhenStoreNil enforces the same
// fail-closed contract when a deployment mis-configuration omits the
// shared store.
func TestNewGate3RequestBuilderSkipsWhenStoreNil(t *testing.T) {
	builder := NewGate3RequestBuilder(nil)
	if _, ok := builder(context.Background(), &runtimeauth.AgentDelegationSessionCapabilityClaims{}, &v1alpha1.AgentIdentity{}, "delegation-1", "team-a", "peer-service", "reviewer", "peer-service-uid", 3, "msg"); ok {
		t.Fatalf("builder returned ok=true with nil store; want ok=false")
	}
}
