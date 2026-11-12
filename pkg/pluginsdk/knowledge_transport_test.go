// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pluginsdk_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

const coreIdentity = "spiffe://kognition.test/core/knowledge"
const providerIdentity = "spiffe://kognition.test/providers/pgvector"

type knowledgeQueryFunc func(context.Context, *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error)

func (query knowledgeQueryFunc) Query(ctx context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
	return query(ctx, request)
}

type knowledgeCleanupFunc func(context.Context, *protocol.KnowledgeCleanupRequest) (*protocol.KnowledgeCleanupResponse, error)

func (cleanup knowledgeCleanupFunc) Cleanup(ctx context.Context, request *protocol.KnowledgeCleanupRequest) (*protocol.KnowledgeCleanupResponse, error) {
	return cleanup(ctx, request)
}

type recordingKnowledgeIndexer struct {
	batches  []*protocol.KnowledgeIndexBatchRequest
	commits  atomic.Int32
	aborts   atomic.Int32
	response *protocol.KnowledgeIndexResponse
}

func (indexer *recordingKnowledgeIndexer) BeginIndex(_ context.Context, scope *protocol.KnowledgeScope, operationID, sourceDigest string) (pluginsdk.KnowledgeIndexTransaction, error) {
	indexer.response = &protocol.KnowledgeIndexResponse{Scope: proto.Clone(scope).(*protocol.KnowledgeScope), OperationId: operationID,
		SourceDigest: sourceDigest, DocumentCount: 2, IndexedChunks: 2, BackendOperationId: "azure-operation-1"}
	return indexer, nil
}

func (indexer *recordingKnowledgeIndexer) WriteBatch(_ context.Context, batch *protocol.KnowledgeIndexBatchRequest) error {
	indexer.batches = append(indexer.batches, proto.Clone(batch).(*protocol.KnowledgeIndexBatchRequest))
	return nil
}

func (indexer *recordingKnowledgeIndexer) Commit(context.Context) (*protocol.KnowledgeIndexResponse, error) {
	indexer.commits.Add(1)
	return proto.Clone(indexer.response).(*protocol.KnowledgeIndexResponse), nil
}

func (indexer *recordingKnowledgeIndexer) Abort(context.Context) error {
	indexer.aborts.Add(1)
	return nil
}

type knowledgeCertificates struct {
	roots    *x509.CertPool
	core     tls.Certificate
	provider tls.Certificate
	other    tls.Certificate
}

func newKnowledgeCertificates(t *testing.T) knowledgeCertificates {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "knowledge test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	issue := func(serial int64, identity string) tls.Certificate {
		privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, err := url.Parse(identity)
		if err != nil {
			t.Fatal(err)
		}
		certificate := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter,
			DNSNames: []string{"knowledge.test"}, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		encoded, err := x509.CreateCertificate(rand.Reader, certificate, ca, &privateKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{encoded}, PrivateKey: privateKey}
	}
	return knowledgeCertificates{roots: roots, core: issue(2, coreIdentity), provider: issue(3, providerIdentity), other: issue(4, "spiffe://kognition.test/tenant/agent")}
}

func knowledgeCapabilities() *protocol.KnowledgeProviderCapabilities {
	return &protocol.KnowledgeProviderCapabilities{ProtocolVersion: pluginsdk.KnowledgeProtocolVersion,
		ConfigurationKinds: []*protocol.KnowledgeConfigurationKind{{ApiGroup: "pgvector.knowledge.kognition.dev", Kind: "PgvectorBackend"}},
		Operations:         []string{"keyword", "dense", "hybrid", "filter", "citations"}}
}

func knowledgeResult(request *protocol.KnowledgeQueryRequest) *protocol.KnowledgeQueryResponse {
	return &protocol.KnowledgeQueryResponse{Scope: proto.Clone(request.Scope).(*protocol.KnowledgeScope), RequestId: request.RequestId,
		Matches: []*protocol.KnowledgeMatch{{DocumentId: "document-1", ChunkId: "chunk-1", Content: "Scoped knowledge result", Score: 0.75, SourceUri: "https://example.test/manual", Location: "page 3"}}}
}

