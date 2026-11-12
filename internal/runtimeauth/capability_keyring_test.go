// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func rotationOldKey() []byte { return []byte("0123456789abcdef0123456789abcdef") }
func rotationNewKey() []byte { return []byte("abcdef0123456789abcdef0123456789") }

func TestMemoryCapabilityRotationAcceptsPreviousKeyAndIssuesActiveKid(t *testing.T) {
	oldRing, err := NewCapabilityKeyRing("2026-08-old", rotationOldKey(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldIssuer, err := NewMemoryCapabilityIssuerWithKeyRing(oldRing, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	binding := MemoryCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", VolumeHandle: "team-a-memory",
	}
	oldToken, err := oldIssuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	rotatingRing, err := NewCapabilityKeyRing("2026-09-new", rotationNewKey(), "2026-08-old", rotationOldKey())
	if err != nil {
		t.Fatal(err)
	}
	rotatingIssuer, err := NewMemoryCapabilityIssuerWithKeyRing(rotatingRing, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotatingIssuer.Verify(oldToken, binding); err != nil {
		t.Fatalf("rotating issuer rejected previous capability: %v", err)
	}
	newToken, err := rotatingIssuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, err := new(jwt.Parser).ParseUnverified(newToken, jwt.MapClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Header["kid"] != "2026-09-new" {
		t.Fatalf("active capability kid = %#v, want 2026-09-new", parsed.Header["kid"])
	}
	if _, err := oldIssuer.Verify(newToken, binding); err == nil {
		t.Fatal("previous-only issuer accepted a capability signed by the active rotation key")
	}
}

func TestToolCapabilityRotationAcceptsPreviousSessionAndExactCapabilities(t *testing.T) {
	oldRing, err := NewCapabilityKeyRing("old", rotationOldKey(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldSession, err := NewToolSessionCapabilityIssuerWithKeyRing(oldRing, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	oldExact, err := NewToolCapabilityIssuerWithKeyRing(oldRing, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sessionBinding := ToolSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1"}
	exactBinding := ToolCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", ToolName: "crm-update", ToolResourceVersion: 7, Verb: "invoke", InvocationID: "invocation-1", InputDigest: "sha256:af6f5f23a70d6c0b4a4567f635f5694605c3f6f20cfb5c2947df47d585de60df"}
	oldSessionToken, err := oldSession.Issue(sessionBinding)
	if err != nil {
		t.Fatal(err)
	}
	oldExactToken, err := oldExact.Issue(exactBinding)
	if err != nil {
		t.Fatal(err)
	}
	rotatingRing, err := NewCapabilityKeyRing("new", rotationNewKey(), "old", rotationOldKey())
	if err != nil {
		t.Fatal(err)
	}
	rotatingSession, err := NewToolSessionCapabilityIssuerWithKeyRing(rotatingRing, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rotatingExact, err := NewToolCapabilityIssuerWithKeyRing(rotatingRing, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotatingSession.VerifyClaims(oldSessionToken); err != nil {
		t.Fatalf("rotating session issuer rejected previous capability: %v", err)
	}
	if _, err := rotatingExact.Verify(oldExactToken, exactBinding); err != nil {
		t.Fatalf("rotating exact issuer rejected previous capability: %v", err)
	}
}

func TestCapabilityKeyRingRejectsInvalidRotationConfiguration(t *testing.T) {
	if _, err := NewCapabilityKeyRing("active", rotationNewKey(), "active", rotationOldKey()); err == nil {
		t.Fatal("identical active and previous IDs were accepted")
	}
	if _, err := NewCapabilityKeyRing("active", rotationNewKey(), "previous", nil); err == nil {
		t.Fatal("previous key ID without material was accepted")
	}
	if _, err := NewConfiguredCapabilityKeyRing("", rotationNewKey(), "previous", nil); err != nil {
		t.Fatalf("empty optional previous key configuration failed: %v", err)
	}
}
