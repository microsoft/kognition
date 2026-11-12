// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
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

// ProbeReconcilerConfig controls probe polling intervals and defaults.
type ProbeReconcilerConfig struct {
	// DefaultHealthInterval is the fallback health probe interval.
	DefaultHealthInterval time.Duration

	// DefaultReadinessInterval is the fallback readiness probe interval.
	DefaultReadinessInterval time.Duration

	// DefaultReasoningInterval is the fallback reasoning probe interval.
	DefaultReasoningInterval time.Duration
}

// CanaryExecutor runs an isolated semantic canary for a reasoning probe.
type CanaryExecutor interface {
	PrepareCanary(context.Context, CanaryRequest) (CanaryRequest, error)
	ExecuteCanary(context.Context, CanaryRequest) (CanaryResult, error)
}

// CanaryRequest contains the immutable inputs and execution bounds for one canary.
type CanaryRequest struct {
	OperationID     string
	ScheduleSlot    time.Time
	RequestDigest   string
	Instance        *v1alpha1.AgentInstance
	SessionID       string
	AgentRevision   int64
	Bindings        []v1alpha1.CanaryResourceBinding
	Instructions    string
	CanaryTask      string
	ExpectedOutcome string
	Timeout         time.Duration
	MaxInputTokens  int32
	MaxOutputTokens int32
}

// CanaryOutcome distinguishes semantic mismatch from successful execution.
type CanaryOutcome = v1alpha1.CanaryOutcome

const (
	CanaryOutcomePassed          = v1alpha1.CanaryOutcomePassed
	CanaryOutcomeMismatch        = v1alpha1.CanaryOutcomeMismatch
	CanaryOutcomeExecutionFailed = v1alpha1.CanaryOutcomeExecutionFailed
)

// CanaryResult is the metadata-only result of a canary execution.
type CanaryResult struct {
	OperationID            string
	ExpectedBytes          int32
	ActualBytes            int32
	ExpectedSHA256         string
	ActualSHA256           string
	PromptTokens           int64
	CompletionTokens       int64
	TotalTokens            int64
	ProviderDurationMillis int64
	TraceID                string
	SpanID                 string
	Outcome                CanaryOutcome
	ClosedReason           v1alpha1.CanaryFailureReason
}

func (c *ProbeReconcilerConfig) setDefaults() {
	if c.DefaultHealthInterval <= 0 {
		c.DefaultHealthInterval = 30 * time.Second
	}
	if c.DefaultReadinessInterval <= 0 {
		c.DefaultReadinessInterval = 30 * time.Second
	}
	if c.DefaultReasoningInterval <= 0 {
		c.DefaultReasoningInterval = 300 * time.Second
	}
}

// ProbeReconciler reconciles probe checks for running AgentInstances.
// It reads the parent Agent's probe spec and evaluates health/readiness
// against the RuntimeDispatcher status.
type ProbeReconciler struct {
	store          store.Store
	dispatcher     RuntimeDispatcher
	canaryExecutor CanaryExecutor
	logger         *zap.Logger
	config         ProbeReconcilerConfig
	now            func() time.Time
}

// SetCanaryExecutor configures semantic canary execution for ReasoningProbe.
func (r *ProbeReconciler) SetCanaryExecutor(executor CanaryExecutor) {
	r.canaryExecutor = executor
}

// NewProbeReconciler creates a ProbeReconciler with default configuration.
func NewProbeReconciler(s store.Store, dispatcher RuntimeDispatcher, logger *zap.Logger) *ProbeReconciler {
	config := ProbeReconcilerConfig{}
	config.setDefaults()
	return &ProbeReconciler{
		store:      s,
		dispatcher: dispatcher,
		logger:     logger.With(zap.String("reconciler", "probe")),
		config:     config,
		now:        time.Now,
	}
}

// NewProbeReconcilerWithConfig creates a ProbeReconciler with custom configuration.
func NewProbeReconcilerWithConfig(s store.Store, dispatcher RuntimeDispatcher, logger *zap.Logger, config ProbeReconcilerConfig) *ProbeReconciler {
	config.setDefaults()
	return &ProbeReconciler{
		store:      s,
		dispatcher: dispatcher,
		logger:     logger.With(zap.String("reconciler", "probe")),
		config:     config,
		now:        time.Now,
	}
}

