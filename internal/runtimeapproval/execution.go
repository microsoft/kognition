// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// Closed pre-dispatch and reconciliation dispositions defined by ADR-0014.
// Every terminal execution state has exactly one of these dispositions attached.
const (
	// Pre-dispatch (NotStarted → Failed) closed values.
	ExecutionDispositionQuotaDenied                = "QuotaDenied"
	ExecutionDispositionReceiptConflict            = "ReceiptConflict"
	ExecutionDispositionPolicyChanged              = "PolicyChanged"
	ExecutionDispositionTargetChanged              = "TargetChanged"
	ExecutionDispositionSessionChanged             = "SessionChanged"
	ExecutionDispositionPreparationRejected        = "PreparationRejected"
	ExecutionDispositionApprovalExpiredBeforeClaim = "ApprovalExpiredBeforeClaim"
	ExecutionDispositionAbandonedPreparation       = "AbandonedPreparation"

	// Reconciliation of an Uncertain claim.
	ExecutionDispositionReconciledCompleted   = "ReconciledCompleted"
	ExecutionDispositionReconciledNotExecuted = "ReconciledNotExecuted"

	// Post-claim terminal dispositions (Claimed → Completed/Failed).
	ExecutionDispositionSucceeded = "Succeeded"
	ExecutionDispositionFailed    = "Failed"
)

// preparationTokenBytes is the crypto random material length; 32 bytes → 43
// chars after base64url no-pad, which comfortably meets the 32..128 char
// persistence constraint enforced by the migration.
const preparationTokenBytes = 32

// MaxPreparationDeadline caps the preparation window enforced by
// ADR-0014 §Preparation, Claim, and Accounting Order. Callers must reject
// deadlines beyond this bound.
const MaxPreparationDeadline = 5 * time.Minute

// MinFencingToken enforces monotonic strictly-positive fencing tokens.
const MinFencingToken = int64(1)

// ErrPreDispatchDispositionInvalid is returned when a caller supplies a
// disposition that is not part of the closed pre-dispatch set.
var ErrPreDispatchDispositionInvalid = errors.New("invalid pre-dispatch disposition")

// ErrReconciliationDispositionInvalid is returned when a caller supplies a
// disposition that is not part of the closed reconciliation set.
var ErrReconciliationDispositionInvalid = errors.New("invalid reconciliation disposition")

// ErrTerminalDispositionInvalid is returned for post-claim terminal dispositions
// that fall outside the {Succeeded, Failed} closed set.
var ErrTerminalDispositionInvalid = errors.New("invalid terminal disposition")

// ToolPreparationReapStore owns the terminal transition that fences stale
// preparation cleanup from a concurrent execution claim.
type ToolPreparationReapStore interface {
	ReapRuntimeApprovalPreparation(context.Context, string, string, int64, string) (api.RuntimeApproval, error)
}

// ToolPreparationCleanupStore owns the exact-token accounting and receipt
// records associated with a Tool preparation.
type ToolPreparationCleanupStore interface {
	ReleaseToolCallReservation(context.Context, string, string, string) (bool, error)
	ReleasePreparedToolInvocation(context.Context, string, string, string) (bool, error)
}

// ExpiredToolPreparationStore discovers and owns the durable records needed
// to reconcile stale standalone Tool preparations.
type ExpiredToolPreparationStore interface {
	ToolPreparationReapStore
	ToolPreparationCleanupStore
	GetRuntimeApprovalByInvocation(context.Context, string, string) (api.RuntimeApproval, error)
	ListExpiredToolCallReservations(context.Context, int) ([]store.ToolCallReservation, error)
}

// MemoryPreparationApprovalStore owns the RuntimeApproval side of stale
// Memory preparation recovery.
type MemoryPreparationApprovalStore interface {
	ToolPreparationReapStore
	GetRuntimeApprovalByInvocation(context.Context, string, string) (api.RuntimeApproval, error)
}

