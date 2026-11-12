// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"encoding/hex"
	"time"
)

// CostUnit represents a normalized unit of cost for cross-model comparison.
// 1 CostUnit = 1 micro-dollar (0.000001 USD).
type CostUnit int64

// CostRecord captures the cost of a single operation (model inference or tool call)
// tagged with full attribution context.
type CostRecord struct {
	// ID is the idempotency key for this immutable usage event.
	ID string `json:"id,omitempty"`

	// Attribution context.
	Workspace      string `json:"workspace"`
	Agent          string `json:"agent,omitempty"`
	AgentIdentity  string `json:"agentIdentity"`
	AgentNamespace string `json:"agentNamespace,omitempty"`
	Workflow       string `json:"workflow,omitempty"`
	Step           string `json:"step,omitempty"`
	Solution       string `json:"solution,omitempty"`
	SolutionRev    int64  `json:"solutionRevision,omitempty"`
	SessionID      string `json:"sessionId,omitempty"`
	ReservationID  string `json:"reservationId,omitempty"`

	// What incurred the cost.
	OperationType     CostOperationType `json:"operationType"`
	Purpose           UsagePurpose      `json:"purpose,omitempty"`
	ModelName         string            `json:"modelName,omitempty"`
	ModelProvider     string            `json:"modelProvider,omitempty"`
	ModelClass        string            `json:"modelClass,omitempty"`
	ModelClassVersion string            `json:"modelClassVersion,omitempty"`
	ModelCostTier     string            `json:"modelCostTier,omitempty"`
	ToolName          string            `json:"toolName,omitempty"`

	// Token counts (model inference only).
	PromptTokens     int64 `json:"promptTokens,omitempty"`
	CompletionTokens int64 `json:"completionTokens,omitempty"`
	TotalTokens      int64 `json:"totalTokens,omitempty"`
	UsageReported    bool  `json:"usageReported,omitempty"`
	UsageEstimated   bool  `json:"usageEstimated,omitempty"`

	// Cost.
	PricingConfigured                  bool      `json:"pricingConfigured,omitempty"`
	InputCostMicrousdPerMillionTokens  int64     `json:"inputCostMicrousdPerMillionTokens,omitempty"`
	OutputCostMicrousdPerMillionTokens int64     `json:"outputCostMicrousdPerMillionTokens,omitempty"`
	CostUnits                          CostUnit  `json:"costUnits"`
	ProviderDurationMillis             int64     `json:"providerDurationMillis,omitempty"`
	Timestamp                          time.Time `json:"timestamp"`

	// Trace correlation.
	TraceID string `json:"traceId,omitempty"`
	SpanID  string `json:"spanId,omitempty"`
}

// CostOperationType classifies what operation incurred cost.
type CostOperationType string

const (
	CostOpModelInference CostOperationType = "model_inference"
	CostOpToolExecution  CostOperationType = "tool_execution"
	CostOpMemoryOp       CostOperationType = "memory_operation"
	CostOpEmbedding      CostOperationType = "embedding"
)

// UsagePurpose classifies bounded product workflows without changing cost aggregation.
type UsagePurpose string

const (
	UsagePurposeReasoningProbe UsagePurpose = "reasoning-probe"
	UsagePurposeModelCanary    UsagePurpose = "model-version-canary"
)

// Valid reports whether the purpose is supported or omitted for a legacy record.
func (purpose UsagePurpose) Valid() bool {
	return purpose == "" || purpose == UsagePurposeReasoningProbe || purpose == UsagePurposeModelCanary
}

// CostTagger populates cost attribution fields from a span's attributes.
type CostTagger struct{}

// NewCostTagger creates a new CostTagger.
func NewCostTagger() *CostTagger {
	return &CostTagger{}
}

// TagFromSpan extracts cost attribution metadata from a span's attributes
// and returns a partially populated CostRecord. The caller must still set
// CostUnits and operation-specific fields.
func (t *CostTagger) TagFromSpan(span *Span) CostRecord {
	record := CostRecord{
		Timestamp: span.EndTime,
	}
	record.TraceID = hex.EncodeToString(span.Context.TraceID[:])
	record.SpanID = hex.EncodeToString(span.Context.SpanID[:])
	record.ID = record.TraceID + ":" + record.SpanID
	if record.Timestamp.IsZero() {
		record.Timestamp = span.StartTime
	}
	if !span.EndTime.IsZero() && !span.StartTime.IsZero() && !span.EndTime.Before(span.StartTime) {
		duration := span.EndTime.Sub(span.StartTime)
		record.ProviderDurationMillis = duration.Milliseconds()
		if duration > 0 && record.ProviderDurationMillis == 0 {
			record.ProviderDurationMillis = 1
		}
	}

	attrs := span.Attributes
	if attrs == nil {
		return record
	}

	record.Workspace = stringAttr(attrs, AttrWorkspace)
	record.Agent = stringAttr(attrs, AttrAgentName)
	record.AgentIdentity = stringAttr(attrs, AttrAgentIdentity)
	record.AgentNamespace = stringAttr(attrs, AttrAgentNamespace)
	record.Workflow = stringAttr(attrs, AttrWorkflowName)
	record.Step = stringAttr(attrs, AttrStepName)
	record.Solution = stringAttr(attrs, AttrSolution)
	record.SolutionRev = int64Attr(attrs, AttrSolutionRev)
	record.SessionID = stringAttr(attrs, AttrSessionID)
	record.ReservationID = stringAttr(attrs, AttrReservationID)
	record.ModelName = stringAttr(attrs, AttrModelName)
	record.ModelProvider = stringAttr(attrs, AttrModelProvider)
	record.ModelClass = stringAttr(attrs, AttrModelClass)
	record.ModelClassVersion = stringAttr(attrs, AttrModelClassVersion)
	record.ModelCostTier = stringAttr(attrs, AttrModelCostTier)
	record.PricingConfigured = boolAttr(attrs, AttrModelPricingConfigured)
	record.InputCostMicrousdPerMillionTokens = int64Attr(attrs, AttrModelInputRate)
	record.OutputCostMicrousdPerMillionTokens = int64Attr(attrs, AttrModelOutputRate)
	record.ToolName = stringAttr(attrs, AttrToolName)
	record.PromptTokens = int64Attr(attrs, AttrPromptTokens)
	record.CompletionTokens = int64Attr(attrs, AttrCompletionTokens)
	record.TotalTokens = int64Attr(attrs, AttrTotalTokens)
	record.UsageReported = boolAttr(attrs, AttrModelUsageReported)

	// Infer operation type and extract cost units from span name.
	switch span.Name {
	case SpanNameModelInfer:
		record.OperationType = CostOpModelInference
		record.CostUnits = CostUnit(int64Attr(attrs, AttrModelCostUnits))
	case SpanNameToolCall:
		record.OperationType = CostOpToolExecution
		record.CostUnits = CostUnit(int64Attr(attrs, AttrToolCostUnits))
	}

	return record
}

// stringAttr extracts a string attribute value, returning "" if absent.
func stringAttr(attrs map[string]interface{}, key string) string {
	v, ok := attrs[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// int64Attr extracts an int64 attribute value, returning 0 if absent.
func int64Attr(attrs map[string]interface{}, key string) int64 {
	v, ok := attrs[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func boolAttr(attrs map[string]interface{}, key string) bool {
	value, ok := attrs[key]
	if !ok {
		return false
	}
	result, _ := value.(bool)
	return result
}