// Reconcile performs a probe check cycle for a single AgentInstance.
func (r *ProbeReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	// Fetch the AgentInstance.
	raw, err := r.store.Get(ctx, agentInstanceKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("agent instance not found, skipping probe")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get agent instance: %w", err)
	}

	var instance v1alpha1.AgentInstance
	if err := json.Unmarshal(raw, &instance); err != nil {
		return Result{}, fmt.Errorf("unmarshal agent instance: %w", err)
	}

	// Only probe instances in Running phase.
	if instance.Status.Phase != v1alpha1.PhaseRunning {
		return Result{}, nil
	}

	// Resolve the parent Agent to get probe configuration.
	probeSpec, err := r.resolveProbeSpec(ctx, &instance)
	if err != nil {
		log.Warn("failed to resolve probe spec, skipping probe cycle", zap.Error(err))
		return Result{RequeueAfter: r.config.DefaultHealthInterval}, nil
	}

	// If no probes are configured, skip.
	if probeSpec == nil {
		return Result{}, nil
	}

	// Initialize ProbeStatus if absent.
	if instance.Status.ProbeStatus == nil {
		instance.Status.ProbeStatus = &v1alpha1.ProbeStatus{}
	}

	// Get runtime status from the dispatcher.
	status, err := r.dispatcher.Status(ctx, &instance)

	now := r.now().UTC()
	nowStr := now.Format(time.RFC3339)
	instance.Status.ProbeStatus.LastProbeTime = &nowStr

	changed := false

	// Evaluate health probe.
	if probeSpec.HealthProbe != nil {
		changed = r.evaluateHealthProbe(probeSpec.HealthProbe, &instance, status, err) || changed
	}

	// Evaluate readiness probe.
	if probeSpec.ReadinessProbe != nil {
		changed = r.evaluateReadinessProbe(probeSpec.ReadinessProbe, &instance, status, err) || changed
	}

	// Evaluate reasoning probe.
	if probeSpec.ReasoningProbe != nil {
		owned, ownershipChanged, ownershipErr := r.prepareReasoningProbe(ctx, probeSpec.ReasoningProbe, &instance, now)
		changed = ownershipChanged || changed
		if ownershipErr != nil {
			return Result{}, ownershipErr
		}
		if owned {
			if ownershipChanged {
				if updateErr := r.claimReasoningSlot(ctx, &instance); updateErr != nil {
					if errors.Is(updateErr, store.ErrConflict) {
						join, reloadErr := r.reloadReasoningSlotAfterConflict(ctx, &instance)
						if reloadErr != nil {
							return Result{}, reloadErr
						}
						if !join {
							return Result{RequeueAfter: r.shortestInterval(probeSpec)}, nil
						}
					} else {
						return Result{}, fmt.Errorf("claim reasoning probe slot: %w", updateErr)
					}
				}
				changed = false
			}
			canaryResult, canaryErr := r.executeReasoningProbe(ctx, probeSpec.ReasoningProbe, &instance)
			changed = r.evaluateReasoningProbe(probeSpec.ReasoningProbe, &instance, canaryResult, canaryErr) || changed
		}
	}

	// Update conditions on the instance.
	if changed {
		if updateErr := r.updateInstance(ctx, &instance); updateErr != nil {
			return Result{}, fmt.Errorf("update instance after probe: %w", updateErr)
		}
	}

	// Determine next requeue interval (shortest configured interval).
	requeueAfter := r.shortestInterval(probeSpec)
	return Result{RequeueAfter: requeueAfter}, nil
}

