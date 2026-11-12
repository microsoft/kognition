// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package opa

import (
	"context"
	"testing"
)

func TestNewEngine(t *testing.T) {
	e := NewEngine()
	if e == nil {
		t.Fatal("expected non-nil engine")
	}
	if e.PolicyCount() != 0 {
		t.Fatalf("expected 0 policies, got %d", e.PolicyCount())
	}
}

func TestLoadPolicy_Empty(t *testing.T) {
	e := NewEngine()
	err := e.LoadPolicy("test", "")
	if err == nil {
		t.Fatal("expected error for empty source")
	}
}

func TestLoadPolicy_Valid(t *testing.T) {
	e := NewEngine()
	err := e.LoadPolicy("test", "package policy\ndeny[msg] { msg = \"denied\" }")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.PolicyCount() != 1 {
		t.Fatalf("expected 1 policy, got %d", e.PolicyCount())
	}
}

func TestLoadPolicy_Overwrite(t *testing.T) {
	e := NewEngine()
	e.LoadPolicy("test", "package p\ndeny[msg] { msg = \"v1\" }")
	e.LoadPolicy("test", "package p\ndeny[msg] { msg = \"v2\" }")
	if e.PolicyCount() != 1 {
		t.Fatalf("expected 1 policy after overwrite, got %d", e.PolicyCount())
	}
}

func TestLoadPolicy_InvalidUpdatePreservesActivePolicy(t *testing.T) {
	e := NewEngine()
	if err := e.LoadPolicy("test", `deny[msg] { input.tool == "dangerous"; msg = "blocked" }`); err != nil {
		t.Fatalf("load valid policy: %v", err)
	}

	if err := e.LoadPolicy("test", `deny[msg] { input.tool == "safe"`); err == nil {
		t.Fatal("expected malformed policy update to fail")
	}

	result, err := e.Evaluate(context.Background(), "test", map[string]interface{}{"tool": "dangerous"})
	if err != nil {
		t.Fatalf("evaluate preserved policy: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected the last valid policy to remain active")
	}
}

