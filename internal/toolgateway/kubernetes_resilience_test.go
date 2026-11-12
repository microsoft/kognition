// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

type failingLeaseClient struct {
	client.Client
	err error
}

func (c failingLeaseClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return c.err
}

// TC-0296: The Kubernetes E2E verifies these shared decisions through two
// gateway Pods and a Pod replacement; this test covers the deterministic
// Lease mutation and handoff state transitions.
func TestKubernetesLeaseResilienceCoordinatorSharesConcurrencyAndCircuit(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	client := newLeaseResilienceTestClient(t)
	first := newLeaseResilienceTestCoordinator(t, client, "gateway-a")
	second := newLeaseResilienceTestCoordinator(t, client, "gateway-b")
	policy := &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1, CircuitBreaker: &v1alpha1.ToolCircuitBreaker{FailureThreshold: 1, Cooldown: "1m"}}

	release, err := first.Acquire(ctx, "tenant-a/payment@7", policy, now)
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	if _, err := second.Acquire(ctx, "tenant-a/payment@7", policy, now); !errors.Is(err, ErrToolConcurrencyLimit) {
		t.Fatalf("second Acquire() error = %v, want ErrToolConcurrencyLimit", err)
	}
	if err := release(ctx); err != nil {
		t.Fatalf("release() error = %v", err)
	}

	release, err = first.Acquire(ctx, "tenant-a/payment@7", policy, now)
	if err != nil {
		t.Fatalf("reacquire() error = %v", err)
	}
	if err := first.Record(ctx, "tenant-a/payment@7", policy, false, true, now); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if err := release(ctx); err != nil {
		t.Fatalf("release after failure error = %v", err)
	}
	if _, err := second.Acquire(ctx, "tenant-a/payment@7", policy, now.Add(10*time.Second)); !errors.Is(err, ErrToolCircuitOpen) {
		t.Fatalf("Acquire() with shared open circuit error = %v, want ErrToolCircuitOpen", err)
	}

	release, err = second.Acquire(ctx, "tenant-a/payment@7", policy, now.Add(61*time.Second))
	if err != nil {
		t.Fatalf("Acquire() after circuit cooldown error = %v", err)
	}
	if err := second.Record(ctx, "tenant-a/payment@7", policy, true, false, now.Add(61*time.Second)); err != nil {
		t.Fatalf("Record success() error = %v", err)
	}
	if err := release(ctx); err != nil {
		t.Fatalf("release after recovery error = %v", err)
	}
}

func TestKubernetesLeaseResilienceCoordinatorExpiresAbandonedHolder(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	client := newLeaseResilienceTestClient(t)
	first := newLeaseResilienceTestCoordinator(t, client, "gateway-a")
	second := newLeaseResilienceTestCoordinator(t, client, "gateway-b")
	first.holdDuration = 50 * time.Millisecond
	policy := &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1, Timeout: "1ms"}

	if _, err := first.Acquire(ctx, "tenant-a/report@3", policy, now); err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	if _, err := second.Acquire(ctx, "tenant-a/report@3", policy, now.Add(40*time.Millisecond)); !errors.Is(err, ErrToolConcurrencyLimit) {
		t.Fatalf("Acquire() before holder expiry error = %v, want ErrToolConcurrencyLimit", err)
	}
	release, err := second.Acquire(ctx, "tenant-a/report@3", policy, now.Add(60*time.Millisecond))
	if err != nil {
		t.Fatalf("Acquire() after abandoned holder expiry error = %v", err)
	}
	if err := release(ctx); err != nil {
		t.Fatalf("release() error = %v", err)
	}
}

func TestKubernetesLeaseResilienceCoordinatorFailsClosedOnAPIError(t *testing.T) {
	coordinator, err := NewKubernetesLeaseResilienceCoordinator(failingLeaseClient{err: errors.New("API unavailable")}, "kognition-system", "gateway-a")
	if err != nil {
		t.Fatal(err)
	}
	policy := &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1}
	if _, err := coordinator.Acquire(context.Background(), "tenant-a/report@3", policy, time.Now()); !errors.Is(err, ErrToolResilienceCoordination) {
		t.Fatalf("Acquire() error = %v, want ErrToolResilienceCoordination", err)
	}
}

func TestKubernetesLeaseResilienceCoordinatorKeepsActiveHolders(t *testing.T) {
	for _, test := range []struct {
		name     string
		policy   *v1alpha1.ToolExecutionPolicy
		deadline bool
	}{
		{name: "long execution", policy: &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1, Timeout: "2m"}},
		{name: "retry budget", policy: &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1, Timeout: "30s", Idempotent: true, RetryMaxAttempts: 4}},
		{name: "unbounded execution", policy: &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1}},
		{name: "context deadline", policy: &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1}, deadline: true},
		{name: "overflow fails closed", policy: &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1, Timeout: "2562047h47m16.854775807s", Idempotent: true, RetryMaxAttempts: 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			ctx := context.Background()
			if test.deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, now.Add(2*time.Minute))
				defer cancel()
			}
			leaseClient := newLeaseResilienceTestClient(t)
			first := newLeaseResilienceTestCoordinator(t, leaseClient, "gateway-a")
			second := newLeaseResilienceTestCoordinator(t, leaseClient, "gateway-b")
			release, err := first.Acquire(ctx, "tenant-a/report@3", test.policy, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := second.Acquire(ctx, "tenant-a/report@3", test.policy, now.Add(61*time.Second)); !errors.Is(err, ErrToolConcurrencyLimit) {
				t.Fatalf("active execution lost its concurrency slot: %v", err)
			}
			_, err = second.Acquire(ctx, "tenant-a/report@3", test.policy, now.Add(181*time.Second))
			if test.name == "unbounded execution" || test.name == "overflow fails closed" {
				if !errors.Is(err, ErrToolConcurrencyLimit) {
					t.Fatalf("unbounded execution holder must require release: %v", err)
				}
			} else if err != nil {
				t.Fatalf("bounded execution holder did not expire: %v", err)
			}
			if err := release(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func newLeaseResilienceTestClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

func newLeaseResilienceTestCoordinator(t *testing.T, client client.Client, holder string) *KubernetesLeaseResilienceCoordinator {
	t.Helper()
	coordinator, err := NewKubernetesLeaseResilienceCoordinator(client, "kognition-system", holder)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.newHolderID = func() string { return "hold" }
	return coordinator
}