// evaluateHealthProbe checks runtime liveness and updates probe status.
// Returns true if the instance was modified.
func (r *ProbeReconciler) evaluateHealthProbe(
	probe *v1alpha1.AgentHealthProbe,
	instance *v1alpha1.AgentInstance,
	status RuntimeStatus,
	statusErr error,
) bool {
	ps := instance.Status.ProbeStatus
	failureThreshold := probe.FailureThreshold
	if failureThreshold <= 0 {
		failureThreshold = 3
	}
	successThreshold := probe.SuccessThreshold
	if successThreshold <= 0 {
		successThreshold = 1
	}

	healthy := statusErr == nil && status.Alive

	if healthy {
		ps.ConsecutiveHealthFailures = 0
		if ps.Healthy == nil || !*ps.Healthy {
			ps.ConsecutiveHealthSuccesses++
			if ps.ConsecutiveHealthSuccesses >= successThreshold {
				ps.ConsecutiveHealthSuccesses = 0
				h := true
				ps.Healthy = &h
				r.setProbeCondition(instance, lifecycle.ConditionProbeHealthy, lifecycle.ConditionTrue, "ProbeHealthy", "health probe passing")
			}
			return true
		}
		return false
	}

	// Probe failed.
	ps.ConsecutiveHealthSuccesses = 0
	ps.ConsecutiveHealthFailures++
	if ps.ConsecutiveHealthFailures >= failureThreshold {
		h := false
		ps.Healthy = &h
		r.setProbeCondition(instance, lifecycle.ConditionProbeHealthy, lifecycle.ConditionFalse, "ProbeUnhealthy",
			fmt.Sprintf("health probe failed %d consecutive times (threshold: %d)", ps.ConsecutiveHealthFailures, failureThreshold))
		return true
	}

	return true // counter changed
}

// evaluateReadinessProbe checks runtime readiness and updates probe status.
// Returns true if the instance was modified.
func (r *ProbeReconciler) evaluateReadinessProbe(
	probe *v1alpha1.AgentReadinessProbe,
	instance *v1alpha1.AgentInstance,
	status RuntimeStatus,
	statusErr error,
) bool {
	ps := instance.Status.ProbeStatus
	failureThreshold := probe.FailureThreshold
	if failureThreshold <= 0 {
		failureThreshold = 3
	}
	successThreshold := probe.SuccessThreshold
	if successThreshold <= 0 {
		successThreshold = 1
	}

	// Readiness is determined by runtime being alive (no error from dispatcher).
	// In production this would check model/tool/memory reachability;
	// for now we use the same alive signal.
	ready := statusErr == nil && status.Alive

	if ready {
		ps.ConsecutiveReadinessFailures = 0
		if ps.Ready == nil || !*ps.Ready {
			ps.ConsecutiveReadinessSuccesses++
			if ps.ConsecutiveReadinessSuccesses >= successThreshold {
				ps.ConsecutiveReadinessSuccesses = 0
				rd := true
				ps.Ready = &rd
				r.setProbeCondition(instance, lifecycle.ConditionProbeReady, lifecycle.ConditionTrue, "ProbeReady", "readiness probe passing")
			}
			return true
		}
		return false
	}

	// Probe failed.
	ps.ConsecutiveReadinessSuccesses = 0
	ps.ConsecutiveReadinessFailures++
	if ps.ConsecutiveReadinessFailures >= failureThreshold {
		rd := false
		ps.Ready = &rd
		r.setProbeCondition(instance, lifecycle.ConditionProbeReady, lifecycle.ConditionFalse, "ProbeNotReady",
			fmt.Sprintf("readiness probe failed %d consecutive times (threshold: %d)", ps.ConsecutiveReadinessFailures, failureThreshold))
		return true
	}

	return true // counter changed
}

