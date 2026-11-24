// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kognition-ai/kognition/internal/store"
)

var _ store.ModelCallHealthStore = (*Store)(nil)

// modelCallHealthRetention removes records of reporters that stopped publishing
// without clearing their records, such as a crashed process.
const modelCallHealthRetention = time.Hour

func (s *Store) ReplaceModelCallHealth(ctx context.Context, reporter string, records []store.ModelCallHealth) error {
	if reporter == "" {
		return fmt.Errorf("model call health requires a reporter")
	}
	if len(records) > store.MaxModelCallHealthTargets {
		return fmt.Errorf("model call health accepts at most %d targets per reporter", store.MaxModelCallHealthTargets)
	}
	// A nil array would be sent as NULL and make the delete below match nothing.
	targets := make([]string, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		if err := validateModelCallHealth(record); err != nil {
			return err
		}
		if _, duplicate := seen[record.Target]; duplicate {
			return fmt.Errorf("model call health target %q is published twice", record.Target)
		}
		seen[record.Target] = struct{}{}
		targets = append(targets, record.Target)
	}

	transaction, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin model call health replace: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	if _, err := transaction.Exec(ctx, `DELETE FROM model_call_health WHERE reporter = $1 AND NOT (target = ANY($2))`, reporter, targets); err != nil {
		return fmt.Errorf("remove retired model call health: %w", err)
	}
	batch := &pgx.Batch{}
	for _, record := range records {
		batch.Queue(`INSERT INTO model_call_health
			(reporter, target, calls, faults, consecutive_faults, consecutive_successes, last_fault_reason, last_fault_at, last_success_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, transaction_timestamp())
			ON CONFLICT (reporter, target) DO UPDATE SET
				calls = EXCLUDED.calls, faults = EXCLUDED.faults,
				consecutive_faults = EXCLUDED.consecutive_faults, consecutive_successes = EXCLUDED.consecutive_successes,
				last_fault_reason = EXCLUDED.last_fault_reason, last_fault_at = EXCLUDED.last_fault_at,
				last_success_at = EXCLUDED.last_success_at, updated_at = EXCLUDED.updated_at`,
			reporter, record.Target, record.Calls, record.Faults, record.ConsecutiveFaults, record.ConsecutiveSuccesses,
			record.LastFaultReason, nullableTime(record.LastFaultAt), nullableTime(record.LastSuccessAt))
	}
	if batch.Len() > 0 {
		results := transaction.SendBatch(ctx, batch)
		for range records {
			if _, err := results.Exec(); err != nil {
				_ = results.Close()
				return fmt.Errorf("publish model call health: %w", err)
			}
		}
		if err := results.Close(); err != nil {
			return fmt.Errorf("publish model call health: %w", err)
		}
	}
	if _, err := transaction.Exec(ctx, `DELETE FROM model_call_health WHERE updated_at < transaction_timestamp() - make_interval(secs => $1)`,
		modelCallHealthRetention.Seconds()); err != nil {
		return fmt.Errorf("expire model call health: %w", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("commit model call health: %w", err)
	}
	return nil
}

func (s *Store) ListModelCallHealth(ctx context.Context, maxAge time.Duration) ([]store.ModelCallHealth, error) {
	rows, err := s.pool.Query(ctx, `SELECT target, calls, faults, consecutive_faults, consecutive_successes,
			last_fault_reason, last_fault_at, last_success_at
		FROM model_call_health
		WHERE updated_at >= transaction_timestamp() - make_interval(secs => $1)
		ORDER BY target, reporter`, maxAge.Seconds())
	if err != nil {
		return nil, fmt.Errorf("list model call health: %w", err)
	}
	defer rows.Close()

	var records []store.ModelCallHealth
	for rows.Next() {
		var record store.ModelCallHealth
		var faultAt, successAt *time.Time
		if err := rows.Scan(&record.Target, &record.Calls, &record.Faults, &record.ConsecutiveFaults, &record.ConsecutiveSuccesses,
			&record.LastFaultReason, &faultAt, &successAt); err != nil {
			return nil, fmt.Errorf("scan model call health: %w", err)
		}
		if faultAt != nil {
			record.LastFaultAt = *faultAt
		}
		if successAt != nil {
			record.LastSuccessAt = *successAt
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list model call health: %w", err)
	}
	return records, nil
}

func validateModelCallHealth(record store.ModelCallHealth) error {
	switch {
	case record.Target == "" || len(record.Target) > store.MaxModelCallHealthTargetLength:
		return fmt.Errorf("model call health target must be 1 to %d characters", store.MaxModelCallHealthTargetLength)
	case len(record.LastFaultReason) > store.MaxModelCallHealthReasonLength:
		return fmt.Errorf("model call health reason must be at most %d characters", store.MaxModelCallHealthReasonLength)
	case record.Calls < 0 || record.Faults < 0 || record.Faults > record.Calls:
		return fmt.Errorf("model call health requires 0 <= faults <= calls")
	case record.ConsecutiveFaults < 0 || record.ConsecutiveSuccesses < 0:
		return fmt.Errorf("model call health streaks must not be negative")
	}
	return nil
}
