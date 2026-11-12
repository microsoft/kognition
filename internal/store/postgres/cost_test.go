// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
)

func TestQueryUsageEventsRetainsAttributionAndExcludesLegacyCosts(t *testing.T) {
	storage := newTestStore(t)
	ctx := context.Background()
	event := observability.CostRecord{
		ID: "retained-usage", Timestamp: time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC),
		Workspace: "team-a", Agent: "planner", AgentNamespace: "team-a", Workflow: "research", Step: "plan",
		OperationType: observability.CostOpModelInference, ModelName: "demo", ModelProvider: "openai-compatible",
		PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, CostUnits: 45, ProviderDurationMillis: 1250,
		UsageReported: true, PricingConfigured: true,
	}
	if applied, err := storage.ApplyUsageEvent(ctx, event); err != nil || !applied {
		t.Fatalf("ApplyUsageEvent() = (%t, %v)", applied, err)
	}
	if err := storage.RecordCost(ctx, observability.CostRecord{Workspace: "team-a", OperationType: observability.CostOpToolExecution}); err != nil {
		t.Fatal(err)
	}
	records, err := storage.QueryUsageEvents(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("QueryUsageEvents() = (%#v, %v), want one event", records, err)
	}
	actual := records[0]
	if !actual.Timestamp.Equal(event.Timestamp) {
		t.Fatalf("timestamp = %v, want %v", actual.Timestamp, event.Timestamp)
	}
	actual.Timestamp = event.Timestamp
	if actual != event {
		t.Fatalf("retained event = %#v, want %#v", actual, event)
	}
}

func TestApplyUsageEventAtomicallyProjectsCostAndModelTierOnce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	quota := v1alpha1.CognitiveQuota{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "CognitiveQuota", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "default", Namespace: "team-a"},
		Spec: v1alpha1.CognitiveQuotaSpec{
			Hard:        map[string]int64{},
			TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 1000},
			CostBudget:  &v1alpha1.CostBudget{MaxCostPerPeriod: 1000, MaxModelTierSeconds: map[string]int64{"premium": 60}},
		},
		Status: v1alpha1.CognitiveQuotaStatus{LastResetTime: "2026-08-25T13:00:00Z"},
	}
	data, err := json.Marshal(quota)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, "CognitiveQuota", "team-a", "default", data); err != nil {
		t.Fatal(err)
	}
	event := observability.CostRecord{
		ID: "usage-1", Timestamp: time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC),
		Workspace: "team-a", Agent: "planner", AgentIdentity: "spiffe://planner", AgentNamespace: "team-a", Workflow: "research-flow", Step: "plan", SessionID: "session-1", ReservationID: "agentinstance:instance-1",
		OperationType: observability.CostOpModelInference, Purpose: observability.UsagePurposeReasoningProbe, ModelName: "gpt", ModelProvider: "openai-compatible", ModelClass: "frontier-v1", ModelClassVersion: "2026-08-01", ModelCostTier: "premium",
		PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, UsageReported: false, UsageEstimated: true, PricingConfigured: true,
		InputCostMicrousdPerMillionTokens: 2500000, OutputCostMicrousdPerMillionTokens: 10000000,
		CostUnits: 45, ProviderDurationMillis: 1250, TraceID: "trace-1", SpanID: "span-1",
	}
	applied, err := store.ApplyUsageEvent(ctx, event)
	if err != nil || !applied {
		t.Fatalf("first ApplyUsageEvent() = (%t, %v), want (true, nil)", applied, err)
	}
	applied, err = store.ApplyUsageEvent(ctx, event)
	if err != nil || applied {
		t.Fatalf("retry ApplyUsageEvent() = (%t, %v), want (false, nil)", applied, err)
	}
	historical := event
	historical.ID, historical.TraceID, historical.SpanID = "usage-before-reset", "trace-before-reset", "span-before-reset"
	historical.Timestamp = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	historical.CostUnits = 100
	if applied, err = store.ApplyUsageEvent(ctx, historical); err != nil || !applied {
		t.Fatalf("historical ApplyUsageEvent() = (%t, %v), want (true, nil)", applied, err)
	}

	raw, err := store.Get(ctx, "CognitiveQuota", "team-a", "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &quota); err != nil {
		t.Fatal(err)
	}
	if quota.Status.CostUsage == nil || quota.Status.CostUsage.TotalCost != 45 || quota.Status.CostUsage.ModelTierSeconds["premium"] != 2 {
		t.Fatalf("cost usage = %#v, want 45 micro-USD and 2 premium seconds", quota.Status.CostUsage)
	}
	if quota.Status.TokenUsage != nil {
		t.Fatalf("token reservation projection was double charged: %#v", quota.Status.TokenUsage)
	}
	result, err := store.QueryCosts(ctx, observability.CostQuery{Workspace: "team-a", Start: time.Date(2026, 8, 25, 13, 0, 0, 0, time.UTC)})
	if err != nil || result.TotalCount != 1 || len(result.Aggregations) != 1 || result.Aggregations[0].TotalOperations != 1 || result.Aggregations[0].TotalCostUnits != 45 {
		t.Fatalf("persisted usage aggregation = %#v, err=%v", result, err)
	}
	var purpose observability.UsagePurpose
	var usageReported, usageEstimated bool
	var agent, workflow, step string
	if err := testPool.QueryRow(ctx, `SELECT usage_purpose, usage_reported, usage_estimated, agent, workflow, step FROM cost_records WHERE event_id = $1`, event.ID).Scan(&purpose, &usageReported, &usageEstimated, &agent, &workflow, &step); err != nil || purpose != observability.UsagePurposeReasoningProbe || usageReported || !usageEstimated || agent != event.Agent || workflow != event.Workflow || step != event.Step {
		t.Fatalf("persisted usage attribution = (%q, reported=%t, estimated=%t, agent=%q, workflow=%q, step=%q, %v)", purpose, usageReported, usageEstimated, agent, workflow, step, err)
	}
}

