// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package opa

import (
	"context"
	"fmt"
	"sync"

	"go.uber.org/zap"
)

// RuntimeAction represents the type of runtime operation being evaluated.
type RuntimeAction string

const (
	// ActionToolCall evaluates a tool invocation.
	ActionToolCall RuntimeAction = "tool_call"
	// ActionMemoryOp evaluates a memory read/write operation.
	ActionMemoryOp RuntimeAction = "memory_op"
	// ActionModelCall evaluates an LLM model invocation.
	ActionModelCall RuntimeAction = "model_call"
)

// RuntimeContext carries session-level data passed into OPA evaluation.
type RuntimeContext struct {
	AgentName string
	Namespace string
	SessionID string
	TrustTier string
}

// RuntimeRequest describes the runtime operation to evaluate.
type RuntimeRequest struct {
	Action RuntimeAction
	// Tool-call fields
	ToolName string
	ToolArgs map[string]interface{}
	// Memory-op fields
	MemoryOp   string // "read", "write", "delete"
	MemoryKey  string
	MemoryMeta map[string]interface{}
	// Model-call fields
	ModelName  string
	ModelTier  string
	TokenCount int64
}

// RuntimePolicyEvaluator evaluates OPA/Rego policies at runtime for tool calls,
// memory operations, and model invocations. It provides defense-in-depth
// enforcement beyond admission-time checks.
type RuntimePolicyEvaluator struct {
	engine *Engine
	logger *zap.Logger
	mu     sync.RWMutex
}

// NewRuntimePolicyEvaluator creates a new RuntimePolicyEvaluator.
func NewRuntimePolicyEvaluator(engine *Engine, logger *zap.Logger) *RuntimePolicyEvaluator {
	return &RuntimePolicyEvaluator{
		engine: engine,
		logger: logger.With(zap.String("component", "runtime-policy")),
	}
}

// LoadPolicy loads a Rego policy for runtime evaluation.
func (r *RuntimePolicyEvaluator) LoadPolicy(name, regoSource string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.engine.LoadPolicy(name, regoSource)
}

// RemovePolicy removes a runtime policy.
func (r *RuntimePolicyEvaluator) RemovePolicy(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.engine.RemovePolicy(name)
}

// Evaluate evaluates all loaded runtime policies against the given context and request.
// Returns an EvalResult indicating whether the operation is allowed.
func (r *RuntimePolicyEvaluator) Evaluate(ctx context.Context, rctx RuntimeContext, req RuntimeRequest) (*EvalResult, error) {
	input := r.buildInput(rctx, req)

	r.mu.RLock()
	defer r.mu.RUnlock()

	result, err := r.engine.EvaluateAll(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("runtime policy evaluation: %w", err)
	}

	if !result.Allowed {
		r.logger.Warn("runtime policy denied operation",
			zap.String("action", string(req.Action)),
			zap.String("agent", rctx.AgentName),
			zap.String("namespace", rctx.Namespace),
			zap.String("reason", result.Reason))
	}

	return result, nil
}

// EvaluateToolCall is a convenience method for evaluating tool invocations.
func (r *RuntimePolicyEvaluator) EvaluateToolCall(ctx context.Context, rctx RuntimeContext, toolName string, args map[string]interface{}) (*EvalResult, error) {
	return r.Evaluate(ctx, rctx, RuntimeRequest{
		Action:   ActionToolCall,
		ToolName: toolName,
		ToolArgs: args,
	})
}

// EvaluateMemoryOp is a convenience method for evaluating memory operations.
func (r *RuntimePolicyEvaluator) EvaluateMemoryOp(ctx context.Context, rctx RuntimeContext, op string, key string, meta map[string]interface{}) (*EvalResult, error) {
	return r.Evaluate(ctx, rctx, RuntimeRequest{
		Action:     ActionMemoryOp,
		MemoryOp:   op,
		MemoryKey:  key,
		MemoryMeta: meta,
	})
}

// EvaluateModelCall is a convenience method for evaluating model invocations.
func (r *RuntimePolicyEvaluator) EvaluateModelCall(ctx context.Context, rctx RuntimeContext, modelName, modelTier string, tokenCount int64) (*EvalResult, error) {
	return r.Evaluate(ctx, rctx, RuntimeRequest{
		Action:     ActionModelCall,
		ModelName:  modelName,
		ModelTier:  modelTier,
		TokenCount: tokenCount,
	})
}

// buildInput constructs the OPA input map from context and request.
func (r *RuntimePolicyEvaluator) buildInput(rctx RuntimeContext, req RuntimeRequest) map[string]interface{} {
	input := map[string]interface{}{
		"action": string(req.Action),
		"agent": map[string]interface{}{
			"name":      rctx.AgentName,
			"namespace": rctx.Namespace,
			"trustTier": rctx.TrustTier,
		},
		"session": map[string]interface{}{
			"id": rctx.SessionID,
		},
	}

	switch req.Action {
	case ActionToolCall:
		tool := map[string]interface{}{
			"name": req.ToolName,
		}
		if req.ToolArgs != nil {
			tool["args"] = req.ToolArgs
		}
		input["tool"] = tool
	case ActionMemoryOp:
		mem := map[string]interface{}{
			"operation": req.MemoryOp,
			"key":       req.MemoryKey,
		}
		if req.MemoryMeta != nil {
			mem["meta"] = req.MemoryMeta
		}
		input["memory"] = mem
	case ActionModelCall:
		input["model"] = map[string]interface{}{
			"name":       req.ModelName,
			"tier":       req.ModelTier,
			"tokenCount": req.TokenCount,
		}
	}

	return input
}
