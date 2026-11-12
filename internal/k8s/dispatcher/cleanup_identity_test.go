// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dispatcher

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCleanupAgentResourcesPreservesReplacement(t *testing.T) {
	for _, scenario := range []struct{ path, kind string }{
		{"shared", "deployment"}, {"shared", "service"}, {"shared", "policy"},
		{"revision", "deployment"}, {"revision", "service"}, {"revision", "policy"},
		{"legacy", "deployment"}, {"legacy", "service"},
	} {
		t.Run(scenario.path+"/"+scenario.kind, func(t *testing.T) {
			kind := scenario.kind
			ctx := context.Background()
			scheme := runtime.NewScheme()
			for _, register := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
				if err := register(scheme); err != nil {
					t.Fatal(err)
				}
			}
			metadata := metav1.ObjectMeta{Name: "agent-worker", Namespace: "work", UID: "original", Annotations: map[string]string{AnnotationOwnerAgent: "work/worker"}}
			config := Config{}
			if scenario.path == "revision" {
				metadata.Labels = map[string]string{labelAgentServiceWorkload: "worker"}
				metadata.Annotations[annotationOwnerAgentService] = "work/service"
			}
			if scenario.path == "legacy" {
				metadata.Namespace = "runtimes"
				config.AgentNamespace = "runtimes"
			}
			var original client.Object = &appsv1.Deployment{ObjectMeta: metadata}
			switch kind {
			case "service":
				original = &corev1.Service{ObjectMeta: metadata}
			case "policy":
				metadata.Name += "-network"
				original = &networkingv1.NetworkPolicy{ObjectMeta: metadata}
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(original).WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.DeleteOption) error {
					if err := underlying.Delete(ctx, original); err != nil {
						return err
					}
					replacement := original.DeepCopyObject().(client.Object)
					replacement.SetUID("replacement")
					replacement.SetResourceVersion("")
					if err := underlying.Create(ctx, replacement); err != nil {
						return err
					}
					deleteOptions := (&client.DeleteOptions{}).ApplyOptions(options)
					if deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil && *deleteOptions.Preconditions.UID != replacement.GetUID() {
						return apierrors.NewConflict(schema.GroupResource{Resource: kind}, object.GetName(), errors.New("UID precondition does not match"))
					}
					return underlying.Delete(ctx, object, options...)
				},
			}).Build()
			producer := NewK8sDispatcher(kube, nil, zap.NewNop(), config)
			cleanup := func() error { return producer.CleanupAgentResources(ctx, "worker", "work") }
			if scenario.path == "revision" {
				cleanup = func() error { return producer.CleanupAgentServiceRevisionWorkloads(ctx, "work", "service", "") }
			}
			if err := cleanup(); !apierrors.IsConflict(err) {
				t.Fatalf("cleanup error=%v, want replacement conflict", err)
			}
			current := original.DeepCopyObject().(client.Object)
			if err := kube.Get(ctx, client.ObjectKeyFromObject(original), current); err != nil || current.GetUID() != "replacement" {
				t.Fatalf("replacement UID=%s error=%v", current.GetUID(), err)
			}
		})
	}
}

func TestRevisionCleanupPreservesPolicyUntilDeploymentAbsent(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	metadata := metav1.ObjectMeta{Name: "revision", Namespace: "work", UID: "deployment-uid",
		Labels:      map[string]string{labelAgentServiceWorkload: "worker"},
		Annotations: map[string]string{annotationOwnerAgentService: "work/service"}}
	deployment := &appsv1.Deployment{ObjectMeta: metadata}
	deployment.Finalizers = []string{"test.kognition.dev/hold"}
	policy := &networkingv1.NetworkPolicy{ObjectMeta: *metadata.DeepCopy()}
	policy.Name += "-network"
	policy.UID = "policy-uid"
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, policy).Build()
	producer := NewK8sDispatcher(kube, nil, zap.NewNop(), Config{})
	if err := producer.CleanupAgentServiceRevisionWorkloads(ctx, "work", "service", ""); err == nil {
		t.Fatal("cleanup reported success while deployment remains")
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
		t.Fatalf("policy removed before deployment absence: %v", err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(deployment), deployment); err != nil || deployment.DeletionTimestamp.IsZero() {
		t.Fatalf("deployment deletion was not requested: %v", err)
	}
	deployment.Finalizers = nil
	if err := kube.Update(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	if err := producer.CleanupAgentServiceRevisionWorkloads(ctx, "work", "service", ""); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(policy), policy); !apierrors.IsNotFound(err) {
		t.Fatalf("policy remains after deployment deletion: %v", err)
	}
}
