// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// AgentServiceFinalizer is the finalizer applied to AgentService resources.
	AgentServiceFinalizer = "kognition.dev/agent-service-cleanup"

	// AgentServiceExecutionAnnotation identifies AgentJob and AgentInstance
	// executions that must invoke a specific ready AgentService endpoint.
	AgentServiceExecutionAnnotation = "kognition.dev/agent-service"

	// agentServiceKind is the kind string for AgentService resources.
	agentServiceKind = "AgentService"
)

// AgentServiceReconciler reconciles AgentService resources by resolving the
// referenced Agent, selecting the active revision, and computing the service endpoint.
type AgentServiceReconciler struct {
	store     store.Store
	publisher AgentServiceEndpointPublisher
	logger    *zap.Logger
}

// AgentServiceDependencyHandler requeues standalone AgentServices when their
// referenced Agent changes or when an AgentInstance changes. Delete events for
// AgentInstances contain only a key, so every service in that namespace is
// re-evaluated to avoid retaining stale readiness.
type AgentServiceDependencyHandler struct {
	store          store.Store
	dependencyKind string
	target         EventHandler
	logger         *zap.Logger
}

// NewAgentServiceDependencyHandler creates an event handler for an Agent or
// AgentInstance dependency.
func NewAgentServiceDependencyHandler(st store.Store, dependencyKind string, target EventHandler, logger *zap.Logger) *AgentServiceDependencyHandler {
	return &AgentServiceDependencyHandler{
		store:          st,
		dependencyKind: dependencyKind,
		target:         target,
		logger:         logger.Named("agent-service-dependency"),
	}
}

func (h *AgentServiceDependencyHandler) OnAdd(key string)    { h.enqueueDependents(key) }
func (h *AgentServiceDependencyHandler) OnUpdate(key string) { h.enqueueDependents(key) }
func (h *AgentServiceDependencyHandler) OnDelete(key string) { h.enqueueDependents(key) }

func (h *AgentServiceDependencyHandler) enqueueDependents(key string) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return
	}
	namespace, dependencyName := parts[0], parts[1]
	rawServices, err := h.store.List(context.Background(), agentServiceKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		h.logger.Error("list agent services for dependency event", zap.Error(err))
		return
	}
	for _, raw := range rawServices {
		var service v1alpha1.AgentService
		if err := json.Unmarshal(raw, &service); err != nil {
			continue
		}
		if h.dependencyKind == "Agent" && service.Spec.AgentRef.Name != dependencyName {
			continue
		}
		h.target.OnUpdate(KeyFunc(namespace, service.ObjectMeta.Name))
	}
}

// AgentServiceEndpointPublisher materializes and cleans a callable service endpoint.
type AgentServiceEndpointPublisher interface {
	Ensure(context.Context, *v1alpha1.AgentService, *v1alpha1.Agent) (AgentServiceEndpoint, error)
	Delete(context.Context, *v1alpha1.AgentService) error
}

// AgentServiceEndpoint describes the published endpoint and, when the
// publisher owns its runtime workload, its observed ready replica count.
type AgentServiceEndpoint struct {
	URL            string
	ReadyInstances *int32
}

// AgentServiceReconcilerOption configures AgentService reconciliation.
type AgentServiceReconcilerOption func(*AgentServiceReconciler)

// WithAgentServiceEndpointPublisher configures endpoint materialization.
func WithAgentServiceEndpointPublisher(publisher AgentServiceEndpointPublisher) AgentServiceReconcilerOption {
	return func(reconciler *AgentServiceReconciler) {
		reconciler.publisher = publisher
	}
}

// NewAgentServiceReconciler creates a new AgentServiceReconciler.
func NewAgentServiceReconciler(s store.Store, logger *zap.Logger, opts ...AgentServiceReconcilerOption) *AgentServiceReconciler {
	reconciler := &AgentServiceReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "agent-service")),
	}
	for _, opt := range opts {
		opt(reconciler)
	}
	return reconciler
}

