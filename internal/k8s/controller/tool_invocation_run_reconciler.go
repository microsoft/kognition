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

// ToolInvocationRunReconcilerAdapter connects durable stdio invocation
// requests to the shared controller reconciliation framework.
type ToolInvocationRunReconcilerAdapter struct {
	internal *intctrl.ToolInvocationRunReconciler
	logger   *zap.Logger
	events   eventEmitter
}

func NewToolInvocationRunReconcilerAdapter(s store.Store, logger *zap.Logger, options ...intctrl.ToolInvocationRunReconcilerOption) *ToolInvocationRunReconcilerAdapter {
	return &ToolInvocationRunReconcilerAdapter{internal: intctrl.NewToolInvocationRunReconciler(s, logger, options...), logger: logger}
}

func (a *ToolInvocationRunReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{Namespace: req.Namespace, Name: req.Name, Kind: "ToolInvocationRun"})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *ToolInvocationRunReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "ToolInvocationRun", "kognition-toolinvocationrun-controller")
	return ctrl.NewControllerManagedBy(mgr).Named("toolinvocationrun").For(&k8s.ToolInvocationRun{}).Complete(a)
}
