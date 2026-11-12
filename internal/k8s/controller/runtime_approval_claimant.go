// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func (s *StoreAdapter) VerifyRuntimeApprovalClaimant(ctx context.Context, username, podName, podUID, plane string, claimant api.RuntimeApprovalClaimant) error {
	if s.controlNamespace == "" || podName == "" || podUID == "" || claimant.WorkloadUID != podUID {
		return fmt.Errorf("claimant requires exact authenticated Pod binding")
	}
	pod := &corev1.Pod{}
	if err := s.runtimeApprovalReader.Get(ctx, client.ObjectKey{Namespace: s.controlNamespace, Name: podName}, pod); err != nil {
		return err
	}
	if pod.DeletionTimestamp != nil || string(pod.UID) != podUID || pod.Spec.ServiceAccountName == "" || username != "system:serviceaccount:"+s.controlNamespace+":"+pod.Spec.ServiceAccountName {
		return fmt.Errorf("claimant Pod is not the current authenticated gateway workload")
	}
	containerName := ""
	switch plane {
	case api.RuntimeApprovalPlaneTool:
		containerName = "tool-gateway"
	case api.RuntimeApprovalPlaneMemory:
		containerName = "memory-policy-gateway"
	case api.RuntimeApprovalPlaneCommunication:
		containerName = "agent-communication-gateway"
	default:
		return fmt.Errorf("unsupported claimant execution plane")
	}
	resolved, err := ResolveRuntimeApprovalClaimant(ctx, s.runtimeApprovalReader, s.controlNamespace, podName, containerName, podUID)
	if err != nil {
		return err
	}
	if claimant.ContainerID == "" || claimant.ContainerID != resolved.ContainerID {
		return fmt.Errorf("claimant container is no longer the live gateway process")
	}
	return nil
}

type claimantStatusNotReadyError struct {
	err error
}

func (e claimantStatusNotReadyError) Error() string { return e.err.Error() }
func (e claimantStatusNotReadyError) Unwrap() error { return e.err }

func WaitForRuntimeApprovalClaimant(ctx context.Context, reader client.Reader, namespace, podName, containerName, expectedWorkloadUID string) (api.RuntimeApprovalClaimant, error) {
	for {
		claimant, err := ResolveRuntimeApprovalClaimant(ctx, reader, namespace, podName, containerName, expectedWorkloadUID)
		if err == nil {
			return claimant, nil
		}
		var notReady claimantStatusNotReadyError
		if !errors.As(err, &notReady) {
			return api.RuntimeApprovalClaimant{}, err
		}

		select {
		case <-ctx.Done():
			return api.RuntimeApprovalClaimant{}, fmt.Errorf("wait for RuntimeApproval claimant status: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func ResolveRuntimeApprovalClaimant(ctx context.Context, reader client.Reader, namespace, podName, containerName, expectedWorkloadUID string) (api.RuntimeApprovalClaimant, error) {
	namespace = strings.TrimSpace(namespace)
	podName = strings.TrimSpace(podName)
	containerName = strings.TrimSpace(containerName)
	expectedWorkloadUID = strings.TrimSpace(expectedWorkloadUID)
	if namespace == "" || podName == "" || containerName == "" || expectedWorkloadUID == "" {
		return api.RuntimeApprovalClaimant{}, fmt.Errorf("resolve RuntimeApproval claimant requires namespace, Pod name, container name, and workload UID")
	}

	pod := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, pod); err != nil {
		return api.RuntimeApprovalClaimant{}, fmt.Errorf("get claimant Pod %s/%s: %w", namespace, podName, err)
	}
	if string(pod.UID) != expectedWorkloadUID {
		return api.RuntimeApprovalClaimant{}, fmt.Errorf("claimant Pod UID %q does not match injected workload UID %q", pod.UID, expectedWorkloadUID)
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != containerName {
			continue
		}
		if status.State.Waiting != nil {
			return api.RuntimeApprovalClaimant{}, claimantStatusNotReadyError{err: fmt.Errorf("claimant container %q is waiting", containerName)}
		}
		if status.State.Running == nil {
			return api.RuntimeApprovalClaimant{}, fmt.Errorf("claimant container %q is not running", containerName)
		}
		if status.ContainerID == "" {
			return api.RuntimeApprovalClaimant{}, claimantStatusNotReadyError{err: fmt.Errorf("claimant container %q has no runtime container ID", containerName)}
		}
		return api.RuntimeApprovalClaimant{WorkloadUID: expectedWorkloadUID, ContainerID: status.ContainerID}, nil
	}
	return api.RuntimeApprovalClaimant{}, claimantStatusNotReadyError{err: fmt.Errorf("claimant Pod %s/%s has no status for container %q", namespace, podName, containerName)}
}