func TestLoadPolicy_ValidatesSupportedStructure(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		wantErr bool
	}{
		{name: "mismatched delimiter", source: `deny[msg] { input.values[0) }`, wantErr: true},
		{name: "unterminated string", source: `deny[msg] { msg = "blocked }`, wantErr: true},
		{name: "delimiters in string", source: `deny[msg] { msg = "blocked } ] )" }`},
		{name: "escaped quote", source: `deny[msg] { msg = "blocked \"value\"" }`},
		{name: "delimiters in comment", source: "deny[msg] { # unmatched } ] )\nmsg = \"blocked\" }"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewEngine().LoadPolicy("test", tt.source)
			if (err != nil) != tt.wantErr {
				t.Fatalf("LoadPolicy() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRemovePolicy(t *testing.T) {
	e := NewEngine()
	e.LoadPolicy("p1", "package p\ndeny[msg] { msg = \"x\" }")
	e.RemovePolicy("p1")
	if e.PolicyCount() != 0 {
		t.Fatalf("expected 0 policies after removal, got %d", e.PolicyCount())
	}
}

func TestEvaluate_NotLoaded(t *testing.T) {
	e := NewEngine()
	_, err := e.Evaluate(context.Background(), "missing", nil)
	if err == nil {
		t.Fatal("expected error for missing policy")
	}
}

func TestEvaluate_DenyRule(t *testing.T) {
	e := NewEngine()
	rego := "package policy\ndeny[msg] { not input.metadata.labels; msg = \"labels required\" }"
	e.LoadPolicy("require-labels", rego)

	input := map[string]interface{}{
		"kind": "Agent",
		"metadata": map[string]interface{}{
			"name":      "test-agent",
			"namespace": "default",
		},
	}

	result, err := e.Evaluate(context.Background(), "require-labels", input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected denial")
	}
	if result.Reason == "" {
		t.Fatal("expected a reason")
	}
}

func TestEvaluate_AllowWhenConditionNotMet(t *testing.T) {
	e := NewEngine()
	rego := "package policy\ndeny[msg] { not input.metadata.labels; msg = \"labels required\" }"
	e.LoadPolicy("require-labels", rego)

	input := map[string]interface{}{
		"kind": "Agent",
		"metadata": map[string]interface{}{
			"name":   "test-agent",
			"labels": map[string]interface{}{"app": "test"},
		},
	}

	result, err := e.Evaluate(context.Background(), "require-labels", input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatalf("expected allow, got denial: %s", result.Reason)
	}
}

func TestEvaluate_EqualityCondition(t *testing.T) {
	e := NewEngine()
	rego := `package policy
deny[msg] { input.metadata.namespace == "kube-system"; msg = "cannot create in kube-system" }`
	e.LoadPolicy("ns-check", rego)

	// Should deny kube-system.
	input := map[string]interface{}{
		"metadata": map[string]interface{}{
			"namespace": "kube-system",
		},
	}
	result, err := e.Evaluate(context.Background(), "ns-check", input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected denial for kube-system")
	}

	// Should allow default.
	input2 := map[string]interface{}{
		"metadata": map[string]interface{}{
			"namespace": "default",
		},
	}
	result2, err := e.Evaluate(context.Background(), "ns-check", input2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result2.Allowed {
		t.Fatal("expected allow for default namespace")
	}
}

func TestEvaluate_InequalityCondition(t *testing.T) {
	e := NewEngine()
	rego := `package policy
deny[msg] { input.spec.image != "approved:latest"; msg = "unapproved image" }`
	e.LoadPolicy("image-check", rego)

	// Should deny unapproved image.
	input := map[string]interface{}{
		"spec": map[string]interface{}{
			"image": "malicious:v1",
		},
	}
	result, err := e.Evaluate(context.Background(), "image-check", input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected denial for unapproved image")
	}

	// Should allow approved image.
	input2 := map[string]interface{}{
		"spec": map[string]interface{}{
			"image": "approved:latest",
		},
	}
	result2, err := e.Evaluate(context.Background(), "image-check", input2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result2.Allowed {
		t.Fatal("expected allow for approved image")
	}
}

func TestEvaluateAll_NoPolicies(t *testing.T) {
	e := NewEngine()
	result, err := e.EvaluateAll(context.Background(), map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatal("expected allow with no policies")
	}
}

func TestEvaluateAll_MultiplePolicies(t *testing.T) {
	e := NewEngine()
	e.LoadPolicy("p1", "package p1\ndeny[msg] { not input.metadata.labels; msg = \"no labels\" }")
	e.LoadPolicy("p2", "package p2\ndeny[msg] { input.spec.dangerous == \"true\"; msg = \"dangerous\" }")

	// Triggers p1 (no labels).
	input := map[string]interface{}{
		"metadata": map[string]interface{}{"name": "x"},
		"spec":     map[string]interface{}{},
	}
	result, err := e.EvaluateAll(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected denial from at least one policy")
	}
}

func TestEvaluateAll_AllPass(t *testing.T) {
	e := NewEngine()
	e.LoadPolicy("p1", "package p1\ndeny[msg] { not input.metadata.labels; msg = \"no labels\" }")

	input := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":   "x",
			"labels": map[string]interface{}{"app": "ok"},
		},
	}
	result, err := e.EvaluateAll(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatalf("expected allow, got: %s", result.Reason)
	}
}

func TestInterpreter_MultiLineRule(t *testing.T) {
	source := `package policy

deny[msg] {
    not input.spec.resources
    msg = "resources required"
}`
	input := map[string]interface{}{
		"spec": map[string]interface{}{},
	}

	result, err := interpretRego(source, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected denial for missing resources")
	}
}

func TestInterpreter_BracketNotation(t *testing.T) {
	source := `package policy
deny[msg] { not input.metadata.annotations["approved"]; msg = "needs approved annotation" }`

	input := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{},
		},
	}

	result, err := interpretRego(source, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected denial for missing annotation")
	}

	// With the annotation present.
	input2 := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{"approved": "true"},
		},
	}
	result2, err := interpretRego(source, input2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result2.Allowed {
		t.Fatalf("expected allow, got: %s", result2.Reason)
	}
}

