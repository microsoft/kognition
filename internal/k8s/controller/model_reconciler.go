// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"strings"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

// ModelReconcilerAdapter wraps the internal ModelReconciler for Model
// resources under controller-runtime.
type ModelReconcilerAdapter struct {
	internal *intctrl.ModelReconciler
	logger   *zap.Logger
	events   eventEmitter
	// healthEvents carries Models whose passive health changed, from the health
	// watcher to the controller's work queue.
	healthEvents chan event.GenericEvent
}

// NewModelReconcilerAdapter creates an adapter that delegates to the internal ModelReconciler.
func NewModelReconcilerAdapter(s store.Store, registry *cpi.Registry, logger *zap.Logger) *ModelReconcilerAdapter {
	return &ModelReconcilerAdapter{
		internal:     intctrl.NewModelReconciler(s, registry, logger),
		logger:       logger,
		healthEvents: make(chan event.GenericEvent, healthEventBuffer),
	}
}

// healthEventBuffer is how many queued Models the adapter holds for the work
// queue. Overflow is dropped: the watcher queues a Model again on its next check
// for as long as its status does not match the evidence.
const healthEventBuffer = 256

// ConfigureCallHealth enables passive model health from a store shared with the
// processes that make model calls.
func (a *ModelReconcilerAdapter) ConfigureCallHealth(health store.ModelCallHealthStore) {
	a.internal.SetCallHealth(health)
}

// Enqueue queues the Model identified by a "namespace/name" key for
// reconciliation. It never blocks and ignores malformed keys.
func (a *ModelReconcilerAdapter) Enqueue(key string) {
	namespace, name, found := strings.Cut(key, "/")
	if !found || namespace == "" || name == "" {
		return
	}
	select {
	case a.healthEvents <- event.GenericEvent{Object: &k8s.Model{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}}:
	default:
	}
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *ModelReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "Model",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the Model reconciler with the controller-runtime manager.
func (a *ModelReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "Model", "kognition-model-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("model").
		For(&k8s.Model{}).
		WatchesRawSource(source.Channel(a.healthEvents, &handler.EnqueueRequestForObject{})).
		Watches(&k8s.ModelInvocationProfile{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
			var models k8s.ModelList
			if err := mgr.GetClient().List(ctx, &models, client.InNamespace(object.GetNamespace())); err != nil {
				a.logger.Error("list Models referencing invocation profile", zap.Error(err))
				return nil
			}
			var requests []reconcile.Request
			for _, model := range models.Items {
				if model.Spec.InvocationProfileRef != nil && model.Spec.InvocationProfileRef.Name == object.GetName() {
					requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&model)})
				}
			}
			return requests
		})).
		Complete(a)
}

// ModelClassReconcilerAdapter wraps the internal ModelClassReconciler for ModelClass
// resources under controller-runtime.
type ModelClassReconcilerAdapter struct {
	internal *intctrl.ModelClassReconciler
	logger   *zap.Logger
	events   eventEmitter
}

func (a *ModelClassReconcilerAdapter) SetCanaryAccounting(ledger store.ModelDispatchStore, usage observability.UsageEventWriter, audit *observability.CognitiveAuditLog) *ModelClassReconcilerAdapter {
	a.internal.SetCanaryAccounting(ledger, usage, audit)
	return a
}

// NewModelClassReconcilerAdapter creates an adapter that delegates to the internal ModelClassReconciler.
func NewModelClassReconcilerAdapter(s store.Store, registry *cpi.Registry, logger *zap.Logger) *ModelClassReconcilerAdapter {
	return &ModelClassReconcilerAdapter{
		internal: intctrl.NewModelClassReconciler(s, registry, logger),
		logger:   logger,
	}
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *ModelClassReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "ModelClass",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the ModelClass reconciler with the controller-runtime manager.
func (a *ModelClassReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "ModelClass", "kognition-modelclass-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("modelclass").
		For(&k8s.ModelClass{}).
		Complete(a)
}
