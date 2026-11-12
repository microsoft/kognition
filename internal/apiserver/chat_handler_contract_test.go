// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/openai"
	"github.com/kognition-ai/kognition/internal/modelgateway"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/opa"
)

type recordingChatDriver struct {
	request  *cpi.CompletionRequest
	response *cpi.CompletionResponse
	err      error
}

type scriptedChatDriver struct {
	responses []*cpi.CompletionResponse
	requests  []cpi.CompletionRequest
}

type chatSpanExporter struct {
	spans []*observability.Span
}

func (e *chatSpanExporter) ExportSpans(_ context.Context, spans []*observability.Span) error {
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *chatSpanExporter) Shutdown(context.Context) error { return nil }

func (*scriptedChatDriver) Name() string { return "recording" }
func (*scriptedChatDriver) Validate(context.Context, *v1alpha1.ModelSpec, *v1alpha1.ModelClassSpec) error {
	return nil
}
func (*scriptedChatDriver) Probe(context.Context, *v1alpha1.ModelSpec, *cpi.Credentials) (*cpi.HealthResult, error) {
	return &cpi.HealthResult{Available: true}, nil
}
func (d *scriptedChatDriver) Complete(_ context.Context, request *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	copyRequest := *request
	copyRequest.Messages = append([]cpi.Message(nil), request.Messages...)
	copyRequest.Tools = append([]cpi.ToolDefinition(nil), request.Tools...)
	d.requests = append(d.requests, copyRequest)
	if len(d.responses) == 0 {
		return nil, errors.New("unexpected completion call")
	}
	response := d.responses[0]
	d.responses = d.responses[1:]
	return response, nil
}
func (*scriptedChatDriver) Embed(context.Context, *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return nil, nil
}

type recordingChatToolInvoker struct {
	toolName string
	input    map[string]any
	output   map[string]any
	err      error
	calls    int
}

func (i *recordingChatToolInvoker) InvokeTool(_ context.Context, _, _, toolName string, input map[string]any) (map[string]any, error) {
	i.calls++
	i.toolName = toolName
	i.input = input
	return i.output, i.err
}

type chatReadFaultStore struct {
	*memStore
	failKind string
	err      error
}

type recordingChatQuota struct {
	checkErr      error
	modelErr      error
	reserveErr    error
	budgetErr     error
	settleErr     error
	checks        int
	modelChecks   int
	reserves      int
	budgets       int
	settles       int
	namespace     string
	sessionID     string
	inputCeiling  int64
	outputCeiling int64
	actualInput   int64
	actualOutput  int64
}

func (q *recordingChatQuota) CheckModelQuota(_ context.Context, namespace, _ string) error {
	q.modelChecks++
	q.namespace = namespace
	return q.modelErr
}

func (q *recordingChatQuota) CheckCallBudget(_ context.Context, namespace, _ string, _, _ int64) error {
	q.budgets++
	q.namespace = namespace
	return q.budgetErr
}

func (q *recordingChatQuota) CheckSchedulerQuota(_ context.Context, namespace string) error {
	q.checks++
	q.namespace = namespace
	return q.checkErr
}

func (q *recordingChatQuota) ReserveTokens(_ context.Context, namespace, sessionID string, inputCeiling, outputCeiling int64) error {
	q.reserves++
	q.namespace = namespace
	q.sessionID = sessionID
	q.inputCeiling = inputCeiling
	q.outputCeiling = outputCeiling
	return q.reserveErr
}

func (q *recordingChatQuota) Settle(_ context.Context, sessionID string, actualInput, actualOutput int64) error {
	q.settles++
	q.sessionID = sessionID
	q.actualInput = actualInput
	q.actualOutput = actualOutput
	return q.settleErr
}

func (s *chatReadFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == s.failKind {
		return nil, s.err
	}
	return s.memStore.Get(ctx, kind, namespace, name)
}

func (*recordingChatDriver) Name() string { return "recording" }
func (*recordingChatDriver) Validate(context.Context, *v1alpha1.ModelSpec, *v1alpha1.ModelClassSpec) error {
	return nil
}
func (*recordingChatDriver) Probe(context.Context, *v1alpha1.ModelSpec, *cpi.Credentials) (*cpi.HealthResult, error) {
	return &cpi.HealthResult{Available: true}, nil
}
func (d *recordingChatDriver) Complete(_ context.Context, req *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	d.request = req
	return d.response, d.err
}
func (*recordingChatDriver) Embed(context.Context, *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return nil, nil
}

func TestChatHandler_ValidatesRequest(t *testing.T) {
	tests := []struct {
		name       string
		registry   *cpi.Registry
		body       string
		wantStatus int
		wantError  string
	}{
		{name: "unavailable", body: `{"message":"hello"}`, wantStatus: http.StatusServiceUnavailable, wantError: "no CPI drivers configured"},
		{name: "empty body", registry: cpi.NewRegistry(), wantStatus: http.StatusBadRequest, wantError: "request body is required"},
		{name: "malformed JSON", registry: cpi.NewRegistry(), body: `{`, wantStatus: http.StatusBadRequest, wantError: "invalid JSON body"},
		{name: "missing message", registry: cpi.NewRegistry(), body: `{}`, wantStatus: http.StatusBadRequest, wantError: "message is required"},
		{name: "blank message", registry: cpi.NewRegistry(), body: `{"message":"  "}`, wantStatus: http.StatusBadRequest, wantError: "message is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := New(Config{}, newMemStore(), zap.NewNop(), WithCPIRegistry(tt.registry))
			response := performChatRequest(srv, tt.body)
			if response.Code != tt.wantStatus || !strings.Contains(response.Body.String(), tt.wantError) {
				t.Fatalf("response = %d %s, want %d containing %q", response.Code, response.Body, tt.wantStatus, tt.wantError)
			}
		})
	}
}

