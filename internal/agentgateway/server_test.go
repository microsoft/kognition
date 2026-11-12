// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestEnsureClaimantDefaultsUsesInjectedWorkloadUID(t *testing.T) {
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_WORKLOAD_UID", "pod-uid-0123456789")
	server := &Server{}

	server.ensureClaimantDefaults()

	if got := server.claimant.WorkloadUID; got != "pod-uid-0123456789" {
		t.Fatalf("claim workload UID = %q, want injected Pod UID", got)
	}
	if server.claimant.ProcessStartNonce == "" {
		t.Fatal("claim process-start nonce is empty")
	}
}

type testStore struct {
	data     map[string][]byte
	receipts map[string]store.ToolInvocationReceipt
}

type communicationAuditStore struct {
	entries       []observability.AuditEntry
	err           error
	targetReached func() bool
}

func (s *communicationAuditStore) Append(_ context.Context, entry observability.AuditEntry) error {
	if s.err != nil {
		return s.err
	}
	hasInvokeEvidence := false
	for _, existing := range s.entries {
		if existing.Verb == "invoke" {
			hasInvokeEvidence = true
			break
		}
	}
	if entry.Verb == "invoke" && !hasInvokeEvidence && s.targetReached != nil && s.targetReached() {
		return errors.New("invoke authorization evidence was written after target dispatch")
	}
	s.entries = append(s.entries, entry)
	return nil
}
func (s *communicationAuditStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	for _, entry := range entries {
		if err := s.Append(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}
func (s *communicationAuditStore) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return append([]observability.AuditEntry(nil), s.entries...), nil
}

func (s *testStore) Ping(context.Context) error { return nil }
func (s *testStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	value, found := s.data[resourceKey(kind, namespace, name)]
	if !found {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), value...), nil
}
func (s *testStore) List(_ context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	prefix := kind + "/" + opts.Namespace + "/"
	var values [][]byte
	for key, value := range s.data {
		if strings.HasPrefix(key, prefix) {
			values = append(values, append([]byte(nil), value...))
		}
	}
	return values, nil
}
func (s *testStore) Create(_ context.Context, kind, namespace, name string, data []byte) error {
	key := resourceKey(kind, namespace, name)
	if _, found := s.data[key]; found {
		return store.ErrAlreadyExists
	}
	s.data[key] = append([]byte(nil), data...)
	return nil
}
func (s *testStore) Update(_ context.Context, kind, namespace, name string, data []byte, _ int64) error {
	key := resourceKey(kind, namespace, name)
	if _, found := s.data[key]; !found {
		return store.ErrNotFound
	}
	s.data[key] = append([]byte(nil), data...)
	return nil
}
func (s *testStore) Delete(_ context.Context, kind, namespace, name string) error {
	key := resourceKey(kind, namespace, name)
	if _, found := s.data[key]; !found {
		return store.ErrNotFound
	}
	delete(s.data, key)
	return nil
}
func (s *testStore) Watch(context.Context, string, store.ListOptions) (<-chan store.WatchEvent, error) {
	return nil, fmt.Errorf("not implemented")
}
func (s *testStore) CreateRevision(context.Context, string, string, string, int64, []byte, []byte) error {
	return fmt.Errorf("not implemented")
}
func (s *testStore) ListRevisions(context.Context, string, string, string) ([]store.Revision, error) {
	return nil, fmt.Errorf("not implemented")
}
func (s *testStore) GetRevision(context.Context, string, string, string, int64) (*store.Revision, error) {
	return nil, fmt.Errorf("not implemented")
}
func (s *testStore) AppendEvent(context.Context, string, string, string, store.InstanceEvent) error {
	return nil
}
func (s *testStore) ListEvents(context.Context, string, string, string) ([]store.InstanceEvent, error) {
	return nil, nil
}
func (s *testStore) ClaimToolInvocation(_ context.Context, receipt store.ToolInvocationReceipt) (bool, store.ToolInvocationReceipt, error) {
	if s.receipts == nil {
		s.receipts = make(map[string]store.ToolInvocationReceipt)
	}
	key := receipt.Namespace + "/" + receipt.InvocationID
	if existing, found := s.receipts[key]; found {
		if existing.Binding != receipt.Binding {
			return false, store.ToolInvocationReceipt{}, store.ErrConflict
		}
		return false, existing, nil
	}
	s.receipts[key] = receipt
	return true, receipt, nil
}
func (s *testStore) PrepareToolInvocation(_ context.Context, receipt store.ToolInvocationReceipt) (bool, store.ToolInvocationReceipt, error) {
	if s.receipts == nil {
		s.receipts = make(map[string]store.ToolInvocationReceipt)
	}
	key := receipt.Namespace + "/" + receipt.InvocationID
	if existing, found := s.receipts[key]; found {
		if existing.Status != store.ToolInvocationPrepared || existing.Binding != receipt.Binding || existing.PreparationToken != receipt.PreparationToken {
			return false, store.ToolInvocationReceipt{}, store.ErrConflict
		}
		return true, existing, nil
	}
	s.receipts[key] = receipt
	return true, receipt, nil
}
func (s *testStore) ReleasePreparedToolInvocation(_ context.Context, namespace, invocationID, preparationToken string) (bool, error) {
	key := namespace + "/" + invocationID
	existing, found := s.receipts[key]
	if !found {
		return false, nil
	}
	if existing.Status != store.ToolInvocationPrepared || existing.PreparationToken != preparationToken {
		return false, store.ErrConflict
	}
	delete(s.receipts, key)
	return true, nil
}
func (s *testStore) CompleteToolInvocation(_ context.Context, receipt store.ToolInvocationReceipt) error {
	key := receipt.Namespace + "/" + receipt.InvocationID
	existing, found := s.receipts[key]
	if !found {
		return store.ErrNotFound
	}
	if existing.Binding != receipt.Binding {
		return store.ErrConflict
	}
	if existing.Status == store.ToolInvocationPending {
		s.receipts[key] = receipt
	}
	return nil
}

