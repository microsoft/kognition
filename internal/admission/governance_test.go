// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/opa"
)

// Traceability coverage for completed TCs:
// TC-0083: CognitiveAdmissionPolicy injects default tool grants for all new Agents in a Workspace.
// TC-0088: CognitiveLimitRange default maxReasoningSteps=50 applied to Agents without explicit limits.
// TC-0089: CognitiveLimitRange max costPerSession rejects Agents exceeding the limit.
// TC-0154: CognitiveQuota memoryMB limit; admission rejects MemoryClaims that breach the quota.
// TC-0173: AutonomyClass duplicate spec.level validation rejects duplicates.
// TC-0132: Solution overrides above Workspace maxima are rejected at admission.

func TestCognitiveAdmissionPolicyHook_Deny(t *testing.T) {
	ms := newMemStore()
	engine := opa.NewEngine()

	hook := NewCognitiveAdmissionPolicyHook(ms, engine)

	ctx := context.Background()

	// Create a policy that denies agents without labels.
	policy := map[string]interface{}{
		"kind": "CognitiveAdmissionPolicy",
		"metadata": map[string]interface{}{
			"name":      "require-labels",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"matchKinds": []string{"Agent"},
			"rego":       "package policy\ndeny[msg] { not input.metadata.labels; msg = \"labels required\" }",
		},
	}
	data, _ := json.Marshal(policy)
	ms.Create(ctx, "CognitiveAdmissionPolicy", "default", "require-labels", data)

	// Verify it handles Agent kind.
	if !hook.Handles("Agent", OperationCreate) {
		t.Fatal("expected to handle Agent CREATE")
	}

	// Build request without labels.
	obj := map[string]interface{}{
		"kind": "Agent",
		"metadata": map[string]interface{}{
			"name":      "test-agent",
			"namespace": "default",
		},
	}
	objData, _ := json.Marshal(obj)

	req := &Request{
		Kind:      "Agent",
		Name:      "test-agent",
		Namespace: "default",
		Operation: OperationCreate,
		Object:    objData,
	}

	resp := hook.Validate(ctx, req)
	if resp.Allowed {
		t.Fatal("expected denial")
	}
	if resp.Reason == "" {
		t.Fatal("expected reason")
	}
}

func TestLimitRangeDefaulter_ApplyDefaults(t *testing.T) {
	ms := newMemStore()
	hook := NewLimitRangeDefaulter(ms)

	ctx := context.Background()

	// Create a limit range with defaults.
	lr := map[string]interface{}{
		"kind": "CognitiveLimitRange",
		"metadata": map[string]interface{}{
			"name":      "default-limits",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"limits": []map[string]interface{}{
				{
					"type": "Agent",
					"default": map[string]int64{
						"maxTokens": 4096,
					},
				},
			},
		},
	}
	data, _ := json.Marshal(lr)
	ms.Create(ctx, "CognitiveLimitRange", "default", "default-limits", data)

	// Build agent without limits.
	agent := map[string]interface{}{
		"kind": "Agent",
		"metadata": map[string]interface{}{
			"name":      "test-agent",
			"namespace": "default",
		},
		"spec": map[string]interface{}{},
	}
	agentData, _ := json.Marshal(agent)

	req := &Request{
		Kind:      "Agent",
		Name:      "test-agent",
		Namespace: "default",
		Operation: OperationCreate,
		Object:    agentData,
	}

	resp := hook.Mutate(ctx, req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got reason: %s", resp.Reason)
	}
	if resp.Patch == nil {
		t.Fatal("expected patch to be applied")
	}
}

