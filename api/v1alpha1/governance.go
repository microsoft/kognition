// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

// AgentPolicy defines governance rules for agents in a namespace.
type AgentPolicy struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       AgentPolicySpec   `json:"spec" yaml:"spec"`
	Status     AgentPolicyStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (a *AgentPolicy) GetTypeMeta() TypeMeta      { return a.TypeMeta }
func (a *AgentPolicy) GetObjectMeta() *ObjectMeta { return &a.ObjectMeta }

// AgentPolicyStatus reports the compilation state of an AgentPolicy.
type AgentPolicyStatus struct {
	Phase              Phase       `json:"phase,omitempty" yaml:"phase,omitempty"`
	Message            string      `json:"message,omitempty" yaml:"message,omitempty"`
	ObservedGeneration int64       `json:"observedGeneration,omitempty" yaml:"observedGeneration,omitempty"`
	Conditions         []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// AgentPolicySpec defines policy rules.
type AgentPolicySpec struct {
	// Rules contains embedded OPA/Rego policy rules.
	Rules string `json:"rules,omitempty" yaml:"rules,omitempty"`

	// Enforce determines if the policy blocks or warns.
	Enforce bool `json:"enforce" yaml:"enforce"`

	// Scope determines policy scope: "workspace" applies to all agents, "agent" targets specific agents.
	Scope string `json:"scope,omitempty" yaml:"scope,omitempty"`

	// TargetAgents lists agent names this policy applies to (only used when scope=agent).
	TargetAgents []string `json:"targetAgents,omitempty" yaml:"targetAgents,omitempty"`

	// ToolGrants defines which tools agents may use and under what conditions.
	ToolGrants []PolicyToolGrant `json:"toolGrants,omitempty" yaml:"toolGrants,omitempty"`

	// SafetyRules defines safety constraints evaluated at runtime.
	SafetyRules []GovernanceRule `json:"safetyRules,omitempty" yaml:"safetyRules,omitempty"`

	// ComplianceRules defines compliance constraints evaluated at admission and runtime.
	ComplianceRules []GovernanceRule `json:"complianceRules,omitempty" yaml:"complianceRules,omitempty"`
}

// PolicyToolGrant defines tool access permissions within an AgentPolicy.
type PolicyToolGrant struct {
	// ToolRef is the name of the tool or tool class.
	ToolRef string `json:"toolRef" yaml:"toolRef"`

	// Allow determines whether this tool is allowed (true) or denied (false).
	Allow bool `json:"allow" yaml:"allow"`

	// Conditions is optional Rego for conditional access.
	Conditions string `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// GovernanceRule defines a named rule within a governance policy.
type GovernanceRule struct {
	// Name identifies the rule.
	Name string `json:"name" yaml:"name"`

	// Rego contains the rule logic.
	Rego string `json:"rego" yaml:"rego"`

	// Severity indicates violation severity: "error" (deny) or "warning" (log only).
	Severity string `json:"severity,omitempty" yaml:"severity,omitempty"`
}

// CognitiveAdmissionPolicy defines admission control rules evaluated by OPA.
type CognitiveAdmissionPolicy struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       CognitiveAdmissionPolicySpec `json:"spec" yaml:"spec"`
}

func (c *CognitiveAdmissionPolicy) GetTypeMeta() TypeMeta      { return c.TypeMeta }
func (c *CognitiveAdmissionPolicy) GetObjectMeta() *ObjectMeta { return &c.ObjectMeta }

// CognitiveAdmissionPolicySpec defines admission policy rules.
type CognitiveAdmissionPolicySpec struct {
	// Rego contains the OPA/Rego policy source for validation.
	Rego string `json:"rego" yaml:"rego"`

	// MatchKinds lists the resource kinds this policy applies to.
	MatchKinds []string `json:"matchKinds,omitempty" yaml:"matchKinds,omitempty"`

	// MatchOperations limits which operations trigger this policy (CREATE, UPDATE).
	MatchOperations []string `json:"matchOperations,omitempty" yaml:"matchOperations,omitempty"`

	// MutationRules contains optional Rego for mutating admission (defaults injection).
	MutationRules string `json:"mutationRules,omitempty" yaml:"mutationRules,omitempty"`

	// FailurePolicy determines behavior on evaluation error: "Deny" or "Ignore".
	FailurePolicy string `json:"failurePolicy,omitempty" yaml:"failurePolicy,omitempty"`
}

// CognitiveQuota defines resource quotas for a namespace.
type CognitiveQuota struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       CognitiveQuotaSpec   `json:"spec" yaml:"spec"`
	Status     CognitiveQuotaStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (c *CognitiveQuota) GetTypeMeta() TypeMeta      { return c.TypeMeta }
func (c *CognitiveQuota) GetObjectMeta() *ObjectMeta { return &c.ObjectMeta }

// CognitiveQuotaSpec defines quota limits.
type CognitiveQuotaSpec struct {
	// Hard limits keyed by resource name (agents, tools, sessions, etc.).
	Hard map[string]int64 `json:"hard" yaml:"hard"`

	// TokenBudget defines token-based quotas for the namespace.
	TokenBudget *TokenBudget `json:"tokenBudget,omitempty" yaml:"tokenBudget,omitempty"`

	// CostBudget defines cost-based quotas for the namespace.
	CostBudget *CostBudget `json:"costBudget,omitempty" yaml:"costBudget,omitempty"`

	// ResetInterval defines the period for resetting usage counters (e.g., "1h", "24h", "720h").
	ResetInterval string `json:"resetInterval,omitempty" yaml:"resetInterval,omitempty"`
}

// TokenBudget defines token usage limits.
type TokenBudget struct {
	// MaxInputTokens is the maximum input tokens per period.
	MaxInputTokens int64 `json:"maxInputTokens,omitempty" yaml:"maxInputTokens,omitempty"`

	// MaxOutputTokens is the maximum output tokens per period.
	MaxOutputTokens int64 `json:"maxOutputTokens,omitempty" yaml:"maxOutputTokens,omitempty"`

	// MaxTotalTokens is the combined max (if set overrides in+out individually).
	MaxTotalTokens int64 `json:"maxTotalTokens,omitempty" yaml:"maxTotalTokens,omitempty"`
}

// CostBudget defines cost-based usage limits.
type CostBudget struct {
	// MaxCostPerPeriod is the maximum normalized micro-USD cost per period.
	MaxCostPerPeriod int64 `json:"maxCostPerPeriod" yaml:"maxCostPerPeriod"`

	// MaxToolCallsPerPeriod limits tool invocations per period.
	MaxToolCallsPerPeriod int64 `json:"maxToolCallsPerPeriod,omitempty" yaml:"maxToolCallsPerPeriod,omitempty"`

	// MaxModelTierSeconds limits provider time per model cost tier in each period.
	MaxModelTierSeconds map[string]int64 `json:"maxModelTierSeconds,omitempty" yaml:"maxModelTierSeconds,omitempty"`
}

// CognitiveQuotaStatus reports current usage against limits.
type CognitiveQuotaStatus struct {
	Phase   Phase  `json:"phase,omitempty" yaml:"phase,omitempty"`
	Message string `json:"message,omitempty" yaml:"message,omitempty"`

	Used map[string]int64 `json:"used,omitempty" yaml:"used,omitempty"`

	// TokenUsage tracks token consumption in the current period.
	TokenUsage *QuotaTokenUsage `json:"tokenUsage,omitempty" yaml:"tokenUsage,omitempty"`

	// CostUsage tracks cost consumption in the current period.
	CostUsage *QuotaCostUsage `json:"costUsage,omitempty" yaml:"costUsage,omitempty"`

	// LastResetTime is the timestamp of the last usage counter reset.
	LastResetTime string `json:"lastResetTime,omitempty" yaml:"lastResetTime,omitempty"`

	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// QuotaTokenUsage tracks token consumption against quota limits.
type QuotaTokenUsage struct {
	InputTokens  int64 `json:"inputTokens" yaml:"inputTokens"`
	OutputTokens int64 `json:"outputTokens" yaml:"outputTokens"`
	TotalTokens  int64 `json:"totalTokens" yaml:"totalTokens"`
}

// QuotaCostUsage tracks cost consumption against quota limits.
type QuotaCostUsage struct {
	TotalCost             int64            `json:"totalCost" yaml:"totalCost"`
	ToolCalls             int64            `json:"toolCalls" yaml:"toolCalls"`
	ModelTierSeconds      map[string]int64 `json:"modelTierSeconds,omitempty" yaml:"modelTierSeconds,omitempty"`
	ModelTierMilliseconds map[string]int64 `json:"modelTierMilliseconds,omitempty" yaml:"modelTierMilliseconds,omitempty"`
}

// CognitiveLimitRange defines default and max limits for resources in a namespace.
type CognitiveLimitRange struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       CognitiveLimitRangeSpec `json:"spec" yaml:"spec"`
}

func (c *CognitiveLimitRange) GetTypeMeta() TypeMeta      { return c.TypeMeta }
func (c *CognitiveLimitRange) GetObjectMeta() *ObjectMeta { return &c.ObjectMeta }

// CognitiveLimitRangeSpec defines limit ranges.
type CognitiveLimitRangeSpec struct {
	Limits []LimitRangeItem `json:"limits" yaml:"limits"`
}

// LimitRangeItem defines a single limit range entry.
type LimitRangeItem struct {
	Type    string           `json:"type" yaml:"type"`
	Default map[string]int64 `json:"default,omitempty" yaml:"default,omitempty"`
	Max     map[string]int64 `json:"max,omitempty" yaml:"max,omitempty"`
	Min     map[string]int64 `json:"min,omitempty" yaml:"min,omitempty"`

	// TokensPerSession is the max tokens an agent can use per session.
	TokensPerSession int64 `json:"tokensPerSession,omitempty" yaml:"tokensPerSession,omitempty"`

	// TimeoutSeconds is the max duration in seconds for a single agent session.
	TimeoutSeconds int64 `json:"timeoutSeconds,omitempty" yaml:"timeoutSeconds,omitempty"`

	// MaxConcurrentSessions limits simultaneous sessions per agent.
	MaxConcurrentSessions int64 `json:"maxConcurrentSessions,omitempty" yaml:"maxConcurrentSessions,omitempty"`
}
