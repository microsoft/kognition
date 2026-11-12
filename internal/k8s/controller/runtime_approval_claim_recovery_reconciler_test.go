// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

type runtimeApprovalRecoveryStaleCache struct{ client.Client }

func (c *runtimeApprovalRecoveryStaleCache) List(ctx context.Context, objects client.ObjectList, options ...client.ListOption) error {
	if pods, ok := objects.(*corev1.PodList); ok {
		pods.Items = nil
		return nil
	}
	return c.Client.List(ctx, objects, options...)
}

type runtimeApprovalRecoveryListErrorClient struct {
	client.Client
	err error
}

func (c *runtimeApprovalRecoveryListErrorClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return c.err
}

func TestRuntimeApprovalClaimRecoveryReconcilerRequiresExactIncarnationTermination(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	client := newFakeClient(t)
	adapter := newRuntimeApprovalTestStore(t, client)
	adapter.now = func() time.Time { return now }
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))

	createClaimed := func(t *testing.T, invocationID, containerID string) ctrl.Request {
		t.Helper()
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, invocationID, api.RuntimeApprovalDecisionNotRequired)
		token := mustGenerateKubernetesPreparationToken(t)
		prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", invocationID, approval.ResourceVersion, token, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		claimant := newKubernetesClaimant(now)
		claimant.WorkloadUID = "pod-uid-1"
		claimant.ContainerID = containerID
		if _, err := adapter.ClaimRuntimeApprovalExecution(ctx, "team-a", invocationID, prepared.ResourceVersion, token, claimant); err != nil {
			t.Fatal(err)
		}
		return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: runtimeapproval.CanonicalName("team-a", invocationID)}}
	}

	liveRequest := createClaimed(t, "live-incarnation", "containerd://live")
	if err := client.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kognition-system", Name: "tool-gateway", UID: types.UID("pod-uid-1")},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{ContainerID: "containerd://live"}}},
	}); err != nil {
		t.Fatal(err)
	}
	reconciler := NewRuntimeApprovalClaimRecoveryReconciler(&runtimeApprovalRecoveryStaleCache{Client: client}, adapter)
	if _, err := reconciler.Reconcile(ctx, liveRequest); err != nil {
		t.Fatalf("reconcile live claimant: %v", err)
	}
	assertRuntimeApprovalExecutionState(t, ctx, client, liveRequest.NamespacedName, api.RuntimeApprovalExecutionClaimed)

	deadRequest := createClaimed(t, "dead-incarnation", "containerd://dead")
	if _, err := reconciler.Reconcile(ctx, deadRequest); err != nil {
		t.Fatalf("reconcile terminated claimant: %v", err)
	}
	object := &k8s.RuntimeApproval{}
	if err := client.Get(ctx, deadRequest.NamespacedName, object); err != nil {
		t.Fatal(err)
	}
	if object.Status.ExecutionState != api.RuntimeApprovalExecutionUncertain {
		t.Fatalf("execution state = %q, want %q", object.Status.ExecutionState, api.RuntimeApprovalExecutionUncertain)
	}
	if object.Status.TerminationProofRef == "" {
		t.Fatal("termination proof reference is empty")
	}

	incompleteRequest := createClaimed(t, "incomplete-incarnation", "")
	if _, err := reconciler.Reconcile(ctx, incompleteRequest); err != nil {
		t.Fatalf("reconcile incomplete claimant: %v", err)
	}
	assertRuntimeApprovalExecutionState(t, ctx, client, incompleteRequest.NamespacedName, api.RuntimeApprovalExecutionClaimed)

	replacedRequest := createClaimed(t, "replaced-incarnation", "containerd://old")
	if _, err := reconciler.Reconcile(ctx, replacedRequest); err != nil {
		t.Fatalf("reconcile replaced claimant: %v", err)
	}
	assertRuntimeApprovalExecutionState(t, ctx, client, replacedRequest.NamespacedName, api.RuntimeApprovalExecutionUncertain)
}

