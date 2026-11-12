// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

// ErrAgentIdentityNotFound indicates that an agent has no declared workload identity.
var ErrAgentIdentityNotFound = errors.New("agent identity not found")

// ErrAgentIdentityNotReady indicates that an AgentIdentity exists but cannot
// yet authenticate an AgentInstance dispatch.
var ErrAgentIdentityNotReady = errors.New("agent identity is not ready")

// ErrAgentIdentityAmbiguous indicates that more than one active identity
// references the same legacy Agent principal.
var ErrAgentIdentityAmbiguous = errors.New("agent identity is ambiguous")

const (
	// AgentIdentityFinalizer is applied to AgentIdentity resources for cleanup.
	AgentIdentityFinalizer = "kognition.dev/identity-cleanup"

	agentIdentityKind = "AgentIdentity"
)

// spiffeIDFormat is the SPIFFE ID format template for agent identities.
const spiffeIDFormat = "spiffe://kognition.dev/workspace/%s/agent/%s"

// AgentIdentityReconciler reconciles deterministic SPIFFE identifiers and
// manages their lifecycle. Cryptographic SVID issuance is not implemented.
type AgentIdentityReconciler struct {
	store          store.Store
	logger         *zap.Logger
	workloadStatus WorkloadIdentityStatusReader
}

type WorkloadIdentityStatusReader interface {
	AgentStatus(namespace, agentName, agentUID string) (runtimeauth.WorkloadIdentityStatus, error)
}

// NewAgentIdentityReconciler creates a new AgentIdentityReconciler.
func NewAgentIdentityReconciler(s store.Store, logger *zap.Logger) *AgentIdentityReconciler {
	return &AgentIdentityReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "agent-identity")),
	}
}

func NewAgentIdentityReconcilerWithWorkloadIdentity(s store.Store, logger *zap.Logger, reader WorkloadIdentityStatusReader) *AgentIdentityReconciler {
	reconciler := NewAgentIdentityReconciler(s, logger)
	reconciler.workloadStatus = reader
	return reconciler
}

// Reconcile implements the Reconciler interface for AgentIdentity resources.
func (r *AgentIdentityReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	raw, err := r.store.Get(ctx, agentIdentityKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("agent identity not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get agent identity: %w", err)
	}

	var identity v1alpha1.AgentIdentity
	if err := json.Unmarshal(raw, &identity); err != nil {
		return Result{}, fmt.Errorf("unmarshal agent identity: %w", err)
	}

	// Handle deletion.
	if identity.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &identity, log)
	}

	// Ensure finalizer is present.
	if !lifecycle.HasFinalizer(identity.ObjectMeta.Finalizers, AgentIdentityFinalizer) {
		finalizers, _ := lifecycle.AddFinalizer(identity.ObjectMeta.Finalizers, AgentIdentityFinalizer)
		identity.ObjectMeta.Finalizers = finalizers
		if err := r.updateIdentity(ctx, &identity); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	// Assign or validate the deterministic SPIFFE identifier.
	if err := r.ensureSPIFFEID(ctx, &identity, log); err != nil {
		return Result{}, fmt.Errorf("ensure spiffe id: %w", err)
	}

	if r.workloadStatus != nil {
		return Result{RequeueAfter: 5 * time.Second}, nil
	}
	return Result{}, nil
}