type recordingAuthorizer struct {
	allowed    bool
	sameCalls  int
	crossCalls int
}

func (a *recordingAuthorizer) EvaluateDispatch(context.Context, string, string, string) (bool, error) {
	a.sameCalls++
	return a.allowed, nil
}
func (a *recordingAuthorizer) EvaluateCrossWorkspaceDispatch(context.Context, string, string, string, string) (bool, error) {
	a.crossCalls++
	return a.allowed, nil
}

func resourceKey(kind, namespace, name string) string { return kind + "/" + namespace + "/" + name }

type rewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t rewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL = new(url.URL)
	*clone.URL = *request.URL
	clone.URL.Scheme, clone.URL.Host = t.target.Scheme, t.target.Host
	return t.base.RoundTrip(clone)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func storeObject(t *testing.T, s *testStore, kind, namespace, name string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	s.data[resourceKey(kind, namespace, name)] = raw
}

func delegationGatewayFixture(t *testing.T, allowed bool) (*Server, *runtimeauth.AgentDelegationSessionCapabilityIssuer, *runtimeauth.AgentDelegationCapabilityIssuer, *recordingAuthorizer, *int, *httptest.Server) {
	t.Helper()
	key := []byte("0123456789abcdef0123456789abcdef")
	sessionIssuer, err := runtimeauth.NewAgentDelegationSessionCapabilityIssuer(key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	exactIssuer, err := runtimeauth.NewAgentDelegationCapabilityIssuer(key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	calls := new(int)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Method != http.MethodPost || r.URL.Path != "/invoke" || r.Header.Get("Authorization") != "Bearer runtime-token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["message"] != "review this" || body["session_id"] != "delegation-1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(target.Close)

	targetURL, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := &testStore{data: map[string][]byte{}}
	sourceAgent := v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "team-a", UID: "planner-uid"}}
	sourceIdentity := v1alpha1.AgentIdentity{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-identity-planner", Namespace: "team-a", Labels: map[string]string{"kognition.dev/auto-created-for-agent": "planner"}, OwnerReferences: []v1alpha1.OwnerReference{{Kind: "Agent", Name: "planner", UID: "planner-uid"}}},
		Spec:       v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}, SPIFFEID: "spiffe://kognition.dev/workspace/team-a/agent/planner"},
		Status:     v1alpha1.AgentIdentityStatus{Phase: v1alpha1.PhaseReady},
	}
	sourceInstance := v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-r1", Namespace: "team-a", UID: "instance-uid"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}}, Status: v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhaseRunning, SessionID: "session-1"}}
	targetService := v1alpha1.AgentService{ObjectMeta: v1alpha1.ObjectMeta{Name: "review-service", Namespace: "team-b"}, Spec: v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}}, Status: v1alpha1.AgentServiceStatus{Phase: v1alpha1.PhaseReady, ReadyInstances: 1, Endpoint: "http://untrusted.invalid/invoke"}}
	storeObject(t, s, "Agent", "team-a", "planner", sourceAgent)
	storeObject(t, s, "AgentIdentity", "team-a", "agent-identity-planner", sourceIdentity)
	storeObject(t, s, "AgentInstance", "team-a", "planner-r1", sourceInstance)
	storeObject(t, s, "AgentService", "team-b", "review-service", targetService)
	authorizer := &recordingAuthorizer{allowed: allowed}
	client := &http.Client{Transport: rewriteTransport{target: targetURL, base: target.Client().Transport}}
	server, err := NewServer(s, sessionIssuer, exactIssuer, authorizer, "runtime-token", "team-b", client, zaptest.NewLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	return server, sessionIssuer, exactIssuer, authorizer, calls, target
}

