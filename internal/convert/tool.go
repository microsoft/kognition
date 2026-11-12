// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// ToolToK8s converts an internal Tool to a K8s Tool.
func ToolToK8s(in *api.Tool) *k8s.Tool {
	out := &k8s.Tool{
		Spec:   toolSpecToK8s(in.Spec),
		Status: toolStatusToK8s(in.Status),
	}
	out.TypeMeta = metav1.TypeMeta{Kind: "Tool", APIVersion: "kognition.dev/v1alpha1"}
	out.ObjectMeta = objectMetaToK8s(in.ObjectMeta)
	return out
}

// ToolFromK8s converts a K8s Tool to an internal Tool.
func ToolFromK8s(in *k8s.Tool) *api.Tool {
	out := &api.Tool{
		Spec:   toolSpecFromK8s(in.Spec),
		Status: toolStatusFromK8s(in.Status),
	}
	out.TypeMeta = api.TypeMeta{APIVersion: api.APIVersion, Kind: "Tool"}
	out.ObjectMeta = objectMetaFromK8s(in.ObjectMeta)
	return out
}

// ToolClassToK8s converts an internal ToolClass to a K8s ToolClass.
func ToolClassToK8s(in *api.ToolClass) *k8s.ToolClass {
	out := &k8s.ToolClass{
		Spec:   toolClassSpecToK8s(in.Spec),
		Status: toolClassStatusToK8s(in.Status),
	}
	out.TypeMeta = metav1.TypeMeta{Kind: "ToolClass", APIVersion: "kognition.dev/v1alpha1"}
	out.ObjectMeta = objectMetaToK8s(in.ObjectMeta)
	return out
}

// ToolClassFromK8s converts a K8s ToolClass to an internal ToolClass.
func ToolClassFromK8s(in *k8s.ToolClass) *api.ToolClass {
	out := &api.ToolClass{
		Spec:   toolClassSpecFromK8s(in.Spec),
		Status: toolClassStatusFromK8s(in.Status),
	}
	out.TypeMeta = api.TypeMeta{APIVersion: api.APIVersion, Kind: "ToolClass"}
	out.ObjectMeta = objectMetaFromK8s(in.ObjectMeta)
	return out
}

// ToolGrantToK8s converts an internal ToolGrant to a K8s ToolGrant.
func ToolGrantToK8s(in *api.ToolGrant) *k8s.ToolGrant {
	if in == nil {
		return nil
	}
	out := &k8s.ToolGrant{
		Spec: k8s.ToolGrantSpec{
			AgentSelector:       cloneStringMap(in.Spec.AgentSelector),
			AgentRef:            localRefToK8s(in.Spec.AgentRef.Name),
			ToolRef:             localRefToK8s(in.Spec.ToolRef.Name),
			ArgumentConstraints: cloneJSONMap(in.Spec.ArgumentConstraints),
		},
		Status: k8s.ToolGrantStatus{
			Active:            in.Status.Active,
			LastEvaluatedTime: in.Status.LastEvaluatedTime,
			GrantedAgents:     in.Status.GrantedAgents,
		},
	}
	out.TypeMeta = metav1.TypeMeta{Kind: "ToolGrant", APIVersion: api.APIVersion}
	out.ObjectMeta = objectMetaToK8s(in.ObjectMeta)
	if in.Spec.Verbs != nil {
		out.Spec.Verbs = make([]k8s.ToolGrantVerb, len(in.Spec.Verbs))
		for i, verb := range in.Spec.Verbs {
			out.Spec.Verbs[i] = k8s.ToolGrantVerb(verb)
		}
	}
	if in.Spec.RateLimits != nil {
		out.Spec.RateLimits = &k8s.ToolGrantRateLimits{
			CallsPerMinute: in.Spec.RateLimits.CallsPerMinute,
			CallsPerHour:   in.Spec.RateLimits.CallsPerHour,
			CallsPerDay:    in.Spec.RateLimits.CallsPerDay,
		}
	}
	if in.Spec.Conditions != nil {
		out.Spec.Conditions = &k8s.ToolGrantConditions{
			ValidFrom:           in.Spec.Conditions.ValidFrom,
			ValidUntil:          in.Spec.Conditions.ValidUntil,
			RequiredAgentLabels: cloneStringMap(in.Spec.Conditions.RequiredAgentLabels),
		}
	}
	return out
}

