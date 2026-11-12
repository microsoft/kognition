// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

type toolPreparationReaperStore struct {
	runtimeapproval.ExpiredToolPreparationStore
	limits []int
	err    error
}

func (s *toolPreparationReaperStore) ListExpiredToolCallReservations(_ context.Context, limit int) ([]store.ToolCallReservation, error) {
	s.limits = append(s.limits, limit)
	return nil, s.err
}

func TestToolPreparationReaperRunnableUsesConfiguredBatch(t *testing.T) {
	lifecycle := &toolPreparationReaperStore{}
	runnable := NewToolPreparationReaperRunnable(lifecycle, nil, time.Minute, 25)
	runnable.sweep(context.Background())

	if len(lifecycle.limits) != 1 || lifecycle.limits[0] != 25 {
		t.Fatalf("discovery limits = %v, want [25]", lifecycle.limits)
	}
	if !runnable.NeedLeaderElection() {
		t.Fatal("Tool preparation reaper must require leader election")
	}
}

func TestToolPreparationReaperRunnableDefaultsInvalidScheduleAndRetries(t *testing.T) {
	lifecycle := &toolPreparationReaperStore{err: errors.New("temporary failure")}
	runnable := NewToolPreparationReaperRunnable(lifecycle, nil, 0, 0)
	if runnable.interval != DefaultToolPreparationReapInterval || runnable.limit != DefaultToolPreparationReapLimit {
		t.Fatalf("defaults = (%v, %d), want (%v, %d)", runnable.interval, runnable.limit, DefaultToolPreparationReapInterval, DefaultToolPreparationReapLimit)
	}

	runnable.sweep(context.Background())
	lifecycle.err = nil
	runnable.sweep(context.Background())
	if len(lifecycle.limits) != 2 {
		t.Fatalf("discovery calls = %d, want 2", len(lifecycle.limits))
	}
}
