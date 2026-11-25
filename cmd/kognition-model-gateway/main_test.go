// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kognition-ai/kognition/internal/gatewaybackend"
	"github.com/kognition-ai/kognition/internal/k8s/callhealth"
	"github.com/kognition-ai/kognition/internal/store"
)

type healthCapableStore struct {
	gatewaybackend.Store
	store.ModelCallHealthStore
}

type plainStore struct{ gatewaybackend.Store }

func TestModelCallHealthStoreFollowsTheBackendMode(t *testing.T) {
	logger := zap.NewNop()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	kubernetes := &gatewaybackend.Backend{KubernetesClient: kubeClient, Store: plainStore{}}
	configMaps, err := modelCallHealthStore(kubernetes, "kognition-system", logger)
	if err != nil {
		t.Fatalf("Kubernetes mode error: %v", err)
	}
	if _, isConfigMapStore := configMaps.(*callhealth.ConfigMapStore); !isConfigMapStore {
		t.Fatalf("Kubernetes mode returned %T, want the ConfigMap store", configMaps)
	}
	// Admission policy binds each writer identity to the component in the object
	// name, so the gateway's objects must be named for the model gateway.
	if err := configMaps.ReplaceModelCallHealth(context.Background(), "gateway-pod", []store.ModelCallHealth{{Target: "t", Calls: 1}}); err != nil {
		t.Fatalf("ReplaceModelCallHealth() error: %v", err)
	}
	var published corev1.ConfigMapList
	if err := kubeClient.List(context.Background(), &published, client.InNamespace("kognition-system")); err != nil {
		t.Fatal(err)
	}
	if len(published.Items) != 1 || !strings.HasPrefix(published.Items[0].Name, callhealth.NamePrefix+"model-gateway-") ||
		published.Items[0].Labels[callhealth.HealthLabel] != "model-gateway" {
		t.Fatalf("published %+v, want one ConfigMap named and labeled for the model gateway", published.Items)
	}

	standalone := &gatewaybackend.Backend{Store: healthCapableStore{}}
	if got, err := modelCallHealthStore(standalone, "ignored", logger); err != nil || got == nil {
		t.Fatalf("standalone mode returned %v, %v, want the PostgreSQL store's health support", got, err)
	}

	if got, err := modelCallHealthStore(&gatewaybackend.Backend{Store: plainStore{}}, "ignored", logger); err != nil || got != nil {
		t.Fatalf("a backend that cannot share health returned %v, %v, want nil", got, err)
	}

	if _, err := modelCallHealthStore(kubernetes, "", logger); err == nil {
		t.Fatal("a Kubernetes backend without a namespace must be rejected")
	}
}
