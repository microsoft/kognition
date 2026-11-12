// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	azuresearchprovider "github.com/kognition-ai/kognition/providers/azuresearch"
)

type testCredential func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error)

func (credential testCredential) GetToken(ctx context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return credential(ctx, options)
}

func newAzureSearchFixture(t *testing.T) (*unstructured.Unstructured, *protocol.KnowledgeScope, *azureSearchQuerier) {
	t.Helper()
	gvk := schema.GroupVersionKind{Group: azuresearchprovider.APIGroup, Version: azuresearchprovider.APIVersion, Kind: azuresearchprovider.Kind}
	configuration := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
		"serviceName": "kognition-search", "indexName": "knowledge_chunks", "embeddingDimensions": int64(3),
		"authentication": map[string]any{"mode": "workload-identity"},
	}}}
	configuration.SetGroupVersionKind(gvk)
	configuration.SetNamespace("tenant-a")
	configuration.SetName("shared")
	configuration.SetUID("config-uid")
	digest, err := knowledge.ConfigurationDigest(configuration)
	if err != nil {
		t.Fatal(err)
	}
	scope := &protocol.KnowledgeScope{
		Namespace: "tenant-a", KnowledgeBaseUid: "kb-uid", Generation: 3,
		OperationId:           "tenant-a/kb-index-3",
		ConfigurationApiGroup: gvk.Group, ConfigurationKind: gvk.Kind, ConfigurationName: "shared",
		ConfigurationUid: "config-uid", ConfigurationDigest: digest,
	}
	reader := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(configuration).Build()
	credential := testCredential(func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
		return azcore.AccessToken{Token: "unused", ExpiresOn: time.Now().Add(time.Hour)}, nil
	})
	return configuration, scope, newAzureSearchQuerier(reader, credential, zap.NewNop())
}

func TestAzureSearchQuerierBuildsAndCachesCommittedConfiguration(t *testing.T) {
	_, scope, querier := newAzureSearchFixture(t)
	first, err := querier.acquire(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	second, err := querier.acquire(context.Background(), proto.Clone(scope).(*protocol.KnowledgeScope))
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(querier.entries) != 1 {
		t.Fatal("expected one cached querier for an identical committed scope")
	}
	querier.Close()
	querier.Close()
	if _, err := querier.acquire(context.Background(), scope); status.Code(err) != codes.Unavailable {
		t.Fatalf("unexpected post-close error: %v", err)
	}
}

func TestAzureSearchQuerierRejectsUncommittedConfiguration(t *testing.T) {
	_, scope, querier := newAzureSearchFixture(t)
	tests := []struct {
		name   string
		mutate func(*protocol.KnowledgeScope)
	}{
		{name: "foreign kind", mutate: func(scope *protocol.KnowledgeScope) { scope.ConfigurationKind = "OtherBackend" }},
		{name: "wrong uid", mutate: func(scope *protocol.KnowledgeScope) { scope.ConfigurationUid = "other-uid" }},
		{name: "wrong digest", mutate: func(scope *protocol.KnowledgeScope) { scope.ConfigurationDigest = strings.Repeat("0", 64) }},
		{name: "missing configuration", mutate: func(scope *protocol.KnowledgeScope) { scope.ConfigurationName = "missing" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := proto.Clone(scope).(*protocol.KnowledgeScope)
			test.mutate(changed)
			if _, err := querier.acquire(context.Background(), changed); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestAzureSearchQuerierRejectsMissingQuery(t *testing.T) {
	_, _, querier := newAzureSearchFixture(t)
	if _, err := querier.Query(context.Background(), nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unexpected nil request error: %v", err)
	}
	if _, err := querier.Query(context.Background(), &protocol.KnowledgeQueryRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unexpected nil scope error: %v", err)
	}
}
