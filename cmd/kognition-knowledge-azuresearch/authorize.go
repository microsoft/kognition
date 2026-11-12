// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

type scopeAuthorizer struct {
	index    *knowledgeBaseUIDIndex
	resolver *knowledge.BindingResolver
}

func (authorizer *scopeAuthorizer) lookup(scope *protocol.KnowledgeScope) (types.NamespacedName, error) {
	if authorizer == nil || authorizer.index == nil || authorizer.resolver == nil {
		return types.NamespacedName{}, status.Error(codes.Internal, "knowledge scope authorizer is not initialized")
	}
	if scope == nil {
		return types.NamespacedName{}, status.Error(codes.PermissionDenied, "knowledge scope is required")
	}
	return authorizer.index.Lookup(scope.Namespace, scope.KnowledgeBaseUid)
}

func (authorizer *scopeAuthorizer) AuthorizeQueryScope(ctx context.Context, scope *protocol.KnowledgeScope) error {
	key, err := authorizer.lookup(scope)
	if err != nil {
		return err
	}
	return authorizer.resolver.AuthorizeScope(ctx, key, scope)
}

func (authorizer *scopeAuthorizer) AuthorizeIndexScope(ctx context.Context, scope *protocol.KnowledgeScope) error {
	key, err := authorizer.lookup(scope)
	if err != nil {
		return err
	}
	return authorizer.resolver.AuthorizePendingScope(ctx, key, scope)
}

func (authorizer *scopeAuthorizer) AuthorizeCleanup(ctx context.Context, request *protocol.KnowledgeCleanupRequest) error {
	if request == nil {
		return status.Error(codes.PermissionDenied, "knowledge cleanup request is required")
	}
	key, err := authorizer.lookup(request.Scope)
	if err != nil {
		return err
	}
	binding, err := authorizer.resolver.ResolveCleanup(ctx, key)
	if err != nil {
		return err
	}
	if binding.BackendOperationID != request.BackendOperationId {
		return status.Error(codes.PermissionDenied, "knowledge cleanup backend operation does not match committed binding")
	}
	if !cleanupPolicyMatches(binding.ReclaimPolicy, request.ReclaimPolicy) {
		return status.Error(codes.PermissionDenied, "knowledge cleanup reclaim policy does not match committed binding")
	}
	return authorizer.resolver.AuthorizeCleanupScope(ctx, key, request.Scope)
}

func cleanupPolicyMatches(policy k8s.ReclaimPolicy, requested protocol.KnowledgeReclaimPolicy) bool {
	switch policy {
	case k8s.ReclaimPolicyDelete:
		return requested == protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE
	case k8s.ReclaimPolicyRetain:
		return requested == protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_RETAIN
	case k8s.ReclaimPolicyArchive:
		return requested == protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_ARCHIVE
	default:
		return false
	}
}
