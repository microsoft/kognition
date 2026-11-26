// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	modelClassKind      = "ModelClass"
	modelClassFinalizer = "kognition.dev/model-class-protection"
)

// ModelClassReconciler provisions and manages the lifecycle of ModelClass resources.
// State machine: "" → Registered → Validating → Ready ⇄ Degraded; on deletion → Terminated.
type ModelClassReconciler struct {
	store        store.Store
	registry     *cpi.Registry
	logger       *zap.Logger
	canaryLedger store.ModelDispatchStore
	canaryUsage  observability.UsageEventWriter
	canaryAudit  *observability.CognitiveAuditLog
	canaryQuota  *QuotaEnforcer
}

func (r *ModelClassReconciler) SetCanaryAccounting(ledger store.ModelDispatchStore, usage observability.UsageEventWriter, audit *observability.CognitiveAuditLog) {
	r.canaryLedger, r.canaryUsage, r.canaryAudit = ledger, usage, audit
}

func NewModelClassReconciler(s store.Store, registry *cpi.Registry, logger *zap.Logger) *ModelClassReconciler {
	return &ModelClassReconciler{
		store:       s,
		registry:    registry,
		logger:      logger.With(zap.String("reconciler", "model-class")),
		canaryQuota: NewQuotaEnforcer(s, NewCognitiveQuotaReconciler(s, logger), logger),
	}
}

func (r *ModelClassReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	raw, err := r.store.Get(ctx, modelClassKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get model class: %w", err)
	}

	var mc v1alpha1.ModelClass
	if err := json.Unmarshal(raw, &mc); err != nil {
		return Result{}, fmt.Errorf("unmarshal model class: %w", err)
	}

	// Handle deletion with finalizer
	if mc.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &mc)
	}

	// Ensure finalizer is present
	if updated, added := lifecycle.AddFinalizer(mc.ObjectMeta.Finalizers, modelClassFinalizer); added {
		mc.ObjectMeta.Finalizers = updated
		mc.Status.Phase = v1alpha1.PhaseRegistered
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(mc.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"Registered",
			"model class registered, awaiting validation",
		)
		mc.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateModelClass(ctx, &mc); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	// State machine dispatch
	switch mc.Status.Phase {
	case v1alpha1.PhaseRegistered:
		return r.reconcileRegistered(ctx, &mc)
	case v1alpha1.PhaseValidating:
		return r.reconcileValidating(ctx, &mc)
	case v1alpha1.PhaseReady:
		return r.reconcileReady(ctx, &mc)
	case v1alpha1.PhaseDegraded:
		return r.reconcileDegraded(ctx, &mc)
	case v1alpha1.PhaseFailed:
		// Re-attempt validation: the driver registry or static config may have
		// changed since the last failure (e.g. controller restart wired up the
		// missing provider). reconcileRegistered is idempotent and will route
		// back to Failed if the configuration is still invalid.
		return r.reconcileRegistered(ctx, &mc)
	case v1alpha1.PhaseTerminated:
		// Already terminated — awaiting garbage collection.
		return Result{}, nil
	default:
		// Unknown or empty phase — treat as newly registered.
		mc.Status.Phase = v1alpha1.PhaseRegistered
		if err := r.updateModelClass(ctx, &mc); err != nil {
			return Result{}, fmt.Errorf("reset to registered: %w", err)
		}
		return Result{Requeue: true}, nil
	}
}

// reconcileRegistered performs static configuration validation (provisioner, probe, latencyClass).
// On success transitions to Validating; on failure transitions to Failed.
func (r *ModelClassReconciler) reconcileRegistered(ctx context.Context, mc *v1alpha1.ModelClass) (Result, error) {
	valid, reason, message := r.validateConfig(mc)
	if !valid {
		mc.Status.Phase = v1alpha1.PhaseFailed
		mc.Status.Provisioned = false
		mc.Status.Health = ""
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(mc.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			reason,
			message,
		)
		mc.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateModelClass(ctx, mc); err != nil {
			return Result{}, fmt.Errorf("update failed status: %w", err)
		}
		return Result{}, nil
	}

	// Static validation passed — advance to Validating.
	mc.Status.Phase = v1alpha1.PhaseValidating
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(mc.Status.Conditions),
		lifecycle.ConditionProgressing,
		lifecycle.ConditionTrue,
		"ConfigValid",
		"static configuration validated, verifying driver availability",
	)
	mc.Status.Conditions = lifecycleToAPIConditions(lc)
	if err := r.updateModelClass(ctx, mc); err != nil {
		return Result{}, fmt.Errorf("advance to validating: %w", err)
	}
	return Result{Requeue: true}, nil
}

