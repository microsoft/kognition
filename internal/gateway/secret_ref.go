// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

const internalSecretDataAnnotation = "kognition.dev/internal-secret-data"

// EnvironmentLookup obtains one environment value for an env-backed SecretRef.
type EnvironmentLookup func(string) (string, bool)

// StoreAPIKeyResolver resolves API keys from SecretRef resources in a Store.
type StoreAPIKeyResolver struct {
	store  store.Store
	lookup EnvironmentLookup
}

// NewStoreAPIKeyResolver creates a SecretRef-backed API key resolver.
func NewStoreAPIKeyResolver(st store.Store, lookup EnvironmentLookup) *StoreAPIKeyResolver {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	return &StoreAPIKeyResolver{store: st, lookup: lookup}
}

// ResolveAPIKey implements APIKeyResolver.
func (r *StoreAPIKeyResolver) ResolveAPIKey(ctx context.Context, namespace, name string) (string, error) {
	raw, err := r.store.Get(ctx, "SecretRef", namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", ErrCredentialUnavailable
		}
		return "", err
	}
	var secretRef v1alpha1.SecretRef
	if err := json.Unmarshal(raw, &secretRef); err != nil {
		return "", err
	}
	return ResolveAPIKeyFromSecretRef(&secretRef, r.lookup)
}

// ResolveAPIKeyFromSecretRef resolves an API key using the currently supported
// SecretRef backends. This intentionally mirrors standalone SecretRef behavior.
func ResolveAPIKeyFromSecretRef(secretRef *v1alpha1.SecretRef, lookup EnvironmentLookup) (string, error) {
	if secretRef == nil {
		return "", ErrCredentialUnavailable
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	switch secretRef.Spec.ResolvedBackendType() {
	case "env":
		value, found := lookup(secretRef.Spec.ResolvedBackendRef())
		if !found || value == "" {
			return "", ErrCredentialUnavailable
		}
		return value, nil
	case "internal":
		if secretRef.ObjectMeta.Annotations == nil {
			return "", ErrCredentialUnavailable
		}
		raw := secretRef.ObjectMeta.Annotations[internalSecretDataAnnotation]
		if raw == "" || secretRef.Spec.ResolvedKey() == "" {
			return "", ErrCredentialUnavailable
		}
		values := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return "", ErrCredentialUnavailable
		}
		value := values[secretRef.Spec.ResolvedKey()]
		if value == "" {
			return "", ErrCredentialUnavailable
		}
		return value, nil
	default:
		return "", ErrCredentialUnavailable
	}
}
