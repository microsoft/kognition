// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/apimachinery/pkg/types"
)

func TestKnowledgeBaseReconcilerAdapter_KindMapping(t *testing.T) {
	fs := newFakeStore()
	adapter := NewKnowledgeBaseReconcilerAdapter(fs, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "kb1"}}
	_, _ = adapter.Reconcile(context.Background(), req)

	if len(fs.getCalls) == 0 {
		t.Fatal("expected at least one Get call to the store")
	}
	if fs.getCalls[0].Kind != "KnowledgeBase" {
		t.Errorf("kind = %q, want %q", fs.getCalls[0].Kind, "KnowledgeBase")
	}
	if fs.getCalls[0].Namespace != "ns" {
		t.Errorf("namespace = %q, want %q", fs.getCalls[0].Namespace, "ns")
	}
	if fs.getCalls[0].Name != "kb1" {
		t.Errorf("name = %q, want %q", fs.getCalls[0].Name, "kb1")
	}
}

func TestKnowledgeBaseReconcilerAdapter_InterfaceCompliance(t *testing.T) {
	fs := newFakeStore()
	var _ reconcile.Reconciler = NewKnowledgeBaseReconcilerAdapter(fs, testLogger(t))
}

func TestKnowledgeBaseReconcilerAdapter_NotFoundReturnsEmpty(t *testing.T) {
	fs := newFakeStore()
	adapter := NewKnowledgeBaseReconcilerAdapter(fs, testLogger(t))

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "missing"}}
	result, err := adapter.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error for not-found: %v", err)
	}
	if result.Requeue { //nolint:staticcheck // Verify compatibility with immediate requeue results from controller-runtime 1.x.
		t.Error("expected Requeue=false for not-found")
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected RequeueAfter=0, got %v", result.RequeueAfter)
	}
}
