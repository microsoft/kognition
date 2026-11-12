// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type RuntimeApprovalClaimRecoveryReconciler struct {
	client client.Client
	store  *StoreAdapter
}

func NewRuntimeApprovalClaimRecoveryReconciler(c client.Client, store *StoreAdapter) *RuntimeApprovalClaimRecoveryReconciler {
	return &RuntimeApprovalClaimRecoveryReconciler{client: c, store: store}
}

func (r *RuntimeApprovalClaimRecoveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	object := &k8s.RuntimeApproval{}
	if err := r.client.Get(ctx, req.NamespacedName, object); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	approval := runtimeApprovalFromKubernetes(object)
	if approval.Status.ExecutionState != api.RuntimeApprovalExecutionClaimed || approval.Status.Claimant == nil {
		return ctrl.Result{}, nil
	}
	claimant := approval.Status.Claimant
	if claimant.WorkloadUID == "" || claimant.ContainerID == "" {
		return ctrl.Result{}, nil
	}

	proofRef, err := r.store.RuntimeApprovalTerminationProof(ctx, *claimant)
	if err != nil {
		if errors.Is(err, errClaimantIncarnationPresent) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if _, err := r.store.MarkRuntimeApprovalClaimUncertain(ctx, approval.Namespace, approval.Spec.InvocationID, approval.ResourceVersion, approval.Status.FencingToken, proofRef); err != nil && !errors.Is(err, store.ErrConflict) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

var errClaimantIncarnationPresent = errors.New("RuntimeApproval claimant incarnation is still present")

func (s *StoreAdapter) RuntimeApprovalTerminationProof(ctx context.Context, claimant api.RuntimeApprovalClaimant) (string, error) {
	if claimant.WorkloadUID == "" || claimant.ContainerID == "" {
		return "", fmt.Errorf("termination proof requires exact retained Pod and container identity")
	}
	namespace := s.controlNamespace
	if namespace == "" {
		namespace = defaultControlNamespace
	}
	pods := &corev1.PodList{}
	if err := s.runtimeApprovalReader.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("read authoritative RuntimeApproval claimant termination: %w", err)
	}
	if podIncarnationExists(pods.Items, claimant.WorkloadUID, claimant.ContainerID) {
		return "", errClaimantIncarnationPresent
	}
	return kubernetesTerminationProofRef(claimant.WorkloadUID, claimant.ContainerID), nil
}

func podIncarnationExists(pods []corev1.Pod, workloadUID, containerID string) bool {
	for i := range pods {
		if string(pods[i].UID) != workloadUID {
			continue
		}
		for _, status := range append(pods[i].Status.InitContainerStatuses, pods[i].Status.ContainerStatuses...) {
			if status.ContainerID == containerID {
				return true
			}
		}
		for _, status := range pods[i].Status.EphemeralContainerStatuses {
			if status.ContainerID == containerID {
				return true
			}
		}
	}
	return false
}

func kubernetesTerminationProofRef(workloadUID, containerID string) string {
	digest := sha256.Sum256([]byte(workloadUID + "\x00" + containerID))
	return fmt.Sprintf("kubernetes://pod-incarnation-absent/sha256/%x", digest)
}

func (r *RuntimeApprovalClaimRecoveryReconciler) podToRuntimeApprovals(ctx context.Context, object client.Object) []reconcile.Request {
	approvals := &k8s.RuntimeApprovalList{}
	if err := r.client.List(ctx, approvals); err != nil {
		return nil
	}
	workloadUID := string(object.GetUID())
	requests := make([]reconcile.Request, 0)
	for i := range approvals.Items {
		approval := &approvals.Items[i]
		if approval.Status.ExecutionState != api.RuntimeApprovalExecutionClaimed || approval.Status.Claimant == nil || approval.Status.Claimant.WorkloadUID != workloadUID {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: approval.Namespace, Name: approval.Name}})
	}
	return requests
}

func (r *RuntimeApprovalClaimRecoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("runtimeapproval-claim-recovery").
		For(&k8s.RuntimeApproval{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.podToRuntimeApprovals)).
		Complete(r)
}
