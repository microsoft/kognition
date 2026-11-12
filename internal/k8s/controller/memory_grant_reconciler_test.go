// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
)

func TestMemoryGrantReconcilerAdapterAddsFinalizerThroughStoreAdapter(t *testing.T) {
	grant := &k8s.MemoryGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "assistant-memory"},
		Spec: k8s.MemoryGrantSpec{
			AgentRef:       k8s.LocalObjectReference{Name: "assistant"},
			MemoryClaimRef: k8s.LocalObjectReference{Name: "conversation"},
			AccessMode:     "ReadWrite",
		},
	}
	client := newFakeClient(t, grant)
	adapter := NewMemoryGrantReconcilerAdapter(NewStoreAdapter(client), testLogger(t))

	result, err := adapter.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: grant.Namespace, Name: grant.Name},
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !result.Requeue { //nolint:staticcheck // Immediate requeue remains part of the controller-runtime 1.x contract.
		t.Fatal("Reconcile() did not requeue after adding the finalizer")
	}

	var updated k8s.MemoryGrant
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: grant.Namespace, Name: grant.Name}, &updated); err != nil {
		t.Fatalf("get updated MemoryGrant: %v", err)
	}
	if len(updated.Finalizers) != 1 || updated.Finalizers[0] != intctrl.MemoryGrantFinalizer {
		t.Fatalf("finalizers = %v, want [%s]", updated.Finalizers, intctrl.MemoryGrantFinalizer)
	}
}

func TestMemoryGrantReconcilerAdapterCascadesRevocationThroughStoreAdapter(t *testing.T) {
	grant := &k8s.MemoryGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "writer-memory"},
		Spec: k8s.MemoryGrantSpec{
			AgentRef:       k8s.LocalObjectReference{Name: "writer"},
			MemoryClaimRef: k8s.LocalObjectReference{Name: "conversation"},
			AccessMode:     "ReadWrite",
		},
	}
	claim := &k8s.MemoryClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "conversation"}}
	direct := &k8s.MemoryRecord{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "direct"}, Spec: k8s.MemoryRecordSpec{Provenance: &k8s.MemoryProvenance{GrantID: "writer-memory"}}}
	derived := &k8s.MemoryRecord{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "derived"}, Spec: k8s.MemoryRecordSpec{Provenance: &k8s.MemoryProvenance{DerivedFromRecordNames: []string{"direct"}}}}
	client := newFakeClient(t, grant, claim, direct, derived)
	adapter := NewMemoryGrantReconcilerAdapter(NewStoreAdapter(client), testLogger(t))
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: grant.Namespace, Name: grant.Name}}
	if _, err := adapter.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}

	if err := client.Delete(context.Background(), grant); err != nil {
		t.Fatalf("delete grant: %v", err)
	}
	if _, err := adapter.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("cascade deletion: %v", err)
	}

	var updatedClaim k8s.MemoryClaim
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: "conversation"}, &updatedClaim); err != nil {
		t.Fatal(err)
	}
	revoked, err := intctrl.RevokedMemoryGrantIDs(updatedClaim.Annotations)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := revoked["writer-memory"]; !found {
		t.Fatalf("claim revocations = %#v, want writer-memory", revoked)
	}
	for _, name := range []string{"direct", "derived"} {
		var record k8s.MemoryRecord
		if err := client.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: name}, &record); err != nil {
			t.Fatal(err)
		}
		revoked, err := intctrl.RevokedMemoryGrantIDs(record.Annotations)
		if err != nil {
			t.Fatal(err)
		}
		if _, found := revoked["writer-memory"]; !found {
			t.Fatalf("MemoryRecord %q revocations = %#v, want writer-memory", name, revoked)
		}
	}
}
