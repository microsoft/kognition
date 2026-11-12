// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pluginsdk_test

import (
	"math"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func validKnowledgeQuery() *protocol.KnowledgeQueryRequest {
	return &protocol.KnowledgeQueryRequest{
		Scope: &protocol.KnowledgeScope{Namespace: "tenant-a", KnowledgeBaseUid: "kb-uid", Generation: 1,
			ConfigurationApiGroup: "pgvector.knowledge.kognition.dev", ConfigurationKind: "PgvectorBackend", ConfigurationName: "shared",
			ConfigurationUid: "config-uid", ConfigurationDigest: strings.Repeat("a", 64), OperationId: "index-operation-1"},
		RequestId: "request-1", Mode: "keyword", Text: "knowledge", TopK: 3,
	}
}

func validKnowledgeIndexBatch() *protocol.KnowledgeIndexBatchRequest {
	return &protocol.KnowledgeIndexBatchRequest{
		Scope:        validKnowledgeQuery().Scope,
		OperationId:  "index-operation-1",
		SourceDigest: "sha256:" + strings.Repeat("b", 64),
		Chunks: []*protocol.KnowledgeIndexChunk{{
			DocumentId: "document-1", ChunkId: "chunk-1", Content: "indexed knowledge",
			SourceUri: "https://example.test/document", Location: "paragraph=1", Vector: []float64{0.1, 0.2, 0.3},
		}},
	}
}

func validKnowledgeCleanup() *protocol.KnowledgeCleanupRequest {
	return &protocol.KnowledgeCleanupRequest{
		Scope:              validKnowledgeQuery().Scope,
		CleanupId:          "cleanup-kb-uid",
		ReclaimPolicy:      protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE,
		BackendOperationId: "committed-backend-operation",
	}
}

func TestKnowledgeCleanupScopeAndBounds(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*protocol.KnowledgeCleanupRequest)
	}{
		{"missing scope", func(request *protocol.KnowledgeCleanupRequest) { request.Scope = nil }},
		{"missing committed operation", func(request *protocol.KnowledgeCleanupRequest) { request.Scope.OperationId = "" }},
		{"missing backend operation", func(request *protocol.KnowledgeCleanupRequest) { request.BackendOperationId = "" }},
		{"missing cleanup id", func(request *protocol.KnowledgeCleanupRequest) { request.CleanupId = "" }},
		{"unspecified policy", func(request *protocol.KnowledgeCleanupRequest) {
			request.ReclaimPolicy = protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_UNSPECIFIED
		}},
		{"unknown policy", func(request *protocol.KnowledgeCleanupRequest) {
			request.ReclaimPolicy = protocol.KnowledgeReclaimPolicy(99)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := validKnowledgeCleanup()
			test.mutate(request)
			if status.Code(pluginsdk.ValidateKnowledgeCleanup(request)) != codes.InvalidArgument {
				t.Fatal("invalid cleanup request was accepted")
			}
		})
	}
	if err := pluginsdk.ValidateKnowledgeCleanup(validKnowledgeCleanup()); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeQueryScopeAndBounds(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*protocol.KnowledgeQueryRequest)
	}{
		{"missing scope", func(request *protocol.KnowledgeQueryRequest) { request.Scope = nil }},
		{"missing namespace", func(request *protocol.KnowledgeQueryRequest) { request.Scope.Namespace = "" }},
		{"missing uid", func(request *protocol.KnowledgeQueryRequest) { request.Scope.KnowledgeBaseUid = "" }},
		{"uncommitted generation", func(request *protocol.KnowledgeQueryRequest) { request.Scope.Generation = 0 }},
		{"missing configuration uid", func(request *protocol.KnowledgeQueryRequest) { request.Scope.ConfigurationUid = "" }},
		{"invalid digest", func(request *protocol.KnowledgeQueryRequest) { request.Scope.ConfigurationDigest = "stale" }},
		{"unbounded results", func(request *protocol.KnowledgeQueryRequest) { request.TopK = 101 }},
		{"scope override", func(request *protocol.KnowledgeQueryRequest) {
			request.Filters = []*protocol.KnowledgeFilter{{Key: "namespace", Value: "tenant-b"}}
		}},
		{"empty query", func(request *protocol.KnowledgeQueryRequest) { request.Text = " " }},
		{"unknown mode", func(request *protocol.KnowledgeQueryRequest) { request.Mode = "unknown" }},
		{"oversized text", func(request *protocol.KnowledgeQueryRequest) { request.Text = strings.Repeat("a", 65537) }},
		{"missing request id", func(request *protocol.KnowledgeQueryRequest) { request.RequestId = "" }},
		{"zero results", func(request *protocol.KnowledgeQueryRequest) { request.TopK = 0 }},
		{"dense without vector", func(request *protocol.KnowledgeQueryRequest) { request.Mode = "dense"; request.Text = "" }},
		{"hybrid without vector", func(request *protocol.KnowledgeQueryRequest) { request.Mode = "hybrid" }},
		{"NaN", func(request *protocol.KnowledgeQueryRequest) {
			request.Mode = "hybrid"
			request.Vector = []float64{math.NaN()}
		}},
		{"infinity", func(request *protocol.KnowledgeQueryRequest) {
			request.Mode = "hybrid"
			request.Vector = []float64{math.Inf(1)}
		}},
		{"oversized vector", func(request *protocol.KnowledgeQueryRequest) {
			request.Mode = "hybrid"
			request.Vector = make([]float64, 65537)
		}},
		{"nil filter", func(request *protocol.KnowledgeQueryRequest) { request.Filters = []*protocol.KnowledgeFilter{nil} }},
		{"duplicate filter", func(request *protocol.KnowledgeQueryRequest) {
			request.Filters = []*protocol.KnowledgeFilter{{Key: "metadata.tag"}, {Key: "metadata.tag"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := validKnowledgeQuery()
			test.mutate(request)
			if err := pluginsdk.ValidateKnowledgeQuery(request); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
		})
	}
	if err := pluginsdk.ValidateKnowledgeQuery(validKnowledgeQuery()); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"dense", "hybrid"} {
		request := validKnowledgeQuery()
		request.Mode = mode
		request.Vector = []float64{0.1, 0.2, 0.3}
		if mode == "dense" {
			request.Text = ""
		}
		if err := pluginsdk.ValidateKnowledgeQuery(request); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
	}
}

