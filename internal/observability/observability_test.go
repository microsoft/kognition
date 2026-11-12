// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// =============================================================================
// TC-0133: 3 tool calls + 2 memory ops traced end-to-end
// =============================================================================

func TestTracer_EndToEnd_ToolCallsAndMemoryOps(t *testing.T) {
	config := TracerConfig{SampleRate: 1.0, CostTagger: NewCostTagger()}
	tracer := NewTracer(config)

	var costRecords []CostRecord
	tracer.SetCostSink(func(r CostRecord) {
		costRecords = append(costRecords, r)
	})

	ctx := context.Background()

	// Simulate 3 tool calls.
	for i := 0; i < 3; i++ {
		span := tracer.StartSpan(ctx, SpanNameToolCall, SpanKindClient, map[string]interface{}{
			AttrToolName:       "search",
			AttrToolLatencyMs:  int64(50 + i*10),
			AttrToolCostUnits:  int64(100),
			AttrWorkspace:      "default",
			AttrAgentName:      "assistant",
			AttrAgentNamespace: "prod",
		})
		tracer.EndSpan(span, StatusOK, "")
	}

	// Simulate 2 memory ops.
	for i := 0; i < 2; i++ {
		span := tracer.StartSpan(ctx, SpanNameMemoryOp, SpanKindInternal, map[string]interface{}{
			AttrMemoryOp:       "write",
			AttrMemoryKey:      "context",
			AttrMemoryValue:    "some value",
			AttrWorkspace:      "default",
			AttrAgentName:      "assistant",
			AttrAgentNamespace: "prod",
		})
		tracer.EndSpan(span, StatusOK, "")
	}

	// Verify spans.
	spans := tracer.Spans()
	if len(spans) != 5 {
		t.Fatalf("expected 5 spans, got %d", len(spans))
	}
	if tracer.SpanCount() != 5 {
		t.Fatalf("expected span count 5, got %d", tracer.SpanCount())
	}

	// Verify tool call spans.
	toolSpans := 0
	memSpans := 0
	for _, s := range spans {
		switch s.Name {
		case SpanNameToolCall:
			toolSpans++
		case SpanNameMemoryOp:
			memSpans++
		}
	}
	if toolSpans != 3 {
		t.Errorf("expected 3 tool call spans, got %d", toolSpans)
	}
	if memSpans != 2 {
		t.Errorf("expected 2 memory op spans, got %d", memSpans)
	}

	// Verify all spans share same trace ID.
	traceID := tracer.TraceID()
	for _, s := range spans {
		if s.Context.TraceID != traceID {
			t.Errorf("span %q has wrong trace ID", s.Name)
		}
	}

	// Verify cost records extracted (only tool call spans have cost attributes).
	if len(costRecords) != 3 {
		t.Errorf("expected 3 cost records, got %d", len(costRecords))
	}
}

func TestCostRecordUsageStateJSONCompatibility(t *testing.T) {
	for _, test := range []struct {
		name           string
		usageReported  bool
		usageEstimated bool
		wantReported   bool
		wantEstimated  bool
	}{
		{name: "absent usage preserves legacy shape"},
		{name: "reported usage is explicit", usageReported: true, wantReported: true},
		{name: "estimated usage is explicit", usageEstimated: true, wantEstimated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(CostRecord{UsageReported: test.usageReported, UsageEstimated: test.usageEstimated})
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			if value, present := record["usageReported"]; present != test.wantReported || (present && value != true) {
				t.Fatalf("usageReported JSON = (%v, present=%t), want true with present=%t", value, present, test.wantReported)
			}
			if value, present := record["usageEstimated"]; present != test.wantEstimated || (present && value != true) {
				t.Fatalf("usageEstimated JSON = (%v, present=%t), want true with present=%t", value, present, test.wantEstimated)
			}
		})
	}
}

// =============================================================================
// TC-0134: MVP-4 demo scenario (full reasoning trace)
// =============================================================================

