// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelgateway"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

type canaryExecutorStore struct {
	*memStore
	operation            store.CanaryOperation
	hasOperation         bool
	tombstone            store.CanaryOperation
	hasTombstone         bool
	claimCalls           int
	reservationCalls     int
	reservationError     error
	settlementCalls      int
	settlementEffects    int
	settlementHook       func(context.Context) error
	providerFenceError   error
	providerResultError  error
	rejectProviderResult bool
	releaseCalls         int
	releaseEffects       int
	settledReservations  map[string]store.CanaryReservationDisposition
	ackFailures          map[store.CanaryFinalizationStep]int
	usageEvents          []observability.CostRecord
	auditEntries         []observability.AuditEntry
}

func (s *canaryExecutorStore) GetCanaryOperation(_ context.Context, operationID string) (store.CanaryOperation, error) {
	if s.hasOperation && s.operation.OperationID == operationID {
		return s.operation, nil
	}
	if s.hasTombstone && s.tombstone.OperationID == operationID {
		return s.tombstone, nil
	}
	return store.CanaryOperation{}, store.ErrNotFound
}

func (s *canaryExecutorStore) ClaimCanaryOperation(_ context.Context, candidate store.CanaryOperation) (bool, store.CanaryOperation, error) {
	s.claimCalls++
	if s.hasOperation {
		if s.operation.RequestDigest != candidate.RequestDigest {
			return false, store.CanaryOperation{}, store.ErrConflict
		}
		return false, s.operation, nil
	}
	if s.hasTombstone {
		if s.tombstone.RequestDigest != candidate.RequestDigest {
			return false, store.CanaryOperation{}, store.ErrConflict
		}
		return false, s.tombstone, nil
	}
	now := time.Now().UTC()
	candidate.CreatedAt, candidate.UpdatedAt = now, now
	s.operation, s.hasOperation = candidate, true
	return true, candidate, nil
}

func (s *canaryExecutorStore) TransitionCanaryOperation(_ context.Context, operationID, digest string, from, to store.CanaryOperationPhase, terminal *store.CanaryTerminalRecord) (bool, store.CanaryOperation, error) {
	if !s.hasOperation || s.operation.OperationID != operationID {
		return false, store.CanaryOperation{}, store.ErrNotFound
	}
	if s.operation.RequestDigest != digest {
		return false, store.CanaryOperation{}, store.ErrConflict
	}
	if s.operation.Phase != from {
		return false, s.operation, nil
	}
	if from == store.CanaryClaimed && to == store.CanaryProviderStarted && s.providerFenceError != nil {
		return false, s.operation, s.providerFenceError
	}
	if from == store.CanaryProviderStarted && to == store.CanaryProviderCompleted && (s.providerResultError != nil || s.rejectProviderResult) {
		return false, s.operation, s.providerResultError
	}
	s.operation.Phase = to
	s.operation.UpdatedAt = time.Now().UTC()
	if terminal != nil {
		s.operation.Result = append(json.RawMessage(nil), terminal.Result...)
		s.operation.UsageEvent = append(json.RawMessage(nil), terminal.UsageEvent...)
		s.operation.AuditEvent = append(json.RawMessage(nil), terminal.AuditEvent...)
		s.operation.ReservationCreated = terminal.ReservationCreated
	}
	return true, s.operation, nil
}

func (s *canaryExecutorStore) AcknowledgeCanaryFinalization(_ context.Context, operationID, digest string, step store.CanaryFinalizationStep) (store.CanaryOperation, error) {
	if !s.hasOperation || s.operation.OperationID != operationID {
		return store.CanaryOperation{}, store.ErrNotFound
	}
	if s.operation.RequestDigest != digest {
		return store.CanaryOperation{}, store.ErrConflict
	}
	if s.operation.Phase != store.CanaryFinalizing && s.operation.Phase != store.CanaryCompleted {
		return store.CanaryOperation{}, errors.New("operation is not finalizing")
	}
	if s.ackFailures[step] > 0 {
		s.ackFailures[step]--
		return store.CanaryOperation{}, errors.New("injected acknowledgement failure")
	}
	switch step {
	case store.CanaryReservationFinalized:
		s.operation.ReservationFinalized = true
	case store.CanaryUsageApplied:
		s.operation.UsageApplied = true
	case store.CanaryAuditDelivered:
		s.operation.AuditDelivered = true
	default:
		return store.CanaryOperation{}, errors.New("unknown finalization step")
	}
	return s.operation, nil
}

func (s *canaryExecutorStore) CompactCanaryOperationsBefore(_ context.Context, cutoff time.Time) (int64, error) {
	if !s.hasOperation || s.hasTombstone || s.operation.Phase != store.CanaryCompleted || !s.operation.UpdatedAt.Before(cutoff) ||
		!s.operation.ReservationFinalized || !s.operation.AuditDelivered || (len(s.operation.UsageEvent) > 0 && !s.operation.UsageApplied) {
		return 0, nil
	}
	s.tombstone = store.CanaryOperation{
		OperationID:          s.operation.OperationID,
		RequestDigest:        s.operation.RequestDigest,
		Phase:                store.CanaryCompleted,
		Result:               append(json.RawMessage(nil), s.operation.Result...),
		ReservationFinalized: true,
		UsageApplied:         true,
		AuditDelivered:       true,
		CreatedAt:            s.operation.CreatedAt,
		UpdatedAt:            s.operation.UpdatedAt,
	}
	s.hasTombstone = true
	s.operation = store.CanaryOperation{}
	s.hasOperation = false
	return 1, nil
}

func (s *canaryExecutorStore) CreateCanaryReservation(_ context.Context, _ store.TokenReservation) (bool, error) {
	s.reservationCalls++
	if s.reservationError != nil {
		return false, s.reservationError
	}
	return s.reservationCalls == 1, nil
}

func (s *canaryExecutorStore) SettleCanaryReservation(ctx context.Context, reservationID string, input, output int64) (store.CanaryReservationDisposition, error) {
	s.settlementCalls++
	if s.settlementHook != nil {
		if err := s.settlementHook(ctx); err != nil {
			return store.CanaryReservationDisposition{}, err
		}
	}
	if disposition, ok := s.settledReservations[reservationID]; ok {
		return disposition, nil
	}
	disposition := store.CanaryReservationDisposition{ReservationID: reservationID, State: store.CanaryReservationSettled, InputTokens: input, OutputTokens: output}
	if s.settledReservations == nil {
		s.settledReservations = make(map[string]store.CanaryReservationDisposition)
	}
	s.settledReservations[reservationID] = disposition
	s.settlementEffects++
	return disposition, nil
}