func TestInterpreter_ContainsLower(t *testing.T) {
	source := `package policy
deny[msg] {
    contains(lower(input.output), "ssn")
    msg := "output appears to reference SSN"
}`

	result, err := interpretRego(source, map[string]interface{}{})
	if err != nil {
		t.Fatalf("interpretRego() error: %v", err)
	}
	if !result.Allowed {
		t.Fatalf("missing output should not match contains/lower rule: %s", result.Reason)
	}

	result, err = interpretRego(source, map[string]interface{}{"output": "The response includes SSN data."})
	if err != nil {
		t.Fatalf("interpretRego() error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected matching output to be denied")
	}
	if result.Reason != "output appears to reference SSN" {
		t.Fatalf("Reason = %q, want SSN denial", result.Reason)
	}
}

func TestInterpreter_NoDenyRules(t *testing.T) {
	source := `package policy
# Just a comment, no deny rules
allow = true`
	result, err := interpretRego(source, map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatal("expected allow with no deny rules")
	}
}

func TestInterpreter_DenialMessageForms(t *testing.T) {
	tests := []struct {
		name       string
		rule       string
		wantReason string
	}{
		{
			name:       "backtick literal",
			rule:       "deny[msg] { input.blocked == true; msg := `blocked by policy` }",
			wantReason: "blocked by policy",
		},
		{
			name:       "raw reason",
			rule:       "deny[msg] { input.blocked == true; msg = policy_violation }",
			wantReason: "policy_violation",
		},
		{
			name:       "default reason",
			rule:       "deny[msg] { input.blocked == true }",
			wantReason: "policy violation",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := interpretRego(test.rule, map[string]interface{}{"blocked": true})
			if err != nil {
				t.Fatalf("interpretRego() error: %v", err)
			}
			if result.Allowed {
				t.Fatal("interpretRego() allowed matching deny rule")
			}
			if result.Reason != test.wantReason {
				t.Errorf("Reason = %q, want %q", result.Reason, test.wantReason)
			}
		})
	}
}

func TestSplitPath(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"metadata.name", []string{"metadata", "name"}},
		{"metadata.annotations[\"key\"]", []string{"metadata", "annotations", "key"}},
		{"spec.resources.cpu", []string{"spec", "resources", "cpu"}},
	}

	for _, tc := range tests {
		got := splitPath(tc.input)
		if len(got) != len(tc.expected) {
			t.Errorf("splitPath(%q): expected %v, got %v", tc.input, tc.expected, got)
			continue
		}
		for i := range got {
			if got[i] != tc.expected[i] {
				t.Errorf("splitPath(%q)[%d]: expected %q, got %q", tc.input, i, tc.expected[i], got[i])
			}
		}
	}
}

func TestResolvePath(t *testing.T) {
	data := map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      "test",
			"namespace": "default",
			"labels": map[string]interface{}{
				"app": "web",
			},
		},
		"spec": map[string]interface{}{
			"replicas": 3,
		},
	}

	tests := []struct {
		path     string
		expected interface{}
	}{
		{"metadata.name", "test"},
		{"metadata.namespace", "default"},
		{"metadata.labels.app", "web"},
		{"spec.replicas", 3},
		{"spec.missing", nil},
	}

	for _, tc := range tests {
		got := resolvePath(tc.path, data)
		if got != tc.expected {
			t.Errorf("resolvePath(%q): expected %v, got %v", tc.path, tc.expected, got)
		}
	}
}
