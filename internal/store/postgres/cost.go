// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	defaultCostQueryLimit = 100
	maxCostQueryLimit     = 1000
)

var _ observability.CostStore = (*Store)(nil)
var _ observability.UsageEventStore = (*Store)(nil)

func (storage *Store) QueryUsageEvents(ctx context.Context) ([]observability.CostRecord, error) {
	rows, err := storage.pool.Query(ctx, `SELECT
		cost_records.created_at, workspace, agent, agent_identity, agent_namespace, workflow, step,
		solution, solution_rev, session_id, operation_type, model_name, model_provider,
		tool_name, prompt_tokens, completion_tokens, total_tokens, cost_units, trace_id, span_id,
		cost_records.event_id, reservation_id, model_class, model_class_version, model_cost_tier,
		pricing_configured, input_cost_microusd_per_million_tokens, output_cost_microusd_per_million_tokens,
		provider_duration_ms, usage_purpose, usage_reported, usage_estimated,
		cost_records.event_fingerprint, COALESCE(receipts.event_fingerprint, '')
		FROM cost_records LEFT JOIN usage_event_receipts AS receipts ON receipts.event_id = cost_records.event_id
		WHERE cost_records.event_id <> '' ORDER BY cost_records.created_at, cost_records.event_id`)
	if err != nil {
		return nil, fmt.Errorf("query retained usage events: %w", err)
	}
	defer rows.Close()
	records := make([]observability.CostRecord, 0)
	for rows.Next() {
		var record observability.CostRecord
		var fingerprint, receiptFingerprint string
		if err := rows.Scan(
			&record.Timestamp, &record.Workspace, &record.Agent, &record.AgentIdentity, &record.AgentNamespace, &record.Workflow, &record.Step,
			&record.Solution, &record.SolutionRev, &record.SessionID, &record.OperationType, &record.ModelName, &record.ModelProvider,
			&record.ToolName, &record.PromptTokens, &record.CompletionTokens, &record.TotalTokens, &record.CostUnits, &record.TraceID, &record.SpanID,
			&record.ID, &record.ReservationID, &record.ModelClass, &record.ModelClassVersion, &record.ModelCostTier,
			&record.PricingConfigured, &record.InputCostMicrousdPerMillionTokens, &record.OutputCostMicrousdPerMillionTokens,
			&record.ProviderDurationMillis, &record.Purpose, &record.UsageReported, &record.UsageEstimated,
			&fingerprint, &receiptFingerprint,
		); err != nil {
			return nil, fmt.Errorf("scan retained usage event: %w", err)
		}
		if fingerprint == "" || fingerprint != receiptFingerprint || record.OperationType != observability.CostOpModelInference {
			return nil, fmt.Errorf("invalid retained usage event receipt %q", record.ID)
		}
		if err := validateCostRecord(&record); err != nil {
			return nil, fmt.Errorf("validate retained usage event %q: %w", record.ID, err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate retained usage events: %w", err)
	}
	return records, nil
}

// ApplyUsageEvent atomically persists one immutable model usage event and
// projects its cost and provider duration into every matching quota.
func (s *Store) ApplyUsageEvent(ctx context.Context, event observability.CostRecord) (bool, error) {
	if event.OperationType != observability.CostOpModelInference {
		return false, fmt.Errorf("usage event requires model inference operation type")
	}
	if err := validateCostRecord(&event); err != nil {
		return false, err
	}
	if event.ID == "" {
		return false, fmt.Errorf("usage event requires an idempotency ID")
	}
	fingerprint, err := usageEventFingerprint(event)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin usage event: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	receiptResult, err := tx.Exec(ctx, `INSERT INTO usage_event_receipts (event_id, event_fingerprint) VALUES ($1, $2) ON CONFLICT DO NOTHING`, event.ID, fingerprint)
	if err != nil {
		return false, fmt.Errorf("record usage event receipt: %w", err)
	}
	if receiptResult.RowsAffected() == 0 {
		var storedFingerprint string
		if err := tx.QueryRow(ctx, `SELECT event_fingerprint FROM usage_event_receipts WHERE event_id = $1`, event.ID).Scan(&storedFingerprint); err != nil {
			return false, fmt.Errorf("load duplicate usage event receipt: %w", err)
		}
		if storedFingerprint != fingerprint {
			return false, fmt.Errorf("usage event ID %q was reused with different content", event.ID)
		}
		return false, nil
	}
	result, err := tx.Exec(ctx, costRecordInsert, costRecordArguments(event, fingerprint)...)
	if err != nil {
		return false, fmt.Errorf("record usage event: %w", err)
	}
	if result.RowsAffected() == 0 {
		return false, fmt.Errorf("usage event conflicts with an existing trace/span")
	}
	quotas, err := lockCostQuotas(ctx, tx, event.Workspace)
	if err != nil {
		return false, err
	}
	for index := range quotas {
		periodStart, err := quotaPeriodStart(&quotas[index].quota)
		if err != nil {
			return false, fmt.Errorf("resolve period for CognitiveQuota %q: %w", quotas[index].name, err)
		}
		tierSeconds, tierMilliseconds, err := projectedModelTierUsage(ctx, tx, event.Workspace, periodStart)
		if err != nil {
			return false, fmt.Errorf("project model-tier usage for CognitiveQuota %q: %w", quotas[index].name, err)
		}
		if err := applyUsageEventToQuota(&quotas[index].quota, event, periodStart, tierSeconds, tierMilliseconds); err != nil {
			return false, fmt.Errorf("apply usage event to CognitiveQuota %q: %w", quotas[index].name, err)
		}
	}
	if err := persistLockedCostQuotas(ctx, tx, quotas); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit usage event: %w", err)
	}
	return true, nil
}

// RecordCost persists one normalized cost record. Trace-correlated records are
// idempotent, so exporter retries do not double charge an operation.
func (s *Store) RecordCost(ctx context.Context, record observability.CostRecord) error {
	hasEventID := record.ID != ""
	if err := validateCostRecord(&record); err != nil {
		return err
	}
	if record.OperationType == observability.CostOpModelInference && hasEventID {
		return fmt.Errorf("model usage events with an ID require ApplyUsageEvent")
	}
	if _, err := s.pool.Exec(ctx, costRecordInsert, costRecordArguments(record, "")...); err != nil {
		return fmt.Errorf("record cost: %w", err)
	}
	return nil
}

// RecordCostBatch persists cost records atomically.
func (s *Store) RecordCostBatch(ctx context.Context, records []observability.CostRecord) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin cost batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for index := range records {
		hasEventID := records[index].ID != ""
		if err := validateCostRecord(&records[index]); err != nil {
			return fmt.Errorf("cost record %d: %w", index, err)
		}
		if records[index].OperationType == observability.CostOpModelInference && hasEventID {
			return fmt.Errorf("cost record %d: model usage events with an ID require ApplyUsageEvent", index)
		}
		if _, err := tx.Exec(ctx, costRecordInsert, costRecordArguments(records[index], "")...); err != nil {
			return fmt.Errorf("record cost %d: %w", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit cost batch: %w", err)
	}
	return nil
}

// QueryCosts returns cost records aggregated over the requested dimensions.
func (s *Store) QueryCosts(ctx context.Context, query observability.CostQuery) (*observability.CostQueryResult, error) {
	groups, err := normalizeCostGroups(query.GroupBy)
	if err != nil {
		return nil, err
	}
	if query.Limit < 0 || query.Offset < 0 {
		return nil, fmt.Errorf("cost query limit and offset must be non-negative")
	}
	limit := query.Limit
	if limit == 0 {
		limit = defaultCostQueryLimit
	}
	if limit > maxCostQueryLimit {
		limit = maxCostQueryLimit
	}
	clauses, arguments := costFilterClauses(query)
	querySQL := `SELECT ` + costGroupSelect(groups) + `,
        SUM(cost_units), SUM(prompt_tokens), SUM(completion_tokens), COUNT(*),
        MIN(created_at), MAX(created_at), COUNT(*) OVER()
        FROM cost_records`
	if len(clauses) > 0 {
		querySQL += " WHERE " + strings.Join(clauses, " AND ")
	}
	groupColumns := costGroupColumns(groups)
	querySQL += " GROUP BY " + strings.Join(groupColumns, ", ")
	querySQL += " ORDER BY " + strings.Join(groupColumns, ", ")
	arguments = append(arguments, limit, query.Offset)
	querySQL += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(arguments)-1, len(arguments))

	rows, err := s.pool.Query(ctx, querySQL, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query costs: %w", err)
	}
	defer rows.Close()
	result := &observability.CostQueryResult{Aggregations: make([]observability.CostAggregation, 0)}
	for rows.Next() {
		var aggregation observability.CostAggregation
		var totalCost int64
		if err := rows.Scan(&aggregation.Workspace, &aggregation.AgentIdentity, &aggregation.Solution, &aggregation.SolutionRev, &aggregation.ModelName, &aggregation.OperationType,
			&totalCost, &aggregation.TotalPromptTokens, &aggregation.TotalCompletionTokens, &aggregation.TotalOperations,
			&aggregation.PeriodStart, &aggregation.PeriodEnd, &result.TotalCount); err != nil {
			return nil, fmt.Errorf("scan cost aggregation: %w", err)
		}
		aggregation.TotalCostUnits = observability.CostUnit(totalCost)
		result.Aggregations = append(result.Aggregations, aggregation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cost aggregations: %w", err)
	}
	return result, nil
}

// GetTotalCost returns the total normalized cost for a workspace and time range.
func (s *Store) GetTotalCost(ctx context.Context, workspace string, start, end time.Time) (observability.CostUnit, error) {
	if workspace == "" {
		return 0, fmt.Errorf("cost total requires workspace")
	}
	var total int64
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(cost_units), 0) FROM cost_records WHERE workspace = $1 AND ($2::timestamptz IS NULL OR created_at >= $2) AND ($3::timestamptz IS NULL OR created_at <= $3)`, workspace, nullableTime(start), nullableTime(end)).Scan(&total); err != nil {
		return 0, fmt.Errorf("get total cost: %w", err)
	}
	return observability.CostUnit(total), nil
}

// DeleteCostRecordsBefore deletes expired cost records for one workspace.
func (s *Store) DeleteCostRecordsBefore(ctx context.Context, workspace string, before time.Time) (int64, error) {
	if workspace == "" || before.IsZero() {
		return 0, fmt.Errorf("cost retention requires workspace and cutoff")
	}
	periodStart, err := s.activeCostPeriodStart(ctx, workspace)
	if err != nil {
		return 0, err
	}
	if !periodStart.IsZero() && periodStart.Before(before) {
		before = periodStart
	}
	result, err := s.pool.Exec(ctx, `DELETE FROM cost_records WHERE workspace = $1 AND created_at < $2`, workspace, before)
	if err != nil {
		return 0, fmt.Errorf("delete expired cost records: %w", err)
	}
	return result.RowsAffected(), nil
}

func (s *Store) activeCostPeriodStart(ctx context.Context, workspace string) (time.Time, error) {
	items, err := s.List(ctx, cognitiveQuotaKind, store.ListOptions{Namespace: workspace})
	if err != nil {
		return time.Time{}, fmt.Errorf("list CognitiveQuotas for cost retention: %w", err)
	}
	var earliest time.Time
	for index := range items {
		var quota v1alpha1.CognitiveQuota
		if err := json.Unmarshal(items[index], &quota); err != nil {
			return time.Time{}, fmt.Errorf("decode CognitiveQuota for cost retention: %w", err)
		}
		if quota.Spec.CostBudget == nil {
			continue
		}
		start, err := quotaPeriodStart(&quota)
		if err != nil {
			return time.Time{}, fmt.Errorf("resolve CognitiveQuota period for cost retention: %w", err)
		}
		if earliest.IsZero() || start.Before(earliest) {
			earliest = start
		}
	}
	return earliest, nil
}

const costRecordInsert = `INSERT INTO cost_records
	(created_at, workspace, agent, agent_identity, agent_namespace, workflow, step, solution, solution_rev, session_id, operation_type, model_name, model_provider, tool_name, prompt_tokens, completion_tokens, total_tokens, cost_units, trace_id, span_id,
	 event_id, event_fingerprint, reservation_id, model_class, model_class_version, model_cost_tier, pricing_configured, input_cost_microusd_per_million_tokens, output_cost_microusd_per_million_tokens, provider_duration_ms, usage_purpose, usage_reported, usage_estimated)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33)
    ON CONFLICT DO NOTHING`

func validateCostRecord(record *observability.CostRecord) error {
	if record.Workspace == "" || record.OperationType == "" {
		return fmt.Errorf("cost record requires workspace and operation type")
	}
	if record.PromptTokens < 0 || record.CompletionTokens < 0 || record.TotalTokens < 0 || record.CostUnits < 0 || record.InputCostMicrousdPerMillionTokens < 0 || record.OutputCostMicrousdPerMillionTokens < 0 || record.ProviderDurationMillis < 0 {
		return fmt.Errorf("cost record values must be non-negative")
	}
	if record.OperationType == observability.CostOpModelInference && record.TotalTokens != record.PromptTokens+record.CompletionTokens {
		return fmt.Errorf("model cost record total tokens must equal prompt plus completion tokens")
	}
	if !record.Purpose.Valid() {
		return fmt.Errorf("unsupported usage purpose %q", record.Purpose)
	}
	if record.ID == "" && record.TraceID != "" && record.SpanID != "" {
		record.ID = record.TraceID + ":" + record.SpanID
	}
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now().UTC()
	}
	return nil
}

func costRecordArguments(record observability.CostRecord, fingerprint string) []any {
	eventID := ""
	if fingerprint != "" {
		eventID = record.ID
	}
	return []any{record.Timestamp, record.Workspace, record.Agent, record.AgentIdentity, record.AgentNamespace, record.Workflow, record.Step, record.Solution, record.SolutionRev, record.SessionID, record.OperationType, record.ModelName, record.ModelProvider, record.ToolName, record.PromptTokens, record.CompletionTokens, record.TotalTokens, record.CostUnits, record.TraceID, record.SpanID,
		eventID, fingerprint, record.ReservationID, record.ModelClass, record.ModelClassVersion, record.ModelCostTier, record.PricingConfigured, record.InputCostMicrousdPerMillionTokens, record.OutputCostMicrousdPerMillionTokens, record.ProviderDurationMillis, record.Purpose, record.UsageReported, record.UsageEstimated}
}

func usageEventFingerprint(event observability.CostRecord) (string, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return "", fmt.Errorf("marshal usage event fingerprint: %w", err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func lockCostQuotas(ctx context.Context, tx pgx.Tx, namespace string) ([]lockedTokenQuota, error) {
	rows, err := tx.Query(ctx, `SELECT name, id::text, data, resource_version FROM resources
		WHERE kind = $1 AND namespace = $2 AND deleted_at IS NULL ORDER BY name FOR UPDATE`, cognitiveQuotaKind, namespace)
	if err != nil {
		return nil, fmt.Errorf("lock cost CognitiveQuotas: %w", err)
	}
	defer rows.Close()
	var quotas []lockedTokenQuota
	for rows.Next() {
		var item lockedTokenQuota
		var data []byte
		if err := rows.Scan(&item.name, &item.resourceID, &data, &item.resourceVersion); err != nil {
			return nil, fmt.Errorf("scan cost CognitiveQuota: %w", err)
		}
		if err := json.Unmarshal(data, &item.quota); err != nil {
			return nil, fmt.Errorf("unmarshal CognitiveQuota %q: %w", item.name, err)
		}
		if item.quota.Spec.CostBudget != nil {
			quotas = append(quotas, item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list locked cost CognitiveQuotas: %w", err)
	}
	return quotas, nil
}

func persistLockedCostQuotas(ctx context.Context, tx pgx.Tx, quotas []lockedTokenQuota) error {
	return persistLockedTokenQuotas(ctx, tx, quotas)
}

func applyUsageEventToQuota(quota *v1alpha1.CognitiveQuota, event observability.CostRecord, periodStart time.Time, tierSeconds, tierMilliseconds map[string]int64) error {
	if !periodStart.IsZero() && event.Timestamp.Before(periodStart) {
		return nil
	}
	if quota.Status.CostUsage == nil {
		quota.Status.CostUsage = &v1alpha1.QuotaCostUsage{}
	}
	usage := quota.Status.CostUsage
	var err error
	usage.TotalCost, err = checkedTokenDelta(usage.TotalCost, int64(event.CostUnits))
	if err != nil {
		return err
	}
	usage.ModelTierSeconds = tierSeconds
	usage.ModelTierMilliseconds = tierMilliseconds
	return nil
}

func quotaPeriodStart(quota *v1alpha1.CognitiveQuota) (time.Time, error) {
	periodStart := quota.ObjectMeta.CreationTimestamp
	if quota.Status.LastResetTime == "" {
		return periodStart, nil
	}
	lastReset, err := time.Parse(time.RFC3339Nano, quota.Status.LastResetTime)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid last reset time %q", quota.Status.LastResetTime)
	}
	if lastReset.After(periodStart) {
		return lastReset, nil
	}
	return periodStart, nil
}

func projectedModelTierUsage(ctx context.Context, tx pgx.Tx, workspace string, periodStart time.Time) (map[string]int64, map[string]int64, error) {
	rows, err := tx.Query(ctx, `SELECT model_cost_tier, SUM(provider_duration_ms)
		FROM cost_records
		WHERE workspace = $1 AND operation_type = $2 AND model_cost_tier <> ''
		  AND ($3::timestamptz IS NULL OR created_at >= $3)
		GROUP BY model_cost_tier`, workspace, observability.CostOpModelInference, nullableTime(periodStart))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	result := make(map[string]int64)
	millisecondsByTier := make(map[string]int64)
	for rows.Next() {
		var tier string
		var milliseconds int64
		if err := rows.Scan(&tier, &milliseconds); err != nil {
			return nil, nil, err
		}
		seconds := milliseconds / 1000
		if milliseconds%1000 != 0 {
			seconds++
		}
		result[tier] = seconds
		millisecondsByTier[tier] = milliseconds
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(result) == 0 {
		return nil, nil, nil
	}
	return result, millisecondsByTier, nil
}

func normalizeCostGroups(groups []observability.CostGroupBy) ([]observability.CostGroupBy, error) {
	if len(groups) == 0 {
		return []observability.CostGroupBy{observability.GroupByWorkspace}, nil
	}
	seen := make(map[observability.CostGroupBy]struct{}, len(groups))
	for _, group := range groups {
		switch group {
		case observability.GroupByWorkspace, observability.GroupByAgent, observability.GroupBySolution, observability.GroupByModel, observability.GroupByOperationType:
		default:
			return nil, fmt.Errorf("unsupported cost group %q", group)
		}
		if _, exists := seen[group]; exists {
			return nil, fmt.Errorf("duplicate cost group %q", group)
		}
		seen[group] = struct{}{}
	}
	return groups, nil
}

func costFilterClauses(query observability.CostQuery) ([]string, []any) {
	clauses := make([]string, 0, 5)
	arguments := make([]any, 0, 5)
	add := func(column string, value any) {
		arguments = append(arguments, value)
		clauses = append(clauses, fmt.Sprintf("%s = $%d", column, len(arguments)))
	}
	if query.Workspace != "" {
		add("workspace", query.Workspace)
	}
	if query.AgentIdentity != "" {
		add("agent_identity", query.AgentIdentity)
	}
	if query.Solution != "" {
		add("solution", query.Solution)
	}
	if query.SolutionRev != nil {
		add("solution_rev", *query.SolutionRev)
	}
	if !query.Start.IsZero() {
		arguments = append(arguments, query.Start)
		clauses = append(clauses, fmt.Sprintf("created_at >= $%d", len(arguments)))
	}
	if !query.End.IsZero() {
		arguments = append(arguments, query.End)
		clauses = append(clauses, fmt.Sprintf("created_at <= $%d", len(arguments)))
	}
	return clauses, arguments
}

func costGroupSelect(groups []observability.CostGroupBy) string {
	return strings.Join([]string{
		costGroupExpression(groups, observability.GroupByWorkspace, "workspace", "TEXT"),
		costGroupExpression(groups, observability.GroupByAgent, "agent_identity", "TEXT"),
		costGroupExpression(groups, observability.GroupBySolution, "solution", "TEXT"),
		costGroupExpression(groups, observability.GroupBySolution, "solution_rev", "BIGINT"),
		costGroupExpression(groups, observability.GroupByModel, "model_name", "TEXT"),
		costGroupExpression(groups, observability.GroupByOperationType, "operation_type", "TEXT"),
	}, ", ")
}

func costGroupExpression(groups []observability.CostGroupBy, group observability.CostGroupBy, column, typeName string) string {
	for _, candidate := range groups {
		if candidate == group {
			return column
		}
	}
	if typeName == "BIGINT" {
		return "0::BIGINT"
	}
	return "''::TEXT"
}

func costGroupColumns(groups []observability.CostGroupBy) []string {
	columns := make([]string, 0, len(groups)+1)
	for _, group := range groups {
		switch group {
		case observability.GroupByWorkspace:
			columns = append(columns, "workspace")
		case observability.GroupByAgent:
			columns = append(columns, "agent_identity")
		case observability.GroupBySolution:
			columns = append(columns, "solution", "solution_rev")
		case observability.GroupByModel:
			columns = append(columns, "model_name")
		case observability.GroupByOperationType:
			columns = append(columns, "operation_type")
		}
	}
	return columns
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}
