// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// AgentJobFinalizer is the finalizer applied to AgentJob resources.
	AgentJobFinalizer = "kognition.dev/agentjob-cleanup"
)

// AgentJobReconciler reconciles AgentJob resources by creating and tracking
// one-shot AgentInstance executions.
type AgentJobReconciler struct {
	store  store.Store
	logger *zap.Logger
}

// NewAgentJobReconciler creates a new AgentJobReconciler.
func NewAgentJobReconciler(s store.Store, logger *zap.Logger) *AgentJobReconciler {
	return &AgentJobReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "agent-job")),
	}
}

// Reconcile implements the Reconciler interface for AgentJob resources.
func (r *AgentJobReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	raw, err := r.store.Get(ctx, "AgentJob", req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("agentjob not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get agentjob: %w", err)
	}

	var job v1alpha1.AgentJob
	if err := json.Unmarshal(raw, &job); err != nil {
		return Result{}, fmt.Errorf("unmarshal agentjob: %w", err)
	}
	originalStatus := job.Status
	originalFinalizers := append([]string(nil), job.ObjectMeta.Finalizers...)

	if job.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &job, originalStatus, originalFinalizers, log)
	}

	if !lifecycle.HasFinalizer(job.ObjectMeta.Finalizers, AgentJobFinalizer) {
		finalizers, _ := lifecycle.AddFinalizer(job.ObjectMeta.Finalizers, AgentJobFinalizer)
		job.ObjectMeta.Finalizers = finalizers
		if err := r.updateJob(ctx, &job, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	if job.Status.Phase == v1alpha1.PhaseReady || job.Status.Phase == v1alpha1.PhaseFailed {
		log.Debug("agentjob already in terminal phase, skipping", zap.String("phase", string(job.Status.Phase)))
		return Result{}, nil
	}

	instances, err := r.listOwnedInstances(ctx, &job)
	if err != nil {
		return Result{}, fmt.Errorf("list owned instances: %w", err)
	}

	var succeeded, failed int32
	var activeCount int
	hardTerminalFailure := false
	failureMessages := make([]string, 0)
	for _, inst := range instances {
		switch inst.Status.Phase {
		case v1alpha1.PhaseReady:
			succeeded++
		case v1alpha1.PhaseFailed:
			failed++
			if message := terminalInstanceMessage(inst); message != "" {
				failureMessages = append(failureMessages, message)
			}
		case v1alpha1.PhasePending, v1alpha1.PhaseRunning:
			if message, terminal := terminalPendingInstanceFailure(inst); terminal {
				failed++
				hardTerminalFailure = true
				failureMessages = append(failureMessages, message)
			} else {
				activeCount++
			}
		}
	}

	job.Status.Succeeded = succeeded
	job.Status.Failed = failed

	if succeeded > 0 {
		// Prefer the ready instance that produced output, preserving the previous
		// terminal output behavior while also carrying runtime telemetry forward.
		selected := -1
		for i := range instances {
			if instances[i].Status.Phase == v1alpha1.PhaseReady {
				if selected == -1 {
					selected = i
				}
				if instances[i].Status.Output != "" {
					selected = i
					break
				}
			}
		}
		if selected >= 0 {
			job.Status.StartTime = instances[selected].Status.StartTime
			job.Status.TokenUsage = cloneTokenUsage(instances[selected].Status.TokenUsage)
			job.Status.ToolCalls = instances[selected].Status.ToolCalls
			job.Status.Output = instances[selected].Status.Output
		}
		now := time.Now().UTC()
		job.Status.Phase = v1alpha1.PhaseReady
		job.Status.CompletionTime = &now
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(job.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionTrue,
			"Completed",
			"agent job completed successfully",
		)
		job.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateJob(ctx, &job, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("update completed job: %w", err)
		}
		return Result{}, nil
	}

	completionPolicy := job.Spec.CompletionPolicy
	if completionPolicy == "" {
		completionPolicy = v1alpha1.CompletionPolicySuccess
	}

	backoffLimit := int32(0)
	if job.Spec.BackoffLimit != nil {
		backoffLimit = *job.Spec.BackoffLimit
	}

	if failed > 0 {
		terminalFailure := hardTerminalFailure || completionPolicy == v1alpha1.CompletionPolicyAny || failed > backoffLimit
		if terminalFailure {
			now := time.Now().UTC()
			message := fmt.Sprintf("agent job failed after %d failed attempt(s)", failed)
			if len(failureMessages) > 0 {
				message = failureMessages[0]
				job.Status.Output = message
			}
			job.Status.Phase = v1alpha1.PhaseFailed
			job.Status.CompletionTime = &now
			lc := lifecycle.SetCondition(
				apiToLifecycleConditions(job.Status.Conditions),
				lifecycle.ConditionReady,
				lifecycle.ConditionFalse,
				"Failed",
				message,
			)
			job.Status.Conditions = lifecycleToAPIConditions(lc)
			if err := r.updateJob(ctx, &job, originalStatus, originalFinalizers); err != nil {
				return Result{}, fmt.Errorf("update failed job: %w", err)
			}
			return Result{}, nil
		}
	}

	if activeCount == 0 {
		agentRefName, err := r.resolveJobAgentRef(ctx, &job)
		if err != nil {
			now := time.Now().UTC()
			job.Status.Phase = v1alpha1.PhaseFailed
			job.Status.Failed = 1
			job.Status.CompletionTime = &now
			lc := lifecycle.SetCondition(
				apiToLifecycleConditions(job.Status.Conditions),
				lifecycle.ConditionReady,
				lifecycle.ConditionFalse,
				"InvalidSpec",
				err.Error(),
			)
			job.Status.Conditions = lifecycleToAPIConditions(lc)
			if updateErr := r.updateJob(ctx, &job, originalStatus, originalFinalizers); updateErr != nil {
				return Result{}, fmt.Errorf("update invalid-spec job: %w", updateErr)
			}
			return Result{}, nil
		}

		if err := r.createInstance(ctx, &job, agentRefName, int64(failed+1), log); err != nil {
			return Result{}, fmt.Errorf("create job instance: %w", err)
		}
		job.Status.Phase = v1alpha1.PhasePending
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(job.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"Executing",
			"agent job execution in progress",
		)
		job.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateJob(ctx, &job, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("update pending job: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	job.Status.Phase = v1alpha1.PhaseRunning
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(job.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		"Executing",
		"agent job execution in progress",
	)
	job.Status.Conditions = lifecycleToAPIConditions(lc)
	if err := r.updateJob(ctx, &job, originalStatus, originalFinalizers); err != nil {
		return Result{}, fmt.Errorf("update running job: %w", err)
	}

	return Result{RequeueAfter: 2 * time.Second}, nil
}

func (r *AgentJobReconciler) handleDeletion(ctx context.Context, job *v1alpha1.AgentJob, originalStatus v1alpha1.AgentJobStatus, originalFinalizers []string, _ *zap.Logger) (Result, error) {
	instances, err := r.listOwnedInstances(ctx, job)
	if err != nil {
		return Result{}, fmt.Errorf("list instances for deletion: %w", err)
	}
	for _, inst := range instances {
		if err := r.store.Delete(ctx, agentInstanceKind, inst.ObjectMeta.Namespace, inst.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
			return Result{}, fmt.Errorf("delete job instance %s: %w", inst.ObjectMeta.Name, err)
		}
	}

	if job.Spec.AgentRef == nil && job.Spec.AgentSpec != nil {
		inlineAgentName := inlineAgentNameForJob(job)
		raw, err := r.store.Get(ctx, "Agent", job.ObjectMeta.Namespace, inlineAgentName)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return Result{}, fmt.Errorf("get inline agent %s for deletion: %w", inlineAgentName, err)
		}
		if err == nil {
			owned, ownershipErr := inlineAgentOwnedByJob(raw, job)
			if ownershipErr == nil && owned {
				if err := r.store.Delete(ctx, "Agent", job.ObjectMeta.Namespace, inlineAgentName); err != nil && !errors.Is(err, store.ErrNotFound) {
					return Result{}, fmt.Errorf("delete inline agent %s: %w", inlineAgentName, err)
				}
			}
		}
	}

	finalizers, removed := lifecycle.RemoveFinalizer(job.ObjectMeta.Finalizers, AgentJobFinalizer)
	if removed {
		job.ObjectMeta.Finalizers = finalizers
		if err := r.updateJob(ctx, job, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
	}

	return Result{}, nil
}

func (r *AgentJobReconciler) createInstance(ctx context.Context, job *v1alpha1.AgentJob, agentRefName string, attempt int64, _ *zap.Logger) error {
	// Deterministic instance name keyed by attempt number so concurrent/retried
	// reconciles converge on the same object rather than spawning duplicates.
	instanceName := fmt.Sprintf("%s-r%d", job.ObjectMeta.Name, attempt)
	instance := v1alpha1.AgentInstance{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: agentInstanceKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              instanceName,
			Namespace:         job.ObjectMeta.Namespace,
			UID:               uuid.New().String(),
			CreationTimestamp: time.Now().UTC(),
			Annotations:       cloneStringMap(job.ObjectMeta.Annotations),
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       "AgentJob",
				Name:       job.ObjectMeta.Name,
				UID:        job.ObjectMeta.UID,
			}},
		},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef:       v1alpha1.LocalObjectReference{Name: agentRefName},
			Revision:       attempt,
			AgentRevision:  job.Spec.AgentRevision,
			SessionContext: map[string]string{"input": job.Spec.Input},
		},
		Status: v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhasePending},
	}

	data, err := json.Marshal(instance)
	if err != nil {
		return fmt.Errorf("marshal instance: %w", err)
	}
	if err := r.store.Create(ctx, agentInstanceKind, job.ObjectMeta.Namespace, instanceName, data); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil
		}
		return fmt.Errorf("store instance: %w", err)
	}
	return nil
}

