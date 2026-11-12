// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package policygateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

func (s *Server) replayMemoryMutation(ctx context.Context, approval *v1alpha1.RuntimeApproval, operation string) (*memoryMutationOutcome, error) {
	reader, ok := s.memoryReceipts.(store.MemoryMutationReceiptReader)
	if approval == nil || !ok {
		return nil, nil
	}
	receipt, found, err := reader.GetMemoryMutation(ctx, approval.ObjectMeta.Namespace, approval.Spec.InvocationID)
	if err != nil || !found {
		return nil, err
	}
	if receipt.Namespace != approval.ObjectMeta.Namespace || receipt.InvocationID != approval.Spec.InvocationID ||
		receipt.Binding == "" || receipt.Binding != approval.Spec.Binding.Tag || receipt.Operation != operation {
		return nil, errors.New("Memory receipt does not match the approved invocation")
	}
	if receipt.Status == store.MemoryMutationPrepared {
		return nil, nil
	}
	if (receipt.Status != store.MemoryMutationSucceeded && receipt.Status != store.MemoryMutationFailed) || receipt.CompletedAt == nil {
		return nil, errors.New("Memory receipt has no valid terminal outcome")
	}
	var result struct {
		Version     int             `json:"version"`
		StatusCode  int             `json:"statusCode"`
		Body        json.RawMessage `json:"body"`
		Payload     *[]byte         `json:"payload"`
		ContentType string          `json:"contentType"`
	}
	if err := json.Unmarshal(receipt.Result, &result); err != nil || result.StatusCode < http.StatusOK || result.StatusCode > 599 {
		return nil, errors.New("Memory receipt contains an invalid response")
	}
	outcome := &memoryMutationOutcome{statusCode: result.StatusCode, payload: result.Body, contentType: "application/json", errorMessage: receipt.ErrorMessage, terminal: true}
	switch result.Version {
	case 0:
		if len(result.Body) == 0 {
			return nil, errors.New("Memory receipt contains no response body")
		}
		var text string
		if result.Body[0] == '"' {
			if err := json.Unmarshal(result.Body, &text); err != nil {
				return nil, errors.New("Memory receipt contains an invalid response body")
			}
			outcome.payload = []byte(text)
			outcome.contentType = "text/plain; charset=utf-8"
		}
	case 2:
		if result.Payload == nil || strings.ContainsAny(result.ContentType, "\r\n") {
			return nil, errors.New("Memory receipt contains an invalid stored response")
		}
		outcome.payload = *result.Payload
		outcome.contentType = result.ContentType
	default:
		return nil, errors.New("Memory receipt response version is unsupported")
	}
	if len(outcome.payload) > maxRequestBytes {
		return nil, errors.New("Memory receipt response is too large")
	}
	if (receipt.Status == store.MemoryMutationSucceeded) != outcome.succeeded() {
		return nil, errors.New("Memory receipt response contradicts its terminal status")
	}
	expectedState := v1alpha1.RuntimeApprovalExecutionFailed
	disposition := runtimeapproval.ExecutionDispositionFailed
	if outcome.succeeded() {
		expectedState = v1alpha1.RuntimeApprovalExecutionCompleted
		disposition = runtimeapproval.ExecutionDispositionSucceeded
	}
	if approval.Status.ExecutionState == v1alpha1.RuntimeApprovalExecutionClaimed {
		if s.runtimeApprovals == nil || approval.Status.FencingToken <= 0 {
			return nil, errors.New("Memory receipt approval cannot be finalized")
		}
		if err := s.finalizeApproval(ctx, approval, disposition); err != nil {
			return nil, err
		}
	} else if approval.Status.ExecutionState != expectedState {
		return nil, errors.New("Memory receipt contradicts approval execution state")
	}
	return outcome, nil
}

