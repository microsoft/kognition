// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/store"
)

type executorCoordinatorAuthority struct {
	decision string
	err      error
}

func (authority *executorCoordinatorAuthority) AuthorizeWorkflowExecutorAction(context.Context, WorkflowExecutorAction) (string, error) {
	return authority.decision, authority.err
}

func (authority *executorCoordinatorAuthority) AuthorizeWorkflowExecutorActivation(_ context.Context, _ WorkflowExecutorAction, activation store.WorkflowActivation) error {
	if activation.Mode != store.WorkflowGateActive {
		return errors.New("activation is no longer active")
	}
	return authority.err
}

func executorCoordinatorFixture(t *testing.T, decision string) (*WorkflowExecutorCoordinator, *executorCoordinatorAuthority, *fakeGate3Store, *workflowDispatchFixtureStore, WorkflowExecutorAction) {
	t.Helper()
	action, keys, now := executorActionFixture(t)
	proposed, err := BuildWorkflowExecutorApproval(context.Background(), keys, action, decision, now)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := WorkflowApprovalActorDigest(proposed)
	if err != nil {
		t.Fatal(err)
	}
	activation, err := store.NewWorkflowActivation(store.WorkflowActivationSpec{Key: action.Activation, BindingDigest: action.BindingDigest,
		Actors: []string{actor}, MaxEffects: 4, MaxBytes: 32768, ExpiresAt: action.ExpiresAt}, now)
	if err != nil {
		t.Fatal(err)
	}
	approvals := newFakeGate3Store()
	approvals.now = func() time.Time { return now }
	approvals.strictBinding = true
	fixture := &workflowDispatchFixtureStore{activation: activation, now: now}
	dispatch, err := NewWorkflowDispatchGate(WorkflowDispatchConfig{Approvals: approvals, Activations: fixture, Keys: keys,
		Clock: func() (time.Time, error) { return now, nil }, Eligibility: func(context.Context, api.RuntimeApproval) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	authority := &executorCoordinatorAuthority{decision: decision}
	coordinator, err := NewWorkflowExecutorCoordinator(WorkflowExecutorCoordinatorConfig{Store: approvals, Keys: keys,
		Authorizer: authority, Dispatch: dispatch, Clock: func() (time.Time, error) { return now, nil }, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator, authority, approvals, fixture, action
}

func TestExecutorCoordinatorPreservesPendingAndRejectedDecisions(t *testing.T) {
	coordinator, _, approvals, fixture, action := executorCoordinatorFixture(t, api.RuntimeApprovalDecisionPending)
	result, err := coordinator.Resolve(context.Background(), action)
	if err != nil || result.State != Gate3StatePending || len(fixture.activation.Effects) != 1 || result.Approval.Spec.AgentRef != (api.RuntimeApprovalReference{}) {
		t.Fatalf("executor initiation pending state = %+v %v", result, err)
	}
	replay, err := coordinator.Resolve(context.Background(), action)
	if err != nil || replay.Approval.UID != result.Approval.UID || replay.State != Gate3StatePending {
		t.Fatalf("pending executor replay = %+v %v", replay, err)
	}
	stored := approvals.approvals[action.InvocationID]
	stored.Status.DecisionState = api.RuntimeApprovalDecisionRejected
	decidedAt := fixture.now
	stored.Status.DecidedAt = &decidedAt
	PopulateDecisionAudit(stored)
	result, err = coordinator.Resolve(context.Background(), action)
	if err != nil || result.State != Gate3StateRejected || !result.Approval.Status.DecisionEvidenceDelivered {
		t.Fatalf("executor rejection was bypassed or lost evidence: %+v %v", result, err)
	}
	result, err = coordinator.Resolve(context.Background(), action)
	if err != nil || result.State != Gate3StateRejected {
		t.Fatalf("rejected executor replay changed decision: %+v %v", result, err)
	}
}

func TestExecutorCoordinatorReadyStillRequiresClaimAndLiveAuthority(t *testing.T) {
	coordinator, authority, approvals, _, action := executorCoordinatorFixture(t, api.RuntimeApprovalDecisionNotRequired)
	result, err := coordinator.Resolve(context.Background(), action)
	if err != nil || result.State != Gate3StateReady || !result.Approval.Status.DecisionEvidenceDelivered {
		t.Fatalf("authorized executor decision not ready: %+v %v", result, err)
	}
	calls := 0
	target := func(context.Context) error { calls++; return nil }
	if dispatch, err := coordinator.Dispatch(context.Background(), result.Approval, action.Payload, target); err == nil || dispatch.TargetCalled || calls != 0 {
		t.Fatalf("decision alone contacted target: %+v %v", dispatch, err)
	}
	stored := approvals.approvals[action.InvocationID]
	stored.Status.ExecutionState = api.RuntimeApprovalExecutionClaimed
	stored.Status.FencingToken = 1
	now := stored.Spec.CreatedAt
	stored.Status.Claimant = &api.RuntimeApprovalClaimant{WorkloadUID: "executor-gateway", ProcessStartNonce: "executor-process-0001", ClaimedAt: now, LastHeartbeat: now}
	authority.err = errors.New("executor binding revoked")
	if dispatch, err := coordinator.Dispatch(context.Background(), *stored, action.Payload, target); err == nil || dispatch.TargetCalled || calls != 0 {
		t.Fatalf("revoked executor contacted target: %+v %v", dispatch, err)
	}
	authority.err = nil
	dispatch, err := coordinator.Dispatch(context.Background(), *stored, action.Payload, target)
	if err != nil || !dispatch.TargetCalled || calls != 1 {
		t.Fatalf("exact executor claim did not dispatch: %+v %v", dispatch, err)
	}
	if dispatch, err := coordinator.Dispatch(context.Background(), *stored, action.Payload, target); err == nil || dispatch.TargetCalled || calls != 1 {
		t.Fatalf("executor initiation replayed: %+v %v", dispatch, err)
	}
}

func TestExecutorCoordinatorPolicyDowngradeBlocksDispatch(t *testing.T) {
	for _, decision := range []string{api.RuntimeApprovalDecisionPending, api.RuntimeApprovalDecisionRejected, api.RuntimeApprovalDecisionExpired, "unknown", ""} {
		t.Run(decision, func(t *testing.T) {
			coordinator, authority, approvals, _, action := executorCoordinatorFixture(t, api.RuntimeApprovalDecisionNotRequired)
			result, err := coordinator.Resolve(context.Background(), action)
			if err != nil || result.State != Gate3StateReady {
				t.Fatalf("initial resolution = %+v %v", result, err)
			}
			stored := approvals.approvals[action.InvocationID]
			stored.Status.ExecutionState = api.RuntimeApprovalExecutionClaimed
			stored.Status.FencingToken = 1
			now := stored.Spec.CreatedAt
			stored.Status.Claimant = &api.RuntimeApprovalClaimant{WorkloadUID: "executor-gateway", ProcessStartNonce: "executor-process-0001", ClaimedAt: now, LastHeartbeat: now}
			authority.decision = decision
			calls := 0
			dispatch, err := coordinator.Dispatch(context.Background(), *stored, action.Payload, func(context.Context) error { calls++; return nil })
			if err == nil || dispatch.DispatchCommitted || dispatch.TargetCalled || calls != 0 {
				t.Fatalf("policy downgrade dispatched a retained decision: %+v %v calls=%d", dispatch, err, calls)
			}
		})
	}
}

func TestExecutorCoordinatorRejectsExpandedActivationAuthority(t *testing.T) {
	for name, mutate := range map[string]func(*store.WorkflowActivationSpec){
		"effects":  func(spec *store.WorkflowActivationSpec) { spec.MaxEffects = 9 },
		"bytes":    func(spec *store.WorkflowActivationSpec) { spec.MaxBytes = 32769 },
		"handoffs": func(spec *store.WorkflowActivationSpec) { spec.MaxHandoffs = 101 },
		"expiry":   func(spec *store.WorkflowActivationSpec) { spec.ExpiresAt = spec.ExpiresAt.Add(time.Second) },
		"bounded":  func(*store.WorkflowActivationSpec) {},
	} {
		t.Run(name, func(t *testing.T) {
			coordinator, _, approvals, fixture, _ := executorCoordinatorFixture(t, api.RuntimeApprovalDecisionNotRequired)
			authorizer, _, action := liveExecutorAuthorityFixture(t)
			coordinator.config.Authorizer = authorizer
			fixture.activation.Spec.BindingDigest = action.BindingDigest
			mutate(&fixture.activation.Spec)
			var err error
			coordinator, err = NewWorkflowExecutorCoordinator(coordinator.config)
			if err != nil {
				t.Fatal(err)
			}
			result, err := coordinator.Resolve(context.Background(), action)
			if name == "bounded" {
				if err != nil || result.State != Gate3StateReady {
					t.Fatalf("bounded activation rejected: %+v %v", result, err)
				}
				return
			}
			if err == nil || result.State == Gate3StateReady || approvals.creates != 0 {
				t.Fatalf("expanded %s created initiation authority: %+v %v", name, result, err)
			}
		})
	}
}

func TestExecutorInitiationCommittedTargetFailureCannotReplay(t *testing.T) {
	coordinator, _, approvals, _, action := executorCoordinatorFixture(t, api.RuntimeApprovalDecisionNotRequired)
	now, _ := coordinator.config.Clock()
	service, err := NewWorkflowExecutorInitiationService(WorkflowExecutorInitiationConfig{Keys: coordinator.config.Keys, Coordinator: coordinator,
		CurrentExecutor:      func(context.Context) (api.ControlPlaneRoleSubject, error) { return action.Executor, nil },
		CurrentControllerUID: func(context.Context) (string, error) { return "controller-uid", nil }, Clock: coordinator.config.Clock})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := service.Issue(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Resolve(context.Background(), action, capability)
	if err != nil || result.State != Gate3StateReady {
		t.Fatalf("resolve: %+v %v", result, err)
	}
	calls := 0
	target := func(context.Context) error { calls++; return errors.New("target acknowledgement lost") }
	if dispatch, err := service.Dispatch(context.Background(), action, capability, result.Approval, target); err == nil || dispatch.TargetCalled || calls != 0 {
		t.Fatalf("unclaimed capability invoked target: %+v %v", dispatch, err)
	}
	claimed := approvals.approvals[action.InvocationID]
	claimed.Status.ExecutionState = api.RuntimeApprovalExecutionClaimed
	claimed.Status.FencingToken = 1
	claimed.Status.Claimant = &api.RuntimeApprovalClaimant{WorkloadUID: "executor-gateway", ProcessStartNonce: "executor-process-0001", ClaimedAt: now, LastHeartbeat: now}
	dispatch, err := service.Dispatch(context.Background(), action, capability, *claimed, target)
	if err == nil || !dispatch.DispatchCommitted || !dispatch.TargetCalled || calls != 1 {
		t.Fatalf("target failure lost committed dispatch evidence: %+v %v calls=%d", dispatch, err, calls)
	}
	if retry, err := service.Dispatch(context.Background(), action, capability, *claimed, target); err == nil || retry.TargetCalled || calls != 1 {
		t.Fatalf("lost acknowledgement permitted replay: %+v %v calls=%d", retry, err, calls)
	}
}

func TestExecutorCoordinatorDenialMakesNoDecisionAndStopBlocksReady(t *testing.T) {
	coordinator, authority, approvals, fixture, action := executorCoordinatorFixture(t, api.RuntimeApprovalDecisionNotRequired)
	authority.err = errors.New("target invocation denied")
	if result, err := coordinator.Resolve(context.Background(), action); err == nil || result.State == Gate3StateReady || approvals.creates != 0 {
		t.Fatalf("denied executor minted a decision: %+v %v", result, err)
	}
	authority.err = nil
	stopWorkflowDispatchFixture(t, fixture)
	if result, err := coordinator.Resolve(context.Background(), action); err == nil || result.State == Gate3StateReady {
		t.Fatalf("stopped executor became ready: %+v %v", result, err)
	}
}

func TestExecutorInitiationCapabilityHasSeparateExactShortLivedScope(t *testing.T) {
	coordinator, authority, approvals, _, action := executorCoordinatorFixture(t, api.RuntimeApprovalDecisionPending)
	controllerUID := "controller-uid"
	now, _ := coordinator.config.Clock()
	service, err := NewWorkflowExecutorInitiationService(WorkflowExecutorInitiationConfig{Keys: coordinator.config.Keys, Coordinator: coordinator,
		CurrentExecutor:      func(context.Context) (api.ControlPlaneRoleSubject, error) { return action.Executor, nil },
		CurrentControllerUID: func(context.Context) (string, error) { return controllerUID, nil }, Clock: func() (time.Time, error) { return now, nil }})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := service.Issue(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if capability.ExpiresAt.Sub(capability.IssuedAt) > time.Minute || capability.Binding.Domain == WorkflowExecutorBindingDomain || approvals.creates != 0 {
		t.Fatal("capability grants decisions or reuses an approval domain")
	}
	for name, mutate := range map[string]func(*WorkflowExecutorAction, *WorkflowExecutorInitiationCapability){
		"changed payload": func(action *WorkflowExecutorAction, _ *WorkflowExecutorInitiationCapability) {
			action.Payload = json.RawMessage(`{"changed":true}`)
		},
		"changed target": func(action *WorkflowExecutorAction, _ *WorkflowExecutorInitiationCapability) {
			action.Target.UID = "recreated"
		},
		"changed epoch": func(action *WorkflowExecutorAction, _ *WorkflowExecutorInitiationCapability) { action.OwnerEpoch++ },
		"changed invocation": func(action *WorkflowExecutorAction, _ *WorkflowExecutorInitiationCapability) {
			action.InvocationID = "other"
		},
		"approval domain": func(_ *WorkflowExecutorAction, capability *WorkflowExecutorInitiationCapability) {
			capability.Binding.Domain = WorkflowExecutorBindingDomain
		},
		"extended expiry": func(_ *WorkflowExecutorAction, capability *WorkflowExecutorInitiationCapability) {
			capability.ExpiresAt = capability.ExpiresAt.Add(time.Second)
		},
		"wrong controller": func(_ *WorkflowExecutorAction, capability *WorkflowExecutorInitiationCapability) {
			capability.ControllerUID = "other"
		},
		"expired": func(_ *WorkflowExecutorAction, capability *WorkflowExecutorInitiationCapability) {
			capability.ExpiresAt = now
		},
	} {
		t.Run(name, func(t *testing.T) {
			changedAction, changedCap := action, capability
			mutate(&changedAction, &changedCap)
			if _, err := service.Resolve(context.Background(), changedAction, changedCap); err == nil {
				t.Fatal("altered capability created initiation authority")
			}
		})
	}
	if approvals.creates != 0 {
		t.Fatal("invalid capabilities reached approval persistence")
	}
	result, err := service.Resolve(context.Background(), action, capability)
	if err != nil || result.State != Gate3StatePending {
		t.Fatalf("valid capability did not preserve pending approval: %+v %v", result, err)
	}
	replay, err := service.Resolve(context.Background(), action, capability)
	if err != nil || replay.State != Gate3StatePending || replay.Approval.UID != result.Approval.UID {
		t.Fatalf("exact capability replay changed decision ownership: %+v %v", replay, err)
	}
	authority.err = errors.New("binding revoked")
	if _, err := service.Resolve(context.Background(), action, capability); err == nil {
		t.Fatal("valid signature bypassed binding revocation")
	}
	authority.err = nil
	controllerUID = "recreated-controller"
	if _, err := service.Resolve(context.Background(), action, capability); err == nil {
		t.Fatal("copied capability survived a controller incarnation change")
	}
}

type executorAuthorityReader struct {
	authority  WorkflowExecutorAuthority
	acceptance WorkflowRunAcceptance
	err        error
	stepErr    error
	targetErr  error
	targetHook func()
}

func (reader *executorAuthorityReader) GetWorkflowExecutorAuthority(context.Context, api.RuntimeApprovalReference) (WorkflowExecutorAuthority, error) {
	return reader.authority, reader.err
}

func (reader *executorAuthorityReader) GetWorkflowRunAcceptance(context.Context, string, string) (WorkflowRunAcceptance, error) {
	return reader.acceptance, reader.err
}

func (reader *executorAuthorityReader) AuthorizeWorkflowExecutorTarget(context.Context, WorkflowExecutorAction) error {
	if reader.targetHook != nil {
		reader.targetHook()
	}
	return reader.targetErr
}

func (reader *executorAuthorityReader) AuthorizeWorkflowExecutorStep(context.Context, WorkflowExecutorAuthority, WorkflowExecutorAction) error {
	return reader.stepErr
}

func liveExecutorAuthorityFixture(t *testing.T) (*LiveWorkflowExecutorAuthorizer, *executorAuthorityReader, WorkflowExecutorAction) {
	t.Helper()
	action, _, now := executorActionFixture(t)
	reader := &executorAuthorityReader{
		authority: WorkflowExecutorAuthority{PolicyVersion: WorkflowExecutorPolicyVersion, Reference: action.ExecutionBinding, Workflow: action.Workflow,
			WorkflowRevisionDigest: action.WorkflowRevisionDigest, Executor: action.Executor, Enabled: true,
			NotBefore: now.Add(-time.Hour), ExpiresAt: action.ExpiresAt,
			ControllerUIDs: []string{"controller-uid"}, ApprovedBy: action.Initiator,
			Limits:  WorkflowExecutorLimits{MaxEffects: 8, MaxPayloadBytes: 32768, MaxDurationSeconds: 86400, MaxMembers: 8, MaxTurns: 100, MaxMessages: 500, MaxDelegations: 100, MaxHandoffDepth: 5, MaxParallelTasks: 8, ContentRetentionSeconds: 604800, ReplayRetentionSeconds: 7776000},
			Targets: []WorkflowExecutorTarget{{Agent: action.Target, Service: action.Service, Operation: WorkflowExecutorInitiate, InitialDecision: api.RuntimeApprovalDecisionNotRequired}}},
		acceptance: WorkflowRunAcceptance{Workspace: action.Activation.Workspace, RunUID: action.Activation.RunUID,
			Workflow: action.Workflow, WorkflowRevisionDigest: action.WorkflowRevisionDigest, ExecutionBinding: action.ExecutionBinding,
			Executor: action.Executor, Initiator: action.Initiator, BindingDigest: action.BindingDigest, AcceptedAt: now, ExpiresAt: action.ExpiresAt},
	}
	reader.acceptance.Run = api.RuntimeApprovalReference{Kind: "WorkflowRun", Scope: "Namespaced", Namespace: action.Activation.Workspace, Name: "run", UID: action.Activation.RunUID, ResourceVersion: 1}
	reader.acceptance.RequestDigest = strings.Repeat("b", 64)
	var digestErr error
	reader.acceptance.BindingDigest, digestErr = WorkflowRunExecutionScopeDigest(reader.acceptance, reader.authority)
	if digestErr != nil {
		t.Fatal(digestErr)
	}
	action.BindingDigest = reader.acceptance.BindingDigest
	authorizer, err := NewLiveWorkflowExecutorAuthorizer(WorkflowExecutorAuthorityConfig{Reader: reader,
		CurrentExecutor:      func(context.Context) (api.ControlPlaneRoleSubject, error) { return action.Executor, nil },
		CurrentControllerUID: func(context.Context) (string, error) { return "controller-uid", nil },
		Clock:                func() (time.Time, error) { return now, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return authorizer, reader, action
}

func TestExecutorAuthorityRequiresLiveProtectedEvidence(t *testing.T) {
	changes := map[string]func(*executorAuthorityReader, *WorkflowExecutorAction){
		"member preflight unavailable": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.stepErr = errors.New("members unavailable")
		},
		"revoked": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) { reader.authority.Enabled = false },
		"controller incarnation changed": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.ControllerUIDs = []string{"replacement-controller"}
		},
		"payload exceeds operator bound": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.Limits.MaxPayloadBytes = 1
		},
		"binding recreated": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.Reference.UID = "replacement"
		},
		"binding changed": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.Reference.ResourceVersion++
		},
		"workflow recreated": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.Workflow.UID = "replacement"
		},
		"workflow changed": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.Workflow.ResourceVersion++
		},
		"revision changed": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.WorkflowRevisionDigest = "other"
		},
		"wrong executor": func(_ *executorAuthorityReader, action *WorkflowExecutorAction) { action.Executor.Subject = "other" },
		"authority executor changed": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.Executor.Subject = "other"
		},
		"future authority": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.NotBefore = reader.acceptance.AcceptedAt.Add(time.Minute)
		},
		"authority expired": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.ExpiresAt = reader.acceptance.AcceptedAt
		},
		"unbounded authority": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.NotBefore = reader.authority.NotBefore.Add(-24 * time.Hour)
		},
		"no targets": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) { reader.authority.Targets = nil },
		"ambiguous target": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.Targets = append(reader.authority.Targets, reader.authority.Targets[0])
		},
		"unknown policy": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.Targets[0].InitialDecision = "unknown"
		},
		"wrong Agent": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.Targets[0].Agent.UID = "replacement"
		},
		"wrong service": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.authority.Targets[0].Service.ResourceVersion++
		},
		"foreign run": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) { reader.acceptance.RunUID = "other" },
		"foreign workspace": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.acceptance.Workspace = "other"
		},
		"acceptance Workflow changed": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.acceptance.Workflow.UID = "replacement"
		},
		"acceptance revision changed": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.acceptance.WorkflowRevisionDigest = "other"
		},
		"acceptance binding changed": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.acceptance.ExecutionBinding.ResourceVersion++
		},
		"acceptance executor changed": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.acceptance.Executor.Subject = "other"
		},
		"forged initiator": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.acceptance.Initiator.Subject = "other"
		},
		"scope changed": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.acceptance.BindingDigest = "other"
		},
		"future acceptance": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.acceptance.AcceptedAt = reader.acceptance.AcceptedAt.Add(time.Minute)
		},
		"acceptance before authority": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.acceptance.AcceptedAt = reader.authority.NotBefore.Add(-time.Minute)
		},
		"acceptance expired": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.acceptance.ExpiresAt = reader.acceptance.AcceptedAt
		},
		"acceptance extended": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.acceptance.ExpiresAt = reader.authority.ExpiresAt.Add(time.Minute)
		},
		"unavailable": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.err = errors.New("store unavailable")
		},
		"target unavailable": func(reader *executorAuthorityReader, _ *WorkflowExecutorAction) {
			reader.targetErr = errors.New("target unavailable")
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			authorizer, reader, action := liveExecutorAuthorityFixture(t)
			change(reader, &action)
			if decision, err := authorizer.AuthorizeWorkflowExecutorAction(context.Background(), action); err == nil || decision != "" {
				t.Fatalf("invalid evidence granted initiation: %q %v", decision, err)
			}
		})
	}
	for _, decision := range []string{api.RuntimeApprovalDecisionNotRequired, api.RuntimeApprovalDecisionPending} {
		authorizer, reader, action := liveExecutorAuthorityFixture(t)
		reader.authority.Targets[0].InitialDecision = decision
		reader.acceptance.BindingDigest, _ = WorkflowRunExecutionScopeDigest(reader.acceptance, reader.authority)
		action.BindingDigest = reader.acceptance.BindingDigest
		if actual, err := authorizer.AuthorizeWorkflowExecutorAction(context.Background(), action); err != nil || actual != decision {
			t.Fatalf("valid authority decision = %q %v, want %q", actual, err, decision)
		}
		action.Payload = nil
		if _, err := authorizer.AuthorizeWorkflowExecutorAction(context.Background(), action); err != nil {
			t.Fatalf("retained metadata requires transient payload: %v", err)
		}
	}
}

