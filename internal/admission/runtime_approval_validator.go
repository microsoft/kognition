// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

// RuntimeApprovalValidator protects the dedicated-operation lifecycle at the
// Kubernetes admission boundary. RBAC separately restricts status writers.
type RuntimeApprovalValidator struct {
	controllerUsername        string
	decisionAuthorityUsername string
	lifecycleWriters          map[string]struct{}
	executionWriters          map[string]string
	claimantReader            store.RuntimeApprovalClaimantReader
	terminationReader         store.RuntimeApprovalTerminationReader
}

func NewRuntimeApprovalValidator(controllerUsernames ...string) *RuntimeApprovalValidator {
	usernames := make(map[string]struct{}, len(controllerUsernames))
	for _, username := range controllerUsernames {
		if username != "" {
			usernames[username] = struct{}{}
		}
	}
	controllerUsername := ""
	if len(controllerUsernames) > 0 {
		controllerUsername = controllerUsernames[0]
	}
	return &RuntimeApprovalValidator{controllerUsername: controllerUsername, lifecycleWriters: usernames}
}

func (v *RuntimeApprovalValidator) WithDecisionAuthority(username string) *RuntimeApprovalValidator {
	v.decisionAuthorityUsername = username
	return v
}

func (v *RuntimeApprovalValidator) WithExecutionWriters(writers map[string]string) *RuntimeApprovalValidator {
	v.executionWriters = make(map[string]string, len(writers))
	for username, plane := range writers {
		if username != "" && (plane == api.RuntimeApprovalPlaneTool || plane == api.RuntimeApprovalPlaneMemory || plane == api.RuntimeApprovalPlaneCommunication || plane == api.RuntimeApprovalPlaneWorkflow) {
			v.executionWriters[username] = plane
		}
	}
	return v
}

func (v *RuntimeApprovalValidator) WithClaimantReader(reader store.RuntimeApprovalClaimantReader) *RuntimeApprovalValidator {
	v.claimantReader = reader
	return v
}

func (v *RuntimeApprovalValidator) WithTerminationReader(reader store.RuntimeApprovalTerminationReader) *RuntimeApprovalValidator {
	v.terminationReader = reader
	return v
}

func (*RuntimeApprovalValidator) Name() string { return "runtime-approval-validator" }
func (*RuntimeApprovalValidator) Handles(kind string, operation Operation) bool {
	return kind == "RuntimeApproval" && (operation == OperationCreate || operation == OperationUpdate || operation == OperationDelete)
}