// ExpiredMemoryPreparationReceiptStore discovers and removes exact-token
// Memory receipt preparations after RuntimeApproval is terminal.
type ExpiredMemoryPreparationReceiptStore interface {
	ListExpiredMemoryMutations(context.Context, int) ([]store.MemoryMutationReceipt, error)
	ReleasePreparedMemoryMutation(context.Context, string, string, string) (bool, error)
}

// ReapToolPreparation first wins the RuntimeApproval terminal transition,
// then idempotently removes only accounting and receipt records carrying the
// same preparation token. Reservation cleanup precedes receipt cleanup so an
// ambiguous quota result retains the receipt for reconciliation.
func ReapToolPreparation(ctx context.Context, reaper ToolPreparationReapStore, cleanup ToolPreparationCleanupStore, workspace, invocationID string, expectedResourceVersion int64, preparationToken string) (api.RuntimeApproval, error) {
	reaped, err := reaper.ReapRuntimeApprovalPreparation(ctx, workspace, invocationID, expectedResourceVersion, preparationToken)
	if err != nil {
		return api.RuntimeApproval{}, err
	}
	if err := CleanupReapedToolPreparation(ctx, cleanup, workspace, invocationID, preparationToken); err != nil {
		return reaped, err
	}
	return reaped, nil
}

// CleanupReapedToolPreparation idempotently removes the exact-token accounting
// and receipt records after the caller has proved the RuntimeApproval reached
// AbandonedPreparation. Reservation cleanup precedes receipt cleanup so an
// ambiguous quota result retains the receipt for reconciliation.
func CleanupReapedToolPreparation(ctx context.Context, cleanup ToolPreparationCleanupStore, workspace, invocationID, preparationToken string) error {
	if _, err := cleanup.ReleaseToolCallReservation(ctx, workspace, invocationID, preparationToken); err != nil {
		return fmt.Errorf("release reaped Tool call reservation: %w", err)
	}
	if _, err := cleanup.ReleasePreparedToolInvocation(ctx, workspace, invocationID, preparationToken); err != nil {
		return fmt.Errorf("release reaped Tool invocation receipt: %w", err)
	}
	return nil
}

// ReapExpiredToolPreparations reconciles a bounded database-clock snapshot of
// stale standalone Tool preparations. A claim or token mismatch always keeps
// accounting charged; an already-abandoned preparation resumes cleanup.
func ReapExpiredToolPreparations(ctx context.Context, lifecycle ExpiredToolPreparationStore, limit int) (int, error) {
	reservations, err := lifecycle.ListExpiredToolCallReservations(ctx, limit)
	if err != nil {
		return 0, err
	}
	reaped := 0
	for _, reservation := range reservations {
		approval, err := lifecycle.GetRuntimeApprovalByInvocation(ctx, reservation.Namespace, reservation.InvocationID)
		if err != nil {
			return reaped, err
		}
		if toolPreparationWasReaped(approval) {
			if err := CleanupReapedToolPreparation(ctx, lifecycle, reservation.Namespace, reservation.InvocationID, reservation.PreparationToken); err != nil {
				return reaped, err
			}
			reaped++
			continue
		}
		if approval.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted || approval.Status.PreparationToken != reservation.PreparationToken {
			continue
		}
		if _, err := ReapToolPreparation(ctx, lifecycle, lifecycle, reservation.Namespace, reservation.InvocationID, approval.ResourceVersion, reservation.PreparationToken); err == nil {
			reaped++
			continue
		} else if !errors.Is(err, store.ErrConflict) {
			return reaped, err
		}

		winner, err := lifecycle.GetRuntimeApprovalByInvocation(ctx, reservation.Namespace, reservation.InvocationID)
		if err != nil {
			return reaped, err
		}
		if toolPreparationWasReaped(winner) {
			if err := CleanupReapedToolPreparation(ctx, lifecycle, reservation.Namespace, reservation.InvocationID, reservation.PreparationToken); err != nil {
				return reaped, err
			}
			reaped++
			continue
		}
		if winner.Status.ExecutionState == api.RuntimeApprovalExecutionNotStarted && winner.Status.PreparationToken == reservation.PreparationToken {
			return reaped, store.ErrConflict
		}
	}
	return reaped, nil
}