func TestChatHandler_ReportsResourceResolutionFailures(t *testing.T) {
	tests := []struct {
		name       string
		seed       func(*memStore)
		wantStatus int
		wantError  string
	}{
		{name: "agent not found", seed: func(*memStore) {}, wantStatus: http.StatusNotFound, wantError: "not found in namespace"},
		{name: "malformed agent", seed: func(s *memStore) { putRaw(s, "Agent", "default", "test-agent", []byte(`{`)) }, wantStatus: http.StatusInternalServerError, wantError: "internal error"},
		{name: "agent not ready", seed: func(s *memStore) { putAgent(s, v1alpha1.PhasePending) }, wantStatus: http.StatusServiceUnavailable, wantError: "is not ready"},
		{name: "model not found", seed: func(s *memStore) { putAgent(s, v1alpha1.PhaseReady) }, wantStatus: http.StatusUnprocessableEntity, wantError: "referenced by agent not found"},
		{name: "malformed model", seed: func(s *memStore) {
			putAgent(s, v1alpha1.PhaseReady)
			putRaw(s, "Model", "default", "test-model", []byte(`{`))
		}, wantStatus: http.StatusInternalServerError, wantError: "internal error"},
		{name: "class not found", seed: func(s *memStore) { putAgent(s, v1alpha1.PhaseReady); putModel(s) }, wantStatus: http.StatusUnprocessableEntity, wantError: "referenced by model not found"},
		{name: "malformed class", seed: func(s *memStore) {
			putAgent(s, v1alpha1.PhaseReady)
			putModel(s)
			putRaw(s, "ModelClass", "", "test-class", []byte(`{`))
		}, wantStatus: http.StatusInternalServerError, wantError: "internal error"},
		{name: "driver not registered", seed: func(s *memStore) { putAgent(s, v1alpha1.PhaseReady); putModel(s); putModelClass(s) }, wantStatus: http.StatusUnprocessableEntity, wantError: "no CPI driver registered for provisioner"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			memory := newMemStore()
			tt.seed(memory)
			srv := New(Config{}, memory, zap.NewNop(), WithCPIRegistry(cpi.NewRegistry()))
			response := performChatRequest(srv, `{"message":"hello"}`)
			if response.Code != tt.wantStatus || !strings.Contains(response.Body.String(), tt.wantError) {
				t.Fatalf("response = %d %s, want %d containing %q", response.Code, response.Body, tt.wantStatus, tt.wantError)
			}
		})
	}
}

func TestChatHandler_MasksResourceLookupFailures(t *testing.T) {
	tests := []struct {
		name     string
		failKind string
		seed     func(*memStore)
	}{
		{name: "agent lookup", failKind: "Agent", seed: func(*memStore) {}},
		{name: "model lookup", failKind: "Model", seed: func(s *memStore) { putAgent(s, v1alpha1.PhaseReady) }},
		{name: "model class lookup", failKind: "ModelClass", seed: func(s *memStore) { putAgent(s, v1alpha1.PhaseReady); putModel(s) }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newMemStore()
			test.seed(memory)
			backendMessage := "storage credentials expired"
			faults := &chatReadFaultStore{memStore: memory, failKind: test.failKind, err: errors.New(backendMessage)}
			srv := New(Config{}, faults, zap.NewNop(), WithCPIRegistry(cpi.NewRegistry()))

			response := performChatRequest(srv, `{"message":"hello"}`)
			if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "internal error") {
				t.Fatalf("response = %d %s, want 500 internal error", response.Code, response.Body)
			}
			if strings.Contains(response.Body.String(), backendMessage) {
				t.Fatalf("response leaked backend error: %s", response.Body)
			}
		})
	}
}

func TestChatHandler_CompletesWithResolvedPromptAndCredentials(t *testing.T) {
	memory := newMemStore()
	putAgentWithPromptRef(memory)
	putModel(memory)
	putModelClass(memory)
	putJSON(memory, "Prompt", "default", "system-prompt", v1alpha1.Prompt{Spec: v1alpha1.PromptSpec{Content: "be concise"}})
	putJSON(memory, "SecretRef", "default", "model-secret", v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"api-key","apiKey":"secret"}`,
		}},
		Spec: v1alpha1.SecretRefSpec{BackendType: "internal"},
	})

	driver := &recordingChatDriver{response: &cpi.CompletionResponse{
		Content: "hello back",
		Usage:   cpi.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5, Reported: true},
	}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	srv := New(Config{}, memory, zap.NewNop(), WithCPIRegistry(registry))

	response := performChatRequest(srv, `{"message":"hello","sessionID":"session-1"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body)
	}
	var got ChatResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.SessionID != "session-1" || got.Response != "hello back" || got.Usage == nil || got.Usage.TotalTokens != 5 {
		t.Fatalf("response = %+v", got)
	}
	if driver.request == nil || driver.request.Model != "model-id" || driver.request.Endpoint != "https://model.test" {
		t.Fatalf("completion request = %+v", driver.request)
	}
	if driver.request.Creds == nil || driver.request.Creds.Type != "api-key" || driver.request.Creds.Data["apiKey"] != "secret" {
		t.Fatalf("credentials = %+v", driver.request.Creds)
	}
	if len(driver.request.Messages) != 2 || driver.request.Messages[0].Role != "system" || driver.request.Messages[0].Content != "be concise" || driver.request.Messages[1].Content != "hello" {
		t.Fatalf("messages = %+v", driver.request.Messages)
	}
	if driver.request.Parameters["temperature"] != "0.2" {
		t.Fatalf("parameters = %+v", driver.request.Parameters)
	}
	events, err := memory.ListEvents(context.Background(), "Agent", "default", "test-agent")
	if err != nil || len(events) != 1 || events[0].Reason != "ChatCompleted" || !strings.Contains(events[0].Message, "tokens=5") {
		t.Fatalf("events = %+v, err = %v", events, err)
	}
}

