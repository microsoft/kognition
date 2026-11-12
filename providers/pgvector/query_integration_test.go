// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector_test

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func isolatedDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("KOGNITION_PGVECTOR_TEST_DSN")
	if dsn == "" {
		t.Skip("set KOGNITION_PGVECTOR_TEST_DSN to run real pgvector retrieval tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid pgvector test DSN")
	}
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot construct pgvector test admin pool")
	}
	t.Cleanup(admin.Close)
	database := "knowledge_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
		t.Fatal("cannot create isolated pgvector test database")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error("cannot remove isolated pgvector test database")
		}
	})
	config = config.Copy()
	config.ConnConfig.Database = database
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot construct isolated pgvector test pool")
	}
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `CREATE EXTENSION vector WITH SCHEMA public;
		CREATE TABLE public.knowledge_chunks (
		 namespace text NOT NULL, knowledge_base_uid text NOT NULL, generation bigint NOT NULL,
		 configuration_uid text NOT NULL, configuration_digest text NOT NULL,
		 document_id text NOT NULL, chunk_id text NOT NULL, content text NOT NULL,
		 source_uri text NOT NULL, location text NOT NULL, metadata jsonb NOT NULL,
		 embedding public.vector(3),
		 PRIMARY KEY(namespace, knowledge_base_uid, generation, configuration_uid, configuration_digest, document_id, chunk_id));`)
	if err != nil {
		t.Fatal("pgvector extension or provider fixture schema unavailable")
	}
	return pool
}