func TestModelCanaryUsagePurposePersistsOnce(test *testing.T) {
	storage := newTestStore(test)
	ctx := context.Background()
	event := observability.CostRecord{ID: "model-canary:durable", Workspace: "tenant", Purpose: observability.UsagePurposeModelCanary, OperationType: observability.CostOpModelInference, PromptTokens: 12, CompletionTokens: 6, TotalTokens: 18, UsageReported: true, Timestamp: time.Now().UTC()}
	if applied, err := storage.ApplyUsageEvent(ctx, event); err != nil || !applied {
		test.Fatalf("model canary usage rejected: applied=%t error=%v", applied, err)
	}
	if applied, err := storage.ApplyUsageEvent(ctx, event); err != nil || applied {
		test.Fatal("model canary usage applied more than once")
	}
	var purpose string
	if err := testPool.QueryRow(ctx, `SELECT usage_purpose FROM cost_records WHERE event_id = $1`, event.ID).Scan(&purpose); err != nil || purpose != string(observability.UsagePurposeModelCanary) {
		test.Fatalf("model canary purpose not persisted: purpose=%q error=%v", purpose, err)
	}
}

func TestApplyUsageEventRejectsInvalidAndConflictingEvents(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.ApplyUsageEvent(ctx, observability.CostRecord{ID: "tool-1", Workspace: "team-a", OperationType: observability.CostOpToolExecution}); err == nil {
		t.Fatal("non-model usage event was accepted")
	}
	if _, err := store.ApplyUsageEvent(ctx, observability.CostRecord{Workspace: "team-a", OperationType: observability.CostOpModelInference}); err == nil {
		t.Fatal("usage event without idempotency ID was accepted")
	}
	if _, err := store.ApplyUsageEvent(ctx, observability.CostRecord{ID: "inconsistent", Workspace: "team-a", OperationType: observability.CostOpModelInference, PromptTokens: 2, CompletionTokens: 1, TotalTokens: 1}); err == nil {
		t.Fatal("usage event with inconsistent total tokens was accepted")
	}
	if _, err := store.ApplyUsageEvent(ctx, observability.CostRecord{ID: "unknown-purpose", Workspace: "team-a", OperationType: observability.CostOpModelInference, Purpose: "unknown"}); err == nil {
		t.Fatal("usage event with unknown purpose was accepted")
	}

	event := observability.CostRecord{
		ID: "usage-conflict", Timestamp: time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC), Workspace: "team-a",
		OperationType: observability.CostOpModelInference, Purpose: observability.UsagePurposeReasoningProbe, CostUnits: 45, TraceID: "trace-conflict", SpanID: "span-conflict",
	}
	if applied, err := store.ApplyUsageEvent(ctx, event); err != nil || !applied {
		t.Fatalf("first ApplyUsageEvent() = (%t, %v), want (true, nil)", applied, err)
	}
	conflict := event
	conflict.Purpose = ""
	if _, err := store.ApplyUsageEvent(ctx, conflict); err == nil {
		t.Fatal("usage event ID reuse with different purpose was accepted")
	}
	conflict = event
	conflict.UsageEstimated = true
	if _, err := store.ApplyUsageEvent(ctx, conflict); err == nil {
		t.Fatal("usage event ID reuse with different estimated state was accepted")
	}
	conflict = event
	conflict.UsageReported = true
	if _, err := store.ApplyUsageEvent(ctx, conflict); err == nil {
		t.Fatal("usage event ID reuse with different reported state was accepted")
	}
	total, err := store.GetTotalCost(ctx, "team-a", time.Time{}, time.Time{})
	if err != nil || total != 45 {
		t.Fatalf("cost after conflicting retry = (%d, %v), want (45, nil)", total, err)
	}
	deleted, err := store.DeleteCostRecordsBefore(ctx, "team-a", event.Timestamp.Add(time.Hour))
	if err != nil || deleted != 1 {
		t.Fatalf("usage event retention = (%d, %v), want (1, nil)", deleted, err)
	}
	if applied, err := store.ApplyUsageEvent(ctx, event); err != nil || applied {
		t.Fatalf("retry after retention = (%t, %v), want (false, nil)", applied, err)
	}
}

