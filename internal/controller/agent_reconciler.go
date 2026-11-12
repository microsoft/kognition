// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// AgentFinalizer is the finalizer applied to Agent resources.
	AgentFinalizer = "kognition.dev/agent-cleanup"

	// agentInstanceKind is the kind string for AgentInstance resources.
	agentInstanceKind = "AgentInstance"

	autoAgentIdentityLabel = "kognition.dev/auto-created-for-agent"

	requireAgentIdentityAnnotation = "kognition.dev/require-agent-identity"

	autoAgentIdentityNameAnnotation = "kognition.dev/agent-identity-name"
)

// DeploymentCleaner cleans up cross-namespace Deployment/Service resources
// associated with an Agent. This is called during Agent deletion to ensure
// the runtime pods are removed even though ownerReferences cannot cross namespaces.
type DeploymentCleaner interface {
	CleanupAgentResources(ctx context.Context, agentName, namespace string) error
}

// AgentReconciler reconciles Agent resources by managing AgentInstances
// and tracking revisions when the spec changes.
type AgentReconciler struct {
	store   store.Store
	logger  *zap.Logger
	cleaner DeploymentCleaner
}

// AgentIdentityDependencyHandler requeues Agents in the identity namespace.
// Standalone delete events expose only an identity key, not its AgentRef, so
// every Agent in that namespace is reconciled to restore a missing identity.
type AgentIdentityDependencyHandler struct {
	store  store.Store
	target EventHandler
	logger *zap.Logger
}

// NewAgentIdentityDependencyHandler creates a standalone identity dependency handler.
func NewAgentIdentityDependencyHandler(st store.Store, target EventHandler, logger *zap.Logger) *AgentIdentityDependencyHandler {
	return &AgentIdentityDependencyHandler{store: st, target: target, logger: logger.Named("agent-identity-dependency")}
}

func (h *AgentIdentityDependencyHandler) OnAdd(key string)    { h.enqueueNamespaceAgents(key) }
func (h *AgentIdentityDependencyHandler) OnUpdate(key string) { h.enqueueNamespaceAgents(key) }
func (h *AgentIdentityDependencyHandler) OnDelete(key string) { h.enqueueNamespaceAgents(key) }

func (h *AgentIdentityDependencyHandler) enqueueNamespaceAgents(key string) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 {
		return
	}
	agents, err := h.store.List(context.Background(), "Agent", store.ListOptions{Namespace: parts[0]})
	if err != nil {
		h.logger.Error("list agents for identity dependency event", zap.Error(err))
		return
	}
	for _, raw := range agents {
		var agent v1alpha1.Agent
		if err := json.Unmarshal(raw, &agent); err != nil {
			continue
		}
		h.target.OnUpdate(KeyFunc(parts[0], agent.ObjectMeta.Name))
	}
}

// NewAgentReconciler creates a new AgentReconciler.
func NewAgentReconciler(s store.Store, logger *zap.Logger, opts ...AgentReconcilerOption) *AgentReconciler {
	r := &AgentReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "agent")),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// AgentReconcilerOption configures the AgentReconciler.
type AgentReconcilerOption func(*AgentReconciler)

// WithDeploymentCleaner sets a DeploymentCleaner that is invoked during Agent deletion.
func WithDeploymentCleaner(c DeploymentCleaner) AgentReconcilerOption {
	return func(r *AgentReconciler) {
		r.cleaner = c
	}
}

