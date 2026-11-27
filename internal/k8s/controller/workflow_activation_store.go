// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kognition-ai/kognition/internal/store"
)

type WorkflowActivationGateStore struct {
	writer    client.Client
	reader    client.Reader
	namespace string
	clock     func() (time.Time, error)
}

var _ store.WorkflowActivationStore = (*WorkflowActivationGateStore)(nil)

func NewWorkflowActivationGateStore(writer client.Client, reader client.Reader, namespace string, clock func() (time.Time, error)) (*WorkflowActivationGateStore, error) {
	if writer == nil || reader == nil || namespace == "" || clock == nil {
		return nil, store.ErrConflict
	}
	return &WorkflowActivationGateStore{writer: writer, reader: reader, namespace: namespace, clock: clock}, nil
}

func workflowActivationObjectName(key store.WorkflowActivationKey) string {
	encoded, _ := json.Marshal(key)
	digest := sha256.Sum256(encoded)
	return "kognition-workflow-activation-" + hex.EncodeToString(digest[:])
}

func (storage *WorkflowActivationGateStore) CreateWorkflowActivation(ctx context.Context, spec store.WorkflowActivationSpec) (store.WorkflowActivation, error) {
	now, err := storage.clock()
	if err != nil {
		return store.WorkflowActivation{}, err
	}
	proposed, err := store.NewWorkflowActivation(spec, now)
	if err != nil {
		return store.WorkflowActivation{}, err
	}
	encoded, err := json.Marshal(proposed)
	if err != nil {
		return store.WorkflowActivation{}, err
	}
	object := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: storage.namespace,
			Name:      workflowActivationObjectName(spec.Key),
			Labels:    map[string]string{"kognition.dev/internal-record": "workflow-activation"},
		},
		Data: map[string]string{"activation.json": string(encoded)},
	}
	if err := storage.writer.Create(ctx, object); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return store.WorkflowActivation{}, err
		}
		retained, err := storage.GetWorkflowActivation(ctx, spec.Key)
		if err != nil {
			return store.WorkflowActivation{}, err
		}
		if !reflect.DeepEqual(retained.Spec, proposed.Spec) {
			return store.WorkflowActivation{}, store.ErrConflict
		}
		return retained, nil
	}
	return proposed, nil
}

func (storage *WorkflowActivationGateStore) read(ctx context.Context, key store.WorkflowActivationKey) (*corev1.ConfigMap, store.WorkflowActivation, error) {
	if !store.ValidWorkflowActivationKey(key) {
		return nil, store.WorkflowActivation{}, store.ErrConflict
	}
	object := &corev1.ConfigMap{}
	err := storage.reader.Get(ctx, client.ObjectKey{Namespace: storage.namespace, Name: workflowActivationObjectName(key)}, object)
	if apierrors.IsNotFound(err) {
		return nil, store.WorkflowActivation{}, store.ErrNotFound
	}
	if err != nil {
		return nil, store.WorkflowActivation{}, err
	}
	raw := object.Data["activation.json"]
	if object.DeletionTimestamp != nil || object.Immutable != nil && *object.Immutable ||
		object.Labels["kognition.dev/internal-record"] != "workflow-activation" || len(raw) > store.WorkflowGateMaxBytes {
		return nil, store.WorkflowActivation{}, store.ErrConflict
	}
	var activation store.WorkflowActivation
	if err := json.Unmarshal([]byte(raw), &activation); err != nil {
		return nil, store.WorkflowActivation{}, errors.Join(store.ErrConflict, err)
	}
	if activation.Spec.Key != key || store.ValidateWorkflowActivation(activation) != nil {
		return nil, store.WorkflowActivation{}, store.ErrConflict
	}
	return object, activation, nil
}

func (storage *WorkflowActivationGateStore) GetWorkflowActivation(ctx context.Context, key store.WorkflowActivationKey) (store.WorkflowActivation, error) {
	_, activation, err := storage.read(ctx, key)
	return activation, err
}

func (storage *WorkflowActivationGateStore) MutateWorkflowActivation(ctx context.Context, key store.WorkflowActivationKey, mutation store.WorkflowActivationMutation) (store.WorkflowActivationResult, error) {
	object, activation, err := storage.read(ctx, key)
	if err != nil {
		return store.WorkflowActivationResult{}, err
	}
	now, err := storage.clock()
	if err != nil {
		return store.WorkflowActivationResult{}, err
	}
	result, err := store.ApplyWorkflowActivation(activation, mutation, now)
	if err != nil || !result.Changed {
		return result, err
	}
	encoded, err := json.Marshal(result.Activation)
	if err != nil {
		return store.WorkflowActivationResult{}, err
	}
	object.Data["activation.json"] = string(encoded)
	if err := storage.writer.Update(ctx, object); err != nil {
		if apierrors.IsConflict(err) {
			return store.WorkflowActivationResult{}, store.ErrConflict
		}
		return store.WorkflowActivationResult{}, err
	}
	return result, nil
}
