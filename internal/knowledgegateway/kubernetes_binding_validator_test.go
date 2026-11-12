// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package knowledgegateway

import (
	"context"
	"errors"
	"testing"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubernetesCapabilityBindingValidator(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	claims := &runtimeauth.KnowledgeSessionCapabilityClaims{
		Namespace: "team-a", AgentName: "planner", AgentUID: "agent-uid",
		AgentIdentityName: "planner-identity", AgentIdentityUID: "identity-uid", KnowledgeBases: []string{"docs"},
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1",
	}
	validAgent := &k8s.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: claims.AgentName, Namespace: claims.Namespace, UID: types.UID(claims.AgentUID)},
		Spec:       k8s.AgentSpec{KnowledgeBaseClaims: []k8s.KnowledgeBaseClaimRef{{Name: "docs-access"}}},
	}
	validInstance := &k8s.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: claims.InstanceName, Namespace: claims.Namespace, UID: types.UID(claims.InstanceUID)},
		Spec:       k8s.AgentInstanceSpec{AgentRef: k8s.LocalObjectReference{Name: claims.AgentName}},
		Status:     k8s.AgentInstanceStatus{Phase: string(api.PhaseRunning), SessionID: claims.SessionID},
	}
	validIdentity := &k8s.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: claims.AgentIdentityName, Namespace: claims.Namespace, UID: types.UID(claims.AgentIdentityUID)},
		Spec:       k8s.AgentIdentitySpec{AgentRef: k8s.LocalObjectReference{Name: claims.AgentName}},
		Status:     k8s.AgentIdentityStatus{Phase: string(api.PhaseReady)},
	}
	validClaim := &k8s.KnowledgeBaseClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "docs-access", Namespace: claims.Namespace, Generation: 1},
		Spec:       k8s.KnowledgeBaseClaimSpec{KnowledgeBaseName: "docs"},
		Status:     k8s.KnowledgeBaseClaimStatus{Phase: string(api.PhaseBound), BoundKnowledgeBase: "docs", ObservedGeneration: 1},
	}

	tests := []struct {
		name     string
		mutate   func(*k8s.AgentInstance, *k8s.AgentIdentity)
		objects  func(*k8s.AgentInstance, *k8s.AgentIdentity) []ctrlclient.Object
		accepted bool
	}{
		{name: "active binding", accepted: true},
		{name: "instance pending deletion", mutate: func(instance *k8s.AgentInstance, _ *k8s.AgentIdentity) {
			deletedAt := metav1.Now()
			instance.DeletionTimestamp = &deletedAt
			instance.Finalizers = []string{"kognition.dev/runtime-cleanup"}
		}},
		{name: "instance UID changed", mutate: func(instance *k8s.AgentInstance, _ *k8s.AgentIdentity) {
			instance.ObjectMeta.UID = types.UID("replacement-uid")
		}},
		{name: "agent changed", mutate: func(instance *k8s.AgentInstance, _ *k8s.AgentIdentity) { instance.Spec.AgentRef.Name = "other" }},
		{name: "instance no longer running", mutate: func(instance *k8s.AgentInstance, _ *k8s.AgentIdentity) {
			instance.Status.Phase = string(api.PhaseSucceeded)
		}},
		{name: "session changed", mutate: func(instance *k8s.AgentInstance, _ *k8s.AgentIdentity) { instance.Status.SessionID = "session-2" }},
		{name: "identity not ready", mutate: func(_ *k8s.AgentInstance, identity *k8s.AgentIdentity) {
			identity.Status.Phase = string(api.PhasePending)
		}},
		{name: "identity belongs to another agent", mutate: func(_ *k8s.AgentInstance, identity *k8s.AgentIdentity) { identity.Spec.AgentRef.Name = "other" }},
		{name: "instance deleted", objects: func(_ *k8s.AgentInstance, identity *k8s.AgentIdentity) []ctrlclient.Object {
			return []ctrlclient.Object{identity}
		}},
		{name: "identity deleted", objects: func(instance *k8s.AgentInstance, _ *k8s.AgentIdentity) []ctrlclient.Object {
			return []ctrlclient.Object{instance}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance := validInstance.DeepCopy()
			identity := validIdentity.DeepCopy()
			if test.mutate != nil {
				test.mutate(instance, identity)
			}
			objects := []ctrlclient.Object{validAgent.DeepCopy(), instance, identity, validClaim.DeepCopy()}
			if test.objects != nil {
				objects = append([]ctrlclient.Object{validAgent.DeepCopy(), validClaim.DeepCopy()}, test.objects(instance, identity)...)
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			validator, err := NewKubernetesCapabilityBindingValidator(reader)
			if err != nil {
				t.Fatal(err)
			}
			err = validator.Validate(context.Background(), claims, "docs")
			if test.accepted && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if !test.accepted && !errors.Is(err, ErrInactiveCapabilityBinding) {
				t.Fatalf("Validate() error = %v, want inactive binding", err)
			}
		})
	}
}