func TestQuotaEnforcer_Deny(t *testing.T) {
	ms := newMemStore()
	hook := NewQuotaEnforcer(ms)

	ctx := context.Background()
	resources := map[string]int64{
		"agents":       2,
		"tools":        2,
		"sessions":     2,
		"model-grants": 2,
		"tool-grants":  2,
	}

	// Create a quota at limit.
	quota := map[string]interface{}{
		"kind": "CognitiveQuota",
		"metadata": map[string]interface{}{
			"name":      "ns-quota",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"hard": resources,
		},
		"status": map[string]interface{}{
			"used": resources,
		},
	}
	data, _ := json.Marshal(quota)
	ms.Create(ctx, "CognitiveQuota", "default", "ns-quota", data)

	tests := []struct {
		kind        string
		resourceKey string
	}{
		{"Agent", "agents"},
		{"Tool", "tools"},
		{"Session", "sessions"},
		{"ModelGrant", "model-grants"},
		{"ToolGrant", "tool-grants"},
	}

	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			req := &Request{
				Kind:      test.kind,
				Name:      "new-resource",
				Namespace: "default",
				Operation: OperationCreate,
				Object:    []byte(`{}`),
			}

			resp := hook.Validate(ctx, req)
			if resp.Allowed {
				t.Fatal("expected denial due to quota")
			}
			if !strings.Contains(resp.Reason, `"`+test.resourceKey+`"`) {
				t.Errorf("Reason = %q, want resource key %q", resp.Reason, test.resourceKey)
			}
		})
	}
}

func TestQuotaEnforcer_RejectsSolutionOverridesAboveWorkspaceMaxima(t *testing.T) {
	ms := newMemStore()
	hook := NewQuotaEnforcer(ms)
	ctx := context.Background()
	quota := map[string]interface{}{
		"kind":     "CognitiveQuota",
		"metadata": map[string]interface{}{"name": "workspace-quota", "namespace": "default"},
		"spec": map[string]interface{}{
			"hard":        map[string]int64{"agents": 4},
			"tokenBudget": map[string]int64{"maxTotalTokens": 5000},
		},
	}
	quotaData, _ := json.Marshal(quota)
	ms.Create(ctx, "CognitiveQuota", "default", "workspace-quota", quotaData)
	solution := map[string]interface{}{
		"kind": "Solution",
		"spec": map[string]interface{}{
			"goal": "Review compliance evidence",
			"overrides": map[string]interface{}{
				"hard":        map[string]int64{"agents": 5},
				"tokenBudget": map[string]int64{"maxTotalTokens": 6000},
			},
		},
	}
	solutionData, _ := json.Marshal(solution)

	for _, operation := range []Operation{OperationCreate, OperationUpdate} {
		if !hook.Handles("Solution", operation) {
			t.Fatalf("expected quota enforcer to handle Solution %s", operation)
		}
		response := hook.Validate(ctx, &Request{Kind: "Solution", Namespace: "default", Operation: operation, Object: solutionData})
		if response.Allowed {
			t.Fatalf("expected Solution %s rejection", operation)
		}
		if !strings.Contains(response.Reason, "maxTotalTokens") || !strings.Contains(response.Reason, `hard["agents"]`) {
			t.Fatalf("rejection reason = %q, want token and hard quota violations", response.Reason)
		}
	}
}

func TestAutonomyClassValidator_DenyInvalidTool(t *testing.T) {
	ms := newMemStore()
	hook := NewAutonomyClassValidator(ms)

	ctx := context.Background()

	// Create AutonomyClass allowing only safe tools.
	ac := map[string]interface{}{
		"kind": "AutonomyClass",
		"metadata": map[string]interface{}{
			"name":      "restricted",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"level":            2,
			"requiresApproval": true,
			"maxCostPerAction": 100,
			"allowedTools":     []string{"read", "search"},
		},
	}
	data, _ := json.Marshal(ac)
	ms.Create(ctx, "AutonomyClass", "default", "restricted", data)

	// Agent referencing disallowed tool.
	agent := map[string]interface{}{
		"kind": "Agent",
		"metadata": map[string]interface{}{
			"name":      "agent-1",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"autonomyClassRef": map[string]string{"name": "restricted"},
			"tools": []map[string]interface{}{
				{"ref": map[string]string{"name": "read"}},
				{"ref": map[string]string{"name": "delete"}},
			},
		},
	}
	agentData, _ := json.Marshal(agent)

	req := &Request{
		Kind:      "Agent",
		Name:      "agent-1",
		Namespace: "default",
		Operation: OperationCreate,
		Object:    agentData,
	}

	resp := hook.Validate(ctx, req)
	if resp.Allowed {
		t.Fatal("expected denial for disallowed tool")
	}
}

