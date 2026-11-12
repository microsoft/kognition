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

// WorkspaceReconcilerAdapter wraps the internal WorkspaceReconciler for use
// with controller-runtime.
type WorkspaceReconcilerAdapter struct {
	internal *intctrl.WorkspaceReconciler
	logger   *zap.Logger
	events   eventEmitter
}

// NewWorkspaceReconcilerAdapter creates an adapter that delegates to the internal WorkspaceReconciler.
func NewWorkspaceReconcilerAdapter(s store.Store, logger *zap.Logger) *WorkspaceReconcilerAdapter {
	return &WorkspaceReconcilerAdapter{
		internal: intctrl.NewWorkspaceReconciler(s, logger),
		logger:   logger,
	}
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *WorkspaceReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "Workspace",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the Workspace reconciler with the controller-runtime manager.
func (a *WorkspaceReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "Workspace", "kognition-workspace-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("workspace").
		For(&k8s.Workspace{}).
		Complete(a)
}