func (v *RuntimeApprovalValidator) Validate(ctx context.Context, req *Request) *Response {
	if req.Operation == OperationDelete {
		return v.validateDelete(req)
	}
	var current api.RuntimeApproval
	if err := json.Unmarshal(req.Object, &current); err != nil {
		return denyRuntimeApproval("invalid RuntimeApproval: " + err.Error())
	}
	canonical, err := runtimeapproval.CanonicalizeJSON(req.Object)
	if err != nil {
		return denyRuntimeApproval("invalid RuntimeApproval JSON")
	}
	var raw struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(canonical, &raw); err != nil {
		return denyRuntimeApproval("invalid RuntimeApproval specification")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw.Spec))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&current.Spec); err != nil {
		return denyRuntimeApproval("invalid RuntimeApproval specification: " + err.Error())
	}
	if current.Spec.Workspace != req.Namespace {
		return denyRuntimeApproval("spec.workspace must equal metadata.namespace")
	}
	if current.Name != runtimeapproval.CanonicalName(current.Spec.Workspace, current.Spec.InvocationID) {
		return denyRuntimeApproval("metadata.name must be the canonical RuntimeApproval name")
	}
	unnormalized := current.Spec
	if err := runtimeapproval.NormalizeAndValidateReferences(&current); err != nil {
		return denyRuntimeApproval(err.Error())
	}
	if !reflect.DeepEqual(unnormalized, current.Spec) {
		return denyRuntimeApproval("RuntimeApproval references must use canonical ordering")
	}
	if v.isLifecycleWriter(req.UserInfo) && !v.isController(req.UserInfo) && !v.isDecisionAuthority(req.UserInfo) &&
		(v.executionWriters[req.UserInfo.Username] == "" || v.executionWriters[req.UserInfo.Username] != current.Spec.Plane) {
		return denyRuntimeApproval("RuntimeApproval lifecycle writer does not match the operation plane")
	}
	if req.Operation == OperationCreate {
		if !v.isLifecycleWriter(req.UserInfo) {
			return denyRuntimeApproval("RuntimeApproval creation is reserved for trusted lifecycle writers")
		}
		if !hasRuntimeApprovalCompactionFinalizer(current.Finalizers) {
			return denyRuntimeApproval("RuntimeApproval creation requires the compaction finalizer")
		}
		if current.Spec.InitialDecision == "" || current.Spec.Binding.Epoch <= 0 {
			return denyRuntimeApproval("RuntimeApproval creation requires initialDecision and binding epoch")
		}
		if reflect.DeepEqual(current.Status, api.RuntimeApprovalStatus{}) {
			return &Response{Allowed: true}
		}
		if err := runtimeapproval.ValidateInitialStatus(current.Status); err != nil {
			return denyRuntimeApproval(err.Error())
		}
		return &Response{Allowed: true}
	}
	var previous api.RuntimeApproval
	if len(req.OldObject) == 0 || json.Unmarshal(req.OldObject, &previous) != nil {
		return denyRuntimeApproval("existing RuntimeApproval is required")
	}
	runtimeapproval.NormalizeLegacyStoredApproval(&previous)
	runtimeapproval.NormalizeLegacyStoredApproval(&current)
	if !reflect.DeepEqual(previous.Spec, current.Spec) {
		return denyRuntimeApproval("RuntimeApproval spec is immutable")
	}
	if !reflect.DeepEqual(previous.Finalizers, current.Finalizers) && !v.isController(req.UserInfo) {
		return denyRuntimeApproval("RuntimeApproval finalizers are reserved for the controller")
	}
	if !reflect.DeepEqual(previous.Status, current.Status) && !v.isLifecycleWriter(req.UserInfo) && !v.isDecisionAuthority(req.UserInfo) {
		return denyRuntimeApproval("RuntimeApproval status is reserved for trusted lifecycle writers")
	}
	if previous.Status.DecisionState != current.Status.DecisionState &&
		(current.Status.DecisionState == api.RuntimeApprovalDecisionApproved || current.Status.DecisionState == api.RuntimeApprovalDecisionRejected) &&
		!v.isDecisionAuthority(req.UserInfo) {
		return denyRuntimeApproval("human RuntimeApproval decisions are reserved for the decision authority")
	}
	if (current.Status.DecisionState == api.RuntimeApprovalDecisionApproved || current.Status.DecisionState == api.RuntimeApprovalDecisionRejected) &&
		!v.isDecisionAuthority(req.UserInfo) &&
		(previous.Status.DecidedByIssuer != current.Status.DecidedByIssuer || previous.Status.DecidedBySubject != current.Status.DecidedBySubject ||
			!equalRuntimeApprovalDecisionTime(previous.Status.DecidedAt, current.Status.DecidedAt) ||
			!equalRuntimeApprovalDecisionAudit(previous.Status.DecisionAudit, current.Status.DecisionAudit)) {
		return denyRuntimeApproval("human RuntimeApproval decision evidence is reserved for the decision authority")
	}
	if current.Status.DecisionState == api.RuntimeApprovalDecisionPending &&
		(current.Status.DecidedByIssuer != "" || current.Status.DecidedBySubject != "" || current.Status.DecidedAt != nil || current.Status.DecisionAudit != nil) {
		return denyRuntimeApproval("Pending RuntimeApproval cannot contain human decision evidence")
	}
	if (previous.Status.ReconciledByIssuer != current.Status.ReconciledByIssuer ||
		previous.Status.ReconciledBySubject != current.Status.ReconciledBySubject ||
		previous.Status.ReconciliationEvidenceRef != current.Status.ReconciliationEvidenceRef ||
		(previous.Status.ExecutionDisposition != current.Status.ExecutionDisposition &&
			(current.Status.ExecutionDisposition == "ReconciledCompleted" || current.Status.ExecutionDisposition == "ReconciledNotExecuted"))) &&
		!v.isDecisionAuthority(req.UserInfo) {
		return denyRuntimeApproval("human RuntimeApproval reconciliation is reserved for the decision authority")
	}
	humanReconciliation := v.isDecisionAuthority(req.UserInfo) && (current.Status.ExecutionDisposition == runtimeapproval.ExecutionDispositionReconciledCompleted || current.Status.ExecutionDisposition == runtimeapproval.ExecutionDispositionReconciledNotExecuted)
	if runtimeApprovalExecutionChanged(previous.Status, current.Status) && v.isController(req.UserInfo) && !humanReconciliation && previous.Status.Claimant == nil {
		if err := validateControllerPreparationTransition(previous, current); err != nil {
			return denyRuntimeApproval(err.Error())
		}
	}
	if runtimeApprovalExecutionChanged(previous.Status, current.Status) && v.isController(req.UserInfo) && !humanReconciliation && previous.Status.Claimant != nil &&
		(previous.Status.ExecutionState != api.RuntimeApprovalExecutionClaimed || current.Status.ExecutionState != api.RuntimeApprovalExecutionUncertain) {
		return denyRuntimeApproval("controller cannot finalize or reassign a retained execution claim")
	}
	if runtimeApprovalExecutionChanged(previous.Status, current.Status) && v.isController(req.UserInfo) && !humanReconciliation && previous.Status.Claimant != nil {
		if previous.Status.FencingToken < runtimeapproval.MinFencingToken || current.Status.TerminationProofRef == "" || v.terminationReader == nil {
			return denyRuntimeApproval("controller recovery requires authoritative claimant termination evidence")
		}
		proof, err := v.terminationReader.RuntimeApprovalTerminationProof(ctx, *previous.Status.Claimant)
		if err != nil || proof == "" || proof != current.Status.TerminationProofRef {
			return denyRuntimeApproval("controller recovery claimant termination proof is unavailable or mismatched")
		}
		retained := current.Status
		retained.ExecutionState = previous.Status.ExecutionState
		retained.TerminationProofRef = previous.Status.TerminationProofRef
		if runtimeApprovalExecutionChanged(previous.Status, retained) {
			return denyRuntimeApproval("controller recovery may change only uncertainty and termination evidence")
		}
	}
	if runtimeApprovalExecutionChanged(previous.Status, current.Status) && v.isDecisionAuthority(req.UserInfo) && (!v.isController(req.UserInfo) || humanReconciliation) {
		if err := validateHumanExecutionReconciliation(previous, current); err != nil {
			return denyRuntimeApproval(err.Error())
		}
	}
	if runtimeApprovalExecutionChanged(previous.Status, current.Status) && !v.isDecisionAuthority(req.UserInfo) && !v.isController(req.UserInfo) {
		if v.executionWriters[req.UserInfo.Username] == "" || v.executionWriters[req.UserInfo.Username] != current.Spec.Plane {
			return denyRuntimeApproval("RuntimeApproval execution writer does not match the operation plane")
		}
		if err := validateGatewayExecutionTransition(previous, current); err != nil {
			return denyRuntimeApproval(err.Error())
		}
		owner := current.Status.Claimant
		if previous.Status.Claimant != nil {
			owner = previous.Status.Claimant
		}
		if owner != nil {
			podUIDs := req.UserInfo.Extra["authentication.kubernetes.io/pod-uid"]
			if len(podUIDs) != 1 || podUIDs[0] == "" || podUIDs[0] != owner.WorkloadUID {
				return denyRuntimeApproval("RuntimeApproval execution writer is not the authenticated claimant Pod")
			}
			podNames := req.UserInfo.Extra["authentication.kubernetes.io/pod-name"]
			if len(podNames) != 1 || podNames[0] == "" || v.claimantReader == nil {
				return denyRuntimeApproval("RuntimeApproval execution requires live authenticated claimant verification")
			}
			if err := v.claimantReader.VerifyRuntimeApprovalClaimant(ctx, req.UserInfo.Username, podNames[0], podUIDs[0], current.Spec.Plane, *owner); err != nil {
				return denyRuntimeApproval("RuntimeApproval claimant is not the live authenticated gateway container")
			}
		}
	}
	if previous.Status.Claimant != nil {
		before, after := previous.Status.Claimant, current.Status.Claimant
		if after == nil || before.WorkloadUID != after.WorkloadUID || before.ContainerID != after.ContainerID || before.ProcessStartNonce != after.ProcessStartNonce ||
			!equalRuntimeApprovalDecisionTime(&before.ClaimedAt, &after.ClaimedAt) || previous.Status.FencingToken != current.Status.FencingToken {
			return denyRuntimeApproval("RuntimeApproval retained execution claimant and fence are immutable")
		}
	}
	if previous.Status.TerminationProofRef != "" && previous.Status.TerminationProofRef != current.Status.TerminationProofRef {
		return denyRuntimeApproval("RuntimeApproval retained termination proof is immutable")
	}
	if isTerminalRuntimeApprovalDecision(previous.Status.DecisionState) && previous.Status.DecisionState != current.Status.DecisionState {
		return denyRuntimeApproval("terminal RuntimeApproval decision is immutable")
	}
	if previous.Status.DecisionAudit != nil && !equalRuntimeApprovalDecisionAudit(previous.Status.DecisionAudit, current.Status.DecisionAudit) {
		return denyRuntimeApproval("RuntimeApproval decision audit outbox is immutable")
	}
	if isTerminalRuntimeApprovalDecision(current.Status.DecisionState) {
		requireActor := current.Status.DecisionState == api.RuntimeApprovalDecisionApproved || current.Status.DecisionState == api.RuntimeApprovalDecisionRejected
		if err := validateRuntimeApprovalTerminalEvidence(current, requireActor); err != nil {
			return denyRuntimeApproval(err.Error())
		}
	}
	if previous.Status.DecisionState == "" {
		if current.Status.DecisionState != current.Spec.InitialDecision || current.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted {
			return denyRuntimeApproval("RuntimeApproval initial status must match immutable initialDecision")
		}
		if current.Spec.InitialDecision == api.RuntimeApprovalDecisionPending && !reflect.DeepEqual(current.Status, runtimeapproval.InitialStatus(api.RuntimeApprovalDecisionPending)) {
			return denyRuntimeApproval("Pending RuntimeApproval initialization contains lifecycle fields")
		}
		if current.Spec.InitialDecision == api.RuntimeApprovalDecisionNotRequired {
			if err := validateRuntimeApprovalTerminalEvidence(current, false); err != nil {
				return denyRuntimeApproval(err.Error())
			}
		}
		return &Response{Allowed: true}
	}
	if previous.Status.DecisionState == api.RuntimeApprovalDecisionPending && current.Status.DecisionState != api.RuntimeApprovalDecisionPending {
		if err := validateRuntimeApprovalDecisionTransition(current); err != nil {
			return denyRuntimeApproval(err.Error())
		}
	}
	return &Response{Allowed: true}
}