func (r *AgentJobReconciler) listOwnedInstances(ctx context.Context, job *v1alpha1.AgentJob) ([]v1alpha1.AgentInstance, error) {
	rawList, err := r.store.List(ctx, agentInstanceKind, store.ListOptions{Namespace: job.ObjectMeta.Namespace})
	if err != nil {
		return nil, err
	}

	var owned []v1alpha1.AgentInstance
	for _, raw := range rawList {
		var inst v1alpha1.AgentInstance
		if err := json.Unmarshal(raw, &inst); err != nil {
			continue
		}
		for _, ref := range inst.ObjectMeta.OwnerReferences {
			if ref.Kind == "AgentJob" && ref.Name == job.ObjectMeta.Name && ref.UID == job.ObjectMeta.UID {
				owned = append(owned, inst)
				break
			}
		}
	}
	return owned, nil
}

func terminalPendingInstanceFailure(inst v1alpha1.AgentInstance) (string, bool) {
	if inst.Status.Phase != v1alpha1.PhasePending {
		return "", false
	}
	condition := conditionByType(inst.Status.Conditions, string(lifecycle.ConditionReady))
	if condition == nil || condition.Status != string(lifecycle.ConditionFalse) {
		return "", false
	}
	switch condition.Reason {
	case "ModelAccessDenied":
		return terminalConditionMessage(*condition), true
	default:
		return "", false
	}
}

