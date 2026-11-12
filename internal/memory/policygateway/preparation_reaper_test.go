// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package policygateway

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

type memoryPreparationReaperApprovals struct {
	runtimeapproval.MemoryPreparationApprovalStore
}

type memoryPreparationReaperReceipts struct {
	runtimeapproval.ExpiredMemoryPreparationReceiptStore
	limits []int
	err    error
}

func (s *memoryPreparationReaperApprovals) GetRuntimeApprovalByInvocation(context.Context, string, string) (api.RuntimeApproval, error) {
	return api.RuntimeApproval{}, nil
}

func (s *memoryPreparationReaperReceipts) ListExpiredMemoryMutations(_ context.Context, limit int) ([]store.MemoryMutationReceipt, error) {
	s.limits = append(s.limits, limit)
	return nil, s.err
}

func TestMemoryPreparationReaperUsesConfiguredBatch(t *testing.T) {
	receipts := &memoryPreparationReaperReceipts{}
	reaper := NewMemoryPreparationReaper(&memoryPreparationReaperApprovals{}, receipts, nil, time.Minute, 25)
	reaper.sweep(context.Background())

	if len(receipts.limits) != 1 || receipts.limits[0] != 25 {
		t.Fatalf("discovery limits = %v, want [25]", receipts.limits)
	}
}

func TestMemoryPreparationReaperDefaultsInvalidScheduleAndRetries(t *testing.T) {
	receipts := &memoryPreparationReaperReceipts{err: errors.New("temporary failure")}
	reaper := NewMemoryPreparationReaper(&memoryPreparationReaperApprovals{}, receipts, nil, 0, 0)
	if reaper.interval != DefaultMemoryPreparationReapInterval || reaper.limit != DefaultMemoryPreparationReapLimit {
		t.Fatalf("defaults = (%v, %d), want (%v, %d)", reaper.interval, reaper.limit, DefaultMemoryPreparationReapInterval, DefaultMemoryPreparationReapLimit)
	}

	reaper.sweep(context.Background())
	receipts.err = nil
	reaper.sweep(context.Background())
	if len(receipts.limits) != 2 {
		t.Fatalf("discovery calls = %d, want 2", len(receipts.limits))
	}
}
