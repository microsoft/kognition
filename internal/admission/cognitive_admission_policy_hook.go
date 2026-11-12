// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/store"
)

// CognitiveAdmissionPolicyHook is a ValidatingHook that evaluates admission
// requests against CognitiveAdmissionPolicy Rego rules loaded from the store.
type CognitiveAdmissionPolicyHook struct {
	store  store.Store
	engine *opa.Engine
}

// NewCognitiveAdmissionPolicyHook creates a new CognitiveAdmissionPolicyHook.
func NewCognitiveAdmissionPolicyHook(s store.Store, engine *opa.Engine) *CognitiveAdmissionPolicyHook {
	return &CognitiveAdmissionPolicyHook{
		store:  s,
		engine: engine,
	}
}

func (h *CognitiveAdmissionPolicyHook) Name() string {
	return "cognitive-admission-policy"
}

func (h *CognitiveAdmissionPolicyHook) Handles(_ string, op Operation) bool {
	return op == OperationCreate || op == OperationUpdate
}

func (h *CognitiveAdmissionPolicyHook) Validate(ctx context.Context, req *Request) *Response {
	// List all CognitiveAdmissionPolicies in the namespace.
	items, err := h.store.List(ctx, "CognitiveAdmissionPolicy", store.ListOptions{Namespace: req.Namespace})
	if err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("failed to list admission policies: %v", err)}
	}

	if len(items) == 0 {
		return &Response{Allowed: true}
	}

	// Build the OPA input from the admission request.
	input := map[string]interface{}{
		"kind":      req.Kind,
		"namespace": req.Namespace,
		"name":      req.Name,
		"operation": string(req.Operation),
	}

	// Parse the object into the input map.
	var objMap map[string]interface{}
	if err := json.Unmarshal(req.Object, &objMap); err == nil {
		for k, v := range objMap {
			input[k] = v
		}
	}

	// Evaluate each matching policy.
	for _, raw := range items {
		var policy v1alpha1.CognitiveAdmissionPolicy
		if err := json.Unmarshal(raw, &policy); err != nil {
			continue
		}

		// Check if this policy matches the request kind.
		if !policyMatchesKind(policy.Spec.MatchKinds, req.Kind) {
			continue
		}
		if !policyMatchesOperation(policy.Spec.MatchOperations, req.Operation) {
			continue
		}

		// Load/update the policy in the engine.
		policyKey := fmt.Sprintf("%s/%s", policy.ObjectMeta.Namespace, policy.ObjectMeta.Name)
		if err := h.engine.LoadPolicy(policyKey, policy.Spec.Rego); err != nil {
			return &Response{
				Allowed: false,
				Reason:  fmt.Sprintf("failed to load policy %q: %v", policy.ObjectMeta.Name, err),
			}
		}

		// Evaluate the policy.
		result, err := h.engine.Evaluate(ctx, policyKey, input)
		if err != nil {
			return &Response{
				Allowed: false,
				Reason:  fmt.Sprintf("policy %q evaluation error: %v", policy.ObjectMeta.Name, err),
			}
		}

		if !result.Allowed {
			return &Response{
				Allowed: false,
				Reason:  fmt.Sprintf("denied by policy %q: %s", policy.ObjectMeta.Name, result.Reason),
			}
		}
	}

	return &Response{Allowed: true}
}

// policyMatchesKind checks if the policy applies to the given resource kind.
func policyMatchesKind(matchKinds []string, kind string) bool {
	if len(matchKinds) == 0 {
		return true // empty matchKinds means match all
	}
	for _, k := range matchKinds {
		if k == kind || k == "*" {
			return true
		}
	}
	return false
}

func policyMatchesOperation(matchOperations []string, operation Operation) bool {
	if len(matchOperations) == 0 {
		return true
	}
	for _, candidate := range matchOperations {
		if strings.EqualFold(strings.TrimSpace(candidate), string(operation)) {
			return true
		}
	}
	return false
}
