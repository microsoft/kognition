// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	pgvectorprovider "github.com/kognition-ai/kognition/providers/pgvector"
)

// pgvectorQuerier is the pluginsdk.KnowledgeQuerier installed on the sidecar's
// gRPC server. It caches at most one *pgvectorprovider.Querier per fully-
// resolved scope: because the scope is validated against the committed
// BackendBinding before this code runs, the (configuration UID, digest) tuple
// pins both the pool topology and the schema the querier expects.
//
// The cache is cold-started per unique scope and lives for the process
// lifetime. Rotating credentials or re-materializing a backend requires the
// KnowledgeBase generation to change (which produces a new scope key). Close
// tears down every pool and refuses further cache reads.
type pgvectorQuerier struct {
	reader          client.Reader
	logger          *zap.Logger
	mu              sync.Mutex
	entries         map[string]*querierEntry
	closed          bool
	allowedDatabase string
}

type querierEntry struct {
	pool      *pgxpool.Pool
	querier   *pgvectorprovider.Querier
	lifecycle *pgvectorprovider.Lifecycle
}

var _ pluginsdk.KnowledgeQuerier = (*pgvectorQuerier)(nil)
var _ pluginsdk.KnowledgeIndexer = (*pgvectorQuerier)(nil)
var _ pluginsdk.KnowledgeCleaner = (*pgvectorQuerier)(nil)

func newPgvectorQuerier(reader client.Reader, logger *zap.Logger, databaseAddress ...string) *pgvectorQuerier {
	allowed := ""
	if len(databaseAddress) == 1 {
		allowed = databaseAddress[0]
	}
	return &pgvectorQuerier{
		reader:          reader,
		logger:          logger,
		entries:         make(map[string]*querierEntry),
		allowedDatabase: allowed,
	}
}

// Query implements pluginsdk.KnowledgeQuerier. The scope has already been
// validated by pluginsdk (structural) and by scopeAuthorizer (against the
// committed binding), so this method only has to resolve or build the per-
// scope pool and delegate.
func (q *pgvectorQuerier) Query(ctx context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
	if request == nil || request.Scope == nil {
		return nil, status.Error(codes.InvalidArgument, "knowledge query is required")
	}
	entry, err := q.acquire(ctx, request.Scope)
	if err != nil {
		return nil, err
	}
	return entry.querier.Query(ctx, request)
}

func (q *pgvectorQuerier) BeginIndex(ctx context.Context, scope *protocol.KnowledgeScope, operationID, sourceDigest string) (pluginsdk.KnowledgeIndexTransaction, error) {
	if scope == nil {
		return nil, status.Error(codes.InvalidArgument, "knowledge index scope is required")
	}
	entry, err := q.acquire(ctx, scope)
	if err != nil {
		return nil, err
	}
	return entry.lifecycle.BeginIndex(ctx, scope, operationID, sourceDigest)
}

func (q *pgvectorQuerier) Cleanup(ctx context.Context, request *protocol.KnowledgeCleanupRequest) (*protocol.KnowledgeCleanupResponse, error) {
	if err := pluginsdk.ValidateKnowledgeCleanup(request); err != nil {
		return nil, err
	}
	spec, err := pgvectorprovider.DecodeBackendOperation(request.BackendOperationId, request.Scope)
	if err != nil {
		return nil, err
	}
	if !q.databaseAllowed(spec) {
		return nil, status.Error(codes.PermissionDenied, "pgvector cleanup database route is not approved")
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, status.Error(codes.Internal, "cannot restore pgvector cleanup configuration")
	}
	var values map[string]any
	if err := json.Unmarshal(encoded, &values); err != nil {
		return nil, status.Error(codes.Internal, "cannot restore pgvector cleanup configuration")
	}
	configuration := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": pgvectorprovider.APIGroup + "/" + pgvectorprovider.APIVersion,
		"kind":       pgvectorprovider.Kind,
		"metadata":   map[string]any{"namespace": request.Scope.Namespace, "name": request.Scope.ConfigurationName, "uid": request.Scope.ConfigurationUid},
		"spec":       values,
	}}
	material, err := pgvectorprovider.LoadConnectionMaterial(ctx, q.reader, configuration)
	if err != nil {
		return nil, err
	}
	pool, err := pgvectorprovider.NewPool(ctx, spec, material, pgvectorprovider.PoolBounds{})
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	lifecycle, err := pgvectorprovider.NewLifecycle(pool, spec)
	if err != nil {
		return nil, err
	}
	return lifecycle.Cleanup(ctx, request)
}

