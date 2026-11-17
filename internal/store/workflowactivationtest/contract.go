package workflowactivationtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kognition-ai/kognition/internal/store"
)

type Factory func(*testing.T) (store.WorkflowActivationStore, store.WorkflowActivationStore)

func Run(test *testing.T, factory Factory) {
	test.Run("stop before dispatch", func(test *testing.T) {
		first, second := factory(test)
		gate, effect, claim := fixture(test, first)
		gate = mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}).Activation
		stopped := mutate(test, second, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: "cancel-1", StopMode: store.WorkflowGateStopping}).Activation
		_, err := first.MutateWorkflowActivation(context.Background(), gate.Spec.Key, store.WorkflowActivationMutation{ExpectedVersion: stopped.Version, Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim})
		if !errors.Is(err, store.ErrConflict) {
			test.Fatalf("stopped gate admitted dispatch: %v", err)
		}
		stopped = mutate(test, first, stopped, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "NotDispatched", EvidenceDigest: strings.Repeat("d", 64)}).Activation
		stopped = mutate(test, second, stopped, store.WorkflowActivationMutation{Kind: store.WorkflowGateDrain, StopID: "cancel-1"}).Activation
		retained, err := first.GetWorkflowActivation(context.Background(), gate.Spec.Key)
		if err != nil || retained.Mode != store.WorkflowGateStopped || retained.Version != stopped.Version {
			test.Fatalf("drain not durable: %+v %v", retained, err)
		}
	})
	test.Run("dispatch before stop and replay", func(test *testing.T) {
		first, second := factory(test)
		gate, effect, claim := fixture(test, first)
		gate = mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}).Activation
		committed := mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim})
		if !committed.DispatchGranted {
			test.Fatal("first commit did not grant dispatch")
		}
		gate = mutate(test, second, committed.Activation, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: "handoff", StopMode: store.WorkflowGateQuiescing}).Activation
		if repeated := mutate(test, second, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}); repeated.DispatchGranted || repeated.Changed {
			test.Fatal("duplicate commit granted target contact")
		}
		if _, err := second.MutateWorkflowActivation(context.Background(), gate.Spec.Key, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateDrain, StopID: "handoff"}); !errors.Is(err, store.ErrConflict) {
			test.Fatal("unsettled effect drained")
		}
		gate = mutate(test, second, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "Succeeded", EvidenceDigest: strings.Repeat("d", 64)}).Activation
		gate = mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateDrain, StopID: "handoff"}).Activation
		if gate.Mode != store.WorkflowGateStopped {
			test.Fatal("settled handoff did not drain")
		}
	})
	test.Run("immutable binding and namespace isolation", func(test *testing.T) {
		first, second := factory(test)
		gate, _, _ := fixture(test, first)
		changed := gate.Spec
		changed.BindingDigest = strings.Repeat("d", 64)
		if _, err := second.CreateWorkflowActivation(context.Background(), changed); !errors.Is(err, store.ErrConflict) {
			test.Fatal("changed binding replaced an existing gate")
		}
		key := gate.Spec.Key
		key.Workspace = "another-workspace"
		if _, err := second.GetWorkflowActivation(context.Background(), key); !errors.Is(err, store.ErrNotFound) {
			test.Fatal("foreign workspace resolved the gate")
		}
		changed = gate.Spec
		changed.Key.ActivationID = uuid.NewString()
		other, err := second.CreateWorkflowActivation(context.Background(), changed)
		if err != nil || other.Version != 1 || other.Spec.Key == gate.Spec.Key {
			test.Fatal("new graph activation reused an old visit")
		}
	})
	test.Run("simultaneous stop and dispatch", func(test *testing.T) {
		first, second := factory(test)
		for range 12 {
			gate, effect, claim := fixture(test, first)
			gate = mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}).Activation
			start := make(chan struct{})
			type outcome struct {
				result store.WorkflowActivationResult
				err    error
			}
			results := make(chan outcome, 2)
			go func() {
				<-start
				result, err := first.MutateWorkflowActivation(context.Background(), gate.Spec.Key, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim})
				results <- outcome{result, err}
			}()
			go func() {
				<-start
				result, err := second.MutateWorkflowActivation(context.Background(), gate.Spec.Key, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateStop, StopID: "cancel-race", StopMode: store.WorkflowGateStopping})
				results <- outcome{result, err}
			}()
			close(start)
			winners, conflicts, admissions := 0, 0, 0
			for range 2 {
				item := <-results
				if item.err == nil {
					winners++
					if item.result.DispatchGranted {
						admissions++
					}
				} else if errors.Is(item.err, store.ErrConflict) {
					conflicts++
				} else {
					test.Fatalf("unexpected race result: %v", item.err)
				}
			}
			if winners != 1 || conflicts != 1 || admissions > 1 {
				test.Fatalf("race: winners=%d conflicts=%d admissions=%d", winners, conflicts, admissions)
			}
			retained, err := second.GetWorkflowActivation(context.Background(), gate.Spec.Key)
			if err != nil {
				test.Fatal(err)
			}
			if admissions == 0 && retained.Mode != store.WorkflowGateStopping {
				test.Fatal("stop winner not retained")
			}
			if admissions == 1 && retained.Effects[effect.ID].State != store.WorkflowEffectCommitted {
				test.Fatal("dispatch winner not retained")
			}
		}
	})
}

func fixture(test *testing.T, storage store.WorkflowActivationStore) (store.WorkflowActivation, store.WorkflowEffect, store.WorkflowEffectClaim) {
	test.Helper()
	gate, err := storage.CreateWorkflowActivation(context.Background(), store.WorkflowActivationSpec{
		Key:           store.WorkflowActivationKey{Workspace: "work", RunUID: uuid.NewString(), Step: "team", ActivationID: uuid.NewString(), Attempt: 1},
		BindingDigest: strings.Repeat("a", 64), Actors: []string{strings.Repeat("b", 64)}, MaxEffects: 2, MaxBytes: 16384, ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		test.Fatal(err)
	}
	effect := store.WorkflowEffect{ID: "effect", BindingDigest: strings.Repeat("c", 64), Actor: strings.Repeat("b", 64), Plane: "tool", Epoch: 1}
	claim := store.WorkflowEffectClaim{Kind: "RuntimeApproval", Namespace: "worker", UID: "approval-uid", Claimant: "gateway-uid", ProcessNonce: "nonce-1", FencingToken: 1}
	return gate, effect, claim
}

func mutate(test *testing.T, storage store.WorkflowActivationStore, gate store.WorkflowActivation, mutation store.WorkflowActivationMutation) store.WorkflowActivationResult {
	test.Helper()
	mutation.ExpectedVersion = gate.Version
	result, err := storage.MutateWorkflowActivation(context.Background(), gate.Spec.Key, mutation)
	if err != nil {
		test.Fatalf("%s: %v", mutation.Kind, err)
	}
	return result
}
