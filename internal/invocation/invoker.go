// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package invocation

import (
	"context"
	"time"
)

// ToolFunc represents the actual tool execution function.
type ToolFunc func(ctx context.Context, args string) (payload string, err error)

// Invoker wraps tool execution with traceability recording.
type Invoker struct {
	Tracer Tracer
}

// InvokeParams holds parameters for a traced tool invocation.
type InvokeParams struct {
	ToolName       string
	ToolDigest     string
	ToolRef        string // namespace/name
	GrantName      string
	GrantRef       string // namespace/grant-name
	GrantVerbs     []string
	Workspace      string
	AgentNamespace string
	AgentIdentity  string
	Solution       string
	SolutionRev    int64
	SessionID      string
	SandboxTier    string
	TraceID        string
	Arguments      string
	CostUnits      int64
	TokenCost      int64
}

// Invoke executes a tool function and records the invocation span and audit entry.
func (inv *Invoker) Invoke(ctx context.Context, params InvokeParams, fn ToolFunc) (string, error) {
	start := time.Now()
	payload, err := fn(ctx, params.Arguments)
	latency := time.Since(start).Milliseconds()

	status := ResultSuccess
	if err != nil {
		status = ResultError
	}
	if ctx.Err() == context.DeadlineExceeded {
		status = ResultTimeout
	}

	span := SpanAttributes{
		Workspace:      params.Workspace,
		AgentNamespace: params.AgentNamespace,
		AgentIdentity:  params.AgentIdentity,
		Solution:       params.Solution,
		SolutionRev:    params.SolutionRev,
		SessionID:      params.SessionID,
		ToolName:       params.ToolName,
		ToolDigest:     params.ToolDigest,
		GrantName:      params.GrantName,
		GrantVerbs:     params.GrantVerbs,
		Arguments:      params.Arguments,
		ResultStatus:   status,
		ResultPayload:  payload,
		SandboxTier:    params.SandboxTier,
		LatencyMs:      latency,
		CostUnits:      params.CostUnits,
	}

	audit := AuditEntry{
		Timestamp:      start,
		Workspace:      params.Workspace,
		AgentNamespace: params.AgentNamespace,
		AgentIdentity:  params.AgentIdentity,
		Solution:       params.Solution,
		SolutionRev:    params.SolutionRev,
		SessionID:      params.SessionID,
		ToolRef:        params.ToolRef,
		ToolDigest:     params.ToolDigest,
		GrantRef:       params.GrantRef,
		Verb:           "invoke",
		Arguments:      params.Arguments,
		Result:         status,
		LatencyMs:      latency,
		TokenCost:      params.TokenCost,
		TraceID:        params.TraceID,
	}

	if inv != nil && inv.Tracer != nil {
		inv.Tracer.RecordInvocation(ctx, span, audit)
	}
	return payload, err
}
