// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// TC-0165: ToolGrant verb invoke allowed, inspect denied.
func TestToolGrantValidator_VerbInvokeAllowed_InspectDenied(t *testing.T) {
	grant := v1alpha1.ToolGrant{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolGrant"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "grant-invoke-only",
			Namespace: "default",
		},
		Spec: v1alpha1.ToolGrantSpec{
			ToolRef: v1alpha1.LocalObjectReference{Name: "web-search"},
			Verbs:   []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
		},
	}

	// Verify HasVerb works correctly.
	if !grant.Spec.HasVerb(v1alpha1.ToolGrantVerbInvoke) {
		t.Fatal("expected HasVerb(invoke) to be true")
	}
	if grant.Spec.HasVerb(v1alpha1.ToolGrantVerbInspect) {
		t.Fatal("expected HasVerb(inspect) to be false")
	}

	// Verify admission accepts the valid grant.
	v := NewToolGrantValidator()
	data, _ := json.Marshal(grant)
	req := &Request{Operation: OperationCreate, Kind: "ToolGrant", Namespace: "default", Name: "grant-invoke-only", Object: data}
	resp := v.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected grant to be accepted, got reason: %s", resp.Reason)
	}
}

// TC-0166: ToolGrant with invalid verb rejected by admission.
func TestToolGrantValidator_RejectsInvalidVerb(t *testing.T) {
	grant := v1alpha1.ToolGrant{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolGrant"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "grant-bad-verb",
			Namespace: "default",
		},
		Spec: v1alpha1.ToolGrantSpec{
			ToolRef: v1alpha1.LocalObjectReference{Name: "web-search"},
			Verbs:   []v1alpha1.ToolGrantVerb{"execute"},
		},
	}

	v := NewToolGrantValidator()
	data, _ := json.Marshal(grant)
	req := &Request{Operation: OperationCreate, Kind: "ToolGrant", Namespace: "default", Name: "grant-bad-verb", Object: data}
	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for invalid verb")
	}
}

func TestToolGrantValidator_RejectsEmptyVerbs(t *testing.T) {
	grant := v1alpha1.ToolGrant{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolGrant"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "grant-empty-verbs",
			Namespace: "default",
		},
		Spec: v1alpha1.ToolGrantSpec{
			ToolRef: v1alpha1.LocalObjectReference{Name: "web-search"},
			Verbs:   []v1alpha1.ToolGrantVerb{},
		},
	}

	v := NewToolGrantValidator()
	data, _ := json.Marshal(grant)
	req := &Request{Operation: OperationCreate, Kind: "ToolGrant", Namespace: "default", Name: "grant-empty-verbs", Object: data}
	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for empty verbs")
	}
}

func TestToolGrantValidator_RejectsMissingToolRef(t *testing.T) {
	grant := v1alpha1.ToolGrant{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolGrant"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "grant-no-toolref",
			Namespace: "default",
		},
		Spec: v1alpha1.ToolGrantSpec{
			Verbs: []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
		},
	}

	v := NewToolGrantValidator()
	data, _ := json.Marshal(grant)
	req := &Request{Operation: OperationCreate, Kind: "ToolGrant", Namespace: "default", Name: "grant-no-toolref", Object: data}
	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for missing toolRef")
	}
}

func TestToolGrantValidator_RejectsNegativeRateLimits(t *testing.T) {
	grant := v1alpha1.ToolGrant{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolGrant"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "grant-bad-limits",
			Namespace: "default",
		},
		Spec: v1alpha1.ToolGrantSpec{
			ToolRef: v1alpha1.LocalObjectReference{Name: "web-search"},
			Verbs:   []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
			RateLimits: &v1alpha1.ToolGrantRateLimits{
				CallsPerMinute: -1,
			},
		},
	}

	v := NewToolGrantValidator()
	data, _ := json.Marshal(grant)
	req := &Request{Operation: OperationCreate, Kind: "ToolGrant", Namespace: "default", Name: "grant-bad-limits", Object: data}
	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for negative rate limits")
	}
}

