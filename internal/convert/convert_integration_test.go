// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package convert_test

import (
	"encoding/json"
	"testing"

	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
)

// Integration tests verify that objects survive the full pipeline:
// internal -> K8s -> JSON -> K8s -> internal
// This catches issues where converters produce K8s objects that don't serialize
// correctly or where JSON tags differ from converter expectations.

func TestAgentJSONRoundTrip(t *testing.T) {
	replicas := int32(2)
	in := &api.Agent{
		TypeMeta:   api.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "agent-1", Namespace: "default", Labels: map[string]string{"env": "test"}},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "gpt4"},
			Instructions: api.AgentInstructions{Inline: "Be helpful"},
			Replicas:     &replicas,
			Tools: []api.AgentToolRef{
				{Ref: api.LocalObjectReference{Name: "tool-1"}, Required: true},
			},
		},
	}

	k8sObj := convert.AgentToK8s(in)

	data, err := json.Marshal(k8sObj)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var deserialized k8sapi.Agent
	if err := json.Unmarshal(data, &deserialized); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	out := convert.AgentFromK8s(&deserialized)

	if out.Name != "agent-1" || out.Namespace != "default" {
		t.Errorf("metadata lost: %s/%s", out.Namespace, out.Name)
	}
	if out.Spec.ModelRef.Name != "gpt4" {
		t.Errorf("ModelRef: got %q", out.Spec.ModelRef.Name)
	}
	if out.Spec.Replicas == nil || *out.Spec.Replicas != 2 {
		t.Errorf("Replicas: %v", out.Spec.Replicas)
	}
	if len(out.Spec.Tools) != 1 || out.Spec.Tools[0].Ref.Name != "tool-1" {
		t.Errorf("Tools: %+v", out.Spec.Tools)
	}
	if out.Labels["env"] != "test" {
		t.Errorf("Labels lost: %v", out.Labels)
	}
}

func TestToolJSONRoundTrip(t *testing.T) {
	in := &api.Tool{
		TypeMeta:   api.TypeMeta{Kind: "Tool", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "my-tool", Namespace: "ns"},
		Spec: api.ToolSpec{
			ClassName:   "http-class",
			Description: "Does things",
			Endpoint:    "https://example.com",
			SecretRefs: []api.ToolSecretRef{
				{Name: "secret-1", Key: "api-key"},
			},
			Sandbox: &api.ToolSandboxSpec{
				Timeout:  "5s",
				CPUMilli: 500,
				MemoryMB: 128,
			},
		},
	}

	k8sObj := convert.ToolToK8s(in)
	data, err := json.Marshal(k8sObj)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var deserialized k8sapi.Tool
	if err := json.Unmarshal(data, &deserialized); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	out := convert.ToolFromK8s(&deserialized)

	if out.Spec.ClassName != "http-class" {
		t.Errorf("ClassName: %q", out.Spec.ClassName)
	}
	if len(out.Spec.SecretRefs) != 1 || out.Spec.SecretRefs[0].Name != "secret-1" || out.Spec.SecretRefs[0].Key != "api-key" {
		t.Errorf("SecretRefs: %+v", out.Spec.SecretRefs)
	}
	if out.Spec.Sandbox == nil || out.Spec.Sandbox.CPUMilli != 500 || out.Spec.Sandbox.MemoryMB != 128 {
		t.Errorf("Sandbox: %+v", out.Spec.Sandbox)
	}
}

