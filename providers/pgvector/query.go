// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	vector "github.com/pgvector/pgvector-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type Querier struct {
	pool       *pgxpool.Pool
	table      string
	dimensions int
	scope      *protocol.KnowledgeScope
}

var _ pluginsdk.KnowledgeQuerier = (*Querier)(nil)

func NewQuerier(pool *pgxpool.Pool, spec BackendSpec, scope *protocol.KnowledgeScope) (*Querier, error) {
	if pool == nil || spec.Validate() != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid pgvector query configuration")
	}
	probe := &protocol.KnowledgeQueryRequest{Scope: scope, RequestId: "scope-validation", Mode: "keyword", Text: "scope", TopK: 1}
	if pluginsdk.ValidateKnowledgeQuery(probe) != nil || scope.ConfigurationApiGroup != APIGroup || scope.ConfigurationKind != Kind {
		return nil, status.Error(codes.InvalidArgument, "invalid pgvector query binding")
	}
	return &Querier{pool: pool, table: pgx.Identifier{"public", spec.Table}.Sanitize(), dimensions: int(spec.EmbeddingDimensions), scope: proto.CloneOf(scope)}, nil
}

func (querier *Querier) Query(ctx context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
	if err := pluginsdk.ValidateKnowledgeQuery(request); err != nil {
		return nil, err
	}
	if !proto.Equal(request.Scope, querier.scope) {
		return nil, status.Error(codes.PermissionDenied, "pgvector query binding mismatch")
	}
	if request.Mode != "dense" {
		return nil, status.Error(codes.Unimplemented, "pgvector query mode is not implemented")
	}
	values, err := preparedVector(request.Vector, querier.dimensions)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return nil, queryError(ctx)
	}
	arguments := []any{querier.scope.Namespace, querier.scope.KnowledgeBaseUid, querier.scope.Generation, querier.scope.ConfigurationUid, querier.scope.ConfigurationDigest, values.String()}
	predicate := "namespace = $1 AND knowledge_base_uid = $2 AND generation = $3 AND configuration_uid = $4 AND configuration_digest = $5"
	for _, filter := range request.Filters {
		arguments = append(arguments, strings.TrimPrefix(filter.Key, "metadata."), filter.Value)
		predicate += fmt.Sprintf(" AND metadata ->> $%d::text = $%d::text", len(arguments)-1, len(arguments))
	}
	arguments = append(arguments, request.TopK)
	statement := fmt.Sprintf(`SELECT left(document_id, 1025), left(chunk_id, 1025), left(content, 65537),
		left(source_uri, 4097), left(location, 4097),
		1 - (embedding OPERATOR(public.<=>) $6::public.vector) AS score
		FROM %s WHERE %s AND embedding IS NOT NULL
		ORDER BY embedding OPERATOR(public.<=>) $6::public.vector, document_id, chunk_id LIMIT $%d`, querier.table, predicate, len(arguments))
	rows, err := querier.pool.Query(ctx, statement, arguments...)
	if err != nil {
		return nil, queryError(ctx)
	}
	defer rows.Close()
	response := &protocol.KnowledgeQueryResponse{Scope: proto.CloneOf(querier.scope), RequestId: request.RequestId}
	seen := make(map[[2]string]bool)
	for rows.Next() {
		match := &protocol.KnowledgeMatch{}
		if err := rows.Scan(&match.DocumentId, &match.ChunkId, &match.Content, &match.SourceUri, &match.Location, &match.Score); err != nil {
			return nil, queryError(ctx)
		}
		identity := [2]string{match.DocumentId, match.ChunkId}
		if !validMatch(match) || seen[identity] {
			return nil, status.Error(codes.DataLoss, "invalid pgvector citation data")
		}
		seen[identity] = true
		response.Matches = append(response.Matches, match)
		if proto.Size(response) > 4<<20 {
			return nil, status.Error(codes.ResourceExhausted, "pgvector response exceeds message limit")
		}
	}
	if rows.Err() != nil || ctx.Err() != nil {
		return nil, queryError(ctx)
	}
	return response, nil
}

func validMatch(match *protocol.KnowledgeMatch) bool {
	if match.DocumentId == "" || len(match.DocumentId) > 1024 || match.ChunkId == "" || len(match.ChunkId) > 1024 || len(match.Content) > 65536 || len(match.Location) > 4096 || len(match.SourceUri) > 4096 || math.IsNaN(match.Score) || math.IsInf(match.Score, 0) {
		return false
	}
	source, err := url.Parse(match.SourceUri)
	return err == nil && source.User == nil && source.RawQuery == "" && !source.ForceQuery && source.Fragment == "" && ((source.Scheme == "https" || source.Scheme == "http") && source.Host != "" || source.Scheme == "urn" && source.Opaque != "")
}

func preparedVector(input []float64, dimensions int) (vector.Vector, error) {
	if len(input) != dimensions {
		return vector.Vector{}, status.Error(codes.InvalidArgument, "pgvector vector dimension mismatch")
	}
	values := make([]float32, len(input))
	var squaredNorm float32
	for index, value := range input {
		values[index] = float32(value)
		if math.IsNaN(value) || math.IsInf(float64(values[index]), 0) {
			return vector.Vector{}, status.Error(codes.InvalidArgument, "pgvector vector exceeds precision bounds")
		}
		squaredNorm += values[index] * values[index]
	}
	if squaredNorm == 0 || math.IsInf(float64(squaredNorm), 0) {
		return vector.Vector{}, status.Error(codes.InvalidArgument, "pgvector cosine vector norm exceeds precision bounds")
	}
	return vector.NewVector(values), nil
}

func queryError(ctx context.Context) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	return status.Error(codes.Unavailable, "pgvector query failed")
}
