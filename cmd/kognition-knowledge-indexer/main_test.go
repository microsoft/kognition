// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/protobuf/proto"
)

type fakeIndexClient struct {
	response *protocol.KnowledgeIndexResponse
	batches  []*protocol.KnowledgeIndexBatchRequest
}

func (client *fakeIndexClient) Index(_ context.Context, batches []*protocol.KnowledgeIndexBatchRequest) (*protocol.KnowledgeIndexResponse, error) {
	client.batches = batches
	return proto.Clone(client.response).(*protocol.KnowledgeIndexResponse), nil
}

func validTransportConfig() transportConfig {
	return transportConfig{
		ProviderName: "azuresearch", ProviderAddress: "provider:9443", OperationID: "operation-1", Dimension: 2,
		ClassUID: "class-uid", ClassGeneration: 7,
		Scope: &protocol.KnowledgeScope{
			Namespace: "tenant-a", KnowledgeBaseUid: "kb-uid", Generation: 4,
			ConfigurationApiGroup: "knowledge.kognition.dev", ConfigurationKind: "AzureSearchBackend",
			ConfigurationName: "search", ConfigurationUid: "config-uid", ConfigurationDigest: strings.Repeat("a", 64),
		},
	}
}

func validSpool() string {
	return `{"type":"chunk","documentId":"document-a","chunkId":"00000000","content":"content","sourceUri":"inline://doc","location":"","vector":[0.1,0.2]}` + "\n" +
		`{"type":"summary","sourceDocuments":2,"indexedChunks":1,"sourceDigest":"sha256:` + strings.Repeat("b", 64) + `"}` + "\n"
}

func TestTransportEmitsExactProviderConfirmation(t *testing.T) {
	config := validTransportConfig()
	client := &fakeIndexClient{response: &protocol.KnowledgeIndexResponse{
		Scope: proto.Clone(config.Scope).(*protocol.KnowledgeScope), OperationId: config.OperationID,
		SourceDigest: "sha256:" + strings.Repeat("b", 64), DocumentCount: 1, IndexedChunks: 1, BackendOperationId: "azure-operation-1",
	}}
	result, err := transport(context.Background(), config, strings.NewReader(validSpool()), client)
	if err != nil {
		t.Fatal(err)
	}
	if result.Documents != 2 || result.Chunks != 1 || result.BackendBinding.ProviderName != "azuresearch" || result.BackendBinding.ClassUID != "class-uid" ||
		result.ProviderConfirmation.DocumentCount != 1 || result.ProviderConfirmation.BackendOperationID != "azure-operation-1" || len(client.batches) != 1 {
		t.Fatalf("transport result = %#v", result)
	}
}

func TestTransportRejectsMismatchedConfirmation(t *testing.T) {
	config := validTransportConfig()
	response := &protocol.KnowledgeIndexResponse{
		Scope: proto.Clone(config.Scope).(*protocol.KnowledgeScope), OperationId: "other-operation",
		SourceDigest: "sha256:" + strings.Repeat("b", 64), DocumentCount: 1, IndexedChunks: 1, BackendOperationId: "azure-operation-1",
	}
	if _, err := transport(context.Background(), config, strings.NewReader(validSpool()), &fakeIndexClient{response: response}); err == nil {
		t.Fatal("transport() error = nil")
	}
}

func TestLoadTransportConfigFailsClosed(t *testing.T) {
	config := validTransportConfig()
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		content string
	}{
		{name: "unknown field", content: strings.TrimSuffix(string(encoded), "}") + `,"extra":true}`},
		{name: "trailing document", content: string(encoded) + `{}`},
		{name: "incomplete", content: `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadTransportConfig(path); err == nil {
				t.Fatal("loadTransportConfig() error = nil")
			}
		})
	}
}
