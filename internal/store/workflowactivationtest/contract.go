package workflowactivationtest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kognition-ai/kognition/internal/store"
)

type Factory func(*testing.T) (store.WorkflowActivationStore, store.WorkflowActivationStore)

func Run(test *testing.T, factory Factory) {
	test.Run("task CREATE marker survives reconstruction and settlement", func(test *testing.T) {
		first, second := factory(test)
		gate, effect, claim := fixture(test, first)
		effect.Plane = "communication"
		gate = mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}).Activation
		gate = mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}).Activation
		observation := store.WorkflowTaskObservation{Protocol: store.WorkflowTaskA2AGRPC, ActionDigest: strings.Repeat("d", 64), TaskID: "task-1", ContextID: "context-1"}
		admit := store.WorkflowActivationMutation{Kind: store.WorkflowGateAdmitTaskCreate, Effect: effect, Claim: claim, TaskObservation: observation, TaskCreateReceiptDigest: strings.Repeat("e", 64)}
		result := mutate(test, second, gate, admit)
		if !result.Changed || result.DispatchGranted || result.OwnerActivated || result.Activation.Owner != gate.Owner || result.Activation.Epoch != gate.Epoch {
			test.Fatal("CREATE marker changed execution ownership or granted dispatch")
		}
		gate, err := first.GetWorkflowActivation(test.Context(), gate.Spec.Key)
		if err != nil || gate.Effects[effect.ID].TaskCreateReceiptDigest != admit.TaskCreateReceiptDigest || gate.Effects[effect.ID].Claim != claim || gate.Effects[effect.ID].TaskObservation == nil || *gate.Effects[effect.ID].TaskObservation != observation {
			test.Fatalf("CREATE marker was not durable across clients: %v", err)
		}
		for _, settled := range []bool{false, true} {
			if settled {
				gate = mutate(test, second, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "Succeeded", EvidenceDigest: strings.Repeat("f", 64)}).Activation
			}
			for _, digest := range []string{strings.Repeat("e", 64), strings.Repeat("f", 64)} {
				admit.ExpectedVersion, admit.TaskCreateReceiptDigest = gate.Version, digest
				if result, err := first.MutateWorkflowActivation(test.Context(), gate.Spec.Key, admit); !errors.Is(err, store.ErrConflict) || result.Changed || result.DispatchGranted || result.OwnerActivated {
					test.Fatalf("consumed CREATE marker admitted replay: %v", err)
				}
				retained, err := second.GetWorkflowActivation(test.Context(), gate.Spec.Key)
				if err != nil || !reflect.DeepEqual(retained, gate) {
					test.Fatalf("denied CREATE replay changed durable evidence: %v", err)
				}
			}
		}
	})
	test.Run("simultaneous task CREATE consumption has one winner", func(test *testing.T) {
		first, second := factory(test)
		for range 4 {
			gate, effect, claim := fixture(test, first)
			effect.Plane = "communication"
			gate = mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}).Activation
			gate = mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}).Activation
			observation := store.WorkflowTaskObservation{Protocol: store.WorkflowTaskA2AGRPC, ActionDigest: strings.Repeat("d", 64), TaskID: "racing-task", ContextID: "racing-context"}
			type outcome struct {
				result store.WorkflowActivationResult
				err    error
				digest string
			}
			start, results := make(chan struct{}), make(chan outcome, 2)
			for index, storage := range []store.WorkflowActivationStore{first, second} {
				digest := strings.Repeat([]string{"e", "f"}[index], 64)
				go func() {
					<-start
					result, err := storage.MutateWorkflowActivation(test.Context(), gate.Spec.Key, store.WorkflowActivationMutation{Kind: store.WorkflowGateAdmitTaskCreate, ExpectedVersion: gate.Version, Effect: effect, Claim: claim, TaskObservation: observation, TaskCreateReceiptDigest: digest})
					results <- outcome{result, err, digest}
				}()
			}
			close(start)
			winners, conflicts, winnerDigest := 0, 0, ""
			for range 2 {
				item := <-results
				if item.result.DispatchGranted || item.result.OwnerActivated {
					test.Fatal("CREATE race granted execution ownership")
				}
				if item.err == nil && item.result.Changed {
					winners++
					winnerDigest = item.digest
				} else if errors.Is(item.err, store.ErrConflict) {
					conflicts++
				} else {
					test.Fatalf("unexpected CREATE consumption result: %v", item.err)
				}
			}
			if winners != 1 || conflicts != 1 {
				test.Fatalf("CREATE consumption winners=%d conflicts=%d", winners, conflicts)
			}
			for _, storage := range []store.WorkflowActivationStore{first, second} {
				retained, err := storage.GetWorkflowActivation(test.Context(), gate.Spec.Key)
				if err != nil || retained.Version != gate.Version+1 || retained.Effects[effect.ID].TaskCreateReceiptDigest != winnerDigest || retained.Effects[effect.ID].TaskObservation == nil || *retained.Effects[effect.ID].TaskObservation != observation {
					test.Fatalf("CREATE winner not durable: %v", err)
				}
			}
		}
	})
	test.Run("committed task observation is immutable and never grants dispatch", func(test *testing.T) {
		first, second := factory(test)
		gate, effect, claim := fixture(test, first)
		effect.Plane = "communication"
		gate = mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateRegister, Effect: effect}).Activation
		observation := store.WorkflowTaskObservation{Protocol: store.WorkflowTaskA2AGRPC, ActionDigest: strings.Repeat("e", 64), TaskID: "task-1", ContextID: "context-1"}
		observe := store.WorkflowActivationMutation{Kind: store.WorkflowGateObserveTask, Effect: effect, Claim: claim, TaskObservation: observation, ExpectedVersion: gate.Version}
		if _, err := second.MutateWorkflowActivation(context.Background(), gate.Spec.Key, observe); !errors.Is(err, store.ErrConflict) {
			test.Fatalf("uncommitted effect retained an external task: %v", err)
		}
		gate = mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}).Activation
		observed := mutate(test, second, gate, observe)
		if !observed.Changed || observed.DispatchGranted || observed.OwnerActivated || observed.Activation.Effects[effect.ID].TaskObservation == nil {
			test.Fatalf("observation replaced execution ownership: %+v", observed)
		}
		gate = observed.Activation
		retained, err := first.GetWorkflowActivation(context.Background(), gate.Spec.Key)
		if err != nil || retained.Effects[effect.ID].TaskObservation == nil || *retained.Effects[effect.ID].TaskObservation != observation {
			test.Fatalf("task observation was not durable across clients: %+v %v", retained, err)
		}
		repeated := mutate(test, first, gate, observe)
		if repeated.Changed || repeated.DispatchGranted || repeated.OwnerActivated {
			test.Fatal("identical observation granted or changed execution")
		}
		for _, change := range []string{"task", "context", "action", "protocol", "claim", "effect", "stale"} {
			changed := observe
			changed.ExpectedVersion = gate.Version
			switch change {
			case "task":
				changed.TaskObservation.TaskID = "replacement-task"
			case "context":
				changed.TaskObservation.ContextID = "replacement-context"
			case "action":
				changed.TaskObservation.ActionDigest = strings.Repeat("f", 64)
			case "protocol":
				changed.TaskObservation.Protocol = "rest"
			case "claim":
				changed.Claim.FencingToken++
			case "effect":
				changed.Effect.BindingDigest = strings.Repeat("f", 64)
			case "stale":
				changed.ExpectedVersion--
			}
			if _, err := second.MutateWorkflowActivation(context.Background(), gate.Spec.Key, changed); !errors.Is(err, store.ErrConflict) {
				test.Fatalf("%s replacement accepted: %v", change, err)
			}
		}
		if replay := mutate(test, second, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}); replay.DispatchGranted || replay.Changed {
			test.Fatal("task observation permitted dispatch replay")
		}
		gate = mutate(test, second, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: "cancel-observed", StopMode: store.WorkflowGateStopping}).Activation
		gate = mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateSettle, Effect: effect, Claim: claim, Disposition: "Succeeded", EvidenceDigest: strings.Repeat("d", 64)}).Activation
		gate = mutate(test, second, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateDrain, StopID: "cancel-observed"}).Activation
		if gate.Effects[effect.ID].TaskObservation == nil || *gate.Effects[effect.ID].TaskObservation != observation {
			test.Fatal("settlement discarded retained polling identity")
		}
	})
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
	test.Run("cancellation supersedes handoff drain", func(test *testing.T) {
		for _, ordering := range []string{"cancel-first", "drain-first", "concurrent"} {
			test.Run(ordering, func(test *testing.T) {
				first, second := factory(test)
				gate, _, _ := fixture(test, first)
				gate = mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: "handoff", StopMode: store.WorkflowGateQuiescing}).Activation
				cancel := store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateStop, StopID: "cancel", StopMode: store.WorkflowGateStopping}
				drain := store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateDrain, StopID: "handoff"}
				switch ordering {
				case "cancel-first":
					mutate(test, first, gate, cancel)
				case "drain-first":
					drained := mutate(test, first, gate, drain)
					mutate(test, second, drained.Activation, cancel)
				case "concurrent":
					start := make(chan struct{})
					results := make(chan error, 2)
					go func() {
						<-start
						_, err := first.MutateWorkflowActivation(context.Background(), gate.Spec.Key, cancel)
						results <- err
					}()
					go func() {
						<-start
						_, err := second.MutateWorkflowActivation(context.Background(), gate.Spec.Key, drain)
						results <- err
					}()
					close(start)
					winners, conflicts := 0, 0
					for range 2 {
						err := <-results
						if err == nil {
							winners++
						} else if errors.Is(err, store.ErrConflict) {
							conflicts++
						} else {
							test.Fatalf("unexpected cancellation race result: %v", err)
						}
					}
					if winners != 1 || conflicts != 1 {
						test.Fatalf("cancellation race: winners=%d conflicts=%d", winners, conflicts)
					}
					retained, err := second.GetWorkflowActivation(context.Background(), gate.Spec.Key)
					if err != nil {
						test.Fatal(err)
					}
					if retained.StopMode == store.WorkflowGateQuiescing {
						mutate(test, second, retained, cancel)
					}
				}
				retained, err := second.GetWorkflowActivation(context.Background(), gate.Spec.Key)
				if err != nil || retained.Mode != store.WorkflowGateStopping || retained.StopID != "cancel" ||
					retained.StopMode != store.WorkflowGateStopping || retained.QuiesceID != "handoff" || retained.Epoch != gate.Epoch+1 {
					test.Fatalf("cancellation fence not retained: %+v %v", retained, err)
				}
				for _, oldOperation := range []store.WorkflowActivationMutation{
					{Kind: store.WorkflowGateDrain, StopID: "handoff"},
					{Kind: store.WorkflowGateStop, StopID: "handoff", StopMode: store.WorkflowGateQuiescing},
				} {
					oldOperation.ExpectedVersion = retained.Version
					if _, err := first.MutateWorkflowActivation(context.Background(), gate.Spec.Key, oldOperation); !errors.Is(err, store.ErrConflict) {
						test.Fatalf("superseded handoff retained authority: %v", err)
					}
				}
				repeated := mutate(test, first, retained, cancel)
				if repeated.Changed || repeated.DispatchGranted {
					test.Fatal("cancellation retry mutated the retained fence")
				}
			})
		}
	})
	test.Run("handoff activation races cancellation", func(test *testing.T) {
		for _, ordering := range []string{"cancel-first", "activate-first", "concurrent"} {
			test.Run(ordering, func(test *testing.T) {
				first, second := factory(test)
				gate, err := first.CreateWorkflowActivation(context.Background(), store.WorkflowActivationSpec{
					Key:           store.WorkflowActivationKey{Workspace: "work", RunUID: uuid.NewString(), Step: "team", ActivationID: uuid.NewString(), Attempt: 1},
					BindingDigest: strings.Repeat("a", 64), Actors: []string{strings.Repeat("b", 64), strings.Repeat("d", 64)},
					InitialOwner: strings.Repeat("b", 64), MaxHandoffs: 2, MaxEffects: 2, MaxBytes: 32768, ExpiresAt: time.Now().Add(time.Hour),
				})
				if err != nil {
					test.Fatal(err)
				}
				proposal := store.WorkflowActivationMutation{Kind: store.WorkflowGateProposeHandoff, HandoffID: "handoff", Actor: strings.Repeat("b", 64), Epoch: 1,
					Recipient: strings.Repeat("d", 64), ContextDigest: strings.Repeat("e", 64)}
				gate = mutate(test, first, gate, proposal).Activation
				accept := proposal
				accept.Kind, accept.Actor, accept.AcceptanceDigest = store.WorkflowGateAcceptHandoff, proposal.Recipient, strings.Repeat("f", 64)
				gate = mutate(test, second, gate, accept).Activation
				gate = mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateStop, StopID: "handoff", StopMode: store.WorkflowGateQuiescing, Actor: proposal.Actor, Epoch: 1}).Activation
				gate = mutate(test, first, gate, store.WorkflowActivationMutation{Kind: store.WorkflowGateDrain, StopID: "handoff"}).Activation
				activate := proposal
				activate.Kind, activate.AcceptanceDigest, activate.ExpectedVersion = store.WorkflowGateActivateHandoff, accept.AcceptanceDigest, gate.Version
				cancel := store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateStop, StopID: "cancel", StopMode: store.WorkflowGateStopping}
				activated := false
				switch ordering {
				case "cancel-first":
					mutate(test, first, gate, cancel)
				case "activate-first":
					activatedGate := mutate(test, first, gate, activate)
					activated = activatedGate.OwnerActivated
					mutate(test, second, activatedGate.Activation, cancel)
				case "concurrent":
					type outcome struct {
						result store.WorkflowActivationResult
						err    error
					}
					start := make(chan struct{})
					results := make(chan outcome, 2)
					go func() {
						<-start
						result, err := first.MutateWorkflowActivation(context.Background(), gate.Spec.Key, activate)
						results <- outcome{result, err}
					}()
					go func() {
						<-start
						result, err := second.MutateWorkflowActivation(context.Background(), gate.Spec.Key, cancel)
						results <- outcome{result, err}
					}()
					close(start)
					winners, conflicts := 0, 0
					for range 2 {
						item := <-results
						if item.err == nil {
							winners++
							activated = activated || item.result.OwnerActivated
						} else if errors.Is(item.err, store.ErrConflict) {
							conflicts++
						} else {
							test.Fatalf("unexpected activation race result: %v", item.err)
						}
					}
					if winners != 1 || conflicts != 1 {
						test.Fatalf("activation race: winners=%d conflicts=%d", winners, conflicts)
					}
					retained, err := second.GetWorkflowActivation(context.Background(), gate.Spec.Key)
					if err != nil {
						test.Fatal(err)
					}
					if activated {
						mutate(test, second, retained, cancel)
					}
				}
				retained, err := second.GetWorkflowActivation(context.Background(), gate.Spec.Key)
				if err != nil || retained.Mode != store.WorkflowGateStopping || retained.StopID != "cancel" || retained.Epoch != 3 {
					test.Fatalf("cancellation was lost: %+v %v", retained, err)
				}
				if activated && retained.Owner != proposal.Recipient || !activated && retained.Owner != proposal.Actor {
					test.Fatal("owner activation did not match the winning transition")
				}
				activate.ExpectedVersion = retained.Version
				repeated, err := first.MutateWorkflowActivation(context.Background(), gate.Spec.Key, activate)
				if err != nil && !errors.Is(err, store.ErrConflict) || repeated.OwnerActivated || repeated.DispatchGranted {
					test.Fatal("stale handoff activated a cancelled owner")
				}
			})
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
