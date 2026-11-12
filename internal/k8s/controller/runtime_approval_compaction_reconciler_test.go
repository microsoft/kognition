// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

func terminalKubernetesRuntimeApproval(t *testing.T, c client.Client, invocationID string) (*StoreAdapter, api.RuntimeApproval) {
	t.Helper()
	ctx := context.Background()
	adapter := newRuntimeApprovalTestStore(t, c)
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
	approval, err = adapter.PrepareRuntimeApproval(ctx, "team-a", invocationID, approval.ResourceVersion, token, adapter.runtimeApprovalNow().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	approval, err = adapter.ClaimRuntimeApprovalExecution(ctx, "team-a", invocationID, approval.ResourceVersion, token, newKubernetesClaimant(adapter.runtimeApprovalNow()))
	if err != nil {
		t.Fatal(err)
	}
	approval, err = adapter.FinalizeRuntimeApprovalExecution(ctx, "team-a", invocationID, approval.ResourceVersion, approval.Status.FencingToken, runtimeapproval.ExecutionDispositionSucceeded)
	if err != nil {
		t.Fatal(err)
	}
	return adapter, approval
}

func TestRuntimeApprovalCompactionCreatesVerifiedTombstoneBeforeDeletingLiveRecord(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	adapter, approval := terminalKubernetesRuntimeApproval(t, c, "compact-happy")
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	adapter.now = func() time.Time { return now }
	reconciler := NewRuntimeApprovalCompactionReconciler(c, adapter, nil, DefaultRuntimeApprovalTombstoneRetention)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: approval.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	live := &k8s.RuntimeApproval{}
	if err := c.Get(ctx, request.NamespacedName, live); !apierrors.IsNotFound(err) {
		t.Fatalf("live RuntimeApproval remains after compaction: %v", err)
	}
	tombstone := &k8s.RuntimeApprovalTombstone{}
	if err := c.Get(ctx, request.NamespacedName, tombstone); err != nil {
		t.Fatalf("get tombstone: %v", err)
	}
	if tombstone.Spec.RuntimeApprovalUID != approval.UID || tombstone.Spec.Binding.KeyVersion != approval.Spec.Binding.KeyVersion ||
		tombstone.Spec.Binding.Tag != approval.Spec.Binding.Tag || tombstone.Spec.DecisionState != approval.Status.DecisionState ||
		tombstone.Spec.ExecutionDisposition != runtimeapproval.ExecutionDispositionSucceeded {
		t.Fatalf("tombstone lost replay evidence: %#v", tombstone.Spec)
	}
	if !tombstone.Spec.RetentionDeadline.Time.Equal(now.Add(DefaultRuntimeApprovalTombstoneRetention)) || !runtimeApprovalHasFinalizer(tombstone, k8s.RuntimeApprovalRetentionFinalizer) {
		t.Fatalf("tombstone retention fence = %v finalizers %v", tombstone.Spec.RetentionDeadline, tombstone.Finalizers)
	}
	ticket := &k8s.RuntimeApprovalQuotaTicket{}
	if err := c.Get(ctx, request.NamespacedName, ticket); err != nil {
		t.Fatalf("get quota ticket: %v", err)
	}
	if ticket.Spec.Phase != k8s.RuntimeApprovalQuotaTicketPhaseCompacted || ticket.Spec.CompactedAt == nil || ticket.Spec.RetentionDeadline == nil ||
		!ticket.Spec.CompactedAt.Time.Equal(tombstone.Spec.CompactedAt.Time) || !ticket.Spec.RetentionDeadline.Time.Equal(tombstone.Spec.RetentionDeadline.Time) {
		t.Fatalf("compacted quota ticket does not match tombstone retention: %#v", ticket.Spec)
	}
	lookedUp, err := adapter.GetRuntimeApprovalByInvocation(ctx, "team-a", "compact-happy")
	if err != nil || lookedUp.UID != approval.UID || lookedUp.Status.ExecutionState != api.RuntimeApprovalExecutionCompleted {
		t.Fatalf("compacted lookup = approval %#v error %v", lookedUp, err)
	}

	created, replay, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, "compact-happy", api.RuntimeApprovalDecisionNotRequired), kubernetesRuntimeApprovalPayload, time.Minute)
	if err != nil || created || replay.Status.ExecutionState != api.RuntimeApprovalExecutionCompleted || replay.Status.ExecutionDisposition != runtimeapproval.ExecutionDispositionSucceeded {
		t.Fatalf("compacted replay = created %v approval %#v error %v", created, replay, err)
	}
	conflict := kubernetesRuntimeApprovalFixture(t, "compact-happy", api.RuntimeApprovalDecisionNotRequired)
	if _, _, err := adapter.CreateOrGetRuntimeApproval(ctx, conflict, json.RawMessage(`{"argument":"changed"}`), time.Minute); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("conflicting compacted replay error = %v, want ErrConflict", err)
	}
}

