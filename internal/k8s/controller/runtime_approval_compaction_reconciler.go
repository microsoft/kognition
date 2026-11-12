// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

const runtimeApprovalRetentionRetry = 5 * time.Second

func RuntimeApprovalTombstoneRetention(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultRuntimeApprovalTombstoneRetention, nil
	}
	retention, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse RuntimeApproval tombstone retention: %w", err)
	}
	if retention < DefaultRuntimeApprovalTombstoneRetention {
		return 0, fmt.Errorf("RuntimeApproval tombstone retention must be at least %s", DefaultRuntimeApprovalTombstoneRetention)
	}
	return retention, nil
}

type RuntimeApprovalCompactionReconciler struct {
	client    client.Client
	store     *StoreAdapter
	logger    *zap.Logger
	retention time.Duration
}

func NewRuntimeApprovalCompactionReconciler(c client.Client, store *StoreAdapter, logger *zap.Logger, retention time.Duration) *RuntimeApprovalCompactionReconciler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &RuntimeApprovalCompactionReconciler{client: c, store: store, logger: logger.Named("runtime-approval-compaction"), retention: retention}
}

func (r *RuntimeApprovalCompactionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	object := &k8s.RuntimeApproval{}
	if err := r.client.Get(ctx, req.NamespacedName, object); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !runtimeApprovalHasFinalizer(object, k8s.RuntimeApprovalCompactionFinalizer) {
		if !object.DeletionTimestamp.IsZero() {
			// A legacy object deleted before the protection was established cannot
			// safely gain a finalizer after deletion has begun.
			return ctrl.Result{}, fmt.Errorf("deleting RuntimeApproval %s/%s lacks compaction finalizer", object.Namespace, object.Name)
		}
		base := object.DeepCopy()
		addRuntimeApprovalFinalizer(object, k8s.RuntimeApprovalCompactionFinalizer)
		if err := r.client.Patch(ctx, object, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("add RuntimeApproval compaction finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if !runtimeApprovalCompactable(object) {
		if !object.DeletionTimestamp.IsZero() {
			r.logger.Warn("blocking deletion of non-terminal RuntimeApproval", zap.String("namespace", object.Namespace), zap.String("name", object.Name))
		}
		return ctrl.Result{}, nil
	}
	if _, err := r.store.CreateOrVerifyRuntimeApprovalTombstone(ctx, object, r.retention); err != nil {
		return ctrl.Result{}, fmt.Errorf("persist RuntimeApproval tombstone: %w", err)
	}
	base := object.DeepCopy()
	removeRuntimeApprovalFinalizer(object, k8s.RuntimeApprovalCompactionFinalizer)
	if err := r.client.Patch(ctx, object, client.MergeFrom(base)); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("remove RuntimeApproval compaction finalizer: %w", err)
	}
	if err := r.client.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete compacted RuntimeApproval: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *RuntimeApprovalCompactionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("runtimeapproval-compaction").
		For(&k8s.RuntimeApproval{}).
		Complete(r)
}

type RuntimeApprovalRetentionReconciler struct {
	client client.Client
	store  *StoreAdapter
	logger *zap.Logger
	now    func() time.Time
}

func NewRuntimeApprovalRetentionReconciler(c client.Client, store *StoreAdapter, logger *zap.Logger) *RuntimeApprovalRetentionReconciler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &RuntimeApprovalRetentionReconciler{client: c, store: store, logger: logger.Named("runtime-approval-retention"), now: time.Now}
}

func (r *RuntimeApprovalRetentionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, resultErr error) {
	tombstone := &k8s.RuntimeApprovalTombstone{}
	if err := r.client.Get(ctx, req.NamespacedName, tombstone); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if tombstone.Spec.Workspace != tombstone.Namespace || tombstone.Name != runtimeapproval.CanonicalName(tombstone.Spec.Workspace, tombstone.Spec.InvocationID) {
		return ctrl.Result{}, fmt.Errorf("RuntimeApprovalTombstone identity does not match its logical key")
	}
	if !runtimeApprovalHasFinalizer(tombstone, k8s.RuntimeApprovalRetentionFinalizer) {
		if !tombstone.DeletionTimestamp.IsZero() {
			return ctrl.Result{}, fmt.Errorf("deleting RuntimeApprovalTombstone lacks retention finalizer")
		}
		base := tombstone.DeepCopy()
		addRuntimeApprovalFinalizer(tombstone, k8s.RuntimeApprovalRetentionFinalizer)
		if err := r.client.Patch(ctx, tombstone, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("repair RuntimeApprovalTombstone retention finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}
	now := r.now().UTC()
	if now.Before(tombstone.Spec.RetentionDeadline.Time) {
		return ctrl.Result{RequeueAfter: tombstone.Spec.RetentionDeadline.Time.Sub(now)}, nil
	}
	lease, err := r.store.acquireRuntimeApprovalWorkspaceLease(ctx, tombstone.Spec.Workspace)
	if err != nil {
		return ctrl.Result{RequeueAfter: runtimeApprovalRetentionRetry}, nil
	}
	defer func() {
		if err := r.store.releaseRuntimeApprovalWorkspaceLease(ctx, lease); err != nil && resultErr == nil {
			resultErr = err
		}
	}()

	// Repeat both reads while holding the workspace Lease. A live overlap means
	// compaction has not completed, so retention remains a conservative over-retain.
	current := &k8s.RuntimeApprovalTombstone{}
	if err := r.client.Get(ctx, req.NamespacedName, current); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if r.now().UTC().Before(current.Spec.RetentionDeadline.Time) {
		return ctrl.Result{RequeueAfter: current.Spec.RetentionDeadline.Time.Sub(r.now().UTC())}, nil
	}
	live := &k8s.RuntimeApproval{}
	if err := r.client.Get(ctx, req.NamespacedName, live); err == nil {
		if live.Spec.Workspace != current.Spec.Workspace || live.Spec.InvocationID != current.Spec.InvocationID {
			return ctrl.Result{}, fmt.Errorf("live RuntimeApproval conflicts with retained tombstone logical key")
		}
		return ctrl.Result{RequeueAfter: runtimeApprovalRetentionRetry}, nil
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("prove RuntimeApproval absence before tombstone retention: %w", err)
	}
	if !runtimeApprovalHasFinalizer(current, k8s.RuntimeApprovalRetentionFinalizer) {
		return ctrl.Result{}, fmt.Errorf("RuntimeApprovalTombstone lacks retention finalizer")
	}
	if current.DeletionTimestamp.IsZero() {
		if err := r.client.Delete(ctx, current); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("begin expired RuntimeApprovalTombstone deletion: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}
	lease, err = r.store.renewRuntimeApprovalWorkspaceLease(ctx, lease)
	if err != nil {
		return ctrl.Result{RequeueAfter: runtimeApprovalRetentionRetry}, nil
	}
	writeCtx, cancel := context.WithTimeout(ctx, runtimeApprovalWorkspaceWritePeriod)
	defer cancel()
	base := current.DeepCopy()
	removeRuntimeApprovalFinalizer(current, k8s.RuntimeApprovalRetentionFinalizer)
	if err := r.client.Patch(writeCtx, current, client.MergeFrom(base)); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("remove RuntimeApprovalTombstone retention finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *RuntimeApprovalRetentionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("runtimeapproval-retention").
		For(&k8s.RuntimeApprovalTombstone{}).
		Complete(r)
}
