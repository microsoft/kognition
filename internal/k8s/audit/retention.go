// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package audit

import (
	"context"
	"time"

	"go.uber.org/zap"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

// RetentionRunnable deletes expired audit batches according to Workspace retention policies.
type RetentionRunnable struct {
	client   client.Client
	store    *ConfigMapStore
	logger   *zap.Logger
	interval time.Duration
	now      func() time.Time
}

// NewRetentionRunnable constructs the leader-elected Kubernetes audit retention worker.
func NewRetentionRunnable(client client.Client, store *ConfigMapStore, logger *zap.Logger, interval time.Duration) *RetentionRunnable {
	if logger == nil {
		logger = zap.NewNop()
	}
	if interval <= 0 {
		interval = time.Hour
	}
	return &RetentionRunnable{client: client, store: store, logger: logger.Named("audit-retention"), interval: interval, now: time.Now}
}

// Start runs an initial sweep, then sweeps until the manager stops.
func (r *RetentionRunnable) Start(ctx context.Context) error {
	r.sweep(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

// NeedLeaderElection ensures only one controller replica deletes audit records.
func (*RetentionRunnable) NeedLeaderElection() bool { return true }

func (r *RetentionRunnable) sweep(ctx context.Context) {
	if r.client == nil || r.store == nil {
		r.logger.Error("audit retention is not configured")
		return
	}
	var workspaces k8s.WorkspaceList
	if err := r.client.List(ctx, &workspaces); err != nil {
		r.logger.Error("list workspaces for audit retention", zap.Error(err))
		return
	}
	for index := range workspaces.Items {
		workspace := workspaces.Items[index]
		policy := workspace.Spec.Retention
		if policy == nil || policy.AuditLogTTL == "" {
			continue
		}
		ttl, err := time.ParseDuration(policy.AuditLogTTL)
		if err != nil || ttl <= 0 {
			r.logger.Warn("skip invalid audit retention TTL", zap.String("workspace", workspaceNamespace(&workspace)), zap.String("ttl", policy.AuditLogTTL), zap.Error(err))
			continue
		}
		workspaceName := workspaceNamespace(&workspace)
		deleted, err := r.store.DeleteAuditLogsBefore(ctx, workspaceName, r.now().UTC().Add(-ttl))
		if err != nil {
			r.logger.Error("delete expired audit records", zap.String("workspace", workspaceName), zap.Error(err))
			continue
		}
		if deleted > 0 {
			r.logger.Info("deleted expired audit records", zap.String("workspace", workspaceName), zap.Int64("entries", deleted))
		}
	}
}

func workspaceNamespace(workspace *k8s.Workspace) string {
	if workspace.Namespace != "" {
		return workspace.Namespace
	}
	return workspace.Name
}
