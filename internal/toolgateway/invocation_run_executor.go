// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/store"
)

const toolInvocationRunKind = "ToolInvocationRun"

// InvocationRunExecutor delegates network protocols to fallback and submits
// Shell Tools as durable controller-owned sandbox requests. The gateway never
// chooses an image, command, Pod, or Job.
type InvocationRunExecutor struct {
	store    store.Store
	fallback Executor
	poll     time.Duration
}

func NewInvocationRunExecutor(st store.Store, fallback Executor) *InvocationRunExecutor {
	return &InvocationRunExecutor{store: st, fallback: fallback, poll: 100 * time.Millisecond}
}

func (e *InvocationRunExecutor) Execute(ctx context.Context, request runtime.ToolExecutionRequest) (runtime.ToolExecutionResult, error) {
	if request.Tool.Spec.EffectiveProtocol() != v1alpha1.ToolProtocolShell {
		if e == nil || e.fallback == nil {
			return runtime.ToolExecutionResult{}, errors.New("tool gateway fallback executor is required")
		}
		return e.fallback.Execute(ctx, request)
	}
	if e == nil || e.store == nil {
		return runtime.ToolExecutionResult{}, errors.New("tool invocation run executor requires a store")
	}
	if strings.TrimSpace(request.InvocationID) == "" || request.Tool.ObjectMeta.Namespace == "" || request.Tool.ObjectMeta.ResourceVersion <= 0 {
		return runtime.ToolExecutionResult{}, &runtime.StructuredToolError{Code: "ToolInvocationRunInvalid", Message: "Shell Tool invocation requires a namespace, resource version, and invocation ID"}
	}
	name := invocationRunName(request.InvocationID)
	run := v1alpha1.ToolInvocationRun{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: toolInvocationRunKind},
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: request.Tool.ObjectMeta.Namespace},
		Spec:       v1alpha1.ToolInvocationRunSpec{ToolName: request.Tool.ObjectMeta.Name, ToolResourceVersion: request.Tool.ObjectMeta.ResourceVersion, InvocationID: request.InvocationID, Input: cloneInvocationInput(request.Input)},
	}
	if err := e.createOrVerifyRun(ctx, &run); err != nil {
		return runtime.ToolExecutionResult{}, err
	}
	return e.waitForRun(ctx, run.ObjectMeta.Namespace, run.ObjectMeta.Name)
}

func (e *InvocationRunExecutor) createOrVerifyRun(ctx context.Context, run *v1alpha1.ToolInvocationRun) error {
	data, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("marshal ToolInvocationRun: %w", err)
	}
	if err := e.store.Create(ctx, toolInvocationRunKind, run.ObjectMeta.Namespace, run.ObjectMeta.Name, data); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrAlreadyExists) {
		return fmt.Errorf("create ToolInvocationRun: %w", err)
	}
	raw, err := e.store.Get(ctx, toolInvocationRunKind, run.ObjectMeta.Namespace, run.ObjectMeta.Name)
	if err != nil {
		return fmt.Errorf("get existing ToolInvocationRun: %w", err)
	}
	var existing v1alpha1.ToolInvocationRun
	if err := json.Unmarshal(raw, &existing); err != nil {
		return fmt.Errorf("unmarshal existing ToolInvocationRun: %w", err)
	}
	if existing.Spec.InvocationID != run.Spec.InvocationID || existing.Spec.ToolName != run.Spec.ToolName || existing.Spec.ToolResourceVersion != run.Spec.ToolResourceVersion || !sameJSONMap(existing.Spec.Input, run.Spec.Input) {
		return store.ErrConflict
	}
	return nil
}

func (e *InvocationRunExecutor) waitForRun(ctx context.Context, namespace, name string) (runtime.ToolExecutionResult, error) {
	poll := e.poll
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		raw, err := e.store.Get(ctx, toolInvocationRunKind, namespace, name)
		if err != nil {
			return runtime.ToolExecutionResult{}, fmt.Errorf("get ToolInvocationRun: %w", err)
		}
		var run v1alpha1.ToolInvocationRun
		if err := json.Unmarshal(raw, &run); err != nil {
			return runtime.ToolExecutionResult{}, fmt.Errorf("unmarshal ToolInvocationRun: %w", err)
		}
		switch run.Status.Phase {
		case v1alpha1.PhaseReady:
			return runtime.ToolExecutionResult{Output: cloneInvocationInput(run.Status.Output), ExecutionMode: run.Status.ExecutionMode, Duration: time.Duration(run.Status.DurationMillis) * time.Millisecond}, nil
		case v1alpha1.PhaseFailed, v1alpha1.PhaseTerminated:
			code := run.Status.ErrorCode
			if code == "" {
				code = "ToolInvocationRunFailed"
			}
			message := run.Status.ErrorMessage
			if message == "" {
				message = "durable Tool invocation failed"
			}
			return runtime.ToolExecutionResult{}, &runtime.StructuredToolError{Code: code, Message: message}
		case v1alpha1.PhasePending, v1alpha1.PhaseRunning, "":
		default:
			return runtime.ToolExecutionResult{}, fmt.Errorf("ToolInvocationRun %q has unexpected phase %q", name, run.Status.Phase)
		}
		select {
		case <-ctx.Done():
			return runtime.ToolExecutionResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func invocationRunName(invocationID string) string {
	digest := sha256.Sum256([]byte(invocationID))
	return "tool-invocation-" + hex.EncodeToString(digest[:8])
}

func cloneInvocationInput(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	data, _ := json.Marshal(value)
	var clone map[string]any
	_ = json.Unmarshal(data, &clone)
	return clone
}

func sameJSONMap(left, right map[string]any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}
