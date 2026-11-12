// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	ModelAutoscalerFinalizer = "kognition.dev/model-autoscaler-cleanup"
	modelAutoscalerKind      = "ModelAutoscaler"
	modelAutoscalerRequeue   = 30 * time.Second
)

// ModelAutoscalerReconciler performs minimal vertical scaling by rebinding an
// Agent between a preferred model and a Ready fallback model selected from the
// degraded preferred ModelClass's fallbackClass.
type ModelAutoscalerReconciler struct {
	store  store.Store
	logger *zap.Logger
}

func NewModelAutoscalerReconciler(s store.Store, logger *zap.Logger) *ModelAutoscalerReconciler {
	return &ModelAutoscalerReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "model-autoscaler")),
	}
}

func (r *ModelAutoscalerReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	raw, err := r.store.Get(ctx, modelAutoscalerKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get model autoscaler: %w", err)
	}

	var mas v1alpha1.ModelAutoscaler
	if err := json.Unmarshal(raw, &mas); err != nil {
		return Result{}, fmt.Errorf("unmarshal model autoscaler: %w", err)
	}
	originalStatus := snapshotModelAutoscalerStatus(mas.Status)
	originalFinalizers := append([]string(nil), mas.ObjectMeta.Finalizers...)

	if mas.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &mas, originalStatus, originalFinalizers)
	}

	if !lifecycle.HasFinalizer(mas.ObjectMeta.Finalizers, ModelAutoscalerFinalizer) {
		finalizers, _ := lifecycle.AddFinalizer(mas.ObjectMeta.Finalizers, ModelAutoscalerFinalizer)
		mas.ObjectMeta.Finalizers = finalizers
		if err := r.updateAutoscaler(ctx, &mas, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	agent, err := r.resolveAgent(ctx, mas.ObjectMeta.Namespace, mas.Spec.AgentRef.Name)
	if err != nil {
		if err := r.setNotReady(ctx, &mas, originalStatus, originalFinalizers, "AgentNotFound", err.Error()); err != nil {
			return Result{}, err
		}
		return Result{RequeueAfter: modelAutoscalerRequeue}, nil
	}

	preferred, err := r.resolveModel(ctx, mas.ObjectMeta.Namespace, mas.Spec.PreferredModelRef.Name)
	if err != nil {
		if err := r.setNotReady(ctx, &mas, originalStatus, originalFinalizers, "PreferredModelNotFound", err.Error()); err != nil {
			return Result{}, err
		}
		return Result{RequeueAfter: modelAutoscalerRequeue}, nil
	}

	preferredClass, err := r.resolveModelClass(ctx, mas.ObjectMeta.Namespace, preferred.Spec.ClassName)
	if err != nil {
		if err := r.setNotReady(ctx, &mas, originalStatus, originalFinalizers, "PreferredModelClassNotFound", err.Error()); err != nil {
			return Result{}, err
		}
		return Result{RequeueAfter: modelAutoscalerRequeue}, nil
	}

	currentModel := agent.Spec.ModelRef.Name
	desiredModel := preferred.ObjectMeta.Name
	reason := "PreferredModelHealthy"
	message := fmt.Sprintf("agent is using preferred model %q", preferred.ObjectMeta.Name)

	if preferredClass.Status.Phase == v1alpha1.PhaseDegraded {
		fallback, ferr := r.findFallbackModel(ctx, mas.ObjectMeta.Namespace, preferredClass.Status.FallbackClass, preferred.ObjectMeta.Name)
		if ferr != nil {
			if err := r.setNotReady(ctx, &mas, originalStatus, originalFinalizers, "FallbackModelUnavailable", ferr.Error()); err != nil {
				return Result{}, err
			}
			return Result{RequeueAfter: modelAutoscalerRequeue}, nil
		}
		desiredModel = fallback.ObjectMeta.Name
		reason = "UsingFallbackModel"
		message = fmt.Sprintf("preferred model class %q degraded; switching to fallback model %q", preferredClass.ObjectMeta.Name, fallback.ObjectMeta.Name)
	}

	if currentModel != desiredModel {
		if blocked, remaining := r.isStabilizationBlocked(&mas, time.Now().UTC()); blocked {
			mas.Status.CurrentModelRef = currentModel
			mas.Status.DesiredModelRef = desiredModel
			if err := r.setConditionAndUpdate(ctx, &mas, originalStatus, originalFinalizers, "ScalingBlocked", fmt.Sprintf("stabilization window active for %s", remaining.Round(time.Second)), lifecycle.ConditionFalse); err != nil {
				return Result{}, err
			}
			return Result{RequeueAfter: remaining}, nil
		}

		previousModel := agent.Spec.ModelRef.Name
		agent.Spec.ModelRef = v1alpha1.LocalObjectReference{Name: desiredModel}
		if err := r.updateAgent(ctx, agent); err != nil {
			return Result{}, fmt.Errorf("update agent modelRef: %w", err)
		}
		now := time.Now().UTC()
		mas.Status.LastScaleTime = &now
		currentModel = desiredModel
		log.Info("switched agent model",
			zap.String("agent", agent.ObjectMeta.Name),
			zap.String("from", previousModel),
			zap.String("to", desiredModel))
	}

	mas.Status.CurrentModelRef = currentModel
	mas.Status.DesiredModelRef = desiredModel
	if err := r.setConditionAndUpdate(ctx, &mas, originalStatus, originalFinalizers, reason, message, lifecycle.ConditionTrue); err != nil {
		return Result{}, err
	}

	return Result{RequeueAfter: modelAutoscalerRequeue}, nil
}

func (r *ModelAutoscalerReconciler) handleDeletion(ctx context.Context, mas *v1alpha1.ModelAutoscaler, originalStatus v1alpha1.ModelAutoscalerStatus, originalFinalizers []string) (Result, error) {
	if lifecycle.HasFinalizer(mas.ObjectMeta.Finalizers, ModelAutoscalerFinalizer) && mas.Spec.PreferredModelRef.Name != "" {
		agent, err := r.resolveAgent(ctx, mas.ObjectMeta.Namespace, mas.Spec.AgentRef.Name)
		if err != nil {
			r.logger.Warn("unable to restore preferred model while deleting autoscaler",
				zap.String("namespace", mas.ObjectMeta.Namespace),
				zap.String("autoscaler", mas.ObjectMeta.Name),
				zap.Error(err))
		} else if agent.Spec.ModelRef.Name != mas.Spec.PreferredModelRef.Name {
			agent.Spec.ModelRef = v1alpha1.LocalObjectReference{Name: mas.Spec.PreferredModelRef.Name}
			if err := r.updateAgent(ctx, agent); err != nil {
				return Result{}, fmt.Errorf("restore preferred model before deletion: %w", err)
			}
		}
	}
	finalizers, removed := lifecycle.RemoveFinalizer(mas.ObjectMeta.Finalizers, ModelAutoscalerFinalizer)
	if removed {
		mas.ObjectMeta.Finalizers = finalizers
		if err := r.updateAutoscaler(ctx, mas, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
	}
	return Result{}, nil
}

func (r *ModelAutoscalerReconciler) resolveAgent(ctx context.Context, namespace, name string) (*v1alpha1.Agent, error) {
	if name == "" {
		return nil, fmt.Errorf("agentRef.name is required")
	}
	raw, err := r.store.Get(ctx, "Agent", namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("agent %q not found", name)
		}
		return nil, err
	}
	var agent v1alpha1.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		return nil, err
	}
	return &agent, nil
}

