// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"encoding/json"
	"fmt"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controlplaneauth"
	"k8s.io/apimachinery/pkg/util/validation"
)

type WorkflowCollaborationResourceReader interface {
	WorkflowExecutorResourceReader
	ListWorkflowExecutorResources(context.Context, string, string) ([][]byte, error)
}

type WorkflowCollaborationMemberResolver struct {
	Reader             WorkflowCollaborationResourceReader
	AuthorizeReference func(context.Context, string, api.RuntimeApprovalReference) error
	AuthorizeInvoke    func(context.Context, api.RuntimeApprovalReference, api.RuntimeApprovalReference) error
}

type ResolvedWorkflowCollaborationMember struct {
	Name     string                       `json:"name"`
	Agent    api.RuntimeApprovalReference `json:"agent"`
	Service  api.RuntimeApprovalReference `json:"service"`
	Revision int64                        `json:"revision"`
}

func qualifiedCollaborationAgent(agent api.Agent) bool {
	return agent.UID != "" && agent.ResourceVersion > 0 && agent.DeletionTimestamp == nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.Class == "native" &&
		agent.Status.Phase == api.PhaseReady && agent.Status.RevisionCount > 0 && agent.Status.Runtime != nil && agent.Status.Runtime.Ready == "True" &&
		agent.Status.Runtime.ReadyReplicas > 0 && agent.Status.Runtime.AvailableReplicas > 0
}

func qualifiedCollaborationService(agent api.Agent, service api.AgentService) bool {
	return service.Namespace == agent.Namespace && service.UID != "" && service.ResourceVersion > 0 && service.DeletionTimestamp == nil && service.Spec.AgentRef.Name == agent.Name &&
		service.Status.Phase == api.PhaseReady && service.Status.ReadyInstances > 0 && service.Spec.RevisionPin != nil && *service.Spec.RevisionPin > 0 &&
		service.Status.ActiveRevision == *service.Spec.RevisionPin && *service.Spec.RevisionPin <= agent.Status.RevisionCount
}

func resolvedCollaborationReference(kind string, metadata api.ObjectMeta, raw []byte) (api.RuntimeApprovalReference, error) {
	digest, err := WorkflowExecutorSpecDigest(raw)
	if err != nil {
		return api.RuntimeApprovalReference{}, err
	}
	return api.RuntimeApprovalReference{Kind: kind, Scope: "Namespaced", Namespace: metadata.Namespace, Name: metadata.Name, UID: metadata.UID, ResourceVersion: metadata.ResourceVersion, ConfigurationDigest: digest}, nil
}

