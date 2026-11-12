// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"github.com/kognition-ai/kognition/providers/pgvector"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestLifecyclePostgresCommitReplayAbortAndCleanup(t *testing.T) {
	pool := isolatedDatabase(t)
	spec, err := pgvector.DecodeBackend(validBackend())
	if err != nil {
		t.Fatal(err)
	}
	spec.Table, spec.EmbeddingDimensions = "knowledge_chunks", 3
	schema, err := pgvector.SchemaSQL(spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, schema); err != nil {
		t.Fatal("cannot prepare provider-owned lifecycle schema")
	}
	lifecycle, err := pgvector.NewLifecycle(pool, spec)
	if err != nil {
		t.Fatal(err)
	}
	scope := queryRequest().Scope
	pendingScope := proto.Clone(scope).(*protocol.KnowledgeScope)
	pendingScope.OperationId = ""
	batch := &protocol.KnowledgeIndexBatchRequest{
		Scope: pendingScope, OperationId: scope.OperationId, SourceDigest: "sha256:" + strings.Repeat("b", 64),
		Chunks: []*protocol.KnowledgeIndexChunk{
			{DocumentId: "document", ChunkId: "chunk-1", Content: "Known product fact.", SourceUri: "urn:kognition:test:document", Vector: []float64{1, 0, 0}},
			{DocumentId: "document", ChunkId: "chunk-2", Content: "Another product fact.", SourceUri: "urn:kognition:test:document", Vector: []float64{0, 1, 0}},
		},
	}
	begin := func(request *protocol.KnowledgeIndexBatchRequest) pluginsdk.KnowledgeIndexTransaction {
		t.Helper()
		transaction, err := lifecycle.BeginIndex(ctx, request.Scope, request.OperationId, request.SourceDigest)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := transaction.Abort(context.Background()); err != nil {
				t.Error(err)
			}
		})
		return transaction
	}
	transaction := begin(batch)
	if err := transaction.WriteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	querier := testQuerier(t, pool)
	before, err := querier.Query(ctx, queryRequest())
	if err != nil || len(before.Matches) != 0 {
		t.Fatalf("staged rows became visible: %v", err)
	}
	committed, err := transaction.Commit(ctx)
	if err != nil || committed.DocumentCount != 1 || committed.IndexedChunks != 2 || committed.BackendOperationId == "" {
		t.Fatalf("commit confirmation: %v, %v", committed, err)
	}
	after, err := querier.Query(ctx, queryRequest())
	if err != nil || len(after.Matches) != 2 || after.Matches[0].Content != "Known product fact." {
		t.Fatalf("committed retrieval: %v, %v", after, err)
	}
	replay := begin(batch)
	if err := replay.WriteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	replayed, err := replay.Commit(ctx)
	if err != nil || !proto.Equal(replayed, committed) {
		t.Fatalf("idempotent replay differs: %v", err)
	}
	changed := proto.Clone(batch).(*protocol.KnowledgeIndexBatchRequest)
	changed.Chunks[0].Content = "Changed content under the same operation."
	different := begin(changed)
	if err := different.WriteBatch(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := different.Commit(ctx); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("changed replay accepted: %v", err)
	}
	if err := different.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	staged := proto.Clone(batch).(*protocol.KnowledgeIndexBatchRequest)
	staged.Scope.Generation++
	staged.OperationId = "index-3"
	aborted := begin(staged)
	if err := aborted.WriteBatch(ctx, staged); err != nil {
		t.Fatal(err)
	}
	if err := aborted.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	stagedQueryScope := proto.Clone(staged.Scope).(*protocol.KnowledgeScope)
	stagedQueryScope.OperationId = staged.OperationId
	stagedQuerier, err := pgvector.NewQuerier(pool, spec, stagedQueryScope)
	if err != nil {
		t.Fatal(err)
	}
	request := queryRequest()
	request.Scope = stagedQueryScope
	if result, err := stagedQuerier.Query(ctx, request); err != nil || len(result.Matches) != 0 {
		t.Fatalf("aborted rows remained: %v", err)
	}
	cleanup := &protocol.KnowledgeCleanupRequest{Scope: scope, CleanupId: "cleanup", BackendOperationId: committed.BackendOperationId, ReclaimPolicy: protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_RETAIN}
	if _, err := lifecycle.Cleanup(ctx, cleanup); err != nil {
		t.Fatal(err)
	}
	if result, err := querier.Query(ctx, queryRequest()); err != nil || len(result.Matches) != 2 {
		t.Fatalf("Retain deleted data: %v", err)
	}
	cleanup.ReclaimPolicy = protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := lifecycle.Cleanup(ctx, cleanup); err != nil {
			t.Fatalf("idempotent cleanup %d: %v", attempt, err)
		}
	}
	if result, err := querier.Query(ctx, queryRequest()); err != nil || len(result.Matches) != 0 {
		t.Fatalf("Delete retained data: %v", err)
	}
	if _, err := lifecycle.BeginIndex(ctx, pendingScope, batch.OperationId, batch.SourceDigest); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("retired generation was recreated: %v", err)
	}
}
