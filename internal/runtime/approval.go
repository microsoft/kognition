// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ApprovalState represents the state of a human approval decision.
type ApprovalState string

const (
	ApprovalStatePending                  ApprovalState = "pending"
	ApprovalStateApproved                 ApprovalState = "approved"
	ApprovalStateApprovedWithModification ApprovalState = "approved-with-modification"
	ApprovalStateRejected                 ApprovalState = "rejected"
	ApprovalStateTimedOut                 ApprovalState = "timed-out"
)

var (
	ErrApprovalNotFound = errors.New("approval request not found")
	ErrApprovalClosed   = errors.New("approval request already closed")
)

// ApprovalRequest represents a pending approval request.
type ApprovalRequest struct {
	ID        string
	Namespace string
	AgentName string
	ToolName  string
	Input     map[string]any
	CreatedAt time.Time
}

// ApprovalDecision carries the final decision for an approval request.
type ApprovalDecision struct {
	State        ApprovalState  `json:"state"`
	Reason       string         `json:"reason,omitempty"`
	Modification map[string]any `json:"modification,omitempty"`
}

func (d ApprovalDecision) validateSubmission() error {
	switch d.State {
	case ApprovalStateApproved, ApprovalStateApprovedWithModification, ApprovalStateRejected:
		return nil
	case ApprovalStatePending, ApprovalStateTimedOut, "":
		return fmt.Errorf("state must be approved, approved-with-modification, or rejected")
	default:
		return fmt.Errorf("unsupported state %q", d.State)
	}
}

type approvalWaiter struct {
	decisionCh chan ApprovalDecision
	closed     bool
}

const maxClosedApprovals = 1024

// ApprovalBroker coordinates creation and completion of approval requests.
type ApprovalBroker struct {
	mu          sync.Mutex
	waiters     map[string]*approvalWaiter
	closed      map[string]struct{}
	closedOrder []string
}

func NewApprovalBroker() *ApprovalBroker {
	return &ApprovalBroker{
		waiters: make(map[string]*approvalWaiter),
		closed:  make(map[string]struct{}),
	}
}

// Wait registers an approval request and blocks until a decision or timeout.
func (b *ApprovalBroker) Wait(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error) {
	if req.ID == "" {
		return ApprovalDecision{}, errors.New("approval request id is required")
	}

	b.mu.Lock()
	if _, exists := b.waiters[req.ID]; exists {
		b.mu.Unlock()
		return ApprovalDecision{}, fmt.Errorf("approval request %q already exists", req.ID)
	}
	if _, closed := b.closed[req.ID]; closed {
		b.mu.Unlock()
		return ApprovalDecision{}, fmt.Errorf("%w: %q", ErrApprovalClosed, req.ID)
	}
	waiter := &approvalWaiter{decisionCh: make(chan ApprovalDecision, 1)}
	b.waiters[req.ID] = waiter
	b.mu.Unlock()

	select {
	case decision := <-waiter.decisionCh:
		return decision, nil
	case <-ctx.Done():
		b.mu.Lock()
		if current, ok := b.waiters[req.ID]; ok && current == waiter {
			waiter.closed = true
			delete(b.waiters, req.ID)
			b.mu.Unlock()
			return ApprovalDecision{State: ApprovalStateTimedOut, Reason: "approval request timed out"}, nil
		}
		b.mu.Unlock()

		// SubmitDecision already claimed this waiter and will publish its
		// decision after releasing the broker lock.
		decision := <-waiter.decisionCh
		return decision, nil
	}
}

// SubmitDecision submits a decision for an existing pending approval request.
func (b *ApprovalBroker) SubmitDecision(approvalID string, decision ApprovalDecision) error {
	if err := decision.validateSubmission(); err != nil {
		return err
	}

	b.mu.Lock()
	waiter, ok := b.waiters[approvalID]
	if !ok {
		_, closed := b.closed[approvalID]
		b.mu.Unlock()
		if closed {
			return ErrApprovalClosed
		}
		return ErrApprovalNotFound
	}
	if waiter.closed {
		b.mu.Unlock()
		return ErrApprovalClosed
	}
	waiter.closed = true
	delete(b.waiters, approvalID)
	b.closed[approvalID] = struct{}{}
	b.closedOrder = append(b.closedOrder, approvalID)
	if len(b.closedOrder) > maxClosedApprovals {
		oldest := b.closedOrder[0]
		b.closedOrder = b.closedOrder[1:]
		delete(b.closed, oldest)
	}
	b.mu.Unlock()

	waiter.decisionCh <- decision
	close(waiter.decisionCh)
	return nil
}
