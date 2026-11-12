// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/apiserver"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelgateway"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

// TC-0301: Standalone CPI, direct standalone chat, and Kubernetes HTTP gateway
// transports execute the same deterministic Model contract through the shared
// authorization core. Kubernetes additionally enforces its session-bound
// Model revision capability.
func TestModelGatewayCrossModeConformance(t *testing.T) {
	runAcrossModelTransports(t, "authorized execution", func(t *testing.T, fixture *modelTransportFixture) modelTransportResult {
		return fixture.invoke(t)
	}, func(t *testing.T, results map[string]modelTransportResult) {
		for transport, result := range results {
			if result.Outcome != "success" || result.Output != "ok" || result.ModelID != "mock" || result.Usage != (modelUsage{2, 1, 3}) || result.Credential != "upstream-secret" {
				t.Fatalf("%s result = %#v", transport, result)
			}
		}
	})

	runAcrossModelTransports(t, "live policy revocation", func(t *testing.T, fixture *modelTransportFixture) modelTransportResult {
		fixture.authorizer.err = errors.New("ModelGrant revoked")
		return fixture.invoke(t)
	}, func(t *testing.T, results map[string]modelTransportResult) {
		for transport, result := range results {
			if result.Outcome != "rejected" || result.ProviderCalls != 0 {
				t.Fatalf("%s result = %#v", transport, result)
			}
		}
	})

	runAcrossModelTransports(t, "safe provider failure", func(t *testing.T, fixture *modelTransportFixture) modelTransportResult {
		fixture.provider.fail = true
		return fixture.invoke(t)
	}, func(t *testing.T, results map[string]modelTransportResult) {
		for transport, result := range results {
			if result.Outcome != "provider-failure" || result.ProviderCalls != 1 || strings.Contains(result.Error, "provider internal detail") {
				t.Fatalf("%s result = %#v", transport, result)
			}
		}
	})

	// A Kubernetes capability binds the dispatched Model revision. Standalone
	// execution is synchronous and deliberately authorizes the current Model
	// just before calling the provider, so it uses the updated revision instead
	// of executing a stale one.
	runAcrossModelTransports(t, "Model revision behavior", func(t *testing.T, fixture *modelTransportFixture) modelTransportResult {
		fixture.bumpModelRevision(t)
		return fixture.invoke(t)
	}, func(t *testing.T, results map[string]modelTransportResult) {
		if results["kubernetes-http"].Outcome != "rejected" || results["kubernetes-http"].ProviderCalls != 0 {
			t.Fatalf("HTTP stale revision result = %#v", results["kubernetes-http"])
		}
		for _, transport := range []string{"standalone-cpi", "standalone-chat"} {
			if results[transport].Outcome != "success" || results[transport].ProviderCalls != 1 {
				t.Fatalf("%s current revision result = %#v", transport, results[transport])
			}
		}
	})
}

type modelUsage struct{ PromptTokens, CompletionTokens, TotalTokens int }

type modelTransportResult struct {
	Outcome       string
	Output        string
	Error         string
	ModelID       string
	Usage         modelUsage
	Credential    string
	ProviderCalls int
}

func runAcrossModelTransports(t *testing.T, name string, invoke func(*testing.T, *modelTransportFixture) modelTransportResult, assert func(*testing.T, map[string]modelTransportResult)) {
	t.Helper()
	results := make(map[string]modelTransportResult, 3)
	for _, transport := range []string{"standalone-cpi", "standalone-chat", "kubernetes-http"} {
		t.Run(transport, func(t *testing.T) {
			fixture := newModelTransportFixture(t, transport)
			results[transport] = invoke(t, fixture)
		})
	}
	assert(t, results)
}

type modelTransportFixture struct {
	store      *conformanceStore
	authorizer *conformanceAuthorizer
	provider   *conformanceProvider
	transport  string
	dispatcher interface {
		Dispatch(context.Context, *v1alpha1.AgentInstance) (string, error)
		Status(context.Context, *v1alpha1.AgentInstance) (controller.RuntimeStatus, error)
	}
	chat    *apiserver.Server
	gateway *modelgateway.Server
	issuer  *runtimeauth.ModelSessionCapabilityIssuer
	binding runtimeauth.ModelSessionCapabilityBinding
}

