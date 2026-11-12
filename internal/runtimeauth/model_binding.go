// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

func ModelConfigurationDigest(model *v1alpha1.Model) (string, error) {
	if model == nil {
		return "", errors.New("model configuration is required")
	}
	encoded, err := json.Marshal(model.Spec)
	if err != nil {
		return "", errors.New("invalid model configuration")
	}
	canonical, err := runtimeapproval.CanonicalizeJSON(encoded)
	if err != nil {
		return "", errors.New("invalid model configuration")
	}
	digest := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func ResolveModelBinding(ctx context.Context, storage store.Store, binding v1alpha1.ModelVersionBinding) (*v1alpha1.Model, error) {
	return resolveModelBinding(ctx, storage, binding, true)
}

// ResolveModelConfigurationBinding permits status-only Model updates after indexing.
func ResolveModelConfigurationBinding(ctx context.Context, storage store.Store, binding v1alpha1.ModelVersionBinding) (*v1alpha1.Model, error) {
	return resolveModelBinding(ctx, storage, binding, false)
}

func resolveModelBinding(ctx context.Context, storage store.Store, binding v1alpha1.ModelVersionBinding, requireRevision bool) (*v1alpha1.Model, error) {
	if binding.Namespace == "" || binding.ModelName == "" || binding.ModelUID == "" || binding.ModelResourceVersion <= 0 || len(binding.SpecDigest) != 71 {
		return nil, errors.New("incomplete model version binding")
	}
	raw, err := storage.Get(ctx, "Model", binding.Namespace, binding.ModelName)
	if err != nil {
		return nil, errors.New("candidate Model is unavailable")
	}
	var model v1alpha1.Model
	if json.Unmarshal(raw, &model) != nil || model.ObjectMeta.DeletionTimestamp != nil || model.ObjectMeta.UID != binding.ModelUID ||
		model.ObjectMeta.Namespace != binding.Namespace || model.ObjectMeta.Name != binding.ModelName ||
		(requireRevision && model.ObjectMeta.ResourceVersion != binding.ModelResourceVersion) {
		return nil, errors.New("candidate Model revision does not match")
	}
	digest, err := ModelConfigurationDigest(&model)
	if err != nil || digest != binding.SpecDigest {
		return nil, errors.New("candidate Model digest does not match")
	}
	profile, err := modelinvocation.Load(ctx, storage, &model)
	if err != nil {
		return nil, err
	}
	if profile == nil {
		if binding.InvocationProfileUID != "" || binding.InvocationProfileDigest != "" {
			return nil, errors.New("candidate invocation profile is no longer bound")
		}
	} else if binding.InvocationProfileUID != profile.Binding.UID || binding.InvocationProfileDigest != profile.Binding.SpecDigest {
		return nil, errors.New("candidate invocation profile identity or digest does not match")
	}
	return &model, nil
}

func CaptureModelBinding(ctx context.Context, storage store.Store, model *v1alpha1.Model) (*v1alpha1.ModelVersionBinding, error) {
	if model == nil || model.Namespace == "" || model.Name == "" || model.UID == "" || model.ResourceVersion < 1 || model.DeletionTimestamp != nil {
		return nil, errors.New("live Model identity is required")
	}
	digest, err := ModelConfigurationDigest(model)
	if err != nil {
		return nil, err
	}
	binding := &v1alpha1.ModelVersionBinding{
		Namespace: model.Namespace, ModelName: model.Name, ModelUID: model.UID,
		ModelResourceVersion: model.ResourceVersion, SpecDigest: digest,
	}
	profile, err := modelinvocation.Load(ctx, storage, model)
	if err != nil {
		return nil, err
	}
	if profile != nil {
		binding.InvocationProfileUID, binding.InvocationProfileDigest = profile.Binding.UID, profile.Binding.SpecDigest
	}
	return binding, nil
}
