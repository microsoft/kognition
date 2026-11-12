// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	"go.uber.org/zap"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

// ModelReconcilerAdapter wraps the internal ModelReconciler for Model
// resources under controller-runtime.
type ModelReconcilerAdapter struct {
	internal *intctrl.ModelReconciler
	logger   *zap.Logger
	events   eventEmitter
}

// NewModelReconcilerAdapter creates an adapter that delegates to the internal ModelReconciler.
func NewModelReconcilerAdapter(s store.Store, registry *cpi.Registry, logger *zap.Logger) *ModelReconcilerAdapter {
	return &ModelReconcilerAdapter{
		internal: intctrl.NewModelReconciler(s, registry, logger),
		logger:   logger,
	}
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *ModelReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "Model",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the Model reconciler with the controller-runtime manager.
func (a *ModelReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "Model", "kognition-model-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("model").
		For(&k8s.Model{}).
		Watches(&k8s.ModelInvocationProfile{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
			var models k8s.ModelList
			if err := mgr.GetClient().List(ctx, &models, client.InNamespace(object.GetNamespace())); err != nil {
				a.logger.Error("list Models referencing invocation profile", zap.Error(err))
				return nil
			}
			var requests []reconcile.Request
			for _, model := range models.Items {
				if model.Spec.InvocationProfileRef != nil && model.Spec.InvocationProfileRef.Name == object.GetName() {
					requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&model)})
				}
			}
			return requests
		})).
		Complete(a)
}

// ModelClassReconcilerAdapter wraps the internal ModelClassReconciler for ModelClass
// resources under controller-runtime.
type ModelClassReconcilerAdapter struct {
	internal *intctrl.ModelClassReconciler
	logger   *zap.Logger
	events   eventEmitter
}

func (a *ModelClassReconcilerAdapter) SetCanaryAccounting(ledger store.ModelDispatchStore, usage observability.UsageEventWriter, audit *observability.CognitiveAuditLog) *ModelClassReconcilerAdapter {
	a.internal.SetCanaryAccounting(ledger, usage, audit)
	return a
}

// NewModelClassReconcilerAdapter creates an adapter that delegates to the internal ModelClassReconciler.
func NewModelClassReconcilerAdapter(s store.Store, registry *cpi.Registry, logger *zap.Logger) *ModelClassReconcilerAdapter {
	return &ModelClassReconcilerAdapter{
		internal: intctrl.NewModelClassReconciler(s, registry, logger),
		logger:   logger,
	}
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *ModelClassReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "ModelClass",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the ModelClass reconciler with the controller-runtime manager.
func (a *ModelClassReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "ModelClass", "kognition-modelclass-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("modelclass").
		For(&k8s.ModelClass{}).
		Complete(a)
}
