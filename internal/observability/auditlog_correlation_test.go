// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestAuditLogPersistsDecisionCorrelation(t *testing.T) {
	for _, method := range []string{"before", "record", "batch"} {
		t.Run(method, func(t *testing.T) {
			store := &mockAuditStore{}
			log := NewCognitiveAuditLog(store)
			span := NewTracer(TracerConfig{SampleRate: 1}).StartSpan(context.Background(), "test", SpanKindInternal, nil)
			ctx := ContextWithSpanContext(context.Background(), span.Context)
			entry := AuditEntry{EventType: AuditToolCall, Details: json.RawMessage(`{"schemaVersion":1,"futureField":"preserved"}`)}
			var err error
			switch method {
			case "before":
				err = log.RecordBeforeExec(ctx, entry)
			case "record":
				err = log.Record(ctx, entry)
			case "batch":
				err = log.RecordBatch(ctx, []AuditEntry{entry})
			}
			if err != nil {
				t.Fatal(err)
			}
			entries, err := log.Query(ctx, AuditFilter{})
			if err != nil || len(entries) != 1 {
				t.Fatalf("entries=%v err=%v", entries, err)
			}
			var decision struct {
				Trace       *TrustDecisionTrace `json:"trace"`
				FutureField string              `json:"futureField"`
			}
			if err := json.Unmarshal(entries[0].Details, &decision); err != nil {
				t.Fatal(err)
			}
			if decision.Trace == nil || decision.Trace.TraceID != HexTraceID(span.Context.TraceID) || decision.Trace.SpanID != HexSpanID(span.Context.SpanID) || decision.FutureField != "preserved" {
				t.Fatalf("correlation lost: %+v", decision)
			}
		})
	}
}

func TestAuditCorrelationPreservesExistingEvidence(t *testing.T) {
	for _, test := range []struct {
		name        string
		event       AuditEventType
		details     string
		withContext bool
	}{
		{"generic payload", AuditToolCall, `{"operation":"legacy"}`, true},
		{"explicit trace", AuditToolCall, `{"schemaVersion":1,"trace":{"traceId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","spanId":"bbbbbbbbbbbbbbbb"}}`, true},
		{"missing context", AuditToolCall, `{"schemaVersion":1}`, false},
		{"malformed payload", AuditToolCall, `{"schemaVersion":`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.withContext {
				ctx = ContextWithTraceHeaders(ctx, http.Header{"Traceparent": {"00-11111111111111111111111111111111-2222222222222222-01"}})
			}
			store := &mockAuditStore{}
			log := NewCognitiveAuditLog(store)
			if err := log.RecordBeforeExec(ctx, AuditEntry{EventType: test.event, Details: json.RawMessage(test.details)}); err != nil {
				t.Fatal(err)
			}
			entries, err := log.Query(ctx, AuditFilter{})
			if err != nil || len(entries) != 1 {
				t.Fatalf("entries=%v err=%v", entries, err)
			}
			if string(entries[0].Details) != test.details {
				t.Fatalf("payload changed: %s", entries[0].Details)
			}
			if !test.withContext && entries[0].TraceID != "" {
				t.Fatal("invented trace ID")
			}
		})
	}
}

func TestContextWithTraceHeaders(t *testing.T) {
	for _, test := range []struct {
		name, header string
		valid        bool
	}{
		{"valid", "00-11111111111111111111111111111111-2222222222222222-01", true},
		{"absent", "", false},
		{"malformed", "not-a-trace", false},
		{"zero trace", "00-00000000000000000000000000000000-2222222222222222-01", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := ContextWithTraceHeaders(context.Background(), http.Header{"Traceparent": {test.header}})
			span, ok := SpanContextFromContext(ctx)
			if ok != test.valid {
				t.Fatalf("valid=%v, want %v", ok, test.valid)
			}
			if test.valid && (HexTraceID(span.TraceID) != "11111111111111111111111111111111" || HexSpanID(span.SpanID) != "2222222222222222") {
				t.Fatalf("incorrect parent: %+v", span)
			}
		})
	}
}
