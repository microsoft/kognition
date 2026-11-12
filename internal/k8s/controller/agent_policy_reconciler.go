// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	"go.uber.org/zap"
	ctrl "sigs.k8s.io/controller-runtime"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

// AgentPolicyReconcilerAdapter wraps the internal AgentPolicyReconciler for use
// with controller-runtime.
type AgentPolicyReconcilerAdapter struct {
	internal *intctrl.AgentPolicyReconciler
	logger   *zap.Logger
	events   eventEmitter
}

// NewAgentPolicyReconcilerAdapter creates an adapter that delegates to the internal AgentPolicyReconciler.
func NewAgentPolicyReconcilerAdapter(s store.Store, engine *opa.Engine, logger *zap.Logger) *AgentPolicyReconcilerAdapter {
	return &AgentPolicyReconcilerAdapter{
		internal: intctrl.NewAgentPolicyReconciler(s, engine, logger),
		logger:   logger,
	}
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *AgentPolicyReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "AgentPolicy",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the AgentPolicy reconciler with the controller-runtime manager.
func (a *AgentPolicyReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "AgentPolicy", "kognition-agentpolicy-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("agentpolicy").
		For(&k8s.AgentPolicy{}).
		Complete(a)
}
