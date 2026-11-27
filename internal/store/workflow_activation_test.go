// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

func TestWorkflowActivationTaskCreateAdmissionIsSingleUse(test *testing.T) {
	gate, effect, claim, now := workflowGateFixture(test)
	effect.Plane = "communication"
	registered := workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}, now).Activation
	committed := workflowGateApply(test, registered, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}, now).Activation
	observation := store.WorkflowTaskObservation{Protocol: store.WorkflowTaskA2AGRPC, ActionDigest: strings.Repeat("d", 64), TaskID: "task-1", ContextID: "context-1"}
	mutation := store.WorkflowActivationMutation{Kind: store.WorkflowGateAdmitTaskCreate, Effect: effect, Claim: claim, TaskObservation: observation, TaskCreateReceiptDigest: strings.Repeat("e", 64)}
	admitted := workflowGateApply(test, committed, mutation, now)
	if !admitted.Changed || admitted.DispatchGranted || admitted.OwnerActivated || len(admitted.Activation.Effects) != 1 || admitted.Activation.Owner != committed.Owner || admitted.Activation.Epoch != committed.Epoch || admitted.Activation.Effects[effect.ID].Claim != claim {
		test.Fatal("task admission granted dispatch, replaced ownership or discarded the existing claim")
	}
	if committed.Effects[effect.ID].TaskObservation != nil || committed.Effects[effect.ID].TaskCreateReceiptDigest != "" {
		test.Fatal("admission mutated the independent prior snapshot")
	}
	for _, digest := range []string{strings.Repeat("e", 64), strings.Repeat("f", 64)} {
		mutation.ExpectedVersion, mutation.TaskCreateReceiptDigest = admitted.Activation.Version, digest
		if result, err := store.ApplyWorkflowActivation(admitted.Activation, mutation, now); !errors.Is(err, store.ErrConflict) || result.Changed || result.DispatchGranted {
			test.Fatalf("consumed CREATE authorization was reused: %v", err)
		}
	}
	mutation.ExpectedVersion = committed.Version
	if _, err := store.ApplyWorkflowActivation(admitted.Activation, mutation, now); !errors.Is(err, store.ErrConflict) {
		test.Fatal("racing stale CREATE admission won")
	}
	settled := workflowGateApply(test, admitted.Activation, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "Succeeded", EvidenceDigest: strings.Repeat("f", 64)}, now).Activation
	if settled.Effects[effect.ID].TaskCreateReceiptDigest != strings.Repeat("e", 64) || *settled.Effects[effect.ID].TaskObservation != observation || store.ValidateWorkflowActivation(settled) != nil {
		test.Fatal("settlement discarded CREATE replay evidence")
	}
	for name, change := range map[string]func(*store.WorkflowActivation, *store.WorkflowActivationMutation){
		"uncommitted": func(current *store.WorkflowActivation, _ *store.WorkflowActivationMutation) { *current = registered },
		"stopped": func(current *store.WorkflowActivation, _ *store.WorkflowActivationMutation) {
			*current = workflowGateApply(test, committed, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: "cancel", StopMode: store.WorkflowGateStopping}, now).Activation
		},
		"wrong-owner": func(_ *store.WorkflowActivation, changed *store.WorkflowActivationMutation) {
			changed.Effect.Actor = strings.Repeat("f", 64)
		},
		"wrong-claim": func(_ *store.WorkflowActivation, changed *store.WorkflowActivationMutation) {
			changed.Claim.ProcessNonce = "foreign"
		},
		"wrong-epoch": func(_ *store.WorkflowActivation, changed *store.WorkflowActivationMutation) { changed.Effect.Epoch++ },
		"unbounded-marker": func(_ *store.WorkflowActivation, changed *store.WorkflowActivationMutation) {
			changed.TaskCreateReceiptDigest = strings.Repeat("f", 65)
		},
		"foreign-task": func(current *store.WorkflowActivation, _ *store.WorkflowActivationMutation) {
			changedObservation := observation
			changedObservation.TaskID = "foreign"
			*current = workflowGateApply(test, committed, store.WorkflowActivationMutation{Kind: store.WorkflowGateObserveTask, Effect: effect, Claim: claim, TaskObservation: changedObservation}, now).Activation
		},
	} {
		test.Run(name, func(test *testing.T) {
			current, changed := committed, mutation
			changed.TaskCreateReceiptDigest = strings.Repeat("e", 64)
			change(&current, &changed)
			changed.ExpectedVersion = current.Version
			if result, err := store.ApplyWorkflowActivation(current, changed, now); !errors.Is(err, store.ErrConflict) || result.Changed || result.DispatchGranted {
				test.Fatalf("invalid task admission accepted: %v", err)
			}
		})
	}
	mutation.ExpectedVersion = committed.Version
	if _, err := store.ApplyWorkflowActivation(committed, mutation, committed.Spec.ExpiresAt); !errors.Is(err, store.ErrConflict) {
		test.Fatal("expired CREATE admission accepted")
	}
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

