package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/store"
)

func workflowGateFixture(test *testing.T) (store.WorkflowActivation, store.WorkflowEffect, store.WorkflowEffectClaim, time.Time) {
	test.Helper()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	gate, err := store.NewWorkflowActivation(store.WorkflowActivationSpec{
		Key:           store.WorkflowActivationKey{Workspace: "work", RunUID: "run-uid", Step: "team", ActivationID: "visit-1", Attempt: 1},
		BindingDigest: strings.Repeat("a", 64), Actors: []string{strings.Repeat("b", 64)},
		MaxEffects: 2, MaxBytes: 16384, ExpiresAt: now.Add(time.Hour),
	}, now)
	if err != nil {
		test.Fatal(err)
	}
	effect := store.WorkflowEffect{ID: "invoke-1", BindingDigest: strings.Repeat("c", 64), Actor: strings.Repeat("b", 64), Plane: "tool", Epoch: 1}
	claim := store.WorkflowEffectClaim{Kind: "RuntimeApproval", Namespace: "worker", UID: "approval-uid", Claimant: "gateway-uid", ProcessNonce: "process-1", FencingToken: 1}
	return gate, effect, claim, now
}

func workflowGateApply(test *testing.T, gate store.WorkflowActivation, mutation store.WorkflowActivationMutation, now time.Time) store.WorkflowActivationResult {
	test.Helper()
	mutation.ExpectedVersion = gate.Version
	result, err := store.ApplyWorkflowActivation(gate, mutation, now)
	if err != nil {
		test.Fatal(err)
	}
	return result
}

func TestWorkflowActivationStopPreventsDispatch(test *testing.T) {
	gate, effect, claim, now := workflowGateFixture(test)
	registered := workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}, now)
	stopped := workflowGateApply(test, registered.Activation, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: "cancel-1", StopMode: store.WorkflowGateStopping}, now)
	if _, err := store.ApplyWorkflowActivation(stopped.Activation, store.WorkflowActivationMutation{
		ExpectedVersion: stopped.Activation.Version, Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim,
	}, now); !errors.Is(err, store.ErrConflict) {
		test.Fatalf("stopped activation admitted dispatch: %v", err)
	}
	if gate.Version != 1 || gate.Mode != store.WorkflowGateActive || len(gate.Effects) != 0 {
		test.Fatal("transition mutated the caller's prior snapshot")
	}
	aborted := workflowGateApply(test, stopped.Activation, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "NotDispatched", EvidenceDigest: strings.Repeat("d", 64)}, now)
	drained := workflowGateApply(test, aborted.Activation, store.WorkflowActivationMutation{Kind: store.WorkflowGateDrain, StopID: "cancel-1"}, now)
	if drained.Activation.Mode != store.WorkflowGateStopped || drained.DispatchGranted {
		test.Fatal("drain did not preserve the no-dispatch outcome")
	}
}

func TestWorkflowActivationCommitNeverGrantsReplay(test *testing.T) {
	gate, effect, claim, now := workflowGateFixture(test)
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}, now).Activation
	committed := workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}, now)
	if !committed.DispatchGranted {
		test.Fatal("first exact commit did not grant dispatch")
	}
	gate = workflowGateApply(test, committed.Activation, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: "handoff-1", StopMode: store.WorkflowGateQuiescing}, now).Activation
	repeated := workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}, now)
	if repeated.DispatchGranted || repeated.Changed {
		test.Fatal("a retained dispatch admission granted another execution")
	}
	if _, err := store.ApplyWorkflowActivation(gate, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateDrain, StopID: "handoff-1"}, now); !errors.Is(err, store.ErrConflict) {
		test.Fatal("handoff drained an unsettled committed operation")
	}
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "Uncertain", EvidenceDigest: strings.Repeat("d", 64)}, now).Activation
	if _, err := store.ApplyWorkflowActivation(gate, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "Succeeded", EvidenceDigest: strings.Repeat("e", 64)}, now); !errors.Is(err, store.ErrConflict) {
		test.Fatal("ordinary settlement erased an uncertain outcome")
	}
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateResolve, Effect: effect, Claim: claim, Disposition: "Succeeded", EvidenceDigest: strings.Repeat("e", 64), ProofDigest: strings.Repeat("f", 64)}, now).Activation
	if gate.Effects[effect.ID].ProofDigest != strings.Repeat("f", 64) {
		test.Fatal("reconciliation discarded its proof binding")
	}
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateDrain, StopID: "handoff-1"}, now).Activation
	if gate.Mode != store.WorkflowGateStopped {
		test.Fatal("known reconciled outcome could not drain")
	}
}

