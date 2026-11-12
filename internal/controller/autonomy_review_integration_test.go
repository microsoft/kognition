// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// Integration test: AutonomyReview full lifecycle with human approval.
// Phases: create → reconcile (pending init) → reconcile (still pending) →
// human approval → reconcile (already decided, no-op).
// TC-0178, TC-0179: Full lifecycle - pending review, human approval, TrustProfile updated.
func TestIntegration_AutonomyReview_HumanApproval(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAutonomyReviewReconciler(s, logger).WithTTL(10 * time.Minute)
	ctx := context.Background()
	ns := "team-alpha"

	// --- Phase 1: Create review via reconciler API ---
	err := r.CreateReview(ctx, ns, "review-human", "agent-1", "deploy to production", "session-001")
	if err != nil {
		t.Fatalf("Phase 1: CreateReview: %v", err)
	}

	// Verify stored state.
	raw, err := s.Get(ctx, "AutonomyReview", ns, "review-human")
	if err != nil {
		t.Fatalf("Phase 1: Get: %v", err)
	}
	var review v1alpha1.AutonomyReview
	json.Unmarshal(raw, &review)
	if review.Status.Decision != DecisionPending {
		t.Fatalf("Phase 1: expected Pending, got %q", review.Status.Decision)
	}
	if review.Spec.AgentRef.Name != "agent-1" {
		t.Fatalf("Phase 1: expected agentRef=agent-1, got %q", review.Spec.AgentRef.Name)
	}
	if review.Spec.SessionID != "session-001" {
		t.Fatalf("Phase 1: expected sessionID=session-001, got %q", review.Spec.SessionID)
	}

	// --- Phase 2: Reconcile pending review → still within TTL, requeues ---
	result, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "review-human", Kind: "AutonomyReview"})
	if err != nil {
		t.Fatalf("Phase 2: Reconcile: %v", err)
	}
	// Review was already initialized as Pending by CreateReview, so Reconcile
	// sees it as pending and requeues for its persisted deadline.
	if result.RequeueAfter <= 9*time.Minute || result.RequeueAfter > 10*time.Minute {
		t.Fatalf("Phase 2: expected RequeueAfter near 10m, got %v", result.RequeueAfter)
	}

	// --- Phase 3: Human approves the review ---
	err = r.Approve(ctx, ns, "review-human", "admin@team-alpha.com", "reviewed and safe")
	if err != nil {
		t.Fatalf("Phase 3: Approve: %v", err)
	}

	raw, _ = s.Get(ctx, "AutonomyReview", ns, "review-human")
	json.Unmarshal(raw, &review)
	if review.Status.Decision != DecisionApproved {
		t.Fatalf("Phase 3: expected Approved, got %q", review.Status.Decision)
	}
	if review.Status.DecidedBy != "admin@team-alpha.com" {
		t.Fatalf("Phase 3: expected decidedBy=admin@team-alpha.com, got %q", review.Status.DecidedBy)
	}
	if review.Status.DecidedAt == "" {
		t.Fatal("Phase 3: expected non-empty DecidedAt")
	}

	// --- Phase 4: Reconcile already-decided review → no-op ---
	result, err = r.Reconcile(ctx, Request{Namespace: ns, Name: "review-human", Kind: "AutonomyReview"})
	if err != nil {
		t.Fatalf("Phase 4: Reconcile: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatal("Phase 4: expected no requeue for decided review")
	}

	// --- Phase 5: Cannot re-decide ---
	err = r.Deny(ctx, ns, "review-human", "attacker", "reverse decision")
	if err == nil {
		t.Fatal("Phase 5: expected error when re-deciding approved review")
	}
}

