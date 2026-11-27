// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/toolexec"
	"github.com/kognition-ai/kognition/internal/store"
)

type workflowRecordingExecutor struct {
	request runtime.ToolExecutionRequest
	result  runtime.ToolExecutionResult
	err     error
	calls   int
}

func (e *workflowRecordingExecutor) Execute(_ context.Context, request runtime.ToolExecutionRequest, _ runtime.ToolSandboxLimits) (runtime.ToolExecutionResult, error) {
	e.calls++
	e.request = request
	return e.result, e.err
}

type workflowToolGetErrorStore struct {
	store.Store
	err error
}

type workflowToolQuotaStub struct {
	err   error
	calls int
}

type workflowSpanExporter struct {
	spans []*observability.Span
}

func (e *workflowSpanExporter) ExportSpans(_ context.Context, spans []*observability.Span) error {
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *workflowSpanExporter) Shutdown(context.Context) error { return nil }

func (q *workflowToolQuotaStub) ReserveToolCall(context.Context, string) error {
	q.calls++
	return q.err
}

func (s *workflowToolGetErrorStore) Get(context.Context, string, string, string) ([]byte, error) {
	return nil, s.err
}

func TestDefaultWorkflowToolInvokerDispatchesStoredTool(t *testing.T) {
	store := newMemStore()
	storeTool(t, store, v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "http-get"},
	})
	executor := &workflowRecordingExecutor{result: runtime.ToolExecutionResult{
		Output: map[string]any{"status": "ok"},
	}}
	invoker := NewDefaultWorkflowToolInvoker(
		store,
		nil,
		runtime.NewToolDispatcher(executor, nil),
		zaptest.NewLogger(t),
	)
	input := map[string]any{"url": "https://example.com"}

	output, err := invoker.InvokeTool(context.Background(), "tenant-a", "workflow:nightly", "http-get", input)
	if err != nil {
		t.Fatalf("invoke tool: %v", err)
	}
	if got := output["status"]; got != "ok" {
		t.Fatalf("output status = %v, want ok", got)
	}
	if got := executor.request.Tool.ObjectMeta.Namespace; got != "tenant-a" {
		t.Fatalf("dispatched tool namespace = %q, want tenant-a", got)
	}
	if got := executor.request.Tool.ObjectMeta.Name; got != "http-get" {
		t.Fatalf("dispatched tool name = %q, want http-get", got)
	}
	if got := executor.request.Input["url"]; got != "https://example.com" {
		t.Fatalf("dispatched input url = %v, want https://example.com", got)
	}
}

func TestDefaultWorkflowToolInvokerEmitsObservabilitySpan(t *testing.T) {
	store := newMemStore()
	storeWorkflow(t, store, v1alpha1.Workflow{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "nightly", Labels: map[string]string{"kognition.dev/solution": "research-solution"}}})
	storeSolution(t, store, v1alpha1.Solution{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "research-solution"}, Status: v1alpha1.SolutionStatus{ActiveRevision: 6}})
	storeTool(t, store, v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "http-get"},
	})
	executor := &workflowRecordingExecutor{result: runtime.ToolExecutionResult{
		Output: map[string]any{"status": "ok"},
	}}
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0})
	collector := observability.NewCollector()
	exporter := &workflowSpanExporter{}
	collector.RegisterSpanExporter(exporter)
	invoker := NewDefaultWorkflowToolInvoker(
		store,
		nil,
		runtime.NewToolDispatcher(executor, nil),
		zaptest.NewLogger(t),
		WithWorkflowToolTracing(tracer, collector),
	)

	output, err := invoker.InvokeTool(context.Background(), "tenant-a", "workflow:nightly", "http-get", map[string]any{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("invoke tool: %v", err)
	}
	if output["status"] != "ok" {
		t.Fatalf("output = %v", output)
	}
	if len(exporter.spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(exporter.spans))
	}
	span := exporter.spans[0]
	if span.Name != observability.SpanNameToolCall || span.StatusCode != observability.StatusOK {
		t.Fatalf("span = (%q, %v), want tool call OK", span.Name, span.StatusCode)
	}
	if got := span.Attributes[observability.AttrToolName]; got != "http-get" {
		t.Fatalf("tool attr = %v, want http-get", got)
	}
	if got := span.Attributes[observability.AttrAgentIdentity]; got != "workflow:nightly" {
		t.Fatalf("principal attr = %v, want workflow:nightly", got)
	}
	if got := span.Attributes[observability.AttrWorkspace]; got != "tenant-a" {
		t.Fatalf("workspace attr = %v, want tenant-a", got)
	}
	if span.Attributes[observability.AttrSolution] != "research-solution" || span.Attributes[observability.AttrSolutionRev] != int64(6) {
		t.Fatalf("solution attrs = %#v", span.Attributes)
	}
}

