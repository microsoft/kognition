// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cost

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/kognition-ai/kognition/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const runtimeRecoveryLabel = "kognition.dev/runtime-recovery"

var _ store.RuntimeRecoveryStore = (*ConfigMapStore)(nil)

func runtimeRecoveryName(namespace, session string) string {
	return fmt.Sprintf("kognition-rr-%x", sha256.Sum256([]byte(namespace+"\x00"+session)))
}

func runtimeRecoveryValid(record store.RuntimeRecoveryRecord) bool {
	return record.Workspace != "" && len(record.Workspace) <= 63 && record.SessionID != "" && len(record.SessionID) <= 128 && len(record.BindingDigest) == 64 && len(record.Ciphertext) > 0 && len(record.Ciphertext) <= 768<<10 && json.Valid(record.Ciphertext) && !record.ExpiresAt.IsZero()
}

func (s *ConfigMapStore) PrepareRuntimeRecovery(ctx context.Context, record store.RuntimeRecoveryRecord) (store.RuntimeRecoveryRecord, error) {
	if !runtimeRecoveryValid(record) {
		return store.RuntimeRecoveryRecord{}, store.ErrConflict
	}
	record.Phase, record.Version, record.Owner = store.RuntimeRecoveryPrepared, 1, ""
	encoded, err := json.Marshal(record)
	if err != nil || len(encoded) > 800<<10 {
		return record, store.ErrConflict
	}
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: runtimeRecoveryName(record.Workspace, record.SessionID), Namespace: s.namespace, Labels: map[string]string{runtimeRecoveryLabel: "true", "kognition.dev/recovery-phase": record.Phase}}, Data: map[string]string{"workspace": record.Workspace, "sessionID": record.SessionID, "bindingDigest": record.BindingDigest}, BinaryData: map[string][]byte{"record.json": encoded}}
	if err := s.client.Create(ctx, object); err != nil && !apierrors.IsAlreadyExists(err) {
		return record, err
	}
	stored, err := s.GetRuntimeRecovery(ctx, record.Workspace, record.SessionID)
	if err == nil && stored.BindingDigest != record.BindingDigest {
		return stored, store.ErrConflict
	}
	return stored, err
}

func (s *ConfigMapStore) loadRuntimeRecovery(ctx context.Context, namespace, session string) (*corev1.ConfigMap, store.RuntimeRecoveryRecord, error) {
	var object corev1.ConfigMap
	var record store.RuntimeRecoveryRecord
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: runtimeRecoveryName(namespace, session)}, &object); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, record, store.ErrNotFound
		}
		return nil, record, err
	}
	if object.Labels[runtimeRecoveryLabel] != "true" || json.Unmarshal(object.BinaryData["record.json"], &record) != nil || object.Labels["kognition.dev/recovery-phase"] != record.Phase || record.Workspace != namespace || record.SessionID != session || object.Data["workspace"] != record.Workspace || object.Data["sessionID"] != record.SessionID || object.Data["bindingDigest"] != record.BindingDigest || len(record.BindingDigest) != 64 || record.Version < 1 {
		return nil, record, store.ErrConflict
	}
	return &object, record, nil
}

func (s *ConfigMapStore) GetRuntimeRecovery(ctx context.Context, namespace, session string) (store.RuntimeRecoveryRecord, error) {
	_, record, err := s.loadRuntimeRecovery(ctx, namespace, session)
	return record, err
}

func (s *ConfigMapStore) ClaimRuntimeRecovery(ctx context.Context, namespace, session string, version int64, owner string) (store.RuntimeRecoveryRecord, error) {
	object, record, err := s.loadRuntimeRecovery(ctx, namespace, session)
	if err != nil {
		return record, err
	}
	if owner == "" || len(owner) > 128 || record.Phase != store.RuntimeRecoveryPrepared || record.Version != version || !record.ExpiresAt.After(time.Now()) {
		return record, store.ErrConflict
	}
	record.Phase, record.Owner, record.Version = store.RuntimeRecoveryClaimed, owner, record.Version+1
	object.Labels["kognition.dev/recovery-phase"] = record.Phase
	object.BinaryData["record.json"], err = json.Marshal(record)
	if err != nil {
		return record, err
	}
	if err := s.client.Update(ctx, object); err != nil {
		if apierrors.IsConflict(err) {
			return record, store.ErrConflict
		}
		return record, err
	}
	return record, nil
}

func (s *ConfigMapStore) CompleteRuntimeRecovery(ctx context.Context, record store.RuntimeRecoveryRecord) error {
	if !runtimeRecoveryValid(record) || record.Owner == "" {
		return store.ErrConflict
	}
	object, stored, err := s.loadRuntimeRecovery(ctx, record.Workspace, record.SessionID)
	if err != nil {
		return err
	}
	if stored.Phase != store.RuntimeRecoveryClaimed || stored.Version != record.Version || stored.Owner != record.Owner || stored.BindingDigest != record.BindingDigest {
		return store.ErrConflict
	}
	record.Phase, record.Version = store.RuntimeRecoveryTerminal, record.Version+1
	object.Labels["kognition.dev/recovery-phase"] = record.Phase
	object.BinaryData["record.json"], err = json.Marshal(record)
	if err != nil {
		return err
	}
	if err := s.client.Update(ctx, object); err != nil {
		if apierrors.IsConflict(err) {
			return store.ErrConflict
		}
		return err
	}
	return nil
}

func (s *ConfigMapStore) ExpireRuntimeRecovery(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, store.ErrConflict
	}
	var objects corev1.ConfigMapList
	if err := s.client.List(ctx, &objects, client.InNamespace(s.namespace), client.MatchingLabels{runtimeRecoveryLabel: "true", "kognition.dev/recovery-phase": store.RuntimeRecoveryTerminal}, client.Limit(int64(limit))); err != nil {
		return 0, err
	}
	expired := 0
	for index := range objects.Items {
		object := &objects.Items[index]
		var record store.RuntimeRecoveryRecord
		if json.Unmarshal(object.BinaryData["record.json"], &record) != nil || record.Phase != store.RuntimeRecoveryTerminal || record.ExpiresAt.After(now) {
			continue
		}
		record.Phase, record.Ciphertext, record.Version = store.RuntimeRecoveryRetired, nil, record.Version+1
		object.Labels["kognition.dev/recovery-phase"] = record.Phase
		var err error
		object.BinaryData["record.json"], err = json.Marshal(record)
		if err != nil {
			return expired, err
		}
		object.Annotations = map[string]string{"kognition.dev/recovery-retired-version": strconv.FormatInt(record.Version, 10)}
		if err := s.client.Update(ctx, object); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return expired, err
		}
		expired++
	}
	return expired, nil
}