func (s *canaryExecutorStore) ReleaseCanaryReservation(_ context.Context, reservationID string) (store.CanaryReservationDisposition, error) {
	s.releaseCalls++
	if disposition, ok := s.settledReservations[reservationID]; ok {
		return disposition, nil
	}
	disposition := store.CanaryReservationDisposition{ReservationID: reservationID, State: store.CanaryReservationReleased}
	if s.settledReservations == nil {
		s.settledReservations = make(map[string]store.CanaryReservationDisposition)
	}
	s.settledReservations[reservationID] = disposition
	s.releaseEffects++
	return disposition, nil
}

func (s *canaryExecutorStore) ApplyUsageEvent(_ context.Context, event observability.CostRecord) (bool, error) {
	for _, existing := range s.usageEvents {
		if existing.ID == event.ID {
			return false, nil
		}
	}
	s.usageEvents = append(s.usageEvents, event)
	return true, nil
}

func (s *canaryExecutorStore) Append(_ context.Context, entry observability.AuditEntry) error {
	for _, existing := range s.auditEntries {
		if existing.ID == entry.ID {
			return nil
		}
	}
	s.auditEntries = append(s.auditEntries, entry)
	return nil
}

func (s *canaryExecutorStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	for _, entry := range entries {
		if err := s.Append(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *canaryExecutorStore) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return append([]observability.AuditEntry(nil), s.auditEntries...), nil
}

func newCanaryExecutorFixture(t *testing.T, driver *fakeCPIDriver) (*CPICanaryExecutor, *canaryExecutorStore, CanaryRequest) {
	t.Helper()
	base := newMemStore()
	seedCPIResources(t, base)
	st := &canaryExecutorStore{memStore: base}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	executor, err := NewCPICanaryExecutor(st, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := CanaryRequest{
		OperationID: "operation-1", RequestDigest: "sha256:request", MaxInputTokens: 100, MaxOutputTokens: 16,
		CanaryTask: "Return READY", ExpectedOutcome: "READY",
		Instance: &v1alpha1.AgentInstance{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "instance", Namespace: "default", UID: "instance-uid"},
			Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}},
		},
	}
	prepared, err := executor.PrepareCanary(context.Background(), request)
	if err != nil {
		t.Fatalf("PrepareCanary() error = %v", err)
	}
	return executor, st, prepared
}

func TestEvaluateCanaryResponse_PrecedenceAndAccounting(t *testing.T) {
	request := CanaryRequest{OperationID: "operation-1", ExpectedOutcome: "READY", MaxOutputTokens: 16}
	validUsage := cpi.Usage{PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9, Reported: true}
	tests := []struct {
		name          string
		response      *cpi.CompletionResponse
		wantOutcome   CanaryOutcome
		wantReason    v1alpha1.CanaryFailureReason
		wantEstimated bool
	}{
		{name: "exact bytes pass", response: &cpi.CompletionResponse{Content: "READY", Usage: validUsage}, wantOutcome: CanaryOutcomePassed},
		{name: "different bytes mismatch", response: &cpi.CompletionResponse{Content: "READY\n", Usage: validUsage}, wantOutcome: CanaryOutcomeMismatch},
		{name: "tool call precedes output and usage", response: &cpi.CompletionResponse{Content: strings.Repeat("x", maxCanaryOutcomeBytes+1), ToolCalls: []cpi.ToolCall{{Name: "unsafe"}}}, wantOutcome: CanaryOutcomeExecutionFailed, wantReason: v1alpha1.CanaryFailureToolCallRequested, wantEstimated: true},
		{name: "output limit precedes encoding and usage", response: &cpi.CompletionResponse{Content: strings.Repeat("x", maxCanaryOutcomeBytes) + string([]byte{0xff})}, wantOutcome: CanaryOutcomeExecutionFailed, wantReason: v1alpha1.CanaryFailureOutputLimitExceeded, wantEstimated: true},
		{name: "encoding precedes usage", response: &cpi.CompletionResponse{Content: string([]byte{0xff})}, wantOutcome: CanaryOutcomeExecutionFailed, wantReason: v1alpha1.CanaryFailureInvalidEncoding, wantEstimated: true},
		{name: "missing usage is uncertain", response: &cpi.CompletionResponse{Content: "READY"}, wantOutcome: CanaryOutcomeExecutionFailed, wantReason: v1alpha1.CanaryFailureAccountingUncertain, wantEstimated: true},
		{name: "inconsistent usage is uncertain", response: &cpi.CompletionResponse{Content: "READY", Usage: cpi.Usage{PromptTokens: 7, CompletionTokens: 2, TotalTokens: 8, Reported: true}}, wantOutcome: CanaryOutcomeExecutionFailed, wantReason: v1alpha1.CanaryFailureAccountingUncertain, wantEstimated: true},
		{name: "nil response is uncertain", wantOutcome: CanaryOutcomeExecutionFailed, wantReason: v1alpha1.CanaryFailureAccountingUncertain, wantEstimated: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, estimated := evaluateCanaryResponse(request, test.response, 7, time.Unix(1, 0), time.Unix(2, 0))
			if result.Outcome != test.wantOutcome || result.ClosedReason != test.wantReason || estimated != test.wantEstimated {
				t.Fatalf("evaluateCanaryResponse() = (%s, %q, estimated=%t), want (%s, %q, estimated=%t)", result.Outcome, result.ClosedReason, estimated, test.wantOutcome, test.wantReason, test.wantEstimated)
			}
			if estimated && (result.PromptTokens != 7 || result.CompletionTokens != 16 || result.TotalTokens != 23) {
				t.Fatalf("estimated usage = (%d, %d, %d), want ceilings (7, 16, 23)", result.PromptTokens, result.CompletionTokens, result.TotalTokens)
			}
		})
	}
}

func mutateCanaryResourceIdentity(t *testing.T, st *canaryExecutorStore, kind, namespace, name string) {
	t.Helper()
	raw, err := st.Get(context.Background(), kind, namespace, name)
	if err != nil {
		t.Fatalf("get %s %s: %v", kind, name, err)
	}
	var resource map[string]any
	if err := json.Unmarshal(raw, &resource); err != nil {
		t.Fatalf("unmarshal %s %s: %v", kind, name, err)
	}
	metadata := resource["metadata"].(map[string]any)
	metadata["uid"] = "changed-uid"
	updated, err := json.Marshal(resource)
	if err != nil {
		t.Fatalf("marshal %s %s: %v", kind, name, err)
	}
	version := st.versions[st.key(kind, namespace, name)]
	if err := st.Update(context.Background(), kind, namespace, name, updated, version); err != nil {
		t.Fatalf("update %s %s: %v", kind, name, err)
	}
}

