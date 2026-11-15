// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// AgentToK8s converts an internal Agent to a K8s Agent.
func AgentToK8s(in *api.Agent) *k8s.Agent {
	out := &k8s.Agent{
		Spec:   agentSpecToK8s(in.Spec),
		Status: agentStatusToK8s(in.Status),
	}
	out.ObjectMeta = objectMetaToK8s(in.ObjectMeta)
	out.TypeMeta = agentTypeMeta()
	return out
}

// AgentFromK8s converts a K8s Agent to an internal Agent.
func AgentFromK8s(in *k8s.Agent) *api.Agent {
	out := &api.Agent{
		Spec:   agentSpecFromK8s(in.Spec),
		Status: agentStatusFromK8s(in.Status),
	}
	out.ObjectMeta = objectMetaFromK8s(in.ObjectMeta)
	out.TypeMeta = api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"}
	return out
}

// AgentInstanceToK8s converts an internal AgentInstance to a K8s AgentInstance.
func AgentInstanceToK8s(in *api.AgentInstance) *k8s.AgentInstance {
	out := &k8s.AgentInstance{
		Spec: k8s.AgentInstanceSpec{
			AgentRef:       localRefToK8s(in.Spec.AgentRef.Name),
			Revision:       in.Spec.Revision,
			SessionContext: in.Spec.SessionContext,
		},
		Status: agentInstanceStatusToK8s(in.Status),
	}
	out.ObjectMeta = objectMetaToK8s(in.ObjectMeta)
	if in.Spec.AgentRevision != nil {
		out.Spec.AgentRevision = new(int64)
		*out.Spec.AgentRevision = *in.Spec.AgentRevision
	}
	out.TypeMeta = agentInstanceTypeMeta()
	return out
}

// AgentInstanceFromK8s converts a K8s AgentInstance to an internal AgentInstance.
func AgentInstanceFromK8s(in *k8s.AgentInstance) *api.AgentInstance {
	out := &api.AgentInstance{
		Spec: api.AgentInstanceSpec{
			AgentRef:       api.LocalObjectReference{Name: localRefFromK8s(in.Spec.AgentRef)},
			Revision:       in.Spec.Revision,
			SessionContext: in.Spec.SessionContext,
		},
		Status: agentInstanceStatusFromK8s(in.Status),
	}
	out.ObjectMeta = objectMetaFromK8s(in.ObjectMeta)
	if in.Spec.AgentRevision != nil {
		out.Spec.AgentRevision = new(int64)
		*out.Spec.AgentRevision = *in.Spec.AgentRevision
	}
	out.TypeMeta = api.TypeMeta{APIVersion: api.APIVersion, Kind: "AgentInstance"}
	return out
}

// --- helpers ---

func agentSpecToK8s(in api.AgentSpec) k8s.AgentSpec {
	out := k8s.AgentSpec{
		ModelRef:     localRefToK8s(in.ModelRef.Name),
		Instructions: agentInstructionsToK8s(in.Instructions),
		Replicas:     in.Replicas,
	}
	if len(in.Tools) > 0 {
		out.Tools = make([]k8s.AgentToolRef, len(in.Tools))
		for i, t := range in.Tools {
			out.Tools[i] = k8s.AgentToolRef{
				Ref:      localRefToK8s(t.Ref.Name),
				Required: t.Required,
			}
		}
	}
	if in.Memory != nil {
		out.Memory = &k8s.AgentMemorySpec{}
		if len(in.Memory.Claims) > 0 {
			out.Memory.Claims = make([]k8s.LocalObjectReference, len(in.Memory.Claims))
			for i, c := range in.Memory.Claims {
				out.Memory.Claims[i] = localRefToK8s(c.Name)
			}
		}
	}
	if in.Runtime != nil {
		out.Runtime = &k8s.AgentRuntimeSpec{
			Class:     in.Runtime.Class,
			Timeout:   in.Runtime.Timeout,
			MaxTokens: in.Runtime.MaxTokens,
		}
	}
	if len(in.Companions) > 0 {
		out.Companions = make([]k8s.LocalObjectReference, len(in.Companions))
		for i, c := range in.Companions {
			out.Companions[i] = localRefToK8s(c.Name)
		}
	}
	if len(in.KnowledgeBaseClaims) > 0 {
		out.KnowledgeBaseClaims = make([]k8s.KnowledgeBaseClaimRef, len(in.KnowledgeBaseClaims))
		for i, kb := range in.KnowledgeBaseClaims {
			out.KnowledgeBaseClaims[i] = k8s.KnowledgeBaseClaimRef{
				Name:     kb.Name,
				Required: kb.Required,
			}
		}
	}
	if in.AutonomyClassRef != nil {
		ref := localRefToK8s(in.AutonomyClassRef.Name)
		out.AutonomyClassRef = &ref
	}
	if in.Probes != nil {
		out.Probes = agentProbeSpecToK8s(in.Probes)
	}
	if in.RestartLadder != nil {
		out.RestartLadder = restartLadderToK8s(in.RestartLadder)
	}
	return out
}

