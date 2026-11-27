// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type queryEmbeddingFixture struct {
	executor       *EmbeddingExecutor
	storage        *memoryStore
	authorizer     *modelAuthorizer
	quota          *recordingModelQuota
	audit          *modelAuditStore
	claims         *runtimeauth.KnowledgeSessionCapabilityClaims
	binding        *knowledge.ResolvedBinding
	calls          int
	dimensions     int
	providerStatus int
}

func newQueryEmbeddingFixture(t *testing.T, extra ...Option) *queryEmbeddingFixture {
	t.Helper()
	_, storage, authorizer, _, _, _ := fixture(t)
	result := &queryEmbeddingFixture{storage: storage, authorizer: authorizer, quota: &recordingModelQuota{}, audit: &modelAuditStore{}, dimensions: 3}
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		result.calls++
		if len(result.audit.entries) == 0 {
			t.Error("embedding provider reached before durable authorization evidence")
		}
		if request.URL.Path != "/v1/embeddings" || request.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Error("embedding route or server-owned credential mismatch")
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "opaque-embedding-deployment" || body["dimensions"] != float64(3) || body["temperature"] != nil || body["max_tokens"] != nil {
			t.Errorf("embedding invocation does not match its profile: %v", body)
		}
		if result.providerStatus != 0 && result.providerStatus != http.StatusOK {
			writer.WriteHeader(result.providerStatus)
			_, _ = writer.Write([]byte("synthetic provider failure"))
			return
		}
		vector := make([]float64, result.dimensions)
		vector[0] = 1
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": vector}}, "usage": map[string]int{"prompt_tokens": 3, "total_tokens": 3}})
	}))
	t.Cleanup(upstream.Close)
	dimensions := int64(3)
	profile := api.ModelInvocationProfile{ObjectMeta: api.ObjectMeta{Name: "embedding-profile", Namespace: "team-a", UID: "profile-uid"}, Spec: api.ModelInvocationProfileSpec{Operation: "embedding", Defaults: api.InvocationDefaults{Dimensions: &dimensions}}}
	digest, err := modelinvocation.Digest(profile.Spec)
	if err != nil {
		t.Fatal(err)
	}
	model := api.Model{ObjectMeta: api.ObjectMeta{Name: "embedding-model", Namespace: "team-a", UID: "embedding-uid", ResourceVersion: 9}, Spec: api.ModelSpec{Provider: "openai-compatible", ModelID: "opaque-embedding-deployment", Endpoint: upstream.URL, CredentialRef: &api.LocalObjectReference{Name: "model-key"}, InvocationProfileRef: &api.ModelInvocationProfileReference{Name: profile.Name, SpecDigest: digest}, InvocationContract: "openai-embedding-v1"}}
	put(t, storage, "ModelInvocationProfile", "team-a", profile.Name, profile)
	put(t, storage, "Model", "team-a", model.Name, model)
	snapshot, err := runtimeauth.CaptureModelBinding(context.Background(), storage, &model)
	if err != nil {
		t.Fatal(err)
	}
	var agent api.Agent
	if err := json.Unmarshal(storage.data["Agent/team-a/planner"], &agent); err != nil {
		t.Fatal(err)
	}
	agent.Spec.KnowledgeBaseClaims = []api.KnowledgeBaseClaimRef{{Name: "docs-claim", Required: true}}
	put(t, storage, "Agent", "team-a", agent.Name, agent)
	claim := api.KnowledgeBaseClaim{ObjectMeta: api.ObjectMeta{Name: "docs-claim", Namespace: "team-a", UID: "claim-uid", Generation: 1}, Spec: api.KnowledgeBaseClaimSpec{KnowledgeBaseName: "docs"}, Status: api.KnowledgeBaseClaimStatus{Phase: api.PhaseBound, ObservedGeneration: 1, BoundKnowledgeBase: "docs"}}
	put(t, storage, "KnowledgeBaseClaim", "team-a", claim.Name, claim)
	class := api.KnowledgeBaseClass{ObjectMeta: api.ObjectMeta{Name: "docs-class", UID: "class-uid", Generation: 1}, Spec: api.KnowledgeBaseClassSpec{EmbeddingModelRef: &api.LocalObjectReference{Name: model.Name}}}
	put(t, storage, "KnowledgeBaseClass", "", class.Name, class)
	scope := &protocol.KnowledgeScope{Namespace: "team-a", KnowledgeBaseUid: "kb-uid", Generation: 1, ConfigurationApiGroup: "pgvector.knowledge.kognition.dev", ConfigurationKind: "PgvectorBackend", ConfigurationName: "backend", ConfigurationUid: "backend-uid", ConfigurationDigest: strings.Repeat("a", 64), OperationId: "index-1"}
	kb := api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a", UID: "kb-uid"}, Spec: api.KnowledgeBaseSpec{KnowledgeBaseClassName: class.Name}, Status: api.KnowledgeBaseStatus{Phase: api.PhaseReady, Generation: 1, BackendBinding: &api.KnowledgeBackendBinding{Generation: 1, ClassUID: class.UID, ClassGeneration: 1, ConfigurationUID: scope.ConfigurationUid, ConfigurationDigest: scope.ConfigurationDigest, OperationID: scope.OperationId, EmbeddingModelBinding: snapshot}}}
	put(t, storage, "KnowledgeBase", "team-a", kb.Name, kb)
	var identity api.AgentIdentity
	if err := json.Unmarshal(storage.data["AgentIdentity/team-a/agent-identity-planner"], &identity); err != nil {
		t.Fatal(err)
	}
	identity.UID = "identity-uid"
	put(t, storage, "AgentIdentity", "team-a", identity.Name, identity)
	result.claims = &runtimeauth.KnowledgeSessionCapabilityClaims{Namespace: "team-a", AgentName: "planner", AgentUID: "agent-uid", AgentIdentityName: identity.Name, AgentIdentityUID: identity.UID, InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", KnowledgeBases: []string{"docs"}}
	result.binding = &knowledge.ResolvedBinding{ProviderName: "pgvector", Scope: scope, EmbeddingDimension: 3}
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1})
	tracer.SetFallibleCostSink(func(observability.CostRecord) error { return nil })
	options := append([]Option{WithQuotaChecker(result.quota), WithAuditLog(observability.NewCognitiveAuditLog(result.audit)), WithObservability(tracer, nil)}, extra...)
	result.executor, err = NewEmbeddingExecutor(storage, authorizer, upstream.Client(), zap.NewNop(), options...)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestQueryEmbeddingUsesSharedContractsAndServerCredentials(t *testing.T) {
	f := newQueryEmbeddingFixture(t)
	vector, err := f.executor.EmbedQuery(context.Background(), f.claims, "docs", f.binding, "supported regions", "query-1")
	if err != nil || len(vector) != 3 || f.calls != 1 || len(f.audit.entries) != 2 {
		t.Fatalf("embedding=%v calls=%d audit=%d err=%v", vector, f.calls, len(f.audit.entries), err)
	}
	for _, entry := range f.audit.entries {
		if entry.Verb != "embed" || strings.Contains(string(entry.Details), "supported regions") || strings.Contains(string(entry.Details), "upstream-secret") {
			t.Fatalf("embedding audit is wrong or sensitive: %s", entry.Details)
		}
	}
	if _, err := f.executor.server.core.Authorize(context.Background(), "team-a", "planner", "embedding-model"); err == nil {
		t.Fatal("embedding authorization weakened chat Model assignment")
	}
}

