// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"text/template"
	"unicode/utf8"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
)

const (
	// maxOutputBytes is the maximum size (in bytes) for a step output.
	// Outputs exceeding this limit are truncated with a marker.
	maxOutputBytes = 64 * 1024 // 64 KiB

	// truncationMarker is appended to indicate output was truncated.
	truncationMarker = "\n... [output truncated at 64 KiB]"
)

// templateContext is the data object exposed to Go text/template expressions
// inside WorkflowStep.Input fields.
type templateContext struct {
	// Steps maps step name → step output data.
	Steps map[string]stepData `json:"steps"`

	// Input is the workflow-level input string.
	Input string `json:"input"`

	// Params are the workflow-level parameters.
	Params map[string]string `json:"params"`
}

// stepData carries the resolved output of a completed step.
type stepData struct {
	Output string `json:"output"`
	Phase  string `json:"phase"`
}

// buildTemplateContext constructs the template data from completed step statuses
// and the workflow-level input/parameters.
func buildTemplateContext(stepStatuses []v1alpha1.WorkflowStepStatus, workflowInput string, params map[string]string) templateContext {
	steps := make(map[string]stepData, len(stepStatuses))
	for _, s := range stepStatuses {
		steps[s.Name] = stepData{
			Output: s.Output,
			Phase:  string(s.Phase),
		}
	}
	if params == nil {
		params = map[string]string{}
	}
	return templateContext{
		Steps:  steps,
		Input:  workflowInput,
		Params: params,
	}
}

// resolveStepInput evaluates the step's Input field as a Go text/template
// against the provided context. If the input contains no template directives,
// it is returned as-is. Returns the resolved string or an error describing the
// template failure.
func resolveStepInput(rawInput string, ctx templateContext) (string, error) {
	// Fast path: no template delimiters means nothing to resolve.
	if !containsTemplateDirective(rawInput) {
		return rawInput, nil
	}

	tmpl, err := template.New("step-input").
		Option("missingkey=error").
		Parse(rawInput)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

// containsTemplateDirective checks whether s contains Go template delimiters.
func containsTemplateDirective(s string) bool {
	return bytes.Contains([]byte(s), []byte("{{"))
}

// truncateOutput enforces the 64 KiB size limit on step outputs.
// If the output exceeds maxOutputBytes, it is truncated and a marker is appended.
// The cut point is adjusted backward so that multi-byte UTF-8 runes are never split.
// The returned bool indicates whether truncation occurred.
func truncateOutput(output string) (string, bool) {
	if len(output) <= maxOutputBytes {
		return output, false
	}
	end := maxOutputBytes
	// If the byte at the cut point is a UTF-8 continuation byte, the cut
	// would split a multi-byte rune. Back up to the rune's start byte so
	// the result is always valid UTF-8.
	for end > 0 && !utf8.RuneStart(output[end]) {
		end--
	}
	return output[:end] + truncationMarker, true
}

// syncStepStatuses captures outputs from completed acyclic jobs into
// wf.Status.StepStatuses, applying truncation as needed.
func (r *WorkflowReconciler) syncStepStatuses(wf *v1alpha1.Workflow, jobsByStep map[string]v1alpha1.AgentJob) {
	statusMap := make(map[string]int, len(wf.Status.StepStatuses))
	for i, s := range wf.Status.StepStatuses {
		statusMap[s.Name] = i
	}
	for stepName, job := range jobsByStep {
		output, _ := truncateOutput(job.Status.Output)
		if idx, ok := statusMap[stepName]; ok {
			wf.Status.StepStatuses[idx].Phase = job.Status.Phase
			wf.Status.StepStatuses[idx].Output = output
		} else {
			wf.Status.StepStatuses = append(wf.Status.StepStatuses, v1alpha1.WorkflowStepStatus{
				Name:   stepName,
				Phase:  job.Status.Phase,
				Output: output,
			})
			statusMap[stepName] = len(wf.Status.StepStatuses) - 1
		}
	}
}

// syncCyclicStepStatuses captures outputs from completed cyclic jobs.
// For cyclic workflows, we track the latest iteration's output per step.
func (r *WorkflowReconciler) syncCyclicStepStatuses(wf *v1alpha1.Workflow, jobsByStepIteration map[string]map[int]v1alpha1.AgentJob) {
	statusMap := make(map[string]int, len(wf.Status.StepStatuses))
	for i, s := range wf.Status.StepStatuses {
		statusMap[s.Name] = i
	}
	for stepName, byIter := range jobsByStepIteration {
		// Find the highest iteration for this step.
		var latest v1alpha1.AgentJob
		latestIter := 0
		for iter, job := range byIter {
			if iter > latestIter {
				latestIter = iter
				latest = job
			}
		}
		if latestIter == 0 {
			continue
		}
		output, _ := truncateOutput(latest.Status.Output)
		if idx, ok := statusMap[stepName]; ok {
			wf.Status.StepStatuses[idx].Phase = latest.Status.Phase
			wf.Status.StepStatuses[idx].Output = output
		} else {
			wf.Status.StepStatuses = append(wf.Status.StepStatuses, v1alpha1.WorkflowStepStatus{
				Name:   stepName,
				Phase:  latest.Status.Phase,
				Output: output,
			})
			statusMap[stepName] = len(wf.Status.StepStatuses) - 1
		}
	}
}

// resolveStepInputFromWorkflow builds the template context from the workflow's
// current step statuses and resolves the step's Input template.
func (r *WorkflowReconciler) resolveStepInputFromWorkflow(wf *v1alpha1.Workflow, step v1alpha1.WorkflowStep) (string, error) {
	ctx := buildTemplateContext(wf.Status.StepStatuses, r.workflowInput, wf.Spec.Parameters)
	return resolveStepInput(step.Input, ctx)
}

// failWorkflowWithTemplateError sets the workflow to Failed with a
// TemplateResolutionFailed condition and emits an event.
func (r *WorkflowReconciler) failWorkflowWithTemplateError(ctx context.Context, wf *v1alpha1.Workflow, stepName string, tmplErr error) (Result, error) {
	msg := fmt.Sprintf("template resolution failed for step %q: %v", stepName, tmplErr)
	wf.Status.Phase = v1alpha1.PhaseFailed
	wf.Status.ActiveStep = stepName
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(wf.Status.Conditions),
		"TemplateResolutionFailed",
		lifecycle.ConditionTrue,
		"TemplateError",
		msg,
	)
	wf.Status.Conditions = lifecycleToAPIConditions(lc)
	if err := r.emitEvent(ctx, wf, "TemplateResolutionFailed", msg, map[string]string{"step": stepName, "error": tmplErr.Error()}); err != nil {
		return Result{}, fmt.Errorf("append workflow event: %w", err)
	}
	if err := r.updateWorkflow(ctx, wf); err != nil {
		return Result{}, fmt.Errorf("update workflow after template error: %w", err)
	}
	return Result{}, nil
}

