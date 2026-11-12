// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// ToolClassValidator validates ToolClass resources on CREATE and UPDATE.
type ToolClassValidator struct{}

// NewToolClassValidator creates a new ToolClassValidator.
func NewToolClassValidator() *ToolClassValidator {
	return &ToolClassValidator{}
}

func (v *ToolClassValidator) Name() string { return "toolclass-validator" }

func (v *ToolClassValidator) Handles(kind string, op Operation) bool {
	return kind == "ToolClass" && (op == OperationCreate || op == OperationUpdate)
}

func (v *ToolClassValidator) Validate(_ context.Context, req *Request) *Response {
	var tc v1alpha1.ToolClass
	if err := json.Unmarshal(req.Object, &tc); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid toolclass JSON: %v", err)}
	}
	if err := tc.Spec.Validate(); err != nil {
		return &Response{Allowed: false, Reason: err.Error()}
	}
	return &Response{Allowed: true}
}
