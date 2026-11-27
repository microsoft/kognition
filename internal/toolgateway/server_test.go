// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/invocation"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

func storeGatewayObject(t *testing.T, store *gatewayStore, kind, namespace, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	store.data[kind+"/"+namespace+"/"+name] = data
}

func toolGatewayServerFixture(t *testing.T) (*Server, *gatewayStore, *runtimeauth.ToolSessionCapabilityIssuer, *runtimeauth.ToolCapabilityIssuer, runtimeauth.ToolCapabilityBinding, *gatewayExecutor, *gatewayLedger) {
	t.Helper()
	const namespace = "team-a"
	store := &gatewayStore{data: map[string][]byte{}}
	agent := v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: namespace, UID: "agent-uid"}}
	identity := v1alpha1.AgentIdentity{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-identity-planner", Namespace: namespace, Labels: map[string]string{"kognition.dev/auto-created-for-agent": "planner"}, OwnerReferences: []v1alpha1.OwnerReference{{Kind: "Agent", Name: "planner", UID: "agent-uid"}}},
		Spec:       v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}, SPIFFEID: "spiffe://kognition.dev/workspace/team-a/agent/planner"},
		Status:     v1alpha1.AgentIdentityStatus{Phase: v1alpha1.PhaseReady},
	}
	instance := v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-r1", Namespace: namespace, UID: "instance-uid"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}}, Status: v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhaseRunning, SessionID: "session-1"}}
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: namespace, ResourceVersion: 7}}
	storeGatewayObject(t, store, "Agent", namespace, agent.ObjectMeta.Name, agent)
	storeGatewayObject(t, store, "AgentIdentity", namespace, identity.ObjectMeta.Name, identity)
	storeGatewayObject(t, store, "AgentInstance", namespace, instance.ObjectMeta.Name, instance)
	storeGatewayObject(t, store, "Tool", namespace, tool.ObjectMeta.Name, tool)

	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	ledger := &gatewayLedger{}
	core, err := New(store, &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}, &gatewayQuota{}, executor, zaptest.NewLogger(t), WithLedger(ledger))
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := runtimeauth.NewToolCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	inputDigest, err := toolInputDigest(map[string]any{"ticket": "42"})
	if err != nil {
		t.Fatal(err)
	}
	binding := runtimeauth.ToolCapabilityBinding{Namespace: namespace, AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", ToolName: "crm-update", ToolResourceVersion: 7, Verb: "invoke", InvocationID: "invocation-1", InputDigest: inputDigest}
	sessionIssuer, err := runtimeauth.NewToolSessionCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(store, sessionIssuer, issuer, core, zaptest.NewLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	return server, store, sessionIssuer, issuer, binding, executor, ledger
}

func toolGatewayRequest(t *testing.T, token string, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/tools/invoke", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request
}

func TestServerWritesUnavailableInvocationAsServiceUnavailable(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&Server{}).writeInvocationError(recorder, Response{
		InvocationID:         "invocation-1",
		RuntimeApprovalState: RuntimeApprovalStateUnavailable,
	}, errors.New("finalize runtime approval execution: unavailable"))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	var response Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.ErrorCode != "ToolInvocationUnavailable" || response.RuntimeApprovalState != RuntimeApprovalStateUnavailable {
		t.Fatalf("unexpected unavailable response: %#v", response)
	}
}

func TestServerVerifiesCapabilityAndExecutesBoundTool(t *testing.T) {
	server, _, _, issuer, binding, executor, ledger := toolGatewayServerFixture(t)
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := toolGatewayRequest(t, capability, `{"tool_name":"crm-update","tool_resource_version":7,"invocation_id":"invocation-1","input":{"ticket":"42"}}`)
	request.Header.Set("X-Kognition-Run-ID", "demo-run")
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || executor.calls != 1 || len(ledger.starts) != 1 || len(ledger.completions) != 1 {
		t.Fatalf("status=%d executor=%d starts=%d completions=%d body=%s", response.Code, executor.calls, len(ledger.starts), len(ledger.completions), response.Body.String())
	}
	if ledger.starts[0].RunID != "demo-run" || ledger.completions[0].RunID != "demo-run" {
		t.Fatalf("ledger run IDs = start %q, completion %q", ledger.starts[0].RunID, ledger.completions[0].RunID)
	}
	body := response.Body.Bytes()
	var result Response
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result.InvocationID != binding.InvocationID || result.Output["updated"] != true {
		t.Fatalf("result = %#v", result)
	}
	var wireBody map[string]any
	if err := json.Unmarshal(body, &wireBody); err != nil {
		t.Fatal(err)
	}
	if wireBody["invocation_id"] != binding.InvocationID || wireBody["output"].(map[string]any)["updated"] != true {
		t.Fatalf("wire response = %#v", wireBody)
	}
}

