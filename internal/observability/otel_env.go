// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	envOTELTracesEnabled            = "KOGNITION_OTEL_TRACES_ENABLED"
	envOTELTracesEndpoint           = "KOGNITION_OTEL_TRACES_ENDPOINT"
	envOTELTracesProtocol           = "KOGNITION_OTEL_TRACES_PROTOCOL"
	envOTELTracesSampleRate         = "KOGNITION_OTEL_TRACES_SAMPLE_RATE"
	envOTELTracesServiceName        = "KOGNITION_OTEL_TRACES_SERVICE_NAME"
	envOTELTracesServiceVersion     = "KOGNITION_OTEL_TRACES_SERVICE_VERSION"
	envOTELTracesEnvironment        = "KOGNITION_OTEL_TRACES_ENVIRONMENT"
	envOTELTracesHeaders            = "KOGNITION_OTEL_TRACES_HEADERS"
	envOTELTracesResourceAttributes = "KOGNITION_OTEL_TRACES_RESOURCE_ATTRIBUTES"
)

// OpenTelemetryConfigFromEnv loads Kognition OpenTelemetry trace export settings from process environment.
func OpenTelemetryConfigFromEnv() (OpenTelemetryConfig, error) {
	return openTelemetryConfigFromLookup(os.LookupEnv)
}

func openTelemetryConfigFromLookup(lookup func(string) (string, bool)) (OpenTelemetryConfig, error) {
	config := OpenTelemetryConfig{
		Endpoint:       lookupString(lookup, envOTELTracesEndpoint),
		Protocol:       OTLPProtocol(lookupString(lookup, envOTELTracesProtocol)),
		SampleRate:     1.0,
		ServiceName:    lookupString(lookup, envOTELTracesServiceName),
		ServiceVersion: lookupString(lookup, envOTELTracesServiceVersion),
		Environment:    lookupString(lookup, envOTELTracesEnvironment),
	}

	if raw, ok := lookup(envOTELTracesEnabled); ok && strings.TrimSpace(raw) != "" {
		enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return OpenTelemetryConfig{}, &OpenTelemetryConfigError{Field: envOTELTracesEnabled, Message: "must be a boolean"}
		}
		config.Enabled = enabled
	}
	if raw, ok := lookup(envOTELTracesSampleRate); ok && strings.TrimSpace(raw) != "" {
		sampleRate, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return OpenTelemetryConfig{}, &OpenTelemetryConfigError{Field: envOTELTracesSampleRate, Message: "must be a number between 0.0 and 1.0"}
		}
		config.SampleRate = sampleRate
	}

	var err error
	config.Headers, err = lookupStringMap(lookup, envOTELTracesHeaders)
	if err != nil {
		return OpenTelemetryConfig{}, err
	}
	config.ResourceAttributes, err = lookupStringMap(lookup, envOTELTracesResourceAttributes)
	if err != nil {
		return OpenTelemetryConfig{}, err
	}

	return NormalizeOpenTelemetryConfig(config)
}

func lookupString(lookup func(string) (string, bool), key string) string {
	value, ok := lookup(key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func lookupStringMap(lookup func(string) (string, bool), key string) (map[string]string, error) {
	raw, ok := lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, &OpenTelemetryConfigError{Field: key, Message: fmt.Sprintf("must be a JSON object with string values: %v", err)}
	}
	return out, nil
}
