// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/kognition-ai/kognition/internal/toolgateway"
)

type auditedToolGatewayStore struct {
	*memStore
	receipts map[string]store.ToolInvocationReceipt
	audits   []observability.AuditEntry
}

type separateAuditToolGatewayStore struct {
	store.Store
	store.ToolInvocationReceiptStore
}

func TestToolGatewayInvokerSeparateAuditDrivesRealTrustDemotion(t *testing.T) {
	ctx := context.Background()
	logger := zaptest.NewLogger(t)
	resources := &auditedToolGatewayStore{memStore: newMemStore(), receipts: make(map[string]store.ToolInvocationReceipt)}
	storage := &separateAuditToolGatewayStore{Store: resources, ToolInvocationReceiptStore: resources}
	audits := &auditedToolGatewayStore{memStore: newMemStore()}
	auditLog := observability.NewCognitiveAuditLog(audits)
	storeTool(t, resources.memStore, v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "calculator"}})
	storeToolGrant(t, resources.memStore, makeToolGrant("tenant-a", "calculator-access", v1alpha1.ToolGrantSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "worker"}, ToolRef: v1alpha1.LocalObjectReference{Name: "calculator"},
		Verbs: []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
		ArgumentConstraints: map[string]any{"type": "object", "properties": map[string]any{
			"expression": map[string]any{"const": "17 * 23"},
		}, "required": []any{"expression"}},
	}))
	executor := &workflowRecordingExecutor{}
	policies := NewInvocationPolicyEngine(NewToolGrantEngine(storage, logger), nil)
	invoker, err := NewToolGatewayInvoker(storage, policies, nil, runtime.NewToolDispatcher(executor, nil), logger, nil,
		toolgateway.WithLedger(toolgateway.NewStoreLedgerWithAudit(storage, auditLog)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invoker.InvokeTool(ctx, "tenant-a", "worker", "calculator", map[string]any{"expression": "17 * 24"}); err == nil {
		t.Fatal("expected a real argument-constraint denial")
	}
	if executor.calls != 0 || len(audits.audits) != 1 || len(resources.audits) != 0 {
		t.Fatalf("executions=%d separate audits=%d resource audits=%d", executor.calls, len(audits.audits), len(resources.audits))
	}
	var decision observability.TrustDecisionV1
	if err := json.Unmarshal(audits.audits[0].Details, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.ReasonCode != "argument_constraints_denied" || decision.Identity == nil || decision.Identity.AgentIdentity != "worker" {
		t.Fatalf("denial is not attributable to the scoring Agent: %#v", decision)
	}
	profile := v1alpha1.TrustProfile{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "worker-trust", Namespace: "tenant-a"},
		Spec: v1alpha1.TrustProfileSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "worker"},
			AgentIncarnationUID: "worker-incarnation", InitialScore: 80, InitialClass: "assisted",
			MaxClass: "assisted", EvaluationInterval: "1m",
			DemotionPolicy: &v1alpha1.DemotionPolicy{Mode: "immediate", FallbackClass: "supervised"}},
		Status: v1alpha1.TrustProfileStatus{CurrentClass: "assisted", CurrentScore: 80},
	}
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Create(ctx, "TrustProfile", "tenant-a", profile.Name, data); err != nil {
		t.Fatal(err)
	}
	reconciler := NewTrustProfileReconciler(storage, logger)
	reconciler.SetAuditLog(auditLog)
	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "tenant-a", Name: profile.Name, Kind: "TrustProfile"}); err != nil {
		t.Fatal(err)
	}
	raw, err := storage.Get(ctx, "TrustProfile", "tenant-a", profile.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	if profile.Status.CurrentClass != "supervised" || profile.Status.AuditedActions != 1 {
		t.Fatalf("real denial did not demote the profile: %#v", profile.Status)
	}
	found := false
	for _, condition := range profile.Status.Conditions {
		if condition.Type == "Demoted" && condition.Status == "True" && strings.Contains(condition.Reason, "critical policy violation") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing policy-violation demotion condition: %#v", profile.Status.Conditions)
	}
}

