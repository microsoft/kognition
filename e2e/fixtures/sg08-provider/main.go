// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"os"
	"sync"

	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type fixture struct {
	mu        sync.Mutex
	expected  *protocol.KnowledgeScope
	committed *protocol.KnowledgeIndexResponse
}

type transaction struct {
	owner    *fixture
	response *protocol.KnowledgeIndexResponse
}

func (provider *fixture) authorize(_ context.Context, scope *protocol.KnowledgeScope) error {
	if !proto.Equal(scope, provider.expected) {
		return fmt.Errorf("scope does not match isolated fixture")
	}
	return nil
}

func (provider *fixture) BeginIndex(ctx context.Context, scope *protocol.KnowledgeScope, operation, digest string) (pluginsdk.KnowledgeIndexTransaction, error) {
	if err := provider.authorize(ctx, scope); err != nil {
		return nil, err
	}
	return &transaction{owner: provider, response: &protocol.KnowledgeIndexResponse{
		Scope: proto.CloneOf(scope), OperationId: operation, SourceDigest: digest,
		BackendOperationId: "sg08-local-commit",
	}}, nil
}

func (pending *transaction) WriteBatch(_ context.Context, batch *protocol.KnowledgeIndexBatchRequest) error {
	for _, chunk := range batch.Chunks {
		if chunk.Content != "SG08 provider-backed document." || len(chunk.Vector) != 8 {
			return fmt.Errorf("unexpected fixture document or embedding dimension")
		}
		pending.response.IndexedChunks++
	}
	return nil
}

func (pending *transaction) Commit(context.Context) (*protocol.KnowledgeIndexResponse, error) {
	if pending.response.IndexedChunks != 1 {
		return nil, fmt.Errorf("expected exactly one indexed chunk")
	}
	pending.response.DocumentCount = 1
	pending.owner.mu.Lock()
	defer pending.owner.mu.Unlock()
	pending.owner.committed = proto.CloneOf(pending.response)
	log.Printf("SG08 provider committed documents=1 chunks=1 dimension=8 operation=%s", pending.response.OperationId)
	return proto.CloneOf(pending.response), nil
}

func (*transaction) Abort(context.Context) error { return nil }

func (*fixture) Query(context.Context, *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
	return nil, fmt.Errorf("query is outside this indexing fixture")
}

func (provider *fixture) authorizeCleanup(_ context.Context, request *protocol.KnowledgeCleanupRequest) error {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.committed == nil {
		return fmt.Errorf("no committed fixture operation")
	}
	scope := proto.CloneOf(provider.committed.Scope)
	scope.OperationId = provider.committed.OperationId
	if !proto.Equal(scope, request.Scope) || request.BackendOperationId != provider.committed.BackendOperationId || request.ReclaimPolicy != protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE {
		return fmt.Errorf("cleanup does not match committed fixture operation")
	}
	return nil
}

func (provider *fixture) Cleanup(ctx context.Context, request *protocol.KnowledgeCleanupRequest) (*protocol.KnowledgeCleanupResponse, error) {
	if err := provider.authorizeCleanup(ctx, request); err != nil {
		return nil, err
	}
	provider.mu.Lock()
	provider.committed = nil
	provider.mu.Unlock()
	log.Printf("SG08 provider cleaned operation=%s", request.Scope.OperationId)
	return &protocol.KnowledgeCleanupResponse{Scope: proto.CloneOf(request.Scope), CleanupId: request.CleanupId,
		ReclaimPolicy: request.ReclaimPolicy, BackendOperationId: "sg08-local-cleanup"}, nil
}

func main() {
	certificate, err := tls.LoadX509KeyPair("/fixture/tls.crt", "/fixture/tls.key")
	if err != nil {
		log.Fatal(err)
	}
	ca, err := os.ReadFile("/fixture/ca.crt")
	if err != nil {
		log.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		log.Fatal("invalid fixture CA")
	}
	provider := &fixture{expected: &protocol.KnowledgeScope{}}
	if err := protojson.Unmarshal([]byte(os.Getenv("SG08_EXPECTED_SCOPE")), provider.expected); err != nil {
		log.Fatal(err)
	}
	server, err := pluginsdk.NewKnowledgeServer(pluginsdk.KnowledgeServerConfig{
		Name: "sg08-local", Version: "1.0.0", TLS: pluginsdk.KnowledgeTLSConfig{Certificate: certificate, Roots: roots, PeerURI: "spiffe://kognition.test/core/sg08"},
		Capabilities: &protocol.KnowledgeProviderCapabilities{ProtocolVersion: pluginsdk.KnowledgeProtocolVersion,
			ConfigurationKinds: []*protocol.KnowledgeConfigurationKind{{ApiGroup: "azuresearch.knowledge.kognition.dev", Kind: "AzureSearchBackend"}}, Operations: []string{"index", "cleanup-delete"}},
		AuthorizeQueryScope: provider.authorize, AuthorizeIndexScope: provider.authorize, AuthorizeCleanup: provider.authorizeCleanup,
		Querier: provider, Indexer: provider, Cleaner: provider, Probe: func(context.Context) error { return nil },
	})
	if err != nil {
		log.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", ":9443")
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(server.Serve(listener))
}
