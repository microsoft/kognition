// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

type RuntimeApprovalToolPreparationReconciler struct {
	client client.Client
	store  *StoreAdapter
	now    func() time.Time
}

func NewRuntimeApprovalToolPreparationReconciler(c client.Client, store *StoreAdapter) *RuntimeApprovalToolPreparationReconciler {
	return &RuntimeApprovalToolPreparationReconciler{client: c, store: store, now: time.Now}
}

func (r *RuntimeApprovalToolPreparationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ledgerObject := &corev1.ConfigMap{}
	if err := r.client.Get(ctx, req.NamespacedName, ledgerObject); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !isToolCallLedger(ledgerObject) {
		return ctrl.Result{}, nil
	}
	workspace := ledgerObject.Annotations[toolCallLedgerNamespaceAnnotation]
	if workspace == "" {
		return ctrl.Result{}, fmt.Errorf("Tool-call reservation ledger %s/%s lacks workspace annotation", ledgerObject.Namespace, ledgerObject.Name)
	}
	ledger, err := decodeToolCallLedger(ledgerObject)
	if err != nil {
		return ctrl.Result{}, err
	}

	now := r.now().UTC()
	var nextDeadline time.Time
	for invocationID, reservation := range ledger.Reservations {
		if now.Before(reservation.PreparationDeadline) {
			if nextDeadline.IsZero() || reservation.PreparationDeadline.Before(nextDeadline) {
				nextDeadline = reservation.PreparationDeadline
			}
			continue
		}
		if err := r.reapExpiredPreparation(ctx, workspace, invocationID, reservation.PreparationToken); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !nextDeadline.IsZero() {
		return ctrl.Result{RequeueAfter: nextDeadline.Sub(now)}, nil
	}
	return ctrl.Result{}, nil
}

func (r *RuntimeApprovalToolPreparationReconciler) reapExpiredPreparation(ctx context.Context, workspace, invocationID, preparationToken string) error {
	key := client.ObjectKey{Namespace: workspace, Name: runtimeapproval.CanonicalName(workspace, invocationID)}
	object := &k8s.RuntimeApproval{}
	if err := r.client.Get(ctx, key, object); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get RuntimeApproval for Tool preparation reap: %w", err)
	}
	approval := runtimeApprovalFromKubernetes(object)
	if toolPreparationWasReaped(approval) {
		return runtimeapproval.CleanupReapedToolPreparation(ctx, r.store, workspace, invocationID, preparationToken)
	}
	if approval.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted || approval.Status.PreparationToken != preparationToken {
		return nil
	}
	if _, err := runtimeapproval.ReapToolPreparation(ctx, r.store, r.store, workspace, invocationID, approval.ResourceVersion, preparationToken); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrConflict) {
		return err
	}

	current := &k8s.RuntimeApproval{}
	if err := r.client.Get(ctx, key, current); err != nil {
		return client.IgnoreNotFound(err)
	}
	winner := runtimeApprovalFromKubernetes(current)
	if toolPreparationWasReaped(winner) {
		return runtimeapproval.CleanupReapedToolPreparation(ctx, r.store, workspace, invocationID, preparationToken)
	}
	if winner.Status.ExecutionState == api.RuntimeApprovalExecutionNotStarted && winner.Status.PreparationToken == preparationToken {
		return store.ErrConflict
	}
	return nil
}

func toolPreparationWasReaped(approval api.RuntimeApproval) bool {
	return approval.Status.ExecutionState == api.RuntimeApprovalExecutionFailed &&
		approval.Status.ExecutionDisposition == runtimeapproval.ExecutionDispositionAbandonedPreparation
}

func isToolCallLedger(object client.Object) bool {
	return object.GetLabels()[toolCallLedgerManagedLabel] == toolCallLedgerManagedLabelValue
}

func (r *RuntimeApprovalToolPreparationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("runtimeapproval-tool-preparation").
		For(&corev1.ConfigMap{}, builder.WithPredicates(predicate.NewPredicateFuncs(isToolCallLedger))).
		Complete(r)
}
