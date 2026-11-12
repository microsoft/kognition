// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

// The Prepare/Claim/Reap/Reconcile/Finalize/Abort transitions implement the
// execution state machine defined by ADR-0014 §Preparation, Claim, and
// Accounting Order on the controller-runtime backing store. Each transition:
//
//   1. Reads the RuntimeApproval object through the client.
//   2. Validates the retained binding-key material.
//   3. Confirms the expected resource version matches the persisted one.
//   4. Applies a domain callback that enforces ADR-0014 preconditions and
//      mutates only Status fields (never Spec, never ResourceVersion).
//   5. Writes the mutated status via client.Status().Update, letting the
//      apiserver perform its own resource-version compare-and-swap.
//   6. On transport failure, re-reads and DeepEquals Status to distinguish
//      "spurious failure after successful write" from "real conflict".
//
// Callers may supply an empty preparation token to AbortRuntimeApprovalBeforeDispatch.
// That models the pre-Prepare aborts (SessionChanged, TargetChanged,
// PolicyChanged) that ADR-0014 step 5 discovers before a token is generated.

// kubernetesMaxTerminationProofRef caps the reference string stored for an
// Uncertain claim's termination-proof pointer. Intentionally small because the
// reference is a URI or content-addressed identifier, never a payload.
const kubernetesMaxTerminationProofRef = 512

// kubernetesMaxReconciliationEvidenceRef caps the Uncertain-recovery evidence
// reference for the same reason.
const kubernetesMaxReconciliationEvidenceRef = 512

