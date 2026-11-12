// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// QuotaEnforcer is a ValidatingHook that denies admission when creating resources
// would exceed the defined CognitiveQuota limits.
type QuotaEnforcer struct {
	store store.Store
}

// NewQuotaEnforcer creates a new QuotaEnforcer.
func NewQuotaEnforcer(s store.Store) *QuotaEnforcer {
	return &QuotaEnforcer{store: s}
}

func (e *QuotaEnforcer) Name() string { return "quota-enforcer" }

func (e *QuotaEnforcer) Handles(kind string, op Operation) bool {
	return op == OperationCreate || (kind == "Solution" && op == OperationUpdate)
}

func (e *QuotaEnforcer) Validate(ctx context.Context, req *Request) *Response {
	// List quotas in the namespace.
	items, err := e.store.List(ctx, "CognitiveQuota", store.ListOptions{Namespace: req.Namespace})
	if err != nil || len(items) == 0 {
		return &Response{Allowed: true}
	}
	if req.Kind == "Solution" {
		return validateSolutionOverrides(req.Object, items)
	}

	// Determine which resource types this kind consumes.
	resourceKey := kindToResourceKey(req.Kind)
	if resourceKey == "" {
		return &Response{Allowed: true}
	}

	var violations []string

	for _, raw := range items {
		var quota v1alpha1.CognitiveQuota
		if err := json.Unmarshal(raw, &quota); err != nil {
			continue
		}

		hardLimit, hasLimit := quota.Spec.Hard[resourceKey]
		if !hasLimit {
			continue
		}

		var used int64
		if quota.Status.Used != nil {
			used = quota.Status.Used[resourceKey]
		}

		if used+1 > hardLimit {
			violations = append(violations, fmt.Sprintf(
				"quota %q exceeded for %q: used %d / limit %d",
				quota.ObjectMeta.Name, resourceKey, used, hardLimit))
		}
	}

	if len(violations) > 0 {
		return &Response{Allowed: false, Reason: strings.Join(violations, "; ")}
	}

	return &Response{Allowed: true}
}

func validateSolutionOverrides(rawSolution []byte, rawQuotas [][]byte) *Response {
	var solution v1alpha1.Solution
	if err := json.Unmarshal(rawSolution, &solution); err != nil || solution.Spec.Overrides == nil {
		return &Response{Allowed: true}
	}

	var violations []string
	for _, rawQuota := range rawQuotas {
		var quota v1alpha1.CognitiveQuota
		if err := json.Unmarshal(rawQuota, &quota); err != nil {
			continue
		}
		if override := solution.Spec.Overrides.TokenBudget; override != nil && quota.Spec.TokenBudget != nil {
			limits := []struct {
				name     string
				override int64
				maximum  int64
			}{
				{"maxInputTokens", override.MaxInputTokens, quota.Spec.TokenBudget.MaxInputTokens},
				{"maxOutputTokens", override.MaxOutputTokens, quota.Spec.TokenBudget.MaxOutputTokens},
				{"maxTotalTokens", override.MaxTotalTokens, quota.Spec.TokenBudget.MaxTotalTokens},
			}
			for _, limit := range limits {
				if limit.maximum > 0 && limit.override > limit.maximum {
					violations = append(violations, fmt.Sprintf(
						"spec.overrides.tokenBudget.%s exceeds Workspace quota %q maximum: %d > %d",
						limit.name, quota.ObjectMeta.Name, limit.override, limit.maximum))
				}
			}
		}
		for resource, override := range solution.Spec.Overrides.Hard {
			if maximum, ok := quota.Spec.Hard[resource]; ok && override > maximum {
				violations = append(violations, fmt.Sprintf(
					"spec.overrides.hard[%q] exceeds Workspace quota %q maximum: %d > %d",
					resource, quota.ObjectMeta.Name, override, maximum))
			}
		}
	}
	if len(violations) > 0 {
		return &Response{Allowed: false, Reason: strings.Join(violations, "; ")}
	}
	return &Response{Allowed: true}
}

// kindToResourceKey maps a resource kind to its quota resource key.
func kindToResourceKey(kind string) string {
	switch kind {
	case "Agent":
		return "agents"
	case "Tool":
		return "tools"
	case "Session":
		return "sessions"
	case "ModelGrant":
		return "model-grants"
	case "ToolGrant":
		return "tool-grants"
	default:
		return ""
	}
}
