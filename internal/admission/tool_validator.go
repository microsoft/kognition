// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// ToolValidator validates Tool resources on CREATE and UPDATE.
type ToolValidator struct{}

// NewToolValidator creates a new ToolValidator.
func NewToolValidator() *ToolValidator {
	return &ToolValidator{}
}

func (v *ToolValidator) Name() string { return "tool-validator" }

func (v *ToolValidator) Handles(kind string, op Operation) bool {
	return kind == "Tool" && (op == OperationCreate || op == OperationUpdate)
}

func (v *ToolValidator) Validate(_ context.Context, req *Request) *Response {
	var tool v1alpha1.Tool
	if err := json.Unmarshal(req.Object, &tool); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid tool JSON: %v", err)}
	}
	if err := tool.Spec.Validate(); err != nil {
		return &Response{Allowed: false, Reason: err.Error()}
	}
	return &Response{Allowed: true}
}
