// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/store"
)

type agentPolicyFaultStore struct {
	store.Store
	getErr    error
	updateErr error
}

func (s *agentPolicyFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.Store.Get(ctx, kind, namespace, name)
}

func (s *agentPolicyFaultStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

// TC-0084: Rego policy compiled and evaluates deny correctly.
func TestAgentPolicyReconciler_CompilePolicy(t *testing.T) {
	ms := newMemStore()
	engine := opa.NewEngine()
	logger := zaptest.NewLogger(t)
	r := NewAgentPolicyReconciler(ms, engine, logger)

	ctx := context.Background()
	ns := "default"

	// Create a policy with deny rule.
	policy := v1alpha1.AgentPolicy{}
	policy.TypeMeta.Kind = "AgentPolicy"
	policy.ObjectMeta.Name = "no-dangerous-tools"
	policy.ObjectMeta.Namespace = ns
	policy.Spec.Enforce = true
	policy.Spec.Scope = "workspace"
	policy.Spec.Rules = `deny[msg] { input.tool == "rm_rf"; msg := "dangerous tool blocked" }`
	data, _ := json.Marshal(policy)
	ms.Create(ctx, "AgentPolicy", ns, "no-dangerous-tools", data)

	// Reconcile should compile into OPA engine.
	_, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "no-dangerous-tools", Kind: "AgentPolicy"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if engine.PolicyCount() != 1 {
		t.Fatalf("expected 1 loaded policy, got %d", engine.PolicyCount())
	}
}

func TestAgentPolicyReconciler_CompileFailurePreservesLastValidPolicy(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	engine := opa.NewEngine()
	r := NewAgentPolicyReconciler(ms, engine, zaptest.NewLogger(t))

	policy := v1alpha1.AgentPolicy{
		TypeMeta: v1alpha1.TypeMeta{Kind: agentPolicyKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "validated-policy",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: v1alpha1.AgentPolicySpec{
			Scope: "workspace",
			Rules: `deny[msg] { input.tool == "dangerous"; msg = "blocked" }`,
		},
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.Create(ctx, agentPolicyKind, "default", policy.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	request := Request{Namespace: "default", Name: policy.ObjectMeta.Name, Kind: agentPolicyKind}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("compile valid policy: %v", err)
	}

	raw, err := ms.Get(ctx, agentPolicyKind, "default", policy.ObjectMeta.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	policy.ObjectMeta.Generation = 2
	policy.Spec.Rules = `deny[msg] { input.tool == "safe"`
	data, err = json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.Update(ctx, agentPolicyKind, "default", policy.ObjectMeta.Name, data, policy.ObjectMeta.ResourceVersion); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile invalid policy: %v", err)
	}
	raw, err = ms.Get(ctx, agentPolicyKind, "default", policy.ObjectMeta.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Status.Phase != v1alpha1.PhaseFailed || policy.Status.ObservedGeneration != 2 {
		t.Fatalf("status = %+v, want Failed at generation 2", policy.Status)
	}
	condition := findCondition(policy.Status.Conditions, "Ready")
	if condition == nil || condition.Status != "False" || condition.Reason != "CompileFailed" {
		t.Fatalf("Ready condition = %+v, want False/CompileFailed", condition)
	}

	result, err := engine.Evaluate(ctx, "default/validated-policy", map[string]interface{}{"tool": "dangerous"})
	if err != nil {
		t.Fatalf("evaluate last valid policy: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected the last valid deny policy to remain active")
	}
}

func TestAgentPolicyReconciler_DeleteRemovesPolicy(t *testing.T) {
	ms := newMemStore()
	engine := opa.NewEngine()
	logger := zaptest.NewLogger(t)
	r := NewAgentPolicyReconciler(&agentPolicyFaultStore{
		Store:  ms,
		getErr: fmt.Errorf("wrapped missing policy: %w", store.ErrNotFound),
	}, engine, logger)

	ctx := context.Background()
	ns := "default"

	// Pre-load a policy in the engine.
	engine.LoadPolicy("default/old-policy", `deny[msg] { msg := "blocked" }`)
	if engine.PolicyCount() != 1 {
		t.Fatal("setup failed")
	}

	// Reconcile a non-existent policy (simulates delete).
	_, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "old-policy", Kind: "AgentPolicy"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if engine.PolicyCount() != 0 {
		t.Fatalf("expected 0 policies after delete, got %d", engine.PolicyCount())
	}
}

func TestAgentPolicyReconciler_WrappedStatusConflictRequeues(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	policy := v1alpha1.AgentPolicy{
		TypeMeta: v1alpha1.TypeMeta{Kind: agentPolicyKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "conflicting-policy",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: v1alpha1.AgentPolicySpec{
			Rules: `deny[msg] { input.tool == "dangerous"; msg = "blocked" }`,
		},
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.Create(ctx, agentPolicyKind, "default", policy.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}

	r := NewAgentPolicyReconciler(&agentPolicyFaultStore{
		Store:     ms,
		updateErr: fmt.Errorf("wrapped status conflict: %w", store.ErrConflict),
	}, opa.NewEngine(), zaptest.NewLogger(t))
	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: policy.ObjectMeta.Name, Kind: agentPolicyKind})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected wrapped status conflict to requeue")
	}
}

// TC-0086: Policy denies tool access; tool doesn't execute.
func TestAgentPolicyReconciler_ToolGrantDeny(t *testing.T) {
	ms := newMemStore()
	engine := opa.NewEngine()
	logger := zaptest.NewLogger(t)
	r := NewAgentPolicyReconciler(ms, engine, logger)

	ctx := context.Background()
	ns := "default"

	// Create policy with denied tool grant.
	policy := v1alpha1.AgentPolicy{}
	policy.TypeMeta.Kind = "AgentPolicy"
	policy.ObjectMeta.Name = "tool-policy"
	policy.ObjectMeta.Namespace = ns
	policy.Spec.Scope = "workspace"
	policy.Spec.ToolGrants = []v1alpha1.PolicyToolGrant{
		{ToolRef: "exec_shell", Allow: false},
	}
	data, _ := json.Marshal(policy)
	ms.Create(ctx, "AgentPolicy", ns, "tool-policy", data)

	_, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "tool-policy", Kind: "AgentPolicy"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Evaluate with denied tool.
	result, err := engine.Evaluate(ctx, "default/tool-policy", map[string]interface{}{
		"tool": "exec_shell",
	})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if result.Allowed {
		t.Error("expected deny for exec_shell tool")
	}

	// Evaluate with allowed tool.
	result, err = engine.Evaluate(ctx, "default/tool-policy", map[string]interface{}{
		"tool": "search",
	})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if !result.Allowed {
		t.Error("expected allow for search tool")
	}
}

// TC-0084, TC-0085: Scoped policy denies risky agent, allows safe agent.
func TestAgentPolicyReconciler_EvaluateAgent_ScopedPolicy(t *testing.T) {
	ms := newMemStore()
	engine := opa.NewEngine()
	logger := zaptest.NewLogger(t)
	r := NewAgentPolicyReconciler(ms, engine, logger)

	ctx := context.Background()
	ns := "default"

	// Create agent-scoped policy.
	policy := v1alpha1.AgentPolicy{}
	policy.TypeMeta.Kind = "AgentPolicy"
	policy.ObjectMeta.Name = "agent-specific"
	policy.ObjectMeta.Namespace = ns
	policy.Spec.Scope = "agent"
	policy.Spec.Enforce = true
	policy.Spec.TargetAgents = []string{"risky-agent"}
	policy.Spec.Rules = `deny[msg] { input.tool == "dangerous"; msg := "blocked for agent" }`
	data, _ := json.Marshal(policy)
	ms.Create(ctx, "AgentPolicy", ns, "agent-specific", data)

	// Reconcile to load the policy.
	_, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "agent-specific", Kind: "AgentPolicy"})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Evaluate for targeted agent — should deny.
	result, err := r.EvaluateAgent(ctx, ns, "risky-agent", map[string]interface{}{
		"tool": "dangerous",
	})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if result.Allowed {
		t.Error("expected deny for risky-agent with dangerous tool")
	}

	// Evaluate for non-targeted agent — should allow.
	result, err = r.EvaluateAgent(ctx, ns, "safe-agent", map[string]interface{}{
		"tool": "dangerous",
	})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if !result.Allowed {
		t.Error("expected allow for safe-agent (not targeted)")
	}
}

func TestInvocationPolicyEngine_ModelAndToolCallsIncludeAgentSpec(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	policyReconciler := NewAgentPolicyReconciler(ms, opa.NewEngine(), logger)
	invocations := NewInvocationPolicyEngine(NewToolGrantEngine(ms, logger), policyReconciler)

	agent := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "hello-agent", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4-1"},
			Instructions: v1alpha1.AgentInstructions{Inline: "Answer concisely."},
		},
	}
	grant := v1alpha1.ModelGrant{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ModelGrant"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "hello-agent-model", Namespace: "default"},
		Spec: v1alpha1.ModelGrantSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "hello-agent"},
			ModelRef: v1alpha1.LocalObjectReference{Name: "gpt-4-1"},
		},
	}
	policy := v1alpha1.AgentPolicy{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: agentPolicyKind},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "require-instructions", Namespace: "default"},
		Spec: v1alpha1.AgentPolicySpec{
			Enforce: true,
			Scope:   "workspace",
			ToolGrants: []v1alpha1.PolicyToolGrant{{
				ToolRef: "calculator",
				Allow:   true,
			}},
			ComplianceRules: []v1alpha1.GovernanceRule{{
				Name: "require-instructions",
				Rego: `package kognition.compliance
deny[msg] {
  not input.spec.instructions.inline
  not input.spec.instructions.ref
  msg := "agents must have instructions defined"
}`,
			}},
		},
	}

	for _, resource := range []struct {
		kind string
		name string
		obj  any
	}{
		{kind: "Agent", name: agent.ObjectMeta.Name, obj: agent},
		{kind: "ModelGrant", name: grant.ObjectMeta.Name, obj: grant},
		{kind: agentPolicyKind, name: policy.ObjectMeta.Name, obj: policy},
	} {
		data, err := json.Marshal(resource.obj)
		if err != nil {
			t.Fatalf("marshal %s: %v", resource.kind, err)
		}
		if err := ms.Create(ctx, resource.kind, "default", resource.name, data); err != nil {
			t.Fatalf("store %s: %v", resource.kind, err)
		}
	}

	if err := invocations.EvaluateModelAccess(ctx, "default", "hello-agent", "gpt-4-1"); err != nil {
		t.Fatalf("EvaluateModelAccess() error = %v, want allow", err)
	}
	if err := invocations.EvaluateToolAccess(ctx, "default", "hello-agent", "calculator", v1alpha1.ToolGrantVerbInvoke); err != nil {
		t.Fatalf("EvaluateToolAccess() error = %v, want allow", err)
	}
}

