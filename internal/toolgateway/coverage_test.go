// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

type coverageResilienceCoordinator struct {
	acquireCalls int
	recordCalls  int
}

func (c *coverageResilienceCoordinator) Acquire(context.Context, string, *v1alpha1.ToolExecutionPolicy, time.Time) (func(context.Context) error, error) {
	c.acquireCalls++
	return func(context.Context) error { return nil }, nil
}

func (c *coverageResilienceCoordinator) Record(context.Context, string, *v1alpha1.ToolExecutionPolicy, bool, bool, time.Time) error {
	c.recordCalls++
	return nil
}

func TestGatewayConstructorAndInvokerContracts(t *testing.T) {
	if _, err := New(nil, &gatewayAuthorizer{}, nil, &gatewayExecutor{}, nil, WithLedger(&gatewayLedger{})); err == nil {
		t.Fatal("New() accepted a nil store")
	}
	if _, err := New(&gatewayStore{data: map[string][]byte{}}, nil, nil, &gatewayExecutor{}, nil, WithLedger(&gatewayLedger{})); err == nil {
		t.Fatal("New() accepted a nil authorizer")
	}
	if _, err := New(&gatewayStore{data: map[string][]byte{}}, &legacyGatewayAuthorizer{}, nil, &gatewayExecutor{}, nil, WithLedger(&gatewayLedger{})); err == nil || !strings.Contains(err.Error(), "invocation-aware") {
		t.Fatalf("New() legacy authorizer error = %v, want invocation-aware rejection", err)
	}
	if _, err := New(&gatewayStore{data: map[string][]byte{}}, &gatewayAuthorizer{}, nil, nil, nil, WithLedger(&gatewayLedger{})); err == nil {
		t.Fatal("New() accepted a nil executor")
	}
	if _, err := New(&gatewayStore{data: map[string][]byte{}}, &gatewayAuthorizer{}, nil, &gatewayExecutor{}, nil); err == nil {
		t.Fatal("New() accepted a missing ledger")
	}

	coordinator := &coverageResilienceCoordinator{}
	gateway := testGateway(t, &gatewayAuthorizer{}, &gatewayQuota{}, &gatewayExecutor{output: map[string]any{"ok": true}}, &gatewayLedger{}, 7)
	gateway.resilience = coordinator
	output, err := gateway.InvokeTool(context.Background(), "team-a", "workflow:nightly", "crm-update", map[string]any{"ticket": "42"})
	if err != nil || output["ok"] != true || coordinator.acquireCalls != 1 || coordinator.recordCalls != 1 {
		t.Fatalf("InvokeTool() output=%#v err=%v acquire=%d record=%d", output, err, coordinator.acquireCalls, coordinator.recordCalls)
	}
	entry := gateway.ledger.(*gatewayLedger).starts[0]
	if entry.SubjectKind != "Workflow" || entry.SubjectName != "nightly" {
		t.Fatalf("workflow ledger attribution = %#v", entry)
	}
}

func TestReceiptAndToolLoadFailureContracts(t *testing.T) {
	for _, receipt := range []store.ToolInvocationReceipt{
		{InvocationID: "pending", Status: store.ToolInvocationPending},
		{InvocationID: "failed", Status: store.ToolInvocationFailed, ErrorMessage: "target rejected"},
		{InvocationID: "broken-json", Status: store.ToolInvocationSucceeded, Output: []byte("{")},
		{InvocationID: "unknown", Status: "unknown"},
	} {
		t.Run(receipt.InvocationID, func(t *testing.T) {
			_, err := responseFromReceipt(receipt)
			if err == nil {
				t.Fatalf("responseFromReceipt(%#v) succeeded", receipt)
			}
		})
	}

	st := &gatewayStore{data: map[string][]byte{}}
	if _, err := loadTool(context.Background(), st, "team-a", "missing"); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("missing Tool error = %v, want ErrToolNotFound", err)
	}
	st.data["Tool/team-a/broken"] = []byte("not-json")
	if _, err := loadTool(context.Background(), st, "team-a", "broken"); err == nil {
		t.Fatal("loadTool() accepted malformed Tool data")
	}
}

