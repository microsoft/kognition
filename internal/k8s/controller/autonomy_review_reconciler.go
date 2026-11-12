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

// AutonomyReviewReconcilerAdapter wraps the backend-neutral AutonomyReview reconciler.
type AutonomyReviewReconcilerAdapter struct {
	internal *intctrl.AutonomyReviewReconciler
	events   eventEmitter
}

func NewAutonomyReviewReconcilerAdapter(s store.Store, logger *zap.Logger) *AutonomyReviewReconcilerAdapter {
	return &AutonomyReviewReconcilerAdapter{internal: intctrl.NewAutonomyReviewReconciler(s, logger)}
}

// SetAuditLog forwards durable decision evidence recording to the shared reconciler.
func (a *AutonomyReviewReconcilerAdapter) SetAuditLog(audit *observability.CognitiveAuditLog) *AutonomyReviewReconcilerAdapter {
	a.internal.SetAuditLog(audit)
	return a
}

func (a *AutonomyReviewReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "AutonomyReview",
	})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *AutonomyReviewReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "AutonomyReview", "kognition-autonomyreview-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("autonomyreview").
		For(&k8s.AutonomyReview{}).
		Complete(a)
}