func (resolver *WorkflowCollaborationMemberResolver) Resolve(ctx context.Context, workspace string, member api.CollaborationMember) (ResolvedWorkflowCollaborationMember, error) {
	if resolver == nil || resolver.Reader == nil || resolver.AuthorizeReference == nil || resolver.AuthorizeInvoke == nil {
		return ResolvedWorkflowCollaborationMember{}, ErrWorkflowDispatchUnsupported
	}
	if err := ctx.Err(); err != nil {
		return ResolvedWorkflowCollaborationMember{}, err
	}
	if workspace == "" || member.Name == "" || member.AgentRef.Name == "" {
		return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member alias and namespaced Agent are required")
	}
	namespace := member.AgentRef.Namespace
	if namespace == "" {
		namespace = workspace
	}
	reference := api.RuntimeApprovalReference{Kind: "Agent", Scope: "Namespaced", Namespace: namespace, Name: member.AgentRef.Name}
	if err := resolver.AuthorizeReference(ctx, workspace, reference); err != nil {
		return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member %q Agent reference denied: %w", member.Name, err)
	}
	raw, err := resolver.Reader.GetWorkflowExecutorResource(ctx, "Agent", namespace, member.AgentRef.Name)
	if err != nil {
		return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member %q Agent unavailable: %w", member.Name, err)
	}
	var agent api.Agent
	if len(raw) > WorkflowExecutorRecordMaxBytes || json.Unmarshal(raw, &agent) != nil || agent.Namespace != namespace || agent.Name != member.AgentRef.Name || !qualifiedCollaborationAgent(agent) {
		return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member %q requires a Ready native Agent incarnation", member.Name)
	}
	agentReference, err := resolvedCollaborationReference("Agent", agent.ObjectMeta, raw)
	if err != nil {
		return ResolvedWorkflowCollaborationMember{}, err
	}
	var candidates [][]byte
	if member.ServiceRef != nil {
		if member.ServiceRef.Name == "" {
			return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member %q serviceRef is empty", member.Name)
		}
		reference.Kind, reference.Name = "AgentService", member.ServiceRef.Name
		if err := resolver.AuthorizeReference(ctx, workspace, reference); err != nil {
			return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member %q service reference denied: %w", member.Name, err)
		}
		candidate, err := resolver.Reader.GetWorkflowExecutorResource(ctx, "AgentService", namespace, member.ServiceRef.Name)
		if err != nil {
			return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member %q service unavailable: %w", member.Name, err)
		}
		candidates = [][]byte{candidate}
	} else {
		candidates, err = resolver.Reader.ListWorkflowExecutorResources(ctx, "AgentService", namespace)
		if err != nil {
			return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member %q service discovery unavailable: %w", member.Name, err)
		}
	}
	if len(candidates) > 256 {
		return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member %q service discovery exceeds its finite bound", member.Name)
	}
	var selected api.AgentService
	var selectedRaw []byte
	eligible := 0
	for _, candidate := range candidates {
		var service api.AgentService
		if len(candidate) > WorkflowExecutorRecordMaxBytes || json.Unmarshal(candidate, &service) != nil || service.Namespace != namespace {
			return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member %q service discovery returned an invalid namespaced resource", member.Name)
		}
		if member.ServiceRef != nil && service.Name != member.ServiceRef.Name {
			return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member %q service identity changed", member.Name)
		}
		if !qualifiedCollaborationService(agent, service) {
			continue
		}
		eligible++
		selected, selectedRaw = service, candidate
	}
	if eligible != 1 {
		return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member %q requires exactly one Ready revision-pinned AgentService; found %d", member.Name, eligible)
	}
	serviceReference, err := resolvedCollaborationReference("AgentService", selected.ObjectMeta, selectedRaw)
	if err != nil {
		return ResolvedWorkflowCollaborationMember{}, err
	}
	if err := resolver.AuthorizeReference(ctx, workspace, agentReference); err != nil {
		return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member %q Agent reference revoked: %w", member.Name, err)
	}
	if err := resolver.AuthorizeReference(ctx, workspace, serviceReference); err != nil {
		return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member %q service reference denied: %w", member.Name, err)
	}
	if err := resolver.AuthorizeInvoke(ctx, agentReference, serviceReference); err != nil {
		return ResolvedWorkflowCollaborationMember{}, fmt.Errorf("member %q invocation denied: %w", member.Name, err)
	}
	if err := ctx.Err(); err != nil {
		return ResolvedWorkflowCollaborationMember{}, err
	}
	return ResolvedWorkflowCollaborationMember{Name: member.Name, Agent: agentReference, Service: serviceReference, Revision: *selected.Spec.RevisionPin}, nil
}

func (resolver *WorkflowCollaborationMemberResolver) ResolveWorkflowStep(ctx context.Context, authority WorkflowExecutorAuthority, workflowJSON []byte, stepName string) ([]ResolvedWorkflowCollaborationMember, error) {
	if err := ValidateWorkflowExecutorAuthorityDefinition(authority, workflowJSON); err != nil {
		return nil, err
	}
	var workflow api.Workflow
	if err := json.Unmarshal(workflowJSON, &workflow); err != nil {
		return nil, err
	}
	var collaboration *api.WorkflowCollaboration
	for _, step := range workflow.Spec.Steps {
		if step.Name != stepName {
			continue
		}
		if collaboration != nil || step.Type != api.WorkflowStepTypeCollaboration || step.Collaboration == nil {
			return nil, fmt.Errorf("execution requires one exact collaboration step")
		}
		collaboration = step.Collaboration
	}
	if collaboration == nil {
		return nil, fmt.Errorf("approved Workflow has no matching collaboration step")
	}
	limits := collaboration.Limits
	if limits.MaxTurns < 1 || int64(limits.MaxTurns) > int64(authority.Limits.MaxTurns) || limits.MaxMessages < 1 || int64(limits.MaxMessages) > int64(authority.Limits.MaxMessages) ||
		limits.MaxDelegations < 1 || int64(limits.MaxDelegations) > int64(authority.Limits.MaxDelegations) || limits.MaxDelegationDepth < 1 || int64(limits.MaxDelegationDepth) > int64(authority.Limits.MaxHandoffDepth) ||
		limits.MaxParallelTasks < 1 || int64(limits.MaxParallelTasks) > int64(authority.Limits.MaxParallelTasks) || limits.MaxTokens < 1 {
		return nil, fmt.Errorf("Workflow collaboration limits exceed the approved execution policy")
	}
	return resolver.ResolveApprovedMembers(ctx, authority, collaboration.Members)
}

