// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/store"
)

const toolInvocationRunKind = "ToolInvocationRun"

// StdioInvocationTarget is the trusted sandbox configuration resolved from a
// ToolClass. Neither the Tool nor invocation input can select its image or argv.
type StdioInvocationTarget struct {
	Image    string
	Command  []string
	CPUMilli int64
	MemoryMB int64
	Timeout  time.Duration
}

// StdioInvocationResult is the normalized result returned by a sandboxed MCP
// stdio server for one tools/call request.
type StdioInvocationResult struct {
	Output        map[string]any
	ExecutionMode string
}

// StdioInvocationRunner starts an isolated, class-owned stdio sandbox. It
// must never execute Tool input in the controller process.
type StdioInvocationRunner interface {
	RunStdioInvocation(context.Context, *v1alpha1.Tool, StdioInvocationTarget, map[string]any) (StdioInvocationResult, error)
}

// ToolInvocationRunReconciler reconciles durable tool invocation requests.
// It persists Running before sandbox execution so status always explains an
// interrupted or failed invocation.
type ToolInvocationRunReconciler struct {
	store  store.Store
	logger *zap.Logger
	runner StdioInvocationRunner
	now    func() time.Time
}

// ToolInvocationRunReconcilerOption configures the invocation reconciler.
type ToolInvocationRunReconcilerOption func(*ToolInvocationRunReconciler)

// WithStdioInvocationRunner configures the isolated sandbox backend.
func WithStdioInvocationRunner(runner StdioInvocationRunner) ToolInvocationRunReconcilerOption {
	return func(reconciler *ToolInvocationRunReconciler) { reconciler.runner = runner }
}

// NewToolInvocationRunReconciler creates a durable stdio invocation reconciler.
func NewToolInvocationRunReconciler(s store.Store, logger *zap.Logger, options ...ToolInvocationRunReconcilerOption) *ToolInvocationRunReconciler {
	if logger == nil {
		logger = zap.NewNop()
	}
	reconciler := &ToolInvocationRunReconciler{store: s, logger: logger.Named("tool-invocation-run"), now: time.Now}
	for _, option := range options {
		option(reconciler)
	}
	return reconciler
}