func TestQuerierPostgresIsolationAndCitations(t *testing.T) {
	pool := isolatedDatabase(t)
	querier := testQuerier(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	insert := func(name string, scope *protocol.KnowledgeScope, embedding, source string) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO public.knowledge_chunks VALUES ($1,$2,$3,$4,$5,$6,'chunk-1','Known corpus content',$7,'page 2','{"type":"public","nested.key":"literal"}',$8::public.vector)`, scope.Namespace, scope.KnowledgeBaseUid, scope.Generation, scope.ConfigurationUid, scope.ConfigurationDigest, name, source, embedding)
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, mutate := range map[string]func(*protocol.KnowledgeScope){
		"other-tenant":      func(scope *protocol.KnowledgeScope) { scope.Namespace = "tenant-b" },
		"other-KB":          func(scope *protocol.KnowledgeScope) { scope.KnowledgeBaseUid = "kb-b" },
		"old-generation":    func(scope *protocol.KnowledgeScope) { scope.Generation = 1 },
		"staged-generation": func(scope *protocol.KnowledgeScope) { scope.Generation = 3 },
		"other-config":      func(scope *protocol.KnowledgeScope) { scope.ConfigurationUid = "config-b" },
		"other-digest":      func(scope *protocol.KnowledgeScope) { scope.ConfigurationDigest = strings.Repeat("b", 64) },
	} {
		scope := queryRequest().Scope
		mutate(scope)
		insert(name, scope, "[1,0,0]", "https://example.org/other")
	}
	insert("allowed", queryRequest().Scope, "[0.8,0.6,0]", "https://example.org/document")
	insert("allowed-second", queryRequest().Scope, "[0,1,0]", "urn:example:second")
	request := queryRequest()
	response, err := querier.Query(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(response.Scope, request.Scope) || response.RequestId != request.RequestId || len(response.Matches) != 2 {
		t.Fatalf("incorrect bound results: %v", response)
	}
	match := response.Matches[0]
	if match.DocumentId != "allowed" || match.ChunkId != "chunk-1" || match.Content != "Known corpus content" || match.SourceUri != "https://example.org/document" || match.Location != "page 2" || math.Abs(match.Score-0.8) > 0.00001 {
		t.Fatalf("incorrect citation: %v", match)
	}
	request.TopK = 1
	response, err = querier.Query(ctx, request)
	if err != nil || len(response.Matches) != 1 || response.Matches[0].DocumentId != "allowed" {
		t.Fatalf("top-k result: %v %v", response, err)
	}
	for _, filter := range []*protocol.KnowledgeFilter{{Key: "metadata.type", Value: "public"}, {Key: "metadata.nested.key", Value: "literal"}, {Key: "metadata.type", Value: "' OR true --"}, {Key: "metadata.namespace", Value: "tenant-b"}} {
		request := queryRequest()
		request.Filters = []*protocol.KnowledgeFilter{filter}
		response, err := querier.Query(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if filter.Value == "public" || filter.Value == "literal" {
			want = 2
		}
		if len(response.Matches) != want {
			t.Fatalf("filter returned %d matches, want %d", len(response.Matches), want)
		}
	}
	for _, source := range []string{"https://user:private-value@example.org/doc", "https://example.org/doc?sig=private-value", "https://example.org/doc#private-value", "file:///private-value"} {
		insert("invalid-citation", queryRequest().Scope, "[1,0,0]", source)
		response, err := querier.Query(ctx, queryRequest())
		if response != nil || status.Code(err) != codes.DataLoss || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("unsafe citation not rejected: %v", err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM public.knowledge_chunks WHERE document_id = 'invalid-citation'"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("invalid and oversized backend response", func(t *testing.T) {
		_, err := pool.Exec(ctx, `UPDATE public.knowledge_chunks SET content = repeat('x', 65537) WHERE document_id = 'allowed'`)
		if err != nil {
			t.Fatal(err)
		}
		if response, err := querier.Query(ctx, queryRequest()); response != nil || status.Code(err) != codes.DataLoss {
			t.Fatalf("oversized content: %v", err)
		}
		_, err = pool.Exec(ctx, `UPDATE public.knowledge_chunks SET content = 'Known corpus content' WHERE document_id = 'allowed'`)
		if err != nil {
			t.Fatal(err)
		}
		_, err = pool.Exec(ctx, `INSERT INTO public.knowledge_chunks SELECT namespace, knowledge_base_uid, generation, configuration_uid, configuration_digest, 'large-' || series::text, chunk_id, repeat('x',65536), source_uri, location, metadata, embedding FROM public.knowledge_chunks CROSS JOIN generate_series(1,65) AS series WHERE document_id = 'allowed'`)
		if err != nil {
			t.Fatal(err)
		}
		request := queryRequest()
		request.TopK = 100
		if response, err := querier.Query(ctx, request); response != nil || status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("oversized response: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM public.knowledge_chunks WHERE document_id LIKE 'large-%'`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE public.knowledge_chunks SET embedding = '[0,0,0]' WHERE document_id = 'allowed'`); err != nil {
			t.Fatal(err)
		}
		if response, err := querier.Query(ctx, queryRequest()); response != nil || status.Code(err) != codes.DataLoss {
			t.Fatalf("nonfinite backend score: %v", err)
		}
	})
	transaction, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback(context.Background())
	if _, err := transaction.Exec(ctx, "LOCK TABLE public.knowledge_chunks IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	deadline, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stop()
	if _, err := querier.Query(deadline, queryRequest()); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("database cancellation failed: %v", err)
	}
}

func TestQuerierPostgresIndexedScopeCompleteness(t *testing.T) {
	pool := isolatedDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `INSERT INTO public.knowledge_chunks
		SELECT 'tenant-b', 'kb-a', 2, 'config-a', repeat('a',64), 'other-' || series,
		'chunk', 'other', 'urn:example:other', '', '{}', '[1,0,0]'::public.vector
		FROM generate_series(1,1000) AS series;
		INSERT INTO public.knowledge_chunks VALUES
		('tenant-a','kb-a',2,'config-a',repeat('a',64),'allowed','chunk','allowed','urn:example:allowed','','{}','[0,1,0]');
		CREATE INDEX ON public.knowledge_chunks USING hnsw (embedding public.vector_cosine_ops);
		ANALYZE public.knowledge_chunks;`)
	if err != nil {
		t.Fatal(err)
	}
	config := pool.Config()
	config.ConnConfig.RuntimeParams["enable_seqscan"] = "off"
	config.ConnConfig.RuntimeParams["hnsw.ef_search"] = "1"
	config.ConnConfig.RuntimeParams["hnsw.iterative_scan"] = "off"
	indexed, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer indexed.Close()
	response, err := testQuerier(t, indexed).Query(ctx, queryRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Matches) != 1 || response.Matches[0].DocumentId != "allowed" {
		t.Fatalf("indexed retrieval lost an authorized match: %v", response)
	}
}