func TestKubernetesCapabilityBindingValidatorRejectsRevokedKnowledgeAccess(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	claims := &runtimeauth.KnowledgeSessionCapabilityClaims{
		Namespace: "team-a", AgentName: "planner", AgentUID: "agent-uid",
		AgentIdentityName: "planner-identity", AgentIdentityUID: "identity-uid",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1",
		KnowledgeBases: []string{"docs"},
	}
	newObjects := func() (*k8s.Agent, *k8s.AgentIdentity, *k8s.AgentInstance, *k8s.KnowledgeBaseClaim) {
		return &k8s.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: claims.AgentName, Namespace: claims.Namespace, UID: types.UID(claims.AgentUID)},
				Spec:       k8s.AgentSpec{KnowledgeBaseClaims: []k8s.KnowledgeBaseClaimRef{{Name: "docs-access"}}},
			}, &k8s.AgentIdentity{
				ObjectMeta: metav1.ObjectMeta{Name: claims.AgentIdentityName, Namespace: claims.Namespace, UID: types.UID(claims.AgentIdentityUID)},
				Spec:       k8s.AgentIdentitySpec{AgentRef: k8s.LocalObjectReference{Name: claims.AgentName}},
				Status:     k8s.AgentIdentityStatus{Phase: string(api.PhaseReady)},
			}, &k8s.AgentInstance{
				ObjectMeta: metav1.ObjectMeta{Name: claims.InstanceName, Namespace: claims.Namespace, UID: types.UID(claims.InstanceUID)},
				Spec:       k8s.AgentInstanceSpec{AgentRef: k8s.LocalObjectReference{Name: claims.AgentName}},
				Status:     k8s.AgentInstanceStatus{Phase: string(api.PhaseRunning), SessionID: claims.SessionID},
			}, &k8s.KnowledgeBaseClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "docs-access", Namespace: claims.Namespace, Generation: 2},
				Spec:       k8s.KnowledgeBaseClaimSpec{KnowledgeBaseName: "docs"},
				Status:     k8s.KnowledgeBaseClaimStatus{Phase: string(api.PhaseBound), BoundKnowledgeBase: "docs", ObservedGeneration: 2},
			}
	}

	tests := []struct {
		name   string
		mutate func(*k8s.Agent, *k8s.AgentIdentity, *k8s.KnowledgeBaseClaim)
		claim  bool
	}{
		{name: "Agent replaced", claim: true, mutate: func(agent *k8s.Agent, _ *k8s.AgentIdentity, _ *k8s.KnowledgeBaseClaim) {
			agent.UID = "replacement-agent-uid"
		}},
		{name: "identity replaced", claim: true, mutate: func(_ *k8s.Agent, identity *k8s.AgentIdentity, _ *k8s.KnowledgeBaseClaim) {
			identity.UID = "replacement-identity-uid"
		}},
		{name: "claim reference removed", claim: true, mutate: func(agent *k8s.Agent, _ *k8s.AgentIdentity, _ *k8s.KnowledgeBaseClaim) {
			agent.Spec.KnowledgeBaseClaims = nil
		}},
		{name: "claim deleted"},
		{name: "claim retargeted", claim: true, mutate: func(_ *k8s.Agent, _ *k8s.AgentIdentity, claim *k8s.KnowledgeBaseClaim) {
			claim.Spec.KnowledgeBaseName = "other"
		}},
		{name: "claim unbound", claim: true, mutate: func(_ *k8s.Agent, _ *k8s.AgentIdentity, claim *k8s.KnowledgeBaseClaim) {
			claim.Status.Phase = string(api.PhasePending)
		}},
		{name: "claim binding changed", claim: true, mutate: func(_ *k8s.Agent, _ *k8s.AgentIdentity, claim *k8s.KnowledgeBaseClaim) {
			claim.Status.BoundKnowledgeBase = "other"
		}},
		{name: "claim status stale", claim: true, mutate: func(_ *k8s.Agent, _ *k8s.AgentIdentity, claim *k8s.KnowledgeBaseClaim) {
			claim.Status.ObservedGeneration--
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent, identity, instance, claim := newObjects()
			if test.mutate != nil {
				test.mutate(agent, identity, claim)
			}
			objects := []ctrlclient.Object{agent, identity, instance}
			if test.claim {
				objects = append(objects, claim)
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			validator, err := NewKubernetesCapabilityBindingValidator(reader)
			if err != nil {
				t.Fatal(err)
			}
			if err := validator.Validate(context.Background(), claims, "docs"); !errors.Is(err, ErrInactiveCapabilityBinding) {
				t.Fatalf("Validate() error = %v, want inactive binding", err)
			}
		})
	}
}