func newModelTransportFixture(t *testing.T, transport string) *modelTransportFixture {
	t.Helper()
	provider := &conformanceProvider{}
	upstream := httptest.NewServer(provider.httpHandler())
	t.Cleanup(upstream.Close)
	st := newConformanceStore()
	seedModelTransportResources(t, st, upstream.URL)
	authorizer := &conformanceAuthorizer{}
	core, err := modelgateway.NewCore(st, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &modelTransportFixture{store: st, authorizer: authorizer, provider: provider, transport: transport}
	registry := cpi.NewRegistry()
	registry.MustRegister(provider)
	switch transport {
	case "standalone-cpi":
		dispatcher := controller.NewCPIDispatcher(st, registry, zap.NewNop())
		dispatcher.SetModelGatewayCore(core)
		fixture.dispatcher = dispatcher
	case "standalone-chat":
		fixture.chat = apiserver.New(apiserver.Config{}, st, zap.NewNop(), apiserver.WithCPIRegistry(registry), apiserver.WithModelGatewayCore(core), apiserver.WithModelAccessAuthorizer(authorizer))
	case "kubernetes-http":
		issuer, issueErr := runtimeauth.NewModelSessionCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
		if issueErr != nil {
			t.Fatal(issueErr)
		}
		fixture.issuer = issuer
		fixture.binding = runtimeauth.ModelSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", ModelName: "mock-model", ModelResourceVersion: 7}
		gateway, gatewayErr := modelgateway.New(st, issuer, authorizer, upstream.Client(), zap.NewNop())
		if gatewayErr != nil {
			t.Fatal(gatewayErr)
		}
		fixture.gateway = gateway
	}
	return fixture
}

func (f *modelTransportFixture) invoke(t *testing.T) modelTransportResult {
	t.Helper()
	switch f.transport {
	case "standalone-cpi":
		instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "standalone", Namespace: "team-a"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}, SessionContext: map[string]string{"input": "hello"}}}
		sessionID, err := f.dispatcher.Dispatch(t.Context(), instance)
		if err != nil {
			return f.failureResult(err)
		}
		instance.Status.SessionID = sessionID
		status, err := f.dispatcher.Status(t.Context(), instance)
		if err != nil {
			return f.failureResult(err)
		}
		if !status.Completed || status.TokenUsage == nil {
			return f.failureResult(errors.New("standalone dispatcher did not return completion usage"))
		}
		return f.successResult(status.Output, modelUsage{int(status.TokenUsage.PromptTokens), int(status.TokenUsage.CompletionTokens), int(status.TokenUsage.TotalTokens)})
	case "standalone-chat":
		request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner/chat", bytes.NewBufferString(`{"message":"hello","sessionID":"chat-1"}`))
		response := httptest.NewRecorder()
		f.chat.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			return f.failureResult(errors.New(response.Body.String()))
		}
		var payload struct {
			Response string `json:"response"`
			Usage    struct {
				PromptTokens     int `json:"promptTokens"`
				CompletionTokens int `json:"completionTokens"`
				TotalTokens      int `json:"totalTokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return f.successResult(payload.Response, modelUsage{payload.Usage.PromptTokens, payload.Usage.CompletionTokens, payload.Usage.TotalTokens})
	default:
		capability, err := f.issuer.Issue(f.binding)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"attacker-selected","messages":[{"role":"user","content":"hello"}]}`))
		request.Header.Set("Authorization", "Bearer "+capability)
		response := httptest.NewRecorder()
		f.gateway.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			return f.failureResult(errors.New(response.Body.String()))
		}
		var payload struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || len(payload.Choices) != 1 {
			t.Fatalf("decode gateway response err=%v body=%s", err, response.Body.String())
		}
		return f.successResult(payload.Choices[0].Message.Content, modelUsage{payload.Usage.PromptTokens, payload.Usage.CompletionTokens, payload.Usage.TotalTokens})
	}
}

func (f *modelTransportFixture) successResult(output string, usage modelUsage) modelTransportResult {
	return modelTransportResult{Outcome: "success", Output: output, ModelID: f.provider.modelID(), Usage: usage, Credential: f.provider.credential(), ProviderCalls: f.provider.calls()}
}