func TestWorkflowActivationRejectsChangedEffectIdentity(test *testing.T) {
	gate, effect, claim, now := workflowGateFixture(test)
	registered := workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}, now).Activation
	committed := workflowGateApply(test, registered, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}, now).Activation
	uncertain := workflowGateApply(test, committed, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "Uncertain", EvidenceDigest: strings.Repeat("d", 64)}, now).Activation
	changes := map[string]func(*store.WorkflowEffect){
		"actor": func(changed *store.WorkflowEffect) { changed.Actor = strings.Repeat("e", 64) },
		"plane": func(changed *store.WorkflowEffect) { changed.Plane = "model" },
		"epoch": func(changed *store.WorkflowEffect) { changed.Epoch++ },
	}
	for name, change := range changes {
		test.Run(name, func(test *testing.T) {
			changed := effect
			change(&changed)
			cases := []struct {
				gate     store.WorkflowActivation
				mutation store.WorkflowActivationMutation
			}{
				{registered, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: changed, Claim: claim}},
				{committed, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: changed, Claim: claim}},
				{committed, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: changed, Claim: claim, Disposition: "Succeeded", EvidenceDigest: strings.Repeat("d", 64)}},
				{uncertain, store.WorkflowActivationMutation{Kind: store.WorkflowGateResolve, Effect: changed, Claim: claim, Disposition: "Succeeded", EvidenceDigest: strings.Repeat("d", 64), ProofDigest: strings.Repeat("f", 64)}},
			}
			for _, item := range cases {
				item.mutation.ExpectedVersion = item.gate.Version
				result, err := store.ApplyWorkflowActivation(item.gate, item.mutation, now)
				if !errors.Is(err, store.ErrConflict) || result.DispatchGranted || result.Changed {
					test.Fatalf("%s accepted changed %s: result=%+v error=%v", item.mutation.Kind, name, result, err)
				}
			}
		})
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

