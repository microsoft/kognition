// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// AgentAutoscalerFinalizer is the finalizer applied to AgentAutoscaler resources.
	AgentAutoscalerFinalizer = "kognition.dev/agent-autoscaler-cleanup"

	// agentAutoscalerKind is the kind string for AgentAutoscaler resources.
	agentAutoscalerKind = "AgentAutoscaler"
)

// AgentAutoscalerReconciler reconciles AgentAutoscaler resources by observing
// the referenced Agent's instance metrics and adjusting Agent.Spec.Replicas.
type AgentAutoscalerReconciler struct {
	store  store.Store
	logger *zap.Logger
}

// NewAgentAutoscalerReconciler creates a new AgentAutoscalerReconciler.
func NewAgentAutoscalerReconciler(s store.Store, logger *zap.Logger) *AgentAutoscalerReconciler {
	return &AgentAutoscalerReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "agent-autoscaler")),
	}
}

// Reconcile implements the Reconciler interface for AgentAutoscaler resources.
func (r *AgentAutoscalerReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	// Fetch the AgentAutoscaler from the store.
	raw, err := r.store.Get(ctx, agentAutoscalerKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("agent autoscaler not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get agent autoscaler: %w", err)
	}

	var as v1alpha1.AgentAutoscaler
	if err := json.Unmarshal(raw, &as); err != nil {
		return Result{}, fmt.Errorf("unmarshal agent autoscaler: %w", err)
	}
	originalStatus := snapshotAgentAutoscalerStatus(as.Status)
	originalFinalizers := append([]string(nil), as.ObjectMeta.Finalizers...)

	// Handle deletion: remove finalizer.
	if as.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &as, originalStatus, originalFinalizers, log)
	}

	// Ensure finalizer is present.
	if !lifecycle.HasFinalizer(as.ObjectMeta.Finalizers, AgentAutoscalerFinalizer) {
		finalizers, _ := lifecycle.AddFinalizer(as.ObjectMeta.Finalizers, AgentAutoscalerFinalizer)
		as.ObjectMeta.Finalizers = finalizers
		if err := r.updateAutoscaler(ctx, &as, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	// Resolve the referenced Agent.
	agent, err := r.resolveAgent(ctx, &as)
	if err != nil {
		log.Warn("failed to resolve agent", zap.Error(err))
		if err := r.setNotReady(ctx, &as, originalStatus, originalFinalizers, "AgentNotFound", err.Error()); err != nil {
			return Result{}, err
		}
		return Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Compute current replica count from the Agent.
	currentReplicas := int32(1)
	if agent.Spec.Replicas != nil {
		currentReplicas = *agent.Spec.Replicas
	}

	// Compute desired replicas based on metrics.
	desiredReplicas, err := r.computeDesiredReplicas(ctx, &as, agent, currentReplicas)
	if err != nil {
		log.Warn("failed to compute desired replicas", zap.Error(err))
		if err := r.setNotReady(ctx, &as, originalStatus, originalFinalizers, "MetricError", err.Error()); err != nil {
			return Result{}, err
		}
		return Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Check stabilization window before scaling.
	now := time.Now().UTC()
	if desiredReplicas != currentReplicas {
		if blocked, remaining := r.isStabilizationBlocked(&as, currentReplicas, desiredReplicas, now); blocked {
			log.Debug("stabilization window active", zap.Duration("remaining", remaining))
			if err := r.updateAutoscalerStatus(ctx, &as, originalStatus, originalFinalizers, currentReplicas, desiredReplicas); err != nil {
				return Result{}, err
			}
			return Result{RequeueAfter: remaining}, nil
		}
	}

	// Apply the scaling decision to the Agent.
	if desiredReplicas != currentReplicas {
		agent.Spec.Replicas = &desiredReplicas
		if err := r.updateAgent(ctx, agent, desiredReplicas); err != nil {
			return Result{}, fmt.Errorf("update agent replicas: %w", err)
		}
		scaleTime := now
		as.Status.LastScaleTime = &scaleTime
		log.Info("scaled agent", zap.Int32("from", currentReplicas), zap.Int32("to", desiredReplicas))
	}

	// Update autoscaler status.
	if err := r.updateAutoscalerStatus(ctx, &as, originalStatus, originalFinalizers, currentReplicas, desiredReplicas); err != nil {
		return Result{}, err
	}

	// Re-enqueue to keep monitoring.
	return Result{RequeueAfter: 15 * time.Second}, nil
}

// handleDeletion removes the finalizer and persists the update.
func (r *AgentAutoscalerReconciler) handleDeletion(ctx context.Context, as *v1alpha1.AgentAutoscaler, originalStatus v1alpha1.AgentAutoscalerStatus, originalFinalizers []string, log *zap.Logger) (Result, error) {
	log.Info("handling agent autoscaler deletion")

	finalizers, removed := lifecycle.RemoveFinalizer(as.ObjectMeta.Finalizers, AgentAutoscalerFinalizer)
	if removed {
		as.ObjectMeta.Finalizers = finalizers
		if err := r.updateAutoscaler(ctx, as, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
	}
	return Result{}, nil
}

// resolveAgent fetches the Agent referenced by the autoscaler spec.
func (r *AgentAutoscalerReconciler) resolveAgent(ctx context.Context, as *v1alpha1.AgentAutoscaler) (*v1alpha1.Agent, error) {
	agentName := as.Spec.AgentRef.Name
	if agentName == "" {
		return nil, fmt.Errorf("agentRef.name is required")
	}

	raw, err := r.store.Get(ctx, "Agent", as.ObjectMeta.Namespace, agentName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("agent %q not found", agentName)
		}
		return nil, fmt.Errorf("get agent %q: %w", agentName, err)
	}

	var agent v1alpha1.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		return nil, fmt.Errorf("unmarshal agent %q: %w", agentName, err)
	}
	return &agent, nil
}

// computeDesiredReplicas implements the HPA-style scaling algorithm:
// desired = ceil(currentReplicas * (currentMetricValue / targetValue))
// It evaluates all configured metrics and takes the maximum recommendation.
func (r *AgentAutoscalerReconciler) computeDesiredReplicas(ctx context.Context, as *v1alpha1.AgentAutoscaler, agent *v1alpha1.Agent, currentReplicas int32) (int32, error) {
	if len(as.Spec.Metrics) == 0 {
		return currentReplicas, nil
	}

	desired := currentReplicas

	for _, metric := range as.Spec.Metrics {
		currentValue, observed, err := r.getMetricValue(ctx, metric.Type, agent)
		if err != nil {
			return 0, fmt.Errorf("get metric %s: %w", metric.Type, err)
		}
		if !observed {
			continue
		}

		if metric.TargetValue <= 0 {
			continue
		}

		// HPA formula: desired = ceil(current * currentValue / target)
		ratio := float64(currentValue) / float64(metric.TargetValue)
		metricDesired := int32(math.Ceil(float64(currentReplicas) * ratio))

		if metricDesired > desired {
			desired = metricDesired
		}
	}

	// Clamp to min/max bounds.
	if desired < as.Spec.MinReplicas {
		desired = as.Spec.MinReplicas
	}
	if desired > as.Spec.MaxReplicas {
		desired = as.Spec.MaxReplicas
	}

	return desired, nil
}

// getMetricValue retrieves the current value for a given metric type.
// The bool return indicates whether the metric had enough observations.
func (r *AgentAutoscalerReconciler) getMetricValue(ctx context.Context, metricType v1alpha1.MetricType, agent *v1alpha1.Agent) (int64, bool, error) {
	// Count active instances as a metric proxy.
	rawList, err := r.store.List(ctx, agentInstanceKind, store.ListOptions{
		Namespace: agent.ObjectMeta.Namespace,
	})
	if err != nil {
		return 0, false, err
	}

	var activeCount int64
	latencySamplesMs := make([]int64, 0, len(rawList))
	for _, raw := range rawList {
		var inst v1alpha1.AgentInstance
		if err := json.Unmarshal(raw, &inst); err != nil {
			continue
		}
		if !isOwnedBy(inst.ObjectMeta.OwnerReferences, agent) {
			continue
		}
		if inst.Status.Phase == v1alpha1.PhaseRunning || inst.Status.Phase == v1alpha1.PhaseReady {
			activeCount++
		}

		if inst.Status.StartTime == nil {
			continue
		}
		if inst.Status.Phase != v1alpha1.PhaseReady && inst.Status.Phase != v1alpha1.PhaseFailed {
			continue
		}
		endedAt, ok := readyConditionTransitionTime(inst.Status.Conditions)
		if !ok || endedAt.Before(*inst.Status.StartTime) {
			continue
		}
		latencyMs := endedAt.Sub(*inst.Status.StartTime).Milliseconds()
		if latencyMs >= 0 {
			latencySamplesMs = append(latencySamplesMs, latencyMs)
		}
	}

	// For QueueDepth: use active session count (instances) as the metric value.
	// For P95Latency: compute p95 over recent terminal instance durations.
	switch metricType {
	case v1alpha1.MetricQueueDepth:
		return activeCount, true, nil
	case v1alpha1.MetricP95Latency:
		if len(latencySamplesMs) == 0 {
			return 0, false, nil
		}
		sort.Slice(latencySamplesMs, func(i, j int) bool { return latencySamplesMs[i] < latencySamplesMs[j] })
		idx := int(math.Ceil(0.95*float64(len(latencySamplesMs)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(latencySamplesMs) {
			idx = len(latencySamplesMs) - 1
		}
		return latencySamplesMs[idx], true, nil
	default:
		return 0, false, fmt.Errorf("unsupported metric type: %s", metricType)
	}
}

func readyConditionTransitionTime(conditions []v1alpha1.Condition) (time.Time, bool) {
	var latest time.Time
	for _, condition := range conditions {
		if condition.Type != string(lifecycle.ConditionReady) ||
			condition.Status != string(lifecycle.ConditionTrue) ||
			condition.LastTransitionTime.IsZero() {
			continue
		}
		if latest.IsZero() || condition.LastTransitionTime.After(latest) {
			latest = condition.LastTransitionTime
		}
	}
	if latest.IsZero() {
		return time.Time{}, false
	}
	return latest, true
}

// isStabilizationBlocked checks whether the stabilization window prevents scaling.
func (r *AgentAutoscalerReconciler) isStabilizationBlocked(as *v1alpha1.AgentAutoscaler, current, desired int32, now time.Time) (bool, time.Duration) {
	if as.Status.LastScaleTime == nil {
		return false, 0
	}

	var windowSeconds int32
	if desired > current && as.Spec.ScaleUpPolicy != nil {
		windowSeconds = as.Spec.ScaleUpPolicy.StabilizationWindowSeconds
	} else if desired < current && as.Spec.ScaleDownPolicy != nil {
		windowSeconds = as.Spec.ScaleDownPolicy.StabilizationWindowSeconds
	}

	if windowSeconds <= 0 {
		return false, 0
	}

	elapsed := now.Sub(*as.Status.LastScaleTime)
	window := time.Duration(windowSeconds) * time.Second

	if elapsed < window {
		return true, window - elapsed
	}
	return false, 0
}

// updateAutoscalerStatus updates the status fields of the autoscaler.
func (r *AgentAutoscalerReconciler) updateAutoscalerStatus(ctx context.Context, as *v1alpha1.AgentAutoscaler, originalStatus v1alpha1.AgentAutoscalerStatus, originalFinalizers []string, current, desired int32) error {
	as.Status.CurrentReplicas = current
	as.Status.DesiredReplicas = desired

	lcConditions := lifecycle.SetCondition(
		apiToLifecycleConditions(as.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionTrue,
		"ScalingActive",
		fmt.Sprintf("current=%d desired=%d", current, desired),
	)
	as.Status.Conditions = lifecycleToAPIConditions(lcConditions)

	return r.updateAutoscaler(ctx, as, originalStatus, originalFinalizers)
}

// setNotReady sets a not-ready condition on the autoscaler.
func (r *AgentAutoscalerReconciler) setNotReady(ctx context.Context, as *v1alpha1.AgentAutoscaler, originalStatus v1alpha1.AgentAutoscalerStatus, originalFinalizers []string, reason, message string) error {
	lcConditions := lifecycle.SetCondition(
		apiToLifecycleConditions(as.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		reason,
		message,
	)
	as.Status.Conditions = lifecycleToAPIConditions(lcConditions)
	return r.updateAutoscaler(ctx, as, originalStatus, originalFinalizers)
}

// updateAutoscaler persists the current AgentAutoscaler state back to the store.
func (r *AgentAutoscalerReconciler) updateAutoscaler(ctx context.Context, as *v1alpha1.AgentAutoscaler, originalStatus v1alpha1.AgentAutoscalerStatus, originalFinalizers []string) error {
	if reflect.DeepEqual(as.Status, originalStatus) && reflect.DeepEqual(as.ObjectMeta.Finalizers, originalFinalizers) {
		return nil
	}

	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		data, err := json.Marshal(as)
		if err != nil {
			return fmt.Errorf("marshal agent autoscaler: %w", err)
		}
		err = r.store.Update(ctx, agentAutoscalerKind, as.ObjectMeta.Namespace, as.ObjectMeta.Name, data, as.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, getErr := r.store.Get(ctx, agentAutoscalerKind, as.ObjectMeta.Namespace, as.ObjectMeta.Name)
		if getErr != nil {
			return fmt.Errorf("re-read agent autoscaler after conflict: %w", getErr)
		}
		var latest v1alpha1.AgentAutoscaler
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal agent autoscaler after conflict: %w", getErr)
		}
		mergeAgentAutoscalerStatus(&latest.Status, originalStatus, as.Status)
		originalHasFinalizer := hasString(originalFinalizers, AgentAutoscalerFinalizer)
		desiredHasFinalizer := hasString(as.ObjectMeta.Finalizers, AgentAutoscalerFinalizer)
		if originalHasFinalizer != desiredHasFinalizer {
			latest.ObjectMeta.Finalizers = mergeOwnedFinalizer(latest.ObjectMeta.Finalizers, as.ObjectMeta.Finalizers, AgentAutoscalerFinalizer, !desiredHasFinalizer)
		}
		*as = latest
	}
	return store.ErrConflict
}

func snapshotAgentAutoscalerStatus(status v1alpha1.AgentAutoscalerStatus) v1alpha1.AgentAutoscalerStatus {
	cloned := status
	cloned.Conditions = append([]v1alpha1.Condition(nil), status.Conditions...)
	if status.LastScaleTime != nil {
		lastScaleTime := *status.LastScaleTime
		cloned.LastScaleTime = &lastScaleTime
	}
	return cloned
}

func mergeAgentAutoscalerStatus(latest *v1alpha1.AgentAutoscalerStatus, original, desired v1alpha1.AgentAutoscalerStatus) {
	if desired.CurrentReplicas != original.CurrentReplicas {
		latest.CurrentReplicas = desired.CurrentReplicas
	}
	if desired.DesiredReplicas != original.DesiredReplicas {
		latest.DesiredReplicas = desired.DesiredReplicas
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

// updateAgent persists the Agent replica adjustment with retry-on-conflict.
func (r *AgentAutoscalerReconciler) updateAgent(ctx context.Context, agent *v1alpha1.Agent, desiredReplicas int32) error {
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
			return fmt.Errorf("re-read after conflict: %w", getErr)
		}
		var latest v1alpha1.Agent
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal after conflict: %w", getErr)
		}
		latest.Spec.Replicas = &desiredReplicas
		*agent = latest
	}
	return store.ErrConflict
}
