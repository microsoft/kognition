// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuresearch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const searchSelectFields = "documentId,chunkId,content,sourceUri,location"

type Searcher interface {
	Search(context.Context, SearchRequest) ([]SearchDocument, error)
}

type Querier struct {
	client     Searcher
	dimensions int
	scope      *protocol.KnowledgeScope
}

var _ pluginsdk.KnowledgeQuerier = (*Querier)(nil)

func NewQuerier(client Searcher, spec BackendSpec, scope *protocol.KnowledgeScope) (*Querier, error) {
	if client == nil || spec.Validate() != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid azure search query configuration")
	}
	probe := &protocol.KnowledgeQueryRequest{Scope: scope, RequestId: "scope-validation", Mode: "keyword", Text: "scope", TopK: 1}
	if pluginsdk.ValidateKnowledgeQuery(probe) != nil || scope.ConfigurationApiGroup != APIGroup || scope.ConfigurationKind != Kind {
		return nil, status.Error(codes.InvalidArgument, "invalid azure search query binding")
	}
	clonedScope, ok := proto.Clone(scope).(*protocol.KnowledgeScope)
	if !ok {
		return nil, status.Error(codes.Internal, "clone azure search query scope")
	}
	return &Querier{client: client, dimensions: int(spec.EmbeddingDimensions), scope: clonedScope}, nil
}

func (querier *Querier) Query(ctx context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
	if err := pluginsdk.ValidateKnowledgeQuery(request); err != nil {
		return nil, err
	}
	if !proto.Equal(request.Scope, querier.scope) {
		return nil, status.Error(codes.PermissionDenied, "azure search query binding mismatch")
	}
	input := SearchRequest{Filter: searchFilter(querier.scope, request.Filters), Select: searchSelectFields, Top: request.TopK}
	switch request.Mode {
	case "keyword":
		input.Search = request.Text
	case "dense", "hybrid":
		if len(request.Vector) != querier.dimensions {
			return nil, status.Error(codes.InvalidArgument, "azure search query dimension mismatch")
		}
		var squaredNorm float32
		for _, value := range request.Vector {
			converted := float32(value)
			if math.IsInf(float64(converted), 0) {
				return nil, status.Error(codes.InvalidArgument, "azure search query exceeds vector precision")
			}
			squaredNorm += converted * converted
		}
		if squaredNorm == 0 || math.IsInf(float64(squaredNorm), 0) {
			return nil, status.Error(codes.InvalidArgument, "azure search cosine query norm exceeds vector precision")
		}
		input.VectorQueries = []VectorQuery{{Kind: "vector", Vector: request.Vector, Fields: "embedding", K: request.TopK}}
		if request.Mode == "hybrid" {
			input.Search = request.Text
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return nil, searchError(ctx, nil)
	}
	documents, err := querier.client.Search(ctx, input)
	if err != nil {
		return nil, searchError(ctx, err)
	}
	responseScope, ok := proto.Clone(querier.scope).(*protocol.KnowledgeScope)
	if !ok {
		return nil, status.Error(codes.Internal, "clone azure search response scope")
	}
	response := &protocol.KnowledgeQueryResponse{Scope: responseScope, RequestId: request.RequestId}
	seen := make(map[[2]string]bool)
	for _, document := range documents {
		match := &protocol.KnowledgeMatch{DocumentId: document.DocumentID, ChunkId: document.ChunkID, Content: document.Content, Score: document.Score, SourceUri: document.SourceURI, Location: document.Location}
		identity := [2]string{match.DocumentId, match.ChunkId}
		if !validSearchMatch(match) || seen[identity] {
			return nil, status.Error(codes.DataLoss, "invalid azure search citation data")
		}
		seen[identity] = true
		response.Matches = append(response.Matches, match)
		if proto.Size(response) > 4<<20 {
			return nil, status.Error(codes.ResourceExhausted, "azure search response exceeds message limit")
		}
	}
	if len(response.Matches) > int(request.TopK) {
		return nil, status.Error(codes.DataLoss, "azure search returned too many matches")
	}
	return response, nil
}

func searchFilter(scope *protocol.KnowledgeScope, filters []*protocol.KnowledgeFilter) string {
	clauses := []string{
		"namespace eq '" + odataString(scope.Namespace) + "'",
		"knowledgeBaseUid eq '" + odataString(scope.KnowledgeBaseUid) + "'",
		"generation eq " + strconv.FormatInt(scope.Generation, 10),
		"operationId eq '" + odataString(scope.OperationId) + "'",
		"configurationUid eq '" + odataString(scope.ConfigurationUid) + "'",
		"configurationDigest eq '" + odataString(scope.ConfigurationDigest) + "'",
	}
	for index, filter := range filters {
		pair := strings.TrimPrefix(filter.Key, "metadata.") + "=" + filter.Value
		clauses = append(clauses, fmt.Sprintf("metadataFilters/any(item%d: item%d eq '%s')", index, index, odataString(pair)))
	}
	return strings.Join(clauses, " and ")
}

func odataString(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func validSearchMatch(match *protocol.KnowledgeMatch) bool {
	if match.DocumentId == "" || len(match.DocumentId) > 1024 || match.ChunkId == "" || len(match.ChunkId) > 1024 || len(match.Content) > 65536 || len(match.Location) > 4096 || len(match.SourceUri) > 4096 || math.IsNaN(match.Score) || math.IsInf(match.Score, 0) {
		return false
	}
	source, err := url.Parse(match.SourceUri)
	return err == nil && source.User == nil && source.RawQuery == "" && !source.ForceQuery && source.Fragment == "" && ((source.Scheme == "https" || source.Scheme == "http") && source.Host != "" || source.Scheme == "urn" && source.Opaque != "")
}

func searchError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	var serviceError *ServiceError
	if errors.As(err, &serviceError) {
		switch serviceError.StatusCode {
		case http.StatusUnauthorized:
			return status.Error(codes.Unauthenticated, "azure search authentication failed")
		case http.StatusForbidden:
			return status.Error(codes.PermissionDenied, "azure search access denied")
		case http.StatusNotFound:
			return status.Error(codes.NotFound, "azure search resource not found")
		case http.StatusTooManyRequests:
			return status.Error(codes.ResourceExhausted, "azure search request throttled")
		}
	}
	var responseError *ResponseError
	if errors.As(err, &responseError) {
		return status.Error(codes.DataLoss, "invalid azure search response")
	}
	return status.Error(codes.Unavailable, "azure search query failed")
}
