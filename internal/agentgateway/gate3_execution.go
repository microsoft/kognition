// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentgateway

import (
	"context"
	"fmt"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

func (s *Server) prepareAndClaimApproval(ctx context.Context, approval *v1alpha1.RuntimeApproval) (*v1alpha1.RuntimeApproval, error) {
	if approval == nil || s.runtimeApprovals == nil {
		return nil, nil
	}
	prepared := *approval
	token := prepared.Status.PreparationToken
	if token == "" {
		var err error
		token, err = s.newPrepToken()
		if err != nil {
			return nil, fmt.Errorf("generate preparation token: %w", err)
		}
		window := s.preparationWindow
		if window <= 0 || window > runtimeapproval.MaxPreparationDeadline {
			window = runtimeapproval.MaxPreparationDeadline
		}
		prepared, err = s.runtimeApprovals.PrepareRuntimeApproval(ctx, approval.Namespace, approval.Spec.InvocationID, approval.ResourceVersion, token, s.now().Add(window))
		if err != nil {
			return nil, fmt.Errorf("prepare runtime approval: %w", err)
		}
	}
	claimant := s.claimant
	claimant.ClaimedAt = s.now()
	claimant.LastHeartbeat = claimant.ClaimedAt
	claimed, err := s.runtimeApprovals.ClaimRuntimeApprovalExecution(ctx, prepared.Namespace, prepared.Spec.InvocationID, prepared.ResourceVersion, token, claimant)
	if err != nil {
		return nil, fmt.Errorf("claim runtime approval execution: %w", err)
	}
	if claimed.Status.ExecutionState != v1alpha1.RuntimeApprovalExecutionClaimed ||
		claimed.Status.FencingToken < runtimeapproval.MinFencingToken ||
		claimed.Status.Claimant == nil ||
		claimed.Status.Claimant.WorkloadUID != claimant.WorkloadUID ||
		claimed.Status.Claimant.ContainerID != claimant.ContainerID ||
		claimed.Status.Claimant.ProcessStartNonce != claimant.ProcessStartNonce {
		return nil, fmt.Errorf("claim runtime approval execution: store returned execution state %q without ownership for this gateway incarnation", claimed.Status.ExecutionState)
	}
	return &claimed, nil
}

func (s *Server) finalizeApproval(ctx context.Context, claimed *v1alpha1.RuntimeApproval, disposition string) error {
	if claimed == nil || s.runtimeApprovals == nil {
		return nil
	}
	_, err := s.runtimeApprovals.FinalizeRuntimeApprovalExecution(ctx, claimed.Namespace, claimed.Spec.InvocationID, claimed.ResourceVersion, claimed.Status.FencingToken, disposition)
	if err != nil {
		return fmt.Errorf("finalize runtime approval execution: %w", err)
	}
	return nil
}
