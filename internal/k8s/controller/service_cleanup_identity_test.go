// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"net/http"
	"testing"

	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestAgentServicePublisherPreservesForeignService(t *testing.T) {
	for _, operation := range []string{"ensure", "delete"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			foreign := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: "work", UID: "foreign", Annotations: map[string]string{"kognition.dev/owner-agent-service": "other/endpoint"}}}
			adapter, kube := serviceIdentityFixture(t, foreign, interceptor.Funcs{})
			service := &api.AgentService{ObjectMeta: api.ObjectMeta{Name: "endpoint", Namespace: "work"}}
			var err error
			if operation == "ensure" {
				_, err = adapter.Ensure(ctx, service, &api.Agent{ObjectMeta: api.ObjectMeta{Name: "worker", Namespace: "work"}})
			} else {
				err = adapter.Delete(ctx, service)
			}
			if err == nil {
				t.Fatal("foreign Service was not rejected")
			}
			var current corev1.Service
			if err := kube.Get(ctx, client.ObjectKeyFromObject(foreign), &current); err != nil || current.UID != foreign.UID || current.ResourceVersion != foreign.ResourceVersion {
				t.Fatalf("foreign Service changed: UID=%s resourceVersion=%s error=%v", current.UID, current.ResourceVersion, err)
			}
		})
	}
}

func TestAgentServicePublisherPreservesReplacement(t *testing.T) {
	ctx := context.Background()
	original := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: "work", UID: "original", Annotations: map[string]string{"kognition.dev/owner-agent-service": "work/endpoint"}}}
	adapter, kube := serviceIdentityFixture(t, original, interceptor.Funcs{
		Delete: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.DeleteOption) error {
			if err := underlying.Delete(ctx, original); err != nil {
				return err
			}
			replacement := original.DeepCopy()
			replacement.UID = "replacement"
			replacement.ResourceVersion = ""
			if err := underlying.Create(ctx, replacement); err != nil {
				return err
			}
			deleteOptions := (&client.DeleteOptions{}).ApplyOptions(options)
			if deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil && *deleteOptions.Preconditions.UID != replacement.UID {
				return apierrors.NewConflict(schema.GroupResource{Resource: "services"}, object.GetName(), errors.New("UID precondition does not match"))
			}
			return underlying.Delete(ctx, object, options...)
		},
	})
	if err := adapter.Delete(ctx, &api.AgentService{ObjectMeta: api.ObjectMeta{Name: "endpoint", Namespace: "work"}}); !apierrors.IsConflict(err) {
		t.Fatalf("delete error=%v, want replacement conflict", err)
	}
	var current corev1.Service
	if err := kube.Get(ctx, client.ObjectKeyFromObject(original), &current); err != nil || current.UID != "replacement" {
		t.Fatalf("replacement UID=%s error=%v", current.UID, err)
	}
}

func serviceIdentityFixture(t *testing.T, object client.Object, interceptors interceptor.Funcs) (*AgentServiceReconcilerAdapter, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, k8sapi.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(object).WithInterceptorFuncs(interceptors).Build()
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(object), object); err != nil {
		t.Fatal(err)
	}
	manager, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Controller:     config.Controller{SkipNameValidation: ptr.To(true)},
		MapperProvider: func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return meta.NewDefaultRESTMapper(nil), nil },
		NewClient:      func(*rest.Config, client.Options) (client.Client, error) { return kube, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAgentServiceReconcilerAdapter(nil, zap.NewNop())
	if err := adapter.SetupWithManager(manager); err != nil {
		t.Fatal(err)
	}
	return adapter, kube
}
