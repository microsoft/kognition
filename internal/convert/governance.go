// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// --- AgentPolicy ---

func AgentPolicyToK8s(in *api.AgentPolicy) *k8s.AgentPolicy {
	out := &k8s.AgentPolicy{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AgentPolicy",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.AgentPolicySpec{
			Rules:        in.Spec.Rules,
			Enforce:      in.Spec.Enforce,
			Scope:        k8s.PolicyScope(in.Spec.Scope),
			TargetAgents: in.Spec.TargetAgents,
		},
	}
	if len(in.Spec.ToolGrants) > 0 {
		out.Spec.ToolGrants = make([]k8s.PolicyToolGrant, len(in.Spec.ToolGrants))
		for i, g := range in.Spec.ToolGrants {
			out.Spec.ToolGrants[i] = k8s.PolicyToolGrant{
				ToolRef:    g.ToolRef,
				Allow:      g.Allow,
				Conditions: g.Conditions,
			}
		}
	}
	if len(in.Spec.SafetyRules) > 0 {
		out.Spec.SafetyRules = make([]k8s.GovernanceRule, len(in.Spec.SafetyRules))
		for i, r := range in.Spec.SafetyRules {
			out.Spec.SafetyRules[i] = k8s.GovernanceRule{
				Name:     r.Name,
				Rego:     r.Rego,
				Severity: k8s.RuleSeverity(r.Severity),
			}
		}
	}
	if len(in.Spec.ComplianceRules) > 0 {
		out.Spec.ComplianceRules = make([]k8s.GovernanceRule, len(in.Spec.ComplianceRules))
		for i, r := range in.Spec.ComplianceRules {
			out.Spec.ComplianceRules[i] = k8s.GovernanceRule{
				Name:     r.Name,
				Rego:     r.Rego,
				Severity: k8s.RuleSeverity(r.Severity),
			}
		}
	}
	out.Status = k8s.AgentPolicyStatus{
		Phase:              string(in.Status.Phase),
		Message:            in.Status.Message,
		ObservedGeneration: in.Status.ObservedGeneration,
		Conditions:         conditionsToK8s(in.Status.Conditions),
	}
	return out
}

func AgentPolicyFromK8s(in *k8s.AgentPolicy) *api.AgentPolicy {
	out := &api.AgentPolicy{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.AgentPolicySpec{
			Rules:        in.Spec.Rules,
			Enforce:      in.Spec.Enforce,
			Scope:        string(in.Spec.Scope),
			TargetAgents: in.Spec.TargetAgents,
		},
	}
	if len(in.Spec.ToolGrants) > 0 {
		out.Spec.ToolGrants = make([]api.PolicyToolGrant, len(in.Spec.ToolGrants))
		for i, g := range in.Spec.ToolGrants {
			out.Spec.ToolGrants[i] = api.PolicyToolGrant{
				ToolRef:    g.ToolRef,
				Allow:      g.Allow,
				Conditions: g.Conditions,
			}
		}
	}
	if len(in.Spec.SafetyRules) > 0 {
		out.Spec.SafetyRules = make([]api.GovernanceRule, len(in.Spec.SafetyRules))
		for i, r := range in.Spec.SafetyRules {
			out.Spec.SafetyRules[i] = api.GovernanceRule{
				Name:     r.Name,
				Rego:     r.Rego,
				Severity: string(r.Severity),
			}
		}
	}
	if len(in.Spec.ComplianceRules) > 0 {
		out.Spec.ComplianceRules = make([]api.GovernanceRule, len(in.Spec.ComplianceRules))
		for i, r := range in.Spec.ComplianceRules {
			out.Spec.ComplianceRules[i] = api.GovernanceRule{
				Name:     r.Name,
				Rego:     r.Rego,
				Severity: string(r.Severity),
			}
		}
	}
	out.Status = api.AgentPolicyStatus{
		Phase:              api.Phase(in.Status.Phase),
		Message:            in.Status.Message,
		ObservedGeneration: in.Status.ObservedGeneration,
		Conditions:         conditionsFromK8s(in.Status.Conditions),
	}
	return out
}