func (r *ProbeReconciler) executeReasoningProbe(ctx context.Context, probe *v1alpha1.AgentReasoningProbe, instance *v1alpha1.AgentInstance) (CanaryResult, error) {
	if r.canaryExecutor == nil {
		return CanaryResult{}, errors.New("reasoning canary executor is not configured")
	}
	timeout := time.Duration(probe.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	maxOutputTokens := probe.MaxTokens
	if maxOutputTokens <= 0 {
		maxOutputTokens = 256
	}
	maxInputTokens := probe.MaxInputTokens
	if maxInputTokens <= 0 {
		maxInputTokens = 4096
	}
	canaryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	scheduleSlot, err := parseReasoningSlot(instance.Status.ProbeStatus.LastReasoningScheduleSlot)
	if err != nil {
		return CanaryResult{}, fmt.Errorf("parse owned reasoning schedule slot: %w", err)
	}
	return r.canaryExecutor.ExecuteCanary(canaryCtx, CanaryRequest{
		OperationID:     instance.Status.ProbeStatus.ReasoningOperationID,
		ScheduleSlot:    scheduleSlot,
		RequestDigest:   instance.Status.ProbeStatus.ReasoningRequestDigest,
		Instance:        instance,
		SessionID:       instance.Status.SessionID,
		AgentRevision:   selectedAgentRevision(instance),
		Bindings:        instance.Status.ProbeStatus.ReasoningBindings,
		CanaryTask:      probe.CanaryTask,
		ExpectedOutcome: probe.ExpectedOutcome,
		Timeout:         timeout,
		MaxInputTokens:  maxInputTokens,
		MaxOutputTokens: maxOutputTokens,
	})
}

func (r *ProbeReconciler) prepareReasoningProbe(ctx context.Context, probe *v1alpha1.AgentReasoningProbe, instance *v1alpha1.AgentInstance, now time.Time) (bool, bool, error) {
	if r.canaryExecutor == nil {
		return true, false, nil
	}
	interval := time.Duration(probe.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = r.config.DefaultReasoningInterval
	}
	slot, err := canonicalReasoningSlot(instance.ObjectMeta.CreationTimestamp, now, interval)
	if err != nil {
		return false, false, err
	}
	ps := instance.Status.ProbeStatus
	if ps.LastReasoningScheduleSlot != nil {
		last, err := parseReasoningSlot(ps.LastReasoningScheduleSlot)
		if err != nil {
			return false, false, fmt.Errorf("parse persisted reasoning schedule slot: %w", err)
		}
		if last.After(slot) {
			return false, false, nil
		}
		if last.Equal(slot) {
			return ps.LastReasoningResult == nil, false, nil
		}
		if ps.LastReasoningResult == nil {
			return true, false, nil
		}
	}
	request := CanaryRequest{
		OperationID:  deriveCanaryOperationID(instance.ObjectMeta.UID, instance.Status.SessionID, selectedAgentRevision(instance), slot),
		ScheduleSlot: slot, Instance: instance, SessionID: instance.Status.SessionID, AgentRevision: selectedAgentRevision(instance),
		CanaryTask: probe.CanaryTask, ExpectedOutcome: probe.ExpectedOutcome, Timeout: time.Duration(probe.TimeoutSeconds) * time.Second,
		MaxInputTokens: probe.MaxInputTokens, MaxOutputTokens: probe.MaxTokens,
	}
	if request.Timeout <= 0 {
		request.Timeout = 60 * time.Second
	}
	if request.MaxInputTokens <= 0 {
		request.MaxInputTokens = 4096
	}
	if request.MaxOutputTokens <= 0 {
		request.MaxOutputTokens = 256
	}
	prepared, err := r.canaryExecutor.PrepareCanary(ctx, request)
	if err != nil {
		return false, false, fmt.Errorf("prepare reasoning canary: %w", err)
	}
	request = prepared
	slotText := slot.Format(time.RFC3339Nano)
	ps.LastReasoningScheduleSlot = &slotText
	ps.ReasoningOperationID = request.OperationID
	ps.ReasoningRequestDigest = request.RequestDigest
	ps.ReasoningBindings = request.Bindings
	ps.LastReasoningResult = nil
	return true, true, nil
}

func (r *ProbeReconciler) claimReasoningSlot(ctx context.Context, instance *v1alpha1.AgentInstance) error {
	data, err := json.Marshal(instance)
	if err != nil {
		return err
	}
	return r.store.Update(ctx, agentInstanceKind, instance.ObjectMeta.Namespace, instance.ObjectMeta.Name, data, instance.ObjectMeta.ResourceVersion)
}

func (r *ProbeReconciler) reloadReasoningSlotAfterConflict(ctx context.Context, instance *v1alpha1.AgentInstance) (bool, error) {
	desiredSlot, err := parseReasoningSlot(instance.Status.ProbeStatus.LastReasoningScheduleSlot)
	if err != nil {
		return false, fmt.Errorf("parse desired reasoning schedule slot after conflict: %w", err)
	}
	raw, err := r.store.Get(ctx, agentInstanceKind, instance.ObjectMeta.Namespace, instance.ObjectMeta.Name)
	if err != nil {
		return false, fmt.Errorf("reload reasoning probe slot after conflict: %w", err)
	}
	var latest v1alpha1.AgentInstance
	if err := json.Unmarshal(raw, &latest); err != nil {
		return false, fmt.Errorf("unmarshal reasoning probe slot after conflict: %w", err)
	}
	if latest.Status.ProbeStatus == nil || latest.Status.ProbeStatus.LastReasoningScheduleSlot == nil {
		return false, nil
	}
	latestSlot, err := parseReasoningSlot(latest.Status.ProbeStatus.LastReasoningScheduleSlot)
	if err != nil {
		return false, fmt.Errorf("parse persisted reasoning schedule slot after conflict: %w", err)
	}
	*instance = latest
	return latestSlot.Equal(desiredSlot) && latest.Status.ProbeStatus.LastReasoningResult == nil, nil
}

func canonicalReasoningSlot(anchor, now time.Time, interval time.Duration) (time.Time, error) {
	anchor = anchor.UTC()
	if anchor.IsZero() {
		return time.Time{}, errors.New("reasoning schedule requires an instance creation timestamp")
	}
	if interval <= 0 {
		return time.Time{}, errors.New("reasoning schedule interval must be positive")
	}
	if now.Before(anchor) {
		return time.Time{}, errors.New("reasoning schedule time precedes the instance creation timestamp")
	}
	return anchor.Add(now.Sub(anchor) / interval * interval), nil
}

func deriveCanaryOperationID(instanceUID, sessionID string, revision int64, slot time.Time) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s", instanceUID, sessionID, revision, slot.UTC().Format(time.RFC3339Nano))))
	return fmt.Sprintf("reasoning-canary-%x", digest[:])
}

