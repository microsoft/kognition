// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func retainedKnowledgeBase() *api.KnowledgeBase {
	return &api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "handbook", UID: "kb-uid"},
		Status: api.KnowledgeBaseStatus{BackendBinding: &api.KnowledgeBackendBinding{
			Generation: 3, ProviderName: "azuresearch", ReclaimPolicy: api.ReclaimRetain,
			ClassUID: "class-uid", ClassGeneration: 2,
			ConfigurationAPIGroup: "azuresearch.knowledge.kognition.dev", ConfigurationKind: "AzureSearchBackend", ConfigurationName: "shared",
			ConfigurationUID: "config-uid", ConfigurationDigest: strings.Repeat("a", 64),
			OperationID: "team-a/handbook-3", BackendOperationID: "search-index-handbook",
		}},
	}
}

func TestRetainedKnowledgeBindingRecorderCreatesPermanentRecord(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientBuilder().WithScheme(SetupScheme()).Build()
	recorder := &retainedKnowledgeBindingRecorder{client: client}
	kb := retainedKnowledgeBase()

	if err := recorder.Record(ctx, kb); err != nil {
		t.Fatalf("Record() error = %v", err)
	}

	want := retainedKnowledgeBindingRecord(kb)
	var got k8s.KnowledgeBindingRecord
	if err := client.Get(ctx, types.NamespacedName{Name: want.Name}, &got); err != nil {
		t.Fatalf("get KnowledgeBindingRecord: %v", err)
	}
	if got.Namespace != "" {
		t.Fatalf("record namespace = %q, want cluster scoped", got.Namespace)
	}
	if got.Spec != want.Spec {
		t.Fatalf("record spec = %+v, want %+v", got.Spec, want.Spec)
	}
	if got.Spec.Binding.ReclaimPolicy != k8s.ReclaimPolicyRetain {
		t.Fatalf("binding reclaim policy = %q, want Retain", got.Spec.Binding.ReclaimPolicy)
	}
}

func TestRetainedKnowledgeBindingRecorderRetryIsIdempotent(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientBuilder().WithScheme(SetupScheme()).Build()
	recorder := &retainedKnowledgeBindingRecorder{client: client}
	kb := retainedKnowledgeBase()

	if err := recorder.Record(ctx, kb); err != nil {
		t.Fatalf("first Record() error = %v", err)
	}
	if err := recorder.Record(ctx, kb); err != nil {
		t.Fatalf("retry Record() error = %v", err)
	}
}

func TestRetainedKnowledgeBindingRecordIdentityIsDeterministicAndUIDBound(t *testing.T) {
	kb := retainedKnowledgeBase()
	first := retainedKnowledgeBindingRecord(kb).Name
	if second := retainedKnowledgeBindingRecord(kb).Name; second != first {
		t.Fatalf("record names differ across retries: %q and %q", first, second)
	}
	kb.ObjectMeta.UID = "replacement-uid"
	if replacement := retainedKnowledgeBindingRecord(kb).Name; replacement == first {
		t.Fatalf("replacement KnowledgeBase reused record name %q", first)
	}
}

func TestRetainedKnowledgeBindingRecorderRejectsMismatchedExistingRecord(t *testing.T) {
	ctx := context.Background()
	kb := retainedKnowledgeBase()
	existing := retainedKnowledgeBindingRecord(kb)
	existing.Spec.Binding.BackendOperationID = "different-index"
	client := fake.NewClientBuilder().WithScheme(SetupScheme()).WithObjects(existing).Build()
	recorder := &retainedKnowledgeBindingRecorder{client: client}

	err := recorder.Record(ctx, kb)
	if err == nil || !strings.Contains(err.Error(), "does not match retained backend ownership") {
		t.Fatalf("Record() error = %v, want ownership mismatch", err)
	}
}

func TestRetainedKnowledgeBindingRecorderReportsCreateFailure(t *testing.T) {
	wantErr := errors.New("API unavailable")
	client := fake.NewClientBuilder().WithScheme(SetupScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, ctrlclient.WithWatch, ctrlclient.Object, ...ctrlclient.CreateOption) error {
			return wantErr
		},
	}).Build()

	err := (&retainedKnowledgeBindingRecorder{client: client}).Record(context.Background(), retainedKnowledgeBase())
	if err == nil || !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "create KnowledgeBindingRecord") {
		t.Fatalf("Record() error = %v, want wrapped create failure", err)
	}
}

func TestRetainedKnowledgeBindingRecorderReportsReadFailureAfterConflict(t *testing.T) {
	wantErr := errors.New("API unavailable")
	client := fake.NewClientBuilder().WithScheme(SetupScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(_ context.Context, _ ctrlclient.WithWatch, obj ctrlclient.Object, _ ...ctrlclient.CreateOption) error {
			return apierrors.NewAlreadyExists(schema.GroupResource{Group: k8s.GroupVersion.Group, Resource: "knowledgebindingrecords"}, obj.GetName())
		},
		Get: func(context.Context, ctrlclient.WithWatch, ctrlclient.ObjectKey, ctrlclient.Object, ...ctrlclient.GetOption) error {
			return wantErr
		},
	}).Build()

	err := (&retainedKnowledgeBindingRecorder{client: client}).Record(context.Background(), retainedKnowledgeBase())
	if err == nil || !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "read existing KnowledgeBindingRecord") {
		t.Fatalf("Record() error = %v, want wrapped read failure", err)
	}
}

func TestRetainedKnowledgeBindingRecorderRequiresRetainBinding(t *testing.T) {
	client := fake.NewClientBuilder().WithScheme(SetupScheme()).Build()
	recorder := &retainedKnowledgeBindingRecorder{client: client}
	for _, binding := range []*api.KnowledgeBackendBinding{nil, {ReclaimPolicy: api.ReclaimDelete}} {
		kb := retainedKnowledgeBase()
		kb.Status.BackendBinding = binding
		if err := recorder.Record(context.Background(), kb); err == nil || !strings.Contains(err.Error(), "missing or has a non-Retain reclaim policy") {
			t.Fatalf("Record() with binding %+v error = %v, want policy rejection", binding, err)
		}
	}
}