func delegationRequest(t *testing.T, path, token, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func TestServerExchangesAndInvokesBoundCrossWorkspaceAgentService(t *testing.T) {
	// TC-0303
	server, sessionIssuer, _, authorizer, calls, _ := delegationGatewayFixture(t, true)
	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	exchange := httptest.NewRecorder()
	server.ServeHTTP(exchange, delegationRequest(t, "/v1/agent-services/capabilities", session, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`))
	if exchange.Code != http.StatusOK || authorizer.crossCalls != 1 {
		t.Fatalf("exchange status=%d crossCalls=%d body=%s", exchange.Code, authorizer.crossCalls, exchange.Body.String())
	}
	var exchanged struct {
		Capability string `json:"capability"`
	}
	if err := json.NewDecoder(exchange.Body).Decode(&exchanged); err != nil || exchanged.Capability == "" {
		t.Fatalf("exchange response = %s, error=%v", exchange.Body.String(), err)
	}

	invoke := httptest.NewRecorder()
	server.ServeHTTP(invoke, delegationRequest(t, "/v1/agent-services/invoke", exchanged.Capability, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`))
	if invoke.Code != http.StatusAccepted || authorizer.crossCalls != 2 || *calls != 1 {
		t.Fatalf("invoke status=%d crossCalls=%d targetCalls=%d body=%s", invoke.Code, authorizer.crossCalls, *calls, invoke.Body.String())
	}
}

func TestServerRecordsRedactedCommunicationTrustDecisionLifecycle(t *testing.T) {
	server, sessionIssuer, _, _, calls, _ := delegationGatewayFixture(t, true)
	auditStore := &communicationAuditStore{targetReached: func() bool { return *calls > 0 }}
	server.audit = observability.NewCognitiveAuditLog(auditStore)
	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	exchange := httptest.NewRecorder()
	exchangeRequest := delegationRequest(t, "/v1/agent-services/capabilities", session, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this secret"}`)
	exchangeRequest.Header.Set(observability.TrustDecisionRunIDHeader, "demo-run")
	server.ServeHTTP(exchange, exchangeRequest)
	var exchanged struct {
		Capability string `json:"capability"`
	}
	if exchange.Code != http.StatusOK {
		t.Fatalf("exchange status=%d body=%s", exchange.Code, exchange.Body.String())
	}
	if err := json.NewDecoder(exchange.Body).Decode(&exchanged); err != nil {
		t.Fatal(err)
	}
	invoke := httptest.NewRecorder()
	invokeRequest := delegationRequest(t, "/v1/agent-services/invoke", exchanged.Capability, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this secret"}`)
	invokeRequest.Header.Set(observability.TrustDecisionRunIDHeader, "demo-run")
	server.ServeHTTP(invoke, invokeRequest)
	if invoke.Code != http.StatusBadGateway {
		t.Fatalf("invoke status=%d body=%s", invoke.Code, invoke.Body.String())
	}
	if len(auditStore.entries) != 3 {
		t.Fatalf("entries=%d, want exchange allowed plus invoke allowed/failed", len(auditStore.entries))
	}
	schema := loadCommunicationTrustDecisionSchema(t)
	for index, entry := range auditStore.entries {
		var decision any
		if err := json.Unmarshal(entry.Details, &decision); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(decision); err != nil {
			t.Fatalf("decision %d does not match schema: %v\n%s", index, err, entry.Details)
		}
		payload := string(entry.Details)
		if strings.Contains(payload, "review this secret") || strings.Contains(payload, exchanged.Capability) || !strings.Contains(payload, `"plane":"communication"`) || !strings.Contains(payload, `"runId":"demo-run"`) {
			t.Fatalf("decision %d is incorrect or sensitive: %s", index, payload)
		}
	}
}

func TestServerRejectsInvalidTrustDecisionRunIDBeforeDelegation(t *testing.T) {
	server, sessionIssuer, _, authorizer, calls, _ := delegationGatewayFixture(t, true)
	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	request := delegationRequest(t, "/v1/agent-services/capabilities", session, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`)
	request.Header.Set(observability.TrustDecisionRunIDHeader, strings.Repeat("a", observability.MaxTrustDecisionRunIDLength+1))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || authorizer.crossCalls != 0 || *calls != 0 {
		t.Fatalf("status=%d policyCalls=%d targetCalls=%d body=%s", response.Code, authorizer.crossCalls, *calls, response.Body.String())
	}
}

func TestServerBlocksTargetWhenCommunicationEvidenceCannotBePersisted(t *testing.T) {
	server, _, exactIssuer, _, calls, _ := delegationGatewayFixture(t, true)
	server.audit = observability.NewCognitiveAuditLog(&communicationAuditStore{err: errors.New("audit unavailable")})
	capability, err := exactIssuer.Issue(runtimeauth.AgentDelegationCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", TargetNamespace: "team-b", TargetAgentService: "review-service", TargetAgent: "reviewer", InvocationID: "delegation-1", MessageDigest: delegationMessageDigest("review this")})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, delegationRequest(t, "/v1/agent-services/invoke", capability, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`))
	if response.Code != http.StatusServiceUnavailable || *calls != 0 {
		t.Fatalf("status=%d targetCalls=%d body=%s", response.Code, *calls, response.Body.String())
	}
}

func loadCommunicationTrustDecisionSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	schema, err := compiler.Compile(filepath.Join("..", "..", "api", "schemas", "trust-decision-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestServerDeniesDelegationBeforeCapabilityAndTargetInvocation(t *testing.T) {
	server, sessionIssuer, _, authorizer, calls, _ := delegationGatewayFixture(t, false)
	auditStore := &communicationAuditStore{}
	server.audit = observability.NewCognitiveAuditLog(auditStore)
	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, delegationRequest(t, "/v1/agent-services/capabilities", session, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`))
	if response.Code != http.StatusForbidden || authorizer.crossCalls != 1 || *calls != 0 {
		t.Fatalf("status=%d crossCalls=%d targetCalls=%d body=%s", response.Code, authorizer.crossCalls, *calls, response.Body.String())
	}
	assertCommunicationDecisionOutcome(t, auditStore.entries, "denied", "policy_denied")
}

func TestServerRejectsMismatchedExactDelegationRequest(t *testing.T) {
	server, _, exactIssuer, authorizer, calls, _ := delegationGatewayFixture(t, true)
	auditStore := &communicationAuditStore{}
	server.audit = observability.NewCognitiveAuditLog(auditStore)
	capability, err := exactIssuer.Issue(runtimeauth.AgentDelegationCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", TargetNamespace: "team-b", TargetAgentService: "review-service", TargetAgent: "reviewer", InvocationID: "delegation-1", MessageDigest: delegationMessageDigest("review this")})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, delegationRequest(t, "/v1/agent-services/invoke", capability, `{"target_namespace":"team-b","target_agent_service":"other-service","invocation_id":"delegation-1","message":"review this"}`))
	if response.Code != http.StatusForbidden || authorizer.crossCalls != 0 || *calls != 0 {
		t.Fatalf("status=%d crossCalls=%d targetCalls=%d body=%s", response.Code, authorizer.crossCalls, *calls, response.Body.String())
	}
	assertCommunicationDecisionOutcome(t, auditStore.entries, "denied", "capability_invalid")
}

func TestServerRevalidatesPolicyBeforeExactDelegation(t *testing.T) {
	server, sessionIssuer, _, authorizer, calls, _ := delegationGatewayFixture(t, true)
	auditStore := &communicationAuditStore{}
	server.audit = observability.NewCognitiveAuditLog(auditStore)
	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	exchange := httptest.NewRecorder()
	server.ServeHTTP(exchange, delegationRequest(t, "/v1/agent-services/capabilities", session, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`))
	if exchange.Code != http.StatusOK {
		t.Fatalf("exchange status=%d body=%s", exchange.Code, exchange.Body.String())
	}
	var exchanged struct {
		Capability string `json:"capability"`
	}
	if err := json.NewDecoder(exchange.Body).Decode(&exchanged); err != nil {
		t.Fatal(err)
	}
	authorizer.allowed = false
	invoke := httptest.NewRecorder()
	server.ServeHTTP(invoke, delegationRequest(t, "/v1/agent-services/invoke", exchanged.Capability, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`))
	if invoke.Code != http.StatusForbidden || authorizer.crossCalls != 2 || *calls != 0 {
		t.Fatalf("invoke status=%d crossCalls=%d targetCalls=%d body=%s", invoke.Code, authorizer.crossCalls, *calls, invoke.Body.String())
	}
	assertCommunicationDecisionOutcome(t, auditStore.entries[1:], "revoked", "policy_denied")
}

func assertCommunicationDecisionOutcome(t *testing.T, entries []observability.AuditEntry, outcome, reasonCode string) {
	t.Helper()
	if len(entries) != 1 {
		t.Fatalf("entries=%d, want 1", len(entries))
	}
	var decision observability.TrustDecisionV1
	if err := json.Unmarshal(entries[0].Details, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Plane != "communication" || decision.Outcome != outcome || decision.ReasonCode != reasonCode || decision.Execution.TargetReached == nil || *decision.Execution.TargetReached {
		t.Fatalf("decision = %#v", decision)
	}
	if decision.Identity == nil || decision.Identity.AgentIdentity == "" || decision.Identity.AgentIdentityRef == nil || decision.Identity.AgentIdentityRef.Name != "agent-identity-planner" || decision.Authorization.Identity != "allowed" {
		t.Fatalf("verified identity lost on subsequent denial: %#v", decision)
	}
	if decision.Approval == nil || decision.Approval.Status != "not_evaluated" || decision.Approval.DecisionRef != nil {
		t.Fatalf("approval must be unevaluated after authorization denial: %#v", decision.Approval)
	}
}

func TestServerReturnsStoredResponseForReplayAndRejectsMessageSubstitutionBeforeTarget(t *testing.T) {
	server, _, exactIssuer, authorizer, calls, _ := delegationGatewayFixture(t, true)
	capability, err := exactIssuer.Issue(runtimeauth.AgentDelegationCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", TargetNamespace: "team-b", TargetAgentService: "review-service", TargetAgent: "reviewer", InvocationID: "delegation-1", MessageDigest: delegationMessageDigest("review this")})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`
	for attempt := 0; attempt < 2; attempt++ {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, delegationRequest(t, "/v1/agent-services/invoke", capability, body))
		if response.Code != http.StatusAccepted {
			t.Fatalf("attempt %d status=%d body=%s", attempt, response.Code, response.Body.String())
		}
	}
	if *calls != 1 || authorizer.crossCalls != 2 {
		t.Fatalf("targetCalls=%d crossCalls=%d, want one target call and two live policy checks", *calls, authorizer.crossCalls)
	}
	substitution := httptest.NewRecorder()
	server.ServeHTTP(substitution, delegationRequest(t, "/v1/agent-services/invoke", capability, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"changed message"}`))
	if substitution.Code != http.StatusForbidden || *calls != 1 {
		t.Fatalf("substitution status=%d targetCalls=%d body=%s", substitution.Code, *calls, substitution.Body.String())
	}
}

func TestServerProxyBoundsTargetInvocation(t *testing.T) {
	server, _, _, _, _, _ := delegationGatewayFixture(t, true)
	var deadline time.Time
	incoming := httptest.NewRequest(http.MethodPost, "/", nil)
	incoming.Header.Set("X-Kognition-Run-ID", "delegation-run")
	incoming.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	server.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("X-Kognition-Run-ID") != "delegation-run" || request.Header.Get("traceparent") != incoming.Header.Get("traceparent") {
			t.Fatal("target invocation lost run or trace correlation")
		}
		var hasDeadline bool
		deadline, hasDeadline = request.Context().Deadline()
		if !hasDeadline {
			t.Fatal("target invocation request has no deadline")
		}
		return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	if err := server.proxy(context.Background(), incoming, "http://target.example/invoke", "delegation-1", "review this"); err != nil {
		t.Fatalf("proxy() error = %v", err)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > targetInvokeTimeout {
		t.Fatalf("target invocation deadline has %s remaining, want (0, %s]", remaining, targetInvokeTimeout)
	}
}