func TestDefaultWorkflowToolInvokerReportsToolLookupFailures(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		invoker := NewDefaultWorkflowToolInvoker(
			newMemStore(),
			nil,
			runtime.NewToolDispatcher(&workflowRecordingExecutor{}, nil),
			zaptest.NewLogger(t),
		)

		_, err := invoker.InvokeTool(context.Background(), "tenant-a", "workflow:nightly", "missing", nil)
		if err == nil || !strings.Contains(err.Error(), `tool "missing" not found in namespace "tenant-a"`) {
			t.Fatalf("error = %v, want contextual not-found error", err)
		}
	})

	t.Run("store error", func(t *testing.T) {
		getErr := errors.New("store unavailable")
		invoker := NewDefaultWorkflowToolInvoker(
			&workflowToolGetErrorStore{Store: newMemStore(), err: getErr},
			nil,
			runtime.NewToolDispatcher(&workflowRecordingExecutor{}, nil),
			zaptest.NewLogger(t),
		)

		_, err := invoker.InvokeTool(context.Background(), "tenant-a", "workflow:nightly", "http-get", nil)
		if !errors.Is(err, getErr) {
			t.Fatalf("error = %v, want wrapped store error", err)
		}
	})

	t.Run("malformed tool", func(t *testing.T) {
		memoryStore := newMemStore()
		if err := memoryStore.Create(context.Background(), "Tool", "tenant-a", "broken", []byte(`{"spec":`)); err != nil {
			t.Fatalf("store malformed Tool: %v", err)
		}
		invoker := NewDefaultWorkflowToolInvoker(
			memoryStore,
			nil,
			runtime.NewToolDispatcher(&workflowRecordingExecutor{}, nil),
			zaptest.NewLogger(t),
		)

		_, err := invoker.InvokeTool(context.Background(), "tenant-a", "workflow:nightly", "broken", nil)
		if err == nil || !strings.Contains(err.Error(), "unmarshal tool") {
			t.Fatalf("error = %v, want unmarshal context", err)
		}
	})
}

func TestDefaultWorkflowToolInvokerEnforcesGrantBeforeDispatch(t *testing.T) {
	memoryStore := newMemStore()
	storeTool(t, memoryStore, v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "http-get"},
	})
	executor := &workflowRecordingExecutor{}
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0})
	collector := observability.NewCollector()
	exporter := &workflowSpanExporter{}
	collector.RegisterSpanExporter(exporter)
	invoker := NewDefaultWorkflowToolInvoker(
		memoryStore,
		NewToolGrantEngine(memoryStore, zaptest.NewLogger(t)),
		runtime.NewToolDispatcher(executor, nil),
		zaptest.NewLogger(t),
		WithWorkflowToolTracing(tracer, collector),
	)

	_, err := invoker.InvokeTool(context.Background(), "tenant-a", "workflow:nightly", "http-get", nil)
	if err == nil || !strings.Contains(err.Error(), "no ToolGrant permits") {
		t.Fatalf("error = %v, want ToolGrant denial", err)
	}
	if executor.calls != 0 {
		t.Fatalf("executor calls = %d, want 0 after grant denial", executor.calls)
	}
	if len(exporter.spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(exporter.spans))
	}
	span := exporter.spans[0]
	if span.StatusCode != observability.StatusError {
		t.Fatalf("span status = %v, want error", span.StatusCode)
	}
	if got := span.Attributes[observability.AttrErrorMessage]; got == nil || !strings.Contains(got.(string), "no ToolGrant permits") {
		t.Fatalf("error attr = %v, want ToolGrant denial", got)
	}
}

func TestDefaultWorkflowToolInvokerEnforcesToolCallQuotaBeforeDispatch(t *testing.T) {
	memoryStore := newMemStore()
	storeTool(t, memoryStore, v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "http-get"}})
	executor := &workflowRecordingExecutor{}
	quota := &workflowToolQuotaStub{err: errors.New("tool-call quota exhausted")}
	invoker := NewDefaultWorkflowToolInvoker(
		memoryStore,
		nil,
		runtime.NewToolDispatcher(executor, nil),
		zaptest.NewLogger(t),
		WithWorkflowToolCallQuota(quota),
	)

	_, err := invoker.InvokeTool(context.Background(), "tenant-a", "workflow:nightly", "http-get", nil)
	if err == nil || !strings.Contains(err.Error(), "reserve tool-call quota") {
		t.Fatalf("error = %v, want tool-call quota denial", err)
	}
	if quota.calls != 1 || executor.calls != 0 {
		t.Fatalf("quota calls = %d, executor calls = %d, want 1/0", quota.calls, executor.calls)
	}
}

