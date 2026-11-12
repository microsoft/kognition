// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"strings"
	"testing"
)

func TestOpenTelemetryConfigFromLookupKeepsExportDisabledByDefault(t *testing.T) {
	config, err := openTelemetryConfigFromLookup(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("openTelemetryConfigFromLookup() error = %v", err)
	}
	if config.Enabled {
		t.Fatal("OpenTelemetry export should be disabled by default")
	}
	if config.ServiceName != "kognition" || config.SampleRate != 1.0 || config.Protocol != OTLPProtocolGRPC {
		t.Fatalf("defaults = %+v", config)
	}
}

func TestOpenTelemetryConfigFromLookupParsesOTLPSettings(t *testing.T) {
	env := map[string]string{
		envOTELTracesEnabled:            "true",
		envOTELTracesEndpoint:           "collector:4318",
		envOTELTracesProtocol:           "http/protobuf",
		envOTELTracesSampleRate:         "0.25",
		envOTELTracesServiceName:        "kognition-k8s-controller",
		envOTELTracesServiceVersion:     "0.4.0",
		envOTELTracesEnvironment:        "prod",
		envOTELTracesHeaders:            `{"x-tenant":"platform"}`,
		envOTELTracesResourceAttributes: `{"kognition.runtime":"kubernetes"}`,
	}

	config, err := openTelemetryConfigFromLookup(func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("openTelemetryConfigFromLookup() error = %v", err)
	}
	if !config.Enabled || config.Endpoint != "collector:4318" || config.Protocol != OTLPProtocolHTTPProtobuf || config.SampleRate != 0.25 {
		t.Fatalf("config = %+v", config)
	}
	if config.ServiceName != "kognition-k8s-controller" || config.ServiceVersion != "0.4.0" || config.Environment != "prod" {
		t.Fatalf("resource identity = %+v", config)
	}
	if config.Headers["x-tenant"] != "platform" {
		t.Fatalf("headers = %+v", config.Headers)
	}
	if config.ResourceAttributes["kognition.runtime"] != "kubernetes" {
		t.Fatalf("resource attributes = %+v", config.ResourceAttributes)
	}
}

func TestOpenTelemetryConfigFromLookupPreservesExplicitZeroSampleRate(t *testing.T) {
	env := map[string]string{
		envOTELTracesSampleRate: "0",
	}
	config, err := openTelemetryConfigFromLookup(func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("openTelemetryConfigFromLookup() error = %v", err)
	}
	if config.SampleRate != 0 {
		t.Fatalf("sample rate = %v, want 0", config.SampleRate)
	}
}

func TestOpenTelemetryConfigFromLookupRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "enabled", env: map[string]string{envOTELTracesEnabled: "sometimes"}, want: envOTELTracesEnabled},
		{name: "sample rate", env: map[string]string{envOTELTracesSampleRate: "not-a-number"}, want: envOTELTracesSampleRate},
		{name: "headers", env: map[string]string{envOTELTracesHeaders: `{"x":1}`}, want: envOTELTracesHeaders},
		{name: "resource attributes", env: map[string]string{envOTELTracesResourceAttributes: `{bad-json}`}, want: envOTELTracesResourceAttributes},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := openTelemetryConfigFromLookup(func(key string) (string, bool) {
				value, ok := tt.env[key]
				return value, ok
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want field %s", err, tt.want)
			}
		})
	}
}
