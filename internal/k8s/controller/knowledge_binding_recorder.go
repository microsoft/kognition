// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
)

type retainedKnowledgeBindingRecorder struct {
	client client.Client
}

func (recorder *retainedKnowledgeBindingRecorder) Record(ctx context.Context, kb *api.KnowledgeBase) error {
	if kb.Status.BackendBinding == nil || kb.Status.BackendBinding.ReclaimPolicy != api.ReclaimRetain {
		return fmt.Errorf("retained knowledge backend binding is missing or has a non-Retain reclaim policy")
	}
	record := retainedKnowledgeBindingRecord(kb)
	if err := recorder.client.Create(ctx, record); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create KnowledgeBindingRecord %q: %w", record.Name, err)
	}

	existing := &k8s.KnowledgeBindingRecord{}
	if err := recorder.client.Get(ctx, types.NamespacedName{Name: record.Name}, existing); err != nil {
		return fmt.Errorf("read existing KnowledgeBindingRecord %q: %w", record.Name, err)
	}
	if !reflect.DeepEqual(existing.Spec, record.Spec) {
		return fmt.Errorf("existing KnowledgeBindingRecord %q does not match retained backend ownership", record.Name)
	}
	return nil
}

func retainedKnowledgeBindingRecord(kb *api.KnowledgeBase) *k8s.KnowledgeBindingRecord {
	identity := kb.ObjectMeta.Namespace + "\x00" + kb.ObjectMeta.Name + "\x00" + kb.ObjectMeta.UID
	name := fmt.Sprintf("knowledge-binding-%x", sha256.Sum256([]byte(identity)))
	return &k8s.KnowledgeBindingRecord{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: k8s.KnowledgeBindingRecordSpec{
			KnowledgeBaseNamespace: kb.ObjectMeta.Namespace,
			KnowledgeBaseName:      kb.ObjectMeta.Name,
			KnowledgeBaseUID:       kb.ObjectMeta.UID,
			ReclaimPolicy:          k8s.ReclaimPolicyRetain,
			Binding:                *convert.KnowledgeBackendBindingToK8s(kb.Status.BackendBinding),
		},
	}
}