// --- CognitiveAdmissionPolicy ---

func CognitiveAdmissionPolicyToK8s(in *api.CognitiveAdmissionPolicy) *k8s.CognitiveAdmissionPolicy {
	return &k8s.CognitiveAdmissionPolicy{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CognitiveAdmissionPolicy",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.CognitiveAdmissionPolicySpec{
			Rego:            in.Spec.Rego,
			MatchKinds:      in.Spec.MatchKinds,
			MatchOperations: in.Spec.MatchOperations,
			MutationRules:   in.Spec.MutationRules,
			FailurePolicy:   k8s.FailurePolicy(in.Spec.FailurePolicy),
		},
	}
}

func CognitiveAdmissionPolicyFromK8s(in *k8s.CognitiveAdmissionPolicy) *api.CognitiveAdmissionPolicy {
	return &api.CognitiveAdmissionPolicy{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.CognitiveAdmissionPolicySpec{
			Rego:            in.Spec.Rego,
			MatchKinds:      in.Spec.MatchKinds,
			MatchOperations: in.Spec.MatchOperations,
			MutationRules:   in.Spec.MutationRules,
			FailurePolicy:   string(in.Spec.FailurePolicy),
		},
	}
}

// --- CognitiveQuota ---

func CognitiveQuotaToK8s(in *api.CognitiveQuota) *k8s.CognitiveQuota {
	out := &k8s.CognitiveQuota{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CognitiveQuota",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.CognitiveQuotaSpec{
			Hard:          in.Spec.Hard,
			ResetInterval: in.Spec.ResetInterval,
		},
		Status: k8s.CognitiveQuotaStatus{
			Phase:         string(in.Status.Phase),
			Message:       in.Status.Message,
			Used:          in.Status.Used,
			LastResetTime: in.Status.LastResetTime,
			Conditions:    conditionsToK8s(in.Status.Conditions),
		},
	}
	if in.Spec.TokenBudget != nil {
		out.Spec.TokenBudget = &k8s.TokenBudget{
			MaxInputTokens:  in.Spec.TokenBudget.MaxInputTokens,
			MaxOutputTokens: in.Spec.TokenBudget.MaxOutputTokens,
			MaxTotalTokens:  in.Spec.TokenBudget.MaxTotalTokens,
		}
	}
	if in.Spec.CostBudget != nil {
		out.Spec.CostBudget = &k8s.CostBudget{
			MaxCostPerPeriod:      in.Spec.CostBudget.MaxCostPerPeriod,
			MaxToolCallsPerPeriod: in.Spec.CostBudget.MaxToolCallsPerPeriod,
			MaxModelTierSeconds:   cloneInt64Map(in.Spec.CostBudget.MaxModelTierSeconds),
		}
	}
	if in.Status.TokenUsage != nil {
		out.Status.TokenUsage = &k8s.QuotaTokenUsage{
			InputTokens:  in.Status.TokenUsage.InputTokens,
			OutputTokens: in.Status.TokenUsage.OutputTokens,
			TotalTokens:  in.Status.TokenUsage.TotalTokens,
		}
	}
	if in.Status.CostUsage != nil {
		out.Status.CostUsage = &k8s.QuotaCostUsage{
			TotalCost:             in.Status.CostUsage.TotalCost,
			ToolCalls:             in.Status.CostUsage.ToolCalls,
			ModelTierSeconds:      cloneInt64Map(in.Status.CostUsage.ModelTierSeconds),
			ModelTierMilliseconds: cloneInt64Map(in.Status.CostUsage.ModelTierMilliseconds),
		}
	}
	return out
}