func (s *auditedToolGatewayStore) ClaimToolInvocation(_ context.Context, receipt store.ToolInvocationReceipt) (bool, store.ToolInvocationReceipt, error) {
	key := receipt.Namespace + "/" + receipt.InvocationID
	if existing, found := s.receipts[key]; found {
		return false, existing, nil
	}
	s.receipts[key] = receipt
	return true, receipt, nil
}

func (s *auditedToolGatewayStore) PrepareToolInvocation(_ context.Context, receipt store.ToolInvocationReceipt) (bool, store.ToolInvocationReceipt, error) {
	key := receipt.Namespace + "/" + receipt.InvocationID
	if existing, found := s.receipts[key]; found {
		if existing.Status != store.ToolInvocationPrepared || existing.Binding != receipt.Binding || existing.PreparationToken != receipt.PreparationToken {
			return false, store.ToolInvocationReceipt{}, store.ErrConflict
		}
		return true, existing, nil
	}
	s.receipts[key] = receipt
	return true, receipt, nil
}

func (s *auditedToolGatewayStore) ReleasePreparedToolInvocation(_ context.Context, namespace, invocationID, preparationToken string) (bool, error) {
	key := namespace + "/" + invocationID
	existing, found := s.receipts[key]
	if !found {
		return false, nil
	}
	if existing.Status != store.ToolInvocationPrepared || existing.PreparationToken != preparationToken {
		return false, store.ErrConflict
	}
	delete(s.receipts, key)
	return true, nil
}

func (s *auditedToolGatewayStore) CompleteToolInvocation(_ context.Context, receipt store.ToolInvocationReceipt) error {
	s.receipts[receipt.Namespace+"/"+receipt.InvocationID] = receipt
	return nil
}

func (s *auditedToolGatewayStore) Append(_ context.Context, entry observability.AuditEntry) error {
	s.audits = append(s.audits, entry)
	return nil
}

func (s *auditedToolGatewayStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	for _, entry := range entries {
		if err := s.Append(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *auditedToolGatewayStore) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return append([]observability.AuditEntry(nil), s.audits...), nil
}

func TestUnavailableToolGatewayInvokerFailsClosed(t *testing.T) {
	cause := errors.New("configuration error")
	invoker := NewUnavailableToolGatewayInvoker(cause)
	if _, err := invoker.InvokeTool(context.Background(), "team-a", "planner", "crm-update", nil); !errors.Is(err, cause) {
		t.Fatalf("InvokeTool error = %v, want configuration cause", err)
	}
}

// TC-0085: The in-process production gateway enforces ToolGrant argument
// patterns before dispatch and emits durable, redacted denial evidence.
func TestToolGatewayInvokerDeniesArgumentConstraintBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	memoryStore := &auditedToolGatewayStore{memStore: newMemStore(), receipts: make(map[string]store.ToolInvocationReceipt)}
	storeTool(t, memoryStore.memStore, v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "search"}})
	storeToolGrant(t, memoryStore.memStore, makeToolGrant("tenant-a", "constrained-search", v1alpha1.ToolGrantSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "workflow:research"},
		ToolRef:  v1alpha1.LocalObjectReference{Name: "search"},
		Verbs:    []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
		ArgumentConstraints: map[string]any{
			"type":     "object",
			"required": []any{"query"},
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "pattern": "^safe-"},
			},
		},
	}))
	executor := &workflowRecordingExecutor{result: runtime.ToolExecutionResult{Output: map[string]any{"status": "ok"}}}
	policies := NewInvocationPolicyEngine(NewToolGrantEngine(memoryStore, zaptest.NewLogger(t)), nil)
	invoker, err := NewToolGatewayInvoker(memoryStore, policies, nil, runtime.NewToolDispatcher(executor, nil), zaptest.NewLogger(t), nil)
	if err != nil {
		t.Fatal(err)
	}

	secret := "xy-sensitive-value"
	if _, err := invoker.InvokeTool(ctx, "tenant-a", "workflow:research", "search", map[string]any{"query": secret}); err == nil || !strings.Contains(err.Error(), "argument constraints") {
		t.Fatalf("InvokeTool() error = %v, want argument constraint denial", err)
	}
	if executor.calls != 0 || len(memoryStore.audits) != 1 {
		t.Fatalf("executor calls=%d audit entries=%d, want 0/1", executor.calls, len(memoryStore.audits))
	}
	var decision observability.TrustDecisionV1
	if err := json.Unmarshal(memoryStore.audits[0].Details, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != "denied" || decision.ReasonCode != "argument_constraints_denied" || decision.Authorization.Grant != "denied" || decision.Authorization.Policy != "not_evaluated" || decision.Authorization.GrantRef == nil || decision.Authorization.GrantRef.Name != "constrained-search" || decision.Execution.TargetReached == nil || *decision.Execution.TargetReached {
		t.Fatalf("denial decision = %#v", decision)
	}
	if strings.Contains(string(memoryStore.audits[0].Details), secret) {
		t.Fatalf("denial evidence leaked invocation input: %s", memoryStore.audits[0].Details)
	}
}