func TestExecutorApprovedLimitsRemainProtected(t *testing.T) {
	for _, scenario := range []string{"valid", "limits-during-authorization", "revoked", "member-denied"} {
		t.Run(scenario, func(t *testing.T) {
			authorizer, reader, action := liveExecutorAuthorityFixture(t)
			switch scenario {
			case "limits-during-authorization":
				reader.targetHook = func() { reader.authority.Limits.MaxMessages = 1 }
			case "revoked":
				reader.authority.Enabled = false
			case "member-denied":
				reader.stepErr = errors.New("member is no longer authorized")
			}
			limits, err := authorizer.ApprovedWorkflowExecutorLimits(t.Context(), action)
			if scenario == "valid" {
				if err != nil || limits.MaxMessages != 500 || limits.MaxPayloadBytes != 32768 {
					t.Fatalf("approved limits changed: %+v %v", limits, err)
				}
			} else if err == nil || limits != (WorkflowExecutorLimits{}) {
				t.Fatalf("unprotected limits escaped live authorization: %+v %v", limits, err)
			}
		})
	}
}

func TestExecutorRecordsRejectInvalidConfiguration(t *testing.T) {
	_, reader, action := liveExecutorAuthorityFixture(t)
	authority := reader.authority
	authority.ControllerUIDs = []string{"controller-uid"}
	authority.ApprovedBy = action.Initiator
	authority.Limits.MaxDurationSeconds = 3600
	authority.Targets[0].Operation = WorkflowExecutorInitiate
	raw, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeWorkflowExecutorAuthority(raw); err != nil {
		t.Fatalf("valid protected authority rejected: %v", err)
	}
	for _, field := range []string{"unexpected", "credentials", "serviceAccount"} {
		invalid := append([]byte(`{"`+field+`":true,`), raw[1:]...)
		if _, err := DecodeWorkflowExecutorAuthority(invalid); err == nil {
			t.Fatalf("unknown authority field %q accepted", field)
		}
	}
	for name, change := range map[string]func(*WorkflowExecutorAuthority){
		"unbounded effects":             func(value *WorkflowExecutorAuthority) { value.Limits.MaxEffects = 0 },
		"missing policy":                func(value *WorkflowExecutorAuthority) { value.PolicyVersion = "" },
		"unsupported policy":            func(value *WorkflowExecutorAuthority) { value.PolicyVersion = "future-policy" },
		"unbounded members":             func(value *WorkflowExecutorAuthority) { value.Limits.MaxMembers = 0 },
		"excess turns":                  func(value *WorkflowExecutorAuthority) { value.Limits.MaxTurns = 101 },
		"excess messages":               func(value *WorkflowExecutorAuthority) { value.Limits.MaxMessages = 501 },
		"excess delegations":            func(value *WorkflowExecutorAuthority) { value.Limits.MaxDelegations = 101 },
		"excess handoff depth":          func(value *WorkflowExecutorAuthority) { value.Limits.MaxHandoffDepth = 6 },
		"excess parallel tasks":         func(value *WorkflowExecutorAuthority) { value.Limits.MaxParallelTasks = 9 },
		"excess content retention":      func(value *WorkflowExecutorAuthority) { value.Limits.ContentRetentionSeconds = 604801 },
		"insufficient replay retention": func(value *WorkflowExecutorAuthority) { value.Limits.ReplayRetentionSeconds = 86400 },
		"unbounded duration":            func(value *WorkflowExecutorAuthority) { value.Limits.MaxDurationSeconds = 86401 },
		"executor self approval":        func(value *WorkflowExecutorAuthority) { value.ApprovedBy = value.Executor },
		"controller wildcard":           func(value *WorkflowExecutorAuthority) { value.ControllerUIDs = []string{"*"} },
		"unsupported operation":         func(value *WorkflowExecutorAuthority) { value.Targets[0].Operation = "invoke-any" },
		"duplicate target":              func(value *WorkflowExecutorAuthority) { value.Targets = append(value.Targets, value.Targets[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			value := authority
			value.Targets = append([]WorkflowExecutorTarget(nil), authority.Targets...)
			change(&value)
			if err := ValidateWorkflowExecutorAuthority(value); err == nil {
				t.Fatal("invalid authority accepted")
			}
		})
	}
	acceptance := reader.acceptance
	acceptance.Run = api.RuntimeApprovalReference{Kind: "WorkflowRun", Scope: "Namespaced", Namespace: acceptance.Workspace,
		Name: "run", UID: acceptance.RunUID, ResourceVersion: 1}
	acceptance.RequestDigest = strings.Repeat("b", 64)
	raw, err = json.Marshal(acceptance)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeWorkflowRunAcceptance(raw); err != nil {
		t.Fatalf("valid acceptance rejected: %v", err)
	}
	acceptance.RunUID = "recreated-run"
	if err := ValidateWorkflowRunAcceptance(acceptance); err == nil {
		t.Fatal("acceptance allowed a recreated run")
	}
}

func TestWorkflowRunAcceptanceRejectsAlreadyStartedDefinitions(t *testing.T) {
	_, reader, _ := liveExecutorAuthorityFixture(t)
	acceptance, authority := reader.acceptance, reader.authority
	workflow := api.Workflow{ObjectMeta: api.ObjectMeta{Name: acceptance.Workflow.Name, Namespace: acceptance.Workflow.Namespace, UID: acceptance.Workflow.UID, ResourceVersion: acceptance.Workflow.ResourceVersion}}
	run := api.WorkflowRun{ObjectMeta: api.ObjectMeta{Name: acceptance.Run.Name, Namespace: acceptance.Run.Namespace, UID: acceptance.Run.UID, ResourceVersion: acceptance.Run.ResourceVersion, Annotations: map[string]string{WorkflowRunAcceptanceRequiredAnnotation: "true"}}, Spec: api.WorkflowRunSpec{WorkflowRef: api.LocalObjectReference{Name: workflow.Name}}}
	workflowJSON, _ := json.Marshal(workflow)
	runJSON, _ := json.Marshal(run)
	acceptance.WorkflowRevisionDigest, _ = WorkflowExecutorSpecDigest(workflowJSON)
	authority.WorkflowRevisionDigest = acceptance.WorkflowRevisionDigest
	acceptance.RequestDigest, _ = WorkflowExecutorSpecDigest(runJSON)
	acceptance.BindingDigest, _ = WorkflowRunExecutionScopeDigest(acceptance, authority)
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{Method: acceptance.Initiator.Method, Issuer: acceptance.Initiator.Issuer, Subject: acceptance.Initiator.Subject})
	if err := ValidateWorkflowRunAcceptanceDefinitions(ctx, acceptance, authority, workflowJSON, runJSON); err != nil {
		t.Fatalf("untouched run rejected: %v", err)
	}
	for name, change := range map[string]func(*api.WorkflowRun){
		"running":       func(run *api.WorkflowRun) { run.Status.Phase = api.PhaseRunning },
		"started":       func(run *api.WorkflowRun) { run.Status.StartTime = &acceptance.AcceptedAt },
		"step progress": func(run *api.WorkflowRun) { run.Status.ActiveStep = "already-started" },
		"snapshot":      func(run *api.WorkflowRun) { run.Status.SnapshotSpec = &api.WorkflowSpec{} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := run
			change(&changed)
			changedJSON, _ := json.Marshal(changed)
			if err := ValidateWorkflowRunAcceptanceDefinitions(ctx, acceptance, authority, workflowJSON, changedJSON); err == nil {
				t.Fatal("acceptance retroactively authorized a started run")
			}
		})
	}
}

