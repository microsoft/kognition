// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelgateway"
	"github.com/kognition-ai/kognition/internal/observability"
)

type recordingCPIUsageStore struct {
	*memStore
	events []observability.CostRecord
	err    error
}

func (s *recordingCPIUsageStore) ApplyUsageEvent(_ context.Context, event observability.CostRecord) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	s.events = append(s.events, event)
	return true, nil
}

type cpiModelAuthorizer struct{ err error }

func (a cpiModelAuthorizer) EvaluateModelAccess(context.Context, string, string, string) error {
	return a.err
}

// fakeCPIDriver implements cpi.Driver for CPI dispatcher testing.
type fakeCPIDriver struct {
	name        string
	response    *cpi.CompletionResponse
	err         error
	complete    func(context.Context, *cpi.CompletionRequest) (*cpi.CompletionResponse, error)
	exactTokens int
	exactErr    error
	lastReq     *cpi.CompletionRequest
	calls       int
}

func (d *fakeCPIDriver) Name() string { return d.name }
func (d *fakeCPIDriver) Validate(_ context.Context, _ *v1alpha1.ModelSpec, _ *v1alpha1.ModelClassSpec) error {
	return nil
}
func (d *fakeCPIDriver) Probe(_ context.Context, _ *v1alpha1.ModelSpec, _ *cpi.Credentials) (*cpi.HealthResult, error) {
	return nil, nil
}
func (d *fakeCPIDriver) Complete(ctx context.Context, req *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	d.lastReq = req
	d.calls++
	if d.complete != nil {
		return d.complete(ctx, req)
	}
	return d.response, d.err
}
func (d *fakeCPIDriver) Embed(_ context.Context, _ *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return nil, nil
}
func (d *fakeCPIDriver) CountInputTokens(_ *cpi.CompletionRequest) (int, error) {
	return d.exactTokens, d.exactErr
}

func seedCPIResources(t *testing.T, st *memStore) {
	t.Helper()
	ctx := context.Background()

	agent := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "test-agent", Namespace: "default", UID: "test-agent-uid"},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "test-model"},
			Instructions: v1alpha1.AgentInstructions{Inline: "You are a helpful assistant."},
		},
	}
	data, _ := json.Marshal(agent)
	if err := st.Create(ctx, "Agent", "default", "test-agent", data); err != nil {
		t.Fatal(err)
	}

	model := v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "test-model", Namespace: "default", UID: "test-model-uid"},
		Spec: v1alpha1.ModelSpec{
			ClassName:     "test-class",
			ModelID:       "gpt-4o",
			Endpoint:      "https://api.example.com/v1",
			CredentialRef: &v1alpha1.LocalObjectReference{Name: "test-secret"},
		},
	}
	data, _ = json.Marshal(model)
	if err := st.Create(ctx, "Model", "default", "test-model", data); err != nil {
		t.Fatal(err)
	}

	modelClass := v1alpha1.ModelClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ModelClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "test-class", Namespace: "default", UID: "test-class-uid"},
		Spec: v1alpha1.ModelClassSpec{Provisioner: "mock", CostTier: "premium", VersionPin: "2026-08-01", Parameters: map[string]string{
			observability.ModelInputCostMicrousdPerMillionTokensParameter:  "2500000",
			observability.ModelOutputCostMicrousdPerMillionTokensParameter: "10000000",
		}},
	}
	data, _ = json.Marshal(modelClass)
	if err := st.Create(ctx, "ModelClass", "", "test-class", data); err != nil {
		t.Fatal(err)
	}

	secretRef := v1alpha1.SecretRef{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "SecretRef"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "default",
			UID:       "test-secret-uid",
			Annotations: map[string]string{
				internalSecretDataAnnotation: `{"type":"api-key","key":"sk-test-123"}`,
			},
		},
		Spec: v1alpha1.SecretRefSpec{
			BackendType: "internal",
			BackendRef:  "inline",
			Key:         "key",
		},
	}
	data, _ = json.Marshal(secretRef)
	if err := st.Create(ctx, "SecretRef", "default", "test-secret", data); err != nil {
		t.Fatal(err)
	}
}

