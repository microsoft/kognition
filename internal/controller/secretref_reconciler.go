// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const secretRefKind = "SecretRef"

// SecretRefReconciler validates SecretRef specs and reports synchronization phase.
type SecretRefReconciler struct {
	store  store.Store
	logger *zap.Logger
}

func NewSecretRefReconciler(s store.Store, logger *zap.Logger) *SecretRefReconciler {
	return &SecretRefReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "secretref")),
	}
}

func (r *SecretRefReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	raw, err := r.store.Get(ctx, secretRefKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get SecretRef: %w", err)
	}

	var sr v1alpha1.SecretRef
	if err := json.Unmarshal(raw, &sr); err != nil {
		return Result{}, fmt.Errorf("unmarshal SecretRef: %w", err)
	}

	oldPhase := sr.Status.Phase
	oldMessage := sr.Status.Message
	oldSynced := sr.Status.Synced

	var (
		newPhase   v1alpha1.Phase
		newMessage string
		newSynced  bool
		readyState lifecycle.ConditionStatus
		reason     string
	)
	if err := sr.Spec.Validate(); err != nil {
		newPhase = v1alpha1.PhaseFailed
		newMessage = err.Error()
		newSynced = false
		readyState = lifecycle.ConditionFalse
		reason = "ValidationFailed"
	} else {
		newPhase = v1alpha1.PhaseReady
		newMessage = fmt.Sprintf("secret reference validated (backend %q)", sr.Spec.ResolvedBackendType())
		newSynced = true
		readyState = lifecycle.ConditionTrue
		reason = "Synced"
	}

	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(sr.Status.Conditions),
		lifecycle.ConditionReady,
		readyState,
		reason,
		newMessage,
	)
	desiredConditions := lifecycleToAPIConditions(lc)
	if oldPhase == newPhase && oldMessage == newMessage && oldSynced == newSynced && reflect.DeepEqual(sr.Status.Conditions, desiredConditions) {
		return Result{}, nil
	}

	sr.Status.Phase = newPhase
	sr.Status.Message = newMessage
	sr.Status.Synced = newSynced
	now := time.Now().UTC()
	sr.Status.LastSyncTime = &now
	sr.Status.Conditions = desiredConditions

	data, err := json.Marshal(sr)
	if err != nil {
		return Result{}, fmt.Errorf("marshal SecretRef: %w", err)
	}
	if err := r.store.Update(ctx, secretRefKind, req.Namespace, req.Name, data, sr.ObjectMeta.ResourceVersion); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return Result{Requeue: true}, nil
		}
		return Result{}, fmt.Errorf("update SecretRef status: %w", err)
	}
	r.logger.Info("SecretRef reconciled",
		zap.String("namespace", req.Namespace),
		zap.String("name", req.Name),
		zap.String("phase", string(newPhase)),
		requestid.ZapField(ctx))
	return Result{}, nil
}
