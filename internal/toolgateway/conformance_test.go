// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/toolexec"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

// TC-0297: The same authorization, receipt, resilience, quota, audit, and
// normalized response contract must hold for standalone and Kubernetes HTTP
// Tool Gateway transports.
func TestToolGatewayCrossModeConformance(t *testing.T) {
	runAcrossGatewayTransports(t, "successful invocation returns normalized output", func(t *testing.T, fixture *gatewayConformanceFixture) gatewayContractResult {
		return fixture.invoke(Request{InvocationID: "success", Input: map[string]any{"ticket": "42"}})
	}, func(t *testing.T, result gatewayContractResult, fixture *gatewayConformanceFixture) {
		if result.Outcome != gatewayOutcomeSuccess || result.InvocationID != "success" || result.Output["updated"] != true || fixture.executor.callCount() != 1 {
			t.Fatalf("success result=%#v calls=%d", result, fixture.executor.callCount())
		}
	})

	runAcrossGatewayTransports(t, "authorization and quota deny before execution", func(t *testing.T, fixture *gatewayConformanceFixture) gatewayContractResult {
		fixture.authorizer.err = errors.New("grant denied")
		denied := fixture.invoke(Request{InvocationID: "authorization-denied"})
		if fixture.executor.callCount() != 0 {
			t.Fatalf("authorization denial executed Tool %d times", fixture.executor.callCount())
		}
		fixture.authorizer.err = nil
		fixture.quota.err = errors.New("quota exhausted")
		quotaDenied := fixture.invoke(Request{InvocationID: "quota-denied"})
		if fixture.executor.callCount() != 0 {
			t.Fatalf("quota denial executed Tool %d times", fixture.executor.callCount())
		}
		return gatewayContractResult{Outcome: denied.Outcome + "+" + quotaDenied.Outcome}
	}, func(t *testing.T, result gatewayContractResult, _ *gatewayConformanceFixture) {
		if result.Outcome != gatewayOutcomeRejected+"+"+gatewayOutcomeRejected {
			t.Fatalf("denial result=%#v", result)
		}
	})

	runAcrossGatewayTransports(t, "argument pattern denies before execution", func(t *testing.T, fixture *gatewayConformanceFixture) gatewayContractResult {
		fixture.authorizer.authorizeInvocation = func(input map[string]any) (AuthorizationDecision, error) {
			query, _ := input["query"].(string)
			if !strings.HasPrefix(query, "safe-") {
				return AuthorizationDecision{GrantRef: "team-a/constrained-search@3"}, ErrToolArgumentConstraintsDenied
			}
			return AuthorizationDecision{GrantRef: "team-a/constrained-search@3"}, nil
		}
		result := fixture.invoke(Request{InvocationID: "argument-denied", Input: map[string]any{"query": "blocked-secret"}})
		if fixture.executor.callCount() != 0 || len(fixture.ledger.decisions) != 1 || fixture.ledger.decisions[0].ReasonCode != "argument_constraints_denied" || fixture.ledger.decisions[0].GrantRef != "team-a/constrained-search@3" {
			t.Fatalf("argument denial result=%#v calls=%d decisions=%#v", result, fixture.executor.callCount(), fixture.ledger.decisions)
		}
		return result
	}, func(t *testing.T, result gatewayContractResult, _ *gatewayConformanceFixture) {
		if result.Outcome != gatewayOutcomeRejected {
			t.Fatalf("argument denial result=%#v", result)
		}
	})

	runAcrossGatewayTransports(t, "receipt replay and conflicting binding", func(t *testing.T, fixture *gatewayConformanceFixture) gatewayContractResult {
		first := fixture.invoke(Request{InvocationID: "replay", Input: map[string]any{"ticket": "42"}})
		duplicate := fixture.invoke(Request{InvocationID: "replay", Input: map[string]any{"ticket": "42"}})
		conflict := fixture.invoke(Request{InvocationID: "replay", Input: map[string]any{"ticket": "43"}})
		if fixture.executor.callCount() != 1 || first.Outcome != gatewayOutcomeSuccess || duplicate.Outcome != gatewayOutcomeSuccess {
			t.Fatalf("receipt replay first=%#v duplicate=%#v calls=%d", first, duplicate, fixture.executor.callCount())
		}
		return conflict
	}, func(t *testing.T, result gatewayContractResult, _ *gatewayConformanceFixture) {
		if result.Outcome != gatewayOutcomeConflict {
			t.Fatalf("conflicting receipt result=%#v", result)
		}
	})

	runAcrossGatewayTransports(t, "idempotent retries but nonidempotent calls once", func(t *testing.T, fixture *gatewayConformanceFixture) gatewayContractResult {
		fixture.setToolPolicy(&v1alpha1.ToolExecutionPolicy{Idempotent: true, RetryMaxAttempts: 2})
		fixture.executor.errs = []error{&runtime.StructuredToolError{Code: "ToolTransportError", Message: "temporary", Retryable: true}, nil}
		retried := fixture.invoke(Request{InvocationID: "retry"})
		if retried.Outcome != gatewayOutcomeSuccess || fixture.executor.callCount() != 2 {
			t.Fatalf("retry result=%#v calls=%d", retried, fixture.executor.callCount())
		}
		fixture.setToolPolicy(nil)
		fixture.executor.reset(&runtime.StructuredToolError{Code: "ToolTransportError", Message: "temporary", Retryable: true})
		nonIdempotent := fixture.invoke(Request{InvocationID: "no-retry"})
		if fixture.executor.callCount() != 1 {
			t.Fatalf("non-idempotent Tool calls=%d, want 1", fixture.executor.callCount())
		}
		return nonIdempotent
	}, func(t *testing.T, result gatewayContractResult, _ *gatewayConformanceFixture) {
		if result.Outcome != gatewayOutcomeRejected {
			t.Fatalf("non-idempotent result=%#v", result)
		}
	})

	runAcrossGatewayTransports(t, "timeout and circuit rejection", func(t *testing.T, fixture *gatewayConformanceFixture) gatewayContractResult {
		fixture.setToolPolicy(&v1alpha1.ToolExecutionPolicy{Timeout: "1ms"})
		fixture.executor.waitForContext = true
		timeout := fixture.invoke(Request{InvocationID: "timeout"})
		if fixture.executor.callCount() != 1 {
			t.Fatalf("timeout Tool calls=%d, want 1", fixture.executor.callCount())
		}
		fixture.executor.waitForContext = false
		fixture.executor.reset(&runtime.StructuredToolError{Code: "ToolTransportError", Message: "temporary", Retryable: true})
		fixture.setToolPolicy(&v1alpha1.ToolExecutionPolicy{CircuitBreaker: &v1alpha1.ToolCircuitBreaker{FailureThreshold: 1, Cooldown: "1m"}})
		first := fixture.invoke(Request{InvocationID: "circuit-failure"})
		second := fixture.invoke(Request{InvocationID: "circuit-open"})
		if first.Outcome != gatewayOutcomeRejected || fixture.executor.callCount() != 1 {
			t.Fatalf("circuit first=%#v calls=%d", first, fixture.executor.callCount())
		}
		return gatewayContractResult{Outcome: timeout.Outcome + "+" + second.Outcome}
	}, func(t *testing.T, result gatewayContractResult, _ *gatewayConformanceFixture) {
		if result.Outcome != gatewayOutcomeRejected+"+"+gatewayOutcomeRejected {
			t.Fatalf("timeout/circuit result=%#v", result)
		}
	})

	runAcrossGatewayTransports(t, "completion audit failure preserves executed output", func(t *testing.T, fixture *gatewayConformanceFixture) gatewayContractResult {
		fixture.ledger.completionErr = errors.New("audit unavailable")
		return fixture.invoke(Request{InvocationID: "audit"})
	}, func(t *testing.T, result gatewayContractResult, fixture *gatewayConformanceFixture) {
		if result.Outcome != gatewayOutcomeAuditFailure || result.ErrorCode != "ToolAuditCompletionFailed" || result.Output["updated"] != true || fixture.executor.callCount() != 1 {
			t.Fatalf("audit result=%#v calls=%d", result, fixture.executor.callCount())
		}
	})
}

