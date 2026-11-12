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

// RestartLadderReconcilerConfig controls restart-ladder behavior.
type RestartLadderReconcilerConfig struct {
	// DefaultStabilisation is the wait time after a ladder action before
	// deciding whether to escalate. Defaults to 60s.
	DefaultStabilisation time.Duration
}

func (c *RestartLadderReconcilerConfig) setDefaults() {
	if c.DefaultStabilisation <= 0 {
		c.DefaultStabilisation = 60 * time.Second
	}
}

// RestartLadderReconciler watches for breached probe thresholds and walks
// the Agent's RestartLadder steps to restore healthy operation.
type RestartLadderReconciler struct {
	store      store.Store
	dispatcher RuntimeDispatcher
	logger     *zap.Logger
	config     RestartLadderReconcilerConfig
}

// NewRestartLadderReconciler creates a RestartLadderReconciler with default configuration.
func NewRestartLadderReconciler(s store.Store, dispatcher RuntimeDispatcher, logger *zap.Logger) *RestartLadderReconciler {
	config := RestartLadderReconcilerConfig{}
	config.setDefaults()
	return &RestartLadderReconciler{
		store:      s,
		dispatcher: dispatcher,
		logger:     logger.With(zap.String("reconciler", "restart-ladder")),
		config:     config,
	}
}

// NewRestartLadderReconcilerWithConfig creates a RestartLadderReconciler with custom configuration.
func NewRestartLadderReconcilerWithConfig(s store.Store, dispatcher RuntimeDispatcher, logger *zap.Logger, config RestartLadderReconcilerConfig) *RestartLadderReconciler {
	config.setDefaults()
	return &RestartLadderReconciler{
		store:      s,
		dispatcher: dispatcher,
		logger:     logger.With(zap.String("reconciler", "restart-ladder")),
		config:     config,
	}
}

