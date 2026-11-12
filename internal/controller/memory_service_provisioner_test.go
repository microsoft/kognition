// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/memory/inmemory"
	"github.com/kognition-ai/kognition/internal/memory/service"
)

func TestMemoryServiceProvisioner_ManagesBackendThroughHTTPContract(t *testing.T) {
	backend := inmemory.New()
	handler := service.NewHandler(backend, zaptest.NewLogger(t))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	provisioner := NewMemoryServiceProvisioner(server.URL+"/", server.Client())
	request := memory.ProvisionRequest{
		BackendHandle:      "pgvector-test-volume",
		EmbeddingDimension: 1536,
		Parameters:         map[string]string{"index": "hnsw"},
	}
	if err := provisioner.Provision(context.Background(), request); err != nil {
		t.Fatalf("Provision() error = %v", err)
	}
	if _, err := backend.Store(context.Background(), memory.StoreRequest{BackendHandle: request.BackendHandle, ID: "record-1", Content: "hello"}); err != nil {
		t.Fatalf("store through provisioned backend: %v", err)
	}
	if err := provisioner.Provision(context.Background(), request); err != nil {
		t.Fatalf("idempotent Provision() error = %v", err)
	}
	if err := provisioner.Deprovision(context.Background(), memory.DeprovisionRequest{BackendHandle: request.BackendHandle}); err != nil {
		t.Fatalf("Deprovision() error = %v", err)
	}
	if _, err := backend.Store(context.Background(), memory.StoreRequest{BackendHandle: request.BackendHandle, ID: "record-2", Content: "gone"}); err == nil {
		t.Fatal("store after Deprovision() error = nil, want unprovisioned volume error")
	}
}
