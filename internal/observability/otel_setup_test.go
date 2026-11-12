// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

func TestNewConfiguredOpenTelemetrySpanExporterReturnsNoopWhenDisabled(t *testing.T) {
	exporter, err := NewConfiguredOpenTelemetrySpanExporter(context.Background(), OpenTelemetryConfig{})
	if err != nil {
		t.Fatalf("NewConfiguredOpenTelemetrySpanExporter() error = %v", err)
	}
	if _, ok := exporter.(*NoopSpanExporter); !ok {
		t.Fatalf("exporter type = %T, want *NoopSpanExporter", exporter)
	}
}

func TestNewConfiguredOpenTelemetrySpanExporterSelectsGRPC(t *testing.T) {
	shutdown := &recordingOTLPSDKExporter{}
	var gotConfig OpenTelemetryConfig
	restore := replaceOTLPFactories(
		func(_ context.Context, cfg OpenTelemetryConfig) (tracesdk.SpanExporter, error) {
			gotConfig = cfg
			return shutdown, nil
		},
		func(context.Context, OpenTelemetryConfig) (tracesdk.SpanExporter, error) {
			t.Fatal("HTTP factory should not be called for gRPC config")
			return nil, nil
		},
	)
	defer restore()

	exporter, err := NewConfiguredOpenTelemetrySpanExporter(context.Background(), OpenTelemetryConfig{
		Enabled:  true,
		Endpoint: "collector:4317",
		Protocol: OTLPProtocolGRPC,
		Headers: map[string]string{
			"x-tenant": "platform",
		},
	})
	if err != nil {
		t.Fatalf("NewConfiguredOpenTelemetrySpanExporter() error = %v", err)
	}
	if gotConfig.Endpoint != "collector:4317" || gotConfig.Protocol != OTLPProtocolGRPC {
		t.Fatalf("factory config = %+v, want gRPC collector endpoint", gotConfig)
	}
	if gotConfig.Headers["x-tenant"] != "platform" {
		t.Fatalf("factory headers = %+v, want x-tenant header", gotConfig.Headers)
	}
	if err := exporter.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if !shutdown.shutdown {
		t.Fatal("Shutdown() did not close the selected OTLP backend")
	}
}

func TestNewConfiguredOpenTelemetrySpanExporterSelectsHTTP(t *testing.T) {
	var gotConfig OpenTelemetryConfig
	restore := replaceOTLPFactories(
		func(context.Context, OpenTelemetryConfig) (tracesdk.SpanExporter, error) {
			t.Fatal("gRPC factory should not be called for HTTP config")
			return nil, nil
		},
		func(_ context.Context, cfg OpenTelemetryConfig) (tracesdk.SpanExporter, error) {
			gotConfig = cfg
			return &recordingOTLPSDKExporter{}, nil
		},
	)
	defer restore()

	_, err := NewConfiguredOpenTelemetrySpanExporter(context.Background(), OpenTelemetryConfig{
		Enabled:  true,
		Endpoint: "http://collector:4318/v1/traces",
		Protocol: OTLPProtocolHTTPProtobuf,
	})
	if err != nil {
		t.Fatalf("NewConfiguredOpenTelemetrySpanExporter() error = %v", err)
	}
	if gotConfig.Endpoint != "http://collector:4318/v1/traces" || gotConfig.Protocol != OTLPProtocolHTTPProtobuf {
		t.Fatalf("factory config = %+v, want HTTP collector endpoint", gotConfig)
	}
}