func (f *modelTransportFixture) failureResult(err error) modelTransportResult {
	if f.provider.calls() == 0 {
		return modelTransportResult{Outcome: "rejected", ProviderCalls: 0}
	}
	return modelTransportResult{Outcome: "provider-failure", Error: err.Error(), ProviderCalls: f.provider.calls()}
}

func (f *modelTransportFixture) bumpModelRevision(t *testing.T) {
	t.Helper()
	var model v1alpha1.Model
	if err := json.Unmarshal(f.store.mustGet("Model", "team-a", "mock-model"), &model); err != nil {
		t.Fatal(err)
	}
	model.ObjectMeta.ResourceVersion++
	f.store.put(t, "Model", "team-a", "mock-model", model)
}

type conformanceAuthorizer struct{ err error }

func (a *conformanceAuthorizer) EvaluateModelAccess(context.Context, string, string, string) error {
	return a.err
}

type conformanceProvider struct {
	mu              sync.Mutex
	fail            bool
	invoked         int
	model           string
	credentialValue string
}

func (*conformanceProvider) Name() string { return "openai-compatible" }
func (*conformanceProvider) Validate(context.Context, *v1alpha1.ModelSpec, *v1alpha1.ModelClassSpec) error {
	return nil
}
func (*conformanceProvider) Probe(context.Context, *v1alpha1.ModelSpec, *cpi.Credentials) (*cpi.HealthResult, error) {
	return &cpi.HealthResult{Available: true}, nil
}
func (p *conformanceProvider) Complete(_ context.Context, request *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.invoked++
	p.model = request.Model
	if request.Creds != nil {
		p.credentialValue = request.Creds.Data["apiKey"]
	}
	if p.fail {
		return nil, errors.New("provider internal detail")
	}
	return &cpi.CompletionResponse{Content: "ok", Usage: cpi.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3, Reported: true}}, nil
}
func (*conformanceProvider) Embed(context.Context, *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return nil, nil
}
func (p *conformanceProvider) httpHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid provider request", http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.invoked++
		p.model = request.Model
		p.credentialValue = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		fail := p.fail
		p.mu.Unlock()
		if fail {
			http.Error(w, "provider internal detail", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "ok"}}}, "usage": map[string]int{"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3}})
	})
}
func (p *conformanceProvider) calls() int      { p.mu.Lock(); defer p.mu.Unlock(); return p.invoked }
func (p *conformanceProvider) modelID() string { p.mu.Lock(); defer p.mu.Unlock(); return p.model }
func (p *conformanceProvider) credential() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.credentialValue
}

type conformanceStore struct {
	mu        sync.Mutex
	values    map[string][]byte
	revisions map[string]store.Revision
	events    map[string][]store.InstanceEvent
}

