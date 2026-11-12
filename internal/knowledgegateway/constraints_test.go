// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package knowledgegateway

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

type constraintValidator struct {
	*fakeBindingValidator
	constraints *QueryConstraints
}

func (validator constraintValidator) QueryConstraints(context.Context, *runtimeauth.KnowledgeSessionCapabilityClaims, string) (*QueryConstraints, error) {
	return validator.constraints, nil
}

func TestLiveQueryConstraintsCannotBeOverriddenByRuntime(t *testing.T) {
	for _, body := range []string{
		`{"knowledge_base":"docs","request_id":"query-1","mode":"keyword","text":"regions","top_k":2,"filters":{"metadata.team":"platform"}}`,
		`{"knowledge_base":"docs","request_id":"query-1","mode":"dense","query_text":"regions","top_k":10,"filters":{"metadata.team":"platform"}}`,
		`{"knowledge_base":"docs","request_id":"query-1","mode":"dense","query_text":"regions","top_k":2}`,
	} {
		_, issuer, binding, base, resolver, client := gatewayFixture(t)
		embedder := &recordingEmbedder{vector: []float64{1}}
		validator := constraintValidator{base, &QueryConstraints{Mode: "dense", TopK: 3, Filters: map[string]string{"metadata.team": "platform"}}}
		server, err := New(issuer, validator, resolver, map[string]QueryClient{"azuresearch": client}, nil,
			WithQueryEmbedder(embedder), WithQueryAuditLog(observability.NewCognitiveAuditLog(&queryAuditStore{})))
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, knowledgeRequest(t, issuer, binding, body))
		if response.Code != 403 || embedder.calls != 0 || client.request != nil {
			t.Fatalf("status=%d calls=%d provider=%v", response.Code, embedder.calls, client.request)
		}
	}
}

func TestUnavailableQueryAuditBlocksEmbeddingAndProviderCalls(t *testing.T) {
	_, issuer, binding, validator, resolver, client := gatewayFixture(t)
	embedder := &recordingEmbedder{vector: []float64{1}}
	server, err := New(issuer, validator, resolver, map[string]QueryClient{"azuresearch": client}, nil,
		WithQueryEmbedder(embedder), WithQueryAuditLog(observability.NewCognitiveAuditLog(&queryAuditStore{err: errors.New("audit unavailable")})))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, knowledgeRequest(t, issuer, binding, `{"knowledge_base":"docs","request_id":"query-1","mode":"dense","query_text":"regions","top_k":2}`))
	if response.Code != 503 || embedder.calls != 0 || client.request != nil || !strings.Contains(response.Body.String(), "audit") {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, embedder.calls, response.Body.String())
	}
}