// Integration test: AutonomyReview TTL auto-denial.
// Verifies that expired reviews cannot be subsequently approved by humans.
// TC-0180: Expired review transitions to Denied status.
func TestIntegration_AutonomyReview_TTLExpiration(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAutonomyReviewReconciler(s, logger).WithTTL(3 * time.Minute)
	ctx := context.Background()
	ns := "team-gamma"

	// --- Phase 1: Create a review that has already exceeded TTL ---
	review := v1alpha1.AutonomyReview{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "AutonomyReview", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: "review-expired", CreationTimestamp: time.Now().Add(-10 * time.Minute)},
		Spec: v1alpha1.AutonomyReviewSpec{
			AgentRef:          v1alpha1.LocalObjectReference{Name: "agent-3"},
			ActionDescription: "destructive cleanup",
			SessionID:         "session-003",
		},
		Status: v1alpha1.AutonomyReviewStatus{Decision: DecisionPending},
	}
	data, _ := json.Marshal(review)
	s.Create(ctx, "AutonomyReview", ns, "review-expired", data)

	// --- Phase 2: Reconcile → TTL expired, marked as Denied ---
	result, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "review-expired", Kind: "AutonomyReview"})
	if err != nil {
		t.Fatalf("Phase 2: Reconcile: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatal("Phase 2: expected no requeue after expiration")
	}

	raw, _ := s.Get(ctx, "AutonomyReview", ns, "review-expired")
	var got v1alpha1.AutonomyReview
	json.Unmarshal(raw, &got)
	if got.Status.Decision != DecisionDenied {
		t.Fatalf("Phase 2: expected Denied, got %q", got.Status.Decision)
	}
	if got.Status.DecidedBy != "system/ttl" || got.Status.Reason != "TTL expired" {
		t.Fatalf("Phase 2: unexpected TTL decision metadata: %#v", got.Status)
	}

	// --- Phase 3: Cannot approve an expired review ---
	err = r.Approve(ctx, ns, "review-expired", "admin@late.com", "I was late")
	if err == nil {
		t.Fatal("Phase 3: expected error when approving expired review")
	}

	// --- Phase 4: Cannot deny an expired review ---
	err = r.Deny(ctx, ns, "review-expired", "admin@late.com", "too late")
	if err == nil {
		t.Fatal("Phase 4: expected error when denying expired review")
	}
}

// Integration test: Multiple reviews in the same namespace with different outcomes.
func TestIntegration_AutonomyReview_MultipleReviews(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAutonomyReviewReconciler(s, logger).WithTTL(5 * time.Minute)
	ctx := context.Background()
	ns := "multi-ns"

	// Create three reviews.
	if err := r.CreateReview(ctx, ns, "review-a", "agent-1", "action-a", "sess-a"); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateReview(ctx, ns, "review-b", "agent-2", "action-b", "sess-b"); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateReview(ctx, ns, "review-c", "agent-3", "action-c", "sess-c"); err != nil {
		t.Fatal(err)
	}

	// Approve one, deny another, leave third pending.
	r.Approve(ctx, ns, "review-a", "admin", "ok")
	r.Deny(ctx, ns, "review-b", "security", "too risky")

	// Reconcile all three.
	res, _ := r.Reconcile(ctx, Request{Namespace: ns, Name: "review-a", Kind: "AutonomyReview"})
	if res.RequeueAfter != 0 {
		t.Error("review-a: expected no requeue (approved)")
	}

	res, _ = r.Reconcile(ctx, Request{Namespace: ns, Name: "review-b", Kind: "AutonomyReview"})
	if res.RequeueAfter != 0 {
		t.Error("review-b: expected no requeue (denied)")
	}

	res, _ = r.Reconcile(ctx, Request{Namespace: ns, Name: "review-c", Kind: "AutonomyReview"})
	if res.RequeueAfter == 0 {
		t.Error("review-c: expected requeue (still pending)")
	}

	// Verify final states.
	verifyDecision := func(name, expected string) {
		raw, _ := s.Get(ctx, "AutonomyReview", ns, name)
		var rev v1alpha1.AutonomyReview
		json.Unmarshal(raw, &rev)
		if rev.Status.Decision != expected {
			t.Errorf("%s: expected %s, got %s", name, expected, rev.Status.Decision)
		}
	}
	verifyDecision("review-a", DecisionApproved)
	verifyDecision("review-b", DecisionDenied)
	verifyDecision("review-c", DecisionPending)
}
