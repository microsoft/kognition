// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"sync"
	"testing"
)

func TestTracerHonorsDisabledSampling(t *testing.T) {
	tracer := NewTracer(TracerConfig{SampleRate: 0, CostTagger: NewCostTagger()})
	var costs []CostRecord
	tracer.SetCostSink(func(record CostRecord) { costs = append(costs, record) })
	span := tracer.StartSpan(context.Background(), SpanNameToolCall, SpanKindClient, map[string]interface{}{
		AttrToolName: "search", AttrToolCostUnits: int64(7), AttrWorkspace: "team-a",
	})

	tracer.EndSpan(span, StatusOK, "")

	if got := tracer.SpanCount(); got != 0 {
		t.Fatalf("SpanCount() = %d, want 0 when sample rate is disabled", got)
	}
	if spans := tracer.Spans(); len(spans) != 0 {
		t.Fatalf("Spans() length = %d, want 0 when sample rate is disabled", len(spans))
	}
	if len(costs) != 1 || costs[0].Workspace != "team-a" || costs[0].CostUnits != 7 {
		t.Fatalf("costs = %#v, want durable accounting without span sampling", costs)
	}
}

func TestTracerHonorsFullSampling(t *testing.T) {
	tracer := NewTracer(TracerConfig{SampleRate: 1.0})
	span := tracer.StartSpan(context.Background(), SpanNameToolCall, SpanKindClient, map[string]interface{}{
		AttrToolName: "search",
	})

	tracer.EndSpan(span, StatusOK, "")

	if got := tracer.SpanCount(); got != 1 {
		t.Fatalf("SpanCount() = %d, want 1", got)
	}
	if spans := tracer.Spans(); len(spans) != 1 {
		t.Fatalf("Spans() length = %d, want 1", len(spans))
	}
}

func TestTracerConcurrentSpanCompletionAndReads(t *testing.T) {
	tracer := NewTracer(TracerConfig{SampleRate: 1.0})
	ctx := context.Background()
	const spanTotal = 64

	var writers sync.WaitGroup
	writers.Add(spanTotal)
	for i := 0; i < spanTotal; i++ {
		go func() {
			defer writers.Done()
			span := tracer.StartSpan(ctx, SpanNameReasoningStep, SpanKindInternal, nil)
			tracer.EndSpan(span, StatusOK, "")
		}()
	}

	var reader sync.WaitGroup
	reader.Add(1)
	go func() {
		defer reader.Done()
		for tracer.SpanCount() < spanTotal {
			_ = tracer.Spans()
		}
	}()

	writers.Wait()
	reader.Wait()

	if got := tracer.SpanCount(); got != spanTotal {
		t.Fatalf("SpanCount() = %d, want %d", got, spanTotal)
	}
	if spans := tracer.Spans(); len(spans) != spanTotal {
		t.Fatalf("Spans() length = %d, want %d", len(spans), spanTotal)
	}
}
