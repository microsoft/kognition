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

// AgentIdentityReconcilerAdapter wraps the internal AgentIdentityReconciler for
// use with controller-runtime.
type AgentIdentityReconcilerAdapter struct {
	internal *intctrl.AgentIdentityReconciler
	logger   *zap.Logger
	events   eventEmitter
}

// NewAgentIdentityReconcilerAdapter creates an adapter that delegates to the internal AgentIdentityReconciler.
func NewAgentIdentityReconcilerAdapter(s store.Store, logger *zap.Logger) *AgentIdentityReconcilerAdapter {
	return &AgentIdentityReconcilerAdapter{
		internal: intctrl.NewAgentIdentityReconciler(s, logger),
		logger:   logger,
	}
}

func NewAgentIdentityReconcilerAdapterWithWorkloadIdentity(s store.Store, logger *zap.Logger, reader intctrl.WorkloadIdentityStatusReader) *AgentIdentityReconcilerAdapter {
	return &AgentIdentityReconcilerAdapter{
		internal: intctrl.NewAgentIdentityReconcilerWithWorkloadIdentity(s, logger, reader),
		logger:   logger,
	}
}

func (a *AgentIdentityReconcilerAdapter) ConfigureWorkloadIdentityReader(s store.Store, reader intctrl.WorkloadIdentityStatusReader) {
	a.internal = intctrl.NewAgentIdentityReconcilerWithWorkloadIdentity(s, a.logger, reader)
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *AgentIdentityReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "AgentIdentity",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the AgentIdentity reconciler with the controller-runtime manager.
func (a *AgentIdentityReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "AgentIdentity", "kognition-agentidentity-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("agentidentity").
		For(&k8s.AgentIdentity{}).
		Complete(a)
}