func TestToolGatewayRemoteBrowserCrossMode(t *testing.T) {
	var received map[string]any
	browser := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Fatalf("browser method = %s, want POST", request.Method)
		}
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Fatalf("decode browser request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"page":"example"}`))
	}))
	defer browser.Close()

	runAcrossGatewayTransports(t, "remote browser invocation", func(t *testing.T, fixture *gatewayConformanceFixture) gatewayContractResult {
		protocolExecutor := toolexec.New(browser.Client())
		fixture.replaceExecutor(t, runtime.NewToolDispatcher(protocolExecutor, protocolExecutor))
		storeGatewayObject(t, fixture.store, "Tool", "team-a", "crm-update", v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: "team-a", ResourceVersion: 7},
			Spec: v1alpha1.ToolSpec{
				Protocol: v1alpha1.ToolProtocolBrowser,
				Endpoint: browser.URL,
				Metadata: map[string]string{"allowPrivateNetwork": "true"},
			},
		})
		return fixture.invoke(Request{InvocationID: "browser", Input: map[string]any{"url": "https://8.8.8.8", "action": "navigate"}})
	}, func(t *testing.T, result gatewayContractResult, _ *gatewayConformanceFixture) {
		if result.Outcome != gatewayOutcomeSuccess || result.Output["page"] != "example" || received["action"] != "navigate" {
			t.Fatalf("browser result=%#v request=%#v", result, received)
		}
	})
}