func TestRuntimeApprovalCompactionRecoversAmbiguousTombstoneCreate(t *testing.T) {
	ctx := context.Background()
	base := newFakeClient(t)
	adapter, approval := terminalKubernetesRuntimeApproval(t, base, "compact-ambiguous")
	wrapped := &runtimeApprovalSuccessfulCreateErrorClient{
		Client: base, targetName: approval.Name, targetObject: &k8s.RuntimeApprovalTombstone{},
	}
	adapter.client = wrapped
	reconciler := NewRuntimeApprovalCompactionReconciler(wrapped, adapter, nil, DefaultRuntimeApprovalTombstoneRetention)
	key := types.NamespacedName{Namespace: "team-a", Name: approval.Name}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile() after lost create result = %v", err)
	}
	if err := base.Get(ctx, key, &k8s.RuntimeApproval{}); !apierrors.IsNotFound(err) {
		t.Fatalf("live RuntimeApproval remains after recovered create: %v", err)
	}
	if err := base.Get(ctx, key, &k8s.RuntimeApprovalTombstone{}); err != nil {
		t.Fatalf("durable tombstone missing after recovered create: %v", err)
	}
}

func TestRuntimeApprovalCompactionBlocksNonTerminalDeletion(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	adapter := newRuntimeApprovalTestStore(t, c)
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	_, approval, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, "pending-delete", api.RuntimeApprovalDecisionPending), kubernetesRuntimeApprovalPayload, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "team-a", Name: approval.Name}
	live := &k8s.RuntimeApproval{}
	if err := c.Get(ctx, key, live); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, live); err != nil {
		t.Fatal(err)
	}
	reconciler := NewRuntimeApprovalCompactionReconciler(c, adapter, nil, DefaultRuntimeApprovalTombstoneRetention)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, live); err != nil || live.DeletionTimestamp.IsZero() || !runtimeApprovalHasFinalizer(live, k8s.RuntimeApprovalCompactionFinalizer) {
		t.Fatalf("non-terminal deletion was not blocked: object %#v error %v", live, err)
	}
	if err := c.Get(ctx, key, &k8s.RuntimeApprovalTombstone{}); !apierrors.IsNotFound(err) {
		t.Fatalf("non-terminal approval produced tombstone: %v", err)
	}
}

