// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type workflowDispatchFixtureStore struct {
	mutex      sync.Mutex
	activation store.WorkflowActivation
	approval   api.RuntimeApproval
	now        time.Time
	readErr    error
	beforeCAS  func(store.WorkflowActivationMutation)
	afterCAS   func(store.WorkflowActivationMutation)
	commitErr  error
}

func (fixture *workflowDispatchFixtureStore) GetRuntimeApprovalByInvocation(_ context.Context, workspace, invocationID string) (api.RuntimeApproval, error) {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	if fixture.readErr != nil {
		return api.RuntimeApproval{}, fixture.readErr
	}
	if workspace != fixture.approval.Spec.Workspace || invocationID != fixture.approval.Spec.InvocationID {
		return api.RuntimeApproval{}, store.ErrNotFound
	}
	return fixture.approval, nil
}

func (fixture *workflowDispatchFixtureStore) CreateWorkflowActivation(_ context.Context, spec store.WorkflowActivationSpec) (store.WorkflowActivation, error) {
	return store.NewWorkflowActivation(spec, fixture.now)
}

func (fixture *workflowDispatchFixtureStore) GetWorkflowActivation(_ context.Context, key store.WorkflowActivationKey) (store.WorkflowActivation, error) {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	if key != fixture.activation.Spec.Key {
		return store.WorkflowActivation{}, store.ErrNotFound
	}
	return fixture.activation, nil
}

func (fixture *workflowDispatchFixtureStore) MutateWorkflowActivation(_ context.Context, key store.WorkflowActivationKey, mutation store.WorkflowActivationMutation) (store.WorkflowActivationResult, error) {
	if fixture.beforeCAS != nil {
		fixture.beforeCAS(mutation)
	}
	fixture.mutex.Lock()
	if key != fixture.activation.Spec.Key {
		fixture.mutex.Unlock()
		return store.WorkflowActivationResult{}, store.ErrNotFound
	}
	result, err := store.ApplyWorkflowActivation(fixture.activation, mutation, fixture.now)
	if err == nil {
		fixture.activation = result.Activation
	}
	fixture.mutex.Unlock()
	if err == nil && fixture.afterCAS != nil {
		fixture.afterCAS(mutation)
	}
	if err == nil && mutation.Kind == store.WorkflowGateCommit && fixture.commitErr != nil {
		return store.WorkflowActivationResult{}, fixture.commitErr
	}
	return result, err
}

