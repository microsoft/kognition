// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const operationTable = "public.kognition_knowledge_operations"

type Lifecycle struct {
	pool *pgxpool.Pool
	spec BackendSpec
}

var _ pluginsdk.KnowledgeIndexer = (*Lifecycle)(nil)
var _ pluginsdk.KnowledgeCleaner = (*Lifecycle)(nil)

func NewLifecycle(pool *pgxpool.Pool, spec BackendSpec) (*Lifecycle, error) {
	if pool == nil || spec.Validate() != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid pgvector lifecycle configuration")
	}
	if spec.TLS.CASecretRef != nil {
		reference := *spec.TLS.CASecretRef
		spec.TLS.CASecretRef = &reference
	}
	return &Lifecycle{pool: pool, spec: spec}, nil
}

// SchemaSQL is applied by a reviewed database migration, not by the adapter role.
func SchemaSQL(spec BackendSpec) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
 namespace text NOT NULL, knowledge_base_uid text NOT NULL, generation bigint NOT NULL CHECK (generation > 0),
 configuration_uid text NOT NULL, configuration_digest text NOT NULL,
 document_id text NOT NULL, chunk_id text NOT NULL, content text NOT NULL, source_uri text NOT NULL,
 location text NOT NULL, metadata jsonb NOT NULL DEFAULT '{}', embedding public.vector(%d) NOT NULL,
 PRIMARY KEY(namespace, knowledge_base_uid, generation, configuration_uid, configuration_digest, document_id, chunk_id));
