// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// SecretRefValidator validates SecretRef resources on CREATE and UPDATE.
type SecretRefValidator struct{}

// NewSecretRefValidator creates a new SecretRefValidator.
func NewSecretRefValidator() *SecretRefValidator {
	return &SecretRefValidator{}
}

func (v *SecretRefValidator) Name() string { return "secretref-validator" }

func (v *SecretRefValidator) Handles(kind string, op Operation) bool {
	return kind == "SecretRef" && (op == OperationCreate || op == OperationUpdate)
}

func (v *SecretRefValidator) Validate(_ context.Context, req *Request) *Response {
	var secretRef v1alpha1.SecretRef
	if err := json.Unmarshal(req.Object, &secretRef); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid secretRef JSON: %v", err)}
	}
	if err := secretRef.Spec.Validate(); err != nil {
		return &Response{Allowed: false, Reason: err.Error()}
	}
	return &Response{Allowed: true}
}
