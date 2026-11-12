// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import "time"

const (
	RuntimeApprovalPlaneTool          = "Tool"
	RuntimeApprovalPlaneMemory        = "Memory"
	RuntimeApprovalPlaneCommunication = "Communication"

	RuntimeApprovalDecisionPending     = "Pending"
	RuntimeApprovalDecisionNotRequired = "NotRequired"
	RuntimeApprovalDecisionApproved    = "Approved"
	RuntimeApprovalDecisionRejected    = "Rejected"
	RuntimeApprovalDecisionExpired     = "Expired"

	RuntimeApprovalExecutionNotStarted = "NotStarted"
	RuntimeApprovalExecutionClaimed    = "Claimed"
	RuntimeApprovalExecutionUncertain  = "Uncertain"
	RuntimeApprovalExecutionCompleted  = "Completed"
	RuntimeApprovalExecutionFailed     = "Failed"
)

// RuntimeApproval is the durable, exact-bound decision and replay owner for a
// protected runtime action. It is distinct from workflow ApprovalDecision and
// trust-promotion AutonomyReview resources.
type RuntimeApproval struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       RuntimeApprovalSpec   `json:"spec" yaml:"spec"`
	Status     RuntimeApprovalStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (r *RuntimeApproval) GetTypeMeta() TypeMeta      { return r.TypeMeta }
func (r *RuntimeApproval) GetObjectMeta() *ObjectMeta { return &r.ObjectMeta }

// RuntimeApprovalSpec contains only bounded references and keyed binding
// evidence. Complete action payloads and secrets are never persisted here.
type RuntimeApprovalSpec struct {
	Workspace         string                     `json:"workspace" yaml:"workspace"`
	InvocationID      string                     `json:"invocationID" yaml:"invocationID"`
	InitialDecision   string                     `json:"initialDecision" yaml:"initialDecision"`
	Plane             string                     `json:"plane" yaml:"plane"`
	Operation         string                     `json:"operation" yaml:"operation"`
	AgentRef          RuntimeApprovalReference   `json:"agentRef" yaml:"agentRef"`
	AgentIdentityRef  RuntimeApprovalReference   `json:"agentIdentityRef" yaml:"agentIdentityRef"`
	AgentInstanceRef  RuntimeApprovalReference   `json:"agentInstanceRef" yaml:"agentInstanceRef"`
	SessionRef        string                     `json:"sessionRef" yaml:"sessionRef"`
	TargetRef         RuntimeApprovalReference   `json:"targetRef" yaml:"targetRef"`
	TrustProfileRef   *RuntimeApprovalReference  `json:"trustProfileRef,omitempty" yaml:"trustProfileRef,omitempty"`
	AutonomyClassRef  RuntimeApprovalReference   `json:"autonomyClassRef" yaml:"autonomyClassRef"`
	AuthorizationRefs []RuntimeApprovalReference `json:"authorizationRefs,omitempty" yaml:"authorizationRefs,omitempty"`
	Binding           RuntimeActionBinding       `json:"binding" yaml:"binding"`
	CreatedAt         time.Time                  `json:"createdAt" yaml:"createdAt"`
	ExpiresAt         time.Time                  `json:"expiresAt" yaml:"expiresAt"`
}

// RuntimeApprovalReference identifies one exact persisted resource incarnation.
type RuntimeApprovalReference struct {
	Kind            string `json:"kind" yaml:"kind"`
	Scope           string `json:"scope" yaml:"scope"`
	Namespace       string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Name            string `json:"name" yaml:"name"`
	UID             string `json:"uid" yaml:"uid"`
	ResourceVersion int64  `json:"resourceVersion" yaml:"resourceVersion"`
	// ConfigurationDigest binds AgentInstance execution configuration while allowing status-only resourceVersion changes.
	ConfigurationDigest string `json:"configurationDigest,omitempty" yaml:"configurationDigest,omitempty"`
}

// RuntimeActionBinding identifies the canonical format and retained HMAC key
// needed to compare a retry without persisting its sensitive payload.
type RuntimeActionBinding struct {
	Format     string `json:"format" yaml:"format"`
	Algorithm  string `json:"algorithm" yaml:"algorithm"`
	Domain     string `json:"domain" yaml:"domain"`
	Epoch      int64  `json:"epoch" yaml:"epoch"`
	KeyVersion int64  `json:"keyVersion" yaml:"keyVersion"`
	Tag        string `json:"tag" yaml:"tag"`
}

