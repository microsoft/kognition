// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestAgentIdentityValidatorReservesAutomaticProvenance(t *testing.T) {
	validator := NewAgentIdentityValidator("system:serviceaccount:kognition-system:kognition")
	base := v1alpha1.AgentIdentity{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "manual-identity", Namespace: "team-a"},
		Spec:       v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}},
	}
	tests := []struct {
		name    string
		mutate  func(*v1alpha1.AgentIdentity)
		user    string
		allowed bool
	}{
		{name: "manual identity", allowed: true},
		{name: "automatic name", mutate: func(identity *v1alpha1.AgentIdentity) { identity.ObjectMeta.Name = "agent-identity-planner" }},
		{name: "automatic label", mutate: func(identity *v1alpha1.AgentIdentity) {
			identity.ObjectMeta.Labels = map[string]string{automaticAgentIdentityLabel: "planner"}
		}},
		{name: "owner reference", mutate: func(identity *v1alpha1.AgentIdentity) {
			identity.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{Kind: "Agent", Name: "planner", UID: "current-uid"}}
		}},
		{name: "ready status", mutate: func(identity *v1alpha1.AgentIdentity) { identity.Status.Phase = v1alpha1.PhaseReady }},
		{name: "controller automatic identity", user: "system:serviceaccount:kognition-system:kognition", allowed: true, mutate: func(identity *v1alpha1.AgentIdentity) {
			identity.ObjectMeta.Name = "agent-identity-planner"
			identity.ObjectMeta.Labels = map[string]string{automaticAgentIdentityLabel: "planner"}
			identity.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{Kind: "Agent", Name: "planner", UID: "current-uid"}}
			identity.Status.Phase = v1alpha1.PhaseReady
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity := base
			if test.mutate != nil {
				test.mutate(&identity)
			}
			data, err := json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			response := validator.Validate(context.Background(), &Request{
				Operation: OperationCreate,
				Kind:      "AgentIdentity",
				Object:    data,
				UserInfo:  UserInfo{Username: test.user},
			})
			if response.Allowed != test.allowed {
				t.Fatalf("Allowed = %v, reason = %q, want %v", response.Allowed, response.Reason, test.allowed)
			}
		})
	}
}

func TestAgentIdentityValidatorAllowsManualUpdateWithUnchangedReadyStatus(t *testing.T) {
	validator := NewAgentIdentityValidator("system:serviceaccount:kognition-system:kognition")
	previous := v1alpha1.AgentIdentity{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "manual-identity", Namespace: "team-a"},
		Spec:       v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}, SPIFFEID: "spiffe://kognition.dev/workspace/team-a/agent/planner"},
		Status:     v1alpha1.AgentIdentityStatus{Phase: v1alpha1.PhaseReady},
	}
	current := previous
	current.ObjectMeta.Annotations = map[string]string{"example.test/note": "updated"}
	previousData, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	currentData, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "AgentIdentity", Object: currentData, OldObject: previousData})
	if !response.Allowed {
		t.Fatalf("manual metadata update denied: %q", response.Reason)
	}

	current.Status.Phase = v1alpha1.PhaseFailed
	currentData, err = json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	response = validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "AgentIdentity", Object: currentData, OldObject: previousData})
	if response.Allowed {
		t.Fatal("manual status mutation was allowed")
	}
}

func TestAgentIdentityValidatorRestrictsAutomaticIdentityDeletion(t *testing.T) {
	validator := NewAgentIdentityValidator("system:serviceaccount:kognition-system:kognition")
	identity := v1alpha1.AgentIdentity{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-identity-planner", Namespace: "team-a", Labels: map[string]string{automaticAgentIdentityLabel: "planner"}},
		Spec:       v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}},
	}
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	response := validator.Validate(context.Background(), &Request{Operation: OperationDelete, Kind: "AgentIdentity", OldObject: data, UserInfo: UserInfo{Username: "tenant"}})
	if response.Allowed {
		t.Fatal("tenant automatic identity deletion was allowed")
	}
	response = validator.Validate(context.Background(), &Request{Operation: OperationDelete, Kind: "AgentIdentity", OldObject: data, UserInfo: UserInfo{Username: "system:serviceaccount:kognition-system:kognition"}})
	if !response.Allowed {
		t.Fatalf("controller automatic identity deletion denied: %q", response.Reason)
	}
}
