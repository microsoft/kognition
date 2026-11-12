// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway

import (
	"context"
	"encoding/json"
	"errors"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/store"
)

func resolveModelCredentials(ctx context.Context, storage store.Store, namespace string, model *api.Model, provider string) (*cpi.Credentials, error) {
	if model.Spec.CredentialRef == nil || model.Spec.CredentialRef.Name == "" {
		if provider == "ollama" {
			return nil, nil
		}
		return nil, errors.New("model gateway requires a credential SecretRef")
	}
	secret, err := loadResource[api.SecretRef](ctx, storage, "SecretRef", namespace, model.Spec.CredentialRef.Name)
	if err != nil || secret.DeletionTimestamp != nil || secret.Spec.ResolvedBackendType() != "internal" || secret.Annotations == nil {
		return nil, errors.New("model credential is not an available internal SecretRef")
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(secret.Annotations["kognition.dev/internal-secret-data"]), &values); err != nil || values == nil {
		return nil, errors.New("model credential is not a valid internal SecretRef")
	}
	kind := values["type"]
	if kind == "" && values["credentialType"] != "" {
		kind = "azure-identity"
	}
	if (provider == "openai-compatible" && (kind != "api-key" || values["apiKey"] == "")) ||
		(provider == "foundry" && !((kind == "api-key" && values["apiKey"] != "") || kind == "azure-identity")) {
		return nil, errors.New("model credential does not support the selected adapter")
	}
	return &cpi.Credentials{Type: kind, Data: values}, nil
}