func TestTracer_MVP4DemoScenario(t *testing.T) {
	redactor := NewRedactor(DefaultRedactionConfig())
	costTagger := NewCostTagger()
	config := TracerConfig{
		SampleRate: 1.0,
		Redactor:   redactor,
		CostTagger: costTagger,
	}
	tracer := NewTracer(config)
	ctx := context.Background()

	// Step 1: Model inference.
	modelSpan := tracer.StartSpan(ctx, SpanNameModelInfer, SpanKindClient, map[string]interface{}{
		AttrModelName:        "gpt-4",
		AttrModelProvider:    "openai",
		AttrPromptTokens:     int64(150),
		AttrCompletionTokens: int64(50),
		AttrTotalTokens:      int64(200),
		AttrModelCostUnits:   int64(600),
		AttrWorkspace:        "demo",
		AttrAgentName:        "planner",
		AttrAgentNamespace:   "mvp4",
		AttrSolution:         "task-planner",
		AttrSessionID:        "session-001",
	})
	tracer.EndSpan(modelSpan, StatusOK, "")

	// Step 2: Tool call.
	toolSpan := tracer.StartSpan(ctx, SpanNameToolCall, SpanKindClient, map[string]interface{}{
		AttrToolName:      "calendar_lookup",
		AttrToolArgs:      `{"date":"2024-01-15"}`,
		AttrToolResult:    `{"events":["meeting at 10am"]}`,
		AttrToolLatencyMs: int64(120),
		AttrToolCostUnits: int64(50),
		AttrWorkspace:     "demo",
		AttrAgentName:     "planner",
	})
	WithParent(toolSpan, modelSpan)
	tracer.EndSpan(toolSpan, StatusOK, "")

	// Step 3: Reasoning step.
	reasonSpan := tracer.StartSpan(ctx, SpanNameReasoningStep, SpanKindInternal, map[string]interface{}{
		AttrReasoningStepIndex: int64(1),
		AttrReasoningRole:      "assistant",
		AttrReasoningContent:   "Based on calendar, user has a meeting at 10am.",
		AttrWorkspace:          "demo",
		AttrAgentName:          "planner",
	})
	tracer.EndSpan(reasonSpan, StatusOK, "")

	spans := tracer.Spans()
	if len(spans) != 3 {
		t.Fatalf("expected 3 spans, got %d", len(spans))
	}

	// Verify parent-child relationship.
	if toolSpan.Context.TraceID != modelSpan.Context.TraceID {
		t.Error("tool span should share trace ID with model span")
	}
}

// =============================================================================
// TC-0135: PII redaction (email, SSN redacted in spans)
// =============================================================================

func TestRedactor_PIIRedaction(t *testing.T) {
	redactor := NewRedactor(DefaultRedactionConfig())

	tests := []struct {
		name     string
		field    RedactableField
		input    string
		contains string
		excludes string
	}{
		{
			name:     "email in tool args",
			field:    FieldToolArgs,
			input:    `{"email":"user@example.com","query":"hello"}`,
			contains: "[REDACTED:email]",
			excludes: "user@example.com",
		},
		{
			name:     "SSN in tool result",
			field:    FieldToolResult,
			input:    `SSN: 123-45-6789`,
			contains: "[REDACTED:ssn]",
			excludes: "123-45-6789",
		},
		{
			name:     "phone in memory value",
			field:    FieldMemoryValue,
			input:    "Call me at 555-123-4567 tomorrow",
			contains: "[REDACTED:phone]",
			excludes: "555-123-4567",
		},
		{
			name:     "credit card in reasoning",
			field:    FieldReasoningContent,
			input:    "Card number is 4111-1111-1111-1111",
			contains: "[REDACTED:credit_card]",
			excludes: "4111-1111-1111-1111",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := redactor.Redact(tc.field, tc.input)
			if tc.contains != "" && !containsStr(result, tc.contains) {
				t.Errorf("expected result to contain %q, got %q", tc.contains, result)
			}
			if tc.excludes != "" && containsStr(result, tc.excludes) {
				t.Errorf("expected result to NOT contain %q, got %q", tc.excludes, result)
			}
		})
	}
}

func TestRedactor_RedactSpanAttributes(t *testing.T) {
	redactor := NewRedactor(DefaultRedactionConfig())
	attrs := map[string]interface{}{
		AttrToolArgs:         map[string]interface{}{"email": "secret@corp.com", "nested": map[string]interface{}{"ssn": "987-65-4321"}},
		AttrToolResult:       `SSN: 987-65-4321`,
		AttrMemoryValue:      "bearer eyJhbGciOiJIUzI1NiJ9.test.sig",
		AttrReasoningContent: "Normal reasoning content",
		AttrToolName:         "search", // Non-redactable field.
	}

	redacted := redactor.RedactSpanAttributes(attrs)

	// Tool args should be redacted.
	if containsStr(redacted[AttrToolArgs].(string), "secret@corp.com") {
		t.Error("email should be redacted in tool args")
	}
	if containsStr(redacted[AttrToolArgs].(string), "987-65-4321") {
		t.Error("SSN should be redacted in structured tool args")
	}
	// SSN should be redacted.
	if containsStr(redacted[AttrToolResult].(string), "987-65-4321") {
		t.Error("SSN should be redacted in tool result")
	}
	// Non-redactable fields should pass through.
	if redacted[AttrToolName] != "search" {
		t.Error("non-redactable field should not be modified")
	}
}