func AuthorizeWorkflowExecutorStep(ctx context.Context, reader WorkflowCollaborationResourceReader, authority WorkflowExecutorAuthority, action WorkflowExecutorAction, authorizeTarget func(context.Context, WorkflowExecutorAction) error) error {
	if reader == nil || authorizeTarget == nil {
		return ErrWorkflowDispatchUnsupported
	}
	workflowJSON, err := reader.GetWorkflowExecutorResource(ctx, "Workflow", authority.Workflow.Namespace, authority.Workflow.Name)
	if err != nil {
		return err
	}
	resolver := &WorkflowCollaborationMemberResolver{Reader: reader,
		AuthorizeReference: func(ctx context.Context, workspace string, reference api.RuntimeApprovalReference) error {
			return AuthorizeWorkflowExecutorReference(ctx, reader, workspace, reference)
		},
		AuthorizeInvoke: func(ctx context.Context, agent, service api.RuntimeApprovalReference) error {
			memberAction := action
			memberAction.Target, memberAction.Service = agent, service
			return authorizeTarget(ctx, memberAction)
		},
	}
	members, err := resolver.ResolveWorkflowStep(ctx, authority, workflowJSON, action.Activation.Step)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.Agent == action.Target && member.Service == action.Service {
			return ctx.Err()
		}
	}
	return fmt.Errorf("initiation target is not in the approved collaboration step")
}

func (resolver *WorkflowCollaborationMemberResolver) ResolveApprovedMembers(ctx context.Context, authority WorkflowExecutorAuthority, members []api.CollaborationMember) ([]ResolvedWorkflowCollaborationMember, error) {
	if err := ValidateWorkflowExecutorAuthority(authority); err != nil {
		return nil, err
	}
	if !authority.Enabled || len(members) == 0 || len(members) > authority.Limits.MaxMembers {
		return nil, fmt.Errorf("declared members require a bounded approved target set")
	}
	aliases, targets := map[string]bool{}, map[api.RuntimeApprovalReference]bool{}
	resolved := make([]ResolvedWorkflowCollaborationMember, 0, len(members))
	for _, member := range members {
		if len(validation.IsDNS1123Label(member.Name)) != 0 || aliases[member.Name] {
			return nil, fmt.Errorf("member aliases must be unique DNS labels")
		}
		aliases[member.Name] = true
		current, err := resolver.Resolve(ctx, authority.Workflow.Namespace, member)
		if err != nil {
			return nil, err
		}
		approved := false
		for _, target := range authority.Targets {
			approved = approved || target.Agent == current.Agent && target.Service == current.Service && target.Operation == WorkflowExecutorInitiate
		}
		if !approved || targets[current.Agent] {
			return nil, fmt.Errorf("member %q changed its approved Agent or pinned service", member.Name)
		}
		targets[current.Agent] = true
		resolved = append(resolved, current)
	}
	return resolved, ctx.Err()
}

func AuthorizeWorkflowExecutorReference(ctx context.Context, reader WorkflowCollaborationResourceReader, workspace string, reference api.RuntimeApprovalReference) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if reader == nil || workspace == "" || reference.Namespace == "" || reference.Name == "" || reference.Scope != "Namespaced" || reference.Kind != "Agent" && reference.Kind != "AgentService" {
		return controlplaneauth.ErrForbidden
	}
	if reference.Namespace == workspace {
		return nil
	}
	grants, err := reader.ListWorkflowExecutorResources(ctx, "CrossNamespaceGrant", reference.Namespace)
	if err != nil {
		return fmt.Errorf("executor reference grants unavailable: %w", err)
	}
	if len(grants) > 256 {
		return controlplaneauth.ErrUnavailable
	}
	for _, raw := range grants {
		var grant api.CrossNamespaceGrant
		if len(raw) > WorkflowExecutorRecordMaxBytes || json.Unmarshal(raw, &grant) != nil {
			return controlplaneauth.ErrUnavailable
		}
		if grant.Namespace != reference.Namespace || grant.UID == "" || grant.ResourceVersion <= 0 || grant.DeletionTimestamp != nil ||
			grant.Spec.From != reference.Namespace || grant.Spec.To != workspace && grant.Spec.To != "*" {
			continue
		}
		for _, resource := range grant.Spec.Resources {
			if resource.Kind == reference.Kind && (resource.Name == reference.Name || resource.Name == "*") {
				return ctx.Err()
			}
		}
	}
	return controlplaneauth.ErrForbidden
}
