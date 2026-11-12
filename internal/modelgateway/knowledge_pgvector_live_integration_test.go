//go:build integration

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/foundry"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/internal/knowledgegateway"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"github.com/kognition-ai/kognition/providers/pgvector"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/types"
)

type liveQueryValidator struct{}

func (liveQueryValidator) Validate(context.Context, *runtimeauth.KnowledgeSessionCapabilityClaims, string) error {
	return nil
}

type liveKnowledgeResolver struct{ binding *knowledge.ResolvedBinding }

func (resolver liveKnowledgeResolver) Resolve(_ context.Context, key types.NamespacedName) (*knowledge.ResolvedBinding, error) {
	if key.Namespace != resolver.binding.Scope.Namespace || key.Name != "docs" {
		return nil, status.Error(codes.PermissionDenied, "wrong knowledge locator")
	}
	return resolver.binding, nil
}

// This test uses local resource/authorization fixtures, real Foundry embeddings,
// authenticated CPI transport and verified PostgreSQL TLS; it is not deployed acceptance.
func TestKnowledgePgvectorFoundryLive(t *testing.T) {
	if os.Getenv("KOGNITION_KNOWLEDGE_LIVE_TESTS") != "true" {
		t.Skip("explicit bounded paid embedding opt-in required")
	}
	for _, name := range []string{"MS_FOUNDRY_INFERENCE_ENDPOINT", "MS_FOUNDRY_EMBEDDING_MODEL", "MS_FOUNDRY_EMBEDDING_API_VERSION", "AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET", "KOGNITION_PGVECTOR_TLS_TEST_PORT", "KOGNITION_KNOWLEDGE_TEST_PKI"} {
		if os.Getenv(name) == "" {
			t.Fatalf("live verification requires %s", name)
		}
	}
	endpoint, err := url.Parse(os.Getenv("MS_FOUNDRY_INFERENCE_ENDPOINT"))
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil || !strings.HasSuffix(endpoint.Hostname(), ".cognitiveservices.azure.com") || endpoint.RawQuery != "" || strings.Trim(endpoint.Path, "/") != "" {
		t.Fatal("a reviewed Cognitive Services HTTPS resource root is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	f := newQueryEmbeddingFixture(t)
	dimensions := int64(1536)
	var model api.Model
	if err := json.Unmarshal(f.storage.data["Model/team-a/embedding-model"], &model); err != nil {
		t.Fatal(err)
	}
	model.Spec.Provider, model.Spec.Endpoint, model.Spec.ModelID = "foundry", endpoint.String(), os.Getenv("MS_FOUNDRY_EMBEDDING_MODEL")
	model.Spec.Parameters = map[string]string{"apiVersion": os.Getenv("MS_FOUNDRY_EMBEDDING_API_VERSION")}
	var profile api.ModelInvocationProfile
	if err := json.Unmarshal(f.storage.data["ModelInvocationProfile/team-a/embedding-profile"], &profile); err != nil {
		t.Fatal(err)
	}
	profile.Spec.Defaults.Dimensions = &dimensions
	model.Spec.InvocationProfileRef.SpecDigest, err = modelinvocation.Digest(profile.Spec)
	if err != nil {
		t.Fatal(err)
	}
	put(t, f.storage, "ModelInvocationProfile", "team-a", profile.Name, profile)
	put(t, f.storage, "Model", "team-a", model.Name, model)
	snapshot, err := runtimeauth.CaptureModelBinding(ctx, f.storage, &model)
	if err != nil {
		t.Fatal(err)
	}
	var kb api.KnowledgeBase
	if err := json.Unmarshal(f.storage.data["KnowledgeBase/team-a/docs"], &kb); err != nil {
		t.Fatal(err)
	}
	kb.Status.BackendBinding.EmbeddingModelBinding = snapshot
	put(t, f.storage, "KnowledgeBase", "team-a", kb.Name, kb)
	values := map[string]string{"type": "azure-identity", "credentialType": "client-secret", "tenantId": os.Getenv("AZURE_TENANT_ID"), "clientId": os.Getenv("AZURE_CLIENT_ID"), "clientSecret": os.Getenv("AZURE_CLIENT_SECRET")}
	credentialJSON, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	put(t, f.storage, "SecretRef", "team-a", "model-key", api.SecretRef{ObjectMeta: api.ObjectMeta{Name: "model-key", Namespace: "team-a", Annotations: map[string]string{"kognition.dev/internal-secret-data": string(credentialJSON)}}, Spec: api.SecretRefSpec{BackendType: "internal"}})
	providerTransport := &liveInvocationTransport{upstream: http.DefaultTransport, host: endpoint.Host, maximum: 2}
	t.Cleanup(func() { t.Logf("LIVE_USAGE %s", providerTransport.summary()) })
	httpClient := &http.Client{Transport: providerTransport, Timeout: 70 * time.Second}
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1})
	tracer.SetFallibleCostSink(func(observability.CostRecord) error { return nil })
	f.executor, err = NewEmbeddingExecutor(f.storage, f.authorizer, httpClient, zap.NewNop(),
		WithQuotaChecker(f.quota), WithAuditLog(observability.NewCognitiveAuditLog(f.audit)), WithObservability(tracer, nil))
	if err != nil {
		t.Fatal(err)
	}
	pki := os.Getenv("KOGNITION_KNOWLEDGE_TEST_PKI")
	ca, err := os.ReadFile(filepath.Join(pki, "ca.crt"))
	if err != nil {
		t.Fatal("cannot read private test trust root")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("invalid private test trust root")
	}
	port, err := strconv.Atoi(os.Getenv("KOGNITION_PGVECTOR_TLS_TEST_PORT"))
	if err != nil {
		t.Fatal("invalid isolated TLS database port")
	}
	spec := pgvector.BackendSpec{Host: "localhost", Port: int32(port), Database: "postgres", Table: "knowledge_chunks",
		EmbeddingDimensions: 1536, CredentialsSecretRef: pgvector.CredentialsReference{Name: "database", UsernameKey: "username", PasswordKey: "password"}, TLS: pgvector.TLSConfig{Mode: "verify-full"}}
	material := &pgvector.ConnectionMaterial{Username: "postgres", Password: "local-database-qualification-only", TLS: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "localhost"}}
	admin, err := pgvector.NewPool(ctx, spec, material, pgvector.PoolBounds{MinConns: 1, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	database := "rag_live_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
		t.Fatal("cannot create isolated live-verification database")
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
			t.Error("cannot remove isolated live-verification database")
		}
	})
	spec.Database = database
	pool, err := pgvector.NewPool(ctx, spec, material, pgvector.PoolBounds{MinConns: 1, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	schema, err := pgvector.SchemaSQL(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "CREATE EXTENSION vector WITH SCHEMA public;"+schema); err != nil {
		t.Fatal("cannot prepare isolated provider schema")
	}
	lifecycle, err := pgvector.NewLifecycle(pool, spec)
	if err != nil {
		t.Fatal(err)
	}
	scope := f.binding.Scope
	pending := proto.Clone(scope).(*protocol.KnowledgeScope)
	pending.OperationId = ""
	querier, err := pgvector.NewQuerier(pool, spec, scope)
	if err != nil {
		t.Fatal(err)
	}
	serverCertificate, err := tls.LoadX509KeyPair(filepath.Join(pki, "provider.crt"), filepath.Join(pki, "provider.key"))
	if err != nil {
		t.Fatal("cannot read provider test identity")
	}
	clientCertificate, err := tls.LoadX509KeyPair(filepath.Join(pki, "client.crt"), filepath.Join(pki, "client.key"))
	if err != nil {
		t.Fatal("cannot read client test identity")
	}
	grpcServer, err := pluginsdk.NewKnowledgeServer(pluginsdk.KnowledgeServerConfig{
		Name: "pgvector", Version: "live-test", TLS: pluginsdk.KnowledgeTLSConfig{Certificate: serverCertificate, Roots: roots, PeerURI: "spiffe://kognition.dev/local/manual/knowledge-client"},
		Capabilities: &protocol.KnowledgeProviderCapabilities{ProtocolVersion: pluginsdk.KnowledgeProtocolVersion, ConfigurationKinds: []*protocol.KnowledgeConfigurationKind{{ApiGroup: pgvector.APIGroup, Kind: pgvector.Kind}}, Operations: []string{"dense", "citations", "filter", "index"}},
		AuthorizeQueryScope: func(_ context.Context, actual *protocol.KnowledgeScope) error {
			if !proto.Equal(actual, scope) {
				return status.Error(codes.PermissionDenied, "wrong scope")
			}
			return nil
		},
		AuthorizeIndexScope: func(_ context.Context, actual *protocol.KnowledgeScope) error {
			if !proto.Equal(actual, pending) {
				return status.Error(codes.PermissionDenied, "wrong pending scope")
			}
			return nil
		},
		Probe: func(context.Context) error { return nil }, Querier: querier, Indexer: lifecycle,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		if err := <-serveErrors; err != nil {
			t.Error(err)
		}
	})
	client, err := pluginsdk.NewKnowledgeClient(pluginsdk.KnowledgeClientConfig{Name: "pgvector", Address: listener.Addr().String(), TLS: pluginsdk.KnowledgeTLSConfig{
		Certificate: clientCertificate, Roots: roots, PeerURI: "spiffe://kognition.dev/local/manual/pgvector", ServerName: "kognition-knowledge-pgvector.kognition-pgvector-system.svc.cluster.local",
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	driver := foundry.New(httpClient)
	invocation, err := cpi.ResolveInvocation(ctx, f.storage, driver, &model)
	if err != nil {
		t.Fatal(err)
	}
	document := "Orbital Mark 7 supported launch regions are westus3, eastus2, northeurope, and australiaeast."
	indexed, err := driver.Embed(ctx, &cpi.EmbedRequest{Model: model.Spec.ModelID, Endpoint: model.Spec.Endpoint, Creds: &cpi.Credentials{Type: "azure-identity", Data: values}, Inputs: []string{document}, Parameters: model.Spec.Parameters, Invocation: invocation})
	if err != nil || len(indexed.Embeddings) != 1 || len(indexed.Embeddings[0]) != 1536 || !indexed.Usage.Reported {
		t.Fatal("real index embedding did not satisfy the profile")
	}
	confirmation, err := client.Index(ctx, []*protocol.KnowledgeIndexBatchRequest{{
		Scope: pending, OperationId: scope.OperationId, SourceDigest: "sha256:" + strings.Repeat("b", 64),
		Chunks: []*protocol.KnowledgeIndexChunk{{DocumentId: "regions", ChunkId: "0", Content: document, SourceUri: "urn:kognition:source:s3:product-docs/regions.md", Vector: indexed.Embeddings[0]}},
	}})
	if err != nil || confirmation.DocumentCount != 1 || confirmation.IndexedChunks != 1 {
		t.Fatalf("real authenticated indexing failed: %v", err)
	}
	issuer, err := runtimeauth.NewKnowledgeSessionCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	token, err := issuer.Issue(runtimeauth.KnowledgeSessionCapabilityBinding{
		Namespace: f.claims.Namespace, AgentName: f.claims.AgentName, AgentUID: f.claims.AgentUID, AgentIdentityName: f.claims.AgentIdentityName, AgentIdentityUID: f.claims.AgentIdentityUID,
		InstanceName: f.claims.InstanceName, InstanceUID: f.claims.InstanceUID, SessionID: f.claims.SessionID, KnowledgeBases: []string{"docs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.binding.EmbeddingDimension = 1536
	audit := &modelAuditStore{}
	gateway, err := knowledgegateway.New(issuer, liveQueryValidator{}, liveKnowledgeResolver{f.binding}, map[string]knowledgegateway.QueryClient{"pgvector": client}, zap.NewNop(),
		knowledgegateway.WithQueryEmbedder(f.executor), knowledgegateway.WithQueryAuditLog(observability.NewCognitiveAuditLog(audit)))
	if err != nil {
		t.Fatal(err)
	}
	call := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader(`{"knowledge_base":"docs","request_id":"live-query","mode":"dense","query_text":"Which regions support Orbital Mark 7 at launch?","top_k":1}`))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, request)
		return response
	}
	response := call()
	if response.Code != 200 || !strings.Contains(response.Body.String(), "westus3") || !strings.Contains(response.Body.String(), "urn:kognition:source:s3:product-docs/regions.md") {
		t.Fatalf("governed real retrieval failed: HTTP %d %s", response.Code, response.Body.String())
	}
	f.authorizer.err = errors.New("revoked")
	if response := call(); response.Code != http.StatusForbidden {
		t.Fatalf("revoked live ModelGrant returned HTTP %d", response.Code)
	}
	t.Log("Verified real Foundry index/query embeddings, 1536 dimensions, PostgreSQL verify-full, mutual TLS CPI indexing/query, cited known passage and revoked model access without an extra provider request. Resource/authorization state used local fixtures.")
}
