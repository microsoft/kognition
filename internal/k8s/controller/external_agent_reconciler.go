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

// ExternalAgentReconcilerAdapter wraps the internal ExternalAgentReconciler for controller-runtime.
type ExternalAgentReconcilerAdapter struct {
	internal *intctrl.ExternalAgentReconciler
	events   eventEmitter
}

// NewExternalAgentReconcilerAdapter creates the adapter.
func NewExternalAgentReconcilerAdapter(s store.Store, logger *zap.Logger, opts ...intctrl.ExternalAgentReconcilerOption) *ExternalAgentReconcilerAdapter {
	return &ExternalAgentReconcilerAdapter{internal: intctrl.NewExternalAgentReconciler(s, logger, opts...)}
}

func (a *ExternalAgentReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{Namespace: req.Namespace, Name: req.Name, Kind: "ExternalAgent"})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *ExternalAgentReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "ExternalAgent", "kognition-externalagent-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("externalagent").
		For(&k8s.ExternalAgent{}).
		Complete(a)
}