// Reconcile implements the Reconciler interface for Agent resources.
func (r *AgentReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))
	log.Debug("reconciling agent")

	// Fetch the Agent from the store.
	raw, err := r.store.Get(ctx, "Agent", req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("agent not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get agent: %w", err)
	}

	var agent v1alpha1.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		return Result{}, fmt.Errorf("unmarshal agent: %w", err)
	}

	// Handle deletion: clean up owned AgentInstances.
	if agent.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &agent, log)
	}

	// Ensure finalizer is present.
	if !lifecycle.HasFinalizer(agent.ObjectMeta.Finalizers, AgentFinalizer) {
		finalizers, _ := lifecycle.AddFinalizer(agent.ObjectMeta.Finalizers, AgentFinalizer)
		agent.ObjectMeta.Finalizers = finalizers
		if err := r.updateAgent(ctx, &agent); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		// Re-enqueue so the next reconcile works with the updated resourceVersion.
		return Result{Requeue: true}, nil
	}

	if err := r.ensureAgentIdentity(ctx, &agent); err != nil {
		return Result{}, fmt.Errorf("ensure agent identity: %w", err)
	}

	promptHash, err := r.resolvePromptHash(ctx, &agent)
	if err != nil {
		if err := r.setDriftCondition(ctx, &agent, lifecycle.ConditionTrue, "PromptResolveFailed", err.Error()); err != nil {
			return Result{}, fmt.Errorf("set prompt resolve failure condition: %w", err)
		}
		return Result{}, nil
	}

	// Check for spec changes and create a new revision if needed.
	currentRevision, err := r.ensureRevision(ctx, &agent, promptHash, log)
	if err != nil {
		return Result{}, fmt.Errorf("ensure revision: %w", err)
	}

	// Reconcile AgentInstances to match desired replicas and current revision.
	if err := r.reconcileInstances(ctx, &agent, currentRevision, log); err != nil {
		return Result{}, fmt.Errorf("reconcile instances: %w", err)
	}

	// Update Agent status.
	if err := r.updateStatus(ctx, &agent, currentRevision, promptHash); err != nil {
		return Result{}, fmt.Errorf("update status: %w", err)
	}

	// Re-check periodically until all replicas are ready. AgentInstance status changes
	// re-enqueue the Agent via the Owns watch; this requeue is a fallback safety net,
	// so use a long interval to avoid log/CPU noise when ready replicas never converge
	// (e.g., workflow-driven Agents that never run their self-created replica).
	if agent.Status.Phase != v1alpha1.PhaseReady {
		return Result{RequeueAfter: 30 * time.Second}, nil
	}

	return Result{}, nil
}

func (r *AgentReconciler) ensureAgentIdentity(ctx context.Context, agent *v1alpha1.Agent) error {
	identityName := defaultAgentIdentityName(agent.ObjectMeta.Name)
	raw, err := r.store.Get(ctx, agentIdentityKind, agent.ObjectMeta.Namespace, identityName)
	if err == nil {
		var existing v1alpha1.AgentIdentity
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("unmarshal existing agent identity: %w", err)
		}
		if existing.ObjectMeta.Labels[autoAgentIdentityLabel] != agent.ObjectMeta.Name {
			return fmt.Errorf("AgentIdentity %q is reserved for automatic provisioning", identityName)
		}
		return validateAgentIdentityForAgent(&existing, agent)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("get automatic agent identity: %w", err)
	}

	identity := v1alpha1.AgentIdentity{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: agentIdentityKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      identityName,
			Namespace: agent.ObjectMeta.Namespace,
			UID:       uuid.New().String(),
			Labels: map[string]string{
				autoAgentIdentityLabel: agent.ObjectMeta.Name,
			},
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       "Agent",
				Name:       agent.ObjectMeta.Name,
				UID:        agent.ObjectMeta.UID,
			}},
		},
		Spec: v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: agent.ObjectMeta.Name}},
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("marshal agent identity: %w", err)
	}
	if err := r.store.Create(ctx, agentIdentityKind, agent.ObjectMeta.Namespace, identityName, data); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrAlreadyExists) {
		return fmt.Errorf("create agent identity: %w", err)
	}

	raw, err = r.store.Get(ctx, agentIdentityKind, agent.ObjectMeta.Namespace, identityName)
	if err != nil {
		return fmt.Errorf("get existing agent identity: %w", err)
	}
	var existing v1alpha1.AgentIdentity
	if err := json.Unmarshal(raw, &existing); err != nil {
		return fmt.Errorf("unmarshal existing agent identity: %w", err)
	}
	if existing.ObjectMeta.Labels[autoAgentIdentityLabel] != agent.ObjectMeta.Name {
		return fmt.Errorf("AgentIdentity %q is reserved for automatic provisioning", identityName)
	}
	if err := validateAgentIdentityForAgent(&existing, agent); err != nil {
		return err
	}
	return nil
}