func agentSpecFromK8s(in k8s.AgentSpec) api.AgentSpec {
	out := api.AgentSpec{
		ModelRef:     api.LocalObjectReference{Name: localRefFromK8s(in.ModelRef)},
		Instructions: agentInstructionsFromK8s(in.Instructions),
		Replicas:     in.Replicas,
	}
	if len(in.Tools) > 0 {
		out.Tools = make([]api.AgentToolRef, len(in.Tools))
		for i, t := range in.Tools {
			out.Tools[i] = api.AgentToolRef{
				Ref:      api.LocalObjectReference{Name: localRefFromK8s(t.Ref)},
				Required: t.Required,
			}
		}
	}
	if in.Memory != nil {
		out.Memory = &api.AgentMemorySpec{}
		if len(in.Memory.Claims) > 0 {
			out.Memory.Claims = make([]api.LocalObjectReference, len(in.Memory.Claims))
			for i, c := range in.Memory.Claims {
				out.Memory.Claims[i] = api.LocalObjectReference{Name: localRefFromK8s(c)}
			}
		}
	}
	if in.Runtime != nil {
		out.Runtime = &api.AgentRuntimeSpec{
			Class:     in.Runtime.Class,
			Timeout:   in.Runtime.Timeout,
			MaxTokens: in.Runtime.MaxTokens,
		}
	}
	if len(in.Companions) > 0 {
		out.Companions = make([]api.LocalObjectReference, len(in.Companions))
		for i, c := range in.Companions {
			out.Companions[i] = api.LocalObjectReference{Name: localRefFromK8s(c)}
		}
	}
	if len(in.KnowledgeBaseClaims) > 0 {
		out.KnowledgeBaseClaims = make([]api.KnowledgeBaseClaimRef, len(in.KnowledgeBaseClaims))
		for i, kb := range in.KnowledgeBaseClaims {
			out.KnowledgeBaseClaims[i] = api.KnowledgeBaseClaimRef{
				Name:     kb.Name,
				Required: kb.Required,
			}
		}
	}
	if in.AutonomyClassRef != nil {
		out.AutonomyClassRef = &api.LocalObjectReference{Name: localRefFromK8s(*in.AutonomyClassRef)}
	}
	if in.Probes != nil {
		out.Probes = agentProbeSpecFromK8s(in.Probes)
	}
	if in.RestartLadder != nil {
		out.RestartLadder = restartLadderFromK8s(in.RestartLadder)
	}
	return out
}

func agentInstructionsToK8s(in api.AgentInstructions) k8s.AgentInstructions {
	out := k8s.AgentInstructions{Inline: in.Inline}
	if in.Ref != nil {
		ref := localRefToK8s(in.Ref.Name)
		out.Ref = &ref
	}
	return out
}

func agentInstructionsFromK8s(in k8s.AgentInstructions) api.AgentInstructions {
	out := api.AgentInstructions{Inline: in.Inline}
	if in.Ref != nil {
		out.Ref = &api.LocalObjectReference{Name: localRefFromK8s(*in.Ref)}
	}
	return out
}

