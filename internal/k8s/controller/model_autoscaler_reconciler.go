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
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

// ModelAutoscalerReconcilerAdapter wraps the internal ModelAutoscalerReconciler
// for ModelAutoscaler resources under controller-runtime.
type ModelAutoscalerReconcilerAdapter struct {
	internal *intctrl.ModelAutoscalerReconciler
	logger   *zap.Logger
	events   eventEmitter
	client   client.Client
}

// NewModelAutoscalerReconcilerAdapter creates an adapter that delegates to the internal ModelAutoscalerReconciler.
func NewModelAutoscalerReconcilerAdapter(s store.Store, logger *zap.Logger) *ModelAutoscalerReconcilerAdapter {
	return &ModelAutoscalerReconcilerAdapter{
		internal: intctrl.NewModelAutoscalerReconciler(s, logger),
		logger:   logger,
	}
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *ModelAutoscalerReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "ModelAutoscaler",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the ModelAutoscaler reconciler with the controller-runtime manager.
func (a *ModelAutoscalerReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "ModelAutoscaler", "kognition-modelautoscaler-controller")
	a.client = mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).
		Named("modelautoscaler").
		For(&k8s.ModelAutoscaler{}).
		Watches(&k8s.Agent{}, handler.EnqueueRequestsFromMapFunc(a.agentToAutoscalers)).
		Watches(&k8s.Model{}, handler.EnqueueRequestsFromMapFunc(a.namespacedDependencyToAutoscalers)).
		Watches(&k8s.ModelClass{}, handler.EnqueueRequestsFromMapFunc(a.modelClassToAutoscalers)).
		Complete(a)
}

func (a *ModelAutoscalerReconcilerAdapter) agentToAutoscalers(ctx context.Context, obj client.Object) []reconcile.Request {
	var autoscalers k8s.ModelAutoscalerList
	if err := a.client.List(ctx, &autoscalers, client.InNamespace(obj.GetNamespace())); err != nil {
		a.logger.Error("list model autoscalers for agent watch", zap.Error(err))
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range autoscalers.Items {
		if autoscalers.Items[i].Spec.AgentRef.Name == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&autoscalers.Items[i])})
		}
	}
	return requests
}

func (a *ModelAutoscalerReconcilerAdapter) namespacedDependencyToAutoscalers(ctx context.Context, obj client.Object) []reconcile.Request {
	return a.listAutoscalerRequests(ctx, client.InNamespace(obj.GetNamespace()))
}

func (a *ModelAutoscalerReconcilerAdapter) modelClassToAutoscalers(ctx context.Context, _ client.Object) []reconcile.Request {
	return a.listAutoscalerRequests(ctx)
}

func (a *ModelAutoscalerReconcilerAdapter) listAutoscalerRequests(ctx context.Context, opts ...client.ListOption) []reconcile.Request {
	var autoscalers k8s.ModelAutoscalerList
	if err := a.client.List(ctx, &autoscalers, opts...); err != nil {
		a.logger.Error("list model autoscalers for dependency watch", zap.Error(err))
		return nil
	}
	requests := make([]reconcile.Request, 0, len(autoscalers.Items))
	for i := range autoscalers.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&autoscalers.Items[i])})
	}
	return requests
}