func TestKnowledgeIndexBatchScopeAndBounds(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*protocol.KnowledgeIndexBatchRequest)
		code   codes.Code
	}{
		{"missing scope", func(request *protocol.KnowledgeIndexBatchRequest) { request.Scope = nil }, codes.InvalidArgument},
		{"uncommitted generation", func(request *protocol.KnowledgeIndexBatchRequest) { request.Scope.Generation = 0 }, codes.InvalidArgument},
		{"missing operation", func(request *protocol.KnowledgeIndexBatchRequest) { request.OperationId = "" }, codes.InvalidArgument},
		{"invalid source digest", func(request *protocol.KnowledgeIndexBatchRequest) { request.SourceDigest = "sha256:abc" }, codes.InvalidArgument},
		{"negative sequence", func(request *protocol.KnowledgeIndexBatchRequest) { request.Sequence = -1 }, codes.InvalidArgument},
		{"empty batch", func(request *protocol.KnowledgeIndexBatchRequest) { request.Chunks = nil }, codes.InvalidArgument},
		{"too many chunks", func(request *protocol.KnowledgeIndexBatchRequest) {
			request.Chunks = make([]*protocol.KnowledgeIndexChunk, 101)
		}, codes.InvalidArgument},
		{"missing citation", func(request *protocol.KnowledgeIndexBatchRequest) { request.Chunks[0].SourceUri = "" }, codes.InvalidArgument},
		{"empty vector", func(request *protocol.KnowledgeIndexBatchRequest) { request.Chunks[0].Vector = nil }, codes.InvalidArgument},
		{"nonfinite vector", func(request *protocol.KnowledgeIndexBatchRequest) { request.Chunks[0].Vector[0] = math.Inf(1) }, codes.InvalidArgument},
		{"duplicate chunk", func(request *protocol.KnowledgeIndexBatchRequest) {
			request.Chunks = append(request.Chunks, request.Chunks[0])
		}, codes.InvalidArgument},
		{"oversized batch", func(request *protocol.KnowledgeIndexBatchRequest) {
			request.Chunks[0].Content = strings.Repeat("a", (4<<20)+1)
		}, codes.InvalidArgument},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := validKnowledgeIndexBatch()
			test.mutate(request)
			if err := pluginsdk.ValidateKnowledgeIndexBatch(request); status.Code(err) != test.code {
				t.Fatalf("code = %v, want %v: %v", status.Code(err), test.code, err)
			}
		})
	}
	if err := pluginsdk.ValidateKnowledgeIndexBatch(validKnowledgeIndexBatch()); err != nil {
		t.Fatal(err)
	}
}
