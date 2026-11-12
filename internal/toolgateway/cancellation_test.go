// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type cancellationLeaseClient struct {
	client.Client
	cleanup  bool
	contexts []context.Context
}

type cancellationContextKey struct{}

func (leaseClient *cancellationLeaseClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if leaseClient.cleanup {
		deadline, bounded := ctx.Deadline()
		if !bounded || time.Until(deadline) > 5*time.Second {
			return errors.New("cleanup must have a bounded deadline of at most five seconds")
		}
		if ctx.Value(cancellationContextKey{}) != "request-value" {
			return errors.New("cleanup lost request context values")
		}
		leaseClient.contexts = append(leaseClient.contexts, ctx)
	}
	return leaseClient.Client.Get(ctx, key, object, options...)
}

type failingRecordCoordinator struct {
	ResilienceCoordinator
	err             error
	waitForDeadline bool
}

func (coordinator failingRecordCoordinator) Record(ctx context.Context, _ string, _ *v1alpha1.ToolExecutionPolicy, _, _ bool, _ time.Time) error {
	if coordinator.waitForDeadline {
		deadline, bounded := ctx.Deadline()
		if !bounded || time.Until(deadline) > 5*time.Second {
			return errors.New("recording must have a bounded deadline of at most five seconds")
		}
		<-ctx.Done()
		return ctx.Err()
	}
	return coordinator.err
}

func TestGatewayCancellationReleasesConcurrencyHolder(t *testing.T) {
	recordFailure := errors.New("record unavailable")
	for _, test := range []struct {
		name                  string
		executionError        error
		recordError           error
		waitForRecordDeadline bool
	}{
		{name: "execution succeeds after cancellation"},
		{name: "execution fails after cancellation", executionError: context.Canceled},
		{name: "record fails after cancellation", executionError: context.Canceled, recordError: recordFailure},
		{name: "record timeout leaves release a fresh budget", executionError: context.Canceled, recordError: context.DeadlineExceeded, waitForRecordDeadline: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), cancellationContextKey{}, "request-value"))
			defer cancel()
			leaseClient := &cancellationLeaseClient{Client: newLeaseResilienceTestClient(t)}
			coordinator := newLeaseResilienceTestCoordinator(t, leaseClient, "gateway-a")
			policy := &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1, CircuitBreaker: &v1alpha1.ToolCircuitBreaker{FailureThreshold: 2, Cooldown: "1m"}}
			tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: "team-a", ResourceVersion: 7}, Spec: v1alpha1.ToolSpec{ExecutionPolicy: policy}}
			executor := &gatewayExecutor{output: map[string]any{"ok": true}, err: test.executionError}
			gateway := testGateway(t, &gatewayAuthorizer{}, &gatewayQuota{}, executor, &gatewayLedger{}, 7)
			storeGatewayObject(t, gateway.store.(*gatewayStore), "Tool", "team-a", "crm-update", tool)
			gateway.resilience = coordinator
			if test.recordError != nil {
				gateway.resilience = failingRecordCoordinator{ResilienceCoordinator: coordinator, err: test.recordError, waitForDeadline: test.waitForRecordDeadline}
			}
			executor.before = func() {
				cancel()
				if _, err := coordinator.Acquire(context.Background(), toolResilienceKey(&tool), policy, time.Now().Add(24*time.Hour)); !errors.Is(err, ErrToolConcurrencyLimit) {
					t.Fatalf("slot released before executor returned: %v", err)
				}
				leaseClient.cleanup = true
			}
			request := Request{Namespace: "team-a", Principal: "planner", ToolName: "crm-update", ToolResourceVersion: 7, InvocationID: "canceled", Input: map[string]any{}}
			_, err := gateway.Invoke(ctx, request)
			if test.executionError == nil && err != nil {
				t.Fatalf("successful execution lost its result: %v", err)
			}
			if test.executionError != nil && !errors.Is(err, test.executionError) {
				t.Fatalf("execution error lost: %v", err)
			}
			if test.recordError != nil && !errors.Is(err, test.recordError) {
				t.Fatalf("recording error lost: %v", err)
			}
			if len(leaseClient.contexts) == 0 {
				t.Fatal("cleanup never reached the lease store with a live bounded context")
			}
			for _, cleanupCtx := range leaseClient.contexts {
				if cleanupCtx.Err() == nil {
					t.Fatal("cleanup context was not canceled on return")
				}
			}
			leaseClient.cleanup = false
			executor.before, executor.err = nil, nil
			gateway.resilience = coordinator
			request.InvocationID = "next"
			response, err := gateway.Invoke(context.Background(), request)
			if err != nil || response.Output["ok"] != true || executor.calls != 2 {
				t.Fatalf("next invocation could not use released slot: response=%+v err=%v calls=%d", response, err, executor.calls)
			}
		})
	}
}
