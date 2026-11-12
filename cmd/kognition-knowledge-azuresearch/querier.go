// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	azuresearchprovider "github.com/kognition-ai/kognition/providers/azuresearch"
)

type azureSearchQuerier struct {
	reader     client.Reader
	credential azcore.TokenCredential
	logger     *zap.Logger
	mu         sync.Mutex
	entries    map[string]*azuresearchprovider.Querier
	closed     bool
}

var _ pluginsdk.KnowledgeQuerier = (*azureSearchQuerier)(nil)

func newAzureSearchQuerier(reader client.Reader, credential azcore.TokenCredential, logger *zap.Logger) *azureSearchQuerier {
	return &azureSearchQuerier{reader: reader, credential: credential, logger: logger, entries: make(map[string]*azuresearchprovider.Querier)}
}

func (querier *azureSearchQuerier) Query(ctx context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
	if request == nil || request.Scope == nil {
		return nil, status.Error(codes.InvalidArgument, "knowledge query is required")
	}
	entry, err := querier.acquire(ctx, request.Scope)
	if err != nil {
		return nil, err
	}
	return entry.Query(ctx, request)
}

func (querier *azureSearchQuerier) Close() {
	querier.mu.Lock()
	defer querier.mu.Unlock()
	querier.entries = make(map[string]*azuresearchprovider.Querier)
	querier.closed = true
}

func (querier *azureSearchQuerier) acquire(ctx context.Context, scope *protocol.KnowledgeScope) (*azuresearchprovider.Querier, error) {
	key := azureSearchScopeKey(scope)
	querier.mu.Lock()
	if querier.closed {
		querier.mu.Unlock()
		return nil, status.Error(codes.Unavailable, "azure search knowledge adapter is shutting down")
	}
	if entry, found := querier.entries[key]; found {
		querier.mu.Unlock()
		return entry, nil
	}
	querier.mu.Unlock()

	entry, err := querier.build(ctx, scope)
	if err != nil {
		return nil, err
	}
	querier.mu.Lock()
	defer querier.mu.Unlock()
	if querier.closed {
		return nil, status.Error(codes.Unavailable, "azure search knowledge adapter is shutting down")
	}
	if existing, found := querier.entries[key]; found {
		return existing, nil
	}
	querier.entries[key] = entry
	querier.logger.Info("azure search query client materialized",
		zap.String("namespace", scope.Namespace),
		zap.String("configurationName", scope.ConfigurationName),
		zap.String("configurationUID", scope.ConfigurationUid))
	return entry, nil
}

func (querier *azureSearchQuerier) build(ctx context.Context, scope *protocol.KnowledgeScope) (*azuresearchprovider.Querier, error) {
	gvk := schema.GroupVersionKind{Group: azuresearchprovider.APIGroup, Version: azuresearchprovider.APIVersion, Kind: azuresearchprovider.Kind}
	if scope.ConfigurationApiGroup != gvk.Group || scope.ConfigurationKind != gvk.Kind {
		return nil, status.Error(codes.FailedPrecondition, "azure search adapter cannot serve foreign configuration")
	}
	configuration := &unstructured.Unstructured{}
	configuration.SetGroupVersionKind(gvk)
	if err := querier.reader.Get(ctx, types.NamespacedName{Namespace: scope.Namespace, Name: scope.ConfigurationName}, configuration); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "azure search configuration is unavailable")
	}
	digest, err := knowledge.ConfigurationDigest(configuration)
	if err != nil {
		return nil, err
	}
	if configuration.GroupVersionKind() != gvk || configuration.GetNamespace() != scope.Namespace || configuration.GetName() != scope.ConfigurationName ||
		configuration.GetUID() == "" || string(configuration.GetUID()) != scope.ConfigurationUid || !configuration.GetDeletionTimestamp().IsZero() || digest != scope.ConfigurationDigest {
		return nil, status.Error(codes.FailedPrecondition, "azure search configuration differs from committed binding")
	}
	spec, err := azuresearchprovider.DecodeBackend(configuration)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "azure search configuration is invalid")
	}
	searchClient, err := azuresearchprovider.NewClient(querier.credential, spec, nil)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "azure search client configuration is invalid")
	}
	entry, err := azuresearchprovider.NewQuerier(searchClient, spec, scope)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "azure search query binding is invalid")
	}
	return entry, nil
}

func azureSearchScopeKey(scope *protocol.KnowledgeScope) string {
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