// RuntimeApprovalStatus contains the independent decision and execution state
// machines. This foundation exposes decision transitions; later gateway slices
// fill preparation, claim, recovery, and terminal execution fields.
type RuntimeApprovalStatus struct {
	DecisionState             string                        `json:"decisionState" yaml:"decisionState"`
	DecisionEvidenceDelivered bool                          `json:"decisionEvidenceDelivered,omitempty" yaml:"decisionEvidenceDelivered,omitempty"`
	DecisionAudit             *RuntimeApprovalDecisionAudit `json:"decisionAudit,omitempty" yaml:"decisionAudit,omitempty"`
	DecidedByIssuer           string                        `json:"decidedByIssuer,omitempty" yaml:"decidedByIssuer,omitempty"`
	DecidedBySubject          string                        `json:"decidedBySubject,omitempty" yaml:"decidedBySubject,omitempty"`
	DecidedAt                 *time.Time                    `json:"decidedAt,omitempty" yaml:"decidedAt,omitempty"`
	ExecutionState            string                        `json:"executionState" yaml:"executionState"`
	ExecutionDisposition      string                        `json:"executionDisposition,omitempty" yaml:"executionDisposition,omitempty"`
	PreparationToken          string                        `json:"preparationToken,omitempty" yaml:"preparationToken,omitempty"`
	PreparationDeadline       *time.Time                    `json:"preparationDeadline,omitempty" yaml:"preparationDeadline,omitempty"`
	Claimant                  *RuntimeApprovalClaimant      `json:"claimant,omitempty" yaml:"claimant,omitempty"`
	FencingToken              int64                         `json:"fencingToken,omitempty" yaml:"fencingToken,omitempty"`
	TerminationProofRef       string                        `json:"terminationProofRef,omitempty" yaml:"terminationProofRef,omitempty"`
	ReconciliationEvidenceRef string                        `json:"reconciliationEvidenceRef,omitempty" yaml:"reconciliationEvidenceRef,omitempty"`
	ReconciledByIssuer        string                        `json:"reconciledByIssuer,omitempty" yaml:"reconciledByIssuer,omitempty"`
	ReconciledBySubject       string                        `json:"reconciledBySubject,omitempty" yaml:"reconciledBySubject,omitempty"`
}

// RuntimeApprovalDecisionAudit is the durable decision outbox. It is created
// atomically with the terminal decision and acknowledged independently.
type RuntimeApprovalDecisionAudit struct {
	ID      string                              `json:"id" yaml:"id"`
	Payload RuntimeApprovalDecisionAuditPayload `json:"payload" yaml:"payload"`
}

// RuntimeApprovalDecisionAuditPayload is deliberately bounded and contains no
// action payload or secret material.
type RuntimeApprovalDecisionAuditPayload struct {
	Workspace    string    `json:"workspace" yaml:"workspace"`
	InvocationID string    `json:"invocationID" yaml:"invocationID"`
	ApprovalUID  string    `json:"approvalUID" yaml:"approvalUID"`
	Decision     string    `json:"decision" yaml:"decision"`
	Issuer       string    `json:"issuer,omitempty" yaml:"issuer,omitempty"`
	Subject      string    `json:"subject,omitempty" yaml:"subject,omitempty"`
	DecidedAt    time.Time `json:"decidedAt" yaml:"decidedAt"`
}

// RuntimeApprovalClaimant identifies the gateway process incarnation that owns
// a successful execution claim.
type RuntimeApprovalClaimant struct {
	WorkloadUID       string     `json:"workloadUID" yaml:"workloadUID"`
	ContainerID       string     `json:"containerID,omitempty" yaml:"containerID,omitempty"`
	ProcessStartNonce string     `json:"processStartNonce" yaml:"processStartNonce"`
	ClaimedAt         time.Time  `json:"claimedAt" yaml:"claimedAt"`
	LastHeartbeat     time.Time  `json:"lastHeartbeat" yaml:"lastHeartbeat"`
	LeaseDeadline     *time.Time `json:"leaseDeadline,omitempty" yaml:"leaseDeadline,omitempty"`
}
