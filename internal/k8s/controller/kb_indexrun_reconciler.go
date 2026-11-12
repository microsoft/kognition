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

// KnowledgeBaseIndexRunReconcilerAdapter wraps the internal
// KnowledgeBaseIndexRunReconciler for controller-runtime and injects the
// Job-based ingester launcher when configured.
type KnowledgeBaseIndexRunReconcilerAdapter struct {
	internal   *intctrl.KnowledgeBaseIndexRunReconciler
	logger     *zap.Logger
	events     eventEmitter
	store      store.Store
	ingesterOp KBIngesterOptions
}

// NewKnowledgeBaseIndexRunReconcilerAdapter creates the adapter.
func NewKnowledgeBaseIndexRunReconcilerAdapter(s store.Store, logger *zap.Logger) *KnowledgeBaseIndexRunReconcilerAdapter {
	return &KnowledgeBaseIndexRunReconcilerAdapter{
		internal: intctrl.NewKnowledgeBaseIndexRunReconciler(s, logger),
		logger:   logger,
		store:    s,
	}
}

// WithIngesterOptions configures the Job-based ingester launcher used to
// execute the run.
func (a *KnowledgeBaseIndexRunReconcilerAdapter) WithIngesterOptions(opts KBIngesterOptions) *KnowledgeBaseIndexRunReconcilerAdapter {
	a.ingesterOp = opts
	return a
}

// Reconcile implements reconcile.Reconciler.
func (a *KnowledgeBaseIndexRunReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "KnowledgeBaseIndexRun",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the reconciler with the manager.
func (a *KnowledgeBaseIndexRunReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "KnowledgeBaseIndexRun", "kognition-knowledgebaseindexrun-controller")
	if a.ingesterOp.Image != "" {
		launcher, err := newConfiguredKBIngesterLauncher(mgr, a.store, a.logger, a.ingesterOp)
		if err != nil {
			return err
		}
		a.internal.WithIndexRunLauncher(launcher)
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("knowledgebaseindexrun").
		For(&k8s.KnowledgeBaseIndexRun{}).
		Complete(a)
}
