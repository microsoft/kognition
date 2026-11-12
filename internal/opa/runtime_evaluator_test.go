// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package opa

import (
	"context"
	"testing"

	"go.uber.org/zap/zaptest"
)

func TestRuntimePolicyEvaluator_DenyToolCall(t *testing.T) {
	engine := NewEngine()
	logger := zaptest.NewLogger(t)
	eval := NewRuntimePolicyEvaluator(engine, logger)

	// Policy: deny tool calls named "delete-file".
	rego := `package runtime
deny[msg] { input.action == "tool_call"; input.tool.name == "delete-file"; msg = "delete-file tool is forbidden" }`
	if err := eval.LoadPolicy("no-delete", rego); err != nil {
		t.Fatalf("load policy: %v", err)
	}

	ctx := context.Background()
	rctx := RuntimeContext{
		AgentName: "agent-1",
		Namespace: "default",
		SessionID: "sess-001",
		TrustTier: "standard",
	}

	// Should deny delete-file.
	result, err := eval.EvaluateToolCall(ctx, rctx, "delete-file", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected denial for delete-file tool")
	}
	if result.Reason == "" {
		t.Fatal("expected denial reason")
	}

	// Should allow other tools.
	result, err = eval.EvaluateToolCall(ctx, rctx, "read-file", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatalf("expected allow for read-file, got: %s", result.Reason)
	}
}

