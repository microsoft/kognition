// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"errors"
	"testing"
)

type recordingSpanExporter struct {
	exportCalls   int
	shutdownCalls int
	exportErr     error
	shutdownErr   error
}

func (e *recordingSpanExporter) ExportSpans(context.Context, []*Span) error {
	e.exportCalls++
	return e.exportErr
}

func (e *recordingSpanExporter) Shutdown(context.Context) error {
	e.shutdownCalls++
	return e.shutdownErr
}

type recordingCostExporter struct {
	exportCalls   int
	shutdownCalls int
	exportErr     error
	shutdownErr   error
}

func (e *recordingCostExporter) ExportCostRecords(context.Context, []CostRecord) error {
	e.exportCalls++
	return e.exportErr
}

func (e *recordingCostExporter) Shutdown(context.Context) error {
	e.shutdownCalls++
	return e.shutdownErr
}

func TestCollectorAttemptsEveryExporterAndReturnsFirstError(t *testing.T) {
	firstErr := errors.New("first exporter failed")
	secondErr := errors.New("second exporter failed")
	spanFirst := &recordingSpanExporter{exportErr: firstErr, shutdownErr: firstErr}
	spanSecond := &recordingSpanExporter{exportErr: secondErr, shutdownErr: secondErr}
	costFirst := &recordingCostExporter{exportErr: firstErr, shutdownErr: secondErr}
	costSecond := &recordingCostExporter{exportErr: secondErr}
	collector := NewCollector()
	collector.RegisterSpanExporter(spanFirst)
	collector.RegisterSpanExporter(spanSecond)
	collector.RegisterCostExporter(costFirst)
	collector.RegisterCostExporter(costSecond)

	if err := collector.FlushSpans(context.Background(), nil); !errors.Is(err, firstErr) {
		t.Fatalf("FlushSpans() error = %v, want first error", err)
	}
	if err := collector.FlushCostRecords(context.Background(), nil); !errors.Is(err, firstErr) {
		t.Fatalf("FlushCostRecords() error = %v, want first error", err)
	}
	if err := collector.Shutdown(context.Background()); !errors.Is(err, firstErr) {
		t.Fatalf("Shutdown() error = %v, want first error", err)
	}
	if spanFirst.exportCalls != 1 || spanSecond.exportCalls != 1 || costFirst.exportCalls != 1 || costSecond.exportCalls != 1 {
		t.Fatal("collector did not attempt every registered exporter")
	}
	if spanFirst.shutdownCalls != 1 || spanSecond.shutdownCalls != 1 || costFirst.shutdownCalls != 1 || costSecond.shutdownCalls != 1 {
		t.Fatal("collector did not shut down every registered exporter")
	}
}

func TestNoopExportersAcceptOperations(t *testing.T) {
	ctx := context.Background()
	spanExporter := &NoopSpanExporter{}
	costExporter := &NoopCostExporter{}
	if err := spanExporter.ExportSpans(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := spanExporter.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := costExporter.ExportCostRecords(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := costExporter.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