// Reconcile implements the durable ToolInvocationRun state machine.
func (r *ToolInvocationRunReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	raw, err := r.store.Get(ctx, toolInvocationRunKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get ToolInvocationRun: %w", err)
	}
	var run v1alpha1.ToolInvocationRun
	if err := json.Unmarshal(raw, &run); err != nil {
		return Result{}, fmt.Errorf("unmarshal ToolInvocationRun: %w", err)
	}
	if isTerminalInvocationPhase(run.Status.Phase) {
		return Result{}, nil
	}
	if run.Status.Phase == "" || run.Status.Phase == v1alpha1.PhasePending {
		run.Status.Phase = v1alpha1.PhaseRunning
		now := r.now().UTC()
		run.Status.StartedAt = &now
		run.Status.Conditions = lifecycleToAPIConditions(lifecycle.SetCondition(
			apiToLifecycleConditions(run.Status.Conditions), lifecycle.ConditionProgressing, lifecycle.ConditionTrue,
			"Executing", "class-owned stdio sandbox invocation is starting",
		))
		if err := r.updateRun(ctx, &run); err != nil {
			return Result{}, fmt.Errorf("persist running ToolInvocationRun: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	tool, target, err := r.resolveTarget(ctx, &run)
	if err != nil {
		return r.failRun(ctx, &run, "ToolInvocationTargetUnavailable", err)
	}
	if r.runner == nil {
		return r.failRun(ctx, &run, "StdioSandboxUnavailable", fmt.Errorf("no stdio sandbox runner is configured"))
	}

	start := r.now()
	result, err := r.runner.RunStdioInvocation(ctx, tool, target, cloneInvocationJSONMap(run.Spec.Input))
	duration := r.now().Sub(start)
	if err != nil {
		return r.failRunWithDuration(ctx, &run, "StdioInvocationFailed", err, duration)
	}
	now := r.now().UTC()
	run.Status.Phase = v1alpha1.PhaseReady
	run.Status.Output = cloneInvocationJSONMap(result.Output)
	run.Status.ExecutionMode = result.ExecutionMode
	run.Status.DurationMillis = duration.Milliseconds()
	run.Status.CompletedAt = &now
	run.Status.Conditions = lifecycleToAPIConditions(lifecycle.SetCondition(
		apiToLifecycleConditions(run.Status.Conditions), lifecycle.ConditionReady, lifecycle.ConditionTrue,
		"Completed", "stdio sandbox invocation completed successfully",
	))
	if err := r.updateRun(ctx, &run); err != nil {
		return Result{}, fmt.Errorf("persist completed ToolInvocationRun: %w", err)
	}
	return Result{}, nil
}

func (r *ToolInvocationRunReconciler) resolveTarget(ctx context.Context, run *v1alpha1.ToolInvocationRun) (*v1alpha1.Tool, StdioInvocationTarget, error) {
	if strings.TrimSpace(run.Spec.ToolName) == "" || run.Spec.ToolResourceVersion <= 0 || strings.TrimSpace(run.Spec.InvocationID) == "" {
		return nil, StdioInvocationTarget{}, fmt.Errorf("toolName, toolResourceVersion, and invocationID are required")
	}
	raw, err := r.store.Get(ctx, toolKind, run.ObjectMeta.Namespace, run.Spec.ToolName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, StdioInvocationTarget{}, fmt.Errorf("Tool %q was not found", run.Spec.ToolName)
		}
		return nil, StdioInvocationTarget{}, fmt.Errorf("get Tool %q: %w", run.Spec.ToolName, err)
	}
	var tool v1alpha1.Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		return nil, StdioInvocationTarget{}, fmt.Errorf("unmarshal Tool %q: %w", run.Spec.ToolName, err)
	}
	if tool.ObjectMeta.ResourceVersion != run.Spec.ToolResourceVersion {
		return nil, StdioInvocationTarget{}, fmt.Errorf("Tool resource version changed: expected %d, got %d", run.Spec.ToolResourceVersion, tool.ObjectMeta.ResourceVersion)
	}
	if tool.Spec.EffectiveProtocol() != v1alpha1.ToolProtocolShell {
		return nil, StdioInvocationTarget{}, fmt.Errorf("Tool %q must use shell protocol for stdio sandbox invocation", tool.ObjectMeta.Name)
	}
	if tool.Status.Phase != v1alpha1.PhaseReady {
		return nil, StdioInvocationTarget{}, fmt.Errorf("Tool %q is not Ready", tool.ObjectMeta.Name)
	}
	target, err := resolveClassStdioInvocationTarget(ctx, r.store, &tool)
	if err != nil {
		return nil, StdioInvocationTarget{}, err
	}
	return &tool, target, nil
}

func resolveClassStdioInvocationTarget(ctx context.Context, st store.Store, tool *v1alpha1.Tool) (StdioInvocationTarget, error) {
	if strings.TrimSpace(tool.Spec.ClassName) == "" {
		return StdioInvocationTarget{}, fmt.Errorf("Tool %q requires spec.className", tool.ObjectMeta.Name)
	}

	classRaw, err := st.Get(ctx, toolClassKind, "", tool.Spec.ClassName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return StdioInvocationTarget{}, fmt.Errorf("ToolClass %q was not found", tool.Spec.ClassName)
		}
		return StdioInvocationTarget{}, fmt.Errorf("get ToolClass %q: %w", tool.Spec.ClassName, err)
	}
	var class v1alpha1.ToolClass
	if err := json.Unmarshal(classRaw, &class); err != nil {
		return StdioInvocationTarget{}, fmt.Errorf("unmarshal ToolClass %q: %w", tool.Spec.ClassName, err)
	}
	if class.Status.Phase != v1alpha1.PhaseReady || class.Spec.ExecutionTier != v1alpha1.ExecutionTierPod || strings.TrimSpace(class.Spec.SandboxImage) == "" || len(class.Spec.StdioCommand) == 0 {
		return StdioInvocationTarget{}, fmt.Errorf("ToolClass %q must be Ready with pod executionTier, sandboxImage, and stdioCommand", class.ObjectMeta.Name)
	}
	timeout, err := resolveStdioInvocationTimeout(class.Spec.SandboxDefaults, tool.Spec.Sandbox, class.Spec.ResourceLimits)
	if err != nil {
		return StdioInvocationTarget{}, err
	}
	target := StdioInvocationTarget{Image: class.Spec.SandboxImage, Command: append([]string(nil), class.Spec.StdioCommand...), CPUMilli: defaultExecProbeCPUMilli, MemoryMB: defaultExecProbeMemoryMB, Timeout: timeout}
	if class.Spec.SandboxDefaults != nil {
		if class.Spec.SandboxDefaults.CPUMilli > 0 {
			target.CPUMilli = class.Spec.SandboxDefaults.CPUMilli
		}
		if class.Spec.SandboxDefaults.MemoryMB > 0 {
			target.MemoryMB = class.Spec.SandboxDefaults.MemoryMB
		}
	}
	if tool.Spec.Sandbox != nil {
		if tool.Spec.Sandbox.CPUMilli > 0 {
			target.CPUMilli = tool.Spec.Sandbox.CPUMilli
		}
		if tool.Spec.Sandbox.MemoryMB > 0 {
			target.MemoryMB = tool.Spec.Sandbox.MemoryMB
		}
	}
	if limits := class.Spec.ResourceLimits; limits != nil {
		if limits.MaxCPUMilli > 0 && target.CPUMilli > limits.MaxCPUMilli {
			return StdioInvocationTarget{}, fmt.Errorf("stdio invocation CPU %d exceeds ToolClass limit %d", target.CPUMilli, limits.MaxCPUMilli)
		}
		if limits.MaxMemoryMB > 0 && target.MemoryMB > limits.MaxMemoryMB {
			return StdioInvocationTarget{}, fmt.Errorf("stdio invocation memory %d exceeds ToolClass limit %d", target.MemoryMB, limits.MaxMemoryMB)
		}
	}
	return target, nil
}

