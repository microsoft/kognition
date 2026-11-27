// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration"
)

type collaborationOutcome int

const (
	collaborationRunning collaborationOutcome = iota
	collaborationSucceeded
	collaborationFailed
	collaborationRetrying
)

func withoutString(values []string, drop string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != drop {
			out = append(out, v)
		}
	}
	return out
}

// advanceCollaborationStep records the attempt, advances the runner once and
// applies its progress to the step status.
func (r *WorkflowReconciler) advanceCollaborationStep(ctx context.Context, wf *v1alpha1.Workflow, step v1alpha1.WorkflowStep, input string, now time.Time, resuming bool) (collaborationOutcome, time.Duration, error) {
	ss := findStepStatus(wf, step.Name)
	if !resuming {
		startedAt := now
		if ss == nil {
			wf.Status.StepStatuses = append(wf.Status.StepStatuses, v1alpha1.WorkflowStepStatus{
				Name: step.Name, Phase: v1alpha1.PhaseRunning, Attempts: 1, StartedAt: &startedAt,
			})
			ss = findStepStatus(wf, step.Name)
		} else {
			ss.Attempts++
			ss.Phase = v1alpha1.PhaseRunning
			ss.Output = ""
			ss.StartedAt = &startedAt
			ss.NextRetryAt = nil
		}
	}
	run := collaborationRunFor(wf, r.ownerKind, step, ss.Attempts, input)
	var progress WorkflowCollaborationProgress
	inputErr := collaboration.ValidateInput(input)
	if inputErr != nil {
		if resuming {
			if err := r.collaborationRunner.Stop(ctx, run, "InvalidInput"); err != nil {
				return collaborationRunning, 0, fmt.Errorf("stop collaboration step %q with invalid input: %w", step.Name, err)
			}
		}
		progress = WorkflowCollaborationProgress{Done: true, Reason: inputErr.Error()}
	} else {
		if !resuming {
			if err := r.emitEvent(ctx, wf, "StepDispatched", fmt.Sprintf("collaboration step %q dispatched", step.Name), map[string]string{"step": step.Name}); err != nil {
				return collaborationRunning, 0, fmt.Errorf("append workflow event: %w", err)
			}
			// Persist the timeout and cleanup state before member contact is possible.
			wf.Status.Phase = v1alpha1.PhaseRunning
			if err := r.updateWorkflow(ctx, wf); err != nil {
				return collaborationRunning, 0, fmt.Errorf("persist collaboration step %q start: %w", step.Name, err)
			}
		}
		var err error
		progress, err = r.collaborationRunner.Advance(ctx, run)
		if err != nil {
			return collaborationRunning, 0, fmt.Errorf("advance collaboration step %q: %w", step.Name, err)
		}
	}
	switch {
	case !progress.Done:
		return collaborationRunning, 0, nil
	case progress.Succeeded:
		ss.Phase = v1alpha1.PhaseReady
		ss.Output, _ = truncateOutput(progress.Output)
		if err := r.emitEvent(ctx, wf, "CollaborationStepCompleted", fmt.Sprintf("collaboration step %q completed", step.Name), map[string]string{"step": step.Name}); err != nil {
			return collaborationSucceeded, 0, fmt.Errorf("append workflow event: %w", err)
		}
		return collaborationSucceeded, 0, nil
	}
	ss.Output = ""
	details := map[string]string{"step": step.Name, "reason": progress.Reason}
	if inputErr == nil && retryEligible(step.Retry, ss.Attempts) {
		delay := computeBackoff(step.Retry, ss.Attempts)
		retryAt := now.Add(delay)
		ss.Phase = v1alpha1.PhasePending
		ss.NextRetryAt = &retryAt
		ss.StartedAt = nil
		if err := r.emitEvent(ctx, wf, "StepRetrying", fmt.Sprintf("collaboration step %q scheduled for retry in %s", step.Name, delay), details); err != nil {
			return collaborationRetrying, delay, fmt.Errorf("append workflow event: %w", err)
		}
		return collaborationRetrying, delay, nil
	}
	ss.Phase = v1alpha1.PhaseFailed
	if err := r.emitEvent(ctx, wf, "CollaborationStepFailed", fmt.Sprintf("collaboration step %q failed: %s", step.Name, progress.Reason), details); err != nil {
		return collaborationFailed, 0, fmt.Errorf("append workflow event: %w", err)
	}
	return collaborationFailed, 0, nil
}

// WorkflowCollaborationRun identifies one attempt of a collaboration step.
type WorkflowCollaborationRun struct {
	Namespace string
	// OwnerKind, RunName and RunUID identify the object that owns the work of the step.
	OwnerKind string
	RunName   string
	RunUID    string
	Step      string
	Attempt   int32
	Input     string
	Spec      *v1alpha1.WorkflowCollaboration
}

// WorkflowCollaborationProgress is the state of a collaboration step after one pass.
type WorkflowCollaborationProgress struct {
	Done      bool
	Succeeded bool
	Output    string
	Reason    string
}

// WorkflowCollaborationRunner drives collaboration steps. Advance is idempotent
// and resumes from durable state; it is called on every reconcile pass.
type WorkflowCollaborationRunner interface {
	Advance(context.Context, WorkflowCollaborationRun) (WorkflowCollaborationProgress, error)
	Stop(context.Context, WorkflowCollaborationRun, string) error
}

// SetCollaborationRunner enables collaboration steps.
func (r *WorkflowReconciler) SetCollaborationRunner(runner WorkflowCollaborationRunner) {
	r.collaborationRunner = runner
}

// SetCollaborationRunner enables collaboration steps for WorkflowRuns.
func (r *WorkflowRunReconciler) SetCollaborationRunner(runner WorkflowCollaborationRunner) {
	r.collaborationRunner = runner
}

func collaborationRunFor(wf *v1alpha1.Workflow, ownerKind string, step v1alpha1.WorkflowStep, attempt int32, input string) WorkflowCollaborationRun {
	return WorkflowCollaborationRun{
		Namespace: wf.ObjectMeta.Namespace, OwnerKind: ownerKind, RunName: wf.ObjectMeta.Name, RunUID: wf.ObjectMeta.UID,
		Step: step.Name, Attempt: attempt, Input: input, Spec: step.Collaboration,
	}
}

// stopCollaborations stops every running collaboration step of wf.
func (r *WorkflowReconciler) stopCollaborations(ctx context.Context, wf *v1alpha1.Workflow, reason string) error {
	if r.collaborationRunner == nil {
		return nil
	}
	return stopWorkflowCollaborations(ctx, r.collaborationRunner, wf, r.ownerKind, reason)
}

func stopWorkflowCollaborations(ctx context.Context, runner WorkflowCollaborationRunner, wf *v1alpha1.Workflow, ownerKind, reason string) error {
	for _, step := range wf.Spec.Steps {
		if step.Type != v1alpha1.WorkflowStepTypeCollaboration {
			continue
		}
		ss := findStepStatus(wf, step.Name)
		if ss == nil || ss.Phase != v1alpha1.PhaseRunning {
			continue
		}
		if err := runner.Stop(ctx, collaborationRunFor(wf, ownerKind, step, ss.Attempts, ""), reason); err != nil {
			return fmt.Errorf("stop collaboration step %q: %w", step.Name, err)
		}
	}
	return nil
}
