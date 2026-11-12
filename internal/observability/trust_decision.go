// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	TrustDecisionRunIDHeader    = "X-Kognition-Run-ID"
	MaxTrustDecisionRunIDLength = 128
)

func NormalizeTrustDecisionRunID(value string) (string, error) {
	runID := strings.TrimSpace(value)
	if utf8.RuneCountInString(runID) > MaxTrustDecisionRunIDLength {
		return "", fmt.Errorf("trust decision run ID exceeds %d characters", MaxTrustDecisionRunIDLength)
	}
	return runID, nil
}

// TrustDecisionV1 is the normalized redacted audit payload defined by
// api/schemas/trust-decision-v1.schema.json.
type TrustDecisionV1 struct {
	SchemaVersion      int                        `json:"schemaVersion"`
	ID                 string                     `json:"id"`
	Timestamp          time.Time                  `json:"timestamp"`
	Plane              string                     `json:"plane"`
	Outcome            string                     `json:"outcome"`
	ReasonCode         string                     `json:"reasonCode"`
	Workspace          string                     `json:"workspace"`
	RunID              string                     `json:"runId,omitempty"`
	Reason             string                     `json:"reason,omitempty"`
	Identity           *TrustDecisionIdentity     `json:"identity,omitempty"`
	AgentRef           *TrustDecisionResourceRef  `json:"agentRef,omitempty"`
	ClaimedIdentityRef *TrustDecisionResourceRef  `json:"claimedIdentityRef,omitempty"`
	Session            *TrustDecisionSession      `json:"session,omitempty"`
	Request            TrustDecisionRequest       `json:"request"`
	Authorization      TrustDecisionAuthorization `json:"authorization"`
	Approval           *TrustDecisionApproval     `json:"approval,omitempty"`
	Execution          TrustDecisionExecution     `json:"execution"`
	Trace              *TrustDecisionTrace        `json:"trace,omitempty"`
}

// TrustDecisionResourceRef identifies a durable Kognition resource without
// serializing its complete spec or sensitive values.
type TrustDecisionResourceRef struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	UID             string `json:"uid,omitempty"`
}

type TrustDecisionIdentity struct {
	AgentIdentity    string                    `json:"agentIdentity"`
	AgentIdentityRef *TrustDecisionResourceRef `json:"agentIdentityRef,omitempty"`
}

type TrustDecisionSession struct {
	AuthorityMode string                    `json:"authorityMode,omitempty"`
	SessionID     string                    `json:"sessionId"`
	InstanceRef   *TrustDecisionResourceRef `json:"instanceRef,omitempty"`
	CapabilityID  string                    `json:"capabilityId,omitempty"`
}

type TrustDecisionRequest struct {
	Operation     string                    `json:"operation"`
	InvocationID  string                    `json:"invocationId,omitempty"`
	TargetRef     TrustDecisionResourceRef  `json:"targetRef"`
	ConstraintRef *TrustDecisionResourceRef `json:"constraintRef,omitempty"`
}

type TrustDecisionAuthorization struct {
	Identity  string                    `json:"identity"`
	Role      string                    `json:"role"`
	Grant     string                    `json:"grant"`
	Policy    string                    `json:"policy"`
	GrantRef  *TrustDecisionResourceRef `json:"grantRef,omitempty"`
	PolicyRef *TrustDecisionResourceRef `json:"policyRef,omitempty"`
}

type TrustDecisionApproval struct {
	Status      string                    `json:"status"`
	DecisionRef *TrustDecisionResourceRef `json:"decisionRef,omitempty"`
}

type TrustDecisionExecution struct {
	Status        string `json:"status"`
	TargetReached *bool  `json:"targetReached,omitempty"`
}

type TrustDecisionTrace struct {
	TraceID string `json:"traceId,omitempty"`
	SpanID  string `json:"spanId,omitempty"`
}