func parseReasoningSlot(slot *string) (time.Time, error) {
	if slot == nil || *slot == "" {
		return time.Time{}, errors.New("reasoning schedule slot is missing")
	}
	parsed, err := time.Parse(time.RFC3339Nano, *slot)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func selectedAgentRevision(instance *v1alpha1.AgentInstance) int64 {
	if instance.Spec.AgentRevision != nil {
		return *instance.Spec.AgentRevision
	}
	return instance.Spec.Revision
}

// evaluateReasoningProbe checks whether the semantic canary passed.
// Returns true if the instance was modified.
func (r *ProbeReconciler) evaluateReasoningProbe(
	probe *v1alpha1.AgentReasoningProbe,
	instance *v1alpha1.AgentInstance,
	result CanaryResult,
	canaryErr error,
) bool {
	ps := instance.Status.ProbeStatus
	failureThreshold := probe.FailureThreshold
	if failureThreshold <= 0 {
		failureThreshold = 2
	}
	successThreshold := probe.SuccessThreshold
	if successThreshold <= 0 {
		successThreshold = 1
	}
	if canaryErr != nil && result.Outcome == "" {
		result.Outcome = CanaryOutcomeExecutionFailed
	}
	if result.Outcome == CanaryOutcomeExecutionFailed && result.ClosedReason == 0 {
		result.ClosedReason = v1alpha1.CanaryFailureAccountingUncertain
	}

	reasoningOK := canaryErr == nil && result.Outcome == CanaryOutcomePassed
	ps.LastReasoningResult = &v1alpha1.ReasoningCanaryResult{
		OperationID:            result.OperationID,
		ExpectedBytes:          result.ExpectedBytes,
		ActualBytes:            result.ActualBytes,
		ExpectedSHA256:         result.ExpectedSHA256,
		ActualSHA256:           result.ActualSHA256,
		PromptTokens:           result.PromptTokens,
		CompletionTokens:       result.CompletionTokens,
		TotalTokens:            result.TotalTokens,
		ProviderDurationMillis: result.ProviderDurationMillis,
		TraceID:                result.TraceID,
		SpanID:                 result.SpanID,
		Outcome:                result.Outcome,
		ClosedReason:           result.ClosedReason,
	}

	if reasoningOK {
		ps.ConsecutiveReasoningFailures = 0
		if ps.ReasoningHealthy == nil || !*ps.ReasoningHealthy {
			ps.ConsecutiveReasoningSuccesses++
			if ps.ConsecutiveReasoningSuccesses >= successThreshold {
				ps.ConsecutiveReasoningSuccesses = 0
				rh := true
				ps.ReasoningHealthy = &rh
				r.setProbeCondition(instance, lifecycle.ConditionReasoningHealthy, lifecycle.ConditionTrue, "ReasoningHealthy", "reasoning probe passing")
			}
			return true
		}
		return false
	}

	// Probe failed.
	ps.ConsecutiveReasoningSuccesses = 0
	ps.ConsecutiveReasoningFailures++
	if ps.ConsecutiveReasoningFailures >= failureThreshold {
		reason := "ReasoningExecutionFailed"
		if canaryErr == nil && result.Outcome == CanaryOutcomeMismatch {
			reason = "ReasoningOutcomeMismatch"
		}
		rh := false
		ps.ReasoningHealthy = &rh
		r.setProbeCondition(instance, lifecycle.ConditionReasoningHealthy, lifecycle.ConditionFalse, reason,
			fmt.Sprintf("reasoning probe failed %d consecutive times (threshold: %d)", ps.ConsecutiveReasoningFailures, failureThreshold))
		return true
	}

	return true // counter changed
}

// setProbeCondition updates a condition on the instance using lifecycle helpers.
func (r *ProbeReconciler) setProbeCondition(instance *v1alpha1.AgentInstance, condType lifecycle.ConditionType, status lifecycle.ConditionStatus, reason, message string) {
	lcConditions := lifecycle.SetCondition(
		apiToLifecycleConditions(instance.Status.Conditions),
		condType,
		status,
		reason,
		message,
	)
	instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
}

// resolveProbeSpec fetches the parent Agent and returns its probe specification.
func (r *ProbeReconciler) resolveProbeSpec(ctx context.Context, instance *v1alpha1.AgentInstance) (*v1alpha1.AgentProbeSpec, error) {
	agentName := instance.Spec.AgentRef.Name
	if agentName == "" {
		return nil, nil
	}

	raw, err := r.store.Get(ctx, "Agent", instance.ObjectMeta.Namespace, agentName)
	if err != nil {
		return nil, fmt.Errorf("get agent %q: %w", agentName, err)
	}

	var agent v1alpha1.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		return nil, fmt.Errorf("unmarshal agent %q: %w", agentName, err)
	}

	return agent.Spec.Probes, nil
}

