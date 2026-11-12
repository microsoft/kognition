// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/internal/memory/service"
	"github.com/kognition-ai/kognition/internal/memory/serviceapp"
)

func postMemoryJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("create POST %s: %v", url, err)
	}
	request.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// US-0084: Environment-configured Foundry-compatible embeddings flow through
// the production memory-service composition and HTTP handler boundary.
func TestMemoryService_EmbedsStoreAndSearch(t *testing.T) {
	var inputsMu sync.Mutex
	var inputs []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/deployments/embedding-deployment/embeddings" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if got := r.URL.Query().Get("api-version"); got != "2025-01-01-preview" {
			http.Error(w, "unexpected API version", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("api-key"); got != "test-key" {
			http.Error(w, "unexpected API key", http.StatusUnauthorized)
			return
		}

		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Input) != 1 {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		input := request.Input[0]
		inputsMu.Lock()
		inputs = append(inputs, input)
		inputsMu.Unlock()

		vector := []float64{0, 1}
		if input == "release policy" || input == "deployment rules" {
			vector = []float64{1, 0}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  []map[string]any{{"embedding": vector}},
			"usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1},
		})
	}))
	defer provider.Close()

	t.Setenv("MEMORY_BACKEND_TYPE", "in-memory")
	t.Setenv("MEMORY_EMBEDDING_PROVIDER", "foundry")
	t.Setenv("MEMORY_EMBEDDING_ENDPOINT", provider.URL)
	t.Setenv("MEMORY_EMBEDDING_MODEL", "embedding-deployment")
	t.Setenv("MEMORY_EMBEDDING_API_KEY", "test-key")
	t.Setenv("MEMORY_EMBEDDING_API_VERSION", "2025-01-01-preview")
	handler, cleanup, err := serviceapp.NewHandler(t.Context(), serviceapp.ConfigFromEnv(), zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("compose memory service: %v", err)
	}
	defer cleanup()
	memoryService := httptest.NewServer(handler)
	defer memoryService.Close()

	resp := postMemoryJSON(t, memoryService.URL+"/v1/memory/provision", service.ProvisionRequest{
		VolumeHandle:       "integration-volume",
		EmbeddingDimension: 2,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("provision status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}

	for _, record := range []service.StoreMemoryRequest{
		{VolumeHandle: "integration-volume", ID: "relevant", Content: "release policy"},
		{VolumeHandle: "integration-volume", ID: "unrelated", Content: "lunch menu"},
	} {
		resp = postMemoryJSON(t, memoryService.URL+"/v1/memory/store", record)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("store %q status = %d, want %d", record.ID, resp.StatusCode, http.StatusCreated)
		}
	}

	resp = postMemoryJSON(t, memoryService.URL+"/v1/memory/search", service.SearchMemoryRequest{
		VolumeHandle: "integration-volume",
		Query:        "deployment rules",
		TopK:         2,
		MinScore:     0.5,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var results []service.SearchMemoryResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode search results: %v", err)
	}
	if len(results) != 1 || results[0].Record.ID != "relevant" {
		t.Fatalf("search results = %+v, want only relevant record", results)
	}

	inputsMu.Lock()
	defer inputsMu.Unlock()
	wantInputs := []string{"release policy", "lunch menu", "deployment rules"}
	if len(inputs) != len(wantInputs) {
		t.Fatalf("provider inputs = %v, want %v", inputs, wantInputs)
	}
	for i := range wantInputs {
		if inputs[i] != wantInputs[i] {
			t.Fatalf("provider inputs = %v, want %v", inputs, wantInputs)
		}
	}
}