func TestCPIDispatcher_Dispatch(t *testing.T) {
	st := newMemStore()
	seedCPIResources(t, st)
	usageStore := &recordingCPIUsageStore{memStore: st}

	driver := &fakeCPIDriver{
		name: "mock",
		response: &cpi.CompletionResponse{
			Content:      "Hello! How can I help?",
			FinishReason: "stop",
			Usage:        cpi.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, Reported: true},
		},
	}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)

	logger := zaptest.NewLogger(t)
	dispatcher := NewCPIDispatcher(usageStore, registry, logger)
	auditStore := &memoryGrantAuditStore{}
	dispatcher.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))

	instance := &v1alpha1.AgentInstance{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentInstance"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "inst-1", Namespace: "default", UID: "instance-uid"},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef:       v1alpha1.LocalObjectReference{Name: "test-agent"},
			SessionContext: map[string]string{"input": "What is Go?"},
		},
	}

	sessionID, err := dispatcher.Dispatch(context.Background(), instance)
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}
	if sessionID == "" {
		t.Fatal("expected non-empty sessionID")
	}

	// Verify messages sent to driver.
	if driver.lastReq == nil {
		t.Fatal("driver.Complete was not called")
	}
	if len(driver.lastReq.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(driver.lastReq.Messages))
	}
	if driver.lastReq.Messages[0].Role != "system" || driver.lastReq.Messages[0].Content != "You are a helpful assistant." {
		t.Errorf("unexpected system message: %+v", driver.lastReq.Messages[0])
	}
	if driver.lastReq.Messages[1].Role != "user" || driver.lastReq.Messages[1].Content != "What is Go?" {
		t.Errorf("unexpected user message: %+v", driver.lastReq.Messages[1])
	}
	if driver.lastReq.Model != "gpt-4o" {
		t.Errorf("expected model gpt-4o, got %s", driver.lastReq.Model)
	}
	if driver.lastReq.Creds == nil {
		t.Fatal("expected credentials to be resolved")
	}
	if driver.lastReq.Creds.Type != "api-key" {
		t.Errorf("expected creds type api-key, got %s", driver.lastReq.Creds.Type)
	}
	if len(usageStore.events) != 1 {
		t.Fatalf("usage events = %#v", usageStore.events)
	}
	event := usageStore.events[0]
	if event.ID != "agentinstance:instance-uid:model" || event.ReservationID != "agentinstance:instance-uid" || event.SessionID != sessionID || event.ModelClass != "test-class" || event.ModelClassVersion != "2026-08-01" || event.ModelCostTier != "premium" || !event.PricingConfigured || !event.UsageReported || event.UsageEstimated || event.PromptTokens != 10 || event.CompletionTokens != 5 || event.TotalTokens != 15 || event.CostUnits != 75 || event.ProviderDurationMillis < 1 {
		t.Fatalf("usage event = %#v", event)
	}
	assertCPIModelOutcomes(t, auditStore.entries, "allowed", "completed")
}

