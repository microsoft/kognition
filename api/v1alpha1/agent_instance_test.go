// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAgentInstance_ObjectInterface(t *testing.T) {
	inst := &AgentInstance{
		TypeMeta:   TypeMeta{APIVersion: APIVersion, Kind: "AgentInstance"},
		ObjectMeta: ObjectMeta{Name: "test-inst", Namespace: "default"},
	}
	// Verify it satisfies the Object interface.
	var _ Object = inst

	if got := inst.GetTypeMeta(); got.Kind != "AgentInstance" {
		t.Errorf("GetTypeMeta().Kind = %q, want %q", got.Kind, "AgentInstance")
	}
	if got := inst.GetObjectMeta(); got.Name != "test-inst" {
		t.Errorf("GetObjectMeta().Name = %q, want %q", got.Name, "test-inst")
	}
}

func TestAgentInstanceSpec_JSONRoundtrip(t *testing.T) {
	spec := AgentInstanceSpec{
		AgentRef: LocalObjectReference{Name: "my-agent"},
		Revision: 3,
		SessionContext: map[string]string{
			"user_id":    "u-123",
			"session_id": "sess-abc",
		},
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got AgentInstanceSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.AgentRef.Name != spec.AgentRef.Name {
		t.Errorf("AgentRef.Name = %q, want %q", got.AgentRef.Name, spec.AgentRef.Name)
	}
	if got.Revision != spec.Revision {
		t.Errorf("Revision = %d, want %d", got.Revision, spec.Revision)
	}
	if len(got.SessionContext) != 2 {
		t.Fatalf("SessionContext len = %d, want 2", len(got.SessionContext))
	}
	if got.SessionContext["user_id"] != "u-123" {
		t.Errorf("SessionContext[user_id] = %q, want %q", got.SessionContext["user_id"], "u-123")
	}
}

func TestAgentInstanceSpec_OmitsEmptySessionContext(t *testing.T) {
	spec := AgentInstanceSpec{
		AgentRef: LocalObjectReference{Name: "my-agent"},
		Revision: 1,
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// sessionContext should not appear in JSON when nil.
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	if _, exists := raw["sessionContext"]; exists {
		t.Error("expected sessionContext to be omitted when nil")
	}
}

func TestAgentInstanceStatus_JSONRoundtrip_AllFields(t *testing.T) {
	now := time.Date(2026, 5, 21, 10, 30, 0, 0, time.UTC)
	status := AgentInstanceStatus{
		Phase: PhaseRunning,
		Conditions: []Condition{
			{Type: "Ready", Status: "True", LastTransitionTime: now},
		},
		SessionID: "sess-xyz",
		StartTime: &now,
		TokenUsage: &TokenUsage{
			PromptTokens:     150,
			CompletionTokens: 80,
			TotalTokens:      230,
		},
		ToolCalls: 5,
		ToolCallsDetail: []ToolCallDetail{{
			Name: "calculator", Args: `{"expression":"17 * 23"}`, Result: "391", DurationMS: 12,
		}},
	}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got AgentInstanceStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Phase != PhaseRunning {
		t.Errorf("Phase = %q, want %q", got.Phase, PhaseRunning)
	}
	if got.SessionID != "sess-xyz" {
		t.Errorf("SessionID = %q, want %q", got.SessionID, "sess-xyz")
	}
	if got.StartTime == nil || !got.StartTime.Equal(now) {
		t.Errorf("StartTime = %v, want %v", got.StartTime, now)
	}
	if got.TokenUsage == nil {
		t.Fatal("TokenUsage is nil")
	}
	if got.TokenUsage.PromptTokens != 150 {
		t.Errorf("PromptTokens = %d, want 150", got.TokenUsage.PromptTokens)
	}
	if got.TokenUsage.CompletionTokens != 80 {
		t.Errorf("CompletionTokens = %d, want 80", got.TokenUsage.CompletionTokens)
	}
	if got.TokenUsage.TotalTokens != 230 {
		t.Errorf("TotalTokens = %d, want 230", got.TokenUsage.TotalTokens)
	}
	if got.ToolCalls != 5 {
		t.Errorf("ToolCalls = %d, want 5", got.ToolCalls)
	}
	if len(got.ToolCallsDetail) != 1 || got.ToolCallsDetail[0].Name != "calculator" || got.ToolCallsDetail[0].Result != "391" {
		t.Errorf("ToolCallsDetail = %#v, want calculator result", got.ToolCallsDetail)
	}
	if len(got.Conditions) != 1 {
		t.Fatalf("Conditions len = %d, want 1", len(got.Conditions))
	}
}

func TestAgentInstanceStatus_OmitsZeroFields(t *testing.T) {
	status := AgentInstanceStatus{
		Phase: PhasePending,
	}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)

	for _, field := range []string{"startTime", "tokenUsage", "toolCalls", "sessionID", "conditions"} {
		if _, exists := raw[field]; exists {
			t.Errorf("expected %q to be omitted when zero/nil", field)
		}
	}

	// Phase should be present.
	if _, exists := raw["phase"]; !exists {
		t.Error("expected phase to be present")
	}
}

func TestTokenUsage_JSONRoundtrip(t *testing.T) {
	usage := TokenUsage{
		PromptTokens:     1024,
		CompletionTokens: 512,
		TotalTokens:      1536,
	}

	data, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got TokenUsage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got != usage {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", got, usage)
	}
}

func TestTokenUsage_OmitsZeroTokens(t *testing.T) {
	usage := TokenUsage{}

	data, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// All fields should be omitted when zero.
	if string(data) != "{}" {
		t.Errorf("expected empty JSON object, got %s", string(data))
	}
}

func TestAgentInstance_FullJSONRoundtrip(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	inst := AgentInstance{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: "AgentInstance"},
		ObjectMeta: ObjectMeta{
			Name:              "my-agent-abc123",
			Namespace:         "production",
			UID:               "uid-001",
			ResourceVersion:   5,
			CreationTimestamp: now,
			OwnerReferences: []OwnerReference{
				{APIVersion: APIVersion, Kind: "Agent", Name: "my-agent", UID: "uid-parent"},
			},
		},
		Spec: AgentInstanceSpec{
			AgentRef:       LocalObjectReference{Name: "my-agent"},
			Revision:       2,
			SessionContext: map[string]string{"tenant": "acme"},
		},
		Status: AgentInstanceStatus{
			Phase:     PhaseRunning,
			SessionID: "sess-full",
			StartTime: &now,
			ProbeStatus: &ProbeStatus{
				ReasoningOperationID:   "canary-operation",
				ReasoningRequestDigest: "sha256:request",
				ReasoningBindings: []CanaryResourceBinding{{
					Kind: "Model", Namespace: "production", Name: "model", UID: "model-uid", ResourceVersion: "7",
				}},
				LastReasoningResult: &ReasoningCanaryResult{
					OperationID: "canary-operation", ExpectedBytes: 20, ActualBytes: 20,
					ExpectedSHA256: "sha256:expected", ActualSHA256: "sha256:actual",
					PromptTokens: 31, CompletionTokens: 7, TotalTokens: 38,
					Outcome: "Passed",
				},
			},
			TokenUsage: &TokenUsage{
				PromptTokens:     200,
				CompletionTokens: 100,
				TotalTokens:      300,
			},
			ToolCalls: 3,
		},
	}

	data, err := json.Marshal(inst)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got AgentInstance
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify key fields survived the roundtrip.
	if got.TypeMeta.Kind != "AgentInstance" {
		t.Errorf("Kind = %q", got.TypeMeta.Kind)
	}
	if got.ObjectMeta.Name != "my-agent-abc123" {
		t.Errorf("Name = %q", got.ObjectMeta.Name)
	}
	if got.Spec.SessionContext["tenant"] != "acme" {
		t.Errorf("SessionContext[tenant] = %q", got.Spec.SessionContext["tenant"])
	}
	if got.Status.ToolCalls != 3 {
		t.Errorf("ToolCalls = %d", got.Status.ToolCalls)
	}
	if got.Status.TokenUsage.TotalTokens != 300 {
		t.Errorf("TotalTokens = %d", got.Status.TokenUsage.TotalTokens)
	}
	if got.Status.ProbeStatus == nil || got.Status.ProbeStatus.LastReasoningResult == nil || got.Status.ProbeStatus.LastReasoningResult.OperationID != "canary-operation" || got.Status.ProbeStatus.ReasoningBindings[0].UID != "model-uid" {
		t.Errorf("ProbeStatus reasoning metadata = %#v", got.Status.ProbeStatus)
	}
	if !got.Status.StartTime.Equal(now) {
		t.Errorf("StartTime = %v, want %v", got.Status.StartTime, now)
	}
}
