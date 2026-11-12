// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// ApprovalDecisionValidator enforces two invariants on ApprovalDecision objects:
//  1. On CREATE, Spec.Approver must match the authenticated request user. This
//     prevents one identity from forging another user's vote toward the gate
//     quorum.
//  2. On UPDATE, Spec is immutable. Re-voting requires creating a new
//     ApprovalDecision (which preserves the audit trail).
//
// The Approver==user check is skipped when the API is running without
// authentication. Kubernetes webhooks and authenticated standalone requests
// both populate UserInfo.
type ApprovalDecisionValidator struct{}

// NewApprovalDecisionValidator creates a new ApprovalDecisionValidator.
func NewApprovalDecisionValidator() *ApprovalDecisionValidator {
	return &ApprovalDecisionValidator{}
}

func (v *ApprovalDecisionValidator) Name() string { return "approvaldecision-validator" }

func (v *ApprovalDecisionValidator) Handles(kind string, op Operation) bool {
	return kind == "ApprovalDecision" && (op == OperationCreate || op == OperationUpdate)
}

func (v *ApprovalDecisionValidator) Validate(_ context.Context, req *Request) *Response {
	var ad v1alpha1.ApprovalDecision
	if err := json.Unmarshal(req.Object, &ad); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid ApprovalDecision JSON: %v", err)}
	}

	switch req.Operation {
	case OperationCreate:
		if ad.Spec.Approver == "" {
			return &Response{Allowed: false, Reason: "spec.approver is required"}
		}
		if req.UserInfo.Username != "" && ad.Spec.Approver != req.UserInfo.Username {
			return &Response{Allowed: false, Reason: fmt.Sprintf(
				"spec.approver (%q) must match the requesting user (%q)",
				ad.Spec.Approver, req.UserInfo.Username,
			)}
		}
	case OperationUpdate:
		var old v1alpha1.ApprovalDecision
		if err := json.Unmarshal(req.OldObject, &old); err != nil {
			return &Response{Allowed: false, Reason: fmt.Sprintf("invalid old ApprovalDecision JSON: %v", err)}
		}
		if !reflect.DeepEqual(ad.Spec, old.Spec) {
			return &Response{Allowed: false, Reason: "ApprovalDecision.spec is immutable"}
		}
	}

	return &Response{Allowed: true}
}
