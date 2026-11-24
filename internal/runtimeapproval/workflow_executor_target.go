package runtimeapproval

import (
	"context"
	"encoding/json"
	"fmt"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

type WorkflowExecutorResourceReader interface {
	GetWorkflowExecutorResource(context.Context, string, string, string) ([]byte, error)
}

func AuthorizeWorkflowExecutorTarget(ctx context.Context, reader WorkflowExecutorResourceReader, action WorkflowExecutorAction,
	invokePermission func(context.Context, WorkflowExecutorAction) error) error {
	if reader == nil || invokePermission == nil {
		return ErrWorkflowDispatchUnsupported
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateExecutorActionIdentity(action); err != nil {
		return err
	}
	if err := invokePermission(ctx, action); err != nil {
		return err
	}
	agentData, err := reader.GetWorkflowExecutorResource(ctx, "Agent", action.Target.Namespace, action.Target.Name)
	if err != nil {
		return fmt.Errorf("read executor target Agent: %w", err)
	}
	if len(agentData) > WorkflowExecutorRecordMaxBytes {
		return fmt.Errorf("executor Agent definition exceeds its content bound")
	}
	var agent api.Agent
	if err := json.Unmarshal(agentData, &agent); err != nil {
		return fmt.Errorf("decode executor target Agent: %w", err)
	}
	if !executorResourceMatches(agent.ObjectMeta, action.Target) || !qualifiedCollaborationAgent(agent) {
		return fmt.Errorf("executor target Agent is changed, unready, or not a qualified native runtime")
	}
	if action.Target.ConfigurationDigest != "" {
		digest, err := WorkflowExecutorSpecDigest(agentData)
		if err != nil || digest != action.Target.ConfigurationDigest {
			return fmt.Errorf("executor target Agent configuration changed")
		}
	}
	serviceData, err := reader.GetWorkflowExecutorResource(ctx, "AgentService", action.Service.Namespace, action.Service.Name)
	if err != nil {
		return fmt.Errorf("read executor target AgentService: %w", err)
	}
	if len(serviceData) > WorkflowExecutorRecordMaxBytes {
		return fmt.Errorf("executor service definition exceeds its content bound")
	}
	var service api.AgentService
	if err := json.Unmarshal(serviceData, &service); err != nil {
		return fmt.Errorf("decode executor target AgentService: %w", err)
	}
	if !executorResourceMatches(service.ObjectMeta, action.Service) || !qualifiedCollaborationService(agent, service) {
		return fmt.Errorf("executor target AgentService is changed, unready, or not pinned to the accepted Agent revision")
	}
	if action.Service.ConfigurationDigest != "" {
		digest, err := WorkflowExecutorSpecDigest(serviceData)
		if err != nil || digest != action.Service.ConfigurationDigest {
			return fmt.Errorf("executor target service configuration changed")
		}
	}
	return ctx.Err()
}

func executorResourceMatches(metadata api.ObjectMeta, reference api.RuntimeApprovalReference) bool {
	return metadata.DeletionTimestamp == nil && metadata.UID != "" && metadata.ResourceVersion > 0 && metadata.Namespace == reference.Namespace && metadata.Name == reference.Name && metadata.UID == reference.UID &&
		metadata.ResourceVersion == reference.ResourceVersion
}
