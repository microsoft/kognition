// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuresearch_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"github.com/kognition-ai/kognition/providers/azuresearch"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type recordingDocumentIndexer struct {
	requests [][]azuresearch.IndexDocument
	failAt   int
	outcomes [][]int
	counts   []int64
	filters  []string
}

func (indexer *recordingDocumentIndexer) Index(_ context.Context, documents []azuresearch.IndexDocument) (azuresearch.IndexBatchResult, error) {
	copied := append([]azuresearch.IndexDocument(nil), documents...)
	indexer.requests = append(indexer.requests, copied)
	if indexer.failAt > 0 && len(indexer.requests) == indexer.failAt {
		return azuresearch.IndexBatchResult{}, errors.New("backend detail")
	}
	result := azuresearch.IndexBatchResult{RequestID: "request-id"}
	statuses := []int(nil)
	if len(indexer.outcomes) > 0 {
		statuses = indexer.outcomes[0]
		indexer.outcomes = indexer.outcomes[1:]
	}
	for position, document := range documents {
		statusCode := http.StatusOK
		if position < len(statuses) {
			statusCode = statuses[position]
		}
		result.Documents = append(result.Documents, azuresearch.IndexDocumentResult{Key: document.Key, Succeeded: statusCode >= 200 && statusCode < 300, StatusCode: statusCode})
	}
	return result, nil
}

func (indexer *recordingDocumentIndexer) Count(_ context.Context, filter string) (int64, error) {
	indexer.filters = append(indexer.filters, filter)
	if len(indexer.counts) == 0 {
		keys := make(map[string]struct{})
		for _, request := range indexer.requests {
			for _, document := range request {
				if document.Action == "delete" {
					delete(keys, document.Key)
				} else {
					keys[document.Key] = struct{}{}
				}
			}
		}
		return int64(len(keys)), nil
	}
	count := indexer.counts[0]
	indexer.counts = indexer.counts[1:]
	return count, nil
}