func TestWorkflowActivationRejectsStaleAndChangedBindings(test *testing.T) {
	gate, effect, claim, now := workflowGateFixture(test)
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}, now).Activation
	for _, mutation := range []store.WorkflowActivationMutation{
		{ExpectedVersion: 1, Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim},
		{ExpectedVersion: gate.Version, Kind: store.WorkflowGateCommit, Effect: store.WorkflowEffect{ID: effect.ID, BindingDigest: strings.Repeat("d", 64)}, Claim: claim},
		{ExpectedVersion: gate.Version, Kind: store.WorkflowGateStop, StopID: "cancel", StopMode: store.WorkflowGateActive},
	} {
		if _, err := store.ApplyWorkflowActivation(gate, mutation, now); !errors.Is(err, store.ErrConflict) {
			test.Fatalf("invalid mutation accepted: %+v, %v", mutation, err)
		}
	}
	if _, err := store.ApplyWorkflowActivation(gate, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}, gate.Spec.ExpiresAt); !errors.Is(err, store.ErrConflict) {
		test.Fatal("expired activation admitted dispatch")
	}
	effect.ID, effect.Actor = "foreign", strings.Repeat("d", 64)
	if _, err := store.ApplyWorkflowActivation(gate, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateRegister, Effect: effect}, now); !errors.Is(err, store.ErrConflict) {
		test.Fatal("unapproved actor registered an effect")
	}
}

func TestWorkflowActivationRequiresFiniteBounds(test *testing.T) {
	gate, effect, _, now := workflowGateFixture(test)
	for _, maxEffects := range []int{0, store.WorkflowGateMaxEffects + 1} {
		spec := gate.Spec
		spec.MaxEffects = maxEffects
		if _, err := store.NewWorkflowActivation(spec, now); !errors.Is(err, store.ErrConflict) {
			test.Fatal("invalid effect ceiling accepted")
		}
	}
	spec := gate.Spec
	spec.MaxEffects = 1
	gate, err := store.NewWorkflowActivation(spec, now)
	if err != nil {
		test.Fatal(err)
	}
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}, now).Activation
	effect.ID = "invoke-2"
	if _, err := store.ApplyWorkflowActivation(gate, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateRegister, Effect: effect}, now); !errors.Is(err, store.ErrConflict) {
		test.Fatal("activation exceeded its finite effect ceiling")
	}
}

func TestWorkflowActivationReservesRoomForStopAndSettlement(test *testing.T) {
	gate, effect, claim, now := workflowGateFixture(test)
	spec := gate.Spec
	spec.MaxBytes = 2500
	gate, err := store.NewWorkflowActivation(spec, now)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := store.ApplyWorkflowActivation(gate, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateRegister, Effect: effect}, now); !errors.Is(err, store.ErrConflict) {
		test.Fatal("registration consumed reserved lifecycle capacity")
	}
	spec.MaxBytes = 10000
	gate, err = store.NewWorkflowActivation(spec, now)
	if err != nil {
		test.Fatal(err)
	}
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}, now).Activation
	claim.Kind, claim.Namespace, claim.UID = strings.Repeat("<", 64), strings.Repeat("<", 63), strings.Repeat("<", 128)
	claim.Claimant, claim.ProcessNonce = strings.Repeat("<", 128), strings.Repeat("<", 128)
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}, now).Activation
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: strings.Repeat("<", 128), StopMode: store.WorkflowGateStopping}, now).Activation
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "Succeeded", EvidenceDigest: strings.Repeat("d", 64)}, now).Activation
	if gate.Effects[effect.ID].State != store.WorkflowEffectSettled {
		test.Fatal("reserved lifecycle capacity could not retain settlement")
	}
}

func TestWorkflowActivationRejectsMalformedRetainedState(test *testing.T) {
	gate, effect, claim, now := workflowGateFixture(test)
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}, now).Activation
	gate.Mode = "Unknown"
	if err := store.ValidateWorkflowActivation(gate); !errors.Is(err, store.ErrConflict) {
		test.Fatal("unknown retained mode accepted")
	}
	gate.Mode = store.WorkflowGateActive
	for _, state := range []string{store.WorkflowEffectCommitted, store.WorkflowEffectSettled, "Unknown"} {
		broken := gate.Effects[effect.ID]
		broken.State = state
		gate.Effects[effect.ID] = broken
		if err := store.ValidateWorkflowActivation(gate); !errors.Is(err, store.ErrConflict) {
			test.Fatalf("malformed retained effect %q accepted", state)
		}
	}
	if result, err := store.ApplyWorkflowActivation(gate, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}, now); !errors.Is(err, store.ErrConflict) || result.DispatchGranted {
		test.Fatal("malformed retained state produced dispatch authority")
	}
}
