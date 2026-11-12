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

// KnowledgeBaseIndexScheduleReconcilerAdapter wraps the internal
// KnowledgeBaseIndexScheduleReconciler for controller-runtime.
type KnowledgeBaseIndexScheduleReconcilerAdapter struct {
	internal *intctrl.KnowledgeBaseIndexScheduleReconciler
	logger   *zap.Logger
	events   eventEmitter
	store    store.Store
}

// NewKnowledgeBaseIndexScheduleReconcilerAdapter creates the adapter.
func NewKnowledgeBaseIndexScheduleReconcilerAdapter(s store.Store, logger *zap.Logger) *KnowledgeBaseIndexScheduleReconcilerAdapter {
	return &KnowledgeBaseIndexScheduleReconcilerAdapter{
		internal: intctrl.NewKnowledgeBaseIndexScheduleReconciler(s, logger),
		logger:   logger,
		store:    s,
	}
}

// Reconcile implements reconcile.Reconciler.
func (a *KnowledgeBaseIndexScheduleReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "KnowledgeBaseIndexSchedule",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the reconciler with the manager.
func (a *KnowledgeBaseIndexScheduleReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "KnowledgeBaseIndexSchedule", "kognition-knowledgebaseindexschedule-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("knowledgebaseindexschedule").
		For(&k8s.KnowledgeBaseIndexSchedule{}).
		Complete(a)
}
