// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// AgentJobToK8s converts an internal AgentJob to a K8s AgentJob.
func AgentJobToK8s(in *api.AgentJob) *k8s.AgentJob {
	out := &k8s.AgentJob{
		Spec:   agentJobSpecToK8s(in.Spec),
		Status: agentJobStatusToK8s(in.Status),
	}
	out.ObjectMeta = objectMetaToK8s(in.ObjectMeta)
	out.TypeMeta = agentJobTypeMeta()
	return out
}

// AgentJobFromK8s converts a K8s AgentJob to an internal AgentJob.
func AgentJobFromK8s(in *k8s.AgentJob) *api.AgentJob {
	out := &api.AgentJob{
		Spec:   agentJobSpecFromK8s(in.Spec),
		Status: agentJobStatusFromK8s(in.Status),
	}
	out.ObjectMeta = objectMetaFromK8s(in.ObjectMeta)
	out.TypeMeta = api.TypeMeta{APIVersion: api.APIVersion, Kind: "AgentJob"}
	return out
}

func agentJobSpecToK8s(in api.AgentJobSpec) k8s.AgentJobSpec {
	out := k8s.AgentJobSpec{
		CompletionPolicy: string(in.CompletionPolicy),
		BackoffLimit:     in.BackoffLimit,
		Input:            in.Input,
	}
	if in.AgentRevision != nil {
		out.AgentRevision = new(int64)
		*out.AgentRevision = *in.AgentRevision
	}
	if in.AgentRef != nil {
		ref := localRefToK8s(in.AgentRef.Name)
		out.AgentRef = &ref
	}
	if in.AgentSpec != nil {
		spec := agentSpecToK8s(*in.AgentSpec)
		out.AgentSpec = &spec
	}
	if in.MemoryClaimRef != nil {
		ref := localRefToK8s(in.MemoryClaimRef.Name)
		out.MemoryClaimRef = &ref
	}
	return out
}

func agentJobSpecFromK8s(in k8s.AgentJobSpec) api.AgentJobSpec {
	out := api.AgentJobSpec{
		CompletionPolicy: api.AgentJobCompletionPolicy(in.CompletionPolicy),
		BackoffLimit:     in.BackoffLimit,
		Input:            in.Input,
	}
	if in.AgentRevision != nil {
		out.AgentRevision = new(int64)
		*out.AgentRevision = *in.AgentRevision
	}
	if in.AgentRef != nil {
		out.AgentRef = &api.LocalObjectReference{Name: localRefFromK8s(*in.AgentRef)}
	}
	if in.AgentSpec != nil {
		spec := agentSpecFromK8s(*in.AgentSpec)
		out.AgentSpec = &spec
	}
	if in.MemoryClaimRef != nil {
		out.MemoryClaimRef = &api.LocalObjectReference{Name: localRefFromK8s(*in.MemoryClaimRef)}
	}
	return out
}

func agentJobStatusToK8s(in api.AgentJobStatus) k8s.AgentJobStatus {
	out := k8s.AgentJobStatus{
		Phase:          phaseToString(in.Phase),
		Conditions:     conditionsToK8s(in.Conditions),
		Succeeded:      in.Succeeded,
		Failed:         in.Failed,
		StartTime:      timeToMetav1(in.StartTime),
		CompletionTime: timeToMetav1(in.CompletionTime),
		ToolCalls:      in.ToolCalls,
		Output:         in.Output,
	}
	if in.TokenUsage != nil {
		out.TokenUsage = &k8s.TokenUsage{
			PromptTokens:     in.TokenUsage.PromptTokens,
			CompletionTokens: in.TokenUsage.CompletionTokens,
			TotalTokens:      in.TokenUsage.TotalTokens,
		}
	}
	return out
}

func agentJobStatusFromK8s(in k8s.AgentJobStatus) api.AgentJobStatus {
	out := api.AgentJobStatus{
		Phase:          phaseFromString(in.Phase),
		Conditions:     conditionsFromK8s(in.Conditions),
		Succeeded:      in.Succeeded,
		Failed:         in.Failed,
		StartTime:      timeFromMetav1(in.StartTime),
		CompletionTime: timeFromMetav1(in.CompletionTime),
		ToolCalls:      in.ToolCalls,
		Output:         in.Output,
	}
	if in.TokenUsage != nil {
		out.TokenUsage = &api.TokenUsage{
			PromptTokens:     in.TokenUsage.PromptTokens,
			CompletionTokens: in.TokenUsage.CompletionTokens,
			TotalTokens:      in.TokenUsage.TotalTokens,
		}
	}
	return out
}

func agentJobTypeMeta() metav1.TypeMeta {
	return metav1.TypeMeta{Kind: "AgentJob", APIVersion: "kognition.dev/v1alpha1"}
}
