// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// TC-0018: Invalid modelRef → admission rejects
// ---------------------------------------------------------------------------

func TestAgentValidator_RejectsEmptyModelRef(t *testing.T) {
	v := NewAgentValidator()
	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "bad-agent",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: ""},
			Instructions: v1alpha1.AgentInstructions{Inline: "hello"},
		},
	}
	data, _ := json.Marshal(agent)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "bad-agent",
		Object:    data,
	}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for empty modelRef.name")
	}
	if resp.Reason == "" {
		t.Error("expected denial reason")
	}
}

func TestAgentValidator_RejectsNoInstructions(t *testing.T) {
	v := NewAgentValidator()
	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "no-instructions",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{}, // neither inline nor ref
		},
	}
	data, _ := json.Marshal(agent)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "no-instructions",
		Object:    data,
	}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for missing instructions")
	}
}

func TestAgentValidator_RejectsBothInlineAndRef(t *testing.T) {
	v := NewAgentValidator()
	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "dual-instructions",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef: v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{
				Inline: "hello",
				Ref:    &v1alpha1.LocalObjectReference{Name: "some-ref"},
			},
		},
	}
	data, _ := json.Marshal(agent)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "dual-instructions",
		Object:    data,
	}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for both inline and ref instructions")
	}
}

func TestAgentValidator_RejectsNegativeReplicas(t *testing.T) {
	v := NewAgentValidator()
	neg := int32(-1)
	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "neg-replicas",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{Inline: "hello"},
			Replicas:     &neg,
		},
	}
	data, _ := json.Marshal(agent)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "neg-replicas",
		Object:    data,
	}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for negative replicas")
	}
}

func TestAgentValidator_RejectsEmptyToolRef(t *testing.T) {
	v := NewAgentValidator()
	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "empty-tool",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{Inline: "hello"},
			Tools:        []v1alpha1.AgentToolRef{{Ref: v1alpha1.LocalObjectReference{Name: ""}}},
		},
	}
	data, _ := json.Marshal(agent)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "empty-tool",
		Object:    data,
	}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for empty tool ref name")
	}
}

func TestAgentValidator_AcceptsValidAgent(t *testing.T) {
	v := NewAgentValidator()
	replicas := int32(2)
	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "good-agent",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{Inline: "You are helpful."},
			Replicas:     &replicas,
			Tools:        []v1alpha1.AgentToolRef{{Ref: v1alpha1.LocalObjectReference{Name: "search"}}},
			Companions:   []v1alpha1.LocalObjectReference{{Name: "buddy"}},
		},
	}
	data, _ := json.Marshal(agent)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "good-agent",
		Object:    data,
	}

	resp := v.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected valid agent to be accepted, got reason: %s", resp.Reason)
	}
}

func TestAgentValidator_DoesNotHandleOtherKinds(t *testing.T) {
	v := NewAgentValidator()
	if v.Handles("Tool", OperationCreate) {
		t.Error("validator should not handle Tool kind")
	}
}

func TestAgentValidator_RejectsEmptyCompanionName(t *testing.T) {
	v := NewAgentValidator()
	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "empty-companion",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{Inline: "hello"},
			Companions:   []v1alpha1.LocalObjectReference{{Name: ""}},
		},
	}
	data, _ := json.Marshal(agent)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "empty-companion",
		Object:    data,
	}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for empty companion name")
	}
}

func TestAgentValidator_RejectsEmptyKnowledgeBaseName(t *testing.T) {
	v := NewAgentValidator()
	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "empty-kb",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:            v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions:        v1alpha1.AgentInstructions{Inline: "hello"},
			KnowledgeBaseClaims: []v1alpha1.KnowledgeBaseClaimRef{{Name: ""}},
		},
	}
	data, _ := json.Marshal(agent)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "empty-kb",
		Object:    data,
	}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for empty knowledgeBaseClaims name")
	}
}

// ---------------------------------------------------------------------------
// Defaulter tests
// ---------------------------------------------------------------------------

func TestAgentDefaulter_DefaultsReplicas(t *testing.T) {
	d := NewAgentDefaulter()
	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "no-replicas",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{Inline: "hello"},
			// Replicas is nil
		},
	}
	data, _ := json.Marshal(agent)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "no-replicas",
		Object:    data,
	}

	resp := d.Mutate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got reason: %s", resp.Reason)
	}
	if resp.Patch == nil {
		t.Fatal("expected patch with defaults")
	}

	var patched v1alpha1.Agent
	json.Unmarshal(resp.Patch, &patched)
	if patched.Spec.Replicas == nil || *patched.Spec.Replicas != 1 {
		t.Errorf("expected replicas=1, got %v", patched.Spec.Replicas)
	}
}

func TestAgentDefaulter_DefaultsRuntime(t *testing.T) {
	d := NewAgentDefaulter()
	replicas := int32(3)
	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "no-runtime",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{Inline: "hello"},
			Replicas:     &replicas, // already set
			// Runtime is nil
		},
	}
	data, _ := json.Marshal(agent)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "no-runtime",
		Object:    data,
	}

	resp := d.Mutate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got reason: %s", resp.Reason)
	}
	if resp.Patch == nil {
		t.Fatal("expected patch with runtime default")
	}

	var patched v1alpha1.Agent
	json.Unmarshal(resp.Patch, &patched)
	if patched.Spec.Runtime == nil {
		t.Fatal("expected runtime to be defaulted")
	}
	if patched.Spec.Runtime.Class != "default" {
		t.Errorf("expected runtime.class=%q, got %q", "default", patched.Spec.Runtime.Class)
	}
}

func TestAgentDefaulter_NoOpWhenAlreadySet(t *testing.T) {
	d := NewAgentDefaulter()
	replicas := int32(5)
	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "fully-set",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{Inline: "hello"},
			Replicas:     &replicas,
			Runtime:      &v1alpha1.AgentRuntimeSpec{Class: "custom"},
		},
	}
	data, _ := json.Marshal(agent)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "fully-set",
		Object:    data,
	}

	resp := d.Mutate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got reason: %s", resp.Reason)
	}
	if resp.Patch != nil {
		t.Error("expected no patch when all fields already set")
	}
}

func TestAgentDefaulter_DoesNotHandleUpdate(t *testing.T) {
	d := NewAgentDefaulter()
	if d.Handles("Agent", OperationUpdate) {
		t.Error("defaulter should not handle UPDATE")
	}
}

func TestAgentDefaulter_DefaultsRuntimeClassWhenEmpty(t *testing.T) {
	d := NewAgentDefaulter()
	replicas := int32(2)
	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "empty-class",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{Inline: "hello"},
			Replicas:     &replicas,
			Runtime:      &v1alpha1.AgentRuntimeSpec{Class: ""},
		},
	}
	data, _ := json.Marshal(agent)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "Agent",
		Namespace: "default",
		Name:      "empty-class",
		Object:    data,
	}

	resp := d.Mutate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got reason: %s", resp.Reason)
	}
	if resp.Patch == nil {
		t.Fatal("expected patch for empty runtime.class")
	}

	var patched v1alpha1.Agent
	json.Unmarshal(resp.Patch, &patched)
	if patched.Spec.Runtime == nil || patched.Spec.Runtime.Class != "default" {
		t.Errorf("expected runtime.class=%q, got %v", "default", patched.Spec.Runtime)
	}
}
