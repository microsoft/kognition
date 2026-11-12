// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"time"
)

// CostAggregation represents the aggregated cost for a grouping key.
type CostAggregation struct {
	// Grouping keys (populated based on GroupBy).
	Workspace     string `json:"workspace,omitempty"`
	AgentIdentity string `json:"agentIdentity,omitempty"`
	Solution      string `json:"solution,omitempty"`
	SolutionRev   int64  `json:"solutionRevision,omitempty"`
	ModelName     string `json:"modelName,omitempty"`
	OperationType string `json:"operationType,omitempty"`

	// Aggregated metrics.
	TotalCostUnits        CostUnit `json:"totalCostUnits"`
	TotalPromptTokens     int64    `json:"totalPromptTokens"`
	TotalCompletionTokens int64    `json:"totalCompletionTokens"`
	TotalOperations       int64    `json:"totalOperations"`

	// Time range.
	PeriodStart time.Time `json:"periodStart"`
	PeriodEnd   time.Time `json:"periodEnd"`
}

// CostGroupBy specifies how to aggregate cost records.
type CostGroupBy string

const (
	GroupByWorkspace     CostGroupBy = "workspace"
	GroupByAgent         CostGroupBy = "agent"
	GroupBySolution      CostGroupBy = "solution"
	GroupByModel         CostGroupBy = "model"
	GroupByOperationType CostGroupBy = "operation_type"
)

// CostQuery defines the parameters for querying aggregated costs.
type CostQuery struct {
	// Filter criteria.
	Workspace     string
	AgentIdentity string
	Solution      string
	SolutionRev   *int64

	// Time range.
	Start time.Time
	End   time.Time

	// Aggregation.
	GroupBy []CostGroupBy

	// Pagination.
	Limit  int
	Offset int
}

// CostQueryResult is the response from a cost aggregation query.
type CostQueryResult struct {
	Aggregations []CostAggregation `json:"aggregations"`
	TotalCount   int64             `json:"totalCount"`
}

// CostStore defines the persistence interface for cost records and aggregation queries.
type CostStore interface {
	// RecordCost persists a cost record.
	RecordCost(ctx context.Context, record CostRecord) error

	// RecordCostBatch persists multiple cost records atomically.
	RecordCostBatch(ctx context.Context, records []CostRecord) error

	// QueryCosts returns aggregated cost data based on the query parameters.
	QueryCosts(ctx context.Context, query CostQuery) (*CostQueryResult, error)

	// GetTotalCost returns the total cost for a workspace within a time range.
	GetTotalCost(ctx context.Context, workspace string, start, end time.Time) (CostUnit, error)
}

// UsageEventWriter persists immutable model usage and applies its cost and
// model-tier duration to current quota projections.
type UsageEventWriter interface {
	// ApplyUsageEvent returns true only when the event was newly applied.
	ApplyUsageEvent(ctx context.Context, event CostRecord) (bool, error)
}

// UsageEventApplyQueryStore persists and enumerates immutable model usage
// events for retained telemetry projections.
type UsageEventApplyQueryStore interface {
	UsageEventWriter
	QueryUsageEvents(ctx context.Context) ([]CostRecord, error)
}

// UsageEventStore combines reporting queries with immutable usage writes.
type UsageEventStore interface {
	CostStore
	UsageEventApplyQueryStore
}
