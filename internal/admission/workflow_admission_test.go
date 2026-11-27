// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"sigs.k8s.io/yaml"
)

func validWorkflow() v1alpha1.Workflow {
	return v1alpha1.Workflow{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "test-wf", Namespace: "default"},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "fetch", AgentRef: v1alpha1.LocalObjectReference{Name: "fetcher"}},
				{Name: "summarize", AgentRef: v1alpha1.LocalObjectReference{Name: "summarizer"}},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "fetch", To: "summarize"},
			},
		},
	}
}

func workflowRequest(wf v1alpha1.Workflow) *Request {
	data, _ := json.Marshal(wf)
	return &Request{Operation: OperationCreate, Kind: "Workflow", Namespace: wf.Namespace, Name: wf.Name, Object: data}
}

func TestWorkflowValidator_Handles(t *testing.T) {
	v := NewWorkflowValidator()
	if !v.Handles("Workflow", OperationCreate) {
		t.Error("expected to handle Workflow CREATE")
	}
	if !v.Handles("Workflow", OperationUpdate) {
		t.Error("expected to handle Workflow UPDATE")
	}
	if v.Handles("Agent", OperationCreate) {
		t.Error("should not handle Agent")
	}
}

func TestWorkflowValidator_AcceptsValid(t *testing.T) {
	v := NewWorkflowValidator()
	resp := v.Validate(context.Background(), workflowRequest(validWorkflow()))
	if !resp.Allowed {
		t.Fatalf("expected valid workflow to be accepted, got: %s", resp.Reason)
	}
}

func TestWorkflowRunProtectionCannotBecomeLegacyExecution(t *testing.T) {
	prior := v1alpha1.WorkflowRun{ObjectMeta: v1alpha1.ObjectMeta{Name: "accepted", Namespace: "team-a", UID: "run-uid", Annotations: map[string]string{runtimeapproval.WorkflowRunAcceptanceRequiredAnnotation: "true"}},
		Spec: v1alpha1.WorkflowRunSpec{WorkflowRef: v1alpha1.LocalObjectReference{Name: "approved"}}}
	oldJSON, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*v1alpha1.WorkflowRun){
		"remove execution mode":   func(run *v1alpha1.WorkflowRun) { run.Annotations = nil },
		"replace accepted spec":   func(run *v1alpha1.WorkflowRun) { run.Spec.WorkflowRef.Name = "other" },
		"replace run incarnation": func(run *v1alpha1.WorkflowRun) { run.UID = "recreated" },
	} {
		t.Run(name, func(t *testing.T) {
			current := prior
			mutate(&current)
			raw, err := json.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			response := NewWorkflowValidator().Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "WorkflowRun", Object: raw, OldObject: oldJSON})
			if response.Allowed {
				t.Fatal("protected run became mutable or executable by the legacy controller")
			}
		})
	}
	prior.Status.Phase = v1alpha1.PhasePending
	raw, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	if response := NewWorkflowValidator().Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "WorkflowRun", Object: raw, OldObject: oldJSON}); !response.Allowed {
		t.Fatalf("safe status update denied: %s", response.Reason)
	}
}

func TestWorkflowValidator_ParallelReviewDraft(t *testing.T) {
	data, err := os.ReadFile("../../docs/development-plans/features/workflow-collaboration/parallel-review.draft.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow v1alpha1.Workflow
	if err := yaml.UnmarshalStrict(data, &workflow); err != nil {
		t.Fatalf("draft does not match the Workflow schema: %v", err)
	}
	request := workflowRequest(workflow)
	if response := NewWorkflowValidator().Validate(context.Background(), request); response.Allowed || !strings.Contains(response.Reason, "collaboration execution has not been qualified") {
		t.Fatalf("default admission must reject the collaboration draft, got: %+v", response)
	}
	if response := NewWorkflowValidator(WithCollaborationEnabled(true)).Validate(context.Background(), request); !response.Allowed {
		t.Fatalf("draft must satisfy structural admission: %s", response.Reason)
	}
}

func TestWorkflowValidator_RejectsNoSteps(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps = nil
	wf.Spec.Edges = nil

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for empty steps")
	}
	if !strings.Contains(resp.Reason, "at least one step") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

func TestWorkflowValidator_RejectsEmptyStepName(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps = append(wf.Spec.Steps, v1alpha1.WorkflowStep{
		Name:     "",
		AgentRef: v1alpha1.LocalObjectReference{Name: "agent"},
	})

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for empty step name")
	}
	if !strings.Contains(resp.Reason, "name is required") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

func TestWorkflowValidator_RejectsDuplicateStepNames(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "fetch", AgentRef: v1alpha1.LocalObjectReference{Name: "a1"}},
		{Name: "fetch", AgentRef: v1alpha1.LocalObjectReference{Name: "a2"}},
	}
	wf.Spec.Edges = nil

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for duplicate step names")
	}
	if !strings.Contains(resp.Reason, "duplicated") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