func validateAgentIdentityForAgent(identity *v1alpha1.AgentIdentity, agent *v1alpha1.Agent) error {
	if identity.Spec.AgentRef.Name != agent.ObjectMeta.Name {
		return fmt.Errorf("AgentIdentity %q is already reserved for Agent %q", identity.ObjectMeta.Name, identity.Spec.AgentRef.Name)
	}
	if identity.ObjectMeta.DeletionTimestamp != nil {
		return fmt.Errorf("AgentIdentity %q is deleting", identity.ObjectMeta.Name)
	}
	if identity.ObjectMeta.Labels[autoAgentIdentityLabel] == agent.ObjectMeta.Name && !identityOwnedByAgent(identity, agent) {
		return fmt.Errorf("automatic AgentIdentity %q belongs to a previous Agent instance", identity.ObjectMeta.Name)
	}
	return nil
}

func defaultAgentIdentityName(agentName string) string {
	const prefix = "agent-identity-"
	if len(prefix)+len(agentName) <= 63 {
		return prefix + agentName
	}
	digest := sha256.Sum256([]byte(agentName))
	const suffixLength = 1 + 8
	maxNameLength := 63 - len(prefix) - suffixLength
	trimmed := strings.TrimRight(agentName[:maxNameLength], "-.")
	if trimmed == "" {
		trimmed = "agent"
	}
	return fmt.Sprintf("%s%s-%x", prefix, trimmed, digest[:4])
}