func TestRedactor_NoneLevel(t *testing.T) {
	config := DefaultRedactionConfig()
	config.Level = RedactionNone
	redactor := NewRedactor(config)

	input := "user@example.com and 123-45-6789"
	result := redactor.Redact(FieldToolArgs, input)
	if result != input {
		t.Errorf("RedactionNone should return input unchanged, got %q", result)
	}
}

// =============================================================================
// TC-0136: Cost query by workspace returns correct aggregation
// =============================================================================

func TestCostTagger_TagFromSpan(t *testing.T) {
	tagger := NewCostTagger()
	startedAt := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)

	span := &Span{
		Name:      SpanNameModelInfer,
		StartTime: startedAt,
		EndTime:   startedAt.Add(1250 * time.Millisecond),
		Attributes: map[string]interface{}{
			AttrWorkspace:              "billing-ws",
			AttrAgentName:              "summarizer-agent",
			AttrAgentIdentity:          "summarizer",
			AttrAgentNamespace:         "prod",
			AttrWorkflowName:           "document-pipeline",
			AttrStepName:               "summarize",
			AttrSolution:               "doc-summary",
			AttrSolutionRev:            int64(3),
			AttrSessionID:              "sess-99",
			AttrReservationID:          "agentinstance:instance-1",
			AttrModelName:              "gpt-4-turbo",
			AttrModelProvider:          "openai",
			AttrModelClass:             "frontier",
			AttrModelClassVersion:      "2026-08-01",
			AttrModelCostTier:          "premium",
			AttrModelInputRate:         int64(2500000),
			AttrModelOutputRate:        int64(10000000),
			AttrModelPricingConfigured: true,
			AttrPromptTokens:           int64(500),
			AttrCompletionTokens:       int64(200),
			AttrTotalTokens:            int64(700),
			AttrModelCostUnits:         int64(2100),
		},
	}

	record := tagger.TagFromSpan(span)

	if record.ID != record.TraceID+":"+record.SpanID {
		t.Errorf("event ID = %q, want trace/span-derived ID", record.ID)
	}
	if record.Workspace != "billing-ws" {
		t.Errorf("expected workspace 'billing-ws', got %q", record.Workspace)
	}
	if record.AgentIdentity != "summarizer" {
		t.Errorf("expected agent 'summarizer', got %q", record.AgentIdentity)
	}
	if record.Agent != "summarizer-agent" || record.Workflow != "document-pipeline" || record.Step != "summarize" {
		t.Errorf("stable execution attribution = agent %q, workflow %q, step %q", record.Agent, record.Workflow, record.Step)
	}
	if record.ModelName != "gpt-4-turbo" {
		t.Errorf("expected model 'gpt-4-turbo', got %q", record.ModelName)
	}
	if record.PromptTokens != 500 {
		t.Errorf("expected 500 prompt tokens, got %d", record.PromptTokens)
	}
	if record.CompletionTokens != 200 {
		t.Errorf("expected 200 completion tokens, got %d", record.CompletionTokens)
	}
	if record.CostUnits != 2100 {
		t.Errorf("expected 2100 cost units, got %d", record.CostUnits)
	}
	if record.ReservationID != "agentinstance:instance-1" || record.ModelClass != "frontier" || record.ModelClassVersion != "2026-08-01" || record.ModelCostTier != "premium" {
		t.Errorf("model accounting attribution = %#v", record)
	}
	if !record.PricingConfigured || record.InputCostMicrousdPerMillionTokens != 2500000 || record.OutputCostMicrousdPerMillionTokens != 10000000 {
		t.Errorf("pricing snapshot = %#v", record)
	}
	if record.ProviderDurationMillis != 1250 {
		t.Errorf("provider duration = %dms, want 1250ms", record.ProviderDurationMillis)
	}
	span.EndTime = span.StartTime.Add(time.Nanosecond)
	if duration := tagger.TagFromSpan(span).ProviderDurationMillis; duration != 1 {
		t.Errorf("sub-millisecond provider duration = %dms, want 1ms", duration)
	}
	if record.OperationType != CostOpModelInference {
		t.Errorf("expected operation type %q, got %q", CostOpModelInference, record.OperationType)
	}
}