func TestDefaultWorkflowToolInvokerEnforcesAgentPolicyBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	memoryStore := newMemStore()
	storeTool(t, memoryStore, v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "http-get"}})
	storeToolGrant(t, memoryStore, makeToolGrant("tenant-a", "workflow-tools", v1alpha1.ToolGrantSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "workflow:nightly"},
		ToolRef:  v1alpha1.LocalObjectReference{Name: "http-get"},
		Verbs:    []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
	}))
	policy := v1alpha1.AgentPolicy{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "deny-http", Namespace: "tenant-a"},
		Spec: v1alpha1.AgentPolicySpec{
			Enforce: true,
			Scope:   "workspace",
			Rules:   `deny[msg] { input.tool == "http-get"; msg := "network tool blocked" }`,
		},
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Create(ctx, agentPolicyKind, "tenant-a", policy.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	policyEvaluator := NewAgentPolicyReconciler(memoryStore, opa.NewEngine(), zaptest.NewLogger(t))
	if _, err := policyEvaluator.Reconcile(ctx, Request{Namespace: "tenant-a", Name: policy.ObjectMeta.Name, Kind: agentPolicyKind}); err != nil {
		t.Fatalf("compile AgentPolicy: %v", err)
	}
	executor := &workflowRecordingExecutor{}
	invoker := NewDefaultWorkflowToolInvoker(
		memoryStore,
		NewInvocationPolicyEngine(NewToolGrantEngine(memoryStore, zaptest.NewLogger(t)), policyEvaluator),
		runtime.NewToolDispatcher(executor, nil),
		zaptest.NewLogger(t),
	)

	_, err = invoker.InvokeTool(ctx, "tenant-a", "workflow:nightly", "http-get", nil)
	if err == nil || !strings.Contains(err.Error(), "AgentPolicy denied") {
		t.Fatalf("error = %v, want AgentPolicy denial", err)
	}
	if executor.calls != 0 {
		t.Fatalf("executor calls = %d, want 0 after AgentPolicy denial", executor.calls)
	}
}

func TestDefaultWorkflowToolInvokerEnforcesIdentityAndRBACForAgentPrincipal(t *testing.T) {
	ctx := context.Background()
	memoryStore := newMemStore()
	storeTool(t, memoryStore, v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "http-get"}})
	storeToolGrant(t, memoryStore, makeToolGrant("tenant-a", "agent-tools", v1alpha1.ToolGrantSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"},
		ToolRef:  v1alpha1.LocalObjectReference{Name: "http-get"},
		Verbs:    []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
	}))
	executor := &workflowRecordingExecutor{}
	policies := NewInvocationPolicyEngine(NewToolGrantEngine(memoryStore, zaptest.NewLogger(t)), nil).
		WithAgentRBAC(memoryStore, NewRBACEngine(memoryStore, zaptest.NewLogger(t)))
	invoker := NewDefaultWorkflowToolInvoker(memoryStore, policies, runtime.NewToolDispatcher(executor, nil), zaptest.NewLogger(t))
	agent := v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "researcher", UID: "researcher-current-uid"}}
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Create(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}

	_, err = invoker.InvokeTool(ctx, "tenant-a", "researcher", "http-get", nil)
	if !errors.Is(err, ErrAgentIdentityNotFound) {
		t.Fatalf("error = %v, want identity denial", err)
	}
	manualIdentity := makeAgentIdentity("tenant-a", "researcher-manual", "researcher")
	manualIdentity.Spec.SPIFFEID = "spiffe://kognition.dev/workspace/tenant-a/agent/researcher"
	manualIdentity.Status.Phase = v1alpha1.PhaseReady
	storeAgentIdentity(t, memoryStore, manualIdentity)
	_, err = invoker.InvokeTool(ctx, "tenant-a", "researcher", "http-get", nil)
	if !errors.Is(err, ErrAgentIdentityNotFound) {
		t.Fatalf("unbound manual identity error = %v, want identity denial", err)
	}
	identity := makeAgentIdentity("tenant-a", defaultAgentIdentityName("researcher"), "researcher")
	identity.Spec.SPIFFEID = "spiffe://kognition.dev/workspace/tenant-a/agent/researcher"
	identity.Status.Phase = v1alpha1.PhaseReady
	identity.ObjectMeta.Labels = map[string]string{autoAgentIdentityLabel: "researcher"}
	identity.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{
		APIVersion: v1alpha1.APIVersion,
		Kind:       "Agent",
		Name:       "researcher",
		UID:        agent.ObjectMeta.UID,
	}}
	storeAgentIdentity(t, memoryStore, identity)
	_, err = invoker.InvokeTool(ctx, "tenant-a", "researcher", "http-get", nil)
	if err == nil || !strings.Contains(err.Error(), "AgentRole denies") {
		t.Fatalf("error = %v, want AgentRole denial", err)
	}
	storeAgentRole(t, memoryStore, makeAgentRole("tenant-a", "tool-invoker", []v1alpha1.PolicyRule{{Resources: []string{"Tool"}, Verbs: []string{"invoke"}}}))
	storeAgentRoleBinding(t, memoryStore, makeAgentRoleBinding("tenant-a", "researcher-tools", "tool-invoker", []v1alpha1.Subject{{Kind: "Agent", Name: "researcher"}}))
	if _, err := invoker.InvokeTool(ctx, "tenant-a", "researcher", "http-get", nil); err != nil {
		t.Fatalf("invoke with identity and AgentRole: %v", err)
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want 1 after all gates allow", executor.calls)
	}
}