func (s *StoreAdapter) PrepareRuntimeApproval(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, preparationToken string, preparationDeadline time.Time) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval prepare requires expected resource version")
	}
	if err := runtimeapproval.ValidatePreparationToken(preparationToken); err != nil {
		return api.RuntimeApproval{}, err
	}
	return s.transitionKubernetesRuntimeApprovalExecution(ctx, workspace, invocationID, expectedResourceVersion,
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

func (s *StoreAdapter) AbortRuntimeApprovalBeforeDispatch(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, preparationToken string, disposition string) (api.RuntimeApproval, error) {
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
	return s.transitionKubernetesRuntimeApprovalExecution(ctx, workspace, invocationID, expectedResourceVersion,
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

func (s *StoreAdapter) ClaimRuntimeApprovalExecution(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, preparationToken string, claimant api.RuntimeApprovalClaimant) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval claim requires expected resource version")
	}
	if err := runtimeapproval.ValidatePreparationToken(preparationToken); err != nil {
		return api.RuntimeApproval{}, err
	}
	if err := runtimeapproval.ValidateClaimant(claimant); err != nil {
		return api.RuntimeApproval{}, err
	}
	return s.transitionKubernetesRuntimeApprovalExecution(ctx, workspace, invocationID, expectedResourceVersion,
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

func (s *StoreAdapter) MarkRuntimeApprovalClaimUncertain(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, fencingToken int64, terminationProofRef string) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval uncertain requires expected resource version")
	}
	if fencingToken < runtimeapproval.MinFencingToken {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval uncertain requires positive fencing token, got %d", fencingToken)
	}
	if l := len(terminationProofRef); l < 1 || l > kubernetesMaxTerminationProofRef {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval uncertain termination proof reference length %d out of bounds [1,%d]", l, kubernetesMaxTerminationProofRef)
	}
	return s.transitionKubernetesRuntimeApprovalExecution(ctx, workspace, invocationID, expectedResourceVersion,
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

func (s *StoreAdapter) FinalizeRuntimeApprovalExecution(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, fencingToken int64, disposition string) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval finalize requires expected resource version")
	}
	if err := runtimeapproval.ValidateTerminalDisposition(disposition); err != nil {
		return api.RuntimeApproval{}, err
	}
	if fencingToken < runtimeapproval.MinFencingToken {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval finalize requires positive fencing token, got %d", fencingToken)
	}
	return s.transitionKubernetesRuntimeApprovalExecution(ctx, workspace, invocationID, expectedResourceVersion,
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

func (s *StoreAdapter) ReconcileRuntimeApprovalExecution(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, disposition string, evidenceRef, issuer, subject string) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval reconcile requires expected resource version")
	}
	if err := runtimeapproval.ValidateReconciliationDisposition(disposition); err != nil {
		return api.RuntimeApproval{}, err
	}
	if l := len(evidenceRef); l < 1 || l > kubernetesMaxReconciliationEvidenceRef {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval reconcile evidence reference length %d out of bounds [1,%d]", l, kubernetesMaxReconciliationEvidenceRef)
	}
	if !runtimeapproval.ValidDecisionActor(issuer, subject) {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval reconcile requires a bounded canonical actor")
	}
	return s.transitionKubernetesRuntimeApprovalExecution(ctx, workspace, invocationID, expectedResourceVersion,
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

func (s *StoreAdapter) ReapRuntimeApprovalPreparation(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, preparationToken string) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval reap requires expected resource version")
	}
	if err := runtimeapproval.ValidatePreparationToken(preparationToken); err != nil {
		return api.RuntimeApproval{}, err
	}
	return s.transitionKubernetesRuntimeApprovalExecution(ctx, workspace, invocationID, expectedResourceVersion,
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

func (s *StoreAdapter) transitionKubernetesRuntimeApprovalExecution(
	ctx context.Context,
	workspace, invocationID string,
	expectedResourceVersion int64,
	transition func(*api.RuntimeApproval, time.Time) error,
) (api.RuntimeApproval, error) {
	if err := s.RuntimeApprovalClockReady(); err != nil {
		return api.RuntimeApproval{}, err
	}
	key := client.ObjectKey{Namespace: workspace, Name: runtimeapproval.CanonicalName(workspace, invocationID)}
	object := &k8s.RuntimeApproval{}
	if err := s.client.Get(ctx, key, object); err != nil {
		if apierrors.IsNotFound(err) {
			return api.RuntimeApproval{}, store.ErrNotFound
		}
		return api.RuntimeApproval{}, fmt.Errorf("k8s get RuntimeApproval execution: %w", err)
	}
	approval := runtimeApprovalFromKubernetes(object)
	if err := runtimeapproval.ValidateRetainedBindingKey(ctx, s.runtimeApprovalKeys(), approval.Spec.Binding); err != nil {
		return api.RuntimeApproval{}, err
	}
	if approval.Spec.Workspace != workspace || approval.Spec.InvocationID != invocationID {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	if approval.ResourceVersion != expectedResourceVersion {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	now := s.runtimeApprovalNow()
	if err := transition(&approval, now); err != nil {
		return api.RuntimeApproval{}, err
	}
	updated := convert.RuntimeApprovalToK8s(&approval)
	updated.ResourceVersion = object.ResourceVersion
	base := object.DeepCopy()
	if err := s.client.Status().Patch(ctx, updated, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		current := &k8s.RuntimeApproval{}
		if readErr := s.client.Get(ctx, key, current); readErr != nil {
			return api.RuntimeApproval{}, fmt.Errorf("k8s update RuntimeApproval execution: %w (reread failed: %w)", err, readErr)
		}
		winner := runtimeApprovalFromKubernetes(current)
		if err := runtimeapproval.ValidateRetainedBindingKey(ctx, s.runtimeApprovalKeys(), winner.Spec.Binding); err != nil {
			return api.RuntimeApproval{}, err
		}
		if winner.Spec.Workspace != workspace || winner.Spec.InvocationID != invocationID {
			return api.RuntimeApproval{}, store.ErrConflict
		}
		if reflect.DeepEqual(winner.Status, approval.Status) {
			return winner, nil
		}
		if apierrors.IsConflict(err) {
			return api.RuntimeApproval{}, store.ErrConflict
		}
		return api.RuntimeApproval{}, fmt.Errorf("k8s update RuntimeApproval execution remained ambiguous: %w", err)
	}
	return runtimeApprovalFromKubernetes(updated), nil
}

var _ store.RuntimeApprovalExecutionStore = (*StoreAdapter)(nil)
