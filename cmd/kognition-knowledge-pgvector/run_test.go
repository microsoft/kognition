// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestRunFailsWhenLoadConfigFails(t *testing.T) {
	clearKnowledgeEnv(t)
	dir := t.TempDir()
	certPath, keyPath, caPath := writeCertificateMaterial(t, dir)
	t.Setenv("KOGNITION_KNOWLEDGE_LISTEN_ADDRESS", "not-a-host-port")
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_CERT_FILE", certPath)
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_KEY_FILE", keyPath)
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_CA_FILE", caPath)
	t.Setenv("KOGNITION_KNOWLEDGE_CLIENT_PEER_URI", "spiffe://example.test/knowledge")

	err := run(zap.NewNop())
	if err == nil {
		t.Fatal("run should fail when listen address is invalid")
	}
	if !strings.Contains(err.Error(), "KOGNITION_KNOWLEDGE_LISTEN_ADDRESS") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunFailsWhenLoadTLSMaterialFails(t *testing.T) {
	clearKnowledgeEnv(t)
	dir := t.TempDir()
	missingCert := filepath.Join(dir, "does-not-exist.crt")
	missingKey := filepath.Join(dir, "does-not-exist.key")
	missingCA := filepath.Join(dir, "does-not-exist-ca.crt")
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_CERT_FILE", missingCert)
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_KEY_FILE", missingKey)
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_CA_FILE", missingCA)
	t.Setenv("KOGNITION_KNOWLEDGE_CLIENT_PEER_URI", "spiffe://example.test/knowledge")

	err := run(zap.NewNop())
	if err == nil {
		t.Fatal("run should fail when TLS material is missing")
	}
	if !strings.Contains(err.Error(), "load knowledge server certificate") {
		t.Fatalf("unexpected error: %v", err)
	}
}
