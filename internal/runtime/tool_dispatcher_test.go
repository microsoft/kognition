// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

type fakeExecutor struct {
	called    int
	lastReq   ToolExecutionRequest
	lastLimit ToolSandboxLimits
	delay     time.Duration
	res       ToolExecutionResult
	err       error
}

func (f *fakeExecutor) Execute(ctx context.Context, req ToolExecutionRequest, limits ToolSandboxLimits) (ToolExecutionResult, error) {
	f.called++
	f.lastReq = req
	f.lastLimit = limits
	if f.delay > 0 {
		timer := time.NewTimer(f.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ToolExecutionResult{}, ctx.Err()
		case <-timer.C:
		}
	}
	if f.err != nil {
		return ToolExecutionResult{}, f.err
	}
	return f.res, nil
}

// TC-0050: sandboxed tools cannot access agent runtime memory space.
func TestToolDispatcher_TC0050_SandboxIsolationBlocksRuntimeMemory(t *testing.T) {
	sandbox := &fakeExecutor{
		res: ToolExecutionResult{Output: map[string]any{"ok": true}},
	}
	trusted := &fakeExecutor{}
	d := NewToolDispatcher(sandbox, trusted)

	req := ToolExecutionRequest{
		Tool: v1alpha1.Tool{Spec: v1alpha1.ToolSpec{Trusted: false}},
		RuntimeMemory: map[string]any{
			"session.private": "top-secret",
		},
	}

	res, err := d.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("execute sandboxed tool: %v", err)
	}
	if res.ExecutionMode != "sandbox" {
		t.Fatalf("execution mode = %q, want sandbox", res.ExecutionMode)
	}
	if sandbox.called != 1 {
		t.Fatalf("sandbox executor calls = %d, want 1", sandbox.called)
	}
	if trusted.called != 0 {
		t.Fatalf("trusted executor should not be called, got %d", trusted.called)
	}
	if sandbox.lastReq.RuntimeMemory != nil {
		t.Fatalf("expected runtime memory to be stripped for sandbox execution, got %+v", sandbox.lastReq.RuntimeMemory)
	}
}

// TC-0051: timeout in sandboxed execution is killed and surfaced as structured timeout error.
func TestToolDispatcher_TC0051_SandboxTimeoutReturnsStructuredError(t *testing.T) {
	sandbox := &fakeExecutor{delay: 60 * time.Millisecond}
	trusted := &fakeExecutor{}
	d := NewToolDispatcher(sandbox, trusted)

	req := ToolExecutionRequest{
		Tool: v1alpha1.Tool{Spec: v1alpha1.ToolSpec{
			Sandbox: &v1alpha1.ToolSandboxSpec{Timeout: "15ms"},
		}},
	}

	_, err := d.Execute(context.Background(), req)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var structured *StructuredToolError
	if !errors.As(err, &structured) {
		t.Fatalf("expected StructuredToolError, got %T (%v)", err, err)
	}
	if structured.Code != "ToolTimeout" {
		t.Fatalf("error code = %q, want ToolTimeout", structured.Code)
	}
}

// TC-0052: trusted tools execute in-process and are faster than sandbox path.
func TestToolDispatcher_TC0052_TrustedTierUsesInProcessPathWithLowerLatency(t *testing.T) {
	sandbox := &fakeExecutor{delay: 20 * time.Millisecond, res: ToolExecutionResult{Output: map[string]any{"mode": "sandbox"}}}
	trusted := &fakeExecutor{delay: 1 * time.Millisecond, res: ToolExecutionResult{Output: map[string]any{"mode": "trusted"}}}
	d := NewToolDispatcher(sandbox, trusted)

	sandboxTool := ToolExecutionRequest{Tool: v1alpha1.Tool{Spec: v1alpha1.ToolSpec{Trusted: false}}}
	trustedTool := ToolExecutionRequest{Tool: v1alpha1.Tool{Spec: v1alpha1.ToolSpec{Trusted: true}}}

	sandboxRes, err := d.Execute(context.Background(), sandboxTool)
	if err != nil {
		t.Fatalf("sandbox execute: %v", err)
	}
	trustedRes, err := d.Execute(context.Background(), trustedTool)
	if err != nil {
		t.Fatalf("trusted execute: %v", err)
	}

	if trustedRes.ExecutionMode != "trusted" {
		t.Fatalf("trusted mode = %q, want trusted", trustedRes.ExecutionMode)
	}
	if sandboxRes.ExecutionMode != "sandbox" {
		t.Fatalf("sandbox mode = %q, want sandbox", sandboxRes.ExecutionMode)
	}
	if !(trustedRes.Duration < sandboxRes.Duration) {
		t.Fatalf("expected trusted duration (%s) < sandbox duration (%s)", trustedRes.Duration, sandboxRes.Duration)
	}
}

