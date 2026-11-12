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
)

// writeCertificateMaterial issues a fresh self-signed leaf certificate and
// writes cert, key, and (leaf-as-)trust-bundle PEM files into dir. It returns
// the file paths so callers can point loadConfig at them.
func writeCertificateMaterial(t *testing.T, dir string) (certPath, keyPath, caPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kognition knowledge test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	certPath = filepath.Join(dir, "server.crt")
	keyPath = filepath.Join(dir, "server.key")
	caPath = filepath.Join(dir, "trust-bundle.crt")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return certPath, keyPath, caPath
}

// clearKnowledgeEnv unsets every KOGNITION_KNOWLEDGE_* environment variable
// touched by loadConfig for the duration of the test.
func clearKnowledgeEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"KOGNITION_KNOWLEDGE_LISTEN_ADDRESS",
		"KOGNITION_KNOWLEDGE_ADMIN_ADDRESS",
		"KOGNITION_KNOWLEDGE_PROVIDER_VERSION",
		"KOGNITION_KNOWLEDGE_TLS_CERT_FILE",
		"KOGNITION_KNOWLEDGE_TLS_KEY_FILE",
		"KOGNITION_KNOWLEDGE_TLS_CA_FILE",
		"KOGNITION_KNOWLEDGE_CLIENT_PEER_URI",
		"KOGNITION_KNOWLEDGE_WATCH_NAMESPACE",
	} {
		t.Setenv(name, "")
	}
}

func TestLoadConfigAppliesDefaultsWhenTLSAndPeerAreProvided(t *testing.T) {
	clearKnowledgeEnv(t)
	dir := t.TempDir()
	certPath, keyPath, caPath := writeCertificateMaterial(t, dir)

	t.Setenv("KOGNITION_KNOWLEDGE_TLS_CERT_FILE", certPath)
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_KEY_FILE", keyPath)
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_CA_FILE", caPath)
	t.Setenv("KOGNITION_KNOWLEDGE_CLIENT_PEER_URI", "spiffe://kognition.test/core/knowledge")

	config, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if config.listenAddress != defaultListen {
		t.Fatalf("listenAddress = %q, want %q", config.listenAddress, defaultListen)
	}
	if config.adminAddress != defaultAdmin {
		t.Fatalf("adminAddress = %q, want %q", config.adminAddress, defaultAdmin)
	}
	if config.providerVersion != "0.0.0-dev" {
		t.Fatalf("providerVersion = %q, want %q", config.providerVersion, "0.0.0-dev")
	}
	if config.certificateFile != certPath || config.privateKeyFile != keyPath || config.trustBundleFile != caPath {
		t.Fatalf("TLS paths not propagated: %+v", config)
	}
	if config.clientPeerURI != "spiffe://kognition.test/core/knowledge" {
		t.Fatalf("clientPeerURI = %q", config.clientPeerURI)
	}
	if config.watchedNamespace != "" {
		t.Fatalf("watchedNamespace should default to empty, got %q", config.watchedNamespace)
	}
}

func TestLoadConfigHonoursExplicitOverrides(t *testing.T) {
	clearKnowledgeEnv(t)
	dir := t.TempDir()
	certPath, keyPath, caPath := writeCertificateMaterial(t, dir)

	t.Setenv("KOGNITION_KNOWLEDGE_LISTEN_ADDRESS", "127.0.0.1:19443")
	t.Setenv("KOGNITION_KNOWLEDGE_ADMIN_ADDRESS", "127.0.0.1:19092")
	t.Setenv("KOGNITION_KNOWLEDGE_PROVIDER_VERSION", "1.2.3")
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_CERT_FILE", certPath)
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_KEY_FILE", keyPath)
	t.Setenv("KOGNITION_KNOWLEDGE_TLS_CA_FILE", caPath)
	t.Setenv("KOGNITION_KNOWLEDGE_CLIENT_PEER_URI", "spiffe://kognition.test/core/knowledge")
	t.Setenv("KOGNITION_KNOWLEDGE_WATCH_NAMESPACE", "tenant-a")

	config, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if config.listenAddress != "127.0.0.1:19443" {
		t.Fatalf("listenAddress = %q", config.listenAddress)
	}
	if config.adminAddress != "127.0.0.1:19092" {
		t.Fatalf("adminAddress = %q", config.adminAddress)
	}
	if config.providerVersion != "1.2.3" {
		t.Fatalf("providerVersion = %q", config.providerVersion)
	}
	if config.watchedNamespace != "tenant-a" {
		t.Fatalf("watchedNamespace = %q", config.watchedNamespace)
	}
}