func TestToolGatewayShellInvocationRunCrossMode(t *testing.T) {
	runAcrossGatewayTransports(t, "Shell invocation through durable run", func(t *testing.T, fixture *gatewayConformanceFixture) gatewayContractResult {
		fixture.store.onCreate = func(kind, namespace, name string, data []byte) error {
			var run v1alpha1.ToolInvocationRun
			if err := json.Unmarshal(data, &run); err != nil {
				return err
			}
			run.Status = v1alpha1.ToolInvocationRunStatus{Phase: v1alpha1.PhaseReady, Output: map[string]any{"status": "ok"}, ExecutionMode: "sandbox"}
			updated, _ := json.Marshal(run)
			fixture.store.data[kind+"/"+namespace+"/"+name] = updated
			return nil
		}
		fixture.replaceExecutor(t, NewInvocationRunExecutor(fixture.store, fixture.executor))
		storeGatewayObject(t, fixture.store, "Tool", "team-a", "crm-update", v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: "team-a", ResourceVersion: 7}, Spec: v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolShell}})
		return fixture.invoke(Request{InvocationID: "shell", Input: map[string]any{"command": "status"}})
	}, func(t *testing.T, result gatewayContractResult, _ *gatewayConformanceFixture) {
		if result.Outcome != gatewayOutcomeSuccess || result.Output["status"] != "ok" {
			t.Fatalf("shell result = %#v", result)
		}
	})
}

const (
	gatewayOutcomeSuccess      = "success"
	gatewayOutcomeRejected     = "rejected"
	gatewayOutcomeConflict     = "conflict"
	gatewayOutcomeInProgress   = "in-progress"
	gatewayOutcomeAuditFailure = "audit-failure"
)

type gatewayContractResult struct {
	Outcome      string
	InvocationID string
	Output       map[string]any
	ErrorCode    string
}

type gatewayConformanceFixture struct {
	store         *gatewayStore
	authorizer    *gatewayAuthorizer
	quota         *gatewayQuota
	executor      *conformanceGatewayExecutor
	ledger        *gatewayLedger
	core          *Gateway
	server        *Server
	issuer        *runtimeauth.ToolCapabilityIssuer
	sessionIssuer *runtimeauth.ToolSessionCapabilityIssuer
	binding       runtimeauth.ToolCapabilityBinding
	http          bool
}

type conformanceGatewayExecutor struct {
	mu             sync.Mutex
	calls          int
	errs           []error
	waitForContext bool
}

