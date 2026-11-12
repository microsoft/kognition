// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

// The Prepare/Claim/Reap/Reconcile/Finalize/Abort transitions implement the
// execution state machine defined by ADR-0014 §Preparation, Claim, and
// Accounting Order. Every mutation runs inside a transaction that (1) takes
// the same per-(workspace,invocation-id) advisory lock as the decision path
// so decision and execution transitions serialize with one another, and
// (2) samples `clock_timestamp()` after the lock so time predicates observe
// the same monotone clock the decision path uses. The CAS `WHERE` clause
// keeps `resource_version` plus the exact predecessor `execution_state` (and
// `claim_fencing_token` where relevant) to guarantee first-writer-wins even
// against a hypothetical concurrent writer that skipped the advisory lock.
//
// Callers may supply an empty preparation token to Abort. That models the
// pre-Prepare aborts (`SessionChanged`, `TargetChanged`, `PolicyChanged`) that
// ADR-0014 step 5 discovers before a token has been generated.

// maxTerminationProofRef caps the reference string stored for an Uncertain
// claim's termination-proof pointer. This is intentionally small because the
// reference is a URI or content-addressed identifier, never a payload.
const maxTerminationProofRef = 512

// maxReconciliationEvidenceRef caps the Uncertain-recovery evidence reference
// for the same reason.
const maxReconciliationEvidenceRef = 512

func (s *Store) PrepareRuntimeApproval(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, preparationToken string, preparationDeadline time.Time) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval prepare requires expected resource version")
	}
	if err := runtimeapproval.ValidatePreparationToken(preparationToken); err != nil {
		return api.RuntimeApproval{}, err
	}
	return s.transitionRuntimeApprovalExecution(ctx, workspace, invocationID, expectedResourceVersion,
		func(approval *api.RuntimeApproval, now time.Time) error {
			if !runtimeapproval.PermitsExecution(approval.Status.DecisionState) {
				return fmt.Errorf("%w: prepare requires Approved or NotRequired, got %q", store.ErrConflict, approval.Status.DecisionState)
			}
			if !approval.Status.DecisionEvidenceDelivered {
				return fmt.Errorf("%w: prepare requires decision evidence delivered", store.ErrConflict)
			}
			if approval.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted {
				return fmt.Errorf("%w: prepare requires NotStarted execution, got %q", store.ErrConflict, approval.Status.ExecutionState)
			}
			if approval.Status.PreparationToken != "" {
				return fmt.Errorf("%w: prepare requires no active preparation token", store.ErrConflict)
			}
			if err := runtimeapproval.ValidatePreparationDeadline(now, preparationDeadline); err != nil {
				return err
			}
			approval.Status.PreparationToken = preparationToken
			deadline := preparationDeadline.UTC()
			approval.Status.PreparationDeadline = &deadline
			return nil
		})
}

func (s *Store) AbortRuntimeApprovalBeforeDispatch(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, preparationToken string, disposition string) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval abort requires expected resource version")
	}
	if err := runtimeapproval.ValidatePreDispatchDisposition(disposition); err != nil {
		return api.RuntimeApproval{}, err
	}
	// ApprovalExpiredBeforeClaim and AbandonedPreparation are produced only by
	// the specific expired-approval-at-claim and reaper transitions.
	if disposition == runtimeapproval.ExecutionDispositionApprovalExpiredBeforeClaim ||
		disposition == runtimeapproval.ExecutionDispositionAbandonedPreparation {
		return api.RuntimeApproval{}, fmt.Errorf("%w: %q is not a caller-initiated abort disposition", runtimeapproval.ErrPreDispatchDispositionInvalid, disposition)
	}
	if preparationToken != "" {
		if err := runtimeapproval.ValidatePreparationToken(preparationToken); err != nil {
			return api.RuntimeApproval{}, err
		}
	}
	return s.transitionRuntimeApprovalExecution(ctx, workspace, invocationID, expectedResourceVersion,
		func(approval *api.RuntimeApproval, now time.Time) error {
			if !runtimeapproval.PermitsExecution(approval.Status.DecisionState) {
				return fmt.Errorf("%w: abort requires Approved or NotRequired, got %q", store.ErrConflict, approval.Status.DecisionState)
			}
			if approval.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted {
				return fmt.Errorf("%w: abort requires NotStarted execution, got %q", store.ErrConflict, approval.Status.ExecutionState)
			}
			if approval.Status.PreparationToken != preparationToken {
				return fmt.Errorf("%w: abort preparation token mismatch", store.ErrConflict)
			}
			approval.Status.ExecutionState = api.RuntimeApprovalExecutionFailed
			approval.Status.ExecutionDisposition = disposition
			approval.Status.PreparationToken = ""
			approval.Status.PreparationDeadline = nil
			return nil
		})
}