func TestChatHandler_ResolvesInvocationProfileBeforeQuotaAndDispatch(t *testing.T) {
	memory := newMemStore()
	putAgent(memory, v1alpha1.PhaseReady)
	maximum := int64(128)
	profile := v1alpha1.ModelInvocationProfile{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "chat-profile", Namespace: "default", UID: "profile-uid"},
		Spec: v1alpha1.ModelInvocationProfileSpec{
			Operation: "chat",
			Defaults:  v1alpha1.InvocationDefaults{MaxCompletionTokens: &maximum},
			Limits:    v1alpha1.InvocationLimits{MaxCompletionTokens: &maximum},
		},
	}
	digest, err := modelinvocation.Digest(profile.Spec)
	if err != nil {
		t.Fatal(err)
	}
	putJSON(memory, "ModelInvocationProfile", "default", profile.Name, profile)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body["max_tokens"]) != "128" || string(body["model"]) != `"model-id"` {
			t.Errorf("provider request did not use the bounded profile: %s", mustMarshal(t, body))
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()
	putJSON(memory, "Model", "default", "test-model", v1alpha1.Model{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "test-model", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			ClassName: "test-class", ModelID: "model-id", Endpoint: upstream.URL,
			InvocationProfileRef: &v1alpha1.ModelInvocationProfileReference{Name: profile.Name, SpecDigest: digest},
			InvocationContract:   "openai-chat-v1",
		},
	})
	putJSON(memory, "ModelClass", "", "test-class", v1alpha1.ModelClass{
		Spec: v1alpha1.ModelClassSpec{Provisioner: "openai-compatible"},
	})
	registry := cpi.NewRegistry()
	registry.MustRegister(openai.New(upstream.Client()))
	quota := &recordingChatQuota{}
	server := New(Config{}, memory, zap.NewNop(), WithCPIRegistry(registry), WithChatQuotaEnforcer(quota))

	response := performChatRequest(server, `{"message":"hello"}`)
	if response.Code != http.StatusOK || calls != 1 || quota.outputCeiling != maximum {
		t.Fatalf("status=%d body=%s provider calls=%d reserved output=%d", response.Code, response.Body, calls, quota.outputCeiling)
	}
	profile.Spec.Defaults.Reasoning = "disabled"
	putJSON(memory, "ModelInvocationProfile", "default", profile.Name, profile)
	response = performChatRequest(server, `{"message":"hello"}`)
	if response.Code != http.StatusUnprocessableEntity || calls != 1 || quota.reserves != 1 {
		t.Fatalf("changed profile reached quota or provider: status=%d calls=%d reservations=%d", response.Code, calls, quota.reserves)
	}
}

func TestChatHandler_DoesNotAcknowledgeProviderSuccessWhenUsagePersistenceFails(t *testing.T) {
	memory := newMemStore()
	putAgentWithPromptRef(memory)
	putModel(memory)
	putModelClass(memory)
	putJSON(memory, "Prompt", "default", "system-prompt", v1alpha1.Prompt{Spec: v1alpha1.PromptSpec{Content: "be concise"}})
	putJSON(memory, "SecretRef", "default", "model-secret", v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{Annotations: map[string]string{"kognition.dev/internal-secret-data": `{"type":"api-key","apiKey":"secret"}`}},
		Spec:       v1alpha1.SecretRefSpec{BackendType: "internal"},
	})
	driver := &recordingChatDriver{response: &cpi.CompletionResponse{Content: "hello back", Usage: cpi.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5, Reported: true}}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0, CostTagger: observability.NewCostTagger()})
	tracer.SetFallibleCostSink(func(observability.CostRecord) error { return errors.New("cost store unavailable") })
	server := New(Config{}, memory, zap.NewNop(), WithCPIRegistry(registry), WithChatTracing(tracer, nil))

	response := performChatRequest(server, `{"message":"hello","sessionID":"session-1"}`)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), modelInferenceFailedMessage) {
		t.Fatalf("response = %d %s, want accounting failure", response.Code, response.Body)
	}
	if driver.request == nil {
		t.Fatal("provider was not called before accounting failure")
	}
}