// TC-0053: human-approval tool blocks and resumes after approval submission.
func TestToolDispatcher_TC0053_HumanApprovalBlocksUntilApproved(t *testing.T) {
	broker := NewApprovalBroker()
	sandbox := &fakeExecutor{}
	trusted := &fakeExecutor{}
	d := NewToolDispatcherWithApprovalBroker(sandbox, trusted, broker)
	d.newID = func() string { return "approval-53" }

	resultCh := make(chan ToolExecutionResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := d.Execute(context.Background(), ToolExecutionRequest{
			Tool: v1alpha1.Tool{Spec: v1alpha1.ToolSpec{Endpoint: "builtin://human-approval"}},
		})
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- res
	}()

	select {
	case <-resultCh:
		t.Fatal("expected request to remain blocked before approval")
	case <-errCh:
		t.Fatal("expected no error before approval")
	case <-time.After(20 * time.Millisecond):
	}

	if err := broker.SubmitDecision("approval-53", ApprovalDecision{State: ApprovalStateApproved, Reason: "approved"}); err != nil {
		t.Fatalf("submit approval: %v", err)
	}

	select {
	case err := <-errCh:
		t.Fatalf("execute returned error: %v", err)
	case res := <-resultCh:
		if got := res.Output["decision"]; got != string(ApprovalStateApproved) {
			t.Fatalf("decision = %v, want %s", got, ApprovalStateApproved)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for approved result")
	}
}

// TC-0054: human-approval tool returns rejected decision when rejected.
func TestToolDispatcher_TC0054_HumanApprovalReturnsRejectedDecision(t *testing.T) {
	broker := NewApprovalBroker()
	d := NewToolDispatcherWithApprovalBroker(&fakeExecutor{}, &fakeExecutor{}, broker)
	d.newID = func() string { return "approval-54" }

	resultCh := make(chan ToolExecutionResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := d.Execute(context.Background(), ToolExecutionRequest{
			Tool: v1alpha1.Tool{Spec: v1alpha1.ToolSpec{Endpoint: "builtin://human-approval"}},
		})
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- res
	}()

	time.Sleep(20 * time.Millisecond)
	if err := broker.SubmitDecision("approval-54", ApprovalDecision{State: ApprovalStateRejected, Reason: "policy denied"}); err != nil {
		t.Fatalf("submit rejection: %v", err)
	}

	select {
	case err := <-errCh:
		t.Fatalf("execute returned error: %v", err)
	case res := <-resultCh:
		if got := res.Output["decision"]; got != string(ApprovalStateRejected) {
			t.Fatalf("decision = %v, want %s", got, ApprovalStateRejected)
		}
		if got := res.Output["reason"]; got != "policy denied" {
			t.Fatalf("reason = %v, want policy denied", got)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for rejected result")
	}
}

// TC-0055: human-approval tool timeout yields structured timeout response.
func TestToolDispatcher_TC0055_HumanApprovalTimeoutReturnsStructuredError(t *testing.T) {
	broker := NewApprovalBroker()
	d := NewToolDispatcherWithApprovalBroker(&fakeExecutor{}, &fakeExecutor{}, broker)
	d.newID = func() string { return "approval-55" }

	_, err := d.Execute(context.Background(), ToolExecutionRequest{
		Tool: v1alpha1.Tool{Spec: v1alpha1.ToolSpec{
			Endpoint: "builtin://human-approval",
			Sandbox:  &v1alpha1.ToolSandboxSpec{Timeout: "15ms"},
		}},
	})
	if err == nil {
		t.Fatal("expected approval timeout error")
	}
	var structured *StructuredToolError
	if !errors.As(err, &structured) {
		t.Fatalf("expected StructuredToolError, got %T (%v)", err, err)
	}
	if structured.Code != "ApprovalTimedOut" {
		t.Fatalf("error code = %q, want ApprovalTimedOut", structured.Code)
	}
}

func TestToolDispatcher_ExecutionFailureContracts(t *testing.T) {
	t.Run("nil dispatcher", func(t *testing.T) {
		var dispatcher *ToolDispatcher
		if _, err := dispatcher.Execute(context.Background(), ToolExecutionRequest{}); err == nil || err.Error() != "tool dispatcher is nil" {
			t.Fatalf("Execute() error = %v, want nil-dispatcher error", err)
		}
	})

	t.Run("missing executors", func(t *testing.T) {
		dispatcher := NewToolDispatcher(nil, nil)
		if _, err := dispatcher.Execute(context.Background(), ToolExecutionRequest{}); err == nil || err.Error() != "sandbox executor is required" {
			t.Fatalf("sandbox Execute() error = %v, want required-executor error", err)
		}

		dispatcher = NewToolDispatcher(&fakeExecutor{}, nil)
		request := ToolExecutionRequest{Tool: v1alpha1.Tool{Spec: v1alpha1.ToolSpec{Trusted: true}}}
		if _, err := dispatcher.Execute(context.Background(), request); err == nil || err.Error() != "trusted executor is required when tool.spec.trusted=true" {
			t.Fatalf("trusted Execute() error = %v, want required-executor error", err)
		}
	})

	t.Run("executor errors preserve identity", func(t *testing.T) {
		sandboxErr := errors.New("sandbox unavailable")
		trustedErr := errors.New("trusted unavailable")
		sandbox := &fakeExecutor{err: sandboxErr}
		trusted := &fakeExecutor{err: trustedErr}
		dispatcher := NewToolDispatcher(sandbox, trusted)

		if _, err := dispatcher.Execute(context.Background(), ToolExecutionRequest{}); !errors.Is(err, sandboxErr) {
			t.Fatalf("sandbox Execute() error = %v, want original executor error", err)
		}
		request := ToolExecutionRequest{Tool: v1alpha1.Tool{Spec: v1alpha1.ToolSpec{Trusted: true}}}
		if _, err := dispatcher.Execute(context.Background(), request); !errors.Is(err, trustedErr) {
			t.Fatalf("trusted Execute() error = %v, want original executor error", err)
		}
	})

	t.Run("invalid sandbox configuration fails before execution", func(t *testing.T) {
		sandbox := &fakeExecutor{}
		dispatcher := NewToolDispatcher(sandbox, &fakeExecutor{})
		request := ToolExecutionRequest{Tool: v1alpha1.Tool{Spec: v1alpha1.ToolSpec{
			Sandbox: &v1alpha1.ToolSandboxSpec{Timeout: "not-a-duration"},
		}}}

		_, err := dispatcher.Execute(context.Background(), request)
		var structured *StructuredToolError
		if !errors.As(err, &structured) || structured.Code != "InvalidSandboxConfig" || structured.Retryable {
			t.Fatalf("Execute() error = %v, want non-retryable InvalidSandboxConfig", err)
		}
		if sandbox.called != 0 {
			t.Fatalf("sandbox executor calls = %d, want 0", sandbox.called)
		}
	})

	t.Run("approval metadata requires broker", func(t *testing.T) {
		dispatcher := NewToolDispatcher(&fakeExecutor{}, &fakeExecutor{})
		request := ToolExecutionRequest{Tool: v1alpha1.Tool{Spec: v1alpha1.ToolSpec{
			Metadata: map[string]string{"builtin": "human-approval"},
		}}}
		if _, err := dispatcher.Execute(context.Background(), request); err == nil || err.Error() != "approval broker is required for builtin human-approval tool" {
			t.Fatalf("Execute() error = %v, want required-broker error", err)
		}
	})
}
