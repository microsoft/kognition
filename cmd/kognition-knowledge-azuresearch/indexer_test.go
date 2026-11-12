// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAzureSearchIndexerBeginsForCommittedConfiguration(t *testing.T) {
	_, scope, querier := newAzureSearchFixture(t)
	indexer := newAzureSearchIndexer(querier.reader, querier.credential)
	transaction, err := indexer.BeginIndex(context.Background(), scope, "operation-1", "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if transaction == nil {
		t.Fatal("expected a transaction for the committed configuration")
	}
}

func TestAzureSearchIndexerRejectsChangedConfiguration(t *testing.T) {
	configuration, scope, querier := newAzureSearchFixture(t)
	kubernetesClient := querier.reader.(client.Client)
	if err := querier.reader.Get(context.Background(), types.NamespacedName{Namespace: configuration.GetNamespace(), Name: configuration.GetName()}, configuration); err != nil {
		t.Fatal(err)
	}
	configuration.Object["spec"].(map[string]any)["indexName"] = "replacement_index"
	if err := kubernetesClient.Update(context.Background(), configuration); err != nil {
		t.Fatal(err)
	}
	indexer := newAzureSearchIndexer(querier.reader, querier.credential)
	if _, err := indexer.BeginIndex(context.Background(), scope, "operation-1", "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("changed configuration status=%s err=%v", status.Code(err), err)
	}
}

func TestAzureSearchIndexerRejectsReplacedConfiguration(t *testing.T) {
	configuration, scope, querier := newAzureSearchFixture(t)
	kubernetesClient := querier.reader.(client.Client)
	if err := kubernetesClient.Delete(context.Background(), configuration); err != nil {
		t.Fatal(err)
	}
	replacement := configuration.DeepCopy()
	replacement.SetResourceVersion("")
	replacement.SetUID("replacement-uid")
	if err := kubernetesClient.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	indexer := newAzureSearchIndexer(querier.reader, querier.credential)
	if _, err := indexer.BeginIndex(context.Background(), scope, "operation-1", "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("replaced configuration status=%s err=%v", status.Code(err), err)
	}
}

func TestAzureSearchIndexerRejectsDeletedConfiguration(t *testing.T) {
	configuration, scope, querier := newAzureSearchFixture(t)
	if err := querier.reader.(client.Client).Delete(context.Background(), configuration); err != nil {
		t.Fatal(err)
	}
	indexer := newAzureSearchIndexer(querier.reader, querier.credential)
	if _, err := indexer.BeginIndex(context.Background(), scope, "operation-1", "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("deleted configuration status=%s err=%v", status.Code(err), err)
	}
}