func TestCPIDispatcherRejectsNonNativeRuntimeBeforeProvider(test *testing.T) {
	for _, class := range []string{"autogen", "custom", " native-other "} {
		test.Run(class, func(test *testing.T) {
			storage := newMemStore()
			seedCPIResources(test, storage)
			raw, err := storage.Get(context.Background(), "Agent", "default", "test-agent")
			if err != nil {
				test.Fatal(err)
			}
			var agent v1alpha1.Agent
			if err := json.Unmarshal(raw, &agent); err != nil {
				test.Fatal(err)
			}
			agent.Spec.Runtime = &v1alpha1.AgentRuntimeSpec{Class: class}
			updated, err := json.Marshal(agent)
			if err != nil {
				test.Fatal(err)
			}
			if err := storage.Update(context.Background(), "Agent", "default", "test-agent", updated, agent.ObjectMeta.ResourceVersion); err != nil {
				test.Fatal(err)
			}
			driver := &fakeCPIDriver{name: "mock", response: &cpi.CompletionResponse{Content: "incorrect native fallback", Usage: cpi.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2, Reported: true}}}
			registry := cpi.NewRegistry()
			registry.MustRegister(driver)
			dispatcher := NewCPIDispatcher(&recordingCPIUsageStore{memStore: storage}, registry, zaptest.NewLogger(test))
			dispatcher.SetAuditLog(observability.NewCognitiveAuditLog(&memoryGrantAuditStore{}))
			instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "instance", Namespace: "default", UID: "instance-uid"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}, SessionContext: map[string]string{"input": "question"}}}
			if _, err := dispatcher.Dispatch(context.Background(), instance); err == nil || !strings.Contains(err.Error(), "runtime class") || driver.calls != 0 {
				test.Fatalf("non-native runtime silently fell back: calls=%d error=%v", driver.calls, err)
			}
		})
	}
}

func TestCPIDispatcherPreservesNativeRuntimeClasses(test *testing.T) {
	for _, testcase := range []struct {
		name    string
		runtime *v1alpha1.AgentRuntimeSpec
	}{
		{name: "omitted"},
		{name: "empty", runtime: &v1alpha1.AgentRuntimeSpec{}},
		{name: "default", runtime: &v1alpha1.AgentRuntimeSpec{Class: "default"}},
		{name: "native", runtime: &v1alpha1.AgentRuntimeSpec{Class: "native"}},
		{name: "native whitespace", runtime: &v1alpha1.AgentRuntimeSpec{Class: " native "}},
	} {
		test.Run(testcase.name, func(test *testing.T) {
			storage := newMemStore()
			seedCPIResources(test, storage)
			raw, err := storage.Get(context.Background(), "Agent", "default", "test-agent")
			if err != nil {
				test.Fatal(err)
			}
			var agent v1alpha1.Agent
			if err := json.Unmarshal(raw, &agent); err != nil {
				test.Fatal(err)
			}
			agent.Spec.Runtime = testcase.runtime
			updated, err := json.Marshal(agent)
			if err != nil {
				test.Fatal(err)
			}
			if err := storage.Update(context.Background(), "Agent", "default", "test-agent", updated, agent.ObjectMeta.ResourceVersion); err != nil {
				test.Fatal(err)
			}
			driver := &fakeCPIDriver{name: "mock", response: &cpi.CompletionResponse{
				Content: "native completion", Usage: cpi.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2, Reported: true},
			}}
			registry := cpi.NewRegistry()
			registry.MustRegister(driver)
			usageStore := &recordingCPIUsageStore{memStore: storage}
			dispatcher := NewCPIDispatcher(usageStore, registry, zaptest.NewLogger(test))
			dispatcher.SetAuditLog(observability.NewCognitiveAuditLog(&memoryGrantAuditStore{}))
			instance := &v1alpha1.AgentInstance{
				ObjectMeta: v1alpha1.ObjectMeta{Name: "instance", Namespace: "default", UID: "instance-uid"},
				Spec: v1alpha1.AgentInstanceSpec{
					AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}, SessionContext: map[string]string{"input": "question"},
				},
			}
			sessionID, err := dispatcher.Dispatch(context.Background(), instance)
			if err != nil || sessionID == "" || driver.calls != 1 || len(usageStore.events) != 1 {
				test.Fatalf("native dispatch: session=%q calls=%d usage events=%d error=%v", sessionID, driver.calls, len(usageStore.events), err)
			}
		})
	}
}