func (s *Store) ClaimRuntimeApprovalExecution(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, preparationToken string, claimant api.RuntimeApprovalClaimant) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval claim requires expected resource version")
	}
	if err := runtimeapproval.ValidatePreparationToken(preparationToken); err != nil {
		return api.RuntimeApproval{}, err
	}
	if err := runtimeapproval.ValidateClaimant(claimant); err != nil {
		return api.RuntimeApproval{}, err
	}
	return s.transitionRuntimeApprovalExecution(ctx, workspace, invocationID, expectedResourceVersion,
		func(approval *api.RuntimeApproval, now time.Time) error {
			if !runtimeapproval.PermitsExecution(approval.Status.DecisionState) {
				return fmt.Errorf("%w: claim requires Approved or NotRequired, got %q", store.ErrConflict, approval.Status.DecisionState)
			}
			if !approval.Status.DecisionEvidenceDelivered {
				return fmt.Errorf("%w: claim requires decision evidence delivered", store.ErrConflict)
			}
			if approval.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted {
				return fmt.Errorf("%w: claim requires NotStarted execution, got %q", store.ErrConflict, approval.Status.ExecutionState)
			}
			if approval.Status.PreparationToken != preparationToken {
				return fmt.Errorf("%w: claim preparation token mismatch", store.ErrConflict)
			}
			if approval.Status.DecisionState == api.RuntimeApprovalDecisionApproved && !now.Before(approval.Spec.ExpiresAt) {
				// Approved-but-past-decision-expiry consumes the invocation and blocks any future claim.
				approval.Status.ExecutionState = api.RuntimeApprovalExecutionFailed
				approval.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionApprovalExpiredBeforeClaim
				approval.Status.PreparationToken = ""
				approval.Status.PreparationDeadline = nil
				return nil
			}
			if approval.Status.PreparationDeadline == nil || !now.Before(*approval.Status.PreparationDeadline) {
				return fmt.Errorf("%w: preparation deadline expired", store.ErrConflict)
			}
			claim := claimant
			claim.ClaimedAt = claim.ClaimedAt.UTC()
			claim.LastHeartbeat = claim.LastHeartbeat.UTC()
			if claim.LeaseDeadline != nil {
				lease := claim.LeaseDeadline.UTC()
				claim.LeaseDeadline = &lease
			}
			approval.Status.ExecutionState = api.RuntimeApprovalExecutionClaimed
			approval.Status.Claimant = &claim
			approval.Status.FencingToken = runtimeapproval.MinFencingToken
			approval.Status.PreparationToken = ""
			approval.Status.PreparationDeadline = nil
			return nil
		})
}

func (s *Store) MarkRuntimeApprovalClaimUncertain(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, fencingToken int64, terminationProofRef string) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval uncertain requires expected resource version")
	}
	if fencingToken < runtimeapproval.MinFencingToken {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval uncertain requires positive fencing token, got %d", fencingToken)
	}
	if l := len(terminationProofRef); l < 1 || l > maxTerminationProofRef {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval uncertain termination proof reference length %d out of bounds [1,%d]", l, maxTerminationProofRef)
	}
	return s.transitionRuntimeApprovalExecution(ctx, workspace, invocationID, expectedResourceVersion,
		func(approval *api.RuntimeApproval, now time.Time) error {
			if approval.Status.ExecutionState != api.RuntimeApprovalExecutionClaimed {
				return fmt.Errorf("%w: uncertain requires Claimed execution, got %q", store.ErrConflict, approval.Status.ExecutionState)
			}
			if approval.Status.FencingToken != fencingToken {
				return fmt.Errorf("%w: uncertain fencing token mismatch", store.ErrConflict)
			}
			approval.Status.ExecutionState = api.RuntimeApprovalExecutionUncertain
			approval.Status.TerminationProofRef = terminationProofRef
			return nil
		})
}

