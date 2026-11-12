// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

const internalSecretDataAnnotation = "kognition.dev/internal-secret-data"

type runtimeSecretResolver interface {
	ResolveAgentRuntimeSecrets(ctx context.Context, namespace string, instance *v1alpha1.AgentInstance) (map[string]string, error)
}

type storeSecretResolver struct {
	store     store.Store
	lookupEnv func(string) (string, bool)
}

func newStoreSecretResolver(s store.Store) *storeSecretResolver {
	return &storeSecretResolver{store: s, lookupEnv: os.LookupEnv}
}

func (r *storeSecretResolver) ResolveAgentRuntimeSecrets(ctx context.Context, namespace string, instance *v1alpha1.AgentInstance) (map[string]string, error) {
	agent, err := ResolveExecutionAgent(ctx, r.store, namespace, instance)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("resolve agent runtime secrets: %w", err)
	}

	resolved := make(map[string]string)

	model, err := getStoredResource[v1alpha1.Model](ctx, r.store, "Model", namespace, agent.Spec.ModelRef.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return resolved, nil
		}
		return nil, fmt.Errorf("resolve model for agent %s: %w", agent.ObjectMeta.Name, err)
	}
	if model.Spec.CredentialRef != nil && model.Spec.CredentialRef.Name != "" {
		secretRef, err := getStoredResource[v1alpha1.SecretRef](ctx, r.store, "SecretRef", namespace, model.Spec.CredentialRef.Name)
		if err == nil {
			value, err := r.resolveSecretValue(secretRef)
			if err == nil {
				resolved["model."+secretRef.Spec.ResolvedKey()] = value
			}
			// If resolveSecretValue fails, skip this secret (non-fatal).
		}
		// If SecretRef lookup fails (e.g. unsupported kind in K8s mode), skip (non-fatal).
	}

	for _, toolRef := range agent.Spec.Tools {
		tool, err := getStoredResource[v1alpha1.Tool](ctx, r.store, "Tool", namespace, toolRef.Ref.Name)
		if err != nil {
			continue
		}
		for _, secretBinding := range tool.Spec.SecretRefs {
			secretRef, err := getStoredResource[v1alpha1.SecretRef](ctx, r.store, "SecretRef", namespace, secretBinding.Name)
			if err != nil {
				continue
			}
			value, err := r.resolveSecretValue(secretRef)
			if err != nil {
				continue
			}
			resolved["tool."+tool.ObjectMeta.Name+"."+secretBinding.Key] = value
		}
	}

	return resolved, nil
}

func (r *storeSecretResolver) resolveSecretValue(secretRef *v1alpha1.SecretRef) (string, error) {
	switch secretRef.Spec.ResolvedBackendType() {
	case "env":
		value, ok := r.lookupEnv(secretRef.Spec.ResolvedBackendRef())
		if !ok {
			return "", fmt.Errorf("environment variable %q is not set", secretRef.Spec.ResolvedBackendRef())
		}
		return value, nil
	case "internal":
		if secretRef.ObjectMeta.Annotations == nil {
			return "", fmt.Errorf("annotation %q is missing", internalSecretDataAnnotation)
		}
		raw := secretRef.ObjectMeta.Annotations[internalSecretDataAnnotation]
		if raw == "" {
			return "", fmt.Errorf("annotation %q is missing", internalSecretDataAnnotation)
		}
		var values map[string]string
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return "", fmt.Errorf("parse annotation %q: %w", internalSecretDataAnnotation, err)
		}
		value, ok := values[secretRef.Spec.ResolvedKey()]
		if !ok {
			return "", fmt.Errorf("internal secret key %q is not present", secretRef.Spec.ResolvedKey())
		}
		return value, nil
	default:
		return "", fmt.Errorf("secret backend %q is not supported", secretRef.Spec.ResolvedBackendType())
	}
}

func getStoredResource[T any](ctx context.Context, s store.Store, kind, namespace, name string) (*T, error) {
	raw, err := s.Get(ctx, kind, namespace, name)
	if err != nil {
		return nil, err
	}
	var obj T
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("unmarshal %s %s/%s: %w", kind, namespace, name, err)
	}
	return &obj, nil
}