func TestCPIDispatcher_CompletedOutputSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	seedCPIResources(t, st)
	driver := &fakeCPIDriver{name: "mock", response: &cpi.CompletionResponse{Content: "private completed output", Usage: cpi.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5, Reported: true}}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "inst-1", Namespace: "default", UID: "instance-uid"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}, SessionContext: map[string]string{"input": "hello"}}}
	dispatcher := NewCPIDispatcher(st, registry, zaptest.NewLogger(t))
	sessionID, err := dispatcher.Dispatch(ctx, instance)
	if err != nil {
		t.Fatal(err)
	}
	instance.Status.SessionID = sessionID
	restarted := NewCPIDispatcher(st, registry, zaptest.NewLogger(t))
	status, err := restarted.Status(ctx, instance)
	if err != nil || !status.Completed || status.Output != driver.response.Content || status.TokenUsage == nil || status.TokenUsage.TotalTokens != 5 {
		t.Fatalf("completed output not recovered: completed=%t usage=%v error=%v", status.Completed, status.TokenUsage, err)
	}
	if err := restarted.Terminate(ctx, instance, "finished"); err != nil {
		t.Fatal(err)
	}
	recoveredSession, err := restarted.Dispatch(ctx, instance)
	if err != nil || recoveredSession != sessionID || driver.calls != 1 {
		t.Fatalf("recovery repeated or rejected completion: sameSession=%t calls=%d error=%v", recoveredSession == sessionID, driver.calls, err)
	}
	events, err := st.ListEvents(ctx, "AgentInstance", "default", "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(event.Message, driver.response.Content) {
			t.Fatal("private output leaked to event evidence")
		}
	}
}

func TestCPIDispatcher_DoesNotReportSuccessWhenUsagePersistenceFails(t *testing.T) {
	st := newMemStore()
	seedCPIResources(t, st)
	usageStore := &recordingCPIUsageStore{memStore: st, err: errors.New("cost store unavailable")}
	driver := &fakeCPIDriver{name: "mock", response: &cpi.CompletionResponse{Content: "completed", Usage: cpi.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2, Reported: true}}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	dispatcher := NewCPIDispatcher(usageStore, registry, zaptest.NewLogger(t))
	instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "inst-1", Namespace: "default", UID: "instance-uid"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}, SessionContext: map[string]string{"input": "hello"}}}

	if sessionID, err := dispatcher.Dispatch(context.Background(), instance); err == nil || sessionID != "" {
		t.Fatalf("Dispatch() = (%q, %v), want accounting failure", sessionID, err)
	}
	if driver.lastReq == nil {
		t.Fatal("provider was not called before accounting failure")
	}
}

func TestCPIDispatcher_DoesNotReportSuccessWhenProviderOmitsUsage(t *testing.T) {
	st := newMemStore()
	seedCPIResources(t, st)
	driver := &fakeCPIDriver{name: "mock", response: &cpi.CompletionResponse{Content: "unaccounted"}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	dispatcher := NewCPIDispatcher(st, registry, zaptest.NewLogger(t))
	auditStore := &memoryGrantAuditStore{}
	dispatcher.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "inst-1", Namespace: "default"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}, SessionContext: map[string]string{"input": "hello"}}}
	if sessionID, err := dispatcher.Dispatch(context.Background(), instance); err == nil || sessionID != "" {
		t.Fatalf("Dispatch() = (%q, %v), want missing-usage failure", sessionID, err)
	}
	assertCPIModelOutcomes(t, auditStore.entries, "allowed", "failed")
}

func TestCPIDispatcher_ProviderErrorAfterStartIsAccountingUncertain(t *testing.T) {
	st := newMemStore()
	seedCPIResources(t, st)
	driver := &fakeCPIDriver{name: "mock", err: errors.New("decode successful response")}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	dispatcher := NewCPIDispatcher(st, registry, zaptest.NewLogger(t))
	auditStore := &memoryGrantAuditStore{}
	dispatcher.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "inst-1", Namespace: "default"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}, SessionContext: map[string]string{"input": "hello"}}}
	if _, err := dispatcher.Dispatch(context.Background(), instance); !errors.Is(err, ErrDispatchAccountingUncertain) {
		t.Fatalf("Dispatch() error = %v, want ErrDispatchAccountingUncertain", err)
	}
	if _, err := dispatcher.Dispatch(context.Background(), instance); !errors.Is(err, ErrDispatchAccountingUncertain) || driver.calls != 1 {
		t.Fatalf("retry error = %v, want durable uncertainty without replay", err)
	}
	assertCPIModelOutcomes(t, auditStore.entries, "allowed", "failed")
}

