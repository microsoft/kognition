// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package embed

import (
	"context"
	"errors"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
)

// fakeDriver implements cpi.Driver; only Embed is meaningful.
type fakeDriver struct {
	resp      *cpi.EmbedResponse
	err       error
	gotInputs []string
	gotParams map[string]string
}

func (f *fakeDriver) Name() string { return "fake" }
func (f *fakeDriver) Validate(_ context.Context, _ *v1alpha1.ModelSpec, _ *v1alpha1.ModelClassSpec) error {
	return nil
}
func (f *fakeDriver) Probe(_ context.Context, _ *v1alpha1.ModelSpec, _ *cpi.Credentials) (*cpi.HealthResult, error) {
	return nil, nil
}
func (f *fakeDriver) Complete(_ context.Context, _ *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	return nil, nil
}
func (f *fakeDriver) Embed(_ context.Context, req *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	f.gotInputs = req.Inputs
	f.gotParams = req.Parameters
	return f.resp, f.err
}

func TestConfig_Enabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"complete", Config{Provider: "ollama", Endpoint: "http://x", Model: "m"}, true},
		{"missing provider", Config{Endpoint: "http://x", Model: "m"}, false},
		{"missing endpoint", Config{Provider: "ollama", Model: "m"}, false},
		{"missing model", Config{Provider: "ollama", Endpoint: "http://x"}, false},
		{"empty", Config{}, false},
	}
	for _, tc := range cases {
		if got := tc.cfg.Enabled(); got != tc.want {
			t.Errorf("%s: Enabled()=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNew_IncompleteConfig(t *testing.T) {
	for _, cfg := range []Config{
		{Provider: "ollama"},
		{APIKey: "orphaned"},
		{APIVersion: "2025-01-01-preview"},
	} {
		if !cfg.Requested() {
			t.Fatalf("configuration %+v should be considered requested", cfg)
		}
		if _, err := New(cfg); err == nil {
			t.Fatalf("expected error for incomplete config %+v", cfg)
		}
	}
}

func TestNew_APIVersionRequiresFoundry(t *testing.T) {
	_, err := New(Config{
		Provider:   "openai-compatible",
		Endpoint:   "https://api.openai.com/v1",
		Model:      "text-embedding-3-small",
		APIVersion: "2025-01-01-preview",
	})
	if err == nil {
		t.Fatal("expected non-Foundry apiVersion to be rejected")
	}
}

func TestNew_UnsupportedProvider(t *testing.T) {
	if _, err := New(Config{Provider: "telepathy", Endpoint: "http://x", Model: "m"}); err == nil {
		t.Fatal("expected error for unsupported provider")
	}
}

func TestNew_SupportedProviders(t *testing.T) {
	for _, p := range []string{"openai-compatible", "foundry", "ollama"} {
		cfg := Config{Provider: p, Endpoint: "https://example.com", Model: "m"}
		if p == "foundry" {
			cfg.APIKey = "secret"
		}
		e, err := New(cfg)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", p, err)
		}
		if e == nil || e.driver == nil {
			t.Fatalf("%s: expected constructed embedder", p)
		}
	}
}

func TestNew_FoundryDefaultsToAzureIdentity(t *testing.T) {
	e, err := New(Config{Provider: "foundry", Endpoint: "https://example.openai.azure.com", Model: "m"})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if e.creds == nil || e.creds.Type != "azure-identity" || e.creds.Data["credentialType"] != "default" {
		t.Fatalf("expected default Azure identity credentials, got %+v", e.creds)
	}
}

func TestNew_RejectsInvalidEndpoint(t *testing.T) {
	for _, endpoint := range []string{"not-a-url", "ftp://example.com"} {
		if _, err := New(Config{Provider: "ollama", Endpoint: endpoint, Model: "m"}); err == nil {
			t.Fatalf("expected endpoint %q to be rejected", endpoint)
		}
	}
}

func TestNew_APIKeyRequiresSecureRemoteEndpoint(t *testing.T) {
	if _, err := New(Config{Provider: "foundry", Endpoint: "http://example.com", Model: "m", APIKey: "secret"}); err == nil {
		t.Fatal("expected plaintext remote endpoint with API key to be rejected")
	}
	for _, endpoint := range []string{"http://127.0.0.1:8080", "http://LOCALHOST:8080"} {
		if _, err := New(Config{Provider: "foundry", Endpoint: endpoint, Model: "m", APIKey: "secret"}); err != nil {
			t.Fatalf("loopback development endpoint %q should be allowed: %v", endpoint, err)
		}
	}
}

func TestNew_APIKeySetsCredentials(t *testing.T) {
	e, err := New(Config{Provider: "openai-compatible", Endpoint: "https://example.com", Model: "m", APIKey: "secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.creds == nil || e.creds.Data["apiKey"] != "secret" {
		t.Fatalf("expected api-key credentials, got %+v", e.creds)
	}
}

func TestNew_FoundryAPIVersion(t *testing.T) {
	tests := []struct {
		name       string
		apiVersion string
		want       string
	}{
		{name: "default", want: defaultFoundryAPIVersion},
		{name: "override", apiVersion: "2025-01-01-preview", want: "2025-01-01-preview"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := New(Config{
				Provider:   "foundry",
				Endpoint:   "https://example.openai.azure.com",
				Model:      "embedding-deployment",
				APIKey:     "secret",
				APIVersion: tc.apiVersion,
			})
			if err != nil {
				t.Fatalf("New() error: %v", err)
			}
			if got := e.params["api-version"]; got != tc.want {
				t.Fatalf("api-version = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEmbed_ConvertsFloat64ToFloat32(t *testing.T) {
	fd := &fakeDriver{resp: &cpi.EmbedResponse{Embeddings: [][]float64{{0.5, -0.25, 2}}}}
	e := &CPIEmbedder{driver: fd, model: "m", endpoint: "http://x"}

	vec, err := e.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	want := []float32{0.5, -0.25, 2}
	if len(vec) != len(want) {
		t.Fatalf("length mismatch: got %v", vec)
	}
	for i := range want {
		if vec[i] != want[i] {
			t.Fatalf("index %d: got %v want %v", i, vec[i], want[i])
		}
	}
	if len(fd.gotInputs) != 1 || fd.gotInputs[0] != "hello" {
		t.Fatalf("expected single input 'hello', got %v", fd.gotInputs)
	}
}

func TestEmbed_PassesProviderParameters(t *testing.T) {
	fd := &fakeDriver{resp: &cpi.EmbedResponse{Embeddings: [][]float64{{1}}}}
	e := &CPIEmbedder{
		driver:   fd,
		model:    "m",
		endpoint: "http://x",
		params:   map[string]string{"api-version": "2024-02-01"},
	}
	if _, err := e.Embed(context.Background(), "hello"); err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
	if got := fd.gotParams["api-version"]; got != "2024-02-01" {
		t.Fatalf("api-version = %q, want %q", got, "2024-02-01")
	}
}

func TestEmbed_EmptyResponseError(t *testing.T) {
	e := &CPIEmbedder{driver: &fakeDriver{resp: &cpi.EmbedResponse{}}, model: "m", endpoint: "http://x"}
	if _, err := e.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected error on empty embeddings")
	}
}

func TestEmbed_DriverError(t *testing.T) {
	e := &CPIEmbedder{driver: &fakeDriver{err: errors.New("boom")}, model: "m", endpoint: "http://x"}
	if _, err := e.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected driver error to propagate")
	}
}
