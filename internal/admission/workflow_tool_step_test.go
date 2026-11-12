// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// TC-0262: a step with type=tool but no toolRef.name must be rejected.
func TestWorkflowValidator_RejectsToolStepMissingToolRef(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "fetch", Type: v1alpha1.WorkflowStepTypeTool},
	}
	wf.Spec.Edges = nil

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for tool step missing toolRef")
	}
	if !strings.Contains(resp.Reason, "toolRef.name is required") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

// TC-0263: a step with type=agent (or default) must not carry a toolRef.
func TestWorkflowValidator_RejectsAgentStepWithToolRef(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{
			Name:     "fetch",
			AgentRef: v1alpha1.LocalObjectReference{Name: "fetcher"},
			ToolRef:  v1alpha1.LocalObjectReference{Name: "http-get"},
		},
	}
	wf.Spec.Edges = nil

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for agent step carrying toolRef")
	}
	if !strings.Contains(resp.Reason, "toolRef must be empty") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

// TC-0264: an unknown step type value must be rejected.
func TestWorkflowValidator_RejectsInvalidStepType(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{
			Name:     "fetch",
			Type:     v1alpha1.WorkflowStepType("bogus"),
			AgentRef: v1alpha1.LocalObjectReference{Name: "fetcher"},
		},
	}
	wf.Spec.Edges = nil

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for invalid step type")
	}
	if !strings.Contains(resp.Reason, "is invalid") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

// TC-0266: type=tool with toolRef set and no retry/timeout is accepted.
func TestWorkflowValidator_AcceptsValidToolStep(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "fetch", Type: v1alpha1.WorkflowStepTypeTool, ToolRef: v1alpha1.LocalObjectReference{Name: "http-get"}},
	}
	wf.Spec.Edges = nil
	resp := v.Validate(context.Background(), workflowRequest(wf))
	if !resp.Allowed {
		t.Fatalf("expected valid tool step to be accepted, got: %s", resp.Reason)
	}
}

// TC-0267: an empty Type defaults to Agent semantics; AgentRef-only step is accepted.
func TestWorkflowValidator_AcceptsEmptyTypeAsAgent(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "fetch", Type: "", AgentRef: v1alpha1.LocalObjectReference{Name: "fetcher"}},
	}
	wf.Spec.Edges = nil
	resp := v.Validate(context.Background(), workflowRequest(wf))
	if !resp.Allowed {
		t.Fatalf("expected empty-type step with agentRef to be accepted, got: %s", resp.Reason)
	}
}

// TC-0268: retry is now wired for tool steps and must be accepted.
func TestWorkflowValidator_AcceptsRetryOnToolStep(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{
			Name:    "fetch",
			Type:    v1alpha1.WorkflowStepTypeTool,
			ToolRef: v1alpha1.LocalObjectReference{Name: "http-get"},
			Retry:   &v1alpha1.WorkflowStepRetry{MaxAttempts: 3, Backoff: "1s"},
		},
	}
	wf.Spec.Edges = nil
	resp := v.Validate(context.Background(), workflowRequest(wf))
	if !resp.Allowed {
		t.Fatalf("expected tool step with retry to be accepted, got: %s", resp.Reason)
	}
}

// TC-0269: timeout is now wired for tool steps and must be accepted.
func TestWorkflowValidator_AcceptsTimeoutOnToolStep(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{
			Name:    "fetch",
			Type:    v1alpha1.WorkflowStepTypeTool,
			ToolRef: v1alpha1.LocalObjectReference{Name: "http-get"},
			Timeout: "30s",
		},
	}
	wf.Spec.Edges = nil
	resp := v.Validate(context.Background(), workflowRequest(wf))
	if !resp.Allowed {
		t.Fatalf("expected tool step with timeout to be accepted, got: %s", resp.Reason)
	}
}

// TC-0270: changing a step's Type on UPDATE is rejected (name-keyed state would otherwise corrupt).
func TestWorkflowValidator_RejectsStepTypeChangeOnUpdate(t *testing.T) {
	v := NewWorkflowValidator()

	oldWF := validWorkflow()
	oldWF.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "s", Type: v1alpha1.WorkflowStepTypeAgent, AgentRef: v1alpha1.LocalObjectReference{Name: "a"}},
	}
	oldWF.Spec.Edges = nil
	oldBytes, _ := json.Marshal(oldWF)

	newWF := oldWF
	newWF.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "s", Type: v1alpha1.WorkflowStepTypeTool, ToolRef: v1alpha1.LocalObjectReference{Name: "t"}},
	}
	newBytes, _ := json.Marshal(newWF)

	req := &Request{
		Operation: OperationUpdate,
		Kind:      "Workflow",
		Namespace: newWF.Namespace,
		Name:      newWF.Name,
		Object:    newBytes,
		OldObject: oldBytes,
	}
	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected step type change on UPDATE to be rejected")
	}
	if !strings.Contains(resp.Reason, "is immutable") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}