func (v *RuntimeApprovalValidator) validateDelete(req *Request) *Response {
	if !v.isController(req.UserInfo) {
		return denyRuntimeApproval("RuntimeApproval deletion is reserved for the controller")
	}
	var approval api.RuntimeApproval
	if len(req.OldObject) == 0 || json.Unmarshal(req.OldObject, &approval) != nil {
		return denyRuntimeApproval("existing RuntimeApproval is required for deletion")
	}
	if approval.Spec.Workspace != req.Namespace || approval.Name != runtimeapproval.CanonicalName(approval.Spec.Workspace, approval.Spec.InvocationID) {
		return denyRuntimeApproval("RuntimeApproval deletion requires its canonical workspace identity")
	}
	if hasRuntimeApprovalCompactionFinalizer(approval.Finalizers) {
		return denyRuntimeApproval("RuntimeApproval deletion requires durable compaction")
	}
	if !approval.Status.DecisionEvidenceDelivered {
		return denyRuntimeApproval("RuntimeApproval deletion requires delivered decision evidence")
	}
	terminalExecution := approval.Status.ExecutionState == api.RuntimeApprovalExecutionCompleted || approval.Status.ExecutionState == api.RuntimeApprovalExecutionFailed
	terminalDecisionOnly := (approval.Status.DecisionState == api.RuntimeApprovalDecisionRejected || approval.Status.DecisionState == api.RuntimeApprovalDecisionExpired) && approval.Status.ExecutionState == api.RuntimeApprovalExecutionNotStarted
	if !terminalExecution && !terminalDecisionOnly {
		return denyRuntimeApproval("RuntimeApproval deletion requires a terminal decision and execution state")
	}
	return &Response{Allowed: true}
}