// reconcileValidating verifies the CPI driver is registered and available.
// On success transitions to Ready; on failure transitions to Failed.
func (r *ModelClassReconciler) reconcileValidating(ctx context.Context, mc *v1alpha1.ModelClass) (Result, error) {
	providerName := mc.Spec.Provisioner

	_, ok := r.registry.Get(providerName)
	if !ok {
		mc.Status.Phase = v1alpha1.PhaseFailed
		mc.Status.Provisioned = false
		mc.Status.Health = ""
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(mc.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"DriverNotFound",
			fmt.Sprintf("no CPI driver registered for provisioner %q", mc.Spec.Provisioner),
		)
		mc.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateModelClass(ctx, mc); err != nil {
			return Result{}, fmt.Errorf("update driver-not-found: %w", err)
		}
		return Result{}, nil
	}

	// Driver available — mark as Ready.
	if len(mc.Spec.VersionCandidates) != 0 && mc.Spec.VersionPin != mc.Status.ActiveVersion {
		return r.handleVersionPinChange(ctx, mc)
	}
	mc.Status.Phase = v1alpha1.PhaseReady
	mc.Status.Provisioned = true
	mc.Status.Health = "Healthy"
	if mc.Spec.VersionPin != "" {
		mc.Status.ActiveVersion = mc.Spec.VersionPin
	}

	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(mc.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionTrue,
		"Provisioned",
		"provisioner validated and driver available",
	)
	lc = lifecycle.SetCondition(lc, lifecycle.ConditionProgressing, lifecycle.ConditionFalse,
		"Complete", "validation complete")
	mc.Status.Conditions = lifecycleToAPIConditions(lc)

	if err := r.updateModelClass(ctx, mc); err != nil {
		return Result{}, fmt.Errorf("advance to ready: %w", err)
	}
	return Result{}, nil
}

// reconcileReady handles version pin drift detection and health probe execution.
func (r *ModelClassReconciler) reconcileReady(ctx context.Context, mc *v1alpha1.ModelClass) (Result, error) {
	// Version pin drift: spec changed but status not yet promoted.
	if mc.Spec.VersionPin != "" && mc.Spec.VersionPin != mc.Status.ActiveVersion {
		return r.handleVersionPinChange(ctx, mc)
	}

	// If no health probe configured, nothing else to do.
	if mc.Spec.HealthProbe == nil {
		return Result{}, nil
	}

	// Execute health probe.
	passed, reason, message := r.probeHealth(ctx, mc)
	mc.Status.LastProbeTime = time.Now().UTC().Format(time.RFC3339)

	if passed {
		// Probe passed — reset failure counter, stay Ready.
		mc.Status.ConsecutiveFailures = 0
		mc.Status.Health = "Healthy"
		if err := r.updateModelClass(ctx, mc); err != nil {
			return Result{}, fmt.Errorf("update after probe success: %w", err)
		}
		return Result{RequeueAfter: r.probeInterval(mc)}, nil
	}

	// Probe failed — increment failures.
	mc.Status.ConsecutiveFailures++
	failureThreshold := r.failureThreshold(mc)

	if mc.Status.ConsecutiveFailures >= failureThreshold {
		// Exceeded failure threshold — transition to Degraded.
		mc.Status.Phase = v1alpha1.PhaseDegraded
		mc.Status.Health = "Degraded"
		mc.Status.ConsecutiveSuccesses = 0

		// Find fallback class in the same latency tier.
		fallback, err := r.findFallbackClass(ctx, mc)
		if err != nil {
			r.logger.Warn("failed to find fallback class", zap.Error(err), requestid.ZapField(ctx))
		}
		mc.Status.FallbackClass = fallback

		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(mc.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			reason,
			message,
		)
		mc.Status.Conditions = lifecycleToAPIConditions(lc)

		if err := r.updateModelClass(ctx, mc); err != nil {
			return Result{}, fmt.Errorf("transition to degraded: %w", err)
		}
		r.logger.Info("model class degraded",
			zap.String("modelClass", mc.ObjectMeta.Name),
			zap.String("reason", reason),
			zap.Int32("consecutiveFailures", mc.Status.ConsecutiveFailures),
			zap.String("fallbackClass", fallback),
			requestid.ZapField(ctx),
		)
		return Result{RequeueAfter: r.probeInterval(mc)}, nil
	}

	// Below threshold — stay Ready but record the failure.
	if err := r.updateModelClass(ctx, mc); err != nil {
		return Result{}, fmt.Errorf("update after probe failure: %w", err)
	}
	return Result{RequeueAfter: r.probeInterval(mc)}, nil
}

