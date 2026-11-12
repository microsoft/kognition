// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

// WorkflowRunReconcilerAdapter wraps the internal WorkflowRunReconciler for use with
// controller-runtime.
type WorkflowRunReconcilerAdapter struct {
	internal *intctrl.WorkflowRunReconciler
	logger   *zap.Logger
	events   eventEmitter
}

// NewWorkflowRunReconcilerAdapter creates an adapter that delegates to the internal WorkflowRunReconciler.
func NewWorkflowRunReconcilerAdapter(s store.Store, logger *zap.Logger) *WorkflowRunReconcilerAdapter {
	return &WorkflowRunReconcilerAdapter{
		internal: intctrl.NewWorkflowRunReconciler(s, logger),
		logger:   logger,
	}
}

// SetToolInvoker forwards the tool invoker to the wrapped internal reconciler so
// that tool-typed workflow steps can execute synchronously.
func (a *WorkflowRunReconcilerAdapter) SetToolInvoker(invoker intctrl.WorkflowToolInvoker) {
	a.internal.SetToolInvoker(invoker)
}

// SetCommunicationPolicyAuthorizer forwards communication policy enforcement
// to the shared WorkflowRun execution engine.
func (a *WorkflowRunReconcilerAdapter) SetCommunicationPolicyAuthorizer(authorizer intctrl.CommunicationPolicyAuthorizer) {
	a.internal.SetCommunicationPolicyAuthorizer(authorizer)
}

// SetAuditLog forwards durable communication evidence recording to the shared execution engine.
func (a *WorkflowRunReconcilerAdapter) SetAuditLog(audit *observability.CognitiveAuditLog) {
	a.internal.SetAuditLog(audit)
}

// SetTracing forwards WorkflowRun tracing to the wrapped internal reconciler.
func (a *WorkflowRunReconcilerAdapter) SetTracing(tracer *observability.Tracer, collector *observability.Collector) {
	a.internal.SetTracing(tracer, collector)
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *WorkflowRunReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "WorkflowRun",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers WorkflowRun execution and watches approval decisions.
func (a *WorkflowRunReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "WorkflowRun", "kognition-workflowrun-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("workflowrun").
		For(&k8s.WorkflowRun{}).
		Owns(&k8s.AgentJob{}).
		Owns(&k8s.MemoryClaim{}).
		Watches(&k8s.ApprovalDecision{}, handler.EnqueueRequestsFromMapFunc(approvalDecisionToWorkflowRuns)).
		Complete(a)
}

func approvalDecisionToWorkflowRuns(_ context.Context, obj client.Object) []reconcile.Request {
	decision, ok := obj.(*k8s.ApprovalDecision)
	if !ok || decision.Spec.WorkflowRunRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: decision.Namespace,
		Name:      decision.Spec.WorkflowRunRef.Name,
	}}}
}
