// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package serviceapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/internal/memory/embed"
	"github.com/kognition-ai/kognition/internal/memory/service"
	"github.com/kognition-ai/kognition/internal/observability"
)

type memoryAuditStore struct {
	entries []observability.AuditEntry
	err     error
}

func (s *memoryAuditStore) Append(_ context.Context, entry observability.AuditEntry) error {
	if s.err != nil {
		return s.err
	}
	s.entries = append(s.entries, entry)
	return nil
}

func (s *memoryAuditStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	for index := range entries {
		if err := s.Append(ctx, entries[index]); err != nil {
			return err
		}
	}
	return nil
}

func (s *memoryAuditStore) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return append([]observability.AuditEntry(nil), s.entries...), nil
}

func TestConfigFromEnv(t *testing.T) {
	for _, key := range []string{
		"MEMORY_SERVICE_ADDR", "MEMORY_BACKEND_TYPE", "DATABASE_URL", "MEM0_API_URL",
		"MEM0_API_KEY", "MEM0_ORG_ID", "MEM0_PROJECT_ID", "MEMORY_EMBEDDING_PROVIDER",
		"MEMORY_EMBEDDING_ENDPOINT", "MEMORY_EMBEDDING_MODEL", "MEMORY_EMBEDDING_API_KEY",
		"MEMORY_EMBEDDING_API_VERSION", "MEMORY_SERVICE_INTERNAL_TOKEN", "KOGNITION_AUDIT_NAMESPACE",
	} {
		t.Setenv(key, "")
	}

	cfg := ConfigFromEnv()
	if cfg.Addr != ":8090" || cfg.BackendType != "in-memory" || cfg.Mem0BaseURL != "https://api.mem0.ai" {
		t.Fatalf("default config = %+v", cfg)
	}
	if cfg.DatabaseURL != "" || cfg.Embedding.Requested() {
		t.Fatalf("optional config should be empty by default: %+v", cfg)
	}

	t.Setenv("MEMORY_SERVICE_ADDR", "127.0.0.1:9090")
	t.Setenv("MEMORY_BACKEND_TYPE", "mem0")
	t.Setenv("DATABASE_URL", "postgres://database")
	t.Setenv("MEM0_API_URL", "https://mem0.example.test")
	t.Setenv("MEM0_API_KEY", "mem0-key")
	t.Setenv("MEM0_ORG_ID", "org-id")
	t.Setenv("MEM0_PROJECT_ID", "project-id")
	t.Setenv("MEMORY_EMBEDDING_PROVIDER", "foundry")
	t.Setenv("MEMORY_EMBEDDING_ENDPOINT", "https://models.example.test")
	t.Setenv("MEMORY_EMBEDDING_MODEL", "embedding-model")
	t.Setenv("MEMORY_EMBEDDING_API_KEY", "embedding-key")
	t.Setenv("MEMORY_EMBEDDING_API_VERSION", "2025-01-01-preview")
	t.Setenv("MEMORY_SERVICE_INTERNAL_TOKEN", "internal-token")
	t.Setenv("KOGNITION_AUDIT_NAMESPACE", "kognition-system")

	cfg = ConfigFromEnv()
	if cfg.Addr != "127.0.0.1:9090" || cfg.BackendType != "mem0" || cfg.DatabaseURL != "postgres://database" {
		t.Fatalf("process config not mapped from environment: %+v", cfg)
	}
	if cfg.Mem0BaseURL != "https://mem0.example.test" || cfg.Mem0APIKey != "mem0-key" ||
		cfg.Mem0OrgID != "org-id" || cfg.Mem0ProjectID != "project-id" {
		t.Fatalf("Mem0 config not mapped from environment: %+v", cfg)
	}
	if cfg.InternalToken != "internal-token" {
		t.Fatalf("internal token = %q", cfg.InternalToken)
	}
	if cfg.AuditNamespace != "kognition-system" {
		t.Fatalf("audit namespace = %q", cfg.AuditNamespace)
	}
	wantEmbedding := embed.Config{
		Provider: "foundry", Endpoint: "https://models.example.test", Model: "embedding-model",
		APIKey: "embedding-key", APIVersion: "2025-01-01-preview",
	}
	if cfg.Embedding != wantEmbedding {
		t.Fatalf("embedding config = %+v, want %+v", cfg.Embedding, wantEmbedding)
	}
}

func TestEmbeddingConfigDistinguishesDisabledFromPartial(t *testing.T) {
	disabled := embed.Config{}
	if disabled.Requested() || disabled.Enabled() {
		t.Fatal("empty embedding config should be disabled and not requested")
	}

	partial := embed.Config{Provider: "foundry"}
	if !partial.Requested() || partial.Enabled() {
		t.Fatal("partial embedding config should be requested but not enabled")
	}
	if _, err := embed.New(partial); err == nil {
		t.Fatal("partial embedding config should be rejected")
	}
}