func TestServerDoesNotLogSensitiveInvocationErrors(t *testing.T) {
	const secret = "synthetic-backend-credential-do-not-log"
	failures := []struct {
		name string
		err  error
		code string
	}{
		{name: "plain", err: errors.New("backend response: " + secret), code: "ToolInvocationDenied"},
		{name: "wrapped", err: fmt.Errorf("execute backend: %w", errors.New(secret)), code: "ToolInvocationDenied"},
		{name: "joined", err: errors.Join(errors.New("backend response: "+secret), errors.New("another failure")), code: "ToolInvocationDenied"},
		{name: "structured", err: &runtime.StructuredToolError{Code: "ToolTransportError", Message: secret}, code: "ToolTransportError"},
	}
	for _, test := range failures {
		t.Run(test.name, func(t *testing.T) {
			server, _, _, issuer, binding, executor, ledger := toolGatewayServerFixture(t)
			core, logs := observer.New(zap.InfoLevel)
			logger := zap.New(core)
			server.logger = logger.Named("tool-gateway-server")
			server.gateway.logger = logger
			executor.err = test.err
			capability, err := issuer.Issue(binding)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, toolGatewayRequest(t, capability, `{"tool_name":"crm-update","tool_resource_version":7,"invocation_id":"invocation-1","input":{"ticket":"42"}}`))
			if recorder.Code != http.StatusForbidden || executor.calls != 1 || len(ledger.starts) != 1 || len(ledger.completions) != 1 {
				t.Fatalf("status=%d calls=%d starts=%d completions=%d", recorder.Code, executor.calls, len(ledger.starts), len(ledger.completions))
			}
			var response Response
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.ErrorCode != test.code || response.ErrorMessage != "tool invocation is not authorized" || response.InvocationID != binding.InvocationID ||
				strings.Contains(recorder.Body.String(), secret) {
				t.Fatalf("unexpected error response: %s", recorder.Body.String())
			}
			failures := logs.FilterMessage("tool invocation failed").All()
			if len(failures) != 1 {
				t.Fatalf("failure log count = %d, want 1", len(failures))
			}
			fields := failures[0].ContextMap()
			if fields["namespace"] != binding.Namespace || fields["agent"] != binding.AgentName ||
				fields["tool"] != binding.ToolName || fields["invocation"] != binding.InvocationID {
				t.Fatalf("missing failure correlation fields: %v", fields)
			}
			for _, entry := range logs.All() {
				if strings.Contains(entry.Message, secret) || strings.Contains(fmt.Sprint(entry.ContextMap()), secret) {
					t.Fatalf("sensitive content reached logs: %s", entry.Message)
				}
			}
			if _, exists := fields["error"]; exists {
				t.Fatal("raw error field was logged")
			}
		})
	}
}

func TestServerPreservesIntegerRevisionAfterStrictJSONValidation(t *testing.T) {
	for _, revision := range []int64{10, 36910} {
		server, storage, _, issuer, binding, executor, _ := toolGatewayServerFixture(t)
		tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: binding.ToolName, Namespace: binding.Namespace, ResourceVersion: revision}}
		storeGatewayObject(t, storage, "Tool", binding.Namespace, binding.ToolName, tool)
		binding.ToolResourceVersion = revision
		capability, err := issuer.Issue(binding)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(map[string]any{"tool_name": binding.ToolName, "tool_resource_version": revision, "invocation_id": binding.InvocationID, "input": map[string]any{"ticket": "42"}})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, toolGatewayRequest(t, capability, string(body)))
		if response.Code != http.StatusOK || executor.calls != 1 {
			t.Fatalf("valid integer revision %d rejected: status=%d calls=%d body=%s", revision, response.Code, executor.calls, response.Body.String())
		}
		duplicate := httptest.NewRecorder()
		server.ServeHTTP(duplicate, toolGatewayRequest(t, capability, `{"tool_name":"crm-update","tool_resource_version":7,"tool_resource_version":10,"invocation_id":"invocation-1","input":{"ticket":"42"}}`))
		if duplicate.Code != http.StatusBadRequest || executor.calls != 1 {
			t.Fatal("duplicate revision escaped strict validation or reached execution")
		}
	}
}