func TestRuntimeApprovalRetentionUsesWorkspaceLeaseAndRequiresNoLiveOverlap(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	adapter, approval := terminalKubernetesRuntimeApproval(t, c, "retain-proof")
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	adapter.now = func() time.Time { return now }
	live := &k8s.RuntimeApproval{}
	key := types.NamespacedName{Namespace: "team-a", Name: approval.Name}
	if err := c.Get(ctx, key, live); err != nil {
		t.Fatal(err)
	}
	tombstone, err := adapter.CreateOrVerifyRuntimeApprovalTombstone(ctx, live, DefaultRuntimeApprovalTombstoneRetention)
	if err != nil {
		t.Fatal(err)
	}
	tombstone.Spec.RetentionDeadline.Time = now.Add(-time.Minute)
	// The fake client bypasses immutable CRD admission, which lets this test
	// place the clock after the persisted deadline without waiting 90 days.
	if err := c.Update(ctx, tombstone); err != nil {
		t.Fatal(err)
	}
	reconciler := NewRuntimeApprovalRetentionReconciler(c, adapter, nil)
	reconciler.now = func() time.Time { return now }
	ticketReconciler := NewRuntimeApprovalQuotaTicketRetirementReconciler(c, adapter, nil)
	request := ctrl.Request{NamespacedName: key}
	if result, err := ticketReconciler.Reconcile(ctx, request); err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("ticket retirement with live/tombstone overlap = result %#v error %v", result, err)
	}
	result, err := reconciler.Reconcile(ctx, request)
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("retention with live overlap = result %#v error %v", result, err)
	}
	if err := c.Delete(ctx, live); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, live); err != nil {
		t.Fatal(err)
	}
	live.Finalizers = nil
	if err := c.Update(ctx, live); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("begin retention after live deletion: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("finish retention after live deletion: %v", err)
	}
	if err := c.Get(ctx, key, &k8s.RuntimeApprovalTombstone{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expired tombstone remains: %v", err)
	}
	if _, err := ticketReconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("begin orphan ticket retirement: %v", err)
	}
	if _, err := ticketReconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("finish orphan ticket retirement: %v", err)
	}
	if err := c.Get(ctx, key, &k8s.RuntimeApprovalQuotaTicket{}); !apierrors.IsNotFound(err) {
		t.Fatalf("orphan quota ticket remains: %v", err)
	}
	lease, err := adapter.acquireRuntimeApprovalWorkspaceLease(ctx, "team-a")
	if err != nil {
		t.Fatalf("workspace Lease remained held: %v", err)
	}
	if err := adapter.releaseRuntimeApprovalWorkspaceLease(ctx, lease); err != nil {
		t.Fatalf("release verification Lease: %v", err)
	}
}

func TestRuntimeApprovalQuotaTicketRetirementAcceptsLegacyFinalizerDuringDeletion(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient(t)
	adapter := NewStoreAdapter(c)
	ticket := &k8s.RuntimeApprovalQuotaTicket{
		ObjectMeta: metav1.ObjectMeta{
			Name:       runtimeapproval.CanonicalName("team-a", "legacy-finalizer"),
			Namespace:  "team-a",
			Finalizers: []string{legacyRuntimeApprovalQuotaTicketRetirementFinalizer},
		},
		Spec: k8s.RuntimeApprovalQuotaTicketSpec{
			Workspace:    "team-a",
			InvocationID: "legacy-finalizer",
		},
	}
	if err := c.Create(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	reconciler := NewRuntimeApprovalQuotaTicketRetirementReconciler(c, adapter, nil)
	key := types.NamespacedName{Namespace: ticket.Namespace, Name: ticket.Name}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("retire legacy-finalized ticket: %v", err)
	}
	if err := c.Get(ctx, key, &k8s.RuntimeApprovalQuotaTicket{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy-finalized ticket remains: %v", err)
	}
}

func TestRuntimeApprovalTombstoneRetentionConfiguration(t *testing.T) {
	if got, err := RuntimeApprovalTombstoneRetention(""); err != nil || got != DefaultRuntimeApprovalTombstoneRetention {
		t.Fatalf("default retention = %v, %v", got, err)
	}
	if _, err := RuntimeApprovalTombstoneRetention("2160h"); err != nil {
		t.Fatalf("90-day retention rejected: %v", err)
	}
	if _, err := RuntimeApprovalTombstoneRetention("2159h"); err == nil {
		t.Fatal("retention below 90 days accepted")
	}
}