func configureCanaryPrompt(t *testing.T, st *canaryExecutorStore) {
	t.Helper()
	raw, err := st.Get(context.Background(), "Agent", "default", "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	var agent v1alpha1.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		t.Fatal(err)
	}
	agent.Spec.Instructions = v1alpha1.AgentInstructions{Ref: &v1alpha1.LocalObjectReference{Name: "canary-prompt"}}
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(context.Background(), "Agent", "default", "test-agent", data, agent.ObjectMeta.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	prompt := v1alpha1.Prompt{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Prompt"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "canary-prompt", Namespace: "default", UID: "prompt-uid"},
		Spec:       v1alpha1.PromptSpec{Content: "private canary prompt content"},
	}
	data, err = json.Marshal(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Create(context.Background(), "Prompt", "default", "canary-prompt", data); err != nil {
		t.Fatal(err)
	}
}

func TestCPICanaryExecutor_PreflightFailureHasNoDurableSideEffects(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11}
	executor, st, request := newCanaryExecutorFixture(t, driver)
	driver.exactErr = errors.New("unsupported model")

	result, err := executor.ExecuteCanary(context.Background(), request)
	if err == nil || result.ClosedReason != v1alpha1.CanaryFailureInvalidConfiguration {
		t.Fatalf("ExecuteCanary() = (%+v, %v), want exact-tokenization failure", result, err)
	}
	if st.claimCalls != 0 || st.reservationCalls != 0 || driver.calls != 0 {
		t.Fatalf("side effects = claims %d reservations %d provider calls %d, want zero", st.claimCalls, st.reservationCalls, driver.calls)
	}
}

func TestCPICanaryExecutor_EmitsOneCorrelatedMetadataOnlySpanPerClaim(t *testing.T) {
	driver := &fakeCPIDriver{
		name:        "mock",
		exactTokens: 11,
		response:    &cpi.CompletionResponse{Content: "READY", Usage: cpi.Usage{PromptTokens: 11, CompletionTokens: 1, TotalTokens: 12, Reported: true}},
	}
	executor, st, request := newCanaryExecutorFixture(t, driver)
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0})
	collector := observability.NewCollector()
	exporter := &workflowSpanExporter{}
	collector.RegisterSpanExporter(exporter)
	executor.SetTracing(tracer, collector)

	result, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || result.Outcome != CanaryOutcomePassed {
		t.Fatalf("ExecuteCanary() = (%+v, %v), want passed result", result, err)
	}
	if len(exporter.spans) != 1 {
		t.Fatalf("exported spans = %d, want one", len(exporter.spans))
	}
	span := exporter.spans[0]
	if span.Name != observability.SpanNameReasoningProbe || span.StatusCode != observability.StatusOK {
		t.Fatalf("span = (%q, %v), want reasoning probe with OK status", span.Name, span.StatusCode)
	}
	wantAttributes := map[string]interface{}{
		observability.AttrReasoningProbeOperationID: request.OperationID,
		observability.AttrAgentName:                 "test-agent",
		observability.AttrAgentNamespace:            "default",
		observability.AttrAgentInstanceName:         "instance",
		observability.AttrAgentRevision:             request.AgentRevision,
		observability.AttrModelName:                 "test-model",
	}
	for key, want := range wantAttributes {
		if got := span.Attributes[key]; got != want {
			t.Errorf("span attribute %q = %#v, want %#v", key, got, want)
		}
	}
	attributes, err := json.Marshal(span.Attributes)
	if err != nil {
		t.Fatal(err)
	}
	for _, prohibited := range []string{request.Instructions, request.CanaryTask, request.ExpectedOutcome, driver.response.Content} {
		if prohibited != "" && strings.Contains(string(attributes), prohibited) {
			t.Errorf("span attributes contain prohibited canary content %q", prohibited)
		}
	}
	wantTraceID := observability.HexTraceID(span.Context.TraceID)
	wantSpanID := observability.HexSpanID(span.Context.SpanID)
	if result.TraceID != wantTraceID || result.SpanID != wantSpanID {
		t.Fatalf("result correlation = (%q, %q), want (%q, %q)", result.TraceID, result.SpanID, wantTraceID, wantSpanID)
	}
	if len(st.usageEvents) != 1 || st.usageEvents[0].TraceID != wantTraceID || st.usageEvents[0].SpanID != wantSpanID {
		t.Fatalf("usage correlation = %+v, want trace %q span %q", st.usageEvents, wantTraceID, wantSpanID)
	}
	if st.usageEvents[0].Purpose != observability.UsagePurposeReasoningProbe {
		t.Fatalf("usage purpose = %q, want %q", st.usageEvents[0].Purpose, observability.UsagePurposeReasoningProbe)
	}
	if !st.usageEvents[0].UsageReported || st.usageEvents[0].UsageEstimated {
		t.Fatal("reported provider usage was not classified as reported")
	}
	if len(st.auditEntries) != 1 || st.auditEntries[0].TraceID != wantTraceID || st.auditEntries[0].SpanID != wantSpanID {
		t.Fatalf("audit correlation = %+v, want trace %q span %q", st.auditEntries, wantTraceID, wantSpanID)
	}

	replayed, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || replayed != result {
		t.Fatalf("replay ExecuteCanary() = (%+v, %v), want %+v", replayed, err, result)
	}
	if len(exporter.spans) != 1 {
		t.Fatalf("exported spans after replay = %d, want one", len(exporter.spans))
	}
}

type failingCanarySpanExporter struct {
	calls int
}

func (e *failingCanarySpanExporter) ExportSpans(context.Context, []*observability.Span) error {
	e.calls++
	return errors.New("injected span export failure")
}

func (*failingCanarySpanExporter) Shutdown(context.Context) error { return nil }