func TestServerRecordsSignedRuntimeAttribution(t *testing.T) {
	server, _, _, issuer, binding, _, _ := toolGatewayServerFixture(t)
	lineage := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0, CostTagger: observability.NewCostTagger()})
	var costs []observability.CostRecord
	lineage.SetCostSink(func(record observability.CostRecord) { costs = append(costs, record) })
	server.gateway.tracer = invocation.NewObservabilityTracer(lineage)
	binding.Solution = "research-solution"
	binding.SolutionRev = 4
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, toolGatewayRequest(t, capability, `{"tool_name":"crm-update","tool_resource_version":7,"invocation_id":"invocation-1","input":{"ticket":"42"}}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	spans := lineage.Spans()
	if len(spans) != 1 {
		t.Fatalf("spans = %#v", spans)
	}
	span := spans[0]
	if span.Attributes[observability.AttrWorkspace] != "team-a" || span.Attributes[observability.AttrAgentNamespace] != "team-a" || span.Attributes[observability.AttrAgentIdentity] != "spiffe://kognition.dev/workspace/team-a/agent/planner" || span.Attributes[observability.AttrSolution] != "research-solution" || span.Attributes[observability.AttrSolutionRev] != int64(4) || span.Attributes[observability.AttrSessionID] != "session-1" {
		t.Fatalf("tool span attribution = %#v", span.Attributes)
	}
	if len(costs) != 1 || costs[0].OperationType != observability.CostOpToolExecution || costs[0].ToolName != "crm-update" || costs[0].CostUnits != 0 {
		t.Fatalf("unpriced tool invocation cost records: %#v", costs)
	}
}

func TestServerRejectsInvalidAndMismatchedCapabilitiesBeforeExecution(t *testing.T) {
	server, _, _, issuer, binding, executor, _ := toolGatewayServerFixture(t)
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		token string
		body  string
		want  int
	}{
		{name: "missing", body: `{"tool_name":"crm-update","tool_resource_version":7,"invocation_id":"invocation-1"}`, want: http.StatusUnauthorized},
		{name: "invalid", token: "invalid", body: `{"tool_name":"crm-update","tool_resource_version":7,"invocation_id":"invocation-1"}`, want: http.StatusUnauthorized},
		{name: "wrong tool", token: capability, body: `{"tool_name":"other","tool_resource_version":7,"invocation_id":"invocation-1"}`, want: http.StatusForbidden},
		{name: "wrong invocation", token: capability, body: `{"tool_name":"crm-update","tool_resource_version":7,"invocation_id":"other"}`, want: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.ServeHTTP(response, toolGatewayRequest(t, test.token, test.body))
			if response.Code != test.want {
				t.Fatalf("status=%d, want %d: %s", response.Code, test.want, response.Body.String())
			}
		})
	}
	if executor.calls != 0 {
		t.Fatalf("executor calls=%d, want no execution", executor.calls)
	}
}

func TestServerRejectsCapabilityAfterSessionReplacement(t *testing.T) {
	server, store, _, issuer, binding, executor, _ := toolGatewayServerFixture(t)
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	var instance v1alpha1.AgentInstance
	if err := json.Unmarshal(store.data["AgentInstance/team-a/planner-r1"], &instance); err != nil {
		t.Fatal(err)
	}
	instance.Status.SessionID = "replacement-session"
	storeGatewayObject(t, store, "AgentInstance", "team-a", "planner-r1", instance)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, toolGatewayRequest(t, capability, `{"tool_name":"crm-update","tool_resource_version":7,"invocation_id":"invocation-1"}`))
	if response.Code != http.StatusForbidden || executor.calls != 0 {
		t.Fatalf("status=%d executor=%d, want session rejection before execution", response.Code, executor.calls)
	}
}

func TestServerExchangesSessionCapabilityForExactToolCapability(t *testing.T) {
	server, _, sessionIssuer, toolIssuer, binding, executor, ledger := toolGatewayServerFixture(t)
	sessionCapability, err := sessionIssuer.Issue(runtimeauth.ToolSessionCapabilityBinding{
		Namespace: binding.Namespace, AgentName: binding.AgentName, AgentIdentityName: binding.AgentIdentityName,
		InstanceName: binding.InstanceName, InstanceUID: binding.InstanceUID, SessionID: binding.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	ambiguousExchange := httptest.NewRequest(http.MethodPost, "/v1/tools/capabilities", bytes.NewBufferString(`{"tool_name":"crm-update","invocation_id":"invocation-2","input":{"ticket":"43","ticket":"44"}}`))
	ambiguousExchange.Header.Set("Authorization", "Bearer "+sessionCapability)
	ambiguousResponse := httptest.NewRecorder()
	server.ServeHTTP(ambiguousResponse, ambiguousExchange)
	if ambiguousResponse.Code != http.StatusBadRequest || executor.calls != 0 || len(ledger.starts) != 0 {
		t.Fatalf("ambiguous exchange status=%d executor=%d starts=%d body=%s", ambiguousResponse.Code, executor.calls, len(ledger.starts), ambiguousResponse.Body.String())
	}
	exchange := httptest.NewRequest(http.MethodPost, "/v1/tools/capabilities", bytes.NewBufferString(`{"tool_name":"crm-update","invocation_id":"invocation-2","input":{"ticket":"43"}}`))
	exchange.Header.Set("Authorization", "Bearer "+sessionCapability)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, exchange)
	if response.Code != http.StatusOK || executor.calls != 0 || len(ledger.starts) != 0 || len(ledger.decisions) != 0 {
		t.Fatalf("exchange status=%d executor=%d starts=%d body=%s", response.Code, executor.calls, len(ledger.starts), response.Body.String())
	}
	var exchanged struct {
		Capability          string `json:"capability"`
		ToolResourceVersion int64  `json:"tool_resource_version"`
	}
	if err := json.NewDecoder(response.Body).Decode(&exchanged); err != nil {
		t.Fatal(err)
	}
	if exchanged.ToolResourceVersion != binding.ToolResourceVersion {
		t.Fatalf("exchanged tool resource version=%d, want %d", exchanged.ToolResourceVersion, binding.ToolResourceVersion)
	}
	inputDigest, err := toolInputDigest(map[string]any{"ticket": "43"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := toolIssuer.Verify(exchanged.Capability, runtimeauth.ToolCapabilityBinding{
		Namespace: binding.Namespace, AgentName: binding.AgentName, AgentIdentityName: binding.AgentIdentityName,
		InstanceName: binding.InstanceName, InstanceUID: binding.InstanceUID, SessionID: binding.SessionID,
		ToolName: binding.ToolName, ToolResourceVersion: binding.ToolResourceVersion, Verb: "invoke", InvocationID: "invocation-2", InputDigest: inputDigest,
	}); err != nil {
		t.Fatalf("exchanged capability binding: %v", err)
	}
	substitutedResponse := httptest.NewRecorder()
	server.ServeHTTP(substitutedResponse, toolGatewayRequest(t, exchanged.Capability, `{"tool_name":"crm-update","tool_resource_version":7,"invocation_id":"invocation-2","input":{"ticket":"44"}}`))
	if substitutedResponse.Code != http.StatusForbidden || executor.calls != 0 || len(ledger.starts) != 0 {
		t.Fatalf("substituted input status=%d executor=%d starts=%d body=%s", substitutedResponse.Code, executor.calls, len(ledger.starts), substitutedResponse.Body.String())
	}
	invoke := toolGatewayRequest(t, exchanged.Capability, `{"tool_name":"crm-update","tool_resource_version":7,"invocation_id":"invocation-2","input":{"ticket":"43"}}`)
	invokeResponse := httptest.NewRecorder()
	server.ServeHTTP(invokeResponse, invoke)
	if invokeResponse.Code != http.StatusOK || executor.calls != 1 || len(ledger.starts) != 1 {
		t.Fatalf("invoke status=%d executor=%d starts=%d body=%s", invokeResponse.Code, executor.calls, len(ledger.starts), invokeResponse.Body.String())
	}
	duplicateResponse := httptest.NewRecorder()
	server.ServeHTTP(duplicateResponse, toolGatewayRequest(t, exchanged.Capability, `{"tool_name":"crm-update","tool_resource_version":7,"invocation_id":"invocation-2","input":{"ticket":"43"}}`))
	if duplicateResponse.Code != http.StatusOK || executor.calls != 1 || len(ledger.starts) != 1 {
		t.Fatalf("duplicate status=%d executor=%d starts=%d body=%s", duplicateResponse.Code, executor.calls, len(ledger.starts), duplicateResponse.Body.String())
	}
}

func TestServerRejectsSessionCapabilityAfterSessionReplacement(t *testing.T) {
	server, store, sessionIssuer, _, binding, executor, ledger := toolGatewayServerFixture(t)
	sessionCapability, err := sessionIssuer.Issue(runtimeauth.ToolSessionCapabilityBinding{
		Namespace: binding.Namespace, AgentName: binding.AgentName, AgentIdentityName: binding.AgentIdentityName,
		InstanceName: binding.InstanceName, InstanceUID: binding.InstanceUID, SessionID: binding.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	var instance v1alpha1.AgentInstance
	if err := json.Unmarshal(store.data["AgentInstance/team-a/planner-r1"], &instance); err != nil {
		t.Fatal(err)
	}
	instance.Status.SessionID = "replacement-session"
	storeGatewayObject(t, store, "AgentInstance", "team-a", "planner-r1", instance)
	exchange := httptest.NewRequest(http.MethodPost, "/v1/tools/capabilities", bytes.NewBufferString(`{"tool_name":"crm-update","invocation_id":"invocation-2","input":{}}`))
	exchange.Header.Set("Authorization", "Bearer "+sessionCapability)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, exchange)
	if response.Code != http.StatusForbidden || executor.calls != 0 || len(ledger.starts) != 0 || len(ledger.decisions) != 0 {
		t.Fatalf("status=%d executor=%d starts=%d body=%s", response.Code, executor.calls, len(ledger.starts), response.Body.String())
	}
}

func TestServerAuditsAuthenticatedCapabilityAuthorizationDenial(t *testing.T) {
	server, storage, sessionIssuer, _, binding, executor, ledger := toolGatewayServerFixture(t)
	server.gateway.authorizer = &gatewayAuthorizer{
		decision: AuthorizationDecision{GrantRef: "team-a/planner-crm@3"},
		err:      ErrToolArgumentConstraintsDenied,
	}
	token, err := sessionIssuer.Issue(runtimeauth.ToolSessionCapabilityBinding{
		Namespace: binding.Namespace, AgentName: binding.AgentName, AgentIdentityName: binding.AgentIdentityName,
		InstanceName: binding.InstanceName, InstanceUID: binding.InstanceUID, SessionID: binding.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/tools/capabilities",
		strings.NewReader(`{"tool_name":"crm-update","invocation_id":"denied-exchange","input":{"ticket":"sensitive-input"}}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(observability.TrustDecisionRunIDHeader, "trust-denial-run")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || executor.calls != 0 || len(ledger.decisions) != 1 {
		t.Fatalf("status=%d executions=%d decisions=%d, want 403/0/1", response.Code, executor.calls, len(ledger.decisions))
	}
	entry := ledger.decisions[0]
	if entry.Status != "denied" || entry.ReasonCode != "argument_constraints_denied" ||
		entry.AgentIdentity != "spiffe://kognition.dev/workspace/team-a/agent/planner" ||
		entry.SessionID != binding.SessionID || entry.SubjectName != binding.InstanceName ||
		entry.InvocationID != "denied-exchange" || entry.RunID != "trust-denial-run" ||
		entry.GrantRef != "team-a/planner-crm@3" || entry.ToolVersion != binding.ToolResourceVersion {
		t.Fatalf("denial lost verified attribution or authority: %#v", entry)
	}
	wire, err := json.Marshal(toolTrustDecision("denial", time.Now(), entry, entry.Status))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "sensitive-input") || strings.Contains(string(wire), token) {
		t.Fatal("denial evidence includes input or session credentials")
	}
	if len(ledger.starts) != 0 || len(ledger.completions) != 0 || len(storage.receipts) != 0 {
		t.Fatal("denied capability exchange reserved an execution")
	}
}