// Reconcile implements the Reconciler interface for AgentService resources.
func (r *AgentServiceReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	// Fetch the AgentService from the store.
	raw, err := r.store.Get(ctx, agentServiceKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("agent service not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get agent service: %w", err)
	}

	var svc v1alpha1.AgentService
	if err := json.Unmarshal(raw, &svc); err != nil {
		return Result{}, fmt.Errorf("unmarshal agent service: %w", err)
	}

	// Handle deletion: remove finalizer.
	if svc.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &svc, log)
	}

	// Ensure finalizer is present.
	if !lifecycle.HasFinalizer(svc.ObjectMeta.Finalizers, AgentServiceFinalizer) {
		finalizers, _ := lifecycle.AddFinalizer(svc.ObjectMeta.Finalizers, AgentServiceFinalizer)
		svc.ObjectMeta.Finalizers = finalizers
		if err := r.updateService(ctx, &svc); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		// Re-enqueue so the next reconcile works with the updated resourceVersion.
		return Result{Requeue: true}, nil
	}

	// Resolve the referenced Agent.
	agent, err := r.resolveAgent(ctx, &svc)
	if err != nil {
		log.Warn("failed to resolve agent", zap.Error(err))
		if cleanupErr := r.withdrawEndpoint(ctx, &svc); cleanupErr != nil {
			return Result{}, fmt.Errorf("withdraw endpoint for unavailable agent: %w", cleanupErr)
		}
		if err := r.setNotReady(ctx, &svc, "AgentNotFound", err.Error()); err != nil {
			return Result{}, err
		}
		return Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Determine active revision (pinned or latest).
	activeRevision, err := r.resolveRevision(ctx, &svc, agent)
	if err != nil {
		log.Warn("failed to resolve revision", zap.Error(err))
		if cleanupErr := r.withdrawEndpoint(ctx, &svc); cleanupErr != nil {
			return Result{}, fmt.Errorf("withdraw endpoint for unavailable revision: %w", cleanupErr)
		}
		if err := r.setNotReady(ctx, &svc, "RevisionError", err.Error()); err != nil {
			return Result{}, err
		}
		return Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Count ready instances for the active revision.
	readyInstances, err := r.countReadyInstances(ctx, agent, activeRevision)
	if err != nil {
		return Result{}, fmt.Errorf("count ready instances: %w", err)
	}
	workloadAgent, err := r.resolveWorkloadAgent(ctx, &svc, agent, activeRevision)
	if err != nil {
		log.Warn("failed to resolve revision workload", zap.Error(err))
		if cleanupErr := r.withdrawEndpoint(ctx, &svc); cleanupErr != nil {
			return Result{}, fmt.Errorf("withdraw endpoint for unavailable revision workload: %w", cleanupErr)
		}
		if err := r.setNotReady(ctx, &svc, "RevisionError", err.Error()); err != nil {
			return Result{}, err
		}
		return Result{RequeueAfter: 10 * time.Second}, nil
	}
	endpoint := fmt.Sprintf("/apis/kognition.dev/v1alpha1/namespaces/%s/agentservices/%s/invoke", svc.ObjectMeta.Namespace, svc.ObjectMeta.Name)
	if r.publisher != nil {
		published, publishErr := r.publisher.Ensure(ctx, &svc, workloadAgent)
		err = publishErr
		if err != nil {
			return Result{}, fmt.Errorf("publish agent service endpoint: %w", err)
		}
		endpoint = published.URL
		if published.ReadyInstances != nil {
			readyInstances = *published.ReadyInstances
		}
	}

	// Update status.
	if err := r.updateStatus(ctx, &svc, endpoint, activeRevision, readyInstances); err != nil {
		return Result{}, fmt.Errorf("update status: %w", err)
	}

	return Result{}, nil
}

func (r *AgentServiceReconciler) withdrawEndpoint(ctx context.Context, svc *v1alpha1.AgentService) error {
	if r.publisher == nil {
		return nil
	}
	return r.publisher.Delete(ctx, svc)
}

// handleDeletion removes the finalizer and persists the update.
func (r *AgentServiceReconciler) handleDeletion(ctx context.Context, svc *v1alpha1.AgentService, log *zap.Logger) (Result, error) {
	log.Info("handling agent service deletion")
	if r.publisher != nil {
		if err := r.publisher.Delete(ctx, svc); err != nil {
			return Result{}, fmt.Errorf("delete agent service endpoint: %w", err)
		}
	}

	finalizers, removed := lifecycle.RemoveFinalizer(svc.ObjectMeta.Finalizers, AgentServiceFinalizer)
	if removed {
		svc.ObjectMeta.Finalizers = finalizers
		if err := r.updateService(ctx, svc); err != nil {
			return Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
	}
	return Result{}, nil
}

// resolveAgent fetches the Agent referenced by the AgentService spec.
func (r *AgentServiceReconciler) resolveAgent(ctx context.Context, svc *v1alpha1.AgentService) (*v1alpha1.Agent, error) {
	agentName := svc.Spec.AgentRef.Name
	if agentName == "" {
		return nil, fmt.Errorf("agentRef.name is required")
	}

	raw, err := r.store.Get(ctx, "Agent", svc.ObjectMeta.Namespace, agentName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("agent %q not found", agentName)
		}
		return nil, fmt.Errorf("get agent %q: %w", agentName, err)
	}

	var agent v1alpha1.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		return nil, fmt.Errorf("unmarshal agent %q: %w", agentName, err)
	}
	return &agent, nil
}