func hasRuntimeApprovalCompactionFinalizer(finalizers []string) bool {
	for _, finalizer := range finalizers {
		if finalizer == k8sapi.RuntimeApprovalCompactionFinalizer {
			return true
		}
	}
	return false
}

func (v *RuntimeApprovalValidator) isController(user UserInfo) bool {
	return v.controllerUsername != "" && user.Username == v.controllerUsername
}

func (v *RuntimeApprovalValidator) isDecisionAuthority(user UserInfo) bool {
	return v.decisionAuthorityUsername != "" && user.Username == v.decisionAuthorityUsername
}

func (v *RuntimeApprovalValidator) isLifecycleWriter(user UserInfo) bool {
	_, allowed := v.lifecycleWriters[user.Username]
	return allowed
}

func validateRuntimeApprovalDecisionTransition(approval api.RuntimeApproval) error {
	decision := approval.Status.DecisionState
	if decision != api.RuntimeApprovalDecisionApproved && decision != api.RuntimeApprovalDecisionRejected && decision != api.RuntimeApprovalDecisionExpired {
		return fmt.Errorf("Pending RuntimeApproval may transition only to Approved, Rejected, or Expired")
	}
	return validateRuntimeApprovalTerminalEvidence(approval, decision != api.RuntimeApprovalDecisionExpired)
}

