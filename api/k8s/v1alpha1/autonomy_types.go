// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Level",type=integer,JSONPath=`.spec.level`
// +kubebuilder:printcolumn:name="Approval",type=boolean,JSONPath=`.spec.requiresApproval`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AutonomyClass defines a tier of agent autonomy.
type AutonomyClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AutonomyClassSpec `json:"spec"`
}

// AutonomyClassSpec defines the autonomy tier parameters.
type AutonomyClassSpec struct {
	Level            int                  `json:"level"`
	RequiresApproval bool                 `json:"requiresApproval"`
	MaxCostPerAction int64                `json:"maxCostPerAction,omitempty"`
	AllowedTools     []string             `json:"allowedTools,omitempty"`
	ApprovalPolicy   *ApprovalPolicy      `json:"approvalPolicy,omitempty"`
	Constraints      *AutonomyConstraints `json:"constraints,omitempty"`
}

// ApprovalPolicy defines approval requirements for different action types.
type ApprovalPolicy struct {
	ToolInvocation        string `json:"toolInvocation,omitempty"`
	MemoryWrite           string `json:"memoryWrite,omitempty"`
	ExternalCommunication string `json:"externalCommunication,omitempty"`
	CostThreshold         int64  `json:"costThreshold,omitempty"`
}

// AutonomyConstraints defines action limits for an autonomy level.
type AutonomyConstraints struct {
	MaxConcurrentActions    int   `json:"maxConcurrentActions,omitempty"`
	MaxTokenBudgetPerAction int64 `json:"maxTokenBudgetPerAction,omitempty"`
}

// +kubebuilder:object:root=true

// AutonomyClassList contains a list of AutonomyClasses.
type AutonomyClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AutonomyClass `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Decision",type=string,JSONPath=`.status.decision`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AutonomyReview records a human review of an agent's autonomous actions.
type AutonomyReview struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AutonomyReviewSpec   `json:"spec"`
	Status            AutonomyReviewStatus `json:"status,omitempty"`
}

// AutonomyReviewSpec defines what is being reviewed.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="AutonomyReview spec is immutable after creation"
type AutonomyReviewSpec struct {
	// +kubebuilder:validation:XValidation:rule="size(self.name) > 0",message="Agent reference name must not be empty"
	AgentRef LocalObjectReference `json:"agentRef"`
	// +kubebuilder:validation:MinLength=1
	ActionDescription string `json:"actionDescription"`
	SessionID         string `json:"sessionID,omitempty"`
	CurrentClass      string `json:"currentClass,omitempty"`
	RequestedClass    string `json:"requestedClass,omitempty"`
	TrustProfileRef   string `json:"trustProfileRef,omitempty"`
	Evidence          string `json:"evidence,omitempty"`
}

// AutonomyReviewStatus describes the review decision.
type AutonomyReviewStatus struct {
	// +kubebuilder:validation:Enum=Pending;Approved;Denied
	Decision  string `json:"decision,omitempty"`
	DecidedBy string `json:"decidedBy,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// +kubebuilder:validation:Format=date-time
	DecidedAt string `json:"decidedAt,omitempty"`
	// +kubebuilder:validation:Format=date-time
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// +kubebuilder:object:root=true

// AutonomyReviewList contains a list of AutonomyReviews.
type AutonomyReviewList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AutonomyReview `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AutonomyClass{}, &AutonomyClassList{}, &AutonomyReview{}, &AutonomyReviewList{})
}
