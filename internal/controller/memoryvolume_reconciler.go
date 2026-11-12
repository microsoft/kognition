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

// MemoryVolumeReconciler reports the phase of a MemoryVolume based on its binding.
//
// MemoryClaimReconciler is the authoritative writer of MemoryVolume lifecycle
// transitions (Bound on provision, Released/Archived on reclaim). This
// reconciler is a safety net that ensures every MemoryVolume has a non-empty
// phase even when it was created out-of-band (e.g., manually for testing).
type MemoryVolumeReconciler struct {
	store  store.Store
	logger *zap.Logger
}

func NewMemoryVolumeReconciler(s store.Store, logger *zap.Logger) *MemoryVolumeReconciler {
	return &MemoryVolumeReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "memoryvolume")),
	}
}

func (r *MemoryVolumeReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	raw, err := r.store.Get(ctx, memoryVolumeKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get MemoryVolume: %w", err)
	}

	var mv v1alpha1.MemoryVolume
	if err := json.Unmarshal(raw, &mv); err != nil {
		return Result{}, fmt.Errorf("unmarshal MemoryVolume: %w", err)
	}

	oldPhase := mv.Status.Phase

	var (
		newPhase   v1alpha1.Phase
		readyState lifecycle.ConditionStatus
		reason     string
		message    string
	)
	switch {
	case strings.TrimSpace(mv.Spec.MemoryClassName) == "":
		newPhase = v1alpha1.PhaseFailed
		readyState = lifecycle.ConditionFalse
		reason = "MissingMemoryClass"
		message = "spec.memoryClassName is required"
	case mv.Spec.ClaimRef != nil && mv.Spec.ClaimRef.Name != "":
		// Don't downgrade Released/Archived terminal states.
		if oldPhase == v1alpha1.PhaseReleased || oldPhase == v1alpha1.PhaseArchived {
			newPhase = oldPhase
		} else {
			newPhase = v1alpha1.PhaseBound
		}
		readyState = lifecycle.ConditionTrue
		reason = "Bound"
		message = fmt.Sprintf("volume bound to claim %q", mv.Spec.ClaimRef.Name)
	default:
		// Preserve Released/Archived; otherwise mark Released as the natural state
		// for an unbound volume (no claim, but still exists).
		if oldPhase == v1alpha1.PhaseArchived {
			newPhase = v1alpha1.PhaseArchived
		} else {
			newPhase = v1alpha1.PhaseReleased
		}
		readyState = lifecycle.ConditionTrue
		reason = "Unbound"
		message = "volume has no active claim"
	}

	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(mv.Status.Conditions),
		lifecycle.ConditionReady,
		readyState,
		reason,
		message,
	)
	desiredConditions := lifecycleToAPIConditions(lc)
	if oldPhase == newPhase && reflect.DeepEqual(mv.Status.Conditions, desiredConditions) {
		return Result{}, nil
	}

	mv.Status.Phase = newPhase
	mv.Status.Conditions = desiredConditions

	data, err := json.Marshal(mv)
	if err != nil {
		return Result{}, fmt.Errorf("marshal MemoryVolume: %w", err)
	}
	if err := r.store.Update(ctx, memoryVolumeKind, req.Namespace, req.Name, data, mv.ObjectMeta.ResourceVersion); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return Result{Requeue: true}, nil
		}
		return Result{}, fmt.Errorf("update MemoryVolume status: %w", err)
	}
	r.logger.Info("MemoryVolume reconciled",
		zap.String("namespace", req.Namespace),
		zap.String("name", req.Name),
		zap.String("phase", string(newPhase)),
		requestid.ZapField(ctx))
	return Result{}, nil
}