func TestServerCapabilityDenialAuditFailsClosedAndRejectsUntrustedAttribution(t *testing.T) {
	for _, test := range []struct {
		name        string
		stale       bool
		invalidRun  bool
		invalidAuth bool
		auditError  bool
		status      int
		decisions   int
	}{
		{name: "audit-unavailable", auditError: true, status: http.StatusServiceUnavailable, decisions: 1},
		{name: "stale-instance", stale: true, status: http.StatusForbidden},
		{name: "invalid-run", invalidRun: true, status: http.StatusBadRequest},
		{name: "invalid-auth", invalidAuth: true, status: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, storage, issuer, _, binding, executor, ledger := toolGatewayServerFixture(t)
			server.gateway.authorizer = &gatewayAuthorizer{err: ErrToolArgumentConstraintsDenied}
			if test.auditError {
				ledger.decisionErr = errors.New("audit storage unavailable")
			}
			if test.stale {
				binding.InstanceUID = "replaced-instance"
			}
			token, err := issuer.Issue(runtimeauth.ToolSessionCapabilityBinding{
				Namespace: binding.Namespace, AgentName: binding.AgentName, AgentIdentityName: binding.AgentIdentityName,
				InstanceName: binding.InstanceName, InstanceUID: binding.InstanceUID, SessionID: binding.SessionID,
			})
			if err != nil {
				t.Fatal(err)
			}
			if test.invalidAuth {
				token = "invalid"
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/tools/capabilities",
				strings.NewReader(`{"tool_name":"crm-update","invocation_id":"denied-exchange","input":{}}`))
			request.Header.Set("Authorization", "Bearer "+token)
			if test.invalidRun {
				request.Header.Set(observability.TrustDecisionRunIDHeader, strings.Repeat("x", observability.MaxTrustDecisionRunIDLength+1))
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.status || len(ledger.decisions) != test.decisions ||
				executor.calls != 0 || len(ledger.starts) != 0 || len(storage.receipts) != 0 {
				t.Fatalf("status=%d decisions=%d executions=%d receipts=%d body=%s",
					response.Code, len(ledger.decisions), executor.calls, len(storage.receipts), response.Body.String())
			}
		})
	}
}

