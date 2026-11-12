// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	"go.uber.org/zap"
	ctrl "sigs.k8s.io/controller-runtime"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

// TrustProfileReconcilerAdapter wraps the backend-neutral TrustProfile reconciler.
type TrustProfileReconcilerAdapter struct {
	internal *intctrl.TrustProfileReconciler
	events   eventEmitter
}

func (a *TrustProfileReconcilerAdapter) SetAuditLog(audit *observability.CognitiveAuditLog) *TrustProfileReconcilerAdapter {
	a.internal.SetAuditLog(audit)
	return a
}

func NewTrustProfileReconcilerAdapter(s store.Store, logger *zap.Logger) *TrustProfileReconcilerAdapter {
	return &TrustProfileReconcilerAdapter{internal: intctrl.NewTrustProfileReconciler(s, logger)}
}

func (a *TrustProfileReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "TrustProfile",
	})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *TrustProfileReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "TrustProfile", "kognition-trustprofile-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("trustprofile").
		For(&k8s.TrustProfile{}).
		Complete(a)
}
