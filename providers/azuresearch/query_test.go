// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuresearch_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"github.com/kognition-ai/kognition/providers/azuresearch"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type searcherFunc func(context.Context, azuresearch.SearchRequest) ([]azuresearch.SearchDocument, error)

func (fn searcherFunc) Search(ctx context.Context, request azuresearch.SearchRequest) ([]azuresearch.SearchDocument, error) {
	return fn(ctx, request)
}

func validScope() *protocol.KnowledgeScope {
	return &protocol.KnowledgeScope{
		Namespace: "tenant-a", KnowledgeBaseUid: "kb'uid", Generation: 2,
		OperationId:           "operation'1",
		ConfigurationApiGroup: azuresearch.APIGroup, ConfigurationKind: azuresearch.Kind,
		ConfigurationName: "shared", ConfigurationUid: "config'uid",
		ConfigurationDigest: strings.Repeat("a", 64),
	}
}

func validAzureSearchSpec() azuresearch.BackendSpec {
	return azuresearch.BackendSpec{ServiceName: "kognition-search", IndexName: "knowledge_chunks", EmbeddingDimensions: 3, Authentication: azuresearch.AuthenticationConfig{Mode: "workload-identity"}}
}

func TestQuerierBuildsModeSpecificScopedRequests(t *testing.T) {
	tests := []struct {
		name  string
		query *protocol.KnowledgeQueryRequest
		check func(*testing.T, azuresearch.SearchRequest)
	}{
		{name: "keyword", query: &protocol.KnowledgeQueryRequest{Mode: "keyword", Text: "find this"}, check: func(t *testing.T, input azuresearch.SearchRequest) {
			if input.Search != "find this" || len(input.VectorQueries) != 0 {
				t.Fatalf("unexpected keyword input: %+v", input)
			}
		}},
		{name: "dense", query: &protocol.KnowledgeQueryRequest{Mode: "dense", Vector: []float64{0.1, 0.2, 0.3}}, check: func(t *testing.T, input azuresearch.SearchRequest) {
			if input.Search != "" || len(input.VectorQueries) != 1 || input.VectorQueries[0].Fields != "embedding" || input.VectorQueries[0].K != 4 {
				t.Fatalf("unexpected dense input: %+v", input)
			}
		}},
		{name: "hybrid", query: &protocol.KnowledgeQueryRequest{Mode: "hybrid", Text: "find this", Vector: []float64{0.1, 0.2, 0.3}}, check: func(t *testing.T, input azuresearch.SearchRequest) {
			if input.Search != "find this" || len(input.VectorQueries) != 1 {
				t.Fatalf("unexpected hybrid input: %+v", input)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := searcherFunc(func(_ context.Context, input azuresearch.SearchRequest) ([]azuresearch.SearchDocument, error) {
				test.check(t, input)
				if input.Top != 4 || input.Select != "documentId,chunkId,content,sourceUri,location" {
					t.Fatalf("unexpected common input: %+v", input)
				}
				for _, fragment := range []string{"namespace eq 'tenant-a'", "knowledgeBaseUid eq 'kb''uid'", "generation eq 2", "operationId eq 'operation''1'", "configurationUid eq 'config''uid'", "configurationDigest eq '" + strings.Repeat("a", 64) + "'", "metadataFilters/any(item0: item0 eq 'author=O''Brien')"} {
					if !strings.Contains(input.Filter, fragment) {
						t.Fatalf("filter %q does not contain %q", input.Filter, fragment)
					}
				}
				return []azuresearch.SearchDocument{{Score: 0.75, DocumentID: "doc", ChunkID: "chunk", Content: "body", SourceURI: "https://example.test/doc", Location: "p1"}}, nil
			})
			querier, err := azuresearch.NewQuerier(client, validAzureSearchSpec(), validScope())
			if err != nil {
				t.Fatal(err)
			}
			test.query.Scope = validScope()
			test.query.RequestId = "request-1"
			test.query.TopK = 4
			test.query.Filters = []*protocol.KnowledgeFilter{{Key: "metadata.author", Value: "O'Brien"}}
			response, err := querier.Query(context.Background(), test.query)
			if err != nil {
				t.Fatal(err)
			}
			if response.RequestId != "request-1" || !proto.Equal(response.Scope, validScope()) || len(response.Matches) != 1 || response.Matches[0].DocumentId != "doc" {
				t.Fatalf("unexpected response: %+v", response)
			}
		})
	}
}

func TestQuerierRejectsBindingDimensionsAndInvalidResults(t *testing.T) {
	client := searcherFunc(func(_ context.Context, _ azuresearch.SearchRequest) ([]azuresearch.SearchDocument, error) {
		return []azuresearch.SearchDocument{
			{Score: 1, DocumentID: "doc", ChunkID: "chunk", SourceURI: "https://example.test/doc"},
			{Score: 0.5, DocumentID: "doc", ChunkID: "chunk", SourceURI: "https://example.test/doc"},
		}, nil
	})
	querier, err := azuresearch.NewQuerier(client, validAzureSearchSpec(), validScope())
	if err != nil {
		t.Fatal(err)
	}
	mismatched := validScope()
	mismatched.Generation++
	if _, err := querier.Query(context.Background(), &protocol.KnowledgeQueryRequest{Scope: mismatched, RequestId: "request", Mode: "keyword", Text: "query", TopK: 2}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unexpected binding error: %v", err)
	}
	if _, err := querier.Query(context.Background(), &protocol.KnowledgeQueryRequest{Scope: validScope(), RequestId: "request", Mode: "dense", Vector: []float64{1, 2}, TopK: 2}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unexpected dimension error: %v", err)
	}
	if _, err := querier.Query(context.Background(), &protocol.KnowledgeQueryRequest{Scope: validScope(), RequestId: "request", Mode: "keyword", Text: "query", TopK: 2}); status.Code(err) != codes.DataLoss {
		t.Fatalf("unexpected citation error: %v", err)
	}
}

func TestQuerierMapsSearchFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code codes.Code
	}{
		{name: "unauthenticated", err: &azuresearch.ServiceError{StatusCode: http.StatusUnauthorized}, code: codes.Unauthenticated},
		{name: "permission denied", err: &azuresearch.ServiceError{StatusCode: http.StatusForbidden}, code: codes.PermissionDenied},
		{name: "not found", err: &azuresearch.ServiceError{StatusCode: http.StatusNotFound}, code: codes.NotFound},
		{name: "throttled", err: &azuresearch.ServiceError{StatusCode: http.StatusTooManyRequests}, code: codes.ResourceExhausted},
		{name: "invalid response", err: &azuresearch.ResponseError{}, code: codes.DataLoss},
		{name: "transport", err: errors.New("private transport detail"), code: codes.Unavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := searcherFunc(func(_ context.Context, _ azuresearch.SearchRequest) ([]azuresearch.SearchDocument, error) {
				return nil, test.err
			})
			querier, err := azuresearch.NewQuerier(client, validAzureSearchSpec(), validScope())
			if err != nil {
				t.Fatal(err)
			}
			_, err = querier.Query(context.Background(), &protocol.KnowledgeQueryRequest{Scope: validScope(), RequestId: "request", Mode: "keyword", Text: "query", TopK: 1})
			if status.Code(err) != test.code || strings.Contains(err.Error(), "private transport detail") {
				t.Fatalf("unexpected mapped error: %v", err)
			}
		})
	}
}
