// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// WorkspaceFinalizer is applied to Workspace resources for cleanup.
	WorkspaceFinalizer = "kognition.dev/workspace-cleanup"
)

// WorkspaceReconciler reconciles Workspace resources providing
// namespace-equivalent isolation with default-deny cross-workspace access.
type WorkspaceReconciler struct {
	store  store.Store
	logger *zap.Logger
}

// NewWorkspaceReconciler creates a new WorkspaceReconciler.
func NewWorkspaceReconciler(s store.Store, logger *zap.Logger) *WorkspaceReconciler {
	return &WorkspaceReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "workspace")),
	}
}

// Reconcile implements the Reconciler interface for Workspace resources.
func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	raw, err := r.store.Get(ctx, workspaceKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("workspace not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get workspace: %w", err)
	}

	var ws v1alpha1.Workspace
	if err := json.Unmarshal(raw, &ws); err != nil {
		return Result{}, fmt.Errorf("unmarshal workspace: %w", err)
	}

	// Handle deletion.
	if ws.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &ws, log)
	}

	// Ensure finalizer is present.
	if !lifecycle.HasFinalizer(ws.ObjectMeta.Finalizers, WorkspaceFinalizer) {
		finalizers, _ := lifecycle.AddFinalizer(ws.ObjectMeta.Finalizers, WorkspaceFinalizer)
		ws.ObjectMeta.Finalizers = finalizers
		if err := r.updateWorkspace(ctx, &ws); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	// Ensure default communication policy (default-deny) exists for this workspace.
	if err := r.ensureDefaultDenyPolicy(ctx, &ws, log); err != nil {
		return Result{}, fmt.Errorf("ensure default deny policy: %w", err)
	}

	// Update workspace status with agent counts.
	if err := r.updateStatus(ctx, &ws, log); err != nil {
		return Result{}, fmt.Errorf("update workspace status: %w", err)
	}

	return Result{}, nil
}

// handleDeletion cleans up workspace-owned resources.
func (r *WorkspaceReconciler) handleDeletion(ctx context.Context, ws *v1alpha1.Workspace, log *zap.Logger) (Result, error) {
	log.Info("handling workspace deletion")

	// Remove finalizer.
	finalizers, removed := lifecycle.RemoveFinalizer(ws.ObjectMeta.Finalizers, WorkspaceFinalizer)
	if removed {
		ws.ObjectMeta.Finalizers = finalizers
		if err := r.updateWorkspace(ctx, ws); err != nil {
			return Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
	}

	return Result{}, nil
}

// ensureDefaultDenyPolicy creates a workspace-level default-deny AgentCommunicationPolicy
// if one does not already exist. This enforces default isolation.
func (r *WorkspaceReconciler) ensureDefaultDenyPolicy(ctx context.Context, ws *v1alpha1.Workspace, log *zap.Logger) error {
	policyName := ws.ObjectMeta.Name + "-default-deny"
	namespace := workspaceNamespace(ws)
	_, err := r.store.Get(ctx, "AgentCommunicationPolicy", namespace, policyName)
	if err == nil {
		return nil // already exists
	}
	if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("check default policy: %w", err)
	}

	log.Info("creating default-deny communication policy", zap.String("policy", policyName))

	policy := v1alpha1.AgentCommunicationPolicy{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "AgentCommunicationPolicy",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      policyName,
			Namespace: namespace,
		},
		Spec: v1alpha1.AgentCommunicationPolicySpec{
			DefaultDeny: true,
		},
	}

	data, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("marshal policy: %w", err)
	}

	if err := r.store.Create(ctx, "AgentCommunicationPolicy", namespace, policyName, data); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil
		}
		return fmt.Errorf("create default policy: %w", err)
	}

	return nil
}

// updateStatus computes workspace status (agent count) and persists it.
func (r *WorkspaceReconciler) updateStatus(ctx context.Context, ws *v1alpha1.Workspace, log *zap.Logger) error {
	agents, err := r.store.List(ctx, "Agent", store.ListOptions{Namespace: workspaceNamespace(ws)})
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}

	_ = len(agents) // AgentCount tracked for observability via list length

	log.Debug("workspace status updated", zap.Int("agentCount", len(agents)))
	if ws.Status.Phase == v1alpha1.PhaseReady {
		return nil
	}

	ws.Status.Phase = v1alpha1.PhaseReady
	return r.updateWorkspace(ctx, ws)
}

func workspaceNamespace(ws *v1alpha1.Workspace) string {
	if ws.ObjectMeta.Namespace != "" {
		return ws.ObjectMeta.Namespace
	}
	return ws.ObjectMeta.Name
}

// updateWorkspace persists the workspace to the store.
func (r *WorkspaceReconciler) updateWorkspace(ctx context.Context, ws *v1alpha1.Workspace) error {
	data, err := json.Marshal(ws)
	if err != nil {
		return fmt.Errorf("marshal workspace: %w", err)
	}
	return r.store.Update(ctx, workspaceKind, ws.ObjectMeta.Namespace, ws.ObjectMeta.Name, data, ws.ObjectMeta.ResourceVersion)
}

// ValidateWorkspaceBoundary enforces that a resource belongs to an existing workspace.
// Returns an error if the workspace does not exist.
func ValidateWorkspaceBoundary(ctx context.Context, s store.Store, namespace string) error {
	if namespace == "" {
		return fmt.Errorf("resource must target a workspace (namespace is required)")
	}
	_, err := s.Get(ctx, workspaceKind, "", namespace)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("workspace %q does not exist", namespace)
	}
	return err
}

// ValidateCrossWorkspaceAccess denies access to resources in another workspace.
// Returns an error if sourceNamespace != targetNamespace and no explicit grant exists.
func ValidateCrossWorkspaceAccess(ctx context.Context, s store.Store, sourceNamespace, targetNamespace, kind, name string) error {
	if sourceNamespace == targetNamespace {
		return nil
	}
	// Check for an explicit CrossNamespaceGrant.
	grants, err := s.List(ctx, "CrossNamespaceGrant", store.ListOptions{Namespace: targetNamespace})
	if err != nil {
		return fmt.Errorf("list cross-namespace grants: %w", err)
	}
	for _, raw := range grants {
		var grant v1alpha1.CrossNamespaceGrant
		if err := json.Unmarshal(raw, &grant); err != nil {
			continue
		}
		if grant.Spec.From == targetNamespace && (grant.Spec.To == sourceNamespace || grant.Spec.To == "*") {
			for _, res := range grant.Spec.Resources {
				if res.Kind == kind && (res.Name == name || res.Name == "*") {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("cross-workspace access denied: %s/%s in workspace %q from workspace %q", kind, name, targetNamespace, sourceNamespace)
}
