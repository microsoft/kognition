// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

// AutonomyClass defines a tier of agent autonomy.
type AutonomyClass struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       AutonomyClassSpec `json:"spec" yaml:"spec"`
}

func (a *AutonomyClass) GetTypeMeta() TypeMeta      { return a.TypeMeta }
func (a *AutonomyClass) GetObjectMeta() *ObjectMeta { return &a.ObjectMeta }

// AutonomyClassSpec defines the autonomy tier parameters.
type AutonomyClassSpec struct {
	// Level is the numeric autonomy tier (0=none, 10=supervised, 50=assisted, 90=autonomous).
	Level int `json:"level" yaml:"level"`

	// RequiresApproval indicates if human approval is needed before action execution.
	RequiresApproval bool `json:"requiresApproval" yaml:"requiresApproval"`

	// MaxCostPerAction is the max token/cost budget before escalation.
	MaxCostPerAction int64 `json:"maxCostPerAction,omitempty" yaml:"maxCostPerAction,omitempty"`

	// AllowedTools restricts which tool classes can be used at this level.
	AllowedTools []string `json:"allowedTools,omitempty" yaml:"allowedTools,omitempty"`

	// ApprovalPolicy defines when human approval is required for different action types.
	ApprovalPolicy *ApprovalPolicy `json:"approvalPolicy,omitempty" yaml:"approvalPolicy,omitempty"`

	// Constraints defines limits on agent actions at this autonomy level.
	Constraints *AutonomyConstraints `json:"constraints,omitempty" yaml:"constraints,omitempty"`
}

// ApprovalPolicy defines approval requirements for different action types.
// Values: "always", "never", "high-risk-only"
type ApprovalPolicy struct {
	ToolInvocation        string `json:"toolInvocation,omitempty" yaml:"toolInvocation,omitempty"`
	MemoryWrite           string `json:"memoryWrite,omitempty" yaml:"memoryWrite,omitempty"`
	ExternalCommunication string `json:"externalCommunication,omitempty" yaml:"externalCommunication,omitempty"`
	CostThreshold         int64  `json:"costThreshold,omitempty" yaml:"costThreshold,omitempty"`
}

// AutonomyConstraints defines action limits for an autonomy level.
type AutonomyConstraints struct {
	MaxConcurrentActions    int   `json:"maxConcurrentActions,omitempty" yaml:"maxConcurrentActions,omitempty"`
	MaxTokenBudgetPerAction int64 `json:"maxTokenBudgetPerAction,omitempty" yaml:"maxTokenBudgetPerAction,omitempty"`
}

// TrustProfile tracks the trust score of an agent over time.
type TrustProfile struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       TrustProfileSpec   `json:"spec" yaml:"spec"`
	Status     TrustProfileStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (t *TrustProfile) GetTypeMeta() TypeMeta      { return t.TypeMeta }
func (t *TrustProfile) GetObjectMeta() *ObjectMeta { return &t.ObjectMeta }

// TrustProfileSpec defines the trust profile configuration.
type TrustProfileSpec struct {
	// AgentRef references the agent being evaluated.
	AgentRef LocalObjectReference `json:"agentRef" yaml:"agentRef"`

	// AgentIncarnationUID binds this profile to one immutable Agent incarnation.
	// Deleting and recreating an Agent with the same name cannot inherit it.
	AgentIncarnationUID string `json:"agentIncarnationUID" yaml:"agentIncarnationUID"`

	// InitialScore is the starting trust score for the agent.
	InitialScore float64 `json:"initialScore" yaml:"initialScore"`

	// InitialClass is the starting autonomy class for the agent.
	InitialClass string `json:"initialClass,omitempty" yaml:"initialClass,omitempty"`

	// MaxClass is the highest autonomy class this agent can reach.
	MaxClass string `json:"maxClass,omitempty" yaml:"maxClass,omitempty"`

	// ScoringWeights configures how different event types affect the score.
	ScoringWeights *ScoringWeights `json:"scoringWeights,omitempty" yaml:"scoringWeights,omitempty"`

	// EvaluationInterval is the duration between periodic score evaluations.
	EvaluationInterval string `json:"evaluationInterval,omitempty" yaml:"evaluationInterval,omitempty"`

	// PromotionPolicy defines promotion behavior.
	PromotionPolicy *PromotionPolicy `json:"promotionPolicy,omitempty" yaml:"promotionPolicy,omitempty"`

	// DemotionPolicy defines demotion behavior.
	DemotionPolicy *DemotionPolicy `json:"demotionPolicy,omitempty" yaml:"demotionPolicy,omitempty"`
}

