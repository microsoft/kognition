// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Run",type=string,JSONPath=`.spec.workflowRunRef.name`
// +kubebuilder:printcolumn:name="Step",type=string,JSONPath=`.spec.step`
// +kubebuilder:printcolumn:name="Decision",type=string,JSONPath=`.spec.decision`
// +kubebuilder:printcolumn:name="Approver",type=string,JSONPath=`.spec.approver`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ApprovalDecision records a single approve/deny vote against a WorkflowRun's
// approval gate. The CRD-only controller lists ApprovalDecisions to enforce
// WorkflowApprovalGate.RequiredApprovers and to detect denials.
type ApprovalDecision struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ApprovalDecisionSpec   `json:"spec,omitempty"`
	Status            ApprovalDecisionStatus `json:"status,omitempty"`
}

// ApprovalDecisionSpec is immutable after creation.
type ApprovalDecisionSpec struct {
	// WorkflowRunRef identifies the WorkflowRun (or Workflow) the decision
	// applies to. Always namespace-scoped to the ApprovalDecision's namespace.
	WorkflowRunRef LocalObjectReference `json:"workflowRunRef"`

	// Step is the destination step of the approval gate (WorkflowApprovalGate.To).
	Step string `json:"step"`

	// Iteration scopes the decision to a specific cyclic-workflow iteration.
	// +optional
	Iteration int32 `json:"iteration,omitempty"`

	// Decision is "approve" or "deny".
	// +kubebuilder:validation:Enum=approve;deny
	Decision string `json:"decision"`

	// Approver is the identity of the approver. Distinct approver values count
	// toward the gate's RequiredApprovers quorum.
	Approver string `json:"approver"`

	// Reason is a free-form justification recorded in the audit log.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// ApprovalDecisionStatus records whether the reconciler has folded the
// decision into the WorkflowRun's audit log.
type ApprovalDecisionStatus struct {
	// +optional
	Observed bool `json:"observed,omitempty"`
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// +kubebuilder:object:root=true

// ApprovalDecisionList contains a list of ApprovalDecisions.
type ApprovalDecisionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ApprovalDecision `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ApprovalDecision{}, &ApprovalDecisionList{})
}
