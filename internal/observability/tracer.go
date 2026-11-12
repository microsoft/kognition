// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// TracerConfig configures the trace instrumentation.
type TracerConfig struct {
	// SampleRate controls the fraction of traces to sample (0.0 to 1.0).
	// 1.0 means trace everything.
	SampleRate float64
	// Redactor applies PII redaction to span attributes. May be nil.
	Redactor *Redactor
	// CostTagger extracts cost attribution from spans. May be nil.
	CostTagger *CostTagger
}

// Tracer collects spans for a session's reasoning lineage trace.
// It is safe for concurrent use and designed to be embedded in runtime
// instrumentation middleware.
type Tracer struct {
	config    TracerConfig
	traceID   TraceID
	mu        sync.RWMutex
	spans     []*Span
	spanCount atomic.Int64
	costSink  func(CostRecord) error // optional callback for cost records
}

// NewTracer creates a tracer for a single session trace.
func NewTracer(config TracerConfig) *Tracer {
	var tid TraceID
	_, _ = rand.Read(tid[:])
	return &Tracer{
		config:  config,
		traceID: tid,
	}
}

// SetCostSink registers a callback invoked for every completed span with cost.
func (t *Tracer) SetCostSink(sink func(CostRecord)) {
	if sink == nil {
		t.costSink = nil
		return
	}
	t.costSink = func(record CostRecord) error {
		sink(record)
		return nil
	}
}

// SetFallibleCostSink registers a persistence callback whose failure can be
// propagated by callers that must not acknowledge unaccounted operations.
func (t *Tracer) SetFallibleCostSink(sink func(CostRecord) error) {
	t.costSink = sink
}

// TraceID returns the tracer's trace identifier.
func (t *Tracer) TraceID() TraceID {
	return t.traceID
}

// TraceIDHex returns the trace ID as a hex string.
func (t *Tracer) TraceIDHex() string {
	return hex.EncodeToString(t.traceID[:])
}

// StartRootSpan begins a new sampled root span with an independent trace id.
func (t *Tracer) StartRootSpan(name string, kind SpanKind, attrs map[string]interface{}) *Span {
	var tid TraceID
	_, _ = rand.Read(tid[:])
	var sid SpanID
	_, _ = rand.Read(sid[:])
	traceFlags := byte(0x00)
	if shouldSample(t.config.SampleRate) {
		traceFlags = 0x01
	}

	return &Span{
		Context: SpanContext{
			TraceID:    tid,
			SpanID:     sid,
			TraceFlags: traceFlags,
		},
		Name:       name,
		Kind:       kind,
		StartTime:  time.Now().UTC(),
		Attributes: attrs,
		StatusCode: StatusUnset,
	}
}

// StartSpan begins a new span as part of this trace.
func (t *Tracer) StartSpan(ctx context.Context, name string, kind SpanKind, attrs map[string]interface{}) *Span {
	var sid SpanID
	_, _ = rand.Read(sid[:])
	traceID := t.traceID
	var parentID SpanID
	traceFlags := byte(0x00)
	if parent, ok := SpanContextFromContext(ctx); ok {
		traceID = parent.TraceID
		parentID = parent.SpanID
		traceFlags = parent.TraceFlags & 0x01
	} else if shouldSample(t.config.SampleRate) {
		traceFlags = 0x01
	}

	span := &Span{
		Context: SpanContext{
			TraceID:    traceID,
			SpanID:     sid,
			ParentID:   parentID,
			TraceFlags: traceFlags,
		},
		Name:       name,
		Kind:       kind,
		StartTime:  time.Now().UTC(),
		Attributes: attrs,
		StatusCode: StatusUnset,
	}
	return span
}

// EndSpan finalizes a span, applies redaction, extracts cost, and records it.
func (t *Tracer) EndSpan(span *Span, status StatusCode, statusMsg string) error {
	if span == nil {
		return nil
	}

	span.EndTime = time.Now().UTC()
	span.StatusCode = status
	span.StatusMessage = statusMsg

	// Apply PII redaction.
	if t.config.Redactor != nil && span.Attributes != nil {
		span.Attributes = t.config.Redactor.RedactSpanAttributes(span.Attributes)
	}

	// Extract cost record if tagger is configured.
	var sinkErr error
	if t.config.CostTagger != nil && t.costSink != nil {
		record := t.config.CostTagger.TagFromSpan(span)
		if record.OperationType != "" && (record.OperationType != CostOpModelInference || status == StatusOK) {
			sinkErr = t.costSink(record)
		}
	}
	if span.Context.TraceFlags&0x01 == 0 {
		return sinkErr
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.spans = append(t.spans, span)
	t.spanCount.Add(1)
	return sinkErr
}

// Spans returns all completed spans in the trace.
func (t *Tracer) Spans() []*Span {
	t.mu.RLock()
	defer t.mu.RUnlock()
	spans := make([]*Span, len(t.spans))
	copy(spans, t.spans)
	return spans
}

// SpanCount returns the total number of completed spans.
func (t *Tracer) SpanCount() int64 {
	return t.spanCount.Load()
}

// WithParent sets the parent span context on a child span.
func WithParent(child *Span, parent *Span) {
	if parent != nil {
		child.Context.ParentID = parent.Context.SpanID
	}
}

func shouldSample(sampleRate float64) bool {
	if sampleRate <= 0 || math.IsNaN(sampleRate) {
		return false
	}
	if sampleRate >= 1 {
		return true
	}

	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return true
	}
	randomValue := float64(binary.BigEndian.Uint64(randomBytes[:])) / float64(math.MaxUint64)
	return randomValue < sampleRate
}
