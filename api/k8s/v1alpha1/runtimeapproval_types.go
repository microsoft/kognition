// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Decision",type=string,JSONPath=`.status.decisionState`
// +kubebuilder:printcolumn:name="Execution",type=string,JSONPath=`.status.executionState`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type RuntimeApproval struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RuntimeApprovalSpec   `json:"spec"`
	Status            RuntimeApprovalStatus `json:"status,omitempty"`
}

type RuntimeApprovalSpec struct {
	Workspace         string                     `json:"workspace"`
	InvocationID      string                     `json:"invocationID"`
	InitialDecision   string                     `json:"initialDecision"`
	Plane             string                     `json:"plane"`
	Operation         string                     `json:"operation"`
	AgentRef          RuntimeApprovalReference   `json:"agentRef"`
	AgentIdentityRef  RuntimeApprovalReference   `json:"agentIdentityRef"`
	AgentInstanceRef  RuntimeApprovalReference   `json:"agentInstanceRef"`
	SessionRef        string                     `json:"sessionRef"`
	TargetRef         RuntimeApprovalReference   `json:"targetRef"`
	TrustProfileRef   *RuntimeApprovalReference  `json:"trustProfileRef,omitempty"`
	AutonomyClassRef  RuntimeApprovalReference   `json:"autonomyClassRef"`
	AuthorizationRefs []RuntimeApprovalReference `json:"authorizationRefs,omitempty"`
	Binding           RuntimeActionBinding       `json:"binding"`
	CreatedAt         metav1.Time                `json:"createdAt"`
	ExpiresAt         metav1.Time                `json:"expiresAt"`
}

type RuntimeApprovalReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Kind string `json:"kind"`
	// +kubebuilder:validation:Enum=Namespaced;Cluster
	Scope string `json:"scope"`
	// +kubebuilder:validation:MaxLength=253
	Namespace string `json:"namespace,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID string `json:"uid"`
	// +kubebuilder:validation:Minimum=1
	ResourceVersion int64 `json:"resourceVersion"`
	// ConfigurationDigest is supported only on agentInstanceRef.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	// +kubebuilder:validation:MinLength=64
	// +kubebuilder:validation:MaxLength=64
	ConfigurationDigest string `json:"configurationDigest,omitempty"`
}

type RuntimeActionBinding struct {
	// +kubebuilder:validation:Enum=kognition-runtime-action-v1
	Format string `json:"format"`
	// +kubebuilder:validation:Enum=HMAC-SHA-256
	Algorithm string `json:"algorithm"`
	// +kubebuilder:validation:Enum=kognition.dev/runtime-approval/action/v1
	Domain string `json:"domain"`
	// +kubebuilder:validation:Minimum=1
	Epoch int64 `json:"epoch"`
	// +kubebuilder:validation:Minimum=1
	KeyVersion int64 `json:"keyVersion"`
	// +kubebuilder:validation:MinLength=43
	// +kubebuilder:validation:MaxLength=64
	Tag string `json:"tag"`
}

type RuntimeApprovalStatus struct {
	DecisionState             string                        `json:"decisionState"`
	DecisionEvidenceDelivered bool                          `json:"decisionEvidenceDelivered,omitempty"`
	DecisionAudit             *RuntimeApprovalDecisionAudit `json:"decisionAudit,omitempty"`
	DecidedByIssuer           string                        `json:"decidedByIssuer,omitempty"`
	DecidedBySubject          string                        `json:"decidedBySubject,omitempty"`
	DecidedAt                 *metav1.Time                  `json:"decidedAt,omitempty"`
	ExecutionState            string                        `json:"executionState"`
	ExecutionDisposition      string                        `json:"executionDisposition,omitempty"`
	PreparationToken          string                        `json:"preparationToken,omitempty"`
	PreparationDeadline       *metav1.Time                  `json:"preparationDeadline,omitempty"`
	Claimant                  *RuntimeApprovalClaimant      `json:"claimant,omitempty"`
	FencingToken              int64                         `json:"fencingToken,omitempty"`
	TerminationProofRef       string                        `json:"terminationProofRef,omitempty"`
	ReconciliationEvidenceRef string                        `json:"reconciliationEvidenceRef,omitempty"`
	ReconciledByIssuer        string                        `json:"reconciledByIssuer,omitempty"`
	ReconciledBySubject       string                        `json:"reconciledBySubject,omitempty"`
}

type RuntimeApprovalDecisionAudit struct {
	ID      string                              `json:"id"`
	Payload RuntimeApprovalDecisionAuditPayload `json:"payload"`
}

type RuntimeApprovalDecisionAuditPayload struct {
	Workspace    string      `json:"workspace"`
	InvocationID string      `json:"invocationID"`
	ApprovalUID  string      `json:"approvalUID"`
	Decision     string      `json:"decision"`
	Issuer       string      `json:"issuer,omitempty"`
	Subject      string      `json:"subject,omitempty"`
	DecidedAt    metav1.Time `json:"decidedAt"`
}

type RuntimeApprovalClaimant struct {
	WorkloadUID       string       `json:"workloadUID"`
	ContainerID       string       `json:"containerID,omitempty"`
	ProcessStartNonce string       `json:"processStartNonce"`
	ClaimedAt         metav1.Time  `json:"claimedAt"`
	LastHeartbeat     metav1.Time  `json:"lastHeartbeat"`
	LeaseDeadline     *metav1.Time `json:"leaseDeadline,omitempty"`
}

// +kubebuilder:object:root=true

type RuntimeApprovalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RuntimeApproval `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RuntimeApproval{}, &RuntimeApprovalList{})
}
