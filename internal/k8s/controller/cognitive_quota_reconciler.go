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

// CognitiveQuotaReconcilerAdapter wraps the internal CognitiveQuotaReconciler
// for use with controller-runtime.
type CognitiveQuotaReconcilerAdapter struct {
	internal *intctrl.CognitiveQuotaReconciler
	logger   *zap.Logger
	events   eventEmitter
}

// NewCognitiveQuotaReconcilerAdapter creates an adapter that delegates to the internal CognitiveQuotaReconciler.
func NewCognitiveQuotaReconcilerAdapter(s store.Store, logger *zap.Logger) *CognitiveQuotaReconcilerAdapter {
	return &CognitiveQuotaReconcilerAdapter{
		internal: intctrl.NewCognitiveQuotaReconciler(s, logger),
		logger:   logger,
	}
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *CognitiveQuotaReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "CognitiveQuota",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the CognitiveQuota reconciler with the controller-runtime manager.
func (a *CognitiveQuotaReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "CognitiveQuota", "kognition-cognitivequota-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("cognitivequota").
		For(&k8s.CognitiveQuota{}).
		Complete(a)
}