func (s *Server) prepareAndClaimApproval(ctx context.Context, approval *v1alpha1.RuntimeApproval, operation string) (*v1alpha1.RuntimeApproval, *store.MemoryMutationReceipt, error) {
	if approval == nil {
		return nil, nil, nil
	}
	if s.runtimeApprovals == nil {
		return nil, nil, errors.New("prepare runtime approval: execution store is unavailable")
	}
	if s.memoryReceipts == nil {
		return nil, nil, errors.New("prepare runtime approval: durable Memory mutation receipt store is unavailable")
	}
	token := approval.Status.PreparationToken
	deadline := approval.Status.PreparationDeadline
	resumed := token != "" || deadline != nil
	if resumed && (token == "" || deadline == nil) {
		return nil, nil, errors.New("prepare runtime approval: incomplete durable preparation identity")
	}
	if !resumed {
		var err error
		token, err = s.newPrepToken()
		if err != nil {
			return nil, nil, fmt.Errorf("generate preparation token: %w", err)
		}
		window := s.preparationWindow
		if window <= 0 || window > runtimeapproval.MaxPreparationDeadline {
			window = runtimeapproval.MaxPreparationDeadline
		}
		generatedDeadline := s.now().Add(window)
		deadline = &generatedDeadline
	}
	receipt := store.MemoryMutationReceipt{
		Namespace:           approval.ObjectMeta.Namespace,
		InvocationID:        approval.Spec.InvocationID,
		Binding:             approval.Spec.Binding.Tag,
		Operation:           operation,
		Status:              store.MemoryMutationPrepared,
		PreparationToken:    token,
		PreparationDeadline: deadline,
		CreatedAt:           s.now().UTC(),
	}
	_, persisted, err := s.memoryReceipts.PrepareMemoryMutation(ctx, receipt)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare Memory mutation receipt: %w", err)
	}
	if persisted.Namespace != receipt.Namespace || persisted.InvocationID != receipt.InvocationID ||
		persisted.Binding != receipt.Binding || persisted.Operation != receipt.Operation ||
		persisted.Status != store.MemoryMutationPrepared || persisted.PreparationToken != token ||
		persisted.PreparationDeadline == nil || !persisted.PreparationDeadline.Equal(*deadline) {
		return nil, nil, errors.New("prepare Memory mutation receipt: persisted preparation identity does not match")
	}
	receipt = persisted
	prepared := *approval
	if !resumed {
		prepared, err = s.runtimeApprovals.PrepareRuntimeApproval(ctx, approval.ObjectMeta.Namespace, approval.Spec.InvocationID, approval.ObjectMeta.ResourceVersion, token, *deadline)
		if err != nil {
			return nil, nil, fmt.Errorf("prepare runtime approval: %w", err)
		}
	}
	claimant := s.claimant
	claimant.ClaimedAt = s.now()
	claimant.LastHeartbeat = claimant.ClaimedAt
	claimed, err := s.runtimeApprovals.ClaimRuntimeApprovalExecution(ctx, prepared.ObjectMeta.Namespace, prepared.Spec.InvocationID, prepared.ObjectMeta.ResourceVersion, token, claimant)
	if err != nil {
		return nil, nil, fmt.Errorf("claim runtime approval execution: %w", err)
	}
	if claimed.ObjectMeta.Namespace != prepared.ObjectMeta.Namespace || claimed.Spec.InvocationID != prepared.Spec.InvocationID ||
		claimed.Status.ExecutionState != v1alpha1.RuntimeApprovalExecutionClaimed || claimed.Status.FencingToken <= 0 ||
		claimed.Status.Claimant == nil || claimed.Status.Claimant.WorkloadUID != claimant.WorkloadUID ||
		claimed.Status.Claimant.ContainerID != claimant.ContainerID || claimed.Status.Claimant.ProcessStartNonce != claimant.ProcessStartNonce {
		return nil, nil, errors.New("claim runtime approval execution: store did not return the current claimed owner")
	}
	return &claimed, &receipt, nil
}

func (s *Server) completeMemoryMutation(ctx context.Context, receipt *store.MemoryMutationReceipt, outcome memoryMutationOutcome) error {
	if receipt == nil || s.memoryReceipts == nil {
		return nil
	}
	completed := *receipt
	completed.Status = store.MemoryMutationFailed
	completed.ErrorMessage = outcome.errorMessage
	if outcome.succeeded() {
		completed.Status = store.MemoryMutationSucceeded
		completed.ErrorMessage = ""
	}
	payload := append([]byte{}, outcome.payload...)
	result, err := json.Marshal(struct {
		Version     int    `json:"version"`
		StatusCode  int    `json:"statusCode"`
		Payload     []byte `json:"payload"`
		ContentType string `json:"contentType"`
	}{Version: 2, StatusCode: outcome.statusCode, Payload: payload, ContentType: outcome.contentType})
	if err != nil {
		return fmt.Errorf("marshal Memory mutation result: %w", err)
	}
	completed.Result = result
	now := s.now().UTC()
	completed.CompletedAt = &now
	completed.PreparationToken = ""
	completed.PreparationDeadline = nil
	if err := s.memoryReceipts.CompleteMemoryMutation(ctx, completed); err != nil {
		return fmt.Errorf("complete Memory mutation receipt: %w", err)
	}
	return nil
}

func (s *Server) finalizeApproval(ctx context.Context, claimed *v1alpha1.RuntimeApproval, disposition string) error {
	if claimed == nil || s.runtimeApprovals == nil {
		return nil
	}
	_, err := s.runtimeApprovals.FinalizeRuntimeApprovalExecution(ctx, claimed.ObjectMeta.Namespace, claimed.Spec.InvocationID, claimed.ObjectMeta.ResourceVersion, claimed.Status.FencingToken, disposition)
	if err != nil {
		return fmt.Errorf("finalize runtime approval execution: %w", err)
	}
	return nil
}

func approvalInvocationID(approval *v1alpha1.RuntimeApproval) string {
	if approval == nil {
		return ""
	}
	return approval.Spec.InvocationID
}
