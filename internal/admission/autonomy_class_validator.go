// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// AutonomyClassValidator is a ValidatingHook that enforces AutonomyClass constraints
// on agents at admission time.
type AutonomyClassValidator struct {
	store store.Store
}

// NewAutonomyClassValidator creates a new AutonomyClassValidator.
func NewAutonomyClassValidator(s store.Store) *AutonomyClassValidator {
	return &AutonomyClassValidator{store: s}
}

func (v *AutonomyClassValidator) Name() string { return "autonomy-class-validator" }

func (v *AutonomyClassValidator) Handles(kind string, op Operation) bool {
	return kind == "Agent" && (op == OperationCreate || op == OperationUpdate)
}

func (v *AutonomyClassValidator) Validate(ctx context.Context, req *Request) *Response {
	// Parse the agent to find its autonomy class reference.
	// AutonomyClassRef is *LocalObjectReference: {"name":"..."} in JSON.
	// Tools is []AgentToolRef: [{"ref":{"name":"..."},"required":bool}] in JSON.
	var agent struct {
		Spec struct {
			AutonomyClassRef *struct {
				Name string `json:"name"`
			} `json:"autonomyClassRef"`
			Tools []struct {
				Ref struct {
					Name string `json:"name"`
				} `json:"ref"`
			} `json:"tools"`
			MaxCostPerAction int64 `json:"maxCostPerAction"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(req.Object, &agent); err != nil {
		return &Response{Allowed: true} // no spec to validate
	}

	if agent.Spec.AutonomyClassRef == nil || agent.Spec.AutonomyClassRef.Name == "" {
		return &Response{Allowed: true}
	}

	acName := agent.Spec.AutonomyClassRef.Name

	// Look up the referenced AutonomyClass.
	raw, err := v.store.Get(ctx, "AutonomyClass", req.Namespace, acName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &Response{
				Allowed: false,
				Reason:  fmt.Sprintf("autonomy class %q not found", acName),
			}
		}
		return &Response{Allowed: false, Reason: fmt.Sprintf("failed to get autonomy class: %v", err)}
	}

	var ac v1alpha1.AutonomyClass
	if err := json.Unmarshal(raw, &ac); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("failed to parse autonomy class: %v", err)}
	}

	var violations []string

	// Validate cost limit.
	if ac.Spec.MaxCostPerAction > 0 && agent.Spec.MaxCostPerAction > ac.Spec.MaxCostPerAction {
		violations = append(violations, fmt.Sprintf(
			"maxCostPerAction %d exceeds autonomy class limit %d",
			agent.Spec.MaxCostPerAction, ac.Spec.MaxCostPerAction))
	}

	// Validate tools against allowed list.
	if len(ac.Spec.AllowedTools) > 0 {
		allowed := make(map[string]bool, len(ac.Spec.AllowedTools))
		for _, t := range ac.Spec.AllowedTools {
			allowed[t] = true
		}
		for _, t := range agent.Spec.Tools {
			if t.Ref.Name != "" && !allowed[t.Ref.Name] {
				violations = append(violations, fmt.Sprintf("tool %q not allowed by autonomy class", t.Ref.Name))
			}
		}
	}

	if len(violations) > 0 {
		return &Response{Allowed: false, Reason: strings.Join(violations, "; ")}
	}

	return &Response{Allowed: true}
}
