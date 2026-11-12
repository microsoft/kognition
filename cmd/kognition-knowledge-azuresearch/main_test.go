// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	azuresearchprovider "github.com/kognition-ai/kognition/providers/azuresearch"
)

func writeAzureSearchCertificateMaterial(t *testing.T) (string, string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "azure search adapter test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certificateFile := filepath.Join(directory, "server.crt")
	privateKeyFile := filepath.Join(directory, "server.key")
	trustBundleFile := filepath.Join(directory, "ca.crt")
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certificateFile, certificatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trustBundleFile, certificatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certificateFile, privateKeyFile, trustBundleFile
}

func setAzureSearchRuntimeEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"KOGNITION_KNOWLEDGE_LISTEN_ADDRESS", "KOGNITION_KNOWLEDGE_ADMIN_ADDRESS", "KOGNITION_KNOWLEDGE_PROVIDER_VERSION",
		"KOGNITION_KNOWLEDGE_TLS_CERT_FILE", "KOGNITION_KNOWLEDGE_TLS_KEY_FILE", "KOGNITION_KNOWLEDGE_TLS_CA_FILE",
		"KOGNITION_KNOWLEDGE_CLIENT_PEER_URI", "KOGNITION_KNOWLEDGE_WATCH_NAMESPACE",
		"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_FEDERATED_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
	certificateFile, privateKeyFile, trustBundleFile := writeAzureSearchCertificateMaterial(t)
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_CERT_FILE", certificateFile)
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_KEY_FILE", privateKeyFile)
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_CA_FILE", trustBundleFile)
	t.Setenv("KOGNITION_KNOWLEDGE_CLIENT_PEER_URI", "spiffe://kognition.test/core/knowledge")
	t.Setenv("AZURE_TENANT_ID", "tenant-id")
	t.Setenv("AZURE_CLIENT_ID", "client-id")
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", filepath.Join(t.TempDir(), "federated-token"))
}

func TestLoadConfigDefaultsAndOverrides(t *testing.T) {
	setAzureSearchRuntimeEnvironment(t)
	t.Setenv("KOGNITION_KNOWLEDGE_ADMIN_ADDRESS", "127.0.0.1:19092")
	t.Setenv("KOGNITION_KNOWLEDGE_PROVIDER_VERSION", "1.2.3")
	t.Setenv("KOGNITION_KNOWLEDGE_WATCH_NAMESPACE", "tenant-a")
	config, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.listenAddress != defaultListen || config.adminAddress != "127.0.0.1:19092" || config.providerVersion != "1.2.3" || config.watchedNamespace != "tenant-a" {
		t.Fatalf("unexpected runtime config: %+v", config)
	}
	material, err := loadTLSMaterial(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(material.Certificate.Certificate) == 0 || material.Roots == nil || material.PeerURI != "spiffe://kognition.test/core/knowledge" {
		t.Fatal("TLS material was not loaded completely")
	}
}

func TestLoadConfigRejectsUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		variable string
		value    string
		contains string
	}{
		{name: "invalid listen", variable: "KOGNITION_KNOWLEDGE_LISTEN_ADDRESS", value: "invalid", contains: "LISTEN_ADDRESS"},
		{name: "invalid admin", variable: "KOGNITION_KNOWLEDGE_ADMIN_ADDRESS", value: "invalid", contains: "ADMIN_ADDRESS"},
		{name: "missing certificate", variable: "KOGNITION_KNOWLEDGE_TLS_CERT_FILE", contains: "TLS_CERT_FILE"},
		{name: "missing peer identity", variable: "KOGNITION_KNOWLEDGE_CLIENT_PEER_URI", contains: "CLIENT_PEER_URI"},
		{name: "missing Azure tenant", variable: "AZURE_TENANT_ID", contains: "AZURE_TENANT_ID"},
		{name: "missing Azure client", variable: "AZURE_CLIENT_ID", contains: "AZURE_CLIENT_ID"},
		{name: "missing Azure token file", variable: "AZURE_FEDERATED_TOKEN_FILE", contains: "AZURE_FEDERATED_TOKEN_FILE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setAzureSearchRuntimeEnvironment(t)
			t.Setenv(test.variable, test.value)
			_, err := loadConfig()
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestNewAzureCredentialUsesExplicitWorkloadIdentity(t *testing.T) {
	setAzureSearchRuntimeEnvironment(t)
	config, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	credential, err := newAzureCredential(config)
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil {
		t.Fatal("expected workload identity credential")
	}
}

func TestAzureSearchConfigurationKind(t *testing.T) {
	kind := azureSearchConfigurationKind()
	if kind.Group != azuresearchprovider.APIGroup || kind.Version != azuresearchprovider.APIVersion || kind.Kind != azuresearchprovider.Kind {
		t.Fatalf("unexpected provider kind: %s", kind.String())
	}
}