func TestExecutorAuthorityRejectsExpiryAndCancellationDuringReads(t *testing.T) {
	for _, failure := range []string{"expired", "canceled", "unhealthy clock", "clock rollback"} {
		t.Run(failure, func(t *testing.T) {
			authorizer, reader, action := liveExecutorAuthorityFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := reader.acceptance.AcceptedAt
			var clockErr error
			authorizer.config.Clock = func() (time.Time, error) { return now, clockErr }
			reader.targetHook = func() {
				switch failure {
				case "expired":
					now = action.ExpiresAt
				case "canceled":
					cancel()
				case "unhealthy clock":
					clockErr = errors.New("clock unavailable")
				case "clock rollback":
					now = now.Add(-time.Minute)
				}
			}
			if decision, err := authorizer.AuthorizeWorkflowExecutorAction(ctx, action); err == nil || decision != "" {
				t.Fatalf("authority survived %s during live reads: %q %v", failure, decision, err)
			}
		})
	}
}

type executorResourceReader struct {
	agent    api.Agent
	service  api.AgentService
	services []api.AgentService
	err      error
}

func (reader *executorResourceReader) ListWorkflowExecutorResources(_ context.Context, kind, namespace string) ([][]byte, error) {
	if reader.err != nil {
		return nil, reader.err
	}
	if kind != "AgentService" || namespace != reader.agent.Namespace {
		return nil, store.ErrNotFound
	}
	services := reader.services
	if services == nil {
		services = []api.AgentService{reader.service}
	}
	var result [][]byte
	for _, service := range services {
		raw, err := json.Marshal(service)
		if err != nil {
			return nil, err
		}
		result = append(result, raw)
	}
	return result, nil
}

