// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conformance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/memory"
)

// MemoryBackendFactory creates a fresh, provisioned memory backend for each test.
type MemoryBackendFactory func(t *testing.T) (memory.Backend, string)

// RunMemoryBackendSuite executes the conformance suite for a memory backend.
// The factory must return a ready backend and a provisioned backendHandle.
func RunMemoryBackendSuite(t *testing.T, factory MemoryBackendFactory) {
	t.Helper()

	t.Run("StoreAndGet", func(t *testing.T) {
		runMemoryStoreAndGet(t, factory)
	})
	t.Run("List", func(t *testing.T) {
		runMemoryList(t, factory)
	})
	t.Run("Search", func(t *testing.T) {
		runMemorySearch(t, factory)
	})
	t.Run("Delete", func(t *testing.T) {
		runMemoryDelete(t, factory)
	})
	t.Run("Provision", func(t *testing.T) {
		runMemoryProvision(t, factory)
	})
	t.Run("Health", func(t *testing.T) {
		runMemoryHealth(t, factory)
	})
}

// --- Store and Get ---

func runMemoryStoreAndGet(t *testing.T, factory MemoryBackendFactory) {
	t.Helper()
	backend, handle := factory(t)
	ctx := context.Background()

	t.Run("Store_ReturnsID", func(t *testing.T) {
		resp, err := backend.Store(ctx, memory.StoreRequest{
			BackendHandle: handle,
			ID:            "rec-001",
			Content:       "hello world",
			Embedding:     []float32{0.1, 0.2, 0.3},
			Metadata:      map[string]string{"type": "test"},
			Timestamp:     time.Now(),
		})
		if err != nil {
			t.Fatalf("Store: %v", err)
		}
		if resp.ID == "" {
			t.Fatal("expected non-empty ID in store response")
		}
	})

	t.Run("Get_ReturnsStoredRecord", func(t *testing.T) {
		now := time.Now().Truncate(time.Millisecond)
		_, err := backend.Store(ctx, memory.StoreRequest{
			BackendHandle: handle,
			ID:            "rec-002",
			Content:       "stored content",
			Embedding:     []float32{0.4, 0.5, 0.6},
			Metadata:      map[string]string{"key": "val"},
			Timestamp:     now,
		})
		if err != nil {
			t.Fatalf("Store: %v", err)
		}

		rec, err := backend.Get(ctx, memory.GetRequest{
			BackendHandle: handle,
			ID:            "rec-002",
		})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if rec.Content != "stored content" {
			t.Errorf("expected content %q, got %q", "stored content", rec.Content)
		}
		if rec.Metadata["key"] != "val" {
			t.Errorf("expected metadata key=val, got %v", rec.Metadata)
		}
	})

	t.Run("Get_NotFound_ReturnsError", func(t *testing.T) {
		_, err := backend.Get(ctx, memory.GetRequest{
			BackendHandle: handle,
			ID:            "nonexistent-id",
		})
		if err == nil {
			t.Fatal("expected error for nonexistent record")
		}
	})
}

// --- List ---

