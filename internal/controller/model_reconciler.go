// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store"
)

const modelKind = "Model"

// ModelReconciler validates model connectivity and reports runtime capabilities.
// When the store can share passive health, it also degrades a Model whose real
// calls are failing, even though its active probe still passes.
type ModelReconciler struct {
	store      store.Store
	callHealth store.ModelCallHealthStore
	registry   *cpi.Registry
	logger     *zap.Logger
}

func NewModelReconciler(s store.Store, registry *cpi.Registry, logger *zap.Logger) *ModelReconciler {
	callHealth, _ := s.(store.ModelCallHealthStore)
	return &ModelReconciler{
		store:      s,
		callHealth: callHealth,
		registry:   registry,
		logger:     logger.With(zap.String("reconciler", "model")),
	}
}

// SetCallHealth enables passive model health from a store shared with the
// processes that make model calls. Call it before the reconciler starts.
func (r *ModelReconciler) SetCallHealth(health store.ModelCallHealthStore) {
	r.callHealth = health
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

	provider, modelClass, failure := resolveModelProvider(ctx, r.store, model)
	if failure != nil {
		return false, time.Since(start), failure.reason, failure.message
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

	if verdict := r.passiveVerdict(ctx, namespace, provider, model); verdict.Degraded {
		return false, time.Since(start), verdict.Reason, verdict.Message
	}
	return true, time.Since(start), health.Reason, health.Message
}

// modelFailure is the reason and message of a Model that cannot be evaluated.
type modelFailure struct {
	reason  string
	message string
}

// resolveModelProvider returns the provisioner that serves model and its
// ModelClass, if it has one. The failure is set when the ModelClass cannot be
// resolved or disagrees with the Model's provider.
func resolveModelProvider(ctx context.Context, st store.Store, model *v1alpha1.Model) (string, *v1alpha1.ModelClass, *modelFailure) {
	provider := model.Spec.Provider
	var modelClass *v1alpha1.ModelClass
	if model.Spec.ClassName != "" {
		raw, err := st.Get(ctx, "ModelClass", "", model.Spec.ClassName)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return "", nil, &modelFailure{"ModelClassMissing", fmt.Sprintf("modelClass %q was not found", model.Spec.ClassName)}
			}
			return "", nil, &modelFailure{"ModelClassLookupFailed", fmt.Sprintf("modelClass lookup failed: %v", err)}
		}
		var resolved v1alpha1.ModelClass
		if err := json.Unmarshal(raw, &resolved); err != nil {
			return "", nil, &modelFailure{"ModelClassLookupFailed", fmt.Sprintf("modelClass unmarshal failed: %v", err)}
		}
		classProvider := resolved.Spec.Provisioner
		if provider != "" && cpi.CanonicalProvisioner(provider) != cpi.CanonicalProvisioner(classProvider) {
			return "", nil, &modelFailure{"ProviderConflict", fmt.Sprintf(
				"provider %q does not match modelClass %q provisioner %q",
				model.Spec.Provider,
				model.Spec.ClassName,
				resolved.Spec.Provisioner,
			)}
		}
		provider = classProvider
		modelClass = &resolved
	}
	if provider == "" {
		provider = "openai-compatible"
	}
	return provider, modelClass, nil
}

// passiveVerdict reports whether real calls to the Model's target are failing. A
// store failure keeps the previous verdict, so a monitoring outage neither
// degrades nor heals a Model.
func (r *ModelReconciler) passiveVerdict(ctx context.Context, namespace, provider string, model *v1alpha1.Model) modelhealth.Verdict {
	if r.callHealth == nil {
		return modelhealth.Verdict{}
	}
	previous, wasDegraded := modelPassiveDegradation(model)
	records, err := r.callHealth.ListModelCallHealth(ctx, modelhealth.ReporterStaleAfter)
	if err != nil {
		r.logger.Warn("read passive model health", zap.Error(err))
		return previous
	}
	targets := modelHealthTargets(ctx, r.store, namespace, provider, model)
	var matching []store.ModelCallHealth
	for _, record := range records {
		if targets.Contains(record.Target) {
			matching = append(matching, record)
		}
	}
	return modelhealth.EvaluateModel(modelhealth.MergeByTarget(matching), targets, previous, wasDegraded)
}

// modelHasActiveFailure reports whether the Model is unready for a reason other
// than passive health, such as a failing probe or an unresolvable ModelClass. The
// passive verdict only applies to a Model whose active checks pass.
func modelHasActiveFailure(model *v1alpha1.Model) bool {
	ready := lifecycle.GetCondition(apiToLifecycleConditions(model.Status.Conditions), lifecycle.ConditionReady)
	return ready != nil && ready.Status == string(lifecycle.ConditionFalse) && !strings.HasPrefix(ready.Message, modelhealth.MessagePrefix)
}

// modelPassiveDegradation reports whether the Model is currently marked degraded
// by passive health, and the verdict that marked it.
func modelPassiveDegradation(model *v1alpha1.Model) (modelhealth.Verdict, bool) {
	ready := lifecycle.GetCondition(apiToLifecycleConditions(model.Status.Conditions), lifecycle.ConditionReady)
	if ready == nil || ready.Status != string(lifecycle.ConditionFalse) || !strings.HasPrefix(ready.Message, modelhealth.MessagePrefix) {
		return modelhealth.Verdict{}, false
	}
	return modelhealth.Verdict{Degraded: true, Reason: ready.Reason, Message: ready.Message}, true
}

func (r *ModelReconciler) updateModel(ctx context.Context, model *v1alpha1.Model) error {
	data, err := json.Marshal(model)
	if err != nil {
		return fmt.Errorf("marshal model: %w", err)
	}
	return r.store.Update(ctx, modelKind, model.ObjectMeta.Namespace, model.ObjectMeta.Name, data, model.ObjectMeta.ResourceVersion)
}