// reconcileDegraded re-evaluates health to attempt recovery with stabilisation window.
func (r *ModelClassReconciler) reconcileDegraded(ctx context.Context, mc *v1alpha1.ModelClass) (Result, error) {
	if len(mc.Spec.VersionCandidates) != 0 && mc.Spec.VersionPin != mc.Status.ActiveVersion {
		return r.handleVersionPinChange(ctx, mc)
	}
	providerName := mc.Spec.Provisioner

	_, ok := r.registry.Get(providerName)
	if !ok {
		// Driver not even registered — stay degraded.
		r.logger.Info("driver still unavailable, staying degraded",
			zap.String("modelClass", mc.ObjectMeta.Name),
			zap.String("provisioner", mc.Spec.Provisioner),
			requestid.ZapField(ctx),
		)
		return Result{RequeueAfter: 30 * time.Second}, nil
	}

	// If no health probe configured, recover immediately on driver availability.
	if mc.Spec.HealthProbe == nil {
		mc.Status.Phase = v1alpha1.PhaseReady
		mc.Status.Health = "Healthy"
		mc.Status.Provisioned = true
		mc.Status.ConsecutiveFailures = 0
		mc.Status.ConsecutiveSuccesses = 0
		mc.Status.FallbackClass = ""
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(mc.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionTrue,
			"Recovered",
			"driver available again, recovered from degraded state",
		)
		mc.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateModelClass(ctx, mc); err != nil {
			return Result{}, fmt.Errorf("recover from degraded: %w", err)
		}
		return Result{}, nil
	}

	// Health probe configured — use stabilisation window for recovery.
	passed, _, _ := r.probeHealth(ctx, mc)
	mc.Status.LastProbeTime = time.Now().UTC().Format(time.RFC3339)

	if passed {
		mc.Status.ConsecutiveSuccesses++
		mc.Status.ConsecutiveFailures = 0

		successThreshold := r.successThreshold(mc)
		if mc.Status.ConsecutiveSuccesses >= successThreshold {
			// Stabilisation window satisfied — recover to Ready.
			mc.Status.Phase = v1alpha1.PhaseReady
			mc.Status.Health = "Healthy"
			mc.Status.ConsecutiveSuccesses = 0
			mc.Status.FallbackClass = ""
			lc := lifecycle.SetCondition(
				apiToLifecycleConditions(mc.Status.Conditions),
				lifecycle.ConditionReady,
				lifecycle.ConditionTrue,
				"Recovered",
				fmt.Sprintf("health probe succeeded %d consecutive times, recovered", successThreshold),
			)
			mc.Status.Conditions = lifecycleToAPIConditions(lc)
			if err := r.updateModelClass(ctx, mc); err != nil {
				return Result{}, fmt.Errorf("recover from degraded: %w", err)
			}
			r.logger.Info("model class recovered",
				zap.String("modelClass", mc.ObjectMeta.Name),
				zap.Int32("successThreshold", successThreshold),
				requestid.ZapField(ctx),
			)
			return Result{}, nil
		}

		// Not yet enough consecutive successes — stay degraded, requeue.
		if err := r.updateModelClass(ctx, mc); err != nil {
			return Result{}, fmt.Errorf("update stabilisation progress: %w", err)
		}
		return Result{RequeueAfter: r.probeInterval(mc)}, nil
	}

	// Probe still failing — reset successes, stay degraded.
	mc.Status.ConsecutiveSuccesses = 0
	mc.Status.ConsecutiveFailures++
	if err := r.updateModelClass(ctx, mc); err != nil {
		return Result{}, fmt.Errorf("update degraded status: %w", err)
	}
	return Result{RequeueAfter: r.probeInterval(mc)}, nil
}

