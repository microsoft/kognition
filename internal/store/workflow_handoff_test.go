package store_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/store"
)

func workflowHandoffFixture(test *testing.T) (store.WorkflowActivation, store.WorkflowActivationMutation, time.Time) {
	test.Helper()
	base, _, _, now := workflowGateFixture(test)
	spec := base.Spec
	spec.Actors = append(spec.Actors, strings.Repeat("d", 64))
	spec.InitialOwner, spec.MaxHandoffs, spec.MaxBytes = strings.Repeat("b", 64), 2, 32768
	gate, err := store.NewWorkflowActivation(spec, now)
	if err != nil {
		test.Fatal(err)
	}
	proposal := store.WorkflowActivationMutation{Kind: store.WorkflowGateProposeHandoff, HandoffID: "transfer-1", Actor: spec.InitialOwner,
		Epoch: 1, Recipient: strings.Repeat("d", 64), ContextDigest: strings.Repeat("e", 64)}
	return gate, proposal, now
}

func TestWorkflowHandoffRequiresAcknowledgedDrainedTransfer(test *testing.T) {
	gate, proposal, now := workflowHandoffFixture(test)
	_, effect, claim, _ := workflowGateFixture(test)
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}, now).Activation
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}, now).Activation
	gate = workflowGateApply(test, gate, proposal, now).Activation
	accept := proposal
	accept.Kind, accept.Actor, accept.AcceptanceDigest = store.WorkflowGateAcceptHandoff, proposal.Recipient, strings.Repeat("f", 64)
	gate = workflowGateApply(test, gate, accept, now).Activation
	if gate.Owner != proposal.Actor || gate.Epoch != 1 {
		test.Fatal("recipient acknowledgement activated its owner")
	}
	quiesce := store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: proposal.HandoffID, StopMode: store.WorkflowGateQuiescing, Actor: proposal.Actor, Epoch: 1}
	gate = workflowGateApply(test, gate, quiesce, now).Activation
	activate := proposal
	activate.Kind, activate.AcceptanceDigest = store.WorkflowGateActivateHandoff, accept.AcceptanceDigest
	activate.ExpectedVersion = gate.Version
	if _, err := store.ApplyWorkflowActivation(gate, activate, now); !errors.Is(err, store.ErrConflict) {
		test.Fatal("recipient activated with unsettled prior work")
	}
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "Succeeded", EvidenceDigest: strings.Repeat("a", 64)}, now).Activation
	committed := workflowGateApply(test, gate, activate, now)
	if !committed.OwnerActivated || committed.DispatchGranted || committed.Activation.Owner != proposal.Recipient || committed.Activation.Epoch != 2 || len(committed.Activation.Effects) != 1 {
		test.Fatal("handoff did not retain the activation, effects, and successor epoch")
	}
	gate = committed.Activation
	if err := store.ValidateWorkflowActivation(gate); err != nil {
		test.Fatal(err)
	}
	repeated := workflowGateApply(test, gate, activate, now)
	if repeated.OwnerActivated || repeated.Changed {
		test.Fatal("handoff retry activated the owner again")
	}
	effect.ID, effect.Epoch = "old-owner-effect", gate.Epoch
	if _, err := store.ApplyWorkflowActivation(gate, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateRegister, Effect: effect}, now); !errors.Is(err, store.ErrConflict) {
		test.Fatal("old owner borrowed the successor epoch")
	}
	effect.ID, effect.Actor = "new-owner-effect", proposal.Recipient
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}, now).Activation
	if gate.Effects[effect.ID].Actor != proposal.Recipient || gate.Handoffs[0].AcceptanceDigest != accept.AcceptanceDigest {
		test.Fatal("recipient authority or retained transfer acknowledgement was lost")
	}
}

func TestWorkflowHandoffCancellationPreventsActivation(test *testing.T) {
	gate, proposal, now := workflowHandoffFixture(test)
	gate = workflowGateApply(test, gate, proposal, now).Activation
	accept := proposal
	accept.Kind, accept.Actor, accept.AcceptanceDigest = store.WorkflowGateAcceptHandoff, proposal.Recipient, strings.Repeat("f", 64)
	gate = workflowGateApply(test, gate, accept, now).Activation
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: proposal.HandoffID, StopMode: store.WorkflowGateQuiescing, Actor: proposal.Actor, Epoch: 1}, now).Activation
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateDrain, StopID: proposal.HandoffID}, now).Activation
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: "cancel", StopMode: store.WorkflowGateStopping}, now).Activation
	activate := proposal
	activate.ExpectedVersion, activate.Kind, activate.AcceptanceDigest = gate.Version, store.WorkflowGateActivateHandoff, accept.AcceptanceDigest
	if _, err := store.ApplyWorkflowActivation(gate, activate, now); !errors.Is(err, store.ErrConflict) {
		test.Fatal("cancelled handoff activated the recipient")
	}
}

