// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	runtimeApprovalWorkspaceLeaseName     = "kognition-runtime-approval-records"
	runtimeApprovalWorkspaceLockSeconds   = int32(30)
	runtimeApprovalWorkspaceAcquireWait   = 2 * time.Second
	runtimeApprovalWorkspaceRetryPeriod   = 25 * time.Millisecond
	runtimeApprovalWorkspaceReleasePeriod = 5 * time.Second
	runtimeApprovalWorkspaceWritePeriod   = 10 * time.Second
)

var errRuntimeApprovalWorkspaceLeaseHeld = errors.New("RuntimeApproval workspace Lease is held")

// acquireRuntimeApprovalWorkspaceLease serializes retention and quota-ticket
// mutations for one workspace. A held or ambiguous Lease fails closed; the
// caller retries through normal reconciliation.
func (s *StoreAdapter) acquireRuntimeApprovalWorkspaceLease(ctx context.Context, workspace string) (*coordinationv1.Lease, error) {
	if workspace == "" {
		return nil, fmt.Errorf("RuntimeApproval workspace Lease requires workspace")
	}
	key := client.ObjectKey{Namespace: workspace, Name: runtimeApprovalWorkspaceLeaseName}
	now := metav1.NewMicroTime(s.runtimeApprovalNow())
	holder := uuid.NewString()
	lease := &coordinationv1.Lease{}
	err := s.runtimeApprovalReader.Get(ctx, key, lease)
	if apierrors.IsNotFound(err) {
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: workspace, Name: key.Name, Labels: map[string]string{"app.kubernetes.io/managed-by": "kognition"}},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &holder,
				LeaseDurationSeconds: int32Pointer(runtimeApprovalWorkspaceLockSeconds),
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		}
		if createErr := s.client.Create(ctx, lease); createErr == nil {
			return lease, nil
		}
		lease = &coordinationv1.Lease{}
		err = s.runtimeApprovalReader.Get(ctx, key, lease)
	}
	if err != nil {
		return nil, fmt.Errorf("read RuntimeApproval workspace Lease: %w", err)
	}
	if bindingLeaseHeld(lease, s.runtimeApprovalNow()) {
		return nil, errRuntimeApprovalWorkspaceLeaseHeld
	}
	lease.Spec.HolderIdentity = &holder
	lease.Spec.LeaseDurationSeconds = int32Pointer(runtimeApprovalWorkspaceLockSeconds)
	lease.Spec.AcquireTime = &now
	lease.Spec.RenewTime = &now
	if err := s.client.Update(ctx, lease); err != nil {
		return nil, fmt.Errorf("acquire RuntimeApproval workspace Lease: %w", err)
	}
	return lease, nil
}

func (s *StoreAdapter) acquireRuntimeApprovalWorkspaceLeaseWithRetry(ctx context.Context, workspace string) (*coordinationv1.Lease, error) {
	deadline := time.NewTimer(runtimeApprovalWorkspaceAcquireWait)
	defer deadline.Stop()
	for {
		lease, err := s.acquireRuntimeApprovalWorkspaceLease(ctx, workspace)
		if err == nil || !errors.Is(err, errRuntimeApprovalWorkspaceLeaseHeld) {
			return lease, err
		}
		retry := time.NewTimer(runtimeApprovalWorkspaceRetryPeriod)
		select {
		case <-ctx.Done():
			retry.Stop()
			return nil, ctx.Err()
		case <-deadline.C:
			retry.Stop()
			return nil, err
		case <-retry.C:
		}
	}
}

func (s *StoreAdapter) releaseRuntimeApprovalWorkspaceLease(ctx context.Context, lease *coordinationv1.Lease) error {
	if lease == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeApprovalWorkspaceReleasePeriod)
	defer cancel()
	current := &coordinationv1.Lease{}
	key := client.ObjectKeyFromObject(lease)
	if err := s.runtimeApprovalReader.Get(cleanupCtx, key, current); err != nil {
		return fmt.Errorf("reread RuntimeApproval workspace Lease for release: %w", err)
	}
	if current.Spec.HolderIdentity == nil || lease.Spec.HolderIdentity == nil || *current.Spec.HolderIdentity != *lease.Spec.HolderIdentity {
		return fmt.Errorf("release RuntimeApproval workspace Lease: holder changed")
	}
	empty := ""
	now := metav1.NewMicroTime(s.runtimeApprovalNow())
	current.Spec.HolderIdentity = &empty
	current.Spec.RenewTime = &now
	if err := s.client.Update(cleanupCtx, current); err != nil {
		return fmt.Errorf("release RuntimeApproval workspace Lease: %w", err)
	}
	return nil
}

func (s *StoreAdapter) renewRuntimeApprovalWorkspaceLease(ctx context.Context, lease *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	if lease == nil || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return nil, fmt.Errorf("renew RuntimeApproval workspace Lease: holder is missing")
	}
	current := &coordinationv1.Lease{}
	if err := s.runtimeApprovalReader.Get(ctx, client.ObjectKeyFromObject(lease), current); err != nil {
		return nil, fmt.Errorf("renew RuntimeApproval workspace Lease: %w", err)
	}
	if current.Spec.HolderIdentity == nil || *current.Spec.HolderIdentity != *lease.Spec.HolderIdentity || !bindingLeaseHeld(current, s.runtimeApprovalNow()) {
		return nil, fmt.Errorf("renew RuntimeApproval workspace Lease: ownership lost")
	}
	now := metav1.NewMicroTime(s.runtimeApprovalNow())
	current.Spec.RenewTime = &now
	if err := s.client.Update(ctx, current); err != nil {
		return nil, fmt.Errorf("renew RuntimeApproval workspace Lease: %w", err)
	}
	return current, nil
}