func TestCPICanaryExecutor_DoesNotExportProviderErrorDetails(t *testing.T) {
	const sensitiveError = "provider rejected credential secret-token-123"
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11, err: errors.New(sensitiveError)}
	executor, _, request := newCanaryExecutorFixture(t, driver)
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0})
	collector := observability.NewCollector()
	exporter := &workflowSpanExporter{}
	collector.RegisterSpanExporter(exporter)
	executor.SetTracing(tracer, collector)

	if _, err := executor.ExecuteCanary(context.Background(), request); err == nil {
		t.Fatal("ExecuteCanary() error = nil, want provider failure")
	}
	if len(exporter.spans) != 1 {
		t.Fatalf("exported spans = %d, want one", len(exporter.spans))
	}
	span := exporter.spans[0]
	if span.StatusCode != observability.StatusError || span.StatusMessage != "reasoning probe canary failed" {
		t.Fatalf("span status = (%v, %q), want bounded canary failure status", span.StatusCode, span.StatusMessage)
	}
	encodedSpan, err := json.Marshal(span)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedSpan), sensitiveError) {
		t.Fatal("exported span contains provider error details")
	}
}

func TestCPICanaryExecutor_SpanExportFailureDoesNotBlockFinalization(t *testing.T) {
	driver := &fakeCPIDriver{
		name:        "mock",
		exactTokens: 11,
		response:    &cpi.CompletionResponse{Content: "READY", Usage: cpi.Usage{PromptTokens: 11, CompletionTokens: 1, TotalTokens: 12, Reported: true}},
	}
	executor, st, request := newCanaryExecutorFixture(t, driver)
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0})
	collector := observability.NewCollector()
	exporter := &failingCanarySpanExporter{}
	collector.RegisterSpanExporter(exporter)
	executor.SetTracing(tracer, collector)

	result, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || result.Outcome != CanaryOutcomePassed {
		t.Fatalf("ExecuteCanary() = (%+v, %v), want passed result despite export failure", result, err)
	}
	if exporter.calls != 1 || st.operation.Phase != store.CanaryCompleted {
		t.Fatalf("export calls = %d, phase = %s; want one export attempt and completed operation", exporter.calls, st.operation.Phase)
	}
	if driver.calls != 1 || st.settlementEffects != 1 || len(st.usageEvents) != 1 || len(st.auditEntries) != 1 {
		t.Fatalf("effects = provider:%d settlement:%d usage:%d audit:%d; want one each", driver.calls, st.settlementEffects, len(st.usageEvents), len(st.auditEntries))
	}
}

func TestCPICanaryExecutor_DoesNotReplayProviderStarted(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11, err: errors.New("provider connection lost")}
	executor, st, request := newCanaryExecutorFixture(t, driver)

	first, err := executor.ExecuteCanary(context.Background(), request)
	if !errors.Is(err, ErrCanaryOutcomeUncertain) || !errors.Is(err, driver.err) || first.ClosedReason != v1alpha1.CanaryFailureAccountingUncertain || st.operation.Phase != store.CanaryProviderStarted {
		t.Fatalf("first ExecuteCanary() = (%+v, %v), phase = %s; want uncertain ProviderStarted", first, err, st.operation.Phase)
	}
	if len(st.operation.Result) == 0 || len(st.operation.UsageEvent) == 0 || len(st.operation.AuditEvent) == 0 {
		t.Fatal("provider fence did not persist complete terminal fallback")
	}
	st.memStore = newMemStore()

	result, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || result.ClosedReason != v1alpha1.CanaryFailureAccountingUncertain {
		t.Fatalf("ExecuteCanary() = (%+v, %v), want completed uncertain non-replay result", result, err)
	}
	if driver.calls != 1 || st.reservationCalls != 1 {
		t.Fatalf("provider calls = %d, reservations = %d; want no calls after the initial fenced attempt", driver.calls, st.reservationCalls)
	}
	if st.settlementCalls != 1 || st.operation.Phase != store.CanaryCompleted {
		t.Fatalf("settlements = %d, phase = %s; want one settlement and completed", st.settlementCalls, st.operation.Phase)
	}
	if result.PromptTokens != 11 || result.CompletionTokens != int64(request.MaxOutputTokens) {
		t.Fatalf("settled tokens = (%d, %d), want exact input and output ceiling", result.PromptTokens, result.CompletionTokens)
	}
	if len(st.usageEvents) != 1 || st.usageEvents[0].Purpose != observability.UsagePurposeReasoningProbe || st.usageEvents[0].UsageReported || !st.usageEvents[0].UsageEstimated {
		t.Fatalf("ambiguous usage = %+v, want estimated reasoning-probe usage", st.usageEvents)
	}
}

func TestCPICanaryExecutor_ProviderResultPersistenceFailureDoesNotReplay(t *testing.T) {
	for _, rejectTransition := range []bool{false, true} {
		t.Run(fmt.Sprintf("transitionRejected=%t", rejectTransition), func(t *testing.T) {
			driver := &fakeCPIDriver{name: "mock", exactTokens: 11}
			executor, st, request := newCanaryExecutorFixture(t, driver)
			writeFailure := errors.New("provider result unavailable")
			st.rejectProviderResult = rejectTransition
			if !rejectTransition {
				st.providerResultError = writeFailure
			}
			_, err := executor.ExecuteCanary(context.Background(), request)
			if !errors.Is(err, ErrCanaryOutcomeUncertain) {
				t.Fatalf("ExecuteCanary() error = %v, want uncertain outcome", err)
			}
			if rejectTransition {
				if !strings.Contains(err.Error(), "transition rejected") {
					t.Fatalf("rejected transition error = %v", err)
				}
			} else if !errors.Is(err, writeFailure) {
				t.Fatalf("persistence error not retained: %v", err)
			}
			if driver.calls != 1 || st.operation.Phase != store.CanaryProviderStarted {
				t.Fatalf("provider calls = %d, phase = %s", driver.calls, st.operation.Phase)
			}
			st.providerResultError = nil
			st.rejectProviderResult = false
			result, err := executor.ExecuteCanary(context.Background(), request)
			if err != nil || result.ClosedReason != v1alpha1.CanaryFailureAccountingUncertain || driver.calls != 1 || st.settlementEffects != 1 {
				t.Fatalf("retry = (%+v, %v), provider calls = %d, settlements = %d", result, err, driver.calls, st.settlementEffects)
			}
		})
	}
}

