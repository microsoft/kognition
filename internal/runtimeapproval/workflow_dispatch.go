package runtimeapproval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

var ErrWorkflowDispatchUnsupported = errors.New("workflow dispatch requires a qualified activation and live authorization boundary")

type WorkflowDispatchApprovalReader interface {
	GetRuntimeApprovalByInvocation(context.Context, string, string) (api.RuntimeApproval, error)
}

type WorkflowDispatchConfig struct {
	Activations store.WorkflowActivationStore
	Approvals   WorkflowDispatchApprovalReader
	Keys        BindingKeyStore
	Clock       func() (time.Time, error)
	Eligibility func(context.Context, api.RuntimeApproval) error
}

type WorkflowDispatchGate struct {
	config WorkflowDispatchConfig
}

type WorkflowDispatchResult struct {
	DispatchCommitted bool
	TargetCalled      bool
}

func NewWorkflowDispatchGate(config WorkflowDispatchConfig) (*WorkflowDispatchGate, error) {
	if config.Activations == nil || config.Approvals == nil || config.Keys == nil || config.Clock == nil || config.Eligibility == nil {
		return nil, ErrWorkflowDispatchUnsupported
	}
	return &WorkflowDispatchGate{config: config}, nil
}

func WorkflowApprovalActorDigest(approval api.RuntimeApproval) (string, error) {
	if err := NormalizeAndValidateReferences(&approval); err != nil {
		return "", err
	}
	if approval.Spec.ActorKind == api.RuntimeApprovalActorExecutor {
		return workflowExecutorActorDigest(approval.Spec.ExecutorAction.Executor, approval.Spec.ExecutorAction.Workflow, approval.Spec.ExecutorAction.ExecutionBinding)
	}
	if approval.Spec.SessionRef == "" || len(approval.Spec.SessionRef) > 256 {
		return "", store.ErrConflict
	}
	encoded, err := json.Marshal(struct {
		Agent    api.RuntimeApprovalReference `json:"agent"`
		Identity api.RuntimeApprovalReference `json:"identity"`
		Instance api.RuntimeApprovalReference `json:"instance"`
		Session  string                       `json:"session"`
	}{workflowActorIncarnationReference(approval.Spec.AgentRef), workflowActorIncarnationReference(approval.Spec.AgentIdentityRef),
		workflowActorIncarnationReference(approval.Spec.AgentInstanceRef), approval.Spec.SessionRef})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// WorkflowExecutorActorDigest is the activation actor that every effect of this executor, Workflow and authority carries.
func WorkflowExecutorActorDigest(action WorkflowExecutorAction) (string, error) {
	return workflowExecutorActorDigest(action.Executor, action.Workflow, action.ExecutionBinding)
}

func workflowExecutorActorDigest(executor api.ControlPlaneRoleSubject, workflow, authority api.RuntimeApprovalReference) (string, error) {
	encoded, err := json.Marshal(struct {
		Executor  api.ControlPlaneRoleSubject  `json:"executor"`
		Workflow  api.RuntimeApprovalReference `json:"workflow"`
		Authority api.RuntimeApprovalReference `json:"authority"`
	}{executor, workflowActorIncarnationReference(workflow), workflowActorIncarnationReference(authority)})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func workflowActorIncarnationReference(reference api.RuntimeApprovalReference) api.RuntimeApprovalReference {
	reference.ResourceVersion = 0
	reference.ConfigurationDigest = ""
	return reference
}

func WorkflowApprovalEffect(approval api.RuntimeApproval) (store.WorkflowEffect, error) {
	if approval.UID == "" || len(approval.UID) > 128 || approval.Spec.InvocationID == "" || len(approval.Spec.InvocationID) > 128 || approval.Spec.WorkflowContext == nil {
		return store.WorkflowEffect{}, ErrWorkflowDispatchUnsupported
	}
	if approval.Spec.Plane != api.RuntimeApprovalPlaneTool && approval.Spec.Plane != api.RuntimeApprovalPlaneMemory && approval.Spec.Plane != api.RuntimeApprovalPlaneCommunication &&
		!(approval.Spec.ActorKind == api.RuntimeApprovalActorExecutor && approval.Spec.Plane == api.RuntimeApprovalPlaneWorkflow) {
		return store.WorkflowEffect{}, ErrWorkflowDispatchUnsupported
	}
	actor, err := WorkflowApprovalActorDigest(approval)
	if err != nil {
		return store.WorkflowEffect{}, err
	}
	encoded, err := json.Marshal(struct {
		UID     string                   `json:"uid"`
		Binding api.RuntimeActionBinding `json:"binding"`
	}{approval.UID, approval.Spec.Binding})
	if err != nil {
		return store.WorkflowEffect{}, err
	}
	digest := sha256.Sum256(encoded)
	plane := strings.ToLower(approval.Spec.Plane)
	if approval.Spec.ActorKind == api.RuntimeApprovalActorExecutor {
		plane = "communication"
	}
	return store.WorkflowEffect{ID: CanonicalName(approval.Spec.Workspace, approval.Spec.InvocationID),
		BindingDigest: hex.EncodeToString(digest[:]), Actor: actor, Plane: plane,
		Epoch: approval.Spec.WorkflowContext.OwnerEpoch}, nil
}

func (gate *WorkflowDispatchGate) Register(ctx context.Context, expected api.RuntimeApproval, payload json.RawMessage) error {
	current, err := gate.authorize(ctx, expected, payload)
	if err != nil {
		return err
	}
	effect, err := WorkflowApprovalEffect(current)
	if err != nil {
		return err
	}
	key := workflowContextKey(*current.Spec.WorkflowContext)
	activation, err := gate.config.Activations.GetWorkflowActivation(ctx, key)
	if err != nil {
		return err
	}
	if activation.Spec.BindingDigest != current.Spec.WorkflowContext.BindingDigest || activation.Mode != store.WorkflowGateActive || activation.Epoch != effect.Epoch {
		return store.ErrConflict
	}
	_, err = gate.config.Activations.MutateWorkflowActivation(ctx, key, store.WorkflowActivationMutation{
		Kind: store.WorkflowGateRegister, ExpectedVersion: activation.Version, Effect: effect,
	})
	return err
}

func (gate *WorkflowDispatchGate) Dispatch(ctx context.Context, expected api.RuntimeApproval, payload json.RawMessage, target func(context.Context) error) (WorkflowDispatchResult, error) {
	if target == nil {
		return WorkflowDispatchResult{}, ErrWorkflowDispatchUnsupported
	}
	current, err := gate.authorize(ctx, expected, payload)
	if err != nil {
		return WorkflowDispatchResult{}, err
	}
	claim, err := workflowApprovalClaim(current, expected)
	if err != nil {
		return WorkflowDispatchResult{}, err
	}
	effect, err := WorkflowApprovalEffect(current)
	if err != nil {
		return WorkflowDispatchResult{}, err
	}
	key := workflowContextKey(*current.Spec.WorkflowContext)
	activation, err := gate.config.Activations.GetWorkflowActivation(ctx, key)
	if err != nil {
		return WorkflowDispatchResult{}, err
	}
	if activation.Spec.BindingDigest != current.Spec.WorkflowContext.BindingDigest {
		return WorkflowDispatchResult{}, store.ErrConflict
	}
	if err := gate.checkDeadline(ctx, current); err != nil {
		return WorkflowDispatchResult{}, err
	}
	committed, err := gate.config.Activations.MutateWorkflowActivation(ctx, key, store.WorkflowActivationMutation{
		Kind: store.WorkflowGateCommit, ExpectedVersion: activation.Version, Effect: effect, Claim: claim,
	})
	if err != nil {
		return WorkflowDispatchResult{}, err
	}
	if !committed.DispatchGranted {
		return WorkflowDispatchResult{}, store.ErrConflict
	}
	result := WorkflowDispatchResult{DispatchCommitted: true}
	current, err = gate.authorize(ctx, expected, payload)
	if err != nil {
		return result, err
	}
	confirmedClaim, err := workflowApprovalClaim(current, expected)
	if err != nil || confirmedClaim != claim {
		return result, errors.Join(store.ErrConflict, err)
	}
	if err := gate.checkDeadline(ctx, current); err != nil {
		return result, err
	}
	result.TargetCalled = true
	return result, target(ctx)
}

func (gate *WorkflowDispatchGate) authorizeCommitted(ctx context.Context, expected api.RuntimeApproval, payload json.RawMessage) error {
	current, err := gate.authorize(ctx, expected, payload)
	if err != nil {
		return err
	}
	claim, err := workflowApprovalClaim(current, expected)
	if err != nil {
		return err
	}
	effect, err := WorkflowApprovalEffect(current)
	if err != nil {
		return err
	}
	activation, err := gate.config.Activations.GetWorkflowActivation(ctx, workflowContextKey(*current.Spec.WorkflowContext))
	if err != nil {
		return err
	}
	retained, found := activation.Effects[effect.ID]
	if !found || activation.Mode != store.WorkflowGateActive || activation.Epoch != effect.Epoch ||
		activation.Spec.BindingDigest != current.Spec.WorkflowContext.BindingDigest || retained.ID != effect.ID ||
		retained.BindingDigest != effect.BindingDigest || retained.Actor != effect.Actor || retained.Plane != effect.Plane ||
		retained.Epoch != effect.Epoch || retained.Claim != claim || retained.State != store.WorkflowEffectCommitted {
		return store.ErrConflict
	}
	return gate.checkDeadline(ctx, current)
}

func (gate *WorkflowDispatchGate) authorize(ctx context.Context, expected api.RuntimeApproval, payload json.RawMessage) (api.RuntimeApproval, error) {
	if gate == nil || gate.config.Activations == nil || gate.config.Approvals == nil || gate.config.Keys == nil ||
		gate.config.Clock == nil || gate.config.Eligibility == nil || expected.Spec.WorkflowContext == nil {
		return api.RuntimeApproval{}, ErrWorkflowDispatchUnsupported
	}
	if err := ctx.Err(); err != nil {
		return api.RuntimeApproval{}, err
	}
	current, err := gate.config.Approvals.GetRuntimeApprovalByInvocation(ctx, expected.Spec.Workspace, expected.Spec.InvocationID)
	if err != nil {
		return api.RuntimeApproval{}, err
	}
	if current.UID == "" || current.UID != expected.UID || current.Spec.WorkflowContext == nil || !SameImmutableBinding(current, expected) {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	if err := AuthenticateRetainedBinding(ctx, gate.config.Keys, current, payload); err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("authenticate Workflow effect: %w", err)
	}
	if err := gate.checkDeadline(ctx, current); err != nil {
		return api.RuntimeApproval{}, err
	}
	if err := gate.config.Eligibility(ctx, current); err != nil {
		return api.RuntimeApproval{}, err
	}
	if err := gate.checkDeadline(ctx, current); err != nil {
		return api.RuntimeApproval{}, err
	}
	return current, nil
}

func (gate *WorkflowDispatchGate) checkDeadline(ctx context.Context, approval api.RuntimeApproval) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now, err := gate.config.Clock()
	if err != nil {
		return err
	}
	if now.IsZero() || approval.Spec.ExpiresAt.IsZero() || !approval.Spec.ExpiresAt.After(now) {
		return store.ErrConflict
	}
	if approval.Status.ExecutionState == api.RuntimeApprovalExecutionClaimed && approval.Status.Claimant != nil &&
		approval.Status.Claimant.LeaseDeadline != nil && !approval.Status.Claimant.LeaseDeadline.After(now) {
		return store.ErrConflict
	}
	return nil
}

func workflowApprovalClaim(current, expected api.RuntimeApproval) (store.WorkflowEffectClaim, error) {
	actual, requested := current.Status.Claimant, expected.Status.Claimant
	if !PermitsExecution(current.Status.DecisionState) || !current.Status.DecisionEvidenceDelivered ||
		current.Status.DecisionAudit == nil || current.Status.DecisionAudit.ID == "" ||
		current.Status.ExecutionState != api.RuntimeApprovalExecutionClaimed || actual == nil || requested == nil ||
		actual.WorkloadUID == "" || actual.ProcessStartNonce == "" || actual.ClaimedAt.IsZero() ||
		actual.WorkloadUID != requested.WorkloadUID || actual.ProcessStartNonce != requested.ProcessStartNonce ||
		actual.ContainerID != requested.ContainerID || current.Status.FencingToken < MinFencingToken ||
		current.Status.FencingToken != expected.Status.FencingToken {
		return store.WorkflowEffectClaim{}, store.ErrConflict
	}
	return store.WorkflowEffectClaim{Kind: "RuntimeApproval", Namespace: current.Spec.Workspace, UID: current.UID,
		Claimant: actual.WorkloadUID, ProcessNonce: actual.ProcessStartNonce, FencingToken: current.Status.FencingToken}, nil
}