func validateRuntimeApprovalTerminalEvidence(approval api.RuntimeApproval, requireActor bool) error {
	decision := approval.Status.DecisionState
	if approval.Status.DecidedAt == nil || approval.Status.DecisionAudit == nil {
		return fmt.Errorf("terminal RuntimeApproval decision requires atomic decision audit evidence")
	}
	audit := approval.Status.DecisionAudit
	payload := audit.Payload
	if audit.ID == "" || payload.Workspace != approval.Spec.Workspace || payload.InvocationID != approval.Spec.InvocationID || payload.ApprovalUID != approval.UID || payload.Decision != decision || !payload.DecidedAt.Equal(*approval.Status.DecidedAt) || payload.Issuer != approval.Status.DecidedByIssuer || payload.Subject != approval.Status.DecidedBySubject {
		return fmt.Errorf("RuntimeApproval decision audit does not match the terminal decision")
	}
	if requireActor && (approval.Status.DecidedByIssuer == "" || approval.Status.DecidedBySubject == "") {
		return fmt.Errorf("human RuntimeApproval decision requires canonical actor")
	}
	return nil
}

func validateControllerPreparationTransition(previous, current api.RuntimeApproval) error {
	before, after := previous.Status, current.Status
	if before.DecisionState == "" && before.ExecutionState == "" && after.ExecutionState == api.RuntimeApprovalExecutionNotStarted && after.DecisionState == current.Spec.InitialDecision {
		return nil
	}
	if before.ExecutionState != api.RuntimeApprovalExecutionNotStarted || after.ExecutionState != api.RuntimeApprovalExecutionFailed || after.ExecutionDisposition != runtimeapproval.ExecutionDispositionAbandonedPreparation || before.PreparationDeadline == nil || time.Now().UTC().Before(*before.PreparationDeadline) || before.PreparationToken == "" || after.PreparationToken != "" || after.PreparationDeadline != nil || after.Claimant != nil || after.FencingToken != 0 || after.TerminationProofRef != "" {
		return fmt.Errorf("controller execution writes require initial state or expired preparation reaping")
	}
	return runtimeapproval.ValidatePreparationToken(before.PreparationToken)
}

