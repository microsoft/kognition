// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inmemory_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/conformance"
	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/memory/inmemory"
)

func TestInMemoryBackend_Conformance(t *testing.T) {
	factory := func(t *testing.T) (memory.Backend, string) {
		t.Helper()
		adapter := inmemory.New()
		handle := "test-volume-" + t.Name()
		if err := adapter.Provision(context.Background(), memory.ProvisionRequest{
			BackendHandle:      handle,
			EmbeddingDimension: 3,
		}); err != nil {
			t.Fatalf("Provision: %v", err)
		}
		return adapter, handle
	}

	conformance.RunMemoryBackendSuite(t, factory)
}

func TestInMemoryBackend_ReadResultsAreIsolated(t *testing.T) {
	ctx := context.Background()
	adapter := inmemory.New()
	handle := "isolated-read"
	if err := adapter.Provision(ctx, memory.ProvisionRequest{BackendHandle: handle}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := adapter.Store(ctx, memory.StoreRequest{
		BackendHandle: handle,
		ID:            "record-1",
		Embedding:     []float32{1, 0},
		Metadata:      map[string]string{"tenant": "original"},
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	got, err := adapter.Get(ctx, memory.GetRequest{BackendHandle: handle, ID: "record-1"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Embedding[0] = 0
	got.Metadata["tenant"] = "mutated"

	stored, err := adapter.Get(ctx, memory.GetRequest{BackendHandle: handle, ID: "record-1"})
	if err != nil {
		t.Fatalf("Get after caller mutation: %v", err)
	}
	if stored.Embedding[0] != 1 || stored.Metadata["tenant"] != "original" {
		t.Fatalf("stored record was mutated through Get result: %#v", stored)
	}

	listed, err := adapter.List(ctx, memory.ListRequest{BackendHandle: handle})
	if err != nil || len(listed) != 1 {
		t.Fatalf("List = (%d records, %v), want one record", len(listed), err)
	}
	listed[0].Embedding[0] = 0
	listed[0].Metadata["tenant"] = "mutated-by-list"

	searched, err := adapter.Search(ctx, memory.SearchRequest{BackendHandle: handle, Vector: []float32{1, 0}})
	if err != nil || len(searched) != 1 {
		t.Fatalf("Search = (%d results, %v), want one result", len(searched), err)
	}
	if searched[0].Record.Embedding[0] != 1 || searched[0].Record.Metadata["tenant"] != "original" {
		t.Fatalf("stored record was mutated through List result: %#v", searched[0].Record)
	}
	searched[0].Record.Embedding[0] = 0
	searched[0].Record.Metadata["tenant"] = "mutated-by-search"

	stored, err = adapter.Get(ctx, memory.GetRequest{BackendHandle: handle, ID: "record-1"})
	if err != nil {
		t.Fatalf("Get after Search result mutation: %v", err)
	}
	if stored.Embedding[0] != 1 || stored.Metadata["tenant"] != "original" {
		t.Fatalf("stored record was mutated through Search result: %#v", stored)
	}
}

func TestInMemoryBackend_LifecycleAndUnknownVolumeErrors(t *testing.T) {
	ctx := context.Background()
	adapter := inmemory.New()
	if err := adapter.Provision(ctx, memory.ProvisionRequest{BackendHandle: "volume"}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Provision(ctx, memory.ProvisionRequest{BackendHandle: "volume"}); err != nil {
		t.Fatalf("idempotent Provision() error = %v", err)
	}

	if _, err := adapter.Store(ctx, memory.StoreRequest{BackendHandle: "missing", ID: "record"}); err == nil || !strings.Contains(err.Error(), "not provisioned") {
		t.Fatalf("Store() error = %v, want unknown volume", err)
	}
	if _, err := adapter.Get(ctx, memory.GetRequest{BackendHandle: "missing", ID: "record"}); err == nil || !strings.Contains(err.Error(), "not provisioned") {
		t.Fatalf("Get() error = %v, want unknown volume", err)
	}
	if _, err := adapter.List(ctx, memory.ListRequest{BackendHandle: "missing"}); err == nil || !strings.Contains(err.Error(), "not provisioned") {
		t.Fatalf("List() error = %v, want unknown volume", err)
	}
	if _, err := adapter.Search(ctx, memory.SearchRequest{BackendHandle: "missing", Vector: []float32{1}}); err == nil || !strings.Contains(err.Error(), "not provisioned") {
		t.Fatalf("Search() error = %v, want unknown volume", err)
	}
	if err := adapter.Delete(ctx, memory.DeleteRequest{BackendHandle: "missing", ID: "record"}); err == nil || !strings.Contains(err.Error(), "not provisioned") {
		t.Fatalf("Delete() error = %v, want unknown volume", err)
	}
	if err := adapter.Delete(ctx, memory.DeleteRequest{BackendHandle: "volume", ID: "missing"}); !errors.Is(err, memory.ErrRecordNotFound) {
		t.Fatalf("Delete() error = %v, want ErrRecordNotFound", err)
	}
}

func TestInMemoryBackend_ListAndSearchFilters(t *testing.T) {
	ctx := context.Background()
	adapter := inmemory.New()
	if err := adapter.Provision(ctx, memory.ProvisionRequest{BackendHandle: "volume"}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	records := []memory.StoreRequest{
		{BackendHandle: "volume", ID: "old", Embedding: []float32{1, 0}, Metadata: map[string]string{"scope": "shared"}, Timestamp: base},
		{BackendHandle: "volume", ID: "best", Embedding: []float32{1, 0}, Metadata: map[string]string{"scope": "shared"}, Timestamp: base.Add(time.Hour)},
		{BackendHandle: "volume", ID: "zero", Embedding: []float32{0, 0}, Metadata: map[string]string{"scope": "private"}, Timestamp: base.Add(2 * time.Hour)},
		{BackendHandle: "volume", ID: "text-only", Metadata: map[string]string{"scope": "shared"}, Timestamp: base.Add(3 * time.Hour)},
	}
	for _, record := range records {
		if _, err := adapter.Store(ctx, record); err != nil {
			t.Fatal(err)
		}
	}

	start, end := base.Add(30*time.Minute), base.Add(150*time.Minute)
	listed, err := adapter.List(ctx, memory.ListRequest{BackendHandle: "volume", StartTime: &start, EndTime: &end, Metadata: map[string]string{"scope": "shared"}})
	if err != nil || len(listed) != 1 || listed[0].ID != "best" {
		t.Fatalf("filtered List() = %+v, %v", listed, err)
	}
	if _, err := adapter.Search(ctx, memory.SearchRequest{BackendHandle: "volume"}); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("empty Search() error = %v", err)
	}

	results, err := adapter.Search(ctx, memory.SearchRequest{
		BackendHandle: "volume", Vector: []float32{1, 0}, Metadata: map[string]string{"scope": "shared"}, MinScore: 0.5,
	})
	if err != nil || len(results) != 2 {
		t.Fatalf("filtered Search() = %+v, %v", results, err)
	}
	zeroResults, err := adapter.Search(ctx, memory.SearchRequest{BackendHandle: "volume", Vector: []float32{0, 0}, TopK: 1})
	if err != nil || len(zeroResults) != 1 || zeroResults[0].Score != 0 {
		t.Fatalf("zero-vector Search() = %+v, %v", zeroResults, err)
	}
	mismatch, err := adapter.Search(ctx, memory.SearchRequest{BackendHandle: "volume", Vector: []float32{1, 0, 0}, TopK: 1})
	if err != nil || len(mismatch) != 1 || mismatch[0].Score != 0 {
		t.Fatalf("dimension-mismatch Search() = %+v, %v", mismatch, err)
	}
}