func TestCostTagger_ToolCallSpan(t *testing.T) {
	tagger := NewCostTagger()

	span := &Span{
		Name: SpanNameToolCall,
		Attributes: map[string]interface{}{
			AttrWorkspace:     "ws-1",
			AttrToolName:      "web_search",
			AttrToolCostUnits: int64(50),
		},
	}

	record := tagger.TagFromSpan(span)
	if record.OperationType != CostOpToolExecution {
		t.Errorf("expected tool execution, got %q", record.OperationType)
	}
	if record.ToolName != "web_search" {
		t.Errorf("expected tool name 'web_search', got %q", record.ToolName)
	}
	if record.CostUnits != 50 {
		t.Errorf("expected 50 cost units, got %d", record.CostUnits)
	}
}

func TestModelCostUnits(t *testing.T) {
	parameters := map[string]string{
		ModelInputCostMicrousdPerMillionTokensParameter:  "2500000",
		ModelOutputCostMicrousdPerMillionTokensParameter: "10000000",
	}
	cost, configured, err := ModelCostUnits(parameters, 500, 200)
	if err != nil || !configured {
		t.Fatalf("ModelCostUnits() = (%d, %t, %v)", cost, configured, err)
	}
	if cost != 3250 {
		t.Fatalf("cost = %d, want 3250 micro-USD", cost)
	}

	if _, configured, err := ModelCostUnits(nil, 500, 200); err != nil || configured {
		t.Fatalf("unconfigured ModelCostUnits() = (_, %t, %v)", configured, err)
	}
	if _, _, err := ModelCostUnits(map[string]string{ModelInputCostMicrousdPerMillionTokensParameter: "2500000"}, 1, 1); err == nil {
		t.Fatal("partial ModelClass pricing was accepted")
	}
	cost, configured, err = ModelCostUnits(map[string]string{
		ModelInputCostMicrousdPerMillionTokensParameter:  "500000",
		ModelOutputCostMicrousdPerMillionTokensParameter: "500000",
	}, 1, 1)
	if err != nil || !configured || cost != 1 {
		t.Fatalf("combined fractional ModelCostUnits() = (%d, %t, %v), want (1, true, nil)", cost, configured, err)
	}
	cost, configured, err = ModelCostUnits(map[string]string{
		ModelInputCostMicrousdPerMillionTokensParameter:  "0",
		ModelOutputCostMicrousdPerMillionTokensParameter: "0",
	}, 500, 200)
	if err != nil || !configured || cost != 0 {
		t.Fatalf("explicit free ModelCostUnits() = (%d, %t, %v), want (0, true, nil)", cost, configured, err)
	}
}

func TestTracerPersistsZeroPricedCostOperations(t *testing.T) {
	tracer := NewTracer(TracerConfig{SampleRate: 1.0, CostTagger: NewCostTagger()})
	var records []CostRecord
	tracer.SetCostSink(func(record CostRecord) { records = append(records, record) })
	for _, name := range []string{SpanNameModelInfer, SpanNameToolCall} {
		span := tracer.StartSpan(context.Background(), name, SpanKindClient, map[string]interface{}{AttrWorkspace: "team-a"})
		tracer.EndSpan(span, StatusOK, "")
	}
	span := tracer.StartSpan(context.Background(), SpanNameReasoningStep, SpanKindInternal, map[string]interface{}{AttrWorkspace: "team-a"})
	tracer.EndSpan(span, StatusOK, "")
	if len(records) != 2 || records[0].OperationType != CostOpModelInference || records[1].OperationType != CostOpToolExecution || records[0].CostUnits != 0 || records[1].CostUnits != 0 {
		t.Fatalf("records = %#v", records)
	}
}

// =============================================================================
// TC-0137: Audit log records CRUD operations
// =============================================================================

type mockAuditStore struct {
	mu      sync.Mutex
	entries []AuditEntry
}

func (m *mockAuditStore) Append(ctx context.Context, entry AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, entry)
	return nil
}

