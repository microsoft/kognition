// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestModelSessionCapabilityBindsExactModelRevision(t *testing.T) {
	issuer, err := NewModelSessionCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuer.now = func() time.Time { return time.Date(2026, time.August, 20, 13, 0, 0, 0, time.UTC) }
	binding := ModelSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", WorkflowName: "research-flow", StepName: "plan", Solution: "research", SolutionRev: 5, ModelName: "gpt-4o", ModelResourceVersion: 9}
	token, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := issuer.VerifyClaims(token)
	if err != nil {
		t.Fatal(err)
	}
	if got := modelSessionBindingFromClaims(claims); got != binding {
		t.Fatalf("binding = %#v, want %#v", got, binding)
	}
	if claims.ID == "" {
		t.Fatal("issued capability has no audit identifier")
	}
	reissued, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	reissuedClaims, err := issuer.VerifyClaims(reissued)
	if err != nil {
		t.Fatal(err)
	}
	if reissuedClaims.ID == claims.ID {
		t.Fatal("separate capability issuances reused an audit identifier")
	}
	if _, err := issuer.Issue(ModelSessionCapabilityBinding{Namespace: binding.Namespace, AgentName: binding.AgentName, AgentIdentityName: binding.AgentIdentityName, InstanceName: binding.InstanceName, InstanceUID: binding.InstanceUID, SessionID: binding.SessionID, ModelName: binding.ModelName}); err == nil {
		t.Fatal("issuer accepted a model session capability without a model revision")
	}
}

func TestModelSessionCapabilityHasDedicatedAudience(t *testing.T) {
	issuer, err := NewModelSessionCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	token, err := issuer.Issue(ModelSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", ModelName: "gpt-4o", ModelResourceVersion: 9})
	if err != nil {
		t.Fatal(err)
	}
	toolIssuer, err := NewToolSessionCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := toolIssuer.VerifyClaims(token); err == nil {
		t.Fatal("model session capability was accepted as a Tool session capability")
	}
}

func TestModelSessionCapabilityIssuerExposesConfiguredTTL(t *testing.T) {
	issuer, err := NewModelSessionCapabilityIssuer(capabilityKey(), 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if issuer.TTL() != 90*time.Second {
		t.Fatalf("TTL() = %s, want 90s", issuer.TTL())
	}
}

func TestModelSessionCapabilityCarriesExactCandidateBinding(t *testing.T) {
	issuer, err := NewModelSessionCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	candidate := v1alpha1.ModelVersionBinding{
		Namespace: "team-a", ModelName: "candidate", ModelUID: "candidate-uid",
		ModelResourceVersion: 7, SpecDigest: "sha256:" + strings.Repeat("a", 64),
	}
	binding := ModelSessionCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "identity",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1",
		ModelName: "logical-model", ModelResourceVersion: 3, CandidateBinding: &candidate,
	}
	token, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := issuer.VerifyClaims(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.CandidateBinding == nil || *claims.CandidateBinding != candidate {
		t.Fatalf("candidate binding = %+v, want %+v", claims.CandidateBinding, candidate)
	}
	candidate.Namespace = "team-b"
	if _, err := issuer.Issue(binding); err == nil {
		t.Fatal("candidate from another workspace accepted")
	}
	candidate.Namespace = binding.Namespace
	candidate.SpecDigest = strings.Repeat("x", 71)
	if _, err := issuer.Issue(binding); err == nil {
		t.Fatal("invalid candidate digest accepted")
	}
}

func TestModelSessionCapabilityBindsOneWorkflowTaskAndItsExpiry(t *testing.T) {
	issuer, err := NewModelSessionCapabilityIssuer(capabilityKey(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.November, 23, 13, 0, 0, 0, time.UTC)
	issuer.now = func() time.Time { return now }
	task := &ModelSessionWorkflowTask{Workspace: "team-a", RunUID: "run-uid", Step: "plan", ActivationID: "act-1", Attempt: 1, InvocationID: "inv-1", TaskID: "task-1"}
	binding := ModelSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", ModelName: "gpt-4o", ModelResourceVersion: 9, WorkflowTask: task}
	token, err := issuer.IssueUntil(binding, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claims, err := issuer.VerifyClaims(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.WorkflowTask == nil || *claims.WorkflowTask != *task || !claims.ExpiresAt.Time.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("claims task=%#v expires=%v", claims.WorkflowTask, claims.ExpiresAt)
	}
	later, err := issuer.IssueUntil(binding, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if claims, err := issuer.VerifyClaims(later); err != nil || !claims.ExpiresAt.Time.Equal(now.Add(time.Hour)) {
		t.Fatalf("a later bound extended the TTL: %v %v", claims, err)
	}
	if _, err := issuer.IssueUntil(binding, now); err == nil {
		t.Fatal("issuer accepted a bound that already passed")
	}
	partial := *task
	partial.TaskID = ""
	binding.WorkflowTask = &partial
	if _, err := issuer.IssueUntil(binding, now.Add(time.Minute)); err == nil {
		t.Fatal("issuer accepted an incomplete workflow task binding")
	}
}
