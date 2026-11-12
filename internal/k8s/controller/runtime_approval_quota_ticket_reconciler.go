// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

type RuntimeApprovalQuotaTicketRetirementReconciler struct {
	client client.Client
	store  *StoreAdapter
	logger *zap.Logger
}

const legacyRuntimeApprovalQuotaTicketRetirementFinalizer = "runtimeapprovalquotaticket.retirement.kognition.dev"

func NewRuntimeApprovalQuotaTicketRetirementReconciler(c client.Client, store *StoreAdapter, logger *zap.Logger) *RuntimeApprovalQuotaTicketRetirementReconciler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &RuntimeApprovalQuotaTicketRetirementReconciler{client: c, store: store, logger: logger.Named("runtime-approval-quota-ticket-retirement")}
}

func (r *RuntimeApprovalQuotaTicketRetirementReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, resultErr error) {
	ticket := &k8s.RuntimeApprovalQuotaTicket{}
	if err := r.client.Get(ctx, req.NamespacedName, ticket); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if ticket.Spec.Workspace != ticket.Namespace || ticket.Name != runtimeapproval.CanonicalName(ticket.Spec.Workspace, ticket.Spec.InvocationID) {
		return ctrl.Result{}, fmt.Errorf("RuntimeApprovalQuotaTicket identity does not match its logical key")
	}
	hasRetirementFinalizer := runtimeApprovalHasFinalizer(ticket, k8s.RuntimeApprovalQuotaTicketRetirementFinalizer)
	hasLegacyRetirementFinalizer := runtimeApprovalHasFinalizer(ticket, legacyRuntimeApprovalQuotaTicketRetirementFinalizer)
	if !hasRetirementFinalizer {
		if !ticket.DeletionTimestamp.IsZero() && !hasLegacyRetirementFinalizer {
			return ctrl.Result{}, fmt.Errorf("deleting RuntimeApprovalQuotaTicket lacks retirement finalizer")
		}
		if ticket.DeletionTimestamp.IsZero() {
			addRuntimeApprovalFinalizer(ticket, k8s.RuntimeApprovalQuotaTicketRetirementFinalizer)
			removeRuntimeApprovalFinalizer(ticket, legacyRuntimeApprovalQuotaTicketRetirementFinalizer)
			if err := r.client.Update(ctx, ticket); err != nil {
				return ctrl.Result{}, fmt.Errorf("repair RuntimeApprovalQuotaTicket retirement finalizer: %w", err)
			}
			return ctrl.Result{Requeue: true}, nil
		}
	}
	lease, err := r.store.acquireRuntimeApprovalWorkspaceLease(ctx, ticket.Spec.Workspace)
	if err != nil {
		return ctrl.Result{RequeueAfter: runtimeApprovalRetentionRetry}, nil
	}
	defer func() {
		if err := r.store.releaseRuntimeApprovalWorkspaceLease(ctx, lease); resultErr == nil && err != nil {
			resultErr = err
		}
	}()
	current := &k8s.RuntimeApprovalQuotaTicket{}
	if err := r.client.Get(ctx, req.NamespacedName, current); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	for _, record := range []client.Object{&k8s.RuntimeApproval{}, &k8s.RuntimeApprovalTombstone{}} {
		if err := r.client.Get(ctx, req.NamespacedName, record); err == nil {
			return ctrl.Result{RequeueAfter: runtimeApprovalRetentionRetry}, nil
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("prove RuntimeApproval record absence before ticket retirement: %w", err)
		}
	}
	if !runtimeApprovalHasFinalizer(current, k8s.RuntimeApprovalQuotaTicketRetirementFinalizer) &&
		!runtimeApprovalHasFinalizer(current, legacyRuntimeApprovalQuotaTicketRetirementFinalizer) {
		return ctrl.Result{}, fmt.Errorf("RuntimeApprovalQuotaTicket lacks retirement finalizer")
	}
	if current.DeletionTimestamp.IsZero() {
		if err := r.client.Delete(ctx, current); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("begin orphan RuntimeApprovalQuotaTicket deletion: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}
	lease, err = r.store.renewRuntimeApprovalWorkspaceLease(ctx, lease)
	if err != nil {
		return ctrl.Result{RequeueAfter: runtimeApprovalRetentionRetry}, nil
	}
	writeCtx, cancel := context.WithTimeout(ctx, runtimeApprovalWorkspaceWritePeriod)
	defer cancel()
	removeRuntimeApprovalFinalizer(current, k8s.RuntimeApprovalQuotaTicketRetirementFinalizer)
	removeRuntimeApprovalFinalizer(current, legacyRuntimeApprovalQuotaTicketRetirementFinalizer)
	if err := r.client.Update(writeCtx, current); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("remove RuntimeApprovalQuotaTicket retirement finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *RuntimeApprovalQuotaTicketRetirementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("runtimeapproval-quota-ticket-retirement").
		For(&k8s.RuntimeApprovalQuotaTicket{}).
		Complete(r)
}