func newConformanceStore() *conformanceStore {
	return &conformanceStore{values: map[string][]byte{}, revisions: map[string]store.Revision{}, events: map[string][]store.InstanceEvent{}}
}
func conformanceKey(kind, namespace, name string) string { return kind + "/" + namespace + "/" + name }
func (s *conformanceStore) Ping(context.Context) error   { return nil }
func (s *conformanceStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[conformanceKey(kind, namespace, name)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), value...), nil
}
func (s *conformanceStore) mustGet(kind, namespace, name string) []byte {
	value, _ := s.Get(context.Background(), kind, namespace, name)
	return value
}
func (s *conformanceStore) List(_ context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := kind + "/"
	if opts.Namespace != "" {
		prefix += opts.Namespace + "/"
	}
	keys := make([]string, 0)
	for key := range s.values {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	result := make([][]byte, 0, len(keys))
	for _, key := range keys {
		result = append(result, append([]byte(nil), s.values[key]...))
	}
	return result, nil
}
func (s *conformanceStore) Create(_ context.Context, kind, namespace, name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conformanceKey(kind, namespace, name)
	if _, exists := s.values[key]; exists {
		return store.ErrAlreadyExists
	}
	s.values[key] = append([]byte(nil), data...)
	return nil
}
func (s *conformanceStore) Update(_ context.Context, kind, namespace, name string, data []byte, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conformanceKey(kind, namespace, name)
	if _, exists := s.values[key]; !exists {
		return store.ErrNotFound
	}
	s.values[key] = append([]byte(nil), data...)
	return nil
}
func (s *conformanceStore) UpdateAndGet(ctx context.Context, kind, namespace, name string, data []byte, version int64) ([]byte, error) {
	if err := s.Update(ctx, kind, namespace, name, data, version); err != nil {
		return nil, err
	}
	return data, nil
}
func (s *conformanceStore) Delete(_ context.Context, kind, namespace, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conformanceKey(kind, namespace, name)
	if _, exists := s.values[key]; !exists {
		return store.ErrNotFound
	}
	delete(s.values, key)
	return nil
}
func (s *conformanceStore) Watch(context.Context, string, store.ListOptions) (<-chan store.WatchEvent, error) {
	return nil, nil
}
func (s *conformanceStore) CreateRevision(_ context.Context, kind, namespace, name string, revision int64, spec, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revisions[fmt.Sprintf("%s@%d", conformanceKey(kind, namespace, name), revision)] = store.Revision{Revision: revision, Spec: spec, Data: data}
	return nil
}
func (s *conformanceStore) ListRevisions(_ context.Context, kind, namespace, name string) ([]store.Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := conformanceKey(kind, namespace, name) + "@"
	result := []store.Revision{}
	for key, revision := range s.revisions {
		if strings.HasPrefix(key, prefix) {
			result = append(result, revision)
		}
	}
	return result, nil
}
func (s *conformanceStore) GetRevision(_ context.Context, kind, namespace, name string, revision int64) (*store.Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.revisions[fmt.Sprintf("%s@%d", conformanceKey(kind, namespace, name), revision)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &value, nil
}
func (s *conformanceStore) AppendEvent(_ context.Context, kind, namespace, name string, event store.InstanceEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conformanceKey(kind, namespace, name)
	s.events[key] = append(s.events[key], event)
	return nil
}
func (s *conformanceStore) ListEvents(_ context.Context, kind, namespace, name string) ([]store.InstanceEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.InstanceEvent(nil), s.events[conformanceKey(kind, namespace, name)]...), nil
}
func (s *conformanceStore) put(t *testing.T, kind, namespace, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[conformanceKey(kind, namespace, name)] = data
}

func seedModelTransportResources(t *testing.T, st *conformanceStore, endpoint string) {
	t.Helper()
	st.put(t, "Agent", "team-a", "planner", v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "team-a", UID: "agent-uid"}, Spec: v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "mock-model"}, Instructions: v1alpha1.AgentInstructions{Inline: "be brief"}}, Status: v1alpha1.AgentStatus{Phase: v1alpha1.PhaseReady}})
	st.put(t, "AgentIdentity", "team-a", "agent-identity-planner", v1alpha1.AgentIdentity{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-identity-planner", Namespace: "team-a", Labels: map[string]string{"kognition.dev/auto-created-for-agent": "planner"}, OwnerReferences: []v1alpha1.OwnerReference{{Kind: "Agent", Name: "planner", UID: "agent-uid"}}}, Spec: v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}}, Status: v1alpha1.AgentIdentityStatus{Phase: v1alpha1.PhaseReady}})
	st.put(t, "AgentInstance", "team-a", "planner-r1", v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-r1", Namespace: "team-a", UID: "instance-uid"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}}, Status: v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhaseRunning, SessionID: "session-1"}})
	st.put(t, "Model", "team-a", "mock-model", v1alpha1.Model{ObjectMeta: v1alpha1.ObjectMeta{Name: "mock-model", Namespace: "team-a", ResourceVersion: 7}, Spec: v1alpha1.ModelSpec{ClassName: "mock-class", ModelID: "mock", Endpoint: endpoint, CredentialRef: &v1alpha1.LocalObjectReference{Name: "model-key"}}})
	st.put(t, "ModelClass", "", "mock-class", v1alpha1.ModelClass{ObjectMeta: v1alpha1.ObjectMeta{Name: "mock-class"}, Spec: v1alpha1.ModelClassSpec{Provisioner: "openai-compatible"}})
	st.put(t, "SecretRef", "team-a", "model-key", v1alpha1.SecretRef{ObjectMeta: v1alpha1.ObjectMeta{Name: "model-key", Namespace: "team-a", Annotations: map[string]string{"kognition.dev/internal-secret-data": `{"type":"api-key","apiKey":"upstream-secret"}`}}, Spec: v1alpha1.SecretRefSpec{BackendType: "internal"}})
}
