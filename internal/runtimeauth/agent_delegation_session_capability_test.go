// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"testing"
	"time"
)

func agentDelegationSessionBinding() AgentDelegationSessionCapabilityBinding {
	return AgentDelegationSessionCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", Solution: "research", SolutionRev: 5,
	}
}

func TestAgentDelegationSessionCapabilityIssuerVerifiesSourceSessionOnly(t *testing.T) {
	issuer, err := NewAgentDelegationSessionCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuer.now = func() time.Time { return time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC) }
	binding := agentDelegationSessionBinding()
	token, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := issuer.VerifyClaims(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.AgentName != binding.AgentName || claims.InstanceUID != binding.InstanceUID || claims.SessionID != binding.SessionID {
		t.Fatalf("claims = %#v", claims)
	}

	exactIssuer, err := NewAgentDelegationCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	exactIssuer.now = issuer.now
	if _, err := exactIssuer.VerifyClaims(token); err == nil {
		t.Fatal("exact delegation issuer accepted source session capability")
	}
}

func TestAgentDelegationSessionCapabilityIssuerRejectsIncompleteConfiguration(t *testing.T) {
	if _, err := NewAgentDelegationSessionCapabilityIssuer(make([]byte, 31), time.Minute); err == nil {
		t.Fatal("short key was accepted")
	}
	issuer, err := NewAgentDelegationSessionCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Issue(AgentDelegationSessionCapabilityBinding{}); err == nil {
		t.Fatal("incomplete binding was accepted")
	}
}
