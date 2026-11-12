// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/store"
)

// reconcileDirect is a TEST-ONLY shim that emulates the pre-deprecation
// direct-mode dispatch behavior of Reconcile (load Workflow, ensure finalizer,
// invoke the execution engine). Production Reconcile is template-only; the
// engine is exercised in production exclusively via WorkflowRunReconciler.
// This shim preserves the extensive engine coverage in
// workflow_reconciler_test.go without rewriting every call site, and it also
// serves as a compact reference for the historical dispatch flow.
func (r *WorkflowReconciler) reconcileDirect(ctx context.Context, req Request) (Result, error) {
	raw, err := r.store.Get(ctx, "Workflow", req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get workflow: %w", err)
	}
	var wf v1alpha1.Workflow
	if err := json.Unmarshal(raw, &wf); err != nil {
		return Result{}, fmt.Errorf("unmarshal workflow: %w", err)
	}
	persistedStatus := snapshotWorkflowStatus(wf.Status)
	persistedAnnotations := cloneStringMap(wf.ObjectMeta.Annotations)
	persistedFinalizers := append([]string(nil), wf.ObjectMeta.Finalizers...)
	r.persistStatus = func(ctx context.Context, desired *v1alpha1.Workflow) error {
		if err := r.persistWorkflowState(ctx, desired, persistedStatus, persistedAnnotations, persistedFinalizers); err != nil {
			return err
		}
		persistedStatus = snapshotWorkflowStatus(desired.Status)
		persistedAnnotations = cloneStringMap(desired.ObjectMeta.Annotations)
		persistedFinalizers = append([]string(nil), desired.ObjectMeta.Finalizers...)
		return nil
	}
	if wf.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &wf)
	}
	if wf.Status.Phase == v1alpha1.PhaseReady || wf.Status.Phase == v1alpha1.PhaseFailed {
		return Result{}, nil
	}
	if !lifecycle.HasFinalizer(wf.ObjectMeta.Finalizers, WorkflowFinalizer) {
		finalizers, _ := lifecycle.AddFinalizer(wf.ObjectMeta.Finalizers, WorkflowFinalizer)
		wf.ObjectMeta.Finalizers = finalizers
		if err := r.updateWorkflow(ctx, &wf); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return Result{Requeue: true}, nil
	}
	return r.executeWorkflow(ctx, &wf)
}
