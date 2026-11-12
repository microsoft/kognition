// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
)

func TestKBIngesterCompletionHonorsCancellation(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{batchv1.AddToScheme, corev1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "work", UID: "kb-uid"}}
	job := (&KBIngesterLauncher{image: "ingester:test"}).buildJob(kb, ingestJobName(kb.Name, 1), "input", nil)
	job.UID = "job-uid"
	job.Status.Succeeded = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	storage := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(readCtx context.Context, underlying client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			err := underlying.Get(readCtx, key, object, options...)
			cancel()
			return err
		},
	}).Build()
	launcher := NewKBIngesterLauncher(storage, nil, zap.NewNop(), "ingester:test", "", "control")
	state, _, err := launcher.Ensure(ctx, kb, &api.KnowledgeBaseClass{})
	if state == intctrl.IngestSucceeded || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled completion state=%v error=%v", state, err)
	}
}
