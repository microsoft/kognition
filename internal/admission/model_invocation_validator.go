// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
)

type ModelInvocationValidator struct{}

func NewModelInvocationValidator() *ModelInvocationValidator { return &ModelInvocationValidator{} }
func (v *ModelInvocationValidator) Name() string             { return "model-invocation-validator" }
func (v *ModelInvocationValidator) Handles(kind string, operation Operation) bool {
	return (kind == "Model" || kind == "ModelInvocationProfile") && (operation == OperationCreate || operation == OperationUpdate)
}

func (v *ModelInvocationValidator) Validate(_ context.Context, request *Request) *Response {
	var object struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(request.Object, &object); err != nil {
		return &Response{Reason: fmt.Sprintf("invalid invocation resource: %v", err)}
	}
	if request.Kind == "Model" {
		var spec api.ModelSpec
		if err := json.Unmarshal(object.Spec, &spec); err != nil {
			return &Response{Reason: fmt.Sprintf("invalid Model spec: %v", err)}
		}
		if err := modelinvocation.ValidateModel(spec); err != nil {
			return &Response{Reason: err.Error()}
		}
		return &Response{Allowed: true}
	}
	var spec api.ModelInvocationProfileSpec
	decoder := json.NewDecoder(bytes.NewReader(object.Spec))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return &Response{Reason: fmt.Sprintf("invalid invocation profile spec: %v", err)}
	}
	if err := spec.Validate(); err != nil {
		return &Response{Reason: err.Error()}
	}
	if request.Operation == OperationUpdate {
		var previous api.ModelInvocationProfile
		if err := json.Unmarshal(request.OldObject, &previous); err != nil {
			return &Response{Reason: "previous invocation profile is unavailable"}
		}
		if !reflect.DeepEqual(spec, previous.Spec) {
			return &Response{Reason: "invocation profile spec is immutable; create a new revision"}
		}
	}
	return &Response{Allowed: true}
}
