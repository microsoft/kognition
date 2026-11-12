// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// WorkflowValidator validates Workflow resources on CREATE and UPDATE.
type WorkflowValidator struct{}

// NewWorkflowValidator creates a new WorkflowValidator.
func NewWorkflowValidator() *WorkflowValidator {
	return &WorkflowValidator{}
}

func (v *WorkflowValidator) Name() string { return "workflow-validator" }

func (v *WorkflowValidator) Handles(kind string, op Operation) bool {
	return kind == "Workflow" && (op == OperationCreate || op == OperationUpdate)
}

func (v *WorkflowValidator) Validate(_ context.Context, req *Request) *Response {
	var wf v1alpha1.Workflow
	if err := json.Unmarshal(req.Object, &wf); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid workflow JSON: %v", err)}
	}

	var errs []string

	// At least one step must exist.
	if len(wf.Spec.Steps) == 0 {
		errs = append(errs, "spec.steps must contain at least one step")
	}

	// Build step name set; validate step fields.
	stepNames := make(map[string]struct{}, len(wf.Spec.Steps))
	// sanitizedStepNames tracks the DNS1123-safe segment used when composing
	// AgentJob / MemoryClaim metadata.name. Two step names that differ only
	// by case or non-alnum characters (e.g. "workerEn" vs "worker_en") would
	// collapse to the same object name and collide in the cluster; reject
	// them at admission time.
	sanitizedStepNames := make(map[string]string, len(wf.Spec.Steps))
	wfNameLen := len(wf.ObjectMeta.Name)
	for i, step := range wf.Spec.Steps {
		if step.Name == "" {
			errs = append(errs, fmt.Sprintf("spec.steps[%d].name is required", i))
			continue
		}
		if _, dup := stepNames[step.Name]; dup {
			errs = append(errs, fmt.Sprintf("spec.steps[%d].name %q is duplicated", i, step.Name))
		}
		stepNames[step.Name] = struct{}{}

		// Guard against illegal / colliding sanitized k8s object names.
		sanitized := sanitizeStepNameForK8s(step.Name)
		if sanitized == "" {
			errs = append(errs, fmt.Sprintf("spec.steps[%d(%s)].name has no DNS1123-safe characters", i, step.Name))
		} else {
			if prev, exists := sanitizedStepNames[sanitized]; exists && prev != step.Name {
				errs = append(errs, fmt.Sprintf("spec.steps[%d(%s)].name collides with step %q after DNS1123 sanitization (both map to %q)", i, step.Name, prev, sanitized))
			}
			sanitizedStepNames[sanitized] = step.Name
			// The composed AgentJob name is "<workflow>-<sanitized-step>",
			// and iteration variants add "-i<n>" (up to ~8 chars for -i9999999).
			// The MemoryClaim variant adds "-memory" (7 chars). Reserve 12
			// characters of headroom so all downstream object names stay
			// within the 253-char DNS1123-subdomain limit.
			const suffixReserve = 12
			if wfNameLen+1+len(sanitized)+suffixReserve > 253 {
				errs = append(errs, fmt.Sprintf("spec.steps[%d(%s)].name is too long: composed object name %q-%q would exceed the 253-character DNS1123 subdomain limit", i, step.Name, wf.ObjectMeta.Name, sanitized))
			}
		}

		// Validate type discriminator and the corresponding required refs.
		switch step.Type {
		case "", v1alpha1.WorkflowStepTypeAgent:
			if step.AgentRef.Name == "" {
				errs = append(errs, fmt.Sprintf("spec.steps[%d(%s)].agentRef.name is required when type=%q", i, step.Name, v1alpha1.WorkflowStepTypeAgent))
			}
			if step.ToolRef.Name != "" {
				errs = append(errs, fmt.Sprintf("spec.steps[%d(%s)].toolRef must be empty when type=%q", i, step.Name, v1alpha1.WorkflowStepTypeAgent))
			}
		case v1alpha1.WorkflowStepTypeTool:
			if step.ToolRef.Name == "" {
				errs = append(errs, fmt.Sprintf("spec.steps[%d(%s)].toolRef.name is required when type=%q", i, step.Name, v1alpha1.WorkflowStepTypeTool))
			}
		default:
			errs = append(errs, fmt.Sprintf("spec.steps[%d(%s)].type %q is invalid (must be %q or %q)", i, step.Name, step.Type, v1alpha1.WorkflowStepTypeAgent, v1alpha1.WorkflowStepTypeTool))
		}

		// Validate per-step Timeout (must be a valid Go duration).
		if step.Timeout != "" {
			if _, err := time.ParseDuration(step.Timeout); err != nil {
				errs = append(errs, fmt.Sprintf("spec.steps[%d(%s)].timeout %q is not a valid duration: %v", i, step.Name, step.Timeout, err))
			}
		}

		// Validate per-step Retry policy.
		if step.Retry != nil {
			if step.Retry.MaxAttempts < 0 {
				errs = append(errs, fmt.Sprintf("spec.steps[%d(%s)].retry.maxAttempts must be >= 0", i, step.Name))
			}
			if step.Retry.BackoffMultiplier < 0 {
				errs = append(errs, fmt.Sprintf("spec.steps[%d(%s)].retry.backoffMultiplier must be >= 0", i, step.Name))
			}
			var backoffDur, maxBackoffDur time.Duration
			var backoffOK, maxBackoffOK bool
			if step.Retry.Backoff != "" {
				d, err := time.ParseDuration(step.Retry.Backoff)
				if err != nil {
					errs = append(errs, fmt.Sprintf("spec.steps[%d(%s)].retry.backoff %q is not a valid duration: %v", i, step.Name, step.Retry.Backoff, err))
				} else {
					backoffDur, backoffOK = d, true
				}
			}
			if step.Retry.MaxBackoff != "" {
				d, err := time.ParseDuration(step.Retry.MaxBackoff)
				if err != nil {
					errs = append(errs, fmt.Sprintf("spec.steps[%d(%s)].retry.maxBackoff %q is not a valid duration: %v", i, step.Name, step.Retry.MaxBackoff, err))
				} else {
					maxBackoffDur, maxBackoffOK = d, true
				}
			}
			if backoffOK && maxBackoffOK && maxBackoffDur < backoffDur {
				errs = append(errs, fmt.Sprintf("spec.steps[%d(%s)].retry.maxBackoff %q must be >= retry.backoff %q", i, step.Name, step.Retry.MaxBackoff, step.Retry.Backoff))
			}
		}
	}

	// Validate edges.
	for i, edge := range wf.Spec.Edges {
		if edge.From == "" {
			errs = append(errs, fmt.Sprintf("spec.edges[%d].from is required", i))
		} else if _, ok := stepNames[edge.From]; !ok {
			errs = append(errs, fmt.Sprintf("spec.edges[%d].from references unknown step %q", i, edge.From))
		}
		if edge.To == "" {
			errs = append(errs, fmt.Sprintf("spec.edges[%d].to is required", i))
		} else if _, ok := stepNames[edge.To]; !ok {
			errs = append(errs, fmt.Sprintf("spec.edges[%d].to references unknown step %q", i, edge.To))
		}
		if edge.From != "" && edge.From == edge.To {
			errs = append(errs, fmt.Sprintf("spec.edges[%d] is a self-loop (%q)", i, edge.From))
		}
	}

	// Validate approval gates.
	for i, gate := range wf.Spec.ApprovalGates {
		if gate.From == "" {
			errs = append(errs, fmt.Sprintf("spec.approvalGates[%d].from is required", i))
		} else if _, ok := stepNames[gate.From]; !ok {
			errs = append(errs, fmt.Sprintf("spec.approvalGates[%d].from references unknown step %q", i, gate.From))
		}
		if gate.To == "" {
			errs = append(errs, fmt.Sprintf("spec.approvalGates[%d].to is required", i))
		} else if _, ok := stepNames[gate.To]; !ok {
			errs = append(errs, fmt.Sprintf("spec.approvalGates[%d].to references unknown step %q", i, gate.To))
		}
	}

	// Validate entryPoint if specified.
	if wf.Spec.EntryPoint != "" {
		if _, ok := stepNames[wf.Spec.EntryPoint]; !ok {
			errs = append(errs, fmt.Sprintf("spec.entryPoint references unknown step %q", wf.Spec.EntryPoint))
		}
	}

	// On UPDATE, reject changing the Type of an existing step: state (StepStatus,
	// AgentJobs, RuntimeMemory) is keyed by step name and assumes a stable kind.
	if req.Operation == OperationUpdate && len(req.OldObject) > 0 {
		var oldWF v1alpha1.Workflow
		if err := json.Unmarshal(req.OldObject, &oldWF); err == nil {
			oldTypes := make(map[string]v1alpha1.WorkflowStepType, len(oldWF.Spec.Steps))
			for _, s := range oldWF.Spec.Steps {
				oldTypes[s.Name] = normalizeStepType(s.Type)
			}
			for i, s := range wf.Spec.Steps {
				if prev, ok := oldTypes[s.Name]; ok {
					if normalizeStepType(s.Type) != prev {
						errs = append(errs, fmt.Sprintf("spec.steps[%d(%s)].type is immutable: was %q, now %q", i, s.Name, prev, normalizeStepType(s.Type)))
					}
				}
			}
		}
	}

	// Graph topology: detect cycles and enforce entryPoint requirement.
	if len(wf.Spec.Steps) > 0 && len(errs) == 0 {
		if hasCycle(wf.Spec.Steps, wf.Spec.Edges) {
			if wf.Spec.EntryPoint == "" {
				errs = append(errs, "spec.entryPoint is required for cyclic workflows")
			}
		} else {
			// DAG: ensure at least one step has in-degree 0.
			inDegree := make(map[string]int, len(wf.Spec.Steps))
			for _, s := range wf.Spec.Steps {
				inDegree[s.Name] = 0
			}
			for _, e := range wf.Spec.Edges {
				inDegree[e.To]++
			}
			hasRoot := false
			for _, d := range inDegree {
				if d == 0 {
					hasRoot = true
					break
				}
			}
			if !hasRoot {
				errs = append(errs, "workflow DAG has no entry point (all steps have inbound edges)")
			}
		}
	}

	if len(errs) > 0 {
		return &Response{Allowed: false, Reason: strings.Join(errs, "; ")}
	}

	return &Response{Allowed: true}
}

