// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingestion_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/ingestion"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

func providerScope() *protocol.KnowledgeScope {
	return &protocol.KnowledgeScope{
		Namespace: "tenant-a", KnowledgeBaseUid: "kb-uid", Generation: 4,
		ConfigurationApiGroup: "knowledge.kognition.dev", ConfigurationKind: "AzureSearchBackend",
		ConfigurationName: "search", ConfigurationUid: "config-uid", ConfigurationDigest: strings.Repeat("a", 64),
	}
}

func providerSpool(chunkLines []string, sourceDocuments, indexedChunks int, digest string) string {
	return strings.Join(append(chunkLines, fmt.Sprintf(`{"type":"summary","sourceDocuments":%d,"indexedChunks":%d,"sourceDigest":%q}`, sourceDocuments, indexedChunks, digest)), "\n") + "\n"
}

func providerChunk(documentID, chunkID string, vector string) string {
	return fmt.Sprintf(`{"type":"chunk","documentId":%q,"chunkId":%q,"content":"content","sourceUri":"inline://doc","location":"","vector":%s}`, documentID, chunkID, vector)
}

func readProviderSpool(t *testing.T, spool string, mutate func(*ingestion.ProviderSpoolOptions)) (*ingestion.PreparedProviderIndex, error) {
	t.Helper()
	options := ingestion.ProviderSpoolOptions{Scope: providerScope(), OperationID: "operation-1", ExpectedDimension: 2}
	if mutate != nil {
		mutate(&options)
	}
	return ingestion.ReadProviderSpool(strings.NewReader(spool), options)
}

func TestProviderSpoolProducesValidatedBatches(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	prepared, err := readProviderSpool(t, providerSpool([]string{
		providerChunk("document-a", "00000000", `[0.1,0.2]`),
		providerChunk("document-a", "00000001", `[0.3,0.4]`),
	}, 2, 2, digest), nil)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.SourceDocuments != 2 || len(prepared.Batches) != 1 || len(prepared.Batches[0].Chunks) != 2 || prepared.Batches[0].SourceDigest != digest || prepared.Batches[0].Scope.Generation != 4 {
		t.Fatalf("prepared index = %#v", prepared)
	}
}

func TestProviderSpoolSplitsBatchesAtProtocolMessageLimit(t *testing.T) {
	lines := make([]string, 100)
	content := strings.Repeat("x", 65536)
	for index := range lines {
		lines[index] = fmt.Sprintf(`{"type":"chunk","documentId":"document-a","chunkId":%q,"content":%q,"sourceUri":"inline://doc","location":"","vector":[0.1,0.2]}`, fmt.Sprintf("%08d", index), content)
	}
	prepared, err := readProviderSpool(t, providerSpool(lines, 1, 100, "sha256:"+strings.Repeat("b", 64)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Batches) < 2 {
		t.Fatalf("batch count = %d, want message-size split", len(prepared.Batches))
	}
	for index, batch := range prepared.Batches {
		if batch.Sequence != int32(index) || len(batch.Chunks) > 100 {
			t.Fatalf("batch %d = %#v", index, batch)
		}
	}
}

func TestProviderSpoolFailsClosed(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	valid := providerChunk("document-a", "00000000", `[0.1,0.2]`)
	tests := []struct {
		name   string
		spool  string
		mutate func(*ingestion.ProviderSpoolOptions)
	}{
		{name: "missing summary", spool: valid + "\n"},
		{name: "summary mismatch", spool: providerSpool([]string{valid}, 1, 2, digest)},
		{name: "summary not final", spool: providerSpool([]string{valid}, 1, 1, digest) + valid + "\n"},
		{name: "unknown field", spool: strings.Replace(valid, `"type":"chunk"`, `"type":"chunk","extra":true`, 1) + "\n"},
		{name: "dimension mismatch", spool: providerSpool([]string{valid}, 1, 1, digest), mutate: func(options *ingestion.ProviderSpoolOptions) { options.ExpectedDimension = 3 }},
		{name: "duplicate identity", spool: providerSpool([]string{valid, valid}, 1, 2, digest)},
		{name: "non finite vector", spool: providerSpool([]string{providerChunk("document-a", "00000000", `[1e999,0.2]`)}, 1, 1, digest)},
		{name: "no chunks", spool: providerSpool(nil, 1, 0, digest)},
		{name: "chunk limit", spool: providerSpool([]string{valid, providerChunk("document-a", "00000001", `[0.3,0.4]`)}, 1, 2, digest), mutate: func(options *ingestion.ProviderSpoolOptions) { options.MaxChunks = 1 }},
		{name: "byte limit", spool: providerSpool([]string{valid}, 1, 1, digest), mutate: func(options *ingestion.ProviderSpoolOptions) { options.MaxBytes = 20 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := readProviderSpool(t, test.spool, test.mutate); err == nil {
				t.Fatal("ReadProviderSpool() error = nil")
			}
		})
	}
}
