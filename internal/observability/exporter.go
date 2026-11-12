// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"sync"
)

// SpanExporter defines the interface for exporting completed spans
// to observability backends (Jaeger, Tempo, Langfuse, etc.).
type SpanExporter interface {
	// ExportSpans sends a batch of completed spans to the backend.
	// Implementations should be idempotent.
	ExportSpans(ctx context.Context, spans []*Span) error

	// Shutdown gracefully closes the exporter, flushing pending data.
	Shutdown(ctx context.Context) error
}

// CostExporter defines the interface for exporting cost records
// to cost aggregation backends.
type CostExporter interface {
	// ExportCostRecords sends cost records to the backend.
	ExportCostRecords(ctx context.Context, records []CostRecord) error

	// Shutdown gracefully closes the exporter.
	Shutdown(ctx context.Context) error
}

// Collector aggregates spans and cost records, then flushes them to
// registered exporters. It supports multiple concurrent exporters.
type Collector struct {
	spanExporters []SpanExporter
	costExporters []CostExporter
	mu            sync.RWMutex
}

// NewCollector creates a new Collector with no exporters.
func NewCollector() *Collector {
	return &Collector{}
}

// RegisterSpanExporter adds a span exporter to the collector.
func (c *Collector) RegisterSpanExporter(e SpanExporter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spanExporters = append(c.spanExporters, e)
}

// RegisterCostExporter adds a cost exporter to the collector.
func (c *Collector) RegisterCostExporter(e CostExporter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.costExporters = append(c.costExporters, e)
}

// FlushSpans exports all spans to registered span exporters.
// Returns the first error encountered (all exporters are attempted).
func (c *Collector) FlushSpans(ctx context.Context, spans []*Span) error {
	c.mu.RLock()
	exporters := make([]SpanExporter, len(c.spanExporters))
	copy(exporters, c.spanExporters)
	c.mu.RUnlock()

	var firstErr error
	for _, e := range exporters {
		if err := e.ExportSpans(ctx, spans); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// FlushCostRecords exports cost records to registered cost exporters.
func (c *Collector) FlushCostRecords(ctx context.Context, records []CostRecord) error {
	c.mu.RLock()
	exporters := make([]CostExporter, len(c.costExporters))
	copy(exporters, c.costExporters)
	c.mu.RUnlock()

	var firstErr error
	for _, e := range exporters {
		if err := e.ExportCostRecords(ctx, records); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Shutdown gracefully shuts down all registered exporters.
func (c *Collector) Shutdown(ctx context.Context) error {
	c.mu.RLock()
	spanExps := make([]SpanExporter, len(c.spanExporters))
	costExps := make([]CostExporter, len(c.costExporters))
	copy(spanExps, c.spanExporters)
	copy(costExps, c.costExporters)
	c.mu.RUnlock()

	var firstErr error
	for _, e := range spanExps {
		if err := e.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, e := range costExps {
		if err := e.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// NoopSpanExporter is a no-op exporter for testing and disabled tracing.
type NoopSpanExporter struct{}

func (n *NoopSpanExporter) ExportSpans(_ context.Context, _ []*Span) error { return nil }
func (n *NoopSpanExporter) Shutdown(_ context.Context) error               { return nil }

// NoopCostExporter is a no-op exporter for testing.
type NoopCostExporter struct{}

func (n *NoopCostExporter) ExportCostRecords(_ context.Context, _ []CostRecord) error { return nil }
func (n *NoopCostExporter) Shutdown(_ context.Context) error                          { return nil }
