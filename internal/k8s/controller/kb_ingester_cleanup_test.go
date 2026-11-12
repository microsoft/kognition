// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestKBIngesterResetPreservesReplacement(t *testing.T) {
	for _, kind := range []string{"job", "secret"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			if err := batchv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "work", UID: "kb-original"}}
			jobName := ingestJobName(kb.ObjectMeta.Name, 1)
			metadata := metav1.ObjectMeta{Name: jobName, Namespace: "work", UID: "original", OwnerReferences: []metav1.OwnerReference{kbOwnerRef(kb)}}
			var original client.Object = &batchv1.Job{ObjectMeta: metadata}
			if kind == "secret" {
				metadata.Name += "-input"
				original = &corev1.Secret{ObjectMeta: metadata}
			}
			replaced := false
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(original).WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.DeleteOption) error {
					if !replaced {
						replaced = true
						if err := underlying.Delete(ctx, original); err != nil {
							return err
						}
						replacement := original.DeepCopyObject().(client.Object)
						replacement.SetResourceVersion("")
						replacement.SetUID("replacement")
						if err := underlying.Create(ctx, replacement); err != nil {
							return err
						}
					}
					deleteOptions := (&client.DeleteOptions{}).ApplyOptions(options)
					if deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil {
						current := original.DeepCopyObject().(client.Object)
						if err := underlying.Get(ctx, client.ObjectKeyFromObject(original), current); err != nil {
							return err
						}
						if *deleteOptions.Preconditions.UID != current.GetUID() {
							return apierrors.NewConflict(schema.GroupResource{Resource: kind}, object.GetName(), errors.New("UID precondition does not match"))
						}
					}
					return underlying.Delete(ctx, object, options...)
				},
			}).Build()
			launcher := NewKBIngesterLauncher(kube, nil, zap.NewNop(), "ingester:test", "", "control")
			if err := launcher.Reset(ctx, kb); !apierrors.IsConflict(err) {
				t.Fatalf("reset error = %v; want replacement conflict", err)
			}
			current := original.DeepCopyObject().(client.Object)
			if err := kube.Get(ctx, client.ObjectKeyFromObject(original), current); err != nil {
				t.Fatal(err)
			}
			if current.GetUID() != "replacement" {
				t.Fatalf("replacement not preserved: UID=%s", current.GetUID())
			}
		})
	}
}

func TestKBIngesterResetRetriesAPIFailures(t *testing.T) {
	resource := schema.GroupResource{Resource: "jobs"}
	for name, failure := range map[string]error{
		"forbidden": apierrors.NewForbidden(resource, "job", errors.New("denied")),
		"conflict":  apierrors.NewConflict(resource, "job", errors.New("changed")),
		"timeout":   apierrors.NewTimeoutError("unavailable", 1),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			if err := batchv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "work", UID: "kb-uid"}}
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: ingestJobName(kb.Name, 1), Namespace: "work", UID: "job-uid", OwnerReferences: []metav1.OwnerReference{kbOwnerRef(kb)}}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-input", Namespace: "work", UID: "secret-uid", OwnerReferences: job.OwnerReferences}}
			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job, secret).Build()
			faulting := interceptor.NewClient(base, interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return failure }})
			launcher := NewKBIngesterLauncher(faulting, nil, zap.NewNop(), "ingester:test", "", "control")
			if err := launcher.Reset(ctx, kb); !errors.Is(err, failure) {
				t.Fatalf("reset error=%v, want wrapped %v", err, failure)
			}
			for _, object := range []client.Object{job, secret} {
				if err := base.Get(ctx, client.ObjectKeyFromObject(object), object.DeepCopyObject().(client.Object)); err != nil {
					t.Fatalf("failed reset removed resource: %v", err)
				}
			}
			launcher = NewKBIngesterLauncher(base, nil, zap.NewNop(), "ingester:test", "", "control")
			if err := launcher.Reset(ctx, kb); err != nil {
				t.Fatal(err)
			}
			for _, object := range []client.Object{job, secret} {
				if err := base.Get(ctx, client.ObjectKeyFromObject(object), object.DeepCopyObject().(client.Object)); !apierrors.IsNotFound(err) {
					t.Fatalf("retry left resource: %v", err)
				}
			}
		})
	}
}

func TestKBIngesterConcurrentResetIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "work", UID: "kb-uid"}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: ingestJobName(kb.Name, 1), Namespace: "work", UID: "job-uid", OwnerReferences: []metav1.OwnerReference{kbOwnerRef(kb)}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-input", Namespace: "work", UID: "secret-uid", OwnerReferences: job.OwnerReferences}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job, secret).Build()
	arrived, release := make(chan struct{}, 2), make(chan struct{})
	kube := interceptor.NewClient(base, interceptor.Funcs{Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
		err := underlying.Get(ctx, key, object, options...)
		if _, ok := object.(*corev1.Secret); ok && err == nil {
			arrived <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return err
	}})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			results <- NewKBIngesterLauncher(kube, nil, zap.NewNop(), "ingester:test", "", "control").Reset(ctx, kb)
		}()
	}
	for range 2 {
		select {
		case <-arrived:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for _, object := range []client.Object{job, secret} {
		if err := base.Get(ctx, client.ObjectKeyFromObject(object), object.DeepCopyObject().(client.Object)); !apierrors.IsNotFound(err) {
			t.Fatalf("concurrent reset left resource: %v", err)
		}
	}
}
