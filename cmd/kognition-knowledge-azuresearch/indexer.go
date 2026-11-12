// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
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

type azureSearchIndexer struct {
	reader     client.Reader
	credential azcore.TokenCredential
}

var _ pluginsdk.KnowledgeIndexer = (*azureSearchIndexer)(nil)

func newAzureSearchIndexer(reader client.Reader, credential azcore.TokenCredential) *azureSearchIndexer {
	return &azureSearchIndexer{reader: reader, credential: credential}
}

func (indexer *azureSearchIndexer) BeginIndex(ctx context.Context, scope *protocol.KnowledgeScope, operationID, sourceDigest string) (pluginsdk.KnowledgeIndexTransaction, error) {
	if scope == nil {
		return nil, status.Error(codes.InvalidArgument, "knowledge index scope is required")
	}
	gvk := schema.GroupVersionKind{Group: azuresearchprovider.APIGroup, Version: azuresearchprovider.APIVersion, Kind: azuresearchprovider.Kind}
	if scope.ConfigurationApiGroup != gvk.Group || scope.ConfigurationKind != gvk.Kind {
		return nil, status.Error(codes.FailedPrecondition, "azure search adapter cannot index foreign configuration")
	}
	configuration := &unstructured.Unstructured{}
	configuration.SetGroupVersionKind(gvk)
	if err := indexer.reader.Get(ctx, types.NamespacedName{Namespace: scope.Namespace, Name: scope.ConfigurationName}, configuration); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "azure search configuration is unavailable")
	}
	digest, err := knowledge.ConfigurationDigest(configuration)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "azure search configuration digest is invalid")
	}
	if configuration.GroupVersionKind() != gvk || configuration.GetNamespace() != scope.Namespace || configuration.GetName() != scope.ConfigurationName ||
		configuration.GetUID() == "" || string(configuration.GetUID()) != scope.ConfigurationUid || !configuration.GetDeletionTimestamp().IsZero() || digest != scope.ConfigurationDigest {
		return nil, status.Error(codes.FailedPrecondition, "azure search configuration differs from index scope")
	}
	spec, err := azuresearchprovider.DecodeBackend(configuration)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "azure search configuration is invalid")
	}
	searchClient, err := azuresearchprovider.NewClient(indexer.credential, spec, nil)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "azure search client configuration is invalid")
	}
	providerIndexer, err := azuresearchprovider.NewIndexer(searchClient, spec)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "azure search index binding is invalid")
	}
	return providerIndexer.BeginIndex(ctx, scope, operationID, sourceDigest)
}
