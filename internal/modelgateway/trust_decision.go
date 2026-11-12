// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

func (s *Server) recordDecision(ctx context.Context, claims *runtimeauth.ModelSessionCapabilityClaims, authorized *AuthorizedModel, identity *v1alpha1.AgentIdentity, runID, outcome, reasonCode, executionStatus string, targetReached, beforeEffect bool) error {
	return s.recordOperationDecision(ctx, claims, authorized, identity, "complete", runID, outcome, reasonCode, executionStatus, targetReached, beforeEffect)
}

func (s *Server) recordOperationDecision(ctx context.Context, claims *runtimeauth.ModelSessionCapabilityClaims, authorized *AuthorizedModel, identity *v1alpha1.AgentIdentity, operation, runID, outcome, reasonCode, executionStatus string, targetReached, beforeEffect bool) error {
	if s.audit == nil {
		return nil
	}
	now := time.Now().UTC()
	decision := observability.TrustDecisionV1{
		SchemaVersion: 1,
		ID:            uuid.NewString(),
		Timestamp:     now,
		Plane:         "model",
		Outcome:       outcome,
		ReasonCode:    reasonCode,
		Workspace:     claims.Namespace,
		RunID:         runID,
		Identity: &observability.TrustDecisionIdentity{
			AgentIdentity:    "unavailable",
			AgentIdentityRef: &observability.TrustDecisionResourceRef{Kind: "AgentIdentity", Namespace: claims.Namespace, Name: claims.AgentIdentityName},
		},
		Session: &observability.TrustDecisionSession{
			SessionID:    claims.SessionID,
			InstanceRef:  &observability.TrustDecisionResourceRef{Kind: "AgentInstance", Namespace: claims.Namespace, Name: claims.InstanceName, UID: claims.InstanceUID},
			CapabilityID: claims.ID,
		},
		Request: observability.TrustDecisionRequest{
			Operation: operation,
			TargetRef: observability.TrustDecisionResourceRef{Kind: "Model", Namespace: claims.Namespace, Name: claims.ModelName, ResourceVersion: fmt.Sprint(claims.ModelResourceVersion)},
		},
		Authorization: observability.TrustDecisionAuthorization{Identity: "allowed", Role: "not_required", Grant: "allowed", Policy: "allowed"},
		Approval:      &observability.TrustDecisionApproval{Status: "not_required"},
		Execution:     observability.TrustDecisionExecution{Status: executionStatus, TargetReached: boolPointer(targetReached)},
	}
	if identity != nil {
		decision.Identity.AgentIdentity = identity.Spec.SPIFFEID
		decision.Identity.AgentIdentityRef.ResourceVersion = fmt.Sprint(identity.ObjectMeta.ResourceVersion)
		decision.Identity.AgentIdentityRef.UID = identity.ObjectMeta.UID
	}
	if authorized != nil && authorized.Model != nil {
		decision.Request.TargetRef.ResourceVersion = fmt.Sprint(authorized.Model.ObjectMeta.ResourceVersion)
		decision.Request.TargetRef.UID = authorized.Model.ObjectMeta.UID
	}
	if outcome == "denied" || outcome == "revoked" {
		decision.Authorization.Policy = "denied"
		if reasonCode == "grant_missing" || reasonCode == "grant_revoked" {
			decision.Authorization.Grant = map[bool]string{true: "revoked", false: "denied"}[reasonCode == "grant_revoked"]
		}
		if reasonCode == "identity_not_ready" {
			decision.Authorization.Identity = "denied"
		}
	}
	details, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("encode model trust decision: %w", err)
	}
	entry := observability.AuditEntry{ID: decision.ID, Timestamp: now, AgentIdentity: decision.Identity.AgentIdentity, Workspace: claims.Namespace, Solution: claims.Solution, SolutionRev: claims.SolutionRev, SessionID: claims.SessionID, EventType: observability.AuditModelCall, SubjectKind: "Model", SubjectRef: claims.ModelName, Verb: operation, Details: details}
	if beforeEffect {
		return s.audit.RecordBeforeExec(ctx, entry)
	}
	return s.audit.Record(ctx, entry)
}

// StandaloneDecisionInput identifies a direct standalone Model operation
// without retaining prompts, provider responses, credentials, or parameters.
type StandaloneDecisionInput struct {
	Agent        *v1alpha1.Agent
	RunID        string
	Workspace    string
	SessionID    string
	InstanceName string
	InstanceUID  string
	ModelName    string
	Model        *v1alpha1.Model
}