// handleVersionPinChange implements canary promotion with rollback.
// When spec.VersionPin diverges from status.ActiveVersion, we validate the new pin.
// On success the new version is promoted; on failure we keep the old version and set Degraded.
func (r *ModelClassReconciler) handleVersionPinChange(ctx context.Context, mc *v1alpha1.ModelClass) (Result, error) {
	previousVersion := mc.Status.ActiveVersion
	candidateVersion := mc.Spec.VersionPin

	// Re-validate driver availability (canary check).
	providerName := mc.Spec.Provisioner
	_, ok := r.registry.Get(providerName)
	canaryFailed := false
	if ok && len(mc.Spec.VersionCandidates) != 0 {
		if err := r.validateCandidate(ctx, mc); err != nil {
			canaryFailed = true
			r.logger.Warn("candidate canary blocked or failed", zap.String("modelClass", mc.ObjectMeta.Name), zap.String("candidate", candidateVersion), zap.Error(err))
		}
	}
	if !ok || canaryFailed {
		// Rollback: keep previous version, degrade.
		mc.Status.Phase = v1alpha1.PhaseDegraded
		mc.Status.Health = "Degraded"
		mc.Status.ConsecutiveFailures = 0
		mc.Status.ConsecutiveSuccesses = 0
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(mc.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"VersionRollback",
			fmt.Sprintf("canary promotion of %q failed, previous version %q retained", candidateVersion, previousVersion),
		)
		mc.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateModelClass(ctx, mc); err != nil {
			return Result{}, fmt.Errorf("rollback version: %w", err)
		}
		return Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Promote new version.
	mc.Status.ActiveVersion = candidateVersion
	if len(mc.Spec.VersionCandidates) != 0 {
		binding := mc.Spec.VersionCandidates[candidateVersion]
		mc.Status.ActiveBinding = &binding
		mc.Status.Phase = v1alpha1.PhaseReady
		mc.Status.Provisioned = true
		mc.Status.Health = "Healthy"
		mc.Status.ConsecutiveFailures = 0
		mc.Status.ConsecutiveSuccesses = 0
		mc.Status.FallbackClass = ""
	}
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(mc.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionTrue,
		"VersionPromoted",
		fmt.Sprintf("version promoted from %q to %q", previousVersion, candidateVersion),
	)
	mc.Status.Conditions = lifecycleToAPIConditions(lc)

	if err := r.updateModelClass(ctx, mc); err != nil {
		return Result{}, fmt.Errorf("promote version: %w", err)
	}
	r.logger.Info("version promoted",
		zap.String("modelClass", mc.ObjectMeta.Name),
		zap.String("from", previousVersion),
		zap.String("to", candidateVersion),
		requestid.ZapField(ctx),
	)
	return Result{}, nil
}

func (r *ModelClassReconciler) validateCandidate(ctx context.Context, modelClass *v1alpha1.ModelClass) error {
	if len(modelClass.Spec.VersionCandidates) > 32 || modelClass.Spec.VersionPin == "" {
		return errors.New("invalid candidate version configuration")
	}
	binding, ok := modelClass.Spec.VersionCandidates[modelClass.Spec.VersionPin]
	if !ok {
		return errors.New("candidate version is not bound")
	}
	candidate, err := runtimeauth.ResolveModelBinding(ctx, r.store, binding)
	if err != nil || candidate.Spec.ClassName != modelClass.ObjectMeta.Name || cpi.CanonicalProvisioner(candidate.Spec.Provider) != cpi.CanonicalProvisioner(modelClass.Spec.Provisioner) {
		return errors.New("candidate Model does not match its class")
	}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return errors.New("candidate Model is invalid")
	}
	verifier := admission.NewSignatureValidator(r.store)
	if err := verifier.VerifySignedArtifact(ctx, &admission.Request{Kind: "Model", Namespace: binding.Namespace, Name: binding.ModelName, Object: encoded}); err != nil {
		return errors.New("candidate Model signature is not trusted")
	}
	driver, ok := r.registry.Get(modelClass.Spec.Provisioner)
	if !ok {
		return errors.New("candidate driver is unavailable")
	}
	probeContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := driver.Validate(probeContext, &candidate.Spec, &modelClass.Spec); err != nil {
		return errors.New("candidate configuration is invalid")
	}
	var credentials *cpi.Credentials
	if candidate.Spec.CredentialRef != nil && candidate.Spec.CredentialRef.Name != "" {
		raw, err := r.store.Get(probeContext, "SecretRef", binding.Namespace, candidate.Spec.CredentialRef.Name)
		if err != nil {
			return errors.New("candidate credentials are unavailable")
		}
		var reference v1alpha1.SecretRef
		values := map[string]string{}
		if json.Unmarshal(raw, &reference) != nil || reference.Spec.ResolvedBackendType() != "internal" || json.Unmarshal([]byte(reference.ObjectMeta.Annotations[internalSecretDataAnnotation]), &values) != nil {
			return errors.New("candidate credentials are invalid")
		}
		credentials = &cpi.Credentials{Type: values["type"], Data: values}
	}
	parameters := make(map[string]string, len(candidate.Spec.Parameters))
	for name, value := range candidate.Spec.Parameters {
		parameters[name] = value
	}
	parameters["max_tokens"] = "16"
	if candidate.Spec.InvocationProfileRef == nil {
		parameters["temperature"] = "0"
	}
	request := &cpi.CompletionRequest{
		Model: candidate.Spec.ModelID, Endpoint: candidate.Spec.Endpoint, Creds: credentials,
		Messages:   []cpi.Message{{Role: "user", Content: "Return exactly KOGNITION_MODEL_CANARY_OK"}},
		Parameters: parameters,
	}
	request, err = cpi.BindCompletion(probeContext, r.store, driver, candidate, request)
	if err != nil {
		return fmt.Errorf("candidate invocation configuration: %w", err)
	}
	if err := r.executeCandidateCanary(probeContext, modelClass, binding, driver, request); err != nil {
		return err
	}
	if _, err := runtimeauth.ResolveModelBinding(ctx, r.store, binding); err != nil {
		return errors.New("candidate changed during validation")
	}
	return nil
}

