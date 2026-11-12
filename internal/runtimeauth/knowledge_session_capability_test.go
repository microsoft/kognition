// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"slices"
	"testing"
	"time"
)

func TestKnowledgeSessionCapabilityBindsExactKnowledgeBases(t *testing.T) {
	issuer, err := NewKnowledgeSessionCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuer.now = func() time.Time { return time.Date(2026, time.September, 21, 13, 0, 0, 0, time.UTC) }
	binding := KnowledgeSessionCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentUID: "agent-uid",
		AgentIdentityName: "agent-identity-planner", AgentIdentityUID: "identity-uid",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1",
		KnowledgeBases: []string{"architecture", "operations"},
	}
	token, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := issuer.VerifyClaims(token)
	if err != nil {
		t.Fatal(err)
	}
	got := knowledgeSessionBindingFromClaims(claims)
	if got.Namespace != binding.Namespace || got.AgentName != binding.AgentName || got.InstanceUID != binding.InstanceUID || !slices.Equal(got.KnowledgeBases, binding.KnowledgeBases) {
		t.Fatalf("binding = %#v, want %#v", got, binding)
	}
	if claims.ID == "" {
		t.Fatal("issued capability has no audit identifier")
	}
}

func TestKnowledgeSessionCapabilityRejectsAmbiguousKnowledgeBases(t *testing.T) {
	issuer, err := NewKnowledgeSessionCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	base := KnowledgeSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentUID: "agent-uid", AgentIdentityName: "identity", AgentIdentityUID: "identity-uid", InstanceName: "planner-r1", InstanceUID: "uid", SessionID: "session-1"}
	for _, names := range [][]string{nil, {"z", "a"}, {"a", "a"}, {"INVALID"}} {
		binding := base
		binding.KnowledgeBases = names
		if _, err := issuer.Issue(binding); err == nil {
			t.Fatalf("Issue accepted KnowledgeBases %#v", names)
		}
	}
}

func TestKnowledgeSessionCapabilityHasDedicatedAudience(t *testing.T) {
	issuer, err := NewKnowledgeSessionCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	token, err := issuer.Issue(KnowledgeSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentUID: "agent-uid", AgentIdentityName: "identity", AgentIdentityUID: "identity-uid", InstanceName: "planner-r1", InstanceUID: "uid", SessionID: "session-1", KnowledgeBases: []string{"docs"}})
	if err != nil {
		t.Fatal(err)
	}
	modelIssuer, err := NewModelSessionCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := modelIssuer.VerifyClaims(token); err == nil {
		t.Fatal("knowledge session capability was accepted as a Model session capability")
	}
}
