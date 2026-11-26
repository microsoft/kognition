// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/store"
)

const modelKind = "Model"

// ModelReconciler validates model connectivity and reports runtime capabilities.
type ModelReconciler struct {
	store    store.Store
	registry *cpi.Registry
	logger   *zap.Logger
}

func NewModelReconciler(s store.Store, registry *cpi.Registry, logger *zap.Logger) *ModelReconciler {
	return &ModelReconciler{
		store:    s,
		registry: registry,
		logger:   logger.With(zap.String("reconciler", "model")),
	}
}

func (r *ModelReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	raw, err := r.store.Get(ctx, modelKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get model: %w", err)
	}

	var model v1alpha1.Model
	if err := json.Unmarshal(raw, &model); err != nil {
		return Result{}, fmt.Errorf("unmarshal model: %w", err)
	}

	oldPhase := model.Status.Phase
	oldAvailable := model.Status.Available
	var oldReason, oldMessage string
	if existing := lifecycle.GetCondition(apiToLifecycleConditions(model.Status.Conditions), lifecycle.ConditionReady); existing != nil {
		oldReason = existing.Reason
		oldMessage = existing.Message
	}

	available, latency, reason, message := r.evaluate(ctx, req.Namespace, &model)
	// Probe results can carry provider-controlled text; make it safe to persist.
	// The Ready condition requires a nonempty reason, unlike ModelClass's empty-reason sentinel.
	fallbackReason := "ProbeFailed"
	if available {
		fallbackReason = "ProbeSucceeded"
	}
	if reason == "" {
		reason = fallbackReason
	}
	reason = sanitizeConditionReason(reason, fallbackReason)
	message = sanitizeConditionMessage(message)
	model.Status.Available = available
	model.Status.Latency = latency.String()
	model.Status.LastCheck = time.Now().UTC().Format(time.RFC3339)
	model.Status.Capabilities = append([]string(nil), model.Spec.Capabilities...)

	readyStatus := lifecycle.ConditionFalse
	model.Status.Phase = v1alpha1.PhaseFailed
	if available {
		readyStatus = lifecycle.ConditionTrue
		model.Status.Phase = v1alpha1.PhaseReady
	}
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(model.Status.Conditions),
		lifecycle.ConditionReady,
		readyStatus,
		reason,
		message,
	)
	model.Status.Conditions = lifecycleToAPIConditions(lc)

	// Only persist if status actually changed to avoid triggering pg_notify loops.
	result := Result{}
	if model.Spec.InvocationProfileRef != nil {
		result.RequeueAfter = 30 * time.Second
	}
	if model.Status.Phase == oldPhase && model.Status.Available == oldAvailable &&
		oldReason == reason && oldMessage == message {
		return result, nil
	}

	if err := r.updateModel(ctx, &model); err != nil {
		return Result{}, fmt.Errorf("update model: %w", err)
	}
	return result, nil
}

func (r *ModelReconciler) evaluate(ctx context.Context, namespace string, model *v1alpha1.Model) (bool, time.Duration, string, string) {
	start := time.Now()

	provider := model.Spec.Provider
	var modelClass *v1alpha1.ModelClass
	if model.Spec.ClassName != "" {
		raw, err := r.store.Get(ctx, "ModelClass", "", model.Spec.ClassName)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return false, time.Since(start), "ModelClassMissing", fmt.Sprintf("modelClass %q was not found", model.Spec.ClassName)
			}
			return false, time.Since(start), "ModelClassLookupFailed", fmt.Sprintf("modelClass lookup failed: %v", err)
		}
		var resolved v1alpha1.ModelClass
		if err := json.Unmarshal(raw, &resolved); err != nil {
			return false, time.Since(start), "ModelClassLookupFailed", fmt.Sprintf("modelClass unmarshal failed: %v", err)
		}
		classProvider := resolved.Spec.Provisioner
		if provider != "" && cpi.CanonicalProvisioner(provider) != cpi.CanonicalProvisioner(classProvider) {
			return false, time.Since(start), "ProviderConflict", fmt.Sprintf(
				"provider %q does not match modelClass %q provisioner %q",
				model.Spec.Provider,
				model.Spec.ClassName,
				resolved.Spec.Provisioner,
			)
		}
		provider = classProvider
		modelClass = &resolved
	}
	if provider == "" {
		provider = "openai-compatible"
	}

	driver, ok := r.registry.Get(provider)
	if !ok {
		return false, time.Since(start), "ProviderUnsupported", fmt.Sprintf("no CPI driver registered for provider %q", provider)
	}
	if _, err := cpi.ResolveInvocation(ctx, r.store, driver, model); err != nil {
		return false, time.Since(start), "InvocationProfileInvalid", err.Error()
	}

	// Validate configuration.
	var modelClassSpec *v1alpha1.ModelClassSpec
	if modelClass != nil {
		modelClassSpec = &modelClass.Spec
	}
	if err := driver.Validate(ctx, &model.Spec, modelClassSpec); err != nil {
		return false, time.Since(start), "ValidationFailed", err.Error()
	}

	// Resolve credentials if referenced.
	var creds *cpi.Credentials
	if model.Spec.CredentialRef != nil && model.Spec.CredentialRef.Name != "" {
		raw, err := r.store.Get(ctx, "SecretRef", namespace, model.Spec.CredentialRef.Name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return false, time.Since(start), "CredentialMissing", fmt.Sprintf("credentialRef %q was not found", model.Spec.CredentialRef.Name)
			}
			return false, time.Since(start), "CredentialLookupFailed", fmt.Sprintf("credentialRef lookup failed: %v", err)
		}
		var secretRef v1alpha1.SecretRef
		if err := json.Unmarshal(raw, &secretRef); err != nil {
			return false, time.Since(start), "CredentialLookupFailed", fmt.Sprintf("credentialRef unmarshal failed: %v", err)
		}
		secretData := make(map[string]string)
		if ann, ok := secretRef.ObjectMeta.Annotations[internalSecretDataAnnotation]; ok && ann != "" {
			_ = json.Unmarshal([]byte(ann), &secretData)
		}
		credType := secretData["type"]
		if credType == "" {
			credType = "api-key"
		}
		creds = &cpi.Credentials{Type: credType, Data: secretData}
	}

	// Probe endpoint health.
	health, err := driver.Probe(ctx, &model.Spec, creds)
	if err != nil {
		return false, time.Since(start), "ProbeFailed", err.Error()
	}
	if !health.Available {
		return false, time.Since(start), health.Reason, health.Message
	}

	if len(model.Spec.Capabilities) == 0 {
		if len(health.Capabilities) > 0 {
			model.Spec.Capabilities = health.Capabilities
		} else {
			model.Spec.Capabilities = []string{"chat"}
		}
	}

	return true, time.Since(start), health.Reason, health.Message
}

func (r *ModelReconciler) updateModel(ctx context.Context, model *v1alpha1.Model) error {
	data, err := json.Marshal(model)
	if err != nil {
		return fmt.Errorf("marshal model: %w", err)
	}
	return r.store.Update(ctx, modelKind, model.ObjectMeta.Namespace, model.ObjectMeta.Name, data, model.ObjectMeta.ResourceVersion)
}
