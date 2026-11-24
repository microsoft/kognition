// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	RuntimeApprovalCompactionFinalizer = "runtimeapproval.compaction.kognition.dev"
	RuntimeApprovalRetentionFinalizer  = "runtimeapprovaltombstone.retention.kognition.dev"
)

// RuntimeApprovalTombstone is the bounded replay fence retained after a
// terminal RuntimeApproval is compacted. It deliberately excludes action
// payloads, credentials, decision actors, and unbounded evidence.
//
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Decision",type=string,JSONPath=`.spec.decisionState`
// +kubebuilder:printcolumn:name="Execution",type=string,JSONPath=`.spec.executionState`
// +kubebuilder:printcolumn:name="Retain Until",type=date,JSONPath=`.spec.retentionDeadline`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.spec == oldSelf.spec",message="RuntimeApprovalTombstone spec is immutable"
type RuntimeApprovalTombstone struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RuntimeApprovalTombstoneSpec `json:"spec"`
}

// +kubebuilder:validation:XValidation:rule="has(self.actorKind) ? (has(self.executorAction) && !has(self.agentRef)) : (has(self.agentRef) && self.agentRef.kind == 'Agent' && self.agentRef.scope == 'Namespaced' && self.agentRef.__namespace__ == self.workspace)",message="replay fence must preserve its actual Agent or executor actor"
// +kubebuilder:validation:XValidation:rule="self.compactedAt >= self.createdAt",message="compactedAt must not precede createdAt"
// +kubebuilder:validation:XValidation:rule="self.retentionDeadline > self.compactedAt",message="retentionDeadline must follow compactedAt"
// +kubebuilder:validation:XValidation:rule="self.decisionState in ['Rejected', 'Expired'] ? self.executionState == 'NotStarted' : self.executionState in ['Completed', 'Failed']",message="decision and execution states must describe a terminal action"
// +kubebuilder:validation:XValidation:rule="self.executionState == 'NotStarted' ? !has(self.executionDisposition) : has(self.executionDisposition)",message="only terminal execution states require an execution disposition"
type RuntimeApprovalTombstoneSpec struct {
	// +kubebuilder:validation:Enum=WorkflowExecutor
	ActorKind       string                           `json:"actorKind,omitempty"`
	ExecutorAction  *WorkflowExecutorActionReference `json:"executorAction,omitempty"`
	WorkflowContext *WorkflowActionContext           `json:"workflowContext,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Workspace string `json:"workspace"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	InvocationID string `json:"invocationID"`
	// +kubebuilder:validation:Enum=Tool;Memory;Communication;Workflow
	Plane string `json:"plane"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Operation string                   `json:"operation"`
	AgentRef  RuntimeApprovalReference `json:"agentRef,omitzero"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	RuntimeApprovalUID string `json:"runtimeApprovalUID"`
	// +kubebuilder:validation:Enum=Pending;NotRequired
	InitialDecision string               `json:"initialDecision"`
	Binding         RuntimeActionBinding `json:"binding"`
	// +kubebuilder:validation:Enum=Approved;Rejected;Expired;NotRequired
	DecisionState string `json:"decisionState"`
	// +kubebuilder:validation:Enum=NotStarted;Completed;Failed
	ExecutionState string `json:"executionState"`
	// +optional
	// +kubebuilder:validation:Enum=QuotaDenied;ReceiptConflict;PolicyChanged;TargetChanged;SessionChanged;PreparationRejected;ApprovalExpiredBeforeClaim;AbandonedPreparation;ReconciledCompleted;ReconciledNotExecuted;Succeeded;Failed
	ExecutionDisposition string      `json:"executionDisposition,omitempty"`
	CreatedAt            metav1.Time `json:"createdAt"`
	ExpiresAt            metav1.Time `json:"expiresAt"`
	CompactedAt          metav1.Time `json:"compactedAt"`
	RetentionDeadline    metav1.Time `json:"retentionDeadline"`
}

// +kubebuilder:object:root=true
type RuntimeApprovalTombstoneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RuntimeApprovalTombstone `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RuntimeApprovalTombstone{}, &RuntimeApprovalTombstoneList{})
}
