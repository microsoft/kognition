// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

func preparedToolReservationFixture(t *testing.T, invocationID string, now *time.Time) (*StoreAdapter, *RuntimeApprovalToolPreparationReconciler, ctrl.Request, api.RuntimeApproval, string) {
	t.Helper()
	ctx := context.Background()
	c := newFakeClient(t)
	adapter := newRuntimeApprovalTestStore(t, c)
	adapter.now = func() time.Time { return *now }
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	_, approval, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, invocationID, api.RuntimeApprovalDecisionNotRequired), kubernetesRuntimeApprovalPayload, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	approval, err = adapter.AcknowledgeRuntimeApprovalDecisionAudit(ctx, "team-a", invocationID, approval.ResourceVersion, approval.Status.DecisionAudit.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := runtimeapproval.GeneratePreparationToken()
	if err != nil {
		t.Fatal(err)
	}
	deadline := now.Add(2 * time.Minute)
	approval, err = adapter.PrepareRuntimeApproval(ctx, "team-a", invocationID, approval.ResourceVersion, token, deadline)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.PrepareToolCallReservation(ctx, store.ToolCallReservation{
		Namespace: "team-a", InvocationID: invocationID, Binding: "binding", PreparationToken: token, PreparationDeadline: deadline,
	}); err != nil {
		t.Fatal(err)
	}
	reconciler := NewRuntimeApprovalToolPreparationReconciler(c, adapter)
	reconciler.now = func() time.Time { return *now }
	key := adapter.toolCallLedgerObjectKey("team-a")
	return adapter, reconciler, ctrl.Request{NamespacedName: types.NamespacedName(key)}, approval, token
}

func TestRuntimeApprovalToolPreparationReconcilerSchedulesFutureDeadline(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	_, reconciler, request, _, _ := preparedToolReservationFixture(t, "future", &now)
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 2*time.Minute {
		t.Fatalf("RequeueAfter = %s, want 2m", result.RequeueAfter)
	}
}

func TestRuntimeApprovalToolPreparationReconcilerReapsExpiredPreparation(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	adapter, reconciler, request, _, token := preparedToolReservationFixture(t, "expired", &now)
	now = now.Add(3 * time.Minute)
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	object := &k8s.RuntimeApproval{}
	if err := adapter.client.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: runtimeapproval.CanonicalName("team-a", "expired")}, object); err != nil {
		t.Fatal(err)
	}
	approval := runtimeApprovalFromKubernetes(object)
	if approval.Status.ExecutionDisposition != runtimeapproval.ExecutionDispositionAbandonedPreparation {
		t.Fatalf("execution disposition = %q", approval.Status.ExecutionDisposition)
	}
	if released, err := adapter.ReleaseToolCallReservation(context.Background(), "team-a", "expired", token); err != nil || released {
		t.Fatalf("reservation remains after reap: released=%v err=%v", released, err)
	}
}

func TestRuntimeApprovalToolPreparationReconcilerResumesCleanupAfterReap(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	adapter, reconciler, request, approval, token := preparedToolReservationFixture(t, "resume", &now)
	now = now.Add(3 * time.Minute)
	if _, err := adapter.ReapRuntimeApprovalPreparation(context.Background(), "team-a", "resume", approval.ResourceVersion, token); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if released, err := adapter.ReleaseToolCallReservation(context.Background(), "team-a", "resume", token); err != nil || released {
		t.Fatalf("reservation remains after resumed cleanup: released=%v err=%v", released, err)
	}
}

func TestRuntimeApprovalToolPreparationReconcilerNeverReleasesClaimedReservation(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	adapter, reconciler, request, approval, token := preparedToolReservationFixture(t, "claimed", &now)
	if _, err := adapter.ClaimRuntimeApprovalExecution(context.Background(), "team-a", "claimed", approval.ResourceVersion, token, newKubernetesClaimant(now)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Minute)
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if released, err := adapter.ReleaseToolCallReservation(context.Background(), "team-a", "claimed", token); err != nil || !released {
		t.Fatalf("claimed reservation was reaped: released=%v err=%v", released, err)
	}
}