func indexScope() *protocol.KnowledgeScope {
	return &protocol.KnowledgeScope{
		Namespace: "tenant-a", KnowledgeBaseUid: "kb-uid", Generation: 4,
		ConfigurationApiGroup: azuresearch.APIGroup, ConfigurationKind: azuresearch.Kind,
		ConfigurationName: "search", ConfigurationUid: "config-uid",
		ConfigurationDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
}

func indexBatch(scope *protocol.KnowledgeScope, sequence int32, chunks ...*protocol.KnowledgeIndexChunk) *protocol.KnowledgeIndexBatchRequest {
	return &protocol.KnowledgeIndexBatchRequest{Scope: scope, OperationId: "operation-1", SourceDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Sequence: sequence, Chunks: chunks}
}

func chunk(documentID, chunkID string) *protocol.KnowledgeIndexChunk {
	return &protocol.KnowledgeIndexChunk{DocumentId: documentID, ChunkId: chunkID, Content: "content", SourceUri: "https://example.test/source", Location: "page 1", Vector: []float64{0.1, 0.2, 0.3}}
}

func newIndexTransaction(t *testing.T, client *recordingDocumentIndexer) pluginsdk.KnowledgeIndexTransaction {
	t.Helper()
	indexer, err := azuresearch.NewIndexer(client, azuresearch.BackendSpec{ServiceName: "kognition-search", IndexName: "knowledge_chunks", EmbeddingDimensions: 3, Authentication: azuresearch.AuthenticationConfig{Mode: "workload-identity"}})
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := indexer.BeginIndex(context.Background(), indexScope(), "operation-1", "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	return transaction
}

func TestIndexerWritesScopedDocumentsAndConfirmsExactCounts(t *testing.T) {
	client := &recordingDocumentIndexer{}
	transaction := newIndexTransaction(t, client)
	if err := transaction.WriteBatch(context.Background(), indexBatch(indexScope(), 0, chunk("doc-1", "chunk-1"), chunk("doc-1", "chunk-2"))); err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteBatch(context.Background(), indexBatch(indexScope(), 1, chunk("doc-2", "chunk-1"))); err != nil {
		t.Fatal(err)
	}
	response, err := transaction.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if response.DocumentCount != 2 || response.IndexedChunks != 3 || response.BackendOperationId == "" || len(client.requests) != 2 {
		t.Fatalf("unexpected confirmation or requests: response=%+v requests=%d", response, len(client.requests))
	}
	written := client.requests[0][0]
	if written.Action != "mergeOrUpload" || written.Namespace != "tenant-a" || written.KnowledgeBaseUID != "kb-uid" || written.Generation != 4 || written.OperationID != "operation-1" || written.ConfigurationUID != "config-uid" || written.ConfigurationDigest != indexScope().ConfigurationDigest {
		t.Fatalf("document is not bound to immutable scope: %+v", written)
	}
}

func TestIndexerCommitWaitsForExactOperationVisibility(t *testing.T) {
	client := &recordingDocumentIndexer{counts: []int64{0, 1, 2}}
	transaction := newIndexTransaction(t, client)
	if err := transaction.WriteBatch(context.Background(), indexBatch(indexScope(), 0, chunk("doc-1", "chunk-1"), chunk("doc-2", "chunk-1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.filters) != 3 || client.filters[0] != client.filters[1] || client.filters[1] != client.filters[2] {
		t.Fatalf("visibility checks = %#v, want stable operation filter", client.filters)
	}
	want := "namespace eq 'tenant-a' and knowledgeBaseUid eq 'kb-uid' and generation eq 4 and operationId eq 'operation-1' and configurationUid eq 'config-uid' and configurationDigest eq '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'"
	if client.filters[0] != want {
		t.Fatalf("visibility filter = %q, want %q", client.filters[0], want)
	}
}

func TestIndexerCommitDoesNotPublishBeforeVisibility(t *testing.T) {
	client := &recordingDocumentIndexer{counts: []int64{0}}
	transaction := newIndexTransaction(t, client)
	if err := transaction.WriteBatch(context.Background(), indexBatch(indexScope(), 0, chunk("doc-1", "chunk-1"))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := transaction.Commit(ctx); status.Code(err) != codes.Canceled {
		t.Fatalf("commit status=%s err=%v, want canceled", status.Code(err), err)
	}
}

func TestIndexerRejectsCrossBatchDuplicateAndAbortsStagedWrites(t *testing.T) {
	client := &recordingDocumentIndexer{}
	transaction := newIndexTransaction(t, client)
	if err := transaction.WriteBatch(context.Background(), indexBatch(indexScope(), 0, chunk("doc-1", "chunk-1"))); err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteBatch(context.Background(), indexBatch(indexScope(), 1, chunk("doc-1", "chunk-1"))); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("duplicate status=%s err=%v", status.Code(err), err)
	}
	if err := transaction.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.requests) != 2 || len(client.requests[1]) != 1 || client.requests[1][0].Action != "delete" || client.requests[1][0].Key != client.requests[0][0].Key {
		t.Fatalf("abort did not compensate staged write: %+v", client.requests)
	}
}

func TestIndexerAbortsAttemptedBatchAfterResponseLoss(t *testing.T) {
	client := &recordingDocumentIndexer{failAt: 1}
	transaction := newIndexTransaction(t, client)
	if err := transaction.WriteBatch(context.Background(), indexBatch(indexScope(), 0, chunk("doc-1", "chunk-1"))); status.Code(err) != codes.Unavailable {
		t.Fatalf("failure status=%s err=%v", status.Code(err), err)
	}
	if err := transaction.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.requests) != 2 || len(client.requests[1]) != 1 || client.requests[1][0].Action != "delete" || client.requests[1][0].Key != client.requests[0][0].Key {
		t.Fatalf("abort did not compensate uncertain write: %+v", client.requests)
	}
}

func TestIndexerAbortRejectsPartialDocumentFailure(t *testing.T) {
	client := &recordingDocumentIndexer{outcomes: [][]int{{http.StatusCreated}, {http.StatusServiceUnavailable}}}
	transaction := newIndexTransaction(t, client)
	if err := transaction.WriteBatch(context.Background(), indexBatch(indexScope(), 0, chunk("doc-1", "chunk-1"))); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Abort(context.Background()); status.Code(err) != codes.Unavailable {
		t.Fatalf("abort status=%s err=%v, want Unavailable", status.Code(err), err)
	}
}

func TestIndexerRetriesOnlyTransientDocumentFailures(t *testing.T) {
	for _, retryableStatus := range []int{http.StatusConflict, http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(retryableStatus), func(t *testing.T) {
			client := &recordingDocumentIndexer{outcomes: [][]int{{http.StatusCreated, retryableStatus}, {http.StatusOK}}}
			transaction := newIndexTransaction(t, client)
			if err := transaction.WriteBatch(context.Background(), indexBatch(indexScope(), 0, chunk("doc-1", "chunk-1"), chunk("doc-2", "chunk-1"))); err != nil {
				t.Fatal(err)
			}
			if len(client.requests) != 2 || len(client.requests[0]) != 2 || len(client.requests[1]) != 1 || client.requests[1][0].Key != client.requests[0][1].Key {
				t.Fatalf("requests = %+v, want only transient document retried", client.requests)
			}
		})
	}
}

func TestIndexerDoesNotRetryPermanentDocumentFailure(t *testing.T) {
	client := &recordingDocumentIndexer{outcomes: [][]int{{http.StatusBadRequest, http.StatusCreated}}}
	transaction := newIndexTransaction(t, client)
	if err := transaction.WriteBatch(context.Background(), indexBatch(indexScope(), 0, chunk("doc-1", "chunk-1"), chunk("doc-2", "chunk-1"))); status.Code(err) != codes.Unavailable {
		t.Fatalf("failure status=%s err=%v", status.Code(err), err)
	}
	if len(client.requests) != 1 {
		t.Fatalf("permanent failure retried: %+v", client.requests)
	}
}

func TestOverlappingTransactionsOwnDistinctDocumentKeys(t *testing.T) {
	client := &recordingDocumentIndexer{}
	first := newIndexTransaction(t, client)
	indexer, err := azuresearch.NewIndexer(client, azuresearch.BackendSpec{ServiceName: "kognition-search", IndexName: "knowledge_chunks", EmbeddingDimensions: 3, Authentication: azuresearch.AuthenticationConfig{Mode: "workload-identity"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := indexer.BeginIndex(context.Background(), indexScope(), "operation-2", "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.WriteBatch(context.Background(), indexBatch(indexScope(), 0, chunk("doc-1", "chunk-1"))); err != nil {
		t.Fatal(err)
	}
	secondBatch := indexBatch(indexScope(), 0, chunk("doc-1", "chunk-1"))
	secondBatch.OperationId = "operation-2"
	if err := second.WriteBatch(context.Background(), secondBatch); err != nil {
		t.Fatal(err)
	}
	if client.requests[0][0].Key == client.requests[1][0].Key {
		t.Fatal("overlapping operations shared a document key")
	}
	if err := first.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.requests[2][0].Key != client.requests[0][0].Key || client.requests[2][0].Key == client.requests[1][0].Key {
		t.Fatalf("abort targeted another operation: %+v", client.requests)
	}
}