func TestRuntimeApprovalClaimRecoveryReconcilerFailsClosedWithoutPodEvidence(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	baseClient := newFakeClient(t)
	adapter := newRuntimeApprovalTestStore(t, baseClient)
	adapter.now = func() time.Time { return now }
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))

	notStarted := newExecutionReadyKubernetesApproval(t, ctx, adapter, "not-claimed", api.RuntimeApprovalDecisionNotRequired)
	faultClient := &runtimeApprovalRecoveryListErrorClient{Client: baseClient, err: errors.New("Pod evidence unavailable")}
	adapter.ConfigureRuntimeApprovalReader(faultClient)
	reconciler := NewRuntimeApprovalClaimRecoveryReconciler(faultClient, adapter)
	notStartedRequest := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: notStarted.Name}}
	if _, err := reconciler.Reconcile(ctx, notStartedRequest); err != nil {
		t.Fatalf("reconcile non-claimed approval: %v", err)
	}
	assertRuntimeApprovalExecutionState(t, ctx, baseClient, notStartedRequest.NamespacedName, api.RuntimeApprovalExecutionNotStarted)

	claimed := newExecutionReadyKubernetesApproval(t, ctx, adapter, "proof-unavailable", api.RuntimeApprovalDecisionNotRequired)
	token := mustGenerateKubernetesPreparationToken(t)
	prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", claimed.Spec.InvocationID, claimed.ResourceVersion, token, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimant := newKubernetesClaimant(now)
	claimant.WorkloadUID = "pod-uid-1"
	claimant.ContainerID = "containerd://gateway"
	claimed, err = adapter.ClaimRuntimeApprovalExecution(ctx, "team-a", claimed.Spec.InvocationID, prepared.ResourceVersion, token, claimant)
	if err != nil {
		t.Fatal(err)
	}
	claimedRequest := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: claimed.Name}}
	if _, err := reconciler.Reconcile(ctx, claimedRequest); err == nil {
		t.Fatal("expected unavailable Pod evidence to fail reconciliation")
	}
	assertRuntimeApprovalExecutionState(t, ctx, baseClient, claimedRequest.NamespacedName, api.RuntimeApprovalExecutionClaimed)
}

func TestRuntimeApprovalTerminationProofUsesAuthoritativeExactIdentity(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"container", "init", "ephemeral"} {
		t.Run(kind, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "control", Name: "owner", UID: types.UID("owner-pod")}}
			status := corev1.ContainerStatus{ContainerID: "containerd://owner"}
			switch kind {
			case "container":
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{status}
			case "init":
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{status}
			case "ephemeral":
				pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{status}
			}
			reader := newFakeClient(t, pod)
			adapter := NewStoreAdapterWithControlNamespace(&runtimeApprovalRecoveryStaleCache{Client: reader}, "control")
			adapter.ConfigureRuntimeApprovalReader(reader)
			claimant := api.RuntimeApprovalClaimant{WorkloadUID: "owner-pod", ContainerID: "containerd://owner"}
			if _, err := adapter.RuntimeApprovalTerminationProof(ctx, claimant); !errors.Is(err, errClaimantIncarnationPresent) {
				t.Fatal("present exact incarnation considered terminated", err)
			}
			claimant.ContainerID = "containerd://old"
			proof, err := adapter.RuntimeApprovalTerminationProof(ctx, claimant)
			if err != nil || proof != kubernetesTerminationProofRef(claimant.WorkloadUID, claimant.ContainerID) {
				t.Fatal("canonical absent-incarnation proof differs", proof, err)
			}
			claimant.ContainerID = ""
			if _, err := adapter.RuntimeApprovalTerminationProof(ctx, claimant); err == nil {
				t.Fatal("incomplete claimant absence inferred")
			}
			claimant.ContainerID = "containerd://old"
			adapter.ConfigureRuntimeApprovalReader(&runtimeApprovalRecoveryListErrorClient{Client: reader, err: errors.New("API unavailable")})
			if _, err := adapter.RuntimeApprovalTerminationProof(ctx, claimant); err == nil {
				t.Fatal("unavailable authoritative evidence became termination")
			}
		})
	}
}

func assertRuntimeApprovalExecutionState(t *testing.T, ctx context.Context, reader client.Reader, key types.NamespacedName, want string) {
	t.Helper()
	object := &k8s.RuntimeApproval{}
	if err := reader.Get(ctx, key, object); err != nil {
		t.Fatal(err)
	}
	if object.Status.ExecutionState != want {
		t.Fatalf("execution state = %q, want %q", object.Status.ExecutionState, want)
	}
}