// RecordStandaloneDecision persists the same normalized Model decision
// contract used by the Kubernetes gateway for an in-process provider call.
func RecordStandaloneDecision(ctx context.Context, audit *observability.CognitiveAuditLog, input StandaloneDecisionInput, outcome, reasonCode, executionStatus string, targetReached, beforeEffect bool) error {
	if audit == nil {
		return nil
	}
	now := time.Now().UTC()
	authorization := observability.TrustDecisionAuthorization{Identity: "not_required", Role: "not_required", Grant: "allowed", Policy: "allowed"}
	if outcome == "denied" || outcome == "revoked" {
		authorization.Policy = "denied"
		if reasonCode == "grant_missing" || reasonCode == "grant_revoked" {
			authorization.Grant = map[bool]string{true: "revoked", false: "denied"}[reasonCode == "grant_revoked"]
		}
	}
	target := observability.TrustDecisionResourceRef{Kind: "Model", Namespace: input.Workspace, Name: input.ModelName}
	if input.Model != nil {
		if input.Model.ObjectMeta.Name != "" {
			target.Name = input.Model.ObjectMeta.Name
		}
		target.ResourceVersion = fmt.Sprint(input.Model.ObjectMeta.ResourceVersion)
		target.UID = input.Model.ObjectMeta.UID
	}
	decision := observability.TrustDecisionV1{
		SchemaVersion: 1,
		ID:            uuid.NewString(),
		Timestamp:     now,
		Plane:         "model",
		Outcome:       outcome,
		ReasonCode:    reasonCode,
		Workspace:     input.Workspace,
		RunID:         input.RunID,
		Session:       &observability.TrustDecisionSession{SessionID: input.SessionID},
		Request:       observability.TrustDecisionRequest{Operation: "complete", TargetRef: target},
		Authorization: authorization,
		Approval:      &observability.TrustDecisionApproval{Status: "not_required"},
		Execution:     observability.TrustDecisionExecution{Status: executionStatus, TargetReached: boolPointer(targetReached)},
	}
	if input.Agent != nil && input.Agent.Name != "" {
		decision.AgentRef = &observability.TrustDecisionResourceRef{Kind: "Agent", Namespace: input.Workspace, Name: input.Agent.Name, UID: input.Agent.UID, ResourceVersion: fmt.Sprint(input.Agent.ResourceVersion)}
		decision.Session.AuthorityMode = "in_process"
	}
	if input.InstanceName != "" || input.InstanceUID != "" {
		decision.Session.InstanceRef = &observability.TrustDecisionResourceRef{Kind: "AgentInstance", Namespace: input.Workspace, Name: input.InstanceName, UID: input.InstanceUID}
	}
	details, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("encode standalone model trust decision: %w", err)
	}
	entry := observability.AuditEntry{ID: decision.ID, Timestamp: now, Workspace: input.Workspace, SessionID: input.SessionID, EventType: observability.AuditModelCall, SubjectKind: "Model", SubjectRef: target.Name, Verb: "complete", Details: details}
	if beforeEffect {
		return audit.RecordBeforeExec(ctx, entry)
	}
	return audit.Record(ctx, entry)
}

func modelDeniedOutcome(err error) string {
	if modelDecisionReasonCode(err) == "grant_revoked" {
		return "revoked"
	}
	return "denied"
}

// DeniedOutcome maps an authorization error to the normalized denied or
// revoked outcome used by Model trust evidence.
func DeniedOutcome(err error) string { return modelDeniedOutcome(err) }

func modelDecisionReasonCode(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "revoked"), strings.Contains(message, "no longer"), strings.Contains(message, "revision"):
		return "grant_revoked"
	case strings.Contains(message, "identity"), strings.Contains(message, "instance"):
		return "identity_not_ready"
	case strings.Contains(message, "grant"):
		return "grant_missing"
	case strings.Contains(message, "capability"):
		return "capability_invalid"
	default:
		return "policy_denied"
	}
}

// DecisionReasonCode maps an authorization error to the normalized Model
// reason-code vocabulary.
func DecisionReasonCode(err error) string { return modelDecisionReasonCode(err) }

func boolPointer(value bool) *bool { return &value }