func TestLoadConfigRejectsInvalidListenAndAdminAddresses(t *testing.T) {
	setValid := func(t *testing.T) {
		t.Helper()
		dir := t.TempDir()
		certPath, keyPath, caPath := writeCertificateMaterial(t, dir)
		t.Setenv("KOGNITION_KNOWLEDGE_TLS_CERT_FILE", certPath)
		t.Setenv("KOGNITION_KNOWLEDGE_TLS_KEY_FILE", keyPath)
		t.Setenv("KOGNITION_KNOWLEDGE_TLS_CA_FILE", caPath)
		t.Setenv("KOGNITION_KNOWLEDGE_CLIENT_PEER_URI", "spiffe://kognition.test/core/knowledge")
	}

	t.Run("listen", func(t *testing.T) {
		clearKnowledgeEnv(t)
		setValid(t)
		t.Setenv("KOGNITION_KNOWLEDGE_LISTEN_ADDRESS", "not-a-hostport")
		_, err := loadConfig()
		if err == nil || !strings.Contains(err.Error(), "KOGNITION_KNOWLEDGE_LISTEN_ADDRESS") {
			t.Fatalf("expected listen address error, got %v", err)
		}
	})

	t.Run("admin", func(t *testing.T) {
		clearKnowledgeEnv(t)
		setValid(t)
		t.Setenv("KOGNITION_KNOWLEDGE_ADMIN_ADDRESS", "still-not-a-hostport")
		_, err := loadConfig()
		if err == nil || !strings.Contains(err.Error(), "KOGNITION_KNOWLEDGE_ADMIN_ADDRESS") {
			t.Fatalf("expected admin address error, got %v", err)
		}
	})
}

func TestLoadConfigRequiresTLSMaterialAndPeerURI(t *testing.T) {
	valid := func(t *testing.T) (string, string, string) {
		t.Helper()
		dir := t.TempDir()
		return writeCertificateMaterial(t, dir)
	}

	cases := []struct {
		name        string
		clearVar    string
		expectedSub string
	}{
		{"missing cert", "KOGNITION_KNOWLEDGE_TLS_CERT_FILE", "TLS_CERT_FILE"},
		{"missing key", "KOGNITION_KNOWLEDGE_TLS_KEY_FILE", "TLS_CERT_FILE"},
		{"missing ca", "KOGNITION_KNOWLEDGE_TLS_CA_FILE", "TLS_CERT_FILE"},
		{"missing peer", "KOGNITION_KNOWLEDGE_CLIENT_PEER_URI", "CLIENT_PEER_URI"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clearKnowledgeEnv(t)
			certPath, keyPath, caPath := valid(t)
			t.Setenv("KOGNITION_KNOWLEDGE_TLS_CERT_FILE", certPath)
			t.Setenv("KOGNITION_KNOWLEDGE_TLS_KEY_FILE", keyPath)
			t.Setenv("KOGNITION_KNOWLEDGE_TLS_CA_FILE", caPath)
			t.Setenv("KOGNITION_KNOWLEDGE_CLIENT_PEER_URI", "spiffe://kognition.test/core/knowledge")
			// Now clear the variable we want to test as missing.
			t.Setenv(testCase.clearVar, "")

			_, err := loadConfig()
			if err == nil {
				t.Fatalf("expected error when %s is unset", testCase.clearVar)
			}
			if !strings.Contains(err.Error(), testCase.expectedSub) {
				t.Fatalf("error %q does not mention %q", err.Error(), testCase.expectedSub)
			}
		})
	}
}

func TestLoadTLSMaterialReturnsCertificateAndTrustRoots(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, caPath := writeCertificateMaterial(t, dir)

	tlsMaterial, err := loadTLSMaterial(runtimeConfig{
		certificateFile: certPath,
		privateKeyFile:  keyPath,
		trustBundleFile: caPath,
		clientPeerURI:   "spiffe://kognition.test/core/knowledge",
	})
	if err != nil {
		t.Fatalf("loadTLSMaterial: %v", err)
	}
	if len(tlsMaterial.Certificate.Certificate) == 0 {
		t.Fatalf("no certificate chain returned")
	}
	if tlsMaterial.Roots == nil {
		t.Fatalf("no trust roots returned")
	}
	if tlsMaterial.PeerURI != "spiffe://kognition.test/core/knowledge" {
		t.Fatalf("PeerURI = %q", tlsMaterial.PeerURI)
	}
}