// handleDeletion cleans up owned AgentInstances and removes the finalizer.
func (r *AgentReconciler) handleDeletion(ctx context.Context, agent *v1alpha1.Agent, log *zap.Logger) (Result, error) {
	log.Info("handling agent deletion")

	// Delete all owned AgentInstances.
	instances, err := r.listOwnedInstances(ctx, agent)
	if err != nil {
		return Result{}, fmt.Errorf("list instances for deletion: %w", err)
	}

	for _, inst := range instances {
		if err := r.store.Delete(ctx, agentInstanceKind, inst.ObjectMeta.Namespace, inst.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
			return Result{}, fmt.Errorf("delete instance %s: %w", inst.ObjectMeta.Name, err)
		}
	}

	if err := r.deleteAutomaticAgentIdentity(ctx, agent); err != nil {
		return Result{}, fmt.Errorf("delete automatic agent identity: %w", err)
	}

	// Clean up cross-namespace Deployment/Service if a cleaner is configured.
	if r.cleaner != nil {
		if err := r.cleaner.CleanupAgentResources(ctx, agent.ObjectMeta.Name, agent.ObjectMeta.Namespace); err != nil {
			return Result{}, fmt.Errorf("cleanup agent deployment resources: %w", err)
		}
	}

	// Remove finalizer.
	finalizers, removed := lifecycle.RemoveFinalizer(agent.ObjectMeta.Finalizers, AgentFinalizer)
	if removed {
		agent.ObjectMeta.Finalizers = finalizers
		if err := r.updateAgent(ctx, agent); err != nil {
			return Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
	}

	return Result{}, nil
}

func (r *AgentReconciler) deleteAutomaticAgentIdentity(ctx context.Context, agent *v1alpha1.Agent) error {
	identities, err := r.store.List(ctx, agentIdentityKind, store.ListOptions{
		Namespace:     agent.ObjectMeta.Namespace,
		LabelSelector: map[string]string{autoAgentIdentityLabel: agent.ObjectMeta.Name},
	})
	if err != nil {
		return fmt.Errorf("list automatic agent identities: %w", err)
	}
	for _, raw := range identities {
		var identity v1alpha1.AgentIdentity
		if err := json.Unmarshal(raw, &identity); err != nil {
			continue
		}
		if identity.ObjectMeta.Labels[autoAgentIdentityLabel] != agent.ObjectMeta.Name {
			continue
		}
		if !identityOwnedByAgent(&identity, agent) {
			continue
		}
		if err := r.store.Delete(ctx, agentIdentityKind, identity.ObjectMeta.Namespace, identity.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("delete AgentIdentity %q: %w", identity.ObjectMeta.Name, err)
		}
	}
	return nil
}

func identityOwnedByAgent(identity *v1alpha1.AgentIdentity, agent *v1alpha1.Agent) bool {
	for _, owner := range identity.ObjectMeta.OwnerReferences {
		if owner.Kind == "Agent" && owner.Name == agent.ObjectMeta.Name && owner.UID == agent.ObjectMeta.UID {
			return true
		}
	}
	return false
}

// ensureRevision checks if the spec has changed since the last revision.
// If so, it creates a new revision and returns the new revision number.
func (r *AgentReconciler) ensureRevision(ctx context.Context, agent *v1alpha1.Agent, promptHash string, log *zap.Logger) (int64, error) {
	revisionSpec := struct {
		Spec       v1alpha1.AgentSpec `json:"spec"`
		PromptHash string             `json:"promptHash,omitempty"`
	}{
		Spec:       agent.Spec,
		PromptHash: promptHash,
	}

	specBytes, err := json.Marshal(revisionSpec)
	if err != nil {
		return 0, fmt.Errorf("marshal spec: %w", err)
	}

	// List existing revisions to determine the current state.
	revisions, err := r.store.ListRevisions(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name)
	if err != nil {
		return 0, fmt.Errorf("list revisions: %w", err)
	}

	// If there are existing revisions, check if the latest spec matches.
	// Compare using canonical JSON to avoid differences caused by JSONB
	// key reordering in PostgreSQL.
	if len(revisions) > 0 {
		latest := revisions[len(revisions)-1]
		if jsonBytesEqual(latest.Spec, specBytes) {
			return latest.Revision, nil
		}
	}

	// Spec has changed (or no revisions exist), create the next revision above
	// the highest stored value. Legacy Kubernetes history may have gaps after
	// immutable ConfigMap snapshots were introduced.
	newRevision := int64(1)
	for _, revision := range revisions {
		if revision.Revision >= newRevision {
			newRevision = revision.Revision + 1
		}
	}
	agentBytes, err := json.Marshal(agent)
	if err != nil {
		return 0, fmt.Errorf("marshal agent: %w", err)
	}

	if err := r.store.CreateRevision(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, newRevision, specBytes, agentBytes); err != nil {
		return 0, fmt.Errorf("create revision %d: %w", newRevision, err)
	}

	log.Info("created new revision", zap.Int64("revision", newRevision))
	return newRevision, nil
}

// reconcileInstances ensures the correct number of AgentInstances exist for the current revision.
// Old-revision instances are drained (deleted) and new ones are created.
func (r *AgentReconciler) reconcileInstances(ctx context.Context, agent *v1alpha1.Agent, currentRevision int64, log *zap.Logger) error {
	instances, err := r.listOwnedInstances(ctx, agent)
	if err != nil {
		return fmt.Errorf("list owned instances: %w", err)
	}
	log.Debug("owned instances", zap.Int("count", len(instances)), zap.Int64("revision", currentRevision))

	desiredReplicas := int32(1)
	if agent.Spec.Replicas != nil {
		desiredReplicas = *agent.Spec.Replicas
	}

	// Separate instances by revision.
	// Failed instances at the current revision are treated as replaceable: they
	// are drained here so a fresh instance can be created and re-dispatched.
	// Without this, a transient dispatch failure (e.g. controller cache race on
	// Agent lookup at startup) would leave the Agent permanently without a
	// running pod because the failed instance still counts against desired
	// replicas.
	var currentInstances []v1alpha1.AgentInstance
	var staleInstances []v1alpha1.AgentInstance
	var failedInstances []v1alpha1.AgentInstance
	for _, inst := range instances {
		if inst.Spec.Revision != currentRevision {
			staleInstances = append(staleInstances, inst)
			continue
		}
		if inst.Status.Phase == v1alpha1.PhaseFailed {
			failedInstances = append(failedInstances, inst)
			continue
		}
		currentInstances = append(currentInstances, inst)
	}

	// Delete stale instances (old revision).
	for _, inst := range staleInstances {
		log.Info("draining stale instance",
			zap.String("instance", inst.ObjectMeta.Name),
			zap.Int64("revision", inst.Spec.Revision),
		)
		if err := r.store.Delete(ctx, agentInstanceKind, inst.ObjectMeta.Namespace, inst.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("delete stale instance %s: %w", inst.ObjectMeta.Name, err)
		}
	}

	// Delete failed instances so a fresh one is created on the next reconcile pass.
	for _, inst := range failedInstances {
		log.Info("replacing failed instance",
			zap.String("instance", inst.ObjectMeta.Name),
			zap.Int64("revision", inst.Spec.Revision),
		)
		if err := r.store.Delete(ctx, agentInstanceKind, inst.ObjectMeta.Namespace, inst.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("delete failed instance %s: %w", inst.ObjectMeta.Name, err)
		}
	}

	// Scale up: create missing instances for the current revision.
	currentCount := int32(len(currentInstances))
	for i := currentCount; i < desiredReplicas; i++ {
		if err := r.createInstance(ctx, agent, currentRevision, log); err != nil {
			return fmt.Errorf("create instance: %w", err)
		}
	}

	// Scale down: delete excess instances for the current revision.
	if currentCount > desiredReplicas {
		excess := currentInstances[desiredReplicas:]
		for _, inst := range excess {
			log.Info("scaling down, deleting excess instance", zap.String("instance", inst.ObjectMeta.Name))
			if err := r.store.Delete(ctx, agentInstanceKind, inst.ObjectMeta.Namespace, inst.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("delete excess instance %s: %w", inst.ObjectMeta.Name, err)
			}
		}
	}

	return nil
}

// createInstance creates a new AgentInstance owned by the given Agent.
func (r *AgentReconciler) createInstance(ctx context.Context, agent *v1alpha1.Agent, revision int64, log *zap.Logger) error {
	instanceName := fmt.Sprintf("%s-%s", agent.ObjectMeta.Name, uuid.New().String()[:8])

	instance := v1alpha1.AgentInstance{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       agentInstanceKind,
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              instanceName,
			Namespace:         agent.ObjectMeta.Namespace,
			UID:               uuid.New().String(),
			CreationTimestamp: time.Now().UTC(),
			Annotations: map[string]string{
				requireAgentIdentityAnnotation:  "true",
				autoAgentIdentityNameAnnotation: defaultAgentIdentityName(agent.ObjectMeta.Name),
			},
			OwnerReferences: []v1alpha1.OwnerReference{
				{
					APIVersion: v1alpha1.APIVersion,
					Kind:       "Agent",
					Name:       agent.ObjectMeta.Name,
					UID:        agent.ObjectMeta.UID,
					Controller: true,
				},
			},
		},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: agent.ObjectMeta.Name},
			Revision: revision,
		},
		Status: v1alpha1.AgentInstanceStatus{
			Phase: v1alpha1.PhasePending,
		},
	}

	data, err := json.Marshal(instance)
	if err != nil {
		return fmt.Errorf("marshal instance: %w", err)
	}

	if err := r.store.Create(ctx, agentInstanceKind, agent.ObjectMeta.Namespace, instanceName, data); err != nil {
		return fmt.Errorf("store instance: %w", err)
	}

	log.Info("created agent instance", zap.String("instance", instanceName), zap.Int64("revision", revision))
	return nil
}