func TestWorkflowJSONRoundTrip(t *testing.T) {
	in := &api.Workflow{
		TypeMeta:   api.TypeMeta{Kind: "Workflow", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "wf-1", Namespace: "ns"},
		Spec: api.WorkflowSpec{
			Steps: []api.WorkflowStep{
				{Name: "step1", AgentRef: api.LocalObjectReference{Name: "agent-a"}},
				{Name: "step2", AgentRef: api.LocalObjectReference{Name: "agent-b"}},
			},
			Edges:   []api.WorkflowEdge{{From: "step1", To: "step2"}},
			Budgets: &api.WorkflowBudget{MaxTokens: 100000, MaxIterations: 10},
		},
	}

	k8sObj := convert.WorkflowToK8s(in)
	data, err := json.Marshal(k8sObj)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var deserialized k8sapi.Workflow
	if err := json.Unmarshal(data, &deserialized); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	out := convert.WorkflowFromK8s(&deserialized)

	if len(out.Spec.Steps) != 2 {
		t.Fatalf("Steps: got %d", len(out.Spec.Steps))
	}
	if len(out.Spec.Edges) != 1 || out.Spec.Edges[0].From != "step1" || out.Spec.Edges[0].To != "step2" {
		t.Errorf("Edges: %+v", out.Spec.Edges)
	}
	if out.Spec.Budgets == nil || out.Spec.Budgets.MaxTokens != 100000 {
		t.Errorf("Budgets: %+v", out.Spec.Budgets)
	}
}

func TestCognitiveQuotaJSONRoundTrip(t *testing.T) {
	in := &api.CognitiveQuota{
		TypeMeta:   api.TypeMeta{Kind: "CognitiveQuota", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "quota-1", Namespace: "ns"},
		Spec: api.CognitiveQuotaSpec{
			Hard:        map[string]int64{"agents": 10, "tools": 50},
			TokenBudget: &api.TokenBudget{MaxInputTokens: 500000, MaxOutputTokens: 200000, MaxTotalTokens: 700000},
			CostBudget:  &api.CostBudget{MaxCostPerPeriod: 10000, MaxToolCallsPerPeriod: 5000},
		},
		Status: api.CognitiveQuotaStatus{
			Used: map[string]int64{"agents": 3, "tools": 12},
		},
	}

	k8sObj := convert.CognitiveQuotaToK8s(in)
	data, err := json.Marshal(k8sObj)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var deserialized k8sapi.CognitiveQuota
	if err := json.Unmarshal(data, &deserialized); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	out := convert.CognitiveQuotaFromK8s(&deserialized)

	if out.Spec.Hard["agents"] != 10 || out.Spec.Hard["tools"] != 50 {
		t.Errorf("Hard: %v", out.Spec.Hard)
	}
	if out.Spec.TokenBudget.MaxTotalTokens != 700000 {
		t.Errorf("TokenBudget: %+v", out.Spec.TokenBudget)
	}
	if out.Spec.CostBudget.MaxCostPerPeriod != 10000 {
		t.Errorf("CostBudget: %+v", out.Spec.CostBudget)
	}
	if out.Status.Used["agents"] != 3 {
		t.Errorf("Used: %v", out.Status.Used)
	}
}

func TestAgentPolicyJSONRoundTrip(t *testing.T) {
	in := &api.AgentPolicy{
		TypeMeta:   api.TypeMeta{Kind: "AgentPolicy", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "policy-1", Namespace: "ns"},
		Spec: api.AgentPolicySpec{
			Rules:        "package kognition.policy\ndefault allow = false",
			Enforce:      true,
			Scope:        "agent",
			TargetAgents: []string{"agent-x"},
			SafetyRules: []api.GovernanceRule{
				{Name: "no-pii", Rego: "contains(output, 'SSN')", Severity: "error"},
			},
		},
	}

	k8sObj := convert.AgentPolicyToK8s(in)
	data, err := json.Marshal(k8sObj)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var deserialized k8sapi.AgentPolicy
	if err := json.Unmarshal(data, &deserialized); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	out := convert.AgentPolicyFromK8s(&deserialized)

	if out.Spec.TargetAgents[0] != "agent-x" {
		t.Errorf("TargetAgents: %v", out.Spec.TargetAgents)
	}
	if out.Spec.Rules != "package kognition.policy\ndefault allow = false" {
		t.Errorf("Rules: %q", out.Spec.Rules)
	}
	if len(out.Spec.SafetyRules) != 1 || out.Spec.SafetyRules[0].Name != "no-pii" {
		t.Errorf("SafetyRules: %+v", out.Spec.SafetyRules)
	}
	if !out.Spec.Enforce {
		t.Errorf("Enforce: expected true")
	}
}
