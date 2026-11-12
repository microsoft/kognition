// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package knowledgegateway

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type recordingEmbedder struct {
	calls  int
	text   string
	vector []float64
	err    error
}

type queryAuditStore struct {
	err     error
	entries []observability.AuditEntry
}

func (store *queryAuditStore) Append(_ context.Context, entry observability.AuditEntry) error {
	if store.err != nil {
		return store.err
	}
	store.entries = append(store.entries, entry)
	return nil
}

func (store *queryAuditStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	for _, entry := range entries {
		if err := store.Append(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

func (store *queryAuditStore) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return append([]observability.AuditEntry(nil), store.entries...), nil
}

func (embedder *recordingEmbedder) EmbedQuery(_ context.Context, claims *runtimeauth.KnowledgeSessionCapabilityClaims, name string, binding *knowledge.ResolvedBinding, text, requestID string) ([]float64, error) {
	embedder.calls++
	embedder.text = text
	if claims.Namespace != "team-a" || name != "docs" || binding.Scope.KnowledgeBaseUid != "kb-uid" || requestID != "query-1" {
		return nil, status.Error(codes.PermissionDenied, "unexpected execution binding")
	}
	return embedder.vector, embedder.err
}

func TestTextDenseQueryEmbedsServerSideBeforeScopedProviderQuery(t *testing.T) {
	_, issuer, binding, validator, resolver, client := gatewayFixture(t)
	embedder := &recordingEmbedder{vector: []float64{1, .5, .25}}
	audit := &queryAuditStore{}
	server, err := New(issuer, validator, resolver, map[string]QueryClient{"azuresearch": client}, nil, WithQueryEmbedder(embedder), WithQueryAuditLog(observability.NewCognitiveAuditLog(audit)))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, knowledgeRequest(t, issuer, binding, `{"knowledge_base":"docs","request_id":"query-1","mode":"dense","query_text":"supported regions","top_k":3}`))
	if response.Code != http.StatusOK || embedder.calls != 1 || embedder.text != "supported regions" ||
		client.request == nil || client.request.Mode != "dense" || client.request.Text != "" || len(client.request.Vector) != 3 {
		t.Fatalf("status=%d embedding=%+v query=%+v body=%s", response.Code, embedder, client.request, response.Body.String())
	}
	if len(audit.entries) != 2 || audit.entries[0].Verb != "query" {
		t.Fatalf("query audit=%+v", audit.entries)
	}
}

func TestTextQueryFailuresNeverBecomeSuccessfulEmptyMatches(t *testing.T) {
	for _, test := range []struct {
		name      string
		body      string
		preflight error
		embedding error
		vector    []float64
		status    int
		calls     int
	}{
		{name: "mixed vector", body: `{"knowledge_base":"docs","request_id":"query-1","mode":"dense","query_text":"regions","vector":[1],"top_k":3}`, status: 400},
		{name: "unsupported provider mode", preflight: status.Error(codes.Unimplemented, "unsupported"), status: 400},
		{name: "revoked embedding grant", embedding: status.Error(codes.PermissionDenied, "revoked"), status: 403, calls: 1},
		{name: "embedding service unavailable", embedding: status.Error(codes.Unavailable, "unavailable"), status: 503, calls: 1},
		{name: "invalid vector", vector: []float64{math.NaN()}, status: 502, calls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, issuer, binding, validator, resolver, client := gatewayFixture(t)
			client.preflightErr = test.preflight
			embedder := &recordingEmbedder{vector: test.vector, err: test.embedding}
			server, err := New(issuer, validator, resolver, map[string]QueryClient{"azuresearch": client}, nil, WithQueryEmbedder(embedder), WithQueryAuditLog(observability.NewCognitiveAuditLog(&queryAuditStore{})))
			if err != nil {
				t.Fatal(err)
			}
			body := test.body
			if body == "" {
				body = `{"knowledge_base":"docs","request_id":"query-1","mode":"dense","query_text":"regions","top_k":3}`
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, knowledgeRequest(t, issuer, binding, body))
			if response.Code != test.status || embedder.calls != test.calls || client.request != nil {
				t.Fatalf("status=%d calls=%d provider=%+v body=%s", response.Code, embedder.calls, client.request, response.Body.String())
			}
		})
	}
}
