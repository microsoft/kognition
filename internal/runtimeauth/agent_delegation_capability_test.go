// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"errors"
	"testing"
	"time"
)

func agentDelegationBinding() AgentDelegationCapabilityBinding {
	return AgentDelegationCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", Solution: "research", SolutionRev: 5,
		TargetNamespace: "team-b", TargetAgentService: "review-service", TargetAgent: "reviewer", InvocationID: "delegation-1",
		MessageDigest: "sha256:review-this",
	}
}

func TestAgentDelegationCapabilityIssuerVerifiesExactBinding(t *testing.T) {
	issuer, err := NewAgentDelegationCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuer.now = func() time.Time { return time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC) }
	binding := agentDelegationBinding()
	token, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := issuer.Verify(token, binding)
	if err != nil {
		t.Fatal(err)
	}
	if claims.AgentName != binding.AgentName || claims.TargetNamespace != binding.TargetNamespace || claims.TargetAgentService != binding.TargetAgentService || claims.TargetAgent != binding.TargetAgent || claims.InvocationID != binding.InvocationID || claims.MessageDigest != binding.MessageDigest {
		t.Fatalf("claims = %#v", claims)
	}
	binding.TargetAgentService = "other-service"
	if _, err := issuer.Verify(token, binding); !errors.Is(err, ErrCapabilityBindingMismatch) {
		t.Fatalf("different target service error = %v, want binding mismatch", err)
	}
}

func TestAgentDelegationCapabilityIssuerRejectsIncompleteConfiguration(t *testing.T) {
	if _, err := NewAgentDelegationCapabilityIssuer(make([]byte, 31), time.Minute); err == nil {
		t.Fatal("short key was accepted")
	}
	if _, err := NewAgentDelegationCapabilityIssuer(capabilityKey(), 0); err == nil {
		t.Fatal("zero TTL was accepted")
	}
	issuer, err := NewAgentDelegationCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Issue(AgentDelegationCapabilityBinding{}); err == nil {
		t.Fatal("incomplete binding was accepted")
	}
}

func TestAgentDelegationCapabilityIssuerRejectsExpiredAndCrossAudienceTokens(t *testing.T) {
	issuer, err := NewAgentDelegationCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	issuer.now = func() time.Time { return now }
	token, err := issuer.Issue(agentDelegationBinding())
	if err != nil {
		t.Fatal(err)
	}

	toolIssuer, err := NewToolCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	toolIssuer.now = issuer.now
	if _, err := toolIssuer.VerifyClaims(token); err == nil {
		t.Fatal("Tool capability issuer accepted delegation capability")
	}

	issuer.now = func() time.Time { return now.Add(time.Minute) }
	if _, err := issuer.VerifyClaims(token); err == nil {
		t.Fatal("expired delegation capability was accepted")
	}
}

func TestAgentDelegationCapabilityIssuerAcceptsPreviousRotationKey(t *testing.T) {
	oldRing, err := NewCapabilityKeyRing("old", rotationOldKey(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldIssuer, err := NewAgentDelegationCapabilityIssuerWithKeyRing(oldRing, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	binding := agentDelegationBinding()
	oldToken, err := oldIssuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	rotatingRing, err := NewCapabilityKeyRing("new", rotationNewKey(), "old", rotationOldKey())
	if err != nil {
		t.Fatal(err)
	}
	rotatingIssuer, err := NewAgentDelegationCapabilityIssuerWithKeyRing(rotatingRing, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotatingIssuer.Verify(oldToken, binding); err != nil {
		t.Fatalf("rotating issuer rejected previous capability: %v", err)
	}
}