func TestNewConfiguredOpenTelemetrySpanExporterSurfacesFactoryErrors(t *testing.T) {
	wantErr := errors.New("dial failed")
	restore := replaceOTLPFactories(
		func(context.Context, OpenTelemetryConfig) (tracesdk.SpanExporter, error) {
			return nil, wantErr
		},
		func(context.Context, OpenTelemetryConfig) (tracesdk.SpanExporter, error) {
			return nil, nil
		},
	)
	defer restore()

	_, err := NewConfiguredOpenTelemetrySpanExporter(context.Background(), OpenTelemetryConfig{
		Enabled:  true,
		Endpoint: "collector:4317",
		Protocol: OTLPProtocolGRPC,
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("NewConfiguredOpenTelemetrySpanExporter() error = %v, want %v", err, wantErr)
	}
}

func TestNewConfiguredOpenTelemetrySpanExporterAddsResourceAttributes(t *testing.T) {
	backend := tracetest.NewInMemoryExporter()
	restore := replaceOTLPFactories(
		func(context.Context, OpenTelemetryConfig) (tracesdk.SpanExporter, error) {
			return backend, nil
		},
		func(context.Context, OpenTelemetryConfig) (tracesdk.SpanExporter, error) {
			return nil, nil
		},
	)
	defer restore()

	exporter, err := NewConfiguredOpenTelemetrySpanExporter(context.Background(), OpenTelemetryConfig{
		Enabled:        true,
		Endpoint:       "collector:4317",
		Protocol:       OTLPProtocolGRPC,
		ServiceName:    "kognition-controller",
		ServiceVersion: "1.2.3",
		Environment:    "prod",
		ResourceAttributes: map[string]string{
			"kognition.runtime": "native",
		},
	})
	if err != nil {
		t.Fatalf("NewConfiguredOpenTelemetrySpanExporter() error = %v", err)
	}
	span := &Span{
		Context: SpanContext{
			TraceID:    TraceID{1},
			SpanID:     SpanID{2},
			TraceFlags: 0x01,
		},
		Name:       "test.span",
		StartTime:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		EndTime:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		StatusCode: StatusOK,
	}
	if err := exporter.ExportSpans(context.Background(), []*Span{span}); err != nil {
		t.Fatalf("ExportSpans() error = %v", err)
	}

	spans := backend.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported span count = %d, want 1", len(spans))
	}
	attrs := attrMap(spans[0].Resource.Attributes())
	if attrs["service.name"].AsString() != "kognition-controller" {
		t.Fatalf("service.name = %q, want kognition-controller", attrs["service.name"].AsString())
	}
	if attrs["service.version"].AsString() != "1.2.3" {
		t.Fatalf("service.version = %q, want 1.2.3", attrs["service.version"].AsString())
	}
	if attrs["deployment.environment.name"].AsString() != "prod" {
		t.Fatalf("deployment.environment.name = %q, want prod", attrs["deployment.environment.name"].AsString())
	}
	if attrs["kognition.runtime"].AsString() != "native" {
		t.Fatalf("kognition.runtime = %q, want native", attrs["kognition.runtime"].AsString())
	}
}

func TestCreateOTLPGRPCExporterUsesPlaintextForSchemeLessEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	receiver := &recordingOTLPTraceService{}
	collectortracepb.RegisterTraceServiceServer(server, receiver)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exporter, err := NewConfiguredOpenTelemetrySpanExporter(ctx, OpenTelemetryConfig{
		Enabled:  true,
		Endpoint: listener.Addr().String(),
		Protocol: OTLPProtocolGRPC,
	})
	if err != nil {
		t.Fatalf("NewConfiguredOpenTelemetrySpanExporter() error = %v", err)
	}
	t.Cleanup(func() {
		_ = exporter.Shutdown(context.Background())
	})

	span := &Span{
		Context: SpanContext{
			TraceID:    TraceID{1},
			SpanID:     SpanID{2},
			TraceFlags: 0x01,
		},
		Name:       SpanNameToolCall,
		StartTime:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		EndTime:    time.Date(2026, 1, 2, 3, 4, 5, 1, time.UTC),
		StatusCode: StatusOK,
	}
	if err := exporter.ExportSpans(ctx, []*Span{span}); err != nil {
		t.Fatalf("ExportSpans() error = %v", err)
	}
	if receiver.exportCount != 1 {
		t.Fatalf("OTLP export count = %d, want 1", receiver.exportCount)
	}
}

type recordingOTLPTraceService struct {
	collectortracepb.UnimplementedTraceServiceServer
	exportCount int
}

func (s *recordingOTLPTraceService) Export(context.Context, *collectortracepb.ExportTraceServiceRequest) (*collectortracepb.ExportTraceServiceResponse, error) {
	s.exportCount++
	return &collectortracepb.ExportTraceServiceResponse{}, nil
}

func replaceOTLPFactories(grpcFactory, httpFactory openTelemetryBackendFactory) func() {
	originalGRPC := newOTLPGRPCExporter
	originalHTTP := newOTLPHTTPExporter
	newOTLPGRPCExporter = grpcFactory
	newOTLPHTTPExporter = httpFactory
	return func() {
		newOTLPGRPCExporter = originalGRPC
		newOTLPHTTPExporter = originalHTTP
	}
}

type recordingOTLPSDKExporter struct {
	shutdown bool
}

func (e *recordingOTLPSDKExporter) ExportSpans(context.Context, []tracesdk.ReadOnlySpan) error {
	return nil
}

func (e *recordingOTLPSDKExporter) Shutdown(context.Context) error {
	e.shutdown = true
	return nil
}
