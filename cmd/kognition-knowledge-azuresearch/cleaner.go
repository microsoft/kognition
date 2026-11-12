// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	azuresearchprovider "github.com/kognition-ai/kognition/providers/azuresearch"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type azureSearchCleaner struct {
	credential azcore.TokenCredential
}

var _ pluginsdk.KnowledgeCleaner = (*azureSearchCleaner)(nil)

func newAzureSearchCleaner(credential azcore.TokenCredential) *azureSearchCleaner {
	return &azureSearchCleaner{credential: credential}
}

func (cleaner *azureSearchCleaner) Cleanup(ctx context.Context, request *protocol.KnowledgeCleanupRequest) (*protocol.KnowledgeCleanupResponse, error) {
	if cleaner == nil || cleaner.credential == nil || request == nil {
		return nil, status.Error(codes.FailedPrecondition, "azure search cleanup is unavailable")
	}
	client, err := azuresearchprovider.NewCleanupClient(cleaner.credential, request.BackendOperationId, request.Scope, nil)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "azure search cleanup locator is invalid")
	}
	providerCleaner, err := azuresearchprovider.NewCleaner(client)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "azure search cleanup configuration is invalid")
	}
	return providerCleaner.Cleanup(ctx, request)
}
