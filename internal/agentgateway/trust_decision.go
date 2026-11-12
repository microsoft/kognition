// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

type communicationDecisionInput struct {
	namespace, identityName, instanceName, instanceUID, sessionID, capabilityID string
	solution, runID, targetNamespace, targetService, invocationID, operation    string
	solutionRev                                                                 int64
	approval                                                                    *observability.TrustDecisionApproval
}

func communicationDecisionInputFromSession(claims *runtimeauth.AgentDelegationSessionCapabilityClaims, targetNamespace, targetService, invocationID, runID string) communicationDecisionInput {
	return communicationDecisionInput{namespace: claims.Namespace, identityName: claims.AgentIdentityName, instanceName: claims.InstanceName, instanceUID: claims.InstanceUID, sessionID: claims.SessionID, capabilityID: claims.ID, solution: claims.Solution, solutionRev: claims.SolutionRev, runID: runID, targetNamespace: targetNamespace, targetService: targetService, invocationID: invocationID, operation: "exchange"}
}

func communicationDecisionInputFromExact(claims *runtimeauth.AgentDelegationCapabilityClaims, runID string) communicationDecisionInput {
	return communicationDecisionInput{namespace: claims.Namespace, identityName: claims.AgentIdentityName, instanceName: claims.InstanceName, instanceUID: claims.InstanceUID, sessionID: claims.SessionID, capabilityID: claims.ID, solution: claims.Solution, solutionRev: claims.SolutionRev, runID: runID, targetNamespace: claims.TargetNamespace, targetService: claims.TargetAgentService, invocationID: claims.InvocationID, operation: "invoke"}
}

func (s *Server) recordCommunicationDecision(ctx context.Context, input communicationDecisionInput, identity *v1alpha1.AgentIdentity, outcome, reasonCode, executionStatus string, targetReached, beforeEffect bool) error {
	if s.audit == nil {
		return nil
	}
	now := time.Now().UTC()
	authorization := observability.TrustDecisionAuthorization{Identity: "allowed", Role: "not_required", Grant: "not_required", Policy: "allowed"}
	if (outcome == "denied" || outcome == "revoked") && reasonCode != "runtime_approval_unavailable" {
		authorization.Policy = "denied"
		if reasonCode == "identity_not_ready" {
			authorization.Identity = "denied"
			authorization.Policy = "not_evaluated"
		}
	}
	if outcome == "replayed" {
		authorization = observability.TrustDecisionAuthorization{Identity: "not_evaluated", Role: "not_required", Grant: "not_required", Policy: "not_evaluated"}
	}
	approval := input.approval
	if approval == nil {
		approval = &observability.TrustDecisionApproval{Status: "unavailable"}
		if s.gate3 == nil && s.gate3Builder == nil {
			approval.Status = "not_required"
		}
	}
	decision := observability.TrustDecisionV1{
		SchemaVersion: 1, ID: uuid.NewString(), Timestamp: now, Plane: "communication", Outcome: outcome, ReasonCode: reasonCode, Workspace: input.namespace, RunID: input.runID,
		Session:       &observability.TrustDecisionSession{SessionID: input.sessionID, CapabilityID: input.capabilityID, InstanceRef: &observability.TrustDecisionResourceRef{Kind: "AgentInstance", Namespace: input.namespace, Name: input.instanceName, UID: input.instanceUID}},
		Request:       observability.TrustDecisionRequest{Operation: input.operation, InvocationID: input.invocationID, TargetRef: observability.TrustDecisionResourceRef{Kind: "AgentService", Namespace: input.targetNamespace, Name: input.targetService}},
		Authorization: authorization,
		Approval:      approval,
		Execution:     observability.TrustDecisionExecution{Status: executionStatus, TargetReached: communicationBoolPointer(targetReached)},
	}
	if identity != nil {
		decision.Identity = &observability.TrustDecisionIdentity{AgentIdentity: identity.Spec.SPIFFEID, AgentIdentityRef: &observability.TrustDecisionResourceRef{Kind: "AgentIdentity", Namespace: input.namespace, Name: identity.ObjectMeta.Name, ResourceVersion: fmt.Sprint(identity.ObjectMeta.ResourceVersion), UID: identity.ObjectMeta.UID}}
	} else {
		decision.ClaimedIdentityRef = &observability.TrustDecisionResourceRef{Kind: "AgentIdentity", Namespace: input.namespace, Name: input.identityName}
	}
	details, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("encode communication trust decision: %w", err)
	}
	entry := observability.AuditEntry{ID: decision.ID, Timestamp: now, Workspace: input.namespace, Solution: input.solution, SolutionRev: input.solutionRev, SessionID: input.sessionID, EventType: observability.AuditCommunication, SubjectKind: "AgentService", SubjectRef: input.targetNamespace + "/" + input.targetService, Verb: input.operation, Details: details}
	if decision.Identity != nil {
		entry.AgentIdentity = decision.Identity.AgentIdentity
	}
	if beforeEffect {
		return s.audit.RecordBeforeExec(ctx, entry)
	}
	return s.audit.Record(ctx, entry)
}