// handleDeletion removes the finalizer from an AgentIdentity being deleted.
func (r *AgentIdentityReconciler) handleDeletion(ctx context.Context, identity *v1alpha1.AgentIdentity, log *zap.Logger) (Result, error) {
	log.Info("handling agent identity deletion")

	finalizers, removed := lifecycle.RemoveFinalizer(identity.ObjectMeta.Finalizers, AgentIdentityFinalizer)
	if removed {
		identity.ObjectMeta.Finalizers = finalizers
		if err := r.updateIdentity(ctx, identity); err != nil {
			return Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
	}

	return Result{}, nil
}

// ensureSPIFFEID generates and persists the SPIFFE ID if not already set.
func (r *AgentIdentityReconciler) ensureSPIFFEID(ctx context.Context, identity *v1alpha1.AgentIdentity, log *zap.Logger) error {
	expectedID := fmt.Sprintf(spiffeIDFormat, identity.ObjectMeta.Namespace, identity.Spec.AgentRef.Name)
	workloadStatus := r.observedWorkloadIdentity(ctx, identity)

	if identity.Spec.SPIFFEID == expectedID && identity.Status.Phase == v1alpha1.PhaseReady && reflect.DeepEqual(identity.Status.WorkloadIdentity, workloadStatus) {
		return nil
	}

	identity.Spec.SPIFFEID = expectedID
	identity.Status.Phase = v1alpha1.PhaseReady
	identity.Status.WorkloadIdentity = workloadStatus
	log.Info("assigned SPIFFE identifier", zap.String("spiffeID", expectedID))

	return r.updateIdentity(ctx, identity)
}

func (r *AgentIdentityReconciler) observedWorkloadIdentity(ctx context.Context, identity *v1alpha1.AgentIdentity) *v1alpha1.WorkloadIdentityStatus {
	if r.workloadStatus == nil {
		return nil
	}
	status := &v1alpha1.WorkloadIdentityStatus{Reason: "AgentBindingInvalid"}
	agent, err := getStoredResource[v1alpha1.Agent](ctx, r.store, "Agent", identity.ObjectMeta.Namespace, identity.Spec.AgentRef.Name)
	if err != nil || agent.ObjectMeta.DeletionTimestamp != nil || agent.ObjectMeta.UID == "" ||
		identity.ObjectMeta.Name != defaultAgentIdentityName(agent.ObjectMeta.Name) ||
		identity.ObjectMeta.Labels[autoAgentIdentityLabel] != agent.ObjectMeta.Name || !identityOwnedByAgent(identity, agent) {
		return status
	}
	status.AgentUID = agent.ObjectMeta.UID
	observed, err := r.workloadStatus.AgentStatus(agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, agent.ObjectMeta.UID)
	if err != nil {
		status.Reason = "IssuerUnavailable"
		return status
	}
	expected, err := runtimeauth.AgentIncarnationSPIFFEID(observed.TrustDomain, agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, agent.ObjectMeta.UID)
	if err != nil || observed.SPIFFEID != expected || observed.Generation > math.MaxInt64 {
		status.Reason = "InvalidIdentity"
		return status
	}
	status.SPIFFEID = observed.SPIFFEID
	status.TrustDomain = observed.TrustDomain
	status.NotBefore = observed.NotBefore.UTC()
	status.ExpiresAt = observed.ExpiresAt.UTC()
	status.SourceGeneration = int64(observed.Generation)
	status.Reason = "InvalidIdentity"
	if !observed.Ready {
		switch observed.Reason {
		case "AwaitingIdentity", "InvalidIdentity", "IssuerUnavailable", "ExpiredIdentity", "Closed":
			status.Reason = observed.Reason
		}
		return status
	}
	if observed.Generation == 0 || observed.NotBefore.IsZero() || observed.ExpiresAt.IsZero() || !observed.NotBefore.Before(observed.ExpiresAt) {
		status.Reason = "InvalidIdentity"
		return status
	}
	now := time.Now()
	if now.Before(observed.NotBefore) || !now.Before(observed.ExpiresAt) {
		status.Reason = "ExpiredIdentity"
		return status
	}
	status.Ready = true
	status.Reason = "Ready"
	return status
}

// updateIdentity persists the AgentIdentity to the store.
func (r *AgentIdentityReconciler) updateIdentity(ctx context.Context, identity *v1alpha1.AgentIdentity) error {
	data, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("marshal identity: %w", err)
	}
	return r.store.Update(ctx, agentIdentityKind, identity.ObjectMeta.Namespace, identity.ObjectMeta.Name, data, identity.ObjectMeta.ResourceVersion)
}

// ResolveAgentIdentity looks up the AgentIdentity for a given agent in a workspace.
// It searches for identities that reference the agent.
func ResolveAgentIdentity(ctx context.Context, s store.Store, namespace, agentName string) (*v1alpha1.AgentIdentity, error) {
	identities, err := s.List(ctx, agentIdentityKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return nil, fmt.Errorf("list identities: %w", err)
	}

	var match *v1alpha1.AgentIdentity
	for _, raw := range identities {
		var id v1alpha1.AgentIdentity
		if err := json.Unmarshal(raw, &id); err != nil {
			continue
		}
		if id.ObjectMeta.DeletionTimestamp != nil {
			continue
		}
		if id.Spec.AgentRef.Name == agentName {
			if match != nil {
				return nil, fmt.Errorf("%w for agent %q in workspace %q", ErrAgentIdentityAmbiguous, agentName, namespace)
			}
			match = &id
		}
	}
	if match != nil {
		return match, nil
	}

	return nil, fmt.Errorf("%w for agent %q in workspace %q", ErrAgentIdentityNotFound, agentName, namespace)
}

// ResolveAutomaticAgentIdentity resolves the deterministic controller-owned
// identity used by a managed Agent replica and verifies it belongs to the
// current Agent incarnation.
func ResolveAutomaticAgentIdentity(ctx context.Context, s store.Store, namespace, agentName, identityName string) (*v1alpha1.AgentIdentity, error) {
	agent, err := getStoredResource[v1alpha1.Agent](ctx, s, "Agent", namespace, agentName)
	if err != nil {
		return nil, fmt.Errorf("load agent for identity: %w", err)
	}
	identity, err := getStoredResource[v1alpha1.AgentIdentity](ctx, s, agentIdentityKind, namespace, identityName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %q", ErrAgentIdentityNotFound, identityName)
		}
		return nil, fmt.Errorf("load automatic agent identity: %w", err)
	}
	if identity.ObjectMeta.DeletionTimestamp != nil ||
		identity.Spec.AgentRef.Name != agentName ||
		identity.ObjectMeta.Labels[autoAgentIdentityLabel] != agentName ||
		!identityOwnedByAgent(identity, agent) {
		return nil, fmt.Errorf("%w: automatic AgentIdentity %q does not belong to the current Agent", ErrAgentIdentityNotReady, identityName)
	}
	return identity, nil
}

// ResolveAuthorizationAgentIdentity resolves the controller-managed identity
// used to authorize an Agent's tool calls. Manual identities are attribution
// metadata and cannot authorize AgentRole-protected operations.
func ResolveAuthorizationAgentIdentity(ctx context.Context, s store.Store, namespace, agentName string) (*v1alpha1.AgentIdentity, error) {
	automaticIdentityName := defaultAgentIdentityName(agentName)
	identity, err := ResolveAutomaticAgentIdentity(ctx, s, namespace, agentName, automaticIdentityName)
	if err != nil {
		return nil, err
	}
	if identity.Status.Phase != v1alpha1.PhaseReady || identity.Spec.SPIFFEID == "" {
		return nil, fmt.Errorf("%w: %q", ErrAgentIdentityNotReady, identity.ObjectMeta.Name)
	}
	return identity, nil
}