func knowledgeServerConfig(certificates knowledgeCertificates, query knowledgeQueryFunc) pluginsdk.KnowledgeServerConfig {
	return pluginsdk.KnowledgeServerConfig{Name: "pgvector", Version: "1.0.0", Capabilities: knowledgeCapabilities(), Querier: query,
		TLS:   pluginsdk.KnowledgeTLSConfig{Certificate: certificates.provider, Roots: certificates.roots, PeerURI: coreIdentity},
		Probe: func(context.Context) error { return nil },
		AuthorizeQueryScope: func(_ context.Context, scope *protocol.KnowledgeScope) error {
			if !proto.Equal(scope, validKnowledgeQuery().Scope) {
				return status.Error(codes.PermissionDenied, "private binding details")
			}
			return nil
		}}
}

func serveKnowledge(t *testing.T, config pluginsdk.KnowledgeServerConfig) (string, *grpc.Server) {
	t.Helper()
	server, err := pluginsdk.NewKnowledgeServer(config)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close(); <-stopped })
	return listener.Addr().String(), server
}

func newKnowledgeClient(t *testing.T, address string, certificates knowledgeCertificates) *pluginsdk.KnowledgeClient {
	t.Helper()
	client, err := pluginsdk.NewKnowledgeClient(pluginsdk.KnowledgeClientConfig{Name: "pgvector", Address: address,
		TLS: pluginsdk.KnowledgeTLSConfig{Certificate: certificates.core, Roots: certificates.roots, PeerURI: providerIdentity, ServerName: "knowledge.test"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestKnowledgeTransportScopedQuery(t *testing.T) {
	certificates := newKnowledgeCertificates(t)
	var calls atomic.Int32
	config := knowledgeServerConfig(certificates, func(ctx context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
		calls.Add(1)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 30*time.Second {
			return nil, errors.New("unbounded deadline")
		}
		return knowledgeResult(request), nil
	})
	address, server := serveKnowledge(t, config)
	config.Capabilities.Operations[0] = "removed"
	client := newKnowledgeClient(t, address, certificates)
	request := validKnowledgeQuery()
	request.Filters = []*protocol.KnowledgeFilter{{Key: "metadata.category", Value: "manual"}}
	response, err := client.Query(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(response, knowledgeResult(request)) || calls.Load() != 1 {
		t.Fatalf("unexpected response: %v", response)
	}
	server.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Query(ctx, request); err == nil {
		t.Fatal("removed provider remained available")
	}
}

func TestKnowledgeTransportScopedIndex(t *testing.T) {
	certificates := newKnowledgeCertificates(t)
	indexer := &recordingKnowledgeIndexer{}
	config := knowledgeServerConfig(certificates, func(_ context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
		return knowledgeResult(request), nil
	})
	config.Capabilities.Operations = append(config.Capabilities.Operations, "index")
	config.Indexer = indexer
	config.AuthorizeQueryScope = func(context.Context, *protocol.KnowledgeScope) error {
		return status.Error(codes.PermissionDenied, "query authorization must not authorize indexing")
	}
	config.AuthorizeIndexScope = func(_ context.Context, scope *protocol.KnowledgeScope) error {
		if !proto.Equal(scope, validKnowledgeIndexBatch().Scope) {
			return status.Error(codes.PermissionDenied, "private pending binding details")
		}
		return nil
	}
	address, _ := serveKnowledge(t, config)
	client := newKnowledgeClient(t, address, certificates)
	first := validKnowledgeIndexBatch()
	second := proto.Clone(first).(*protocol.KnowledgeIndexBatchRequest)
	second.Sequence = 1
	second.Chunks[0].DocumentId = "document-2"
	second.Chunks[0].ChunkId = "chunk-2"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := client.Index(ctx, []*protocol.KnowledgeIndexBatchRequest{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(indexer.batches) != 2 || indexer.commits.Load() != 1 || indexer.aborts.Load() != 0 {
		t.Fatalf("provider calls: batches=%d commits=%d aborts=%d", len(indexer.batches), indexer.commits.Load(), indexer.aborts.Load())
	}
	if !proto.Equal(response.Scope, first.Scope) || response.OperationId != first.OperationId || response.SourceDigest != first.SourceDigest || response.IndexedChunks != 2 {
		t.Fatalf("unexpected index confirmation: %+v", response)
	}
	first.Chunks[0].Content = "mutated after transport"
	if indexer.batches[0].Chunks[0].Content == first.Chunks[0].Content {
		t.Fatal("provider retained caller-owned index batch")
	}
}

func TestKnowledgeTransportIndexServerDeadline(t *testing.T) {
	for _, sendBatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("active-transaction=%t", sendBatch), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				certificates := newKnowledgeCertificates(t)
				indexer := &recordingKnowledgeIndexer{}
				config := knowledgeServerConfig(certificates, func(_ context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
					return knowledgeResult(request), nil
				})
				config.Capabilities.Operations = append(config.Capabilities.Operations, "index")
				config.Indexer = indexer
				config.AuthorizeIndexScope = func(context.Context, *protocol.KnowledgeScope) error { return nil }
				server, err := pluginsdk.NewKnowledgeServer(config)
				if err != nil {
					t.Fatal(err)
				}
				listener := bufconn.Listen(knowledgeTestBufferSize)
				stopped := make(chan struct{})
				go func() { defer close(stopped); _ = server.Serve(listener) }()
				defer func() { server.Stop(); _ = listener.Close(); <-stopped }()
				connection, err := grpc.NewClient("passthrough:///knowledge.test",
					grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
					grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
						MinVersion: tls.VersionTLS13, RootCAs: certificates.roots, ServerName: "knowledge.test",
						Certificates: []tls.Certificate{certificates.core},
					})))
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				safety := time.AfterFunc(11*time.Minute, cancel)
				defer safety.Stop()
				stream, err := protocol.NewKnowledgeExtensionClient(connection).Index(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if sendBatch {
					if err := stream.Send(validKnowledgeIndexBatch()); err != nil {
						t.Fatal(err)
					}
				}
				if err := stream.RecvMsg(&protocol.KnowledgeIndexResponse{}); status.Code(err) != codes.DeadlineExceeded {
					t.Fatalf("idle stream: code=%s error=%v", status.Code(err), err)
				}
				if ctx.Err() != nil {
					t.Fatal("client cancellation, not the server deadline, ended the stream")
				}
				synctest.Wait()
				wantAborts := int32(0)
				if sendBatch {
					wantAborts = 1
				}
				if indexer.aborts.Load() != wantAborts || indexer.commits.Load() != 0 {
					t.Fatalf("aborts=%d commits=%d", indexer.aborts.Load(), indexer.commits.Load())
				}
			})
		})
	}
}

