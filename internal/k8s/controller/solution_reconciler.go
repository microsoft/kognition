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

type SolutionReconcilerAdapter struct {
	internal *intctrl.SolutionReconciler
	logger   *zap.Logger
	events   eventEmitter
	client   client.Client
}

func NewSolutionReconcilerAdapter(s store.Store, logger *zap.Logger) *SolutionReconcilerAdapter {
	return &SolutionReconcilerAdapter{internal: intctrl.NewSolutionReconciler(s, logger), logger: logger}
}

func (a *SolutionReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{Namespace: req.Namespace, Name: req.Name, Kind: "Solution"})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *SolutionReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "Solution", "kognition-solution-controller")
	a.client = mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).
		Named("solution").
		For(&k8s.Solution{}).
		Owns(&k8s.EvaluationRun{}).
		Watches(&k8s.Evaluation{}, handler.EnqueueRequestsFromMapFunc(a.evaluationToSolutions)).
		Watches(&k8s.Agent{}, handler.EnqueueRequestsFromMapFunc(a.agentToSolution)).
		Watches(&k8s.CognitiveQuota{}, handler.EnqueueRequestsFromMapFunc(a.quotaToSolutions)).
		Complete(a)
}

func (a *SolutionReconcilerAdapter) evaluationToSolutions(ctx context.Context, obj client.Object) []reconcile.Request {
	evaluation, ok := obj.(*k8s.Evaluation)
	if !ok || evaluation == nil {
		return nil
	}
	return listNamespacedRequests(ctx, a.client, a.logger, obj.GetNamespace(), &k8s.SolutionList{}, func(list client.ObjectList) []client.Object {
		solutions, ok := list.(*k8s.SolutionList)
		if !ok || solutions == nil {
			return nil
		}
		objects := make([]client.Object, 0)
		for i := range solutions.Items {
			for _, gate := range solutions.Items[i].Spec.PromotionGates {
				if gate.EvaluationRef.Name == evaluation.Name {
					objects = append(objects, &solutions.Items[i])
					break
				}
			}
		}
		return objects
	})
}

func (a *SolutionReconcilerAdapter) agentToSolution(_ context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetLabels()["kognition.dev/solution"]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: obj.GetNamespace(), Name: name}}}
}

func (a *SolutionReconcilerAdapter) quotaToSolutions(ctx context.Context, obj client.Object) []reconcile.Request {
	return listNamespacedRequests(ctx, a.client, a.logger, obj.GetNamespace(), &k8s.SolutionList{}, func(list client.ObjectList) []client.Object {
		solutionList, ok := list.(*k8s.SolutionList)
		if !ok || solutionList == nil {
			return nil
		}
		items := solutionList.Items
		objects := make([]client.Object, len(items))
		for i := range items {
			objects[i] = &items[i]
		}
		return objects
	})
}
