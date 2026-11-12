// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

const defaultCostRetentionInterval = time.Hour

// CostRetentionStore adds retention deletion to the query/write CostStore contract.
type CostRetentionStore interface {
	observability.CostStore
	DeleteCostRecordsBefore(context.Context, string, time.Time) (int64, error)
}

// CostRetentionReconciler enforces Workspace CostRecordTTL for durable cost records.
type CostRetentionReconciler struct {
	store    store.Store
	costs    CostRetentionStore
	logger   *zap.Logger
	interval time.Duration
	now      func() time.Time
}

// NewCostRetentionReconciler creates a Workspace-driven cost retention reconciler.
func NewCostRetentionReconciler(s store.Store, costs CostRetentionStore, logger *zap.Logger) *CostRetentionReconciler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &CostRetentionReconciler{store: s, costs: costs, logger: logger.Named("cost-retention"), interval: defaultCostRetentionInterval, now: time.Now}
}

// Reconcile deletes expired cost records for one Workspace and schedules the next sweep.
func (r *CostRetentionReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	if r.store == nil || r.costs == nil {
		return Result{}, fmt.Errorf("cost retention requires resource and cost stores")
	}
	raw, err := r.store.Get(ctx, workspaceKind, req.Namespace, req.Name)
	if errors.Is(err, store.ErrNotFound) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("get workspace: %w", err)
	}
	var workspace v1alpha1.Workspace
	if err := json.Unmarshal(raw, &workspace); err != nil {
		return Result{}, fmt.Errorf("unmarshal workspace: %w", err)
	}
	if workspace.Spec.Retention == nil || workspace.Spec.Retention.CostRecordTTL.Duration <= 0 {
		return Result{}, nil
	}
	workspaceName := workspaceNamespace(&workspace)
	deleted, err := r.costs.DeleteCostRecordsBefore(ctx, workspaceName, r.now().UTC().Add(-workspace.Spec.Retention.CostRecordTTL.Duration))
	if err != nil {
		return Result{}, fmt.Errorf("delete expired cost records: %w", err)
	}
	if deleted > 0 {
		r.logger.Info("deleted expired cost records", zap.String("workspace", workspaceName), zap.Int64("records", deleted))
	}
	return Result{RequeueAfter: r.interval}, nil
}
