// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// ToolGrantValidator validates ToolGrant resources on CREATE and UPDATE.
type ToolGrantValidator struct {
	store store.Store
}

// NewToolGrantValidator creates a new ToolGrantValidator.
func NewToolGrantValidator(stores ...store.Store) *ToolGrantValidator {
	validator := &ToolGrantValidator{}
	if len(stores) > 0 {
		validator.store = stores[0]
	}
	return validator
}

func (v *ToolGrantValidator) Name() string { return "toolgrant-validator" }

func (v *ToolGrantValidator) Handles(kind string, op Operation) bool {
	return kind == "ToolGrant" && (op == OperationCreate || op == OperationUpdate)
}

func (v *ToolGrantValidator) Validate(ctx context.Context, req *Request) *Response {
	var grant v1alpha1.ToolGrant
	if err := json.Unmarshal(req.Object, &grant); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid toolGrant JSON: %v", err)}
	}
	if err := grant.Spec.Validate(); err != nil {
		return &Response{Allowed: false, Reason: err.Error()}
	}
	if v.store == nil {
		return &Response{Allowed: true}
	}
	toolData, err := v.store.Get(ctx, "Tool", req.Namespace, grant.Spec.ToolRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		return &Response{Allowed: false, Reason: fmt.Sprintf("toolGrant.spec.toolRef.name references missing Tool %q in namespace %q", grant.Spec.ToolRef.Name, req.Namespace)}
	}
	if err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("load referenced Tool: %v", err)}
	}
	var tool v1alpha1.Tool
	if err := json.Unmarshal(toolData, &tool); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("decode referenced Tool: %v", err)}
	}
	if err := validateConstraintProperties(grant.Spec.ArgumentConstraints, tool.Spec.EffectiveSchema(), "$"); err != nil {
		return &Response{Allowed: false, Reason: err.Error()}
	}
	if err := v.validateRateLimits(ctx, req.Namespace, grant.Spec.RateLimits); err != nil {
		return &Response{Allowed: false, Reason: err.Error()}
	}
	return &Response{Allowed: true}
}

func validateConstraintProperties(constraint, toolSchema map[string]any, path string) error {
	constraintProperties, _ := constraint["properties"].(map[string]any)
	if len(constraintProperties) == 0 {
		return nil
	}
	toolProperties, _ := toolSchema["properties"].(map[string]any)
	for name, rawConstraintProperty := range constraintProperties {
		rawToolProperty, ok := toolProperties[name]
		if !ok {
			return fmt.Errorf("toolGrant.spec.argumentConstraints property %s.%s is not accepted by Tool input schema", path, name)
		}
		constraintProperty, constraintIsSchema := rawConstraintProperty.(map[string]any)
		toolProperty, toolIsSchema := rawToolProperty.(map[string]any)
		if constraintIsSchema && toolIsSchema {
			if err := validateConstraintProperties(constraintProperty, toolProperty, path+"."+name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *ToolGrantValidator) validateRateLimits(ctx context.Context, namespace string, limits *v1alpha1.ToolGrantRateLimits) error {
	if limits == nil {
		return nil
	}
	items, err := v.store.List(ctx, "CognitiveQuota", store.ListOptions{Namespace: namespace})
	if err != nil {
		return fmt.Errorf("list CognitiveQuota resources: %w", err)
	}
	windows := []struct {
		name     string
		calls    int64
		duration time.Duration
	}{
		{name: "callsPerMinute", calls: limits.CallsPerMinute, duration: time.Minute},
		{name: "callsPerHour", calls: limits.CallsPerHour, duration: time.Hour},
		{name: "callsPerDay", calls: limits.CallsPerDay, duration: 24 * time.Hour},
	}
	for _, raw := range items {
		var quota v1alpha1.CognitiveQuota
		if err := json.Unmarshal(raw, &quota); err != nil || quota.Spec.CostBudget == nil || quota.Spec.CostBudget.MaxToolCallsPerPeriod <= 0 {
			continue
		}
		period, err := time.ParseDuration(quota.Spec.ResetInterval)
		if err != nil || period <= 0 {
			continue
		}
		for _, window := range windows {
			if window.calls <= 0 {
				continue
			}
			windowCount := int64(1)
			if period > window.duration {
				windowCount = int64((period + window.duration - 1) / window.duration)
			}
			if window.calls*windowCount > quota.Spec.CostBudget.MaxToolCallsPerPeriod {
				return fmt.Errorf("toolGrant.spec.rateLimits.%s exceeds CognitiveQuota %q tool-call maximum for reset period", window.name, quota.ObjectMeta.Name)
			}
		}
	}
	return nil
}