const knowledgeTestBufferSize = 1024 * 1024

func TestKnowledgeTransportScopedCleanup(t *testing.T) {
	certificates := newKnowledgeCertificates(t)
	var authorized, cleaned *protocol.KnowledgeCleanupRequest
	config := knowledgeServerConfig(certificates, func(_ context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
		return knowledgeResult(request), nil
	})
	config.Capabilities.Operations = append(config.Capabilities.Operations, "cleanup-delete", "cleanup-retain")
	config.AuthorizeCleanup = func(_ context.Context, request *protocol.KnowledgeCleanupRequest) error {
		authorized = request
		request.Scope.Namespace = "authorization-copy"
		return nil
	}
	config.Cleaner = knowledgeCleanupFunc(func(_ context.Context, request *protocol.KnowledgeCleanupRequest) (*protocol.KnowledgeCleanupResponse, error) {
		cleaned = request
		return &protocol.KnowledgeCleanupResponse{Scope: proto.Clone(request.Scope).(*protocol.KnowledgeScope), CleanupId: request.CleanupId,
			ReclaimPolicy: request.ReclaimPolicy, BackendOperationId: "azure-cleanup-1"}, nil
	})
	address, _ := serveKnowledge(t, config)
	client := newKnowledgeClient(t, address, certificates)
	request := validKnowledgeCleanup()
	response, err := client.Cleanup(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if authorized == nil || cleaned == nil || authorized == cleaned || cleaned.Scope.Namespace != request.Scope.Namespace || !proto.Equal(response.Scope, request.Scope) {
		t.Fatalf("cleanup request was not isolated: authorized=%+v cleaned=%+v response=%+v", authorized, cleaned, response)
	}
	request.Scope.Namespace = "caller-copy"
	if cleaned.Scope.Namespace == request.Scope.Namespace {
		t.Fatal("provider retained caller-owned cleanup request")
	}
}

func TestKnowledgeTransportRejectsCleanupConfirmationMismatch(t *testing.T) {
	certificates := newKnowledgeCertificates(t)
	config := knowledgeServerConfig(certificates, func(_ context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
		return knowledgeResult(request), nil
	})
	config.Capabilities.Operations = append(config.Capabilities.Operations, "cleanup-delete")
	config.AuthorizeCleanup = func(context.Context, *protocol.KnowledgeCleanupRequest) error { return nil }
	config.Cleaner = knowledgeCleanupFunc(func(_ context.Context, request *protocol.KnowledgeCleanupRequest) (*protocol.KnowledgeCleanupResponse, error) {
		return &protocol.KnowledgeCleanupResponse{Scope: proto.Clone(request.Scope).(*protocol.KnowledgeScope), CleanupId: "wrong",
			ReclaimPolicy: request.ReclaimPolicy, BackendOperationId: "azure-cleanup-1"}, nil
	})
	address, _ := serveKnowledge(t, config)
	client := newKnowledgeClient(t, address, certificates)
	if _, err := client.Cleanup(context.Background(), validKnowledgeCleanup()); status.Code(err) != codes.DataLoss {
		t.Fatalf("code=%s error=%v", status.Code(err), err)
	}
}

func TestKnowledgeTransportRejectsBeforeBackend(t *testing.T) {
	certificates := newKnowledgeCertificates(t)
	for _, test := range []struct {
		name   string
		mutate func(*protocol.KnowledgeQueryRequest, *pluginsdk.KnowledgeServerConfig)
		code   codes.Code
	}{
		{"namespace", func(request *protocol.KnowledgeQueryRequest, _ *pluginsdk.KnowledgeServerConfig) {
			request.Scope.Namespace = "tenant-b"
		}, codes.PermissionDenied},
		{"uid", func(request *protocol.KnowledgeQueryRequest, _ *pluginsdk.KnowledgeServerConfig) {
			request.Scope.KnowledgeBaseUid = "other"
		}, codes.PermissionDenied},
		{"generation", func(request *protocol.KnowledgeQueryRequest, _ *pluginsdk.KnowledgeServerConfig) {
			request.Scope.Generation++
		}, codes.PermissionDenied},
		{"config uid", func(request *protocol.KnowledgeQueryRequest, _ *pluginsdk.KnowledgeServerConfig) {
			request.Scope.ConfigurationUid = "recreated"
		}, codes.PermissionDenied},
		{"config digest", func(request *protocol.KnowledgeQueryRequest, _ *pluginsdk.KnowledgeServerConfig) {
			request.Scope.ConfigurationDigest = strings.Repeat("b", 64)
		}, codes.PermissionDenied},
		{"config name", func(request *protocol.KnowledgeQueryRequest, _ *pluginsdk.KnowledgeServerConfig) {
			request.Scope.ConfigurationName = "other"
		}, codes.PermissionDenied},
		{"config kind", func(request *protocol.KnowledgeQueryRequest, _ *pluginsdk.KnowledgeServerConfig) {
			request.Scope.ConfigurationKind = "OtherBackend"
		}, codes.FailedPrecondition},
		{"capability", func(_ *protocol.KnowledgeQueryRequest, config *pluginsdk.KnowledgeServerConfig) {
			config.Capabilities.Operations = []string{"dense", "citations"}
		}, codes.FailedPrecondition},
		{"filter capability", func(request *protocol.KnowledgeQueryRequest, config *pluginsdk.KnowledgeServerConfig) {
			request.Filters = []*protocol.KnowledgeFilter{{Key: "metadata.category"}}
			config.Capabilities.Operations = []string{"keyword", "citations"}
		}, codes.FailedPrecondition},
		{"provider name", func(_ *protocol.KnowledgeQueryRequest, config *pluginsdk.KnowledgeServerConfig) {
			config.Name = "other"
		}, codes.FailedPrecondition},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			config := knowledgeServerConfig(certificates, func(_ context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
				calls.Add(1)
				return knowledgeResult(request), nil
			})
			request := validKnowledgeQuery()
			test.mutate(request, &config)
			address, _ := serveKnowledge(t, config)
			client := newKnowledgeClient(t, address, certificates)
			_, err := client.Query(context.Background(), request)
			if status.Code(err) != test.code || calls.Load() != 0 {
				t.Fatalf("code=%v calls=%d error=%v", status.Code(err), calls.Load(), err)
			}
			if strings.Contains(err.Error(), "private binding") {
				t.Fatal("authorization detail leaked")
			}
		})
	}
}