func toolPreparationWasReaped(approval api.RuntimeApproval) bool {
	return approval.Status.ExecutionState == api.RuntimeApprovalExecutionFailed &&
		approval.Status.ExecutionDisposition == ExecutionDispositionAbandonedPreparation
}

// ReapExpiredMemoryPreparations reconciles a bounded database-clock snapshot
// across the PostgreSQL receipt and RuntimeApproval persistence planes.
func ReapExpiredMemoryPreparations(ctx context.Context, approvals MemoryPreparationApprovalStore, receipts ExpiredMemoryPreparationReceiptStore, limit int) (int, error) {
	candidates, err := receipts.ListExpiredMemoryMutations(ctx, limit)
	if err != nil {
		return 0, err
	}
	reaped := 0
	for _, receipt := range candidates {
		approval, err := approvals.GetRuntimeApprovalByInvocation(ctx, receipt.Namespace, receipt.InvocationID)
		if err != nil {
			return reaped, err
		}
		if toolPreparationWasReaped(approval) {
			if _, err := receipts.ReleasePreparedMemoryMutation(ctx, receipt.Namespace, receipt.InvocationID, receipt.PreparationToken); err != nil {
				return reaped, err
			}
			reaped++
			continue
		}
		if approval.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted || approval.Status.PreparationToken != receipt.PreparationToken {
			continue
		}
		if _, err := approvals.ReapRuntimeApprovalPreparation(ctx, receipt.Namespace, receipt.InvocationID, approval.ResourceVersion, receipt.PreparationToken); err == nil {
			if _, err := receipts.ReleasePreparedMemoryMutation(ctx, receipt.Namespace, receipt.InvocationID, receipt.PreparationToken); err != nil {
				return reaped, err
			}
			reaped++
			continue
		} else if !errors.Is(err, store.ErrConflict) {
			return reaped, err
		}

		winner, err := approvals.GetRuntimeApprovalByInvocation(ctx, receipt.Namespace, receipt.InvocationID)
		if err != nil {
			return reaped, err
		}
		if toolPreparationWasReaped(winner) {
			if _, err := receipts.ReleasePreparedMemoryMutation(ctx, receipt.Namespace, receipt.InvocationID, receipt.PreparationToken); err != nil {
				return reaped, err
			}
			reaped++
			continue
		}
		if winner.Status.ExecutionState == api.RuntimeApprovalExecutionNotStarted && winner.Status.PreparationToken == receipt.PreparationToken {
			return reaped, store.ErrConflict
		}
	}
	return reaped, nil
}

// IsTerminalDecision reports whether the decision-state machine has reached a
// terminal state. NotRequired is terminal by ADR-0014 §Decision Semantics.
func IsTerminalDecision(state string) bool {
	switch state {
	case api.RuntimeApprovalDecisionApproved,
		api.RuntimeApprovalDecisionRejected,
		api.RuntimeApprovalDecisionExpired,
		api.RuntimeApprovalDecisionNotRequired:
		return true
	default:
		return false
	}
}

// IsTerminalExecution reports whether the execution-state machine has reached a
// terminal state.
func IsTerminalExecution(state string) bool {
	switch state {
	case api.RuntimeApprovalExecutionCompleted, api.RuntimeApprovalExecutionFailed:
		return true
	default:
		return false
	}
}

// PermitsExecution reports whether the given decision state permits execution
// at all. Approved and NotRequired are the only decisions that permit
// downstream Prepare/Claim transitions.
func PermitsExecution(decision string) bool {
	return decision == api.RuntimeApprovalDecisionApproved ||
		decision == api.RuntimeApprovalDecisionNotRequired
}