func (s *Store) FinalizeRuntimeApprovalExecution(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, fencingToken int64, disposition string) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval finalize requires expected resource version")
	}
	if err := runtimeapproval.ValidateTerminalDisposition(disposition); err != nil {
		return api.RuntimeApproval{}, err
	}
	if fencingToken < runtimeapproval.MinFencingToken {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval finalize requires positive fencing token, got %d", fencingToken)
	}
	return s.transitionRuntimeApprovalExecution(ctx, workspace, invocationID, expectedResourceVersion,
		func(approval *api.RuntimeApproval, now time.Time) error {
			if approval.Status.ExecutionState != api.RuntimeApprovalExecutionClaimed {
				return fmt.Errorf("%w: finalize requires Claimed execution, got %q", store.ErrConflict, approval.Status.ExecutionState)
			}
			if approval.Status.FencingToken != fencingToken {
				return fmt.Errorf("%w: finalize fencing token mismatch", store.ErrConflict)
			}
			if disposition == runtimeapproval.ExecutionDispositionSucceeded {
				approval.Status.ExecutionState = api.RuntimeApprovalExecutionCompleted
			} else {
				approval.Status.ExecutionState = api.RuntimeApprovalExecutionFailed
			}
			approval.Status.ExecutionDisposition = disposition
			return nil
		})
}

func (s *Store) ReconcileRuntimeApprovalExecution(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, disposition string, evidenceRef, issuer, subject string) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval reconcile requires expected resource version")
	}
	if err := runtimeapproval.ValidateReconciliationDisposition(disposition); err != nil {
		return api.RuntimeApproval{}, err
	}
	if l := len(evidenceRef); l < 1 || l > maxReconciliationEvidenceRef {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval reconcile evidence reference length %d out of bounds [1,%d]", l, maxReconciliationEvidenceRef)
	}
	if !runtimeapproval.ValidDecisionActor(issuer, subject) {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval reconcile requires a bounded canonical actor")
	}
	return s.transitionRuntimeApprovalExecution(ctx, workspace, invocationID, expectedResourceVersion,
		func(approval *api.RuntimeApproval, now time.Time) error {
			if approval.Status.ExecutionState != api.RuntimeApprovalExecutionUncertain {
				return fmt.Errorf("%w: reconcile requires Uncertain execution, got %q", store.ErrConflict, approval.Status.ExecutionState)
			}
			if disposition == runtimeapproval.ExecutionDispositionReconciledCompleted {
				approval.Status.ExecutionState = api.RuntimeApprovalExecutionCompleted
			} else {
				approval.Status.ExecutionState = api.RuntimeApprovalExecutionFailed
			}
			approval.Status.ExecutionDisposition = disposition
			approval.Status.ReconciliationEvidenceRef = evidenceRef
			approval.Status.ReconciledByIssuer = issuer
			approval.Status.ReconciledBySubject = subject
			return nil
		})
}

func (s *Store) ReapRuntimeApprovalPreparation(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, preparationToken string) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval reap requires expected resource version")
	}
	if err := runtimeapproval.ValidatePreparationToken(preparationToken); err != nil {
		return api.RuntimeApproval{}, err
	}
	return s.transitionRuntimeApprovalExecution(ctx, workspace, invocationID, expectedResourceVersion,
		func(approval *api.RuntimeApproval, now time.Time) error {
			if approval.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted {
				return fmt.Errorf("%w: reap requires NotStarted execution, got %q", store.ErrConflict, approval.Status.ExecutionState)
			}
			if approval.Status.PreparationToken != preparationToken {
				return fmt.Errorf("%w: reap preparation token mismatch", store.ErrConflict)
			}
			if approval.Status.PreparationDeadline == nil || now.Before(*approval.Status.PreparationDeadline) {
				return fmt.Errorf("%w: preparation deadline has not elapsed", store.ErrConflict)
			}
			approval.Status.ExecutionState = api.RuntimeApprovalExecutionFailed
			approval.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionAbandonedPreparation
			approval.Status.PreparationToken = ""
			approval.Status.PreparationDeadline = nil
			return nil
		})
}

