// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestAutonomyReviewValidatorProtectsSpecAndDecisionIdentity(t *testing.T) {
	validator := NewAutonomyReviewValidator()
	base := v1alpha1.AutonomyReview{
		Spec: v1alpha1.AutonomyReviewSpec{
			AgentRef:          v1alpha1.LocalObjectReference{Name: "researcher"},
			ActionDescription: "promote researcher",
			TrustProfileRef:   "researcher-trust",
			CurrentClass:      "assisted",
			RequestedClass:    "autonomous",
		},
		Status: v1alpha1.AutonomyReviewStatus{Decision: "Pending"},
	}
	oldObject, _ := json.Marshal(base)

	tests := []struct {
		name     string
		mutate   func(*v1alpha1.AutonomyReview)
		username string
		allowed  bool
	}{
		{name: "verified approval", username: "alice", allowed: true, mutate: func(review *v1alpha1.AutonomyReview) {
			review.Status.Decision = "Approved"
			review.Status.DecidedBy = "alice"
			review.Status.DecidedAt = "2026-01-01T00:00:00Z"
		}},
		{name: "forged reviewer", username: "alice", mutate: func(review *v1alpha1.AutonomyReview) {
			review.Status.Decision = "Approved"
			review.Status.DecidedBy = "mallory"
			review.Status.DecidedAt = "2026-01-01T00:00:00Z"
		}},
		{name: "controller TTL denial", username: "system:serviceaccount:kognition-system:controller", allowed: true, mutate: func(review *v1alpha1.AutonomyReview) {
			review.Status.Decision = "Denied"
			review.Status.DecidedBy = "system/ttl"
			review.Status.Reason = "TTL expired"
			review.Status.DecidedAt = "2026-01-01T00:00:00Z"
		}},
		{name: "forged TTL denial", username: "alice", mutate: func(review *v1alpha1.AutonomyReview) {
			review.Status.Decision = "Denied"
			review.Status.DecidedBy = "system/ttl"
			review.Status.Reason = "TTL expired"
			review.Status.DecidedAt = "2026-01-01T00:00:00Z"
		}},
		{name: "retargeted spec", username: "alice", mutate: func(review *v1alpha1.AutonomyReview) {
			review.Spec.TrustProfileRef = "other-trust"
		}},
		{name: "terminal metadata rewrite", username: "alice", mutate: func(review *v1alpha1.AutonomyReview) {
			review.Status.Reason = "rewritten"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			current := base
			test.mutate(&current)
			object, _ := json.Marshal(current)
			response := validator.Validate(context.Background(), &Request{
				Operation: OperationUpdate,
				Kind:      "AutonomyReview",
				Object:    object,
				OldObject: oldObject,
				UserInfo:  UserInfo{Username: test.username},
			})
			if response.Allowed != test.allowed {
				t.Fatalf("Allowed = %v, reason = %q, want %v", response.Allowed, response.Reason, test.allowed)
			}
		})
	}
}