func TestWorkflowValidator_AcceptsCamelCaseStepName(t *testing.T) {
	// Sanitization allows camelCase step names ("workerEn" -> "workeren")
	// as long as the sanitized k8s name is unique and within length limits.
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "workerEn", AgentRef: v1alpha1.LocalObjectReference{Name: "a"}},
		{Name: "workerEs", AgentRef: v1alpha1.LocalObjectReference{Name: "a"}},
	}
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "workerEn", To: "workerEs"}}

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if !resp.Allowed {
		t.Fatalf("expected camelCase step names to be accepted, got: %s", resp.Reason)
	}
}

func TestWorkflowValidator_RejectsSanitizedStepNameCollision(t *testing.T) {
	// "workerEn" and "worker_en" both sanitize to "worker-en"; the two
	// AgentJob objects would collide in the cluster.
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "worker_en", AgentRef: v1alpha1.LocalObjectReference{Name: "a"}},
		{Name: "worker-en", AgentRef: v1alpha1.LocalObjectReference{Name: "a"}},
	}
	wf.Spec.Edges = nil

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for step name collision after sanitization")
	}
	if !strings.Contains(resp.Reason, "collides") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

func TestWorkflowValidator_RejectsStepNameWithNoSafeChars(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "___", AgentRef: v1alpha1.LocalObjectReference{Name: "a"}},
	}
	wf.Spec.Edges = nil

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for step name with no DNS1123-safe characters")
	}
	if !strings.Contains(resp.Reason, "no DNS1123-safe characters") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

func TestWorkflowValidator_RejectsOverlongComposedName(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.ObjectMeta.Name = strings.Repeat("a", 200)
	longStep := strings.Repeat("b", 60)
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: longStep, AgentRef: v1alpha1.LocalObjectReference{Name: "a"}},
	}
	wf.Spec.Edges = nil

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for overlong composed object name")
	}
	if !strings.Contains(resp.Reason, "exceed the 253-character") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

func TestWorkflowValidator_RejectsMissingAgentRef(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps[0].AgentRef.Name = ""

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for missing agentRef")
	}
	if !strings.Contains(resp.Reason, "agentRef.name is required") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

func TestWorkflowValidator_RejectsEdgeUnknownFrom(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Edges = append(wf.Spec.Edges, v1alpha1.WorkflowEdge{From: "ghost", To: "summarize"})

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for edge referencing unknown step")
	}
	if !strings.Contains(resp.Reason, "unknown step") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

func TestWorkflowValidator_RejectsEdgeUnknownTo(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Edges = append(wf.Spec.Edges, v1alpha1.WorkflowEdge{From: "fetch", To: "ghost"})

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for edge referencing unknown step")
	}
	if !strings.Contains(resp.Reason, "unknown step") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

func TestWorkflowValidator_RejectsSelfLoop(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Edges = append(wf.Spec.Edges, v1alpha1.WorkflowEdge{From: "fetch", To: "fetch"})

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for self-loop")
	}
	if !strings.Contains(resp.Reason, "self-loop") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

func TestWorkflowValidator_RejectsUnknownEntryPoint(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.EntryPoint = "nonexistent"

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for unknown entryPoint")
	}
	if !strings.Contains(resp.Reason, "unknown step") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

func TestWorkflowValidator_RejectsCyclicWithoutEntryPoint(t *testing.T) {
	v := NewWorkflowValidator()
	wf := v1alpha1.Workflow{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "cyclic-wf", Namespace: "default"},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "a", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}},
				{Name: "b", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-b"}},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "a", To: "b"},
				{From: "b", To: "a"},
			},
		},
	}

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for cyclic workflow without entryPoint")
	}
	if !strings.Contains(resp.Reason, "entryPoint is required") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

