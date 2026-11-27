// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package externaltransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/kognition-ai/kognition/internal/observability"
)

const (
	auditOperationInvoke = "invoke"
	auditOperationCancel = "cancel"
	maxAuditReasonLength = 512
)

// Decision is one audited contact decision. It never carries a credential, a
// request or a reply: only identities, the outcome and a fixed reason.
type Decision struct {
	Namespace string
	// Session is the Workflow or WorkflowRun that owns the collaboration step.
	Session  string
	Agent    string
	Dispatch string
	// Operation is invoke or cancel.
	Operation  string
	Outcome    string
	ReasonCode string
	Reason     string
	Grant      string
	// Execution is not_started, completed or failed.
	Execution     string
	TargetReached bool
	// BeforeEffect makes the record write-ahead: the caller must not contact
	// the remote agent unless Record returned nil.
	BeforeEffect bool
}

// Auditor writes Communication-plane Trust Decisions for external agent contact.
type Auditor struct {
	Log *observability.CognitiveAuditLog
}

// Record writes decision. A write-ahead record returns only once it is durable.
func (a *Auditor) Record(ctx context.Context, d Decision) error {
	if a == nil || a.Log == nil {
		return errors.New("external agent audit log is not configured")
	}
	now := time.Now().UTC()
	authorization := observability.TrustDecisionAuthorization{Identity: "not_evaluated", Role: "not_required", Grant: "allowed", Policy: "allowed"}
	if d.Grant != "" {
		authorization.GrantRef = &observability.TrustDecisionResourceRef{Kind: kindExternalAgentGrant, Namespace: d.Namespace, Name: d.Grant}
	}
	switch d.ReasonCode {
	case reasonNoGrant:
		authorization.Grant, authorization.Policy = "denied", "not_evaluated"
	case reasonRevoked:
		authorization.Grant, authorization.Policy = "revoked", "not_evaluated"
	case reasonPolicy:
		authorization.Grant, authorization.Policy = "not_evaluated", "denied"
	}
	reached := d.TargetReached
	decision := observability.TrustDecisionV1{
		SchemaVersion: 1, ID: uuid.NewString(), Timestamp: now, Plane: "communication",
		Outcome: d.Outcome, ReasonCode: d.ReasonCode, Workspace: d.Namespace, Reason: truncate(d.Reason, maxAuditReasonLength),
		Session: &observability.TrustDecisionSession{SessionID: d.Session},
		Request: observability.TrustDecisionRequest{
			Operation: d.Operation, InvocationID: d.Dispatch,
			TargetRef: observability.TrustDecisionResourceRef{Kind: kindExternalAgent, Namespace: d.Namespace, Name: d.Agent},
		},
		Authorization: authorization,
		Execution:     observability.TrustDecisionExecution{Status: d.Execution, TargetReached: &reached},
	}
	details, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("encode external agent trust decision: %w", err)
	}
	entry := observability.AuditEntry{
		ID: decision.ID, Timestamp: now, Workspace: d.Namespace, SessionID: d.Session,
		EventType: observability.AuditCommunication, SubjectKind: kindExternalAgent, SubjectRef: d.Namespace + "/" + d.Agent,
		Verb: d.Operation, Details: details,
	}
	if d.BeforeEffect {
		return a.Log.RecordBeforeExec(ctx, entry)
	}
	return a.Log.Record(ctx, entry)
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max])
}
