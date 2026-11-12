// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestResolveRuntimeApprovalClaimant(t *testing.T) {
	ctx := context.Background()
	client := newFakeClient(t, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kognition-system", Name: "gateway-pod", UID: types.UID("pod-uid-1")},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "sidecar", ContainerID: "containerd://sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			{Name: "tool-gateway", ContainerID: "containerd://gateway", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}},
	})

	claimant, err := ResolveRuntimeApprovalClaimant(ctx, client, "kognition-system", "gateway-pod", "tool-gateway", "pod-uid-1")
	if err != nil {
		t.Fatal(err)
	}
	if claimant.WorkloadUID != "pod-uid-1" || claimant.ContainerID != "containerd://gateway" {
		t.Fatalf("claimant = %#v", claimant)
	}

	if _, err := ResolveRuntimeApprovalClaimant(ctx, client, "kognition-system", "gateway-pod", "tool-gateway", "different-uid"); err == nil {
		t.Fatal("expected Pod UID mismatch to fail")
	}
	if _, err := ResolveRuntimeApprovalClaimant(ctx, client, "kognition-system", "gateway-pod", "missing", "pod-uid-1"); err == nil {
		t.Fatal("expected missing container status to fail")
	}

	terminatedClient := newFakeClient(t, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kognition-system", Name: "terminated-gateway-pod", UID: types.UID("pod-uid-2")},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "tool-gateway", ContainerID: "containerd://terminated", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}},
		}}},
	})
	if _, err := ResolveRuntimeApprovalClaimant(ctx, terminatedClient, "kognition-system", "terminated-gateway-pod", "tool-gateway", "pod-uid-2"); err == nil {
		t.Fatal("expected terminated container status to fail")
	}
}

func TestRuntimeApprovalAdmissionClaimantUsesLiveAuthenticatedContainer(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "control", Name: "gateway-pod", UID: types.UID("pod-uid")}, Spec: corev1.PodSpec{ServiceAccountName: "tool-gateway"}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "tool-gateway", ContainerID: "containerd://current", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	adapter := NewStoreAdapterWithControlNamespace(newFakeClient(t, pod), "control")
	claimant := api.RuntimeApprovalClaimant{WorkloadUID: "pod-uid", ContainerID: "containerd://current", ProcessStartNonce: "process-a"}
	ctx := context.Background()
	if err := adapter.VerifyRuntimeApprovalClaimant(ctx, "system:serviceaccount:control:tool-gateway", pod.Name, string(pod.UID), api.RuntimeApprovalPlaneTool, claimant); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct{ user, podName, podUID, plane, container string }{
		{"system:serviceaccount:control:other", pod.Name, "pod-uid", api.RuntimeApprovalPlaneTool, "containerd://current"},
		{"system:serviceaccount:control:tool-gateway", pod.Name, "old-pod", api.RuntimeApprovalPlaneTool, "containerd://current"},
		{"system:serviceaccount:control:tool-gateway", pod.Name, "pod-uid", api.RuntimeApprovalPlaneTool, "containerd://old-process"},
		{"system:serviceaccount:control:tool-gateway", pod.Name, "pod-uid", api.RuntimeApprovalPlaneMemory, "containerd://current"},
	} {
		candidate := claimant
		candidate.ContainerID = scenario.container
		if err := adapter.VerifyRuntimeApprovalClaimant(ctx, scenario.user, scenario.podName, scenario.podUID, scenario.plane, candidate); err == nil {
			t.Fatal("unbound, foreign or restarted claimant accepted", scenario)
		}
	}
}

func TestWaitForRuntimeApprovalClaimantRetriesWaitingContainer(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kognition-system", Name: "starting-gateway-pod", UID: types.UID("pod-uid-3")},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "tool-gateway", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
		}}},
	}
	client := newFakeClient(t, pod)
	go func() {
		time.Sleep(20 * time.Millisecond)
		current := &corev1.Pod{}
		if err := client.Get(context.Background(), types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, current); err != nil {
			return
		}
		current.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		current.Status.ContainerStatuses[0].ContainerID = "containerd://starting-gateway"
		_ = client.Status().Update(context.Background(), current)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	claimant, err := WaitForRuntimeApprovalClaimant(ctx, client, pod.Namespace, pod.Name, "tool-gateway", string(pod.UID))
	if err != nil {
		t.Fatal(err)
	}
	if claimant.ContainerID != "containerd://starting-gateway" {
		t.Fatalf("claimant = %#v", claimant)
	}
}
