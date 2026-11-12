// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

func TestAutonomyReviewReconcilerAdapterInitializesNativeReview(t *testing.T) {
	review := &k8s.AutonomyReview{
		ObjectMeta: metav1.ObjectMeta{Name: "deploy-review", Namespace: "apps"},
		Spec: k8s.AutonomyReviewSpec{
			AgentRef:          k8s.LocalObjectReference{Name: "worker"},
			ActionDescription: "deploy release",
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(review).
		WithStatusSubresource(&k8s.AutonomyReview{}).Build()
	adapter := NewAutonomyReviewReconcilerAdapter(NewStoreAdapter(k8sClient), zap.NewNop())

	result, err := adapter.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "apps", Name: "deploy-review"}})
	if err != nil {
		t.Fatalf("reconcile native review: %v", err)
	}
	if result.RequeueAfter <= 4*time.Minute || result.RequeueAfter > 5*time.Minute {
		t.Fatalf("requeue after = %s, want near 5m", result.RequeueAfter)
	}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "deploy-review"}, review); err != nil {
		t.Fatalf("get native review: %v", err)
	}
	if review.Status.Decision != "Pending" {
		t.Fatalf("decision = %q, want Pending", review.Status.Decision)
	}
	if review.Status.ExpiresAt == "" {
		t.Fatal("expected status.expiresAt to be persisted")
	}
}

func TestAutonomyReviewReconcilerAdapterAutoDeniesExpiredNativeReview(t *testing.T) {
	review := &k8s.AutonomyReview{
		ObjectMeta: metav1.ObjectMeta{Name: "expired-review", Namespace: "apps", CreationTimestamp: metav1.NewTime(time.Now().Add(-10 * time.Minute))},
		Spec: k8s.AutonomyReviewSpec{
			AgentRef:          k8s.LocalObjectReference{Name: "worker"},
			ActionDescription: "deploy release",
		},
		Status: k8s.AutonomyReviewStatus{Decision: "Pending"},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(review).
		WithStatusSubresource(&k8s.AutonomyReview{}).Build()
	adapter := NewAutonomyReviewReconcilerAdapter(NewStoreAdapter(k8sClient), zap.NewNop())

	result, err := adapter.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "apps", Name: "expired-review"}})
	if err != nil {
		t.Fatalf("reconcile native review: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("unexpected requeue after terminal decision: %#v", result)
	}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "apps", Name: "expired-review"}, review); err != nil {
		t.Fatalf("get native review: %v", err)
	}
	if review.Status.Decision != "Denied" || review.Status.Reason != "TTL expired" || review.Status.DecidedBy != "system/ttl" {
		t.Fatalf("unexpected TTL denial: %#v", review.Status)
	}
}
