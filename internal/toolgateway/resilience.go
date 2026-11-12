// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtime"
)

var (
	ErrToolConcurrencyLimit = errors.New("tool concurrency limit reached")
	ErrToolCircuitOpen      = errors.New("tool circuit breaker is open")
	// ErrToolResilienceCoordination means the shared resilience coordinator
	// could not durably acquire, update, or release Tool execution state.
	ErrToolResilienceCoordination = errors.New("tool resilience coordination failed")
)

// ResilienceCoordinator controls per-Tool concurrency and circuit state. The
// default implementation is process-local for standalone mode; Kubernetes
// injects a Lease-backed implementation shared by gateway replicas.
type ResilienceCoordinator interface {
	Acquire(context.Context, string, *v1alpha1.ToolExecutionPolicy, time.Time) (func(context.Context) error, error)
	Record(context.Context, string, *v1alpha1.ToolExecutionPolicy, bool, bool, time.Time) error
}

type resilienceManager struct {
	mu       sync.Mutex
	inflight map[string]int
	circuits map[string]circuitState
}

type circuitState struct {
	failures  int32
	openUntil time.Time
}

func newResilienceManager() *resilienceManager {
	return &resilienceManager{inflight: make(map[string]int), circuits: make(map[string]circuitState)}
}

func toolResilienceKey(tool *v1alpha1.Tool) string {
	return fmt.Sprintf("%s/%s@%d", tool.ObjectMeta.Namespace, tool.ObjectMeta.Name, tool.ObjectMeta.ResourceVersion)
}

func (m *resilienceManager) Acquire(_ context.Context, key string, policy *v1alpha1.ToolExecutionPolicy, now time.Time) (func(context.Context) error, error) {
	if policy == nil {
		return func(context.Context) error { return nil }, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if policy.CircuitBreaker != nil && !m.circuits[key].openUntil.IsZero() && now.Before(m.circuits[key].openUntil) {
		return nil, ErrToolCircuitOpen
	}
	if policy.MaxConcurrent > 0 && m.inflight[key] >= int(policy.MaxConcurrent) {
		return nil, ErrToolConcurrencyLimit
	}
	m.inflight[key]++
	return func(context.Context) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.inflight[key]--
		if m.inflight[key] == 0 {
			delete(m.inflight, key)
		}
		return nil
	}, nil
}

func (m *resilienceManager) Record(_ context.Context, key string, policy *v1alpha1.ToolExecutionPolicy, succeeded, transient bool, now time.Time) error {
	if policy == nil || policy.CircuitBreaker == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.circuits[key]
	if succeeded {
		delete(m.circuits, key)
		return nil
	}
	if !transient {
		return nil
	}
	state.failures++
	if state.failures >= policy.CircuitBreaker.FailureThreshold {
		cooldown, err := time.ParseDuration(policy.CircuitBreaker.Cooldown)
		if err == nil {
			state.openUntil = now.Add(cooldown)
		}
	}
	m.circuits[key] = state
	return nil
}

func executionAttempts(policy *v1alpha1.ToolExecutionPolicy) int {
	if policy == nil || !policy.Idempotent || policy.RetryMaxAttempts < 2 {
		return 1
	}
	return int(policy.RetryMaxAttempts)
}

func executionTimeout(policy *v1alpha1.ToolExecutionPolicy) time.Duration {
	if policy == nil || policy.Timeout == "" {
		return 0
	}
	timeout, err := time.ParseDuration(policy.Timeout)
	if err != nil {
		return 0
	}
	return timeout
}

func toolResilienceHolderExpiry(ctx context.Context, policy *v1alpha1.ToolExecutionPolicy, now time.Time, grace time.Duration) time.Time {
	var deadline time.Time
	timeout := executionTimeout(policy)
	attempts := time.Duration(executionAttempts(policy))
	if timeout > 0 && attempts > 0 && timeout <= time.Duration(math.MaxInt64)/attempts {
		deadline = now.Add(timeout * attempts)
	}
	if contextDeadline, ok := ctx.Deadline(); ok && (deadline.IsZero() || contextDeadline.Before(deadline)) {
		deadline = contextDeadline
	}
	if deadline.IsZero() {
		return time.Time{}
	}
	if deadline.Before(now) {
		deadline = now
	}
	return deadline.Add(grace)
}

func isTransientToolError(err error) bool {
	var structured *runtime.StructuredToolError
	return errors.As(err, &structured) && structured.Retryable
}
