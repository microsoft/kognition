package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kognition-ai/kognition/internal/store"
	"github.com/kognition-ai/kognition/internal/store/workflowactivationtest"
)

func TestKubernetesWorkflowActivationStoreContract(test *testing.T) {
	workflowactivationtest.Run(test, func(test *testing.T) (store.WorkflowActivationStore, store.WorkflowActivationStore) {
		shared := newFakeClient(test)
		clock := func() (time.Time, error) { return time.Now().UTC(), nil }
		first, err := NewWorkflowActivationGateStore(shared, shared, "kognition-control", clock)
		if err != nil {
			test.Fatal(err)
		}
		second, err := NewWorkflowActivationGateStore(shared, shared, "kognition-control", clock)
		if err != nil {
			test.Fatal(err)
		}
		return first, second
	})
}

func TestKubernetesWorkflowActivationStoreRequiresTrustedClock(test *testing.T) {
	shared := newFakeClient(test)
	if _, err := NewWorkflowActivationGateStore(shared, shared, "control", nil); !errors.Is(err, store.ErrConflict) {
		test.Fatal("missing clock boundary accepted")
	}
	cause := errors.New("clock unhealthy")
	storage, err := NewWorkflowActivationGateStore(shared, shared, "control", func() (time.Time, error) { return time.Time{}, cause })
	if err != nil {
		test.Fatal(err)
	}
	if _, err := storage.CreateWorkflowActivation(context.Background(), store.WorkflowActivationSpec{}); !errors.Is(err, cause) {
		test.Fatal("clock health failure was ignored")
	}
}

type workflowActivationLostAcknowledgementClient struct {
	client.Client
	cause error
}

func (writer *workflowActivationLostAcknowledgementClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	if err := writer.Client.Update(ctx, object, options...); err != nil {
		return err
	}
	return writer.cause
}

func TestKubernetesWorkflowActivationLostAcknowledgementCannotReplay(test *testing.T) {
	shared := newFakeClient(test)
	clock := func() (time.Time, error) { return time.Now().UTC(), nil }
	storage, err := NewWorkflowActivationGateStore(shared, shared, "control", clock)
	if err != nil {
		test.Fatal(err)
	}
	gate, err := storage.CreateWorkflowActivation(context.Background(), store.WorkflowActivationSpec{
		Key:           store.WorkflowActivationKey{Workspace: "work", RunUID: "run", Step: "team", ActivationID: "visit", Attempt: 1},
		BindingDigest: strings.Repeat("a", 64), Actors: []string{strings.Repeat("b", 64)}, MaxEffects: 2, MaxBytes: 16384, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		test.Fatal(err)
	}
	effect := store.WorkflowEffect{ID: "invoke", BindingDigest: strings.Repeat("c", 64), Actor: strings.Repeat("b", 64), Plane: "model", Epoch: 1}
	registered, err := storage.MutateWorkflowActivation(context.Background(), gate.Spec.Key, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateRegister, Effect: effect})
	if err != nil {
		test.Fatal(err)
	}
	cause := errors.New("commit acknowledgement lost")
	writer := &workflowActivationLostAcknowledgementClient{Client: shared, cause: cause}
	ambiguous, err := NewWorkflowActivationGateStore(writer, shared, "control", clock)
	if err != nil {
		test.Fatal(err)
	}
	claim := store.WorkflowEffectClaim{Kind: "ModelDispatch", Namespace: "worker", UID: "dispatch", Claimant: "gateway", ProcessNonce: "nonce", FencingToken: 1}
	mutation := store.WorkflowActivationMutation{ExpectedVersion: registered.Activation.Version, Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}
	result, err := ambiguous.MutateWorkflowActivation(context.Background(), gate.Spec.Key, mutation)
	if !errors.Is(err, cause) || result.DispatchGranted {
		test.Fatal("ambiguous write exposed dispatch authority")
	}
	retained, err := storage.GetWorkflowActivation(context.Background(), gate.Spec.Key)
	if err != nil || retained.Effects[effect.ID].State != store.WorkflowEffectCommitted {
		test.Fatalf("committed evidence missing: %+v %v", retained, err)
	}
	mutation.ExpectedVersion = retained.Version
	repeated, err := storage.MutateWorkflowActivation(context.Background(), gate.Spec.Key, mutation)
	if err != nil || repeated.DispatchGranted || repeated.Changed {
		test.Fatal("lost acknowledgement retry created new dispatch authority")
	}
	object := &corev1.ConfigMap{}
	if err := shared.Get(context.Background(), client.ObjectKey{Namespace: "control", Name: workflowActivationObjectName(gate.Spec.Key)}, object); err != nil {
		test.Fatal(err)
	}
	retained.Mode = "ForgedMode"
	encoded, err := json.Marshal(retained)
	if err != nil {
		test.Fatal(err)
	}
	object.Data["activation.json"] = string(encoded)
	if err := shared.Update(context.Background(), object); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.GetWorkflowActivation(context.Background(), gate.Spec.Key); !errors.Is(err, store.ErrConflict) {
		test.Fatal("malformed retained object accepted")
	}
}