func agentProbeSpecToK8s(in *api.AgentProbeSpec) *k8s.AgentProbeSpec {
	out := &k8s.AgentProbeSpec{}
	if in.HealthProbe != nil {
		out.HealthProbe = &k8s.AgentHealthProbe{
			IntervalSeconds:  in.HealthProbe.IntervalSeconds,
			TimeoutSeconds:   in.HealthProbe.TimeoutSeconds,
			FailureThreshold: in.HealthProbe.FailureThreshold,
			SuccessThreshold: in.HealthProbe.SuccessThreshold,
		}
	}
	if in.ReadinessProbe != nil {
		out.ReadinessProbe = &k8s.AgentReadinessProbe{
			IntervalSeconds:  in.ReadinessProbe.IntervalSeconds,
			TimeoutSeconds:   in.ReadinessProbe.TimeoutSeconds,
			FailureThreshold: in.ReadinessProbe.FailureThreshold,
			SuccessThreshold: in.ReadinessProbe.SuccessThreshold,
		}
	}
	if in.ReasoningProbe != nil {
		out.ReasoningProbe = &k8s.AgentReasoningProbe{
			CanaryTask:       in.ReasoningProbe.CanaryTask,
			ExpectedOutcome:  in.ReasoningProbe.ExpectedOutcome,
			IntervalSeconds:  in.ReasoningProbe.IntervalSeconds,
			TimeoutSeconds:   in.ReasoningProbe.TimeoutSeconds,
			MaxTokens:        in.ReasoningProbe.MaxTokens,
			MaxInputTokens:   in.ReasoningProbe.MaxInputTokens,
			FailureThreshold: in.ReasoningProbe.FailureThreshold,
			SuccessThreshold: in.ReasoningProbe.SuccessThreshold,
		}
	}
	return out
}

func agentProbeSpecFromK8s(in *k8s.AgentProbeSpec) *api.AgentProbeSpec {
	out := &api.AgentProbeSpec{}
	if in.HealthProbe != nil {
		out.HealthProbe = &api.AgentHealthProbe{
			IntervalSeconds:  in.HealthProbe.IntervalSeconds,
			TimeoutSeconds:   in.HealthProbe.TimeoutSeconds,
			FailureThreshold: in.HealthProbe.FailureThreshold,
			SuccessThreshold: in.HealthProbe.SuccessThreshold,
		}
	}
	if in.ReadinessProbe != nil {
		out.ReadinessProbe = &api.AgentReadinessProbe{
			IntervalSeconds:  in.ReadinessProbe.IntervalSeconds,
			TimeoutSeconds:   in.ReadinessProbe.TimeoutSeconds,
			FailureThreshold: in.ReadinessProbe.FailureThreshold,
			SuccessThreshold: in.ReadinessProbe.SuccessThreshold,
		}
	}
	if in.ReasoningProbe != nil {
		out.ReasoningProbe = &api.AgentReasoningProbe{
			CanaryTask:       in.ReasoningProbe.CanaryTask,
			ExpectedOutcome:  in.ReasoningProbe.ExpectedOutcome,
			IntervalSeconds:  in.ReasoningProbe.IntervalSeconds,
			TimeoutSeconds:   in.ReasoningProbe.TimeoutSeconds,
			MaxTokens:        in.ReasoningProbe.MaxTokens,
			MaxInputTokens:   in.ReasoningProbe.MaxInputTokens,
			FailureThreshold: in.ReasoningProbe.FailureThreshold,
			SuccessThreshold: in.ReasoningProbe.SuccessThreshold,
		}
	}
	return out
}

func restartLadderToK8s(in *api.RestartLadder) *k8s.RestartLadder {
	if in == nil {
		return nil
	}
	out := &k8s.RestartLadder{}
	if len(in.Steps) > 0 {
		out.Steps = make([]k8s.RestartLadderStep, len(in.Steps))
		for i, s := range in.Steps {
			out.Steps[i] = k8s.RestartLadderStep{
				Action:               k8s.RestartAction(s.Action),
				StabilisationSeconds: s.StabilisationSeconds,
				MaxAttempts:          s.MaxAttempts,
			}
		}
	}
	return out
}

