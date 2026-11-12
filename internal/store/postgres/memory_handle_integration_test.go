// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/memory/pgvector"
)

func TestPgvectorPerVolumeDimensions(t *testing.T) {
	databaseURL := os.Getenv("KOGNITION_TEST_MEMORY_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("requires an explicitly supplied disposable migration-check pgvector database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal("invalid memory verification database configuration")
	}
	t.Cleanup(pool.Close)
	var databaseName string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); err != nil ||
		!strings.HasPrefix(databaseName, "migration_check_") {
		t.Fatal("memory verification requires an isolated migration-check database")
	}
	backend := pgvector.New(pool, zaptest.NewLogger(t))
	handle := "dimensions-" + uuid.NewString()
	legacy := strings.ToUpper(handle)
	recordID := uuid.NewString()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		for _, volume := range []string{handle, legacy} {
			if err := backend.Deprovision(cleanup, memory.DeprovisionRequest{BackendHandle: volume}); err != nil {
				t.Errorf("deprovision owned verification volume: %v", err)
			}
		}
	})
	for _, entry := range []struct {
		handle    string
		dimension int
	}{{handle, 384}, {legacy, 1536}} {
		for range 2 {
			if err := backend.Provision(ctx, memory.ProvisionRequest{
				BackendHandle: entry.handle, EmbeddingDimension: entry.dimension,
			}); err != nil {
				t.Fatalf("idempotent provisioning of %d dimensions: %v", entry.dimension, err)
			}
		}
		vector := make([]float32, entry.dimension)
		vector[0] = 1
		if _, err := backend.Store(ctx, memory.StoreRequest{
			BackendHandle: entry.handle, ID: recordID, Content: entry.handle,
			Embedding: vector, Metadata: map[string]string{"proof": "dimensions"},
		}); err != nil {
			t.Fatalf("store %d-dimensional record: %v", entry.dimension, err)
		}
		record, err := backend.Get(ctx, memory.GetRequest{BackendHandle: entry.handle, ID: recordID})
		if err != nil || record.Content != entry.handle || len(record.Embedding) != entry.dimension {
			t.Fatalf("dimension-preserving read: record=%+v error=%v", record, err)
		}
		results, err := backend.Search(ctx, memory.SearchRequest{
			BackendHandle: entry.handle, Vector: vector, TopK: 5, MinScore: 0.9,
			Metadata: map[string]string{"proof": "dimensions"},
		})
		if err != nil || len(results) != 1 || results[0].Record.Content != entry.handle ||
			results[0].Score < 0.999 {
			t.Fatalf("dimension-isolated semantic search: results=%+v error=%v", results, err)
		}
	}
	if err := backend.Provision(ctx, memory.ProvisionRequest{
		BackendHandle: handle, EmbeddingDimension: 1536,
	}); err == nil {
		t.Fatal("reprovisioning must not silently change a volume's dimensions")
	}
	wrong := make([]float32, 1536)
	wrong[0] = 1
	if _, err := backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handle, ID: recordID, Content: "must not overwrite", Embedding: wrong,
	}); err == nil {
		t.Fatal("storing incompatible dimensions must fail")
	}
	if _, err := backend.Search(ctx, memory.SearchRequest{BackendHandle: handle, Vector: wrong}); err == nil {
		t.Fatal("searching incompatible dimensions must fail")
	}
	retained, err := backend.Get(ctx, memory.GetRequest{BackendHandle: handle, ID: recordID})
	if err != nil || retained.Content != handle || len(retained.Embedding) != 384 {
		t.Fatalf("rejected mutations changed retained memory: %v", err)
	}
	if err := backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: handle}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Get(ctx, memory.GetRequest{BackendHandle: handle, ID: recordID}); !errors.Is(err, memory.ErrRecordNotFound) {
		t.Fatalf("deprovision must remove stored records: %v", err)
	}
	if err := backend.Provision(ctx, memory.ProvisionRequest{BackendHandle: handle}); err != nil {
		t.Fatalf("deprovisioned handle can be recreated with the default dimension: %v", err)
	}
	if _, err := backend.Search(ctx, memory.SearchRequest{BackendHandle: handle, Vector: wrong}); err != nil {
		t.Fatalf("empty default-dimension volume must remain searchable: %v", err)
	}
	wrong384 := make([]float32, 384)
	wrong384[0] = 1
	if _, err := backend.Search(ctx, memory.SearchRequest{BackendHandle: handle, Vector: wrong384}); err == nil {
		t.Fatal("wrong dimensions must be rejected even when the volume is empty")
	}
	for _, dimension := range []int{-1, 16001} {
		if err := backend.Provision(ctx, memory.ProvisionRequest{
			BackendHandle: handle, EmbeddingDimension: dimension,
		}); err == nil {
			t.Fatalf("invalid dimension %d must be rejected", dimension)
		}
	}
	if err := backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: handle}); err != nil {
		t.Fatal(err)
	}
	type provisionResult struct {
		dimension int
		err       error
	}
	provisioned := make(chan provisionResult, 2)
	for _, dimension := range []int{384, 1536} {
		go func() {
			err := backend.Provision(ctx, memory.ProvisionRequest{
				BackendHandle: handle, EmbeddingDimension: dimension,
			})
			provisioned <- provisionResult{dimension: dimension, err: err}
		}()
	}
	winner, successes := 0, 0
	for range 2 {
		result := <-provisioned
		if result.err == nil {
			winner = result.dimension
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("incompatible concurrent provisioning must have exactly one winner, got %d", successes)
	}
	vector := make([]float32, winner)
	vector[0] = 1
	if _, err := backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handle, ID: recordID, Content: "concurrent winner", Embedding: vector,
	}); err != nil {
		t.Fatalf("winning dimension must remain usable: %v", err)
	}
}