func TestChatHandler_EnforcesModelGrantBeforeDispatch(t *testing.T) {
	newServer := func(t *testing.T, withGrant bool, policy *v1alpha1.AgentPolicy) (*Server, *recordingChatDriver, *memoryAuditStore) {
		t.Helper()
		memory := newMemStore()
		putAgent(memory, v1alpha1.PhaseReady)
		putModel(memory)
		putModelClass(memory)
		if withGrant {
			putJSON(memory, "ModelGrant", "default", "test-agent-model", v1alpha1.ModelGrant{
				Spec: v1alpha1.ModelGrantSpec{
					AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"},
					ModelRef: v1alpha1.LocalObjectReference{Name: "test-model"},
				},
			})
		}
		if policy != nil {
			putJSON(memory, "AgentPolicy", "default", policy.ObjectMeta.Name, *policy)
		}
		driver := &recordingChatDriver{response: &cpi.CompletionResponse{Content: "allowed", Usage: cpi.Usage{Reported: true}}}
		registry := cpi.NewRegistry()
		registry.MustRegister(driver)
		policyEvaluator := controller.NewAgentPolicyReconciler(memory, opa.NewEngine(), zap.NewNop())
		invocationPolicies := controller.NewInvocationPolicyEngine(
			controller.NewToolGrantEngine(memory, zap.NewNop()),
			policyEvaluator,
		)
		core, err := modelgateway.NewCore(memory, invocationPolicies)
		if err != nil {
			t.Fatal(err)
		}
		auditStore := &memoryAuditStore{}
		return New(
			Config{},
			memory,
			zap.NewNop(),
			WithCPIRegistry(registry),
			WithModelAccessAuthorizer(invocationPolicies),
			WithModelGatewayCore(core),
			WithAuditLog(observability.NewCognitiveAuditLog(auditStore)),
		), driver, auditStore
	}

	t.Run("denies without grant", func(t *testing.T) {
		server, driver, auditStore := newServer(t, false, nil)
		response := performChatRequest(server, `{"message":"hello"}`)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "model access denied") {
			t.Fatalf("response = %d %s, want 403 model access denied", response.Code, response.Body)
		}
		if driver.request != nil {
			t.Fatalf("driver invoked after authorization denial: %+v", driver.request)
		}
		assertModelDecisions(t, auditStore.entries, []string{"denied"})
	})

	t.Run("dispatches with matching grant", func(t *testing.T) {
		server, driver, auditStore := newServer(t, true, nil)
		response := performChatRequest(server, `{"message":"hello"}`)
		if response.Code != http.StatusOK {
			t.Fatalf("response = %d %s, want 200", response.Code, response.Body)
		}
		if driver.request == nil {
			t.Fatal("driver was not invoked after ModelGrant authorization")
		}
		assertModelDecisions(t, auditStore.entries, []string{"allowed", "completed"})
	})

	for _, test := range []struct {
		name       string
		enforce    bool
		wantStatus int
		wantCall   bool
	}{
		{name: "enforced policy denies", enforce: true, wantStatus: http.StatusForbidden},
		{name: "warn-only policy allows", enforce: false, wantStatus: http.StatusOK, wantCall: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := &v1alpha1.AgentPolicy{
				ObjectMeta: v1alpha1.ObjectMeta{Name: "model-policy", Namespace: "default"},
				Spec: v1alpha1.AgentPolicySpec{
					Enforce:      test.enforce,
					Scope:        "agent",
					TargetAgents: []string{"test-agent"},
					Rules:        `deny[msg] { input.model == "test-model"; msg := "model blocked" }`,
				},
			}
			server, driver, _ := newServer(t, true, policy)
			response := performChatRequest(server, `{"message":"hello"}`)
			if response.Code != test.wantStatus {
				t.Fatalf("response = %d %s, want %d", response.Code, response.Body, test.wantStatus)
			}
			if (driver.request != nil) != test.wantCall {
				t.Fatalf("driver called = %v, want %v", driver.request != nil, test.wantCall)
			}
		})
	}

	t.Run("blocks dispatch when authorization evidence cannot persist", func(t *testing.T) {
		server, driver, auditStore := newServer(t, true, nil)
		auditStore.err = errors.New("audit unavailable")
		response := performChatRequest(server, `{"message":"sensitive prompt"}`)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("response = %d %s, want 503", response.Code, response.Body)
		}
		if driver.request != nil {
			t.Fatalf("driver invoked after audit failure: %+v", driver.request)
		}
	})
}

func assertModelDecisions(t *testing.T, entries []observability.AuditEntry, wantOutcomes []string) {
	t.Helper()
	if len(entries) != len(wantOutcomes) {
		t.Fatalf("model audit entries = %d, want %d: %+v", len(entries), len(wantOutcomes), entries)
	}
	for index, entry := range entries {
		if entry.EventType != observability.AuditModelCall || entry.SubjectRef != "test-model" {
			t.Fatalf("entry[%d] = %+v", index, entry)
		}
		var decision observability.TrustDecisionV1
		if err := json.Unmarshal(entry.Details, &decision); err != nil {
			t.Fatalf("decode entry[%d]: %v", index, err)
		}
		if decision.SchemaVersion != 1 || decision.Plane != "model" || decision.Outcome != wantOutcomes[index] || decision.Request.TargetRef.Name != "test-model" {
			t.Fatalf("decision[%d] = %+v, want outcome %q", index, decision, wantOutcomes[index])
		}
		for _, secret := range []string{"sensitive prompt", "model-id", "https://model.test", "apiKey"} {
			if strings.Contains(string(entry.Details), secret) {
				t.Fatalf("entry[%d] leaked %q: %s", index, secret, entry.Details)
			}
		}
	}
}