// Reconcile checks whether an AgentInstance needs escalation and executes
// the next restart-ladder step if applicable.
func (r *RestartLadderReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	// Fetch the AgentInstance.
	raw, err := r.store.Get(ctx, agentInstanceKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("agent instance not found, skipping ladder check")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get agent instance: %w", err)
	}

	var instance v1alpha1.AgentInstance
	if err := json.Unmarshal(raw, &instance); err != nil {
		return Result{}, fmt.Errorf("unmarshal agent instance: %w", err)
	}
	originalStatus := cloneRestartLadderStatus(instance.Status)

	// Only process running or degraded instances.
	if instance.Status.Phase != v1alpha1.PhaseRunning && instance.Status.Phase != v1alpha1.PhaseDegraded {
		return Result{}, nil
	}

	// Resolve probe spec and restart ladder from parent Agent.
	agentName := instance.Spec.AgentRef.Name
	if agentName == "" {
		return Result{}, nil
	}

	agentRaw, err := r.store.Get(ctx, "Agent", instance.ObjectMeta.Namespace, agentName)
	if err != nil {
		log.Warn("failed to resolve parent agent", zap.Error(err))
		return Result{RequeueAfter: r.config.DefaultStabilisation}, nil
	}

	var agent v1alpha1.Agent
	if err := json.Unmarshal(agentRaw, &agent); err != nil {
		return Result{}, fmt.Errorf("unmarshal agent: %w", err)
	}

	// No restart ladder configured — nothing to do.
	if agent.Spec.RestartLadder == nil || len(agent.Spec.RestartLadder.Steps) == 0 {
		return Result{}, nil
	}

	// Check if threshold is breached.
	if !IsThresholdBreached(instance.Status.ProbeStatus, agent.Spec.Probes) {
		// No breach — if we were mid-ladder, recovery happened.
		if instance.Status.ProbeStatus != nil &&
			(instance.Status.ProbeStatus.LadderStep > 0 ||
				instance.Status.ProbeStatus.LadderAttempts > 0 ||
				instance.Status.ProbeStatus.LastLadderAction != nil) {
			// Reset ladder state on recovery.
			instance.Status.ProbeStatus.LadderStep = 0
			instance.Status.ProbeStatus.LadderAttempts = 0
			instance.Status.ProbeStatus.LastLadderAction = nil
			if instance.Status.Phase == v1alpha1.PhaseDegraded {
				instance.Status.Phase = v1alpha1.PhaseRunning
			}
			r.removeQuarantineCondition(&instance)
			if err := r.updateInstance(ctx, &instance, originalStatus); err != nil {
				return Result{}, fmt.Errorf("update instance after recovery: %w", err)
			}
			log.Info("instance recovered, ladder reset")
		}
		return Result{}, nil
	}

	// Threshold breached — execute ladder.
	ps := instance.Status.ProbeStatus
	ladder := agent.Spec.RestartLadder

	stepIdx := int(ps.LadderStep)
	if stepIdx >= len(ladder.Steps) {
		// Already exhausted all ladder steps — remain in current state.
		log.Warn("restart ladder exhausted, no further escalation possible")
		return Result{}, nil
	}

	step := ladder.Steps[stepIdx]
	maxAttempts := step.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	// Check if we've exhausted attempts at this step — escalate.
	if ps.LadderAttempts >= maxAttempts {
		ps.LadderStep++
		ps.LadderAttempts = 0
		if int(ps.LadderStep) >= len(ladder.Steps) {
			log.Warn("restart ladder exhausted after step escalation")
			if err := r.updateInstance(ctx, &instance, originalStatus); err != nil {
				return Result{}, err
			}
			return Result{}, nil
		}
		step = ladder.Steps[ps.LadderStep]
	}

	// Execute the ladder action.
	action := step.Action
	log.Info("executing restart ladder action",
		zap.String("action", string(action)),
		zap.Int32("step", ps.LadderStep),
		zap.Int32("attempt", ps.LadderAttempts+1),
	)

	if err := r.executeLadderAction(ctx, action, &instance); err != nil {
		log.Error("ladder action failed", zap.String("action", string(action)), zap.Error(err))
		// Still count the attempt.
	}

	ps.LadderAttempts++
	ps.LastLadderAction = &action

	if err := r.updateInstance(ctx, &instance, originalStatus); err != nil {
		return Result{}, fmt.Errorf("update instance after ladder action: %w", err)
	}

	// Requeue after stabilisation period to re-evaluate.
	stabilisation := time.Duration(step.StabilisationSeconds) * time.Second
	if stabilisation <= 0 {
		stabilisation = r.config.DefaultStabilisation
	}
	return Result{RequeueAfter: stabilisation}, nil
}

// executeLadderAction performs the specified restart-ladder action.
func (r *RestartLadderReconciler) executeLadderAction(ctx context.Context, action v1alpha1.RestartAction, instance *v1alpha1.AgentInstance) error {
	switch action {
	case v1alpha1.RestartActionRestart:
		return r.actionRestart(ctx, instance)
	case v1alpha1.RestartActionQuarantine:
		return r.actionQuarantine(instance)
	case v1alpha1.RestartActionAlert:
		return r.actionAlert(ctx, instance)
	case v1alpha1.RestartActionDegradeModel:
		return r.actionDegradeModel(instance)
	case v1alpha1.RestartActionRollbackRevision:
		return r.actionRollbackRevision(ctx, instance)
	default:
		return fmt.Errorf("unknown ladder action: %s", action)
	}
}

// actionRestart re-dispatches the agent instance.
func (r *RestartLadderReconciler) actionRestart(ctx context.Context, instance *v1alpha1.AgentInstance) error {
	sessionID, err := r.dispatcher.Dispatch(ctx, instance)
	if err != nil {
		return fmt.Errorf("restart dispatch: %w", err)
	}
	instance.Status.SessionID = sessionID
	instance.Status.RestartCount++
	instance.Status.Phase = v1alpha1.PhaseRunning
	// Reset probe counters on restart.
	if instance.Status.ProbeStatus != nil {
		instance.Status.ProbeStatus.ConsecutiveHealthFailures = 0
		instance.Status.ProbeStatus.ConsecutiveReadinessFailures = 0
		instance.Status.ProbeStatus.ConsecutiveReasoningFailures = 0
	}
	r.logger.Info("instance restarted", zap.String("sessionID", sessionID))
	return nil
}

