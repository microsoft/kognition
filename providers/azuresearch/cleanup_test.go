// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuresearch

import (
	"context"
	"strings"
	"testing"

	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type cleanupClient struct {
	documents []SearchDocument
	searches  []SearchRequest
	deletes   [][]IndexDocument
	result    *IndexBatchResult
}

func (client *cleanupClient) Search(_ context.Context, request SearchRequest) ([]SearchDocument, error) {
	client.searches = append(client.searches, request)
	return append([]SearchDocument(nil), client.documents...), nil
}

func (client *cleanupClient) Index(_ context.Context, documents []IndexDocument) (IndexBatchResult, error) {
	client.deletes = append(client.deletes, append([]IndexDocument(nil), documents...))
	if client.result != nil {
		return *client.result, nil
	}
	results := make([]IndexDocumentResult, 0, len(documents))
	for _, document := range documents {
		results = append(results, IndexDocumentResult{Key: document.Key, Succeeded: true, StatusCode: 200})
	}
	client.documents = nil
	return IndexBatchResult{RequestID: "cleanup-request", Documents: results}, nil
}

func (client *cleanupClient) Count(context.Context, string) (int64, error) {
	return int64(len(client.documents)), nil
}

func cleanupRequest(policy protocol.KnowledgeReclaimPolicy) *protocol.KnowledgeCleanupRequest {
	return &protocol.KnowledgeCleanupRequest{
		Scope: &protocol.KnowledgeScope{Namespace: "tenant-a", KnowledgeBaseUid: "kb-uid", Generation: 2,
			ConfigurationApiGroup: APIGroup, ConfigurationKind: Kind, ConfigurationName: "shared", ConfigurationUid: "config-uid",
			ConfigurationDigest: strings.Repeat("a", 64), OperationId: "index-operation-2"},
		CleanupId: "cleanup-kb-uid", ReclaimPolicy: policy, BackendOperationId: "v1.cleanup-locator",
	}
}

func TestCleanerDeletesOnlyExactCommittedScopeAndConfirmsZero(t *testing.T) {
	client := &cleanupClient{documents: []SearchDocument{{Key: "key-1"}, {Key: "key-2"}}}
	cleaner, err := NewCleaner(client)
	if err != nil {
		t.Fatal(err)
	}
	request := cleanupRequest(protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE)
	response, err := cleaner.Cleanup(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.searches) != 1 || len(client.deletes) != 1 || len(client.deletes[0]) != 2 {
		t.Fatalf("searches=%d delete batches=%d deletes=%d", len(client.searches), len(client.deletes), len(client.deletes[0]))
	}
	filter := client.searches[0].Filter
	for _, exact := range []string{"namespace eq 'tenant-a'", "knowledgeBaseUid eq 'kb-uid'", "generation eq 2", "operationId eq 'index-operation-2'", "configurationUid eq 'config-uid'"} {
		if !strings.Contains(filter, exact) {
			t.Fatalf("cleanup filter %q is missing %q", filter, exact)
		}
	}
	if response.CleanupId != request.CleanupId || response.ReclaimPolicy != request.ReclaimPolicy || response.BackendOperationId == "" {
		t.Fatalf("unexpected cleanup confirmation: %+v", response)
	}
	request.Scope.Namespace = "mutated"
	if response.Scope.Namespace != "tenant-a" {
		t.Fatal("cleanup confirmation retained caller-owned scope")
	}
}

func TestCleanerRetainDoesNotTouchDocuments(t *testing.T) {
	client := &cleanupClient{documents: []SearchDocument{{Key: "key-1"}}}
	cleaner, _ := NewCleaner(client)
	response, err := cleaner.Cleanup(context.Background(), cleanupRequest(protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_RETAIN))
	if err != nil {
		t.Fatal(err)
	}
	if len(client.searches) != 0 || len(client.deletes) != 0 || response.BackendOperationId == "" {
		t.Fatalf("retain touched backend or omitted confirmation: %+v", response)
	}
}

func TestCleanerRejectsArchive(t *testing.T) {
	cleaner, _ := NewCleaner(&cleanupClient{})
	_, err := cleaner.Cleanup(context.Background(), cleanupRequest(protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_ARCHIVE))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code=%s error=%v", status.Code(err), err)
	}
}

func TestCleanerRejectsIncompleteOrMismatchedDeleteConfirmation(t *testing.T) {
	for name, results := range map[string][]IndexDocumentResult{
		"missing result":   {{Key: "key-1", Succeeded: true, StatusCode: 200}},
		"duplicate result": {{Key: "key-1", Succeeded: true, StatusCode: 200}, {Key: "key-1", Succeeded: true, StatusCode: 200}},
		"foreign result":   {{Key: "key-1", Succeeded: true, StatusCode: 200}, {Key: "other", Succeeded: true, StatusCode: 200}},
	} {
		t.Run(name, func(t *testing.T) {
			client := &cleanupClient{
				documents: []SearchDocument{{Key: "key-1"}, {Key: "key-2"}},
				result:    &IndexBatchResult{RequestID: "cleanup-request", Documents: results},
			}
			cleaner, _ := NewCleaner(client)
			_, err := cleaner.Cleanup(context.Background(), cleanupRequest(protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE))
			if status.Code(err) != codes.DataLoss {
				t.Fatalf("code=%s error=%v", status.Code(err), err)
			}
		})
	}
}