// resolveRevision determines the active revision — pinned or latest.
func (r *AgentServiceReconciler) resolveRevision(ctx context.Context, svc *v1alpha1.AgentService, agent *v1alpha1.Agent) (int64, error) {
	if svc.Spec.RevisionPin != nil {
		pinned := *svc.Spec.RevisionPin
		// Verify the pinned revision exists.
		_, err := r.store.GetRevision(ctx, "Agent", svc.ObjectMeta.Namespace, agent.ObjectMeta.Name, pinned)
		if err != nil {
			return 0, fmt.Errorf("pinned revision %d not found for agent %q: %w", pinned, agent.ObjectMeta.Name, err)
		}
		return pinned, nil
	}

	// Use the latest revision from the Agent's status.
	if agent.Status.RevisionCount > 0 {
		return agent.Status.RevisionCount, nil
	}

	// Fallback: list revisions and pick the latest.
	revisions, err := r.store.ListRevisions(ctx, "Agent", svc.ObjectMeta.Namespace, agent.ObjectMeta.Name)
	if err != nil {
		return 0, fmt.Errorf("list revisions for agent %q: %w", agent.ObjectMeta.Name, err)
	}
	if len(revisions) == 0 {
		return 0, fmt.Errorf("no revisions found for agent %q", agent.ObjectMeta.Name)
	}

	// Revisions are appended in order; the last one is the latest.
	return revisions[len(revisions)-1].Revision, nil
}

func (r *AgentServiceReconciler) resolveWorkloadAgent(ctx context.Context, svc *v1alpha1.AgentService, agent *v1alpha1.Agent, revision int64) (*v1alpha1.Agent, error) {
	if svc.Spec.RevisionPin == nil {
		return agent, nil
	}
	snapshot, err := r.store.GetRevision(ctx, "Agent", svc.ObjectMeta.Namespace, agent.ObjectMeta.Name, revision)
	if err != nil {
		return nil, fmt.Errorf("get pinned revision %d for agent %q: %w", revision, agent.ObjectMeta.Name, err)
	}
	var revisionAgent v1alpha1.Agent
	if err := json.Unmarshal(snapshot.Data, &revisionAgent); err != nil {
		return nil, fmt.Errorf("unmarshal pinned revision %d for agent %q: %w", revision, agent.ObjectMeta.Name, err)
	}
	// A revision controls execution configuration only. Current identity and
	// readiness remain authoritative for lifecycle and authorization checks.
	revisionAgent.ObjectMeta = agent.ObjectMeta
	revisionAgent.Status = agent.Status
	return &revisionAgent, nil
}

// countReadyInstances counts AgentInstances owned by the Agent that match the
// target revision and are in a ready state.
func (r *AgentServiceReconciler) countReadyInstances(ctx context.Context, agent *v1alpha1.Agent, revision int64) (int32, error) {
	rawList, err := r.store.List(ctx, agentInstanceKind, store.ListOptions{
		Namespace: agent.ObjectMeta.Namespace,
	})
	if err != nil {
		return 0, err
	}

	var count int32
	for _, raw := range rawList {
		var inst v1alpha1.AgentInstance
		if err := json.Unmarshal(raw, &inst); err != nil {
			continue
		}
		// Must be owned by this agent.
		if !isOwnedBy(inst.ObjectMeta.OwnerReferences, agent) {
			continue
		}
		// Must match the target revision.
		if inst.Spec.Revision != revision {
			continue
		}
		// Must be ready.
		if inst.Status.Phase == v1alpha1.PhaseRunning || inst.Status.Phase == v1alpha1.PhaseReady {
			count++
		}
	}
	return count, nil
}

