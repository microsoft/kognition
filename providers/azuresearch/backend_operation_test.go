// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuresearch

import (
	"encoding/base64"
	"strings"
	"testing"

	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/protobuf/proto"
)

func TestBackendOperationRoundTrip(t *testing.T) {
	scope := cleanupRequestScope()
	spec := BackendSpec{ServiceName: "kognition-search", IndexName: "knowledge_chunks", EmbeddingDimensions: 3, Authentication: AuthenticationConfig{Mode: "workload-identity"}}
	encoded, err := encodeBackendOperation(spec, scope, scope.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeBackendOperation(encoded, scope)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ServiceName != spec.ServiceName || decoded.IndexName != spec.IndexName {
		t.Fatalf("decoded backend = %+v", decoded)
	}
}

func TestBackendOperationRejectsReplayAndMalformedPayloads(t *testing.T) {
	scope := cleanupRequestScope()
	spec := BackendSpec{ServiceName: "kognition-search", IndexName: "knowledge_chunks", EmbeddingDimensions: 3, Authentication: AuthenticationConfig{Mode: "workload-identity"}}
	encoded, err := encodeBackendOperation(spec, scope, scope.OperationId)
	if err != nil {
		t.Fatal(err)
	}
	foreignScope := proto.CloneOf(scope)
	foreignScope.OperationId = "other-operation"
	if _, err := decodeBackendOperation(encoded, foreignScope); err == nil {
		t.Fatal("operation replay was accepted")
	}
	foreignScope = proto.CloneOf(scope)
	foreignScope.ConfigurationDigest = strings.Repeat("b", 64)
	if _, err := decodeBackendOperation(encoded, foreignScope); err == nil {
		t.Fatal("configuration replay was accepted")
	}
	for _, malformed := range []string{
		"v2." + strings.TrimPrefix(encoded, "v1."),
		"v1.invalid!",
		"v1." + base64.RawURLEncoding.EncodeToString([]byte(`{"serviceName":"kognition-search","indexName":"knowledge_chunks","operationId":"index-operation-2","configurationDigest":"`+strings.Repeat("a", 64)+`","extra":true}`)),
		"v1." + base64.RawURLEncoding.EncodeToString([]byte(`{"serviceName":"kognition-search","indexName":"knowledge_chunks","operationId":"index-operation-2","configurationDigest":"`+strings.Repeat("a", 64)+`"} trailing`)),
	} {
		if _, err := decodeBackendOperation(malformed, scope); err == nil {
			t.Fatalf("malformed locator was accepted: %q", malformed)
		}
	}
}

func cleanupRequestScope() *protocol.KnowledgeScope {
	return &protocol.KnowledgeScope{
		Namespace: "tenant-a", KnowledgeBaseUid: "kb-uid", Generation: 2, OperationId: "index-operation-2",
		ConfigurationApiGroup: APIGroup, ConfigurationKind: Kind, ConfigurationName: "shared", ConfigurationUid: "config-uid",
		ConfigurationDigest: strings.Repeat("a", 64),
	}
}
