// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import "time"

// ApprovalDecision records a single approve/deny vote against a WorkflowRun's
// approval gate. The CRD-only controller path lists ApprovalDecision objects
// to evaluate quorum (WorkflowApprovalGate.RequiredApprovers) and to detect
// rejections.
type ApprovalDecision struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ApprovalDecisionSpec   `json:"spec" yaml:"spec"`
	Status     ApprovalDecisionStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (a *ApprovalDecision) GetTypeMeta() TypeMeta      { return a.TypeMeta }
func (a *ApprovalDecision) GetObjectMeta() *ObjectMeta { return &a.ObjectMeta }

// ApprovalDecisionSpec is immutable after creation. Submitting a different
// decision requires creating a new ApprovalDecision object.
type ApprovalDecisionSpec struct {
	// WorkflowRunRef identifies the WorkflowRun (or Workflow, when no run wraps
	// it) whose gate this decision applies to. Always namespace-scoped to the
	// ApprovalDecision's own namespace.
	WorkflowRunRef LocalObjectReference `json:"workflowRunRef" yaml:"workflowRunRef"`

	// Step is the destination step of the approval gate (matches WorkflowApprovalGate.To).
	Step string `json:"step" yaml:"step"`

	// Iteration scopes the decision to a specific cyclic-workflow iteration.
	// Defaults to 0 for non-cyclic workflows.
	Iteration int32 `json:"iteration,omitempty" yaml:"iteration,omitempty"`

	// Decision is "approve" or "deny".
	Decision string `json:"decision" yaml:"decision"`

	// Approver is the identity of the approver. Distinct approver values count
	// toward the gate's RequiredApprovers quorum.
	Approver string `json:"approver" yaml:"approver"`

	// Reason is a free-form justification recorded in the audit log.
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// ApprovalDecisionStatus records whether the reconciler has observed the
// decision and folded it into the WorkflowRun's audit log.
type ApprovalDecisionStatus struct {
	Observed   bool       `json:"observed,omitempty" yaml:"observed,omitempty"`
	ObservedAt *time.Time `json:"observedAt,omitempty" yaml:"observedAt,omitempty"`
}

// Approval decision values.
const (
	ApprovalDecisionApprove = "approve"
	ApprovalDecisionDeny    = "deny"
)
