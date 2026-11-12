// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

func TestAgentAutoscalerDependencyWatchesEnqueueAffectedAutoscaler(t *testing.T) {
	writer := &k8s.AgentAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "writer-scale", Namespace: "team-a"}, Spec: k8s.AgentAutoscalerSpec{AgentRef: k8s.LocalObjectReference{Name: "writer"}}}
	reader := &k8s.AgentAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "reader-scale", Namespace: "team-a"}, Spec: k8s.AgentAutoscalerSpec{AgentRef: k8s.LocalObjectReference{Name: "reader"}}}
	adapter := &AgentAutoscalerReconcilerAdapter{client: newFakeClient(t, writer, reader), logger: testLogger(t)}

	requests := adapter.agentToAutoscalers(context.Background(), &k8s.Agent{ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: "team-a"}})
	if len(requests) != 1 || requests[0].Name != "writer-scale" {
		t.Fatalf("agent requests = %+v", requests)
	}

	controller := true
	instance := &k8s.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "writer-1", Namespace: "team-a", OwnerReferences: []metav1.OwnerReference{{Kind: "Agent", Name: "writer", UID: types.UID("writer-uid"), Controller: &controller}}}}
	requests = adapter.instanceToAutoscalers(context.Background(), instance)
	if len(requests) != 1 || requests[0].Name != "writer-scale" {
		t.Fatalf("instance requests = %+v", requests)
	}

	instance.OwnerReferences[0].Kind = "Workflow"
	if requests := adapter.instanceToAutoscalers(context.Background(), instance); requests != nil {
		t.Fatalf("unrelated instance requests = %+v", requests)
	}
}

func TestAgentAutoscalerReconcilerAdapterScalesNativeAgent(t *testing.T) {
	replicas := int32(1)
	agent := &k8s.Agent{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "apps", UID: types.UID("worker-uid")}, Spec: k8s.AgentSpec{Replicas: &replicas}}
	autoscaler := &k8s.AgentAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "worker-scale", Namespace: "apps"}, Spec: k8s.AgentAutoscalerSpec{
		AgentRef: k8s.LocalObjectReference{Name: "worker"}, MinReplicas: 1, MaxReplicas: 5,
		Metrics: []k8s.AutoscalerMetric{{Type: k8s.MetricQueueDepth, TargetValue: 1}},
	}}
	controller := true
	instanceA := &k8s.AgentInstance{ObjectMeta: metav1.ObjectMeta{Name: "worker-a", Namespace: "apps", OwnerReferences: []metav1.OwnerReference{{Kind: "Agent", Name: "worker", UID: agent.UID, Controller: &controller}}}, Status: k8s.AgentInstanceStatus{Phase: "Running"}}
	instanceB := instanceA.DeepCopy()
	instanceB.Name = "worker-b"

	k8sClient := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, autoscaler, instanceA, instanceB).
		WithStatusSubresource(&k8s.Agent{}, &k8s.AgentInstance{}, &k8s.AgentAutoscaler{}).Build()
	adapter := NewAgentAutoscalerReconcilerAdapter(NewStoreAdapter(k8sClient), testLogger(t))
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(autoscaler)}
	if result, err := adapter.Reconcile(context.Background(), req); err != nil || !result.Requeue { //nolint:staticcheck // Immediate requeue remains part of the controller-runtime 1.x contract.
		t.Fatalf("finalizer reconcile = (%+v, %v)", result, err)
	}
	if _, err := adapter.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("scale reconcile: %v", err)
	}

	var scaled k8s.Agent
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(agent), &scaled); err != nil {
		t.Fatalf("get scaled agent: %v", err)
	}
	if scaled.Spec.Replicas == nil || *scaled.Spec.Replicas != 2 {
		t.Fatalf("replicas = %v, want 2", scaled.Spec.Replicas)
	}
}
