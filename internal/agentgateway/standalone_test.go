// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/apiserver"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

type standaloneCompletionDriver struct {
	calls atomic.Int32
}

type standaloneAPIStore struct{ *testStore }

func (memory standaloneAPIStore) UpdateAndGet(ctx context.Context, kind, namespace, name string, data []byte, version int64) ([]byte, error) {
	if err := memory.Update(ctx, kind, namespace, name, data, version); err != nil {
		return nil, err
	}
	return memory.Get(ctx, kind, namespace, name)
}

func (*standaloneCompletionDriver) Name() string { return "standalone-test" }
func (*standaloneCompletionDriver) Validate(context.Context, *v1alpha1.ModelSpec, *v1alpha1.ModelClassSpec) error {
	return nil
}
func (*standaloneCompletionDriver) Probe(context.Context, *v1alpha1.ModelSpec, *cpi.Credentials) (*cpi.HealthResult, error) {
	return &cpi.HealthResult{Available: true}, nil
}
func (driver *standaloneCompletionDriver) Complete(context.Context, *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	driver.calls.Add(1)
	return &cpi.CompletionResponse{Content: "delegated response", Usage: cpi.Usage{Reported: true}}, nil
}
func (*standaloneCompletionDriver) Embed(context.Context, *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return nil, nil
}

