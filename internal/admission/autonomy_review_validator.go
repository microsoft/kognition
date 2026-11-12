// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// AutonomyReviewValidator protects immutable review evidence and decision attribution.
type AutonomyReviewValidator struct{}

func NewAutonomyReviewValidator() *AutonomyReviewValidator { return &AutonomyReviewValidator{} }
func (*AutonomyReviewValidator) Name() string              { return "autonomy-review-validator" }
func (*AutonomyReviewValidator) Handles(kind string, _ Operation) bool {
	return kind == "AutonomyReview"
}

func (*AutonomyReviewValidator) Validate(_ context.Context, req *Request) *Response {
	var current v1alpha1.AutonomyReview
	if err := json.Unmarshal(req.Object, &current); err != nil {
		return denyAutonomyReview(fmt.Sprintf("invalid AutonomyReview: %v", err))
	}
	if req.Operation == OperationCreate {
		if current.Status.Decision != "" && current.Status.Decision != "Pending" {
			return denyAutonomyReview("AutonomyReview must be created with an empty or Pending decision")
		}
		return &Response{Allowed: true}
	}
	var previous v1alpha1.AutonomyReview
	if err := json.Unmarshal(req.OldObject, &previous); err != nil {
		return denyAutonomyReview(fmt.Sprintf("invalid existing AutonomyReview: %v", err))
	}
	oldSpec, _ := json.Marshal(previous.Spec)
	newSpec, _ := json.Marshal(current.Spec)
	if string(oldSpec) != string(newSpec) {
		return denyAutonomyReview("AutonomyReview spec is immutable after creation")
	}
	if previous.Status.ExpiresAt != "" && current.Status.ExpiresAt != previous.Status.ExpiresAt {
		return denyAutonomyReview("status.expiresAt is immutable after initialization")
	}
	if current.Status.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, current.Status.ExpiresAt); err != nil {
			return denyAutonomyReview("status.expiresAt must be an RFC3339 timestamp")
		}
		if previous.Status.ExpiresAt == "" && !strings.HasPrefix(req.UserInfo.Username, "system:serviceaccount:") {
			return denyAutonomyReview("only a controller service account may initialize status.expiresAt")
		}
	}
	if previous.Status.Decision == current.Status.Decision {
		oldStatus, _ := json.Marshal(previous.Status)
		newStatus, _ := json.Marshal(current.Status)
		if string(oldStatus) != string(newStatus) {
			return denyAutonomyReview("an unchanged AutonomyReview decision cannot mutate status fields")
		}
		return &Response{Allowed: true}
	}
	if previous.Status.Decision != "" && previous.Status.Decision != "Pending" {
		return denyAutonomyReview("a decided AutonomyReview cannot transition again")
	}
	switch current.Status.Decision {
	case "Pending":
		if current.Status.DecidedBy != "" || current.Status.DecidedAt != "" || current.Status.Reason != "" {
			return denyAutonomyReview("a Pending AutonomyReview cannot contain decision metadata")
		}
		return &Response{Allowed: true}
	case "Approved", "Denied":
		if req.UserInfo.Username == "" {
			return denyAutonomyReview("authenticated reviewer identity is required")
		}
		if current.Status.Decision == "Denied" && current.Status.Reason == "TTL expired" {
			if !strings.HasPrefix(req.UserInfo.Username, "system:serviceaccount:") || current.Status.DecidedBy != "system/ttl" {
				return denyAutonomyReview("only a controller service account may apply a TTL denial")
			}
		} else if current.Status.DecidedBy != req.UserInfo.Username {
			return denyAutonomyReview("status.decidedBy must match the authenticated reviewer")
		}
		if current.Status.DecidedAt == "" {
			return denyAutonomyReview("a terminal AutonomyReview decision requires status.decidedAt")
		}
		if _, err := time.Parse(time.RFC3339, current.Status.DecidedAt); err != nil {
			return denyAutonomyReview("status.decidedAt must be an RFC3339 timestamp")
		}
		return &Response{Allowed: true}
	default:
		return denyAutonomyReview("unsupported AutonomyReview decision transition")
	}
}

func denyAutonomyReview(reason string) *Response {
	return &Response{Allowed: false, Reason: reason}
}