func (m *mockAuditStore) AppendBatch(ctx context.Context, entries []AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, entries...)
	return nil
}

func (m *mockAuditStore) Query(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []AuditEntry
	for _, e := range m.entries {
		if filter.Workspace != "" && e.Workspace != filter.Workspace {
			continue
		}
		if filter.EventType != "" && e.EventType != filter.EventType {
			continue
		}
		result = append(result, e)
	}
	return result, nil
}

func TestCognitiveAuditLog_CRUD(t *testing.T) {
	store := &mockAuditStore{}
	log := NewCognitiveAuditLog(store)
	ctx := context.Background()

	// Record spec change.
	err := log.Record(ctx, AuditEntry{
		AgentIdentity: "agent-1",
		Workspace:     "default",
		EventType:     AuditSpecChange,
		SubjectKind:   "Agent",
		SubjectRef:    "agent-1",
		Verb:          "update",
		Details:       json.RawMessage(`{"field":"model","old":"gpt-3.5","new":"gpt-4"}`),
	})
	if err != nil {
		t.Fatalf("Record spec change: %v", err)
	}

	// Record tool call.
	err = log.Record(ctx, AuditEntry{
		AgentIdentity: "agent-1",
		Workspace:     "default",
		EventType:     AuditToolCall,
		SubjectKind:   "Tool",
		SubjectRef:    "web_search",
		Verb:          "invoke",
	})
	if err != nil {
		t.Fatalf("Record tool call: %v", err)
	}

	// Record grant change.
	err = log.Record(ctx, AuditEntry{
		AgentIdentity: "admin",
		Workspace:     "default",
		EventType:     AuditGrantChange,
		SubjectKind:   "ToolGrant",
		SubjectRef:    "grant-1",
		Verb:          "create",
	})
	if err != nil {
		t.Fatalf("Record grant change: %v", err)
	}

	// Query all for workspace.
	entries, err := log.Query(ctx, AuditFilter{Workspace: "default"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Query by event type.
	entries, err = log.Query(ctx, AuditFilter{
		Workspace: "default",
		EventType: AuditToolCall,
	})
	if err != nil {
		t.Fatalf("Query by type: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 tool call entry, got %d", len(entries))
	}
	if entries[0].SubjectRef != "web_search" {
		t.Errorf("expected subject ref 'web_search', got %q", entries[0].SubjectRef)
	}
}

// =============================================================================
// TC-0138: Durability guarantee (RecordBeforeExec blocks until acknowledged)
// =============================================================================

type slowAuditStore struct {
	mockAuditStore
	delay time.Duration
}

func (s *slowAuditStore) Append(ctx context.Context, entry AuditEntry) error {
	time.Sleep(s.delay)
	return s.mockAuditStore.Append(ctx, entry)
}

func TestCognitiveAuditLog_DurabilityGuarantee(t *testing.T) {
	store := &slowAuditStore{delay: 50 * time.Millisecond}
	log := NewCognitiveAuditLog(store)
	ctx := context.Background()

	entry := AuditEntry{
		AgentIdentity: "agent-1",
		Workspace:     "default",
		EventType:     AuditSpecChange,
		SubjectKind:   "Agent",
		SubjectRef:    "agent-1",
		Verb:          "delete",
	}

	start := time.Now()
	err := log.RecordBeforeExec(ctx, entry)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("RecordBeforeExec: %v", err)
	}

	// Must have blocked at least for the store delay.
	if elapsed < 40*time.Millisecond {
		t.Errorf("RecordBeforeExec should block until acknowledged; elapsed %v", elapsed)
	}

	// Verify entry was persisted.
	entries, _ := log.Query(ctx, AuditFilter{Workspace: "default"})
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after RecordBeforeExec, got %d", len(entries))
	}
}

// =============================================================================
// TC-0139: CloudEvents spec conformance + valid structure
// =============================================================================

