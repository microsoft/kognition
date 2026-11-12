// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package azuresearch_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"github.com/kognition-ai/kognition/providers/azuresearch"
	"google.golang.org/protobuf/proto"
)

func TestLiveAzureSearchLifecycle(t *testing.T) {
	if os.Getenv("KOGNITION_AZURESEARCH_LIVE_TEST") != "1" {
		t.Skip("set KOGNITION_AZURESEARCH_LIVE_TEST=1 to write acceptance documents to an approved disposable index")
	}
	dimensions, err := strconv.ParseInt(os.Getenv("KOGNITION_AZURESEARCH_DIMENSIONS"), 10, 32)
	if err != nil {
		t.Fatal("KOGNITION_AZURESEARCH_DIMENSIONS must match the existing index embedding dimensions")
	}
	spec := azuresearch.BackendSpec{
		ServiceName:         os.Getenv("KOGNITION_AZURESEARCH_SERVICE"),
		IndexName:           os.Getenv("KOGNITION_AZURESEARCH_INDEX"),
		EmbeddingDimensions: int32(dimensions),
		Authentication:      azuresearch.AuthenticationConfig{Mode: "workload-identity"},
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := azuresearch.NewClient(credential, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	indexer, err := azuresearch.NewIndexer(client, spec)
	if err != nil {
		t.Fatal(err)
	}
	cleaner, err := azuresearch.NewCleaner(client)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	identity := strings.ToLower(rand.Text())
	t.Logf("acceptance ownership: namespace=search-acceptance-a knowledgeBaseUid=%s", identity)
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	scope := &protocol.KnowledgeScope{
		Namespace: "search-acceptance-a", KnowledgeBaseUid: identity, Generation: 1,
		OperationId:           identity + "-initial",
		ConfigurationApiGroup: azuresearch.APIGroup, ConfigurationKind: azuresearch.Kind,
		ConfigurationName: "live-acceptance", ConfigurationUid: identity,
		ConfigurationDigest: hex.EncodeToString(digest[:]),
	}
	vector := make([]float64, dimensions)
	vector[0] = 1

	stage := func(binding *protocol.KnowledgeScope, documentIDs ...string) *protocol.KnowledgeCleanupRequest {
		t.Helper()
		chunks := make([]*protocol.KnowledgeIndexChunk, 0, len(documentIDs))
		for _, documentID := range documentIDs {
			chunks = append(chunks, &protocol.KnowledgeIndexChunk{
				DocumentId: documentID, ChunkId: "chunk-1", Content: "kognitionacceptance " + documentID,
				SourceUri: "https://example.test/acceptance/" + documentID, Location: "page 1", Vector: vector,
			})
		}
		content, err := json.Marshal(chunks)
		if err != nil {
			t.Fatal(err)
		}
		sourceHash := sha256.Sum256(content)
		sourceDigest := "sha256:" + hex.EncodeToString(sourceHash[:])
		transaction, err := indexer.BeginIndex(ctx, binding, binding.OperationId, sourceDigest)
		if err != nil {
			t.Fatal(err)
		}
		var cleanup *protocol.KnowledgeCleanupRequest
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cleanupCancel()
			if cleanup == nil {
				if err := transaction.Abort(cleanupCtx); err != nil {
					t.Errorf("abort acceptance operation %s: %v", binding.OperationId, err)
				}
			} else if _, err := cleaner.Cleanup(cleanupCtx, cleanup); err != nil {
				t.Errorf("delete acceptance operation %s: %v", binding.OperationId, err)
			}
		})
		if err := transaction.WriteBatch(ctx, &protocol.KnowledgeIndexBatchRequest{
			Scope: binding, OperationId: binding.OperationId, SourceDigest: sourceDigest, Chunks: chunks,
		}); err != nil {
			t.Fatal(err)
		}
		confirmation, err := transaction.Commit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		cleanup = &protocol.KnowledgeCleanupRequest{
			Scope: proto.Clone(binding).(*protocol.KnowledgeScope), CleanupId: binding.OperationId + "-cleanup",
			BackendOperationId: confirmation.BackendOperationId,
			ReclaimPolicy:      protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE,
		}
		if confirmation.DocumentCount != int64(len(chunks)) || confirmation.IndexedChunks != int64(len(chunks)) ||
			confirmation.BackendOperationId == "" || !proto.Equal(confirmation.Scope, binding) {
			t.Fatalf("unexpected index confirmation: %v", confirmation)
		}
		return cleanup
	}

	query := func(binding *protocol.KnowledgeScope, documentIDs ...string) {
		t.Helper()
		querier, err := azuresearch.NewQuerier(client, spec, binding)
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"keyword", "dense", "hybrid"} {
			request := &protocol.KnowledgeQueryRequest{Scope: binding, RequestId: identity + "-" + mode, Mode: mode, TopK: 10}
			if mode != "dense" {
				request.Text = "kognitionacceptance"
			}
			if mode != "keyword" {
				request.Vector = vector
			}
			response, err := querier.Query(ctx, request)
			if err != nil {
				t.Fatalf("%s query: %v", mode, err)
			}
			if len(response.Matches) != len(documentIDs) || !proto.Equal(response.Scope, binding) || response.RequestId != request.RequestId {
				t.Fatalf("%s query returned unexpected scope or matches: %v", mode, response)
			}
			wanted := make(map[string]bool, len(documentIDs))
			for _, documentID := range documentIDs {
				wanted[documentID] = true
			}
			for _, match := range response.Matches {
				if !wanted[match.DocumentId] || match.ChunkId != "chunk-1" || match.Content != "kognitionacceptance "+match.DocumentId ||
					match.SourceUri != "https://example.test/acceptance/"+match.DocumentId || match.Location != "page 1" {
					t.Fatalf("%s query returned an unexpected document or citation: %v", mode, match)
				}
				delete(wanted, match.DocumentId)
			}
		}
	}

	initialCleanup := stage(scope, "kept", "removed")
	query(scope, "kept", "removed")
	neighbor := proto.Clone(scope).(*protocol.KnowledgeScope)
	neighbor.Namespace = "search-acceptance-b"
	stage(neighbor, "other-tenant")
	query(scope, "kept", "removed")
	query(neighbor, "other-tenant")
	refresh := proto.Clone(scope).(*protocol.KnowledgeScope)
	refresh.Generation++
	refresh.OperationId = identity + "-refresh"
	refreshCleanup := stage(refresh, "kept", "added")
	query(refresh, "kept", "added")
	query(scope, "kept", "removed")
	retained := proto.Clone(refreshCleanup).(*protocol.KnowledgeCleanupRequest)
	retained.ReclaimPolicy = protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_RETAIN
	if _, err := cleaner.Cleanup(ctx, retained); err != nil {
		t.Fatal(err)
	}
	query(refresh, "kept", "added")
	if _, err := cleaner.Cleanup(ctx, initialCleanup); err != nil {
		t.Fatal(err)
	}
	query(scope)
	query(refresh, "kept", "added")
	query(neighbor, "other-tenant")
	if _, err := cleaner.Cleanup(ctx, refreshCleanup); err != nil {
		t.Fatal(err)
	}
	query(refresh)
	query(neighbor, "other-tenant")
}