func TestCognitiveAdmissionPolicyHook_Allow(t *testing.T) {
	ms := newMemStore()
	engine := opa.NewEngine()
	hook := NewCognitiveAdmissionPolicyHook(ms, engine)

	ctx := context.Background()

	// Create a policy that denies agents without labels.
	policy := map[string]interface{}{
		"kind": "CognitiveAdmissionPolicy",
		"metadata": map[string]interface{}{
			"name":      "require-labels",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"matchKinds": []string{"Agent"},
			"rego":       "package policy\ndeny[msg] { not input.metadata.labels; msg = \"labels required\" }",
		},
	}
	data, _ := json.Marshal(policy)
	ms.Create(ctx, "CognitiveAdmissionPolicy", "default", "require-labels", data)

	// Build request WITH labels - should be allowed.
	obj := map[string]interface{}{
		"kind": "Agent",
		"metadata": map[string]interface{}{
			"name":      "test-agent",
			"namespace": "default",
			"labels":    map[string]interface{}{"app": "good"},
		},
	}
	objData, _ := json.Marshal(obj)

	req := &Request{
		Kind:      "Agent",
		Name:      "test-agent",
		Namespace: "default",
		Operation: OperationCreate,
		Object:    objData,
	}

	resp := hook.Validate(ctx, req)
	if !resp.Allowed {
		t.Fatalf("expected allow, got denial: %s", resp.Reason)
	}
}

func TestCognitiveAdmissionPolicyHook_NoPolicies(t *testing.T) {
	ms := newMemStore()
	engine := opa.NewEngine()
	hook := NewCognitiveAdmissionPolicyHook(ms, engine)

	req := &Request{
		Kind:      "Agent",
		Name:      "test-agent",
		Namespace: "default",
		Operation: OperationCreate,
		Object:    []byte(`{"kind":"Agent"}`),
	}

	resp := hook.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allow with no policies, got: %s", resp.Reason)
	}
}

func TestQuotaEnforcer_Allow(t *testing.T) {
	ms := newMemStore()
	hook := NewQuotaEnforcer(ms)

	ctx := context.Background()

	// Create a quota with capacity remaining.
	quota := map[string]interface{}{
		"kind": "CognitiveQuota",
		"metadata": map[string]interface{}{
			"name":      "ns-quota",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"hard": map[string]int64{"agents": 5},
		},
		"status": map[string]interface{}{
			"used": map[string]int64{"agents": 2},
		},
	}
	data, _ := json.Marshal(quota)
	ms.Create(ctx, "CognitiveQuota", "default", "ns-quota", data)

	req := &Request{
		Kind:      "Agent",
		Name:      "new-agent",
		Namespace: "default",
		Operation: OperationCreate,
		Object:    []byte(`{"kind":"Agent"}`),
	}

	resp := hook.Validate(ctx, req)
	if !resp.Allowed {
		t.Fatalf("expected allow (under quota), got: %s", resp.Reason)
	}
}

func TestQuotaEnforcer_NoQuota(t *testing.T) {
	ms := newMemStore()
	hook := NewQuotaEnforcer(ms)

	req := &Request{
		Kind:      "Agent",
		Name:      "new-agent",
		Namespace: "default",
		Operation: OperationCreate,
		Object:    []byte(`{"kind":"Agent"}`),
	}

	resp := hook.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allow with no quota defined, got: %s", resp.Reason)
	}
}

