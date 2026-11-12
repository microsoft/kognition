// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/store"
)

type invocationBindingStore struct {
	store.Store
	objects map[string][]byte
}

func (s *invocationBindingStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	data, found := s.objects[kind+"/"+namespace+"/"+name]
	if !found {
		return nil, store.ErrNotFound
	}
	return data, nil
}

func TestCandidateBindingIncludesInvocationProfileIdentityAndDigest(t *testing.T) {
	maximum := int64(128)
	profile := api.ModelInvocationProfile{ObjectMeta: api.ObjectMeta{Name: "profile", Namespace: "team-a", UID: "profile-uid"},
		Spec: api.ModelInvocationProfileSpec{Operation: "chat", Defaults: api.InvocationDefaults{MaxCompletionTokens: &maximum}}}
	model := api.Model{ObjectMeta: api.ObjectMeta{Name: "model", Namespace: "team-a", UID: "model-uid", ResourceVersion: 7},
		Spec: api.ModelSpec{InvocationProfileRef: &api.ModelInvocationProfileReference{Name: profile.Name}, InvocationContract: "openai-chat-v1"}}
	model.Spec.InvocationProfileRef.SpecDigest, _ = modelinvocation.Digest(profile.Spec)
	modelJSON, _ := json.Marshal(model)
	profileJSON, _ := json.Marshal(profile)
	storage := &invocationBindingStore{objects: map[string][]byte{"Model/team-a/model": modelJSON, "ModelInvocationProfile/team-a/profile": profileJSON}}
	modelDigest, err := ModelConfigurationDigest(&model)
	if err != nil {
		t.Fatal(err)
	}
	profileDigest, err := modelinvocation.Digest(profile.Spec)
	if err != nil {
		t.Fatal(err)
	}
	binding := api.ModelVersionBinding{Namespace: "team-a", ModelName: "model", ModelUID: model.UID, ModelResourceVersion: 7, SpecDigest: modelDigest}
	if _, err := ResolveModelBinding(context.Background(), storage, binding); err == nil {
		t.Fatal("profile-backed candidate accepted an unbound profile")
	}
	binding.InvocationProfileUID, binding.InvocationProfileDigest = profile.UID, profileDigest
	if _, err := ResolveModelBinding(context.Background(), storage, binding); err != nil {
		t.Fatal(err)
	}
	profile.UID = "replacement"
	storage.objects["ModelInvocationProfile/team-a/profile"], _ = json.Marshal(profile)
	if _, err := ResolveModelBinding(context.Background(), storage, binding); err == nil {
		t.Fatal("profile replacement retained candidate binding")
	}
}

func TestModelSessionCapabilityCarriesExactInvocationBinding(t *testing.T) {
	issuer, err := NewModelSessionCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	digest, _ := modelinvocation.Digest(api.ModelInvocationProfileSpec{Operation: "embedding"})
	binding := ModelSessionCapabilityBinding{
		Namespace: "team-a", AgentName: "agent", AgentIdentityName: "identity", InstanceName: "run",
		InstanceUID: "run-uid", SessionID: "session", ModelName: "model", ModelResourceVersion: 1,
		InvocationProfileBinding: &api.ModelInvocationProfileBinding{Name: "profile", UID: "profile-uid", SpecDigest: digest, Contract: "openai-chat-v1"},
	}
	token, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := issuer.VerifyClaims(token)
	if err != nil || !modelinvocation.EqualBinding(claims.InvocationProfileBinding, binding.InvocationProfileBinding) {
		t.Fatalf("profile claims = %+v, %v", claims, err)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != ModelProfileSessionCapabilityAudience {
		t.Fatalf("profile capability uses a legacy audience: %v", claims.Audience)
	}
	if _, err := jwt.ParseWithClaims(token, &jwt.RegisteredClaims{}, func(*jwt.Token) (any, error) {
		return capabilityKey(), nil
	}, jwt.WithAudience(ModelSessionCapabilityAudience)); err == nil {
		t.Fatal("a legacy profile-unaware gateway would accept the profile capability")
	}
	claims.Audience = jwt.ClaimStrings{ModelSessionCapabilityAudience}
	downgraded, err := issuer.keys.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.VerifyClaims(downgraded); err == nil {
		t.Fatal("profile binding was accepted under a downgraded audience")
	}
	binding.InvocationProfileBinding.UID = ""
	if _, err := issuer.Issue(binding); err == nil {
		t.Fatal("incomplete invocation binding accepted")
	}
}

func TestIndexedModelConfigurationIgnoresStatusVersionButRejectsConfigurationChanges(t *testing.T) {
	model := api.Model{
		ObjectMeta: api.ObjectMeta{Name: "embed", Namespace: "team-a", UID: "model-uid", ResourceVersion: 3},
		Spec:       api.ModelSpec{Provider: "foundry", ModelID: "embedding-deployment"},
	}
	storage := &invocationBindingStore{objects: map[string][]byte{}}
	storage.objects["Model/team-a/embed"], _ = json.Marshal(model)
	binding, err := CaptureModelBinding(context.Background(), storage, &model)
	if err != nil {
		t.Fatal(err)
	}
	model.ResourceVersion++
	storage.objects["Model/team-a/embed"], _ = json.Marshal(model)
	if _, err := ResolveModelConfigurationBinding(context.Background(), storage, *binding); err != nil {
		t.Fatalf("status-only version change invalidated indexed vectors: %v", err)
	}
	if _, err := ResolveModelBinding(context.Background(), storage, *binding); err == nil {
		t.Fatal("strict candidate binding lost its version check")
	}
	model.Spec.ModelID = "different-embedding-space"
	storage.objects["Model/team-a/embed"], _ = json.Marshal(model)
	if _, err := ResolveModelConfigurationBinding(context.Background(), storage, *binding); err == nil {
		t.Fatal("changed embedding configuration retained the indexed-vector binding")
	}
}
