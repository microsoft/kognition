// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

const (
	runtimeApprovalBindingLeaseName       = "kognition-runtime-approval-binding-keys"
	runtimeApprovalBindingStateAnnotation = "kognition.dev/runtime-approval-binding-key-state"
	runtimeApprovalBindingLockSeconds     = int32(30)
	runtimeApprovalBindingReleaseTimeout  = 5 * time.Second
	defaultControlNamespace               = "kognition-system"
)

var errRuntimeApprovalBindingLeaseContended = errors.New("RuntimeApproval binding-key Lease is contended")

type kubernetesBindingKeyMetadata struct {
	Version int64                            `json:"version"`
	Epoch   int64                            `json:"epoch"`
	Status  runtimeapproval.BindingKeyStatus `json:"status"`
	Digest  string                           `json:"digest"`
}

type kubernetesBindingKeyState struct {
	Epoch            int64                          `json:"epoch"`
	ActiveKeyVersion int64                          `json:"activeKeyVersion"`
	Keys             []kubernetesBindingKeyMetadata `json:"keys"`
}

func (s *StoreAdapter) runtimeApprovalBindingLeaseKey() client.ObjectKey {
	namespace := s.controlNamespace
	if namespace == "" {
		namespace = defaultControlNamespace
	}
	return client.ObjectKey{Namespace: namespace, Name: runtimeApprovalBindingLeaseName}
}

func (s *StoreAdapter) acquireRuntimeApprovalBindingEpoch(ctx context.Context, keys runtimeapproval.BindingKeyStore) (*coordinationv1.Lease, error) {
	state, err := kubernetesBindingState(ctx, keys)
	if err != nil {
		return nil, err
	}
	now := metav1.NewMicroTime(s.runtimeApprovalNow())
	holder := uuid.NewString()
	lease := &coordinationv1.Lease{}
	key := s.runtimeApprovalBindingLeaseKey()
	err = s.runtimeApprovalReader.Get(ctx, key, lease)
	if apierrors.IsNotFound(err) {
		encoded, marshalErr := json.Marshal(state)
		if marshalErr != nil {
			return nil, fmt.Errorf("marshal RuntimeApproval binding-key state: %w", marshalErr)
		}
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: map[string]string{"app.kubernetes.io/managed-by": "kognition"}, Annotations: map[string]string{runtimeApprovalBindingStateAnnotation: string(encoded)}},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: int32Pointer(runtimeApprovalBindingLockSeconds), AcquireTime: &now, RenewTime: &now},
		}
		if err := s.client.Create(ctx, lease); err == nil {
			return lease, nil
		}
		lease = &coordinationv1.Lease{}
		err = s.runtimeApprovalReader.Get(ctx, key, lease)
	}
	if err != nil {
		return nil, fmt.Errorf("read RuntimeApproval binding-key epoch Lease: %w", runtimeapproval.ErrBindingKeyUnavailable)
	}
	persisted, err := bindingStateFromLease(lease)
	if err == nil && sameKubernetesBindingState(persisted, state) && lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == holder {
		return lease, nil
	}
	if err != nil || !sameKubernetesBindingState(persisted, state) {
		return nil, runtimeapproval.ErrBindingKeyUnavailable
	}
	if bindingLeaseHeld(lease, s.runtimeApprovalNow()) {
		return nil, errors.Join(runtimeapproval.ErrBindingKeyUnavailable, errRuntimeApprovalBindingLeaseContended)
	}
	lease.Spec.HolderIdentity = &holder
	lease.Spec.LeaseDurationSeconds = int32Pointer(runtimeApprovalBindingLockSeconds)
	lease.Spec.AcquireTime = &now
	lease.Spec.RenewTime = &now
	if err := s.client.Update(ctx, lease); err != nil {
		if apierrors.IsConflict(err) {
			return nil, errors.Join(runtimeapproval.ErrBindingKeyUnavailable, errRuntimeApprovalBindingLeaseContended)
		}
		return nil, fmt.Errorf("acquire RuntimeApproval binding-key epoch Lease: %w", runtimeapproval.ErrBindingKeyUnavailable)
	}
	return lease, nil
}

