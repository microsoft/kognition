// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func ContextWithTraceHeaders(ctx context.Context, headers http.Header) context.Context {
	extracted := (propagation.TraceContext{}).Extract(ctx, propagation.HeaderCarrier(headers))
	span := oteltrace.SpanContextFromContext(extracted)
	if !span.IsValid() {
		return ctx
	}
	return ContextWithSpanContext(extracted, SpanContext{
		TraceID: TraceID(span.TraceID()), SpanID: SpanID(span.SpanID()), TraceFlags: byte(span.TraceFlags()),
	})
}

type spanContextKey struct{}

// Annotation keys used to propagate trace context onto Kubernetes objects
// (WorkflowRun and child AgentJob). These mirror the W3C trace-context model
// using simple, human-readable annotations so kognitionctl can stitch a
// timeline without parsing W3C headers.
const (
	AnnotationTraceID       = "kognition.dev/trace-id"
	AnnotationRootSpanID    = "kognition.dev/root-span-id"
	AnnotationSpanID        = "kognition.dev/span-id"
	AnnotationParentSpanID  = "kognition.dev/parent-span-id"
	AnnotationTraceparent   = "kognition.dev/traceparent"
	AnnotationWorkflowName  = "kognition.dev/workflow-name"
	AnnotationWorkflowRun   = "kognition.dev/workflow-run"
	AnnotationStepName      = "kognition.dev/step-name"
	AnnotationStepType      = "kognition.dev/step-type"
	AnnotationStepIteration = "kognition.dev/step-iteration"
)

// NewHexTraceID returns a random 128-bit trace id encoded as 32 lowercase hex chars.
func NewHexTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Errorf("observability: read random for trace id: %w", err))
	}
	return hex.EncodeToString(b[:])
}

// NewHexSpanID returns a random 64-bit span id encoded as 16 lowercase hex chars.
func NewHexSpanID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Errorf("observability: read random for span id: %w", err))
	}
	return hex.EncodeToString(b[:])
}

// HexTraceID returns a trace identifier encoded as lowercase hex.
func HexTraceID(traceID TraceID) string {
	return hex.EncodeToString(traceID[:])
}

// HexSpanID returns a span identifier encoded as lowercase hex.
func HexSpanID(spanID SpanID) string {
	return hex.EncodeToString(spanID[:])
}

// ContextWithSpanContext stores propagated trace context on ctx for native
// instrumentation that cannot rely on Kubernetes annotations directly.
func ContextWithSpanContext(ctx context.Context, spanContext SpanContext) context.Context {
	return context.WithValue(ctx, spanContextKey{}, spanContext)
}

// SpanContextFromContext returns propagated trace context stored on ctx.
func SpanContextFromContext(ctx context.Context) (SpanContext, bool) {
	if ctx == nil {
		return SpanContext{}, false
	}
	spanContext, ok := ctx.Value(spanContextKey{}).(SpanContext)
	return spanContext, ok
}

// IsValidTraceID reports whether s is a 32-char lowercase hex W3C trace id
// and is not the all-zero invalid id.
func IsValidTraceID(s string) bool {
	if len(s) != 32 {
		return false
	}
	return isLowerHex(s) && s != "00000000000000000000000000000000"
}

// IsValidSpanID reports whether s is a 16-char lowercase hex W3C span id
// and is not the all-zero invalid id.
func IsValidSpanID(s string) bool {
	if len(s) != 16 {
		return false
	}
	return isLowerHex(s) && s != "0000000000000000"
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Traceparent formats a W3C traceparent header value (version 00, flags 01).
// Returns "" when either id fails W3C validation so callers can omit the
// annotation rather than emit a malformed header.
func Traceparent(traceID, spanID string) string {
	if !IsValidTraceID(traceID) || !IsValidSpanID(spanID) {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-01", traceID, spanID)
}

// SpanContextFromAnnotations converts Kognition trace annotations into the
// typed span context used by native observability instrumentation.
func SpanContextFromAnnotations(annotations map[string]string) (SpanContext, bool) {
	if annotations == nil {
		return SpanContext{}, false
	}

	traceIDHex := annotations[AnnotationTraceID]
	spanIDHex := annotations[AnnotationSpanID]
	if spanIDHex == "" {
		spanIDHex = annotations[AnnotationRootSpanID]
	}
	if !IsValidTraceID(traceIDHex) || !IsValidSpanID(spanIDHex) {
		return SpanContext{}, false
	}

	var traceID TraceID
	var spanID SpanID
	if _, err := hex.Decode(traceID[:], []byte(traceIDHex)); err != nil {
		return SpanContext{}, false
	}
	if _, err := hex.Decode(spanID[:], []byte(spanIDHex)); err != nil {
		return SpanContext{}, false
	}

	ctx := SpanContext{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: 0x01,
	}
	if parentIDHex := annotations[AnnotationParentSpanID]; parentIDHex != "" {
		if !IsValidSpanID(parentIDHex) {
			return SpanContext{}, false
		}
		if _, err := hex.Decode(ctx.ParentID[:], []byte(parentIDHex)); err != nil {
			return SpanContext{}, false
		}
	}
	return ctx, true
}