func TestCloudEvent_SpecConformance(t *testing.T) {
	data := map[string]string{"key": "value"}
	event, err := NewCloudEvent(EventTypeToolCall, "kognition/default/prod/agent-1", data)
	if err != nil {
		t.Fatalf("NewCloudEvent: %v", err)
	}

	// Required attributes per CloudEvents spec 1.0.
	if event.SpecVersion != CloudEventsSpecVersion {
		t.Errorf("expected specversion %q, got %q", CloudEventsSpecVersion, event.SpecVersion)
	}
	if event.ID == "" {
		t.Error("id must not be empty")
	}
	if event.Type == "" {
		t.Error("type must not be empty")
	}
	if event.Source == "" {
		t.Error("source must not be empty")
	}
	if event.Time.IsZero() {
		t.Error("time should be set")
	}

	// Validate.
	if err := event.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	// Data should be valid JSON.
	var parsed map[string]string
	if err := json.Unmarshal(event.Data, &parsed); err != nil {
		t.Errorf("data should be valid JSON: %v", err)
	}
	if parsed["key"] != "value" {
		t.Errorf("expected data key=value, got %v", parsed)
	}
}

func TestCloudEvent_ValidationErrors(t *testing.T) {
	valid := CloudEvent{
		SpecVersion: CloudEventsSpecVersion,
		ID:          "test-id",
		Type:        EventTypeToolCall,
		Source:      "test-source",
		Time:        time.Now(),
	}
	tests := []struct {
		name    string
		mutate  func(*CloudEvent)
		field   string
		message string
	}{
		{"spec version", func(event *CloudEvent) { event.SpecVersion = "0.3" }, "specversion", "must be 1.0"},
		{"id", func(event *CloudEvent) { event.ID = "" }, "id", "required"},
		{"type", func(event *CloudEvent) { event.Type = "" }, "type", "required"},
		{"source", func(event *CloudEvent) { event.Source = "" }, "source", "required"},
		{"time", func(event *CloudEvent) { event.Time = time.Time{} }, "time", "required"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := valid
			test.mutate(&event)

			err := event.Validate()
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("Validate() error = %T, want *ValidationError", err)
			}
			if validationErr.Field != test.field || validationErr.Message != test.message {
				t.Errorf("Validate() error = %#v, want field %q and message %q", validationErr, test.field, test.message)
			}
			wantText := "cloudevent validation: " + test.field + " " + test.message
			if err.Error() != wantText {
				t.Errorf("error text = %q, want %q", err, wantText)
			}
		})
	}
}

func TestNewCloudEvent_RejectsUnencodableData(t *testing.T) {
	event, err := NewCloudEvent(EventTypeToolCall, "test-source", make(chan int))
	if err == nil {
		t.Fatal("NewCloudEvent() error = nil, want JSON encoding error")
	}
	if event != nil {
		t.Errorf("NewCloudEvent() event = %#v, want nil", event)
	}
}

func TestCloudEvent_WithSubjectAndExtension(t *testing.T) {
	event, _ := NewCloudEvent(EventTypeReasoningStep, "kognition/ws/ns/agent", nil)
	event.WithSubject("step-1").WithExtension("traceid", "abc123")

	if event.Subject != "step-1" {
		t.Errorf("expected subject 'step-1', got %q", event.Subject)
	}
	if event.Extensions["traceid"] != "abc123" {
		t.Errorf("expected extension traceid=abc123, got %v", event.Extensions)
	}
}

func TestEventSource(t *testing.T) {
	source := EventSource("ws-prod", "agents", "planner")
	expected := "/kognition/workspaces/ws-prod/namespaces/agents/agents/planner"
	if source != expected {
		t.Errorf("expected %q, got %q", expected, source)
	}
}

// =============================================================================
// TC-0140: Retention sweep deletes old records
// =============================================================================

type mockRetentionStore struct {
	mu            sync.Mutex
	deletedSpans  map[string]time.Time
	deletedAudits map[string]time.Time
	deletedCosts  map[string]time.Time
	deletedEvents map[string]time.Time
}

func newMockRetentionStore() *mockRetentionStore {
	return &mockRetentionStore{
		deletedSpans:  make(map[string]time.Time),
		deletedAudits: make(map[string]time.Time),
		deletedCosts:  make(map[string]time.Time),
		deletedEvents: make(map[string]time.Time),
	}
}

func (m *mockRetentionStore) DeleteSpansBefore(_ context.Context, ws string, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedSpans[ws] = before
	return 10, nil
}

func (m *mockRetentionStore) DeleteAuditLogsBefore(_ context.Context, ws string, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedAudits[ws] = before
	return 5, nil
}

func (m *mockRetentionStore) DeleteCostRecordsBefore(_ context.Context, ws string, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedCosts[ws] = before
	return 3, nil
}

func (m *mockRetentionStore) DeleteEventsBefore(_ context.Context, ws string, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedEvents[ws] = before
	return 2, nil
}