func TestCPIDispatcher_SharedModelGatewayCoreDeniesBeforeProviderExecution(t *testing.T) {
	st := newMemStore()
	seedCPIResources(t, st)
	driver := &fakeCPIDriver{name: "mock", response: &cpi.CompletionResponse{Content: "must not execute"}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	dispatcher := NewCPIDispatcher(st, registry, zaptest.NewLogger(t))
	auditStore := &memoryGrantAuditStore{}
	dispatcher.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	core, err := modelgateway.NewCore(st, cpiModelAuthorizer{err: errors.New("ModelGrant revoked")})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.SetModelGatewayCore(core)

	_, err = dispatcher.Dispatch(context.Background(), &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "denied", Namespace: "default"},
		Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}, SessionContext: map[string]string{"input": "hello"}},
	})
	if err == nil || driver.lastReq != nil {
		t.Fatalf("err=%v provider request=%#v, want core denial before provider execution", err, driver.lastReq)
	}
	assertCPIModelOutcomes(t, auditStore.entries, "revoked")
}

func TestCPIDispatcher_AuditFailureBlocksProviderExecution(t *testing.T) {
	st := newMemStore()
	seedCPIResources(t, st)
	driver := &fakeCPIDriver{name: "mock", response: &cpi.CompletionResponse{Content: "must not execute"}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	dispatcher := NewCPIDispatcher(st, registry, zaptest.NewLogger(t))
	dispatcher.SetAuditLog(observability.NewCognitiveAuditLog(&memoryGrantAuditStore{err: errors.New("audit unavailable")}))

	_, err := dispatcher.Dispatch(context.Background(), &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "blocked", Namespace: "default"},
		Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}, SessionContext: map[string]string{"input": "secret input"}},
	})
	if err == nil || driver.lastReq != nil {
		t.Fatalf("err=%v provider request=%#v, want evidence failure before provider execution", err, driver.lastReq)
	}
}

func assertCPIModelOutcomes(t *testing.T, entries []observability.AuditEntry, outcomes ...string) {
	t.Helper()
	if len(entries) != len(outcomes) {
		t.Fatalf("model evidence count = %d, want %d: %#v", len(entries), len(outcomes), entries)
	}
	for index, entry := range entries {
		var decision observability.TrustDecisionV1
		if err := json.Unmarshal(entry.Details, &decision); err != nil {
			t.Fatalf("decode evidence[%d]: %v", index, err)
		}
		if decision.Plane != "model" || decision.Outcome != outcomes[index] || decision.Request.TargetRef.Name != "test-model" || decision.Session == nil || decision.Session.InstanceRef == nil {
			t.Fatalf("evidence[%d] = %#v, want outcome %q", index, decision, outcomes[index])
		}
		for _, sensitive := range []string{"secret input", "gpt-4o", "api-key"} {
			if strings.Contains(string(entry.Details), sensitive) {
				t.Fatalf("evidence[%d] leaked %q: %s", index, sensitive, entry.Details)
			}
		}
	}
}

func TestCPIDispatcher_DispatchUsesPinnedAgentRevision(t *testing.T) {
	st := newMemStore()
	seedCPIResources(t, st)
	pinnedRevision := int64(2)
	pinnedAgent := v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "test-model"},
			Instructions: v1alpha1.AgentInstructions{Inline: "Use the pinned instructions."},
		},
	}
	pinnedData, err := json.Marshal(pinnedAgent)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRevision(context.Background(), "Agent", "default", "test-agent", pinnedRevision, nil, pinnedData); err != nil {
		t.Fatal(err)
	}
	driver := &fakeCPIDriver{name: "mock", response: &cpi.CompletionResponse{Content: "done", Usage: cpi.Usage{Reported: true}}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	dispatcher := NewCPIDispatcher(st, registry, zaptest.NewLogger(t))

	_, err = dispatcher.Dispatch(context.Background(), &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "pinned-instance", Namespace: "default"},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef:       v1alpha1.LocalObjectReference{Name: "test-agent"},
			AgentRevision:  &pinnedRevision,
			SessionContext: map[string]string{"input": "run it"},
		},
	})
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if driver.lastReq == nil || len(driver.lastReq.Messages) == 0 || driver.lastReq.Messages[0].Content != "Use the pinned instructions." {
		t.Fatalf("CPI request = %#v, want pinned instructions", driver.lastReq)
	}
}

