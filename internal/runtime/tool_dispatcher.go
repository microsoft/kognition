// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

const (
	defaultSandboxTimeout  = 5 * time.Second
	defaultSandboxCPUMilli = int64(500)
	defaultSandboxMemoryMB = int64(256)
)

// ToolExecutionRequest represents one tool invocation by an agent runtime.
type ToolExecutionRequest struct {
	InvocationID  string
	Tool          v1alpha1.Tool
	Input         map[string]any
	RuntimeMemory map[string]any
}

// ToolExecutionResult captures invocation output and execution metadata.
type ToolExecutionResult struct {
	Output                map[string]any
	CompletionUnconfirmed bool
	ExecutionMode         string
	Duration              time.Duration
	RuntimeMemBytes       int
}

// StructuredToolError provides machine-readable tool execution failures.
type StructuredToolError struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *StructuredToolError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ToolExecutor executes a request.
type ToolExecutor interface {
	Execute(ctx context.Context, req ToolExecutionRequest, limits ToolSandboxLimits) (ToolExecutionResult, error)
}

// ToolSandboxLimits captures runtime limits for sandboxed execution.
type ToolSandboxLimits struct {
	Timeout  time.Duration
	CPUMilli int64
	MemoryMB int64
}

// ToolDispatcher selects sandbox or trusted execution mode per tool.
type ToolDispatcher struct {
	sandboxExecutor ToolExecutor
	trustedExecutor ToolExecutor
	approvalBroker  *ApprovalBroker
	now             func() time.Time
	newID           func() string
}

func NewToolDispatcher(sandboxExecutor, trustedExecutor ToolExecutor) *ToolDispatcher {
	return &ToolDispatcher{
		sandboxExecutor: sandboxExecutor,
		trustedExecutor: trustedExecutor,
		approvalBroker:  nil,
		now:             time.Now,
		newID:           func() string { return uuid.New().String() },
	}
}

func NewToolDispatcherWithApprovalBroker(sandboxExecutor, trustedExecutor ToolExecutor, approvalBroker *ApprovalBroker) *ToolDispatcher {
	d := NewToolDispatcher(sandboxExecutor, trustedExecutor)
	d.approvalBroker = approvalBroker
	return d
}

func (d *ToolDispatcher) Execute(ctx context.Context, req ToolExecutionRequest) (ToolExecutionResult, error) {
	if d == nil {
		return ToolExecutionResult{}, errors.New("tool dispatcher is nil")
	}
	if d.sandboxExecutor == nil {
		return ToolExecutionResult{}, errors.New("sandbox executor is required")
	}

	if isHumanApprovalTool(req.Tool) {
		return d.executeHumanApproval(ctx, req)
	}

	if req.Tool.Spec.Trusted {
		if d.trustedExecutor == nil {
			return ToolExecutionResult{}, errors.New("trusted executor is required when tool.spec.trusted=true")
		}
		start := d.now()
		res, err := d.trustedExecutor.Execute(ctx, req, ToolSandboxLimits{})
		if err != nil {
			return ToolExecutionResult{}, err
		}
		res.ExecutionMode = "trusted"
		res.Duration = d.now().Sub(start)
		return res, nil
	}

	limits, err := resolveSandboxLimits(req.Tool.Spec.Sandbox)
	if err != nil {
		return ToolExecutionResult{}, err
	}
	isolatedReq := req
	isolatedReq.RuntimeMemory = nil

	timedCtx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()

	start := d.now()
	res, err := d.sandboxExecutor.Execute(timedCtx, isolatedReq, limits)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(timedCtx.Err(), context.DeadlineExceeded) {
			return ToolExecutionResult{}, &StructuredToolError{
				Code:      "ToolTimeout",
				Message:   fmt.Sprintf("sandboxed tool invocation exceeded timeout %s", limits.Timeout),
				Retryable: true,
			}
		}
		return ToolExecutionResult{}, err
	}
	res.ExecutionMode = "sandbox"
	res.Duration = d.now().Sub(start)
	return res, nil
}

func isHumanApprovalTool(tool v1alpha1.Tool) bool {
	if tool.Spec.Metadata["builtin"] == "human-approval" {
		return true
	}
	if tool.Spec.Endpoint == "builtin://human-approval" {
		return true
	}
	return false
}

func (d *ToolDispatcher) executeHumanApproval(ctx context.Context, req ToolExecutionRequest) (ToolExecutionResult, error) {
	if d.approvalBroker == nil {
		return ToolExecutionResult{}, errors.New("approval broker is required for builtin human-approval tool")
	}

	limits, err := resolveSandboxLimits(req.Tool.Spec.Sandbox)
	if err != nil {
		return ToolExecutionResult{}, err
	}

	approvalID := d.newID()
	timedCtx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()

	start := d.now()
	decision, err := d.approvalBroker.Wait(timedCtx, ApprovalRequest{
		ID:        approvalID,
		Namespace: req.Tool.ObjectMeta.Namespace,
		ToolName:  req.Tool.ObjectMeta.Name,
		Input:     req.Input,
		CreatedAt: d.now().UTC(),
	})
	if err != nil {
		return ToolExecutionResult{}, err
	}

	if decision.State == ApprovalStateTimedOut {
		return ToolExecutionResult{}, &StructuredToolError{
			Code:      "ApprovalTimedOut",
			Message:   "human approval request timed out",
			Retryable: true,
		}
	}

	result := ToolExecutionResult{
		Output: map[string]any{
			"approvalId":   approvalID,
			"decision":     string(decision.State),
			"reason":       decision.Reason,
			"modification": decision.Modification,
		},
		ExecutionMode: "builtin",
		Duration:      d.now().Sub(start),
	}
	return result, nil
}

func resolveSandboxLimits(spec *v1alpha1.ToolSandboxSpec) (ToolSandboxLimits, error) {
	limits := ToolSandboxLimits{
		Timeout:  defaultSandboxTimeout,
		CPUMilli: defaultSandboxCPUMilli,
		MemoryMB: defaultSandboxMemoryMB,
	}
	if spec == nil {
		return limits, nil
	}
	if spec.Timeout != "" {
		d, err := time.ParseDuration(spec.Timeout)
		if err != nil {
			return ToolSandboxLimits{}, &StructuredToolError{Code: "InvalidSandboxConfig", Message: "tool.spec.sandbox.timeout is invalid", Retryable: false}
		}
		limits.Timeout = d
	}
	if spec.CPUMilli > 0 {
		limits.CPUMilli = spec.CPUMilli
	}
	if spec.MemoryMB > 0 {
		limits.MemoryMB = spec.MemoryMB
	}
	return limits, nil
}