func (s *StoreAdapter) acquireRuntimeApprovalBindingEpochWithRetry(ctx context.Context, keys runtimeapproval.BindingKeyStore) (*coordinationv1.Lease, error) {
	var acquired *coordinationv1.Lease
	var lastError error
	err := wait.PollUntilContextTimeout(ctx, runtimeApprovalWorkspaceRetryPeriod, runtimeApprovalWorkspaceAcquireWait, true, func(ctx context.Context) (bool, error) {
		lease, err := s.acquireRuntimeApprovalBindingEpoch(ctx, keys)
		lastError = err
		if err == nil {
			acquired = lease
			return true, nil
		}
		if errors.Is(err, errRuntimeApprovalBindingLeaseContended) {
			return false, nil
		}
		return false, err
	})
	if err != nil {
		return nil, errors.Join(err, lastError)
	}
	return acquired, nil
}

func (s *StoreAdapter) releaseRuntimeApprovalBindingEpoch(ctx context.Context, lease *coordinationv1.Lease) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeApprovalBindingReleaseTimeout)
	defer cancel()
	empty := ""
	now := metav1.NewMicroTime(s.runtimeApprovalNow())
	lease.Spec.HolderIdentity = &empty
	lease.Spec.RenewTime = &now
	if err := s.client.Update(cleanupCtx, lease); err != nil {
		return fmt.Errorf("release RuntimeApproval binding-key epoch Lease: %w", err)
	}
	return nil
}

func (s *StoreAdapter) ActivateRuntimeApprovalBindingKeys(ctx context.Context, keys runtimeapproval.BindingKeyStore) error {
	s.runtimeApprovalActivationMu.Lock()
	defer s.runtimeApprovalActivationMu.Unlock()
	next, err := kubernetesBindingState(ctx, keys)
	if err != nil {
		return err
	}
	key := s.runtimeApprovalBindingLeaseKey()
	lease := &coordinationv1.Lease{}
	err = s.client.Get(ctx, key, lease)
	if apierrors.IsNotFound(err) {
		encoded, marshalErr := json.Marshal(next)
		if marshalErr != nil {
			return marshalErr
		}
		lease = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: map[string]string{"app.kubernetes.io/managed-by": "kognition"}, Annotations: map[string]string{runtimeApprovalBindingStateAnnotation: string(encoded)}}}
		if err := s.client.Create(ctx, lease); err == nil {
			s.ConfigureRuntimeApprovalBindingKeys(keys)
			return nil
		}
		lease = &coordinationv1.Lease{}
		err = s.client.Get(ctx, key, lease)
	}
	if err != nil || bindingLeaseHeld(lease, s.runtimeApprovalNow()) {
		return runtimeapproval.ErrBindingKeyUnavailable
	}
	current, err := bindingStateFromLease(lease)
	if err != nil {
		return runtimeapproval.ErrBindingKeyUnavailable
	}
	if sameKubernetesBindingState(current, next) {
		s.ConfigureRuntimeApprovalBindingKeys(keys)
		return nil
	}
	if next.Epoch <= current.Epoch {
		return fmt.Errorf("runtime binding key epoch %d must advance beyond %d", next.Epoch, current.Epoch)
	}
	if err := validateKubernetesBindingRotation(current, next); err != nil {
		return err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	lease.Annotations[runtimeApprovalBindingStateAnnotation] = string(encoded)
	if err := s.client.Update(ctx, lease); err != nil {
		return fmt.Errorf("persist RuntimeApproval binding-key activation: %w", runtimeapproval.ErrBindingKeyUnavailable)
	}
	s.ConfigureRuntimeApprovalBindingKeys(keys)
	return nil
}