func validateHumanExecutionReconciliation(previous, current api.RuntimeApproval) error {
	before, after := previous.Status, current.Status
	if before.ExecutionState != api.RuntimeApprovalExecutionUncertain || before.Claimant == nil || before.FencingToken < runtimeapproval.MinFencingToken {
		return fmt.Errorf("human execution reconciliation requires a retained uncertain claim")
	}
	if err := runtimeapproval.ValidateReconciliationDisposition(after.ExecutionDisposition); err != nil {
		return err
	}
	if (after.ExecutionState == api.RuntimeApprovalExecutionCompleted) != (after.ExecutionDisposition == runtimeapproval.ExecutionDispositionReconciledCompleted) || (after.ExecutionState != api.RuntimeApprovalExecutionCompleted && after.ExecutionState != api.RuntimeApprovalExecutionFailed) {
		return fmt.Errorf("human reconciliation state does not match its disposition")
	}
	if after.ReconciledByIssuer == "" || after.ReconciledBySubject == "" || after.ReconciliationEvidenceRef == "" || len(after.ReconciliationEvidenceRef) > 512 {
		return fmt.Errorf("human execution reconciliation requires canonical actor and bounded evidence")
	}
	return nil
}

func validateGatewayExecutionTransition(previous, current api.RuntimeApproval) error {
	before, after := previous.Status, current.Status
	if before.DecisionState == "" && before.ExecutionState == "" && after.ExecutionState == api.RuntimeApprovalExecutionNotStarted && after.DecisionState == current.Spec.InitialDecision {
		return nil
	}
	switch before.ExecutionState {
	case api.RuntimeApprovalExecutionNotStarted:
		if after.ExecutionState == api.RuntimeApprovalExecutionNotStarted {
			if after.Claimant != nil || after.FencingToken != 0 || after.ExecutionDisposition != "" || after.TerminationProofRef != "" {
				return fmt.Errorf("gateway preparation cannot contain claimed execution fields")
			}
			if after.PreparationToken != "" {
				if err := runtimeapproval.ValidatePreparationToken(after.PreparationToken); err != nil {
					return err
				}
				if after.PreparationDeadline == nil {
					return fmt.Errorf("gateway preparation requires its bounded deadline")
				}
			}
			return nil
		}
		if !runtimeapproval.PermitsExecution(before.DecisionState) || !before.DecisionEvidenceDelivered {
			return fmt.Errorf("gateway execution requires authorized delivered decision evidence")
		}
		if after.ExecutionState == api.RuntimeApprovalExecutionClaimed {
			if before.PreparationToken == "" || before.PreparationDeadline == nil || after.Claimant == nil || after.FencingToken != runtimeapproval.MinFencingToken || after.PreparationToken != "" || after.PreparationDeadline != nil {
				return fmt.Errorf("gateway claim requires retained preparation and an initial fence")
			}
			if err := runtimeapproval.ValidatePreparationToken(before.PreparationToken); err != nil {
				return err
			}
			if !time.Now().UTC().Before(*before.PreparationDeadline) {
				return fmt.Errorf("gateway claim preparation has expired")
			}
			if before.DecisionState == api.RuntimeApprovalDecisionApproved && !time.Now().UTC().Before(previous.Spec.ExpiresAt) {
				return fmt.Errorf("gateway claim approval has expired")
			}
			if err := runtimeapproval.ValidateClaimant(*after.Claimant); err != nil {
				return err
			}
			return nil
		}
		if after.ExecutionState == api.RuntimeApprovalExecutionFailed && after.Claimant == nil && after.FencingToken == 0 && after.PreparationToken == "" && after.PreparationDeadline == nil {
			if after.ExecutionDisposition == runtimeapproval.ExecutionDispositionApprovalExpiredBeforeClaim && before.DecisionState == api.RuntimeApprovalDecisionApproved && before.PreparationToken != "" && before.PreparationDeadline != nil && !time.Now().UTC().Before(previous.Spec.ExpiresAt) {
				return nil
			}
			if err := runtimeapproval.ValidatePreDispatchDisposition(after.ExecutionDisposition); err == nil && after.ExecutionDisposition != runtimeapproval.ExecutionDispositionAbandonedPreparation && after.ExecutionDisposition != runtimeapproval.ExecutionDispositionApprovalExpiredBeforeClaim {
				return nil
			}
		}
	case api.RuntimeApprovalExecutionClaimed:
		if before.Claimant == nil || before.FencingToken < runtimeapproval.MinFencingToken {
			return fmt.Errorf("gateway execution requires a retained claimant and positive fence")
		}
		if after.PreparationToken != "" || after.PreparationDeadline != nil || after.TerminationProofRef != before.TerminationProofRef {
			return fmt.Errorf("gateway claimed execution cannot rewrite preparation or recovery evidence")
		}
		if after.ExecutionState == api.RuntimeApprovalExecutionClaimed {
			return nil
		}
		if after.ExecutionState == api.RuntimeApprovalExecutionCompleted || after.ExecutionState == api.RuntimeApprovalExecutionFailed {
			if err := runtimeapproval.ValidateTerminalDisposition(after.ExecutionDisposition); err != nil {
				return err
			}
			if (after.ExecutionState == api.RuntimeApprovalExecutionCompleted) != (after.ExecutionDisposition == runtimeapproval.ExecutionDispositionSucceeded) {
				return fmt.Errorf("gateway terminal state does not match execution disposition")
			}
			return nil
		}
	}
	return fmt.Errorf("gateway execution transition is not owned by this writer")
}