func TestKnowledgeTransportRejectsPeerIdentity(t *testing.T) {
	certificates := newKnowledgeCertificates(t)
	for _, test := range []struct {
		name   string
		mutate func(*pluginsdk.KnowledgeClientConfig)
	}{
		{"wrong client", func(config *pluginsdk.KnowledgeClientConfig) { config.TLS.Certificate = certificates.other }},
		{"wrong provider", func(config *pluginsdk.KnowledgeClientConfig) {
			config.TLS.PeerURI = "spiffe://kognition.test/providers/other"
		}},
		{"wrong DNS identity", func(config *pluginsdk.KnowledgeClientConfig) { config.TLS.ServerName = "wrong.test" }},
		{"untrusted certificate", func(config *pluginsdk.KnowledgeClientConfig) { config.TLS.Roots = x509.NewCertPool() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			address, _ := serveKnowledge(t, knowledgeServerConfig(certificates, func(_ context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
				calls.Add(1)
				return knowledgeResult(request), nil
			}))
			config := pluginsdk.KnowledgeClientConfig{Name: "pgvector", Address: address, TLS: pluginsdk.KnowledgeTLSConfig{Certificate: certificates.core, Roots: certificates.roots, PeerURI: providerIdentity, ServerName: "knowledge.test"}}
			test.mutate(&config)
			client, err := pluginsdk.NewKnowledgeClient(config)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := client.Query(ctx, validKnowledgeQuery()); err == nil || calls.Load() != 0 {
				t.Fatalf("untrusted peer reached backend: %v", err)
			}
		})
	}
}

