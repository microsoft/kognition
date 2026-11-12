// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package policygateway

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

func TestNewGate3RequestBuilderProjectsMemoryClaimTarget(t *testing.T) {
	builder := NewGate3RequestBuilder()

	claims := &runtimeauth.MemoryCapabilityClaims{
		Namespace:         "team-a",
		AgentName:         "planner",
		AgentIdentityName: "agent-identity-planner",
		InstanceName:      "planner-instance",
		InstanceUID:       "instance-uid",
		SessionID:         "session-1",
		VolumeHandle:      "volume-1",
	}
	identity := v1alpha1.AgentIdentity{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "agent-identity-planner", UID: "identity-uid", ResourceVersion: 3}}
	instance := v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "planner-instance", UID: "instance-uid", ResourceVersion: 7}}
	claim := v1alpha1.MemoryClaim{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "planner-claim", UID: "claim-uid", ResourceVersion: 11}}

	invocation := Gate3Invocation{
		Namespace:        "team-a",
		Operation:        "store",
		InvocationID:     "invocation-1",
		RunID:            "run-1",
		Claims:           claims,
		Identity:         identity,
		Instance:         instance,
		Claim:            claim,
		VolumeHandle:     "volume-1",
		CanonicalPayload: []byte(`{"volume_handle":"volume-1","content":"hello"}`),
	}

	request, ok := builder(context.Background(), invocation)
	if !ok {
		t.Fatalf("builder returned ok=false for a fully populated invocation")
	}

	if request.Plane != v1alpha1.RuntimeApprovalPlaneMemory {
		t.Fatalf("Plane = %q, want %q", request.Plane, v1alpha1.RuntimeApprovalPlaneMemory)
	}
	if request.Action != runtimeapproval.ActionMemoryWrite {
		t.Fatalf("Action = %q, want %q", request.Action, runtimeapproval.ActionMemoryWrite)
	}
	if request.Operation != "store" {
		t.Fatalf("Operation = %q, want %q", request.Operation, "store")
	}
	if request.AgentName != "planner" {
		t.Fatalf("AgentName = %q, want %q", request.AgentName, "planner")
	}
	if request.SessionID != "session-1" {
		t.Fatalf("SessionID = %q, want %q", request.SessionID, "session-1")
	}
	if request.InvocationID != "invocation-1" {
		t.Fatalf("InvocationID = %q, want %q", request.InvocationID, "invocation-1")
	}
	if request.Workspace != "team-a" || request.Namespace != "team-a" {
		t.Fatalf("Workspace/Namespace = %q/%q, want team-a/team-a", request.Workspace, request.Namespace)
	}
	if request.AgentIdentity.Name != "agent-identity-planner" || request.AgentIdentity.UID != "identity-uid" {
		t.Fatalf("AgentIdentity = %+v, want name=agent-identity-planner uid=identity-uid", request.AgentIdentity)
	}
	if request.AgentInstance.Name != "planner-instance" || request.AgentInstance.UID != "instance-uid" {
		t.Fatalf("AgentInstance = %+v, want name=planner-instance uid=instance-uid", request.AgentInstance)
	}
	want := v1alpha1.RuntimeApprovalReference{
		Kind:            "MemoryClaim",
		Scope:           "Namespaced",
		Namespace:       "team-a",
		Name:            "planner-claim",
		UID:             "claim-uid",
		ResourceVersion: 11,
	}
	if request.Target != want {
		t.Fatalf("Target = %+v, want %+v", request.Target, want)
	}

	// The canonical payload must be stable JSON parseable form of the input.
	var payload map[string]any
	if err := json.Unmarshal(request.CanonicalPayload, &payload); err != nil {
		t.Fatalf("CanonicalPayload is not valid JSON: %v", err)
	}
	if payload["volume_handle"] != "volume-1" || payload["content"] != "hello" {
		t.Fatalf("CanonicalPayload = %s, want to contain volume_handle=volume-1 and content=hello", string(request.CanonicalPayload))
	}
}

func TestNewGate3RequestBuilderTreatsEmptyPayloadAsEmptyObject(t *testing.T) {
	builder := NewGate3RequestBuilder()
	claims := &runtimeauth.MemoryCapabilityClaims{Namespace: "team-a", AgentName: "planner", SessionID: "session-1"}
	claim := v1alpha1.MemoryClaim{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "planner-claim", UID: "claim-uid", ResourceVersion: 1}}

	request, ok := builder(context.Background(), Gate3Invocation{
		Namespace:    "team-a",
		Operation:    "delete",
		InvocationID: "invocation-2",
		Claims:       claims,
		Claim:        claim,
		VolumeHandle: "volume-1",
	})
	if !ok {
		t.Fatalf("builder returned ok=false when CanonicalPayload was empty")
	}
	if string(request.CanonicalPayload) != "{}" {
		t.Fatalf("CanonicalPayload = %q, want %q", string(request.CanonicalPayload), "{}")
	}
}

func TestNewGate3RequestBuilderSkipsWhenClaimsNil(t *testing.T) {
	builder := NewGate3RequestBuilder()
	if _, ok := builder(context.Background(), Gate3Invocation{Namespace: "team-a", Operation: "store"}); ok {
		t.Fatalf("builder returned ok=true when Claims was nil")
	}
}

func TestNewGate3RequestBuilderSkipsWhenPayloadNotJSON(t *testing.T) {
	builder := NewGate3RequestBuilder()
	claims := &runtimeauth.MemoryCapabilityClaims{Namespace: "team-a", AgentName: "planner"}
	if _, ok := builder(context.Background(), Gate3Invocation{
		Namespace:        "team-a",
		Operation:        "store",
		Claims:           claims,
		CanonicalPayload: []byte("not-json"),
	}); ok {
		t.Fatalf("builder returned ok=true when CanonicalPayload was invalid JSON")
	}
}
