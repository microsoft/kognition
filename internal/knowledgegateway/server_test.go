// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package knowledgegateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/types"
)

type fakeResolver struct {
	binding *knowledge.ResolvedBinding
	key     types.NamespacedName
}

func (resolver *fakeResolver) Resolve(_ context.Context, key types.NamespacedName) (*knowledge.ResolvedBinding, error) {
	resolver.key = key
	if resolver.binding == nil {
		return nil, status.Error(codes.FailedPrecondition, "not bound")
	}
	return resolver.binding, nil
}

type fakeClient struct {
	request      *protocol.KnowledgeQueryRequest
	err          error
	preflightErr error
}

func (client *fakeClient) PreflightQuery(context.Context, *protocol.KnowledgeQueryRequest) error {
	return client.preflightErr
}

func (client *fakeClient) Query(_ context.Context, request *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error) {
	client.request = proto.Clone(request).(*protocol.KnowledgeQueryRequest)
	if client.err != nil {
		return nil, client.err
	}
	return &protocol.KnowledgeQueryResponse{Scope: proto.Clone(request.Scope).(*protocol.KnowledgeScope), RequestId: request.RequestId, Matches: []*protocol.KnowledgeMatch{{DocumentId: "doc-1", ChunkId: "chunk-1", Content: "answer", Score: .9, SourceUri: "https://example.test/doc"}}}, nil
}

type fakeBindingValidator struct {
	err    error
	claims *runtimeauth.KnowledgeSessionCapabilityClaims
}

func (validator *fakeBindingValidator) Validate(_ context.Context, claims *runtimeauth.KnowledgeSessionCapabilityClaims, _ string) error {
	validator.claims = claims
	return validator.err
}

func gatewayFixture(t *testing.T) (*Server, *runtimeauth.KnowledgeSessionCapabilityIssuer, runtimeauth.KnowledgeSessionCapabilityBinding, *fakeBindingValidator, *fakeResolver, *fakeClient) {
	t.Helper()
	issuer, err := runtimeauth.NewKnowledgeSessionCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	scope := &protocol.KnowledgeScope{Namespace: "team-a", KnowledgeBaseUid: "kb-uid", Generation: 3, OperationId: "operation-3", ConfigurationApiGroup: "search.kognition.dev", ConfigurationKind: "AzureAISearch", ConfigurationName: "docs", ConfigurationUid: "config-uid", ConfigurationDigest: strings.Repeat("a", 64)}
	resolver := &fakeResolver{binding: &knowledge.ResolvedBinding{ProviderName: "azuresearch", Scope: scope}}
	client := &fakeClient{}
	validator := &fakeBindingValidator{}
	server, err := New(issuer, validator, resolver, map[string]QueryClient{"azuresearch": client}, nil)
	if err != nil {
		t.Fatal(err)
	}
	binding := runtimeauth.KnowledgeSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentUID: "agent-uid", AgentIdentityName: "identity", AgentIdentityUID: "identity-uid", InstanceName: "planner-r1", InstanceUID: "uid", SessionID: "session-1", KnowledgeBases: []string{"docs"}}
	return server, issuer, binding, validator, resolver, client
}