func (r *ModelAutoscalerReconciler) resolveModel(ctx context.Context, namespace, name string) (*v1alpha1.Model, error) {
	if name == "" {
		return nil, fmt.Errorf("modelRef.name is required")
	}
	raw, err := r.store.Get(ctx, "Model", namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("model %q not found", name)
		}
		return nil, err
	}
	var model v1alpha1.Model
	if err := json.Unmarshal(raw, &model); err != nil {
		return nil, err
	}
	return &model, nil
}

func (r *ModelAutoscalerReconciler) resolveModelClass(ctx context.Context, namespace, name string) (*v1alpha1.ModelClass, error) {
	if name == "" {
		return nil, fmt.Errorf("model class name is required")
	}
	raw, err := r.store.Get(ctx, modelClassKind, namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("modelClass %q not found", name)
		}
		return nil, err
	}
	var mc v1alpha1.ModelClass
	if err := json.Unmarshal(raw, &mc); err != nil {
		return nil, err
	}
	return &mc, nil
}

func (r *ModelAutoscalerReconciler) findFallbackModel(ctx context.Context, namespace, fallbackClass, excludeName string) (*v1alpha1.Model, error) {
	if fallbackClass == "" {
		return nil, fmt.Errorf("no fallbackClass is set on the degraded preferred ModelClass")
	}
	rawList, err := r.store.List(ctx, "Model", store.ListOptions{Namespace: namespace})
	if err != nil {
		return nil, err
	}
	candidates := make([]v1alpha1.Model, 0)
	for _, raw := range rawList {
		var model v1alpha1.Model
		if err := json.Unmarshal(raw, &model); err != nil {
			continue
		}
		if model.ObjectMeta.Name == excludeName {
			continue
		}
		if model.Spec.ClassName != fallbackClass {
			continue
		}
		if model.Status.Phase != v1alpha1.PhaseReady {
			continue
		}
		candidates = append(candidates, model)
	}
	if len(candidates) > 0 {
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].ObjectMeta.Name < candidates[j].ObjectMeta.Name
		})
		return &candidates[0], nil
	}
	return nil, fmt.Errorf("no ready model found for fallback class %q", fallbackClass)
}