func TestRuntimePolicyEvaluator_DenyToolByArgs(t *testing.T) {
	engine := NewEngine()
	logger := zaptest.NewLogger(t)
	eval := NewRuntimePolicyEvaluator(engine, logger)

	// Policy: deny tool calls with args.path containing "/etc/".
	rego := `package runtime
deny[msg] { input.action == "tool_call"; input.tool.args.path == "/etc/passwd"; msg = "access to /etc/passwd denied" }`
	if err := eval.LoadPolicy("no-etc", rego); err != nil {
		t.Fatalf("load policy: %v", err)
	}

	ctx := context.Background()
	rctx := RuntimeContext{
		AgentName: "agent-1",
		Namespace: "default",
		SessionID: "sess-001",
		TrustTier: "standard",
	}

	// Should deny tool call with /etc/passwd path.
	result, err := eval.EvaluateToolCall(ctx, rctx, "read-file", map[string]interface{}{
		"path": "/etc/passwd",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected denial for /etc/passwd access")
	}

	// Should allow other paths.
	result, err = eval.EvaluateToolCall(ctx, rctx, "read-file", map[string]interface{}{
		"path": "/home/user/file.txt",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatalf("expected allow, got: %s", result.Reason)
	}
}

func TestRuntimePolicyEvaluator_DenyMemoryWrite(t *testing.T) {
	engine := NewEngine()
	logger := zaptest.NewLogger(t)
	eval := NewRuntimePolicyEvaluator(engine, logger)

	// Policy: deny memory writes for untrusted agents.
	rego := `package runtime
deny[msg] { input.action == "memory_op"; input.memory.operation == "write"; input.agent.trustTier == "untrusted"; msg = "untrusted agents cannot write memory" }`
	if err := eval.LoadPolicy("no-write-untrusted", rego); err != nil {
		t.Fatalf("load policy: %v", err)
	}

	ctx := context.Background()

	// Untrusted agent should be denied.
	rctx := RuntimeContext{
		AgentName: "sketchy-agent",
		Namespace: "default",
		SessionID: "sess-002",
		TrustTier: "untrusted",
	}
	result, err := eval.EvaluateMemoryOp(ctx, rctx, "write", "secret-key", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected denial for untrusted memory write")
	}

	// Trusted agent should be allowed.
	rctx.TrustTier = "trusted"
	rctx.AgentName = "good-agent"
	result, err = eval.EvaluateMemoryOp(ctx, rctx, "write", "secret-key", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatalf("expected allow for trusted agent, got: %s", result.Reason)
	}
}

func TestRuntimePolicyEvaluator_DenyModelByTier(t *testing.T) {
	engine := NewEngine()
	logger := zaptest.NewLogger(t)
	eval := NewRuntimePolicyEvaluator(engine, logger)

	// Policy: deny frontier model calls in dev namespace.
	rego := `package runtime
deny[msg] { input.action == "model_call"; input.model.tier == "frontier"; input.agent.namespace == "dev"; msg = "frontier models not allowed in dev" }`
	if err := eval.LoadPolicy("no-frontier-dev", rego); err != nil {
		t.Fatalf("load policy: %v", err)
	}

	ctx := context.Background()

	// Frontier model in dev should be denied.
	rctx := RuntimeContext{
		AgentName: "agent-1",
		Namespace: "dev",
		SessionID: "sess-003",
		TrustTier: "standard",
	}
	result, err := eval.EvaluateModelCall(ctx, rctx, "gpt-4", "frontier", 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected denial for frontier model in dev")
	}

	// Frontier model in prod should be allowed.
	rctx.Namespace = "prod"
	result, err = eval.EvaluateModelCall(ctx, rctx, "gpt-4", "frontier", 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatalf("expected allow in prod, got: %s", result.Reason)
	}

	// Non-frontier model in dev should be allowed.
	rctx.Namespace = "dev"
	result, err = eval.EvaluateModelCall(ctx, rctx, "gpt-3.5", "standard", 500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatalf("expected allow for standard model in dev, got: %s", result.Reason)
	}
}

func TestRuntimePolicyEvaluator_NoPolicies(t *testing.T) {
	engine := NewEngine()
	logger := zaptest.NewLogger(t)
	eval := NewRuntimePolicyEvaluator(engine, logger)

	ctx := context.Background()
	rctx := RuntimeContext{
		AgentName: "agent-1",
		Namespace: "default",
		SessionID: "sess-004",
		TrustTier: "standard",
	}

	// No policies loaded = allow.
	result, err := eval.EvaluateToolCall(ctx, rctx, "anything", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatal("expected allow with no policies loaded")
	}
}

func TestRuntimePolicyEvaluator_MultiplePolicies(t *testing.T) {
	engine := NewEngine()
	logger := zaptest.NewLogger(t)
	eval := NewRuntimePolicyEvaluator(engine, logger)

	// Load two policies.
	rego1 := `package p1
deny[msg] { input.action == "tool_call"; input.tool.name == "dangerous"; msg = "dangerous tool denied" }`
	rego2 := `package p2
deny[msg] { input.action == "tool_call"; input.agent.trustTier == "untrusted"; msg = "untrusted agents cannot call tools" }`

	eval.LoadPolicy("no-dangerous", rego1)
	eval.LoadPolicy("no-untrusted-tools", rego2)

	ctx := context.Background()
	rctx := RuntimeContext{
		AgentName: "agent-1",
		Namespace: "default",
		SessionID: "sess-005",
		TrustTier: "standard",
	}

	// Standard agent calling safe tool = allow.
	result, err := eval.EvaluateToolCall(ctx, rctx, "safe-tool", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatalf("expected allow, got: %s", result.Reason)
	}

	// Standard agent calling dangerous tool = deny by p1.
	result, err = eval.EvaluateToolCall(ctx, rctx, "dangerous", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected denial for dangerous tool")
	}

	// Untrusted agent calling safe tool = deny by p2.
	rctx.TrustTier = "untrusted"
	result, err = eval.EvaluateToolCall(ctx, rctx, "safe-tool", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected denial for untrusted agent")
	}
}

func TestRuntimePolicyEvaluator_RemovePolicy(t *testing.T) {
	engine := NewEngine()
	logger := zaptest.NewLogger(t)
	eval := NewRuntimePolicyEvaluator(engine, logger)

	rego := `package runtime
deny[msg] { input.action == "tool_call"; input.tool.name == "blocked"; msg = "blocked" }`
	eval.LoadPolicy("block-tool", rego)

	ctx := context.Background()
	rctx := RuntimeContext{
		AgentName: "agent-1",
		Namespace: "default",
		SessionID: "sess-006",
		TrustTier: "standard",
	}

	// Should deny.
	result, _ := eval.EvaluateToolCall(ctx, rctx, "blocked", nil)
	if result.Allowed {
		t.Fatal("expected denial before removal")
	}

	// Remove policy.
	eval.RemovePolicy("block-tool")

	// Should now allow.
	result, _ = eval.EvaluateToolCall(ctx, rctx, "blocked", nil)
	if !result.Allowed {
		t.Fatal("expected allow after policy removal")
	}
}

func TestRuntimePolicyEvaluator_MemoryReadAllowed(t *testing.T) {
	engine := NewEngine()
	logger := zaptest.NewLogger(t)
	eval := NewRuntimePolicyEvaluator(engine, logger)

	// Only deny writes, reads should pass.
	rego := `package runtime
deny[msg] { input.action == "memory_op"; input.memory.operation == "write"; msg = "writes blocked" }`
	eval.LoadPolicy("no-write", rego)

	ctx := context.Background()
	rctx := RuntimeContext{
		AgentName: "agent-1",
		Namespace: "default",
		SessionID: "sess-007",
		TrustTier: "standard",
	}

	// Read should pass.
	result, err := eval.EvaluateMemoryOp(ctx, rctx, "read", "some-key", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Allowed {
		t.Fatalf("expected allow for read, got: %s", result.Reason)
	}

	// Write should be denied.
	result, err = eval.EvaluateMemoryOp(ctx, rctx, "write", "some-key", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected denial for write")
	}
}