func TestChatHandler_AccountsQuotaAcrossModelLifecycle(t *testing.T) {
	newServer := func(t *testing.T, quota *recordingChatQuota, driver *recordingChatDriver) (*Server, *memStore) {
		t.Helper()
		memory := newMemStore()
		maxTokens := int64(256)
		agent := v1alpha1.Agent{Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "test-model"},
			Instructions: v1alpha1.AgentInstructions{Ref: &v1alpha1.LocalObjectReference{Name: "system-prompt"}},
			Runtime:      &v1alpha1.AgentRuntimeSpec{MaxTokens: &maxTokens},
		}}
		agent.Status.Phase = v1alpha1.PhaseReady
		putJSON(memory, "Agent", "default", "test-agent", agent)
		putModel(memory)
		putModelClass(memory)
		putJSON(memory, "Prompt", "default", "system-prompt", v1alpha1.Prompt{Spec: v1alpha1.PromptSpec{Content: "be concise"}})
		registry := cpi.NewRegistry()
		registry.MustRegister(driver)
		return New(Config{}, memory, zap.NewNop(), WithCPIRegistry(registry), WithChatQuotaEnforcer(quota)), memory
	}

	t.Run("scheduler denial prevents reservation and dispatch", func(t *testing.T) {
		quota := &recordingChatQuota{checkErr: errors.New("quota exhausted")}
		driver := &recordingChatDriver{}
		server, _ := newServer(t, quota, driver)
		response := performChatRequest(server, `{"message":"hello","sessionID":"session-1"}`)
		if response.Code != http.StatusTooManyRequests || quota.checks != 1 || quota.reserves != 0 || driver.request != nil {
			t.Fatalf("response=%d checks=%d reserves=%d driver=%+v", response.Code, quota.checks, quota.reserves, driver.request)
		}
	})

	t.Run("reservation denial prevents dispatch", func(t *testing.T) {
		quota := &recordingChatQuota{reserveErr: errors.New("token budget exceeded")}
		driver := &recordingChatDriver{}
		server, _ := newServer(t, quota, driver)
		response := performChatRequest(server, `{"message":"hello","sessionID":"session-1"}`)
		if response.Code != http.StatusTooManyRequests || quota.checks != 1 || quota.reserves != 1 || quota.settles != 0 || driver.request != nil {
			t.Fatalf("response=%d quota=%+v driver=%+v", response.Code, quota, driver.request)
		}
	})

	t.Run("session budget denial prevents reservation and dispatch", func(t *testing.T) {
		quota := &recordingChatQuota{budgetErr: errors.New("session budget exceeded")}
		driver := &recordingChatDriver{}
		server, _ := newServer(t, quota, driver)
		response := performChatRequest(server, `{"message":"hello","sessionID":"session-1"}`)
		if response.Code != http.StatusTooManyRequests || quota.budgets != 1 || quota.reserves != 0 || driver.request != nil {
			t.Fatalf("response=%d quota=%+v driver=%+v", response.Code, quota, driver.request)
		}
	})

	t.Run("model quota denial prevents reservation and dispatch", func(t *testing.T) {
		quota := &recordingChatQuota{modelErr: errors.New("model-tier quota exhausted")}
		driver := &recordingChatDriver{}
		server, _ := newServer(t, quota, driver)
		response := performChatRequest(server, `{"message":"hello","sessionID":"session-1"}`)
		if response.Code != http.StatusTooManyRequests || quota.modelChecks != 1 || quota.reserves != 0 || driver.request != nil {
			t.Fatalf("response=%d quota=%+v driver=%+v", response.Code, quota, driver.request)
		}
	})

	t.Run("success reserves ceiling and settles exact usage", func(t *testing.T) {
		quota := &recordingChatQuota{}
		driver := &recordingChatDriver{response: &cpi.CompletionResponse{Content: "ok", Usage: cpi.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5, Reported: true}}}
		server, _ := newServer(t, quota, driver)
		response := performChatRequest(server, `{"message":"hello","sessionID":"session-1"}`)
		if response.Code != http.StatusOK {
			t.Fatalf("response = %d %s", response.Code, response.Body)
		}
		if quota.namespace != "default" || !strings.HasPrefix(quota.sessionID, "chat:") || quota.inputCeiling != 4 || quota.outputCeiling != 256 {
			t.Fatalf("reservation = %+v, want server-generated chat reservation at ceiling 4/256", quota)
		}
		if quota.settles != 1 || quota.actualInput != 3 || quota.actualOutput != 2 {
			t.Fatalf("settlement = %+v, want exact usage 3/2", quota)
		}
	})

	t.Run("failed model call refunds reservation", func(t *testing.T) {
		quota := &recordingChatQuota{}
		driver := &recordingChatDriver{err: errors.New("model unavailable")}
		server, _ := newServer(t, quota, driver)
		response := performChatRequest(server, `{"message":"hello","sessionID":"session-1"}`)
		if response.Code != http.StatusBadGateway || quota.settles != 1 || quota.actualInput != 0 || quota.actualOutput != 0 {
			t.Fatalf("response=%d quota=%+v, want failed call refund", response.Code, quota)
		}
	})

	t.Run("missing provider usage retains reservation", func(t *testing.T) {
		quota := &recordingChatQuota{}
		driver := &recordingChatDriver{response: &cpi.CompletionResponse{Content: "unaccounted"}}
		server, _ := newServer(t, quota, driver)
		response := performChatRequest(server, `{"message":"hello","sessionID":"session-1"}`)
		if response.Code != http.StatusBadGateway || quota.reserves != 1 || quota.settles != 0 {
			t.Fatalf("response=%d quota=%+v, want retained reservation", response.Code, quota)
		}
	})

	t.Run("settlement failure is observable without repeating model call", func(t *testing.T) {
		quota := &recordingChatQuota{settleErr: errors.New("quota store unavailable")}
		driver := &recordingChatDriver{response: &cpi.CompletionResponse{Content: "ok", Usage: cpi.Usage{Reported: true}}}
		server, memory := newServer(t, quota, driver)
		response := performChatRequest(server, `{"message":"hello","sessionID":"session-1"}`)
		if response.Code != http.StatusOK || quota.settles != 1 || driver.request == nil {
			t.Fatalf("response=%d quota=%+v driver=%+v", response.Code, quota, driver.request)
		}
		events, err := memory.ListEvents(context.Background(), "Agent", "default", "test-agent")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, event := range events {
			if event.Reason == "QuotaSettlementFailed" {
				found = true
			}
		}
		if !found {
			t.Fatalf("events = %+v, want QuotaSettlementFailed", events)
		}
	})
}