func TestKnowledgeTransportValidatesBackendResponse(t *testing.T) {
	certificates := newKnowledgeCertificates(t)
	for _, test := range []struct {
		name   string
		mutate func(*protocol.KnowledgeQueryResponse)
	}{
		{"scope mismatch", func(response *protocol.KnowledgeQueryResponse) { response.Scope.Namespace = "tenant-b" }},
		{"request mismatch", func(response *protocol.KnowledgeQueryResponse) { response.RequestId = "other" }},
		{"missing citation", func(response *protocol.KnowledgeQueryResponse) { response.Matches[0].SourceUri = "" }},
		{"nonfinite score", func(response *protocol.KnowledgeQueryResponse) { response.Matches[0].Score = math.Inf(1) }},
		{"oversized content", func(response *protocol.KnowledgeQueryResponse) {
			response.Matches[0].Content = strings.Repeat("a", 65537)
		}},
		{"too many results", func(response *protocol.KnowledgeQueryResponse) {
			response.Matches = []*protocol.KnowledgeMatch{response.Matches[0], response.Matches[0], response.Matches[0], response.Matches[0]}
		}},
		{"duplicate result", func(response *protocol.KnowledgeQueryResponse) {
			response.Matches = append(response.Matches, response.Matches[0])
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			address, _ := serveKnowledge(t, knowledgeServerConfig(certificates, func(_ context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
				response := knowledgeResult(request)
				test.mutate(response)
				return response, nil
			}))
			client := newKnowledgeClient(t, address, certificates)
			if response, err := client.Query(context.Background(), validKnowledgeQuery()); status.Code(err) != codes.DataLoss || response != nil {
				t.Fatalf("accepted invalid result: %v %v", response, err)
			}
		})
	}
}

func TestKnowledgeTransportDeadlineAndErrors(t *testing.T) {
	certificates := newKnowledgeCertificates(t)
	t.Run("deadline", func(t *testing.T) {
		entered := make(chan struct{})
		address, _ := serveKnowledge(t, knowledgeServerConfig(certificates, func(ctx context.Context, _ *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}))
		client := newKnowledgeClient(t, address, certificates)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		finished := make(chan error, 1)
		go func() { _, err := client.Query(ctx, validKnowledgeQuery()); finished <- err }()
		select {
		case <-entered:
			cancel()
		case err := <-finished:
			t.Fatalf("query did not reach backend: %v", err)
		}
		if err := <-finished; status.Code(err) != codes.Canceled {
			t.Fatalf("expected cancellation, got %v", err)
		}
	})
	t.Run("sanitized backend error", func(t *testing.T) {
		address, _ := serveKnowledge(t, knowledgeServerConfig(certificates, func(context.Context, *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
			return nil, status.Error(codes.Unavailable, "postgres://user:secret@private-host")
		}))
		client := newKnowledgeClient(t, address, certificates)
		_, err := client.Query(context.Background(), validKnowledgeQuery())
		if status.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private-host") {
			t.Fatalf("unsafe error: %v", err)
		}
	})
}