func restartLadderFromK8s(in *k8s.RestartLadder) *api.RestartLadder {
	if in == nil {
		return nil
	}
	out := &api.RestartLadder{}
	if len(in.Steps) > 0 {
		out.Steps = make([]api.RestartLadderStep, len(in.Steps))
		for i, s := range in.Steps {
			out.Steps[i] = api.RestartLadderStep{
				Action:               api.RestartAction(s.Action),
				StabilisationSeconds: s.StabilisationSeconds,
				MaxAttempts:          s.MaxAttempts,
			}
		}
	}
	return out
}

func agentStatusToK8s(in api.AgentStatus) k8s.AgentStatus {
	out := k8s.AgentStatus{
		Phase:         phaseToString(in.Phase),
		Conditions:    conditionsToK8s(in.Conditions),
		Replicas:      in.Replicas,
		ReadyReplicas: in.ReadyReplicas,
		RevisionCount: in.RevisionCount,
	}
	if in.Runtime != nil {
		out.Runtime = &k8s.AgentRuntimeStatus{
			Ready: in.Runtime.Ready, DesiredReplicas: in.Runtime.DesiredReplicas,
			Replicas: in.Runtime.Replicas, ReadyReplicas: in.Runtime.ReadyReplicas,
			UpdatedReplicas: in.Runtime.UpdatedReplicas, AvailableReplicas: in.Runtime.AvailableReplicas,
		}
	}
	return out
}

func agentStatusFromK8s(in k8s.AgentStatus) api.AgentStatus {
	out := api.AgentStatus{
		Phase:         phaseFromString(in.Phase),
		Conditions:    conditionsFromK8s(in.Conditions),
		Replicas:      in.Replicas,
		ReadyReplicas: in.ReadyReplicas,
		RevisionCount: in.RevisionCount,
	}
	if in.Runtime != nil {
		out.Runtime = &api.AgentRuntimeStatus{
			Ready: in.Runtime.Ready, DesiredReplicas: in.Runtime.DesiredReplicas,
			Replicas: in.Runtime.Replicas, ReadyReplicas: in.Runtime.ReadyReplicas,
			UpdatedReplicas: in.Runtime.UpdatedReplicas, AvailableReplicas: in.Runtime.AvailableReplicas,
		}
	}
	return out
}

func agentInstanceStatusToK8s(in api.AgentInstanceStatus) k8s.AgentInstanceStatus {
	out := k8s.AgentInstanceStatus{
		Phase:            phaseToString(in.Phase),
		Conditions:       conditionsToK8s(in.Conditions),
		SessionID:        in.SessionID,
		StartTime:        timeToMetav1(in.StartTime),
		ToolCalls:        in.ToolCalls,
		ToolCallsDetail:  toolCallDetailsToK8s(in.ToolCallsDetail),
		RestartCount:     in.RestartCount,
		DispatchAttempts: in.DispatchAttempts,
		Output:           in.Output,
	}
	if in.TokenUsage != nil {
		out.TokenUsage = &k8s.TokenUsage{
			PromptTokens:     in.TokenUsage.PromptTokens,
			CompletionTokens: in.TokenUsage.CompletionTokens,
			TotalTokens:      in.TokenUsage.TotalTokens,
		}
	}
	if in.ProbeStatus != nil {
		out.ProbeStatus = probeStatusToK8s(in.ProbeStatus)
	}
	return out
}

func agentInstanceStatusFromK8s(in k8s.AgentInstanceStatus) api.AgentInstanceStatus {
	out := api.AgentInstanceStatus{
		Phase:            phaseFromString(in.Phase),
		Conditions:       conditionsFromK8s(in.Conditions),
		SessionID:        in.SessionID,
		StartTime:        timeFromMetav1(in.StartTime),
		ToolCalls:        in.ToolCalls,
		ToolCallsDetail:  toolCallDetailsFromK8s(in.ToolCallsDetail),
		RestartCount:     in.RestartCount,
		DispatchAttempts: in.DispatchAttempts,
		Output:           in.Output,
	}
	if in.TokenUsage != nil {
		out.TokenUsage = &api.TokenUsage{
			PromptTokens:     in.TokenUsage.PromptTokens,
			CompletionTokens: in.TokenUsage.CompletionTokens,
			TotalTokens:      in.TokenUsage.TotalTokens,
		}
	}
	if in.ProbeStatus != nil {
		out.ProbeStatus = probeStatusFromK8s(in.ProbeStatus)
	}
	return out
}