// ScoringWeights configures how different event types affect trust scores.
type ScoringWeights struct {
	TaskCompletionRate    float64 `json:"taskCompletionRate,omitempty" yaml:"taskCompletionRate,omitempty"`
	PolicyCompliance      float64 `json:"policyCompliance,omitempty" yaml:"policyCompliance,omitempty"`
	ApprovalAlignmentRate float64 `json:"approvalAlignmentRate,omitempty" yaml:"approvalAlignmentRate,omitempty"`
	CostEfficiency        float64 `json:"costEfficiency,omitempty" yaml:"costEfficiency,omitempty"`
	ToolUsageAccuracy     float64 `json:"toolUsageAccuracy,omitempty" yaml:"toolUsageAccuracy,omitempty"`
	// Legacy event-delta weights remain supported for event-driven scoring.
	SuccessWeight         float64 `json:"successWeight,omitempty" yaml:"successWeight,omitempty"`
	FailureWeight         float64 `json:"failureWeight,omitempty" yaml:"failureWeight,omitempty"`
	PolicyViolationWeight float64 `json:"policyViolationWeight,omitempty" yaml:"policyViolationWeight,omitempty"`
}

// PromotionPolicy defines criteria for tier promotion.
type PromotionPolicy struct {
	// RequireReview indicates if promotion requires human review.
	RequireReview bool `json:"requireReview,omitempty" yaml:"requireReview,omitempty"`
	// MinActions is the minimum successful actions required for promotion.
	MinActions int64 `json:"minActions,omitempty" yaml:"minActions,omitempty"`
	// MinScoreDuration is how long the score must be above threshold.
	MinScoreDuration        string               `json:"minScoreDuration,omitempty" yaml:"minScoreDuration,omitempty"`
	StabilizationWindowDays int64                `json:"stabilizationWindowDays,omitempty" yaml:"stabilizationWindowDays,omitempty"`
	Thresholds              []PromotionThreshold `json:"thresholds,omitempty" yaml:"thresholds,omitempty"`
}

type PromotionThreshold struct {
	TargetClass   string  `json:"targetClass" yaml:"targetClass"`
	MinScore      float64 `json:"minScore" yaml:"minScore"`
	RequireReview bool    `json:"requireReview,omitempty" yaml:"requireReview,omitempty"`
}

