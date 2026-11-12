// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/google/uuid"
	"github.com/kognition-ai/kognition/api/v1alpha1"
)

const (
	toolResilienceLeasePrefix     = "kognition-tool-resilience-"
	toolResilienceStateAnnotation = "kognition.dev/tool-resilience-state"
	toolResilienceLeaseRetries    = 8
	defaultToolResilienceHold     = time.Minute
)

type leaseResilienceState struct {
	Holders   map[string]time.Time `json:"holders,omitempty"`
	Failures  int32                `json:"failures,omitempty"`
	OpenUntil *time.Time           `json:"openUntil,omitempty"`
}

// KubernetesLeaseResilienceCoordinator stores global per-Tool concurrency and
// circuit state in a Lease. Every state change uses resource-version updates,
// so concurrent Tool Gateway replicas serialize their decisions through the
// Kubernetes API. Bounded holders expire after a crashed Pod; unbounded holders
// require explicit release.
type KubernetesLeaseResilienceCoordinator struct {
	client         client.Client
	namespace      string
	holderIdentity string
	holdDuration   time.Duration
	now            func() time.Time
	newHolderID    func() string
}

// NewKubernetesLeaseResilienceCoordinator creates shared Tool Gateway
// resilience coordination for one control namespace.
func NewKubernetesLeaseResilienceCoordinator(c client.Client, namespace, holderIdentity string) (*KubernetesLeaseResilienceCoordinator, error) {
	if c == nil {
		return nil, errors.New("Kubernetes resilience coordinator requires a client")
	}
	if namespace == "" || holderIdentity == "" {
		return nil, errors.New("Kubernetes resilience coordinator requires namespace and holder identity")
	}
	return &KubernetesLeaseResilienceCoordinator{
		client: c, namespace: namespace, holderIdentity: holderIdentity,
		holdDuration: defaultToolResilienceHold, now: time.Now, newHolderID: uuid.NewString,
	}, nil
}

func (c *KubernetesLeaseResilienceCoordinator) Acquire(ctx context.Context, key string, policy *v1alpha1.ToolExecutionPolicy, now time.Time) (func(context.Context) error, error) {
	if policy == nil || (policy.MaxConcurrent <= 0 && policy.CircuitBreaker == nil) {
		return func(context.Context) error { return nil }, nil
	}
	holderID := c.holderIdentity + "/" + c.newHolderID()
	err := c.mutate(ctx, key, now, func(state *leaseResilienceState) error {
		pruneExpiredHolders(state, now)
		if policy.CircuitBreaker != nil && state.OpenUntil != nil && now.Before(*state.OpenUntil) {
			return ErrToolCircuitOpen
		}
		if policy.MaxConcurrent > 0 && len(state.Holders) >= int(policy.MaxConcurrent) {
			return ErrToolConcurrencyLimit
		}
		state.Holders[holderID] = toolResilienceHolderExpiry(ctx, policy, now, c.holdDuration)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return func(releaseCtx context.Context) error {
		return c.mutate(releaseCtx, key, c.now(), func(state *leaseResilienceState) error {
			delete(state.Holders, holderID)
			return nil
		})
	}, nil
}

func (c *KubernetesLeaseResilienceCoordinator) Record(ctx context.Context, key string, policy *v1alpha1.ToolExecutionPolicy, succeeded, transient bool, now time.Time) error {
	if policy == nil || policy.CircuitBreaker == nil {
		return nil
	}
	return c.mutate(ctx, key, now, func(state *leaseResilienceState) error {
		pruneExpiredHolders(state, now)
		if succeeded {
			state.Failures = 0
			state.OpenUntil = nil
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

func (c *KubernetesLeaseResilienceCoordinator) mutate(ctx context.Context, key string, now time.Time, mutate func(*leaseResilienceState) error) error {
	leaseKey := client.ObjectKey{Namespace: c.namespace, Name: toolResilienceLeaseName(key)}
	for attempt := 0; attempt < toolResilienceLeaseRetries; attempt++ {
		lease := &coordinationv1.Lease{}
		err := c.client.Get(ctx, leaseKey, lease)
		if apierrors.IsNotFound(err) {
			state := leaseResilienceState{Holders: make(map[string]time.Time)}
			if err := mutate(&state); err != nil {
				return err
			}
			created, err := c.newLease(leaseKey, state, now)
			if err != nil {
				return err
			}
			if err = c.client.Create(ctx, created); err == nil {
				return nil
			} else if apierrors.IsAlreadyExists(err) {
				continue
			} else {
				return fmt.Errorf("%w: create Lease: %w", ErrToolResilienceCoordination, err)
			}
		}
		if err != nil {
			return fmt.Errorf("%w: get Lease: %w", ErrToolResilienceCoordination, err)
		}
		state, err := decodeLeaseResilienceState(lease)
		if err != nil {
			return err
		}
		if err := mutate(&state); err != nil {
			return err
		}
		if err := c.writeLeaseState(ctx, lease, state, now); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return fmt.Errorf("%w: update Lease: %w", ErrToolResilienceCoordination, err)
		}
		return nil
	}
	return fmt.Errorf("%w: exhausted optimistic update retries", ErrToolResilienceCoordination)
}

func (c *KubernetesLeaseResilienceCoordinator) newLease(key client.ObjectKey, state leaseResilienceState, now time.Time) (*coordinationv1.Lease, error) {
	stateBytes, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal Lease state: %w", ErrToolResilienceCoordination, err)
	}
	durationSeconds := int32(c.holdDuration.Seconds())
	holder := c.holderIdentity
	renewTime := metav1.NewMicroTime(now)
	return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Annotations: map[string]string{toolResilienceStateAnnotation: string(stateBytes)}}, Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &durationSeconds, RenewTime: &renewTime}}, nil
}

func (c *KubernetesLeaseResilienceCoordinator) writeLeaseState(ctx context.Context, lease *coordinationv1.Lease, state leaseResilienceState, now time.Time) error {
	stateBytes, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("%w: marshal Lease state: %w", ErrToolResilienceCoordination, err)
	}
	if lease.Annotations == nil {
		lease.Annotations = make(map[string]string)
	}
	lease.Annotations[toolResilienceStateAnnotation] = string(stateBytes)
	holder := c.holderIdentity
	renewTime := metav1.NewMicroTime(now)
	lease.Spec.HolderIdentity = &holder
	lease.Spec.RenewTime = &renewTime
	return c.client.Update(ctx, lease)
}

func decodeLeaseResilienceState(lease *coordinationv1.Lease) (leaseResilienceState, error) {
	state := leaseResilienceState{Holders: make(map[string]time.Time)}
	if lease.Annotations == nil || lease.Annotations[toolResilienceStateAnnotation] == "" {
		return state, nil
	}
	if err := json.Unmarshal([]byte(lease.Annotations[toolResilienceStateAnnotation]), &state); err != nil {
		return leaseResilienceState{}, fmt.Errorf("%w: decode Lease state: %w", ErrToolResilienceCoordination, err)
	}
	if state.Holders == nil {
		state.Holders = make(map[string]time.Time)
	}
	return state, nil
}

func pruneExpiredHolders(state *leaseResilienceState, now time.Time) {
	for holder, expiresAt := range state.Holders {
		if !expiresAt.IsZero() && !expiresAt.After(now) {
			delete(state.Holders, holder)
		}
	}
}

func toolResilienceLeaseName(key string) string {
	digest := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%s%x", toolResilienceLeasePrefix, digest[:12])
}