// TC-0085: An enforced AgentPolicy denies a controller-side Tool call before
// dispatch and persists the policy reference as normalized violation evidence.
func TestToolGatewayInvokerRecordsAgentPolicyDenialBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	memoryStore := &auditedToolGatewayStore{memStore: newMemStore(), receipts: make(map[string]store.ToolInvocationReceipt)}
	storeTool(t, memoryStore.memStore, v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "http-get"}})
	storeToolGrant(t, memoryStore.memStore, makeToolGrant("tenant-a", "workflow-tools", v1alpha1.ToolGrantSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "workflow:nightly"},
		ToolRef:  v1alpha1.LocalObjectReference{Name: "http-get"},
		Verbs:    []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
	}))
	policy := v1alpha1.AgentPolicy{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "deny-http", Namespace: "tenant-a"},
		Spec: v1alpha1.AgentPolicySpec{
			Enforce: true,
			Scope:   "workspace",
			Rules:   `deny[msg] { input.tool == "http-get"; msg := "network tool blocked" }`,
		},
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Create(ctx, agentPolicyKind, "tenant-a", policy.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	policyEvaluator := NewAgentPolicyReconciler(memoryStore, opa.NewEngine(), zaptest.NewLogger(t))
	if _, err := policyEvaluator.Reconcile(ctx, Request{Namespace: "tenant-a", Name: policy.ObjectMeta.Name, Kind: agentPolicyKind}); err != nil {
		t.Fatalf("compile AgentPolicy: %v", err)
	}
	executor := &workflowRecordingExecutor{}
	policies := NewInvocationPolicyEngine(NewToolGrantEngine(memoryStore, zaptest.NewLogger(t)), policyEvaluator)
	invoker, err := NewToolGatewayInvoker(memoryStore, policies, nil, runtime.NewToolDispatcher(executor, nil), zaptest.NewLogger(t), nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := invoker.InvokeTool(ctx, "tenant-a", "workflow:nightly", "http-get", map[string]any{"url": "https://example.com"}); err == nil || !strings.Contains(err.Error(), "AgentPolicy denied") {
		t.Fatalf("InvokeTool() error = %v, want AgentPolicy denial", err)
	}
	if executor.calls != 0 || len(memoryStore.audits) != 1 {
		t.Fatalf("executor calls=%d audit entries=%d, want 0/1", executor.calls, len(memoryStore.audits))
	}
	var decision observability.TrustDecisionV1
	if err := json.Unmarshal(memoryStore.audits[0].Details, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != "denied" || decision.ReasonCode != "policy_denied" || decision.Authorization.Grant != "allowed" || decision.Authorization.Policy != "denied" || decision.Authorization.PolicyRef == nil || decision.Authorization.PolicyRef.Name != "deny-http" || decision.Authorization.GrantRef == nil || decision.Authorization.GrantRef.Name != "workflow-tools" || decision.Execution.TargetReached == nil || *decision.Execution.TargetReached {
		t.Fatalf("policy denial decision = %#v", decision)
	}
	if strings.Contains(string(memoryStore.audits[0].Details), "https://example.com") {
		t.Fatalf("policy denial evidence leaked invocation input: %s", memoryStore.audits[0].Details)
	}
}
