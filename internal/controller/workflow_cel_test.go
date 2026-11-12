// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestEvaluateCELCondition_EmptyIsTrue(t *testing.T) {
	res := evaluateCELCondition(context.Background(), "", nil, "", nil)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if !res.Match {
		t.Fatal("expected empty condition to match")
	}
}

func TestEvaluateCELCondition_BoolLiteral(t *testing.T) {
	res := evaluateCELCondition(context.Background(), "true", nil, "", nil)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if !res.Match {
		t.Fatal("expected true literal to match")
	}

	res = evaluateCELCondition(context.Background(), "false", nil, "", nil)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if res.Match {
		t.Fatal("expected false literal to not match")
	}
}

func TestEvaluateCELCondition_StepOutput(t *testing.T) {
	statuses := []v1alpha1.WorkflowStepStatus{
		{Name: "A", Phase: v1alpha1.PhaseReady, Output: "yes-do-it"},
	}
	res := evaluateCELCondition(context.Background(), `steps.A.output.contains("yes")`, statuses, "", nil)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if !res.Match {
		t.Fatal("expected step A output containing 'yes' to match")
	}

	res = evaluateCELCondition(context.Background(), `steps.A.output.contains("no")`, statuses, "", nil)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if res.Match {
		t.Fatal("expected step A output not containing 'no' to not match")
	}
}

func TestEvaluateCELCondition_StepPhase(t *testing.T) {
	statuses := []v1alpha1.WorkflowStepStatus{
		{Name: "A", Phase: v1alpha1.PhaseFailed, Output: "err"},
	}
	res := evaluateCELCondition(context.Background(), `steps.A.phase == "Ready"`, statuses, "", nil)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if res.Match {
		t.Fatal("expected Failed phase != Ready to not match")
	}

	res = evaluateCELCondition(context.Background(), `steps.A.phase == "Failed"`, statuses, "", nil)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if !res.Match {
		t.Fatal("expected Failed phase == Failed to match")
	}
}

func TestEvaluateCELCondition_Params(t *testing.T) {
	params := map[string]string{"env": "prod"}
	res := evaluateCELCondition(context.Background(), `params.env == "prod"`, nil, "", params)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if !res.Match {
		t.Fatal("expected params.env == prod to match")
	}
}

func TestEvaluateCELCondition_Input(t *testing.T) {
	res := evaluateCELCondition(context.Background(), `input.contains("hello")`, nil, "hello world", nil)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if !res.Match {
		t.Fatal("expected input containing 'hello' to match")
	}
}

func TestEvaluateCELCondition_CompileError(t *testing.T) {
	res := evaluateCELCondition(context.Background(), `this is not valid CEL!!!`, nil, "", nil)
	if res.Err == nil {
		t.Fatal("expected compile error")
	}
}

func TestEvaluateCELCondition_NonBoolResult(t *testing.T) {
	res := evaluateCELCondition(context.Background(), `"a string"`, nil, "", nil)
	if res.Err == nil {
		t.Fatal("expected error for non-bool result")
	}
}

func TestEvaluateEdgeConditions_NoInboundEdges(t *testing.T) {
	edges := []v1alpha1.WorkflowEdge{{From: "A", To: "B"}}
	allowed, err := evaluateEdgeConditions(context.Background(), edges, "A", nil, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatal("step with no inbound edges should be allowed")
	}
}

func TestEvaluateEdgeConditions_EmptyConditionAllows(t *testing.T) {
	edges := []v1alpha1.WorkflowEdge{{From: "A", To: "B", Condition: ""}}
	allowed, err := evaluateEdgeConditions(context.Background(), edges, "B", nil, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatal("empty condition should allow")
	}
}

