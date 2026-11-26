// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/k8s/dispatcher"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestAgentRuntimeRolloutTracksDeploymentWithoutInvocations(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	replicas := int32(2)
	agent := &api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "work", Name: "worker", UID: "worker-uid"},
		Spec:       api.AgentSpec{ModelRef: api.LocalObjectReference{Name: "chat"}, Replicas: &replicas},
	}
	model := &api.Model{Spec: api.ModelSpec{Provider: "openai", ModelID: "chat"}}
	for key, object := range map[string]any{"Agent/work/worker": agent, "Model/work/chat": model} {
		data, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		st.getData[key] = data
	}
	admin := fake.NewClientBuilder().WithScheme(SetupScheme()).WithStatusSubresource(&appsv1.Deployment{}).Build()
	runtime := dispatcher.NewK8sDispatcher(admin, st, zap.NewNop(), dispatcher.Config{AgentImage: "runtime:v1"})
	adapter := NewAgentReconcilerAdapter(st, runtime, zap.NewNop())
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "work", Name: "worker"}}
	observe := func(wantReady string, updated, available float64) {
		t.Helper()
		if _, err := adapter.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
		var saved struct {
			Status struct {
				Runtime map[string]any `json:"runtime"`
			} `json:"status"`
		}
		if err := json.Unmarshal(st.getData["Agent/work/worker"], &saved); err != nil {
			t.Fatal(err)
		}
		rollout := saved.Status.Runtime
		if rollout["ready"] != wantReady || rollout["updatedReplicas"] != updated || rollout["availableReplicas"] != available {
			t.Fatalf("runtime status = %v, want ready=%s updated=%v available=%v", rollout, wantReady, updated, available)
		}
	}
	observe("0/2", 0, 0)
	var deployment appsv1.Deployment
	key := client.ObjectKey{Namespace: "work", Name: "agent-worker"}
	if err := admin.Get(ctx, key, &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Status = appsv1.DeploymentStatus{Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 1, AvailableReplicas: 1}
	if err := admin.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	observe("1/2", 2, 1)
	if err := admin.Get(ctx, key, &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas = 2, 2
	if err := admin.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	observe("2/2", 2, 2)
	if err := admin.Get(ctx, key, &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas = 0, 0
	if err := admin.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	observe("0/2", 2, 0)
}

func TestAgentRuntimeStatusRefreshesWhenProvisioningFails(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	replicas := int32(2)
	agent := &api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "work", Name: "worker", UID: "worker-uid"},
		Spec:       api.AgentSpec{ModelRef: api.LocalObjectReference{Name: "chat"}, Replicas: &replicas},
	}

	model := &api.Model{Spec: api.ModelSpec{Provider: "openai", ModelID: "chat"}}
	for key, object := range map[string]any{"Agent/work/worker": agent, "Model/work/chat": model} {
		data, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		st.getData[key] = data
	}
	failWrites := false
	failure := errors.New("provisioning rejected")
	admin := fake.NewClientBuilder().WithScheme(SetupScheme()).WithStatusSubresource(&appsv1.Deployment{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if failWrites {
					return failure
				}
				return c.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if failWrites {
					return failure
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	runtime := dispatcher.NewK8sDispatcher(admin, st, zap.NewNop(), dispatcher.Config{AgentImage: "runtime:v1"})
	adapter := NewAgentReconcilerAdapter(st, runtime, zap.NewNop())
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "work", Name: "worker"}}
	reconcileReady := func() any {
		t.Helper()
		if _, err := adapter.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
		var saved struct {
			Status struct {
				Runtime map[string]any `json:"runtime"`
			} `json:"status"`
		}
		if err := json.Unmarshal(st.getData["Agent/work/worker"], &saved); err != nil {
			t.Fatal(err)
		}
		return saved.Status.Runtime["ready"]
	}
	reconcileReady()
	var deployment appsv1.Deployment
	key := client.ObjectKey{Namespace: "work", Name: "agent-worker"}
	if err := admin.Get(ctx, key, &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Status = appsv1.DeploymentStatus{Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2}
	if err := admin.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	if got := reconcileReady(); got != "2/2" {
		t.Fatalf("healthy ready = %v, want 2/2", got)
	}

	failWrites = true
	if err := admin.Delete(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	if got := reconcileReady(); got != "0/2" {
		t.Fatalf("ready after failed recreation = %v, want 0/2 instead of a stale 2/2", got)
	}
}

func TestAgentRuntimeWarningsDoNotLogSensitiveErrors(t *testing.T) {
	const secret = "synthetic-kubernetes-credential-do-not-log"
	failure := fmt.Errorf("remote API rejected request: %w", errors.Join(errors.New(secret), errors.New("backend response")))
	st := newFakeStore()
	replicas := int32(1)
	agent := &api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "work", Name: "worker", UID: "worker-uid"},
		Spec:       api.AgentSpec{ModelRef: api.LocalObjectReference{Name: "chat"}, Replicas: &replicas},
	}
	model := &api.Model{Spec: api.ModelSpec{Provider: "openai", ModelID: "chat"}}
	for key, object := range map[string]any{"Agent/work/worker": agent, "Model/work/chat": model} {
		data, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		st.getData[key] = data
	}
	admin := fake.NewClientBuilder().WithScheme(SetupScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					return failure
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	runtime := dispatcher.NewK8sDispatcher(admin, st, logger, dispatcher.Config{AgentImage: "runtime:v1"})
	adapter := NewAgentReconcilerAdapter(st, runtime, logger)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "work", Name: "worker"}}
	if _, err := adapter.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("runtime warning changed reconciliation behavior: %v", err)
	}
	for _, message := range []string{"failed to ensure agent runtime", "failed to refresh agent runtime status"} {
		entries := logs.FilterMessage(message).All()
		if len(entries) != 1 {
			t.Fatalf("warning %q count = %d, want 1", message, len(entries))
		}
		fields := entries[0].ContextMap()
		if fields["namespace"] != request.Namespace || fields["agent"] != request.Name || fields["requestUID"] == nil || fields["requestUID"] == "" {
			t.Fatalf("missing warning correlation fields: %v", fields)
		}
		if _, exists := fields["error"]; exists {
			t.Fatal("raw runtime error field was logged")
		}
	}
	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, secret) || strings.Contains(fmt.Sprint(entry.ContextMap()), secret) {
			t.Fatalf("sensitive runtime content reached logs: %s", entry.Message)
		}
	}
}
