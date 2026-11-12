// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestOpenTelemetrySpanExporterConvertsKognitionSpans(t *testing.T) {
	traceID := TraceID{1, 2, 3}
	parentID := SpanID{4, 5, 6}
	spanID := SpanID{7, 8, 9}
	linkedSpanID := SpanID{10, 11, 12}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	end := start.Add(42 * time.Millisecond)
	eventTime := start.Add(7 * time.Millisecond)

	backend := tracetest.NewInMemoryExporter()
	exporter := NewOpenTelemetrySpanExporter(backend)

	span := &Span{
		Context: SpanContext{
			TraceID:    traceID,
			SpanID:     spanID,
			ParentID:   parentID,
			TraceFlags: 0x01,
		},
		Name:      SpanNameModelInfer,
		Kind:      SpanKindClient,
		StartTime: start,
		EndTime:   end,
		Attributes: map[string]interface{}{
			AttrModelName:    "gpt-4.1-mini",
			AttrPromptTokens: int64(12),
			AttrTotalTokens:  34,
			"cached":         true,
			"score":          0.99,
		},
		StatusCode:    StatusError,
		StatusMessage: "model timeout",
		Links: []SpanLink{
			{
				Context: SpanContext{TraceID: traceID, SpanID: linkedSpanID, TraceFlags: 0x01},
				Attributes: map[string]interface{}{
					"reason": "retry",
				},
			},
		},
		Events: []SpanEvent{
			{
				Name:      "tokens.counted",
				Timestamp: eventTime,
				Attributes: map[string]interface{}{
					"phase": "prompt",
				},
			},
		},
	}

	if err := exporter.ExportSpans(context.Background(), []*Span{span}); err != nil {
		t.Fatalf("ExportSpans() error = %v", err)
	}

	spans := backend.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported span count = %d, want 1", len(spans))
	}

	got := spans[0]
	if got.Name != SpanNameModelInfer {
		t.Fatalf("Name = %q, want %q", got.Name, SpanNameModelInfer)
	}
	if got.SpanKind != oteltrace.SpanKindClient {
		t.Fatalf("SpanKind = %v, want %v", got.SpanKind, oteltrace.SpanKindClient)
	}
	if got.SpanContext.TraceID() != oteltrace.TraceID(traceID) {
		t.Fatalf("TraceID = %s, want %s", got.SpanContext.TraceID(), oteltrace.TraceID(traceID))
	}
	if got.SpanContext.SpanID() != oteltrace.SpanID(spanID) {
		t.Fatalf("SpanID = %s, want %s", got.SpanContext.SpanID(), oteltrace.SpanID(spanID))
	}
	if got.Parent.SpanID() != oteltrace.SpanID(parentID) {
		t.Fatalf("Parent SpanID = %s, want %s", got.Parent.SpanID(), oteltrace.SpanID(parentID))
	}
	if !got.SpanContext.TraceFlags().IsSampled() {
		t.Fatalf("TraceFlags.IsSampled() = false, want true")
	}
	if !got.StartTime.Equal(start) || !got.EndTime.Equal(end) {
		t.Fatalf("time range = %s..%s, want %s..%s", got.StartTime, got.EndTime, start, end)
	}
	if got.Status.Code != codes.Error || got.Status.Description != "model timeout" {
		t.Fatalf("Status = %+v, want error status with description", got.Status)
	}

	attrs := attrMap(got.Attributes)
	if attrs[AttrModelName].AsString() != "gpt-4.1-mini" {
		t.Fatalf("%s = %q, want gpt-4.1-mini", AttrModelName, attrs[AttrModelName].AsString())
	}
	if attrs[AttrPromptTokens].AsInt64() != 12 {
		t.Fatalf("%s = %d, want 12", AttrPromptTokens, attrs[AttrPromptTokens].AsInt64())
	}
	if attrs[AttrTotalTokens].AsInt64() != 34 {
		t.Fatalf("%s = %d, want 34", AttrTotalTokens, attrs[AttrTotalTokens].AsInt64())
	}
	if !attrs["cached"].AsBool() {
		t.Fatalf("cached = false, want true")
	}
	if attrs["score"].AsFloat64() != 0.99 {
		t.Fatalf("score = %f, want 0.99", attrs["score"].AsFloat64())
	}

	if len(got.Links) != 1 {
		t.Fatalf("Links length = %d, want 1", len(got.Links))
	}
	if got.Links[0].SpanContext.SpanID() != oteltrace.SpanID(linkedSpanID) {
		t.Fatalf("Link SpanID = %s, want %s", got.Links[0].SpanContext.SpanID(), oteltrace.SpanID(linkedSpanID))
	}
	linkAttrs := attrMap(got.Links[0].Attributes)
	if linkAttrs["reason"].AsString() != "retry" {
		t.Fatalf("link reason = %q, want retry", linkAttrs["reason"].AsString())
	}

	if len(got.Events) != 1 {
		t.Fatalf("Events length = %d, want 1", len(got.Events))
	}
	if got.Events[0].Name != "tokens.counted" || !got.Events[0].Time.Equal(eventTime) {
		t.Fatalf("event = %+v, want tokens.counted at %s", got.Events[0], eventTime)
	}
	eventAttrs := attrMap(got.Events[0].Attributes)
	if eventAttrs["phase"].AsString() != "prompt" {
		t.Fatalf("event phase = %q, want prompt", eventAttrs["phase"].AsString())
	}

	if err := exporter.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestOpenTelemetrySpanExporterRejectsNilBackend(t *testing.T) {
	exporter := NewOpenTelemetrySpanExporter(nil)
	if err := exporter.ExportSpans(context.Background(), []*Span{{Name: "ignored"}}); err == nil {
		t.Fatalf("ExportSpans() error = nil, want an error")
	}
}

func TestOpenTelemetrySpanExporterReturnsBackendErrors(t *testing.T) {
	wantErr := errors.New("backend unavailable")
	exporter := NewOpenTelemetrySpanExporter(&failingOTelSpanExporter{err: wantErr})
	span := &Span{
		Context: SpanContext{
			TraceID:    TraceID{1},
			SpanID:     SpanID{2},
			TraceFlags: 0x01,
		},
		Name:       SpanNameToolCall,
		StartTime:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		EndTime:    time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC),
		StatusCode: StatusError,
	}

	if err := exporter.ExportSpans(context.Background(), []*Span{span}); !errors.Is(err, wantErr) {
		t.Fatalf("ExportSpans() error = %v, want %v", err, wantErr)
	}
}

func attrMap(attrs []attribute.KeyValue) map[string]attribute.Value {
	result := make(map[string]attribute.Value, len(attrs))
	for _, attr := range attrs {
		result[string(attr.Key)] = attr.Value
	}
	return result
}

var _ tracesdk.SpanExporter = tracetest.NewNoopExporter()

type failingOTelSpanExporter struct {
	err error
}

func (e *failingOTelSpanExporter) ExportSpans(context.Context, []tracesdk.ReadOnlySpan) error {
	return e.err
}

func (e *failingOTelSpanExporter) Shutdown(context.Context) error {
	return nil
}