func TestWorkflowValidator_AcceptsCyclicWithEntryPoint(t *testing.T) {
	v := NewWorkflowValidator()
	wf := v1alpha1.Workflow{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "cyclic-wf", Namespace: "default"},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "a", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}},
				{Name: "b", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-b"}},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "a", To: "b"},
				{From: "b", To: "a"},
			},
			EntryPoint: "a",
		},
	}

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if !resp.Allowed {
		t.Fatalf("expected cyclic workflow with entryPoint to be accepted, got: %s", resp.Reason)
	}
}

func TestWorkflowValidator_RejectsApprovalGateUnknownStep(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{
		{From: "fetch", To: "ghost"},
	}

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for approval gate referencing unknown step")
	}
	if !strings.Contains(resp.Reason, "unknown step") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

func TestWorkflowValidator_AcceptsSingleStepNoEdges(t *testing.T) {
	v := NewWorkflowValidator()
	wf := v1alpha1.Workflow{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "single", Namespace: "default"},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "only", AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}},
			},
		},
	}

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if !resp.Allowed {
		t.Fatalf("expected single-step workflow to be accepted, got: %s", resp.Reason)
	}
}

// TC-0250: Reject Retry.MaxAttempts < 0.
func TestWorkflowValidator_RejectsNegativeRetryMaxAttempts(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps[0].Retry = &v1alpha1.WorkflowStepRetry{MaxAttempts: -1}

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for negative retry.maxAttempts")
	}
	if !strings.Contains(resp.Reason, "retry.maxAttempts must be >= 0") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

// TC-0251: Reject Retry.Backoff that is not a valid Go duration.
func TestWorkflowValidator_RejectsInvalidRetryBackoff(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps[0].Retry = &v1alpha1.WorkflowStepRetry{MaxAttempts: 3, Backoff: "not-a-duration"}

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for invalid retry.backoff")
	}
	if !strings.Contains(resp.Reason, "retry.backoff") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

// TC-0252: Reject Retry.MaxBackoff that is not a valid Go duration.
func TestWorkflowValidator_RejectsInvalidRetryMaxBackoff(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps[0].Retry = &v1alpha1.WorkflowStepRetry{MaxAttempts: 3, MaxBackoff: "5"}

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for invalid retry.maxBackoff")
	}
	if !strings.Contains(resp.Reason, "retry.maxBackoff") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

// TC-0253: Reject Retry.BackoffMultiplier < 0.
func TestWorkflowValidator_RejectsNegativeBackoffMultiplier(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps[0].Retry = &v1alpha1.WorkflowStepRetry{MaxAttempts: 3, BackoffMultiplier: -1.5}

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for negative retry.backoffMultiplier")
	}
	if !strings.Contains(resp.Reason, "retry.backoffMultiplier must be >= 0") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

// TC-0254: Reject step.Timeout that is not a valid Go duration.
func TestWorkflowValidator_RejectsInvalidStepTimeout(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps[0].Timeout = "10minutes"

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection for invalid step.timeout")
	}
	if !strings.Contains(resp.Reason, "timeout") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}

// TC-0255: Accept workflow with valid retry policy and timeout on a step.
func TestWorkflowValidator_AcceptsValidRetryAndTimeout(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps[0].Timeout = "30s"
	wf.Spec.Steps[0].Retry = &v1alpha1.WorkflowStepRetry{
		MaxAttempts:       3,
		Backoff:           "1s",
		MaxBackoff:        "1m",
		BackoffMultiplier: 2.0,
	}

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if !resp.Allowed {
		t.Fatalf("expected valid retry/timeout to be accepted, got: %s", resp.Reason)
	}
}

// TC-0261: Reject retry.maxBackoff that is smaller than retry.backoff.
func TestWorkflowValidator_RejectsMaxBackoffLessThanBackoff(t *testing.T) {
	v := NewWorkflowValidator()
	wf := validWorkflow()
	wf.Spec.Steps[0].Retry = &v1alpha1.WorkflowStepRetry{
		MaxAttempts: 3,
		Backoff:     "1m",
		MaxBackoff:  "5s",
	}

	resp := v.Validate(context.Background(), workflowRequest(wf))
	if resp.Allowed {
		t.Fatal("expected rejection when retry.maxBackoff < retry.backoff")
	}
	if !strings.Contains(resp.Reason, "maxBackoff") || !strings.Contains(resp.Reason, ">= retry.backoff") {
		t.Fatalf("unexpected reason: %s", resp.Reason)
	}
}
