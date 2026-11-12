// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"strings"
	"testing"
)

func TestNormalizeOpenTelemetryConfigKeepsTracingDisabledByDefault(t *testing.T) {
	cfg, err := NormalizeOpenTelemetryConfig(OpenTelemetryConfig{})
	if err != nil {
		t.Fatalf("NormalizeOpenTelemetryConfig() error = %v", err)
	}

	if cfg.Enabled {
		t.Fatal("default OpenTelemetry config should keep tracing disabled")
	}
	if cfg.SampleRate != 0 {
		t.Fatalf("default sample rate = %v, want 0", cfg.SampleRate)
	}
	if cfg.ServiceName != "kognition" {
		t.Fatalf("default service name = %q, want kognition", cfg.ServiceName)
	}
}

func TestNormalizeOpenTelemetryConfigPreservesZeroSampleRate(t *testing.T) {
	cfg, err := NormalizeOpenTelemetryConfig(OpenTelemetryConfig{SampleRate: 0})
	if err != nil {
		t.Fatalf("NormalizeOpenTelemetryConfig() error = %v", err)
	}
	if cfg.SampleRate != 0 {
		t.Fatalf("sample rate = %v, want 0", cfg.SampleRate)
	}
}

func TestNormalizeOpenTelemetryConfigPreservesOTLPSettings(t *testing.T) {
	cfg, err := NormalizeOpenTelemetryConfig(OpenTelemetryConfig{
		Enabled:        true,
		Endpoint:       "http://collector:4318",
		Protocol:       OTLPProtocolHTTPProtobuf,
		SampleRate:     0.25,
		ServiceName:    "kognition-controller",
		ServiceVersion: "v0.4.0",
		Environment:    "production",
		Headers: map[string]string{
			"x-tenant": "prod",
		},
		ResourceAttributes: map[string]string{
			"deployment.region": "eastus",
		},
	})
	if err != nil {
		t.Fatalf("NormalizeOpenTelemetryConfig() error = %v", err)
	}

	if !cfg.Enabled {
		t.Fatal("enabled config should stay enabled")
	}
	if cfg.Endpoint != "http://collector:4318" {
		t.Fatalf("endpoint = %q", cfg.Endpoint)
	}
	if cfg.Protocol != OTLPProtocolHTTPProtobuf {
		t.Fatalf("protocol = %q", cfg.Protocol)
	}
	if cfg.Headers["x-tenant"] != "prod" {
		t.Fatalf("header was not preserved: %#v", cfg.Headers)
	}
	if cfg.ResourceAttributes["deployment.region"] != "eastus" {
		t.Fatalf("resource attributes were not preserved: %#v", cfg.ResourceAttributes)
	}
}

func TestNormalizeOpenTelemetryConfigRejectsInvalidSampleRate(t *testing.T) {
	_, err := NormalizeOpenTelemetryConfig(OpenTelemetryConfig{
		Enabled:    true,
		Endpoint:   "http://collector:4318",
		SampleRate: 1.5,
	})
	if err == nil {
		t.Fatal("expected invalid sample rate error")
	}
	if !strings.Contains(err.Error(), "sampleRate") {
		t.Fatalf("error = %q, want field name sampleRate", err.Error())
	}
}

func TestNormalizeOpenTelemetryConfigRejectsEnabledConfigWithoutEndpoint(t *testing.T) {
	_, err := NormalizeOpenTelemetryConfig(OpenTelemetryConfig{Enabled: true})
	if err == nil {
		t.Fatal("expected endpoint validation error")
	}
	if !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("error = %q, want field name endpoint", err.Error())
	}
}
