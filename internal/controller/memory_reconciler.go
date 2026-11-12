// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// MemoryClaimFinalizer is applied to MemoryClaim resources so the reconciler
	// can enforce reclaimPolicy on the bound MemoryVolume before deletion.
	MemoryClaimFinalizer = "kognition.dev/memoryclaim-cleanup"

	memoryClaimKind  = "MemoryClaim"
	memoryVolumeKind = "MemoryVolume"
	memoryClassKind  = "MemoryClass"
)

// MemoryClaimReconciler reconciles MemoryClaim resources.
//
// On each reconciliation it:
//  1. Adds a finalizer so it can honour reclaimPolicy during deletion.
//  2. If phase is empty or Pending, looks up the referenced MemoryClass and
//     provisions a new MemoryVolume, then transitions the claim to Bound.
//  3. If phase is Bound, verifies the volume still exists.
//  4. On deletion, enforces the class-level reclaimPolicy:
//     - Delete  → deletes the MemoryVolume.
//     - Retain  → transitions the volume phase to Released, leaves data intact.
//     - Archive → transitions the volume phase to Archived.
type MemoryClaimReconciler struct {
	store       store.Store
	provisioner MemoryBackendProvisioner
	logger      *zap.Logger
}

// NewMemoryClaimReconciler creates a new MemoryClaimReconciler.
func NewMemoryClaimReconciler(s store.Store, logger *zap.Logger, opts ...MemoryClaimReconcilerOption) *MemoryClaimReconciler {
	reconciler := &MemoryClaimReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "memory-claim")),
	}
	for _, opt := range opts {
		opt(reconciler)
	}
	return reconciler
}

// MemoryClaimReconcilerOption configures MemoryClaim reconciliation.
type MemoryClaimReconcilerOption func(*MemoryClaimReconciler)

// WithMemoryBackendProvisioner provisions and deprovisions MemoryVolume backend handles.
func WithMemoryBackendProvisioner(provisioner MemoryBackendProvisioner) MemoryClaimReconcilerOption {
	return func(reconciler *MemoryClaimReconciler) {
		reconciler.provisioner = provisioner
	}
}