// listOwnedInstances returns all AgentInstances that reference this Agent via ownerReferences.
func (r *AgentReconciler) listOwnedInstances(ctx context.Context, agent *v1alpha1.Agent) ([]v1alpha1.AgentInstance, error) {
	rawList, err := r.store.List(ctx, agentInstanceKind, store.ListOptions{
		Namespace: agent.ObjectMeta.Namespace,
	})
	if err != nil {
		return nil, err
	}

	var owned []v1alpha1.AgentInstance
	for _, raw := range rawList {
		var inst v1alpha1.AgentInstance
		if err := json.Unmarshal(raw, &inst); err != nil {
			continue
		}
		// Skip instances that are being deleted (soft-deleted).
		if inst.ObjectMeta.DeletionTimestamp != nil {
			continue
		}
		for _, ref := range inst.ObjectMeta.OwnerReferences {
			if ref.Kind == "Agent" && ref.Name == agent.ObjectMeta.Name && ref.UID == agent.ObjectMeta.UID {
				owned = append(owned, inst)
				break
			}
		}
	}
	return owned, nil
}

// updateStatus updates the Agent's status fields based on current state.
func (r *AgentReconciler) updateStatus(ctx context.Context, agent *v1alpha1.Agent, currentRevision int64, promptHash string) error {
	instances, err := r.listOwnedInstances(ctx, agent)
	if err != nil {
		return fmt.Errorf("list instances for status: %w", err)
	}

	desiredReplicas := int32(1)
	if agent.Spec.Replicas != nil {
		desiredReplicas = *agent.Spec.Replicas
	}

	var readyCount int32
	for _, inst := range instances {
		if inst.Status.Phase == v1alpha1.PhaseReady {
			readyCount++
		}
	}

	// Skip update if status hasn't changed.
	if agent.Status.Replicas == desiredReplicas &&
		agent.Status.ReadyReplicas == readyCount &&
		agent.Status.RevisionCount == currentRevision &&
		agent.Status.Phase == r.computePhase(readyCount, desiredReplicas, len(instances)) {
		return nil
	}

	agent.Status.Replicas = desiredReplicas
	agent.Status.ReadyReplicas = readyCount
	agent.Status.RevisionCount = currentRevision

	// Determine conditions.
	phase := r.computePhase(readyCount, desiredReplicas, len(instances))
	agent.Status.Phase = phase

	switch phase {
	case v1alpha1.PhaseReady:
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(agent.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionTrue,
			"AllReplicasReady",
			fmt.Sprintf("%d/%d replicas ready", readyCount, desiredReplicas),
		)
		agent.Status.Conditions = lifecycleToAPIConditions(lcConditions)
	case v1alpha1.PhasePending:
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(agent.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"NoInstances",
			"no instances have been created yet",
		)
		agent.Status.Conditions = lifecycleToAPIConditions(lcConditions)
	default: // Running
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(agent.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"ReplicasNotReady",
			fmt.Sprintf("%d/%d replicas ready", readyCount, desiredReplicas),
		)
		agent.Status.Conditions = lifecycleToAPIConditions(lcConditions)
	}

	if agent.Spec.Instructions.Ref != nil && promptHash != "" {
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(agent.Status.Conditions),
			"DriftDetected",
			lifecycle.ConditionFalse,
			"PromptInSync",
			fmt.Sprintf("prompt %s hash %s applied", agent.Spec.Instructions.Ref.Name, promptHash),
		)
		agent.Status.Conditions = lifecycleToAPIConditions(lcConditions)
	}

	return r.updateAgent(ctx, agent)
}

