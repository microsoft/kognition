// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Guards against go.opentelemetry.io/otel < 1.47.0, where valid U+FFFD runes
// were not counted by AttributeValueLengthLimit and bypassed truncation.
func TestSpanAttributeValueLengthLimitCountsReplacementRunes(t *testing.T) {
	const limit = 5
	cases := map[string]string{
		"replacement runes only":  strings.Repeat("\uFFFD", 1000),
		"replacement runes mixed": strings.Repeat("\uFFFD", 100) + strings.Repeat("A", 6),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			limits := sdktrace.NewSpanLimits()
			limits.AttributeValueLengthLimit = limit
			recorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder), sdktrace.WithRawSpanLimits(limits))
			defer func() { _ = provider.Shutdown(context.Background()) }()

			_, span := provider.Tracer("limit-test").Start(context.Background(), "op")
			span.SetAttributes(attribute.String("k", value))
			span.End()

			ended := recorder.Ended()
			if len(ended) != 1 {
				t.Fatalf("recorded %d spans, want 1", len(ended))
			}
			attributes := ended[0].Attributes()
			if len(attributes) != 1 {
				t.Fatalf("recorded %d attributes, want 1", len(attributes))
			}
			stored := attributes[0].Value.AsString()
			if got := utf8.RuneCountInString(stored); got > limit {
				t.Fatalf("stored %d characters, want at most %d", got, limit)
			}
		})
	}
}
