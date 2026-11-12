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

type EvaluationReconcilerAdapter struct {
	internal *intctrl.EvaluationReconciler
	logger   *zap.Logger
	events   eventEmitter
}

func NewEvaluationReconcilerAdapter(s store.Store, logger *zap.Logger) *EvaluationReconcilerAdapter {
	return &EvaluationReconcilerAdapter{internal: intctrl.NewEvaluationReconciler(s, logger), logger: logger}
}

func (a *EvaluationReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{Namespace: req.Namespace, Name: req.Name, Kind: "Evaluation"})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *EvaluationReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "Evaluation", "kognition-evaluation-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("evaluation").
		For(&k8s.Evaluation{}).
		Owns(&k8s.EvaluationRun{}).
		Complete(a)
}

type EvaluationRunReconcilerAdapter struct {
	internal *intctrl.EvaluationRunReconciler
	logger   *zap.Logger
	events   eventEmitter
}

func NewEvaluationRunReconcilerAdapter(s store.Store, logger *zap.Logger) *EvaluationRunReconcilerAdapter {
	return &EvaluationRunReconcilerAdapter{internal: intctrl.NewEvaluationRunReconciler(s, logger), logger: logger}
}

func (a *EvaluationRunReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{Namespace: req.Namespace, Name: req.Name, Kind: "EvaluationRun"})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *EvaluationRunReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "EvaluationRun", "kognition-evaluationrun-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("evaluationrun").
		For(&k8s.EvaluationRun{}).
		Owns(&k8s.AgentJob{}).
		Complete(a)
}