func workflowDispatchFixture(t *testing.T, plane string) (*WorkflowDispatchGate, *workflowDispatchFixtureStore, api.RuntimeApproval, json.RawMessage) {
	t.Helper()
	_, keys, now := executorActionFixture(t)
	action := bindingAction(`{"a":1}`)
	action.Plane = plane
	action.WorkflowContext = &api.WorkflowActionContext{Workspace: "origin", RunUID: "run-uid", Step: "team",
		ActivationID: "visit-1", Attempt: 1, OwnerEpoch: 1, BindingDigest: strings.Repeat("a", 64)}
	binding, err := Bind(context.Background(), keys, action)
	if err != nil {
		t.Fatal(err)
	}
	approval := approvalFromAction(action, binding)
	approval.Spec.WorkflowContext = cloneWorkflowActionContext(action.WorkflowContext)
	approval.UID = "approval-uid"
	approval.Spec.ExpiresAt = now.Add(time.Hour)
	approval.Status = api.RuntimeApprovalStatus{DecisionState: api.RuntimeApprovalDecisionApproved, DecisionEvidenceDelivered: true,
		DecisionAudit: &api.RuntimeApprovalDecisionAudit{ID: "decision-evidence"}, ExecutionState: api.RuntimeApprovalExecutionClaimed,
		FencingToken: 1, Claimant: &api.RuntimeApprovalClaimant{WorkloadUID: "gateway-uid", ProcessStartNonce: "process-1", ClaimedAt: now}}
	actor, err := WorkflowApprovalActorDigest(approval)
	if err != nil {
		t.Fatal(err)
	}
	activation, err := store.NewWorkflowActivation(store.WorkflowActivationSpec{Key: workflowContextKey(*action.WorkflowContext),
		BindingDigest: action.WorkflowContext.BindingDigest, Actors: []string{actor}, MaxEffects: 4, MaxBytes: 32768, ExpiresAt: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &workflowDispatchFixtureStore{approval: approval, activation: activation, now: now}
	gate, err := NewWorkflowDispatchGate(WorkflowDispatchConfig{Activations: fixture, Approvals: fixture, Keys: keys,
		Clock: func() (time.Time, error) { return now, nil }, Eligibility: func(context.Context, api.RuntimeApproval) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return gate, fixture, approval, action.Payload
}

func stopWorkflowDispatchFixture(t *testing.T, fixture *workflowDispatchFixtureStore) {
	t.Helper()
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	result, err := store.ApplyWorkflowActivation(fixture.activation, store.WorkflowActivationMutation{
		ExpectedVersion: fixture.activation.Version, Kind: store.WorkflowGateStop, StopID: "cancel-1", StopMode: store.WorkflowGateStopping,
	}, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	fixture.activation = result.Activation
}

func TestWorkflowDispatchClaimAndStopBoundary(t *testing.T) {
	for _, plane := range []string{api.RuntimeApprovalPlaneTool, api.RuntimeApprovalPlaneMemory, api.RuntimeApprovalPlaneCommunication} {
		t.Run(plane, func(t *testing.T) {
			gate, fixture, approval, payload := workflowDispatchFixture(t, plane)
			if err := gate.Register(context.Background(), approval, payload); err != nil {
				t.Fatal(err)
			}
			stopWorkflowDispatchFixture(t, fixture)
			calls := 0
			result, err := gate.Dispatch(context.Background(), approval, payload, func(context.Context) error { calls++; return nil })
			if err == nil || result.DispatchCommitted || result.TargetCalled || calls != 0 {
				t.Fatalf("claimed operation dispatched after stop: %+v %v calls=%d", result, err, calls)
			}
		})
	}
}

func TestWorkflowDispatchExpiryDuringEligibilityBlocksTarget(t *testing.T) {
	for _, expireOnCall := range []int{1, 2} {
		t.Run(fmt.Sprintf("eligibility-%d", expireOnCall), func(t *testing.T) {
			gate, _, approval, payload := workflowDispatchFixture(t, api.RuntimeApprovalPlaneTool)
			if err := gate.Register(context.Background(), approval, payload); err != nil {
				t.Fatal(err)
			}
			now := approval.Spec.ExpiresAt.Add(-time.Minute)
			gate.config.Clock = func() (time.Time, error) { return now, nil }
			checks := 0
			gate.config.Eligibility = func(context.Context, api.RuntimeApproval) error {
				checks++
				if checks == expireOnCall {
					now = approval.Spec.ExpiresAt
				}
				return nil
			}
			calls := 0
			result, err := gate.Dispatch(context.Background(), approval, payload, func(context.Context) error { calls++; return nil })
			if err == nil || result.TargetCalled || calls != 0 || result.DispatchCommitted != (expireOnCall == 2) {
				t.Fatalf("expired authorization dispatched: %+v %v checks=%d calls=%d", result, err, checks, calls)
			}
		})
	}
}

func TestWorkflowDispatchExpiredClaimLeaseBlocksTarget(t *testing.T) {
	for _, test := range []struct {
		name   string
		offset time.Duration
		valid  bool
	}{
		{name: "expired", offset: -time.Minute},
		{name: "at deadline"},
		{name: "valid", offset: time.Minute, valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate, fixture, approval, payload := workflowDispatchFixture(t, api.RuntimeApprovalPlaneTool)
			if err := gate.Register(context.Background(), approval, payload); err != nil {
				t.Fatal(err)
			}
			deadline := fixture.now.Add(test.offset)
			claimant := *approval.Status.Claimant
			claimant.ClaimedAt = fixture.now.Add(-2 * time.Minute)
			claimant.LastHeartbeat = fixture.now.Add(-90 * time.Second)
			claimant.LeaseDeadline = &deadline
			approval.Status.Claimant = &claimant
			fixture.approval = approval
			calls := 0
			result, err := gate.Dispatch(context.Background(), approval, payload, func(context.Context) error { calls++; return nil })
			if test.valid {
				if err != nil || !result.DispatchCommitted || !result.TargetCalled || calls != 1 {
					t.Fatalf("valid claimant lease rejected: %+v %v calls=%d", result, err, calls)
				}
			} else if err == nil || result.DispatchCommitted || result.TargetCalled || calls != 0 {
				t.Fatalf("expired claimant lease dispatched: %+v %v calls=%d", result, err, calls)
			}
		})
	}
}

func TestWorkflowDispatchConcurrentRequestsCallTargetOnce(t *testing.T) {
	gate, _, approval, payload := workflowDispatchFixture(t, api.RuntimeApprovalPlaneTool)
	if err := gate.Register(context.Background(), approval, payload); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	var wait sync.WaitGroup
	start := make(chan struct{})
	for range 16 {
		wait.Go(func() {
			<-start
			_, _ = gate.Dispatch(context.Background(), approval, payload, func(context.Context) error { calls.Add(1); return nil })
		})
	}
	close(start)
	wait.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent exact claim made %d target calls", calls.Load())
	}
}

func TestWorkflowDispatchLostCommitAcknowledgementNeverReplays(t *testing.T) {
	gate, fixture, approval, payload := workflowDispatchFixture(t, api.RuntimeApprovalPlaneTool)
	if err := gate.Register(context.Background(), approval, payload); err != nil {
		t.Fatal(err)
	}
	fixture.commitErr = errors.New("lost acknowledgement")
	calls := 0
	target := func(context.Context) error { calls++; return nil }
	if result, err := gate.Dispatch(context.Background(), approval, payload, target); err == nil || result.TargetCalled || calls != 0 {
		t.Fatalf("lost acknowledgement contacted target: %+v %v", result, err)
	}
	fixture.commitErr = nil
	if result, err := gate.Dispatch(context.Background(), approval, payload, target); err == nil || result.TargetCalled || calls != 0 {
		t.Fatalf("retained admission granted replay: %+v %v", result, err)
	}
}

func TestWorkflowDispatchRejectsInvalidClaimAndChangedAction(t *testing.T) {
	for name, change := range map[string]func(*api.RuntimeApproval){
		"pending decision": func(approval *api.RuntimeApproval) {
			approval.Status.DecisionState = api.RuntimeApprovalDecisionPending
		},
		"rejected decision": func(approval *api.RuntimeApproval) {
			approval.Status.DecisionState = api.RuntimeApprovalDecisionRejected
		},
		"undelivered audit": func(approval *api.RuntimeApproval) { approval.Status.DecisionEvidenceDelivered = false },
		"unclaimed": func(approval *api.RuntimeApproval) {
			approval.Status.ExecutionState = api.RuntimeApprovalExecutionNotStarted
		},
		"uncertain": func(approval *api.RuntimeApproval) {
			approval.Status.ExecutionState = api.RuntimeApprovalExecutionUncertain
		},
		"no owner": func(approval *api.RuntimeApproval) { approval.Status.Claimant = nil },
		"wrong owner": func(approval *api.RuntimeApproval) {
			owner := *approval.Status.Claimant
			owner.WorkloadUID = "other-gateway"
			approval.Status.Claimant = &owner
		},
		"wrong process": func(approval *api.RuntimeApproval) {
			owner := *approval.Status.Claimant
			owner.ProcessStartNonce = "other-process"
			approval.Status.Claimant = &owner
		},
		"wrong fence":      func(approval *api.RuntimeApproval) { approval.Status.FencingToken++ },
		"wrong target":     func(approval *api.RuntimeApproval) { approval.Spec.TargetRef.UID = "other-target" },
		"wrong uid":        func(approval *api.RuntimeApproval) { approval.UID = "other-approval" },
		"stripped context": func(approval *api.RuntimeApproval) { approval.Spec.WorkflowContext = nil },
		"changed activation": func(approval *api.RuntimeApproval) {
			context := *approval.Spec.WorkflowContext
			context.ActivationID = "visit-2"
			approval.Spec.WorkflowContext = &context
		},
	} {
		t.Run(name, func(t *testing.T) {
			gate, _, approval, payload := workflowDispatchFixture(t, api.RuntimeApprovalPlaneTool)
			if err := gate.Register(context.Background(), approval, payload); err != nil {
				t.Fatal(err)
			}
			changed := approval
			change(&changed)
			if name == "pending decision" || name == "rejected decision" || name == "undelivered audit" || name == "unclaimed" || name == "uncertain" || name == "no owner" {
				fixture := gate.config.Approvals.(*workflowDispatchFixtureStore)
				fixture.approval = changed
			}
			calls := 0
			result, err := gate.Dispatch(context.Background(), changed, payload, func(context.Context) error { calls++; return nil })
			if err == nil || result.TargetCalled || calls != 0 {
				t.Fatalf("invalid %s reached target: %+v %v calls=%d", name, result, err, calls)
			}
		})
	}
}

func TestWorkflowDispatchRechecksEligibilityAfterCommit(t *testing.T) {
	gate, fixture, approval, payload := workflowDispatchFixture(t, api.RuntimeApprovalPlaneTool)
	if err := gate.Register(context.Background(), approval, payload); err != nil {
		t.Fatal(err)
	}
	revoked := false
	fixture.afterCAS = func(mutation store.WorkflowActivationMutation) {
		if mutation.Kind == store.WorkflowGateCommit {
			revoked = true
		}
	}
	gate.config.Eligibility = func(context.Context, api.RuntimeApproval) error {
		if revoked {
			return errors.New("execution authority revoked")
		}
		return nil
	}
	calls := 0
	result, err := gate.Dispatch(context.Background(), approval, payload, func(context.Context) error { calls++; return nil })
	if err == nil || !result.DispatchCommitted || result.TargetCalled || calls != 0 {
		t.Fatalf("post-commit revocation ignored: %+v %v calls=%d", result, err, calls)
	}
}

func TestWorkflowDispatchBarrierStopBeforeCommit(t *testing.T) {
	gate, fixture, approval, payload := workflowDispatchFixture(t, api.RuntimeApprovalPlaneTool)
	if err := gate.Register(context.Background(), approval, payload); err != nil {
		t.Fatal(err)
	}
	fixture.beforeCAS = func(mutation store.WorkflowActivationMutation) {
		if mutation.Kind == store.WorkflowGateCommit {
			stopWorkflowDispatchFixture(t, fixture)
		}
	}
	calls := 0
	result, err := gate.Dispatch(context.Background(), approval, payload, func(context.Context) error { calls++; return nil })
	if err == nil || result.DispatchCommitted || result.TargetCalled || calls != 0 {
		t.Fatalf("stop lost at final CAS boundary: %+v %v calls=%d", result, err, calls)
	}
}

func TestWorkflowDispatchMissingQualificationFailsClosed(t *testing.T) {
	if _, err := NewWorkflowDispatchGate(WorkflowDispatchConfig{}); !errors.Is(err, ErrWorkflowDispatchUnsupported) {
		t.Fatalf("unqualified gate constructor = %v", err)
	}
	var gate *WorkflowDispatchGate
	if result, err := gate.Dispatch(context.Background(), api.RuntimeApproval{}, nil, func(context.Context) error { t.Fatal("unbound target called"); return nil }); !errors.Is(err, ErrWorkflowDispatchUnsupported) || result.TargetCalled {
		t.Fatalf("unbound operation = %+v %v", result, err)
	}
}

func TestWorkflowDispatchCommitBeforeStopRemainsInFlight(t *testing.T) {
	gate, fixture, approval, payload := workflowDispatchFixture(t, api.RuntimeApprovalPlaneTool)
	if err := gate.Register(context.Background(), approval, payload); err != nil {
		t.Fatal(err)
	}
	fixture.afterCAS = func(mutation store.WorkflowActivationMutation) {
		if mutation.Kind == store.WorkflowGateCommit {
			stopWorkflowDispatchFixture(t, fixture)
		}
	}
	calls := 0
	result, err := gate.Dispatch(context.Background(), approval, payload, func(context.Context) error { calls++; return nil })
	if err != nil || !result.DispatchCommitted || !result.TargetCalled || calls != 1 {
		t.Fatalf("committed in-flight operation discarded: %+v %v calls=%d", result, err, calls)
	}
	activation := fixture.activation
	if _, err := fixture.MutateWorkflowActivation(context.Background(), activation.Spec.Key, store.WorkflowActivationMutation{
		ExpectedVersion: activation.Version, Kind: store.WorkflowGateDrain, StopID: "cancel-1",
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("in-flight operation was reported drained: %v", err)
	}
}

func TestWorkflowDispatchPendingApprovalAfterStopMakesNoCall(t *testing.T) {
	gate, fixture, approval, payload := workflowDispatchFixture(t, api.RuntimeApprovalPlaneTool)
	fixture.approval.Status.DecisionState = api.RuntimeApprovalDecisionPending
	fixture.approval.Status.DecisionEvidenceDelivered = false
	fixture.approval.Status.ExecutionState = api.RuntimeApprovalExecutionNotStarted
	fixture.approval.Status.Claimant = nil
	if err := gate.Register(context.Background(), fixture.approval, payload); err != nil {
		t.Fatal(err)
	}
	stopWorkflowDispatchFixture(t, fixture)
	fixture.approval = approval
	calls := 0
	result, err := gate.Dispatch(context.Background(), approval, payload, func(context.Context) error { calls++; return nil })
	if err == nil || result.DispatchCommitted || result.TargetCalled || calls != 0 {
		t.Fatalf("late approval revived stopped work: %+v %v calls=%d", result, err, calls)
	}
}

func TestWorkflowDispatchClaimantChangesAfterCommitMakeNoCall(t *testing.T) {
	gate, fixture, approval, payload := workflowDispatchFixture(t, api.RuntimeApprovalPlaneTool)
	if err := gate.Register(context.Background(), approval, payload); err != nil {
		t.Fatal(err)
	}
	fixture.afterCAS = func(mutation store.WorkflowActivationMutation) {
		if mutation.Kind == store.WorkflowGateCommit {
			owner := *fixture.approval.Status.Claimant
			owner.ProcessStartNonce = "replacement-process"
			fixture.approval.Status.Claimant = &owner
		}
	}
	result, err := gate.Dispatch(context.Background(), approval, payload, func(context.Context) error { t.Fatal("changed claimant reached target"); return nil })
	if err == nil || !result.DispatchCommitted || result.TargetCalled {
		t.Fatalf("changed claimant retained execution right: %+v %v", result, err)
	}
}

func TestWorkflowDispatchTargetErrorDoesNotGrantReplay(t *testing.T) {
	gate, _, approval, payload := workflowDispatchFixture(t, api.RuntimeApprovalPlaneTool)
	if err := gate.Register(context.Background(), approval, payload); err != nil {
		t.Fatal(err)
	}
	calls := 0
	targetErr := errors.New("ambiguous target acknowledgement")
	target := func(context.Context) error { calls++; return targetErr }
	result, err := gate.Dispatch(context.Background(), approval, payload, target)
	if !errors.Is(err, targetErr) || !result.DispatchCommitted || !result.TargetCalled || calls != 1 {
		t.Fatalf("target uncertainty was lost: %+v %v calls=%d", result, err, calls)
	}
	result, err = gate.Dispatch(context.Background(), approval, payload, target)
	if err == nil || result.TargetCalled || calls != 1 {
		t.Fatalf("ambiguous target was replayed: %+v %v calls=%d", result, err, calls)
	}
}

func TestWorkflowActorDigestUsesImmutableIdentityAndSession(t *testing.T) {
	_, _, approval, _ := workflowDispatchFixture(t, api.RuntimeApprovalPlaneTool)
	original, err := WorkflowApprovalActorDigest(approval)
	if err != nil {
		t.Fatal(err)
	}
	changed := approval
	changed.Spec.AgentRef.ResourceVersion++
	changed.Spec.AgentIdentityRef.ResourceVersion++
	changed.Spec.AgentInstanceRef.ResourceVersion++
	changed.Spec.AgentInstanceRef.ConfigurationDigest = strings.Repeat("b", 64)
	actual, err := WorkflowApprovalActorDigest(changed)
	if err != nil || actual != original {
		t.Fatalf("status/configuration revision changed actor membership: %q != %q, %v", actual, original, err)
	}
	changed.Spec.AgentInstanceRef.UID = "replacement-instance"
	actual, err = WorkflowApprovalActorDigest(changed)
	if err != nil || actual == original {
		t.Fatalf("replacement instance inherited actor membership: %q, %v", actual, err)
	}
	changed = approval
	changed.Spec.SessionRef = "replacement-session"
	actual, err = WorkflowApprovalActorDigest(changed)
	if err != nil || actual == original {
		t.Fatalf("replacement session inherited actor membership: %q, %v", actual, err)
	}
}

func TestWorkflowDispatchDependenciesFailClosed(t *testing.T) {
	for name, change := range map[string]func(*WorkflowDispatchGate, *workflowDispatchFixtureStore){
		"unavailable approval": func(_ *WorkflowDispatchGate, fixture *workflowDispatchFixtureStore) {
			fixture.readErr = errors.New("approval unavailable")
		},
		"unhealthy clock": func(gate *WorkflowDispatchGate, _ *workflowDispatchFixtureStore) {
			gate.config.Clock = func() (time.Time, error) { return time.Time{}, errors.New("clock unhealthy") }
		},
		"zero clock": func(gate *WorkflowDispatchGate, _ *workflowDispatchFixtureStore) {
			gate.config.Clock = func() (time.Time, error) { return time.Time{}, nil }
		},
		"expired authority": func(_ *WorkflowDispatchGate, fixture *workflowDispatchFixtureStore) {
			fixture.approval.Spec.ExpiresAt = fixture.now
		},
		"revoked eligibility": func(gate *WorkflowDispatchGate, _ *workflowDispatchFixtureStore) {
			gate.config.Eligibility = func(context.Context, api.RuntimeApproval) error { return errors.New("authority revoked") }
		},
		"unavailable binding keys": func(gate *WorkflowDispatchGate, _ *workflowDispatchFixtureStore) { gate.config.Keys = brokenKeys{} },
		"wrong gate binding": func(_ *WorkflowDispatchGate, fixture *workflowDispatchFixtureStore) {
			fixture.activation.Spec.BindingDigest = strings.Repeat("b", 64)
		},
		"missing registration": func(_ *WorkflowDispatchGate, fixture *workflowDispatchFixtureStore) {
			fixture.activation.Effects = map[string]store.WorkflowEffect{}
		},
		"missing decision audit": func(_ *WorkflowDispatchGate, fixture *workflowDispatchFixtureStore) {
			fixture.approval.Status.DecisionAudit = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			gate, fixture, approval, payload := workflowDispatchFixture(t, api.RuntimeApprovalPlaneTool)
			if err := gate.Register(context.Background(), approval, payload); err != nil {
				t.Fatal(err)
			}
			change(gate, fixture)
			result, err := gate.Dispatch(context.Background(), approval, payload, func(context.Context) error { t.Fatal("unavailable dependency reached target"); return nil })
			if err == nil || result.TargetCalled {
				t.Fatalf("%s permitted target call: %+v %v", name, result, err)
			}
		})
	}
}

func TestWorkflowDispatchCancelledContextAndChangedPayloadMakeNoCall(t *testing.T) {
	gate, fixture, approval, payload := workflowDispatchFixture(t, api.RuntimeApprovalPlaneTool)
	if err := gate.Register(context.Background(), approval, payload); err != nil {
		t.Fatal(err)
	}
	target := func(context.Context) error { t.Fatal("cancelled or changed action reached target"); return nil }
	if result, err := gate.Dispatch(context.Background(), approval, json.RawMessage(`{"a":2}`), target); err == nil || result.TargetCalled {
		t.Fatalf("changed payload admitted: %+v %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result, err := gate.Dispatch(ctx, approval, payload, target); !errors.Is(err, context.Canceled) || result.TargetCalled {
		t.Fatalf("cancelled request admitted: %+v %v", result, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	fixture.afterCAS = func(mutation store.WorkflowActivationMutation) {
		if mutation.Kind == store.WorkflowGateCommit {
			cancel()
		}
	}
	if result, err := gate.Dispatch(ctx, approval, payload, target); !errors.Is(err, context.Canceled) || !result.DispatchCommitted || result.TargetCalled {
		t.Fatalf("post-commit cancellation discarded state or called target: %+v %v", result, err)
	}
}
