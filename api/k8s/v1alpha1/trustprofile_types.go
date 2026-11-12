// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

type TrustProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TrustProfileSpec   `json:"spec,omitempty"`
	Status            TrustProfileStatus `json:"status,omitempty"`
}

type TrustProfileSpec struct {
	AgentRef LocalObjectReference `json:"agentRef"`
	// +kubebuilder:validation:MinLength=1
	AgentIncarnationUID string `json:"agentIncarnationUID"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	InitialScore       float64          `json:"initialScore"`
	InitialClass       string           `json:"initialClass,omitempty"`
	MaxClass           string           `json:"maxClass,omitempty"`
	ScoringWeights     *ScoringWeights  `json:"scoringWeights,omitempty"`
	EvaluationInterval string           `json:"evaluationInterval,omitempty"`
	PromotionPolicy    *PromotionPolicy `json:"promotionPolicy,omitempty"`
	DemotionPolicy     *DemotionPolicy  `json:"demotionPolicy,omitempty"`
}

type ScoringWeights struct {
	TaskCompletionRate    float64 `json:"taskCompletionRate,omitempty"`
	PolicyCompliance      float64 `json:"policyCompliance,omitempty"`
	ApprovalAlignmentRate float64 `json:"approvalAlignmentRate,omitempty"`
	CostEfficiency        float64 `json:"costEfficiency,omitempty"`
	ToolUsageAccuracy     float64 `json:"toolUsageAccuracy,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	SuccessWeight float64 `json:"successWeight,omitempty"`
	// +kubebuilder:validation:Minimum=-100
	// +kubebuilder:validation:Maximum=0
	FailureWeight float64 `json:"failureWeight,omitempty"`
	// +kubebuilder:validation:Minimum=-100
	// +kubebuilder:validation:Maximum=0
	PolicyViolationWeight float64 `json:"policyViolationWeight,omitempty"`
}

type PromotionPolicy struct {
	RequireReview           bool                 `json:"requireReview,omitempty"`
	MinActions              int64                `json:"minActions,omitempty"`
	MinScoreDuration        string               `json:"minScoreDuration,omitempty"`
	StabilizationWindowDays int64                `json:"stabilizationWindowDays,omitempty"`
	Thresholds              []PromotionThreshold `json:"thresholds,omitempty"`
}

type PromotionThreshold struct {
	TargetClass   string  `json:"targetClass"`
	MinScore      float64 `json:"minScore"`
	RequireReview bool    `json:"requireReview,omitempty"`
}

type DemotionPolicy struct {
	Mode           string `json:"mode,omitempty"`
	FallbackClass  string `json:"fallbackClass,omitempty"`
	FreezeDuration string `json:"freezeDuration,omitempty"`
}

type TrustProfileStatus struct {
	CurrentScore       float64           `json:"currentScore,omitempty"`
	CurrentTier        string            `json:"currentTier,omitempty"`
	CurrentClass       string            `json:"currentClass,omitempty"`
	TotalEvaluations   int64             `json:"totalEvaluations,omitempty"`
	ScoreHistory       []ScoreEntry      `json:"scoreHistory,omitempty"`
	ApprovalStats      *ApprovalStats    `json:"approvalStats,omitempty"`
	Conditions         []StatusCondition `json:"conditions,omitempty"`
	LastEvaluationTime string            `json:"lastEvaluationTime,omitempty"`
	AuditedActions     int64             `json:"auditedActions,omitempty"`
	ThresholdMetSince  string            `json:"thresholdMetSince,omitempty"`
	ThresholdClass     string            `json:"thresholdClass,omitempty"`
	ProbationUntil     string            `json:"probationUntil,omitempty"`
	PendingTrustChange *TrustChangeAudit `json:"pendingTrustChange,omitempty"`
	ProcessedEventIDs  []string          `json:"processedEventIDs,omitempty"`
}

type TrustChangeAudit struct {
	Kind      string `json:"kind"`
	FromClass string `json:"fromClass"`
	ToClass   string `json:"toClass"`
	Evidence  string `json:"evidence"`
	Timestamp string `json:"timestamp,omitempty"`
}

type ScoreEntry struct {
	Timestamp  string                `json:"timestamp"`
	Score      float64               `json:"score"`
	Reason     string                `json:"reason,omitempty"`
	Class      string                `json:"class,omitempty"`
	Dimensions *TrustScoreDimensions `json:"dimensions,omitempty"`
}

type TrustScoreDimensions struct {
	TaskCompletionRate    float64 `json:"taskCompletionRate"`
	PolicyCompliance      float64 `json:"policyCompliance"`
	ApprovalAlignmentRate float64 `json:"approvalAlignmentRate"`
	CostEfficiency        float64 `json:"costEfficiency"`
	ToolUsageAccuracy     float64 `json:"toolUsageAccuracy"`
}

type ApprovalStats struct {
	TotalRequested int64 `json:"totalRequested,omitempty"`
	TotalApproved  int64 `json:"totalApproved,omitempty"`
	TotalDenied    int64 `json:"totalDenied,omitempty"`
}

type StatusCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true

type TrustProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TrustProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TrustProfile{}, &TrustProfileList{})
}
