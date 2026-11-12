// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	"go.uber.org/zap"
	ctrl "sigs.k8s.io/controller-runtime"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

// AgentInstanceReconcilerAdapter wraps the internal AgentInstanceReconciler for
// use with controller-runtime.
type AgentInstanceReconcilerAdapter struct {
	internal *intctrl.AgentInstanceReconciler
	logger   *zap.Logger
	events   eventEmitter
}

// NewAgentInstanceReconcilerAdapter creates an adapter that delegates to the
// internal AgentInstanceReconciler. If dispatcher is nil, the built-in
// default dispatcher is used (generates a session ID without calling any
// external AI service).
func NewAgentInstanceReconcilerAdapter(s store.Store, dispatcher intctrl.RuntimeDispatcher, logger *zap.Logger) *AgentInstanceReconcilerAdapter {
	var r *intctrl.AgentInstanceReconciler
	if dispatcher != nil {
		r = intctrl.NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)
	} else {
		r = intctrl.NewAgentInstanceReconciler(s, logger)
	}
	return &AgentInstanceReconcilerAdapter{
		internal: r,
		logger:   logger,
	}
}

// SetQuotaEnforcer enables quota accounting on the internal reconciler.
func (a *AgentInstanceReconcilerAdapter) SetQuotaEnforcer(enforcer intctrl.AgentInstanceQuotaEnforcer) *AgentInstanceReconcilerAdapter {
	a.internal.SetQuotaEnforcer(enforcer)
	return a
}

// SetScheduler enables best-effort same-Agent runtime placement.
func (a *AgentInstanceReconcilerAdapter) SetScheduler(scheduler intctrl.CognitiveScheduler) *AgentInstanceReconcilerAdapter {
	a.internal.SetScheduler(scheduler)
	return a
}

// SetAuditLog forwards durable Model evidence recording to the internal reconciler.
func (a *AgentInstanceReconcilerAdapter) SetAuditLog(audit *observability.CognitiveAuditLog) *AgentInstanceReconcilerAdapter {
	a.internal.SetAuditLog(audit)
	return a
}

// SetTrustRecorder enables terminal trust scoring on the internal reconciler.
func (a *AgentInstanceReconcilerAdapter) SetTrustRecorder(recorder intctrl.AgentInstanceTrustRecorder) *AgentInstanceReconcilerAdapter {
	a.internal.SetTrustRecorder(recorder)
	return a
}

// SetModelAuthorizer enables model policy checks on the internal reconciler.
func (a *AgentInstanceReconcilerAdapter) SetModelAuthorizer(authorizer intctrl.AgentInstanceModelAuthorizer) *AgentInstanceReconcilerAdapter {
	a.internal.SetModelAuthorizer(authorizer)
	return a
}

// SetRequireAutomaticIdentity requires a controller-managed AgentIdentity for
// every dispatched instance, including model-only Agents.
func (a *AgentInstanceReconcilerAdapter) SetRequireAutomaticIdentity(required bool) *AgentInstanceReconcilerAdapter {
	a.internal.SetRequireAutomaticIdentity(required)
	return a
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *AgentInstanceReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "AgentInstance",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the AgentInstance reconciler with the controller-runtime manager.
func (a *AgentInstanceReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "AgentInstance", "kognition-agentinstance-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("agentinstance").
		For(&k8s.AgentInstance{}).
		Complete(a)
}
