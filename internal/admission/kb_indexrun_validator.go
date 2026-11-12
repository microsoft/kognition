// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// KnowledgeBaseIndexRunValidator validates KnowledgeBaseIndexRun CREATE and
// UPDATE operations. UPDATE is intentionally permissive on status; spec is
// treated as immutable so that a run is a stable audit record of what was
// requested.
type KnowledgeBaseIndexRunValidator struct{}

// NewKnowledgeBaseIndexRunValidator returns a new validator.
func NewKnowledgeBaseIndexRunValidator() *KnowledgeBaseIndexRunValidator {
	return &KnowledgeBaseIndexRunValidator{}
}

func (v *KnowledgeBaseIndexRunValidator) Name() string { return "knowledgebaseindexrun-validator" }

func (v *KnowledgeBaseIndexRunValidator) Handles(kind string, op Operation) bool {
	return kind == "KnowledgeBaseIndexRun" && (op == OperationCreate || op == OperationUpdate)
}

func (v *KnowledgeBaseIndexRunValidator) Validate(_ context.Context, req *Request) *Response {
	var run v1alpha1.KnowledgeBaseIndexRun
	if err := json.Unmarshal(req.Object, &run); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid knowledgeBaseIndexRun JSON: %v", err)}
	}

	var errs []string

	if strings.TrimSpace(run.Spec.KnowledgeBaseName) == "" {
		errs = append(errs, "spec.knowledgeBaseName is required")
	}
	if run.Spec.TTLSecondsAfterFinished != nil && *run.Spec.TTLSecondsAfterFinished < 0 {
		errs = append(errs, "spec.ttlSecondsAfterFinished must be >= 0")
	}

	if req.Operation == OperationUpdate {
		if len(req.OldObject) == 0 {
			errs = append(errs, "old knowledgeBaseIndexRun object is required for updates")
		} else {
			var old v1alpha1.KnowledgeBaseIndexRun
			if err := json.Unmarshal(req.OldObject, &old); err != nil {
				errs = append(errs, fmt.Sprintf("invalid old knowledgeBaseIndexRun JSON: %v", err))
			} else if !reflect.DeepEqual(old.Spec, run.Spec) {
				errs = append(errs, "spec is immutable")
			}
		}
	}

	if len(errs) > 0 {
		return &Response{Allowed: false, Reason: strings.Join(errs, "; ")}
	}
	return &Response{Allowed: true}
}