CREATE TABLE IF NOT EXISTS %s (
 table_name text NOT NULL, namespace text NOT NULL, knowledge_base_uid text NOT NULL, generation bigint NOT NULL,
 configuration_uid text NOT NULL, configuration_digest text NOT NULL, operation_id text NOT NULL,
 source_digest text NOT NULL, content_digest text NOT NULL, document_count bigint NOT NULL,
 chunk_count bigint NOT NULL, deleted boolean NOT NULL DEFAULT false,
 PRIMARY KEY(table_name, namespace, knowledge_base_uid, generation, configuration_uid, configuration_digest));`,
		pgx.Identifier{"public", spec.Table}.Sanitize(), spec.EmbeddingDimensions, operationTable), nil
}

type operationReceipt struct {
	operationID   string
	sourceDigest  string
	contentDigest string
	documents     int64
	chunks        int64
	deleted       bool
}

type indexTransaction struct {
	tx               pgx.Tx
	context          context.Context
	cancel           context.CancelFunc
	spec             BackendSpec
	scope            *protocol.KnowledgeScope
	operationID      string
	sourceDigest     string
	backendOperation string
	receipt          *operationReceipt
	documents        map[string]bool
	chunks           map[[2]string]bool
	digest           hash.Hash
	sequence         int32
	finished         bool
}

func (lifecycle *Lifecycle) BeginIndex(ctx context.Context, scope *protocol.KnowledgeScope, operationID, sourceDigest string) (pluginsdk.KnowledgeIndexTransaction, error) {
	probe := &protocol.KnowledgeIndexBatchRequest{Scope: scope, OperationId: operationID, SourceDigest: sourceDigest, Chunks: []*protocol.KnowledgeIndexChunk{{DocumentId: "validation", ChunkId: "validation", Content: "validation", SourceUri: "urn:kognition:validation", Vector: []float64{1}}}}
	if pluginsdk.ValidateKnowledgeIndexBatch(probe) != nil || scope.ConfigurationApiGroup != APIGroup || scope.ConfigurationKind != Kind || (scope.OperationId != "" && scope.OperationId != operationID) {
		return nil, status.Error(codes.InvalidArgument, "invalid pgvector index binding")
	}
	backendOperation, err := encodeBackendOperation(lifecycle.spec, scope, operationID)
	if err != nil {
		return nil, err
	}
	operationContext, cancel := context.WithTimeout(ctx, 5*time.Minute)
	tx, err := lifecycle.pool.Begin(operationContext)
	if err != nil {
		failure := queryError(operationContext)
		cancel()
		return nil, failure
	}
	transaction := &indexTransaction{
		tx: tx, context: operationContext, cancel: cancel, spec: lifecycle.spec, scope: proto.Clone(scope).(*protocol.KnowledgeScope),
		operationID: operationID, sourceDigest: sourceDigest, backendOperation: backendOperation,
		documents: make(map[string]bool), chunks: make(map[[2]string]bool), digest: sha256.New(),
	}
	if err := lockOperation(operationContext, tx, lifecycle.spec, scope); err != nil {
		return nil, errors.Join(err, transaction.Abort(context.Background()))
	}
	receipt, err := readReceipt(operationContext, tx, lifecycle.spec, scope)
	if err != nil {
		return nil, errors.Join(err, transaction.Abort(context.Background()))
	}
	if receipt != nil && (receipt.deleted || receipt.operationID != operationID || receipt.sourceDigest != sourceDigest) {
		return nil, errors.Join(status.Error(codes.FailedPrecondition, "pgvector generation belongs to another or retired operation"), transaction.Abort(context.Background()))
	}
	transaction.receipt = receipt
	if receipt == nil {
		var count int64
		if err := tx.QueryRow(operationContext, "SELECT count(*) FROM "+pgx.Identifier{"public", lifecycle.spec.Table}.Sanitize()+" WHERE "+dataPredicate, dataArguments(scope)...).Scan(&count); err != nil {
			return nil, errors.Join(queryError(operationContext), transaction.Abort(context.Background()))
		}
		if count != 0 {
			return nil, errors.Join(status.Error(codes.FailedPrecondition, "pgvector generation has no trusted operation receipt"), transaction.Abort(context.Background()))
		}
	}
	return transaction, nil
}

func (transaction *indexTransaction) WriteBatch(ctx context.Context, request *protocol.KnowledgeIndexBatchRequest) error {
	if transaction.finished {
		return status.Error(codes.FailedPrecondition, "pgvector index transaction is closed")
	}
	if pluginsdk.ValidateKnowledgeIndexBatch(request) != nil || !proto.Equal(request.Scope, transaction.scope) ||
		request.OperationId != transaction.operationID || request.SourceDigest != transaction.sourceDigest || request.Sequence != transaction.sequence {
		return status.Error(codes.InvalidArgument, "pgvector index batch binding mismatch")
	}
	ctx, cancel := transaction.requestContext(ctx)
	defer cancel()
	for _, chunk := range request.Chunks {
		if transaction.chunks[[2]string{chunk.DocumentId, chunk.ChunkId}] ||
			!validMatch(&protocol.KnowledgeMatch{DocumentId: chunk.DocumentId, ChunkId: chunk.ChunkId, Content: chunk.Content, SourceUri: chunk.SourceUri, Location: chunk.Location}) {
			return status.Error(codes.InvalidArgument, "invalid or duplicate pgvector index citation")
		}
		if _, err := preparedVector(chunk.Vector, int(transaction.spec.EmbeddingDimensions)); err != nil {
			return err
		}
	}
	for _, chunk := range request.Chunks {
		if transaction.receipt == nil {
			value, err := preparedVector(chunk.Vector, int(transaction.spec.EmbeddingDimensions))
			if err != nil {
				return err
			}
			args := append(dataArguments(transaction.scope), chunk.DocumentId, chunk.ChunkId, chunk.Content, chunk.SourceUri, chunk.Location, value.String())
			statement := "INSERT INTO " + pgx.Identifier{"public", transaction.spec.Table}.Sanitize() +
				" (namespace,knowledge_base_uid,generation,configuration_uid,configuration_digest,document_id,chunk_id,content,source_uri,location,metadata,embedding) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'{}',$11::public.vector)"
			if _, err := transaction.tx.Exec(ctx, statement, args...); err != nil {
				return queryError(ctx)
			}
		}
		encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(chunk)
		if err != nil {
			return status.Error(codes.InvalidArgument, "invalid pgvector index chunk")
		}
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(encoded)))
		_, _ = transaction.digest.Write(length[:])
		_, _ = transaction.digest.Write(encoded)
		transaction.chunks[[2]string{chunk.DocumentId, chunk.ChunkId}] = true
		transaction.documents[chunk.DocumentId] = true
	}
	transaction.sequence++
	return nil
}

func (transaction *indexTransaction) Commit(ctx context.Context) (*protocol.KnowledgeIndexResponse, error) {
	if transaction.finished || len(transaction.chunks) == 0 {
		return nil, status.Error(codes.FailedPrecondition, "pgvector index transaction cannot commit")
	}
	ctx, cancel := transaction.requestContext(ctx)
	defer cancel()
	contentDigest := hex.EncodeToString(transaction.digest.Sum(nil))
	documents, chunks := int64(len(transaction.documents)), int64(len(transaction.chunks))
	if receipt := transaction.receipt; receipt != nil {
		if receipt.contentDigest != contentDigest || receipt.documents != documents || receipt.chunks != chunks {
			return nil, status.Error(codes.FailedPrecondition, "pgvector operation replay content differs")
		}
	} else {
		var storedChunks, storedDocuments int64
		statement := "SELECT count(*),count(DISTINCT document_id) FROM " + pgx.Identifier{"public", transaction.spec.Table}.Sanitize() + " WHERE " + dataPredicate
		if err := transaction.tx.QueryRow(ctx, statement, dataArguments(transaction.scope)...).Scan(&storedChunks, &storedDocuments); err != nil {
			return nil, queryError(ctx)
		}
		if storedChunks != chunks || storedDocuments != documents {
			return nil, status.Error(codes.DataLoss, "pgvector index confirmation differs")
		}
		args := append(receiptArguments(transaction.spec, transaction.scope), transaction.operationID, transaction.sourceDigest, contentDigest, documents, chunks)
		statement = "INSERT INTO " + operationTable + " (table_name,namespace,knowledge_base_uid,generation,configuration_uid,configuration_digest,operation_id,source_digest,content_digest,document_count,chunk_count) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)"
		if _, err := transaction.tx.Exec(ctx, statement, args...); err != nil {
			return nil, queryError(ctx)
		}
	}
	if err := transaction.tx.Commit(ctx); err != nil {
		return nil, queryError(ctx)
	}
	transaction.finished = true
	transaction.cancel()
	return &protocol.KnowledgeIndexResponse{
		Scope: proto.Clone(transaction.scope).(*protocol.KnowledgeScope), OperationId: transaction.operationID,
		SourceDigest: transaction.sourceDigest, DocumentCount: documents, IndexedChunks: chunks,
		BackendOperationId: transaction.backendOperation,
	}, nil
}

func (transaction *indexTransaction) Abort(ctx context.Context) error {
	if transaction.finished {
		return nil
	}
	transaction.finished = true
	cleanup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	defer transaction.cancel()
	if err := transaction.tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return status.Error(codes.Unavailable, "pgvector index rollback failed")
	}
	return nil
}

func (lifecycle *Lifecycle) Cleanup(ctx context.Context, request *protocol.KnowledgeCleanupRequest) (_ *protocol.KnowledgeCleanupResponse, resultErr error) {
	if pluginsdk.ValidateKnowledgeCleanup(request) != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid pgvector cleanup request")
	}
	spec, err := DecodeBackendOperation(request.BackendOperationId, request.Scope)
	if err != nil || !reflect.DeepEqual(spec, lifecycle.spec) {
		return nil, status.Error(codes.PermissionDenied, "pgvector cleanup route does not match its operation")
	}
	if request.ReclaimPolicy == protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_ARCHIVE {
		return nil, status.Error(codes.FailedPrecondition, "pgvector archive reclaim policy is unsupported")
	}
	cleanup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := lifecycle.pool.Begin(cleanup)
	if err != nil {
		return nil, queryError(cleanup)
	}
	finished := false
	defer func() {
		if !finished {
			recovery, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if err := tx.Rollback(recovery); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				resultErr = errors.Join(resultErr, status.Error(codes.Unavailable, "pgvector cleanup rollback failed"))
			}
		}
	}()
	if err := lockOperation(cleanup, tx, spec, request.Scope); err != nil {
		return nil, err
	}
	receipt, err := readReceipt(cleanup, tx, spec, request.Scope)
	if err != nil {
		return nil, err
	}
	if receipt == nil || receipt.operationID != request.Scope.OperationId {
		return nil, status.Error(codes.FailedPrecondition, "pgvector cleanup has no matching committed operation")
	}
	if request.ReclaimPolicy == protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE {
		statement := "DELETE FROM " + pgx.Identifier{"public", spec.Table}.Sanitize() + " WHERE " + dataPredicate
		if _, err := tx.Exec(cleanup, statement, dataArguments(request.Scope)...); err != nil {
			return nil, queryError(cleanup)
		}
		if _, err := tx.Exec(cleanup, "UPDATE "+operationTable+" SET deleted=true WHERE "+receiptPredicate, receiptArguments(spec, request.Scope)...); err != nil {
			return nil, queryError(cleanup)
		}
	}
	if err := tx.Commit(cleanup); err != nil {
		return nil, queryError(cleanup)
	}
	finished = true
	return &protocol.KnowledgeCleanupResponse{
		Scope: proto.Clone(request.Scope).(*protocol.KnowledgeScope), CleanupId: request.CleanupId,
		ReclaimPolicy: request.ReclaimPolicy, BackendOperationId: request.BackendOperationId,
	}, nil
}

const dataPredicate = "namespace=$1 AND knowledge_base_uid=$2 AND generation=$3 AND configuration_uid=$4 AND configuration_digest=$5"
const receiptPredicate = "table_name=$1 AND namespace=$2 AND knowledge_base_uid=$3 AND generation=$4 AND configuration_uid=$5 AND configuration_digest=$6"

func dataArguments(scope *protocol.KnowledgeScope) []any {
	return []any{scope.Namespace, scope.KnowledgeBaseUid, scope.Generation, scope.ConfigurationUid, scope.ConfigurationDigest}
}

func receiptArguments(spec BackendSpec, scope *protocol.KnowledgeScope) []any {
	return append([]any{spec.Table}, dataArguments(scope)...)
}

func lockOperation(ctx context.Context, tx pgx.Tx, spec BackendSpec, scope *protocol.KnowledgeScope) error {
	key := strings.Join([]string{spec.Table, scope.Namespace, scope.KnowledgeBaseUid, fmt.Sprint(scope.Generation), scope.ConfigurationUid, scope.ConfigurationDigest}, "\x00")
	digest := sha256.Sum256([]byte(key))
	// A generation has one immutable operation, including across retries and cleanup.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", hex.EncodeToString(digest[:])); err != nil {
		return queryError(ctx)
	}
	return nil
}

func (transaction *indexTransaction) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, _ := transaction.context.Deadline()
	bounded, cancel := context.WithDeadline(ctx, deadline)
	stop := context.AfterFunc(transaction.context, cancel)
	return bounded, func() {
		stop()
		cancel()
	}
}

func readReceipt(ctx context.Context, tx pgx.Tx, spec BackendSpec, scope *protocol.KnowledgeScope) (*operationReceipt, error) {
	result := &operationReceipt{}
	err := tx.QueryRow(ctx, "SELECT operation_id,source_digest,content_digest,document_count,chunk_count,deleted FROM "+operationTable+" WHERE "+receiptPredicate, receiptArguments(spec, scope)...).
		Scan(&result.operationID, &result.sourceDigest, &result.contentDigest, &result.documents, &result.chunks, &result.deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, queryError(ctx)
	}
	return result, nil
}

type backendOperation struct {
	Spec                BackendSpec `json:"spec"`
	OperationID         string      `json:"operationId"`
	ConfigurationDigest string      `json:"configurationDigest"`
}

func encodeBackendOperation(spec BackendSpec, scope *protocol.KnowledgeScope, operationID string) (string, error) {
	encoded, err := json.Marshal(backendOperation{Spec: spec, OperationID: operationID, ConfigurationDigest: scope.ConfigurationDigest})
	if err != nil {
		return "", status.Error(codes.InvalidArgument, "invalid pgvector operation route")
	}
	result := "v1." + base64.RawURLEncoding.EncodeToString(encoded)
	if len(result) > 1024 {
		return "", status.Error(codes.InvalidArgument, "pgvector operation route exceeds its bound")
	}
	return result, nil
}

func DecodeBackendOperation(value string, scope *protocol.KnowledgeScope) (BackendSpec, error) {
	var empty BackendSpec
	version, encoded, found := strings.Cut(value, ".")
	if !found || version != "v1" || len(value) > 1024 || scope == nil || scope.ConfigurationApiGroup != APIGroup || scope.ConfigurationKind != Kind {
		return empty, status.Error(codes.InvalidArgument, "invalid pgvector operation route")
	}
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return empty, status.Error(codes.InvalidArgument, "invalid pgvector operation route")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var operation backendOperation
	if decoder.Decode(&operation) != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		operation.Spec.Validate() != nil || operation.OperationID != scope.OperationId || operation.ConfigurationDigest != scope.ConfigurationDigest {
		return empty, status.Error(codes.InvalidArgument, "invalid pgvector operation route")
	}
	return operation.Spec, nil
}