func TestUsageFactsAndReceiptsAreImmutable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	event := observability.CostRecord{ID: "usage-immutable", Timestamp: time.Now().UTC(), Workspace: "team-a", OperationType: observability.CostOpModelInference, CostUnits: 45}
	if applied, err := store.ApplyUsageEvent(ctx, event); err != nil || !applied {
		t.Fatalf("ApplyUsageEvent() = (%t, %v)", applied, err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE cost_records SET cost_units = 90 WHERE event_id = $1`, event.ID); err == nil {
		t.Fatal("mutable cost record update was accepted")
	}
	if _, err := testPool.Exec(ctx, `UPDATE usage_event_receipts SET event_fingerprint = 'forged' WHERE event_id = $1`, event.ID); err == nil {
		t.Fatal("mutable usage receipt update was accepted")
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM usage_event_receipts WHERE event_id = $1`, event.ID); err == nil {
		t.Fatal("usage receipt deletion was accepted")
	}
	if deleted, err := store.DeleteCostRecordsBefore(ctx, "team-a", event.Timestamp.Add(time.Hour)); err != nil || deleted != 1 {
		t.Fatalf("cost retention delete = (%d, %v), want (1, nil)", deleted, err)
	}
}

func TestCostStoreRejectsModernModelEventsFromLegacyAPIs(t *testing.T) {
	store := newTestStore(t)
	event := observability.CostRecord{ID: "modern-event", Workspace: "team-a", OperationType: observability.CostOpModelInference}
	if err := store.RecordCost(context.Background(), event); err == nil {
		t.Fatal("RecordCost accepted a modern model usage event")
	}
	if err := store.RecordCostBatch(context.Background(), []observability.CostRecord{event}); err == nil {
		t.Fatal("RecordCostBatch accepted a modern model usage event")
	}
	if applied, err := store.ApplyUsageEvent(context.Background(), event); err != nil || !applied {
		t.Fatalf("ApplyUsageEvent() = (%t, %v), want (true, nil)", applied, err)
	}
	result, err := store.QueryCosts(context.Background(), observability.CostQuery{Workspace: "team-a"})
	if err != nil || result.TotalCount != 1 || result.Aggregations[0].TotalOperations != 1 {
		t.Fatalf("costs = %#v, err=%v; want one event", result, err)
	}
}

func TestApplyUsageEventRoundsAggregateModelTierDurationOnce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	quota := v1alpha1.CognitiveQuota{ObjectMeta: v1alpha1.ObjectMeta{Name: "default", Namespace: "team-a"}, Spec: v1alpha1.CognitiveQuotaSpec{Hard: map[string]int64{}, CostBudget: &v1alpha1.CostBudget{MaxModelTierSeconds: map[string]int64{"premium": 60}}}}
	data, err := json.Marshal(quota)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, "CognitiveQuota", "team-a", "default", data); err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 2; index++ {
		event := observability.CostRecord{ID: fmt.Sprintf("usage-%d", index), Timestamp: time.Now().UTC(), Workspace: "team-a", OperationType: observability.CostOpModelInference, ModelCostTier: "premium", ProviderDurationMillis: 1}
		if _, err := store.ApplyUsageEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := store.Get(ctx, "CognitiveQuota", "team-a", "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &quota); err != nil {
		t.Fatal(err)
	}
	if quota.Status.CostUsage == nil || quota.Status.CostUsage.ModelTierSeconds["premium"] != 1 || quota.Status.CostUsage.ModelTierMilliseconds["premium"] != 2 {
		t.Fatalf("tier usage = %#v, want one aggregate second", quota.Status.CostUsage)
	}
}

