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

type PromptReconcilerAdapter struct {
	internal *intctrl.PromptReconciler
	events   eventEmitter
}

func NewPromptReconcilerAdapter(s store.Store, logger *zap.Logger) *PromptReconcilerAdapter {
	return &PromptReconcilerAdapter{internal: intctrl.NewPromptReconciler(s, logger)}
}

func (a *PromptReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{Namespace: req.Namespace, Name: req.Name, Kind: "Prompt"})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *PromptReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "Prompt", "kognition-prompt-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("prompt").
		For(&k8s.Prompt{}).
		Complete(a)
}