func TestCPICanaryExecutor_OwnsConfiguredLifecycleTimeout(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11}
	executor, st, request := newCanaryExecutorFixture(t, driver)
	request.Timeout = time.Minute
	request, err := executor.PrepareCanary(context.Background(), request)
	if err != nil {
		t.Fatalf("PrepareCanary() error = %v", err)
	}
	deadlineObserved := false
	driver.complete = func(ctx context.Context, _ *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("canary execution context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > request.Timeout {
			return nil, fmt.Errorf("canary execution deadline remaining = %s, want within %s", remaining, request.Timeout)
		}
		deadlineObserved = true
		return nil, context.DeadlineExceeded
	}

	result, err := executor.ExecuteCanary(context.Background(), request)
	if !deadlineObserved {
		t.Fatal("ExecuteCanary() did not propagate the configured lifecycle deadline")
	}
	if !errors.Is(err, ErrCanaryOutcomeUncertain) || !errors.Is(err, context.DeadlineExceeded) || result.ClosedReason != v1alpha1.CanaryFailureAccountingUncertain {
		t.Fatalf("ExecuteCanary() = (%+v, %v), want replay-safe uncertain timeout", result, err)
	}
	if driver.calls != 1 || st.operation.Phase != store.CanaryProviderStarted {
		t.Fatalf("provider calls = %d, phase = %s; want one fenced call", driver.calls, st.operation.Phase)
	}

	result, err = executor.ExecuteCanary(context.Background(), request)
	if err != nil || result.ClosedReason != v1alpha1.CanaryFailureAccountingUncertain {
		t.Fatalf("retry ExecuteCanary() = (%+v, %v), want completed uncertain result", result, err)
	}
	if driver.calls != 1 || st.operation.Phase != store.CanaryCompleted || st.settlementEffects != 1 {
		t.Fatalf("effects = provider:%d phase:%s settlement:%d; want one provider call and one completed settlement", driver.calls, st.operation.Phase, st.settlementEffects)
	}
}

func TestCPICanaryExecutor_ResumesFinalizationAfterLifecycleTimeout(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11, response: &cpi.CompletionResponse{
		Content: "READY", Usage: cpi.Usage{PromptTokens: 11, CompletionTokens: 2, TotalTokens: 13, Reported: true},
	}}
	executor, st, request := newCanaryExecutorFixture(t, driver)
	request.Timeout = 50 * time.Millisecond
	request, err := executor.PrepareCanary(context.Background(), request)
	if err != nil {
		t.Fatalf("PrepareCanary() error = %v", err)
	}
	settlementAttempts := 0
	freshDeadlineObserved := false
	st.settlementHook = func(ctx context.Context) error {
		settlementAttempts++
		if settlementAttempts == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		deadline, ok := ctx.Deadline()
		freshDeadlineObserved = ok && time.Until(deadline) > 0
		return nil
	}

	first, err := executor.ExecuteCanary(context.Background(), request)
	if !errors.Is(err, context.DeadlineExceeded) || first.Outcome != CanaryOutcomePassed {
		t.Fatalf("first ExecuteCanary() = (%+v, %v), want passed result with finalization timeout", first, err)
	}
	if st.operation.Phase != store.CanaryFinalizing || driver.calls != 1 || st.settlementEffects != 0 {
		t.Fatalf("first attempt = phase:%s provider:%d settlement:%d; want durable finalizing intent after one provider call", st.operation.Phase, driver.calls, st.settlementEffects)
	}

	result, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || result != first || st.operation.Phase != store.CanaryCompleted {
		t.Fatalf("retry ExecuteCanary() = (%+v, %v), phase = %s; want identical completed result", result, err, st.operation.Phase)
	}
	if !freshDeadlineObserved {
		t.Fatal("retry finalization did not receive a fresh lifecycle deadline")
	}
	if driver.calls != 1 || st.settlementCalls != 2 || st.settlementEffects != 1 || len(st.usageEvents) != 1 || len(st.auditEntries) != 1 {
		t.Fatalf("effects = provider:%d settlement calls:%d effects:%d usage:%d audit:%d; want one provider and one finalization effect", driver.calls, st.settlementCalls, st.settlementEffects, len(st.usageEvents), len(st.auditEntries))
	}
}

func TestCPICanaryExecutor_DurablyFinalizesReservationDenial(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11}
	executor, st, request := newCanaryExecutorFixture(t, driver)
	st.reservationError = errors.New("quota exhausted")

	first, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || first.Outcome != CanaryOutcomeExecutionFailed || first.ClosedReason != v1alpha1.CanaryFailureBudgetDenied {
		t.Fatalf("first ExecuteCanary() = (%+v, %v), want durable budget denial", first, err)
	}
	if st.operation.Phase != store.CanaryCompleted || st.operation.ReservationCreated || len(st.operation.UsageEvent) != 0 {
		t.Fatalf("operation = %#v, want completed pre-call result without reservation or usage", st.operation)
	}
	if driver.calls != 0 || st.releaseCalls != 0 || st.settlementCalls != 0 || len(st.usageEvents) != 0 || len(st.auditEntries) != 1 {
		t.Fatalf("effects = provider:%d release:%d settle:%d usage:%d audit:%d, want only one audit", driver.calls, st.releaseCalls, st.settlementCalls, len(st.usageEvents), len(st.auditEntries))
	}

	st.reservationError = nil
	replayed, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || replayed != first {
		t.Fatalf("retry ExecuteCanary() = (%+v, %v), want identical result %+v", replayed, err, first)
	}
	if st.reservationCalls != 1 || driver.calls != 0 || len(st.auditEntries) != 1 {
		t.Fatalf("retry effects = reservations:%d provider:%d audit:%d, want no repeated effects", st.reservationCalls, driver.calls, len(st.auditEntries))
	}
}

func TestCPICanaryExecutor_DurablyFinalizesPreCallTimeout(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11}
	executor, st, request := newCanaryExecutorFixture(t, driver)
	st.reservationError = context.DeadlineExceeded

	first, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || first.Outcome != CanaryOutcomeExecutionFailed || first.ClosedReason != v1alpha1.CanaryFailureTimedOut {
		t.Fatalf("first ExecuteCanary() = (%+v, %v), want durable pre-call timeout", first, err)
	}
	if st.operation.Phase != store.CanaryCompleted || st.operation.ReservationCreated || len(st.operation.UsageEvent) != 0 {
		t.Fatalf("operation = %#v, want completed pre-call result without reservation or usage", st.operation)
	}
	if driver.calls != 0 || st.releaseCalls != 0 || st.settlementCalls != 0 || len(st.usageEvents) != 0 || len(st.auditEntries) != 1 {
		t.Fatalf("effects = provider:%d release:%d settle:%d usage:%d audit:%d, want only one audit", driver.calls, st.releaseCalls, st.settlementCalls, len(st.usageEvents), len(st.auditEntries))
	}

	st.reservationError = nil
	replayed, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || replayed != first {
		t.Fatalf("retry ExecuteCanary() = (%+v, %v), want identical result %+v", replayed, err, first)
	}
	if st.reservationCalls != 1 || driver.calls != 0 || len(st.auditEntries) != 1 {
		t.Fatalf("retry effects = reservations:%d provider:%d audit:%d, want no repeated effects", st.reservationCalls, driver.calls, len(st.auditEntries))
	}
}

