// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestKubeRoundTripper_InjectsToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := &kubeRoundTripper{
		base:  http.DefaultTransport,
		token: "my-token-123",
	}

	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if gotAuth != "Bearer my-token-123" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer my-token-123")
	}
}

func TestKubeRoundTripper_EmptyToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := &kubeRoundTripper{
		base:  http.DefaultTransport,
		token: "",
	}

	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (no token)", gotAuth)
	}
}

func TestLoadKubeConfig_FromEnv(t *testing.T) {
	content := `apiVersion: v1
kind: Config
current-context: test-ctx
clusters:
- name: test-cluster
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: test-ctx
  context:
    cluster: test-cluster
    user: test-user
users:
- name: test-user
  user:
    token: static-token
`
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config")
	if err := os.WriteFile(cfgPath, []byte(content), 0600); err != nil {
		t.Fatalf("write temp kubeconfig: %v", err)
	}

	t.Setenv("KUBECONFIG", cfgPath)

	cfg, err := loadKubeConfig()
	if err != nil {
		t.Fatalf("loadKubeConfig() error: %v", err)
	}

	if cfg.CurrentContext != "test-ctx" {
		t.Errorf("CurrentContext = %q, want %q", cfg.CurrentContext, "test-ctx")
	}
	if len(cfg.Clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(cfg.Clusters))
	}
	if cfg.Clusters[0].Cluster.Server != "https://127.0.0.1:6443" {
		t.Errorf("cluster server = %q, want %q", cfg.Clusters[0].Cluster.Server, "https://127.0.0.1:6443")
	}
	if len(cfg.Users) != 1 || cfg.Users[0].User.Token != "static-token" {
		t.Error("expected user with static-token")
	}
}

func TestLoadKubeConfig_MultiplePathsFirstWins(t *testing.T) {
	content1 := `apiVersion: v1
kind: Config
current-context: first
clusters: []
contexts: []
users: []
`
	content2 := `apiVersion: v1
kind: Config
current-context: second
clusters: []
contexts: []
users: []
`
	tmpDir := t.TempDir()
	path1 := filepath.Join(tmpDir, "config1")
	path2 := filepath.Join(tmpDir, "config2")
	os.WriteFile(path1, []byte(content1), 0600)
	os.WriteFile(path2, []byte(content2), 0600)

	sep := kubeConfigPathSeparator()
	t.Setenv("KUBECONFIG", path1+sep+path2)

	cfg, err := loadKubeConfig()
	if err != nil {
		t.Fatalf("loadKubeConfig() error: %v", err)
	}

	if cfg.CurrentContext != "first" {
		t.Errorf("CurrentContext = %q, want %q (first path)", cfg.CurrentContext, "first")
	}
}

func TestLoadKubeConfig_MissingFile(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nonexistent"))

	_, err := loadKubeConfig()
	if err == nil {
		t.Fatal("expected error for missing kubeconfig")
	}
}

func TestResolveToken_StaticToken(t *testing.T) {
	user := &kubeConfigUser{}
	user.User.Token = "my-static-token"

	token, err := resolveToken(user)
	if err != nil {
		t.Fatalf("resolveToken() error: %v", err)
	}
	if token != "my-static-token" {
		t.Errorf("token = %q, want %q", token, "my-static-token")
	}
}

func TestResolveToken_ClientCertNoToken(t *testing.T) {
	user := &kubeConfigUser{}
	user.User.ClientCertificateData = "dGVzdA==" // "test" base64

	token, err := resolveToken(user)
	if err != nil {
		t.Fatalf("resolveToken() error: %v", err)
	}
	if token != "" {
		t.Errorf("token = %q, want empty for client cert auth", token)
	}
}

func TestResolveToken_NoAuthMethod(t *testing.T) {
	user := &kubeConfigUser{}

	_, err := resolveToken(user)
	if err == nil {
		t.Fatal("expected error when no auth method available")
	}
}

