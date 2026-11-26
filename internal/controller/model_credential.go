// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"os"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store"
)

// modelCredentialFingerprint returns the cpi.CredentialFingerprint of the secret
// the Model's calls present, and whether it could be determined. It resolves the
// Model's SecretRef the way the call sites do, so that it equals the fingerprint
// the processes making the calls record: internal secrets from their data
// annotation, environment secrets from the environment.
//
// A Model without a credentialRef presents no secret, which is a known, empty
// fingerprint. A secret that cannot be resolved here, for example an environment
// variable that only the calling process has, is not known: the Model is then
// not attributed any credential failure rather than another account's.
func modelCredentialFingerprint(ctx context.Context, st store.Store, namespace string, model *v1alpha1.Model) (string, bool) {
	if model.Spec.CredentialRef == nil || model.Spec.CredentialRef.Name == "" {
		return "", true
	}
	raw, err := st.Get(ctx, "SecretRef", namespace, model.Spec.CredentialRef.Name)
	if err != nil {
		return "", false
	}
	var secretRef v1alpha1.SecretRef
	if json.Unmarshal(raw, &secretRef) != nil {
		return "", false
	}
	var data map[string]string
	switch secretRef.Spec.ResolvedBackendType() {
	case "internal":
		// A missing annotation is an empty document, which does not parse either.
		if json.Unmarshal([]byte(secretRef.ObjectMeta.Annotations[internalSecretDataAnnotation]), &data) != nil {
			return "", false
		}
	case "env":
		value, ok := os.LookupEnv(secretRef.Spec.ResolvedBackendRef())
		if !ok {
			return "", false
		}
		data = map[string]string{secretRef.Spec.ResolvedKey(): value}
	default:
		return "", false
	}
	return cpi.CredentialFingerprint(&cpi.Credentials{Data: data}), true
}

// modelHealthTargets returns the keys of the passive health evidence that applies
// to a Model served by provider.
func modelHealthTargets(ctx context.Context, st store.Store, namespace, provider string, model *v1alpha1.Model) modelhealth.Targets {
	credential, known := modelCredentialFingerprint(ctx, st, namespace, model)
	return modelhealth.TargetsFor(provider, model.Spec.Endpoint, model.Spec.ModelID, credential, known)
}
