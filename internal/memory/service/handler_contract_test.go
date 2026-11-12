// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/memory/service"
)

type contractBackend struct {
	provision   func(context.Context, memory.ProvisionRequest) error
	deprovision func(context.Context, memory.DeprovisionRequest) error
	store       func(context.Context, memory.StoreRequest) (memory.StoreResponse, error)
	get         func(context.Context, memory.GetRequest) (memory.Record, error)
	list        func(context.Context, memory.ListRequest) ([]memory.Record, error)
	search      func(context.Context, memory.SearchRequest) ([]memory.SearchResult, error)
	delete      func(context.Context, memory.DeleteRequest) error
	health      func(context.Context) error
}

func (b *contractBackend) Provision(ctx context.Context, req memory.ProvisionRequest) error {
	if b.provision != nil {
		return b.provision(ctx, req)
	}
	return nil
}

func (b *contractBackend) Deprovision(ctx context.Context, req memory.DeprovisionRequest) error {
	if b.deprovision != nil {
		return b.deprovision(ctx, req)
	}
	return nil
}

func (b *contractBackend) Store(ctx context.Context, req memory.StoreRequest) (memory.StoreResponse, error) {
	if b.store != nil {
		return b.store(ctx, req)
	}
	return memory.StoreResponse{ID: req.ID}, nil
}

func (b *contractBackend) Get(ctx context.Context, req memory.GetRequest) (memory.Record, error) {
	if b.get != nil {
		return b.get(ctx, req)
	}
	return memory.Record{}, nil
}

func (b *contractBackend) List(ctx context.Context, req memory.ListRequest) ([]memory.Record, error) {
	if b.list != nil {
		return b.list(ctx, req)
	}
	return nil, nil
}

func (b *contractBackend) Search(ctx context.Context, req memory.SearchRequest) ([]memory.SearchResult, error) {
	if b.search != nil {
		return b.search(ctx, req)
	}
	return nil, nil
}

func (b *contractBackend) Delete(ctx context.Context, req memory.DeleteRequest) error {
	if b.delete != nil {
		return b.delete(ctx, req)
	}
	return nil
}

func (b *contractBackend) Health(ctx context.Context) error {
	if b.health != nil {
		return b.health(ctx)
	}
	return nil
}

func newContractHandler(t *testing.T, backend memory.Backend) http.Handler {
	t.Helper()
	handler := service.NewHandler(backend, zaptest.NewLogger(t))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return mux
}

func performJSONRequest(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertErrorResponse(t *testing.T, response *httptest.ResponseRecorder, status int, message string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, status, response.Body.String())
	}
	var body service.ErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Error != message {
		t.Fatalf("error = %q, want %q", body.Error, message)
	}
}

func TestMemoryLifecycleRequestContracts(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		body       string
		wantStatus int
		wantError  string
	}{
		{name: "provision malformed", path: "/v1/memory/provision", body: "{", wantStatus: http.StatusBadRequest, wantError: "invalid request body"},
		{name: "provision missing handle", path: "/v1/memory/provision", body: `{}`, wantStatus: http.StatusBadRequest, wantError: "volume_handle is required"},
		{name: "deprovision malformed", path: "/v1/memory/deprovision", body: "{", wantStatus: http.StatusBadRequest, wantError: "invalid request body"},
		{name: "deprovision missing handle", path: "/v1/memory/deprovision", body: `{}`, wantStatus: http.StatusBadRequest, wantError: "volume_handle is required"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backendCalled := false
			backend := &contractBackend{
				provision:   func(context.Context, memory.ProvisionRequest) error { backendCalled = true; return nil },
				deprovision: func(context.Context, memory.DeprovisionRequest) error { backendCalled = true; return nil },
			}
			request := httptest.NewRequest(http.MethodPost, test.path, bytes.NewBufferString(test.body))
			response := httptest.NewRecorder()
			newContractHandler(t, backend).ServeHTTP(response, request)
			assertErrorResponse(t, response, test.wantStatus, test.wantError)
			if backendCalled {
				t.Fatal("backend called for invalid request")
			}
		})
	}
}