func CognitiveQuotaFromK8s(in *k8s.CognitiveQuota) *api.CognitiveQuota {
	out := &api.CognitiveQuota{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.CognitiveQuotaSpec{
			Hard:          in.Spec.Hard,
			ResetInterval: in.Spec.ResetInterval,
		},
		Status: api.CognitiveQuotaStatus{
			Phase:         api.Phase(in.Status.Phase),
			Message:       in.Status.Message,
			Used:          in.Status.Used,
			LastResetTime: in.Status.LastResetTime,
			Conditions:    conditionsFromK8s(in.Status.Conditions),
		},
	}
	if in.Spec.TokenBudget != nil {
		out.Spec.TokenBudget = &api.TokenBudget{
			MaxInputTokens:  in.Spec.TokenBudget.MaxInputTokens,
			MaxOutputTokens: in.Spec.TokenBudget.MaxOutputTokens,
			MaxTotalTokens:  in.Spec.TokenBudget.MaxTotalTokens,
		}
	}
	if in.Spec.CostBudget != nil {
		out.Spec.CostBudget = &api.CostBudget{
			MaxCostPerPeriod:      in.Spec.CostBudget.MaxCostPerPeriod,
			MaxToolCallsPerPeriod: in.Spec.CostBudget.MaxToolCallsPerPeriod,
			MaxModelTierSeconds:   cloneInt64Map(in.Spec.CostBudget.MaxModelTierSeconds),
		}
	}
	if in.Status.TokenUsage != nil {
		out.Status.TokenUsage = &api.QuotaTokenUsage{
			InputTokens:  in.Status.TokenUsage.InputTokens,
			OutputTokens: in.Status.TokenUsage.OutputTokens,
			TotalTokens:  in.Status.TokenUsage.TotalTokens,
		}
	}
	if in.Status.CostUsage != nil {
		out.Status.CostUsage = &api.QuotaCostUsage{
			TotalCost:             in.Status.CostUsage.TotalCost,
			ToolCalls:             in.Status.CostUsage.ToolCalls,
			ModelTierSeconds:      cloneInt64Map(in.Status.CostUsage.ModelTierSeconds),
			ModelTierMilliseconds: cloneInt64Map(in.Status.CostUsage.ModelTierMilliseconds),
		}
	}
	return out
}

// --- CognitiveLimitRange ---

func CognitiveLimitRangeToK8s(in *api.CognitiveLimitRange) *k8s.CognitiveLimitRange {
	out := &k8s.CognitiveLimitRange{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CognitiveLimitRange",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
	}
	if len(in.Spec.Limits) > 0 {
		out.Spec.Limits = make([]k8s.LimitRangeItem, len(in.Spec.Limits))
		for i, l := range in.Spec.Limits {
			out.Spec.Limits[i] = k8s.LimitRangeItem{
				Type:                  l.Type,
				Default:               l.Default,
				Max:                   l.Max,
				Min:                   l.Min,
				TokensPerSession:      l.TokensPerSession,
				TimeoutSeconds:        l.TimeoutSeconds,
				MaxConcurrentSessions: l.MaxConcurrentSessions,
			}
		}
	}
	return out
}

func CognitiveLimitRangeFromK8s(in *k8s.CognitiveLimitRange) *api.CognitiveLimitRange {
	out := &api.CognitiveLimitRange{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
	}
	if len(in.Spec.Limits) > 0 {
		out.Spec.Limits = make([]api.LimitRangeItem, len(in.Spec.Limits))
		for i, l := range in.Spec.Limits {
			out.Spec.Limits[i] = api.LimitRangeItem{
				Type:                  l.Type,
				Default:               l.Default,
				Max:                   l.Max,
				Min:                   l.Min,
				TokensPerSession:      l.TokensPerSession,
				TimeoutSeconds:        l.TimeoutSeconds,
				MaxConcurrentSessions: l.MaxConcurrentSessions,
			}
		}
	}
	return out
}
