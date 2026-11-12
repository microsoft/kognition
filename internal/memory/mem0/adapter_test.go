// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mem0

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/internal/memory"
)

func TestConfigFromParameters(t *testing.T) {
	config := ConfigFromParameters(map[string]string{
		"baseURL":   "https://mem0.example",
		"apiKey":    "secret",
		"orgId":     "org-1",
		"projectId": "project-1",
	})
	if config.BaseURL != "https://mem0.example" || config.APIKey != "secret" || config.OrgID != "org-1" || config.ProjectID != "project-1" {
		t.Fatalf("unexpected config: %+v", config)
	}
}

func TestAdapterCRUDPreservesScopeAndAuthentication(t *testing.T) {
	requests := make([]string, 0, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Token secret" {
			t.Errorf("Authorization = %q, want Token secret", got)
		}

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/memories/":
			var body mem0AddRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode store request: %v", err)
			}
			if body.UserID != "volume-1" || body.MemoryID != "requested-id" || body.OrgID != "org-1" || body.ProjectID != "project-1" {
				t.Errorf("unexpected store scope: %+v", body)
			}
			if len(body.Messages) != 1 || body.Messages[0].Role != "user" || body.Messages[0].Content != "remember this" {
				t.Errorf("unexpected messages: %+v", body.Messages)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]string{"id": "stored-id"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/memories/stored-id/":
			_ = json.NewEncoder(w).Encode(memoryJSON("stored-id", "shared"))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/memories/stored-id/":
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/memories/":
			if got := r.URL.Query().Get("user_id"); got != "volume-1" {
				t.Errorf("deprovision user_id = %q, want volume-1", got)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := New(Config{BaseURL: server.URL, APIKey: "secret", OrgID: "org-1", ProjectID: "project-1"}, zaptest.NewLogger(t))
	if err := adapter.Provision(context.Background(), memory.ProvisionRequest{BackendHandle: "volume-1"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	stored, err := adapter.Store(context.Background(), memory.StoreRequest{
		BackendHandle: "volume-1",
		ID:            "requested-id",
		Content:       "remember this",
		Metadata:      map[string]string{"scope": "shared"},
	})
	if err != nil || stored.ID != "stored-id" {
		t.Fatalf("Store = %+v, %v", stored, err)
	}
	record, err := adapter.Get(context.Background(), memory.GetRequest{ID: stored.ID, BackendHandle: "volume-1"})
	if err != nil || record.ID != "stored-id" || record.Content != "stored-id content" || record.Metadata["scope"] != "shared" {
		t.Fatalf("Get = %+v, %v", record, err)
	}
	if err := adapter.Delete(context.Background(), memory.DeleteRequest{ID: stored.ID, BackendHandle: "volume-1"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := adapter.Deprovision(context.Background(), memory.DeprovisionRequest{BackendHandle: "volume-1"}); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}

	wantRequests := []string{
		"POST /v1/memories/",
		"GET /v1/memories/stored-id/",
		"DELETE /v1/memories/stored-id/",
		"DELETE /v1/memories/",
	}
	if len(requests) != len(wantRequests) {
		t.Fatalf("requests = %v, want %v", requests, wantRequests)
	}
	for index := range wantRequests {
		if requests[index] != wantRequests[index] {
			t.Fatalf("requests = %v, want %v", requests, wantRequests)
		}
	}
}

func TestAdapterErrorTranslationAndHealth(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		operation  func(*Adapter) error
		wantError  string
		wantTarget error
	}{
		{
			name:       "get not found",
			statusCode: http.StatusNotFound,
			operation: func(adapter *Adapter) error {
				_, err := adapter.Get(context.Background(), memory.GetRequest{ID: "missing"})
				return err
			},
			wantTarget: memory.ErrRecordNotFound,
		},
		{
			name:       "store API error",
			statusCode: http.StatusBadRequest,
			operation: func(adapter *Adapter) error {
				_, err := adapter.Store(context.Background(), memory.StoreRequest{Content: "invalid"})
				return err
			},
			wantError: "store to mem0: mem0 API error 400: failure",
		},
		{
			name:       "deprovision API error",
			statusCode: http.StatusConflict,
			operation: func(adapter *Adapter) error {
				return adapter.Deprovision(context.Background(), memory.DeprovisionRequest{BackendHandle: "volume-1"})
			},
			wantError: "mem0 API error 409: failure",
		},
		{
			name:       "list API error",
			statusCode: http.StatusBadGateway,
			operation: func(adapter *Adapter) error {
				_, err := adapter.List(context.Background(), memory.ListRequest{BackendHandle: "volume-1"})
				return err
			},
			wantError: "mem0 API error 502: failure",
		},
		{
			name:       "health unauthorized",
			statusCode: http.StatusUnauthorized,
			operation: func(adapter *Adapter) error {
				return adapter.Health(context.Background())
			},
			wantError: "mem0: invalid API key",
		},
		{
			name:       "health service failure",
			statusCode: http.StatusServiceUnavailable,
			operation: func(adapter *Adapter) error {
				return adapter.Health(context.Background())
			},
			wantError: "mem0: service returned 503",
		},
		{
			name:       "health rate limited",
			statusCode: http.StatusTooManyRequests,
			operation: func(adapter *Adapter) error {
				return adapter.Health(context.Background())
			},
			wantError: "mem0: service returned 429",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte("failure"))
			}))
			defer server.Close()

			adapter := New(Config{BaseURL: server.URL}, zaptest.NewLogger(t))
			err := test.operation(adapter)
			if test.wantTarget != nil {
				if !errors.Is(err, test.wantTarget) {
					t.Fatalf("error = %v, want target %v", err, test.wantTarget)
				}
				return
			}
			if err == nil || err.Error() != test.wantError {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestListPaginatesBeforeApplyingFilters(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("page_size") != "200" {
			t.Errorf("expected maximum page size, got %q", r.URL.Query().Get("page_size"))
		}
		response := map[string]any{"count": 2, "next": "", "results": []any{}}
		if r.URL.Query().Get("page") == "1" {
			response["next"] = "next-page"
			response["results"] = []any{memoryJSON("excluded", "private")}
		} else {
			response["results"] = []any{memoryJSON("included", "shared")}
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	adapter := New(Config{BaseURL: server.URL}, zaptest.NewLogger(t))
	records, err := adapter.List(context.Background(), memory.ListRequest{
		BackendHandle: "volume",
		Metadata:      map[string]string{"scope": "shared"},
		Limit:         1,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if requests != 2 || len(records) != 1 || records[0].ID != "included" {
		t.Fatalf("expected filtered record from second page, requests=%d records=%+v", requests, records)
	}
}

func TestSearchRejectsUnsupportedVectorSearch(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()

	adapter := New(Config{BaseURL: server.URL}, zaptest.NewLogger(t))
	_, err := adapter.Search(context.Background(), memory.SearchRequest{
		BackendHandle: "volume",
		Vector:        []float32{1, 0, 0},
		TopK:          1,
	})
	if err == nil {
		t.Fatal("expected unsupported vector search error")
	}
	if called {
		t.Fatal("vector search must fail before calling Mem0")
	}
}

func memoryJSON(id, scope string) map[string]any {
	return map[string]any{
		"id":         id,
		"memory":     id + " content",
		"user_id":    "volume",
		"metadata":   map[string]string{"scope": scope},
		"created_at": "2026-01-01T00:00:00Z",
	}
}

func TestListWithZeroLimitReturnsEveryPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := map[string]any{"count": 2, "next": "", "results": []any{memoryJSON("second", "shared")}}
		if r.URL.Query().Get("page") == "1" {
			response["next"] = "next-page"
			response["results"] = []any{memoryJSON("first", "shared")}
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	adapter := New(Config{BaseURL: server.URL}, zaptest.NewLogger(t))
	records, err := adapter.List(context.Background(), memory.ListRequest{BackendHandle: "volume"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 2 || records[0].ID != "first" || records[1].ID != "second" {
		t.Fatalf("expected all paginated records, got %+v", records)
	}
}

func TestListStopsWhenServerRepeatsPage(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 2, "next": "same-page", "results": []any{memoryJSON("first", "shared")},
		})
	}))
	defer server.Close()

	adapter := New(Config{BaseURL: server.URL}, zaptest.NewLogger(t))
	records, err := adapter.List(context.Background(), memory.ListRequest{BackendHandle: "volume"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if requests != 2 || len(records) != 1 {
		t.Fatalf("expected repeated page detection, requests=%d records=%+v", requests, records)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type failingResponseBody struct{}

func (failingResponseBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (failingResponseBody) Close() error             { return nil }

func TestAdapterHTTPAndDecodeBoundaries(t *testing.T) {
	ctx := context.Background()
	logger := zaptest.NewLogger(t)
	defaultAdapter := New(Config{}, logger)
	if defaultAdapter.config.BaseURL != "https://api.mem0.ai" {
		t.Fatalf("default BaseURL = %q", defaultAdapter.config.BaseURL)
	}

	malformed := New(Config{BaseURL: "://invalid"}, logger)
	for name, operation := range map[string]func() error{
		"deprovision": func() error { return malformed.Deprovision(ctx, memory.DeprovisionRequest{}) },
		"list":        func() error { _, err := malformed.List(ctx, memory.ListRequest{}); return err },
		"health":      func() error { return malformed.Health(ctx) },
	} {
		if err := operation(); err == nil || !strings.Contains(err.Error(), "request") {
			t.Errorf("%s error = %v, want request construction failure", name, err)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/memories/empty/":
			_, _ = w.Write([]byte(`{}`))
		case "/v1/memories/bad-get/", "/v1/memories/":
			_, _ = w.Write([]byte(`not-json`))
		case "/v1/memories/delete-failure/":
			http.Error(w, "cannot delete", http.StatusConflict)
		case "/v1/memories/healthy/":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter := New(Config{BaseURL: server.URL}, logger)

	if _, err := adapter.Get(ctx, memory.GetRequest{ID: "empty"}); !errors.Is(err, memory.ErrRecordNotFound) {
		t.Fatalf("empty Get() error = %v, want ErrRecordNotFound", err)
	}
	if _, err := adapter.Get(ctx, memory.GetRequest{ID: "bad-get"}); err == nil || !strings.Contains(err.Error(), "unmarshal get response") {
		t.Fatalf("malformed Get() error = %v", err)
	}
	if _, err := adapter.Store(ctx, memory.StoreRequest{Content: "value"}); err == nil || !strings.Contains(err.Error(), "unmarshal store response") {
		t.Fatalf("malformed Store() error = %v", err)
	}
	if _, err := adapter.List(ctx, memory.ListRequest{}); err == nil || !strings.Contains(err.Error(), "unmarshal list response") {
		t.Fatalf("malformed List() error = %v", err)
	}
	if err := adapter.Delete(ctx, memory.DeleteRequest{ID: "delete-failure"}); err == nil || !strings.Contains(err.Error(), "delete from mem0") {
		t.Fatalf("Delete() error = %v", err)
	}

	healthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer healthServer.Close()
	if err := New(Config{BaseURL: healthServer.URL}, logger).Health(ctx); err != nil {
		t.Fatalf("healthy Health() error = %v", err)
	}

	readFailure := New(Config{BaseURL: "http://mem0.test"}, logger)
	readFailure.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: failingResponseBody{}, Header: make(http.Header)}, nil
	})}
	if _, err := readFailure.doJSON(ctx, http.MethodGet, "/v1/memories/", nil); err == nil || !strings.Contains(err.Error(), "read response") {
		t.Fatalf("doJSON() read error = %v", err)
	}
	if _, err := adapter.doJSON(ctx, http.MethodPost, "/v1/memories/", make(chan int)); err == nil || !strings.Contains(err.Error(), "marshal request body") {
		t.Fatalf("doJSON() marshal error = %v", err)
	}

	closedServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closedServer.URL
	closedServer.Close()
	closedAdapter := New(Config{BaseURL: closedURL}, logger)
	if err := closedAdapter.Deprovision(ctx, memory.DeprovisionRequest{}); err == nil || !strings.Contains(err.Error(), "deprovision mem0 volume") {
		t.Fatalf("Deprovision() transport error = %v", err)
	}
	if _, err := closedAdapter.List(ctx, memory.ListRequest{}); err == nil || !strings.Contains(err.Error(), "list from mem0") {
		t.Fatalf("List() transport error = %v", err)
	}
	if err := closedAdapter.Health(ctx); err == nil || !strings.Contains(err.Error(), "health check failed") {
		t.Fatalf("Health() transport error = %v", err)
	}
}