type candidateCanaryResult struct {
	Version int                       `json:"version"`
	Passed  bool                      `json:"passed"`
	Usage   *observability.CostRecord `json:"usage,omitempty"`
}

func (r *ModelClassReconciler) executeCandidateCanary(ctx context.Context, class *v1alpha1.ModelClass, binding v1alpha1.ModelVersionBinding, driver cpi.Driver, request *cpi.CompletionRequest) error {
	if r.canaryLedger == nil || r.canaryUsage == nil || r.canaryAudit == nil {
		return errors.New("candidate canary requires durable accounting and audit")
	}
	pricing, err := observability.ResolveModelPricing(class.Spec.Parameters)
	if err != nil {
		return errors.New("candidate pricing is invalid")
	}
	identity, err := json.Marshal(struct {
		Class         string                       `json:"class"`
		UID           string                       `json:"uid"`
		Version       string                       `json:"version"`
		Binding       v1alpha1.ModelVersionBinding `json:"binding"`
		Specification v1alpha1.ModelClassSpec      `json:"specification"`
	}{class.ObjectMeta.Name, class.ObjectMeta.UID, class.Spec.VersionPin, binding, class.Spec})
	if err != nil {
		return errors.New("invalid candidate operation")
	}
	operation := fmt.Sprintf("model-canary:%x", sha256.Sum256(identity))
	record, err := r.canaryLedger.GetModelDispatch(ctx, binding.Namespace, operation)
	if err == nil {
		var result candidateCanaryResult
		if record.SessionID != operation || len(record.Response) == 0 || len(record.Response) > 4096 || json.Unmarshal(record.Response, &result) != nil || (result.Version != 1 && result.Version != 2) {
			return ErrDispatchAccountingUncertain
		}
		if result.Version == 2 {
			return r.finalizeCandidateCanary(ctx, binding.Namespace, operation, result)
		}
		if !result.Passed {
			return errors.New("candidate inference canary failed")
		}
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return errors.New("candidate accounting ledger is unavailable")
	}
	if err := r.canaryQuota.CheckModelQuota(ctx, binding.Namespace, class.Spec.CostTier); err != nil {
		return errors.New("candidate model-tier quota is exhausted or unavailable")
	}
	reservations, ok := r.store.(store.CanaryReservationStore)
	counter, countsExactly := driver.(cpi.ExactTokenCounter)
	if !ok || !countsExactly {
		return errors.New("candidate canary requires protected reservations and exact token counting")
	}
	inputTokens, err := counter.CountInputTokens(request)
	if err != nil || inputTokens < 0 || inputTokens > 4096 {
		return errors.New("candidate input token count is unavailable or exceeds its ceiling")
	}
	reservationID := canaryReservationID(operation)
	details, err := json.Marshal(map[string]string{"operation": operation, "modelClass": class.ObjectMeta.Name, "classUID": class.ObjectMeta.UID, "candidateDigest": binding.SpecDigest})
	if err != nil {
		return errors.New("invalid canary audit metadata")
	}
	audit := observability.AuditEntry{ID: operation + ":authorized", Workspace: binding.Namespace, EventType: observability.AuditModelCall, SubjectKind: "ModelClass", SubjectRef: class.ObjectMeta.Name, Verb: "canary", Details: details}
	if err := r.canaryAudit.RecordBeforeExec(ctx, audit); err != nil {
		return errors.New("candidate audit evidence unavailable")
	}
	claimed, err := r.canaryLedger.ClaimModelDispatch(ctx, binding.Namespace, operation, operation)
	if err != nil || !claimed {
		return ErrDispatchAccountingUncertain
	}
	if _, err := reservations.CreateCanaryReservation(ctx, store.TokenReservation{ID: reservationID, Namespace: binding.Namespace, ReservedInput: int64(inputTokens), ReservedOutput: 16}); err != nil {
		if errors.Is(err, store.ErrQuotaExceeded) {
			result, encodeErr := json.Marshal(candidateCanaryResult{Version: 2})
			if encodeErr != nil {
				return ErrDispatchAccountingUncertain
			}
			terminalContext, terminalCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer terminalCancel()
			if persistErr := r.canaryLedger.CompleteModelDispatch(terminalContext, binding.Namespace, operation, operation, result); persistErr != nil {
				return ErrDispatchAccountingUncertain
			}
			return fmt.Errorf("candidate token quota denied: %w", err)
		}
		return ErrDispatchAccountingUncertain
	}
	started := time.Now().UTC()
	response, err := driver.Complete(ctx, request)
	if err != nil || response == nil || !response.Usage.Reported {
		return ErrDispatchAccountingUncertain
	}
	usage, valid := cpi.NormalizeCompletionUsage(response.Usage.PromptTokens, response.Usage.CompletionTokens, &response.Usage.TotalTokens)
	if !valid {
		return ErrDispatchAccountingUncertain
	}
	cost, _, err := observability.ModelCostUnits(class.Spec.Parameters, int64(usage.PromptTokens), int64(usage.CompletionTokens))
	if err != nil {
		return ErrDispatchAccountingUncertain
	}
	completed := time.Now().UTC()
	accountingContext, accountingCancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer accountingCancel()
	event := observability.CostRecord{
		ID: operation, Workspace: binding.Namespace, OperationType: observability.CostOpModelInference,
		Purpose: observability.UsagePurposeModelCanary, SessionID: operation, ModelName: request.Model, ModelProvider: class.Spec.Provisioner,
		ModelClass: class.ObjectMeta.Name, ModelClassVersion: class.Spec.VersionPin, ModelCostTier: class.Spec.CostTier,
		PromptTokens: int64(usage.PromptTokens), CompletionTokens: int64(usage.CompletionTokens), TotalTokens: int64(usage.TotalTokens), UsageReported: true,
		PricingConfigured: pricing.Configured, InputCostMicrousdPerMillionTokens: pricing.InputMicrousdPerMillionTokens,
		OutputCostMicrousdPerMillionTokens: pricing.OutputMicrousdPerMillionTokens, CostUnits: cost,
		ProviderDurationMillis: completed.Sub(started).Milliseconds(), Timestamp: completed,
	}
	passed := response.Content == "KOGNITION_MODEL_CANARY_OK" && len(response.ToolCalls) == 0 && usage.CompletionTokens <= 16
	terminal := candidateCanaryResult{Version: 2, Passed: passed, Usage: &event}
	result, err := json.Marshal(terminal)
	if err != nil {
		return ErrDispatchAccountingUncertain
	}
	if err := r.canaryLedger.CompleteModelDispatch(accountingContext, binding.Namespace, operation, operation, result); err != nil {
		return ErrDispatchAccountingUncertain
	}
	return r.finalizeCandidateCanary(accountingContext, binding.Namespace, operation, terminal)
}