func TestCPICanaryExecutor_DurablyFinalizesAuthorizationDenial(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11}
	executor, st, request := newCanaryExecutorFixture(t, driver)
	core, err := modelgateway.NewCore(st, cpiModelAuthorizer{err: errors.New("ModelGrant revoked")})
	if err != nil {
		t.Fatal(err)
	}
	executor.core = core

	first, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || first.Outcome != CanaryOutcomeExecutionFailed || first.ClosedReason != v1alpha1.CanaryFailureAuthorizationDenied {
		t.Fatalf("first ExecuteCanary() = (%+v, %v), want durable authorization denial", first, err)
	}
	if st.operation.Phase != store.CanaryCompleted || st.operation.ReservationCreated || len(st.operation.UsageEvent) != 0 {
		t.Fatalf("operation = %#v, want completed pre-call result without reservation or usage", st.operation)
	}
	if st.reservationCalls != 0 || driver.calls != 0 || st.releaseCalls != 0 || st.settlementCalls != 0 || len(st.usageEvents) != 0 || len(st.auditEntries) != 1 {
		t.Fatalf("effects = reservation:%d provider:%d release:%d settle:%d usage:%d audit:%d, want only one audit", st.reservationCalls, driver.calls, st.releaseCalls, st.settlementCalls, len(st.usageEvents), len(st.auditEntries))
	}

	replayed, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || replayed != first {
		t.Fatalf("retry ExecuteCanary() = (%+v, %v), want identical result %+v", replayed, err, first)
	}
	if st.claimCalls != 1 || st.reservationCalls != 0 || driver.calls != 0 || len(st.auditEntries) != 1 {
		t.Fatalf("retry effects = claims:%d reservations:%d provider:%d audit:%d, want no repeated effects", st.claimCalls, st.reservationCalls, driver.calls, len(st.auditEntries))
	}
}

func TestCPICanaryExecutor_AnotherExecutorReleasesReservationAfterPreCallCrash(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11}
	firstExecutor, st, request := newCanaryExecutorFixture(t, driver)
	st.providerFenceError = errors.New("injected crash before provider fence")

	first, err := firstExecutor.ExecuteCanary(context.Background(), request)
	if !errors.Is(err, ErrCanaryOutcomeUncertain) || !errors.Is(err, st.providerFenceError) || first.ClosedReason != v1alpha1.CanaryFailureAccountingUncertain {
		t.Fatalf("first ExecuteCanary() = (%+v, %v), want uncertain crash result", first, err)
	}
	if st.operation.Phase != store.CanaryClaimed || st.reservationCalls != 1 || driver.calls != 0 {
		t.Fatalf("crash state = phase:%s reservations:%d provider:%d, want claimed with one reservation and no provider", st.operation.Phase, st.reservationCalls, driver.calls)
	}

	st.providerFenceError = nil
	core, err := modelgateway.NewCore(st, cpiModelAuthorizer{err: errors.New("ModelGrant revoked")})
	if err != nil {
		t.Fatal(err)
	}
	secondExecutor, err := NewCPICanaryExecutor(st, firstExecutor.registry, core)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := secondExecutor.ExecuteCanary(context.Background(), request)
	if err != nil || recovered.ClosedReason != v1alpha1.CanaryFailureAuthorizationDenied || st.operation.Phase != store.CanaryCompleted {
		t.Fatalf("recovery ExecuteCanary() = (%+v, %v), phase = %s; want completed authorization denial", recovered, err, st.operation.Phase)
	}
	disposition := st.settledReservations[canaryReservationID(request.OperationID)]
	if disposition.State != store.CanaryReservationReleased || st.releaseEffects != 1 || driver.calls != 0 || len(st.usageEvents) != 0 || len(st.auditEntries) != 1 {
		t.Fatalf("recovery effects = disposition:%+v releases:%d provider:%d usage:%d audit:%d", disposition, st.releaseEffects, driver.calls, len(st.usageEvents), len(st.auditEntries))
	}

	replayed, err := firstExecutor.ExecuteCanary(context.Background(), request)
	if err != nil || replayed != recovered || st.releaseEffects != 1 || driver.calls != 0 || len(st.usageEvents) != 0 || len(st.auditEntries) != 1 {
		t.Fatalf("replay ExecuteCanary() = (%+v, %v), effects release:%d provider:%d usage:%d audit:%d", replayed, err, st.releaseEffects, driver.calls, len(st.usageEvents), len(st.auditEntries))
	}
}

func TestCPICanaryExecutor_RecoversPreCallFinalizationCrashWindows(t *testing.T) {
	tests := []struct {
		name                 string
		reservationFinalized bool
		auditPersisted       bool
		auditDelivered       bool
	}{
		{name: "after terminal intent"},
		{name: "after release", auditPersisted: false},
		{name: "after release acknowledgement", reservationFinalized: true},
		{name: "after audit delivery", reservationFinalized: true, auditPersisted: true},
		{name: "after audit acknowledgement", reservationFinalized: true, auditPersisted: true, auditDelivered: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver := &fakeCPIDriver{name: "mock", exactTokens: 11}
			executor, st, request := newCanaryExecutorFixture(t, driver)
			result := preCallCanaryFailure(request, 11, v1alpha1.CanaryFailureTimedOut)
			resultData, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			terminal, err := buildCanaryPreCallTerminalRecord(request, result, resultData, time.Now().UTC(), true)
			if err != nil {
				t.Fatal(err)
			}
			st.hasOperation = true
			st.operation = store.CanaryOperation{
				OperationID: request.OperationID, RequestDigest: request.RequestDigest, Phase: store.CanaryFinalizing,
				Result: terminal.Result, AuditEvent: terminal.AuditEvent, ReservationCreated: true,
				ReservationFinalized: test.reservationFinalized, AuditDelivered: test.auditDelivered,
			}
			reservationID := canaryReservationID(request.OperationID)
			if test.name != "after terminal intent" {
				st.settledReservations = map[string]store.CanaryReservationDisposition{
					reservationID: {ReservationID: reservationID, State: store.CanaryReservationReleased},
				}
			}
			if test.auditPersisted {
				entry, err := canaryAuditEntry(terminal.AuditEvent)
				if err != nil {
					t.Fatal(err)
				}
				st.auditEntries = []observability.AuditEntry{entry}
			}

			got, err := executor.ExecuteCanary(context.Background(), request)
			if err != nil || got != result || st.operation.Phase != store.CanaryCompleted {
				t.Fatalf("ExecuteCanary() = (%+v, %v), phase = %s; want identical completed result", got, err, st.operation.Phase)
			}
			disposition, ok := st.settledReservations[reservationID]
			if !ok || disposition.State != store.CanaryReservationReleased || len(st.settledReservations) != 1 {
				t.Fatalf("reservation dispositions = %#v, want one released disposition", st.settledReservations)
			}
			if driver.calls != 0 || len(st.usageEvents) != 0 || len(st.auditEntries) != 1 {
				t.Fatalf("effects = provider:%d usage:%d audit:%d, want no provider or usage and one audit", driver.calls, len(st.usageEvents), len(st.auditEntries))
			}
		})
	}
}

