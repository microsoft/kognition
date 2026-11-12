// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

// StoreLedger persists Tool Gateway lifecycle events through the common Store.
// PostgreSQL stores them durably; the Kubernetes adapter persists them through
// Kubernetes Events. Both records attach to the invocation subject.
type StoreLedger struct {
	store    store.Store
	auditLog *observability.CognitiveAuditLog
}

// NewStoreLedger creates a durable ledger over the shared Store contract.
func NewStoreLedger(st store.Store) *StoreLedger {
	ledger := &StoreLedger{store: st}
	if auditStore, ok := st.(observability.AuditStore); ok {
		ledger.auditLog = observability.NewCognitiveAuditLog(auditStore)
	}
	return ledger
}

// NewStoreLedgerWithAudit creates a ledger whose resource and audit stores are
// separate, as they are in Kubernetes mode.
func NewStoreLedgerWithAudit(st store.Store, auditLog *observability.CognitiveAuditLog) *StoreLedger {
	return &StoreLedger{store: st, auditLog: auditLog}
}

func (l *StoreLedger) RecordStart(ctx context.Context, entry LedgerEntry) error {
	if err := l.appendAudit(ctx, entry, "authorized", true); err != nil {
		return err
	}
	return l.append(ctx, entry, "ToolInvocationAuthorized", "tool invocation authorized before execution")
}

func (l *StoreLedger) RecordCompletion(ctx context.Context, entry LedgerEntry) error {
	if err := l.appendAudit(ctx, entry, entry.Status, false); err != nil {
		return err
	}
	return l.append(ctx, entry, "ToolInvocationCompleted", "tool invocation completed with status "+entry.Status)
}

// RecordDecision records a normalized deny, replay, or quota decision that
// does not have a separate lifecycle start/completion pair.
func (l *StoreLedger) RecordDecision(ctx context.Context, entry LedgerEntry) error {
	return l.appendAudit(ctx, entry, entry.Status, true)
}

func (l *StoreLedger) appendAudit(ctx context.Context, entry LedgerEntry, phase string, writeAhead bool) error {
	if l.auditLog == nil {
		return nil
	}
	auditID := uuid.NewString()
	details, err := json.Marshal(toolTrustDecision(auditID, time.Now().UTC(), entry, phase))
	if err != nil {
		return fmt.Errorf("marshal tool audit details: %w", err)
	}
	auditEntry := observability.AuditEntry{
		ID:            auditID,
		Workspace:     entry.Namespace,
		AgentIdentity: entry.AgentIdentity,
		Solution:      entry.Solution,
		SolutionRev:   entry.SolutionRev,
		SessionID:     entry.SessionID,
		EventType:     observability.AuditToolCall,
		SubjectKind:   entry.SubjectKind,
		SubjectRef:    entry.Namespace + "/" + entry.SubjectName,
		Verb:          entry.Verb,
		Details:       details,
	}
	if writeAhead {
		return l.auditLog.RecordBeforeExec(ctx, auditEntry)
	}
	return l.auditLog.Record(ctx, auditEntry)
}

