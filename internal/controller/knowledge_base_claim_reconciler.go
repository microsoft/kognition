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
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	knowledgeBaseClaimKind = "KnowledgeBaseClaim"

	// conditionBound is the KnowledgeBaseClaim status condition that reports
	// whether the claim is bound to a Ready KnowledgeBase.
	conditionBound lifecycle.ConditionType = "Bound"

	// kbClaimRequeueUnbound is how often an unbound (Pending/Lost) claim
	// re-checks its target KnowledgeBase. Task #6 will make this event-driven
	// by re-enqueuing claims from the KnowledgeBase reconciler.
	kbClaimRequeueUnbound = 30 * time.Second
)

// KnowledgeBaseClaimReconciler reconciles KnowledgeBaseClaim resources.
//
// A KnowledgeBaseClaim is a grant/binding — it does NOT provision a backing
// store (the referenced KnowledgeBase is a shared corpus). On each
// reconciliation the reconciler resolves spec.knowledgeBaseName in the same
// namespace and transitions the claim through:
//
//   - Pending: no matching KnowledgeBase, or KB exists but not Ready, and the
//     claim was never Bound.
//   - Bound:   matching KnowledgeBase found in Ready phase;
//     status.boundKnowledgeBase is set.
//   - Lost:    the claim was previously Bound but the KnowledgeBase is now
//     missing or no longer Ready. status.boundKnowledgeBase is cleared.
//
// The reconciler installs no finalizer: nothing needs to be reclaimed on
// deletion.
type KnowledgeBaseClaimReconciler struct {
	store  store.Store
	logger *zap.Logger
}

// NewKnowledgeBaseClaimReconciler creates a new KnowledgeBaseClaimReconciler.
func NewKnowledgeBaseClaimReconciler(s store.Store, logger *zap.Logger) *KnowledgeBaseClaimReconciler {
	return &KnowledgeBaseClaimReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "knowledge-base-claim")),
	}
}

// Reconcile implements the Reconciler interface for KnowledgeBaseClaim resources.
func (r *KnowledgeBaseClaimReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	raw, err := r.store.Get(ctx, knowledgeBaseClaimKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("knowledgebaseclaim not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get knowledgebaseclaim: %w", err)
	}

	var claim v1alpha1.KnowledgeBaseClaim
	if err := json.Unmarshal(raw, &claim); err != nil {
		return Result{}, fmt.Errorf("unmarshal knowledgebaseclaim: %w", err)
	}

	// A claim owns no backing state, so there is nothing to reclaim on
	// deletion. Let the store finalize the object.
	if claim.ObjectMeta.DeletionTimestamp != nil {
		return Result{}, nil
	}

	kb, kbErr := r.getKnowledgeBase(ctx, claim.ObjectMeta.Namespace, claim.Spec.KnowledgeBaseName)
	switch {
	case errors.Is(kbErr, store.ErrNotFound):
		return r.markUnbound(ctx, &claim,
			"KnowledgeBaseNotFound",
			fmt.Sprintf("KnowledgeBase %q not found in namespace %q", claim.Spec.KnowledgeBaseName, claim.ObjectMeta.Namespace),
			log)
	case kbErr != nil:
		return Result{}, fmt.Errorf("get knowledge base: %w", kbErr)
	case kb.Status.Phase != v1alpha1.PhaseReady:
		return r.markUnbound(ctx, &claim,
			"KnowledgeBaseNotReady",
			fmt.Sprintf("KnowledgeBase %q is in phase %q, waiting for Ready", claim.Spec.KnowledgeBaseName, kb.Status.Phase),
			log)
	default:
		return r.markBound(ctx, &claim, kb, log)
	}
}

// markBound transitions the claim to Phase=Bound and sets Bound=True.
func (r *KnowledgeBaseClaimReconciler) markBound(ctx context.Context, claim *v1alpha1.KnowledgeBaseClaim, kb *v1alpha1.KnowledgeBase, log *zap.Logger) (Result, error) {
	originalStatus := claim.Status
	desiredPhase := v1alpha1.PhaseBound
	desiredBound := kb.ObjectMeta.Name
	desiredMsg := fmt.Sprintf("bound to KnowledgeBase %q (generation %d)", kb.ObjectMeta.Name, kb.Status.Generation)

	newConditions := lifecycleToAPIConditions(lifecycle.SetCondition(
		apiToLifecycleConditions(claim.Status.Conditions),
		conditionBound,
		lifecycle.ConditionTrue,
		"KnowledgeBaseReady",
		desiredMsg,
	))

	if claim.Status.Phase == desiredPhase &&
		claim.Status.BoundKnowledgeBase == desiredBound &&
		claim.Status.ObservedGeneration == claim.ObjectMeta.Generation &&
		claimConditionsEqual(claim.Status.Conditions, newConditions) {
		return Result{}, nil
	}

	claim.Status.Phase = desiredPhase
	claim.Status.BoundKnowledgeBase = desiredBound
	claim.Status.ObservedGeneration = claim.ObjectMeta.Generation
	claim.Status.Conditions = newConditions

	if err := r.updateClaimWithRetry(ctx, claim, originalStatus); err != nil {
		return Result{}, fmt.Errorf("update claim to Bound: %w", err)
	}
	log.Info("KnowledgeBaseClaim bound", zap.String("knowledgeBase", kb.ObjectMeta.Name))
	return Result{}, nil
}