func (r *ModelClassReconciler) finalizeCandidateCanary(ctx context.Context, namespace, operation string, terminal candidateCanaryResult) error {
	if terminal.Usage == nil {
		if terminal.Passed {
			return ErrDispatchAccountingUncertain
		}
		return errors.New("candidate canary denied before provider dispatch")
	}
	event := *terminal.Usage
	if event.ID != operation || event.SessionID != operation || event.Workspace != namespace || event.OperationType != observability.CostOpModelInference || event.CostUnits < 0 || event.ModelClass == "" || event.ModelName == "" || event.ModelProvider == "" || event.Purpose != observability.UsagePurposeModelCanary || !event.UsageReported || event.Timestamp.IsZero() || event.PromptTokens < 0 || event.CompletionTokens < 0 || event.TotalTokens < 0 || event.PromptTokens > event.TotalTokens || event.CompletionTokens != event.TotalTokens-event.PromptTokens {
		return ErrDispatchAccountingUncertain
	}
	reservations, ok := r.store.(store.CanaryReservationStore)
	if !ok {
		return ErrDispatchAccountingUncertain
	}
	finalizationContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	disposition, err := reservations.SettleCanaryReservation(finalizationContext, canaryReservationID(operation), event.PromptTokens, event.CompletionTokens)
	if err != nil || disposition.State != store.CanaryReservationSettled || disposition.InputTokens != event.PromptTokens || disposition.OutputTokens != event.CompletionTokens {
		return ErrDispatchAccountingUncertain
	}
	if _, err := r.canaryUsage.ApplyUsageEvent(finalizationContext, event); err != nil {
		return ErrDispatchAccountingUncertain
	}
	details, err := json.Marshal(candidateCanaryResult{Version: 1, Passed: terminal.Passed})
	if err != nil {
		return ErrDispatchAccountingUncertain
	}
	audit := observability.AuditEntry{ID: operation + ":completed", Timestamp: event.Timestamp, Workspace: namespace, EventType: observability.AuditModelCall, SubjectKind: "ModelClass", SubjectRef: event.ModelClass, Verb: "canary-completed", Details: details}
	if err := r.canaryAudit.Record(finalizationContext, audit); err != nil {
		return ErrDispatchAccountingUncertain
	}
	if !terminal.Passed {
		return errors.New("candidate inference canary failed")
	}
	return nil
}