func runtimeApprovalExecutionChanged(previous, current api.RuntimeApprovalStatus) bool {
	return previous.ExecutionState != current.ExecutionState || previous.ExecutionDisposition != current.ExecutionDisposition ||
		previous.PreparationToken != current.PreparationToken || !equalRuntimeApprovalDecisionTime(previous.PreparationDeadline, current.PreparationDeadline) ||
		!reflect.DeepEqual(previous.Claimant, current.Claimant) || previous.FencingToken != current.FencingToken || previous.TerminationProofRef != current.TerminationProofRef
}

func equalRuntimeApprovalDecisionTime(previous, current *time.Time) bool {
	if previous == nil || current == nil {
		return previous == current
	}
	return previous.UTC().Truncate(time.Second).Equal(current.UTC().Truncate(time.Second))
}

func equalRuntimeApprovalDecisionAudit(previous, current *api.RuntimeApprovalDecisionAudit) bool {
	if previous == nil || current == nil {
		return previous == current
	}
	previousPayload := previous.Payload
	currentPayload := current.Payload
	return previous.ID == current.ID &&
		previousPayload.Workspace == currentPayload.Workspace &&
		previousPayload.InvocationID == currentPayload.InvocationID &&
		previousPayload.ApprovalUID == currentPayload.ApprovalUID &&
		previousPayload.Decision == currentPayload.Decision &&
		previousPayload.Issuer == currentPayload.Issuer &&
		previousPayload.Subject == currentPayload.Subject &&
		previousPayload.DecidedAt.UTC().Truncate(time.Second).Equal(currentPayload.DecidedAt.UTC().Truncate(time.Second))
}

func isTerminalRuntimeApprovalDecision(decision string) bool {
	return decision == api.RuntimeApprovalDecisionNotRequired || decision == api.RuntimeApprovalDecisionApproved || decision == api.RuntimeApprovalDecisionRejected || decision == api.RuntimeApprovalDecisionExpired
}

func denyRuntimeApproval(reason string) *Response { return &Response{Allowed: false, Reason: reason} }