// shortestInterval returns the minimum probe interval for requeue scheduling.
func (r *ProbeReconciler) shortestInterval(spec *v1alpha1.AgentProbeSpec) time.Duration {
	shortest := r.config.DefaultHealthInterval

	if spec.HealthProbe != nil && spec.HealthProbe.IntervalSeconds > 0 {
		d := time.Duration(spec.HealthProbe.IntervalSeconds) * time.Second
		if d < shortest {
			shortest = d
		}
	}

	if spec.ReadinessProbe != nil && spec.ReadinessProbe.IntervalSeconds > 0 {
		d := time.Duration(spec.ReadinessProbe.IntervalSeconds) * time.Second
		if d < shortest {
			shortest = d
		}
	}

	if spec.ReasoningProbe != nil && spec.ReasoningProbe.IntervalSeconds > 0 {
		d := time.Duration(spec.ReasoningProbe.IntervalSeconds) * time.Second
		if d < shortest {
			shortest = d
		}
	}

	return shortest
}

// updateInstance persists the current AgentInstance state back to the store.
func (r *ProbeReconciler) updateInstance(ctx context.Context, instance *v1alpha1.AgentInstance) error {
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
		mergeProbeStatus(&latest.Status, instance.Status)
		*instance = latest
	}
	return store.ErrConflict
}

