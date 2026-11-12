// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PolicyScope determines the scope of an AgentPolicy.
// +kubebuilder:validation:Enum=workspace;agent
type PolicyScope string

const (
	PolicyScopeWorkspace PolicyScope = "workspace"
	PolicyScopeAgent     PolicyScope = "agent"
)

// RuleSeverity indicates the severity of a governance rule violation.
// +kubebuilder:validation:Enum=error;warning
type RuleSeverity string

const (
	RuleSeverityError   RuleSeverity = "error"
	RuleSeverityWarning RuleSeverity = "warning"
)

// FailurePolicy determines behavior when policy evaluation fails.
// +kubebuilder:validation:Enum=Deny;Ignore
type FailurePolicy string

const (
	FailurePolicyDeny   FailurePolicy = "Deny"
	FailurePolicyIgnore FailurePolicy = "Ignore"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scope`
// +kubebuilder:printcolumn:name="Enforce",type=boolean,JSONPath=`.spec.enforce`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentPolicy defines governance rules for agents in a namespace.
type AgentPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentPolicySpec   `json:"spec,omitempty"`
	Status            AgentPolicyStatus `json:"status,omitempty"`
}

// AgentPolicyStatus reports the observed state of an AgentPolicy.
type AgentPolicyStatus struct {
	// Phase is the lifecycle phase (Pending|Ready|Failed).
	Phase string `json:"phase,omitempty"`

	// Message is a human-readable message describing the current phase.
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the .metadata.generation observed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions report the current state of the AgentPolicy.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AgentPolicySpec defines the desired state of an AgentPolicy.
type AgentPolicySpec struct {
	// Rules contains embedded OPA/Rego policy rules.
	// +optional
	Rules string `json:"rules,omitempty"`

	// Enforce determines if the policy blocks or warns.
	Enforce bool `json:"enforce"`

	// Scope determines policy scope.
	// +optional
	// +kubebuilder:default=workspace
	Scope PolicyScope `json:"scope,omitempty"`

	// TargetAgents lists agent names this policy applies to (only used when scope=agent).
	// +optional
	TargetAgents []string `json:"targetAgents,omitempty"`

	// ToolGrants defines which tools agents may use and under what conditions.
	// +optional
	ToolGrants []PolicyToolGrant `json:"toolGrants,omitempty"`

	// SafetyRules defines safety constraints evaluated at runtime.
	// +optional
	SafetyRules []GovernanceRule `json:"safetyRules,omitempty"`

	// ComplianceRules defines compliance constraints evaluated at admission and runtime.
	// +optional
	ComplianceRules []GovernanceRule `json:"complianceRules,omitempty"`
}

// PolicyToolGrant defines tool access permissions within an AgentPolicy.
type PolicyToolGrant struct {
	// ToolRef is the name of the tool or tool class.
	ToolRef string `json:"toolRef"`

	// Allow determines whether this tool is allowed (true) or denied (false).
	Allow bool `json:"allow"`

	// Conditions is optional Rego for conditional access.
	// +optional
	Conditions string `json:"conditions,omitempty"`
}

// GovernanceRule defines a named rule within a governance policy.
type GovernanceRule struct {
	// Name identifies the rule.
	Name string `json:"name"`

	// Rego contains the rule logic.
	Rego string `json:"rego"`

	// Severity indicates violation severity.
	// +optional
	// +kubebuilder:default=error
	Severity RuleSeverity `json:"severity,omitempty"`
}

// +kubebuilder:object:root=true

// AgentPolicyList contains a list of AgentPolicies.
type AgentPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentPolicy `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="FailurePolicy",type=string,JSONPath=`.spec.failurePolicy`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CognitiveAdmissionPolicy defines admission control rules evaluated by OPA.
type CognitiveAdmissionPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CognitiveAdmissionPolicySpec `json:"spec,omitempty"`
}

// CognitiveAdmissionPolicySpec defines the desired state of a CognitiveAdmissionPolicy.
type CognitiveAdmissionPolicySpec struct {
	// Rego contains the OPA/Rego policy source for validation.
	Rego string `json:"rego"`

	// MatchKinds lists the resource kinds this policy applies to.
	// +optional
	MatchKinds []string `json:"matchKinds,omitempty"`

	// MatchOperations limits which operations trigger this policy (CREATE, UPDATE).
	// +optional
	MatchOperations []string `json:"matchOperations,omitempty"`

	// MutationRules contains optional Rego for mutating admission (defaults injection).
	// +optional
	MutationRules string `json:"mutationRules,omitempty"`

	// FailurePolicy determines behavior on evaluation error.
	// +optional
	// +kubebuilder:default=Deny
	FailurePolicy FailurePolicy `json:"failurePolicy,omitempty"`
}

// +kubebuilder:object:root=true