// normalizeStepType returns the canonical step type, treating an empty type as agent.
func normalizeStepType(t v1alpha1.WorkflowStepType) v1alpha1.WorkflowStepType {
	if t == "" {
		return v1alpha1.WorkflowStepTypeAgent
	}
	return t
}

// sanitizeStepNameForK8s mirrors controller.sanitizeK8sName: lowercases the
// input, replaces any character outside [a-z0-9-] with '-', collapses runs of
// '-', and trims leading/trailing '-'. Kept in sync with the controller helper
// so admission-time collision detection matches runtime object-name composition.
func sanitizeStepNameForK8s(name string) string {
	if name == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(name))
	lastDash := true
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// hasCycle uses DFS to detect cycles in the workflow graph.
func hasCycle(steps []v1alpha1.WorkflowStep, edges []v1alpha1.WorkflowEdge) bool {
	adj := make(map[string][]string, len(steps))
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
	}

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(steps))

	var dfs func(string) bool
	dfs = func(node string) bool {
		color[node] = gray
		for _, next := range adj[node] {
			switch color[next] {
			case gray:
				return true
			case white:
				if dfs(next) {
					return true
				}
			}
		}
		color[node] = black
		return false
	}

	for _, s := range steps {
		if color[s.Name] == white {
			if dfs(s.Name) {
				return true
			}
		}
	}
	return false
}