func TestChatHandler_ExecutesDeclaredToolsAndReturnsFollowUpCompletion(t *testing.T) {
	memory := newMemStore()
	agent := v1alpha1.Agent{Spec: v1alpha1.AgentSpec{
		ModelRef: v1alpha1.LocalObjectReference{Name: "test-model"},
		Tools:    []v1alpha1.AgentToolRef{{Ref: v1alpha1.LocalObjectReference{Name: "weather"}}},
	}}
	agent.Status.Phase = v1alpha1.PhaseReady
	putJSON(memory, "Agent", "default", "test-agent", agent)
	putModel(memory)
	putModelClass(memory)
	putJSON(memory, "Tool", "default", "weather", v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "weather", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Description: "Current weather",
			Schema:      map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		},
	})
	driver := &scriptedChatDriver{responses: []*cpi.CompletionResponse{
		{ToolCalls: []cpi.ToolCall{{ID: "call-1", Name: "weather", Arguments: `{"city":"Seattle"}`}}, Usage: cpi.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, Reported: true}},
		{Content: "Rain is expected.", Usage: cpi.Usage{PromptTokens: 8, CompletionTokens: 3, TotalTokens: 11, Reported: true}},
	}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	invoker := &recordingChatToolInvoker{output: map[string]any{"forecast": "rain"}}
	server := New(Config{}, memory, zap.NewNop(), WithCPIRegistry(registry), WithChatToolInvoker(invoker))

	response := performChatRequest(server, `{"message":"What is the weather?","sessionID":"tool-session"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body)
	}
	var chat ChatResponse
	if err := json.Unmarshal(response.Body.Bytes(), &chat); err != nil {
		t.Fatal(err)
	}
	if chat.Response != "Rain is expected." || chat.Usage == nil || chat.Usage.PromptTokens != 18 || chat.Usage.CompletionTokens != 5 || chat.Usage.TotalTokens != 23 {
		t.Fatalf("chat response = %+v, want cumulative tool-loop result", chat)
	}
	if invoker.calls != 1 || invoker.toolName != "weather" || invoker.input["city"] != "Seattle" {
		t.Fatalf("tool invocation = %+v", invoker)
	}
	if len(driver.requests) != 2 || len(driver.requests[0].Tools) != 1 || driver.requests[0].Tools[0].Name != "weather" {
		t.Fatalf("completion requests = %+v", driver.requests)
	}
	followUp := driver.requests[1].Messages
	if len(followUp) != 3 || len(followUp[1].ToolCalls) != 1 || followUp[1].ToolCalls[0].ID != "call-1" || followUp[2].Role != "tool" || followUp[2].ToolCallID != "call-1" || !strings.Contains(followUp[2].Content, "rain") {
		t.Fatalf("follow-up messages = %+v", followUp)
	}
}

func TestChatHandler_EmitsNestedDirectChatSpans(t *testing.T) {
	memory := newMemStore()
	agent := v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "test-agent", Namespace: "default", Labels: map[string]string{"kognition.dev/solution": "research-solution"}}, Spec: v1alpha1.AgentSpec{
		ModelRef: v1alpha1.LocalObjectReference{Name: "test-model"},
		Tools:    []v1alpha1.AgentToolRef{{Ref: v1alpha1.LocalObjectReference{Name: "weather"}}},
	}}
	agent.Status.Phase = v1alpha1.PhaseReady
	putJSON(memory, "Agent", "default", "test-agent", agent)
	putJSON(memory, "AgentIdentity", "default", "agent-identity-test-agent", v1alpha1.AgentIdentity{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-identity-test-agent", Namespace: "default"}, Spec: v1alpha1.AgentIdentitySpec{SPIFFEID: "spiffe://kognition.dev/workspace/default/agent/test-agent"}})
	putJSON(memory, "Solution", "default", "research-solution", v1alpha1.Solution{ObjectMeta: v1alpha1.ObjectMeta{Name: "research-solution", Namespace: "default"}, Status: v1alpha1.SolutionStatus{ActiveRevision: 3}})
	putModel(memory)
	putJSON(memory, "ModelClass", "", "test-class", v1alpha1.ModelClass{Spec: v1alpha1.ModelClassSpec{Provisioner: "recording", Parameters: map[string]string{
		observability.ModelInputCostMicrousdPerMillionTokensParameter:  "2500000",
		observability.ModelOutputCostMicrousdPerMillionTokensParameter: "10000000",
	}}})
	putJSON(memory, "Tool", "default", "weather", v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "weather", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Description: "Current weather",
			Schema:      map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		},
	})
	driver := &scriptedChatDriver{responses: []*cpi.CompletionResponse{
		{ToolCalls: []cpi.ToolCall{{ID: "call-1", Name: "weather", Arguments: `{"city":"Seattle"}`}}, Usage: cpi.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, Reported: true}},
		{Content: "Rain is expected.", Usage: cpi.Usage{PromptTokens: 8, CompletionTokens: 3, TotalTokens: 11, Reported: true}},
	}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0, CostTagger: observability.NewCostTagger()})
	var costs []observability.CostRecord
	tracer.SetCostSink(func(record observability.CostRecord) { costs = append(costs, record) })
	collector := observability.NewCollector()
	exporter := &chatSpanExporter{}
	collector.RegisterSpanExporter(exporter)
	server := New(
		Config{},
		memory,
		zap.NewNop(),
		WithCPIRegistry(registry),
		WithChatToolInvoker(&recordingChatToolInvoker{output: map[string]any{"forecast": "rain"}}),
		WithChatTracing(tracer, collector),
	)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/agents/test-agent/chat", strings.NewReader(`{"message":"What is the weather?","sessionID":"trace-session"}`))
	request.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	server.mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body)
	}
	chatSpans := spansNamed(exporter.spans, observability.SpanNameAgentChat)
	modelSpans := spansNamed(exporter.spans, observability.SpanNameModelInfer)
	toolSpans := spansNamed(exporter.spans, observability.SpanNameToolCall)
	if len(chatSpans) != 1 || len(modelSpans) != 2 || len(toolSpans) != 1 {
		t.Fatalf("exported spans: chat=%d model=%d tool=%d all=%+v", len(chatSpans), len(modelSpans), len(toolSpans), exporter.spans)
	}
	chatSpan := chatSpans[0]
	if observability.HexTraceID(chatSpan.Context.TraceID) != "11111111111111111111111111111111" || observability.HexSpanID(chatSpan.Context.ParentID) != "2222222222222222" {
		t.Fatalf("chat span lost incoming trace parent: %+v", chatSpan.Context)
	}
	if chatSpan.Kind != observability.SpanKindServer || chatSpan.Attributes[observability.AttrAgentName] != "test-agent" || chatSpan.Attributes[observability.AttrSessionID] != "trace-session" {
		t.Fatalf("chat span = %+v", chatSpan)
	}
	if chatSpan.Attributes[observability.AttrPromptTokens] != 18 || chatSpan.Attributes[observability.AttrCompletionTokens] != 5 || chatSpan.Attributes[observability.AttrTotalTokens] != 23 || chatSpan.Attributes[observability.AttrModelUsageReported] != true {
		t.Fatalf("chat span usage = %+v", chatSpan.Attributes)
	}
	for _, span := range append(modelSpans, toolSpans...) {
		if span.Context.TraceID != chatSpan.Context.TraceID || span.Context.ParentID != chatSpan.Context.SpanID {
			t.Fatalf("span %q context = %+v, want trace %x parent %x", span.Name, span.Context, chatSpan.Context.TraceID, chatSpan.Context.SpanID)
		}
	}
	if modelSpans[0].Attributes[observability.AttrModelName] != "model-id" || modelSpans[0].Attributes[observability.AttrPromptTokens] != 10 || modelSpans[0].Attributes[observability.AttrModelUsageReported] != true || modelSpans[1].Attributes[observability.AttrTotalTokens] != 11 || modelSpans[1].Attributes[observability.AttrModelUsageReported] != true {
		t.Fatalf("model span attributes = %+v %+v", modelSpans[0].Attributes, modelSpans[1].Attributes)
	}
	if modelSpans[0].Attributes[observability.AttrAgentIdentity] != "spiffe://kognition.dev/workspace/default/agent/test-agent" || modelSpans[0].Attributes[observability.AttrSolution] != "research-solution" || modelSpans[0].Attributes[observability.AttrSolutionRev] != int64(3) || modelSpans[0].Attributes[observability.AttrSessionID] != "trace-session" || modelSpans[0].Attributes[observability.AttrModelProvider] != "recording" || modelSpans[0].Attributes[observability.AttrModelCostUnits] != int64(45) {
		t.Fatalf("model attribution attrs = %+v", modelSpans[0].Attributes)
	}
	var firstModel, secondModel, toolCost *observability.CostRecord
	for index := range costs {
		record := &costs[index]
		switch {
		case record.OperationType == observability.CostOpModelInference && record.CostUnits == 45:
			firstModel = record
		case record.OperationType == observability.CostOpModelInference && record.CostUnits == 50:
			secondModel = record
		case record.OperationType == observability.CostOpToolExecution && record.ToolName == "weather" && record.CostUnits == 0:
			toolCost = record
		}
	}
	if len(costs) != 3 || firstModel == nil || secondModel == nil || toolCost == nil || firstModel.Workspace != "default" || firstModel.AgentIdentity != "spiffe://kognition.dev/workspace/default/agent/test-agent" || firstModel.Solution != "research-solution" || firstModel.SolutionRev != 3 || firstModel.SessionID != "trace-session" {
		t.Fatalf("model costs = %#v", costs)
	}
	if toolSpans[0].Attributes[observability.AttrToolName] != "weather" || toolSpans[0].Attributes[observability.AttrToolResult] == nil {
		t.Fatalf("tool span attributes = %+v", toolSpans[0].Attributes)
	}
}

func TestChatHandler_ToolDenialStopsBeforeFollowUpAndSettlesUsage(t *testing.T) {
	memory := newMemStore()
	agent := v1alpha1.Agent{Spec: v1alpha1.AgentSpec{
		ModelRef: v1alpha1.LocalObjectReference{Name: "test-model"},
		Tools:    []v1alpha1.AgentToolRef{{Ref: v1alpha1.LocalObjectReference{Name: "weather"}}},
	}}
	agent.Status.Phase = v1alpha1.PhaseReady
	putJSON(memory, "Agent", "default", "test-agent", agent)
	putModel(memory)
	putModelClass(memory)
	putJSON(memory, "Tool", "default", "weather", v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "weather"}})
	driver := &scriptedChatDriver{responses: []*cpi.CompletionResponse{{
		ToolCalls: []cpi.ToolCall{{ID: "call-1", Name: "weather", Arguments: `{}`}},
		Usage:     cpi.Usage{PromptTokens: 6, CompletionTokens: 1, TotalTokens: 7, Reported: true},
	}}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	invoker := &recordingChatToolInvoker{err: errors.New("policy denied")}
	quota := &recordingChatQuota{}
	server := New(Config{}, memory, zap.NewNop(), WithCPIRegistry(registry), WithChatToolInvoker(invoker), WithChatQuotaEnforcer(quota))

	response := performChatRequest(server, `{"message":"weather","sessionID":"denied-tool"}`)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), toolInvocationFailedMessage) {
		t.Fatalf("response = %d %s, want masked tool failure", response.Code, response.Body)
	}
	if len(driver.requests) != 1 || invoker.calls != 1 {
		t.Fatalf("model calls = %d, tool calls = %d, want 1/1", len(driver.requests), invoker.calls)
	}
	if quota.settles != 1 || quota.actualInput != 6 || quota.actualOutput != 1 {
		t.Fatalf("quota settlement = %+v, want consumed usage 6/1", quota)
	}
}

func TestResolveSecretRefCredentials(t *testing.T) {
	t.Setenv("CHAT_API_KEY", "from-env")
	srv := &Server{}
	tests := []struct {
		name   string
		secret v1alpha1.SecretRef
		want   *cpi.Credentials
	}{
		{name: "environment", secret: v1alpha1.SecretRef{Spec: v1alpha1.SecretRefSpec{BackendType: "env", BackendRef: "CHAT_API_KEY", Key: "token"}}, want: &cpi.Credentials{Type: "api-key", Data: map[string]string{"token": "from-env"}}},
		{name: "missing environment", secret: v1alpha1.SecretRef{Spec: v1alpha1.SecretRefSpec{BackendType: "env", BackendRef: "CHAT_MISSING", Key: "token"}}},
		{name: "internal without annotations", secret: v1alpha1.SecretRef{Spec: v1alpha1.SecretRefSpec{BackendType: "internal"}}},
		{name: "internal malformed data", secret: v1alpha1.SecretRef{ObjectMeta: v1alpha1.ObjectMeta{Annotations: map[string]string{"kognition.dev/internal-secret-data": `{`}}, Spec: v1alpha1.SecretRefSpec{BackendType: "internal"}}},
		{name: "unsupported backend", secret: v1alpha1.SecretRef{Spec: v1alpha1.SecretRefSpec{BackendType: "vault"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := srv.resolveSecretRefCredentials(&tt.secret)
			if mustMarshal(t, got) != mustMarshal(t, tt.want) {
				t.Fatalf("credentials = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func performChatRequest(srv *Server, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/agents/test-agent/chat", strings.NewReader(body))
	srv.mux.ServeHTTP(response, request)
	return response
}

func spansNamed(spans []*observability.Span, name string) []*observability.Span {
	var matches []*observability.Span
	for _, span := range spans {
		if span.Name == name {
			matches = append(matches, span)
		}
	}
	return matches
}

func putAgent(s *memStore, phase v1alpha1.Phase) {
	agent := v1alpha1.Agent{Spec: v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "test-model"}}}
	agent.Status.Phase = phase
	putJSON(s, "Agent", "default", "test-agent", agent)
}

func putAgentWithPromptRef(s *memStore) {
	agent := v1alpha1.Agent{Spec: v1alpha1.AgentSpec{
		ModelRef:     v1alpha1.LocalObjectReference{Name: "test-model"},
		Instructions: v1alpha1.AgentInstructions{Ref: &v1alpha1.LocalObjectReference{Name: "system-prompt"}},
	}}
	agent.Status.Phase = v1alpha1.PhaseReady
	putJSON(s, "Agent", "default", "test-agent", agent)
}

func putModel(s *memStore) {
	putJSON(s, "Model", "default", "test-model", v1alpha1.Model{Spec: v1alpha1.ModelSpec{
		ClassName:     "test-class",
		ModelID:       "model-id",
		Endpoint:      "https://model.test",
		CredentialRef: &v1alpha1.LocalObjectReference{Name: "model-secret"},
		Parameters:    map[string]string{"temperature": "0.2"},
	}})
}

func putModelClass(s *memStore) {
	putJSON(s, "ModelClass", "", "test-class", v1alpha1.ModelClass{Spec: v1alpha1.ModelClassSpec{Provisioner: "recording"}})
}

func putJSON(s *memStore, kind, namespace, name string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	putRaw(s, kind, namespace, name, data)
}

func putRaw(s *memStore, kind, namespace, name string, data []byte) {
	s.resources[storeKey(kind, namespace, name)] = memEntry{data: data, version: 1}
}

func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