// Reconcile implements the Reconciler interface for MemoryClaim resources.
func (r *MemoryClaimReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	raw, err := r.store.Get(ctx, memoryClaimKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("memoryclaim not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get memoryclaim: %w", err)
	}

	var claim v1alpha1.MemoryClaim
	if err := json.Unmarshal(raw, &claim); err != nil {
		return Result{}, fmt.Errorf("unmarshal memoryclaim: %w", err)
	}
	originalStatus := snapshotMemoryClaimStatus(claim.Status)
	originalFinalizers := append([]string(nil), claim.ObjectMeta.Finalizers...)

	if claim.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &claim, originalStatus, originalFinalizers, log)
	}

	if !lifecycle.HasFinalizer(claim.ObjectMeta.Finalizers, MemoryClaimFinalizer) {
		finalizers, _ := lifecycle.AddFinalizer(claim.ObjectMeta.Finalizers, MemoryClaimFinalizer)
		claim.ObjectMeta.Finalizers = finalizers
		if err := r.updateClaim(ctx, &claim, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	switch claim.Status.Phase {
	case "", v1alpha1.PhasePending:
		return r.provision(ctx, &claim, originalStatus, originalFinalizers, log)
	case v1alpha1.PhaseBound:
		return r.verifyBound(ctx, &claim, originalStatus, originalFinalizers, log)
	default:
		// Released or terminal — nothing to do.
		return Result{}, nil
	}
}

// provision creates a MemoryVolume and binds the claim to it.
func (r *MemoryClaimReconciler) provision(ctx context.Context, claim *v1alpha1.MemoryClaim, originalStatus v1alpha1.MemoryClaimStatus, originalFinalizers []string, log *zap.Logger) (Result, error) {
	// Mark Pending while we work.
	if claim.Status.Phase == "" {
		claim.Status.Phase = v1alpha1.PhasePending
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(claim.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"Provisioning",
			"waiting for MemoryVolume to be provisioned",
		)
		claim.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateClaim(ctx, claim, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("set pending status: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	// Resolve the MemoryClass to obtain the reclaimPolicy and backendType.
	mc, err := r.getMemoryClass(ctx, claim.Spec.MemoryClassName)
	if err != nil {
		// Only warn the first time; subsequent requeues use Debug to avoid log spam.
		existing := lifecycle.GetCondition(apiToLifecycleConditions(claim.Status.Conditions), lifecycle.ConditionReady)
		if existing != nil && existing.Reason == "MemoryClassNotFound" {
			log.Debug("MemoryClass still not found", zap.String("class", claim.Spec.MemoryClassName))
		} else {
			log.Warn("MemoryClass not found", zap.String("class", claim.Spec.MemoryClassName), zap.Error(err))
		}
		// Only update the condition if it hasn't already been set to avoid
		// triggering an informer event that would immediately re-enqueue.
		if existing == nil || existing.Reason != "MemoryClassNotFound" {
			lc := lifecycle.SetCondition(
				apiToLifecycleConditions(claim.Status.Conditions),
				lifecycle.ConditionReady,
				lifecycle.ConditionFalse,
				"MemoryClassNotFound",
				fmt.Sprintf("MemoryClass %q not found", claim.Spec.MemoryClassName),
			)
			claim.Status.Conditions = lifecycleToAPIConditions(lc)
			if updateErr := r.updateClaim(ctx, claim, originalStatus, originalFinalizers); updateErr != nil {
				return Result{}, fmt.Errorf("update claim condition: %w", updateErr)
			}
		}
		return Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Generate a provisioner-specific backend handle.
	backendHandle := fmt.Sprintf("%s-%s", mc.Spec.BackendType, uuid.New().String())

	capacity := claim.Spec.Capacity
	if capacity == "" {
		capacity = "100Mi"
	}

	// Build the MemoryVolume that backs this claim.
	vol := &v1alpha1.MemoryVolume{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       memoryVolumeKind,
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", claim.ObjectMeta.Namespace, claim.ObjectMeta.Name),
			Namespace: claim.ObjectMeta.Namespace,
			UID:       uuid.New().String(),
			OwnerReferences: []v1alpha1.OwnerReference{
				{
					APIVersion: v1alpha1.APIVersion,
					Kind:       memoryClaimKind,
					Name:       claim.ObjectMeta.Name,
					UID:        claim.ObjectMeta.UID,
				},
			},
		},
		Spec: v1alpha1.MemoryVolumeSpec{
			MemoryClassName: claim.Spec.MemoryClassName,
			ClaimRef:        &v1alpha1.LocalObjectReference{Name: claim.ObjectMeta.Name},
			BackendHandle:   backendHandle,
			Capacity:        capacity,
		},
		Status: v1alpha1.MemoryVolumeStatus{
			Phase:    v1alpha1.PhasePending,
			Capacity: capacity,
		},
	}

	volBytes, err := json.Marshal(vol)
	if err != nil {
		return Result{}, fmt.Errorf("marshal memory volume: %w", err)
	}
	if err := r.store.Create(ctx, memoryVolumeKind, vol.ObjectMeta.Namespace, vol.ObjectMeta.Name, volBytes); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return Result{}, fmt.Errorf("create memory volume: %w", err)
	}
	persistedVolume, getErr := r.getMemoryVolume(ctx, vol.ObjectMeta.Namespace, vol.ObjectMeta.Name)
	if getErr != nil {
		return Result{}, fmt.Errorf("get persisted memory volume: %w", getErr)
	}
	if !memoryVolumeOwnedByClaim(persistedVolume, claim) {
		return Result{}, fmt.Errorf("memory volume %q already exists but is not owned by claim", vol.ObjectMeta.Name)
	}
	vol = persistedVolume

	if vol.Spec.BackendHandle == "" {
		return Result{}, fmt.Errorf("memory volume %q has no backend handle", vol.ObjectMeta.Name)
	}
	if r.provisioner != nil {
		if err := r.provisioner.Provision(ctx, memory.ProvisionRequest{
			BackendHandle:      vol.Spec.BackendHandle,
			EmbeddingDimension: memoryEmbeddingDimension(mc.Spec.Parameters),
			Parameters:         cloneStringMap(mc.Spec.Parameters),
		}); err != nil {
			return Result{}, fmt.Errorf("provision memory backend %q: %w", vol.Spec.BackendHandle, err)
		}
	}
	if vol.Status.Phase != v1alpha1.PhaseBound {
		vol.Status.Phase = v1alpha1.PhaseBound
		updated, err := json.Marshal(vol)
		if err != nil {
			return Result{}, fmt.Errorf("marshal provisioned memory volume: %w", err)
		}
		if err := r.store.Update(ctx, memoryVolumeKind, vol.ObjectMeta.Namespace, vol.ObjectMeta.Name, updated, vol.ObjectMeta.ResourceVersion); err != nil {
			return Result{}, fmt.Errorf("mark memory volume %q bound: %w", vol.ObjectMeta.Name, err)
		}
	}

	log.Info("provisioned MemoryVolume",
		zap.String("volume", vol.ObjectMeta.Name),
		zap.String("backend", mc.Spec.BackendType),
	)

	// Bind the claim.
	claim.Status.Phase = v1alpha1.PhaseBound
	claim.Status.BoundVolumeName = vol.ObjectMeta.Name
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(claim.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionTrue,
		"Bound",
		fmt.Sprintf("bound to MemoryVolume %q", vol.ObjectMeta.Name),
	)
	claim.Status.Conditions = lifecycleToAPIConditions(lc)
	if err := r.updateClaim(ctx, claim, originalStatus, originalFinalizers); err != nil {
		return Result{}, fmt.Errorf("bind claim: %w", err)
	}

	return Result{}, nil
}

func memoryVolumeOwnedByClaim(volume *v1alpha1.MemoryVolume, claim *v1alpha1.MemoryClaim) bool {
	if volume.Spec.ClaimRef == nil || volume.Spec.ClaimRef.Name != claim.ObjectMeta.Name {
		return false
	}
	for _, owner := range volume.ObjectMeta.OwnerReferences {
		if owner.Kind == memoryClaimKind && owner.Name == claim.ObjectMeta.Name && owner.UID == claim.ObjectMeta.UID {
			return true
		}
	}
	return false
}

// verifyBound checks that the referenced volume still exists and is healthy.
func (r *MemoryClaimReconciler) verifyBound(ctx context.Context, claim *v1alpha1.MemoryClaim, originalStatus v1alpha1.MemoryClaimStatus, originalFinalizers []string, log *zap.Logger) (Result, error) {
	if claim.Status.BoundVolumeName == "" {
		// No bound volume recorded; re-provision.
		claim.Status.Phase = v1alpha1.PhasePending
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(claim.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"VolumeMissing",
			"bound volume name not recorded, re-provisioning",
		)
		claim.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateClaim(ctx, claim, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("reset to pending: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	_, err := r.store.Get(ctx, memoryVolumeKind, claim.ObjectMeta.Namespace, claim.Status.BoundVolumeName)
	if errors.Is(err, store.ErrNotFound) {
		log.Warn("bound volume missing, re-provisioning", zap.String("volume", claim.Status.BoundVolumeName))
		claim.Status.Phase = v1alpha1.PhasePending
		claim.Status.BoundVolumeName = ""
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(claim.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"VolumeMissing",
			"bound volume not found, re-provisioning",
		)
		claim.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateClaim(ctx, claim, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("reset missing volume: %w", err)
		}
		return Result{Requeue: true}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("get bound volume: %w", err)
	}

	return Result{}, nil
}

// handleDeletion applies reclaimPolicy to the bound volume and removes the
// finalizer once cleanup is complete.
func (r *MemoryClaimReconciler) handleDeletion(ctx context.Context, claim *v1alpha1.MemoryClaim, originalStatus v1alpha1.MemoryClaimStatus, originalFinalizers []string, log *zap.Logger) (Result, error) {
	if claim.Status.BoundVolumeName != "" {
		mc, err := r.getMemoryClass(ctx, claim.Spec.MemoryClassName)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return Result{}, fmt.Errorf("get memory class %q for deletion: %w", claim.Spec.MemoryClassName, err)
		}
		reclaimPolicy := v1alpha1.ReclaimDelete
		if err == nil {
			reclaimPolicy = mc.Spec.ReclaimPolicy
		}
		if reclaimPolicy == "" {
			reclaimPolicy = v1alpha1.ReclaimDelete
		}

		switch reclaimPolicy {
		case v1alpha1.ReclaimDelete:
			if r.provisioner != nil {
				volume, volumeErr := r.getMemoryVolume(ctx, claim.ObjectMeta.Namespace, claim.Status.BoundVolumeName)
				if volumeErr != nil && !errors.Is(volumeErr, store.ErrNotFound) {
					return Result{}, fmt.Errorf("get memory volume %s for backend cleanup: %w", claim.Status.BoundVolumeName, volumeErr)
				}
				if volumeErr == nil && volume.Spec.BackendHandle != "" {
					if deprovisionErr := r.provisioner.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: volume.Spec.BackendHandle}); deprovisionErr != nil {
						return Result{}, fmt.Errorf("deprovision memory backend %q: %w", volume.Spec.BackendHandle, deprovisionErr)
					}
				}
			}
			if err := r.store.Delete(ctx, memoryVolumeKind, claim.ObjectMeta.Namespace, claim.Status.BoundVolumeName); err != nil && !errors.Is(err, store.ErrNotFound) {
				return Result{}, fmt.Errorf("delete memory volume %s: %w", claim.Status.BoundVolumeName, err)
			}
			log.Info("deleted MemoryVolume per reclaimPolicy=Delete", zap.String("volume", claim.Status.BoundVolumeName))

		case v1alpha1.ReclaimRetain:
			if termErr := r.transitionVolume(ctx, claim.ObjectMeta.Namespace, claim.Status.BoundVolumeName, v1alpha1.PhaseRetained); termErr != nil {
				return Result{}, termErr
			}
			log.Info("retained MemoryVolume per reclaimPolicy=Retain", zap.String("volume", claim.Status.BoundVolumeName))

		case v1alpha1.ReclaimArchive:
			if termErr := r.transitionVolume(ctx, claim.ObjectMeta.Namespace, claim.Status.BoundVolumeName, v1alpha1.PhaseArchived); termErr != nil {
				return Result{}, termErr
			}
			log.Info("archived MemoryVolume per reclaimPolicy=Archive", zap.String("volume", claim.Status.BoundVolumeName))

		default:
			return Result{}, fmt.Errorf("unsupported reclaim policy %q", reclaimPolicy)
		}
	}

	// Transition the claim itself to Released before removing the finalizer.
	claim.Status.Phase = v1alpha1.PhaseReleased
	finalizers, removed := lifecycle.RemoveFinalizer(claim.ObjectMeta.Finalizers, MemoryClaimFinalizer)
	if !removed {
		return Result{}, nil
	}
	claim.ObjectMeta.Finalizers = finalizers
	if err := r.updateClaim(ctx, claim, originalStatus, originalFinalizers); err != nil {
		return Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return Result{}, nil
}

func memoryEmbeddingDimension(parameters map[string]string) int {
	for _, key := range []string{"embeddingDimension", "dimension"} {
		if value := parameters[key]; value != "" {
			dimension, err := strconv.Atoi(value)
			if err == nil && dimension > 0 {
				return dimension
			}
		}
	}
	return 0
}

func (r *MemoryClaimReconciler) getMemoryVolume(ctx context.Context, namespace, name string) (*v1alpha1.MemoryVolume, error) {
	raw, err := r.store.Get(ctx, memoryVolumeKind, namespace, name)
	if err != nil {
		return nil, err
	}
	var volume v1alpha1.MemoryVolume
	if err := json.Unmarshal(raw, &volume); err != nil {
		return nil, fmt.Errorf("unmarshal memory volume %s: %w", name, err)
	}
	return &volume, nil
}

// transitionVolume updates a MemoryVolume's phase to the given terminal phase.
func (r *MemoryClaimReconciler) transitionVolume(ctx context.Context, namespace, name string, phase v1alpha1.Phase) error {
	raw, err := r.store.Get(ctx, memoryVolumeKind, namespace, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get volume %s for transition: %w", name, err)
	}

	var vol v1alpha1.MemoryVolume
	if err := json.Unmarshal(raw, &vol); err != nil {
		return fmt.Errorf("unmarshal volume %s: %w", name, err)
	}

	vol.Status.Phase = phase
	// Remove the ClaimRef to indicate the volume is no longer bound.
	vol.Spec.ClaimRef = nil

	updated, err := json.Marshal(&vol)
	if err != nil {
		return fmt.Errorf("marshal updated volume %s: %w", name, err)
	}
	if err := r.store.Update(ctx, memoryVolumeKind, namespace, name, updated, vol.ObjectMeta.ResourceVersion); err != nil {
		return fmt.Errorf("update volume %s phase to %s: %w", name, phase, err)
	}
	return nil
}

// getMemoryClass fetches and unmarshals the named MemoryClass. MemoryClass
// resources are cluster-scoped (empty namespace).
func (r *MemoryClaimReconciler) getMemoryClass(ctx context.Context, name string) (*v1alpha1.MemoryClass, error) {
	raw, err := r.store.Get(ctx, memoryClassKind, "", name)
	if err != nil {
		return nil, err
	}
	var mc v1alpha1.MemoryClass
	if err := json.Unmarshal(raw, &mc); err != nil {
		return nil, fmt.Errorf("unmarshal memory class: %w", err)
	}
	return &mc, nil
}

// updateClaim persists the latest MemoryClaim state to the store.
func (r *MemoryClaimReconciler) updateClaim(ctx context.Context, claim *v1alpha1.MemoryClaim, originalStatus v1alpha1.MemoryClaimStatus, originalFinalizers []string) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		b, err := json.Marshal(claim)
		if err != nil {
			return fmt.Errorf("marshal claim: %w", err)
		}
		err = r.store.Update(ctx, memoryClaimKind, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name, b, claim.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, getErr := r.store.Get(ctx, memoryClaimKind, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name)
		if getErr != nil {
			return fmt.Errorf("re-read memoryclaim after conflict: %w", getErr)
		}
		var latest v1alpha1.MemoryClaim
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal memoryclaim after conflict: %w", getErr)
		}
		mergeMemoryClaimStatus(&latest.Status, originalStatus, claim.Status)
		originalHasFinalizer := hasString(originalFinalizers, MemoryClaimFinalizer)
		desiredHasFinalizer := hasString(claim.ObjectMeta.Finalizers, MemoryClaimFinalizer)
		if originalHasFinalizer != desiredHasFinalizer {
			latest.ObjectMeta.Finalizers = mergeOwnedFinalizer(latest.ObjectMeta.Finalizers, claim.ObjectMeta.Finalizers, MemoryClaimFinalizer, !desiredHasFinalizer)
		}
		*claim = latest
	}
	return store.ErrConflict
}

func snapshotMemoryClaimStatus(status v1alpha1.MemoryClaimStatus) v1alpha1.MemoryClaimStatus {
	cloned := status
	cloned.Conditions = append([]v1alpha1.Condition(nil), status.Conditions...)
	return cloned
}

func mergeMemoryClaimStatus(latest *v1alpha1.MemoryClaimStatus, original, desired v1alpha1.MemoryClaimStatus) {
	if desired.Phase != original.Phase {
		latest.Phase = desired.Phase
	}
	if desired.BoundVolumeName != original.BoundVolumeName {
		latest.BoundVolumeName = desired.BoundVolumeName
	}
	conditionType := string(lifecycle.ConditionReady)
	originalCondition := restartLadderCondition(original.Conditions, conditionType)
	desiredCondition := restartLadderCondition(desired.Conditions, conditionType)
	if !conditionsMatch(originalCondition, desiredCondition) {
		latest.Conditions = applyConditionChange(latest.Conditions, conditionType, desiredCondition)
	}
}