func runMemoryList(t *testing.T, factory MemoryBackendFactory) {
	t.Helper()
	backend, handle := factory(t)
	ctx := context.Background()

	// Seed data
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		_, err := backend.Store(ctx, memory.StoreRequest{
			BackendHandle: handle,
			ID:            fmt.Sprintf("list-%03d", i),
			Content:       fmt.Sprintf("record %d", i),
			Embedding:     []float32{float32(i) * 0.1, 0.0, 0.0},
			Metadata:      map[string]string{"batch": "list-test"},
			Timestamp:     base.Add(time.Duration(i) * time.Hour),
		})
		if err != nil {
			t.Fatalf("Store[%d]: %v", i, err)
		}
	}

	t.Run("List_ByTimeRange", func(t *testing.T) {
		start := base.Add(1 * time.Hour)
		end := base.Add(3 * time.Hour)
		records, err := backend.List(ctx, memory.ListRequest{
			BackendHandle: handle,
			StartTime:     &start,
			EndTime:       &end,
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if err := validateCount("records in time range", len(records), 2, countAtLeast); err != nil {
			t.Error(err)
		}
	})

	t.Run("List_WithLimit", func(t *testing.T) {
		records, err := backend.List(ctx, memory.ListRequest{
			BackendHandle: handle,
			Limit:         2,
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if err := validateCount("records with limit", len(records), 2, countAtMost); err != nil {
			t.Error(err)
		}
	})

	t.Run("List_ByMetadata", func(t *testing.T) {
		records, err := backend.List(ctx, memory.ListRequest{
			BackendHandle: handle,
			Metadata:      map[string]string{"batch": "list-test"},
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if err := validateCount("records with metadata filter", len(records), 5, countExact); err != nil {
			t.Error(err)
		}
	})
}

type countMode int

const (
	countExact countMode = iota
	countAtLeast
	countAtMost
)

func validateCount(subject string, actual, expected int, mode countMode) error {
	switch mode {
	case countAtLeast:
		if actual < expected {
			return fmt.Errorf("expected at least %d %s, got %d", expected, subject, actual)
		}
	case countAtMost:
		if actual > expected {
			return fmt.Errorf("expected at most %d %s, got %d", expected, subject, actual)
		}
	default:
		if actual != expected {
			return fmt.Errorf("expected %d %s, got %d", expected, subject, actual)
		}
	}
	return nil
}

// --- Search ---

func runMemorySearch(t *testing.T, factory MemoryBackendFactory) {
	t.Helper()
	backend, handle := factory(t)
	ctx := context.Background()

	// Store records with known embeddings for predictable similarity.
	vectors := [][]float32{
		{1.0, 0.0, 0.0},
		{0.0, 1.0, 0.0},
		{0.0, 0.0, 1.0},
		{0.7, 0.7, 0.0},
	}
	for i, v := range vectors {
		_, err := backend.Store(ctx, memory.StoreRequest{
			BackendHandle: handle,
			ID:            fmt.Sprintf("search-%03d", i),
			Content:       fmt.Sprintf("vector record %d", i),
			Embedding:     v,
			Timestamp:     time.Now(),
		})
		if err != nil {
			t.Fatalf("Store[%d]: %v", i, err)
		}
	}

	t.Run("Search_ReturnsTopK", func(t *testing.T) {
		results, err := backend.Search(ctx, memory.SearchRequest{
			BackendHandle: handle,
			Vector:        []float32{1.0, 0.0, 0.0},
			TopK:          2,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("expected at least one search result")
		}
		if err := validateTopKResults(results, 2, "search-000"); err != nil {
			t.Error(err)
		}
	})

	t.Run("Search_ScoresDescending", func(t *testing.T) {
		results, err := backend.Search(ctx, memory.SearchRequest{
			BackendHandle: handle,
			Vector:        []float32{0.5, 0.5, 0.0},
			TopK:          4,
		})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if err := validateDescendingScores(results); err != nil {
			t.Error(err)
		}
	})
}

func validateTopKResults(results []memory.SearchResult, topK int, expectedFirstID string) error {
	if len(results) > topK {
		return fmt.Errorf("expected at most %d results for TopK=%d, got %d", topK, topK, len(results))
	}
	if len(results) > 0 && results[0].Record.ID != expectedFirstID {
		return fmt.Errorf("expected %s as top result, got %s", expectedFirstID, results[0].Record.ID)
	}
	return nil
}

func validateDescendingScores(results []memory.SearchResult) error {
	for index := 1; index < len(results); index++ {
		if results[index].Score > results[index-1].Score {
			return fmt.Errorf("scores not descending: [%d]=%f > [%d]=%f", index, results[index].Score, index-1, results[index-1].Score)
		}
	}
	return nil
}

// --- Delete ---

func runMemoryDelete(t *testing.T, factory MemoryBackendFactory) {
	t.Helper()
	backend, handle := factory(t)
	ctx := context.Background()

	t.Run("Delete_RemovesRecord", func(t *testing.T) {
		_, err := backend.Store(ctx, memory.StoreRequest{
			BackendHandle: handle,
			ID:            "del-001",
			Content:       "to be deleted",
			Embedding:     []float32{0.1, 0.1, 0.1},
			Timestamp:     time.Now(),
		})
		if err != nil {
			t.Fatalf("Store: %v", err)
		}

		err = backend.Delete(ctx, memory.DeleteRequest{
			BackendHandle: handle,
			ID:            "del-001",
		})
		if err != nil {
			t.Fatalf("Delete: %v", err)
		}

		_, err = backend.Get(ctx, memory.GetRequest{
			BackendHandle: handle,
			ID:            "del-001",
		})
		if err == nil {
			t.Error("expected error after deleting record")
		}
	})
}

// --- Provision ---

func runMemoryProvision(t *testing.T, factory MemoryBackendFactory) {
	t.Helper()
	backend, _ := factory(t)
	ctx := context.Background()

	t.Run("Provision_NewVolume", func(t *testing.T) {
		err := backend.Provision(ctx, memory.ProvisionRequest{
			BackendHandle:      "conformance-test-volume",
			EmbeddingDimension: 3,
		})
		if err != nil {
			t.Fatalf("Provision: %v", err)
		}

		// Clean up
		_ = backend.Deprovision(ctx, memory.DeprovisionRequest{
			BackendHandle: "conformance-test-volume",
		})
	})

	t.Run("Deprovision_Existing", func(t *testing.T) {
		err := backend.Provision(ctx, memory.ProvisionRequest{
			BackendHandle:      "conformance-deprovision-test",
			EmbeddingDimension: 3,
		})
		if err != nil {
			t.Fatalf("Provision: %v", err)
		}
		err = backend.Deprovision(ctx, memory.DeprovisionRequest{
			BackendHandle: "conformance-deprovision-test",
		})
		if err != nil {
			t.Fatalf("Deprovision: %v", err)
		}
	})
}

// --- Health ---

func runMemoryHealth(t *testing.T, factory MemoryBackendFactory) {
	t.Helper()
	backend, _ := factory(t)
	ctx := context.Background()

	t.Run("Health_ReturnsNoError", func(t *testing.T) {
		err := backend.Health(ctx)
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
	})
}
