// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// SolutionValidator validates Solution resources on CREATE and UPDATE.
type SolutionValidator struct{}

// NewSolutionValidator creates a new SolutionValidator.
func NewSolutionValidator() *SolutionValidator {
	return &SolutionValidator{}
}

func (v *SolutionValidator) Name() string { return "solution-validator" }

func (v *SolutionValidator) Handles(kind string, op Operation) bool {
	return kind == "Solution" && (op == OperationCreate || op == OperationUpdate)
}

func (v *SolutionValidator) Validate(_ context.Context, req *Request) *Response {
	var sol v1alpha1.Solution
	if err := json.Unmarshal(req.Object, &sol); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid solution JSON: %v", err)}
	}

	var errs []string

	// Goal is required.
	if strings.TrimSpace(sol.Spec.Goal) == "" {
		errs = append(errs, "spec.goal is required")
	}
	if sol.Spec.Target != nil {
		if sol.Spec.Target.Kind != "Agent" && sol.Spec.Target.Kind != "AgentService" && sol.Spec.Target.Kind != "Workflow" {
			errs = append(errs, "spec.target.kind must be Agent, AgentService, or Workflow")
		}
		if strings.TrimSpace(sol.Spec.Target.Ref.Name) == "" {
			errs = append(errs, "spec.target.ref.name is required")
		}
	}
	seenPromotionGates := make(map[string]struct{}, len(sol.Spec.PromotionGates))
	if len(sol.Spec.PromotionGates) > 0 && sol.Spec.Target == nil {
		errs = append(errs, "spec.target is required when spec.promotionGates is configured")
	}
	if len(sol.Spec.PromotionGates) > 0 && sol.Spec.Target != nil && sol.Spec.Target.Kind == "Workflow" {
		errs = append(errs, "spec.promotionGates currently support Agent and AgentService targets")
	}
	for i, gate := range sol.Spec.PromotionGates {
		name := strings.TrimSpace(gate.EvaluationRef.Name)
		if name == "" {
			errs = append(errs, fmt.Sprintf("spec.promotionGates[%d].evaluationRef.name is required", i))
			continue
		}
		if name != gate.EvaluationRef.Name {
			errs = append(errs, fmt.Sprintf("spec.promotionGates[%d].evaluationRef.name must not contain leading or trailing whitespace", i))
		}
		if _, duplicate := seenPromotionGates[name]; duplicate {
			errs = append(errs, fmt.Sprintf("spec.promotionGates[%d] duplicates Evaluation %q", i, name))
		}
		seenPromotionGates[name] = struct{}{}
	}

	// Constraints must be non-empty strings if specified.
	for i, c := range sol.Spec.Constraints {
		if strings.TrimSpace(c) == "" {
			errs = append(errs, fmt.Sprintf("spec.constraints[%d] must be non-empty", i))
		}
	}

	// Seed must be non-negative if specified.
	if sol.Spec.Seed != nil && *sol.Spec.Seed < 0 {
		errs = append(errs, "spec.seed must be >= 0")
	}

	// Overrides validation.
	if sol.Spec.Overrides != nil {
		if tb := sol.Spec.Overrides.TokenBudget; tb != nil {
			if tb.MaxInputTokens < 0 {
				errs = append(errs, "spec.overrides.tokenBudget.maxInputTokens must be >= 0")
			}
			if tb.MaxOutputTokens < 0 {
				errs = append(errs, "spec.overrides.tokenBudget.maxOutputTokens must be >= 0")
			}
			if tb.MaxTotalTokens < 0 {
				errs = append(errs, "spec.overrides.tokenBudget.maxTotalTokens must be >= 0")
			}
		}
		for k, val := range sol.Spec.Overrides.Hard {
			if val < 0 {
				errs = append(errs, fmt.Sprintf("spec.overrides.hard[%q] must be >= 0", k))
			}
		}
	}

	if len(errs) > 0 {
		return &Response{Allowed: false, Reason: strings.Join(errs, "; ")}
	}

	return &Response{Allowed: true}
}