func TestToolGrantValidator_RejectsDuplicateVerbs(t *testing.T) {
	grant := v1alpha1.ToolGrant{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolGrant"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "grant-dup-verbs",
			Namespace: "default",
		},
		Spec: v1alpha1.ToolGrantSpec{
			ToolRef: v1alpha1.LocalObjectReference{Name: "web-search"},
			Verbs:   []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke, v1alpha1.ToolGrantVerbInvoke},
		},
	}

	v := NewToolGrantValidator()
	data, _ := json.Marshal(grant)
	req := &Request{Operation: OperationCreate, Kind: "ToolGrant", Namespace: "default", Name: "grant-dup-verbs", Object: data}
	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for duplicate verbs")
	}
}

func TestToolGrantValidator_RejectsMissingReferencedTool(t *testing.T) {
	grant := validToolGrantForAdmission()
	validator := NewToolGrantValidator(newMemStore())

	resp := validateToolGrantForAdmission(t, validator, grant)
	if resp.Allowed {
		t.Fatal("expected rejection when referenced Tool does not exist")
	}
}

func TestToolGrantValidator_RejectsConstraintPropertyOutsideToolSchema(t *testing.T) {
	ctx := context.Background()
	memoryStore := newMemStore()
	tool := v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "web-search", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
		}},
	}
	toolData, _ := json.Marshal(tool)
	if err := memoryStore.Create(ctx, "Tool", "default", "web-search", toolData); err != nil {
		t.Fatal(err)
	}
	grant := validToolGrantForAdmission()
	grant.Spec.ArgumentConstraints = map[string]any{
		"type":       "object",
		"properties": map[string]any{"admin": map[string]any{"type": "boolean"}},
	}

	resp := validateToolGrantForAdmission(t, NewToolGrantValidator(memoryStore), grant)
	if resp.Allowed {
		t.Fatal("expected rejection for constraint property absent from Tool schema")
	}
}

func TestToolGrantValidator_RejectsRateAboveCognitiveQuota(t *testing.T) {
	ctx := context.Background()
	memoryStore := newMemStore()
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "web-search", Namespace: "default"}}
	toolData, _ := json.Marshal(tool)
	if err := memoryStore.Create(ctx, "Tool", "default", "web-search", toolData); err != nil {
		t.Fatal(err)
	}
	quota := v1alpha1.CognitiveQuota{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "workspace", Namespace: "default"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			ResetInterval: "1h",
			CostBudget:    &v1alpha1.CostBudget{MaxToolCallsPerPeriod: 100},
		},
	}
	quotaData, _ := json.Marshal(quota)
	if err := memoryStore.Create(ctx, "CognitiveQuota", "default", "workspace", quotaData); err != nil {
		t.Fatal(err)
	}
	grant := validToolGrantForAdmission()
	grant.Spec.RateLimits = &v1alpha1.ToolGrantRateLimits{CallsPerHour: 101}

	resp := validateToolGrantForAdmission(t, NewToolGrantValidator(memoryStore), grant)
	if resp.Allowed {
		t.Fatal("expected rejection for ToolGrant rate above CognitiveQuota")
	}
}

func validToolGrantForAdmission() v1alpha1.ToolGrant {
	return v1alpha1.ToolGrant{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "grant", Namespace: "default"},
		Spec: v1alpha1.ToolGrantSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "agent"},
			ToolRef:  v1alpha1.LocalObjectReference{Name: "web-search"},
			Verbs:    []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
		},
	}
}

func validateToolGrantForAdmission(t *testing.T, validator *ToolGrantValidator, grant v1alpha1.ToolGrant) *Response {
	t.Helper()
	data, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	return validator.Validate(context.Background(), &Request{
		Operation: OperationCreate,
		Kind:      "ToolGrant",
		Namespace: grant.ObjectMeta.Namespace,
		Name:      grant.ObjectMeta.Name,
		Object:    data,
	})
}