func (r *AgentReconciler) computePhase(readyCount, desiredReplicas int32, totalInstances int) v1alpha1.Phase {
	if readyCount >= desiredReplicas {
		return v1alpha1.PhaseReady
	}
	if totalInstances == 0 {
		return v1alpha1.PhasePending
	}
	return v1alpha1.PhaseRunning
}

func (r *AgentReconciler) resolvePromptHash(ctx context.Context, agent *v1alpha1.Agent) (string, error) {
	if agent.Spec.Instructions.Ref == nil {
		return "", nil
	}

	promptName := agent.Spec.Instructions.Ref.Name
	raw, err := r.store.Get(ctx, "Prompt", agent.ObjectMeta.Namespace, promptName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("prompt %s not found", promptName)
		}
		return "", fmt.Errorf("get prompt %s: %w", promptName, err)
	}

	var prompt v1alpha1.Prompt
	if err := json.Unmarshal(raw, &prompt); err != nil {
		return "", fmt.Errorf("unmarshal prompt %s: %w", promptName, err)
	}

	if prompt.Status.Hash != "" {
		return prompt.Status.Hash, nil
	}

	return promptContentHash(prompt.Spec.ResolvedContent()), nil
}

func (r *AgentReconciler) setDriftCondition(ctx context.Context, agent *v1alpha1.Agent, status lifecycle.ConditionStatus, reason, message string) error {
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(agent.Status.Conditions),
		"DriftDetected",
		status,
		reason,
		message,
	)
	agent.Status.Conditions = lifecycleToAPIConditions(lc)
	return r.updateAgent(ctx, agent)
}