func TestDefaultWorkflowToolInvokerPreservesExecutionError(t *testing.T) {
	for _, executionErr := range []error{
		errors.New(logSecretSentinel),
		fmt.Errorf("remote tool failed: %w", errors.New(logSecretSentinel)),
		errors.Join(errors.New(logSecretSentinel), errors.New("another backend failure")),
		&runtime.StructuredToolError{Code: "ToolHTTPError", Message: logSecretSentinel},
	} {
		t.Run(fmt.Sprintf("%T", executionErr), func(t *testing.T) {
			memoryStore := newMemStore()
			storeTool(t, memoryStore, v1alpha1.Tool{
				ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "http-get"},
			})
			core, logs := observer.New(zap.WarnLevel)
			executor := &workflowRecordingExecutor{err: executionErr}
			invoker := NewDefaultWorkflowToolInvoker(memoryStore, nil, runtime.NewToolDispatcher(executor, nil), zap.New(core))
			output, err := invoker.InvokeTool(context.Background(), "tenant-a", "workflow:nightly", "http-get", nil)
			if !errors.Is(err, executionErr) || output != nil || executor.calls != 1 {
				t.Fatalf("original execution error or dispatch behavior changed: error=%v output=%v calls=%d", err, output, executor.calls)
			}
			assertConfidentialFailureLog(t, logs, "tool invocation failed", map[string]any{
				"namespace": "tenant-a", "principal": "workflow:nightly", "tool": "http-get",
			})
		})
	}
}

func TestWorkflowToolHTTPFailureLogsOmitRemoteResponse(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Errorf("decode input: %v", err)
		}
		if input["credential"] != logSecretSentinel {
			t.Error("input did not reach the configured executor")
		}
		w.WriteHeader(http.StatusBadRequest)
		if _, err := io.WriteString(w, "backend rejected credential: "+logSecretSentinel); err != nil {
			t.Errorf("write backend response: %v", err)
		}
	}))
	defer server.Close()
	memory := newMemStore()
	storeTool(t, memory, v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "http-tool"},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolOpenAPI, Endpoint: server.URL,
			Metadata: map[string]string{"allowPrivateNetwork": "true"},
		},
	})
	core, logs := observer.New(zap.WarnLevel)
	dispatcher := runtime.NewToolDispatcher(toolexec.New(server.Client()), nil)
	invoker := NewDefaultWorkflowToolInvoker(memory, nil, dispatcher, zap.New(core))
	output, err := invoker.InvokeTool(context.Background(), "tenant-a", "workflow:nightly", "http-tool", map[string]any{"credential": logSecretSentinel})
	var structured *runtime.StructuredToolError
	if output != nil || calls != 1 || !errors.As(err, &structured) || structured.Code != "ToolHTTPError" ||
		!strings.Contains(structured.Message, logSecretSentinel) {
		t.Fatalf("original remote error was not preserved: output=%v calls=%d error=%v", output, calls, err)
	}
	assertConfidentialFailureLog(t, logs, "tool invocation failed", map[string]any{
		"namespace": "tenant-a", "principal": "workflow:nightly", "tool": "http-tool",
	})
}
