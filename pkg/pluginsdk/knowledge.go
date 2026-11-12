// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pluginsdk

import (
	"context"
	"errors"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/util/validation"
)

const KnowledgeProtocolVersion = "kognition.knowledge/v1alpha1"
const knowledgeMessageLimit = 4 << 20
const knowledgeTimeout = 30 * time.Second

var knowledgeDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)
var knowledgeSourceDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var knowledgeKind = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,62}$`)
var knowledgeFilterKey = regexp.MustCompile(`^metadata\.[A-Za-z][A-Za-z0-9_.-]{0,127}$`)

type KnowledgeQuerier interface {
	Query(context.Context, *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error)
}

func validateKnowledgeScope(scope *protocol.KnowledgeScope) error {
	if scope == nil {
		return status.Error(codes.InvalidArgument, "knowledge scope is required")
	}
	if len(validation.IsDNS1123Label(scope.Namespace)) != 0 || scope.KnowledgeBaseUid == "" || len(scope.KnowledgeBaseUid) > 128 || scope.Generation < 1 ||
		len(validation.IsDNS1123Subdomain(scope.ConfigurationApiGroup)) != 0 || !knowledgeKind.MatchString(scope.ConfigurationKind) ||
		len(validation.IsDNS1123Subdomain(scope.ConfigurationName)) != 0 || scope.ConfigurationUid == "" || len(scope.ConfigurationUid) > 128 ||
		!knowledgeDigest.MatchString(scope.ConfigurationDigest) {
		return status.Error(codes.InvalidArgument, "invalid knowledge scope")
	}
	return nil
}

func ValidateKnowledgeQuery(request *protocol.KnowledgeQueryRequest) error {
	if request == nil {
		return status.Error(codes.InvalidArgument, "knowledge query is required")
	}
	if err := validateKnowledgeScope(request.Scope); err != nil {
		return err
	}
	if strings.TrimSpace(request.Scope.OperationId) == "" || len(request.Scope.OperationId) > 128 ||
		strings.TrimSpace(request.RequestId) == "" || len(request.RequestId) > 128 || request.TopK < 1 || request.TopK > 100 || len(request.Text) > 65536 || len(request.Vector) > 65536 || len(request.Filters) > 32 {
		return status.Error(codes.InvalidArgument, "invalid knowledge query bounds")
	}
	switch request.Mode {
	case "keyword":
		if strings.TrimSpace(request.Text) == "" || len(request.Vector) != 0 {
			return status.Error(codes.InvalidArgument, "keyword query requires text and no vector")
		}
	case "dense":
		if len(request.Vector) == 0 || request.Text != "" {
			return status.Error(codes.InvalidArgument, "dense query requires a vector and no text")
		}
	case "hybrid":
		if strings.TrimSpace(request.Text) == "" || len(request.Vector) == 0 {
			return status.Error(codes.InvalidArgument, "hybrid query requires text and a vector")
		}
	default:
		return status.Error(codes.InvalidArgument, "unsupported knowledge query mode")
	}
	for _, value := range request.Vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return status.Error(codes.InvalidArgument, "query vector must contain finite values")
		}
	}
	keys := make(map[string]bool)
	for _, filter := range request.Filters {
		if filter == nil || !knowledgeFilterKey.MatchString(filter.Key) || len(filter.Value) > 4096 || keys[filter.Key] {
			return status.Error(codes.InvalidArgument, "invalid or duplicate metadata filter")
		}
		keys[filter.Key] = true
	}
	return nil
}

func ValidateKnowledgeIndexBatch(request *protocol.KnowledgeIndexBatchRequest) error {
	if request == nil {
		return status.Error(codes.InvalidArgument, "knowledge index batch is required")
	}
	if err := validateKnowledgeScope(request.Scope); err != nil {
		return err
	}
	if strings.TrimSpace(request.OperationId) == "" || len(request.OperationId) > 128 ||
		!knowledgeSourceDigest.MatchString(request.SourceDigest) || request.Sequence < 0 ||
		len(request.Chunks) == 0 || len(request.Chunks) > 100 {
		return status.Error(codes.InvalidArgument, "invalid knowledge index batch bounds")
	}
	seen := make(map[[2]string]bool)
	for _, chunk := range request.Chunks {
		if chunk == nil || strings.TrimSpace(chunk.DocumentId) == "" || len(chunk.DocumentId) > 1024 ||
			strings.TrimSpace(chunk.ChunkId) == "" || len(chunk.ChunkId) > 1024 ||
			strings.TrimSpace(chunk.Content) == "" || len(chunk.Content) > 65536 ||
			strings.TrimSpace(chunk.SourceUri) == "" || len(chunk.SourceUri) > 4096 || len(chunk.Location) > 4096 ||
			len(chunk.Vector) == 0 || len(chunk.Vector) > 65536 {
			return status.Error(codes.InvalidArgument, "invalid knowledge index chunk")
		}
		identity := [2]string{chunk.DocumentId, chunk.ChunkId}
		if seen[identity] {
			return status.Error(codes.InvalidArgument, "duplicate knowledge index chunk")
		}
		seen[identity] = true
		for _, value := range chunk.Vector {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return status.Error(codes.InvalidArgument, "index vector must contain finite values")
			}
		}
	}
	if proto.Size(request) > knowledgeMessageLimit {
		return status.Error(codes.ResourceExhausted, "knowledge index batch exceeds message limit")
	}
	return nil
}

func ValidateKnowledgeCleanup(request *protocol.KnowledgeCleanupRequest) error {
	if request == nil {
		return status.Error(codes.InvalidArgument, "knowledge cleanup request is required")
	}
	if err := validateKnowledgeScope(request.Scope); err != nil {
		return err
	}
	if strings.TrimSpace(request.Scope.OperationId) == "" || len(request.Scope.OperationId) > 128 ||
		strings.TrimSpace(request.CleanupId) == "" || len(request.CleanupId) > 128 ||
		strings.TrimSpace(request.BackendOperationId) == "" || len(request.BackendOperationId) > 1024 {
		return status.Error(codes.InvalidArgument, "invalid knowledge cleanup bounds")
	}
	switch request.ReclaimPolicy {
	case protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE,
		protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_RETAIN,
		protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_ARCHIVE:
		return nil
	default:
		return status.Error(codes.InvalidArgument, "invalid knowledge reclaim policy")
	}
}

func validateKnowledgeIndexResponse(scope *protocol.KnowledgeScope, operationID, sourceDigest string, documentCount, chunkCount int64, response *protocol.KnowledgeIndexResponse) error {
	if response == nil || !proto.Equal(scope, response.Scope) || response.OperationId != operationID || response.SourceDigest != sourceDigest ||
		response.DocumentCount != documentCount || response.IndexedChunks != chunkCount || strings.TrimSpace(response.BackendOperationId) == "" ||
		len(response.BackendOperationId) > 1024 {
		return status.Error(codes.DataLoss, "invalid knowledge index confirmation")
	}
	return nil
}

func validateKnowledgeCleanupResponse(request *protocol.KnowledgeCleanupRequest, response *protocol.KnowledgeCleanupResponse) error {
	if response == nil || !proto.Equal(request.Scope, response.Scope) || response.CleanupId != request.CleanupId ||
		response.ReclaimPolicy != request.ReclaimPolicy || strings.TrimSpace(response.BackendOperationId) == "" ||
		len(response.BackendOperationId) > 1024 {
		return status.Error(codes.DataLoss, "invalid knowledge cleanup confirmation")
	}
	return nil
}

func validateKnowledgeCapabilities(capabilities *protocol.KnowledgeProviderCapabilities, request *protocol.KnowledgeQueryRequest) error {
	if capabilities == nil || capabilities.ProtocolVersion != KnowledgeProtocolVersion {
		return status.Error(codes.FailedPrecondition, "incompatible knowledge protocol")
	}
	matched := false
	for _, kind := range capabilities.ConfigurationKinds {
		if kind.GetApiGroup() == request.Scope.ConfigurationApiGroup && kind.GetKind() == request.Scope.ConfigurationKind {
			matched = true
		}
	}
	if !matched {
		return status.Error(codes.FailedPrecondition, "unsupported knowledge configuration kind")
	}
	operations := []string{request.Mode, "citations"}
	if len(request.Filters) > 0 {
		operations = append(operations, "filter")
	}
	for _, operation := range operations {
		if !slices.Contains(capabilities.Operations, operation) {
			return status.Error(codes.FailedPrecondition, "unsupported knowledge operation")
		}
	}
	return nil
}

func validateKnowledgeIndexCapabilities(capabilities *protocol.KnowledgeProviderCapabilities, scope *protocol.KnowledgeScope) error {
	if capabilities == nil || capabilities.ProtocolVersion != KnowledgeProtocolVersion {
		return status.Error(codes.FailedPrecondition, "incompatible knowledge protocol")
	}
	for _, kind := range capabilities.ConfigurationKinds {
		if kind.GetApiGroup() == scope.ConfigurationApiGroup && kind.GetKind() == scope.ConfigurationKind {
			if slices.Contains(capabilities.Operations, "index") {
				return nil
			}
			return status.Error(codes.FailedPrecondition, "unsupported knowledge operation")
		}
	}
	return status.Error(codes.FailedPrecondition, "unsupported knowledge configuration kind")
}

func validateKnowledgeCleanupCapabilities(capabilities *protocol.KnowledgeProviderCapabilities, request *protocol.KnowledgeCleanupRequest) error {
	if capabilities == nil || capabilities.ProtocolVersion != KnowledgeProtocolVersion {
		return status.Error(codes.FailedPrecondition, "incompatible knowledge protocol")
	}
	matched := false
	for _, kind := range capabilities.ConfigurationKinds {
		if kind.GetApiGroup() == request.Scope.ConfigurationApiGroup && kind.GetKind() == request.Scope.ConfigurationKind {
			matched = true
			break
		}
	}
	if !matched {
		return status.Error(codes.FailedPrecondition, "unsupported knowledge configuration kind")
	}
	operation := map[protocol.KnowledgeReclaimPolicy]string{
		protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE:  "cleanup-delete",
		protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_RETAIN:  "cleanup-retain",
		protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_ARCHIVE: "cleanup-archive",
	}[request.ReclaimPolicy]
	if operation == "" || !slices.Contains(capabilities.Operations, operation) {
		return status.Error(codes.FailedPrecondition, "unsupported knowledge reclaim policy")
	}
	return nil
}

func validateKnowledgeResponse(request *protocol.KnowledgeQueryRequest, response *protocol.KnowledgeQueryResponse) error {
	if response == nil || !proto.Equal(request.Scope, response.Scope) || response.RequestId != request.RequestId || len(response.Matches) > int(request.TopK) {
		return status.Error(codes.DataLoss, "invalid knowledge response binding")
	}
	seen := make(map[[2]string]bool)
	for _, match := range response.Matches {
		if match == nil || match.DocumentId == "" || len(match.DocumentId) > 1024 || match.ChunkId == "" || len(match.ChunkId) > 1024 ||
			match.SourceUri == "" || len(match.SourceUri) > 4096 || len(match.Location) > 4096 || len(match.Content) > 65536 || math.IsNaN(match.Score) || math.IsInf(match.Score, 0) {
			return status.Error(codes.DataLoss, "invalid knowledge citation")
		}
		identity := [2]string{match.DocumentId, match.ChunkId}
		if seen[identity] {
			return status.Error(codes.DataLoss, "duplicate knowledge citation")
		}
		seen[identity] = true
	}
	if proto.Size(response) > knowledgeMessageLimit {
		return status.Error(codes.ResourceExhausted, "knowledge response exceeds message limit")
	}
	return nil
}

func knowledgeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "knowledge provider operation canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "knowledge provider operation deadline exceeded")
	}
	code := status.Code(err)
	switch code {
	case codes.InvalidArgument, codes.NotFound, codes.PermissionDenied, codes.Unauthenticated, codes.FailedPrecondition, codes.Unavailable, codes.ResourceExhausted, codes.DeadlineExceeded, codes.Canceled, codes.DataLoss:
	default:
		code = codes.Internal
	}
	return status.Error(code, "knowledge provider operation failed")
}
