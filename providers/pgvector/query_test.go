// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector_test

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"github.com/kognition-ai/kognition/providers/pgvector"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func queryRequest() *protocol.KnowledgeQueryRequest {
	return &protocol.KnowledgeQueryRequest{Scope: &protocol.KnowledgeScope{Namespace: "tenant-a", KnowledgeBaseUid: "kb-a", Generation: 2, OperationId: "index-2", ConfigurationApiGroup: pgvector.APIGroup, ConfigurationKind: pgvector.Kind, ConfigurationName: "shared", ConfigurationUid: "config-a", ConfigurationDigest: strings.Repeat("a", 64)}, RequestId: "query-1", Mode: "dense", Vector: []float64{1, 0, 0}, TopK: 3}
}

func testQuerier(t *testing.T, pool *pgxpool.Pool) *pgvector.Querier {
	t.Helper()
	spec, err := pgvector.DecodeBackend(validBackend())
	if err != nil {
		t.Fatal(err)
	}
	spec.EmbeddingDimensions = 3
	spec.Table = "knowledge_chunks"
	scope := queryRequest().Scope
	querier, err := pgvector.NewQuerier(pool, spec, scope)
	if err != nil {
		t.Fatal(err)
	}
	scope.Namespace = "changed-after-construction"
	return querier
}

func TestQuerierRejectsInvalidRequestsBeforeDatabaseAccess(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://unused:private-value@127.0.0.1:1/unused?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	querier := testQuerier(t, pool)
	for name, mutate := range map[string]func(*protocol.KnowledgeQueryRequest){
		"namespace":     func(request *protocol.KnowledgeQueryRequest) { request.Scope.Namespace = "tenant-b" },
		"KB":            func(request *protocol.KnowledgeQueryRequest) { request.Scope.KnowledgeBaseUid = "kb-b" },
		"generation":    func(request *protocol.KnowledgeQueryRequest) { request.Scope.Generation++ },
		"operation":     func(request *protocol.KnowledgeQueryRequest) { request.Scope.OperationId = "other-index" },
		"configuration": func(request *protocol.KnowledgeQueryRequest) { request.Scope.ConfigurationUid = "config-b" },
		"digest": func(request *protocol.KnowledgeQueryRequest) {
			request.Scope.ConfigurationDigest = strings.Repeat("b", 64)
		},
		"name": func(request *protocol.KnowledgeQueryRequest) { request.Scope.ConfigurationName = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			request := queryRequest()
			mutate(request)
			_, err := querier.Query(context.Background(), request)
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("expected binding rejection, got %v", err)
			}
		})
	}
	for _, values := range [][]float64{{1}, {0, 0, 0}, {math.MaxFloat64, 0, 0}, {math.SmallestNonzeroFloat64, 0, 0}, {1e30, 0, 0}, {1e-30, 0, 0}, {math.NaN(), 0, 0}} {
		request := queryRequest()
		request.Vector = values
		if _, err := querier.Query(context.Background(), request); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("invalid vector: %v", err)
		}
	}
	request := queryRequest()
	request.Mode = "keyword"
	request.Text = "example"
	request.Vector = nil
	if _, err := querier.Query(context.Background(), request); status.Code(err) != codes.Unimplemented {
		t.Fatalf("unsupported mode: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := querier.Query(ctx, queryRequest()); status.Code(err) != codes.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
	request = queryRequest()
	original := proto.Clone(request)
	if _, err := querier.Query(context.Background(), request); status.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "private-value") {
		t.Fatalf("unsanitized database failure: %v", err)
	}
	if !proto.Equal(request, original) {
		t.Fatal("query mutated caller request")
	}
}

func TestNewQuerierRejectsInvalidBindingAndConfiguration(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://unused@127.0.0.1:1/unused?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	spec, err := pgvector.DecodeBackend(validBackend())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pgvector.NewQuerier(nil, spec, queryRequest().Scope); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil pool: %v", err)
	}
	if _, err := pgvector.NewQuerier(pool, spec, nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil scope: %v", err)
	}
	for _, mutate := range []func(*protocol.KnowledgeScope){
		func(scope *protocol.KnowledgeScope) { scope.ConfigurationApiGroup = "other.example.org" },
		func(scope *protocol.KnowledgeScope) { scope.ConfigurationKind = "OtherBackend" },
		func(scope *protocol.KnowledgeScope) { scope.ConfigurationDigest = "invalid" },
	} {
		scope := queryRequest().Scope
		mutate(scope)
		if _, err := pgvector.NewQuerier(pool, spec, scope); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("invalid scope: %v", err)
		}
	}
	spec.Table = "chunks; DROP TABLE chunks"
	if _, err := pgvector.NewQuerier(pool, spec, queryRequest().Scope); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid table: %v", err)
	}
}
