// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"testing"
)

func TestGeneratedWorkflowTraceIdentifiersAreValid(t *testing.T) {
	if traceID := NewHexTraceID(); !IsValidTraceID(traceID) {
		t.Fatalf("NewHexTraceID() = %q, want valid W3C trace id", traceID)
	}
	if spanID := NewHexSpanID(); !IsValidSpanID(spanID) {
		t.Fatalf("NewHexSpanID() = %q, want valid W3C span id", spanID)
	}
}

func TestWorkflowTraceIdentifierValidation(t *testing.T) {
	tests := []struct {
		name  string
		value string
		valid func(string) bool
		want  bool
	}{
		{name: "valid trace id", value: "0123456789abcdef0123456789abcdef", valid: IsValidTraceID, want: true},
		{name: "short trace id", value: "0123456789abcdef", valid: IsValidTraceID},
		{name: "zero trace id", value: "00000000000000000000000000000000", valid: IsValidTraceID},
		{name: "uppercase trace id", value: "0123456789ABCDEF0123456789abcdef", valid: IsValidTraceID},
		{name: "non-hex trace id", value: "g123456789abcdef0123456789abcdef", valid: IsValidTraceID},
		{name: "valid span id", value: "0123456789abcdef", valid: IsValidSpanID, want: true},
		{name: "short span id", value: "01234567", valid: IsValidSpanID},
		{name: "zero span id", value: "0000000000000000", valid: IsValidSpanID},
		{name: "uppercase span id", value: "0123456789ABCDEF", valid: IsValidSpanID},
		{name: "non-hex span id", value: "g123456789abcdef", valid: IsValidSpanID},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.valid(test.value); got != test.want {
				t.Fatalf("validation of %q = %t, want %t", test.value, got, test.want)
			}
		})
	}
}

func TestTraceparent(t *testing.T) {
	const (
		traceID = "0123456789abcdef0123456789abcdef"
		spanID  = "0123456789abcdef"
	)

	if got, want := Traceparent(traceID, spanID), "00-"+traceID+"-"+spanID+"-01"; got != want {
		t.Fatalf("Traceparent() = %q, want %q", got, want)
	}
	if got := Traceparent("invalid", spanID); got != "" {
		t.Fatalf("Traceparent(invalid trace id) = %q, want empty", got)
	}
	if got := Traceparent(traceID, "invalid"); got != "" {
		t.Fatalf("Traceparent(invalid span id) = %q, want empty", got)
	}
}

func TestSpanContextFromAnnotations(t *testing.T) {
	annotations := map[string]string{
		AnnotationTraceID:      "0123456789abcdef0123456789abcdef",
		AnnotationSpanID:       "0123456789abcdef",
		AnnotationParentSpanID: "fedcba9876543210",
	}

	ctx, ok := SpanContextFromAnnotations(annotations)
	if !ok {
		t.Fatal("SpanContextFromAnnotations() ok = false, want true")
	}
	if got := HexTraceID(ctx.TraceID); got != annotations[AnnotationTraceID] {
		t.Fatalf("trace id = %q, want %q", got, annotations[AnnotationTraceID])
	}
	if got := HexSpanID(ctx.SpanID); got != annotations[AnnotationSpanID] {
		t.Fatalf("span id = %q, want %q", got, annotations[AnnotationSpanID])
	}
	if got := HexSpanID(ctx.ParentID); got != annotations[AnnotationParentSpanID] {
		t.Fatalf("parent id = %q, want %q", got, annotations[AnnotationParentSpanID])
	}
	if ctx.TraceFlags != 0x01 {
		t.Fatalf("trace flags = %x, want 01", ctx.TraceFlags)
	}
}

func TestSpanContextFromAnnotationsUsesRootSpanForWorkflow(t *testing.T) {
	annotations := map[string]string{
		AnnotationTraceID:    "0123456789abcdef0123456789abcdef",
		AnnotationRootSpanID: "0123456789abcdef",
	}

	ctx, ok := SpanContextFromAnnotations(annotations)
	if !ok {
		t.Fatal("SpanContextFromAnnotations() ok = false, want true")
	}
	if got := HexSpanID(ctx.SpanID); got != annotations[AnnotationRootSpanID] {
		t.Fatalf("span id = %q, want root span id %q", got, annotations[AnnotationRootSpanID])
	}
}

func TestSpanContextFromAnnotationsRejectsInvalidIDs(t *testing.T) {
	annotations := map[string]string{
		AnnotationTraceID: "invalid",
		AnnotationSpanID:  "0123456789abcdef",
	}

	if _, ok := SpanContextFromAnnotations(annotations); ok {
		t.Fatal("SpanContextFromAnnotations() ok = true, want false")
	}
}

func TestTracerUsesPropagatedSpanContext(t *testing.T) {
	parent, ok := SpanContextFromAnnotations(map[string]string{
		AnnotationTraceID: "0123456789abcdef0123456789abcdef",
		AnnotationSpanID:  "0123456789abcdef",
	})
	if !ok {
		t.Fatal("SpanContextFromAnnotations() ok = false, want true")
	}

	tracer := NewTracer(TracerConfig{SampleRate: 1.0})
	span := tracer.StartSpan(ContextWithSpanContext(context.Background(), parent), SpanNameToolCall, SpanKindClient, nil)

	if got := HexTraceID(span.Context.TraceID); got != HexTraceID(parent.TraceID) {
		t.Fatalf("child trace id = %q, want propagated %q", got, HexTraceID(parent.TraceID))
	}
	if got := HexSpanID(span.Context.ParentID); got != HexSpanID(parent.SpanID) {
		t.Fatalf("child parent id = %q, want propagated span %q", got, HexSpanID(parent.SpanID))
	}
	if got := HexSpanID(span.Context.SpanID); got == HexSpanID(parent.SpanID) {
		t.Fatalf("child span id reused parent id %q", got)
	}
}
