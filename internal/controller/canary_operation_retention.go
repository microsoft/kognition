// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/store"
)

const (
	DefaultCanaryOperationRetention = 30 * 24 * time.Hour
	DefaultCanaryCompactionInterval = time.Hour
)

// CanaryOperationRetention parses the controller-wide completed-operation retention setting.
func CanaryOperationRetention(value string) (time.Duration, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultCanaryOperationRetention, true, nil
	}
	retention, err := time.ParseDuration(value)
	if err != nil {
		return 0, false, fmt.Errorf("parse canary operation retention: %w", err)
	}
	if retention < 0 {
		return 0, false, fmt.Errorf("canary operation retention must not be negative")
	}
	return retention, retention > 0, nil
}

// CanaryOperationRetentionRunnable periodically replaces finalized operations with permanent tombstones.
type CanaryOperationRetentionRunnable struct {
	operations store.CanaryOperationStore
	logger     *zap.Logger
	retention  time.Duration
	interval   time.Duration
	now        func() time.Time
}

func NewCanaryOperationRetentionRunnable(operations store.CanaryOperationStore, logger *zap.Logger, retention, interval time.Duration) *CanaryOperationRetentionRunnable {
	if logger == nil {
		logger = zap.NewNop()
	}
	if interval <= 0 {
		interval = DefaultCanaryCompactionInterval
	}
	return &CanaryOperationRetentionRunnable{operations: operations, logger: logger.Named("canary-operation-retention"), retention: retention, interval: interval, now: time.Now}
}

func (r *CanaryOperationRetentionRunnable) Start(ctx context.Context) error {
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

func (*CanaryOperationRetentionRunnable) NeedLeaderElection() bool { return true }

func (r *CanaryOperationRetentionRunnable) sweep(ctx context.Context) {
	if r.operations == nil || r.retention <= 0 {
		r.logger.Error("canary operation retention is not configured")
		return
	}
	compacted, err := r.operations.CompactCanaryOperationsBefore(ctx, r.now().UTC().Add(-r.retention))
	if err != nil {
		r.logger.Error("compact completed canary operations", zap.Error(err))
		return
	}
	if compacted > 0 {
		r.logger.Info("compacted completed canary operations", zap.Int64("operations", compacted))
	}
}