func (e *conformanceGatewayExecutor) Execute(ctx context.Context, _ runtime.ToolExecutionRequest) (runtime.ToolExecutionResult, error) {
	e.mu.Lock()
	e.calls++
	waitForContext := e.waitForContext
	var err error
	if len(e.errs) > 0 {
		err = e.errs[0]
		e.errs = e.errs[1:]
	}
	e.mu.Unlock()
	if waitForContext {
		<-ctx.Done()
		return runtime.ToolExecutionResult{}, ctx.Err()
	}
	if err != nil {
		return runtime.ToolExecutionResult{}, err
	}
	return runtime.ToolExecutionResult{Output: map[string]any{"updated": true}, ExecutionMode: "sandbox"}, nil
}

func (e *conformanceGatewayExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *conformanceGatewayExecutor) reset(errs ...error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = 0
	e.errs = errs
}

func runAcrossGatewayTransports(t *testing.T, name string, run func(*testing.T, *gatewayConformanceFixture) gatewayContractResult, assert func(*testing.T, gatewayContractResult, *gatewayConformanceFixture)) {
	t.Helper()
	results := make(map[string]gatewayContractResult, 2)
	for _, httpTransport := range []bool{false, true} {
		name := "standalone"
		if httpTransport {
			name = "kubernetes-http"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newGatewayConformanceFixture(t, httpTransport)
			result := run(t, fixture)
			assert(t, result, fixture)
			results[name] = result
		})
	}
	if !reflect.DeepEqual(results["standalone"], results["kubernetes-http"]) {
		t.Fatalf("%s transport results differ: standalone=%#v kubernetes=%#v", name, results["standalone"], results["kubernetes-http"])
	}
}

