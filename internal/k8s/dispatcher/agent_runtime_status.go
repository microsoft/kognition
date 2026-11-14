// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dispatcher

import (
	"context"
	"fmt"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReadAgentRuntimeStatus observes only the Agent's shared managed Deployment.
func (d *K8sDispatcher) ReadAgentRuntimeStatus(ctx context.Context, agent *api.Agent) (*api.AgentRuntimeStatus, error) {
	if agent == nil {
		return nil, fmt.Errorf("agent is required")
	}
	if d.externalEndpoints != nil {
		return nil, nil
	}
	if d.client == nil {
		return nil, fmt.Errorf("Kubernetes client is required for runtime rollout status")
	}
	desired := int32(1)
	if agent.Spec.Replicas != nil {
		desired = *agent.Spec.Replicas
	}
	status := &api.AgentRuntimeStatus{DesiredReplicas: desired, Ready: fmt.Sprintf("0/%d", desired)}
	namespace := agent.Namespace
	if d.config.AgentNamespace != "" {
		namespace = d.config.AgentNamespace
	}
	key := client.ObjectKey{Namespace: namespace, Name: deploymentName(d.workloadKey(agent.Namespace, agent.Name))}
	var deployment appsv1.Deployment
	if err := d.client.Get(ctx, key, &deployment); apierrors.IsNotFound(err) {
		return status, nil
	} else if err != nil {
		return nil, fmt.Errorf("read Agent runtime Deployment %s: %w", key, err)
	}
	if deployment.Annotations[AnnotationOwnerAgent] != agent.Namespace+"/"+agent.Name {
		return nil, fmt.Errorf("runtime Deployment %s does not belong to Agent %s/%s", key, agent.Namespace, agent.Name)
	}
	if !deployment.DeletionTimestamp.IsZero() {
		return status, nil
	}
	status.Replicas = deployment.Status.Replicas
	status.ReadyReplicas = deployment.Status.ReadyReplicas
	status.UpdatedReplicas = deployment.Status.UpdatedReplicas
	status.AvailableReplicas = deployment.Status.AvailableReplicas
	if deployment.Status.ObservedGeneration < deployment.Generation {
		status.UpdatedReplicas = 0
	}
	status.Ready = fmt.Sprintf("%d/%d", status.ReadyReplicas, desired)
	return status, nil
}
