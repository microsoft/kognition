// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

const celEvalTimeout = 100 * time.Millisecond

// celEnv is the shared CEL environment for workflow condition evaluation.
// It declares three variables available to expressions:
//   - steps: map<string, map<string, string>>  (step name → {"output": ..., "phase": ...})
//   - input: string
//   - params: map<string, string>
var celEnv *cel.Env

func init() {
	env, err := cel.NewEnv(
		cel.Variable("steps", cel.MapType(cel.StringType,
			cel.MapType(cel.StringType, cel.StringType))),
		cel.Variable("input", cel.StringType),
		cel.Variable("params", cel.MapType(cel.StringType, cel.StringType)),
	)
	if err != nil {
		panic(fmt.Sprintf("failed to create CEL environment: %v", err))
	}
	celEnv = env
}

// celResult represents the outcome of a CEL condition evaluation.
type celResult struct {
	// Match is true when the condition evaluated to true (or was empty).
	Match bool

	// Err is non-nil when the condition failed to compile or evaluate.
	Err error
}

// evaluateCELCondition compiles and evaluates a CEL expression against the
// workflow's current step statuses, input, and parameters.
// An empty condition is treated as unconditionally true.
// Evaluation is bounded by celEvalTimeout (100ms).
func evaluateCELCondition(ctx context.Context, condition string, stepStatuses []v1alpha1.WorkflowStepStatus, workflowInput string, params map[string]string) celResult {
	if condition == "" {
		return celResult{Match: true}
	}

	ast, issues := celEnv.Compile(condition)
	if issues != nil && issues.Err() != nil {
		return celResult{Err: fmt.Errorf("compile CEL condition: %w", issues.Err())}
	}

	prg, err := celEnv.Program(ast)
	if err != nil {
		return celResult{Err: fmt.Errorf("program CEL condition: %w", err)}
	}

	activation := buildCELActivation(stepStatuses, workflowInput, params)

	// Evaluate with timeout.
	evalCtx, cancel := context.WithTimeout(ctx, celEvalTimeout)
	defer cancel()

	resultCh := make(chan celResult, 1)
	go func() {
		out, _, evalErr := prg.Eval(activation)
		if evalErr != nil {
			resultCh <- celResult{Err: fmt.Errorf("evaluate CEL condition: %w", evalErr)}
			return
		}
		if out.Type() != types.BoolType {
			resultCh <- celResult{Err: fmt.Errorf("CEL condition returned %s, expected bool", out.Type())}
			return
		}
		match, ok := out.Value().(bool)
		if !ok {
			resultCh <- celResult{Err: fmt.Errorf("CEL condition returned an invalid bool value")}
			return
		}
		resultCh <- celResult{Match: match}
	}()

	select {
	case res := <-resultCh:
		return res
	case <-evalCtx.Done():
		return celResult{Err: fmt.Errorf("CEL evaluation timed out after %v", celEvalTimeout)}
	}
}

// buildCELActivation constructs the variable activation map for CEL evaluation.
// steps is a map<string, map<string, string>> where each step has "output" and "phase" keys.
func buildCELActivation(stepStatuses []v1alpha1.WorkflowStepStatus, workflowInput string, params map[string]string) map[string]any {
	steps := make(map[string]map[string]string, len(stepStatuses))
	for _, s := range stepStatuses {
		steps[s.Name] = map[string]string{
			"output": s.Output,
			"phase":  string(s.Phase),
		}
	}
	if params == nil {
		params = map[string]string{}
	}
	return map[string]any{
		"steps":  steps,
		"input":  workflowInput,
		"params": params,
	}
}

// evaluateEdgeConditions checks whether a target step is allowed to execute
// based on CEL conditions on its inbound edges. Returns true if the step has no
// inbound edges or at least one inbound edge condition evaluates to true.
// Returns false if all inbound edges from completed sources evaluate to false.
func evaluateEdgeConditions(ctx context.Context, edges []v1alpha1.WorkflowEdge, targetStep string, stepStatuses []v1alpha1.WorkflowStepStatus, workflowInput string, params map[string]string) (bool, error) {
	hasInbound := false
	for _, edge := range edges {
		if edge.To != targetStep {
			continue
		}
		hasInbound = true
		res := evaluateCELCondition(ctx, edge.Condition, stepStatuses, workflowInput, params)
		if res.Err != nil {
			return false, fmt.Errorf("edge %s→%s: %w", edge.From, edge.To, res.Err)
		}
		if res.Match {
			return true, nil
		}
	}
	// No inbound edges → entry point, always allowed.
	if !hasInbound {
		return true, nil
	}
	// All inbound edges evaluated to false.
	return false, nil
}

// evaluateStepCondition evaluates the CEL condition on a step itself.
// Returns true if the step should be dispatched, false if it should be skipped.
func evaluateStepCondition(ctx context.Context, step v1alpha1.WorkflowStep, stepStatuses []v1alpha1.WorkflowStepStatus, workflowInput string, params map[string]string) (bool, error) {
	if step.Condition == "" {
		return true, nil
	}
	res := evaluateCELCondition(ctx, step.Condition, stepStatuses, workflowInput, params)
	if res.Err != nil {
		return false, res.Err
	}
	return res.Match, nil
}
