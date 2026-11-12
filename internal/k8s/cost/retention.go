// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cost

import (
	"context"
	"time"

	"go.uber.org/zap"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

// RetentionRunnable deletes expired cost record batches according to Workspace policy.
type RetentionRunnable struct {
	client   client.Client
	store    *ConfigMapStore
	logger   *zap.Logger
	interval time.Duration
	now      func() time.Time
}

// NewRetentionRunnable constructs the leader-elected Kubernetes cost retention worker.
func NewRetentionRunnable(kubernetesClient client.Client, store *ConfigMapStore, logger *zap.Logger, interval time.Duration) *RetentionRunnable {
	if logger == nil {
		logger = zap.NewNop()
	}
	if interval <= 0 {
		interval = time.Hour
	}
	return &RetentionRunnable{client: kubernetesClient, store: store, logger: logger.Named("cost-retention"), interval: interval, now: time.Now}
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

// NeedLeaderElection ensures only one controller replica deletes cost records.
func (*RetentionRunnable) NeedLeaderElection() bool { return true }

func (r *RetentionRunnable) sweep(ctx context.Context) {
	if r.client == nil || r.store == nil {
		r.logger.Error("cost retention is not configured")
		return
	}
	var workspaces k8s.WorkspaceList
	if err := r.client.List(ctx, &workspaces); err != nil {
		r.logger.Error("list workspaces for cost retention", zap.Error(err))
		return
	}
	for index := range workspaces.Items {
		workspace := workspaces.Items[index]
		if workspace.Spec.Retention == nil || workspace.Spec.Retention.CostRecordTTL == "" {
			continue
		}
		ttl, err := time.ParseDuration(workspace.Spec.Retention.CostRecordTTL)
		if err != nil || ttl <= 0 {
			r.logger.Warn("skip invalid cost retention TTL", zap.String("workspace", workspaceNamespace(&workspace)), zap.String("ttl", workspace.Spec.Retention.CostRecordTTL), zap.Error(err))
			continue
		}
		workspaceName := workspaceNamespace(&workspace)
		deleted, err := r.store.DeleteCostRecordsBefore(ctx, workspaceName, r.now().UTC().Add(-ttl))
		if err != nil {
			r.logger.Error("delete expired cost records", zap.String("workspace", workspaceName), zap.Error(err))
			continue
		}
		if deleted > 0 {
			r.logger.Info("deleted expired cost records", zap.String("workspace", workspaceName), zap.Int64("records", deleted))
		}
	}
}

func workspaceNamespace(workspace *k8s.Workspace) string {
	if workspace.Namespace != "" {
		return workspace.Namespace
	}
	return workspace.Name
}
