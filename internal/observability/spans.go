// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package observability implements OTel reasoning-lineage tracing, cost attribution,
// CognitiveAuditLog, CloudEvents wire format, and configurable retention for EPIC-0014.
package observability

import "time"

// --- OTel Span Attribute Keys (semantic conventions for cognitive agents) ---

const (
	// Span names.
	SpanNameReasoningStep  = "kognition.reasoning.step"
	SpanNameToolCall       = "kognition.tool.call"
	SpanNameMemoryOp       = "kognition.memory.op"
	SpanNameModelInfer     = "kognition.model.infer"
	SpanNameAgentChat      = "kognition.agent.chat"
	SpanNameWorkflowRun    = "kognition.workflow.run"
	SpanNameWorkflowStep   = "kognition.workflow.step"
	SpanNameReasoningProbe = "kognition.reasoning.probe"

	// Attribute keys: ReasoningProbe canary execution.
	AttrReasoningProbeOperationID = "kognition.reasoning_probe.operation_id"
	AttrAgentInstanceName         = "kognition.agent_instance.name"
	AttrAgentRevision             = "kognition.agent.revision"

	// Attribute keys: Workflow execution (EPIC-0019 US-0083).
	AttrWorkflowName  = "kognition.workflow.name"
	AttrWorkflowRun   = "kognition.workflow.run"
	AttrStepName      = "kognition.workflow.step.name"
	AttrStepType      = "kognition.workflow.step.type"
	AttrStepIteration = "kognition.workflow.step.iteration"

	// Attribute keys: Agent identity.
	AttrAgentName      = "kognition.agent.name"
	AttrAgentNamespace = "kognition.agent.namespace"
	AttrAgentIdentity  = "kognition.agent.identity"

	// Attribute keys: Workspace & Solution context (cost attribution).
	AttrWorkspace     = "kognition.workspace"
	AttrSolution      = "kognition.solution"
	AttrSolutionRev   = "kognition.solution.revision"
	AttrSessionID     = "kognition.session.id"
	AttrReservationID = "kognition.reservation.id"
	AttrInvocationID  = "kognition.invocation.id"

	// Attribute keys: Model inference.
	AttrModelName              = "kognition.model.name"
	AttrModelProvider          = "kognition.model.provider"
	AttrModelClass             = "kognition.model.class"
	AttrModelClassVersion      = "kognition.model.class_version"
	AttrModelCostTier          = "kognition.model.cost_tier"
	AttrModelInputRate         = "kognition.model.input_cost_microusd_per_million_tokens"
	AttrModelOutputRate        = "kognition.model.output_cost_microusd_per_million_tokens"
	AttrModelPricingConfigured = "kognition.model.pricing_configured"
	AttrPromptTokens           = "kognition.model.prompt_tokens"
	AttrCompletionTokens       = "kognition.model.completion_tokens"
	AttrTotalTokens            = "kognition.model.total_tokens"
	AttrModelUsageReported     = "kognition.model.usage_reported"
	AttrModelCostUnits         = "kognition.model.cost_units"

	// Attribute keys: Tool call.
	AttrToolName        = "kognition.tool.name"
	AttrToolDigest      = "kognition.tool.digest"
	AttrToolGrantRef    = "kognition.tool.grant_ref"
	AttrToolSandboxTier = "kognition.tool.sandbox_tier"
	AttrToolArgs        = "kognition.tool.args"   // subject to PII redaction
	AttrToolResult      = "kognition.tool.result" // subject to PII redaction
	AttrToolLatencyMs   = "kognition.tool.latency_ms"
	AttrToolCostUnits   = "kognition.tool.cost_units"

	// Attribute keys: Memory operations.
	AttrMemoryOp    = "kognition.memory.operation" // read, write, summarize, forget
	AttrMemoryKey   = "kognition.memory.key"
	AttrMemoryValue = "kognition.memory.value" // subject to PII redaction

	// Attribute keys: Reasoning step.
	AttrReasoningStepIndex = "kognition.reasoning.step_index"
	AttrReasoningRole      = "kognition.reasoning.role"
	AttrReasoningContent   = "kognition.reasoning.content" // subject to PII redaction

	// Attribute keys: Status.
	AttrStatus       = "kognition.status"
	AttrErrorMessage = "kognition.error.message"
)

// SpanKind classifies the type of operation a span represents.
type SpanKind int

const (
	SpanKindInternal SpanKind = iota
	SpanKindClient            // outgoing call (model inference, tool call)
	SpanKindServer            // incoming request (session step)
)

// TraceID is a 128-bit trace identifier.
type TraceID [16]byte

// SpanID is a 64-bit span identifier.
type SpanID [8]byte

// SpanContext holds the immutable identifiers of a span.
type SpanContext struct {
	TraceID    TraceID
	SpanID     SpanID
	ParentID   SpanID
	TraceFlags byte // 0x01 = sampled
}

// Span represents a single unit of work in the reasoning lineage.
type Span struct {
	// Identity.
	Context   SpanContext
	Name      string
	Kind      SpanKind
	StartTime time.Time
	EndTime   time.Time

	// Semantic attributes (key-value).
	Attributes map[string]interface{}

	// Status.
	StatusCode    StatusCode
	StatusMessage string

	// Links to related spans (e.g. parent reasoning step → child tool call).
	Links []SpanLink

	// Events within the span (e.g. memory reads during a reasoning step).
	Events []SpanEvent
}

// StatusCode represents the span completion status.
type StatusCode int

const (
	StatusUnset StatusCode = iota
	StatusOK
	StatusError
)

// SpanLink references another span for correlation (not parent-child).
type SpanLink struct {
	Context    SpanContext
	Attributes map[string]interface{}
}

// SpanEvent is a timestamped annotation within a span.
type SpanEvent struct {
	Name       string
	Timestamp  time.Time
	Attributes map[string]interface{}
}