// TC-0082: Safety rules require annotation; reject without it.
func TestAgentPolicyReconciler_SafetyAndComplianceRules(t *testing.T) {
	ms := newMemStore()
	engine := opa.NewEngine()
	logger := zaptest.NewLogger(t)
	r := NewAgentPolicyReconciler(ms, engine, logger)

	ctx := context.Background()
	ns := "default"

	policy := v1alpha1.AgentPolicy{}
	policy.TypeMeta.Kind = "AgentPolicy"
	policy.ObjectMeta.Name = "combined"
	policy.ObjectMeta.Namespace = ns
	policy.Spec.Scope = "workspace"
	policy.Spec.SafetyRules = []v1alpha1.GovernanceRule{
		{Name: "no-pii", Rego: `deny[msg] { input.contains_pii == true; msg := "PII detected" }`, Severity: "critical"},
	}
	policy.Spec.ComplianceRules = []v1alpha1.GovernanceRule{
		{Name: "audit-trail", Rego: `deny[msg] { input.audit_enabled == false; msg := "audit required" }`, Severity: "high"},
	}
	data, _ := json.Marshal(policy)
	ms.Create(ctx, "AgentPolicy", ns, "combined", data)

	_, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "combined", Kind: "AgentPolicy"})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	// Trigger safety rule.
	result, err := engine.Evaluate(ctx, "default/combined", map[string]interface{}{
		"contains_pii":  true,
		"audit_enabled": true,
	})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if result.Allowed {
		t.Error("expected deny for PII content")
	}
}

func TestAgentPolicyReconciler_EmptyRulesRemovesPolicy(t *testing.T) {
	ms := newMemStore()
	engine := opa.NewEngine()
	logger := zaptest.NewLogger(t)
	r := NewAgentPolicyReconciler(ms, engine, logger)

	ctx := context.Background()
	ns := "default"

	// Pre-load a policy.
	engine.LoadPolicy("default/empty", `deny[msg] { msg := "old" }`)

	// Create policy with no rules.
	policy := v1alpha1.AgentPolicy{}
	policy.TypeMeta.Kind = "AgentPolicy"
	policy.ObjectMeta.Name = "empty"
	policy.ObjectMeta.Namespace = ns
	data, _ := json.Marshal(policy)
	ms.Create(ctx, "AgentPolicy", ns, "empty", data)

	_, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "empty", Kind: "AgentPolicy"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if engine.PolicyCount() != 0 {
		t.Fatalf("expected policy removed for empty rules, got %d", engine.PolicyCount())
	}
}
