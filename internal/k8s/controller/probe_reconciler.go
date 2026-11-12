// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/requestid"
)

// ProbeReconcilerAdapter schedules internal AgentInstance probe reconciliation.
type ProbeReconcilerAdapter struct {
	internal *intctrl.ProbeReconciler
}

// NewProbeReconcilerAdapter wraps an internal probe reconciler for controller-runtime.
func NewProbeReconcilerAdapter(reconciler *intctrl.ProbeReconciler) *ProbeReconcilerAdapter {
	return &ProbeReconcilerAdapter{internal: reconciler}
}

func (a *ProbeReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "AgentInstance",
	})
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

// SetupWithManager registers probes independently of lifecycle reconciliation.
func (a *ProbeReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("agentinstance-probes").
		For(&k8s.AgentInstance{}).
		Complete(a)
}