func (s *Store) transitionRuntimeApprovalExecution(
	ctx context.Context,
	workspace, invocationID string,
	expectedResourceVersion int64,
	transition func(*api.RuntimeApproval, time.Time) error,
) (api.RuntimeApproval, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("begin RuntimeApproval execution: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	logicalKey := workspace + "\x1f" + invocationID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, logicalKey); err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("lock RuntimeApproval execution: %w", err)
	}
	approval, found, err := scanRuntimeApproval(ctx, tx, workspace, invocationID, true)
	if err != nil {
		return api.RuntimeApproval{}, err
	}
	if !found {
		return api.RuntimeApproval{}, store.ErrNotFound
	}
	if err := runtimeapproval.ValidateRetainedBindingKey(ctx, s.runtimeApprovalKeys(), approval.Spec.Binding); err != nil {
		return api.RuntimeApproval{}, err
	}
	if approval.ResourceVersion != expectedResourceVersion {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	previousExecutionState := approval.Status.ExecutionState
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("read RuntimeApproval execution time: %w", err)
	}
	if err := transition(&approval, now); err != nil {
		return api.RuntimeApproval{}, err
	}
	approval.ResourceVersion++
	data, err := json.Marshal(approval)
	if err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("marshal RuntimeApproval execution: %w", err)
	}
	preparationDeadline := (*time.Time)(nil)
	if approval.Status.PreparationDeadline != nil {
		utc := approval.Status.PreparationDeadline.UTC()
		preparationDeadline = &utc
	}
	var (
		claimWorkloadUID, claimProcessStartNonce           *string
		claimFencingToken                                  *int64
		claimClaimedAt, claimLastHeartbeat, claimLeaseTime *time.Time
	)
	if approval.Status.Claimant != nil {
		claim := approval.Status.Claimant
		claimWorkloadUID = stringPointer(claim.WorkloadUID)
		claimProcessStartNonce = stringPointer(claim.ProcessStartNonce)
		token := approval.Status.FencingToken
		claimFencingToken = &token
		claimedAt := claim.ClaimedAt.UTC()
		claimClaimedAt = &claimedAt
		heartbeat := claim.LastHeartbeat.UTC()
		claimLastHeartbeat = &heartbeat
		if claim.LeaseDeadline != nil {
			lease := claim.LeaseDeadline.UTC()
			claimLeaseTime = &lease
		}
	}
	command, err := tx.Exec(ctx, `UPDATE runtime_approvals
		SET execution_state = $3,
		    execution_disposition = $4,
		    preparation_token = $5,
		    preparation_deadline = $6,
		    claim_workload_uid = $7,
		    claim_process_start_nonce = $8,
		    claim_fencing_token = $9,
		    claim_claimed_at = $10,
		    claim_last_heartbeat = $11,
		    claim_lease_deadline = $12,
		    termination_proof_ref = $13,
		    reconciliation_evidence_ref = $14,
		    reconciled_by_issuer = $15,
		    reconciled_by_subject = $16,
		    data = $17,
		    resource_version = $18,
		    updated_at = $19
		WHERE workspace = $1 AND invocation_id = $2 AND resource_version = $20 AND execution_state = $21`,
		workspace, invocationID,
		approval.Status.ExecutionState,
		nullableString(approval.Status.ExecutionDisposition),
		nullableString(approval.Status.PreparationToken),
		preparationDeadline,
		claimWorkloadUID, claimProcessStartNonce, claimFencingToken,
		claimClaimedAt, claimLastHeartbeat, claimLeaseTime,
		nullableString(approval.Status.TerminationProofRef),
		nullableString(approval.Status.ReconciliationEvidenceRef),
		nullableString(approval.Status.ReconciledByIssuer),
		nullableString(approval.Status.ReconciledBySubject),
		data, approval.ResourceVersion, now,
		expectedResourceVersion, previousExecutionState)
	if err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("update RuntimeApproval execution: %w", err)
	}
	if command.RowsAffected() != 1 {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("commit RuntimeApproval execution: %w", err)
	}
	return approval, nil
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// silence unused-imports on partial builds.
var _ store.RuntimeApprovalExecutionStore = (*Store)(nil)