func knowledgeRequest(t *testing.T, issuer *runtimeauth.KnowledgeSessionCapabilityIssuer, binding runtimeauth.KnowledgeSessionCapabilityBinding, body string) *http.Request {
	t.Helper()
	token, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func TestServerResolvesCommittedScopeAndRoutesRegisteredProvider(t *testing.T) {
	server, issuer, binding, validator, resolver, client := gatewayFixture(t)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, knowledgeRequest(t, issuer, binding, `{"knowledge_base":"docs","request_id":"query-1","mode":"keyword","text":"architecture","top_k":3,"filters":{"metadata.team":"platform"}}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if resolver.key != (types.NamespacedName{Namespace: "team-a", Name: "docs"}) {
		t.Fatalf("resolved key = %#v", resolver.key)
	}
	if validator.claims == nil || validator.claims.InstanceUID != binding.InstanceUID {
		t.Fatalf("validated claims = %#v", validator.claims)
	}
	if client.request == nil || !proto.Equal(client.request.Scope, resolver.binding.Scope) || client.request.Text != "architecture" || len(client.request.Filters) != 1 {
		t.Fatalf("provider request = %#v", client.request)
	}
	var output queryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &output); err != nil || len(output.Matches) != 1 || output.Matches[0].KnowledgeBase != "docs" || output.Matches[0].Content != "answer" {
		t.Fatalf("response = %s, err=%v", response.Body.String(), err)
	}
}

func TestServerRejectsKnowledgeBaseOutsideCapability(t *testing.T) {
	server, issuer, binding, _, resolver, client := gatewayFixture(t)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, knowledgeRequest(t, issuer, binding, `{"knowledge_base":"other","request_id":"query-1","mode":"keyword","text":"architecture","top_k":3}`))
	if response.Code != http.StatusForbidden || resolver.key.Name != "" || client.request != nil {
		t.Fatalf("status=%d resolver=%#v providerRequest=%#v", response.Code, resolver.key, client.request)
	}
}

func TestServerRejectsOversizedRequest(t *testing.T) {
	server, issuer, binding, _, resolver, client := gatewayFixture(t)
	body := `{"knowledge_base":"docs","text":"` + strings.Repeat("x", maxRequestBytes) + `"}`
	response := httptest.NewRecorder()
	server.ServeHTTP(response, knowledgeRequest(t, issuer, binding, body))
	if response.Code != http.StatusRequestEntityTooLarge || resolver.key.Name != "" || client.request != nil {
		t.Fatalf("status=%d resolver=%#v providerRequest=%#v", response.Code, resolver.key, client.request)
	}
}

func TestServerFailsClosedForUnknownProvider(t *testing.T) {
	server, issuer, binding, _, resolver, client := gatewayFixture(t)
	resolver.binding.ProviderName = "unregistered"
	response := httptest.NewRecorder()
	server.ServeHTTP(response, knowledgeRequest(t, issuer, binding, `{"knowledge_base":"docs","request_id":"query-1","mode":"keyword","text":"architecture","top_k":3}`))
	if response.Code != http.StatusServiceUnavailable || client.request != nil {
		t.Fatalf("status=%d providerRequest=%#v", response.Code, client.request)
	}
}

func TestServerRejectsInactiveCapabilityBinding(t *testing.T) {
	server, issuer, binding, validator, resolver, client := gatewayFixture(t)
	validator.err = ErrInactiveCapabilityBinding
	response := httptest.NewRecorder()
	server.ServeHTTP(response, knowledgeRequest(t, issuer, binding, `{"knowledge_base":"docs","request_id":"query-1","mode":"keyword","text":"architecture","top_k":3}`))
	if response.Code != http.StatusForbidden || resolver.key.Name != "" || client.request != nil {
		t.Fatalf("status=%d resolver=%#v providerRequest=%#v", response.Code, resolver.key, client.request)
	}
}

func TestServerReturnsUnavailableWhenBindingStateCannotBeRead(t *testing.T) {
	server, issuer, binding, validator, resolver, client := gatewayFixture(t)
	validator.err = errors.New("Kubernetes API unavailable")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, knowledgeRequest(t, issuer, binding, `{"knowledge_base":"docs","request_id":"query-1","mode":"keyword","text":"architecture","top_k":3}`))
	if response.Code != http.StatusServiceUnavailable || resolver.key.Name != "" || client.request != nil {
		t.Fatalf("status=%d resolver=%#v providerRequest=%#v", response.Code, resolver.key, client.request)
	}
}

func TestServerReturnsMethodNotAllowedForKnownRoutes(t *testing.T) {
	server, _, _, _, resolver, client := gatewayFixture(t)
	for _, test := range []struct {
		path  string
		allow string
	}{
		{path: "/healthz", allow: http.MethodGet},
		{path: "/v1/query", allow: http.MethodPost},
	} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodPut, test.path, nil))
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != test.allow {
			t.Errorf("path=%s status=%d Allow=%q", test.path, response.Code, response.Header().Get("Allow"))
		}
	}
	if resolver.key.Name != "" || client.request != nil {
		t.Fatalf("method rejection reached downstream: resolver=%#v providerRequest=%#v", resolver.key, client.request)
	}
}

func TestServerMapsProviderErrors(t *testing.T) {
	tests := []struct {
		code       codes.Code
		wantStatus int
	}{
		{code: codes.InvalidArgument, wantStatus: http.StatusBadRequest},
		{code: codes.Unauthenticated, wantStatus: http.StatusBadGateway},
		{code: codes.PermissionDenied, wantStatus: http.StatusBadGateway},
		{code: codes.ResourceExhausted, wantStatus: http.StatusTooManyRequests},
		{code: codes.FailedPrecondition, wantStatus: http.StatusConflict},
		{code: codes.NotFound, wantStatus: http.StatusConflict},
		{code: codes.Unavailable, wantStatus: http.StatusServiceUnavailable},
		{code: codes.DeadlineExceeded, wantStatus: http.StatusServiceUnavailable},
		{code: codes.Internal, wantStatus: http.StatusBadGateway},
	}
	for _, test := range tests {
		t.Run(test.code.String(), func(t *testing.T) {
			server, issuer, binding, _, _, client := gatewayFixture(t)
			client.err = status.Error(test.code, "sensitive upstream detail")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, knowledgeRequest(t, issuer, binding, `{"knowledge_base":"docs","request_id":"query-1","mode":"keyword","text":"architecture","top_k":3}`))
			if response.Code != test.wantStatus || strings.Contains(response.Body.String(), "sensitive") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