func newGatewayConformanceFixture(t *testing.T, httpTransport bool) *gatewayConformanceFixture {
	t.Helper()
	const namespace = "team-a"
	store := &gatewayStore{data: map[string][]byte{}}
	agent := v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: namespace, UID: "agent-uid"}}
	identity := v1alpha1.AgentIdentity{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-identity-planner", Namespace: namespace, Labels: map[string]string{"kognition.dev/auto-created-for-agent": "planner"}, OwnerReferences: []v1alpha1.OwnerReference{{Kind: "Agent", Name: "planner", UID: "agent-uid"}}}, Spec: v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}}, Status: v1alpha1.AgentIdentityStatus{Phase: v1alpha1.PhaseReady}}
	instance := v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-r1", Namespace: namespace, UID: "instance-uid"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}}, Status: v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhaseRunning, SessionID: "session-1"}}
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: namespace, ResourceVersion: 7}}
	storeGatewayObject(t, store, "Agent", namespace, agent.ObjectMeta.Name, agent)
	storeGatewayObject(t, store, "AgentIdentity", namespace, identity.ObjectMeta.Name, identity)
	storeGatewayObject(t, store, "AgentInstance", namespace, instance.ObjectMeta.Name, instance)
	storeGatewayObject(t, store, "Tool", namespace, tool.ObjectMeta.Name, tool)

	fixture := &gatewayConformanceFixture{store: store, authorizer: &gatewayAuthorizer{}, quota: &gatewayQuota{}, executor: &conformanceGatewayExecutor{}, ledger: &gatewayLedger{}, http: httpTransport}
	core, err := New(store, fixture.authorizer, fixture.quota, fixture.executor, zaptest.NewLogger(t), WithLedger(fixture.ledger))
	if err != nil {
		t.Fatal(err)
	}
	fixture.core = core
	if !httpTransport {
		return fixture
	}
	issuer, err := runtimeauth.NewToolCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sessionIssuer, err := runtimeauth.NewToolSessionCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(store, sessionIssuer, issuer, core, zaptest.NewLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	fixture.server = server
	fixture.issuer = issuer
	fixture.sessionIssuer = sessionIssuer
	fixture.binding = runtimeauth.ToolCapabilityBinding{Namespace: namespace, AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", ToolName: "crm-update", ToolResourceVersion: 7, Verb: "invoke"}
	return fixture
}

func (f *gatewayConformanceFixture) replaceExecutor(t *testing.T, executor Executor) {
	t.Helper()
	core, err := New(f.store, f.authorizer, f.quota, executor, zaptest.NewLogger(t), WithLedger(f.ledger))
	if err != nil {
		t.Fatal(err)
	}
	f.core = core
	if !f.http {
		return
	}
	server, err := NewServer(f.store, f.sessionIssuer, f.issuer, core, zaptest.NewLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	f.server = server
}

func (f *gatewayConformanceFixture) setToolPolicy(policy *v1alpha1.ToolExecutionPolicy) {
	var tool v1alpha1.Tool
	if err := json.Unmarshal(f.store.data["Tool/team-a/crm-update"], &tool); err != nil {
		panic(err)
	}
	tool.Spec.ExecutionPolicy = policy
	data, err := json.Marshal(tool)
	if err != nil {
		panic(err)
	}
	f.store.data["Tool/team-a/crm-update"] = data
}

func (f *gatewayConformanceFixture) invoke(request Request) gatewayContractResult {
	request.Namespace = "team-a"
	request.Principal = "planner"
	request.SubjectKind = "AgentInstance"
	request.SubjectName = "planner-r1"
	request.ToolName = "crm-update"
	request.ToolResourceVersion = 7
	if request.Input == nil {
		request.Input = map[string]any{}
	}
	if !f.http {
		response, err := f.core.Invoke(context.Background(), request)
		return normalizeCoreGatewayResult(response, err)
	}
	binding := f.binding
	binding.InvocationID = request.InvocationID
	inputDigest, err := toolInputDigest(request.Input)
	if err != nil {
		panic(err)
	}
	binding.InputDigest = inputDigest
	capability, err := f.issuer.Issue(binding)
	if err != nil {
		panic(err)
	}
	body, err := json.Marshal(map[string]any{"tool_name": request.ToolName, "tool_resource_version": request.ToolResourceVersion, "invocation_id": request.InvocationID, "input": request.Input})
	if err != nil {
		panic(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost, "/v1/tools/invoke", bytes.NewReader(body))
	httpRequest.Header.Set("Authorization", "Bearer "+capability)
	response := httptest.NewRecorder()
	f.server.ServeHTTP(response, httpRequest)
	return normalizeHTTPGatewayResult(response.Code, response.Body.Bytes())
}

func normalizeCoreGatewayResult(response Response, err error) gatewayContractResult {
	result := gatewayContractResult{InvocationID: response.InvocationID, Output: response.Output}
	if err == nil {
		result.Outcome = gatewayOutcomeSuccess
		return result
	}
	switch {
	case errors.Is(err, store.ErrConflict):
		result.Outcome = gatewayOutcomeConflict
	case errors.Is(err, ErrToolInvocationInProgress):
		result.Outcome = gatewayOutcomeInProgress
	default:
		var structured *runtime.StructuredToolError
		if errors.As(err, &structured) && structured.Code == "ToolAuditCompletionFailed" {
			result.Outcome = gatewayOutcomeAuditFailure
			result.ErrorCode = structured.Code
		} else {
			result.Outcome = gatewayOutcomeRejected
			if errors.As(err, &structured) {
				result.ErrorCode = structured.Code
			} else {
				result.ErrorCode = "ToolInvocationDenied"
			}
		}
	}
	return result
}

func normalizeHTTPGatewayResult(status int, body []byte) gatewayContractResult {
	var response Response
	if err := json.Unmarshal(body, &response); err == nil && response.InvocationID != "" {
		result := gatewayContractResult{InvocationID: response.InvocationID, Output: response.Output}
		switch status {
		case http.StatusOK:
			result.Outcome = gatewayOutcomeSuccess
		case http.StatusConflict:
			result.Outcome = gatewayOutcomeConflict
		case http.StatusInternalServerError:
			result.Outcome = gatewayOutcomeAuditFailure
			result.ErrorCode = response.ErrorCode
		default:
			result.Outcome = gatewayOutcomeRejected
			result.ErrorCode = response.ErrorCode
		}
		return result
	}
	if status == http.StatusConflict {
		return gatewayContractResult{Outcome: gatewayOutcomeConflict}
	}
	return gatewayContractResult{Outcome: gatewayOutcomeRejected}
}

func (r gatewayContractResult) String() string {
	return fmt.Sprintf("outcome=%s invocation=%s output=%v error=%s", r.Outcome, r.InvocationID, r.Output, r.ErrorCode)
}
