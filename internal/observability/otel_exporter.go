// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

var errOpenTelemetryExporterRequired = errors.New("open telemetry span exporter is required")

// OpenTelemetrySpanExporter bridges Kognition spans into the OpenTelemetry SDK export pipeline.
type OpenTelemetrySpanExporter struct {
	exporter tracesdk.SpanExporter
	resource *resource.Resource
}

// OpenTelemetrySpanExporterOption configures the OpenTelemetry span bridge.
type OpenTelemetrySpanExporterOption func(*OpenTelemetrySpanExporter)

// WithOpenTelemetryResource attaches resource attributes to exported OpenTelemetry spans.
func WithOpenTelemetryResource(resource *resource.Resource) OpenTelemetrySpanExporterOption {
	return func(exporter *OpenTelemetrySpanExporter) {
		exporter.resource = resource
	}
}

// NewOpenTelemetrySpanExporter creates a span exporter that converts Kognition spans
// into OpenTelemetry SDK spans before passing them to the supplied backend exporter.
func NewOpenTelemetrySpanExporter(exporter tracesdk.SpanExporter, opts ...OpenTelemetrySpanExporterOption) *OpenTelemetrySpanExporter {
	otelExporter := &OpenTelemetrySpanExporter{exporter: exporter}
	for _, opt := range opts {
		opt(otelExporter)
	}
	return otelExporter
}

// ExportSpans converts completed Kognition spans into OpenTelemetry SDK spans.
func (e *OpenTelemetrySpanExporter) ExportSpans(ctx context.Context, spans []*Span) error {
	if e.exporter == nil {
		return errOpenTelemetryExporterRequired
	}

	recordableSpans := sampledSpans(spans)
	if len(recordableSpans) == 0 {
		return nil
	}

	otelSpans := make([]tracesdk.ReadOnlySpan, 0, len(recordableSpans))
	for _, span := range recordableSpans {
		otelSpans = append(otelSpans, newOpenTelemetryReadOnlySpan(span, e.resource))
	}

	return e.exporter.ExportSpans(ctx, otelSpans)
}

// Shutdown gracefully shuts down the wrapped OpenTelemetry exporter.
func (e *OpenTelemetrySpanExporter) Shutdown(ctx context.Context) error {
	if e.exporter == nil {
		return nil
	}
	return e.exporter.Shutdown(ctx)
}

func sampledSpans(spans []*Span) []*Span {
	recordableSpans := make([]*Span, 0, len(spans))
	for _, span := range spans {
		if span == nil || span.Context.TraceFlags&0x01 == 0 {
			continue
		}
		recordableSpans = append(recordableSpans, span)
	}
	return recordableSpans
}

func convertSpanKind(kind SpanKind) oteltrace.SpanKind {
	switch kind {
	case SpanKindClient:
		return oteltrace.SpanKindClient
	case SpanKindServer:
		return oteltrace.SpanKindServer
	default:
		return oteltrace.SpanKindInternal
	}
}

func convertStatus(status StatusCode, message string) (codes.Code, string) {
	switch status {
	case StatusOK:
		return codes.Ok, ""
	case StatusError:
		return codes.Error, message
	default:
		return codes.Unset, ""
	}
}

func convertLinks(links []SpanLink) []tracesdk.Link {
	otelLinks := make([]tracesdk.Link, 0, len(links))
	for _, link := range links {
		spanContext := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
			TraceID:    oteltrace.TraceID(link.Context.TraceID),
			SpanID:     oteltrace.SpanID(link.Context.SpanID),
			TraceFlags: oteltrace.TraceFlags(link.Context.TraceFlags),
		})
		if !spanContext.IsValid() {
			continue
		}
		otelLinks = append(otelLinks, tracesdk.Link{
			SpanContext: spanContext,
			Attributes:  convertAttributes(link.Attributes),
		})
	}
	return otelLinks
}

func convertEvents(events []SpanEvent) []tracesdk.Event {
	otelEvents := make([]tracesdk.Event, 0, len(events))
	for _, event := range events {
		otelEvents = append(otelEvents, tracesdk.Event{
			Name:       event.Name,
			Attributes: convertAttributes(event.Attributes),
			Time:       event.Timestamp,
		})
	}
	return otelEvents
}

func convertAttributes(attrs map[string]interface{}) []attribute.KeyValue {
	if len(attrs) == 0 {
		return nil
	}

	otelAttrs := make([]attribute.KeyValue, 0, len(attrs))
	for key, value := range attrs {
		otelAttrs = append(otelAttrs, convertAttribute(key, value))
	}
	return otelAttrs
}

