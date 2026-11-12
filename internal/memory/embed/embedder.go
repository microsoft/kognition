// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package embed provides a server-side text embedder for the memory service.
// It adapts the in-tree CPI provider drivers (openai-compatible, foundry,
// ollama) to the service.Embedder contract so records can be embedded on write
// and text queries embedded on search — keeping agent runtimes embedder-free.
package embed

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/foundry"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/ollama"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/openai"
)

const defaultFoundryAPIVersion = "2024-02-01"

// Config describes an embedding deployment for the memory service.
type Config struct {
	// Provider selects the CPI driver: "openai-compatible", "foundry", or "ollama".
	Provider string
	// Endpoint is the provider base URL.
	Endpoint string
	// Model is the embedding model/deployment identifier.
	Model string
	// APIKey is an optional bearer/API key credential.
	APIKey string
	// APIVersion selects the Foundry REST API version. Other providers ignore it.
	APIVersion string
}

// Enabled reports whether the configuration requests an embedder. Provider,
// Endpoint, and Model must all be present.
func (c Config) Enabled() bool {
	return c.Provider != "" && c.Endpoint != "" && c.Model != ""
}

// Requested reports whether any required embedding setting was supplied.
// It distinguishes an intentionally disabled embedder from a partial config.
func (c Config) Requested() bool {
	return c.Provider != "" || c.Endpoint != "" || c.Model != "" || c.APIKey != "" || c.APIVersion != ""
}

// CPIEmbedder embeds text via a CPI provider driver.
type CPIEmbedder struct {
	driver   cpi.Driver
	model    string
	endpoint string
	creds    *cpi.Credentials
	params   map[string]string
}

// New constructs a CPIEmbedder from config. Returns an error when the config is
// incomplete or names an unsupported provider.
func New(cfg Config) (*CPIEmbedder, error) {
	if !cfg.Enabled() {
		return nil, fmt.Errorf("embedder config requires provider, endpoint, and model")
	}
	if cfg.APIVersion != "" && cfg.Provider != "foundry" {
		return nil, fmt.Errorf("embedding apiVersion is only supported by the foundry provider")
	}
	parsedEndpoint, err := url.ParseRequestURI(cfg.Endpoint)
	if err != nil || parsedEndpoint.Scheme == "" || parsedEndpoint.Host == "" {
		return nil, fmt.Errorf("embedding endpoint must be a valid absolute URL")
	}
	if parsedEndpoint.Scheme != "http" && parsedEndpoint.Scheme != "https" {
		return nil, fmt.Errorf("embedding endpoint must use HTTP or HTTPS")
	}
	if cfg.APIKey != "" && parsedEndpoint.Scheme != "https" && !isLoopbackHost(parsedEndpoint.Hostname()) {
		return nil, fmt.Errorf("embedding endpoint must use HTTPS when an API key is configured")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var driver cpi.Driver
	switch cfg.Provider {
	case "openai-compatible":
		driver = openai.New(client)
	case "foundry":
		driver = foundry.New(client)
	case "ollama":
		driver = ollama.New(client)
	default:
		return nil, fmt.Errorf("unsupported embedding provider %q", cfg.Provider)
	}

	var creds *cpi.Credentials
	if cfg.APIKey != "" {
		creds = &cpi.Credentials{Type: "api-key", Data: map[string]string{"apiKey": cfg.APIKey}}
	} else if cfg.Provider == "foundry" {
		creds = &cpi.Credentials{Type: "azure-identity", Data: map[string]string{"credentialType": "default"}}
	}
	var params map[string]string
	if cfg.Provider == "foundry" {
		apiVersion := cfg.APIVersion
		if apiVersion == "" {
			apiVersion = defaultFoundryAPIVersion
		}
		params = map[string]string{"api-version": apiVersion}
	}

	return &CPIEmbedder{
		driver:   driver,
		model:    cfg.Model,
		endpoint: cfg.Endpoint,
		creds:    creds,
		params:   params,
	}, nil
}

func isLoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

// Embed returns the embedding vector for a single text input.
func (e *CPIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	resp, err := e.driver.Embed(ctx, &cpi.EmbedRequest{
		Model:      e.model,
		Endpoint:   e.endpoint,
		Creds:      e.creds,
		Inputs:     []string{text},
		Parameters: e.params,
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Embeddings) == 0 || len(resp.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("embedder returned no vector")
	}

	vec := make([]float32, len(resp.Embeddings[0]))
	for i, v := range resp.Embeddings[0] {
		vec[i] = float32(v)
	}
	return vec, nil
}
