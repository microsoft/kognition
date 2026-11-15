// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReadAgentRuntimeStatus(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		replicas    int32
		shared      bool
		missing     bool
		stale       bool
		foreign     bool
		deleting    bool
		ready       int32
		updated     int32
		available   int32
		wantReady   string
		wantUpdated int32
	}{
		{name: "missing", replicas: 2, missing: true, wantReady: "0/2"},
		{name: "starting", replicas: 2, wantReady: "0/2"},
		{name: "partial", replicas: 2, ready: 1, updated: 2, available: 1, wantReady: "1/2", wantUpdated: 2},
		{name: "ready shared namespace", replicas: 2, shared: true, ready: 2, updated: 2, available: 2, wantReady: "2/2", wantUpdated: 2},
		{name: "scale to zero", wantReady: "0/0"},
		{name: "unobserved rollout", replicas: 2, stale: true, ready: 2, updated: 2, available: 2, wantReady: "2/2"},
		{name: "terminating", replicas: 2, deleting: true, ready: 2, updated: 2, available: 2, wantReady: "0/2"},
		{name: "foreign deployment", replicas: 2, foreign: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := appsv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			config := Config{}
			namespace := "tenant"
			if scenario.shared {
				config.AgentNamespace, namespace = "runtimes", "runtimes"
			}
			d := NewK8sDispatcher(nil, nil, zap.NewNop(), config)
			agent := &api.Agent{ObjectMeta: api.ObjectMeta{Namespace: "tenant", Name: "worker"}, Spec: api.AgentSpec{Replicas: &scenario.replicas}}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: deploymentName(d.workloadKey("tenant", "worker")), Namespace: namespace, Generation: 2,
					Annotations: map[string]string{AnnotationOwnerAgent: "tenant/worker"},
				},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 2, Replicas: scenario.replicas, ReadyReplicas: scenario.ready,
					UpdatedReplicas: scenario.updated, AvailableReplicas: scenario.available,
				},
			}
			if scenario.stale {
				deployment.Status.ObservedGeneration = 1
			}
			if scenario.foreign {
				deployment.Annotations[AnnotationOwnerAgent] = "other/worker"
			}
			if scenario.deleting {
				now := metav1.Now()
				deployment.DeletionTimestamp, deployment.Finalizers = &now, []string{"test.example/retained"}
			}
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if !scenario.missing {
				builder.WithObjects(deployment)
			}
			d.client = builder.Build()
			status, err := d.ReadAgentRuntimeStatus(context.Background(), agent)
			if scenario.foreign {
				if err == nil || !strings.Contains(err.Error(), "does not belong") {
					t.Fatalf("foreign workload counted: status=%+v error=%v", status, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			expectedAvailable := scenario.available
			if scenario.deleting {
				expectedAvailable = 0
			}
			if status.Ready != scenario.wantReady || status.DesiredReplicas != scenario.replicas ||
				status.UpdatedReplicas != scenario.wantUpdated || status.AvailableReplicas != expectedAvailable {
				t.Fatalf("runtime status = %+v", status)
			}
		})
	}
}

func TestReadAgentRuntimeStatusSurfacesAPIError(t *testing.T) {
	want := errors.New("API unavailable")
	d := NewK8sDispatcher(fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return want
		},
	}).Build(), nil, zap.NewNop(), Config{})
	status, err := d.ReadAgentRuntimeStatus(context.Background(), &api.Agent{ObjectMeta: api.ObjectMeta{Namespace: "work", Name: "worker"}})
	if status != nil || !errors.Is(err, want) {
		t.Fatalf("API failure became successful zero counts: status=%+v error=%v", status, err)
	}
}
