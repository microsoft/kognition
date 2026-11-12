// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestSecretRefValidator_RejectsMissingResolvedFields(t *testing.T) {
	v := NewSecretRefValidator()
	secret := v1alpha1.SecretRef{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "SecretRef"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "bad-secretref",
			Namespace: "default",
		},
		Spec: v1alpha1.SecretRefSpec{BackendType: "internal", BackendRef: "secret/app"},
	}
	data, _ := json.Marshal(secret)
	req := &Request{Operation: OperationCreate, Kind: "SecretRef", Namespace: "default", Name: "bad-secretref", Object: data}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for missing key")
	}
	if resp.Reason == "" {
		t.Fatal("expected denial reason")
	}
}

func TestSecretRefValidator_AcceptsLegacyFallbackFields(t *testing.T) {
	v := NewSecretRefValidator()
	secret := v1alpha1.SecretRef{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "SecretRef"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "good-secretref",
			Namespace: "default",
		},
		Spec: v1alpha1.SecretRefSpec{Provider: "env", Path: "OPENAI_API_KEY", LegacyKey: "token"},
	}
	data, _ := json.Marshal(secret)
	req := &Request{Operation: OperationCreate, Kind: "SecretRef", Namespace: "default", Name: "good-secretref", Object: data}

	resp := v.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected valid secretRef to be accepted, got reason: %s", resp.Reason)
	}
}

// TC-0034: Unsupported SecretRef backend must be rejected by admission.
func TestSecretRefValidator_RejectsUnsupportedBackendType(t *testing.T) {
	v := NewSecretRefValidator()
	secret := v1alpha1.SecretRef{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "SecretRef"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "vault-secretref",
			Namespace: "default",
		},
		Spec: v1alpha1.SecretRefSpec{BackendType: "vault", BackendRef: "kv/team/app", Key: "token"},
	}
	data, _ := json.Marshal(secret)
	req := &Request{Operation: OperationCreate, Kind: "SecretRef", Namespace: "default", Name: "vault-secretref", Object: data}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for unsupported backend type")
	}
	if resp.Reason == "" {
		t.Fatal("expected denial reason")
	}
}