func TestExecToken(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		behavior  string
		wantToken string
		wantError string
	}{
		{name: "extracts token", behavior: "success", wantToken: "plugin-token"},
		{name: "rejects malformed output", behavior: "malformed", wantError: "parse exec credential output"},
		{name: "rejects empty token", behavior: "empty", wantError: "returned empty token"},
		{name: "reports plugin failure", behavior: "failure", wantError: "exec credential plugin"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := []struct {
				Name  string `yaml:"name"`
				Value string `yaml:"value"`
			}{{Name: "KOGNITION_EXEC_PLUGIN_VALUE", Value: "from-kubeconfig"}}

			token, err := execToken(context.Background(), executable, []string{"-test.run=^TestExecCredentialHelper$", "--", "expected-arg"}, append(env, struct {
				Name  string `yaml:"name"`
				Value string `yaml:"value"`
			}{Name: "KOGNITION_EXEC_HELPER", Value: tt.behavior}))
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("execToken() error = %v, want error containing %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("execToken() error = %v", err)
			}
			if token != tt.wantToken {
				t.Fatalf("execToken() = %q, want %q", token, tt.wantToken)
			}
		})
	}
}

func TestExecCredentialHelper(t *testing.T) {
	switch os.Getenv("KOGNITION_EXEC_HELPER") {
	case "success":
		if os.Getenv("KOGNITION_EXEC_PLUGIN_VALUE") != "from-kubeconfig" || len(os.Args) == 0 || os.Args[len(os.Args)-1] != "expected-arg" {
			os.Exit(2)
		}
		fmt.Print(`{"status":{"token":"plugin-token"}}`)
		os.Exit(0)
	case "malformed":
		fmt.Print("not-json")
		os.Exit(0)
	case "empty":
		fmt.Print(`{"status":{}}`)
		os.Exit(0)
	case "failure":
		os.Exit(3)
	}
}

func TestResolveKubeTransport_StaticToken(t *testing.T) {
	content := `apiVersion: v1
kind: Config
current-context: test-ctx
clusters:
- name: test-cluster
  cluster:
    server: https://10.0.0.1:6443
    insecure-skip-tls-verify: true
contexts:
- name: test-ctx
  context:
    cluster: test-cluster
    user: test-user
users:
- name: test-user
  user:
    token: bearer-abc
`
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config")
	os.WriteFile(cfgPath, []byte(content), 0600)
	t.Setenv("KUBECONFIG", cfgPath)

	proxy, err := resolveKubeTransport("", "kognition", "kognition-apiserver", 8080)
	if err != nil {
		t.Fatalf("resolveKubeTransport() error: %v", err)
	}

	wantURL := "https://10.0.0.1:6443/api/v1/namespaces/kognition/services/kognition-apiserver:8080/proxy"
	if proxy.baseURL != wantURL {
		t.Errorf("baseURL = %q, want %q", proxy.baseURL, wantURL)
	}
	if proxy.client == nil {
		t.Error("client should not be nil")
	}
}

func TestResolveKubeTransport_ContextNotFound(t *testing.T) {
	content := `apiVersion: v1
kind: Config
current-context: missing-ctx
clusters: []
contexts: []
users: []
`
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config")
	os.WriteFile(cfgPath, []byte(content), 0600)
	t.Setenv("KUBECONFIG", cfgPath)

	_, err := resolveKubeTransport("", "ns", "svc", 8080)
	if err == nil {
		t.Fatal("expected error for missing context")
	}
}