func TestKnowledgeServerCannotBypassValidation(t *testing.T) {
	certificates := newKnowledgeCertificates(t)
	var calls atomic.Int32
	config := knowledgeServerConfig(certificates, func(_ context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
		calls.Add(1)
		return knowledgeResult(request), nil
	})
	config.Probe = func(context.Context) error { return status.Error(codes.Unavailable, "private database outage") }
	address, _ := serveKnowledge(t, config)
	connection, err := grpc.NewClient("passthrough:///"+address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: certificates.roots, Certificates: []tls.Certificate{certificates.core}, ServerName: "knowledge.test"})))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request := validKnowledgeQuery()
	request.TopK = 101
	if _, err := protocol.NewKnowledgeExtensionClient(connection).Query(ctx, request); status.Code(err) != codes.InvalidArgument || calls.Load() != 0 {
		t.Fatalf("server accepted invalid query: %v", err)
	}
	request = validKnowledgeQuery()
	request.Scope.ConfigurationKind = "OtherBackend"
	if _, err := protocol.NewKnowledgeExtensionClient(connection).Query(ctx, request); status.Code(err) != codes.FailedPrecondition || calls.Load() != 0 {
		t.Fatalf("server accepted unsupported config: %v", err)
	}
	if response, err := protocol.NewLifecycleClient(connection).HealthCheck(ctx, &protocol.HealthCheckRequest{}); status.Code(err) != codes.Unavailable || response.GetReady() {
		t.Fatalf("unhealthy adapter reported ready: %v %v", response, err)
	}
}

func TestKnowledgeTransportRequiresSecureConfiguration(t *testing.T) {
	certificates := newKnowledgeCertificates(t)
	config := knowledgeServerConfig(certificates, func(_ context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
		return knowledgeResult(request), nil
	})
	config.AuthorizeQueryScope = nil
	if _, err := pluginsdk.NewKnowledgeServer(config); err == nil {
		t.Fatal("server accepted missing authorization")
	}
	config = knowledgeServerConfig(certificates, func(_ context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
		return knowledgeResult(request), nil
	})
	config.Capabilities.Operations = append(config.Capabilities.Operations, "index")
	config.Indexer = &recordingKnowledgeIndexer{}
	if _, err := pluginsdk.NewKnowledgeServer(config); err == nil {
		t.Fatal("server accepted index capability without index authorization")
	}
	for _, mutate := range []func(*pluginsdk.KnowledgeClientConfig){
		func(config *pluginsdk.KnowledgeClientConfig) { config.TLS.Roots = nil },
		func(config *pluginsdk.KnowledgeClientConfig) { config.TLS.Certificate = tls.Certificate{} },
		func(config *pluginsdk.KnowledgeClientConfig) { config.TLS.PeerURI = "" },
		func(config *pluginsdk.KnowledgeClientConfig) { config.TLS.ServerName = "" },
		func(config *pluginsdk.KnowledgeClientConfig) { config.Address = "dns:///tenant-controlled" },
	} {
		clientConfig := pluginsdk.KnowledgeClientConfig{Name: "pgvector", Address: "localhost:1234", TLS: pluginsdk.KnowledgeTLSConfig{Certificate: certificates.core, Roots: certificates.roots, PeerURI: providerIdentity, ServerName: "knowledge.test"}}
		mutate(&clientConfig)
		if client, err := pluginsdk.NewKnowledgeClient(clientConfig); err == nil {
			client.Close()
			t.Fatal("accepted insecure transport configuration")
		}
	}
}

func TestKnowledgeTransportRejectsMissingClientCertificate(t *testing.T) {
	certificates := newKnowledgeCertificates(t)
	var calls atomic.Int32
	address, _ := serveKnowledge(t, knowledgeServerConfig(certificates, func(_ context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
		calls.Add(1)
		return knowledgeResult(request), nil
	}))
	connection, err := grpc.NewClient("passthrough:///"+address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: certificates.roots, ServerName: "knowledge.test"})))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := protocol.NewKnowledgeExtensionClient(connection).Query(ctx, validKnowledgeQuery()); err == nil || calls.Load() != 0 {
		t.Fatalf("anonymous client reached backend: %v", err)
	}
}

