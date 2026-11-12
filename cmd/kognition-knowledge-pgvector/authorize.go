// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

// scopeAuthorizer bridges the pluginsdk knowledge server (which only sees a
// KnowledgeScope) to the shared BindingResolver (which needs the KnowledgeBase
// namespaced name to look up the committed backend binding).
type scopeAuthorizer struct {
	index    *knowledgeBaseUIDIndex
	resolver *knowledge.BindingResolver
}

// AuthorizeScope resolves the scope's KnowledgeBase UID back to a namespaced
// name and delegates to the resolver, which re-fetches the live KnowledgeBase,
// KnowledgeBaseClass, and provider configuration and verifies that every scope
// field matches the committed BackendBinding on the KnowledgeBase status.
func (auth *scopeAuthorizer) AuthorizeScope(ctx context.Context, scope *protocol.KnowledgeScope) error {
	key, err := auth.lookup(scope)
	if err != nil {
		return err
	}
	return auth.resolver.AuthorizeScope(ctx, key, scope)
}

func (auth *scopeAuthorizer) lookup(scope *protocol.KnowledgeScope) (types.NamespacedName, error) {
	if auth == nil || auth.index == nil || auth.resolver == nil {
		return types.NamespacedName{}, status.Error(codes.Internal, "knowledge scope authorizer is not initialized")
	}
	if scope == nil {
		return types.NamespacedName{}, status.Error(codes.PermissionDenied, "knowledge scope is required")
	}
	return auth.index.Lookup(scope.Namespace, scope.KnowledgeBaseUid)
}

func (auth *scopeAuthorizer) AuthorizeIndexScope(ctx context.Context, scope *protocol.KnowledgeScope) error {
	key, err := auth.lookup(scope)
	if err != nil {
		return err
	}
	return auth.resolver.AuthorizePendingScope(ctx, key, scope)
}

func (auth *scopeAuthorizer) AuthorizeCleanup(ctx context.Context, request *protocol.KnowledgeCleanupRequest) error {
	if request == nil {
		return status.Error(codes.PermissionDenied, "knowledge cleanup request is required")
	}
	key, err := auth.lookup(request.Scope)
	if err != nil {
		return err
	}
	return auth.resolver.AuthorizeCleanupRequest(ctx, key, request)
}
