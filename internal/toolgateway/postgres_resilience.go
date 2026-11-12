// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kognition-ai/kognition/api/v1alpha1"
)

type PostgresResilienceCoordinator struct {
	pool *pgxpool.Pool
}

func NewPostgresResilienceCoordinator(pool *pgxpool.Pool) (*PostgresResilienceCoordinator, error) {
	if pool == nil {
		return nil, errors.New("PostgreSQL resilience coordinator requires a pool")
	}
	return &PostgresResilienceCoordinator{pool: pool}, nil
}

func (coordinator *PostgresResilienceCoordinator) Acquire(ctx context.Context, key string, policy *v1alpha1.ToolExecutionPolicy, now time.Time) (func(context.Context) error, error) {
	if policy == nil || (policy.MaxConcurrent <= 0 && policy.CircuitBreaker == nil) {
		return func(context.Context) error { return nil }, nil
	}
	holderID := uuid.NewString()
	err := coordinator.mutate(ctx, key, func(state *leaseResilienceState) error {
		pruneExpiredHolders(state, now)
		if policy.CircuitBreaker != nil && state.OpenUntil != nil && now.Before(*state.OpenUntil) {
			return ErrToolCircuitOpen
		}
		if policy.MaxConcurrent > 0 && len(state.Holders) >= int(policy.MaxConcurrent) {
			return ErrToolConcurrencyLimit
		}
		state.Holders[holderID] = toolResilienceHolderExpiry(ctx, policy, now, defaultToolResilienceHold)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return func(releaseCtx context.Context) error {
		return coordinator.mutate(releaseCtx, key, func(state *leaseResilienceState) error {
			delete(state.Holders, holderID)
			return nil
		})
	}, nil
}

func (coordinator *PostgresResilienceCoordinator) Record(ctx context.Context, key string, policy *v1alpha1.ToolExecutionPolicy, succeeded, transient bool, now time.Time) error {
	if policy == nil || policy.CircuitBreaker == nil {
		return nil
	}
	return coordinator.mutate(ctx, key, func(state *leaseResilienceState) error {
		pruneExpiredHolders(state, now)
		if succeeded {
			state.Failures, state.OpenUntil = 0, nil
			return nil
		}
		if !transient {
			return nil
		}
		state.Failures++
		if state.Failures >= policy.CircuitBreaker.FailureThreshold {
			cooldown, err := time.ParseDuration(policy.CircuitBreaker.Cooldown)
			if err != nil {
				return fmt.Errorf("parse circuit cooldown: %w", err)
			}
			openUntil := now.Add(cooldown)
			state.OpenUntil = &openUntil
		}
		return nil
	})
}

func (coordinator *PostgresResilienceCoordinator) mutate(ctx context.Context, key string, mutate func(*leaseResilienceState) error) error {
	err := coordinator.transaction(ctx, key, mutate)
	if err == nil || errors.Is(err, ErrToolConcurrencyLimit) || errors.Is(err, ErrToolCircuitOpen) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrToolResilienceCoordination, err)
}

func (coordinator *PostgresResilienceCoordinator) transaction(ctx context.Context, key string, mutate func(*leaseResilienceState) error) error {
	transaction, err := coordinator.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	if _, err := transaction.Exec(ctx, `INSERT INTO tool_resilience_state (tool_key) VALUES ($1) ON CONFLICT DO NOTHING`, key); err != nil {
		return err
	}
	var raw json.RawMessage
	if err := transaction.QueryRow(ctx, `SELECT state FROM tool_resilience_state WHERE tool_key = $1 FOR UPDATE`, key).Scan(&raw); err != nil {
		return err
	}
	state := leaseResilienceState{}
	if err := json.Unmarshal(raw, &state); err != nil {
		return err
	}
	if state.Holders == nil {
		state.Holders = make(map[string]time.Time)
	}
	if err := mutate(&state); err != nil {
		return err
	}
	raw, err = json.Marshal(state)
	if err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `UPDATE tool_resilience_state SET state = $2, updated_at = transaction_timestamp() WHERE tool_key = $1`, key, raw); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}
