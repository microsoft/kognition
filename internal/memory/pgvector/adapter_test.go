// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package pgvector_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/memory"
	adapter "github.com/kognition-ai/kognition/internal/memory/pgvector"
	storepostgres "github.com/kognition-ai/kognition/internal/store/postgres"
)

var (
	pool    *pgxpool.Pool
	backend *adapter.Adapter
)

func TestMain(m *testing.M) {
	if databaseURL := os.Getenv("KOGNITION_TEST_MEMORY_DATABASE_URL"); databaseURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		var err error
		pool, err = pgxpool.New(ctx, databaseURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "invalid memory verification database configuration")
			os.Exit(1)
		}
		var name string
		if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil ||
			!strings.HasPrefix(name, "migration_check_") {
			pool.Close()
			fmt.Fprintln(os.Stderr, "memory verification requires an isolated migration-check database")
			os.Exit(1)
		}
		if err := storepostgres.Migrate(ctx, pool, ""); err != nil {
			pool.Close()
			fmt.Fprintf(os.Stderr, "memory verification migrations failed: %v\n", err)
			os.Exit(1)
		}
		cancel()
		backend = adapter.New(pool, zap.NewNop())
		code := m.Run()
		pool.Close()
		os.Exit(code)
	}

	runtimePath, err := os.MkdirTemp("", "kognition-pgvector-postgres-")
	if err != nil {
		fmt.Printf("failed to create embedded postgres runtime directory: %v\n", err)
		os.Exit(1)
	}

	postgres := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Port(15434). // different from store (15432) and apiserver (15433) tests
			Version(embeddedpostgres.V16).
			RuntimePath(runtimePath),
	)

	if err := postgres.Start(); err != nil {
		_ = os.RemoveAll(runtimePath)
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}

	dsn := "postgres://postgres:postgres@localhost:15434/postgres?sslmode=disable"
	ctx := context.Background()

	pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		_ = postgres.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Printf("failed to create pool: %v\n", err)
		os.Exit(1)
	}

	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS "vector"`); err != nil {
		pool.Close()
		_ = postgres.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Printf("SKIP: pgvector extension not available in embedded postgres: %v\n", err)
		os.Exit(0) // exit 0 so tests are skipped, not failed
	}
	if err := storepostgres.Migrate(ctx, pool, ""); err != nil {
		pool.Close()
		_ = postgres.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Printf("failed to apply production memory migrations: %v\n", err)
		os.Exit(1)
	}

	logger, _ := zap.NewDevelopment()
	backend = adapter.New(pool, logger)

	code := m.Run()

	pool.Close()
	_ = postgres.Stop()
	_ = os.RemoveAll(runtimePath)
	os.Exit(code)
}

func TestProvisionAndDeprovision(t *testing.T) {
	ctx := context.Background()
	handle := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	if err := backend.Provision(ctx, memory.ProvisionRequest{BackendHandle: handle}); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Store a record then deprovision should delete it
	_, err := backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handle,
		ID:            "11111111-1111-1111-1111-111111111111",
		Content:       "test",
		Embedding:     makeVector(1536, 0.1),
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	if err := backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: handle}); err != nil {
		t.Fatalf("deprovision: %v", err)
	}

	records, err := backend.List(ctx, memory.ListRequest{BackendHandle: handle})
	if err != nil {
		t.Fatalf("list after deprovision: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected 0 records after deprovision, got %d", len(records))
	}
}

// TC-0120: Store and read episodic records
func TestEpisodicStoreAndRetrieve(t *testing.T) {
	ctx := context.Background()
	handle := "22222222-2222-2222-2222-222222222222"

	now := time.Now().UTC().Truncate(time.Microsecond)

	// Store episodic records with timestamps
	for i := 0; i < 5; i++ {
		_, err := backend.Store(ctx, memory.StoreRequest{
			BackendHandle: handle,
			ID:            fmt.Sprintf("33333333-3333-3333-3333-%012d", i),
			Content:       fmt.Sprintf("event %d", i),
			Embedding:     makeVector(1536, float32(i)*0.1),
			Metadata:      map[string]string{"type": "episodic"},
			Timestamp:     now.Add(time.Duration(i) * time.Hour),
		})
		if err != nil {
			t.Fatalf("store record %d: %v", i, err)
		}
	}

	// List all
	records, err := backend.List(ctx, memory.ListRequest{BackendHandle: handle})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 5 {
		t.Fatalf("expected 5 records, got %d", len(records))
	}

	// List by time range (only middle 3)
	start := now.Add(1 * time.Hour)
	end := now.Add(3 * time.Hour)
	records, err = backend.List(ctx, memory.ListRequest{
		BackendHandle: handle,
		StartTime:     &start,
		EndTime:       &end,
	})
	if err != nil {
		t.Fatalf("list by time: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("expected 3 records in time range, got %d", len(records))
	}

	// Get single record
	rec, err := backend.Get(ctx, memory.GetRequest{
		BackendHandle: handle,
		ID:            "33333333-3333-3333-3333-000000000002",
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec.Content != "event 2" {
		t.Fatalf("expected 'event 2', got %q", rec.Content)
	}

	// Delete
	if err := backend.Delete(ctx, memory.DeleteRequest{BackendHandle: handle, ID: "33333333-3333-3333-3333-000000000002"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	records, err = backend.List(ctx, memory.ListRequest{BackendHandle: handle})
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("expected 4 records after delete, got %d", len(records))
	}

	// Cleanup
	_ = backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: handle})
}

// TC-0121: Store semantic records and query by similarity
func TestSemanticSimilaritySearch(t *testing.T) {
	ctx := context.Background()
	handle := "44444444-4444-4444-4444-444444444444"

	// Store records with distinct embeddings
	// Record A: vector pointing in direction 0
	// Record B: vector pointing in direction 1 (similar to A)
	// Record C: vector pointing in opposite direction
	vecA := makeUnitVector(1536, 0)
	vecB := makeUnitVector(1536, 1)
	vecC := makeOppositeVector(1536, 0)

	records := []struct {
		id      string
		content string
		vec     []float32
	}{
		{"55555555-5555-5555-5555-000000000001", "concept A", vecA},
		{"55555555-5555-5555-5555-000000000002", "concept B similar to A", vecB},
		{"55555555-5555-5555-5555-000000000003", "concept C opposite", vecC},
	}

	for _, r := range records {
		_, err := backend.Store(ctx, memory.StoreRequest{
			BackendHandle: handle,
			ID:            r.id,
			Content:       r.content,
			Embedding:     r.vec,
			Metadata:      map[string]string{"type": "semantic"},
		})
		if err != nil {
			t.Fatalf("store %s: %v", r.id, err)
		}
	}

	// Search with vecA — should return A first, then B, then C
	results, err := backend.Search(ctx, memory.SearchRequest{
		BackendHandle: handle,
		Vector:        vecA,
		TopK:          3,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// First result should be record A (exact match, score ~1.0)
	if results[0].Record.ID != "55555555-5555-5555-5555-000000000001" {
		t.Errorf("expected first result to be concept A, got %s", results[0].Record.Content)
	}
	if results[0].Score < 0.99 {
		t.Errorf("expected score ~1.0 for exact match, got %f", results[0].Score)
	}

	// Last result should be record C (opposite, score near -1 or 0)
	if results[2].Record.ID != "55555555-5555-5555-5555-000000000003" {
		t.Errorf("expected last result to be concept C, got %s", results[2].Record.Content)
	}

	// Search with TopK=1
	results, err = backend.Search(ctx, memory.SearchRequest{
		BackendHandle: handle,
		Vector:        vecA,
		TopK:          1,
	})
	if err != nil {
		t.Fatalf("search top-1: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// Cleanup
	_ = backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: handle})
}

func TestHealth(t *testing.T) {
	if err := backend.Health(context.Background()); err != nil {
		t.Fatalf("health check failed: %v", err)
	}
}

// Helper: create a vector filled with a constant value.
func makeVector(dim int, val float32) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = val
	}
	return v
}

// Helper: create a unit vector with energy concentrated at a given index.
func makeUnitVector(dim int, idx int) []float32 {
	v := make([]float32, dim)
	v[idx] = 1.0
	// Normalize
	return normalize(v)
}

// Helper: create a vector opposite to makeUnitVector at given index.
func makeOppositeVector(dim int, idx int) []float32 {
	v := make([]float32, dim)
	v[idx] = -1.0
	return normalize(v)
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	mag := float32(math.Sqrt(sum))
	if mag == 0 {
		return v
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / mag
	}
	return out
}

func TestGetNonExistent(t *testing.T) {
	ctx := context.Background()
	handle := "66666666-6666-6666-6666-666666666666"

	_, err := backend.Get(ctx, memory.GetRequest{
		BackendHandle: handle,
		ID:            "99999999-9999-9999-9999-999999999999",
	})
	if err == nil {
		t.Fatal("expected error for non-existent record")
	}
	if !errors.Is(err, memory.ErrRecordNotFound) {
		t.Fatalf("expected ErrRecordNotFound, got %q", err)
	}
}

func TestDeleteNonExistent(t *testing.T) {
	ctx := context.Background()
	handle := "66666666-6666-6666-6666-666666666666"

	err := backend.Delete(ctx, memory.DeleteRequest{
		BackendHandle: handle,
		ID:            "99999999-9999-9999-9999-999999999999",
	})
	if err == nil {
		t.Fatal("expected error for non-existent record")
	}
	if !errors.Is(err, memory.ErrRecordNotFound) {
		t.Fatalf("expected ErrRecordNotFound, got %q", err)
	}
}

func TestSearchWithMinScore(t *testing.T) {
	ctx := context.Background()
	handle := "77777777-7777-7777-7777-777777777777"

	vecA := makeUnitVector(1536, 0)
	vecC := makeOppositeVector(1536, 0)

	// Store a similar and an opposite record
	_, _ = backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handle,
		ID:            "77777777-7777-7777-7777-000000000001",
		Content:       "similar",
		Embedding:     vecA,
	})
	_, _ = backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handle,
		ID:            "77777777-7777-7777-7777-000000000002",
		Content:       "opposite",
		Embedding:     vecC,
	})

	// Search with high MinScore — only the similar record should match
	results, err := backend.Search(ctx, memory.SearchRequest{
		BackendHandle: handle,
		Vector:        vecA,
		TopK:          10,
		MinScore:      0.5,
	})
	if err != nil {
		t.Fatalf("search with min score: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result with min score 0.5, got %d", len(results))
	}
	if results[0].Record.Content != "similar" {
		t.Fatalf("expected 'similar', got %q", results[0].Record.Content)
	}

	_ = backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: handle})
}

func TestListWithMetadataFilter(t *testing.T) {
	ctx := context.Background()
	handle := "88888888-8888-8888-8888-888888888888"

	_, _ = backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handle,
		ID:            "88888888-8888-8888-8888-000000000001",
		Content:       "apple",
		Embedding:     makeVector(1536, 0.1),
		Metadata:      map[string]string{"fruit": "yes"},
	})
	_, _ = backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handle,
		ID:            "88888888-8888-8888-8888-000000000002",
		Content:       "carrot",
		Embedding:     makeVector(1536, 0.2),
		Metadata:      map[string]string{"vegetable": "yes"},
	})

	records, err := backend.List(ctx, memory.ListRequest{
		BackendHandle: handle,
		Metadata:      map[string]string{"fruit": "yes"},
	})
	if err != nil {
		t.Fatalf("list with metadata: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record with fruit metadata, got %d", len(records))
	}
	if records[0].Content != "apple" {
		t.Fatalf("expected 'apple', got %q", records[0].Content)
	}

	_ = backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: handle})
}

func TestListWithLimit(t *testing.T) {
	ctx := context.Background()
	handle := "99999999-9999-9999-9999-aaaaaaaaaaaa"

	for i := 0; i < 5; i++ {
		_, _ = backend.Store(ctx, memory.StoreRequest{
			BackendHandle: handle,
			ID:            fmt.Sprintf("99999999-9999-9999-9999-%012d", i),
			Content:       fmt.Sprintf("item %d", i),
			Embedding:     makeVector(1536, float32(i)*0.1),
			Timestamp:     time.Now().UTC().Add(time.Duration(i) * time.Minute),
		})
	}

	records, err := backend.List(ctx, memory.ListRequest{
		BackendHandle: handle,
		Limit:         2,
	})
	if err != nil {
		t.Fatalf("list with limit: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records with limit, got %d", len(records))
	}

	_ = backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: handle})
}

func TestStoreUpsert(t *testing.T) {
	ctx := context.Background()
	handle := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	id := "aaaaaaaa-aaaa-aaaa-aaaa-000000000001"

	_, err := backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handle,
		ID:            id,
		Content:       "original",
		Embedding:     makeVector(1536, 0.1),
	})
	if err != nil {
		t.Fatalf("initial store: %v", err)
	}

	// Upsert same ID with different content
	_, err = backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handle,
		ID:            id,
		Content:       "updated",
		Embedding:     makeVector(1536, 0.2),
	})
	if err != nil {
		t.Fatalf("upsert store: %v", err)
	}

	rec, err := backend.Get(ctx, memory.GetRequest{BackendHandle: handle, ID: id})
	if err != nil {
		t.Fatalf("get after upsert: %v", err)
	}
	if rec.Content != "updated" {
		t.Fatalf("expected 'updated' after upsert, got %q", rec.Content)
	}

	_ = backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: handle})
}

func TestStoreWithoutEmbedding(t *testing.T) {
	ctx := context.Background()
	handle := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	id := "bbbbbbbb-bbbb-bbbb-bbbb-000000000001"

	_, err := backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handle,
		ID:            id,
		Content:       "no embedding",
	})
	if err != nil {
		t.Fatalf("store without embedding: %v", err)
	}

	rec, err := backend.Get(ctx, memory.GetRequest{BackendHandle: handle, ID: id})
	if err != nil {
		t.Fatalf("get record without embedding: %v", err)
	}
	if rec.Content != "no embedding" {
		t.Fatalf("expected 'no embedding', got %q", rec.Content)
	}
	if rec.Embedding != nil {
		t.Fatalf("expected nil embedding, got %v", rec.Embedding)
	}

	_ = backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: handle})
}

func TestSearchWithMetadataFilter(t *testing.T) {
	ctx := context.Background()
	handle := "cccccccc-cccc-cccc-cccc-cccccccccccc"

	vec := makeUnitVector(1536, 0)

	_, _ = backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handle,
		ID:            "cccccccc-cccc-cccc-cccc-000000000001",
		Content:       "tagged",
		Embedding:     vec,
		Metadata:      map[string]string{"category": "important"},
	})
	_, _ = backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handle,
		ID:            "cccccccc-cccc-cccc-cccc-000000000002",
		Content:       "untagged",
		Embedding:     vec,
		Metadata:      map[string]string{"category": "other"},
	})

	results, err := backend.Search(ctx, memory.SearchRequest{
		BackendHandle: handle,
		Vector:        vec,
		TopK:          10,
		Metadata:      map[string]string{"category": "important"},
	})
	if err != nil {
		t.Fatalf("search with metadata: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result with metadata filter, got %d", len(results))
	}
	if results[0].Record.Content != "tagged" {
		t.Fatalf("expected 'tagged', got %q", results[0].Record.Content)
	}

	_ = backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: handle})
}

// TestSearchSkipsNullEmbeddings verifies that Search does not crash when
// the volume contains records without embeddings (NULL embedding column).
func TestSearchSkipsNullEmbeddings(t *testing.T) {
	ctx := context.Background()
	handle := "dddddddd-dddd-dddd-dddd-dddddddddddd"

	// Store one record WITH an embedding
	vec := makeVector(1536, 0.5)
	_, err := backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handle,
		ID:            "dddddddd-dddd-dddd-dddd-000000000001",
		Content:       "has embedding",
		Embedding:     vec,
	})
	if err != nil {
		t.Fatalf("store with embedding: %v", err)
	}

	// Store one record WITHOUT an embedding (NULL)
	_, err = backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handle,
		ID:            "dddddddd-dddd-dddd-dddd-000000000002",
		Content:       "no embedding",
	})
	if err != nil {
		t.Fatalf("store without embedding: %v", err)
	}

	// Search should succeed and only return the embedded record
	results, err := backend.Search(ctx, memory.SearchRequest{
		BackendHandle: handle,
		Vector:        vec,
		TopK:          10,
	})
	if err != nil {
		t.Fatalf("search with null embedding in volume: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Record.Content != "has embedding" {
		t.Fatalf("expected 'has embedding', got %q", results[0].Record.Content)
	}

	_ = backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: handle})
}

// TestCrossVolumeIsolation verifies that two volumes can independently store
// records with the same ID without cross-contamination.
func TestCrossVolumeIsolation(t *testing.T) {
	ctx := context.Background()
	handleA := "eeeeeeee-eeee-eeee-eeee-aaaaaaaaaaaa"
	handleB := "eeeeeeee-eeee-eeee-eeee-bbbbbbbbbbbb"
	sharedID := "eeeeeeee-eeee-eeee-eeee-000000000001"

	// Store same ID in volume A
	_, err := backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handleA,
		ID:            sharedID,
		Content:       "volume A content",
		Embedding:     makeVector(1536, 0.1),
	})
	if err != nil {
		t.Fatalf("store in volume A: %v", err)
	}

	// Store same ID in volume B
	_, err = backend.Store(ctx, memory.StoreRequest{
		BackendHandle: handleB,
		ID:            sharedID,
		Content:       "volume B content",
		Embedding:     makeVector(1536, 0.9),
	})
	if err != nil {
		t.Fatalf("store in volume B: %v", err)
	}

	// Get from volume A should return A's content
	recA, err := backend.Get(ctx, memory.GetRequest{BackendHandle: handleA, ID: sharedID})
	if err != nil {
		t.Fatalf("get from volume A: %v", err)
	}
	if recA.Content != "volume A content" {
		t.Fatalf("volume A: expected 'volume A content', got %q", recA.Content)
	}

	// Get from volume B should return B's content
	recB, err := backend.Get(ctx, memory.GetRequest{BackendHandle: handleB, ID: sharedID})
	if err != nil {
		t.Fatalf("get from volume B: %v", err)
	}
	if recB.Content != "volume B content" {
		t.Fatalf("volume B: expected 'volume B content', got %q", recB.Content)
	}

	// Deprovision A should not affect B
	_ = backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: handleA})
	recB2, err := backend.Get(ctx, memory.GetRequest{BackendHandle: handleB, ID: sharedID})
	if err != nil {
		t.Fatalf("get from volume B after A deprovision: %v", err)
	}
	if recB2.Content != "volume B content" {
		t.Fatalf("volume B after A deprovision: expected 'volume B content', got %q", recB2.Content)
	}

	_ = backend.Deprovision(ctx, memory.DeprovisionRequest{BackendHandle: handleB})
}