func TestWorkflowHandoffRejectsForgedAcceptanceAndConflictingProposals(test *testing.T) {
	gate, proposal, now := workflowHandoffFixture(test)
	gate = workflowGateApply(test, gate, proposal, now).Activation
	for _, mutation := range []store.WorkflowActivationMutation{
		{Kind: store.WorkflowGateAcceptHandoff, HandoffID: proposal.HandoffID, Actor: proposal.Actor, Epoch: 1, Recipient: proposal.Recipient, ContextDigest: proposal.ContextDigest, AcceptanceDigest: strings.Repeat("f", 64)},
		{Kind: store.WorkflowGateAcceptHandoff, HandoffID: proposal.HandoffID, Actor: proposal.Recipient, Epoch: 1, Recipient: proposal.Recipient, ContextDigest: strings.Repeat("a", 64), AcceptanceDigest: strings.Repeat("f", 64)},
		{Kind: store.WorkflowGateAcceptHandoff, HandoffID: proposal.HandoffID, Actor: proposal.Recipient, Epoch: 1, Recipient: proposal.Actor, ContextDigest: proposal.ContextDigest, AcceptanceDigest: strings.Repeat("f", 64)},
		{Kind: store.WorkflowGateProposeHandoff, HandoffID: "transfer-2", Actor: proposal.Actor, Epoch: 1, Recipient: proposal.Recipient, ContextDigest: proposal.ContextDigest},
		{Kind: store.WorkflowGateStop, StopID: proposal.HandoffID, StopMode: store.WorkflowGateQuiescing, Actor: proposal.Actor, Epoch: 1},
	} {
		mutation.ExpectedVersion = gate.Version
		if _, err := store.ApplyWorkflowActivation(gate, mutation, now); !errors.Is(err, store.ErrConflict) {
			test.Fatalf("unauthorized transfer operation %q accepted: %v", mutation.Kind, err)
		}
	}
}

func TestWorkflowHandoffRetainsHistoryAndFiniteAllowance(test *testing.T) {
	gate, proposal, now := workflowHandoffFixture(test)
	_, effect, claim, _ := workflowGateFixture(test)
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}, now).Activation
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}, now).Activation
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "Succeeded", EvidenceDigest: strings.Repeat("a", 64)}, now).Activation
	for index, recipient := range []string{strings.Repeat("d", 64), strings.Repeat("b", 64)} {
		proposal.HandoffID, proposal.Actor, proposal.Epoch, proposal.Recipient = fmt.Sprintf("transfer-%d", index+1), gate.Owner, gate.Epoch, recipient
		gate = workflowGateApply(test, gate, proposal, now).Activation
		accept := proposal
		accept.Kind, accept.Actor, accept.AcceptanceDigest = store.WorkflowGateAcceptHandoff, recipient, strings.Repeat("f", 64)
		gate = workflowGateApply(test, gate, accept, now).Activation
		gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: proposal.HandoffID, StopMode: store.WorkflowGateQuiescing, Actor: proposal.Actor, Epoch: proposal.Epoch}, now).Activation
		activate := proposal
		activate.Kind, activate.AcceptanceDigest = store.WorkflowGateActivateHandoff, accept.AcceptanceDigest
		gate = workflowGateApply(test, gate, activate, now).Activation
	}
	if gate.Owner != strings.Repeat("b", 64) || gate.Epoch != 3 || len(gate.Handoffs) != 2 || len(gate.Effects) != 1 || gate.Effects[effect.ID].State != store.WorkflowEffectSettled {
		test.Fatal("successive handoffs reset retained history or effect state")
	}
	proposal.HandoffID, proposal.Actor, proposal.Epoch, proposal.Recipient = "transfer-3", gate.Owner, gate.Epoch, strings.Repeat("d", 64)
	proposal.ExpectedVersion = gate.Version
	if _, err := store.ApplyWorkflowActivation(gate, proposal, now); !errors.Is(err, store.ErrConflict) {
		test.Fatal("handoffs exceeded the admitted transfer allowance")
	}
	gate.Handoffs[1].SourceOwner = strings.Repeat("d", 64)
	gate.Handoffs[1].SuccessorEpoch = 1
	if err := store.ValidateWorkflowActivation(gate); !errors.Is(err, store.ErrConflict) {
		test.Fatal("invalid retained owner history was accepted")
	}
}

func TestWorkflowHandoffExpiryPreventsActivationButNotMaintenance(test *testing.T) {
	gate, proposal, now := workflowHandoffFixture(test)
	gate = workflowGateApply(test, gate, proposal, now).Activation
	accept := proposal
	accept.Kind, accept.Actor, accept.AcceptanceDigest = store.WorkflowGateAcceptHandoff, proposal.Recipient, strings.Repeat("f", 64)
	gate = workflowGateApply(test, gate, accept, now).Activation
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: proposal.HandoffID, StopMode: store.WorkflowGateQuiescing, Actor: proposal.Actor, Epoch: 1}, now).Activation
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateDrain, StopID: proposal.HandoffID}, now).Activation
	activate := proposal
	activate.Kind, activate.AcceptanceDigest, activate.ExpectedVersion = store.WorkflowGateActivateHandoff, accept.AcceptanceDigest, gate.Version
	if _, err := store.ApplyWorkflowActivation(gate, activate, gate.Spec.ExpiresAt); !errors.Is(err, store.ErrConflict) {
		test.Fatal("expired handoff activated the recipient")
	}
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: "cancel-expired", StopMode: store.WorkflowGateStopping}, gate.Spec.ExpiresAt).Activation
	gate = workflowGateApply(test, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateDrain, StopID: "cancel-expired"}, gate.Spec.ExpiresAt).Activation
	if gate.Mode != store.WorkflowGateStopped || gate.Owner != proposal.Actor || gate.Handoffs[0].State != store.WorkflowHandoffAccepted {
		test.Fatal("expiry prevented safe cancellation or activated a recipient")
	}
}
