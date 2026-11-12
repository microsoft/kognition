// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package policygateway

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

const (
	DefaultMemoryPreparationReapInterval = 30 * time.Second
	DefaultMemoryPreparationReapLimit    = 100
)

type MemoryPreparationReaper struct {
	approvals runtimeapproval.MemoryPreparationApprovalStore
	receipts  runtimeapproval.ExpiredMemoryPreparationReceiptStore
	logger    *zap.Logger
	interval  time.Duration
	limit     int
}

func NewMemoryPreparationReaper(approvals runtimeapproval.MemoryPreparationApprovalStore, receipts runtimeapproval.ExpiredMemoryPreparationReceiptStore, logger *zap.Logger, interval time.Duration, limit int) *MemoryPreparationReaper {
	if logger == nil {
		logger = zap.NewNop()
	}
	if interval <= 0 {
		interval = DefaultMemoryPreparationReapInterval
	}
	if limit <= 0 {
		limit = DefaultMemoryPreparationReapLimit
	}
	return &MemoryPreparationReaper{approvals: approvals, receipts: receipts, logger: logger, interval: interval, limit: limit}
}

func (r *MemoryPreparationReaper) Start(ctx context.Context) {
	r.sweep(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

func (r *MemoryPreparationReaper) sweep(ctx context.Context) {
	if r.approvals == nil || r.receipts == nil {
		r.logger.Error("Memory preparation reaper is not configured")
		return
	}
	reaped, err := runtimeapproval.ReapExpiredMemoryPreparations(ctx, r.approvals, r.receipts, r.limit)
	if err != nil {
		r.logger.Warn("reap expired Memory preparations", zap.Error(err))
		return
	}
	if reaped > 0 {
		r.logger.Info("reaped expired Memory preparations", zap.Int("count", reaped))
	}
}