func toolCallDetailsToK8s(in []api.ToolCallDetail) []k8s.ToolCallDetail {
	if len(in) == 0 {
		return nil
	}
	out := make([]k8s.ToolCallDetail, len(in))
	for index, detail := range in {
		out[index] = k8s.ToolCallDetail{Name: detail.Name, Args: detail.Args, Result: detail.Result, DurationMS: detail.DurationMS, Error: detail.Error}
	}
	return out
}

func toolCallDetailsFromK8s(in []k8s.ToolCallDetail) []api.ToolCallDetail {
	if len(in) == 0 {
		return nil
	}
	out := make([]api.ToolCallDetail, len(in))
	for index, detail := range in {
		out[index] = api.ToolCallDetail{Name: detail.Name, Args: detail.Args, Result: detail.Result, DurationMS: detail.DurationMS, Error: detail.Error}
	}
	return out
}

func probeStatusToK8s(in *api.ProbeStatus) *k8s.ProbeStatus {
	if in == nil {
		return nil
	}
	return &k8s.ProbeStatus{
		Healthy:                       in.Healthy,
		Ready:                         in.Ready,
		ReasoningHealthy:              in.ReasoningHealthy,
		ConsecutiveHealthFailures:     in.ConsecutiveHealthFailures,
		ConsecutiveReadinessFailures:  in.ConsecutiveReadinessFailures,
		ConsecutiveReasoningFailures:  in.ConsecutiveReasoningFailures,
		ConsecutiveHealthSuccesses:    in.ConsecutiveHealthSuccesses,
		ConsecutiveReadinessSuccesses: in.ConsecutiveReadinessSuccesses,
		ConsecutiveReasoningSuccesses: in.ConsecutiveReasoningSuccesses,
		LadderStep:                    in.LadderStep,
		LadderAttempts:                in.LadderAttempts,
		LastProbeTime:                 in.LastProbeTime,
		LastReasoningScheduleSlot:     clonePointer(in.LastReasoningScheduleSlot),
		ReasoningOperationID:          in.ReasoningOperationID,
		ReasoningRequestDigest:        in.ReasoningRequestDigest,
		ReasoningBindings:             canaryResourceBindingsToK8s(in.ReasoningBindings),
		LastReasoningResult:           reasoningCanaryResultToK8s(in.LastReasoningResult),
		LastLadderAction:              (*k8s.RestartAction)(in.LastLadderAction),
		RolledBackToRevision:          in.RolledBackToRevision,
	}
}

func probeStatusFromK8s(in *k8s.ProbeStatus) *api.ProbeStatus {
	if in == nil {
		return nil
	}
	return &api.ProbeStatus{
		Healthy:                       in.Healthy,
		Ready:                         in.Ready,
		ReasoningHealthy:              in.ReasoningHealthy,
		ConsecutiveHealthFailures:     in.ConsecutiveHealthFailures,
		ConsecutiveReadinessFailures:  in.ConsecutiveReadinessFailures,
		ConsecutiveReasoningFailures:  in.ConsecutiveReasoningFailures,
		ConsecutiveHealthSuccesses:    in.ConsecutiveHealthSuccesses,
		ConsecutiveReadinessSuccesses: in.ConsecutiveReadinessSuccesses,
		ConsecutiveReasoningSuccesses: in.ConsecutiveReasoningSuccesses,
		LadderStep:                    in.LadderStep,
		LadderAttempts:                in.LadderAttempts,
		LastProbeTime:                 in.LastProbeTime,
		LastReasoningScheduleSlot:     clonePointer(in.LastReasoningScheduleSlot),
		ReasoningOperationID:          in.ReasoningOperationID,
		ReasoningRequestDigest:        in.ReasoningRequestDigest,
		ReasoningBindings:             canaryResourceBindingsFromK8s(in.ReasoningBindings),
		LastReasoningResult:           reasoningCanaryResultFromK8s(in.LastReasoningResult),
		LastLadderAction:              (*api.RestartAction)(in.LastLadderAction),
		RolledBackToRevision:          in.RolledBackToRevision,
	}
}