func TestEvaluateEdgeConditions_TrueConditionAllows(t *testing.T) {
	statuses := []v1alpha1.WorkflowStepStatus{
		{Name: "A", Phase: v1alpha1.PhaseReady, Output: "yes"},
	}
	edges := []v1alpha1.WorkflowEdge{
		{From: "A", To: "B", Condition: `steps.A.output.contains("yes")`},
	}
	allowed, err := evaluateEdgeConditions(context.Background(), edges, "B", statuses, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatal("true condition should allow")
	}
}

func TestEvaluateEdgeConditions_FalseConditionBlocks(t *testing.T) {
	statuses := []v1alpha1.WorkflowStepStatus{
		{Name: "A", Phase: v1alpha1.PhaseReady, Output: "no"},
	}
	edges := []v1alpha1.WorkflowEdge{
		{From: "A", To: "B", Condition: `steps.A.output.contains("yes")`},
	}
	allowed, err := evaluateEdgeConditions(context.Background(), edges, "B", statuses, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Fatal("false condition should block")
	}
}

func TestEvaluateEdgeConditions_AnyTrueAllows(t *testing.T) {
	statuses := []v1alpha1.WorkflowStepStatus{
		{Name: "X", Phase: v1alpha1.PhaseReady, Output: "nope"},
		{Name: "Y", Phase: v1alpha1.PhaseReady, Output: "yes"},
	}
	edges := []v1alpha1.WorkflowEdge{
		{From: "X", To: "C", Condition: `steps.X.output.contains("yes")`},
		{From: "Y", To: "C", Condition: `steps.Y.output.contains("yes")`},
	}
	allowed, err := evaluateEdgeConditions(context.Background(), edges, "C", statuses, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatal("at least one true edge should allow")
	}
}

func TestEvaluateEdgeConditions_CELError(t *testing.T) {
	edges := []v1alpha1.WorkflowEdge{
		{From: "A", To: "B", Condition: `not valid CEL!!!`},
	}
	_, err := evaluateEdgeConditions(context.Background(), edges, "B", nil, "", nil)
	if err == nil {
		t.Fatal("expected error for invalid CEL in edge")
	}
}

func TestEvaluateStepCondition_EmptyIsTrue(t *testing.T) {
	step := v1alpha1.WorkflowStep{Name: "A"}
	allowed, err := evaluateStepCondition(context.Background(), step, nil, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Fatal("empty condition should allow")
	}
}

func TestEvaluateStepCondition_FalseBlocks(t *testing.T) {
	statuses := []v1alpha1.WorkflowStepStatus{
		{Name: "A", Phase: v1alpha1.PhaseFailed},
	}
	step := v1alpha1.WorkflowStep{Name: "B", Condition: `steps.A.phase == "Ready"`}
	allowed, err := evaluateStepCondition(context.Background(), step, statuses, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Fatal("condition should block when step A is not Ready")
	}
}

func TestBuildCELActivation(t *testing.T) {
	statuses := []v1alpha1.WorkflowStepStatus{
		{Name: "A", Phase: v1alpha1.PhaseReady, Output: "hello"},
	}
	act := buildCELActivation(statuses, "some-input", map[string]string{"k": "v"})

	steps := act["steps"].(map[string]map[string]string)
	if steps["A"]["output"] != "hello" {
		t.Fatalf("expected steps.A.output = hello, got %q", steps["A"]["output"])
	}
	if steps["A"]["phase"] != "Ready" {
		t.Fatalf("expected steps.A.phase = Ready, got %q", steps["A"]["phase"])
	}
	if act["input"] != "some-input" {
		t.Fatalf("expected input = some-input, got %q", act["input"])
	}
	params := act["params"].(map[string]string)
	if params["k"] != "v" {
		t.Fatalf("expected params.k = v, got %q", params["k"])
	}
}

func TestBuildCELActivation_NilParams(t *testing.T) {
	act := buildCELActivation(nil, "", nil)
	params := act["params"].(map[string]string)
	if len(params) != 0 {
		t.Fatalf("expected empty params, got %v", params)
	}
}