func terminalInstanceMessage(inst v1alpha1.AgentInstance) string {
	condition := conditionByType(inst.Status.Conditions, string(lifecycle.ConditionReady))
	if condition == nil || condition.Status != string(lifecycle.ConditionFalse) {
		return ""
	}
	return terminalConditionMessage(*condition)
}

func conditionByType(conditions []v1alpha1.Condition, conditionType string) *v1alpha1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

func terminalConditionMessage(condition v1alpha1.Condition) string {
	if condition.Message != "" {
		return condition.Message
	}
	if condition.Reason != "" {
		return condition.Reason
	}
	return "agent instance failed"
}

func (r *AgentJobReconciler) updateJob(ctx context.Context, job *v1alpha1.AgentJob, originalStatus v1alpha1.AgentJobStatus, originalFinalizers []string) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		data, err := json.Marshal(job)
		if err != nil {
			return fmt.Errorf("marshal job: %w", err)
		}
		err = r.store.Update(ctx, "AgentJob", job.ObjectMeta.Namespace, job.ObjectMeta.Name, data, job.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, getErr := r.store.Get(ctx, "AgentJob", job.ObjectMeta.Namespace, job.ObjectMeta.Name)
		if getErr != nil {
			if errors.Is(getErr, store.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("re-read after conflict: %w", getErr)
		}
		var latest v1alpha1.AgentJob
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal after conflict: %w", getErr)
		}
		mergeAgentJobStatus(&latest.Status, originalStatus, job.Status)
		originalHasFinalizer := hasString(originalFinalizers, AgentJobFinalizer)
		desiredHasFinalizer := hasString(job.ObjectMeta.Finalizers, AgentJobFinalizer)
		if originalHasFinalizer != desiredHasFinalizer {
			latest.ObjectMeta.Finalizers = mergeOwnedFinalizer(
				latest.ObjectMeta.Finalizers,
				job.ObjectMeta.Finalizers,
				AgentJobFinalizer,
				!desiredHasFinalizer,
			)
		}
		*job = latest
	}
	return store.ErrConflict
}