func TestCPIDispatcher_WarmReplicaSkipsCompletion(t *testing.T) {
	st := newMemStore()
	seedCPIResources(t, st)
	driver := &fakeCPIDriver{name: "mock", response: &cpi.CompletionResponse{Content: "must not be used"}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	dispatcher := NewCPIDispatcher(st, registry, zaptest.NewLogger(t))

	instance := &v1alpha1.AgentInstance{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentInstance"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "warm-1", Namespace: "default"},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"},
		},
	}

	sessionID, err := dispatcher.Dispatch(context.Background(), instance)
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if sessionID == "" {
		t.Fatal("Dispatch() returned empty session ID")
	}
	if driver.lastReq != nil {
		t.Fatalf("driver.Complete() request = %+v, want no provider call", driver.lastReq)
	}
	instance.Status.SessionID = sessionID
	status, err := dispatcher.Status(context.Background(), instance)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !status.Alive || status.Completed {
		t.Fatalf("Status() = %+v, want alive idle session", status)
	}
}

func TestCPIDispatcher_Status(t *testing.T) {
	st := newMemStore()
	seedCPIResources(t, st)

	driver := &fakeCPIDriver{
		name: "mock",
		response: &cpi.CompletionResponse{
			Content:      "The answer is 42.",
			FinishReason: "stop",
			Usage:        cpi.Usage{PromptTokens: 8, CompletionTokens: 4, TotalTokens: 12, Reported: true},
		},
	}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)

	logger := zaptest.NewLogger(t)
	dispatcher := NewCPIDispatcher(st, registry, logger)

	instance := &v1alpha1.AgentInstance{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentInstance"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "inst-2", Namespace: "default"},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef:       v1alpha1.LocalObjectReference{Name: "test-agent"},
			SessionContext: map[string]string{"input": "Hello"},
		},
	}

	sessionID, err := dispatcher.Dispatch(context.Background(), instance)
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}

	// Set sessionID on instance status to simulate reconciler behavior.
	instance.Status.SessionID = sessionID

	status, err := dispatcher.Status(context.Background(), instance)
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if !status.Completed {
		t.Fatal("expected status.Completed to be true")
	}
	if status.Output != "The answer is 42." {
		t.Errorf("expected output 'The answer is 42.', got %q", status.Output)
	}
	if status.TokenUsage == nil {
		t.Fatal("expected TokenUsage to be set")
	}
	if status.TokenUsage.TotalTokens != 12 {
		t.Errorf("expected 12 total tokens, got %d", status.TokenUsage.TotalTokens)
	}

	// Repeated polling must replay the terminal result until status is durable.
	status2, err := dispatcher.Status(context.Background(), instance)
	if err != nil {
		t.Fatalf("second Status call failed: %v", err)
	}
	if !status2.Completed || status2.Output != status.Output {
		t.Fatalf("second Status = %+v, want replayable terminal result", status2)
	}
}