func (reader *executorResourceReader) GetWorkflowExecutorResource(_ context.Context, kind, namespace, name string) ([]byte, error) {
	if reader.err != nil {
		return nil, reader.err
	}
	if kind == "Agent" && namespace == reader.agent.Namespace && name == reader.agent.Name {
		return json.Marshal(reader.agent)
	}
	if kind == "AgentService" && namespace == reader.service.Namespace && name == reader.service.Name {
		return json.Marshal(reader.service)
	}
	return nil, store.ErrNotFound
}

func TestExecutorTargetRequiresExactNativeReadyService(t *testing.T) {
	action, _, _ := executorActionFixture(t)
	fixture := func() *executorResourceReader {
		revision := int64(1)
		return &executorResourceReader{
			agent: api.Agent{ObjectMeta: api.ObjectMeta{Name: action.Target.Name, Namespace: action.Target.Namespace, UID: action.Target.UID, ResourceVersion: 1},
				Spec: api.AgentSpec{Runtime: &api.AgentRuntimeSpec{Class: "native"}}, Status: api.AgentStatus{Phase: api.PhaseReady, RevisionCount: 1, Runtime: &api.AgentRuntimeStatus{Ready: "True", ReadyReplicas: 1, AvailableReplicas: 1}}},
			service: api.AgentService{ObjectMeta: api.ObjectMeta{Name: action.Service.Name, Namespace: action.Service.Namespace, UID: action.Service.UID, ResourceVersion: 1},
				Spec:   api.AgentServiceSpec{AgentRef: api.LocalObjectReference{Name: action.Target.Name}, RevisionPin: &revision},
				Status: api.AgentServiceStatus{Phase: api.PhaseReady, ReadyInstances: 1, ActiveRevision: 1}},
		}
	}
	allow := func(context.Context, WorkflowExecutorAction) error { return nil }
	if err := AuthorizeWorkflowExecutorTarget(context.Background(), fixture(), action, allow); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*executorResourceReader){
		"Agent recreated":             func(reader *executorResourceReader) { reader.agent.UID = "other" },
		"Agent changed":               func(reader *executorResourceReader) { reader.agent.ResourceVersion++ },
		"Agent unready":               func(reader *executorResourceReader) { reader.agent.Status.Phase = api.PhasePending },
		"unsupported runtime":         func(reader *executorResourceReader) { reader.agent.Spec.Runtime.Class = "autogen" },
		"unqualified default runtime": func(reader *executorResourceReader) { reader.agent.Spec.Runtime = nil },
		"service recreated":           func(reader *executorResourceReader) { reader.service.UID = "other" },
		"service changed":             func(reader *executorResourceReader) { reader.service.ResourceVersion++ },
		"service unready":             func(reader *executorResourceReader) { reader.service.Status.Phase = api.PhasePending },
		"service drained":             func(reader *executorResourceReader) { reader.service.Status.ReadyInstances = 0 },
		"wrong route":                 func(reader *executorResourceReader) { reader.service.Spec.AgentRef.Name = "other" },
		"floating revision":           func(reader *executorResourceReader) { reader.service.Spec.RevisionPin = nil },
		"revision changed":            func(reader *executorResourceReader) { reader.service.Status.ActiveRevision++ },
		"unknown revision":            func(reader *executorResourceReader) { reader.agent.Status.RevisionCount = 0 },
		"unavailable":                 func(reader *executorResourceReader) { reader.err = errors.New("unavailable") },
	} {
		t.Run(name, func(t *testing.T) {
			reader := fixture()
			change(reader)
			if err := AuthorizeWorkflowExecutorTarget(context.Background(), reader, action, allow); err == nil {
				t.Fatal("invalid executor target allowed")
			}
		})
	}
	denied := errors.New("invoke denied")
	if err := AuthorizeWorkflowExecutorTarget(context.Background(), fixture(), action, func(context.Context, WorkflowExecutorAction) error { return denied }); !errors.Is(err, denied) {
		t.Fatalf("invocation denial lost: %v", err)
	}
}

