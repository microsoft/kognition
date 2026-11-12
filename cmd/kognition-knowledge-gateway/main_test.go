// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"strings"
	"testing"
)

func TestLoadConfigRequiresDistinctRuntimeServerTLS(t *testing.T) {
	setRequiredConfig(t)

	for _, name := range []string{"KOGNITION_KNOWLEDGE_RUNTIME_TLS_CERT_FILE", "KOGNITION_KNOWLEDGE_RUNTIME_TLS_KEY_FILE"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "")
			if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "runtime-facing server TLS") {
				t.Fatalf("loadConfig() error = %v, want runtime TLS requirement", err)
			}
		})
	}
}

func TestLoadConfigAcceptsRuntimeServerTLS(t *testing.T) {
	setRequiredConfig(t)
	config, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.runtimeCertificateFile != "/runtime/tls.crt" || config.runtimePrivateKeyFile != "/runtime/tls.key" {
		t.Fatalf("runtime TLS files = %q, %q", config.runtimeCertificateFile, config.runtimePrivateKeyFile)
	}
}

func setRequiredConfig(t *testing.T) {
	t.Helper()
	values := map[string]string{
		"LISTEN_ADDR":                                   ":8095",
		"KOGNITION_KNOWLEDGE_PROVIDER_NAME":             "azuresearch",
		"KOGNITION_KNOWLEDGE_PROVIDER_ADDRESS":          "provider:9443",
		"KOGNITION_KNOWLEDGE_CONFIGURATION_API_GROUP":   "azuresearch.knowledge.kognition.dev",
		"KOGNITION_KNOWLEDGE_CONFIGURATION_API_VERSION": "v1alpha1",
		"KOGNITION_KNOWLEDGE_CONFIGURATION_KIND":        "AzureSearchBackend",
		"KOGNITION_KNOWLEDGE_TLS_CERT_FILE":             "/provider/tls.crt",
		"KOGNITION_KNOWLEDGE_TLS_KEY_FILE":              "/provider/tls.key",
		"KOGNITION_KNOWLEDGE_TLS_CA_FILE":               "/provider/ca.crt",
		"KOGNITION_KNOWLEDGE_RUNTIME_TLS_CERT_FILE":     "/runtime/tls.crt",
		"KOGNITION_KNOWLEDGE_RUNTIME_TLS_KEY_FILE":      "/runtime/tls.key",
		"KOGNITION_KNOWLEDGE_PROVIDER_PEER_URI":         "spiffe://kognition.dev/knowledge-provider/azuresearch",
		"KOGNITION_KNOWLEDGE_PROVIDER_SERVER_NAME":      "azuresearch-provider",
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
}