func toolTrustDecision(id string, timestamp time.Time, entry LedgerEntry, phase string) observability.TrustDecisionV1 {
	outcome, reasonCode, executionStatus := "allowed", "authorized", "not_started"
	targetReached := false
	authorization := observability.TrustDecisionAuthorization{Identity: "allowed", Role: "allowed", Grant: "allowed", Policy: "allowed"}
	switch phase {
	case "replayed":
		outcome, reasonCode, executionStatus = "replayed", "replay_detected", "not_started"
		authorization = observability.TrustDecisionAuthorization{Identity: "not_evaluated", Role: "not_evaluated", Grant: "not_evaluated", Policy: "not_evaluated"}
	case "denied":
		outcome, reasonCode, executionStatus = "denied", "policy_denied", "blocked"
		authorization = observability.TrustDecisionAuthorization{Identity: "not_evaluated", Role: "denied", Grant: "denied", Policy: "denied"}
	case "revoked":
		outcome, reasonCode, executionStatus = "revoked", "grant_revoked", "blocked"
		authorization = observability.TrustDecisionAuthorization{Identity: "allowed", Role: "allowed", Grant: "revoked", Policy: "not_evaluated"}
	case "quota_denied":
		outcome, reasonCode, executionStatus = "denied", "quota_exhausted", "blocked"
	case "runtime_approval_pending":
		outcome, reasonCode, executionStatus = "pending", "runtime_approval_pending", "blocked"
	case "success":
		outcome, reasonCode, executionStatus, targetReached = "completed", "target_completed", "completed", true
	case "error", "timeout":
		outcome, reasonCode, executionStatus, targetReached = "failed", "target_failed", "failed", true
	}
	if entry.ReasonCode != "" {
		reasonCode = entry.ReasonCode
	}
	if phase == "denied" {
		authorization = deniedToolAuthorization(reasonCode, entry)
	}
	approval := entry.Approval
	if approval == nil {
		approval = &observability.TrustDecisionApproval{Status: "unavailable"}
	}
	if strings.HasPrefix(reasonCode, "runtime_approval_") {
		authorization = observability.TrustDecisionAuthorization{Identity: "allowed", Role: "allowed", Grant: "allowed", Policy: "allowed"}
	}
	target := observability.TrustDecisionResourceRef{Kind: "Tool", Namespace: entry.Namespace, Name: toolName(entry.ToolRef), ResourceVersion: strconv.FormatInt(entry.ToolVersion, 10)}
	if entry.GrantRef != "" {
		authorization.GrantRef = toolResourceRef("ToolGrant", entry.Namespace, entry.GrantRef)
	}
	if len(entry.PolicyRefs) > 0 {
		authorization.PolicyRef = toolResourceRef("AgentPolicy", entry.Namespace, entry.PolicyRefs[0])
	}
	decision := observability.TrustDecisionV1{
		SchemaVersion: 1, ID: id, Timestamp: timestamp, Plane: "tool", Outcome: outcome, ReasonCode: reasonCode, Workspace: entry.Namespace, RunID: entry.RunID,
		Request:       observability.TrustDecisionRequest{Operation: entry.Verb, InvocationID: entry.InvocationID, TargetRef: target},
		Authorization: authorization, Approval: approval,
		Execution: observability.TrustDecisionExecution{Status: executionStatus, TargetReached: &targetReached},
	}
	if entry.AgentIdentity != "" {
		decision.Identity = &observability.TrustDecisionIdentity{AgentIdentity: entry.AgentIdentity}
	}
	if entry.SessionID != "" {
		decision.Session = &observability.TrustDecisionSession{SessionID: entry.SessionID, CapabilityID: entry.CapabilityID, InstanceRef: &observability.TrustDecisionResourceRef{Kind: entry.SubjectKind, Namespace: entry.Namespace, Name: entry.SubjectName}}
	}
	return decision
}

func deniedToolAuthorization(reasonCode string, entry LedgerEntry) observability.TrustDecisionAuthorization {
	authorization := observability.TrustDecisionAuthorization{Identity: "not_evaluated", Role: "not_evaluated", Grant: "not_evaluated", Policy: "not_evaluated"}
	switch reasonCode {
	case "argument_constraints_denied", "grant_missing":
		authorization.Grant = "denied"
	case "role_denied":
		authorization.Identity = "allowed"
		authorization.Role = "denied"
		if entry.GrantRef != "" {
			authorization.Grant = "allowed"
		}
		if len(entry.PolicyRefs) > 0 {
			authorization.Policy = "allowed"
		}
	case "capability_invalid":
		authorization.Identity = "denied"
	default:
		authorization.Policy = "denied"
		if entry.GrantRef != "" {
			authorization.Grant = "allowed"
		}
	}
	return authorization
}

func toolResourceRef(kind, namespace, reference string) *observability.TrustDecisionResourceRef {
	name := toolName(reference)
	if name == "" {
		return nil
	}
	return &observability.TrustDecisionResourceRef{Kind: kind, Namespace: namespace, Name: name}
}

func toolName(reference string) string {
	name := reference
	if _, suffix, found := strings.Cut(reference, "/"); found {
		name = suffix
	}
	if name, _, found := strings.Cut(name, "@"); found {
		return name
	}
	return name
}

func (l *StoreLedger) append(ctx context.Context, entry LedgerEntry, reason, message string) error {
	if l == nil || l.store == nil {
		return fmt.Errorf("tool invocation ledger store is required")
	}
	if entry.Namespace == "" || entry.SubjectKind == "" || entry.SubjectName == "" || entry.InvocationID == "" {
		return fmt.Errorf("tool invocation ledger entry is incomplete")
	}
	details, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal tool invocation ledger entry: %w", err)
	}
	return l.store.AppendEvent(ctx, entry.SubjectKind, entry.Namespace, entry.SubjectName, store.InstanceEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Type: "Normal", Reason: reason, Message: message, Details: details,
	})
}
