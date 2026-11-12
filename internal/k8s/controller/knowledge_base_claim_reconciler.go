// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	"go.uber.org/zap"
	ctrl "sigs.k8s.io/controller-runtime"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

// KnowledgeBaseClaimReconcilerAdapter wraps the internal
// KnowledgeBaseClaimReconciler for KnowledgeBaseClaim resources under
// controller-runtime.
type KnowledgeBaseClaimReconcilerAdapter struct {
	internal *intctrl.KnowledgeBaseClaimReconciler
	logger   *zap.Logger
	events   eventEmitter
}

// NewKnowledgeBaseClaimReconcilerAdapter creates an adapter that delegates to
// the internal KnowledgeBaseClaimReconciler.
func NewKnowledgeBaseClaimReconcilerAdapter(s store.Store, logger *zap.Logger) *KnowledgeBaseClaimReconcilerAdapter {
	return &KnowledgeBaseClaimReconcilerAdapter{
		internal: intctrl.NewKnowledgeBaseClaimReconciler(s, logger),
		logger:   logger,
	}
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *KnowledgeBaseClaimReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "KnowledgeBaseClaim",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the KnowledgeBaseClaim reconciler with the
// controller-runtime manager.
func (a *KnowledgeBaseClaimReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "KnowledgeBaseClaim", "kognition-knowledgebaseclaim-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("knowledgebaseclaim").
		For(&k8s.KnowledgeBaseClaim{}).
		Complete(a)
}