// actionQuarantine isolates the instance from serving traffic.
func (r *RestartLadderReconciler) actionQuarantine(instance *v1alpha1.AgentInstance) error {
	instance.Status.Phase = v1alpha1.PhaseDegraded
	lcConditions := lifecycle.SetCondition(
		apiToLifecycleConditions(instance.Status.Conditions),
		lifecycle.ConditionQuarantined,
		lifecycle.ConditionTrue,
		"QuarantinedByLadder",
		"instance quarantined by restart ladder due to persistent probe failures",
	)
	instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
	return nil
}

// actionAlert emits an alert event on the instance.
func (r *RestartLadderReconciler) actionAlert(ctx context.Context, instance *v1alpha1.AgentInstance) error {
	event := store.InstanceEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      "Warning",
		Reason:    "RestartLadderAlert",
		Message: fmt.Sprintf("probe failures on instance %s/%s (ladder step %d)",
			instance.ObjectMeta.Namespace, instance.ObjectMeta.Name,
			instance.Status.ProbeStatus.LadderStep),
	}
	return r.store.AppendEvent(ctx, agentInstanceKind, instance.ObjectMeta.Namespace, instance.ObjectMeta.Name, event)
}

// actionDegradeModel sets the instance to degraded phase. The runtime
// should pick a fallback model on next dispatch based on this phase.
func (r *RestartLadderReconciler) actionDegradeModel(instance *v1alpha1.AgentInstance) error {
	instance.Status.Phase = v1alpha1.PhaseDegraded
	lcConditions := lifecycle.SetCondition(
		apiToLifecycleConditions(instance.Status.Conditions),
		lifecycle.ConditionProbeHealthy,
		lifecycle.ConditionFalse,
		"ModelDegraded",
		"model degraded by restart ladder escalation",
	)
	instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
	return nil
}

// actionRollbackRevision attempts to roll back to the previous agent revision.
func (r *RestartLadderReconciler) actionRollbackRevision(ctx context.Context, instance *v1alpha1.AgentInstance) error {
	agentName := instance.Spec.AgentRef.Name
	revisions, err := r.store.ListRevisions(ctx, "Agent", instance.ObjectMeta.Namespace, agentName)
	if err != nil {
		return fmt.Errorf("list revisions for rollback: %w", err)
	}
	if len(revisions) < 2 {
		return fmt.Errorf("no previous revision available for rollback (have %d)", len(revisions))
	}
	// The second-to-last revision is the previous one.
	previousRev := revisions[len(revisions)-2]
	r.logger.Info("rolling back to previous revision",
		zap.String("agent", agentName),
		zap.Int64("revision", previousRev.Revision),
	)
	// Re-dispatch with the current instance — the runtime will pick up the rolled-back agent.
	sessionID, err := r.dispatcher.Dispatch(ctx, instance)
	if err != nil {
		return fmt.Errorf("dispatch after rollback: %w", err)
	}
	instance.Status.SessionID = sessionID
	instance.Status.RestartCount++
	if instance.Status.ProbeStatus != nil {
		instance.Status.ProbeStatus.RolledBackToRevision = &previousRev.Revision
	}
	return nil
}

// removeQuarantineCondition removes the quarantine condition if present.
func (r *RestartLadderReconciler) removeQuarantineCondition(instance *v1alpha1.AgentInstance) {
	lcConditions := apiToLifecycleConditions(instance.Status.Conditions)
	lcConditions = lifecycle.RemoveCondition(lcConditions, lifecycle.ConditionQuarantined)
	instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
}

// updateInstance persists the current AgentInstance state back to the store.
func (r *RestartLadderReconciler) updateInstance(ctx context.Context, instance *v1alpha1.AgentInstance, original v1alpha1.AgentInstanceStatus) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		data, err := json.Marshal(instance)
		if err != nil {
			return fmt.Errorf("marshal instance: %w", err)
		}
		err = r.store.Update(ctx, agentInstanceKind, instance.ObjectMeta.Namespace, instance.ObjectMeta.Name, data, instance.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, getErr := r.store.Get(ctx, agentInstanceKind, instance.ObjectMeta.Namespace, instance.ObjectMeta.Name)
		if getErr != nil {
			if errors.Is(getErr, store.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("re-read after conflict: %w", getErr)
		}
		var latest v1alpha1.AgentInstance
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal after conflict: %w", getErr)
		}
		mergeRestartLadderStatus(&latest.Status, original, instance.Status)
		*instance = latest
	}
	return store.ErrConflict
}

