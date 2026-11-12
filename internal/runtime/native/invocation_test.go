// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package native

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/openai"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/store"
)

type profileDriver struct{ fakeDriver }

func (d *profileDriver) InvocationContract(name string) (modelinvocation.Contract, error) {
	return openai.InvocationContract(name)
}

type profileReader struct {
	store.Store
	profile api.ModelInvocationProfile
}

func (s *profileReader) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	if kind != "ModelInvocationProfile" || namespace != s.profile.Namespace || name != s.profile.Name {
		return nil, store.ErrNotFound
	}
	return json.Marshal(s.profile)
}

func TestNativeProfileBindingSurvivesCheckpointWithoutAdoptingReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	maximum := int64(128)
	storage := &profileReader{profile: api.ModelInvocationProfile{
		ObjectMeta: api.ObjectMeta{Name: "profile-v1", Namespace: "tenant", UID: "profile-uid"},
		Spec:       api.ModelInvocationProfileSpec{Operation: "chat", Defaults: api.InvocationDefaults{MaxCompletionTokens: &maximum}},
	}}
	model := &api.Model{ObjectMeta: api.ObjectMeta{Name: "model", Namespace: "tenant"},
		Spec: api.ModelSpec{InvocationProfileRef: &api.ModelInvocationProfileReference{Name: "profile-v1"}, InvocationContract: "openai-chat-v1"}}
	model.Spec.InvocationProfileRef.SpecDigest, _ = modelinvocation.Digest(storage.profile.Spec)
	driver := &profileDriver{}
	config := Config{Driver: driver, DriverModel: "opaque-deployment", Store: storage, Model: model, Agent: newTestAgent("system")}
	adapter := New(config)
	session, err := adapter.CreateSession(ctx, runtime.CreateSessionRequest{Namespace: "tenant", AgentName: "worker", InitialCredits: 32})
	if err != nil {
		t.Fatal(err)
	}
	first, err := adapter.Step(ctx, runtime.StepRequest{SessionID: session.SessionID, Input: "first"})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range drainEvents(t, first, 5*time.Second) {
		if event.Error != nil {
			t.Fatal(event.Error.Message)
		}
	}
	checkpoint, err := adapter.Checkpoint(ctx, session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	fresh := New(config)
	resumed, err := fresh.Resume(ctx, checkpoint.Token)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fresh.Step(ctx, runtime.StepRequest{SessionID: resumed.SessionID, Input: "second"})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range drainEvents(t, second, 5*time.Second) {
		if event.Error != nil {
			t.Fatalf("same-profile checkpoint lost its binding: %s", event.Error.Message)
		}
	}
	storage.profile.UID = "recreated-profile"
	if _, err := New(config).Resume(ctx, checkpoint.Token); !errors.Is(err, runtime.ErrInvalidCheckpoint) {
		t.Fatalf("checkpoint adopted a replacement profile: %v", err)
	}
}