// isOwnedBy checks if the owner references contain a reference to the given Agent.
func isOwnedBy(refs []v1alpha1.OwnerReference, agent *v1alpha1.Agent) bool {
	for _, ref := range refs {
		if ref.Kind == "Agent" && ref.Name == agent.ObjectMeta.Name && ref.UID == agent.ObjectMeta.UID {
			return true
		}
	}
	return false
}

// updateStatus computes the endpoint and updates the AgentService status.
func (r *AgentServiceReconciler) updateStatus(ctx context.Context, svc *v1alpha1.AgentService, endpoint string, activeRevision int64, readyInstances int32) error {
	previousStatus := svc.Status
	svc.Status.ActiveRevision = activeRevision
	svc.Status.ReadyInstances = readyInstances
	svc.Status.Endpoint = endpoint

	if readyInstances > 0 {
		svc.Status.Phase = v1alpha1.PhaseReady
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(svc.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionTrue,
			"EndpointReady",
			fmt.Sprintf("%d instances ready at revision %d", readyInstances, activeRevision),
		)
		svc.Status.Conditions = lifecycleToAPIConditions(lcConditions)
	} else {
		svc.Status.Phase = v1alpha1.PhasePending
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(svc.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"NoReadyInstances",
			fmt.Sprintf("0 instances ready at revision %d", activeRevision),
		)
		svc.Status.Conditions = lifecycleToAPIConditions(lcConditions)
	}
	if reflect.DeepEqual(svc.Status, previousStatus) {
		return nil
	}

	return r.updateServiceStatus(ctx, svc)
}

// setNotReady sets a not-ready condition and persists the status.
func (r *AgentServiceReconciler) setNotReady(ctx context.Context, svc *v1alpha1.AgentService, reason, message string) error {
	previousStatus := svc.Status
	svc.Status.Phase = v1alpha1.PhasePending
	svc.Status.Endpoint = ""
	svc.Status.ActiveRevision = 0
	svc.Status.ReadyInstances = 0
	lcConditions := lifecycle.SetCondition(
		apiToLifecycleConditions(svc.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		reason,
		message,
	)
	svc.Status.Conditions = lifecycleToAPIConditions(lcConditions)
	if reflect.DeepEqual(svc.Status, previousStatus) {
		return nil
	}
	return r.updateServiceStatus(ctx, svc)
}

func (r *AgentServiceReconciler) updateServiceStatus(ctx context.Context, svc *v1alpha1.AgentService) error {
	desiredStatus := svc.Status
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		if err := r.updateService(ctx, svc); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrConflict) {
			return err
		}

		raw, err := r.store.Get(ctx, agentServiceKind, svc.ObjectMeta.Namespace, svc.ObjectMeta.Name)
		if err != nil {
			return fmt.Errorf("re-read agent service after conflict: %w", err)
		}
		var latest v1alpha1.AgentService
		if err := json.Unmarshal(raw, &latest); err != nil {
			return fmt.Errorf("unmarshal agent service after conflict: %w", err)
		}
		latest.Status.Phase = desiredStatus.Phase
		latest.Status.Endpoint = desiredStatus.Endpoint
		latest.Status.ActiveRevision = desiredStatus.ActiveRevision
		latest.Status.ReadyInstances = desiredStatus.ReadyInstances
		latest.Status.Conditions = mergeAgentCondition(latest.Status.Conditions, desiredStatus.Conditions, string(lifecycle.ConditionReady))
		*svc = latest
	}
	return store.ErrConflict
}

// updateService persists the current AgentService state back to the store.
func (r *AgentServiceReconciler) updateService(ctx context.Context, svc *v1alpha1.AgentService) error {
	data, err := json.Marshal(svc)
	if err != nil {
		return fmt.Errorf("marshal agent service: %w", err)
	}
	return r.store.Update(ctx, agentServiceKind, svc.ObjectMeta.Namespace, svc.ObjectMeta.Name, data, svc.ObjectMeta.ResourceVersion)
}
