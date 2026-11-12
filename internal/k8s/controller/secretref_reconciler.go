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

// SecretRefReconcilerAdapter wraps the internal SecretRefReconciler for
// SecretRef resources under controller-runtime.
type SecretRefReconcilerAdapter struct {
	internal *intctrl.SecretRefReconciler
	logger   *zap.Logger
	events   eventEmitter
}

func NewSecretRefReconcilerAdapter(s store.Store, logger *zap.Logger) *SecretRefReconcilerAdapter {
	return &SecretRefReconcilerAdapter{
		internal: intctrl.NewSecretRefReconciler(s, logger),
		logger:   logger,
	}
}

func (a *SecretRefReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "SecretRef",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

func (a *SecretRefReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "SecretRef", "kognition-secretref-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("secretref").
		For(&k8s.SecretRef{}).
		Complete(a)
}