func TestWorkflowActivationTaskObservationMaintainsSnapshotAndCapacity(test *testing.T) {
	gate, effect, claim, now := workflowGateFixture(test)
	effect.Plane = "communication"
	observation := store.WorkflowTaskObservation{Protocol: store.WorkflowTaskA2AGRPC, ActionDigest: strings.Repeat("e", 64),
		TaskID: strings.Repeat("<", 128), ContextID: strings.Repeat("<", 128)}
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}, now).Activation
	claim.Kind, claim.Namespace, claim.UID = strings.Repeat("<", 64), strings.Repeat("<", 63), strings.Repeat("<", 128)
	claim.Claimant, claim.ProcessNonce = strings.Repeat("<", 128), strings.Repeat("<", 128)
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}, now).Activation
	for _, malformed := range []store.WorkflowTaskObservation{
		{Protocol: "rest", ActionDigest: observation.ActionDigest, TaskID: "task", ContextID: "context"},
		{Protocol: store.WorkflowTaskA2AGRPC, TaskID: "task", ContextID: "context"},
		{Protocol: store.WorkflowTaskA2AGRPC, ActionDigest: observation.ActionDigest, TaskID: strings.Repeat("a", 129), ContextID: "context"},
		{Protocol: store.WorkflowTaskA2AGRPC, ActionDigest: observation.ActionDigest, TaskID: "task", ContextID: "context\tother"},
		{Protocol: store.WorkflowTaskA2AGRPC, ActionDigest: observation.ActionDigest, TaskID: string([]byte{0xff}), ContextID: "context"},
	} {
		if _, err := store.ApplyWorkflowActivation(gate, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateObserveTask, Effect: effect, Claim: claim, TaskObservation: malformed}, now); !errors.Is(err, store.ErrConflict) {
			test.Fatalf("malformed observation accepted: %+v %v", malformed, err)
		}
	}
	observed := workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateObserveTask, Effect: effect, Claim: claim, TaskObservation: observation}, gate.Spec.ExpiresAt.Add(time.Hour))
	if observed.DispatchGranted || observed.OwnerActivated || gate.Effects[effect.ID].TaskObservation != nil {
		test.Fatal("expired observation granted execution or modified its prior snapshot")
	}
	gate = observed.Activation
	repeated := workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateObserveTask, Effect: effect, Claim: claim, TaskObservation: observation}, now)
	repeated.Activation.Effects[effect.ID].TaskObservation.TaskID = "caller-mutated"
	if *gate.Effects[effect.ID].TaskObservation != observation {
		test.Fatal("returned observation aliases the caller's prior snapshot")
	}
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: strings.Repeat("<", 128), StopMode: store.WorkflowGateStopping}, now).Activation
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "Succeeded", EvidenceDigest: strings.Repeat("d", 64)}, now).Activation
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateDrain, StopID: strings.Repeat("<", 128)}, now).Activation
	if store.ValidateWorkflowActivation(gate) != nil || *gate.Effects[effect.ID].TaskObservation != observation {
		test.Fatal("maximum observation exhausted reserved stop and settlement capacity")
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

func TestWorkflowActivationCancellationSupersedesQuiescence(test *testing.T) {
	for _, state := range []string{"quiescing", "drained", "uncertain", "legacy"} {
		test.Run(state, func(test *testing.T) {
			gate, effect, claim, now := workflowGateFixture(test)
			gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}, now).Activation
			gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}, now).Activation
			gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: "handoff-1", StopMode: store.WorkflowGateQuiescing}, now).Activation
			if state == "drained" {
				gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "Succeeded", EvidenceDigest: strings.Repeat("d", 64)}, now).Activation
				gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateDrain, StopID: "handoff-1"}, now).Activation
			}
			if state == "uncertain" {
				gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "Uncertain", EvidenceDigest: strings.Repeat("d", 64)}, now).Activation
			}
			if state == "legacy" {
				gate.QuiesceID = ""
			}
			if _, err := store.ApplyWorkflowActivation(gate, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateStop, StopID: "handoff-1", StopMode: store.WorkflowGateStopping}, now); !errors.Is(err, store.ErrConflict) {
				test.Fatal("handoff operation ID was reused for a different cancellation")
			}
			cancelled := workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: "cancel-1", StopMode: store.WorkflowGateStopping}, now)
			if cancelled.DispatchGranted || cancelled.Activation.StopID != "cancel-1" || cancelled.Activation.StopMode != store.WorkflowGateStopping || cancelled.Activation.Epoch != gate.Epoch+1 {
				test.Fatal("cancellation did not durably fence the handoff")
			}
			if cancelled.Activation.QuiesceID != "handoff-1" {
				test.Fatal("cancellation discarded the original quiescence identity")
			}
			if state == "uncertain" && cancelled.Activation.Mode != store.WorkflowGateUncertain {
				test.Fatal("cancellation hid an uncertain effect")
			}
			if _, err := store.ApplyWorkflowActivation(cancelled.Activation, store.WorkflowActivationMutation{ExpectedVersion: cancelled.Activation.Version, Kind: store.WorkflowGateDrain, StopID: "handoff-1"}, now); !errors.Is(err, store.ErrConflict) {
				test.Fatal("superseded handoff still controlled drain")
			}
			repeated := workflowGateApply(test, cancelled.Activation, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: "cancel-1", StopMode: store.WorkflowGateStopping}, now)
			if repeated.Changed || repeated.DispatchGranted {
				test.Fatal("matching cancellation retry changed the fence")
			}
		})
	}
}