// ToolGrantFromK8s converts a K8s ToolGrant to an internal ToolGrant.
func ToolGrantFromK8s(in *k8s.ToolGrant) *api.ToolGrant {
	if in == nil {
		return nil
	}
	out := &api.ToolGrant{
		TypeMeta:   api.TypeMeta{Kind: "ToolGrant", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.ToolGrantSpec{
			AgentSelector:       cloneStringMap(in.Spec.AgentSelector),
			AgentRef:            api.LocalObjectReference{Name: localRefFromK8s(in.Spec.AgentRef)},
			ToolRef:             api.LocalObjectReference{Name: localRefFromK8s(in.Spec.ToolRef)},
			ArgumentConstraints: cloneJSONMap(in.Spec.ArgumentConstraints),
		},
		Status: api.ToolGrantStatus{
			Active:            in.Status.Active,
			LastEvaluatedTime: in.Status.LastEvaluatedTime,
			GrantedAgents:     in.Status.GrantedAgents,
		},
	}
	if in.Spec.Verbs != nil {
		out.Spec.Verbs = make([]api.ToolGrantVerb, len(in.Spec.Verbs))
		for i, verb := range in.Spec.Verbs {
			out.Spec.Verbs[i] = api.ToolGrantVerb(verb)
		}
	}
	if in.Spec.RateLimits != nil {
		out.Spec.RateLimits = &api.ToolGrantRateLimits{
			CallsPerMinute: in.Spec.RateLimits.CallsPerMinute,
			CallsPerHour:   in.Spec.RateLimits.CallsPerHour,
			CallsPerDay:    in.Spec.RateLimits.CallsPerDay,
		}
	}
	if in.Spec.Conditions != nil {
		out.Spec.Conditions = &api.ToolGrantConditions{
			ValidFrom:           in.Spec.Conditions.ValidFrom,
			ValidUntil:          in.Spec.Conditions.ValidUntil,
			RequiredAgentLabels: cloneStringMap(in.Spec.Conditions.RequiredAgentLabels),
		}
	}
	return out
}

// --- Tool Spec ---

// toolSpecToK8s converts internal ToolSpec to K8s ToolSpec.
func toolSpecToK8s(in api.ToolSpec) k8s.ToolSpec {
	out := k8s.ToolSpec{
		ClassName:   in.ClassName,
		Protocol:    k8s.ToolProtocol(in.Protocol),
		Description: in.Description,
		Endpoint:    in.Endpoint,
		Metadata:    cloneStringMap(in.Metadata),
		Schema:      cloneJSONMap(in.Schema),
		InputSchema: cloneJSONMap(in.InputSchema),
		DigestPin:   in.DigestPin,
		Signature:   in.Signature,
		Trusted:     in.Trusted,
	}
	if in.Sandbox != nil {
		out.Sandbox = &k8s.ToolSandboxSpec{
			Timeout:       in.Sandbox.Timeout,
			CPUMilli:      in.Sandbox.CPUMilli,
			MemoryMB:      in.Sandbox.MemoryMB,
			NetworkPolicy: in.Sandbox.NetworkPolicy,
		}
	}
	if in.HealthProbe != nil {
		out.HealthProbe = &k8s.ToolHealthProbe{
			Type:             k8s.ProbeType(in.HealthProbe.Type),
			Command:          append([]string(nil), in.HealthProbe.Command...),
			Interval:         in.HealthProbe.Interval,
			Timeout:          in.HealthProbe.Timeout,
			FailureThreshold: in.HealthProbe.FailureThreshold,
			SuccessThreshold: in.HealthProbe.SuccessThreshold,
		}
	}
	if in.ExecutionPolicy != nil {
		out.ExecutionPolicy = &k8s.ToolExecutionPolicy{Timeout: in.ExecutionPolicy.Timeout, MaxConcurrent: in.ExecutionPolicy.MaxConcurrent, Idempotent: in.ExecutionPolicy.Idempotent, RetryMaxAttempts: in.ExecutionPolicy.RetryMaxAttempts}
		if in.ExecutionPolicy.CircuitBreaker != nil {
			out.ExecutionPolicy.CircuitBreaker = &k8s.ToolCircuitBreaker{FailureThreshold: in.ExecutionPolicy.CircuitBreaker.FailureThreshold, Cooldown: in.ExecutionPolicy.CircuitBreaker.Cooldown}
		}
	}
	if len(in.SecretRefs) > 0 {
		out.SecretRefs = make([]k8s.ToolSecretRef, len(in.SecretRefs))
		for i, ref := range in.SecretRefs {
			out.SecretRefs[i] = k8s.ToolSecretRef{Name: ref.Name, Key: ref.Key}
		}
	}
	return out
}

func toolSpecFromK8s(in k8s.ToolSpec) api.ToolSpec {
	out := api.ToolSpec{
		ClassName:   in.ClassName,
		Protocol:    api.ToolProtocol(in.Protocol),
		Description: in.Description,
		Endpoint:    in.Endpoint,
		Metadata:    cloneStringMap(in.Metadata),
		Schema:      cloneJSONMap(in.Schema),
		InputSchema: cloneJSONMap(in.InputSchema),
		DigestPin:   in.DigestPin,
		Signature:   in.Signature,
		Trusted:     in.Trusted,
	}
	if in.Sandbox != nil {
		out.Sandbox = &api.ToolSandboxSpec{
			Timeout:       in.Sandbox.Timeout,
			CPUMilli:      in.Sandbox.CPUMilli,
			MemoryMB:      in.Sandbox.MemoryMB,
			NetworkPolicy: in.Sandbox.NetworkPolicy,
		}
	}
	if in.HealthProbe != nil {
		out.HealthProbe = &api.ToolHealthProbe{
			Type:             api.ProbeType(in.HealthProbe.Type),
			Command:          append([]string(nil), in.HealthProbe.Command...),
			Interval:         in.HealthProbe.Interval,
			Timeout:          in.HealthProbe.Timeout,
			FailureThreshold: in.HealthProbe.FailureThreshold,
			SuccessThreshold: in.HealthProbe.SuccessThreshold,
		}
	}
	if in.ExecutionPolicy != nil {
		out.ExecutionPolicy = &api.ToolExecutionPolicy{Timeout: in.ExecutionPolicy.Timeout, MaxConcurrent: in.ExecutionPolicy.MaxConcurrent, Idempotent: in.ExecutionPolicy.Idempotent, RetryMaxAttempts: in.ExecutionPolicy.RetryMaxAttempts}
		if in.ExecutionPolicy.CircuitBreaker != nil {
			out.ExecutionPolicy.CircuitBreaker = &api.ToolCircuitBreaker{FailureThreshold: in.ExecutionPolicy.CircuitBreaker.FailureThreshold, Cooldown: in.ExecutionPolicy.CircuitBreaker.Cooldown}
		}
	}
	if len(in.SecretRefs) > 0 {
		out.SecretRefs = make([]api.ToolSecretRef, len(in.SecretRefs))
		for i, ref := range in.SecretRefs {
			out.SecretRefs[i] = api.ToolSecretRef{Name: ref.Name, Key: ref.Key}
		}
	}
	return out
}

// --- Tool Status ---

func toolStatusToK8s(in api.ToolStatus) k8s.ToolStatus {
	out := k8s.ToolStatus{
		Phase:              phaseToString(in.Phase),
		Conditions:         conditionsToK8s(in.Conditions),
		Ready:              in.Available,
		LastHealthCheck:    in.LastHealthCheck,
		ResolvedProtocol:   k8s.ToolProtocol(in.ResolvedProtocol),
		ObservedDigest:     in.ObservedDigest,
		ObservedSpecDigest: in.ObservedSpecDigest,
	}
	if in.CompiledMCP != nil {
		out.CompiledMCP = &k8s.MCPToolDefinition{
			Name:        in.CompiledMCP.Name,
			Description: in.CompiledMCP.Description,
			InputSchema: cloneJSONMap(in.CompiledMCP.InputSchema),
			Transport:   k8s.Transport(in.CompiledMCP.Transport),
			Endpoint:    in.CompiledMCP.Endpoint,
		}
	}
	return out
}

func toolStatusFromK8s(in k8s.ToolStatus) api.ToolStatus {
	out := api.ToolStatus{
		Phase:              phaseFromString(in.Phase),
		Conditions:         conditionsFromK8s(in.Conditions),
		Available:          in.Ready,
		LastHealthCheck:    in.LastHealthCheck,
		ResolvedProtocol:   api.ToolProtocol(in.ResolvedProtocol),
		ObservedDigest:     in.ObservedDigest,
		ObservedSpecDigest: in.ObservedSpecDigest,
	}
	if in.CompiledMCP != nil {
		out.CompiledMCP = &api.MCPToolDefinition{
			Name:        in.CompiledMCP.Name,
			Description: in.CompiledMCP.Description,
			InputSchema: cloneJSONMap(in.CompiledMCP.InputSchema),
			Transport:   api.Transport(in.CompiledMCP.Transport),
			Endpoint:    in.CompiledMCP.Endpoint,
		}
	}
	return out
}

func cloneJSONMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	return runtime.DeepCopyJSON(in)
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// --- ToolClass Spec ---

func toolClassSpecToK8s(in api.ToolClassSpec) k8s.ToolClassSpec {
	out := k8s.ToolClassSpec{
		Provisioner:   in.Provisioner,
		Parameters:    cloneStringMap(in.Parameters),
		ExecutionTier: k8s.ExecutionTier(in.ExecutionTier),
		SandboxImage:  in.SandboxImage,
		StdioCommand:  append([]string(nil), in.StdioCommand...),
		Transport:     k8s.Transport(in.Transport),
	}
	if in.SandboxDefaults != nil {
		out.SandboxDefaults = &k8s.ToolSandboxSpec{
			Timeout:       in.SandboxDefaults.Timeout,
			CPUMilli:      in.SandboxDefaults.CPUMilli,
			MemoryMB:      in.SandboxDefaults.MemoryMB,
			NetworkPolicy: in.SandboxDefaults.NetworkPolicy,
		}
	}
	if in.ResourceLimits != nil {
		out.ResourceLimits = &k8s.ToolClassResourceLimits{
			MaxCPUMilli: in.ResourceLimits.MaxCPUMilli,
			MaxMemoryMB: in.ResourceLimits.MaxMemoryMB,
			MaxTimeout:  in.ResourceLimits.MaxTimeout,
		}
	}
	return out
}

func toolClassSpecFromK8s(in k8s.ToolClassSpec) api.ToolClassSpec {
	out := api.ToolClassSpec{
		Provisioner:   in.Provisioner,
		Parameters:    cloneStringMap(in.Parameters),
		ExecutionTier: api.ExecutionTier(in.ExecutionTier),
		SandboxImage:  in.SandboxImage,
		StdioCommand:  append([]string(nil), in.StdioCommand...),
		Transport:     api.Transport(in.Transport),
	}
	if in.SandboxDefaults != nil {
		out.SandboxDefaults = &api.ToolSandboxSpec{
			Timeout:       in.SandboxDefaults.Timeout,
			CPUMilli:      in.SandboxDefaults.CPUMilli,
			MemoryMB:      in.SandboxDefaults.MemoryMB,
			NetworkPolicy: in.SandboxDefaults.NetworkPolicy,
		}
	}
	if in.ResourceLimits != nil {
		out.ResourceLimits = &api.ToolClassResourceLimits{
			MaxCPUMilli: in.ResourceLimits.MaxCPUMilli,
			MaxMemoryMB: in.ResourceLimits.MaxMemoryMB,
			MaxTimeout:  in.ResourceLimits.MaxTimeout,
		}
	}
	return out
}

// --- ToolClass Status ---

func toolClassStatusToK8s(in api.ToolClassStatus) k8s.ToolClassStatus {
	return k8s.ToolClassStatus{
		Phase:       string(in.Phase),
		Message:     in.Message,
		Provisioned: in.Provisioned,
		Conditions:  conditionsToK8s(in.Conditions),
	}
}

func toolClassStatusFromK8s(in k8s.ToolClassStatus) api.ToolClassStatus {
	return api.ToolClassStatus{
		Phase:       api.Phase(in.Phase),
		Message:     in.Message,
		Provisioned: in.Provisioned,
		Conditions:  conditionsFromK8s(in.Conditions),
	}
}