type staticPolicySource struct {
	policies []WorkspaceRetention
}

func (s *staticPolicySource) ListWorkspaceRetentionPolicies(_ context.Context) ([]WorkspaceRetention, error) {
	return s.policies, nil
}

func TestRetentionSweeper_DeletesOldRecords(t *testing.T) {
	store := newMockRetentionStore()
	policies := &staticPolicySource{
		policies: []WorkspaceRetention{
			{
				Workspace:     "prod",
				TraceTTL:      24 * time.Hour,      // 1 day
				AuditLogTTL:   7 * 24 * time.Hour,  // 7 days
				CostRecordTTL: 30 * 24 * time.Hour, // 30 days
				EventTTL:      48 * time.Hour,      // 2 days
			},
		},
	}

	sweeper := NewRetentionSweeper(store, policies, time.Hour)
	ctx := context.Background()
	result := sweeper.SweepOnce(ctx)

	// Verify no errors.
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}

	// Verify deletions happened.
	if result.SpansDeleted != 10 {
		t.Errorf("expected 10 spans deleted, got %d", result.SpansDeleted)
	}
	if result.AuditLogsDeleted != 5 {
		t.Errorf("expected 5 audit logs deleted, got %d", result.AuditLogsDeleted)
	}
	if result.CostRecordsDeleted != 3 {
		t.Errorf("expected 3 cost records deleted, got %d", result.CostRecordsDeleted)
	}
	if result.EventsDeleted != 2 {
		t.Errorf("expected 2 events deleted, got %d", result.EventsDeleted)
	}

	// Verify cutoff times are reasonable.
	store.mu.Lock()
	defer store.mu.Unlock()

	now := time.Now().UTC()
	spanCutoff := store.deletedSpans["prod"]
	if now.Sub(spanCutoff) < 23*time.Hour || now.Sub(spanCutoff) > 25*time.Hour {
		t.Errorf("span cutoff should be ~24h ago, got %v ago", now.Sub(spanCutoff))
	}

	auditCutoff := store.deletedAudits["prod"]
	if now.Sub(auditCutoff) < 6*24*time.Hour || now.Sub(auditCutoff) > 8*24*time.Hour {
		t.Errorf("audit cutoff should be ~7d ago, got %v ago", now.Sub(auditCutoff))
	}
}

func TestRetentionSweeper_SkipsZeroTTL(t *testing.T) {
	store := newMockRetentionStore()
	policies := &staticPolicySource{
		policies: []WorkspaceRetention{
			{
				Workspace:   "dev",
				TraceTTL:    0, // retain indefinitely
				AuditLogTTL: 24 * time.Hour,
				// CostRecordTTL and EventTTL left as zero.
			},
		},
	}

	sweeper := NewRetentionSweeper(store, policies, time.Hour)
	result := sweeper.SweepOnce(context.Background())

	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	// Spans should NOT have been deleted (zero TTL).
	if _, ok := store.deletedSpans["dev"]; ok {
		t.Error("spans should not be deleted when TraceTTL is zero")
	}
	// Audit logs should have been deleted.
	if _, ok := store.deletedAudits["dev"]; !ok {
		t.Error("audit logs should be deleted when AuditLogTTL is set")
	}
	// Cost records should NOT have been deleted.
	if _, ok := store.deletedCosts["dev"]; ok {
		t.Error("cost records should not be deleted when CostRecordTTL is zero")
	}
}

func TestRetentionSweeper_StartStop(t *testing.T) {
	store := newMockRetentionStore()
	policies := &staticPolicySource{policies: []WorkspaceRetention{}}
	sweeper := NewRetentionSweeper(store, policies, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sweeper.Start(ctx)
	// Starting again should be a no-op.
	sweeper.Start(ctx)

	time.Sleep(30 * time.Millisecond)
	sweeper.Stop()
	// Stopping again should be a no-op.
	sweeper.Stop()
}

// =============================================================================
// Collector (exporter.go) tests
// =============================================================================

type mockSpanExporter struct {
	mu    sync.Mutex
	spans []*Span
	err   error
}

func (m *mockSpanExporter) ExportSpans(_ context.Context, spans []*Span) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.spans = append(m.spans, spans...)
	return nil
}

func (m *mockSpanExporter) Shutdown(_ context.Context) error {
	return m.err
}

type mockCostExporter struct {
	mu      sync.Mutex
	records []CostRecord
	err     error
}

