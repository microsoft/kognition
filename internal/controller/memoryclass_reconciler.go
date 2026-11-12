// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

// MemoryClassReconciler validates MemoryClass specs and reports provisioner phase.
//
// MemoryClass is a cluster-scoped resource describing a class of memory backend
// (pgvector, redis, in-memory, mem0, etc.). The reconciler does not provision
// any backend; it merely surfaces a Ready/Failed phase so operators can confirm
// the class is wired correctly. Backend connectivity is validated lazily by
// the MemoryClaim provisioner and the memory-service.
type MemoryClassReconciler struct {
	store  store.Store
	logger *zap.Logger
}

func NewMemoryClassReconciler(s store.Store, logger *zap.Logger) *MemoryClassReconciler {
	return &MemoryClassReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "memoryclass")),
	}
}

func (r *MemoryClassReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	// MemoryClass is cluster-scoped.
	raw, err := r.store.Get(ctx, memoryClassKind, "", req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get MemoryClass: %w", err)
	}

	var mc v1alpha1.MemoryClass
	if err := json.Unmarshal(raw, &mc); err != nil {
		return Result{}, fmt.Errorf("unmarshal MemoryClass: %w", err)
	}

	oldPhase := mc.Status.Phase
	oldMessage := mc.Status.Message

	var (
		newPhase   v1alpha1.Phase
		newMessage string
		readyState lifecycle.ConditionStatus
		reason     string
	)
	if strings.TrimSpace(mc.Spec.BackendType) == "" {
		newPhase = v1alpha1.PhaseFailed
		newMessage = "spec.backendType is required"
		readyState = lifecycle.ConditionFalse
		reason = "MissingBackendType"
	} else {
		newPhase = v1alpha1.PhaseReady
		newMessage = fmt.Sprintf("memory class registered (backend %q)", mc.Spec.BackendType)
		readyState = lifecycle.ConditionTrue
		reason = "Registered"
	}

	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(mc.Status.Conditions),
		lifecycle.ConditionReady,
		readyState,
		reason,
		newMessage,
	)
	desiredConditions := lifecycleToAPIConditions(lc)
	if oldPhase == newPhase && oldMessage == newMessage && reflect.DeepEqual(mc.Status.Conditions, desiredConditions) {
		return Result{}, nil
	}

	mc.Status.Phase = newPhase
	mc.Status.Message = newMessage
	mc.Status.Conditions = desiredConditions

	data, err := json.Marshal(mc)
	if err != nil {
		return Result{}, fmt.Errorf("marshal MemoryClass: %w", err)
	}
	if err := r.store.Update(ctx, memoryClassKind, "", req.Name, data, mc.ObjectMeta.ResourceVersion); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return Result{Requeue: true}, nil
		}
		return Result{}, fmt.Errorf("update MemoryClass status: %w", err)
	}
	r.logger.Info("MemoryClass reconciled",
		zap.String("name", req.Name),
		zap.String("phase", string(newPhase)),
		requestid.ZapField(ctx))
	return Result{}, nil
}
