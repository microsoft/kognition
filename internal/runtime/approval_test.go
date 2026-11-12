// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

func waitForApprovalRegistration(t *testing.T, broker *ApprovalBroker, approvalID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		broker.mu.Lock()
		_, registered := broker.waiters[approvalID]
		broker.mu.Unlock()
		if registered {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("approval %q was not registered", approvalID)
}

func TestApprovalBrokerWaitValidatesAndOwnsRequestID(t *testing.T) {
	broker := NewApprovalBroker()

	if _, err := broker.Wait(context.Background(), ApprovalRequest{}); err == nil || !strings.Contains(err.Error(), "id is required") {
		t.Fatalf("Wait() error = %v, want required ID error", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan ApprovalDecision, 1)
	go func() {
		decision, _ := broker.Wait(ctx, ApprovalRequest{ID: "duplicate"})
		result <- decision
	}()
	waitForApprovalRegistration(t, broker, "duplicate")

	if _, err := broker.Wait(context.Background(), ApprovalRequest{ID: "duplicate"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate Wait() error = %v", err)
	}
	cancel()
	if decision := <-result; decision.State != ApprovalStateTimedOut {
		t.Fatalf("canceled Wait() state = %q, want %q", decision.State, ApprovalStateTimedOut)
	}
	if err := broker.SubmitDecision("duplicate", ApprovalDecision{State: ApprovalStateApproved}); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("SubmitDecision() after cancellation error = %v, want %v", err, ErrApprovalNotFound)
	}
}

func TestApprovalBrokerSubmitDecisionValidatesState(t *testing.T) {
	broker := NewApprovalBroker()
	for _, state := range []ApprovalState{"", ApprovalStatePending, ApprovalStateTimedOut, "unknown"} {
		err := broker.SubmitDecision("approval", ApprovalDecision{State: state})
		if err == nil {
			t.Fatalf("SubmitDecision(state=%q) error = nil", state)
		}
	}
	if err := broker.SubmitDecision("missing", ApprovalDecision{State: ApprovalStateRejected}); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("SubmitDecision() error = %v, want %v", err, ErrApprovalNotFound)
	}
}

func TestApprovalBrokerRejectsCompletedApprovalID(t *testing.T) {
	broker := NewApprovalBroker()
	result := make(chan ApprovalDecision, 1)
	go func() {
		decision, _ := broker.Wait(context.Background(), ApprovalRequest{ID: "completed"})
		result <- decision
	}()
	waitForApprovalRegistration(t, broker, "completed")

	if err := broker.SubmitDecision("completed", ApprovalDecision{State: ApprovalStateApproved}); err != nil {
		t.Fatalf("first SubmitDecision() error = %v", err)
	}
	if decision := <-result; decision.State != ApprovalStateApproved {
		t.Fatalf("Wait() state = %q, want %q", decision.State, ApprovalStateApproved)
	}
	if err := broker.SubmitDecision("completed", ApprovalDecision{State: ApprovalStateRejected}); !errors.Is(err, ErrApprovalClosed) {
		t.Fatalf("second SubmitDecision() error = %v, want %v", err, ErrApprovalClosed)
	}
	if _, err := broker.Wait(context.Background(), ApprovalRequest{ID: "completed"}); !errors.Is(err, ErrApprovalClosed) {
		t.Fatalf("second Wait() error = %v, want %v", err, ErrApprovalClosed)
	}
}

func TestApprovalBrokerBoundsCompletedApprovalHistory(t *testing.T) {
	broker := NewApprovalBroker()
	for index := 0; index <= maxClosedApprovals; index++ {
		approvalID := fmt.Sprintf("completed-%d", index)
		result := make(chan ApprovalDecision, 1)
		go func() {
			decision, _ := broker.Wait(context.Background(), ApprovalRequest{ID: approvalID})
			result <- decision
		}()
		waitForApprovalRegistration(t, broker, approvalID)
		if err := broker.SubmitDecision(approvalID, ApprovalDecision{State: ApprovalStateApproved}); err != nil {
			t.Fatalf("SubmitDecision(%q) error = %v", approvalID, err)
		}
		<-result
	}

	reuseCtx, cancelReuse := context.WithCancel(context.Background())
	cancelReuse()
	decision, err := broker.Wait(reuseCtx, ApprovalRequest{ID: "completed-0"})
	if err != nil || decision.State != ApprovalStateTimedOut {
		t.Fatalf("Wait(oldest ID) = %#v, %v; want reusable timed-out request", decision, err)
	}
	if _, err := broker.Wait(context.Background(), ApprovalRequest{ID: fmt.Sprintf("completed-%d", maxClosedApprovals)}); !errors.Is(err, ErrApprovalClosed) {
		t.Fatalf("Wait(newest ID) error = %v, want %v", err, ErrApprovalClosed)
	}
}

func TestApprovalBrokerAcceptedDecisionWinsCancellationRace(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		broker := NewApprovalBroker()
		ctx, cancel := context.WithCancel(context.Background())
		decisionCh := make(chan ApprovalDecision, 1)
		errCh := make(chan error, 1)
		go func() {
			decision, err := broker.Wait(ctx, ApprovalRequest{ID: "approval"})
			if err != nil {
				errCh <- err
				return
			}
			decisionCh <- decision
		}()
		waitForApprovalRegistration(t, broker, "approval")

		start := make(chan struct{})
		submitCh := make(chan error, 1)
		go func() {
			<-start
			submitCh <- broker.SubmitDecision("approval", ApprovalDecision{
				State:  ApprovalStateApprovedWithModification,
				Reason: "reviewed",
				Modification: map[string]any{
					"limit": float64(5),
				},
			})
		}()
		close(start)
		cancel()

		submitErr := <-submitCh
		select {
		case err := <-errCh:
			t.Fatalf("iteration %d: Wait() error = %v", iteration, err)
		case decision := <-decisionCh:
			if submitErr == nil {
				if decision.State != ApprovalStateApprovedWithModification || decision.Reason != "reviewed" || decision.Modification["limit"] != float64(5) {
					t.Fatalf("iteration %d: accepted decision = %#v", iteration, decision)
				}
			} else if !errors.Is(submitErr, ErrApprovalNotFound) || decision.State != ApprovalStateTimedOut {
				t.Fatalf("iteration %d: submit error = %v, decision = %#v", iteration, submitErr, decision)
			}
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: Wait() did not complete", iteration)
		}
	}
}