// validateConfig performs static validation of the ModelClass spec.
func (r *ModelClassReconciler) validateConfig(mc *v1alpha1.ModelClass) (bool, string, string) {
	if mc.Spec.Provisioner == "" {
		return false, "ProvisionerMissing", "provisioner must be specified"
	}

	switch mc.Spec.Provisioner {
	case "openai", "anthropic", "ollama", "azure-openai", "foundry", "openai-compatible":
		// supported
	default:
		return false, "ProvisionerUnsupported", fmt.Sprintf("unsupported provisioner %q", mc.Spec.Provisioner)
	}

	if mc.Spec.HealthProbe != nil {
		if mc.Spec.HealthProbe.Endpoint == "" {
			return false, "HealthProbeInvalid", "healthProbe endpoint must be specified"
		}
		parsed, err := url.Parse(mc.Spec.HealthProbe.Endpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return false, "HealthProbeInvalid", "healthProbe endpoint must be a valid URI"
		}
	}

	if mc.Spec.LatencyClass != "" {
		switch mc.Spec.LatencyClass {
		case "low", "medium", "high":
			// valid
		default:
			return false, "LatencyClassInvalid", fmt.Sprintf("invalid latencyClass %q; must be low, medium, or high", mc.Spec.LatencyClass)
		}
	}

	if _, err := observability.ResolveModelPricing(mc.Spec.Parameters); err != nil {
		return false, "PricingInvalid", err.Error()
	}

	return true, "ConfigValid", "provisioner and configuration validated"
}

func (r *ModelClassReconciler) handleDeletion(ctx context.Context, mc *v1alpha1.ModelClass) (Result, error) {
	mc.Status.Phase = v1alpha1.PhaseTerminated

	// Check for active models referencing this class (finalizer drain)
	models, err := r.store.List(ctx, "Model", store.ListOptions{Namespace: mc.ObjectMeta.Namespace})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Result{}, fmt.Errorf("list models for drain: %w", err)
	}

	activeCount := 0
	for _, raw := range models {
		var model v1alpha1.Model
		if err := json.Unmarshal(raw, &model); err != nil {
			continue
		}
		if model.Spec.ClassName == mc.ObjectMeta.Name && model.Status.Phase != v1alpha1.PhaseTerminated {
			activeCount++
		}
	}

	if activeCount > 0 {
		// Still draining — requeue
		r.logger.Info("draining active models before deletion",
			zap.String("modelClass", mc.ObjectMeta.Name),
			zap.Int("activeModels", activeCount),
			requestid.ZapField(ctx),
		)
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(mc.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"Draining",
			fmt.Sprintf("waiting for %d active model(s) to terminate", activeCount),
		)
		mc.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateModelClass(ctx, mc); err != nil {
			return Result{}, fmt.Errorf("update during drain: %w", err)
		}
		return Result{RequeueAfter: 5 * time.Second}, nil
	}

	// All models drained — remove finalizer
	if updated, removed := lifecycle.RemoveFinalizer(mc.ObjectMeta.Finalizers, modelClassFinalizer); removed {
		mc.ObjectMeta.Finalizers = updated
	}

	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(mc.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		"Terminated",
		"model class terminated and finalizer removed",
	)
	mc.Status.Conditions = lifecycleToAPIConditions(lc)

	if err := r.updateModelClass(ctx, mc); err != nil {
		return Result{}, fmt.Errorf("update after drain: %w", err)
	}
	return Result{}, nil
}

// probeHealth executes the configured health probe against the CPI driver.
// Returns (passed bool, reason string, message string).
func (r *ModelClassReconciler) probeHealth(ctx context.Context, mc *v1alpha1.ModelClass) (bool, string, string) {
	passed, reason, message := r.runHealthProbe(ctx, mc)
	// Probe results can carry provider-controlled text; make it safe to persist.
	return passed, sanitizeConditionReason(reason, "ProbeFailed"), sanitizeConditionMessage(message)
}

