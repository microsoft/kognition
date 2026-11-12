// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuresearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type CleanupClient interface {
	Search(context.Context, SearchRequest) ([]SearchDocument, error)
	Index(context.Context, []IndexDocument) (IndexBatchResult, error)
	Count(context.Context, string) (int64, error)
}

type Cleaner struct {
	client CleanupClient
}

var _ pluginsdk.KnowledgeCleaner = (*Cleaner)(nil)

func NewCleaner(client CleanupClient) (*Cleaner, error) {
	if client == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid azure search cleanup configuration")
	}
	return &Cleaner{client: client}, nil
}

func (cleaner *Cleaner) Cleanup(ctx context.Context, request *protocol.KnowledgeCleanupRequest) (*protocol.KnowledgeCleanupResponse, error) {
	if err := pluginsdk.ValidateKnowledgeCleanup(request); err != nil || request.Scope.ConfigurationApiGroup != APIGroup || request.Scope.ConfigurationKind != Kind {
		return nil, status.Error(codes.InvalidArgument, "invalid azure search cleanup binding")
	}
	requestScope, ok := proto.Clone(request.Scope).(*protocol.KnowledgeScope)
	if !ok {
		return nil, status.Error(codes.Internal, "clone azure search cleanup scope")
	}
	switch request.ReclaimPolicy {
	case protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_RETAIN:
		return cleanupConfirmation(requestScope, request), nil
	case protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_ARCHIVE:
		return nil, status.Error(codes.FailedPrecondition, "azure search archive reclaim policy is unsupported")
	case protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE:
	default:
		return nil, status.Error(codes.InvalidArgument, "invalid azure search reclaim policy")
	}

	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	filter := searchFilter(requestScope, nil)
	for delay := 100 * time.Millisecond; ; delay = min(delay*2, 2*time.Second) {
		documents, err := cleaner.client.Search(cleanupCtx, SearchRequest{Search: "*", Filter: filter, Select: "id", Top: 1000})
		if err != nil {
			return nil, searchError(cleanupCtx, err)
		}
		if len(documents) > 0 {
			deletes := make([]IndexDocument, 0, len(documents))
			for _, document := range documents {
				if document.Key == "" {
					return nil, status.Error(codes.DataLoss, "invalid azure search cleanup result")
				}
				deletes = append(deletes, IndexDocument{Action: "delete", Key: document.Key})
			}
			result, err := cleaner.client.Index(cleanupCtx, deletes)
			if err != nil {
				return nil, indexError(cleanupCtx, err)
			}
			expected := make(map[string]struct{}, len(deletes))
			for _, document := range deletes {
				expected[document.Key] = struct{}{}
			}
			if len(result.Documents) != len(expected) {
				return nil, indexError(cleanupCtx, &ResponseError{})
			}
			for _, document := range result.Documents {
				if _, ok := expected[document.Key]; !ok {
					return nil, indexError(cleanupCtx, &ResponseError{})
				}
				delete(expected, document.Key)
				if !document.Succeeded && !retryableDocumentStatus(document.StatusCode) {
					return nil, indexError(cleanupCtx, &ServiceError{StatusCode: document.StatusCode})
				}
			}
			if len(expected) != 0 {
				return nil, indexError(cleanupCtx, &ResponseError{})
			}
		}
		remaining, err := cleaner.client.Count(cleanupCtx, filter)
		if err != nil {
			return nil, searchError(cleanupCtx, err)
		}
		if remaining == 0 {
			return cleanupConfirmation(requestScope, request), nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-cleanupCtx.Done():
			timer.Stop()
			return nil, status.FromContextError(cleanupCtx.Err()).Err()
		case <-timer.C:
		}
	}
}

func cleanupConfirmation(scope *protocol.KnowledgeScope, request *protocol.KnowledgeCleanupRequest) *protocol.KnowledgeCleanupResponse {
	digest := sha256.Sum256([]byte(request.CleanupId + "\x00" + request.ReclaimPolicy.String() + "\x00" + scope.OperationId))
	return &protocol.KnowledgeCleanupResponse{Scope: scope, CleanupId: request.CleanupId, ReclaimPolicy: request.ReclaimPolicy, BackendOperationId: hex.EncodeToString(digest[:])}
}