func convertAttribute(key string, value interface{}) attribute.KeyValue {
	switch typed := value.(type) {
	case string:
		return attribute.String(key, typed)
	case bool:
		return attribute.Bool(key, typed)
	case int:
		return attribute.Int64(key, int64(typed))
	case int8:
		return attribute.Int64(key, int64(typed))
	case int16:
		return attribute.Int64(key, int64(typed))
	case int32:
		return attribute.Int64(key, int64(typed))
	case int64:
		return attribute.Int64(key, typed)
	case uint:
		return attribute.Int64(key, uintToInt64(typed))
	case uint8:
		return attribute.Int64(key, int64(typed))
	case uint16:
		return attribute.Int64(key, int64(typed))
	case uint32:
		return attribute.Int64(key, int64(typed))
	case uint64:
		return attribute.Int64(key, uint64ToInt64(typed))
	case float32:
		return attribute.Float64(key, float64(typed))
	case float64:
		return attribute.Float64(key, typed)
	case []string:
		return attribute.StringSlice(key, typed)
	case []bool:
		return attribute.BoolSlice(key, typed)
	case []int:
		values := make([]int64, len(typed))
		for i, value := range typed {
			values[i] = int64(value)
		}
		return attribute.Int64Slice(key, values)
	case []int64:
		return attribute.Int64Slice(key, typed)
	case []float64:
		return attribute.Float64Slice(key, typed)
	default:
		return attribute.String(key, fmt.Sprint(value))
	}
}

func uintToInt64(value uint) int64 {
	if uint64(value) > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

func uint64ToInt64(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

type openTelemetryReadOnlySpan struct {
	tracesdk.ReadOnlySpan
	name                 string
	spanContext          oteltrace.SpanContext
	parent               oteltrace.SpanContext
	spanKind             oteltrace.SpanKind
	startTime            time.Time
	endTime              time.Time
	attributes           []attribute.KeyValue
	events               []tracesdk.Event
	links                []tracesdk.Link
	status               tracesdk.Status
	resource             *resource.Resource
	instrumentationScope instrumentation.Scope
}

func newOpenTelemetryReadOnlySpan(span *Span, resource *resource.Resource) tracesdk.ReadOnlySpan {
	code, description := convertStatus(span.StatusCode, span.StatusMessage)
	return &openTelemetryReadOnlySpan{
		name: span.Name,
		spanContext: oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
			TraceID:    oteltrace.TraceID(span.Context.TraceID),
			SpanID:     oteltrace.SpanID(span.Context.SpanID),
			TraceFlags: oteltrace.TraceFlags(span.Context.TraceFlags),
		}),
		parent:               parentSpanContext(span.Context),
		spanKind:             convertSpanKind(span.Kind),
		startTime:            span.StartTime,
		endTime:              span.EndTime,
		attributes:           convertAttributes(span.Attributes),
		events:               convertEvents(span.Events),
		links:                convertLinks(span.Links),
		status:               tracesdk.Status{Code: code, Description: description},
		resource:             resource,
		instrumentationScope: instrumentation.Scope{Name: "github.com/kognition-ai/kognition/internal/observability"},
	}
}

func parentSpanContext(spanContext SpanContext) oteltrace.SpanContext {
	parentID := oteltrace.SpanID(spanContext.ParentID)
	traceID := oteltrace.TraceID(spanContext.TraceID)
	if !parentID.IsValid() || !traceID.IsValid() {
		return oteltrace.SpanContext{}
	}
	return oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     parentID,
		TraceFlags: oteltrace.TraceFlags(spanContext.TraceFlags),
	})
}

func (s *openTelemetryReadOnlySpan) Name() string { return s.name }
func (s *openTelemetryReadOnlySpan) SpanContext() oteltrace.SpanContext {
	return s.spanContext
}
func (s *openTelemetryReadOnlySpan) Parent() oteltrace.SpanContext { return s.parent }
func (s *openTelemetryReadOnlySpan) SpanKind() oteltrace.SpanKind  { return s.spanKind }
func (s *openTelemetryReadOnlySpan) StartTime() time.Time          { return s.startTime }
func (s *openTelemetryReadOnlySpan) EndTime() time.Time            { return s.endTime }
func (s *openTelemetryReadOnlySpan) Attributes() []attribute.KeyValue {
	return s.attributes
}
func (s *openTelemetryReadOnlySpan) Links() []tracesdk.Link   { return s.links }
func (s *openTelemetryReadOnlySpan) Events() []tracesdk.Event { return s.events }
func (s *openTelemetryReadOnlySpan) Status() tracesdk.Status  { return s.status }
func (s *openTelemetryReadOnlySpan) InstrumentationScope() instrumentation.Scope {
	return s.instrumentationScope
}
func (s *openTelemetryReadOnlySpan) InstrumentationLibrary() instrumentation.Library { //nolint:staticcheck // Required by the OpenTelemetry ReadOnlySpan compatibility interface.
	return s.instrumentationScope
}
func (s *openTelemetryReadOnlySpan) Resource() *resource.Resource { return s.resource }
func (s *openTelemetryReadOnlySpan) DroppedAttributes() int       { return 0 }
func (s *openTelemetryReadOnlySpan) DroppedLinks() int            { return 0 }
func (s *openTelemetryReadOnlySpan) DroppedEvents() int           { return 0 }
func (s *openTelemetryReadOnlySpan) ChildSpanCount() int          { return 0 }