func TestCPIDispatcher_EnvCredentials(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()

	// Create agent and model with env-backed secret.
	agent := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "env-agent", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "env-model"},
			Instructions: v1alpha1.AgentInstructions{Inline: "system prompt"},
		},
	}
	data, _ := json.Marshal(agent)
	st.Create(ctx, "Agent", "default", "env-agent", data)

	model := v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "env-model", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			ClassName:     "env-class",
			ModelID:       "claude-3",
			Endpoint:      "https://api.anthropic.com",
			CredentialRef: &v1alpha1.LocalObjectReference{Name: "env-secret"},
		},
	}
	data, _ = json.Marshal(model)
	st.Create(ctx, "Model", "default", "env-model", data)

	modelClass := v1alpha1.ModelClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ModelClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "env-class", Namespace: "default"},
		Spec:       v1alpha1.ModelClassSpec{Provisioner: "mock-env"},
	}
	data, _ = json.Marshal(modelClass)
	st.Create(ctx, "ModelClass", "", "env-class", data)

	secretRef := v1alpha1.SecretRef{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "SecretRef"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "env-secret", Namespace: "default"},
		Spec: v1alpha1.SecretRefSpec{
			BackendType: "env",
			BackendRef:  "TEST_CPI_API_KEY",
			Key:         "api-key",
		},
	}
	data, _ = json.Marshal(secretRef)
	st.Create(ctx, "SecretRef", "default", "env-secret", data)

	// Set the env var.
	os.Setenv("TEST_CPI_API_KEY", "sk-env-secret-value")
	t.Cleanup(func() { os.Unsetenv("TEST_CPI_API_KEY") })

	driver := &fakeCPIDriver{
		name: "mock-env",
		response: &cpi.CompletionResponse{
			Content:      "env response",
			FinishReason: "stop",
			Usage:        cpi.Usage{Reported: true},
		},
	}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)

	logger := zaptest.NewLogger(t)
	dispatcher := NewCPIDispatcher(st, registry, logger)

	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "inst-env", Namespace: "default"},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef:       v1alpha1.LocalObjectReference{Name: "env-agent"},
			SessionContext: map[string]string{"input": "hi"},
		},
	}

	_, err := dispatcher.Dispatch(ctx, instance)
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}

	if driver.lastReq.Creds == nil {
		t.Fatal("expected credentials")
	}
	if driver.lastReq.Creds.Type != "api-key" {
		t.Errorf("expected type api-key, got %s", driver.lastReq.Creds.Type)
	}
	if driver.lastReq.Creds.Data["api-key"] != "sk-env-secret-value" {
		t.Errorf("expected env value sk-env-secret-value, got %s", driver.lastReq.Creds.Data["api-key"])
	}
}

func TestCPIDispatcher_NoCredentials(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()

	agent := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "nocred-agent", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "nocred-model"},
			Instructions: v1alpha1.AgentInstructions{Inline: "prompt"},
		},
	}
	data, _ := json.Marshal(agent)
	st.Create(ctx, "Agent", "default", "nocred-agent", data)

	model := v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "nocred-model", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			ClassName: "nocred-class",
			ModelID:   "local-model",
			Endpoint:  "http://localhost:11434",
		},
	}
	data, _ = json.Marshal(model)
	st.Create(ctx, "Model", "default", "nocred-model", data)

	modelClass := v1alpha1.ModelClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ModelClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "nocred-class", Namespace: "default"},
		Spec:       v1alpha1.ModelClassSpec{Provisioner: "mock-nocred"},
	}
	data, _ = json.Marshal(modelClass)
	st.Create(ctx, "ModelClass", "", "nocred-class", data)

	driver := &fakeCPIDriver{
		name: "mock-nocred",
		response: &cpi.CompletionResponse{
			Content:      "local response",
			FinishReason: "stop",
			Usage:        cpi.Usage{Reported: true},
		},
	}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)

	logger := zaptest.NewLogger(t)
	dispatcher := NewCPIDispatcher(st, registry, logger)

	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "inst-nocred", Namespace: "default"},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef:       v1alpha1.LocalObjectReference{Name: "nocred-agent"},
			SessionContext: map[string]string{"input": "test"},
		},
	}

	_, err := dispatcher.Dispatch(ctx, instance)
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}
	if driver.lastReq.Creds != nil {
		t.Error("expected nil credentials for model without credentialRef")
	}
}