func TestSelectBackend(t *testing.T) {
	tests := []struct {
		name       string
		config     Config
		wantErr    string
		wantNonNil bool
	}{
		{name: "in-memory", config: Config{BackendType: "in-memory"}, wantNonNil: true},
		{name: "mem0", config: Config{BackendType: "mem0", Mem0BaseURL: "https://api.mem0.ai"}, wantNonNil: true},
		{name: "pgvector missing dsn", config: Config{BackendType: "pgvector"}, wantErr: "requires DATABASE_URL"},
		{name: "pgvector invalid dsn", config: Config{BackendType: "pgvector", DatabaseURL: "not-a-valid-dsn://"}, wantErr: "connect pgvector pool"},
		{name: "unknown", config: Config{BackendType: "bogus"}, wantErr: "unsupported backend type"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backend, cleanup, err := selectBackend(context.Background(), tc.config, zaptest.NewLogger(t))
			if cleanup != nil {
				defer cleanup()
			}

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectBackend() error: %v", err)
			}
			if tc.wantNonNil && backend == nil {
				t.Fatal("expected non-nil backend")
			}
			if cleanup == nil {
				t.Fatal("expected non-nil cleanup")
			}
		})
	}
}

func TestNewHandlerRejectsPartialEmbeddingConfig(t *testing.T) {
	handler, cleanup, err := NewHandler(context.Background(), Config{
		BackendType: "in-memory",
		Embedding:   embed.Config{Provider: "foundry"},
	}, zaptest.NewLogger(t))
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "init embedder") {
		t.Fatalf("NewHandler() error = %v, want embedder initialization error", err)
	}
	if handler != nil {
		t.Fatal("NewHandler() returned a handler for invalid configuration")
	}
}

func TestNewHandlerPropagatesBackendSelectionError(t *testing.T) {
	handler, cleanup, err := NewHandler(context.Background(), Config{BackendType: "unknown"}, zaptest.NewLogger(t))
	if cleanup != nil {
		cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported backend type") {
		t.Fatalf("NewHandler() error = %v", err)
	}
	if handler != nil {
		t.Fatal("NewHandler() returned a handler for an unknown backend")
	}
}

func TestNewHandlerAcceptsCompleteEmbeddingConfig(t *testing.T) {
	handler, cleanup, err := NewHandler(context.Background(), Config{
		BackendType: "in-memory",
		Embedding: embed.Config{
			Provider: "openai-compatible",
			Endpoint: "https://models.example.test",
			Model:    "embedding-model",
		},
	}, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	defer cleanup()
	if handler == nil {
		t.Fatal("NewHandler() returned nil handler")
	}
}

func TestNewHandlerWithoutEmbeddingUsesTextSearch(t *testing.T) {
	handler, cleanup, err := NewHandler(context.Background(), Config{BackendType: "in-memory"}, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("NewHandler() error: %v", err)
	}
	defer cleanup()
	server := httptest.NewServer(handler)
	defer server.Close()

	post := func(path string, body any) *http.Response {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		resp, err := http.Post(server.URL+path, "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		return resp
	}

	resp := post("/v1/memory/provision", service.ProvisionRequest{VolumeHandle: "vol"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("provision status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	resp = post("/v1/memory/store", service.StoreMemoryRequest{VolumeHandle: "vol", ID: "release", Content: "release policy"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("store status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	resp = post("/v1/memory/search", service.SearchMemoryRequest{VolumeHandle: "vol", Query: "release policy", TopK: 1})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var results []service.SearchMemoryResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode search results: %v", err)
	}
	if len(results) != 1 || results[0].Record.ID != "release" {
		t.Fatalf("search results = %+v, want release record", results)
	}
}

func TestNewHandler_UsesConfiguredAuditStoreBeforeMemoryWrite(t *testing.T) {
	auditStore := &memoryAuditStore{}
	handler, cleanup, err := newHandler(context.Background(), Config{BackendType: "in-memory", AuditNamespace: "kognition-system"}, zaptest.NewLogger(t), func(namespace string) (observability.AuditStore, error) {
		if namespace != "kognition-system" {
			t.Fatalf("audit namespace = %q", namespace)
		}
		return auditStore, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	server := httptest.NewServer(handler)
	defer server.Close()

	post := func(path string, body any) *http.Response {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.Post(server.URL+path, "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := post("/v1/memory/provision", service.ProvisionRequest{VolumeHandle: "workspace"})
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("provision status = %d", response.StatusCode)
	}
	response = post("/v1/memory/store", service.StoreMemoryRequest{VolumeHandle: "workspace", ID: "record", Content: "content"})
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || len(auditStore.entries) != 1 || auditStore.entries[0].Verb != "store" {
		t.Fatalf("store status=%d audit=%#v", response.StatusCode, auditStore.entries)
	}

	auditStore.err = errors.New("audit unavailable")
	deleteRequest, err := http.NewRequest(http.MethodDelete, server.URL+"/v1/memory/delete/workspace/record", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(deleteRequest)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("delete status = %d, want audit failure", response.StatusCode)
	}
}