func (m *mockCostExporter) ExportCostRecords(_ context.Context, records []CostRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.records = append(m.records, records...)
	return nil
}

func (m *mockCostExporter) Shutdown(_ context.Context) error {
	return m.err
}

func TestCollector_FlushSpans(t *testing.T) {
	exp := &mockSpanExporter{}
	c := NewCollector()
	c.RegisterSpanExporter(exp)

	spans := []*Span{
		{Name: SpanNameToolCall},
		{Name: SpanNameModelInfer},
	}

	err := c.FlushSpans(context.Background(), spans)
	if err != nil {
		t.Fatalf("FlushSpans: %v", err)
	}

	exp.mu.Lock()
	defer exp.mu.Unlock()
	if len(exp.spans) != 2 {
		t.Errorf("expected 2 exported spans, got %d", len(exp.spans))
	}
}

func TestCollector_FlushCostRecords(t *testing.T) {
	exp := &mockCostExporter{}
	c := NewCollector()
	c.RegisterCostExporter(exp)

	records := []CostRecord{
		{Workspace: "ws-1", CostUnits: 100},
		{Workspace: "ws-1", CostUnits: 200},
	}

	err := c.FlushCostRecords(context.Background(), records)
	if err != nil {
		t.Fatalf("FlushCostRecords: %v", err)
	}

	exp.mu.Lock()
	defer exp.mu.Unlock()
	if len(exp.records) != 2 {
		t.Errorf("expected 2 exported cost records, got %d", len(exp.records))
	}
}

func TestCollector_FlushSpans_ErrorPropagation(t *testing.T) {
	exp := &mockSpanExporter{err: context.DeadlineExceeded}
	c := NewCollector()
	c.RegisterSpanExporter(exp)

	err := c.FlushSpans(context.Background(), []*Span{{Name: "test"}})
	if err == nil {
		t.Error("expected error from failing exporter")
	}
}

func TestCollector_Shutdown(t *testing.T) {
	spanExp := &mockSpanExporter{}
	costExp := &mockCostExporter{}
	c := NewCollector()
	c.RegisterSpanExporter(spanExp)
	c.RegisterCostExporter(costExp)

	err := c.Shutdown(context.Background())
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// =============================================================================
// CognitiveAuditLog.RecordBatch test
// =============================================================================

func TestCognitiveAuditLog_RecordBatch(t *testing.T) {
	store := &mockAuditStore{}
	log := NewCognitiveAuditLog(store)
	ctx := context.Background()

	entries := []AuditEntry{
		{Workspace: "batch-ws", EventType: AuditToolCall, SubjectRef: "tool-1"},
		{Workspace: "batch-ws", EventType: AuditToolCall, SubjectRef: "tool-2"},
		{Workspace: "batch-ws", EventType: AuditSpecChange, SubjectRef: "agent-1"},
	}

	err := log.RecordBatch(ctx, entries)
	if err != nil {
		t.Fatalf("RecordBatch: %v", err)
	}

	results, _ := log.Query(ctx, AuditFilter{Workspace: "batch-ws"})
	if len(results) != 3 {
		t.Fatalf("expected 3 entries after batch, got %d", len(results))
	}

	// Verify timestamps were set.
	for _, e := range results {
		if e.Timestamp.IsZero() {
			t.Error("expected timestamp to be set by RecordBatch")
		}
	}
}

// =============================================================================
// Redaction strict level test
// =============================================================================

func TestRedactor_StrictLevel(t *testing.T) {
	config := &RedactionConfig{
		Level:          RedactionStrict,
		Rules:          DefaultRules(),
		FieldOverrides: make(map[RedactableField]RedactionLevel),
	}
	redactor := NewRedactor(config)

	// Strict level should redact all content regardless of PII patterns.
	plainText := "This has no PII at all"
	result := redactor.Redact(FieldToolArgs, plainText)
	if result != "[REDACTED]" {
		t.Errorf("strict redaction: expected '[REDACTED]', got %q", result)
	}

	// Verify field override takes precedence.
	config.FieldOverrides[FieldToolResult] = RedactionNone
	result2 := redactor.Redact(FieldToolResult, "sensitive@email.com")
	if result2 != "sensitive@email.com" {
		t.Errorf("field override to None: expected passthrough, got %q", result2)
	}
}

// =============================================================================
// Helpers
// =============================================================================

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && stringContains(s, substr))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