func mergeProbeStatus(latest *v1alpha1.AgentInstanceStatus, desired v1alpha1.AgentInstanceStatus) {
	if desired.ProbeStatus != nil {
		if latest.ProbeStatus == nil {
			latest.ProbeStatus = &v1alpha1.ProbeStatus{}
		}
		latest.ProbeStatus.Healthy = desired.ProbeStatus.Healthy
		latest.ProbeStatus.Ready = desired.ProbeStatus.Ready
		latest.ProbeStatus.ReasoningHealthy = desired.ProbeStatus.ReasoningHealthy
		latest.ProbeStatus.ConsecutiveHealthFailures = desired.ProbeStatus.ConsecutiveHealthFailures
		latest.ProbeStatus.ConsecutiveReadinessFailures = desired.ProbeStatus.ConsecutiveReadinessFailures
		latest.ProbeStatus.ConsecutiveHealthSuccesses = desired.ProbeStatus.ConsecutiveHealthSuccesses
		latest.ProbeStatus.ConsecutiveReadinessSuccesses = desired.ProbeStatus.ConsecutiveReadinessSuccesses
		latest.ProbeStatus.LastProbeTime = desired.ProbeStatus.LastProbeTime
		if reasoningSlotNotOlder(latest.ProbeStatus.LastReasoningScheduleSlot, desired.ProbeStatus.LastReasoningScheduleSlot) {
			latest.ProbeStatus.ReasoningHealthy = desired.ProbeStatus.ReasoningHealthy
			latest.ProbeStatus.ConsecutiveReasoningFailures = desired.ProbeStatus.ConsecutiveReasoningFailures
			latest.ProbeStatus.ConsecutiveReasoningSuccesses = desired.ProbeStatus.ConsecutiveReasoningSuccesses
			latest.ProbeStatus.LastReasoningScheduleSlot = desired.ProbeStatus.LastReasoningScheduleSlot
			latest.ProbeStatus.ReasoningOperationID = desired.ProbeStatus.ReasoningOperationID
			latest.ProbeStatus.ReasoningRequestDigest = desired.ProbeStatus.ReasoningRequestDigest
			latest.ProbeStatus.ReasoningBindings = desired.ProbeStatus.ReasoningBindings
			latest.ProbeStatus.LastReasoningResult = desired.ProbeStatus.LastReasoningResult
		}
	}
	for _, conditionType := range []string{
		string(lifecycle.ConditionProbeHealthy),
		string(lifecycle.ConditionProbeReady),
		string(lifecycle.ConditionReasoningHealthy),
	} {
		latest.Conditions = mergeAgentCondition(latest.Conditions, desired.Conditions, conditionType)
	}
}

func reasoningSlotNotOlder(current, desired *string) bool {
	desiredSlot, err := parseReasoningSlot(desired)
	if err != nil {
		return false
	}
	currentSlot, err := parseReasoningSlot(current)
	return err != nil || !desiredSlot.Before(currentSlot)
}

// IsThresholdBreached returns true if any probe has exceeded its failure threshold,
// indicating the restart ladder should be consulted.
func IsThresholdBreached(ps *v1alpha1.ProbeStatus, spec *v1alpha1.AgentProbeSpec) bool {
	if ps == nil || spec == nil {
		return false
	}
	if spec.HealthProbe != nil {
		threshold := spec.HealthProbe.FailureThreshold
		if threshold <= 0 {
			threshold = 3
		}
		if ps.ConsecutiveHealthFailures >= threshold {
			return true
		}
	}
	if spec.ReadinessProbe != nil {
		threshold := spec.ReadinessProbe.FailureThreshold
		if threshold <= 0 {
			threshold = 3
		}
		if ps.ConsecutiveReadinessFailures >= threshold {
			return true
		}
	}
	if spec.ReasoningProbe != nil {
		threshold := spec.ReasoningProbe.FailureThreshold
		if threshold <= 0 {
			threshold = 2
		}
		if ps.ConsecutiveReasoningFailures >= threshold {
			return true
		}
	}
	return false
}
