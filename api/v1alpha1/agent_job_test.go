// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAgentJob_ObjectInterface(t *testing.T) {
	job := &AgentJob{
		TypeMeta:   TypeMeta{APIVersion: APIVersion, Kind: "AgentJob"},
		ObjectMeta: ObjectMeta{Name: "job-1", Namespace: "default"},
	}

	var _ Object = job

	if got := job.GetTypeMeta(); got.Kind != "AgentJob" {
		t.Errorf("GetTypeMeta().Kind = %q, want %q", got.Kind, "AgentJob")
	}
	if got := job.GetObjectMeta(); got.Name != "job-1" {
		t.Errorf("GetObjectMeta().Name = %q, want %q", got.Name, "job-1")
	}
}

func TestAgentJobSpec_JSONRoundtrip_AllFields(t *testing.T) {
	backoff := int32(5)
	spec := AgentJobSpec{
		AgentRef:         &LocalObjectReference{Name: "my-agent"},
		AgentSpec:        &AgentSpec{Instructions: AgentInstructions{Inline: "Do work"}},
		CompletionPolicy: CompletionPolicySuccess,
		BackoffLimit:     &backoff,
		Input:            "process this payload",
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got AgentJobSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.AgentRef == nil || got.AgentRef.Name != "my-agent" {
		t.Fatalf("AgentRef = %+v, want name my-agent", got.AgentRef)
	}
	if got.AgentSpec == nil || got.AgentSpec.Instructions.Inline != "Do work" {
		t.Fatalf("AgentSpec = %+v, want inline instructions", got.AgentSpec)
	}
	if got.CompletionPolicy != CompletionPolicySuccess {
		t.Errorf("CompletionPolicy = %q, want %q", got.CompletionPolicy, CompletionPolicySuccess)
	}
	if got.BackoffLimit == nil || *got.BackoffLimit != 5 {
		t.Fatalf("BackoffLimit = %+v, want 5", got.BackoffLimit)
	}
	if got.Input != "process this payload" {
		t.Errorf("Input = %q, want %q", got.Input, "process this payload")
	}
}

func TestAgentJobSpec_BackwardCompatibleInputOnly(t *testing.T) {
	spec := AgentJobSpec{Input: "legacy-input"}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got AgentJobSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Input != "legacy-input" {
		t.Errorf("Input = %q, want %q", got.Input, "legacy-input")
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, exists := raw["input"]; !exists {
		t.Fatal("expected input to be present for legacy compatibility")
	}
}

func TestAgentJobSpec_OmitsEmptyOptionalFields(t *testing.T) {
	spec := AgentJobSpec{}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}

	for _, field := range []string{"agentRef", "agentSpec", "completionPolicy", "backoffLimit", "input"} {
		if _, exists := raw[field]; exists {
			t.Errorf("expected %q to be omitted when empty", field)
		}
	}
}

func TestAgentJobCompletionPolicy_EnumValuesRoundtrip(t *testing.T) {
	policies := []AgentJobCompletionPolicy{CompletionPolicySuccess, CompletionPolicyAny}
	for _, policy := range policies {
		data, err := json.Marshal(policy)
		if err != nil {
			t.Fatalf("marshal %q: %v", policy, err)
		}

		var got AgentJobCompletionPolicy
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal %q: %v", policy, err)
		}

		if got != policy {
			t.Errorf("roundtrip policy = %q, want %q", got, policy)
		}
	}
}

func TestAgentJobStatus_JSONRoundtrip_AllFields(t *testing.T) {
	now := time.Date(2026, 5, 21, 11, 0, 0, 0, time.UTC)
	startedAt := now.Add(-30 * time.Second)
	status := AgentJobStatus{
		Phase:          PhaseReady,
		Conditions:     []Condition{{Type: "Complete", Status: "True", LastTransitionTime: now}},
		Succeeded:      1,
		Failed:         0,
		StartTime:      &startedAt,
		CompletionTime: &now,
		TokenUsage:     &TokenUsage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20},
		ToolCalls:      2,
		Output:         "done",
	}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got AgentJobStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Phase != PhaseReady {
		t.Errorf("Phase = %q, want %q", got.Phase, PhaseReady)
	}
	if got.Succeeded != 1 {
		t.Errorf("Succeeded = %d, want 1", got.Succeeded)
	}
	if got.Failed != 0 {
		t.Errorf("Failed = %d, want 0", got.Failed)
	}
	if got.StartTime == nil || !got.StartTime.Equal(startedAt) {
		t.Errorf("StartTime = %v, want %v", got.StartTime, startedAt)
	}
	if got.CompletionTime == nil || !got.CompletionTime.Equal(now) {
		t.Errorf("CompletionTime = %v, want %v", got.CompletionTime, now)
	}
	if got.TokenUsage == nil || got.TokenUsage.PromptTokens != 12 || got.TokenUsage.CompletionTokens != 8 || got.TokenUsage.TotalTokens != 20 {
		t.Errorf("TokenUsage = %+v, want prompt=12 completion=8 total=20", got.TokenUsage)
	}
	if got.ToolCalls != 2 {
		t.Errorf("ToolCalls = %d, want 2", got.ToolCalls)
	}
	if got.Output != "done" {
		t.Errorf("Output = %q, want %q", got.Output, "done")
	}
	if len(got.Conditions) != 1 {
		t.Fatalf("Conditions len = %d, want 1", len(got.Conditions))
	}
}

func TestAgentJobStatus_OmitsZeroOptionalFields(t *testing.T) {
	status := AgentJobStatus{Phase: PhasePending}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}

	for _, field := range []string{"conditions", "succeeded", "failed", "startTime", "completionTime", "tokenUsage", "toolCalls", "output"} {
		if _, exists := raw[field]; exists {
			t.Errorf("expected %q to be omitted when zero/nil", field)
		}
	}

	if _, exists := raw["phase"]; !exists {
		t.Error("expected phase to be present")
	}
}