func TestServerCapabilityDenialPersistsQueryableTrustEvidence(t *testing.T) {
	server, storage, issuer, _, binding, executor, _ := toolGatewayServerFixture(t)
	audits := &auditGatewayStore{gatewayStore: storage}
	server.gateway.ledger = NewStoreLedgerWithAudit(storage, observability.NewCognitiveAuditLog(audits))
	server.gateway.authorizer = &gatewayAuthorizer{err: ErrToolArgumentConstraintsDenied}
	token, err := issuer.Issue(runtimeauth.ToolSessionCapabilityBinding{
		Namespace: binding.Namespace, AgentName: binding.AgentName, AgentIdentityName: binding.AgentIdentityName,
		InstanceName: binding.InstanceName, InstanceUID: binding.InstanceUID, SessionID: binding.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/tools/capabilities",
		strings.NewReader(`{"tool_name":"crm-update","invocation_id":"denied-exchange","input":{"ticket":"private"}}`))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || executor.calls != 0 || len(audits.entries) != 1 {
		t.Fatalf("status=%d executions=%d audits=%d", response.Code, executor.calls, len(audits.entries))
	}
	result, err := observability.QueryTrustDecisions(request.Context(), audits, observability.TrustDecisionQuery{
		Namespace: binding.Namespace, AgentIdentity: "spiffe://kognition.dev/workspace/team-a/agent/planner",
		SessionID: binding.SessionID, Plane: "tool", Outcome: "denied", Limit: 10,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].ReasonCode != "argument_constraints_denied" ||
		result.Items[0].Request.InvocationID != "denied-exchange" || result.Items[0].Execution.TargetReached == nil ||
		*result.Items[0].Execution.TargetReached {
		t.Fatalf("queryable denial evidence = %#v", result)
	}
}

func TestServerHealthDoesNotRequireCapability(t *testing.T) {
	server, _, _, _, _, _, _ := toolGatewayServerFixture(t)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health status=%d, want 200", response.Code)
	}
}