func canaryResourceBindingsToK8s(in []api.CanaryResourceBinding) []k8s.CanaryResourceBinding {
	if in == nil {
		return nil
	}
	out := make([]k8s.CanaryResourceBinding, len(in))
	for index, binding := range in {
		out[index] = k8s.CanaryResourceBinding{
			Kind: binding.Kind, Namespace: binding.Namespace, Name: binding.Name,
			UID: binding.UID, ResourceVersion: binding.ResourceVersion, Revision: clonePointer(binding.Revision),
		}
	}
	return out
}

func canaryResourceBindingsFromK8s(in []k8s.CanaryResourceBinding) []api.CanaryResourceBinding {
	if in == nil {
		return nil
	}
	out := make([]api.CanaryResourceBinding, len(in))
	for index, binding := range in {
		out[index] = api.CanaryResourceBinding{
			Kind: binding.Kind, Namespace: binding.Namespace, Name: binding.Name,
			UID: binding.UID, ResourceVersion: binding.ResourceVersion, Revision: clonePointer(binding.Revision),
		}
	}
	return out
}

func clonePointer[T any](in *T) *T {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func reasoningCanaryResultToK8s(in *api.ReasoningCanaryResult) *k8s.ReasoningCanaryResult {
	if in == nil {
		return nil
	}
	if err := in.Validate(); err != nil {
		return nil
	}
	return &k8s.ReasoningCanaryResult{
		OperationID: in.OperationID, ExpectedBytes: in.ExpectedBytes, ActualBytes: in.ActualBytes,
		ExpectedSHA256: in.ExpectedSHA256, ActualSHA256: in.ActualSHA256,
		PromptTokens: in.PromptTokens, CompletionTokens: in.CompletionTokens, TotalTokens: in.TotalTokens,
		ProviderDurationMillis: in.ProviderDurationMillis, TraceID: in.TraceID, SpanID: in.SpanID,
		Outcome: k8s.CanaryOutcome(in.Outcome), ClosedReason: k8s.CanaryFailureReason(in.ClosedReason.String()),
	}
}

func reasoningCanaryResultFromK8s(in *k8s.ReasoningCanaryResult) *api.ReasoningCanaryResult {
	if in == nil {
		return nil
	}
	if err := in.Validate(); err != nil {
		return nil
	}
	var closedReason api.CanaryFailureReason
	if err := closedReason.UnmarshalText([]byte(in.ClosedReason)); err != nil {
		return nil
	}
	switch in.Outcome {
	case k8s.CanaryOutcomePassed, k8s.CanaryOutcomeMismatch, k8s.CanaryOutcomeExecutionFailed:
	default:
		return nil
	}
	return &api.ReasoningCanaryResult{
		OperationID: in.OperationID, ExpectedBytes: in.ExpectedBytes, ActualBytes: in.ActualBytes,
		ExpectedSHA256: in.ExpectedSHA256, ActualSHA256: in.ActualSHA256,
		PromptTokens: in.PromptTokens, CompletionTokens: in.CompletionTokens, TotalTokens: in.TotalTokens,
		ProviderDurationMillis: in.ProviderDurationMillis, TraceID: in.TraceID, SpanID: in.SpanID,
		Outcome: api.CanaryOutcome(in.Outcome), ClosedReason: closedReason,
	}
}

func agentTypeMeta() metav1.TypeMeta {
	return metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"}
}

func agentInstanceTypeMeta() metav1.TypeMeta {
	return metav1.TypeMeta{Kind: "AgentInstance", APIVersion: "kognition.dev/v1alpha1"}
}