type faultyKnowledgeProvider struct {
	protocol.UnimplementedIdentityServer
	protocol.UnimplementedKnowledgeExtensionServer
	capabilities *protocol.KnowledgeProviderCapabilities
	query        knowledgeQueryFunc
}

func TestKnowledgeTransportSanitizesWrappedContextErrors(t *testing.T) {
	certificates := newKnowledgeCertificates(t)
	for _, test := range []struct {
		name  string
		cause error
		code  codes.Code
	}{
		{"canceled", context.Canceled, codes.Canceled},
		{"deadline", context.DeadlineExceeded, codes.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := fmt.Errorf("postgres://user:secret@private-host: %w", test.cause)
			address, _ := serveKnowledge(t, knowledgeServerConfig(certificates, func(context.Context, *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
				return nil, failure
			}))
			connection, err := grpc.NewClient("passthrough:///"+address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
				MinVersion: tls.VersionTLS13, RootCAs: certificates.roots, Certificates: []tls.Certificate{certificates.core}, ServerName: "knowledge.test"})))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err = protocol.NewKnowledgeExtensionClient(connection).Query(ctx, validKnowledgeQuery())
			if status.Code(err) != test.code || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private-host") {
				t.Fatalf("unsafe context error: %v", err)
			}
		})
	}
}

func (provider *faultyKnowledgeProvider) GetProviderInfo(context.Context, *protocol.GetProviderInfoRequest) (*protocol.GetProviderInfoResponse, error) {
	return &protocol.GetProviderInfoResponse{Name: "pgvector", Dimension: "knowledge"}, nil
}

func (provider *faultyKnowledgeProvider) GetProviderCapabilities(context.Context, *protocol.GetProviderCapabilitiesRequest) (*protocol.GetProviderCapabilitiesResponse, error) {
	return &protocol.GetProviderCapabilitiesResponse{Knowledge: provider.capabilities}, nil
}

func (provider *faultyKnowledgeProvider) Query(ctx context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
	return provider.query(ctx, request)
}

func TestKnowledgeClientRejectsFaultyProvider(t *testing.T) {
	certificates := newKnowledgeCertificates(t)
	for _, test := range []struct {
		name         string
		capabilities *protocol.KnowledgeProviderCapabilities
		query        knowledgeQueryFunc
		code         codes.Code
	}{
		{"missing capabilities", nil, nil, codes.FailedPrecondition},
		{"wrong protocol", &protocol.KnowledgeProviderCapabilities{ProtocolVersion: "unsupported"}, nil, codes.FailedPrecondition},
		{"wrong generation", knowledgeCapabilities(), func(_ context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
			response := knowledgeResult(request)
			response.Scope.Generation++
			return response, nil
		}, codes.DataLoss},
		{"oversized response", knowledgeCapabilities(), func(_ context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
			response := knowledgeResult(request)
			response.Matches[0].Content = strings.Repeat("a", 5<<20)
			return response, nil
		}, codes.ResourceExhausted},
		{"private transport error", knowledgeCapabilities(), func(context.Context, *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
			return nil, status.Error(codes.Internal, "postgres://user:secret@private-host")
		}, codes.Internal},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificates.provider}, ClientCAs: certificates.roots, ClientAuth: tls.RequireAndVerifyClientCert})))
			provider := &faultyKnowledgeProvider{capabilities: test.capabilities, query: test.query}
			protocol.RegisterIdentityServer(server, provider)
			protocol.RegisterKnowledgeExtensionServer(server, provider)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			finished := make(chan struct{})
			go func() { defer close(finished); _ = server.Serve(listener) }()
			defer func() { server.Stop(); _ = listener.Close(); <-finished }()
			client := newKnowledgeClient(t, listener.Addr().String(), certificates)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			response, err := client.Query(ctx, validKnowledgeQuery())
			if status.Code(err) != test.code || response != nil || strings.Contains(err.Error(), "private-host") {
				t.Fatalf("accepted unsafe provider response: %v %v", response, err)
			}
		})
	}
}