// markUnbound transitions the claim to Pending (never bound before) or Lost
// (previously bound). BoundKnowledgeBase is cleared.
func (r *KnowledgeBaseClaimReconciler) markUnbound(ctx context.Context, claim *v1alpha1.KnowledgeBaseClaim, reason, message string, log *zap.Logger) (Result, error) {
	originalStatus := claim.Status
	desiredPhase := v1alpha1.PhasePending
	if claim.Status.Phase == v1alpha1.PhaseBound || claim.Status.Phase == v1alpha1.PhaseLost {
		desiredPhase = v1alpha1.PhaseLost
	}

	newConditions := lifecycleToAPIConditions(lifecycle.SetCondition(
		apiToLifecycleConditions(claim.Status.Conditions),
		conditionBound,
		lifecycle.ConditionFalse,
		reason,
		message,
	))

	if claim.Status.Phase == desiredPhase &&
		claim.Status.BoundKnowledgeBase == "" &&
		claim.Status.ObservedGeneration == claim.ObjectMeta.Generation &&
		claimConditionsEqual(claim.Status.Conditions, newConditions) {
		return Result{RequeueAfter: kbClaimRequeueUnbound}, nil
	}

	claim.Status.Phase = desiredPhase
	claim.Status.BoundKnowledgeBase = ""
	claim.Status.ObservedGeneration = claim.ObjectMeta.Generation
	claim.Status.Conditions = newConditions

	if err := r.updateClaimWithRetry(ctx, claim, originalStatus); err != nil {
		return Result{}, fmt.Errorf("update claim to %s: %w", desiredPhase, err)
	}
	log.Info("KnowledgeBaseClaim unbound",
		zap.String("phase", string(desiredPhase)),
		zap.String("reason", reason))
	return Result{RequeueAfter: kbClaimRequeueUnbound}, nil
}

func (r *KnowledgeBaseClaimReconciler) getKnowledgeBase(ctx context.Context, namespace, name string) (*v1alpha1.KnowledgeBase, error) {
	raw, err := r.store.Get(ctx, knowledgeBaseKind, namespace, name)
	if err != nil {
		return nil, err
	}
	var kb v1alpha1.KnowledgeBase
	if err := json.Unmarshal(raw, &kb); err != nil {
		return nil, fmt.Errorf("unmarshal knowledge base: %w", err)
	}
	return &kb, nil
}

func (r *KnowledgeBaseClaimReconciler) updateClaimWithRetry(ctx context.Context, claim *v1alpha1.KnowledgeBaseClaim, originalStatus v1alpha1.KnowledgeBaseClaimStatus) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		data, err := json.Marshal(claim)
		if err != nil {
			return fmt.Errorf("marshal claim: %w", err)
		}
		err = r.store.Update(ctx, knowledgeBaseClaimKind, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name, data, claim.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, getErr := r.store.Get(ctx, knowledgeBaseClaimKind, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name)
		if getErr != nil {
			return fmt.Errorf("re-read after conflict: %w", getErr)
		}
		var latest v1alpha1.KnowledgeBaseClaim
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal after conflict: %w", getErr)
		}
		if latest.ObjectMeta.Generation != claim.ObjectMeta.Generation ||
			latest.Spec.KnowledgeBaseName != claim.Spec.KnowledgeBaseName {
			return store.ErrConflict
		}
		mergeKnowledgeBaseClaimStatus(&latest.Status, originalStatus, claim.Status)
		*claim = latest
	}
	return store.ErrConflict
}

func mergeKnowledgeBaseClaimStatus(latest *v1alpha1.KnowledgeBaseClaimStatus, original, desired v1alpha1.KnowledgeBaseClaimStatus) {
	if desired.Phase != original.Phase {
		latest.Phase = desired.Phase
	}
	if desired.BoundKnowledgeBase != original.BoundKnowledgeBase {
		latest.BoundKnowledgeBase = desired.BoundKnowledgeBase
	}
	if desired.ObservedGeneration != original.ObservedGeneration {
		latest.ObservedGeneration = desired.ObservedGeneration
	}
	conditionType := string(conditionBound)
	originalCondition := restartLadderCondition(original.Conditions, conditionType)
	desiredCondition := restartLadderCondition(desired.Conditions, conditionType)
	if !conditionsMatch(originalCondition, desiredCondition) {
		latest.Conditions = applyConditionChange(latest.Conditions, conditionType, desiredCondition)
	}
}

// claimConditionsEqual compares two condition slices ignoring
// LastTransitionTime so no-op reconciliations do not repeatedly rewrite status.
func claimConditionsEqual(a, b []v1alpha1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type ||
			a[i].Status != b[i].Status ||
			a[i].Reason != b[i].Reason ||
			a[i].Message != b[i].Message {
			return false
		}
	}
	return true
}