func TestQueryEmbeddingReportsProviderCallsToTheConfiguredRecorder(t *testing.T) {
	recorder := &recordedCalls{}
	f := newQueryEmbeddingFixture(t, WithCallRecorder(recorder))
	if _, err := f.executor.EmbedQuery(context.Background(), f.claims, "docs", f.binding, "regions", "query-1"); err != nil {
		t.Fatal(err)
	}
	if len(recorder.outcomes) != 1 {
		t.Fatalf("recorded %d outcomes, want 1: %+v", len(recorder.outcomes), recorder.outcomes)
	}
	if got := recorder.outcomes[0]; got.Operation != cpi.OperationEmbed || got.Model != "opaque-embedding-deployment" || !got.Succeeded() {
		t.Fatalf("outcome = %+v, want a successful embed call", got)
	}
	if got := recorder.outcomes[0]; got.Endpoint == "" || got.Credential != fixtureCredentialFingerprint() {
		t.Fatalf("outcome = %+v, want the Model's endpoint and the fingerprint of its secret", got)
	}
}

func TestQueryEmbeddingRejectsRevocationOrMissingEvidenceBeforeProvider(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*queryEmbeddingFixture)
		code   codes.Code
	}{
		{name: "model grant", change: func(f *queryEmbeddingFixture) { f.authorizer.err = errors.New("grant revoked") }, code: codes.PermissionDenied},
		{name: "quota", change: func(f *queryEmbeddingFixture) { f.quota.err = errors.New("quota exhausted") }, code: codes.ResourceExhausted},
		{name: "audit", change: func(f *queryEmbeddingFixture) { f.audit.err = errors.New("audit unavailable") }, code: codes.Unavailable},
		{name: "claim", change: func(f *queryEmbeddingFixture) { delete(f.storage.data, "KnowledgeBaseClaim/team-a/docs-claim") }, code: codes.PermissionDenied},
		{name: "session", change: func(f *queryEmbeddingFixture) { f.claims.SessionID = "other" }, code: codes.PermissionDenied},
		{name: "profile replacement", change: func(f *queryEmbeddingFixture) {
			var p api.ModelInvocationProfile
			_ = json.Unmarshal(f.storage.data["ModelInvocationProfile/team-a/embedding-profile"], &p)
			p.UID = "replacement"
			put(t, f.storage, "ModelInvocationProfile", "team-a", p.Name, p)
		}, code: codes.PermissionDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newQueryEmbeddingFixture(t)
			test.change(f)
			if _, err := f.executor.EmbedQuery(context.Background(), f.claims, "docs", f.binding, "regions", "query-1"); status.Code(err) != test.code || f.calls != 0 {
				t.Fatalf("calls=%d error=%v", f.calls, err)
			}
		})
	}
}

func TestQueryEmbeddingInvalidResponsePersistsFailureEvidence(t *testing.T) {
	f := newQueryEmbeddingFixture(t)
	f.dimensions = 4
	if _, err := f.executor.EmbedQuery(context.Background(), f.claims, "docs", f.binding, "regions", "query-1"); status.Code(err) != codes.DataLoss || f.calls != 1 || len(f.audit.entries) != 2 {
		t.Fatalf("calls=%d audit=%d error=%v", f.calls, len(f.audit.entries), err)
	}
}