func TestMemoryLifecycleForwardsRequests(t *testing.T) {
	var provisionRequest memory.ProvisionRequest
	var deprovisionRequest memory.DeprovisionRequest
	backend := &contractBackend{
		provision: func(_ context.Context, req memory.ProvisionRequest) error {
			provisionRequest = req
			return nil
		},
		deprovision: func(_ context.Context, req memory.DeprovisionRequest) error {
			deprovisionRequest = req
			return nil
		},
	}
	handler := newContractHandler(t, backend)

	response := performJSONRequest(t, handler, http.MethodPost, "/v1/memory/provision", service.ProvisionRequest{
		VolumeHandle: "tenant-a-volume", EmbeddingDimension: 1536, Parameters: map[string]string{"region": "east"},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("provision status = %d, want %d", response.Code, http.StatusCreated)
	}
	if provisionRequest.BackendHandle != "tenant-a-volume" || provisionRequest.EmbeddingDimension != 1536 || provisionRequest.Parameters["region"] != "east" {
		t.Fatalf("provision request = %#v", provisionRequest)
	}

	response = performJSONRequest(t, handler, http.MethodPost, "/v1/memory/deprovision", service.DeprovisionRequest{VolumeHandle: "tenant-a-volume"})
	if response.Code != http.StatusNoContent {
		t.Fatalf("deprovision status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if deprovisionRequest.BackendHandle != "tenant-a-volume" {
		t.Fatalf("deprovision request = %#v", deprovisionRequest)
	}
}

func TestMemoryLifecycleBackendFailures(t *testing.T) {
	backendError := errors.New("backend unavailable")
	backend := &contractBackend{
		provision:   func(context.Context, memory.ProvisionRequest) error { return backendError },
		deprovision: func(context.Context, memory.DeprovisionRequest) error { return backendError },
	}
	handler := newContractHandler(t, backend)

	response := performJSONRequest(t, handler, http.MethodPost, "/v1/memory/provision", service.ProvisionRequest{VolumeHandle: "volume-a"})
	assertErrorResponse(t, response, http.StatusInternalServerError, "provision operation failed")

	response = performJSONRequest(t, handler, http.MethodPost, "/v1/memory/deprovision", service.DeprovisionRequest{VolumeHandle: "volume-a"})
	assertErrorResponse(t, response, http.StatusInternalServerError, "deprovision operation failed")
}

func TestMemoryServiceInternalTokenProtectsStatefulOperations(t *testing.T) {
	backendCalled := false
	handler := service.NewHandler(&contractBackend{store: func(context.Context, memory.StoreRequest) (memory.StoreResponse, error) {
		backendCalled = true
		return memory.StoreResponse{ID: "record-1"}, nil
	}}, zaptest.NewLogger(t), service.WithInternalToken("internal-token"))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	request := httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"tenant-a","content":"secret"}`))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	assertErrorResponse(t, response, http.StatusUnauthorized, "memory service authorization is required")
	if backendCalled {
		t.Fatal("backend called without internal token")
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"tenant-a","content":"secret"}`))
	request.Header.Set("Authorization", "Bearer internal-token")
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !backendCalled {
		t.Fatalf("authorized stateful request status = %d, backendCalled = %v", response.Code, backendCalled)
	}

	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/memory/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestMemoryListForwardsFiltersAndMapsFailures(t *testing.T) {
	start := time.Date(2026, time.March, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	var listRequest memory.ListRequest
	backend := &contractBackend{list: func(_ context.Context, req memory.ListRequest) ([]memory.Record, error) {
		listRequest = req
		return []memory.Record{{ID: "record-a", Timestamp: end}}, nil
	}}
	handler := newContractHandler(t, backend)
	response := performJSONRequest(t, handler, http.MethodPost, "/v1/memory/list", service.ListMemoryRequest{
		VolumeHandle: "tenant-a-volume", StartTime: &start, EndTime: &end, Metadata: map[string]string{"kind": "fact"}, Limit: 5,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d", response.Code, http.StatusOK)
	}
	if listRequest.BackendHandle != "tenant-a-volume" || listRequest.StartTime == nil || listRequest.EndTime == nil || listRequest.Metadata["kind"] != "fact" || listRequest.Limit != 5 {
		t.Fatalf("list request = %#v", listRequest)
	}

	backend.list = func(context.Context, memory.ListRequest) ([]memory.Record, error) {
		return nil, errors.New("list failed")
	}
	response = performJSONRequest(t, handler, http.MethodPost, "/v1/memory/list", service.ListMemoryRequest{VolumeHandle: "tenant-a-volume"})
	assertErrorResponse(t, response, http.StatusInternalServerError, "list operation failed")

	response = performJSONRequest(t, handler, http.MethodPost, "/v1/memory/list", map[string]string{})
	assertErrorResponse(t, response, http.StatusBadRequest, "volume_handle is required")

	request := httptest.NewRequest(http.MethodPost, "/v1/memory/list", bytes.NewBufferString("{"))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertErrorResponse(t, response, http.StatusBadRequest, "invalid request body")
}

func TestMemoryDeleteAndHealthMapBackendFailures(t *testing.T) {
	backendError := errors.New("backend unavailable")
	backend := &contractBackend{
		delete: func(context.Context, memory.DeleteRequest) error { return backendError },
		health: func(context.Context) error { return backendError },
	}
	handler := newContractHandler(t, backend)

	response := performJSONRequest(t, handler, http.MethodDelete, "/v1/memory/delete/tenant-a-volume/record-a", nil)
	assertErrorResponse(t, response, http.StatusInternalServerError, "delete operation failed")

	response = performJSONRequest(t, handler, http.MethodGet, "/v1/memory/health", nil)
	assertErrorResponse(t, response, http.StatusServiceUnavailable, backendError.Error())
}

func TestMemoryGetMapsBackendFailure(t *testing.T) {
	backend := &contractBackend{get: func(context.Context, memory.GetRequest) (memory.Record, error) {
		return memory.Record{}, errors.New("get failed")
	}}
	response := performJSONRequest(t, newContractHandler(t, backend), http.MethodGet, "/v1/memory/get/tenant-a-volume/record-a", nil)
	assertErrorResponse(t, response, http.StatusInternalServerError, "get operation failed")
}

func TestMemoryStoreRejectsInvalidRequestsAndMapsBackendFailure(t *testing.T) {
	backendCalled := false
	backend := &contractBackend{store: func(context.Context, memory.StoreRequest) (memory.StoreResponse, error) {
		backendCalled = true
		return memory.StoreResponse{}, errors.New("store failed")
	}}
	handler := newContractHandler(t, backend)

	request := httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString("{"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertErrorResponse(t, response, http.StatusBadRequest, "invalid request body")
	if backendCalled {
		t.Fatal("backend called for malformed store request")
	}

	response = performJSONRequest(t, handler, http.MethodPost, "/v1/memory/store", service.StoreMemoryRequest{})
	assertErrorResponse(t, response, http.StatusBadRequest, "volume_handle is required")
	if backendCalled {
		t.Fatal("backend called for store request without volume handle")
	}

	response = performJSONRequest(t, handler, http.MethodPost, "/v1/memory/store", service.StoreMemoryRequest{VolumeHandle: "tenant-a-volume"})
	assertErrorResponse(t, response, http.StatusInternalServerError, "store operation failed")
	if !backendCalled {
		t.Fatal("backend not called for valid store request")
	}
}
