// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

type memoryStore struct{ data map[string][]byte }

type modelAuditStore struct {
	entries       []observability.AuditEntry
	err           error
	providerCalls func() bool
}

func (s *modelAuditStore) Append(_ context.Context, entry observability.AuditEntry) error {
	if s.err != nil {
		return s.err
	}
	if entry.EventType == observability.AuditModelCall && entry.Verb == "complete" && len(s.entries) == 0 && s.providerCalls != nil && s.providerCalls() {
		return errors.New("authorization evidence was written after provider dispatch")
	}
	s.entries = append(s.entries, entry)
	return nil
}
func (s *modelAuditStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	for _, entry := range entries {
		if err := s.Append(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}
func (s *modelAuditStore) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return append([]observability.AuditEntry(nil), s.entries...), nil
}

func (s *memoryStore) Ping(context.Context) error { return nil }
func (s *memoryStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	data, ok := s.data[kind+"/"+namespace+"/"+name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), data...), nil
}
func (*memoryStore) List(context.Context, string, store.ListOptions) ([][]byte, error) {
	return nil, nil
}
func (*memoryStore) Create(context.Context, string, string, string, []byte) error        { return nil }
func (*memoryStore) Update(context.Context, string, string, string, []byte, int64) error { return nil }
func (*memoryStore) Delete(context.Context, string, string, string) error                { return nil }
func (*memoryStore) Watch(context.Context, string, store.ListOptions) (<-chan store.WatchEvent, error) {
	return nil, nil
}
func (*memoryStore) CreateRevision(context.Context, string, string, string, int64, []byte, []byte) error {
	return nil
}
func (*memoryStore) ListRevisions(context.Context, string, string, string) ([]store.Revision, error) {
	return nil, nil
}
func (*memoryStore) GetRevision(context.Context, string, string, string, int64) (*store.Revision, error) {
	return nil, store.ErrNotFound
}
func (*memoryStore) AppendEvent(context.Context, string, string, string, store.InstanceEvent) error {
	return nil
}
func (*memoryStore) ListEvents(context.Context, string, string, string) ([]store.InstanceEvent, error) {
	return nil, nil
}

type modelAuthorizer struct{ err error }

func (a *modelAuthorizer) EvaluateModelAccess(context.Context, string, string, string) error {
	return a.err
}

type recordingModelQuota struct {
	err       error
	namespace string
	tier      string
}

func (q *recordingModelQuota) CheckModelQuota(_ context.Context, namespace, tier string) error {
	q.namespace, q.tier = namespace, tier
	return q.err
}

type staticFoundryTokenProvider struct {
	token string
	err   error
}

func (p staticFoundryTokenProvider) GetToken(context.Context, *cpi.Credentials) (string, error) {
	return p.token, p.err
}

func put(t *testing.T, store *memoryStore, kind, namespace, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	store.data[kind+"/"+namespace+"/"+name] = data
}

type upstreamCapture struct {
	authorization string
	traceparent   string
	model         string
}