// CognitiveAdmissionPolicyList contains a list of CognitiveAdmissionPolicies.
type CognitiveAdmissionPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CognitiveAdmissionPolicy `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CognitiveQuota defines resource quotas for a namespace.
type CognitiveQuota struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CognitiveQuotaSpec   `json:"spec,omitempty"`
	Status            CognitiveQuotaStatus `json:"status,omitempty"`
}

// CognitiveQuotaSpec defines quota limits.
type CognitiveQuotaSpec struct {
	// Hard limits keyed by resource name (agents, tools, sessions, etc.).
	Hard map[string]int64 `json:"hard"`

	// TokenBudget defines token-based quotas for the namespace.
	// +optional
	TokenBudget *TokenBudget `json:"tokenBudget,omitempty"`

	// CostBudget defines cost-based quotas for the namespace.
	// +optional
	CostBudget *CostBudget `json:"costBudget,omitempty"`

	// ResetInterval defines the period for resetting usage counters (e.g., "1h", "24h", "720h").
	// +optional
	ResetInterval string `json:"resetInterval,omitempty"`
}

// TokenBudget defines token usage limits.
type TokenBudget struct {
	// MaxInputTokens is the maximum input tokens per period.
	// +optional
	MaxInputTokens int64 `json:"maxInputTokens,omitempty"`

	// MaxOutputTokens is the maximum output tokens per period.
	// +optional
	MaxOutputTokens int64 `json:"maxOutputTokens,omitempty"`

	// MaxTotalTokens is the combined max (if set overrides in+out individually).
	// +optional
	MaxTotalTokens int64 `json:"maxTotalTokens,omitempty"`
}

// CostBudget defines cost-based usage limits.
type CostBudget struct {
	// MaxCostPerPeriod is the maximum normalized micro-USD cost per period.
	MaxCostPerPeriod int64 `json:"maxCostPerPeriod"`

	// MaxToolCallsPerPeriod limits tool invocations per period.
	// +optional
	MaxToolCallsPerPeriod int64 `json:"maxToolCallsPerPeriod,omitempty"`

	// MaxModelTierSeconds limits provider time per model cost tier in each period.
	// +optional
	MaxModelTierSeconds map[string]int64 `json:"maxModelTierSeconds,omitempty"`
}

// CognitiveQuotaStatus reports current usage against limits.
type CognitiveQuotaStatus struct {
	// Phase is the lifecycle phase (Pending|Ready|Degraded|Failed).
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message is a human-readable message describing the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// Used tracks resource usage counts.
	// +optional
	Used map[string]int64 `json:"used,omitempty"`

	// TokenUsage tracks token consumption in the current period.
	// +optional
	TokenUsage *QuotaTokenUsage `json:"tokenUsage,omitempty"`

	// CostUsage tracks cost consumption in the current period.
	// +optional
	CostUsage *QuotaCostUsage `json:"costUsage,omitempty"`

	// LastResetTime is the timestamp of the last usage counter reset.
	// +optional
	LastResetTime string `json:"lastResetTime,omitempty"`

	// Conditions report the current state of the CognitiveQuota.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// QuotaTokenUsage tracks token consumption against quota limits.
type QuotaTokenUsage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	TotalTokens  int64 `json:"totalTokens"`
}

// QuotaCostUsage tracks cost consumption against quota limits.
type QuotaCostUsage struct {
	TotalCost             int64            `json:"totalCost"`
	ToolCalls             int64            `json:"toolCalls"`
	ModelTierSeconds      map[string]int64 `json:"modelTierSeconds,omitempty"`
	ModelTierMilliseconds map[string]int64 `json:"modelTierMilliseconds,omitempty"`
}

// +kubebuilder:object:root=true

// CognitiveQuotaList contains a list of CognitiveQuotas.
type CognitiveQuotaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CognitiveQuota `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CognitiveLimitRange defines default and max limits for resources in a namespace.
type CognitiveLimitRange struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CognitiveLimitRangeSpec `json:"spec,omitempty"`
}

// CognitiveLimitRangeSpec defines limit range configuration.
type CognitiveLimitRangeSpec struct {
	// Limits defines the limit range items.
	Limits []LimitRangeItem `json:"limits"`
}

// LimitRangeItem defines a single limit range entry.
type LimitRangeItem struct {
	// Type identifies what this limit applies to.
	Type string `json:"type"`

	// Default defines default resource values.
	// +optional
	Default map[string]int64 `json:"default,omitempty"`

	// Max defines maximum resource values.
	// +optional
	Max map[string]int64 `json:"max,omitempty"`

	// Min defines minimum resource values.
	// +optional
	Min map[string]int64 `json:"min,omitempty"`

	// TokensPerSession is the max tokens an agent can use per session.
	// +optional
	TokensPerSession int64 `json:"tokensPerSession,omitempty"`

	// TimeoutSeconds is the max duration in seconds for a single agent session.
	// +optional
	TimeoutSeconds int64 `json:"timeoutSeconds,omitempty"`

	// MaxConcurrentSessions limits simultaneous sessions per agent.
	// +optional
	MaxConcurrentSessions int64 `json:"maxConcurrentSessions,omitempty"`
}

// +kubebuilder:object:root=true

// CognitiveLimitRangeList contains a list of CognitiveLimitRanges.
type CognitiveLimitRangeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CognitiveLimitRange `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentPolicy{}, &AgentPolicyList{})
	SchemeBuilder.Register(&CognitiveAdmissionPolicy{}, &CognitiveAdmissionPolicyList{})
	SchemeBuilder.Register(&CognitiveQuota{}, &CognitiveQuotaList{})
	SchemeBuilder.Register(&CognitiveLimitRange{}, &CognitiveLimitRangeList{})
}
