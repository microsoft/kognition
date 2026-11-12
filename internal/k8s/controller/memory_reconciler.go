// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	"go.uber.org/zap"
	ctrl "sigs.k8s.io/controller-runtime"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

// MemoryClaimReconcilerAdapter wraps the internal MemoryClaimReconciler for
// MemoryClaim resources under controller-runtime.
type MemoryClaimReconcilerAdapter struct {
	internal *intctrl.MemoryClaimReconciler
	logger   *zap.Logger
	events   eventEmitter
}

// NewMemoryClaimReconcilerAdapter creates an adapter that delegates to the internal MemoryClaimReconciler.
func NewMemoryClaimReconcilerAdapter(s store.Store, logger *zap.Logger, opts ...intctrl.MemoryClaimReconcilerOption) *MemoryClaimReconcilerAdapter {
	return &MemoryClaimReconcilerAdapter{
		internal: intctrl.NewMemoryClaimReconciler(s, logger, opts...),
		logger:   logger,
	}
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *MemoryClaimReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "MemoryClaim",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the MemoryClaim reconciler with the controller-runtime manager.
func (a *MemoryClaimReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "MemoryClaim", "kognition-memoryclaim-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("memoryclaim").
		For(&k8s.MemoryClaim{}).
		Complete(a)
}

// MemoryClassReconcilerAdapter wraps the internal MemoryClassReconciler for
// MemoryClass resources under controller-runtime.
type MemoryClassReconcilerAdapter struct {
	internal *intctrl.MemoryClassReconciler
	logger   *zap.Logger
	events   eventEmitter
}

func NewMemoryClassReconcilerAdapter(s store.Store, logger *zap.Logger) *MemoryClassReconcilerAdapter {
	return &MemoryClassReconcilerAdapter{
		internal: intctrl.NewMemoryClassReconciler(s, logger),
		logger:   logger,
	}
}

func (a *MemoryClassReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "MemoryClass",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

func (a *MemoryClassReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "MemoryClass", "kognition-memoryclass-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("memoryclass").
		For(&k8s.MemoryClass{}).
		Complete(a)
}

// MemoryVolumeReconcilerAdapter wraps the internal MemoryVolumeReconciler for
// MemoryVolume resources under controller-runtime.
type MemoryVolumeReconcilerAdapter struct {
	internal *intctrl.MemoryVolumeReconciler
	logger   *zap.Logger
	events   eventEmitter
}

func NewMemoryVolumeReconcilerAdapter(s store.Store, logger *zap.Logger) *MemoryVolumeReconcilerAdapter {
	return &MemoryVolumeReconcilerAdapter{
		internal: intctrl.NewMemoryVolumeReconciler(s, logger),
		logger:   logger,
	}
}

func (a *MemoryVolumeReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "MemoryVolume",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

func (a *MemoryVolumeReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "MemoryVolume", "kognition-memoryvolume-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("memoryvolume").
		For(&k8s.MemoryVolume{}).
		Complete(a)
}

// MemoryGrantReconcilerAdapter wraps the internal MemoryGrantReconciler for
// MemoryGrant resources under controller-runtime.
type MemoryGrantReconcilerAdapter struct {
	internal *intctrl.MemoryGrantReconciler
	logger   *zap.Logger
	events   eventEmitter
}

func NewMemoryGrantReconcilerAdapter(s store.Store, logger *zap.Logger, options ...intctrl.MemoryGrantReconcilerOption) *MemoryGrantReconcilerAdapter {
	return &MemoryGrantReconcilerAdapter{
		internal: intctrl.NewMemoryGrantReconciler(s, logger, options...),
		logger:   logger,
	}
}

func (a *MemoryGrantReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "MemoryGrant",
	})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *MemoryGrantReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "MemoryGrant", "kognition-memorygrant-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("memorygrant").
		For(&k8s.MemoryGrant{}).
		Complete(a)
}

// MemoryRecordReconcilerAdapter validates and normalizes MemoryRecord resources.
type MemoryRecordReconcilerAdapter struct {
	internal *intctrl.MemoryRecordReconciler
	events   eventEmitter
}

func NewMemoryRecordReconcilerAdapter(s store.Store, logger *zap.Logger) *MemoryRecordReconcilerAdapter {
	return &MemoryRecordReconcilerAdapter{internal: intctrl.NewMemoryRecordReconciler(s, logger)}
}

func (a *MemoryRecordReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "MemoryRecord",
	})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *MemoryRecordReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "MemoryRecord", "kognition-memoryrecord-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("memoryrecord").
		For(&k8s.MemoryRecord{}).
		Complete(a)
}

// MemorySummarizationReconcilerAdapter summarizes a Memory when it changes.
type MemorySummarizationReconcilerAdapter struct {
	internal *intctrl.MemorySummarizationReconciler
	events   eventEmitter
}

func NewMemorySummarizationReconcilerAdapter(s store.Store, logger *zap.Logger) *MemorySummarizationReconcilerAdapter {
	return &MemorySummarizationReconcilerAdapter{internal: intctrl.NewMemorySummarizationReconciler(s, logger)}
}

func (a *MemorySummarizationReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "Memory",
	})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *MemorySummarizationReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "Memory", "kognition-memory-summarization-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("memory-summarization").
		For(&k8s.Memory{}).
		Complete(a)
}

// MemoryRecordSummarizationReconcilerAdapter triggers Memory summarization
// whenever a MemoryRecord changes.
type MemoryRecordSummarizationReconcilerAdapter struct {
	internal *intctrl.MemorySummarizationReconciler
	events   eventEmitter
}

func NewMemoryRecordSummarizationReconcilerAdapter(s store.Store, logger *zap.Logger) *MemoryRecordSummarizationReconcilerAdapter {
	return &MemoryRecordSummarizationReconcilerAdapter{internal: intctrl.NewMemorySummarizationReconciler(s, logger)}
}

func (a *MemoryRecordSummarizationReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "MemoryRecord",
	})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *MemoryRecordSummarizationReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "MemoryRecord", "kognition-memory-summarization-from-record-controller")
	return ctrl.NewControllerManagedBy(mgr).
		Named("memory-summarization-from-record").
		For(&k8s.MemoryRecord{}).
		Complete(a)
}