func TestCPICanaryExecutor_ExecutesOnlyAfterFenceAndFinalizes(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11, response: &cpi.CompletionResponse{
		Content: "READY", Usage: cpi.Usage{PromptTokens: 11, CompletionTokens: 2, TotalTokens: 13, Reported: true},
	}}
	executor, st, request := newCanaryExecutorFixture(t, driver)

	result, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || result.Outcome != CanaryOutcomePassed {
		t.Fatalf("ExecuteCanary() = (%+v, %v), want passed", result, err)
	}
	if driver.calls != 1 || st.operation.Phase != store.CanaryCompleted || st.settlementCalls != 1 {
		t.Fatalf("calls = %d, phase = %s, settlements = %d", driver.calls, st.operation.Phase, st.settlementCalls)
	}
	if driver.lastReq == nil || len(driver.lastReq.Tools) != 0 || driver.lastReq.Parameters["temperature"] != "0" || driver.lastReq.Parameters["max_tokens"] != "16" {
		t.Fatalf("isolated deterministic request = %#v", driver.lastReq)
	}
}

func TestCPICanaryExecutor_ReturnsStoredResultWithoutProviderCall(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11, response: &cpi.CompletionResponse{
		Content: "READY", Usage: cpi.Usage{PromptTokens: 11, CompletionTokens: 2, TotalTokens: 13, Reported: true},
	}}
	executor, st, request := newCanaryExecutorFixture(t, driver)

	first, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || first.Outcome != CanaryOutcomePassed {
		t.Fatalf("first ExecuteCanary() = (%+v, %v), want passed", first, err)
	}
	replayed, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || replayed != first {
		t.Fatalf("replayed ExecuteCanary() = (%+v, %v), want stored result %+v", replayed, err, first)
	}
	if driver.calls != 1 || st.settlementEffects != 1 || len(st.usageEvents) != 1 || len(st.auditEntries) != 1 {
		t.Fatalf("effects after replay = provider:%d settlement:%d usage:%d audit:%d; want one each", driver.calls, st.settlementEffects, len(st.usageEvents), len(st.auditEntries))
	}
}

func TestCPICanaryExecutor_ReplaysCompactedTombstoneWithoutEffects(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11, response: &cpi.CompletionResponse{
		Content: "READY", Usage: cpi.Usage{PromptTokens: 11, CompletionTokens: 2, TotalTokens: 13, Reported: true},
	}}
	executor, st, request := newCanaryExecutorFixture(t, driver)
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0})
	collector := observability.NewCollector()
	exporter := &workflowSpanExporter{}
	collector.RegisterSpanExporter(exporter)
	executor.SetTracing(tracer, collector)

	first, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || first.Outcome != CanaryOutcomePassed {
		t.Fatalf("first ExecuteCanary() = (%+v, %v), want passed", first, err)
	}
	st.operation.UpdatedAt = time.Now().UTC().Add(-2 * time.Hour)
	compacted, err := st.CompactCanaryOperationsBefore(context.Background(), time.Now().UTC().Add(-time.Hour))
	if err != nil || compacted != 1 || st.hasOperation || !st.hasTombstone {
		t.Fatalf("compaction = (%d, %v), active=%t tombstone=%t", compacted, err, st.hasOperation, st.hasTombstone)
	}
	if len(st.tombstone.UsageEvent) != 0 || len(st.tombstone.AuditEvent) != 0 {
		t.Fatal("tombstone retained secondary evidence")
	}

	replayed, err := executor.ExecuteCanary(context.Background(), request)
	if err != nil || replayed != first {
		t.Fatalf("compacted replay = (%+v, %v), want %+v", replayed, err, first)
	}
	changed := request
	changed.RequestDigest = "sha256:changed"
	if _, err := executor.ExecuteCanary(context.Background(), changed); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed-digest replay error = %v, want ErrConflict", err)
	}
	if driver.calls != 1 || st.reservationCalls != 1 || st.settlementEffects != 1 || st.releaseEffects != 0 || len(st.usageEvents) != 1 || len(st.auditEntries) != 1 || len(exporter.spans) != 1 {
		t.Fatalf("effects after compacted replay = provider:%d reservation:%d settlement:%d release:%d usage:%d audit:%d spans:%d; want one original execution only", driver.calls, st.reservationCalls, st.settlementEffects, st.releaseEffects, len(st.usageEvents), len(st.auditEntries), len(exporter.spans))
	}
}

func TestCPICanaryExecutor_TerminalPayloadsExcludeSensitiveContent(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11, response: &cpi.CompletionResponse{
		Content: "privacy-provider-output-marker", Usage: cpi.Usage{PromptTokens: 11, CompletionTokens: 2, TotalTokens: 13, Reported: true},
	}}
	executor, st, request := newCanaryExecutorFixture(t, driver)
	request.CanaryTask = "privacy-canary-task-marker"
	request.ExpectedOutcome = "privacy-expected-output-marker"
	request, err := executor.PrepareCanary(context.Background(), request)
	if err != nil {
		t.Fatalf("PrepareCanary() error = %v", err)
	}

	if _, err := executor.ExecuteCanary(context.Background(), request); err != nil {
		t.Fatalf("ExecuteCanary() error = %v", err)
	}
	persisted := string(st.operation.Result) + string(st.operation.UsageEvent) + string(st.operation.AuditEvent)
	for _, prohibited := range []string{
		request.CanaryTask,
		request.ExpectedOutcome,
		"You are a helpful assistant.",
		"sk-test-123",
		driver.response.Content,
	} {
		if strings.Contains(persisted, prohibited) {
			t.Fatalf("terminal payloads contain prohibited content %q", prohibited)
		}
	}
}

