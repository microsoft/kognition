// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// RuntimeApprovalQuotaTicketRetirementFinalizer guards a ticket from being
// deleted until the orphan reconciler has proven, under the workspace Lease,
// that neither a live RuntimeApproval nor a RuntimeApprovalTombstone still
// references the ticket's logical key. Without the finalizer a manual delete
// could undercount workspace or Agent charges before the record actually goes
// away.
const RuntimeApprovalQuotaTicketRetirementFinalizer = "kognition.dev/runtimeapprovalquotaticket-retirement"

// RuntimeApprovalQuotaTicketPhase names whether the underlying record is still
// a live RuntimeApproval or has already been compacted into a
// RuntimeApprovalTombstone. The same ticket covers both phases: quota is
// charged exactly once for the logical invocation.
type RuntimeApprovalQuotaTicketPhase string

const (
	// RuntimeApprovalQuotaTicketPhaseLive marks a ticket created before
	// admission of a new RuntimeApproval and still tracking the live record.
	RuntimeApprovalQuotaTicketPhaseLive RuntimeApprovalQuotaTicketPhase = "Live"
	// RuntimeApprovalQuotaTicketPhaseCompacted marks a ticket whose live
	// RuntimeApproval has been compacted and now backs the tombstone through
	// the retention deadline.
	RuntimeApprovalQuotaTicketPhaseCompacted RuntimeApprovalQuotaTicketPhase = "Compacted"
)

// RuntimeApprovalQuotaTicket is the bounded per-logical-invocation accounting
// object for the Kubernetes RuntimeApproval quota ledger. It is created under
// a workspace Lease before the RuntimeApproval it charges, covers the live
// record and its tombstone with a single charge, and is retired only after
// the orphan reconciler proves both are gone.
//
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.spec.phase`
// +kubebuilder:printcolumn:name="Bytes",type=integer,JSONPath=`.spec.chargedBytes`
// +kubebuilder:printcolumn:name="Retain Until",type=date,JSONPath=`.spec.retentionDeadline`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type RuntimeApprovalQuotaTicket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RuntimeApprovalQuotaTicketSpec `json:"spec"`
}

// RuntimeApprovalQuotaTicketSpec is intentionally small: the ADR bounds a
// ticket to at most 2 KiB serialized. The binding descriptor is copied so a
// retention pass can validate the associated binding key is still retained.
//
// +kubebuilder:validation:XValidation:rule="has(self.actorKind) ? (has(self.executorRef) && !has(self.agentRef)) : (has(self.agentRef) && self.agentRef.kind == 'Agent' && self.agentRef.scope == 'Namespaced' && self.agentRef.__namespace__ == self.workspace)",message="quota ticket must identify its actual Agent or executor actor"
// +kubebuilder:validation:XValidation:rule="self.phase == 'Compacted' ? (has(self.compactedAt) && has(self.retentionDeadline)) : true",message="compacted tickets require compaction and retention timestamps"
// +kubebuilder:validation:XValidation:rule="self.phase == 'Live' ? !has(self.compactedAt) : true",message="live tickets cannot have a compaction timestamp"
type RuntimeApprovalQuotaTicketSpec struct {
	// +kubebuilder:validation:Enum=WorkflowExecutor
	ActorKind   string                     `json:"actorKind,omitempty"`
	ExecutorRef *WorkflowExecutorPrincipal `json:"executorRef,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Workspace string `json:"workspace"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	InvocationID string `json:"invocationID"`
	// +optional
	// +kubebuilder:validation:MaxLength=128
	RuntimeApprovalUID string                   `json:"runtimeApprovalUID,omitempty"`
	AgentRef           RuntimeApprovalReference `json:"agentRef,omitzero"`
	Binding            RuntimeActionBinding     `json:"binding"`
	// +kubebuilder:validation:Enum=Live;Compacted
	Phase RuntimeApprovalQuotaTicketPhase `json:"phase"`
	// +kubebuilder:validation:Minimum=1
	ChargedBytes int64       `json:"chargedBytes"`
	CreatedAt    metav1.Time `json:"createdAt"`
	// +optional
	CompactedAt *metav1.Time `json:"compactedAt,omitempty"`
	// +optional
	RetentionDeadline *metav1.Time `json:"retentionDeadline,omitempty"`
}

// +kubebuilder:object:root=true
type RuntimeApprovalQuotaTicketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RuntimeApprovalQuotaTicket `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RuntimeApprovalQuotaTicket{}, &RuntimeApprovalQuotaTicketList{})
}