func resolveStdioInvocationTimeout(defaults, sandbox *v1alpha1.ToolSandboxSpec, limits *v1alpha1.ToolClassResourceLimits) (time.Duration, error) {
	timeout := defaultProbeTimeout
	configuredSandbox := defaults
	if sandbox != nil && strings.TrimSpace(sandbox.Timeout) != "" {
		configuredSandbox = sandbox
	}
	if configuredSandbox != nil && strings.TrimSpace(configuredSandbox.Timeout) != "" {
		resolved, err := parsePositiveExecProbeDuration("sandbox timeout", configuredSandbox.Timeout)
		if err != nil {
			return 0, err
		}
		timeout = resolved
	}
	if limits != nil && strings.TrimSpace(limits.MaxTimeout) != "" {
		maximum, err := parsePositiveExecProbeDuration("ToolClass maximum timeout", limits.MaxTimeout)
		if err != nil {
			return 0, err
		}
		if timeout > maximum {
			return 0, fmt.Errorf("stdio invocation timeout %s exceeds ToolClass limit %s", timeout, maximum)
		}
	}
	return timeout, nil
}

func (r *ToolInvocationRunReconciler) failRun(ctx context.Context, run *v1alpha1.ToolInvocationRun, code string, cause error) (Result, error) {
	return r.failRunWithDuration(ctx, run, code, cause, 0)
}

func (r *ToolInvocationRunReconciler) failRunWithDuration(ctx context.Context, run *v1alpha1.ToolInvocationRun, code string, cause error, duration time.Duration) (Result, error) {
	now := r.now().UTC()
	run.Status.Phase = v1alpha1.PhaseFailed
	run.Status.ErrorCode = code
	run.Status.ErrorMessage = cause.Error()
	run.Status.DurationMillis = duration.Milliseconds()
	run.Status.CompletedAt = &now
	run.Status.Conditions = lifecycleToAPIConditions(lifecycle.SetCondition(
		apiToLifecycleConditions(run.Status.Conditions), lifecycle.ConditionReady, lifecycle.ConditionFalse, code, cause.Error(),
	))
	if err := r.updateRun(ctx, run); err != nil {
		return Result{}, fmt.Errorf("persist failed ToolInvocationRun: %w", err)
	}
	return Result{}, nil
}

func (r *ToolInvocationRunReconciler) updateRun(ctx context.Context, run *v1alpha1.ToolInvocationRun) error {
	data, err := json.Marshal(run)
	if err != nil {
		return err
	}
	return r.store.Update(ctx, toolInvocationRunKind, run.ObjectMeta.Namespace, run.ObjectMeta.Name, data, run.ObjectMeta.ResourceVersion)
}

func isTerminalInvocationPhase(phase v1alpha1.Phase) bool {
	return phase == v1alpha1.PhaseReady || phase == v1alpha1.PhaseFailed || phase == v1alpha1.PhaseTerminated
}

func cloneInvocationJSONMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	return kruntime.DeepCopyJSON(value)
}
