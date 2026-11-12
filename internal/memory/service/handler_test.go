// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package service_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/internal/memory/inmemory"
	"github.com/kognition-ai/kognition/internal/memory/service"
)

func setupTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	backend := inmemory.New()
	logger := zaptest.NewLogger(t)
	handler := service.NewHandler(backend, logger)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return httptest.NewServer(mux)
}

func TestProvisionAndStore(t *testing.T) {
	srv := setupTestServer(t)
	defer srv.Close()

	// Provision a volume
	provBody, _ := json.Marshal(service.ProvisionRequest{
		VolumeHandle:       "test-vol",
		EmbeddingDimension: 3,
	})
	resp, err := http.Post(srv.URL+"/v1/memory/provision", "application/json", bytes.NewReader(provBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("provision: expected 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Store a record
	now := time.Now().UTC().Truncate(time.Millisecond)
	storeBody, _ := json.Marshal(service.StoreMemoryRequest{
		VolumeHandle: "test-vol",
		ID:           "rec-1",
		Content:      "hello world",
		Embedding:    []float32{1.0, 0.0, 0.0},
		Metadata:     map[string]string{"source": "test"},
		Timestamp:    &now,
	})
	resp, err = http.Post(srv.URL+"/v1/memory/store", "application/json", bytes.NewReader(storeBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("store: expected 201, got %d", resp.StatusCode)
	}
	var storeResp service.StoreMemoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&storeResp); err != nil {
		t.Fatal(err)
	}
	if storeResp.ID != "rec-1" {
		t.Fatalf("store: expected ID rec-1, got %s", storeResp.ID)
	}
}

func TestGetRecord(t *testing.T) {
	srv := setupTestServer(t)
	defer srv.Close()

	// Provision + store
	provBody, _ := json.Marshal(service.ProvisionRequest{VolumeHandle: "vol"})
	resp, _ := http.Post(srv.URL+"/v1/memory/provision", "application/json", bytes.NewReader(provBody))
	resp.Body.Close()

	storeBody, _ := json.Marshal(service.StoreMemoryRequest{
		VolumeHandle: "vol",
		ID:           "r1",
		Content:      "data",
	})
	resp, _ = http.Post(srv.URL+"/v1/memory/store", "application/json", bytes.NewReader(storeBody))
	resp.Body.Close()

	// Get
	resp, err := http.Get(srv.URL + "/v1/memory/get/vol/r1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", resp.StatusCode)
	}
	var rec service.RecordDTO
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		t.Fatal(err)
	}
	if rec.ID != "r1" || rec.Content != "data" {
		t.Fatalf("get: unexpected record: %+v", rec)
	}
}

func TestGetNotFound(t *testing.T) {
	srv := setupTestServer(t)
	defer srv.Close()

	// Provision only
	provBody, _ := json.Marshal(service.ProvisionRequest{VolumeHandle: "vol"})
	resp, _ := http.Post(srv.URL+"/v1/memory/provision", "application/json", bytes.NewReader(provBody))
	resp.Body.Close()

	resp, err := http.Get(srv.URL + "/v1/memory/get/vol/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get not found: expected 404, got %d", resp.StatusCode)
	}
}

func TestSearch(t *testing.T) {
	srv := setupTestServer(t)
	defer srv.Close()

	// Provision + store 2 records
	provBody, _ := json.Marshal(service.ProvisionRequest{VolumeHandle: "vol", EmbeddingDimension: 3})
	resp, _ := http.Post(srv.URL+"/v1/memory/provision", "application/json", bytes.NewReader(provBody))
	resp.Body.Close()

	for _, rec := range []service.StoreMemoryRequest{
		{VolumeHandle: "vol", ID: "a", Content: "first", Embedding: []float32{1, 0, 0}},
		{VolumeHandle: "vol", ID: "b", Content: "second", Embedding: []float32{0, 1, 0}},
	} {
		body, _ := json.Marshal(rec)
		resp, _ := http.Post(srv.URL+"/v1/memory/store", "application/json", bytes.NewReader(body))
		resp.Body.Close()
	}

	// Search
	searchBody, _ := json.Marshal(service.SearchMemoryRequest{
		VolumeHandle: "vol",
		Vector:       []float32{1, 0, 0},
		TopK:         1,
	})
	resp, err := http.Post(srv.URL+"/v1/memory/search", "application/json", bytes.NewReader(searchBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search: expected 200, got %d", resp.StatusCode)
	}
	var results []service.SearchMemoryResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Record.ID != "a" {
		t.Fatalf("search: unexpected results: %+v", results)
	}
}

func TestDelete(t *testing.T) {
	srv := setupTestServer(t)
	defer srv.Close()

	// Provision + store
	provBody, _ := json.Marshal(service.ProvisionRequest{VolumeHandle: "vol"})
	resp, _ := http.Post(srv.URL+"/v1/memory/provision", "application/json", bytes.NewReader(provBody))
	resp.Body.Close()

	storeBody, _ := json.Marshal(service.StoreMemoryRequest{VolumeHandle: "vol", ID: "r1", Content: "data"})
	resp, _ = http.Post(srv.URL+"/v1/memory/store", "application/json", bytes.NewReader(storeBody))
	resp.Body.Close()

	// Delete
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/memory/delete/vol/r1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d", resp.StatusCode)
	}

	// Verify deleted
	resp, _ = http.Get(srv.URL + "/v1/memory/get/vol/r1")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", resp.StatusCode)
	}
}

func TestHealth(t *testing.T) {
	srv := setupTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/memory/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: expected 200, got %d", resp.StatusCode)
	}
}

func TestList(t *testing.T) {
	srv := setupTestServer(t)
	defer srv.Close()

	// Provision + store
	provBody, _ := json.Marshal(service.ProvisionRequest{VolumeHandle: "vol"})
	resp, _ := http.Post(srv.URL+"/v1/memory/provision", "application/json", bytes.NewReader(provBody))
	resp.Body.Close()

	for _, rec := range []service.StoreMemoryRequest{
		{VolumeHandle: "vol", ID: "a", Content: "first", Metadata: map[string]string{"type": "note"}},
		{VolumeHandle: "vol", ID: "b", Content: "second", Metadata: map[string]string{"type": "doc"}},
	} {
		body, _ := json.Marshal(rec)
		resp, _ := http.Post(srv.URL+"/v1/memory/store", "application/json", bytes.NewReader(body))
		resp.Body.Close()
	}

	// List with metadata filter
	listBody, _ := json.Marshal(service.ListMemoryRequest{
		VolumeHandle: "vol",
		Metadata:     map[string]string{"type": "note"},
	})
	resp, err := http.Post(srv.URL+"/v1/memory/list", "application/json", bytes.NewReader(listBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", resp.StatusCode)
	}
	var records []service.RecordDTO
	if err := json.NewDecoder(resp.Body).Decode(&records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != "a" {
		t.Fatalf("list: unexpected results: %+v", records)
	}
}