func TestLoadTLSMaterialFailsOnMissingCertificateFile(t *testing.T) {
	dir := t.TempDir()
	_, keyPath, caPath := writeCertificateMaterial(t, dir)

	_, err := loadTLSMaterial(runtimeConfig{
		certificateFile: filepath.Join(dir, "does-not-exist.crt"),
		privateKeyFile:  keyPath,
		trustBundleFile: caPath,
	})
	if err == nil || !strings.Contains(err.Error(), "load knowledge server certificate") {
		t.Fatalf("expected certificate load error, got %v", err)
	}
}

func TestLoadTLSMaterialFailsOnMissingTrustBundleFile(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, _ := writeCertificateMaterial(t, dir)

	_, err := loadTLSMaterial(runtimeConfig{
		certificateFile: certPath,
		privateKeyFile:  keyPath,
		trustBundleFile: filepath.Join(dir, "does-not-exist.pem"),
	})
	if err == nil || !strings.Contains(err.Error(), "load knowledge trust bundle") {
		t.Fatalf("expected trust bundle load error, got %v", err)
	}
}

func TestLoadTLSMaterialRejectsUnusableTrustBundle(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, _ := writeCertificateMaterial(t, dir)

	unusablePath := filepath.Join(dir, "empty-bundle.pem")
	if err := os.WriteFile(unusablePath, []byte("this is not a PEM certificate\n"), 0o600); err != nil {
		t.Fatalf("write bad ca: %v", err)
	}

	_, err := loadTLSMaterial(runtimeConfig{
		certificateFile: certPath,
		privateKeyFile:  keyPath,
		trustBundleFile: unusablePath,
	})
	if err == nil || !strings.Contains(err.Error(), "knowledge trust bundle contains no usable certificates") {
		t.Fatalf("expected trust bundle rejection, got %v", err)
	}
}

func TestLoadTLSMaterialRejectsMismatchedKeyPair(t *testing.T) {
	dir := t.TempDir()
	certPath, _, caPath := writeCertificateMaterial(t, dir)

	// Swap the private key with a freshly generated one that does not match
	// the certificate above. tls.LoadX509KeyPair must reject the pair.
	mismatchKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate mismatch key: %v", err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(mismatchKey)
	if err != nil {
		t.Fatalf("marshal mismatch key: %v", err)
	}
	mismatchKeyPath := filepath.Join(dir, "mismatch.key")
	if err := os.WriteFile(mismatchKeyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}), 0o600); err != nil {
		t.Fatalf("write mismatch key: %v", err)
	}

	_, err = loadTLSMaterial(runtimeConfig{
		certificateFile: certPath,
		privateKeyFile:  mismatchKeyPath,
		trustBundleFile: caPath,
	})
	if err == nil || !strings.Contains(err.Error(), "load knowledge server certificate") {
		t.Fatalf("expected key-pair mismatch error, got %v", err)
	}
}

func TestEnvOrDefaultTrimsWhitespaceAndFallsBack(t *testing.T) {
	t.Setenv("KOGNITION_KNOWLEDGE_TEST_ENV", "   ")
	if got := envOrDefault("KOGNITION_KNOWLEDGE_TEST_ENV", "fallback"); got != "fallback" {
		t.Fatalf("whitespace-only value should return fallback, got %q", got)
	}
	t.Setenv("KOGNITION_KNOWLEDGE_TEST_ENV", "  set  ")
	if got := envOrDefault("KOGNITION_KNOWLEDGE_TEST_ENV", "fallback"); got != "set" {
		t.Fatalf("expected trimmed value 'set', got %q", got)
	}
	t.Setenv("KOGNITION_KNOWLEDGE_TEST_ENV", "")
	if got := envOrDefault("KOGNITION_KNOWLEDGE_TEST_ENV", "fallback"); got != "fallback" {
		t.Fatalf("empty value should return fallback, got %q", got)
	}
}
