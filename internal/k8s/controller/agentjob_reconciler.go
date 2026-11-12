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

// AgentJobReconcilerAdapter wraps the internal AgentJobReconciler for use with
// controller-runtime.
type AgentJobReconcilerAdapter struct {
	internal *intctrl.AgentJobReconciler
	logger   *zap.Logger
	events   eventEmitter
}

// NewAgentJobReconcilerAdapter creates an adapter that delegates to the internal AgentJobReconciler.
func NewAgentJobReconcilerAdapter(s store.Store, logger *zap.Logger) *AgentJobReconcilerAdapter {
	return &AgentJobReconcilerAdapter{
		internal: intctrl.NewAgentJobReconciler(s, logger),
		logger:   logger,
	}
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *AgentJobReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "AgentJob",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the AgentJob reconciler with the controller-runtime manager.
func (a *AgentJobReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "AgentJob", "kognition-agentjob-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("agentjob").
		For(&k8s.AgentJob{}).
		Owns(&k8s.AgentInstance{}).
		Complete(a)
}