func fixture(t *testing.T) (*Server, *memoryStore, *modelAuthorizer, *runtimeauth.ModelSessionCapabilityIssuer, runtimeauth.ModelSessionCapabilityBinding, *upstreamCapture) {
	t.Helper()
	capture := &upstreamCapture{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.authorization = r.Header.Get("Authorization")
		capture.traceparent = r.Header.Get("traceparent")
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		capture.model, _ = request["model"].(string)
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "completion", "choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}}, "usage": map[string]int{"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3}})
	}))
	t.Cleanup(upstream.Close)

	const namespace = "team-a"
	store := &memoryStore{data: map[string][]byte{}}
	agent := v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: namespace, UID: "agent-uid"},
		Spec:       v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "mock-model"}},
	}
	identity := v1alpha1.AgentIdentity{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-identity-planner", Namespace: namespace, Labels: map[string]string{"kognition.dev/auto-created-for-agent": "planner"}, OwnerReferences: []v1alpha1.OwnerReference{{Kind: "Agent", Name: "planner", UID: "agent-uid"}}}, Spec: v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}, SPIFFEID: "spiffe://kognition.dev/workspace/team-a/agent/planner"}, Status: v1alpha1.AgentIdentityStatus{Phase: v1alpha1.PhaseReady}}
	instance := v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-r1", Namespace: namespace, UID: "instance-uid"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}}, Status: v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhaseRunning, SessionID: "session-1"}}
	model := v1alpha1.Model{ObjectMeta: v1alpha1.ObjectMeta{Name: "mock-model", Namespace: namespace, ResourceVersion: 7}, Spec: v1alpha1.ModelSpec{Provider: "openai-compatible", Endpoint: upstream.URL, ModelID: "mock", CredentialRef: &v1alpha1.LocalObjectReference{Name: "model-key"}}}
	secret := v1alpha1.SecretRef{ObjectMeta: v1alpha1.ObjectMeta{Name: "model-key", Namespace: namespace, Annotations: map[string]string{"kognition.dev/internal-secret-data": `{"type":"api-key","apiKey":"upstream-secret"}`}}, Spec: v1alpha1.SecretRefSpec{BackendType: "internal"}}
	put(t, store, "Agent", namespace, agent.ObjectMeta.Name, agent)
	put(t, store, "AgentIdentity", namespace, identity.ObjectMeta.Name, identity)
	put(t, store, "AgentInstance", namespace, instance.ObjectMeta.Name, instance)
	put(t, store, "Model", namespace, model.ObjectMeta.Name, model)
	put(t, store, "SecretRef", namespace, secret.ObjectMeta.Name, secret)
	issuer, err := runtimeauth.NewModelSessionCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &modelAuthorizer{}
	server, err := New(store, issuer, authorizer, upstream.Client(), zaptest.NewLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	binding := runtimeauth.ModelSessionCapabilityBinding{Namespace: namespace, AgentName: agent.ObjectMeta.Name, AgentIdentityName: identity.ObjectMeta.Name, InstanceName: instance.ObjectMeta.Name, InstanceUID: instance.ObjectMeta.UID, SessionID: instance.Status.SessionID, ModelName: model.ObjectMeta.Name, ModelResourceVersion: model.ObjectMeta.ResourceVersion}
	return server, store, authorizer, issuer, binding, capture
}

func completionRequest(capability string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"mock","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	request.Header.Set("Authorization", "Bearer "+capability)
	return request
}

func TestCompletionUsageRequiresUsageAndDerivesMissingTotal(t *testing.T) {
	if usage, reported := completionUsage([]byte(`{"choices":[]}`)); reported || usage != (cpi.Usage{}) {
		t.Fatalf("missing usage = (%+v, %t), want zero/false", usage, reported)
	}
	if usage, reported := completionUsage([]byte(`{"usage":{}}`)); reported || usage != (cpi.Usage{}) {
		t.Fatalf("empty usage = (%+v, %t), want zero/false", usage, reported)
	}
	usage, reported := completionUsage([]byte(`{"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	if !reported || usage.PromptTokens != 2 || usage.CompletionTokens != 1 || usage.TotalTokens != 3 {
		t.Fatalf("derived usage = (%+v, %t)", usage, reported)
	}
	for _, payload := range []string{
		`{"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":1}}`,
		`{"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":0}}`,
		`{"usage":{"prompt_tokens":-1,"completion_tokens":1,"total_tokens":0}}`,
		`{"usage":{"prompt_tokens":9223372036854775807,"completion_tokens":1}}`,
	} {
		if usage, reported := completionUsage([]byte(payload)); reported || usage != (cpi.Usage{}) {
			t.Fatalf("invalid usage %s = (%+v, %t), want zero/false", payload, usage, reported)
		}
	}
}

func TestServerProxiesAuthorizedOpenAICompletionWithGatewayCredential(t *testing.T) {
	server, _, _, issuer, binding, capture := fixture(t)
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if capture.authorization != "Bearer upstream-secret" {
		t.Fatalf("upstream authorization = %q, want gateway API key", capture.authorization)
	}
	if capture.traceparent == "" {
		t.Fatal("gateway did not propagate traceparent to the upstream provider")
	}
	if capture.model != "mock" {
		t.Fatalf("upstream model = %q, want capability-bound Model ID", capture.model)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload["usage"] == nil {
		t.Fatalf("usage-preserving response = %s, err=%v", response.Body.String(), err)
	}
}

func TestServerRecordsRedactedModelTrustDecisionLifecycle(t *testing.T) {
	server, _, _, issuer, binding, capture := fixture(t)
	auditStore := &modelAuditStore{providerCalls: func() bool { return capture.model != "" }}
	server.audit = observability.NewCognitiveAuditLog(auditStore)
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := completionRequest(capability)
	request.Header.Set(observability.TrustDecisionRunIDHeader, "demo-run")
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || len(auditStore.entries) != 2 {
		t.Fatalf("status=%d entries=%d body=%s", response.Code, len(auditStore.entries), response.Body.String())
	}
	schema := loadModelTrustDecisionSchema(t)
	wantOutcomes := []string{"allowed", "completed"}
	for index, entry := range auditStore.entries {
		var decision any
		if err := json.Unmarshal(entry.Details, &decision); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(decision); err != nil {
			t.Fatalf("decision %d does not match schema: %v\n%s", index, err, entry.Details)
		}
		payload := string(entry.Details)
		if !strings.Contains(payload, `"outcome":"`+wantOutcomes[index]+`"`) || !strings.Contains(payload, `"runId":"demo-run"`) || strings.Contains(payload, "hello") || strings.Contains(payload, "upstream-secret") || strings.Contains(payload, `"content":"ok"`) {
			t.Fatalf("decision %d is incorrect or sensitive: %s", index, payload)
		}
	}
}

func TestServerAuthorizesAWorkflowMemberTaskCapabilityAndAuditsItsInvocation(t *testing.T) {
	server, st, _, issuer, binding, capture := fixture(t)
	auditStore := &modelAuditStore{providerCalls: func() bool { return capture.model != "" }}
	server.audit = observability.NewCognitiveAuditLog(auditStore)
	binding.WorkflowTask = &runtimeauth.ModelSessionWorkflowTask{Workspace: "team-a", RunUID: "run-uid", Step: "team", ActivationID: "collaboration", Attempt: 1, InvocationID: "wfc-invocation", TaskID: "task-1"}
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusOK || len(auditStore.entries) != 2 {
		t.Fatalf("status=%d entries=%d body=%s", response.Code, len(auditStore.entries), response.Body.String())
	}
	schema := loadModelTrustDecisionSchema(t)
	for index, entry := range auditStore.entries {
		var decision any
		if err := json.Unmarshal(entry.Details, &decision); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(decision); err != nil {
			t.Fatalf("decision %d does not match schema: %v\n%s", index, err, entry.Details)
		}
		if !strings.Contains(string(entry.Details), `"invocationId":"wfc-invocation"`) {
			t.Fatalf("decision %d lacks the member invocation: %s", index, entry.Details)
		}
	}
	// The task claim never substitutes for the live instance: a replaced session still denies the member.
	var instance v1alpha1.AgentInstance
	if err := json.Unmarshal(st.data["AgentInstance/team-a/planner-r1"], &instance); err != nil {
		t.Fatal(err)
	}
	instance.Status.SessionID = "replacement-session"
	put(t, st, "AgentInstance", "team-a", "planner-r1", instance)
	denied := httptest.NewRecorder()
	server.ServeHTTP(denied, completionRequest(capability))
	if denied.Code == http.StatusOK {
		t.Fatalf("a task capability outlived its instance session: %d", denied.Code)
	}
}

type modelRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn modelRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestServerAppliesAgentDeadlineToProviderRequest(t *testing.T) {
	server, st, _, issuer, binding, _ := fixture(t)
	var agent v1alpha1.Agent
	if err := json.Unmarshal(st.data["Agent/"+binding.Namespace+"/"+binding.AgentName], &agent); err != nil {
		t.Fatal(err)
	}
	agent.Spec.Runtime = &v1alpha1.AgentRuntimeSpec{Timeout: "2m"}
	put(t, st, "Agent", binding.Namespace, binding.AgentName, agent)
	called := false
	server.client = &http.Client{Transport: modelRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		deadline, ok := request.Context().Deadline()
		if remaining := time.Until(deadline); !ok || remaining <= time.Minute || remaining > 2*time.Minute {
			t.Errorf("provider deadline remaining = %v, present = %t", remaining, ok)
		}
		return nil, errors.New("synthetic provider failure")
	})}
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := completionRequest(capability)
	if _, ok := request.Context().Deadline(); ok {
		t.Fatal("test request unexpectedly has a client deadline")
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if !called || response.Code != http.StatusBadGateway {
		t.Fatalf("provider called = %t, status = %d", called, response.Code)
	}
}

func TestServerRejectsInvalidTrustDecisionRunIDBeforeProviderDispatch(t *testing.T) {
	server, _, _, issuer, binding, capture := fixture(t)
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := completionRequest(capability)
	request.Header.Set(observability.TrustDecisionRunIDHeader, strings.Repeat("a", observability.MaxTrustDecisionRunIDLength+1))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || capture.model != "" {
		t.Fatalf("status=%d provider model=%q body=%s", response.Code, capture.model, response.Body.String())
	}
}

func TestServerBlocksProviderWhenModelAuthorizationEvidenceCannotBePersisted(t *testing.T) {
	server, _, _, issuer, binding, capture := fixture(t)
	server.audit = observability.NewCognitiveAuditLog(&modelAuditStore{err: errors.New("audit unavailable")})
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusServiceUnavailable || capture.model != "" {
		t.Fatalf("status=%d provider model=%q body=%s", response.Code, capture.model, response.Body.String())
	}
}

func loadModelTrustDecisionSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	schema, err := compiler.Compile(filepath.Join("..", "..", "api", "schemas", "trust-decision-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestServerRejectsExhaustedModelTierBeforeProviderDispatch(t *testing.T) {
	server, store, _, issuer, binding, capture := fixture(t)
	var model v1alpha1.Model
	if err := json.Unmarshal(store.data["Model/team-a/mock-model"], &model); err != nil {
		t.Fatal(err)
	}
	model.Spec.ClassName = "premium-class"
	put(t, store, "Model", "team-a", model.ObjectMeta.Name, model)
	put(t, store, "ModelClass", "", "premium-class", v1alpha1.ModelClass{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "premium-class"},
		Spec:       v1alpha1.ModelClassSpec{Provisioner: "openai-compatible", CostTier: "premium"},
	})
	quota := &recordingModelQuota{err: errors.New("model-tier quota exhausted")}
	server.quota = quota
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusTooManyRequests || quota.namespace != "team-a" || quota.tier != "premium" || capture.model != "" {
		t.Fatalf("status=%d quota=%+v upstream model=%q body=%s", response.Code, quota, capture.model, response.Body.String())
	}
}

func TestServerRejectsMalformedPricingBeforeProviderDispatch(t *testing.T) {
	server, store, _, issuer, binding, capture := fixture(t)
	var model v1alpha1.Model
	if err := json.Unmarshal(store.data["Model/team-a/mock-model"], &model); err != nil {
		t.Fatal(err)
	}
	model.Spec.ClassName = "invalid-pricing"
	put(t, store, "Model", "team-a", model.ObjectMeta.Name, model)
	put(t, store, "ModelClass", "", "invalid-pricing", v1alpha1.ModelClass{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "invalid-pricing"},
		Spec: v1alpha1.ModelClassSpec{Provisioner: "openai-compatible", Parameters: map[string]string{
			observability.ModelInputCostMicrousdPerMillionTokensParameter: "2500000",
		}},
	})
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusForbidden || capture.model != "" {
		t.Fatalf("status=%d upstream model=%q body=%s", response.Code, capture.model, response.Body.String())
	}
}

func TestServerEmitsAttributedModelCostRecord(t *testing.T) {
	server, store, _, issuer, binding, _ := fixture(t)
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0, CostTagger: observability.NewCostTagger()})
	var costs []observability.CostRecord
	tracer.SetCostSink(func(record observability.CostRecord) { costs = append(costs, record) })
	server.tracer = tracer
	binding.Solution = "research-solution"
	binding.SolutionRev = 4

	var model v1alpha1.Model
	if err := json.Unmarshal(store.data["Model/team-a/mock-model"], &model); err != nil {
		t.Fatal(err)
	}
	model.Spec.ClassName = "priced"
	put(t, store, "Model", "team-a", model.ObjectMeta.Name, model)
	put(t, store, "ModelClass", "", "priced", v1alpha1.ModelClass{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "priced"},
		Spec: v1alpha1.ModelClassSpec{Provisioner: "openai-compatible", VersionPin: "2026-08-01", CostTier: "premium", Parameters: map[string]string{
			observability.ModelInputCostMicrousdPerMillionTokensParameter:  "2500000",
			observability.ModelOutputCostMicrousdPerMillionTokensParameter: "10000000",
		}},
	})
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	spans := tracer.Spans()
	if len(spans) != 1 {
		t.Fatalf("spans = %#v", spans)
	}
	span := spans[0]
	if span.Attributes[observability.AttrWorkspace] != "team-a" || span.Attributes[observability.AttrAgentIdentity] != "spiffe://kognition.dev/workspace/team-a/agent/planner" || span.Attributes[observability.AttrSolution] != "research-solution" || span.Attributes[observability.AttrSolutionRev] != int64(4) || span.Attributes[observability.AttrSessionID] != "session-1" || span.Attributes[observability.AttrPromptTokens] != int64(2) || span.Attributes[observability.AttrCompletionTokens] != int64(1) || span.Attributes[observability.AttrModelUsageReported] != true || span.Attributes[observability.AttrModelCostUnits] != int64(15) {
		t.Fatalf("model span attrs = %#v", span.Attributes)
	}
	if len(costs) != 1 || costs[0].Workspace != "team-a" || costs[0].AgentIdentity != "spiffe://kognition.dev/workspace/team-a/agent/planner" || costs[0].Solution != "research-solution" || costs[0].SolutionRev != 4 || costs[0].SessionID != "session-1" || costs[0].ReservationID != "agentinstance:instance-uid" || costs[0].ModelClass != "priced" || costs[0].ModelClassVersion != "2026-08-01" || costs[0].ModelCostTier != "premium" || !costs[0].UsageReported || costs[0].UsageEstimated || !costs[0].PricingConfigured || costs[0].InputCostMicrousdPerMillionTokens != 2500000 || costs[0].OutputCostMicrousdPerMillionTokens != 10000000 || costs[0].CostUnits != 15 {
		t.Fatalf("cost records = %#v", costs)
	}
}

func TestServerDoesNotAcknowledgeProviderSuccessWhenUsagePersistenceFails(t *testing.T) {
	server, _, _, issuer, binding, capture := fixture(t)
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0, CostTagger: observability.NewCostTagger()})
	tracer.SetFallibleCostSink(func(observability.CostRecord) error { return errors.New("cost store unavailable") })
	server.tracer = tracer
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503", response.Code, response.Body.String())
	}
	if capture.model != "mock" {
		t.Fatalf("provider model = %q, want dispatched model mock", capture.model)
	}
}

func TestServerProxiesFoundryProjectCompletion(t *testing.T) {
	server, store, _, issuer, binding, _ := fixture(t)
	var receivedPath, receivedVersion, receivedAPIKey, receivedModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedVersion = r.URL.Query().Get("api-version")
		receivedAPIKey = r.Header.Get("api-key")
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		receivedModel, _ = request["model"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "completion", "choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}}, "usage": map[string]int{"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3}})
	}))
	t.Cleanup(upstream.Close)

	var model v1alpha1.Model
	if err := json.Unmarshal(store.data["Model/team-a/mock-model"], &model); err != nil {
		t.Fatal(err)
	}
	model.Spec.Provider = "foundry"
	model.Spec.Endpoint = upstream.URL
	model.Spec.Parameters = map[string]string{"apiVersion": "2024-12-01-preview"}
	put(t, store, "Model", "team-a", "mock-model", model)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if receivedPath != "/chat/completions" {
		t.Fatalf("Foundry path = %q", receivedPath)
	}
	if receivedVersion != "2024-12-01-preview" {
		t.Fatalf("api-version = %q", receivedVersion)
	}
	if receivedAPIKey != "upstream-secret" {
		t.Fatalf("upstream API key = %q", receivedAPIKey)
	}
	if receivedModel != "mock" {
		t.Fatalf("upstream model = %q", receivedModel)
	}
}

func TestServerProxiesFoundryProjectCompletionWithAzureIdentity(t *testing.T) {
	server, store, _, issuer, binding, _ := fixture(t)
	server.tokens = staticFoundryTokenProvider{token: "entra-token"}
	var receivedAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthorization = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "completion", "choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}}, "usage": map[string]int{"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3}})
	}))
	t.Cleanup(upstream.Close)

	var model v1alpha1.Model
	if err := json.Unmarshal(store.data["Model/team-a/mock-model"], &model); err != nil {
		t.Fatal(err)
	}
	model.Spec.Provider = "foundry"
	model.Spec.Endpoint = upstream.URL + "/api/projects/sample"
	put(t, store, "Model", "team-a", "mock-model", model)
	put(t, store, "SecretRef", "team-a", "model-key", v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "model-key", Namespace: "team-a", Annotations: map[string]string{"kognition.dev/internal-secret-data": `{"type":"azure-identity","credentialType":"client-secret","tenantId":"tenant","clientId":"client","clientSecret":"secret"}`}},
		Spec:       v1alpha1.SecretRefSpec{BackendType: "internal"},
	})

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if receivedAuthorization != "Bearer entra-token" {
		t.Fatalf("upstream authorization = %q", receivedAuthorization)
	}
}

// TC-0299: A live grant or Model revision change invalidates a session capability.
func TestServerDeniesRevokedPolicyAndChangedModelRevision(t *testing.T) {
	server, store, authorizer, issuer, binding, _ := fixture(t)
	auditStore := &modelAuditStore{}
	server.audit = observability.NewCognitiveAuditLog(auditStore)
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	authorizer.err = errors.New("ModelGrant revoked")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusForbidden {
		t.Fatalf("revoked policy status=%d", response.Code)
	}
	assertModelDecisionOutcome(t, auditStore.entries[len(auditStore.entries)-1], "revoked", "grant_revoked")
	authorizer.err = nil
	var model v1alpha1.Model
	if err := json.Unmarshal(store.data["Model/team-a/mock-model"], &model); err != nil {
		t.Fatal(err)
	}
	model.ObjectMeta.ResourceVersion++
	put(t, store, "Model", "team-a", "mock-model", model)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusForbidden {
		t.Fatalf("changed revision status=%d", response.Code)
	}
	assertModelDecisionOutcome(t, auditStore.entries[len(auditStore.entries)-1], "revoked", "grant_revoked")
}

func assertModelDecisionOutcome(t *testing.T, entry observability.AuditEntry, outcome, reasonCode string) {
	t.Helper()
	var decision observability.TrustDecisionV1
	if err := json.Unmarshal(entry.Details, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Plane != "model" || decision.Outcome != outcome || decision.ReasonCode != reasonCode || decision.Execution.TargetReached == nil || *decision.Execution.TargetReached {
		t.Fatalf("decision = %#v", decision)
	}
	if decision.Identity == nil || decision.Identity.AgentIdentity == "" || decision.Identity.AgentIdentity == "unavailable" || decision.Identity.AgentIdentityRef == nil || decision.Identity.AgentIdentityRef.ResourceVersion == "" || decision.Authorization.Identity != "allowed" {
		t.Fatalf("verified identity lost on subsequent denial: %#v", decision)
	}
}

func TestServerPinsProviderRequestToCapabilityBoundModel(t *testing.T) {
	server, _, _, issuer, binding, capture := fixture(t)
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := completionRequest(capability)
	request.Body = io.NopCloser(strings.NewReader(`{"model":"unapproved-model","messages":[{"role":"user","content":"hello"}]}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || capture.model != "mock" {
		t.Fatalf("status=%d upstream model=%q", response.Code, capture.model)
	}
}

func TestServerRejectsStreamingCompletion(t *testing.T) {
	server, _, _, issuer, binding, capture := fixture(t)
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := completionRequest(capability)
	request.Body = io.NopCloser(strings.NewReader(`{"model":"mock","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "streaming model completions are not supported") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if capture.authorization != "" {
		t.Fatal("gateway forwarded an unsupported streaming request")
	}
}

func TestServerAcceptsOpenAICompatibleProviderFromModelClass(t *testing.T) {
	server, store, _, issuer, binding, capture := fixture(t)
	var model v1alpha1.Model
	if err := json.Unmarshal(store.data["Model/team-a/mock-model"], &model); err != nil {
		t.Fatal(err)
	}
	model.Spec.Provider = ""
	model.Spec.ClassName = "openai-class"
	put(t, store, "Model", "team-a", "mock-model", model)
	put(t, store, "ModelClass", "", "openai-class", v1alpha1.ModelClass{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "openai-class"},
		Spec:       v1alpha1.ModelClassSpec{Provisioner: "openai-compatible"},
	})
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusOK || capture.model != "mock" {
		t.Fatalf("status=%d upstream model=%q", response.Code, capture.model)
	}
}

func TestServerUsesFoundryEndpointForModelClassProvider(t *testing.T) {
	server, store, _, issuer, binding, _ := fixture(t)
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "completion", "choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}}, "usage": map[string]int{"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3}})
	}))
	t.Cleanup(upstream.Close)

	var model v1alpha1.Model
	if err := json.Unmarshal(store.data["Model/team-a/mock-model"], &model); err != nil {
		t.Fatal(err)
	}
	model.Spec.Provider = ""
	model.Spec.ClassName = "foundry-class"
	model.Spec.Endpoint = upstream.URL + "/api/projects/sample"
	put(t, store, "Model", "team-a", "mock-model", model)
	put(t, store, "ModelClass", "", "foundry-class", v1alpha1.ModelClass{ObjectMeta: v1alpha1.ObjectMeta{Name: "foundry-class"}, Spec: v1alpha1.ModelClassSpec{Provisioner: "foundry"}})

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if receivedPath != "/api/projects/sample/chat/completions" {
		t.Fatalf("Foundry ModelClass path = %q", receivedPath)
	}
}

func TestServerDeniesCapabilityWhenAgentChangesModelRef(t *testing.T) {
	server, store, _, issuer, binding, _ := fixture(t)
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	var agent v1alpha1.Agent
	if err := json.Unmarshal(store.data["Agent/team-a/planner"], &agent); err != nil {
		t.Fatal(err)
	}
	agent.Spec.ModelRef.Name = "replacement-model"
	put(t, store, "Agent", "team-a", "planner", agent)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestServerRejectsAmbiguousCredentialAndNormalizesProviderErrors(t *testing.T) {
	server, store, _, issuer, binding, _ := fixture(t)
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	var secret v1alpha1.SecretRef
	if err := json.Unmarshal(store.data["SecretRef/team-a/model-key"], &secret); err != nil {
		t.Fatal(err)
	}
	secret.ObjectMeta.Annotations["kognition.dev/internal-secret-data"] = `{"type":"api-key","description":"not-a-key"}`
	put(t, store, "SecretRef", "team-a", "model-key", secret)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusForbidden {
		t.Fatalf("ambiguous credential status=%d", response.Code)
	}
}

func TestServerDeniesCapabilityWhenSessionChanges(t *testing.T) {
	server, store, _, issuer, binding, _ := fixture(t)
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	var instance v1alpha1.AgentInstance
	if err := json.Unmarshal(store.data["AgentInstance/team-a/planner-r1"], &instance); err != nil {
		t.Fatal(err)
	}
	instance.Status.SessionID = "replacement-session"
	put(t, store, "AgentInstance", "team-a", "planner-r1", instance)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
