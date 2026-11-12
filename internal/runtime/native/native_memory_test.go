// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package native

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/kognition-ai/kognition/internal/memory"
)

type recordingMemoryBackend struct {
	searchRequest memory.SearchRequest
	searchResults []memory.SearchResult
	searchErr     error
	storeRequest  memory.StoreRequest
	storeErr      error
}

func (*recordingMemoryBackend) Provision(context.Context, memory.ProvisionRequest) error {
	return nil
}

func (*recordingMemoryBackend) Deprovision(context.Context, memory.DeprovisionRequest) error {
	return nil
}

func (b *recordingMemoryBackend) Store(_ context.Context, request memory.StoreRequest) (memory.StoreResponse, error) {
	b.storeRequest = request
	return memory.StoreResponse{ID: request.ID}, b.storeErr
}

func (*recordingMemoryBackend) Get(context.Context, memory.GetRequest) (memory.Record, error) {
	return memory.Record{}, nil
}

func (*recordingMemoryBackend) List(context.Context, memory.ListRequest) ([]memory.Record, error) {
	return nil, nil
}

func (b *recordingMemoryBackend) Search(_ context.Context, request memory.SearchRequest) ([]memory.SearchResult, error) {
	b.searchRequest = request
	return b.searchResults, b.searchErr
}

func (*recordingMemoryBackend) Delete(context.Context, memory.DeleteRequest) error {
	return nil
}

func (*recordingMemoryBackend) Health(context.Context) error {
	return nil
}

func TestNativeRuntimeMemoryOperationsWithoutBackendAreNoOps(t *testing.T) {
	runtime := New(Config{})

	results, err := runtime.ReadMemory(context.Background(), []float32{0.1, 0.2}, 3)
	if err != nil {
		t.Fatalf("ReadMemory() error = %v", err)
	}
	if results != nil {
		t.Fatalf("ReadMemory() results = %#v, want nil", results)
	}
	if err := runtime.WriteMemory(context.Background(), "record-1", "content", nil, nil); err != nil {
		t.Fatalf("WriteMemory() error = %v", err)
	}
}

func TestNativeRuntimeMemoryOperationsForwardRequests(t *testing.T) {
	wantResults := []memory.SearchResult{{Record: memory.Record{ID: "record-1"}, Score: 0.9}}
	backend := &recordingMemoryBackend{searchResults: wantResults}
	runtime := New(Config{MemoryBackend: backend, MemoryHandle: "tenant-volume"})
	vector := []float32{0.1, 0.2, 0.3}

	results, err := runtime.ReadMemory(context.Background(), vector, 7)
	if err != nil {
		t.Fatalf("ReadMemory() error = %v", err)
	}
	if !reflect.DeepEqual(results, wantResults) {
		t.Fatalf("ReadMemory() results = %#v, want %#v", results, wantResults)
	}
	if backend.searchRequest.BackendHandle != "tenant-volume" || backend.searchRequest.TopK != 7 ||
		!reflect.DeepEqual(backend.searchRequest.Vector, vector) {
		t.Fatalf("Search() request = %#v", backend.searchRequest)
	}

	metadata := map[string]string{"scope": "session"}
	if err := runtime.WriteMemory(context.Background(), "record-1", "durable content", vector, metadata); err != nil {
		t.Fatalf("WriteMemory() error = %v", err)
	}
	if backend.storeRequest.BackendHandle != "tenant-volume" || backend.storeRequest.ID != "record-1" ||
		backend.storeRequest.Content != "durable content" || !reflect.DeepEqual(backend.storeRequest.Embedding, vector) ||
		!reflect.DeepEqual(backend.storeRequest.Metadata, metadata) || backend.storeRequest.Timestamp.IsZero() {
		t.Fatalf("Store() request = %#v", backend.storeRequest)
	}
}

func TestNativeRuntimeMemoryOperationsReturnBackendErrors(t *testing.T) {
	searchErr := errors.New("search failed")
	storeErr := errors.New("store failed")
	backend := &recordingMemoryBackend{searchErr: searchErr, storeErr: storeErr}
	runtime := New(Config{MemoryBackend: backend, MemoryHandle: "volume"})

	if _, err := runtime.ReadMemory(context.Background(), []float32{1}, 1); !errors.Is(err, searchErr) {
		t.Fatalf("ReadMemory() error = %v, want %v", err, searchErr)
	}
	if err := runtime.WriteMemory(context.Background(), "record", "content", nil, nil); !errors.Is(err, storeErr) {
		t.Fatalf("WriteMemory() error = %v, want %v", err, storeErr)
	}
}
