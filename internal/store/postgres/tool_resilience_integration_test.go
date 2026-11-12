// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/toolgateway"
)

func TestStandaloneToolResilienceSharesDurableState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	key := "tenant/tool@" + uuid.NewString()
	defer func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM tool_resilience_state WHERE tool_key = $1", key)
	}()
	newCoordinator := func() (*toolgateway.PostgresResilienceCoordinator, *pgxpool.Pool) {
		t.Helper()
		pool, err := pgxpool.New(ctx, testPool.Config().ConnString())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		coordinator, err := toolgateway.NewPostgresResilienceCoordinator(pool)
		if err != nil {
			t.Fatal(err)
		}
		return coordinator, pool
	}
	first, firstPool := newCoordinator()
	second, _ := newCoordinator()
	policy := &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1, Timeout: "1ms", CircuitBreaker: &v1alpha1.ToolCircuitBreaker{FailureThreshold: 1, Cooldown: "1m"}}
	now := time.Now()
	var workers sync.WaitGroup
	results := make(chan error, 8)
	for index := 0; index < 8; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			coordinator := first
			if index%2 == 0 {
				coordinator = second
			}
			_, err := coordinator.Acquire(ctx, key, policy, now)
			results <- err
		}(index)
	}
	workers.Wait()
	close(results)
	acquired := 0
	for err := range results {
		if err == nil {
			acquired++
		} else if !errors.Is(err, toolgateway.ErrToolConcurrencyLimit) {
			t.Fatal(err)
		}
	}
	if acquired != 1 {
		t.Fatalf("acquired %d slots; want exactly one", acquired)
	}
	firstPool.Close()
	restarted, _ := newCoordinator()
	if _, err := restarted.Acquire(ctx, key, policy, now.Add(10*time.Second)); !errors.Is(err, toolgateway.ErrToolConcurrencyLimit) {
		t.Fatalf("lost holder on restart: %v", err)
	}
	release, err := restarted.Acquire(ctx, key, policy, now.Add(61*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Record(ctx, key, policy, false, true, now.Add(61*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := release(ctx); err != nil {
		t.Fatalf("duplicate release: %v", err)
	}
	if _, err := second.Acquire(ctx, key, policy, now.Add(70*time.Second)); !errors.Is(err, toolgateway.ErrToolCircuitOpen) {
		t.Fatalf("lost shared circuit: %v", err)
	}
	release, err = second.Acquire(ctx, key, policy, now.Add(122*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Record(ctx, key, policy, true, false, now.Add(122*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Acquire(ctx, key, policy, now.Add(123*time.Second)); !errors.Is(err, toolgateway.ErrToolResilienceCoordination) {
		t.Fatalf("closed database must fail closed: %v", err)
	}
}

func TestStandaloneToolResilienceKeepsActiveHolders(t *testing.T) {
	for _, test := range []struct {
		name   string
		policy *v1alpha1.ToolExecutionPolicy
	}{
		{name: "long execution", policy: &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1, Timeout: "2m"}},
		{name: "retry budget", policy: &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1, Timeout: "30s", Idempotent: true, RetryMaxAttempts: 4}},
		{name: "unbounded execution", policy: &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			key := "tenant/tool@" + uuid.NewString()
			t.Cleanup(func() {
				_, _ = testPool.Exec(context.Background(), "DELETE FROM tool_resilience_state WHERE tool_key = $1", key)
			})
			first, err := toolgateway.NewPostgresResilienceCoordinator(testPool)
			if err != nil {
				t.Fatal(err)
			}
			second, err := toolgateway.NewPostgresResilienceCoordinator(testPool)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			release, err := first.Acquire(ctx, key, test.policy, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := second.Acquire(ctx, key, test.policy, now.Add(61*time.Second)); !errors.Is(err, toolgateway.ErrToolConcurrencyLimit) {
				t.Fatalf("active execution lost its concurrency slot: %v", err)
			}
			if err := release(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := second.Acquire(ctx, key, test.policy, now.Add(62*time.Second)); err != nil {
				t.Fatalf("released slot remained unavailable: %v", err)
			}
		})
	}
}