func communicationBoolPointer(value bool) *bool { return &value }

// recordCommunicationApproval writes a Communication Trust Decision that
// captures an ADR-0014 Runtime Autonomy Gate outcome (pending/rejected)
// against a durable RuntimeApproval. The Approval block is populated with
// the resolved status and a canonical resource reference so downstream
// evaluators can reconcile the invocation without replaying the payload.
func (s *Server) recordCommunicationApproval(ctx context.Context, input communicationDecisionInput, identity *v1alpha1.AgentIdentity, outcome, reasonCode, executionStatus string, approval v1alpha1.RuntimeApproval) error {
	if s.audit == nil {
		return nil
	}
	now := time.Now().UTC()
	authorization := observability.TrustDecisionAuthorization{Identity: "allowed", Role: "not_required", Grant: "not_required", Policy: "allowed"}
	approvalStatus := outcome
	switch outcome {
	case "pending":
		approvalStatus = "pending"
	case "rejected":
		approvalStatus = "rejected"
	}
	decisionRef := &observability.TrustDecisionResourceRef{Kind: "RuntimeApproval", Namespace: approval.ObjectMeta.Namespace, Name: approval.ObjectMeta.Name, UID: approval.ObjectMeta.UID, ResourceVersion: fmt.Sprint(approval.ObjectMeta.ResourceVersion)}
	decision := observability.TrustDecisionV1{
		SchemaVersion: 1, ID: uuid.NewString(), Timestamp: now, Plane: "communication", Outcome: outcome, ReasonCode: reasonCode, Workspace: input.namespace, RunID: input.runID,
		Session:       &observability.TrustDecisionSession{SessionID: input.sessionID, CapabilityID: input.capabilityID, InstanceRef: &observability.TrustDecisionResourceRef{Kind: "AgentInstance", Namespace: input.namespace, Name: input.instanceName, UID: input.instanceUID}},
		Request:       observability.TrustDecisionRequest{Operation: input.operation, InvocationID: input.invocationID, TargetRef: observability.TrustDecisionResourceRef{Kind: "AgentService", Namespace: input.targetNamespace, Name: input.targetService}},
		Authorization: authorization,
		Execution:     observability.TrustDecisionExecution{Status: executionStatus, TargetReached: communicationBoolPointer(false)},
		Approval:      &observability.TrustDecisionApproval{Status: approvalStatus, DecisionRef: decisionRef},
	}
	if identity != nil {
		decision.Identity = &observability.TrustDecisionIdentity{AgentIdentity: identity.Spec.SPIFFEID, AgentIdentityRef: &observability.TrustDecisionResourceRef{Kind: "AgentIdentity", Namespace: input.namespace, Name: identity.ObjectMeta.Name, ResourceVersion: fmt.Sprint(identity.ObjectMeta.ResourceVersion), UID: identity.ObjectMeta.UID}}
	} else {
		decision.ClaimedIdentityRef = &observability.TrustDecisionResourceRef{Kind: "AgentIdentity", Namespace: input.namespace, Name: input.identityName}
	}
	details, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("encode communication runtime approval trust decision: %w", err)
	}
	entry := observability.AuditEntry{ID: decision.ID, Timestamp: now, Workspace: input.namespace, Solution: input.solution, SolutionRev: input.solutionRev, SessionID: input.sessionID, EventType: observability.AuditCommunication, SubjectKind: "AgentService", SubjectRef: input.targetNamespace + "/" + input.targetService, Verb: input.operation, Details: details}
	if decision.Identity != nil {
		entry.AgentIdentity = decision.Identity.AgentIdentity
	}
	return s.audit.RecordBeforeExec(ctx, entry)
}
