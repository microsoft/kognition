// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import "context"

// TracePipeline owns the local semantic tracer and configured span exporters.
type TracePipeline struct {
	Config    OpenTelemetryConfig
	Tracer    *Tracer
	Collector *Collector
}

// NewTracePipelineFromEnv creates a trace pipeline from Kognition OpenTelemetry environment variables.
func NewTracePipelineFromEnv(ctx context.Context) (*TracePipeline, error) {
	config, err := OpenTelemetryConfigFromEnv()
	if err != nil {
		return nil, err
	}
	return NewTracePipeline(ctx, config)
}

// NewTracePipeline creates a semantic tracer and collector from an OpenTelemetry configuration.
func NewTracePipeline(ctx context.Context, config OpenTelemetryConfig) (*TracePipeline, error) {
	config, err := NormalizeOpenTelemetryConfig(config)
	if err != nil {
		return nil, err
	}
	exporter, err := NewConfiguredOpenTelemetrySpanExporter(ctx, config)
	if err != nil {
		return nil, err
	}
	collector := NewCollector()
	collector.RegisterSpanExporter(exporter)
	return &TracePipeline{
		Config:    config,
		Tracer:    NewTracer(TracerConfig{SampleRate: config.SampleRate, Redactor: NewRedactor(DefaultRedactionConfig()), CostTagger: NewCostTagger()}),
		Collector: collector,
	}, nil
}

// Enabled reports whether this pipeline should be connected to runtime span producers.
func (p *TracePipeline) Enabled() bool {
	return p != nil && p.Config.Enabled
}

// Shutdown releases exporter resources held by the pipeline.
func (p *TracePipeline) Shutdown(ctx context.Context) error {
	if p == nil || p.Collector == nil {
		return nil
	}
	return p.Collector.Shutdown(ctx)
}
