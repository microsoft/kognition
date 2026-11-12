// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"testing"

	tracesdk "go.opentelemetry.io/otel/sdk/trace"
)

func TestNewTracePipelineBuildsDisabledNoopPipeline(t *testing.T) {
	pipeline, err := NewTracePipeline(context.Background(), OpenTelemetryConfig{})
	if err != nil {
		t.Fatalf("NewTracePipeline() error = %v", err)
	}
	if pipeline.Enabled() {
		t.Fatal("pipeline should report disabled")
	}
	if pipeline.Tracer == nil || pipeline.Collector == nil {
		t.Fatalf("pipeline = %#v", pipeline)
	}
	if err := pipeline.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestNewTracePipelineConfiguresDefaultRedaction(t *testing.T) {
	pipeline, err := NewTracePipeline(context.Background(), OpenTelemetryConfig{SampleRate: 1.0})
	if err != nil {
		t.Fatalf("NewTracePipeline() error = %v", err)
	}
	span := pipeline.Tracer.StartSpan(context.Background(), SpanNameToolCall, SpanKindClient, map[string]interface{}{
		AttrToolArgs: map[string]interface{}{"email": "secret@corp.com"},
	})
	pipeline.Tracer.EndSpan(span, StatusOK, "")

	spans := pipeline.Tracer.Spans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	args, ok := spans[0].Attributes[AttrToolArgs].(string)
	if !ok {
		t.Fatalf("tool args attr = %T, want string", spans[0].Attributes[AttrToolArgs])
	}
	if containsStr(args, "secret@corp.com") || !containsStr(args, "[REDACTED:email]") {
		t.Fatalf("redacted args = %q", args)
	}
}

func TestNewTracePipelineRegistersConfiguredExporter(t *testing.T) {
	backend := &recordingOTLPSDKExporter{}
	restore := replaceOTLPFactories(
		func(context.Context, OpenTelemetryConfig) (tracesdk.SpanExporter, error) {
			return backend, nil
		},
		func(context.Context, OpenTelemetryConfig) (tracesdk.SpanExporter, error) {
			t.Fatal("HTTP factory should not be called")
			return nil, nil
		},
	)
	defer restore()

	pipeline, err := NewTracePipeline(context.Background(), OpenTelemetryConfig{Enabled: true, Endpoint: "collector:4317"})
	if err != nil {
		t.Fatalf("NewTracePipeline() error = %v", err)
	}
	if !pipeline.Enabled() {
		t.Fatal("pipeline should report enabled")
	}
	if err := pipeline.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if !backend.shutdown {
		t.Fatal("configured backend was not shut down")
	}
}