func TestCostStorePersistsAggregatesAndRetainsRecords(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	old := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	current := old.Add(2 * time.Hour)
	records := []observability.CostRecord{
		{Timestamp: old, Workspace: "team-a", AgentIdentity: "spiffe://planner", AgentNamespace: "team-a", Solution: "research", SolutionRev: 2, SessionID: "session-1", OperationType: observability.CostOpModelInference, ModelName: "gpt", ModelProvider: "openai-compatible", PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, CostUnits: 45, TraceID: "trace-1", SpanID: "span-1"},
		{Timestamp: current, Workspace: "team-a", AgentIdentity: "spiffe://writer", AgentNamespace: "team-a", Solution: "research", SolutionRev: 2, SessionID: "session-2", OperationType: observability.CostOpModelInference, ModelName: "gpt", ModelProvider: "openai-compatible", PromptTokens: 8, CompletionTokens: 3, TotalTokens: 11, CostUnits: 50, TraceID: "trace-2", SpanID: "span-2"},
		{Timestamp: current, Workspace: "team-b", AgentIdentity: "spiffe://planner", AgentNamespace: "team-b", OperationType: observability.CostOpToolExecution, ToolName: "search", CostUnits: 7, TraceID: "trace-3", SpanID: "span-3"},
	}
	if err := store.RecordCostBatch(ctx, records); err != nil {
		t.Fatal(err)
	}
	result, err := store.QueryCosts(ctx, observability.CostQuery{Workspace: "team-a", GroupBy: []observability.CostGroupBy{observability.GroupByAgent}})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 2 || len(result.Aggregations) != 2 {
		t.Fatalf("aggregations = %#v", result)
	}
	if result.Aggregations[0].AgentIdentity != "spiffe://planner" || result.Aggregations[0].TotalCostUnits != 45 || result.Aggregations[1].AgentIdentity != "spiffe://writer" || result.Aggregations[1].TotalCostUnits != 50 {
		t.Fatalf("agent aggregation = %#v", result.Aggregations)
	}

	revision := int64(2)
	result, err = store.QueryCosts(ctx, observability.CostQuery{Workspace: "team-a", Solution: "research", SolutionRev: &revision, GroupBy: []observability.CostGroupBy{observability.GroupByModel}})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 1 || len(result.Aggregations) != 1 || result.Aggregations[0].ModelName != "gpt" || result.Aggregations[0].TotalCostUnits != 95 || result.Aggregations[0].TotalPromptTokens != 18 || result.Aggregations[0].TotalCompletionTokens != 5 {
		t.Fatalf("model aggregation = %#v", result)
	}

	total, err := store.GetTotalCost(ctx, "team-a", time.Time{}, time.Time{})
	if err != nil || total != 95 {
		t.Fatalf("GetTotalCost() = (%d, %v), want (95, nil)", total, err)
	}
	deleted, err := store.DeleteCostRecordsBefore(ctx, "team-a", current)
	if err != nil || deleted != 1 {
		t.Fatalf("DeleteCostRecordsBefore() = (%d, %v), want (1, nil)", deleted, err)
	}
	total, err = store.GetTotalCost(ctx, "team-a", time.Time{}, time.Time{})
	if err != nil || total != 50 {
		t.Fatalf("remaining total = (%d, %v), want (50, nil)", total, err)
	}
}

func TestCostStoreDoesNotDoubleCountTraceCorrelatedRecord(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	record := observability.CostRecord{
		Timestamp:     time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		Workspace:     "team-a",
		OperationType: observability.CostOpModelInference,
		CostUnits:     45,
		TraceID:       "trace-1",
		SpanID:        "span-1",
	}
	if err := store.RecordCost(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCost(ctx, record); err != nil {
		t.Fatal(err)
	}

	total, err := store.GetTotalCost(ctx, "team-a", time.Time{}, time.Time{})
	if err != nil || total != 45 {
		t.Fatalf("GetTotalCost() = (%d, %v), want (45, nil)", total, err)
	}
}

func TestCostStoreRejectsInvalidAggregationQueries(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.QueryCosts(context.Background(), observability.CostQuery{GroupBy: []observability.CostGroupBy{"unknown"}}); err == nil {
		t.Fatal("unsupported cost group was accepted")
	}
	if err := store.RecordCost(context.Background(), observability.CostRecord{OperationType: observability.CostOpModelInference, CostUnits: 1}); err == nil {
		t.Fatal("cost record without workspace was accepted")
	}
}