func TestPgvectorOpaqueVolumeHandleIsolation(t *testing.T) {
	databaseURL := os.Getenv("KOGNITION_TEST_MEMORY_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("requires an explicitly supplied disposable migration-check pgvector database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal("invalid memory verification database configuration")
	}
	t.Cleanup(pool.Close)
	var databaseName string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); err != nil ||
		!strings.HasPrefix(databaseName, "migration_check_") {
		t.Fatal("memory verification requires an isolated migration-check database")
	}
	backend := pgvector.New(pool, zaptest.NewLogger(t))
	legacy := uuid.NewString()
	opaque := "pgvector-" + legacy
	sibling := strings.ToUpper(opaque)
	recordID := uuid.NewString()
	handles := []string{legacy, opaque, sibling}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		for _, handle := range handles {
			if err := backend.Deprovision(cleanup, memory.DeprovisionRequest{BackendHandle: handle}); err != nil {
				t.Errorf("deprovision owned verification volume: %v", err)
			}
		}
	})
	vector := make([]float32, 1536)
	vector[0] = 1
	timestamp := time.Now().UTC().Truncate(time.Microsecond)
	for _, handle := range handles {
		if err := backend.Provision(ctx, memory.ProvisionRequest{BackendHandle: handle}); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Store(ctx, memory.StoreRequest{
			BackendHandle: handle, ID: recordID, Content: handle, Embedding: vector,
			Metadata: map[string]string{"proof": "opaque-handle"}, Timestamp: timestamp,
		}); err != nil {
			t.Fatalf("store distinct backend handle %q: %v", handle, err)
		}
	}
	for _, handle := range handles {
		record, err := backend.Get(ctx, memory.GetRequest{BackendHandle: handle, ID: recordID})
		if err != nil || record.Content != handle || !record.Timestamp.Equal(timestamp) ||
			record.Metadata["proof"] != "opaque-handle" {
			t.Fatalf("isolated handle %q returned wrong record: %v", handle, err)
		}
	}
	listed, err := backend.List(ctx, memory.ListRequest{
		BackendHandle: opaque, Metadata: map[string]string{"proof": "opaque-handle"}, Limit: 5,
	})
	if err != nil || len(listed) != 1 || listed[0].Content != opaque {
		t.Fatalf("list crossed volume boundary: count=%d error=%v", len(listed), err)
	}
	hits, err := backend.Search(ctx, memory.SearchRequest{
		BackendHandle: opaque, Vector: vector, TopK: 5, Metadata: map[string]string{"proof": "opaque-handle"},
	})
	if err != nil || len(hits) != 1 || hits[0].Record.Content != opaque {
		t.Fatalf("semantic search crossed volume boundary: count=%d error=%v", len(hits), err)
	}
	if _, err := backend.Store(ctx, memory.StoreRequest{
		BackendHandle: opaque, ID: recordID, Content: "updated opaque volume",
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := backend.Get(ctx, memory.GetRequest{BackendHandle: opaque, ID: recordID})
	if err != nil || updated.Content != "updated opaque volume" {
		t.Fatalf("opaque-volume upsert failed: %v", err)
	}
	if err := backend.Delete(ctx, memory.DeleteRequest{BackendHandle: opaque, ID: recordID}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Get(ctx, memory.GetRequest{BackendHandle: opaque, ID: recordID}); !errors.Is(err, memory.ErrRecordNotFound) {
		t.Fatalf("deleted opaque-volume record remains: %v", err)
	}
	if err := backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: sibling}); err != nil {
		t.Fatal(err)
	}
	for _, handle := range []string{opaque, sibling} {
		records, err := backend.List(ctx, memory.ListRequest{BackendHandle: handle})
		if err != nil || len(records) != 0 {
			t.Fatalf("removed volume retained records: %v", err)
		}
	}
	retained, err := backend.Get(ctx, memory.GetRequest{BackendHandle: legacy, ID: recordID})
	if err != nil || retained.Content != legacy {
		t.Fatalf("delete/deprovision changed the legacy UUID-backed record: %v", err)
	}
}