func cloneRestartLadderStatus(status v1alpha1.AgentInstanceStatus) v1alpha1.AgentInstanceStatus {
	cloned := status
	cloned.Conditions = append([]v1alpha1.Condition(nil), status.Conditions...)
	if status.ProbeStatus != nil {
		probeStatus := *status.ProbeStatus
		cloned.ProbeStatus = &probeStatus
	}
	return cloned
}

func mergeRestartLadderStatus(latest *v1alpha1.AgentInstanceStatus, original, desired v1alpha1.AgentInstanceStatus) {
	if desired.Phase != original.Phase {
		latest.Phase = desired.Phase
	}
	if desired.SessionID != original.SessionID {
		latest.SessionID = desired.SessionID
	}
	if desired.RestartCount != original.RestartCount {
		latest.RestartCount = desired.RestartCount
	}
	latest.ProbeStatus = mergeRestartLadderProbeStatus(latest.ProbeStatus, original.ProbeStatus, desired.ProbeStatus)
	for _, conditionType := range []string{string(lifecycle.ConditionProbeHealthy), string(lifecycle.ConditionQuarantined)} {
		originalCondition := restartLadderCondition(original.Conditions, conditionType)
		desiredCondition := restartLadderCondition(desired.Conditions, conditionType)
		if conditionsMatch(originalCondition, desiredCondition) {
			continue
		}
		latest.Conditions = applyConditionChange(latest.Conditions, conditionType, desiredCondition)
	}
}

func restartLadderCondition(conditions []v1alpha1.Condition, conditionType string) *v1alpha1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func mergeRestartLadderProbeStatus(latest, original, desired *v1alpha1.ProbeStatus) *v1alpha1.ProbeStatus {
	if original == nil || desired == nil {
		if original == desired {
			return latest
		}
		if desired == nil {
			return nil
		}
		cloned := *desired
		return &cloned
	}
	if latest == nil {
		latest = &v1alpha1.ProbeStatus{}
	}
	merged := *latest
	preserveConcurrentProgress := merged.LadderStep > original.LadderStep && merged.LadderStep > desired.LadderStep
	if desired.ConsecutiveHealthFailures != original.ConsecutiveHealthFailures {
		merged.ConsecutiveHealthFailures = desired.ConsecutiveHealthFailures
	}
	if desired.ConsecutiveReadinessFailures != original.ConsecutiveReadinessFailures {
		merged.ConsecutiveReadinessFailures = desired.ConsecutiveReadinessFailures
	}
	if desired.ConsecutiveReasoningFailures != original.ConsecutiveReasoningFailures {
		merged.ConsecutiveReasoningFailures = desired.ConsecutiveReasoningFailures
	}
	if !preserveConcurrentProgress {
		if desired.LadderStep != original.LadderStep {
			merged.LadderStep = desired.LadderStep
		}
		if desired.LadderAttempts != original.LadderAttempts {
			merged.LadderAttempts = desired.LadderAttempts
		}
		if desired.LastLadderAction != original.LastLadderAction {
			merged.LastLadderAction = desired.LastLadderAction
		}
	}
	if desired.RolledBackToRevision != original.RolledBackToRevision {
		merged.RolledBackToRevision = desired.RolledBackToRevision
	}
	return &merged
}

func conditionsMatch(left, right *v1alpha1.Condition) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func applyConditionChange(conditions []v1alpha1.Condition, conditionType string, desired *v1alpha1.Condition) []v1alpha1.Condition {
	merged := make([]v1alpha1.Condition, 0, len(conditions)+1)
	for _, condition := range conditions {
		if condition.Type != conditionType {
			merged = append(merged, condition)
		}
	}
	if desired != nil {
		merged = append(merged, *desired)
	}
	return merged
}