func TestResolveKubeAPITransportRejectsInvalidReferencesAndCredentials(t *testing.T) {
	missingFile := filepath.Join(t.TempDir(), "missing")
	for _, tc := range []struct {
		name      string
		cluster   string
		user      string
		wantError string
	}{
		{name: "missing cluster", cluster: "", user: "    token: valid", wantError: `cluster "test-cluster" not found`},
		{name: "missing user", cluster: "    server: https://example.test", user: "", wantError: `user "test-user" not found`},
		{name: "invalid CA data", cluster: "    server: https://example.test\n    certificate-authority-data: \"@@@\"", user: "    token: valid", wantError: "decode CA data"},
		{name: "missing CA file", cluster: "    server: https://example.test\n    certificate-authority: " + missingFile, user: "    token: valid", wantError: "read CA file"},
		{name: "invalid client certificate data", cluster: "    server: https://example.test", user: "    client-certificate-data: \"@@@\"\n    client-key-data: dmFsaWQ=", wantError: "decode client cert"},
		{name: "invalid client key data", cluster: "    server: https://example.test", user: "    client-certificate-data: dmFsaWQ=\n    client-key-data: \"@@@\"", wantError: "decode client key"},
		{name: "invalid client key pair", cluster: "    server: https://example.test", user: "    client-certificate-data: " + base64.StdEncoding.EncodeToString([]byte("invalid cert")) + "\n    client-key-data: " + base64.StdEncoding.EncodeToString([]byte("invalid key")), wantError: "load client certificate"},
		{name: "missing client certificate files", cluster: "    server: https://example.test", user: "    client-certificate: " + missingFile + "-cert\n    client-key: " + missingFile + "-key", wantError: "load client certificate files"},
		{name: "unsupported auth", cluster: "    server: https://example.test", user: "    {}", wantError: "resolve auth token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clusterEntry := ""
			if tc.cluster != "" {
				clusterEntry = "- name: test-cluster\n  cluster:\n" + tc.cluster + "\n"
			}
			userEntry := ""
			if tc.user != "" {
				userEntry = "- name: test-user\n  user:\n" + tc.user + "\n"
			}
			content := "current-context: test-ctx\nclusters:\n" + clusterEntry + "contexts:\n- name: test-ctx\n  context:\n    cluster: test-cluster\n    user: test-user\nusers:\n" + userEntry
			configPath := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
				t.Fatalf("write kubeconfig: %v", err)
			}
			t.Setenv("KUBECONFIG", configPath)

			_, _, err := resolveKubeAPITransport("")
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("resolveKubeAPITransport() error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestResolveKubeAPITransportAcceptsCAFile(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, []byte("not a certificate"), 0600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	content := "current-context: test-ctx\nclusters:\n- name: test-cluster\n  cluster:\n    server: https://example.test///\n    certificate-authority: " + caFile + "\ncontexts:\n- name: test-ctx\n  context:\n    cluster: test-cluster\n    user: test-user\nusers:\n- name: test-user\n  user:\n    token: valid\n"
	configPath := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", configPath)

	host, httpClient, err := resolveKubeAPITransport("")
	if err != nil {
		t.Fatalf("resolveKubeAPITransport(): %v", err)
	}
	if host != "https://example.test" || httpClient.Transport == nil {
		t.Fatalf("transport = (%q, %#v), want trimmed host and HTTP transport", host, httpClient.Transport)
	}
}

func TestResolveKubeTransport_ExplicitContext(t *testing.T) {
	content := `apiVersion: v1
kind: Config
current-context: default-ctx
clusters:
- name: alt-cluster
  cluster:
    server: https://alt.example.com
    insecure-skip-tls-verify: true
contexts:
- name: default-ctx
  context:
    cluster: alt-cluster
    user: alt-user
- name: alt-ctx
  context:
    cluster: alt-cluster
    user: alt-user
users:
- name: alt-user
  user:
    token: alt-token
`
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config")
	os.WriteFile(cfgPath, []byte(content), 0600)
	t.Setenv("KUBECONFIG", cfgPath)

	proxy, err := resolveKubeTransport("alt-ctx", "my-ns", "my-svc", 9090)
	if err != nil {
		t.Fatalf("resolveKubeTransport() error: %v", err)
	}

	wantURL := "https://alt.example.com/api/v1/namespaces/my-ns/services/my-svc:9090/proxy"
	if proxy.baseURL != wantURL {
		t.Errorf("baseURL = %q, want %q", proxy.baseURL, wantURL)
	}
}

func TestKubeConfigPathSeparator(t *testing.T) {
	got := kubeConfigPathSeparator()
	if runtime.GOOS == "windows" {
		if got != ";" {
			t.Errorf("on windows, got %q, want %q", got, ";")
		}
	} else {
		if got != ":" {
			t.Errorf("on unix, got %q, want %q", got, ":")
		}
	}
}