// GeneratePreparationToken returns a fresh 43-character base64url token
// suitable for the preparation_token column and the RuntimeApproval status
// preparationToken field.
func GeneratePreparationToken() (string, error) {
	buf := make([]byte, preparationTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate preparation token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ValidatePreparationToken enforces the persistence bound (32..128 chars) on a
// caller-supplied token. Empty tokens are rejected.
func ValidatePreparationToken(token string) error {
	if l := len(token); l < 32 || l > 128 {
		return fmt.Errorf("preparation token length %d out of bounds [32,128]", l)
	}
	return nil
}

// ValidatePreparationDeadline enforces both the positivity bound and the
// ADR-0014 cap on preparation windows.
func ValidatePreparationDeadline(now, deadline time.Time) error {
	if deadline.IsZero() {
		return fmt.Errorf("preparation deadline must be set")
	}
	if !deadline.After(now) {
		return fmt.Errorf("preparation deadline must be after now")
	}
	if deadline.Sub(now) > MaxPreparationDeadline {
		return fmt.Errorf("preparation deadline exceeds %s cap", MaxPreparationDeadline)
	}
	return nil
}

// ValidatePreDispatchDisposition rejects dispositions that are not part of the
// closed pre-dispatch set.
func ValidatePreDispatchDisposition(disposition string) error {
	switch disposition {
	case ExecutionDispositionQuotaDenied,
		ExecutionDispositionReceiptConflict,
		ExecutionDispositionPolicyChanged,
		ExecutionDispositionTargetChanged,
		ExecutionDispositionSessionChanged,
		ExecutionDispositionPreparationRejected,
		ExecutionDispositionApprovalExpiredBeforeClaim,
		ExecutionDispositionAbandonedPreparation:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrPreDispatchDispositionInvalid, disposition)
	}
}

// ValidateReconciliationDisposition rejects dispositions that are not part of
// the closed Uncertain-reconciliation set.
func ValidateReconciliationDisposition(disposition string) error {
	switch disposition {
	case ExecutionDispositionReconciledCompleted,
		ExecutionDispositionReconciledNotExecuted:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrReconciliationDispositionInvalid, disposition)
	}
}

// ValidateTerminalDisposition rejects dispositions that are not part of the
// closed post-claim terminal set.
func ValidateTerminalDisposition(disposition string) error {
	switch disposition {
	case ExecutionDispositionSucceeded, ExecutionDispositionFailed:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrTerminalDispositionInvalid, disposition)
	}
}

// ValidateClaimant enforces bounded identity fields for the durable claim
// receipt. It intentionally rejects zero times and empty identity fields to
// prevent silent partial writes.
func ValidateClaimant(claimant api.RuntimeApprovalClaimant) error {
	if l := len(claimant.WorkloadUID); l < 1 || l > 253 {
		return fmt.Errorf("claimant workloadUID length %d out of bounds [1,253]", l)
	}
	if l := len(claimant.ProcessStartNonce); l < 16 || l > 128 {
		return fmt.Errorf("claimant processStartNonce length %d out of bounds [16,128]", l)
	}
	if len(claimant.ContainerID) > 253 {
		return fmt.Errorf("claimant containerID length %d exceeds 253", len(claimant.ContainerID))
	}
	if claimant.ClaimedAt.IsZero() {
		return fmt.Errorf("claimant claimedAt must be set")
	}
	if claimant.LastHeartbeat.IsZero() {
		return fmt.Errorf("claimant lastHeartbeat must be set")
	}
	if claimant.LeaseDeadline != nil && !claimant.LeaseDeadline.After(claimant.ClaimedAt) {
		return fmt.Errorf("claimant leaseDeadline must be after claimedAt")
	}
	return nil
}