// updateAgent persists the current Agent state back to the store.
// It retries on resource version conflicts by re-reading the latest rv and
// merging the in-memory status/finalizers before retrying the write.
func (r *AgentReconciler) updateAgent(ctx context.Context, agent *v1alpha1.Agent) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		data, err := json.Marshal(agent)
		if err != nil {
			return fmt.Errorf("marshal agent: %w", err)
		}
		err = r.store.Update(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, data, agent.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		// Conflict: re-read the latest version and apply our changes on top.
		raw, getErr := r.store.Get(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name)
		if getErr != nil {
			if errors.Is(getErr, store.ErrNotFound) {
				// Object was deleted between Update and Get (e.g. cascade delete
				// finished). There is nothing left to update.
				return nil
			}
			return fmt.Errorf("re-read after conflict: %w", getErr)
		}
		var latest v1alpha1.Agent
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal after conflict: %w", getErr)
		}
		latest.Status.Phase = agent.Status.Phase
		latest.Status.Replicas = agent.Status.Replicas
		latest.Status.ReadyReplicas = agent.Status.ReadyReplicas
		latest.Status.RevisionCount = agent.Status.RevisionCount
		latest.Status.Conditions = mergeAgentCondition(latest.Status.Conditions, agent.Status.Conditions, string(lifecycle.ConditionReady))
		latest.Status.Conditions = mergeAgentCondition(latest.Status.Conditions, agent.Status.Conditions, "DriftDetected")
		latest.ObjectMeta.Finalizers = mergeOwnedFinalizer(
			latest.ObjectMeta.Finalizers,
			agent.ObjectMeta.Finalizers,
			AgentFinalizer,
			agent.ObjectMeta.DeletionTimestamp != nil,
		)
		*agent = latest
	}
	return store.ErrConflict
}

// apiToLifecycleConditions converts API conditions to lifecycle conditions for manipulation.
func apiToLifecycleConditions(conditions []v1alpha1.Condition) []lifecycle.Condition {
	result := make([]lifecycle.Condition, len(conditions))
	for i, c := range conditions {
		result[i] = lifecycle.Condition{
			Type:               c.Type,
			Status:             c.Status,
			LastTransitionTime: c.LastTransitionTime.Format(time.RFC3339),
			Reason:             c.Reason,
			Message:            c.Message,
		}
	}
	return result
}

// lifecycleToAPIConditions converts lifecycle conditions back to API conditions.
func lifecycleToAPIConditions(conditions []lifecycle.Condition) []v1alpha1.Condition {
	result := make([]v1alpha1.Condition, len(conditions))
	for i, c := range conditions {
		t, _ := time.Parse(time.RFC3339, c.LastTransitionTime)
		result[i] = v1alpha1.Condition{
			Type:               c.Type,
			Status:             c.Status,
			LastTransitionTime: t,
			Reason:             c.Reason,
			Message:            c.Message,
		}
	}
	return result
}

func mergeAgentCondition(current, desired []v1alpha1.Condition, conditionType string) []v1alpha1.Condition {
	for _, condition := range desired {
		if condition.Type != conditionType {
			continue
		}
		merged := append([]v1alpha1.Condition(nil), current...)
		for i := range merged {
			if merged[i].Type == conditionType {
				merged[i] = condition
				return merged
			}
		}
		return append(merged, condition)
	}
	return current
}

// jsonBytesEqual compares two JSON byte slices for semantic equality,
// handling differences in key ordering (e.g., from PostgreSQL JSONB normalization).
func jsonBytesEqual(a, b []byte) bool {
	var objA, objB interface{}
	if err := json.Unmarshal(a, &objA); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &objB); err != nil {
		return false
	}
	canonA, err := json.Marshal(objA)
	if err != nil {
		return false
	}
	canonB, err := json.Marshal(objB)
	if err != nil {
		return false
	}
	return string(canonA) == string(canonB)
}