func TestInvocationRunExecutorReturnsTerminalAndMalformedRunErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		status v1alpha1.ToolInvocationRunStatus
		want   string
	}{
		{name: "failed default", status: v1alpha1.ToolInvocationRunStatus{Phase: v1alpha1.PhaseFailed}, want: "durable Tool invocation failed"},
		{name: "terminated explicit", status: v1alpha1.ToolInvocationRunStatus{Phase: v1alpha1.PhaseTerminated, ErrorCode: "Cancelled", ErrorMessage: "operator cancelled"}, want: "operator cancelled"},
		{name: "unexpected", status: v1alpha1.ToolInvocationRunStatus{Phase: v1alpha1.Phase("Corrupt")}, want: "unexpected phase"},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := &gatewayStore{data: map[string][]byte{}}
			st.onCreate = func(kind, namespace, name string, data []byte) error {
				var run v1alpha1.ToolInvocationRun
				if err := json.Unmarshal(data, &run); err != nil {
					return err
				}
				run.Status = test.status
				encoded, _ := json.Marshal(run)
				st.data[kind+"/"+namespace+"/"+name] = encoded
				return nil
			}
			request := runtime.ToolExecutionRequest{InvocationID: test.name, Tool: v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "team-a", ResourceVersion: 7}, Spec: v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolShell}}}
			_, err := NewInvocationRunExecutor(st, nil).Execute(context.Background(), request)
			if err == nil || !containsError(err, test.want) {
				t.Fatalf("Execute() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestServerRejectsExchangeErrorsAndMapsGatewayErrors(t *testing.T) {
	server, store, sessionIssuer, _, binding, _, _ := toolGatewayServerFixture(t)
	sessionCapability, err := sessionIssuer.Issue(runtimeauth.ToolSessionCapabilityBinding{
		Namespace: binding.Namespace, AgentName: binding.AgentName, AgentIdentityName: binding.AgentIdentityName,
		InstanceName: binding.InstanceName, InstanceUID: binding.InstanceUID, SessionID: binding.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}

	invokeExchange := func(token, body string) int {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/v1/tools/capabilities", strings.NewReader(body))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response.Code
	}
	if code := invokeExchange("", `{"tool_name":"crm-update","invocation_id":"exchange"}`); code != http.StatusUnauthorized {
		t.Fatalf("missing session capability status = %d", code)
	}
	if code := invokeExchange("invalid", `{"tool_name":"crm-update","invocation_id":"exchange"}`); code != http.StatusUnauthorized {
		t.Fatalf("invalid session capability status = %d", code)
	}
	if code := invokeExchange(sessionCapability, `{}`); code != http.StatusBadRequest {
		t.Fatalf("invalid exchange body status = %d", code)
	}
	if code := invokeExchange(sessionCapability, `{"tool_name":"missing","invocation_id":"exchange","input":{}}`); code != http.StatusNotFound {
		t.Fatalf("missing Tool exchange status = %d", code)
	}

	var identity v1alpha1.AgentIdentity
	if err := json.Unmarshal(store.data["AgentIdentity/team-a/agent-identity-planner"], &identity); err != nil {
		t.Fatal(err)
	}
	identity.Status.Phase = v1alpha1.PhaseFailed
	storeGatewayObject(t, store, "AgentIdentity", "team-a", identity.ObjectMeta.Name, identity)
	if code := invokeExchange(sessionCapability, `{"tool_name":"crm-update","invocation_id":"exchange","input":{}}`); code != http.StatusForbidden {
		t.Fatalf("stale workload exchange status = %d", code)
	}

	for _, test := range []struct {
		err  error
		want int
	}{
		{err: ErrToolInvocationInProgress, want: http.StatusConflict},
		{err: ErrToolNotFound, want: http.StatusNotFound},
		{err: ErrToolVersionMismatch, want: http.StatusConflict},
		{err: &runtime.StructuredToolError{Code: "ToolAuditCompletionFailed", Message: "audit failed"}, want: http.StatusInternalServerError},
		{err: errors.New("denied"), want: http.StatusForbidden},
	} {
		response := httptest.NewRecorder()
		server.writeGatewayError(response, test.err)
		if response.Code != test.want {
			t.Fatalf("writeGatewayError(%v) status = %d, want %d", test.err, response.Code, test.want)
		}
	}
}

func TestStoreLedgerRejectsIncompleteEntries(t *testing.T) {
	ledger := NewStoreLedger(&gatewayStore{data: map[string][]byte{}})
	if err := ledger.RecordStart(context.Background(), LedgerEntry{}); err == nil {
		t.Fatal("RecordStart() accepted an incomplete entry")
	}
	if err := ledger.RecordCompletion(context.Background(), LedgerEntry{}); err == nil {
		t.Fatal("RecordCompletion() accepted an incomplete entry")
	}
}

func containsError(err error, want string) bool {
	return err != nil && want != "" && strings.Contains(err.Error(), want)
}
