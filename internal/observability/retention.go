// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"sync"
	"time"
)

// RetentionSweeper periodically deletes observability data that has exceeded
// per-workspace retention policies.
type RetentionSweeper struct {
	store    RetentionStore
	policies RetentionPolicySource
	interval time.Duration

	mu      sync.Mutex
	stopCh  chan struct{}
	running bool
}

// RetentionStore defines the operations needed to purge expired data.
type RetentionStore interface {
	// DeleteSpansBefore deletes trace spans older than the given time for a workspace.
	DeleteSpansBefore(ctx context.Context, workspace string, before time.Time) (int64, error)

	// DeleteAuditLogsBefore deletes audit entries older than the given time for a workspace.
	DeleteAuditLogsBefore(ctx context.Context, workspace string, before time.Time) (int64, error)

	// DeleteCostRecordsBefore deletes cost records older than the given time for a workspace.
	DeleteCostRecordsBefore(ctx context.Context, workspace string, before time.Time) (int64, error)

	// DeleteEventsBefore deletes instance events older than the given time for a workspace.
	DeleteEventsBefore(ctx context.Context, workspace string, before time.Time) (int64, error)
}

// RetentionPolicySource provides retention policies for each workspace.
type RetentionPolicySource interface {
	// ListWorkspaceRetentionPolicies returns workspace names with their retention config.
	ListWorkspaceRetentionPolicies(ctx context.Context) ([]WorkspaceRetention, error)
}

// WorkspaceRetention pairs a workspace name with its retention settings.
type WorkspaceRetention struct {
	Workspace     string
	TraceTTL      time.Duration
	AuditLogTTL   time.Duration
	CostRecordTTL time.Duration
	EventTTL      time.Duration
}

// SweepResult reports what was cleaned up in a single sweep.
type SweepResult struct {
	SpansDeleted       int64
	AuditLogsDeleted   int64
	CostRecordsDeleted int64
	EventsDeleted      int64
	Errors             []error
}

// NewRetentionSweeper creates a new sweeper with the given interval.
func NewRetentionSweeper(store RetentionStore, policies RetentionPolicySource, interval time.Duration) *RetentionSweeper {
	return &RetentionSweeper{
		store:    store,
		policies: policies,
		interval: interval,
	}
}

// Start begins the background sweep loop. It is safe to call multiple times;
// subsequent calls are no-ops until Stop is called.
func (s *RetentionSweeper) Start(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.stopCh = make(chan struct{})
	s.mu.Unlock()

	go s.loop(ctx)
}

// Stop halts the background sweep loop.
func (s *RetentionSweeper) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return
	}
	close(s.stopCh)
	s.running = false
}

// SweepOnce runs a single sweep pass. Useful for testing.
func (s *RetentionSweeper) SweepOnce(ctx context.Context) *SweepResult {
	return s.sweep(ctx)
}

func (s *RetentionSweeper) loop(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

func (s *RetentionSweeper) sweep(ctx context.Context) *SweepResult {
	result := &SweepResult{}
	now := time.Now().UTC()

	policies, err := s.policies.ListWorkspaceRetentionPolicies(ctx)
	if err != nil {
		result.Errors = append(result.Errors, err)
		return result
	}

	for _, p := range policies {
		if p.TraceTTL > 0 {
			cutoff := now.Add(-p.TraceTTL)
			n, err := s.store.DeleteSpansBefore(ctx, p.Workspace, cutoff)
			if err != nil {
				result.Errors = append(result.Errors, err)
			} else {
				result.SpansDeleted += n
			}
		}

		if p.AuditLogTTL > 0 {
			cutoff := now.Add(-p.AuditLogTTL)
			n, err := s.store.DeleteAuditLogsBefore(ctx, p.Workspace, cutoff)
			if err != nil {
				result.Errors = append(result.Errors, err)
			} else {
				result.AuditLogsDeleted += n
			}
		}

		if p.CostRecordTTL > 0 {
			cutoff := now.Add(-p.CostRecordTTL)
			n, err := s.store.DeleteCostRecordsBefore(ctx, p.Workspace, cutoff)
			if err != nil {
				result.Errors = append(result.Errors, err)
			} else {
				result.CostRecordsDeleted += n
			}
		}

		if p.EventTTL > 0 {
			cutoff := now.Add(-p.EventTTL)
			n, err := s.store.DeleteEventsBefore(ctx, p.Workspace, cutoff)
			if err != nil {
				result.Errors = append(result.Errors, err)
			} else {
				result.EventsDeleted += n
			}
		}
	}

	return result
}
