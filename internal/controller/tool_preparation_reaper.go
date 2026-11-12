// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

const (
	DefaultToolPreparationReapInterval = 30 * time.Second
	DefaultToolPreparationReapLimit    = 100
)

type ToolPreparationReaperRunnable struct {
	lifecycle runtimeapproval.ExpiredToolPreparationStore
	logger    *zap.Logger
	interval  time.Duration
	limit     int
}

func NewToolPreparationReaperRunnable(lifecycle runtimeapproval.ExpiredToolPreparationStore, logger *zap.Logger, interval time.Duration, limit int) *ToolPreparationReaperRunnable {
	if logger == nil {
		logger = zap.NewNop()
	}
	if interval <= 0 {
		interval = DefaultToolPreparationReapInterval
	}
	if limit <= 0 {
		limit = DefaultToolPreparationReapLimit
	}
	return &ToolPreparationReaperRunnable{lifecycle: lifecycle, logger: logger, interval: interval, limit: limit}
}

func (r *ToolPreparationReaperRunnable) Start(ctx context.Context) error {
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

func (r *ToolPreparationReaperRunnable) NeedLeaderElection() bool { return true }

func (r *ToolPreparationReaperRunnable) sweep(ctx context.Context) {
	if r.lifecycle == nil {
		r.logger.Error("Tool preparation reaper is not configured")
		return
	}
	reaped, err := runtimeapproval.ReapExpiredToolPreparations(ctx, r.lifecycle, r.limit)
	if err != nil {
		r.logger.Warn("reap expired Tool preparations", zap.Error(err))
		return
	}
	if reaped > 0 {
		r.logger.Info("reaped expired Tool preparations", zap.Int("count", reaped))
	}
}