func (r *ModelClassReconciler) runHealthProbe(ctx context.Context, mc *v1alpha1.ModelClass) (bool, string, string) {
	probe := mc.Spec.HealthProbe
	if probe == nil {
		return true, "", ""
	}

	providerName := mc.Spec.Provisioner
	driver, ok := r.registry.Get(providerName)
	if !ok {
		return false, "DriverUnavailable", fmt.Sprintf("CPI driver %q not found", mc.Spec.Provisioner)
	}

	// Construct a synthetic ModelSpec for the probe endpoint.
	syntheticSpec := &v1alpha1.ModelSpec{
		Provider:  mc.Spec.Provisioner,
		ClassName: mc.ObjectMeta.Name,
		Endpoint:  probe.Endpoint,
	}

	result, err := driver.Probe(ctx, syntheticSpec, nil)
	if err != nil {
		return false, "ProbeError", fmt.Sprintf("health probe failed: %v", err)
	}

	if result == nil {
		return false, "ProbeError", "health probe returned nil result"
	}

	// Evaluate result based on probe type.
	probeType := probe.Type
	if probeType == "" {
		probeType = "inference-ping"
	}

	switch probeType {
	case "inference-ping":
		if !result.Available {
			return false, "ProbeFailed", fmt.Sprintf("inference-ping: endpoint unavailable: %s", result.Message)
		}
		return true, "", ""

	case "latency-threshold":
		if !result.Available {
			return false, "ProbeFailed", fmt.Sprintf("latency-threshold: endpoint unavailable: %s", result.Message)
		}
		if probe.LatencyThresholdMs > 0 && result.Latency != "" {
			latency, err := time.ParseDuration(result.Latency)
			if err == nil && latency.Milliseconds() > probe.LatencyThresholdMs {
				return false, "LatencyExceeded", fmt.Sprintf("latency %s exceeds threshold %dms", result.Latency, probe.LatencyThresholdMs)
			}
		}
		return true, "", ""

	case "error-rate-threshold":
		// NOTE: Full error-rate evaluation requires aggregated metrics not available
		// from a single probe. Currently this checks availability only. When the CPI
		// HealthResult gains an ErrorRate field, compare against probe.ErrorRateThreshold.
		if !result.Available {
			return false, "ProbeFailed", fmt.Sprintf("error-rate-threshold: endpoint unavailable: %s", result.Message)
		}
		return true, "", ""

	default:
		// Unknown probe type — treat as inference-ping.
		if !result.Available {
			return false, "ProbeFailed", fmt.Sprintf("endpoint unavailable: %s", result.Message)
		}
		return true, "", ""
	}
}

// probeInterval returns the configured probe interval, defaulting to 30s.
func (r *ModelClassReconciler) probeInterval(mc *v1alpha1.ModelClass) time.Duration {
	if mc.Spec.HealthProbe != nil && mc.Spec.HealthProbe.IntervalSeconds > 0 {
		return time.Duration(mc.Spec.HealthProbe.IntervalSeconds) * time.Second
	}
	return 30 * time.Second
}

// failureThreshold returns the configured failure threshold, defaulting to 3.
func (r *ModelClassReconciler) failureThreshold(mc *v1alpha1.ModelClass) int32 {
	if mc.Spec.HealthProbe != nil && mc.Spec.HealthProbe.FailureThreshold > 0 {
		return mc.Spec.HealthProbe.FailureThreshold
	}
	return 3
}

// successThreshold returns the configured success threshold (stabilisation window), defaulting to 3.
func (r *ModelClassReconciler) successThreshold(mc *v1alpha1.ModelClass) int32 {
	if mc.Spec.HealthProbe != nil && mc.Spec.HealthProbe.SuccessThreshold > 0 {
		return mc.Spec.HealthProbe.SuccessThreshold
	}
	return 3
}

// findFallbackClass searches for a Ready ModelClass in the same LatencyClass tier.
func (r *ModelClassReconciler) findFallbackClass(ctx context.Context, mc *v1alpha1.ModelClass) (string, error) {
	if mc.Spec.LatencyClass == "" {
		return "", nil
	}

	items, err := r.store.List(ctx, modelClassKind, store.ListOptions{Namespace: mc.ObjectMeta.Namespace})
	if err != nil {
		return "", fmt.Errorf("list model classes for fallback: %w", err)
	}

	for _, raw := range items {
		var candidate v1alpha1.ModelClass
		if err := json.Unmarshal(raw, &candidate); err != nil {
			continue
		}
		// Skip self and non-Ready candidates.
		if candidate.ObjectMeta.Name == mc.ObjectMeta.Name {
			continue
		}
		if candidate.Status.Phase != v1alpha1.PhaseReady {
			continue
		}
		if candidate.Spec.LatencyClass == mc.Spec.LatencyClass {
			return candidate.ObjectMeta.Name, nil
		}
	}

	return "", nil
}

func (r *ModelClassReconciler) updateModelClass(ctx context.Context, mc *v1alpha1.ModelClass) error {
	// Conditions written before sanitization existed are re-persisted on every update.
	for i := range mc.Status.Conditions {
		c := &mc.Status.Conditions[i]
		c.Reason = sanitizeConditionReason(c.Reason, "Unknown")
		c.Message = sanitizeConditionMessage(c.Message)
	}
	data, err := json.Marshal(mc)
	if err != nil {
		return fmt.Errorf("marshal model class: %w", err)
	}
	return r.store.Update(ctx, modelClassKind, mc.ObjectMeta.Namespace, mc.ObjectMeta.Name, data, mc.ObjectMeta.ResourceVersion)
}