// failWorkflowWithCELError sets the workflow to Failed with a
// CELConditionFailed condition and emits an event.
func (r *WorkflowReconciler) failWorkflowWithCELError(ctx context.Context, wf *v1alpha1.Workflow, stepName string, celErr error) (Result, error) {
	msg := fmt.Sprintf("CEL condition evaluation failed for step %q: %v", stepName, celErr)
	wf.Status.Phase = v1alpha1.PhaseFailed
	wf.Status.ActiveStep = stepName
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(wf.Status.Conditions),
		"CELConditionFailed",
		lifecycle.ConditionTrue,
		"CELError",
		msg,
	)
	wf.Status.Conditions = lifecycleToAPIConditions(lc)
	if err := r.emitEvent(ctx, wf, "CELConditionFailed", msg, map[string]string{"step": stepName, "error": celErr.Error()}); err != nil {
		return Result{}, fmt.Errorf("append workflow event: %w", err)
	}
	if err := r.updateWorkflow(ctx, wf); err != nil {
		return Result{}, fmt.Errorf("update workflow after CEL error: %w", err)
	}
	return Result{}, nil
}

// failWorkflowWithCommunicationDenied records a terminal failure before the
// target AgentJob is created, preventing policy-bypassing retries or dispatch.
func (r *WorkflowReconciler) failWorkflowWithCommunicationDenied(ctx context.Context, wf *v1alpha1.Workflow, sourceAgent, targetAgent, stepName string, iteration int) (Result, error) {
	msg := fmt.Sprintf("communication from agent %q to agent %q is denied", sourceAgent, targetAgent)
	if iteration > 0 {
		msg = fmt.Sprintf("%s in iteration %d", msg, iteration)
	}
	wf.Status.Phase = v1alpha1.PhaseFailed
	wf.Status.ActiveStep = stepName
	wf.Status.ActiveSteps = nil
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(wf.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		"CommunicationDenied",
		msg,
	)
	wf.Status.Conditions = lifecycleToAPIConditions(lc)
	details := map[string]string{"step": stepName, "sourceAgent": sourceAgent, "targetAgent": targetAgent}
	if iteration > 0 {
		details["iteration"] = strconv.Itoa(iteration)
	}
	if err := r.emitEvent(ctx, wf, "CommunicationDenied", msg, details); err != nil {
		return Result{}, fmt.Errorf("append communication denial event: %w", err)
	}
	if err := r.updateWorkflow(ctx, wf); err != nil {
		return Result{}, fmt.Errorf("update workflow after communication denial: %w", err)
	}
	return Result{}, nil
}

// skipStep records a step as Skipped in the workflow's StepStatuses.
func (r *WorkflowReconciler) skipStep(wf *v1alpha1.Workflow, stepName string) {
	ss := v1alpha1.WorkflowStepStatus{
		Name:  stepName,
		Phase: v1alpha1.PhaseSkipped,
	}
	for i, s := range wf.Status.StepStatuses {
		if s.Name == stepName {
			wf.Status.StepStatuses[i] = ss
			return
		}
	}
	wf.Status.StepStatuses = append(wf.Status.StepStatuses, ss)
}

// isStepSkipped returns true if the given step is marked as Skipped in StepStatuses.
func isStepSkipped(wf *v1alpha1.Workflow, stepName string) bool {
	for _, s := range wf.Status.StepStatuses {
		if s.Name == stepName && s.Phase == v1alpha1.PhaseSkipped {
			return true
		}
	}
	return false
}
