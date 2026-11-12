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

// AgentValidator validates Agent resources on CREATE and UPDATE.
type AgentValidator struct{}

// NewAgentValidator creates a new AgentValidator.
func NewAgentValidator() *AgentValidator {
	return &AgentValidator{}
}

func (v *AgentValidator) Name() string { return "agent-validator" }

func (v *AgentValidator) Handles(kind string, op Operation) bool {
	return kind == "Agent" && (op == OperationCreate || op == OperationUpdate)
}

func (v *AgentValidator) Validate(_ context.Context, req *Request) *Response {
	var agent v1alpha1.Agent
	if err := json.Unmarshal(req.Object, &agent); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid agent JSON: %v", err)}
	}

	var errs []string

	// modelRef.name is required.
	if agent.Spec.ModelRef.Name == "" {
		errs = append(errs, "spec.modelRef.name is required")
	}

	// Instructions must have exactly one of inline or ref.
	hasInline := agent.Spec.Instructions.Inline != ""
	hasRef := agent.Spec.Instructions.Ref != nil && agent.Spec.Instructions.Ref.Name != ""
	if !hasInline && !hasRef {
		errs = append(errs, "spec.instructions must specify either inline or ref")
	}
	if hasInline && hasRef {
		errs = append(errs, "spec.instructions must not specify both inline and ref")
	}

	// Tools: each tool ref.name must be non-empty if specified.
	for i, tool := range agent.Spec.Tools {
		if tool.Ref.Name == "" {
			errs = append(errs, fmt.Sprintf("spec.tools[%d].ref.name is required", i))
		}
	}

	// Replicas must be >= 0 if specified.
	if agent.Spec.Replicas != nil && *agent.Spec.Replicas < 0 {
		errs = append(errs, "spec.replicas must be >= 0")
	}

	// Companions: each ref.name must be non-empty.
	for i, c := range agent.Spec.Companions {
		if c.Name == "" {
			errs = append(errs, fmt.Sprintf("spec.companions[%d].name is required", i))
		}
	}

	// KnowledgeBaseClaims: each ref.name must be non-empty.
	for i, kb := range agent.Spec.KnowledgeBaseClaims {
		if kb.Name == "" {
			errs = append(errs, fmt.Sprintf("spec.knowledgeBaseClaims[%d].name is required", i))
		}
	}

	if len(errs) > 0 {
		return &Response{Allowed: false, Reason: strings.Join(errs, "; ")}
	}

	return &Response{Allowed: true}
}