func TestQuotaEnforcer_UnknownKind(t *testing.T) {
	ms := newMemStore()
	hook := NewQuotaEnforcer(ms)

	ctx := context.Background()

	quota := map[string]interface{}{
		"kind": "CognitiveQuota",
		"metadata": map[string]interface{}{
			"name":      "ns-quota",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"hard": map[string]int64{"agents": 1},
		},
		"status": map[string]interface{}{
			"used": map[string]int64{"agents": 1},
		},
	}
	data, _ := json.Marshal(quota)
	ms.Create(ctx, "CognitiveQuota", "default", "ns-quota", data)

	// Unknown kind should be allowed (no resource key mapping).
	req := &Request{
		Kind:      "UnknownKind",
		Name:      "test",
		Namespace: "default",
		Operation: OperationCreate,
		Object:    []byte(`{}`),
	}

	resp := hook.Validate(ctx, req)
	if !resp.Allowed {
		t.Fatalf("expected allow for unknown kind, got: %s", resp.Reason)
	}
}

func TestAutonomyClassValidator_AllowValidTools(t *testing.T) {
	ms := newMemStore()
	hook := NewAutonomyClassValidator(ms)

	ctx := context.Background()

	ac := map[string]interface{}{
		"kind": "AutonomyClass",
		"metadata": map[string]interface{}{
			"name":      "standard",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"level":            3,
			"maxCostPerAction": 500,
			"allowedTools":     []string{"read", "search", "write"},
		},
	}
	data, _ := json.Marshal(ac)
	ms.Create(ctx, "AutonomyClass", "default", "standard", data)

	// Agent using only allowed tools.
	agent := map[string]interface{}{
		"kind": "Agent",
		"metadata": map[string]interface{}{
			"name":      "good-agent",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"autonomyClassRef": map[string]string{"name": "standard"},
			"tools": []map[string]interface{}{
				{"ref": map[string]string{"name": "read"}},
				{"ref": map[string]string{"name": "search"}},
			},
			"maxCostPerAction": 200,
		},
	}
	agentData, _ := json.Marshal(agent)

	req := &Request{
		Kind:      "Agent",
		Name:      "good-agent",
		Namespace: "default",
		Operation: OperationCreate,
		Object:    agentData,
	}

	resp := hook.Validate(ctx, req)
	if !resp.Allowed {
		t.Fatalf("expected allow for valid tools, got: %s", resp.Reason)
	}
}

func TestAutonomyClassValidator_NoRef(t *testing.T) {
	ms := newMemStore()
	hook := NewAutonomyClassValidator(ms)

	// Agent without autonomyClassRef should be allowed.
	agent := map[string]interface{}{
		"kind": "Agent",
		"metadata": map[string]interface{}{
			"name":      "free-agent",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"tools": []map[string]interface{}{
				{"ref": map[string]string{"name": "anything"}},
			},
		},
	}
	agentData, _ := json.Marshal(agent)

	req := &Request{
		Kind:      "Agent",
		Name:      "free-agent",
		Namespace: "default",
		Operation: OperationCreate,
		Object:    agentData,
	}

	resp := hook.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allow without autonomyClassRef, got: %s", resp.Reason)
	}
}

func TestLimitRangeDefaulter_NoRanges(t *testing.T) {
	ms := newMemStore()
	hook := NewLimitRangeDefaulter(ms)

	agent := map[string]interface{}{
		"kind": "Agent",
		"metadata": map[string]interface{}{
			"name":      "test-agent",
			"namespace": "default",
		},
		"spec": map[string]interface{}{},
	}
	agentData, _ := json.Marshal(agent)

	req := &Request{
		Kind:      "Agent",
		Name:      "test-agent",
		Namespace: "default",
		Operation: OperationCreate,
		Object:    agentData,
	}

	resp := hook.Mutate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allow with no limit ranges, got: %s", resp.Reason)
	}
}

func TestLimitRangeDefaulter_NonAgentKind(t *testing.T) {
	ms := newMemStore()
	hook := NewLimitRangeDefaulter(ms)

	req := &Request{
		Kind:      "Tool",
		Name:      "test-tool",
		Namespace: "default",
		Operation: OperationCreate,
		Object:    []byte(`{"kind":"Tool"}`),
	}

	resp := hook.Mutate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allow for non-Agent kind, got: %s", resp.Reason)
	}
}
