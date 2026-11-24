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

// WorkflowReconcilerAdapter wraps the internal WorkflowReconciler for use with
// controller-runtime.
type WorkflowReconcilerAdapter struct {
	internal *intctrl.WorkflowReconciler
	logger   *zap.Logger
	events   eventEmitter
}

// NewWorkflowReconcilerAdapter creates an adapter that delegates to the internal WorkflowReconciler.
func NewWorkflowReconcilerAdapter(s store.Store, logger *zap.Logger) *WorkflowReconcilerAdapter {
	return &WorkflowReconcilerAdapter{
		internal: intctrl.NewWorkflowReconciler(s, logger),
		logger:   logger,
	}
}

// SetToolInvoker forwards the tool invoker to the wrapped internal reconciler so
// that tool-typed workflow steps can execute synchronously.
func (a *WorkflowReconcilerAdapter) SetToolInvoker(invoker intctrl.WorkflowToolInvoker) {
	a.internal.SetToolInvoker(invoker)
}

// SetCommunicationPolicyAuthorizer forwards agent-to-agent workflow policy
// enforcement to the shared execution engine.
func (a *WorkflowReconcilerAdapter) SetCommunicationPolicyAuthorizer(authorizer intctrl.CommunicationPolicyAuthorizer) {
	a.internal.SetCommunicationPolicyAuthorizer(authorizer)
}

// SetAuditLog forwards durable communication evidence recording to the shared execution engine.
func (a *WorkflowReconcilerAdapter) SetAuditLog(audit *observability.CognitiveAuditLog) {
	a.internal.SetAuditLog(audit)
}

// SetCollaborationRunner forwards the governed collaboration runner to the wrapped internal reconciler.
func (a *WorkflowReconcilerAdapter) SetCollaborationRunner(runner intctrl.WorkflowCollaborationRunner) {
	a.internal.SetCollaborationRunner(runner)
}

// SetTracing forwards workflow execution tracing to the wrapped internal reconciler.
func (a *WorkflowReconcilerAdapter) SetTracing(tracer *observability.Tracer, collector *observability.Collector) {
	a.internal.SetTracing(tracer, collector)
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *WorkflowReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "Workflow",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the Workflow template reconciler with the manager.
func (a *WorkflowReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "Workflow", "kognition-workflow-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("workflow").
		For(&k8s.Workflow{}).
		Complete(a)
}
