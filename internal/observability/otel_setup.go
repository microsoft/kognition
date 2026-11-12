// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
)

type openTelemetryBackendFactory func(context.Context, OpenTelemetryConfig) (tracesdk.SpanExporter, error)

var (
	newOTLPGRPCExporter openTelemetryBackendFactory = createOTLPGRPCExporter
	newOTLPHTTPExporter openTelemetryBackendFactory = createOTLPHTTPExporter
)

// NewConfiguredOpenTelemetrySpanExporter creates a Kognition span exporter from OpenTelemetry config.
func NewConfiguredOpenTelemetrySpanExporter(ctx context.Context, config OpenTelemetryConfig) (SpanExporter, error) {
	config, err := NormalizeOpenTelemetryConfig(config)
	if err != nil {
		return nil, err
	}
	if !config.Enabled {
		return &NoopSpanExporter{}, nil
	}

	var backend tracesdk.SpanExporter
	switch config.Protocol {
	case OTLPProtocolHTTPProtobuf:
		backend, err = newOTLPHTTPExporter(ctx, config)
	default:
		backend, err = newOTLPGRPCExporter(ctx, config)
	}
	if err != nil {
		return nil, err
	}

	return NewOpenTelemetrySpanExporter(backend, WithOpenTelemetryResource(newOpenTelemetryResource(config))), nil
}

func createOTLPGRPCExporter(ctx context.Context, config OpenTelemetryConfig) (tracesdk.SpanExporter, error) {
	opts := make([]otlptracegrpc.Option, 0, 3)
	if config.Endpoint != "" {
		if endpointHasScheme(config.Endpoint) {
			opts = append(opts, otlptracegrpc.WithEndpointURL(config.Endpoint))
		} else {
			opts = append(opts, otlptracegrpc.WithEndpoint(config.Endpoint))
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
	}
	if len(config.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(config.Headers))
	}
	return otlptracegrpc.New(ctx, opts...)
}

func createOTLPHTTPExporter(ctx context.Context, config OpenTelemetryConfig) (tracesdk.SpanExporter, error) {
	opts := make([]otlptracehttp.Option, 0, 3)
	if config.Endpoint != "" {
		if endpointHasScheme(config.Endpoint) {
			opts = append(opts, otlptracehttp.WithEndpointURL(config.Endpoint))
		} else {
			opts = append(opts, otlptracehttp.WithEndpoint(config.Endpoint))
			opts = append(opts, otlptracehttp.WithInsecure())
		}
	}
	if len(config.Headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(config.Headers))
	}
	return otlptracehttp.New(ctx, opts...)
}

func newOpenTelemetryResource(config OpenTelemetryConfig) *resource.Resource {
	attrs := make([]attribute.KeyValue, 0, 3+len(config.ResourceAttributes))
	if config.ServiceName != "" {
		attrs = append(attrs, attribute.String("service.name", config.ServiceName))
	}
	if config.ServiceVersion != "" {
		attrs = append(attrs, attribute.String("service.version", config.ServiceVersion))
	}
	if config.Environment != "" {
		attrs = append(attrs, attribute.String("deployment.environment.name", config.Environment))
	}
	for key, value := range config.ResourceAttributes {
		attrs = append(attrs, attribute.String(key, value))
	}
	return resource.NewSchemaless(attrs...)
}

func endpointHasScheme(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	return err == nil && parsed.Scheme != "" && strings.Contains(endpoint, "://")
}