// RuntimeApprovalBindingKeysReady verifies local key material metadata against
// the replica-shared Lease activation witness.
func (s *StoreAdapter) RuntimeApprovalBindingKeysReady(ctx context.Context) error {
	local, err := kubernetesBindingState(ctx, s.runtimeApprovalKeys())
	if err != nil {
		return err
	}
	lease := &coordinationv1.Lease{}
	if err := s.client.Get(ctx, s.runtimeApprovalBindingLeaseKey(), lease); err != nil {
		return fmt.Errorf("read RuntimeApproval binding-key readiness: %w", err)
	}
	persisted, err := bindingStateFromLease(lease)
	if err != nil || !sameKubernetesBindingState(local, persisted) {
		return runtimeapproval.ErrBindingKeyUnavailable
	}
	return nil
}

func kubernetesBindingState(ctx context.Context, keys runtimeapproval.BindingKeyStore) (kubernetesBindingKeyState, error) {
	active, all, err := runtimeapproval.ValidatedBindingKeys(ctx, keys)
	if err != nil {
		return kubernetesBindingKeyState{}, err
	}
	state := kubernetesBindingKeyState{Epoch: active.Epoch, ActiveKeyVersion: active.Version, Keys: make([]kubernetesBindingKeyMetadata, len(all))}
	for index, key := range all {
		state.Keys[index] = kubernetesBindingKeyMetadata{Version: key.Version, Epoch: key.Epoch, Status: key.Status, Digest: bindingKeyMaterialDigest(key.Material)}
	}
	return state, nil
}

func bindingStateFromLease(lease *coordinationv1.Lease) (kubernetesBindingKeyState, error) {
	var state kubernetesBindingKeyState
	if lease.Annotations == nil {
		return state, fmt.Errorf("binding-key state annotation is missing")
	}
	if err := json.Unmarshal([]byte(lease.Annotations[runtimeApprovalBindingStateAnnotation]), &state); err != nil {
		return state, err
	}
	if state.Epoch <= 0 || state.ActiveKeyVersion <= 0 || len(state.Keys) == 0 {
		return state, fmt.Errorf("binding-key state annotation is invalid")
	}
	return state, nil
}

func bindingLeaseHeld(lease *coordinationv1.Lease, now time.Time) bool {
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return false
	}
	if lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds <= 0 {
		return true
	}
	lastRenewed := lease.Spec.RenewTime
	if lastRenewed == nil {
		lastRenewed = lease.Spec.AcquireTime
	}
	if lastRenewed == nil {
		return true
	}
	expiresAt := lastRenewed.Time.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	return now.Before(expiresAt)
}

func sameKubernetesBindingState(left, right kubernetesBindingKeyState) bool {
	if left.Epoch != right.Epoch || left.ActiveKeyVersion != right.ActiveKeyVersion || len(left.Keys) != len(right.Keys) {
		return false
	}
	for index := range left.Keys {
		if left.Keys[index] != right.Keys[index] {
			return false
		}
	}
	return true
}

func validateKubernetesBindingRotation(previous, next kubernetesBindingKeyState) error {
	nextByVersion := make(map[int64]kubernetesBindingKeyMetadata, len(next.Keys))
	for _, key := range next.Keys {
		nextByVersion[key.Version] = key
	}
	if nextByVersion[previous.ActiveKeyVersion].Status != runtimeapproval.BindingKeyVerifyOnly {
		return fmt.Errorf("previous active runtime binding key %d must be retained verify-only", previous.ActiveKeyVersion)
	}
	for _, key := range previous.Keys {
		retained, exists := nextByVersion[key.Version]
		if !exists {
			return fmt.Errorf("runtime binding key %d cannot be removed", key.Version)
		}
		if retained.Epoch != key.Epoch {
			return fmt.Errorf("runtime binding key %d epoch cannot change", key.Version)
		}
		if retained.Digest != key.Digest {
			return fmt.Errorf("runtime binding key %d material cannot change", key.Version)
		}
		if key.Status == runtimeapproval.BindingKeyVerifyOnly && retained.Status != runtimeapproval.BindingKeyVerifyOnly {
			return fmt.Errorf("verify-only runtime binding key %d status cannot change", key.Version)
		}
	}
	return nil
}

func int32Pointer(value int32) *int32 {
	return &value
}

func bindingKeyMaterialDigest(material []byte) string {
	digest := sha256.Sum256(material)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
