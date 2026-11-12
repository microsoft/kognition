// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/kognition-ai/kognition/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ store.ModelDispatchStore = (*ConfigMapStore)(nil)

func dispatchRecordName(namespace, operation, phase string) string {
	digest := sha256.Sum256([]byte(namespace + "\x00" + operation))
	return "kognition-md-" + hex.EncodeToString(digest[:20]) + "-" + phase
}

func (s *ConfigMapStore) dispatchRecord(namespace, operation, sessionID, phase string, response json.RawMessage) (*corev1.ConfigMap, error) {
	if namespace == "" || operation == "" || sessionID == "" || len(namespace) > 63 || len(operation) > 512 || len(sessionID) > 512 || len(response) > 4096 {
		return nil, errors.New("invalid model dispatch record")
	}
	encoded, err := json.Marshal(struct {
		SessionID string          `json:"sessionID"`
		Response  json.RawMessage `json:"response,omitempty"`
	}{sessionID, response})
	if err != nil {
		return nil, errors.New("invalid model dispatch response")
	}
	immutable := true
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: dispatchRecordName(namespace, operation, phase), Namespace: s.namespace, Labels: map[string]string{"kognition.dev/model-dispatch-record": "true"}},
		Immutable:  &immutable,
		Data:       map[string]string{"workspace": namespace, "operation": operation},
		BinaryData: map[string][]byte{"record.json": encoded},
	}, nil
}

func (s *ConfigMapStore) loadDispatchRecord(ctx context.Context, namespace, operation, phase string) (*corev1.ConfigMap, store.ModelDispatchRecord, error) {
	var object corev1.ConfigMap
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: dispatchRecordName(namespace, operation, phase)}, &object); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, store.ModelDispatchRecord{}, store.ErrNotFound
		}
		return nil, store.ModelDispatchRecord{}, err
	}
	var record store.ModelDispatchRecord
	if object.Immutable == nil || !*object.Immutable || object.Data["workspace"] != namespace || object.Data["operation"] != operation || json.Unmarshal(object.BinaryData["record.json"], &record) != nil || record.SessionID == "" {
		return nil, record, store.ErrConflict
	}
	return &object, record, nil
}

func (s *ConfigMapStore) ClaimModelDispatch(ctx context.Context, namespace, operation, sessionID string) (bool, error) {
	claim, err := s.dispatchRecord(namespace, operation, sessionID, "claim", nil)
	if err != nil {
		return false, err
	}
	if err := s.client.Create(ctx, claim); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return false, err
		}
		_, _, err := s.loadDispatchRecord(ctx, namespace, operation, "claim")
		return false, err
	}
	return true, nil
}

func (s *ConfigMapStore) CompleteModelDispatch(ctx context.Context, namespace, operation, sessionID string, response json.RawMessage) error {
	claimObject, claim, err := s.loadDispatchRecord(ctx, namespace, operation, "claim")
	if err != nil || claim.SessionID != sessionID || len(claim.Response) != 0 || !json.Valid(response) {
		return store.ErrConflict
	}
	terminal, err := s.dispatchRecord(namespace, operation, sessionID, "result", response)
	if err != nil {
		return err
	}
	terminal.Data["claimUID"] = string(claimObject.UID)
	if err := s.client.Create(ctx, terminal); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		stored, _, err := s.loadDispatchRecord(ctx, namespace, operation, "result")
		if err != nil || !bytes.Equal(stored.BinaryData["record.json"], terminal.BinaryData["record.json"]) || stored.Data["claimUID"] != terminal.Data["claimUID"] {
			return store.ErrConflict
		}
	}
	return nil
}

func (s *ConfigMapStore) GetModelDispatch(ctx context.Context, namespace, operation string) (store.ModelDispatchRecord, error) {
	claimObject, claim, err := s.loadDispatchRecord(ctx, namespace, operation, "claim")
	if err != nil {
		return claim, err
	}
	terminalObject, terminal, err := s.loadDispatchRecord(ctx, namespace, operation, "result")
	if errors.Is(err, store.ErrNotFound) {
		return claim, nil
	}
	if err != nil || terminal.SessionID != claim.SessionID || terminalObject.Data["claimUID"] != string(claimObject.UID) {
		return claim, store.ErrConflict
	}
	return terminal, nil
}
