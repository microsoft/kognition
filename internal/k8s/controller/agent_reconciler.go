// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/k8s/dispatcher"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

// AgentReconcilerAdapter wraps the internal AgentReconciler for use with
// controller-runtime. It translates ctrl.Request into internal Request and
// delegates reconciliation to the internal reconciler that operates on the store.
type AgentReconcilerAdapter struct {
	internal   *intctrl.AgentReconciler
	store      store.Store
	dispatcher *dispatcher.K8sDispatcher
	logger     *zap.Logger
	events     eventEmitter
}

// NewAgentReconcilerAdapter creates an adapter that delegates to the internal AgentReconciler.
// The dispatcher is used as a DeploymentCleaner during Agent deletion.
func NewAgentReconcilerAdapter(s store.Store, d *dispatcher.K8sDispatcher, logger *zap.Logger) *AgentReconcilerAdapter {
	var opts []intctrl.AgentReconcilerOption
	if d != nil {
		opts = append(opts, intctrl.WithDeploymentCleaner(d))
	}
	return &AgentReconcilerAdapter{
		internal:   intctrl.NewAgentReconciler(s, logger, opts...),
		store:      s,
		dispatcher: d,
		logger:     logger,
	}
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *AgentReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "Agent",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	if err != nil {
		return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
	}

	// Ensure owner annotations on cross-namespace Deployment/Service.
	if a.dispatcher != nil {
		if agent, loadErr := loadAgentForRuntimeRefresh(ctx, a.store, req.Namespace, req.Name); loadErr != nil {
			a.logger.Warn("failed to load agent for runtime refresh", zap.Error(loadErr))
		} else if agent != nil {
			if runtimeErr := a.dispatcher.EnsureAgentRuntime(ctx, agent); runtimeErr != nil {
				a.logger.Warn("failed to ensure agent runtime", zap.Error(runtimeErr))
			} else if agent.DeletionTimestamp == nil {
				if err := a.refreshRuntimeStatus(ctx, agent); err != nil {
					return ctrl.Result{}, err
				}
			}
		} else if annErr := a.dispatcher.EnsureOwnerAnnotations(ctx, req.Name, req.Namespace); annErr != nil {
			a.logger.Warn("failed to ensure owner annotations", zap.Error(annErr))
		}
	}

	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, nil
}

func (a *AgentReconcilerAdapter) refreshRuntimeStatus(ctx context.Context, agent *api.Agent) error {
	status, err := a.dispatcher.ReadAgentRuntimeStatus(ctx, agent)
	if err != nil {
		return err
	}
	if agent.Status.Runtime == nil && status == nil ||
		agent.Status.Runtime != nil && status != nil && *agent.Status.Runtime == *status {
		return nil
	}
	agent.Status.Runtime = status
	data, err := json.Marshal(agent)
	if err != nil {
		return fmt.Errorf("encode Agent runtime status: %w", err)
	}
	if err := a.store.Update(ctx, "Agent", agent.Namespace, agent.Name, data, agent.ResourceVersion); err != nil {
		return fmt.Errorf("update Agent runtime status: %w", err)
	}
	return nil
}

// SetupWithManager registers the Agent reconciler with the controller-runtime manager.
// It watches Agent CRDs and also watches Deployments annotated with the owner-agent
// annotation to detect drift and trigger re-reconciliation.
func (a *AgentReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "Agent", "kognition-agent-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("agent").
		For(&k8s.Agent{}).
		Owns(&k8s.AgentInstance{}).
		Watches(&k8s.AgentIdentity{}, handler.EnqueueRequestsFromMapFunc(a.identityToAgent)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				annotations := obj.GetAnnotations()
				if annotations == nil {
					return nil
				}
				ownerRef, ok := annotations[dispatcher.AnnotationOwnerAgent]
				if !ok {
					return nil
				}
				// ownerRef format: "namespace/name"
				parts := splitOwnerRef(ownerRef)
				if parts == nil {
					return nil
				}
				return []reconcile.Request{
					{NamespacedName: types.NamespacedName{Namespace: parts[0], Name: parts[1]}},
				}
			},
		)).
		Complete(a)
}

func (a *AgentReconcilerAdapter) identityToAgent(_ context.Context, object client.Object) []reconcile.Request {
	identity, ok := object.(*k8s.AgentIdentity)
	if !ok || identity.Spec.AgentRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: identity.Namespace, Name: identity.Spec.AgentRef.Name}}}
}

func loadAgentForRuntimeRefresh(ctx context.Context, st store.Store, namespace, name string) (*api.Agent, error) {
	if st == nil {
		return nil, nil
	}
	data, err := st.Get(ctx, "Agent", namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var agent api.Agent
	if err := json.Unmarshal(data, &agent); err != nil {
		return nil, err
	}
	return &agent, nil
}

// splitOwnerRef splits a "namespace/name" string into its parts.
func splitOwnerRef(ref string) []string {
	parts := strings.Split(ref, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil
	}
	return parts
}

// Ensure interface compliance.
var _ interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
} = (*AgentReconcilerAdapter)(nil)
