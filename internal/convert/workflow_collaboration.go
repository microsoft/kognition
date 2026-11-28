// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func workflowCollaborationToK8s(in *api.WorkflowCollaboration) *k8s.WorkflowCollaboration {
	if in == nil {
		return nil
	}
	out := &k8s.WorkflowCollaboration{
		Strategy: k8s.CollaborationStrategy(in.Strategy),
		Protocol: k8s.CollaborationProtocol(in.Protocol),
		Guidance: in.Guidance,
		Limits: k8s.CollaborationLimits{
			MaxTurns:           in.Limits.MaxTurns,
			MaxMessages:        in.Limits.MaxMessages,
			MaxDelegations:     in.Limits.MaxDelegations,
			MaxDelegationDepth: in.Limits.MaxDelegationDepth,
			MaxParallelTasks:   in.Limits.MaxParallelTasks,
			MaxTokens:          in.Limits.MaxTokens,
			MaxContentBytes:    in.Limits.MaxContentBytes,
		},
	}
	if in.Members != nil {
		out.Members = make([]k8s.CollaborationMember, len(in.Members))
		for i, m := range in.Members {
			out.Members[i] = k8s.CollaborationMember{
				Name:        m.Name,
				Description: m.Description,
				AgentRef:    k8s.AgentReference{Name: m.AgentRef.Name, Namespace: m.AgentRef.Namespace},
			}
			if m.ExternalAgentRef != nil {
				ref := localRefToK8s(m.ExternalAgentRef.Name)
				out.Members[i].ExternalAgentRef = &ref
			}
			if m.ServiceRef != nil {
				ref := localRefToK8s(m.ServiceRef.Name)
				out.Members[i].ServiceRef = &ref
			}
		}
	}
	if in.AllowedInteractions != nil {
		out.AllowedInteractions = make([]k8s.CollaborationInteraction, len(in.AllowedInteractions))
		for i, a := range in.AllowedInteractions {
			out.AllowedInteractions[i] = k8s.CollaborationInteraction{From: a.From, To: a.To}
		}
	}
	if in.Supervisor != nil {
		out.Supervisor = &k8s.CollaborationSupervisor{Coordinator: in.Supervisor.Coordinator}
	}
	if in.Parallel != nil {
		out.Parallel = &k8s.CollaborationParallel{Synthesizer: in.Parallel.Synthesizer}
		if in.Parallel.Branches != nil {
			out.Parallel.Branches = make([]k8s.CollaborationBranch, len(in.Parallel.Branches))
			for i, b := range in.Parallel.Branches {
				out.Parallel.Branches[i] = k8s.CollaborationBranch{Member: b.Member, Optional: b.Optional}
			}
		}
	}
	if in.Handoff != nil {
		out.Handoff = &k8s.CollaborationHandoff{EntryMember: in.Handoff.EntryMember, MaxHandoffs: in.Handoff.MaxHandoffs}
	}
	if in.Peer != nil {
		out.Peer = &k8s.CollaborationPeer{Finalizer: in.Peer.Finalizer}
	}
	return out
}

func workflowCollaborationFromK8s(in *k8s.WorkflowCollaboration) *api.WorkflowCollaboration {
	if in == nil {
		return nil
	}
	out := &api.WorkflowCollaboration{
		Strategy: api.CollaborationStrategy(in.Strategy),
		Protocol: api.CollaborationProtocol(in.Protocol),
		Guidance: in.Guidance,
		Limits: api.CollaborationLimits{
			MaxTurns:           in.Limits.MaxTurns,
			MaxMessages:        in.Limits.MaxMessages,
			MaxDelegations:     in.Limits.MaxDelegations,
			MaxDelegationDepth: in.Limits.MaxDelegationDepth,
			MaxParallelTasks:   in.Limits.MaxParallelTasks,
			MaxTokens:          in.Limits.MaxTokens,
			MaxContentBytes:    in.Limits.MaxContentBytes,
		},
	}
	if in.Members != nil {
		out.Members = make([]api.CollaborationMember, len(in.Members))
		for i, m := range in.Members {
			out.Members[i] = api.CollaborationMember{
				Name:        m.Name,
				Description: m.Description,
				AgentRef:    api.AgentReference{Name: m.AgentRef.Name, Namespace: m.AgentRef.Namespace},
			}
			if m.ExternalAgentRef != nil {
				out.Members[i].ExternalAgentRef = &api.LocalObjectReference{Name: m.ExternalAgentRef.Name}
			}
			if m.ServiceRef != nil {
				out.Members[i].ServiceRef = &api.LocalObjectReference{Name: m.ServiceRef.Name}
			}
		}
	}
	if in.AllowedInteractions != nil {
		out.AllowedInteractions = make([]api.CollaborationInteraction, len(in.AllowedInteractions))
		for i, a := range in.AllowedInteractions {
			out.AllowedInteractions[i] = api.CollaborationInteraction{From: a.From, To: a.To}
		}
	}
	if in.Supervisor != nil {
		out.Supervisor = &api.CollaborationSupervisor{Coordinator: in.Supervisor.Coordinator}
	}
	if in.Parallel != nil {
		out.Parallel = &api.CollaborationParallel{Synthesizer: in.Parallel.Synthesizer}
		if in.Parallel.Branches != nil {
			out.Parallel.Branches = make([]api.CollaborationBranch, len(in.Parallel.Branches))
			for i, b := range in.Parallel.Branches {
				out.Parallel.Branches[i] = api.CollaborationBranch{Member: b.Member, Optional: b.Optional}
			}
		}
	}
	if in.Handoff != nil {
		out.Handoff = &api.CollaborationHandoff{EntryMember: in.Handoff.EntryMember, MaxHandoffs: in.Handoff.MaxHandoffs}
	}
	if in.Peer != nil {
		out.Peer = &api.CollaborationPeer{Finalizer: in.Peer.Finalizer}
	}
	return out
}
