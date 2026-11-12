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

// AgentServiceValidator validates AgentService routing configuration.
type AgentServiceValidator struct{}

// NewAgentServiceValidator creates an AgentService validator.
func NewAgentServiceValidator() *AgentServiceValidator { return &AgentServiceValidator{} }

func (v *AgentServiceValidator) Name() string { return "agent-service-validator" }

func (v *AgentServiceValidator) Handles(kind string, operation Operation) bool {
	return kind == "AgentService" && (operation == OperationCreate || operation == OperationUpdate)
}

func (v *AgentServiceValidator) Validate(_ context.Context, request *Request) *Response {
	var service v1alpha1.AgentService
	if err := json.Unmarshal(request.Object, &service); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid AgentService JSON: %v", err)}
	}
	if strings.TrimSpace(service.Spec.AgentRef.Name) == "" {
		return &Response{Allowed: false, Reason: "spec.agentRef.name is required"}
	}
	if service.Spec.RevisionPin != nil && *service.Spec.RevisionPin < 1 {
		return &Response{Allowed: false, Reason: "spec.revisionPin must be >= 1"}
	}
	return &Response{Allowed: true}
}