// Close releases every cached pool. It is safe to call multiple times.
func (q *pgvectorQuerier) Close() {
	q.mu.Lock()
	entries := q.entries
	q.entries = make(map[string]*querierEntry)
	q.closed = true
	q.mu.Unlock()
	for _, entry := range entries {
		entry.pool.Close()
	}
}

func (q *pgvectorQuerier) acquire(ctx context.Context, scope *protocol.KnowledgeScope) (*querierEntry, error) {
	key := scopeKey(scope)
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil, status.Error(codes.Unavailable, "pgvector knowledge adapter is shutting down")
	}
	if entry, ok := q.entries[key]; ok {
		q.mu.Unlock()
		return entry, nil
	}
	q.mu.Unlock()

	entry, err := q.build(ctx, scope)
	if err != nil {
		return nil, err
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		entry.pool.Close()
		return nil, status.Error(codes.Unavailable, "pgvector knowledge adapter is shutting down")
	}
	if existing, ok := q.entries[key]; ok {
		entry.pool.Close()
		return existing, nil
	}
	q.entries[key] = entry
	q.logger.Info("pgvector query pool materialized",
		zap.String("namespace", scope.Namespace),
		zap.String("configurationName", scope.ConfigurationName),
		zap.String("configurationUID", scope.ConfigurationUid))
	return entry, nil
}

func (q *pgvectorQuerier) build(ctx context.Context, scope *protocol.KnowledgeScope) (*querierEntry, error) {
	if scope.ConfigurationApiGroup != pgvectorprovider.APIGroup || scope.ConfigurationKind != pgvectorprovider.Kind {
		return nil, status.Error(codes.FailedPrecondition, "pgvector adapter cannot serve non-pgvector configuration")
	}
	configuration := &unstructured.Unstructured{}
	configuration.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   pgvectorprovider.APIGroup,
		Version: pgvectorprovider.APIVersion,
		Kind:    pgvectorprovider.Kind,
	})
	if err := q.reader.Get(ctx, types.NamespacedName{Namespace: scope.Namespace, Name: scope.ConfigurationName}, configuration); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "pgvector configuration is unavailable")
	}
	digest, err := knowledge.ConfigurationDigest(configuration)
	if err != nil || string(configuration.GetUID()) != scope.ConfigurationUid || digest != scope.ConfigurationDigest {
		return nil, status.Error(codes.FailedPrecondition, "pgvector configuration changed before materialization")
	}
	spec, err := pgvectorprovider.DecodeBackend(configuration)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "pgvector configuration is invalid")
	}
	if !q.databaseAllowed(spec) {
		return nil, status.Error(codes.PermissionDenied, "pgvector database route is not approved")
	}
	material, err := pgvectorprovider.LoadConnectionMaterial(ctx, q.reader, configuration)
	if err != nil {
		return nil, err
	}
	pool, err := pgvectorprovider.NewPool(ctx, spec, material, pgvectorprovider.PoolBounds{})
	if err != nil {
		return nil, err
	}
	lifecycle, err := pgvectorprovider.NewLifecycle(pool, spec)
	if err != nil {
		pool.Close()
		return nil, err
	}
	var querier *pgvectorprovider.Querier
	if scope.OperationId != "" {
		querier, err = pgvectorprovider.NewQuerier(pool, spec, scope)
		if err != nil {
			pool.Close()
			return nil, err
		}
	}
	return &querierEntry{pool: pool, querier: querier, lifecycle: lifecycle}, nil
}

func (q *pgvectorQuerier) databaseAllowed(spec pgvectorprovider.BackendSpec) bool {
	return q.allowedDatabase != "" && net.JoinHostPort(spec.Host, strconv.Itoa(int(spec.Port))) == q.allowedDatabase
}

// scopeKey produces a canonical cache key that changes whenever any scope
// field changes. Any change requires a fresh pool because either the schema
// (namespace, configuration UID/digest) or the tenant identity (KnowledgeBase
// UID/generation) has moved.
func scopeKey(scope *protocol.KnowledgeScope) string {
	return strings.Join([]string{
		scope.Namespace,
		scope.KnowledgeBaseUid,
		strconv.FormatInt(scope.Generation, 10),
		scope.ConfigurationApiGroup,
		scope.ConfigurationKind,
		scope.ConfigurationName,
		scope.ConfigurationUid,
		scope.ConfigurationDigest,
		scope.OperationId,
	}, "|")
}