func (r *ModelAutoscalerReconciler) isStabilizationBlocked(mas *v1alpha1.ModelAutoscaler, now time.Time) (bool, time.Duration) {
	if mas.Status.LastScaleTime == nil || mas.Spec.StabilizationWindowSeconds <= 0 {
		return false, 0
	}
	window := time.Duration(mas.Spec.StabilizationWindowSeconds) * time.Second
	elapsed := now.Sub(*mas.Status.LastScaleTime)
	if elapsed < window {
		return true, window - elapsed
	}
	return false, 0
}

func (r *ModelAutoscalerReconciler) setNotReady(ctx context.Context, mas *v1alpha1.ModelAutoscaler, originalStatus v1alpha1.ModelAutoscalerStatus, originalFinalizers []string, reason, message string) error {
	return r.setConditionAndUpdate(ctx, mas, originalStatus, originalFinalizers, reason, message, lifecycle.ConditionFalse)
}

func (r *ModelAutoscalerReconciler) setConditionAndUpdate(ctx context.Context, mas *v1alpha1.ModelAutoscaler, originalStatus v1alpha1.ModelAutoscalerStatus, originalFinalizers []string, reason, message string, status lifecycle.ConditionStatus) error {
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(mas.Status.Conditions),
		lifecycle.ConditionReady,
		status,
		reason,
		message,
	)
	mas.Status.Conditions = lifecycleToAPIConditions(lc)
	return r.updateAutoscaler(ctx, mas, originalStatus, originalFinalizers)
}

func (r *ModelAutoscalerReconciler) updateAutoscaler(ctx context.Context, mas *v1alpha1.ModelAutoscaler, originalStatus v1alpha1.ModelAutoscalerStatus, originalFinalizers []string) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		data, err := json.Marshal(mas)
		if err != nil {
			return fmt.Errorf("marshal model autoscaler: %w", err)
		}
		err = r.store.Update(ctx, modelAutoscalerKind, mas.ObjectMeta.Namespace, mas.ObjectMeta.Name, data, mas.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, getErr := r.store.Get(ctx, modelAutoscalerKind, mas.ObjectMeta.Namespace, mas.ObjectMeta.Name)
		if getErr != nil {
			return fmt.Errorf("re-read model autoscaler after conflict: %w", getErr)
		}
		var latest v1alpha1.ModelAutoscaler
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal model autoscaler after conflict: %w", getErr)
		}
		mergeModelAutoscalerStatus(&latest.Status, originalStatus, mas.Status)
		originalHasFinalizer := hasString(originalFinalizers, ModelAutoscalerFinalizer)
		desiredHasFinalizer := hasString(mas.ObjectMeta.Finalizers, ModelAutoscalerFinalizer)
		if originalHasFinalizer != desiredHasFinalizer {
			latest.ObjectMeta.Finalizers = mergeOwnedFinalizer(latest.ObjectMeta.Finalizers, mas.ObjectMeta.Finalizers, ModelAutoscalerFinalizer, !desiredHasFinalizer)
		}
		*mas = latest
	}
	return store.ErrConflict
}

func snapshotModelAutoscalerStatus(status v1alpha1.ModelAutoscalerStatus) v1alpha1.ModelAutoscalerStatus {
	cloned := status
	cloned.Conditions = append([]v1alpha1.Condition(nil), status.Conditions...)
	if status.LastScaleTime != nil {
		lastScaleTime := *status.LastScaleTime
		cloned.LastScaleTime = &lastScaleTime
	}
	return cloned
}

func mergeModelAutoscalerStatus(latest *v1alpha1.ModelAutoscalerStatus, original, desired v1alpha1.ModelAutoscalerStatus) {
	if desired.CurrentModelRef != original.CurrentModelRef {
		latest.CurrentModelRef = desired.CurrentModelRef
	}
	if desired.DesiredModelRef != original.DesiredModelRef {
		latest.DesiredModelRef = desired.DesiredModelRef
	}
	if !agentJobTimesMatch(original.LastScaleTime, desired.LastScaleTime) {
		latest.LastScaleTime = desired.LastScaleTime
	}
	conditionType := string(lifecycle.ConditionReady)
	originalCondition := restartLadderCondition(original.Conditions, conditionType)
	desiredCondition := restartLadderCondition(desired.Conditions, conditionType)
	if !conditionsMatch(originalCondition, desiredCondition) {
		latest.Conditions = applyConditionChange(latest.Conditions, conditionType, desiredCondition)
	}
}

func (r *ModelAutoscalerReconciler) updateAgent(ctx context.Context, agent *v1alpha1.Agent) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		data, err := json.Marshal(agent)
		if err != nil {
			return fmt.Errorf("marshal agent: %w", err)
		}
		err = r.store.Update(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, data, agent.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, getErr := r.store.Get(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name)
		if getErr != nil {
			return fmt.Errorf("re-read agent after conflict: %w", getErr)
		}
		var latest v1alpha1.Agent
		if err := json.Unmarshal(raw, &latest); err != nil {
			return fmt.Errorf("unmarshal agent after conflict: %w", err)
		}
		latest.Spec.ModelRef = agent.Spec.ModelRef
		*agent = latest
	}
	return store.ErrConflict
}