func TestStandaloneDelegationInvokesAuthenticatedAPIServer(t *testing.T) {
	fixture, sessionIssuer, exactIssuer, authorizer, _, _ := delegationGatewayFixture(t, true)
	memory := fixture.store.(*testStore)
	storeObject(t, memory, "Agent", "team-b", "reviewer", v1alpha1.Agent{
		Spec:   v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "review-model"}},
		Status: v1alpha1.AgentStatus{Phase: v1alpha1.PhaseReady},
	})
	storeObject(t, memory, "Model", "team-b", "review-model", v1alpha1.Model{
		Spec: v1alpha1.ModelSpec{ClassName: "review-class", ModelID: "controlled-model"},
	})
	storeObject(t, memory, "ModelClass", "", "review-class", v1alpha1.ModelClass{
		Spec: v1alpha1.ModelClassSpec{Provisioner: "standalone-test"},
	})
	driver := &standaloneCompletionDriver{}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	api := apiserver.New(apiserver.Config{}, standaloneAPIStore{memory}, zaptest.NewLogger(t), apiserver.WithCPIRegistry(registry),
		apiserver.WithAuthenticator(auth.NewStaticTokenAuthenticator(map[string]string{"operator-credential": "operator"}), nil),
		apiserver.WithAgentServiceToken("api-credential"))
	var response apiserver.ChatResponse
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorded := httptest.NewRecorder()
		api.ServeHTTP(recorded, request)
		if recorded.Code == http.StatusOK {
			if err := json.Unmarshal(recorded.Body.Bytes(), &response); err != nil {
				t.Error(err)
			}
		}
		for name, values := range recorded.Header() {
			writer.Header()[name] = values
		}
		writer.WriteHeader(recorded.Code)
		_, _ = writer.Write(recorded.Body.Bytes())
	}))
	defer target.Close()
	newGateway := func() *Server {
		gateway, err := NewServer(memory, sessionIssuer, exactIssuer, authorizer, "runtime-token", "", target.Client(), zaptest.NewLogger(t), WithStandaloneAPIServer(target.URL, "api-credential"))
		if err != nil {
			t.Fatal(err)
		}
		return gateway
	}
	gateway := newGateway()
	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`
	exchange := httptest.NewRecorder()
	gateway.ServeHTTP(exchange, delegationRequest(t, "/v1/agent-services/capabilities", session, payload))
	var issued struct {
		Capability string `json:"capability"`
	}
	if err := json.Unmarshal(exchange.Body.Bytes(), &issued); err != nil || exchange.Code != http.StatusOK || issued.Capability == "" {
		t.Fatalf("capability exchange failed: status=%d error=%v", exchange.Code, err)
	}
	invoke := httptest.NewRecorder()
	gateway.ServeHTTP(invoke, delegationRequest(t, "/v1/agent-services/invoke", issued.Capability, payload))
	if invoke.Code != http.StatusAccepted || driver.calls.Load() != 1 || response.SessionID != "delegation-1" || response.Response != "delegated response" {
		t.Fatalf("joined invocation: status=%d calls=%d response=%+v", invoke.Code, driver.calls.Load(), response)
	}
	restarted := newGateway()
	replay := httptest.NewRecorder()
	restarted.ServeHTTP(replay, delegationRequest(t, "/v1/agent-services/invoke", issued.Capability, payload))
	if driver.calls.Load() != 1 {
		t.Fatal("gateway recreation replayed the provider effect")
	}
	authorizer.allowed = false
	revoked := httptest.NewRecorder()
	restarted.ServeHTTP(revoked, delegationRequest(t, "/v1/agent-services/invoke", issued.Capability, payload))
	if revoked.Code != http.StatusForbidden || driver.calls.Load() != 1 {
		t.Fatalf("revoked invocation: status=%d calls=%d", revoked.Code, driver.calls.Load())
	}
}

func TestStandaloneDelegationTransportConformance(t *testing.T) {
	for _, scenario := range []string{"allow and replay", "revoked", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			fixture, sessionIssuer, exactIssuer, authorizer, _, _ := delegationGatewayFixture(t, true)
			var calls atomic.Int32
			var redirected atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
			defer other.Close()
			target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				var body map[string]string
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["sessionID"] != "delegation-1" || body["message"] != "review this" || request.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-b/agentservices/review-service/invoke" || request.Header.Get("Authorization") != "Bearer api-credential" {
					t.Error("standalone invocation lost its bound route, credential, or session")
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				if scenario == "redirect" {
					http.Redirect(writer, request, other.URL, http.StatusTemporaryRedirect)
					return
				}
				writer.WriteHeader(http.StatusOK)
			}))
			defer target.Close()
			server, err := NewServer(fixture.store, sessionIssuer, exactIssuer, authorizer, "runtime-token", "", target.Client(), zaptest.NewLogger(t), WithStandaloneAPIServer(target.URL, "api-credential"))
			if err != nil {
				t.Fatal(err)
			}
			session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1"})
			if err != nil {
				t.Fatal(err)
			}
			payload := `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`
			exchange := httptest.NewRecorder()
			server.ServeHTTP(exchange, delegationRequest(t, "/v1/agent-services/capabilities", session, payload))
			var result struct {
				Capability string `json:"capability"`
			}
			if err := json.NewDecoder(exchange.Body).Decode(&result); err != nil || exchange.Code != http.StatusOK || result.Capability == "" {
				t.Fatalf("exchange = %d %s: %v", exchange.Code, exchange.Body.String(), err)
			}
			if scenario == "revoked" {
				authorizer.allowed = false
			}
			invoke := httptest.NewRecorder()
			server.ServeHTTP(invoke, delegationRequest(t, "/v1/agent-services/invoke", result.Capability, payload))
			switch scenario {
			case "allow and replay":
				if invoke.Code != http.StatusAccepted || calls.Load() != 1 {
					t.Fatalf("invoke = %d, calls = %d", invoke.Code, calls.Load())
				}
				replay := httptest.NewRecorder()
				server.ServeHTTP(replay, delegationRequest(t, "/v1/agent-services/invoke", result.Capability, payload))
				if calls.Load() != 1 {
					t.Fatal("replay repeated the external effect")
				}
			case "revoked":
				if invoke.Code != http.StatusForbidden || calls.Load() != 0 {
					t.Fatalf("revoked = %d, calls = %d", invoke.Code, calls.Load())
				}
			case "redirect":
				if invoke.Code != http.StatusBadGateway || redirected.Load() != 0 {
					t.Fatalf("redirect = %d, forwarded = %d", invoke.Code, redirected.Load())
				}
			}
		})
	}
}

func TestStandaloneDelegationRejectsInvalidOrigins(t *testing.T) {
	fixture, sessionIssuer, exactIssuer, authorizer, _, _ := delegationGatewayFixture(t, true)
	for _, endpoint := range []string{"", "file:///tmp/server", "http://user:password@localhost", "http://localhost/path", "http://localhost?token=secret", "http://localhost#fragment"} {
		if _, err := NewServer(fixture.store, sessionIssuer, exactIssuer, authorizer, "runtime-token", "", nil, zaptest.NewLogger(t), WithStandaloneAPIServer(endpoint, "api-credential")); err == nil {
			t.Errorf("accepted invalid origin %q", endpoint)
		}
	}
}

func TestStandaloneDelegationRejectsReusedRuntimeCredential(t *testing.T) {
	fixture, sessionIssuer, exactIssuer, authorizer, _, _ := delegationGatewayFixture(t, true)
	if _, err := NewServer(fixture.store, sessionIssuer, exactIssuer, authorizer, "runtime-token", "", nil, zaptest.NewLogger(t), WithStandaloneAPIServer("http://localhost:8080", "runtime-token")); err == nil {
		t.Fatal("accepted the runtime credential as an API invocation credential")
	}
}