func mergeAgentJobStatus(latest *v1alpha1.AgentJobStatus, original, desired v1alpha1.AgentJobStatus) {
	latestBecameTerminal := latest.Phase != original.Phase &&
		(latest.Phase == v1alpha1.PhaseReady || latest.Phase == v1alpha1.PhaseFailed)
	if latestBecameTerminal {
		return
	}
	if desired.Phase != original.Phase {
		latest.Phase = desired.Phase
	}
	if desired.Succeeded != original.Succeeded {
		latest.Succeeded = desired.Succeeded
	}
	if desired.Failed != original.Failed {
		latest.Failed = desired.Failed
	}
	if !agentJobTimesMatch(original.CompletionTime, desired.CompletionTime) {
		latest.CompletionTime = desired.CompletionTime
	}
	if !agentJobTimesMatch(original.StartTime, desired.StartTime) {
		latest.StartTime = desired.StartTime
	}
	if !tokenUsageMatch(original.TokenUsage, desired.TokenUsage) {
		latest.TokenUsage = cloneTokenUsage(desired.TokenUsage)
	}
	if desired.ToolCalls != original.ToolCalls {
		latest.ToolCalls = desired.ToolCalls
	}
	if desired.Output != original.Output {
		latest.Output = desired.Output
	}
	conditionType := string(lifecycle.ConditionReady)
	originalCondition := restartLadderCondition(original.Conditions, conditionType)
	desiredCondition := restartLadderCondition(desired.Conditions, conditionType)
	if !conditionsMatch(originalCondition, desiredCondition) {
		latest.Conditions = applyConditionChange(latest.Conditions, conditionType, desiredCondition)
	}
}

func agentJobTimesMatch(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Equal(*right)
}

func cloneTokenUsage(in *v1alpha1.TokenUsage) *v1alpha1.TokenUsage {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func tokenUsageMatch(left, right *v1alpha1.TokenUsage) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func (r *AgentJobReconciler) resolveJobAgentRef(ctx context.Context, job *v1alpha1.AgentJob) (string, error) {
	if job.Spec.AgentRef != nil && job.Spec.AgentRef.Name != "" {
		return job.Spec.AgentRef.Name, nil
	}
	if job.Spec.AgentSpec != nil {
		inlineName := inlineAgentNameForJob(job)
		if err := r.ensureInlineAgent(ctx, job, inlineName); err != nil {
			return "", err
		}
		return inlineName, nil
	}
	return "", fmt.Errorf("agentjob spec must set agentRef or agentSpec")
}

func inlineAgentNameForJob(job *v1alpha1.AgentJob) string {
	return fmt.Sprintf("%s-inline", job.ObjectMeta.Name)
}

func (r *AgentJobReconciler) ensureInlineAgent(ctx context.Context, job *v1alpha1.AgentJob, name string) error {
	if raw, err := r.store.Get(ctx, "Agent", job.ObjectMeta.Namespace, name); err == nil {
		return validateInlineAgentOwnership(raw, job, name)
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("get inline agent: %w", err)
	}

	replicas := int32(1)
	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              name,
			Namespace:         job.ObjectMeta.Namespace,
			UID:               uuid.New().String(),
			CreationTimestamp: time.Now().UTC(),
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       "AgentJob",
				Name:       job.ObjectMeta.Name,
				UID:        job.ObjectMeta.UID,
			}},
		},
		Spec: *job.Spec.AgentSpec,
		Status: v1alpha1.AgentStatus{
			Phase: v1alpha1.PhasePending,
		},
	}
	agent.Spec.Replicas = &replicas

	data, err := json.Marshal(agent)
	if err != nil {
		return fmt.Errorf("marshal inline agent: %w", err)
	}
	if err := r.store.Create(ctx, "Agent", job.ObjectMeta.Namespace, name, data); errors.Is(err, store.ErrAlreadyExists) {
		raw, getErr := r.store.Get(ctx, "Agent", job.ObjectMeta.Namespace, name)
		if getErr != nil {
			return fmt.Errorf("get inline agent after create conflict: %w", getErr)
		}
		return validateInlineAgentOwnership(raw, job, name)
	} else if err != nil {
		return fmt.Errorf("create inline agent: %w", err)
	}
	return nil
}

func validateInlineAgentOwnership(raw []byte, job *v1alpha1.AgentJob, name string) error {
	owned, err := inlineAgentOwnedByJob(raw, job)
	if err != nil {
		return fmt.Errorf("unmarshal existing inline agent %s metadata: %w", name, err)
	}
	if !owned {
		return fmt.Errorf("existing Agent/%s is not owned by AgentJob %s/%s: %w", name, job.ObjectMeta.Namespace, job.ObjectMeta.Name, store.ErrAlreadyExists)
	}
	return nil
}

func inlineAgentOwnedByJob(raw []byte, job *v1alpha1.AgentJob) (bool, error) {
	var meta struct {
		Metadata v1alpha1.ObjectMeta `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return false, err
	}
	for _, ref := range meta.Metadata.OwnerReferences {
		if ref.Kind == "AgentJob" && ref.Name == job.ObjectMeta.Name && ref.UID == job.ObjectMeta.UID {
			return true, nil
		}
	}
	return false, nil
}
