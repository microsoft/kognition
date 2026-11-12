// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// LimitRangeDefaulter is a MutatingHook that applies default resource limits
// from CognitiveLimitRange resources to agents that don't specify them.
type LimitRangeDefaulter struct {
	store store.Store
}

// NewLimitRangeDefaulter creates a new LimitRangeDefaulter.
func NewLimitRangeDefaulter(s store.Store) *LimitRangeDefaulter {
	return &LimitRangeDefaulter{store: s}
}

func (d *LimitRangeDefaulter) Name() string { return "limit-range-defaulter" }

func (d *LimitRangeDefaulter) Handles(kind string, op Operation) bool {
	return kind == "Agent" && op == OperationCreate
}

func (d *LimitRangeDefaulter) Mutate(ctx context.Context, req *Request) *Response {
	// List CognitiveLimitRanges in the namespace.
	items, err := d.store.List(ctx, "CognitiveLimitRange", store.ListOptions{Namespace: req.Namespace})
	if err != nil || len(items) == 0 {
		return &Response{Allowed: true}
	}

	// Parse the agent object.
	var objMap map[string]interface{}
	if err := json.Unmarshal(req.Object, &objMap); err != nil {
		return &Response{Allowed: true}
	}

	// Get or create the spec.resources map.
	spec, ok := objMap["spec"].(map[string]interface{})
	if !ok {
		spec = make(map[string]interface{})
		objMap["spec"] = spec
	}

	resources, ok := spec["resources"].(map[string]interface{})
	if !ok {
		resources = make(map[string]interface{})
	}

	mutated := false

	// Apply defaults from all matching CognitiveLimitRanges.
	for _, raw := range items {
		var lr v1alpha1.CognitiveLimitRange
		if err := json.Unmarshal(raw, &lr); err != nil {
			continue
		}

		for _, limit := range lr.Spec.Limits {
			// Apply defaults for resources that are not already set.
			for key, val := range limit.Default {
				if _, exists := resources[key]; !exists {
					resources[key] = val
					mutated = true
				}
			}
		}
	}

	if !mutated {
		return &Response{Allowed: true}
	}

	spec["resources"] = resources
	objMap["spec"] = spec

	patched, err := json.Marshal(objMap)
	if err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("failed to marshal patched object: %v", err)}
	}

	return &Response{Allowed: true, Patch: patched}
}