// DemotionPolicy defines criteria for tier demotion.
type DemotionPolicy struct {
	// Mode: "immediate" (default), "probation"
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`
	// FallbackClass is the class to demote to on critical violations.
	FallbackClass string `json:"fallbackClass,omitempty" yaml:"fallbackClass,omitempty"`
	// FreezeDuration is how long to freeze the agent at the demoted tier.
	FreezeDuration string `json:"freezeDuration,omitempty" yaml:"freezeDuration,omitempty"`
}

// TrustProfileStatus describes the current trust state.
type TrustProfileStatus struct {
	CurrentScore       float64           `json:"currentScore,omitempty" yaml:"currentScore,omitempty"`
	CurrentTier        string            `json:"currentTier,omitempty" yaml:"currentTier,omitempty"`
	CurrentClass       string            `json:"currentClass,omitempty" yaml:"currentClass,omitempty"`
	TotalEvaluations   int64             `json:"totalEvaluations,omitempty" yaml:"totalEvaluations,omitempty"`
	ScoreHistory       []ScoreEntry      `json:"scoreHistory,omitempty" yaml:"scoreHistory,omitempty"`
	ApprovalStats      *ApprovalStats    `json:"approvalStats,omitempty" yaml:"approvalStats,omitempty"`
	Conditions         []StatusCondition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	LastEvaluationTime string            `json:"lastEvaluationTime,omitempty" yaml:"lastEvaluationTime,omitempty"`
	AuditedActions     int64             `json:"auditedActions,omitempty" yaml:"auditedActions,omitempty"`
	ThresholdMetSince  string            `json:"thresholdMetSince,omitempty" yaml:"thresholdMetSince,omitempty"`
	ThresholdClass     string            `json:"thresholdClass,omitempty" yaml:"thresholdClass,omitempty"`
	ProbationUntil     string            `json:"probationUntil,omitempty" yaml:"probationUntil,omitempty"`
	PendingTrustChange *TrustChangeAudit `json:"pendingTrustChange,omitempty" yaml:"pendingTrustChange,omitempty"`
	ProcessedEventIDs  []string          `json:"processedEventIDs,omitempty" yaml:"processedEventIDs,omitempty"`
}

type TrustChangeAudit struct {
	Kind      string `json:"kind" yaml:"kind"`
	FromClass string `json:"fromClass" yaml:"fromClass"`
	ToClass   string `json:"toClass" yaml:"toClass"`
	Evidence  string `json:"evidence" yaml:"evidence"`
	Timestamp string `json:"timestamp,omitempty" yaml:"timestamp,omitempty"`
}

// ScoreEntry records a historical score data point.
type ScoreEntry struct {
	Timestamp  string                `json:"timestamp" yaml:"timestamp"`
	Score      float64               `json:"score" yaml:"score"`
	Reason     string                `json:"reason,omitempty" yaml:"reason,omitempty"`
	Class      string                `json:"class,omitempty" yaml:"class,omitempty"`
	Dimensions *TrustScoreDimensions `json:"dimensions,omitempty" yaml:"dimensions,omitempty"`
}

type TrustScoreDimensions struct {
	TaskCompletionRate    float64 `json:"taskCompletionRate" yaml:"taskCompletionRate"`
	PolicyCompliance      float64 `json:"policyCompliance" yaml:"policyCompliance"`
	ApprovalAlignmentRate float64 `json:"approvalAlignmentRate" yaml:"approvalAlignmentRate"`
	CostEfficiency        float64 `json:"costEfficiency" yaml:"costEfficiency"`
	ToolUsageAccuracy     float64 `json:"toolUsageAccuracy" yaml:"toolUsageAccuracy"`
}

// ApprovalStats tracks approval/denial rates for the agent.
type ApprovalStats struct {
	TotalRequested int64 `json:"totalRequested,omitempty" yaml:"totalRequested,omitempty"`
	TotalApproved  int64 `json:"totalApproved,omitempty" yaml:"totalApproved,omitempty"`
	TotalDenied    int64 `json:"totalDenied,omitempty" yaml:"totalDenied,omitempty"`
}

// StatusCondition represents a condition on a resource.
type StatusCondition struct {
	Type    string `json:"type" yaml:"type"`
	Status  string `json:"status" yaml:"status"`
	Reason  string `json:"reason,omitempty" yaml:"reason,omitempty"`
	Message string `json:"message,omitempty" yaml:"message,omitempty"`
}

// AutonomyReview records a human review of an agent's autonomous actions.
type AutonomyReview struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       AutonomyReviewSpec   `json:"spec" yaml:"spec"`
	Status     AutonomyReviewStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (a *AutonomyReview) GetTypeMeta() TypeMeta      { return a.TypeMeta }
func (a *AutonomyReview) GetObjectMeta() *ObjectMeta { return &a.ObjectMeta }

// AutonomyReviewSpec defines what is being reviewed.
type AutonomyReviewSpec struct {
	// AgentRef references the agent whose action is under review.
	AgentRef LocalObjectReference `json:"agentRef" yaml:"agentRef"`

	// ActionDescription describes the action that requires approval.
	ActionDescription string `json:"actionDescription" yaml:"actionDescription"`

	// SessionID references the session in which the action was proposed.
	SessionID string `json:"sessionID,omitempty" yaml:"sessionID,omitempty"`

	// CurrentClass is the agent's current autonomy class.
	CurrentClass string `json:"currentClass,omitempty" yaml:"currentClass,omitempty"`

	// RequestedClass is the autonomy class being requested.
	RequestedClass string `json:"requestedClass,omitempty" yaml:"requestedClass,omitempty"`

	// TrustProfileRef references the agent's TrustProfile.
	TrustProfileRef string `json:"trustProfileRef,omitempty" yaml:"trustProfileRef,omitempty"`

	// Evidence provides supporting data for the review.
	Evidence string `json:"evidence,omitempty" yaml:"evidence,omitempty"`
}

// AutonomyReviewStatus describes the review decision.
type AutonomyReviewStatus struct {
	Decision  string `json:"decision,omitempty" yaml:"decision,omitempty"` // "Approved", "Denied", "Pending"
	DecidedBy string `json:"decidedBy,omitempty" yaml:"decidedBy,omitempty"`
	Reason    string `json:"reason,omitempty" yaml:"reason,omitempty"`
	DecidedAt string `json:"decidedAt,omitempty" yaml:"decidedAt,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty" yaml:"expiresAt,omitempty"`
}