type declaredCollaborationReader struct {
	WorkflowCollaborationResourceReader
	workflow []byte
	grants   [][]byte
}

func (reader *declaredCollaborationReader) GetWorkflowExecutorResource(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == "Workflow" {
		return reader.workflow, ctx.Err()
	}
	return reader.WorkflowCollaborationResourceReader.GetWorkflowExecutorResource(ctx, kind, namespace, name)
}

func (reader *declaredCollaborationReader) ListWorkflowExecutorResources(ctx context.Context, kind, namespace string) ([][]byte, error) {
	if kind == "CrossNamespaceGrant" {
		return reader.grants, ctx.Err()
	}
	return reader.WorkflowCollaborationResourceReader.ListWorkflowExecutorResources(ctx, kind, namespace)
}

func TestCollaborationMemberResolutionRequiresUniqueReadyPinnedAuthorizedService(t *testing.T) {
	revision := int64(2)
	fixture := func() *executorResourceReader {
		return &executorResourceReader{agent: api.Agent{ObjectMeta: api.ObjectMeta{Name: "analyst", Namespace: "workers", UID: "agent-uid", ResourceVersion: 2},
			Spec: api.AgentSpec{Runtime: &api.AgentRuntimeSpec{Class: "native"}}, Status: api.AgentStatus{Phase: api.PhaseReady, RevisionCount: 2, Runtime: &api.AgentRuntimeStatus{Ready: "True", ReadyReplicas: 1, AvailableReplicas: 1}}},
			service: api.AgentService{ObjectMeta: api.ObjectMeta{Name: "pinned", Namespace: "workers", UID: "service-uid", ResourceVersion: 3},
				Spec: api.AgentServiceSpec{AgentRef: api.LocalObjectReference{Name: "analyst"}, RevisionPin: &revision}, Status: api.AgentServiceStatus{Phase: api.PhaseReady, ReadyInstances: 1, ActiveRevision: 2}}}
	}
	member := api.CollaborationMember{Name: "research", AgentRef: api.AgentReference{Name: "analyst", Namespace: "workers"}}
	agentGrant, serviceGrant, invoke := true, true, true
	resolver := &WorkflowCollaborationMemberResolver{Reader: fixture(), AuthorizeReference: func(_ context.Context, workspace string, reference api.RuntimeApprovalReference) error {
		if workspace != "planning" || reference.Namespace != "workers" || reference.Kind == "Agent" && !agentGrant || reference.Kind == "AgentService" && !serviceGrant {
			return errors.New("reference denied")
		}
		return nil
	}, AuthorizeInvoke: func(_ context.Context, agent, service api.RuntimeApprovalReference) error {
		if !invoke {
			return errors.New("invoke denied")
		}
		return nil
	}}
	resolved, err := resolver.Resolve(context.Background(), "planning", member)
	if err != nil || resolved.Agent.UID != "agent-uid" || resolved.Service.UID != "service-uid" || resolved.Revision != 2 || resolved.Agent.ConfigurationDigest == "" || resolved.Service.ConfigurationDigest == "" {
		t.Fatalf("healthy member resolution = %+v %v", resolved, err)
	}
	_, authorityReader, _ := liveExecutorAuthorityFixture(t)
	authority := authorityReader.authority
	authority.Workflow.Namespace, authority.Reference.Namespace = "planning", "planning"
	authority.Targets = []WorkflowExecutorTarget{{Agent: resolved.Agent, Service: resolved.Service, Operation: WorkflowExecutorInitiate, InitialDecision: api.RuntimeApprovalDecisionPending}}
	if members, err := resolver.ResolveApprovedMembers(context.Background(), authority, []api.CollaborationMember{member}); err != nil || len(members) != 1 {
		t.Fatalf("approved member set rejected: %+v %v", members, err)
	}
	workflow := api.Workflow{ObjectMeta: api.ObjectMeta{Name: authority.Workflow.Name, Namespace: authority.Workflow.Namespace, UID: authority.Workflow.UID, ResourceVersion: authority.Workflow.ResourceVersion}, Spec: api.WorkflowSpec{Steps: []api.WorkflowStep{{Name: "team", Type: api.WorkflowStepTypeCollaboration, Collaboration: &api.WorkflowCollaboration{Members: []api.CollaborationMember{member}, Limits: api.CollaborationLimits{MaxTurns: 10, MaxMessages: 20, MaxDelegations: 10, MaxDelegationDepth: 2, MaxParallelTasks: 1, MaxTokens: 1000}}}}}}
	workflowJSON, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	authority.WorkflowRevisionDigest, err = WorkflowExecutorSpecDigest(workflowJSON)
	if err != nil {
		t.Fatal(err)
	}
	if members, err := resolver.ResolveWorkflowStep(context.Background(), authority, workflowJSON, "team"); err != nil || len(members) != 1 || members[0].Name != "research" {
		t.Fatalf("pinned Workflow member resolution: %+v %v", members, err)
	}
	t.Run("composed-step-target-subset", func(t *testing.T) {
		composed := workflow
		otherMember := member
		otherMember.Name, otherMember.AgentRef.Name = "review", "reviewer"
		composed.Spec.Steps = append(append([]api.WorkflowStep(nil), workflow.Spec.Steps...), api.WorkflowStep{
			Name: "review-team", Type: api.WorkflowStepTypeCollaboration,
			Collaboration: &api.WorkflowCollaboration{Members: []api.CollaborationMember{otherMember}, Limits: workflow.Spec.Steps[0].Collaboration.Limits},
		})
		composedJSON, err := json.Marshal(composed)
		if err != nil {
			t.Fatal(err)
		}
		composedAuthority := authority
		composedAuthority.WorkflowRevisionDigest, err = WorkflowExecutorSpecDigest(composedJSON)
		if err != nil {
			t.Fatal(err)
		}
		otherTarget := authority.Targets[0]
		otherTarget.Agent.Name, otherTarget.Agent.UID = "reviewer", "reviewer-uid"
		otherTarget.Service.Name, otherTarget.Service.UID = "reviewer-service", "reviewer-service-uid"
		composedAuthority.Targets = append(append([]WorkflowExecutorTarget(nil), authority.Targets...), otherTarget)
		composedAuthority.Limits.MaxMembers = 2
		composedAuthority.Limits.MaxParallelTasks = 1
		if err := ValidateWorkflowExecutorAuthority(composedAuthority); err != nil {
			t.Fatalf("composed-step fixture exceeds the operator's finite policy: %v", err)
		}
		members, err := resolver.ResolveWorkflowStep(context.Background(), composedAuthority, composedJSON, "team")
		if err != nil || len(members) != 1 || members[0].Agent != resolved.Agent || members[0].Service != resolved.Service {
			t.Fatalf("binding targets from another declared step prevented this step's exact member resolution: %+v %v", members, err)
		}
		reader := &declaredCollaborationReader{WorkflowCollaborationResourceReader: fixture(), workflow: composedJSON,
			grants: [][]byte{[]byte(`{"metadata":{"name":"members","namespace":"workers","uid":"grant-uid","resourceVersion":1},"spec":{"from":"workers","to":"planning","resources":[{"kind":"Agent","name":"analyst"},{"kind":"AgentService","name":"pinned"}]}}`)}}
		action, _, _ := executorActionFixture(t)
		action.Activation.Step = "team"
		action.Target, action.Service = resolved.Agent, resolved.Service
		authorizeTarget := func(_ context.Context, targetAction WorkflowExecutorAction) error {
			if targetAction.Target != resolved.Agent || targetAction.Service != resolved.Service {
				t.Fatal("selected-step resolution invoked authority for another step's member")
			}
			return nil
		}
		if err := AuthorizeWorkflowExecutorStep(context.Background(), reader, composedAuthority, action, authorizeTarget); err != nil {
			t.Fatalf("exact selected-step target rejected: %v", err)
		}
		action.Target, action.Service = otherTarget.Agent, otherTarget.Service
		if err := AuthorizeWorkflowExecutorStep(context.Background(), reader, composedAuthority, action, authorizeTarget); err == nil || !strings.Contains(err.Error(), "not in the approved collaboration step") {
			t.Fatalf("Workflow-wide authority widened this step's membership: %v", err)
		}
	})
	if _, err := resolver.ResolveWorkflowStep(context.Background(), authority, workflowJSON, "undeclared"); err == nil {
		t.Fatal("undeclared Workflow step resolved")
	}
	workflow.Spec.Steps[0].Collaboration.Limits.MaxMessages = 501
	changedJSON, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveWorkflowStep(context.Background(), authority, changedJSON, "team"); err == nil {
		t.Fatal("changed Workflow escaped its configuration pin")
	}
	changedPolicy := authority
	changedPolicy.WorkflowRevisionDigest, err = WorkflowExecutorSpecDigest(changedJSON)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveWorkflowStep(context.Background(), changedPolicy, changedJSON, "team"); err == nil {
		t.Fatal("Workflow-expanded message budget escaped operator bounds")
	}
	changedAuthority := authority
	changedAuthority.Targets = append([]WorkflowExecutorTarget(nil), authority.Targets...)
	changedAuthority.Targets[0].Service.ConfigurationDigest = strings.Repeat("d", 64)
	if _, err := resolver.ResolveApprovedMembers(context.Background(), changedAuthority, []api.CollaborationMember{member}); err == nil {
		t.Fatal("resolved service escaped the approved configuration pin")
	}
	if _, err := resolver.ResolveApprovedMembers(context.Background(), authority, []api.CollaborationMember{member, member}); err == nil {
		t.Fatal("duplicated member set accepted")
	}
	if _, err := resolver.ResolveApprovedMembers(context.Background(), authority, nil); err == nil {
		t.Fatal("empty member set accepted")
	}
	action, _, _ := executorActionFixture(t)
	action.Target, action.Service = resolved.Agent, resolved.Service
	allowTarget := func(context.Context, WorkflowExecutorAction) error { return nil }
	if err := AuthorizeWorkflowExecutorTarget(context.Background(), resolver.Reader, action, allowTarget); err != nil {
		t.Fatalf("resolved target cannot enter invocation validation: %v", err)
	}
	action.Service.ConfigurationDigest = strings.Repeat("d", 64)
	if err := AuthorizeWorkflowExecutorTarget(context.Background(), resolver.Reader, action, allowTarget); err == nil {
		t.Fatal("forged live target configuration digest accepted")
	}
	for name, change := range map[string]func(*executorResourceReader){
		"no eligible service": func(reader *executorResourceReader) { reader.services = []api.AgentService{} },
		"ambiguous services": func(reader *executorResourceReader) {
			other := reader.service
			other.Name, other.UID = "other", "other-uid"
			reader.services = []api.AgentService{reader.service, other}
		},
		"floating service":      func(reader *executorResourceReader) { reader.service.Spec.RevisionPin = nil },
		"runtime unready":       func(reader *executorResourceReader) { reader.agent.Status.Runtime.Ready = "False" },
		"unsupported runtime":   func(reader *executorResourceReader) { reader.agent.Spec.Runtime.Class = "autogen" },
		"missing runtime proof": func(reader *executorResourceReader) { reader.agent.Status.Runtime = nil },
		"unknown revision":      func(reader *executorResourceReader) { reader.agent.Status.RevisionCount = 1 },
		"drained service":       func(reader *executorResourceReader) { reader.service.Status.ReadyInstances = 0 },
		"deleting service":      func(reader *executorResourceReader) { now := time.Now(); reader.service.DeletionTimestamp = &now },
	} {
		t.Run(name, func(t *testing.T) {
			reader := fixture()
			change(reader)
			resolver.Reader = reader
			if _, err := resolver.Resolve(context.Background(), "planning", member); err == nil {
				t.Fatal("invalid member resolved")
			}
		})
	}
	resolver.Reader = fixture()
	for _, permission := range []*bool{&agentGrant, &serviceGrant, &invoke} {
		*permission = false
		if _, err := resolver.Resolve(context.Background(), "planning", member); err == nil {
			t.Fatal("membership replaced independent authorization")
		}
		*permission = true
	}
	reader := fixture()
	other := reader.service
	other.Name, other.UID = "other", "other-uid"
	reader.services = []api.AgentService{reader.service, other}
	resolver.Reader = reader
	member.ServiceRef = &api.LocalObjectReference{Name: "pinned"}
	if resolved, err := resolver.Resolve(context.Background(), "planning", member); err != nil || resolved.Service.Name != "pinned" {
		t.Fatalf("explicit pinned service did not remove discovery ambiguity: %+v %v", resolved, err)
	}
}
