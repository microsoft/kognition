// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuresearch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type DocumentIndexer interface {
	Index(context.Context, []IndexDocument) (IndexBatchResult, error)
	Count(context.Context, string) (int64, error)
}

type Indexer struct {
	client     DocumentIndexer
	dimensions int
	spec       BackendSpec
}

var _ pluginsdk.KnowledgeIndexer = (*Indexer)(nil)

func NewIndexer(client DocumentIndexer, spec BackendSpec) (*Indexer, error) {
	if client == nil || spec.Validate() != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid azure search index configuration")
	}
	return &Indexer{client: client, dimensions: int(spec.EmbeddingDimensions), spec: spec}, nil
}

func (indexer *Indexer) BeginIndex(_ context.Context, scope *protocol.KnowledgeScope, operationID, sourceDigest string) (pluginsdk.KnowledgeIndexTransaction, error) {
	probe := &protocol.KnowledgeIndexBatchRequest{Scope: scope, OperationId: operationID, SourceDigest: sourceDigest, Chunks: []*protocol.KnowledgeIndexChunk{{DocumentId: "validation", ChunkId: "validation", Content: "validation", SourceUri: "urn:kognition:validation", Vector: make([]float64, indexer.dimensions)}}}
	if pluginsdk.ValidateKnowledgeIndexBatch(probe) != nil || scope.ConfigurationApiGroup != APIGroup || scope.ConfigurationKind != Kind {
		return nil, status.Error(codes.InvalidArgument, "invalid azure search index binding")
	}
	clonedScope, ok := proto.Clone(scope).(*protocol.KnowledgeScope)
	if !ok {
		return nil, status.Error(codes.Internal, "clone azure search index scope")
	}
	return &indexTransaction{
		client: indexer.client, dimensions: indexer.dimensions, spec: indexer.spec, scope: clonedScope,
		operationID: operationID, sourceDigest: sourceDigest, documents: make(map[string]struct{}), chunks: make(map[[2]string]struct{}),
	}, nil
}

type indexTransaction struct {
	client         DocumentIndexer
	dimensions     int
	spec           BackendSpec
	scope          *protocol.KnowledgeScope
	operationID    string
	sourceDigest   string
	documents      map[string]struct{}
	chunks         map[[2]string]struct{}
	keys           []string
	backendRequest []string
	finished       bool
}

