// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import "fmt"

// OTLPProtocol identifies the transport used for OTLP trace export.
type OTLPProtocol string

const (
	OTLPProtocolGRPC         OTLPProtocol = "grpc"
	OTLPProtocolHTTPProtobuf OTLPProtocol = "http/protobuf"
)

const defaultOpenTelemetryServiceName = "kognition"

// OpenTelemetryConfig configures OpenTelemetry trace export for Kognition spans.
type OpenTelemetryConfig struct {
	Enabled            bool
	Endpoint           string
	Protocol           OTLPProtocol
	Headers            map[string]string
	SampleRate         float64
	ServiceName        string
	ServiceVersion     string
	Environment        string
	ResourceAttributes map[string]string
}

// NormalizeOpenTelemetryConfig applies defaults and validates the OpenTelemetry configuration.
func NormalizeOpenTelemetryConfig(config OpenTelemetryConfig) (OpenTelemetryConfig, error) {
	if config.SampleRate < 0 || config.SampleRate > 1 {
		return OpenTelemetryConfig{}, &OpenTelemetryConfigError{Field: "sampleRate", Message: "must be between 0.0 and 1.0"}
	}
	if config.ServiceName == "" {
		config.ServiceName = defaultOpenTelemetryServiceName
	}
	if config.Protocol == "" {
		config.Protocol = OTLPProtocolGRPC
	}
	if config.Protocol != OTLPProtocolGRPC && config.Protocol != OTLPProtocolHTTPProtobuf {
		return OpenTelemetryConfig{}, &OpenTelemetryConfigError{Field: "protocol", Message: fmt.Sprintf("must be %q or %q", OTLPProtocolGRPC, OTLPProtocolHTTPProtobuf)}
	}
	if config.Enabled && config.Endpoint == "" {
		return OpenTelemetryConfig{}, &OpenTelemetryConfigError{Field: "endpoint", Message: "required when OpenTelemetry export is enabled"}
	}
	config.Headers = copyStringMap(config.Headers)
	config.ResourceAttributes = copyStringMap(config.ResourceAttributes)
	return config, nil
}

// OpenTelemetryConfigError describes an invalid OpenTelemetry configuration field.
type OpenTelemetryConfigError struct {
	Field   string
	Message string
}

func (e *OpenTelemetryConfigError) Error() string {
	return "opentelemetry config: " + e.Field + " " + e.Message
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