func TestCPICanaryExecutor_ResumesAfterFinalizationAcknowledgementFailure(t *testing.T) {
	steps := []store.CanaryFinalizationStep{
		store.CanaryReservationFinalized,
		store.CanaryUsageApplied,
		store.CanaryAuditDelivered,
	}
	for _, step := range steps {
		t.Run(string(step), func(t *testing.T) {
			driver := &fakeCPIDriver{name: "mock", exactTokens: 11, response: &cpi.CompletionResponse{
				Content: "READY", Usage: cpi.Usage{PromptTokens: 11, CompletionTokens: 2, TotalTokens: 13, Reported: true},
			}}
			executor, st, request := newCanaryExecutorFixture(t, driver)
			st.ackFailures = map[store.CanaryFinalizationStep]int{step: 1}

			if _, err := executor.ExecuteCanary(context.Background(), request); err == nil {
				t.Fatal("ExecuteCanary() succeeded across injected acknowledgement failure")
			}
			result, err := executor.ExecuteCanary(context.Background(), request)
			if err != nil || result.Outcome != CanaryOutcomePassed || st.operation.Phase != store.CanaryCompleted {
				t.Fatalf("retry ExecuteCanary() = (%+v, %v), phase = %s; want completed", result, err, st.operation.Phase)
			}
			if driver.calls != 1 || st.settlementEffects != 1 || len(st.usageEvents) != 1 || len(st.auditEntries) != 1 {
				t.Fatalf("effects = provider:%d reservation:%d usage:%d audit:%d; want one each", driver.calls, st.settlementEffects, len(st.usageEvents), len(st.auditEntries))
			}
		})
	}
}

func TestCPICanaryExecutor_RejectsDependencyDriftBeforeSideEffects(t *testing.T) {
	tests := []struct {
		name      string
		kind      string
		namespace string
		resource  string
	}{
		{name: "agent", kind: "Agent", namespace: "default", resource: "test-agent"},
		{name: "model", kind: "Model", namespace: "default", resource: "test-model"},
		{name: "model class", kind: "ModelClass", resource: "test-class"},
		{name: "secret ref", kind: "SecretRef", namespace: "default", resource: "test-secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver := &fakeCPIDriver{name: "mock", exactTokens: 11}
			executor, st, request := newCanaryExecutorFixture(t, driver)
			mutateCanaryResourceIdentity(t, st, test.kind, test.namespace, test.resource)

			result, err := executor.ExecuteCanary(context.Background(), request)
			if !errors.Is(err, ErrCanaryBindingChanged) || result.ClosedReason != v1alpha1.CanaryFailureBindingChanged {
				t.Fatalf("ExecuteCanary() = (%+v, %v), want binding-changed failure", result, err)
			}
			if st.claimCalls != 0 || st.reservationCalls != 0 || driver.calls != 0 {
				t.Fatalf("side effects = claims %d reservations %d provider calls %d, want zero", st.claimCalls, st.reservationCalls, driver.calls)
			}
		})
	}
}

func TestCPICanaryExecutor_RejectsRequestDigestTamperingBeforeSideEffects(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11}
	executor, st, request := newCanaryExecutorFixture(t, driver)
	request.CanaryTask = "tampered task"

	result, err := executor.ExecuteCanary(context.Background(), request)
	if !errors.Is(err, ErrCanaryBindingChanged) || result.ClosedReason != v1alpha1.CanaryFailureBindingChanged {
		t.Fatalf("ExecuteCanary() = (%+v, %v), want binding-changed failure", result, err)
	}
	if st.claimCalls != 0 || st.reservationCalls != 0 || driver.calls != 0 {
		t.Fatalf("side effects = claims %d reservations %d provider calls %d, want zero", st.claimCalls, st.reservationCalls, driver.calls)
	}
}

func TestCPICanaryExecutor_PreparesPromptAndSecretMetadataBindings(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11}
	executor, st, request := newCanaryExecutorFixture(t, driver)
	configureCanaryPrompt(t, st)

	prepared, err := executor.PrepareCanary(context.Background(), request)
	if err != nil {
		t.Fatalf("PrepareCanary() error = %v", err)
	}
	wantBindings := map[string]string{"Prompt": "canary-prompt", "SecretRef": "test-secret"}
	for _, binding := range prepared.Bindings {
		if wantName, ok := wantBindings[binding.Kind]; ok {
			if binding.Name != wantName || binding.UID == "" || binding.ResourceVersion == "" {
				t.Fatalf("%s binding = %+v, want immutable metadata", binding.Kind, binding)
			}
			delete(wantBindings, binding.Kind)
		}
	}
	if len(wantBindings) != 0 {
		t.Fatalf("missing bindings: %v", wantBindings)
	}
	bindingsJSON, err := json.Marshal(prepared.Bindings)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(bindingsJSON)
	for _, secret := range []string{"private canary prompt content", "sk-test-123"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("bindings leak sensitive content %q: %s", secret, serialized)
		}
	}
}

func TestCPICanaryExecutor_RejectsPromptDriftBeforeSideEffects(t *testing.T) {
	driver := &fakeCPIDriver{name: "mock", exactTokens: 11}
	executor, st, request := newCanaryExecutorFixture(t, driver)
	configureCanaryPrompt(t, st)
	request, err := executor.PrepareCanary(context.Background(), request)
	if err != nil {
		t.Fatalf("PrepareCanary() error = %v", err)
	}
	mutateCanaryResourceIdentity(t, st, "Prompt", "default", "canary-prompt")

	result, err := executor.ExecuteCanary(context.Background(), request)
	if !errors.Is(err, ErrCanaryBindingChanged) || result.ClosedReason != v1alpha1.CanaryFailureBindingChanged {
		t.Fatalf("ExecuteCanary() = (%+v, %v), want binding-changed failure", result, err)
	}
	if st.claimCalls != 0 || st.reservationCalls != 0 || driver.calls != 0 {
		t.Fatalf("side effects = claims %d reservations %d provider calls %d, want zero", st.claimCalls, st.reservationCalls, driver.calls)
	}
}