func (transaction *indexTransaction) WriteBatch(ctx context.Context, request *protocol.KnowledgeIndexBatchRequest) error {
	if transaction.finished {
		return status.Error(codes.FailedPrecondition, "azure search index transaction is closed")
	}
	if err := pluginsdk.ValidateKnowledgeIndexBatch(request); err != nil || !proto.Equal(request.Scope, transaction.scope) || request.OperationId != transaction.operationID || request.SourceDigest != transaction.sourceDigest {
		return status.Error(codes.InvalidArgument, "azure search index batch binding mismatch")
	}
	batchIdentities := make([][2]string, 0, len(request.Chunks))
	documents := make([]IndexDocument, 0, len(request.Chunks))
	for _, chunk := range request.Chunks {
		identity := [2]string{chunk.DocumentId, chunk.ChunkId}
		if _, found := transaction.chunks[identity]; found {
			return status.Error(codes.InvalidArgument, "duplicate azure search index chunk")
		}
		if len(chunk.Vector) != transaction.dimensions || !azureVector(chunk.Vector) {
			return status.Error(codes.InvalidArgument, "azure search index vector dimension or precision mismatch")
		}
		key := DocumentKey(transaction.scope.Namespace, transaction.scope.KnowledgeBaseUid, transaction.scope.Generation, transaction.operationID, chunk.DocumentId, chunk.ChunkId)
		documents = append(documents, IndexDocument{
			Action: "mergeOrUpload", Key: key, Namespace: transaction.scope.Namespace, KnowledgeBaseUID: transaction.scope.KnowledgeBaseUid,
			Generation: transaction.scope.Generation, OperationID: transaction.operationID, ConfigurationUID: transaction.scope.ConfigurationUid, ConfigurationDigest: transaction.scope.ConfigurationDigest,
			DocumentID: chunk.DocumentId, ChunkID: chunk.ChunkId, Content: chunk.Content, SourceURI: chunk.SourceUri, Location: chunk.Location, Embedding: chunk.Vector,
		})
		batchIdentities = append(batchIdentities, identity)
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, document := range documents {
		transaction.keys = append(transaction.keys, document.Key)
	}
	pending := documents
	for delay := 100 * time.Millisecond; ; delay = min(delay*2, 2*time.Second) {
		result, err := transaction.client.Index(requestCtx, pending)
		if err != nil {
			return indexError(requestCtx, err)
		}
		transaction.backendRequest = append(transaction.backendRequest, result.RequestID)
		byKey := make(map[string]IndexDocument, len(pending))
		for _, document := range pending {
			byKey[document.Key] = document
		}
		retry := make([]IndexDocument, 0, len(pending))
		for _, documentResult := range result.Documents {
			if documentResult.Succeeded {
				continue
			}
			if !retryableDocumentStatus(documentResult.StatusCode) {
				return indexError(requestCtx, &ServiceError{StatusCode: documentResult.StatusCode})
			}
			document, found := byKey[documentResult.Key]
			if !found {
				return status.Error(codes.DataLoss, "invalid azure search index response")
			}
			retry = append(retry, document)
		}
		if len(retry) == 0 {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-requestCtx.Done():
			timer.Stop()
			return status.FromContextError(requestCtx.Err()).Err()
		case <-timer.C:
		}
		pending = retry
	}
	for _, identity := range batchIdentities {
		transaction.chunks[identity] = struct{}{}
		transaction.documents[identity[0]] = struct{}{}
	}
	return nil
}

func (transaction *indexTransaction) Commit(ctx context.Context) (*protocol.KnowledgeIndexResponse, error) {
	if transaction.finished || len(transaction.keys) == 0 {
		return nil, status.Error(codes.FailedPrecondition, "azure search index transaction cannot commit")
	}
	visibilityCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	filter := fmt.Sprintf(
		"namespace eq '%s' and knowledgeBaseUid eq '%s' and generation eq %d and operationId eq '%s' and configurationUid eq '%s' and configurationDigest eq '%s'",
		odataString(transaction.scope.Namespace), odataString(transaction.scope.KnowledgeBaseUid), transaction.scope.Generation,
		odataString(transaction.operationID), odataString(transaction.scope.ConfigurationUid), odataString(transaction.scope.ConfigurationDigest),
	)
	for delay := 100 * time.Millisecond; ; delay = min(delay*2, 2*time.Second) {
		visible, err := transaction.client.Count(visibilityCtx, filter)
		if err != nil {
			return nil, indexError(visibilityCtx, err)
		}
		if visible == int64(len(transaction.keys)) {
			break
		}
		if visible > int64(len(transaction.keys)) {
			return nil, status.Error(codes.DataLoss, "azure search operation contains unexpected documents")
		}
		timer := time.NewTimer(delay)
		select {
		case <-visibilityCtx.Done():
			timer.Stop()
			return nil, status.FromContextError(visibilityCtx.Err()).Err()
		case <-timer.C:
		}
	}
	clonedScope, ok := proto.Clone(transaction.scope).(*protocol.KnowledgeScope)
	if !ok {
		return nil, status.Error(codes.Internal, "clone azure search index scope")
	}
	transaction.finished = true
	backendOperationID, err := encodeBackendOperation(transaction.spec, transaction.scope, transaction.operationID)
	if err != nil {
		return nil, status.Error(codes.Internal, "azure search backend operation could not be encoded")
	}
	return &protocol.KnowledgeIndexResponse{
		Scope: clonedScope, OperationId: transaction.operationID, SourceDigest: transaction.sourceDigest,
		DocumentCount: int64(len(transaction.documents)), IndexedChunks: int64(len(transaction.chunks)), BackendOperationId: backendOperationID,
	}, nil
}

func (transaction *indexTransaction) Abort(ctx context.Context) error {
	if transaction.finished {
		return nil
	}
	transaction.finished = true
	var failures []error
	for offset := 0; offset < len(transaction.keys); offset += 1000 {
		end := min(offset+1000, len(transaction.keys))
		deletes := make([]IndexDocument, 0, end-offset)
		for _, key := range transaction.keys[offset:end] {
			deletes = append(deletes, IndexDocument{Action: "delete", Key: key})
		}
		result, err := transaction.client.Index(ctx, deletes)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		expected := make(map[string]struct{}, len(deletes))
		for _, document := range deletes {
			expected[document.Key] = struct{}{}
		}
		for _, documentResult := range result.Documents {
			if _, found := expected[documentResult.Key]; !found {
				failures = append(failures, status.Error(codes.DataLoss, "invalid azure search abort response"))
				continue
			}
			delete(expected, documentResult.Key)
			if !documentResult.Succeeded {
				failures = append(failures, indexError(ctx, &ServiceError{StatusCode: documentResult.StatusCode}))
			}
		}
		if len(expected) != 0 {
			failures = append(failures, status.Error(codes.DataLoss, "invalid azure search abort response"))
		}
	}
	return errors.Join(failures...)
}

func azureVector(vector []float64) bool {
	var squaredNorm float32
	for _, value := range vector {
		converted := float32(value)
		if math.IsInf(float64(converted), 0) {
			return false
		}
		squaredNorm += converted * converted
	}
	return squaredNorm != 0 && !math.IsInf(float64(squaredNorm), 0)
}

func retryableDocumentStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusConflict, http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	default:
		return false
	}
}

func indexError(ctx context.Context, err error) error {
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
		case http.StatusRequestEntityTooLarge, http.StatusTooManyRequests:
			return status.Error(codes.ResourceExhausted, "azure search index request rejected")
		}
	}
	var responseError *ResponseError
	if errors.As(err, &responseError) {
		return status.Error(codes.DataLoss, "invalid azure search index response")
	}
	return status.Error(codes.Unavailable, "azure search indexing failed")
}
