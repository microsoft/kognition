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

// ToolReconcilerAdapter wraps the internal ToolReconciler for use with
// controller-runtime.
type ToolReconcilerAdapter struct {
	internal *intctrl.ToolReconciler
	logger   *zap.Logger
	events   eventEmitter
}

// NewToolReconcilerAdapter creates an adapter that delegates to the internal ToolReconciler.
func NewToolReconcilerAdapter(s store.Store, logger *zap.Logger, options ...intctrl.ToolReconcilerOption) *ToolReconcilerAdapter {
	return &ToolReconcilerAdapter{
		internal: intctrl.NewToolReconciler(s, logger, options...),
		logger:   logger,
	}
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *ToolReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "Tool",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the Tool reconciler with the controller-runtime manager.
func (a *ToolReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "Tool", "kognition-tool-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("tool").
		For(&k8s.Tool{}).
		Complete(a)
}

// ToolClassReconcilerAdapter wraps the internal ToolReconciler for ToolClass resources.
type ToolClassReconcilerAdapter struct {
	internal *intctrl.ToolReconciler
	logger   *zap.Logger
	events   eventEmitter
}

// NewToolClassReconcilerAdapter creates an adapter for ToolClass resources.
func NewToolClassReconcilerAdapter(s store.Store, logger *zap.Logger) *ToolClassReconcilerAdapter {
	return &ToolClassReconcilerAdapter{
		internal: intctrl.NewToolReconciler(s, logger),
		logger:   logger,
	}
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *ToolClassReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "ToolClass",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the ToolClass reconciler with the controller-runtime manager.
func (a *ToolClassReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "ToolClass", "kognition-toolclass-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("toolclass").
		For(&k8s.ToolClass{}).
		Complete(a)
}
