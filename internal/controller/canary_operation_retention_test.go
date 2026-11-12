// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/store"
)

type retentionOperationStore struct {
	store.CanaryOperationStore
	cutoff time.Time
	calls  int
	err    error
}

func (s *retentionOperationStore) CompactCanaryOperationsBefore(_ context.Context, cutoff time.Time) (int64, error) {
	s.calls++
	s.cutoff = cutoff
	return 1, s.err
}

func TestCanaryOperationRetentionConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		enabled bool
		wantErr bool
	}{
		{name: "default", want: DefaultCanaryOperationRetention, enabled: true},
		{name: "configured", value: "72h", want: 72 * time.Hour, enabled: true},
		{name: "disabled", value: "0", enabled: false},
		{name: "negative", value: "-1h", wantErr: true},
		{name: "invalid", value: "later", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, enabled, err := CanaryOperationRetention(test.value)
			if (err != nil) != test.wantErr || got != test.want || enabled != test.enabled {
				t.Fatalf("CanaryOperationRetention(%q) = (%v, %t, %v), want (%v, %t, error=%t)", test.value, got, enabled, err, test.want, test.enabled, test.wantErr)
			}
		})
	}
}

func TestCanaryOperationRetentionRunnableUsesConfiguredCutoff(t *testing.T) {
	operations := &retentionOperationStore{}
	runnable := NewCanaryOperationRetentionRunnable(operations, zap.NewNop(), 48*time.Hour, time.Hour)
	now := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.FixedZone("test", 3600))
	runnable.now = func() time.Time { return now }
	runnable.sweep(context.Background())

	if operations.calls != 1 || !operations.cutoff.Equal(now.UTC().Add(-48*time.Hour)) {
		t.Fatalf("sweep calls = %d, cutoff = %v", operations.calls, operations.cutoff)
	}
	if !runnable.NeedLeaderElection() {
		t.Fatal("canary operation retention must require leader election")
	}
}

func TestCanaryOperationRetentionRunnableRetriesAfterSweepError(t *testing.T) {
	operations := &retentionOperationStore{err: errors.New("temporary failure")}
	runnable := NewCanaryOperationRetentionRunnable(operations, zap.NewNop(), time.Hour, time.Hour)
	runnable.sweep(context.Background())
	operations.err = nil
	runnable.sweep(context.Background())
	if operations.calls != 2 {
		t.Fatalf("sweep calls = %d, want 2", operations.calls)
	}
}
