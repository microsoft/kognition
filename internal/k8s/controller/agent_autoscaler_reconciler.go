// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

type AgentAutoscalerReconcilerAdapter struct {
	internal *intctrl.AgentAutoscalerReconciler
	logger   *zap.Logger
	events   eventEmitter
	client   client.Client
}

func NewAgentAutoscalerReconcilerAdapter(s store.Store, logger *zap.Logger) *AgentAutoscalerReconcilerAdapter {
	return &AgentAutoscalerReconcilerAdapter{internal: intctrl.NewAgentAutoscalerReconciler(s, logger), logger: logger}
}

func (a *AgentAutoscalerReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{Namespace: req.Namespace, Name: req.Name, Kind: "AgentAutoscaler"})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *AgentAutoscalerReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "AgentAutoscaler", "kognition-agentautoscaler-controller")
	a.client = mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).
		Named("agentautoscaler").
		For(&k8s.AgentAutoscaler{}).
		Watches(&k8s.Agent{}, handler.EnqueueRequestsFromMapFunc(a.agentToAutoscalers)).
		Watches(&k8s.AgentInstance{}, handler.EnqueueRequestsFromMapFunc(a.instanceToAutoscalers)).
		Complete(a)
}

func (a *AgentAutoscalerReconcilerAdapter) agentToAutoscalers(ctx context.Context, obj client.Object) []reconcile.Request {
	return a.autoscalersForAgent(ctx, obj.GetNamespace(), obj.GetName())
}

func (a *AgentAutoscalerReconcilerAdapter) instanceToAutoscalers(ctx context.Context, obj client.Object) []reconcile.Request {
	owner := metav1.GetControllerOf(obj)
	if owner == nil || owner.Kind != "Agent" {
		return nil
	}
	return a.autoscalersForAgent(ctx, obj.GetNamespace(), owner.Name)
}

func (a *AgentAutoscalerReconcilerAdapter) autoscalersForAgent(ctx context.Context, namespace, agentName string) []reconcile.Request {
	var autoscalers k8s.AgentAutoscalerList
	if err := a.client.List(ctx, &autoscalers, client.InNamespace(namespace)); err != nil {
		a.logger.Error("list agent autoscalers for dependency watch", zap.Error(err))
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range autoscalers.Items {
		if autoscalers.Items[i].Spec.AgentRef.Name == agentName {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&autoscalers.Items[i])})
		}
	}
	return requests
}
