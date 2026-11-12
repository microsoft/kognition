// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

// gate3FakeStore is an in-memory Gate3Store that mirrors the persistence
// contract exercised by the runtimeapproval fakes: CreateOrGetRuntimeApproval
// is idempotent by invocation ID and NotRequired approvals are finalized on
// create. This lets the test wire a real Gate3Coordinator and observe the
// durable RuntimeApproval side-effect.
type gate3FakeStore struct {
	mu        sync.Mutex
	approvals map[string]*v1alpha1.RuntimeApproval
	rv        int64
	creates   int
}

func newGate3FakeStore() *gate3FakeStore {
	return &gate3FakeStore{approvals: map[string]*v1alpha1.RuntimeApproval{}}
}

func (s *gate3FakeStore) CreateOrGetRuntimeApproval(_ context.Context, proposed v1alpha1.RuntimeApproval, _ json.RawMessage, ttl time.Duration) (bool, v1alpha1.RuntimeApproval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	if existing, ok := s.approvals[proposed.Spec.InvocationID]; ok {
		return false, *existing, nil
	}
	s.rv++
	stored := proposed
	stored.UID = "approval-" + proposed.Spec.InvocationID + "-uid"
	stored.ResourceVersion = s.rv
	stored.Spec.CreatedAt = time.Now().UTC()
	stored.Spec.ExpiresAt = stored.Spec.CreatedAt.Add(ttl)
	if stored.Status.DecisionState == v1alpha1.RuntimeApprovalDecisionNotRequired {
		runtimeapproval.FinalizeInitialStatus(&stored, time.Now())
	}
	s.approvals[proposed.Spec.InvocationID] = &stored
	return true, stored, nil
}

func (s *gate3FakeStore) AcknowledgeRuntimeApprovalDecisionAudit(_ context.Context, workspace, invocationID string, expectedRV int64, auditID string) (v1alpha1.RuntimeApproval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.approvals[invocationID]
	if !ok || stored.Spec.Workspace != workspace {
		return v1alpha1.RuntimeApproval{}, errors.New("gate3 fake: unknown approval")
	}
	if stored.ResourceVersion != expectedRV {
		return v1alpha1.RuntimeApproval{}, errors.New("gate3 fake: stale resource version")
	}
	if stored.Status.DecisionAudit == nil || stored.Status.DecisionAudit.ID != auditID {
		return v1alpha1.RuntimeApproval{}, errors.New("gate3 fake: audit id mismatch")
	}
	return *stored, nil
}

func (s *gate3FakeStore) snapshot(invocationID string) (v1alpha1.RuntimeApproval, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.approvals[invocationID]
	if !ok {
		return v1alpha1.RuntimeApproval{}, false
	}
	return *stored, true
}

type communicationExecutionStore struct {
	store.RuntimeApprovalExecutionStore
	events              []string
	claimErr            error
	claimResult         *v1alpha1.RuntimeApproval
	approval            *v1alpha1.RuntimeApproval
	finalizeErr         error
	claimed             bool
	prepareWhileClaimed int
	claimTokens         []string
}

func (s *communicationExecutionStore) GetRuntimeApprovalByInvocation(_ context.Context, namespace, invocationID string) (v1alpha1.RuntimeApproval, error) {
	if s.approval == nil || s.approval.Namespace != namespace || s.approval.Spec.InvocationID != invocationID {
		return v1alpha1.RuntimeApproval{}, store.ErrNotFound
	}
	return *s.approval, nil
}

func (s *communicationExecutionStore) PrepareRuntimeApproval(_ context.Context, namespace, invocationID string, expectedResourceVersion int64, token string, deadline time.Time) (v1alpha1.RuntimeApproval, error) {
	s.events = append(s.events, "prepare")
	if s.claimed {
		s.prepareWhileClaimed++
		return v1alpha1.RuntimeApproval{}, store.ErrConflict
	}
	return v1alpha1.RuntimeApproval{ObjectMeta: v1alpha1.ObjectMeta{Namespace: namespace, ResourceVersion: expectedResourceVersion + 1}, Spec: v1alpha1.RuntimeApprovalSpec{InvocationID: invocationID}, Status: v1alpha1.RuntimeApprovalStatus{PreparationToken: token, PreparationDeadline: &deadline}}, nil
}

func (s *communicationExecutionStore) ClaimRuntimeApprovalExecution(_ context.Context, namespace, invocationID string, expectedResourceVersion int64, token string, claimant v1alpha1.RuntimeApprovalClaimant) (v1alpha1.RuntimeApproval, error) {
	s.events = append(s.events, "claim")
	s.claimTokens = append(s.claimTokens, token)
	if s.claimErr != nil {
		return v1alpha1.RuntimeApproval{}, s.claimErr
	}
	if s.claimResult != nil {
		return *s.claimResult, nil
	}
	s.claimed = true
	return v1alpha1.RuntimeApproval{ObjectMeta: v1alpha1.ObjectMeta{Namespace: namespace, ResourceVersion: expectedResourceVersion + 1}, Spec: v1alpha1.RuntimeApprovalSpec{InvocationID: invocationID}, Status: v1alpha1.RuntimeApprovalStatus{ExecutionState: v1alpha1.RuntimeApprovalExecutionClaimed, Claimant: &claimant, FencingToken: 42}}, nil
}

func (s *communicationExecutionStore) AbortRuntimeApprovalBeforeDispatch(_ context.Context, _, _ string, _ int64, _, _ string) (v1alpha1.RuntimeApproval, error) {
	s.events = append(s.events, "abort")
	return v1alpha1.RuntimeApproval{}, nil
}

func (s *communicationExecutionStore) FinalizeRuntimeApprovalExecution(_ context.Context, _, _ string, _ int64, _ int64, disposition string) (v1alpha1.RuntimeApproval, error) {
	s.events = append(s.events, "finalize:"+disposition)
	if s.finalizeErr != nil {
		return v1alpha1.RuntimeApproval{}, s.finalizeErr
	}
	s.claimed = false
	return v1alpha1.RuntimeApproval{}, nil
}

type communicationRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f communicationRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestServerAgentCommunicationGate3ReadyClaimsBeforeIssuanceAndFinalizesSuccess(t *testing.T) {
	server, sessionIssuer, _, _, _, _ := delegationGatewayFixture(t, true)
	auditStore := &communicationAuditStore{}
	server.audit = observability.NewCognitiveAuditLog(auditStore)
	approval := v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "approval-1", Namespace: "team-a", UID: "approval-uid", ResourceVersion: 7},
		Spec: v1alpha1.RuntimeApprovalSpec{
			InvocationID: "delegation-1", Plane: v1alpha1.RuntimeApprovalPlaneCommunication, SessionRef: "session-1",
			AgentInstanceRef: v1alpha1.RuntimeApprovalReference{UID: "instance-uid"},
			TargetRef:        v1alpha1.RuntimeApprovalReference{Namespace: "team-b", Name: "review-service"},
		},
		Status: v1alpha1.RuntimeApprovalStatus{DecisionState: v1alpha1.RuntimeApprovalDecisionApproved},
	}
	executionStore := &communicationExecutionStore{approval: &approval}
	server.gate3 = &captureCommunicationGate3Resolver{result: runtimeapproval.Gate3Result{
		State:    runtimeapproval.Gate3StateReady,
		Approval: approval,
	}}
	server.gate3Builder = func(context.Context, *runtimeauth.AgentDelegationSessionCapabilityClaims, *v1alpha1.AgentIdentity, string, string, string, string, string, int64, string) (runtimeapproval.Gate3Request, bool) {
		return runtimeapproval.Gate3Request{}, true
	}
	server.runtimeApprovals = executionStore
	baseTransport := server.client.Transport
	server.client.Transport = communicationRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		executionStore.events = append(executionStore.events, "target")
		return baseTransport.RoundTrip(request)
	})

	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	exchange := httptest.NewRecorder()
	server.ServeHTTP(exchange, delegationRequest(t, "/v1/agent-services/capabilities", session, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`))
	if exchange.Code != http.StatusOK {
		t.Fatalf("exchange status = %d, want %d: %s", exchange.Code, http.StatusOK, exchange.Body.String())
	}
	wantIssuance := []string{"prepare", "claim", "finalize:Succeeded"}
	if !reflect.DeepEqual(executionStore.events, wantIssuance) {
		t.Fatalf("issuance events = %#v, want %#v", executionStore.events, wantIssuance)
	}
	if len(executionStore.claimTokens) != 1 {
		t.Fatalf("claim tokens = %d, want 1", len(executionStore.claimTokens))
	}
	if err := runtimeapproval.ValidatePreparationToken(executionStore.claimTokens[0]); err != nil {
		t.Fatalf("default preparation token does not satisfy RuntimeApproval contract: %v", err)
	}
	var exchanged struct {
		Capability string `json:"capability"`
	}
	if err := json.Unmarshal(exchange.Body.Bytes(), &exchanged); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, delegationRequest(t, "/v1/agent-services/invoke", exchanged.Capability, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`))

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusAccepted, response.Body.String())
	}
	want := []string{"prepare", "claim", "finalize:Succeeded", "target"}
	if !reflect.DeepEqual(executionStore.events, want) {
		t.Fatalf("execution events = %#v, want %#v", executionStore.events, want)
	}
	if len(auditStore.entries) != 3 {
		t.Fatalf("audit entries=%d, want exchange and invocation lifecycle", len(auditStore.entries))
	}
	for _, entry := range auditStore.entries {
		var decision observability.TrustDecisionV1
		if err := json.Unmarshal(entry.Details, &decision); err != nil {
			t.Fatal(err)
		}
		if decision.Approval == nil || decision.Approval.Status != "approved" || decision.Approval.DecisionRef == nil || decision.Approval.DecisionRef.UID != approval.UID || decision.Session == nil || decision.Session.CapabilityID == "" {
			t.Fatalf("lost approval or capability evidence: %s", entry.Details)
		}
	}
}

func TestServerAgentCommunicationGate3ClaimFailureLeavesPreparationResumable(t *testing.T) {
	server, sessionIssuer, _, _, calls, _ := delegationGatewayFixture(t, true)
	executionStore := &communicationExecutionStore{claimErr: errors.New("claim unavailable")}
	server.gate3 = &captureCommunicationGate3Resolver{result: runtimeapproval.Gate3Result{
		State: runtimeapproval.Gate3StateReady,
		Approval: v1alpha1.RuntimeApproval{
			ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", ResourceVersion: 7},
			Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: "delegation-1"},
		},
	}}
	server.gate3Builder = func(context.Context, *runtimeauth.AgentDelegationSessionCapabilityClaims, *v1alpha1.AgentIdentity, string, string, string, string, string, int64, string) (runtimeapproval.Gate3Request, bool) {
		return runtimeapproval.Gate3Request{}, true
	}
	server.runtimeApprovals = executionStore

	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, delegationRequest(t, "/v1/agent-services/capabilities", session, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if *calls != 0 {
		t.Fatalf("target calls = %d, want 0", *calls)
	}
	want := []string{"prepare", "claim"}
	if !reflect.DeepEqual(executionStore.events, want) {
		t.Fatalf("execution events = %#v, want %#v", executionStore.events, want)
	}
}

func TestServerAgentCommunicationGate3ResumesExistingPreparationToken(t *testing.T) {
	server, _, _, _, _, _ := delegationGatewayFixture(t, true)
	executionStore := &communicationExecutionStore{}
	server.runtimeApprovals = executionStore
	server.claimant = v1alpha1.RuntimeApprovalClaimant{WorkloadUID: "gateway", ProcessStartNonce: "process"}
	deadline := time.Now().Add(time.Minute)
	approval := &v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", ResourceVersion: 8},
		Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: "delegation-1"},
		Status: v1alpha1.RuntimeApprovalStatus{
			PreparationToken:    "original-preparation-token",
			PreparationDeadline: &deadline,
		},
	}

	claimed, err := server.prepareAndClaimApproval(context.Background(), approval)
	if err != nil {
		t.Fatalf("prepareAndClaimApproval() error = %v", err)
	}
	if claimed == nil || claimed.Status.ExecutionState != v1alpha1.RuntimeApprovalExecutionClaimed {
		t.Fatalf("claimed approval = %#v", claimed)
	}
	if !reflect.DeepEqual(executionStore.events, []string{"claim"}) {
		t.Fatalf("execution events = %#v, want claim without prepare", executionStore.events)
	}
	if !reflect.DeepEqual(executionStore.claimTokens, []string{"original-preparation-token"}) {
		t.Fatalf("claim tokens = %#v, want original preparation token", executionStore.claimTokens)
	}
}

func TestServerAgentCommunicationGate3TerminalClaimResultBlocksCapabilityIssuance(t *testing.T) {
	server, sessionIssuer, _, _, calls, _ := delegationGatewayFixture(t, true)
	executionStore := &communicationExecutionStore{claimResult: &v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", ResourceVersion: 9},
		Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: "delegation-1"},
		Status: v1alpha1.RuntimeApprovalStatus{
			ExecutionState:       v1alpha1.RuntimeApprovalExecutionFailed,
			ExecutionDisposition: runtimeapproval.ExecutionDispositionApprovalExpiredBeforeClaim,
		},
	}}
	server.gate3 = &captureCommunicationGate3Resolver{result: runtimeapproval.Gate3Result{
		State: runtimeapproval.Gate3StateReady,
		Approval: v1alpha1.RuntimeApproval{
			ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", ResourceVersion: 7},
			Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: "delegation-1"},
		},
	}}
	server.gate3Builder = func(context.Context, *runtimeauth.AgentDelegationSessionCapabilityClaims, *v1alpha1.AgentIdentity, string, string, string, string, string, int64, string) (runtimeapproval.Gate3Request, bool) {
		return runtimeapproval.Gate3Request{}, true
	}
	server.runtimeApprovals = executionStore

	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, delegationRequest(t, "/v1/agent-services/capabilities", session, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if *calls != 0 {
		t.Fatalf("target calls = %d, want 0", *calls)
	}
	want := []string{"prepare", "claim"}
	if !reflect.DeepEqual(executionStore.events, want) {
		t.Fatalf("execution events = %#v, want %#v", executionStore.events, want)
	}
}

func TestServerAgentCommunicationGate3UnknownStateBlocksCapabilityIssuance(t *testing.T) {
	for _, failure := range []string{"unknown state", "resolver error"} {
		t.Run(failure, func(t *testing.T) { testCommunicationGateUnavailableEvidence(t, failure) })
	}
}

func testCommunicationGateUnavailableEvidence(t *testing.T, failure string) {
	server, sessionIssuer, _, _, calls, _ := delegationGatewayFixture(t, true)
	auditStore := &communicationAuditStore{}
	server.audit = observability.NewCognitiveAuditLog(auditStore)
	executionStore := &communicationExecutionStore{}
	resolver := &captureCommunicationGate3Resolver{result: runtimeapproval.Gate3Result{State: runtimeapproval.Gate3State("unexpected")}}
	if failure == "resolver error" {
		resolver.err = errors.New("approval store unavailable")
	}
	server.gate3 = resolver
	server.gate3Builder = func(context.Context, *runtimeauth.AgentDelegationSessionCapabilityClaims, *v1alpha1.AgentIdentity, string, string, string, string, string, int64, string) (runtimeapproval.Gate3Request, bool) {
		return runtimeapproval.Gate3Request{}, true
	}
	server.runtimeApprovals = executionStore
	issueCalls := 0
	server.issueCapability = func(runtimeauth.AgentDelegationCapabilityBinding) (string, error) {
		issueCalls++
		return "", nil
	}

	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, delegationRequest(t, "/v1/agent-services/capabilities", session, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if issueCalls != 0 || *calls != 0 || len(executionStore.events) != 0 {
		t.Fatalf("issue calls = %d, target calls = %d, execution events = %#v; want all zero", issueCalls, *calls, executionStore.events)
	}
	if len(auditStore.entries) != 1 {
		t.Fatalf("entries=%d, want one blocked decision", len(auditStore.entries))
	}
	var decision observability.TrustDecisionV1
	if err := json.Unmarshal(auditStore.entries[0].Details, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Approval == nil || decision.Approval.Status != "unavailable" || decision.ReasonCode != "runtime_approval_unavailable" || decision.Authorization.Policy != "allowed" || decision.Execution.Status != "blocked" || decision.Execution.TargetReached == nil || *decision.Execution.TargetReached {
		t.Fatalf("incorrect unavailable evidence: %s", auditStore.entries[0].Details)
	}
}

func TestPrepareAndClaimApprovalRejectsMismatchedContainerIncarnation(t *testing.T) {
	server, _, _, _, _, _ := delegationGatewayFixture(t, true)
	server.claimant = v1alpha1.RuntimeApprovalClaimant{WorkloadUID: "gateway", ContainerID: "container-current", ProcessStartNonce: "process"}
	executionStore := &communicationExecutionStore{claimResult: &v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", ResourceVersion: 9},
		Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: "delegation-1"},
		Status: v1alpha1.RuntimeApprovalStatus{
			ExecutionState: v1alpha1.RuntimeApprovalExecutionClaimed,
			FencingToken:   42,
			Claimant:       &v1alpha1.RuntimeApprovalClaimant{WorkloadUID: "gateway", ContainerID: "container-stale", ProcessStartNonce: "process"},
		},
	}}
	server.runtimeApprovals = executionStore

	approval := &v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", ResourceVersion: 7},
		Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: "delegation-1"},
	}
	if claimed, err := server.prepareAndClaimApproval(context.Background(), approval); err == nil || claimed != nil {
		t.Fatalf("prepareAndClaimApproval() = %#v, %v; want ownership error", claimed, err)
	}
}

func TestServerAgentCommunicationGate3AmbiguousCompletionDoesNotIssueAgain(t *testing.T) {
	server, sessionIssuer, _, _, calls, _ := delegationGatewayFixture(t, true)
	executionStore := &communicationExecutionStore{finalizeErr: errors.New("finalization unavailable")}
	server.gate3 = &captureCommunicationGate3Resolver{result: runtimeapproval.Gate3Result{
		State: runtimeapproval.Gate3StateReady,
		Approval: v1alpha1.RuntimeApproval{
			ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", ResourceVersion: 7},
			Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: "delegation-1"},
		},
	}}
	server.gate3Builder = func(context.Context, *runtimeauth.AgentDelegationSessionCapabilityClaims, *v1alpha1.AgentIdentity, string, string, string, string, string, int64, string) (runtimeapproval.Gate3Request, bool) {
		return runtimeapproval.Gate3Request{}, true
	}
	server.runtimeApprovals = executionStore
	issueCalls := 0
	issueCapability := server.issueCapability
	server.issueCapability = func(binding runtimeauth.AgentDelegationCapabilityBinding) (string, error) {
		issueCalls++
		return issueCapability(binding)
	}

	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`
	for attempt := 0; attempt < 2; attempt++ {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, delegationRequest(t, "/v1/agent-services/capabilities", session, body))
		if response.Code < http.StatusInternalServerError {
			t.Fatalf("attempt %d status = %d, want server error: %s", attempt, response.Code, response.Body.String())
		}
	}

	if *calls != 0 {
		t.Fatalf("target calls = %d, want 0", *calls)
	}
	want := []string{"prepare", "claim", "finalize:Succeeded", "prepare"}
	if !reflect.DeepEqual(executionStore.events, want) {
		t.Fatalf("execution events = %#v, want %#v", executionStore.events, want)
	}
	if executionStore.prepareWhileClaimed != 1 {
		t.Fatalf("prepare attempts against claimed approval = %d, want 1", executionStore.prepareWhileClaimed)
	}
	if issueCalls != 1 {
		t.Fatalf("capability issuance calls = %d, want 1", issueCalls)
	}
}

type captureCommunicationGate3Resolver struct {
	result   runtimeapproval.Gate3Result
	requests []runtimeapproval.Gate3Request
	err      error
}

func (r *captureCommunicationGate3Resolver) Resolve(_ context.Context, request runtimeapproval.Gate3Request) (runtimeapproval.Gate3Result, error) {
	r.requests = append(r.requests, request)
	return r.result, r.err
}

func TestServerAgentCommunicationGate3PendingBlocksExactCapabilityIssuance(t *testing.T) {
	server, sessionIssuer, _, _, calls, _ := delegationGatewayFixture(t, true)
	server.gate3 = &captureCommunicationGate3Resolver{result: runtimeapproval.Gate3Result{
		State: runtimeapproval.Gate3StatePending,
		Approval: v1alpha1.RuntimeApproval{
			TypeMeta:   v1alpha1.TypeMeta{Kind: "RuntimeApproval", APIVersion: v1alpha1.APIVersion},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "approval-delegation-1", Namespace: "team-a", UID: "approval-uid", ResourceVersion: 7},
			Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: "delegation-1"},
		},
		ExpiresAt: time.Now().Add(time.Minute),
	}}
	server.gate3Builder = func(context.Context, *runtimeauth.AgentDelegationSessionCapabilityClaims, *v1alpha1.AgentIdentity, string, string, string, string, string, int64, string) (runtimeapproval.Gate3Request, bool) {
		return runtimeapproval.Gate3Request{}, true
	}
	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.ServeHTTP(response, delegationRequest(t, "/v1/agent-services/capabilities", session, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"delegation-1","message":"review this"}`))

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusAccepted, response.Body.String())
	}
	if *calls != 0 {
		t.Fatalf("target calls = %d, want 0", *calls)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode pending response: %v", err)
	}
	if capability, exists := body["capability"]; exists && capability != "" {
		t.Fatalf("pending exchange issued capability %q", capability)
	}
	if body["runtime_approval_state"] != string(runtimeapproval.Gate3StatePending) {
		t.Fatalf("runtime_approval_state = %v, want %q", body["runtime_approval_state"], runtimeapproval.Gate3StatePending)
	}
}

func TestServerAgentCommunicationGate3PendingRetryPreservesIdentityAndCompleteMessage(t *testing.T) {
	server, sessionIssuer, _, _, calls, _ := delegationGatewayFixture(t, true)
	resolver := &captureCommunicationGate3Resolver{result: runtimeapproval.Gate3Result{
		State: runtimeapproval.Gate3StatePending,
		Approval: v1alpha1.RuntimeApproval{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "approval-delegation-1", Namespace: "team-a", UID: "approval-uid", ResourceVersion: 7},
			Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: "delegation-1"},
		},
		ExpiresAt: time.Now().Add(time.Minute),
	}}
	server.gate3 = resolver
	server.gate3Builder = NewGate3RequestBuilder(server.store)
	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	const message = "  review line one\nreview line two  "
	bodyBytes, err := json.Marshal(map[string]string{
		"target_namespace": "team-b", "target_agent_service": "review-service",
		"invocation_id": "delegation-1", "message": message,
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, delegationRequest(t, "/v1/agent-services/capabilities", session, string(bodyBytes)))
		if response.Code != http.StatusAccepted {
			t.Fatalf("attempt %d status = %d, want %d: %s", attempt, response.Code, http.StatusAccepted, response.Body.String())
		}
		var pending delegationPendingResponse
		if err := json.Unmarshal(response.Body.Bytes(), &pending); err != nil {
			t.Fatalf("attempt %d decode pending response: %v", attempt, err)
		}
		if pending.InvocationID != "delegation-1" || pending.RuntimeApproval == nil || pending.RuntimeApproval.UID != "approval-uid" {
			t.Fatalf("attempt %d pending identity = %#v", attempt, pending)
		}
	}

	if *calls != 0 {
		t.Fatalf("target calls = %d, want 0", *calls)
	}
	if len(resolver.requests) != 2 {
		t.Fatalf("Gate 3 requests = %d, want 2", len(resolver.requests))
	}
	for attempt, request := range resolver.requests {
		if request.InvocationID != "delegation-1" || request.SessionID != "session-1" {
			t.Fatalf("attempt %d invocation/session = %q/%q", attempt, request.InvocationID, request.SessionID)
		}
		var payload struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(request.CanonicalPayload, &payload); err != nil {
			t.Fatalf("attempt %d decode canonical payload: %v", attempt, err)
		}
		if payload.Message != message {
			t.Fatalf("attempt %d canonical message = %q, want %q", attempt, payload.Message, message)
		}
	}
	if !reflect.DeepEqual(resolver.requests[0].CanonicalPayload, resolver.requests[1].CanonicalPayload) {
		t.Fatalf("pending retry changed canonical payload:\nfirst:  %s\nsecond: %s", resolver.requests[0].CanonicalPayload, resolver.requests[1].CanonicalPayload)
	}
}

// TestServerAgentCommunicationGate3PendingPersistsApprovalBeforeTargetDispatch
// discharges the ADR-0014 slice-D contract for the Agent Communication
// Gateway: when the Runtime Autonomy Gate parks an exact-capability
// delegation on a durable pending RuntimeApproval, the gateway MUST return
// HTTP 202 with the tagged approval reference, record a pending
// communication trust decision, and MUST NOT dispatch to the target
// AgentService. The test wires the real *runtimeapproval.Gate3Coordinator
// over a real Resolver and an in-memory Gate3Store so the durable
// side-effect is observable. The synthetic supervised AutonomyClass path
// (RequiresApproval:true) is exercised implicitly because no TrustProfile
// or AutonomyClass is stored; the resolver defaults to the synthetic
// supervised class whose ApprovalPolicy has ExternalCommunication: always.
func TestServerAgentCommunicationGate3PendingPersistsApprovalBeforeTargetDispatch(t *testing.T) {
	server, sessionIssuer, _, authorizer, calls, _ := delegationGatewayFixture(t, true)

	// Re-seed the source Agent with a non-zero ResourceVersion so the
	// resolver accepts the incarnation (Resolve requires UID != "" AND
	// ResourceVersion > 0). The fixture seeds ResourceVersion: 0.
	sourceAgent := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "Agent", APIVersion: v1alpha1.APIVersion},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "team-a", UID: "planner-uid", ResourceVersion: 2},
	}
	agentBytes, err := json.Marshal(sourceAgent)
	if err != nil {
		t.Fatal(err)
	}
	storePtr, ok := server.store.(*testStore)
	if !ok {
		t.Fatalf("fixture store is %T, want *testStore", server.store)
	}
	storePtr.data[resourceKey("Agent", "team-a", "planner")] = agentBytes

	// Re-seed the target AgentService with UID + ResourceVersion so the
	// resolved target populates the Gate3Request Target reference. Gate 3
	// binding validation requires Target.UID and Target.Name AND requires
	// Namespaced references to live in the RuntimeApproval workspace, so
	// we place the target in the same workspace as the source (team-a).
	targetService := v1alpha1.AgentService{
		TypeMeta: v1alpha1.TypeMeta{Kind: "AgentService", APIVersion: v1alpha1.APIVersion},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name: "peer-service", Namespace: "team-a",
			UID: "peer-service-uid", ResourceVersion: 3,
		},
		Spec:   v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}},
		Status: v1alpha1.AgentServiceStatus{Phase: v1alpha1.PhaseReady, ReadyInstances: 1, Endpoint: "http://untrusted.invalid/invoke"},
	}
	serviceBytes, err := json.Marshal(targetService)
	if err != nil {
		t.Fatal(err)
	}
	storePtr.data[resourceKey("AgentService", "team-a", "peer-service")] = serviceBytes

	// Re-seed the AgentIdentity with UID + ResourceVersion so the
	// Gate3Coordinator's binding step can build the exact AgentIdentityRef
	// (Bind requires kind/name/UID/positive RV).
	sourceIdentity := v1alpha1.AgentIdentity{
		TypeMeta: v1alpha1.TypeMeta{Kind: "AgentIdentity", APIVersion: v1alpha1.APIVersion},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name: "agent-identity-planner", Namespace: "team-a",
			UID: "agent-identity-planner-uid", ResourceVersion: 4,
			Labels:          map[string]string{"kognition.dev/auto-created-for-agent": "planner"},
			OwnerReferences: []v1alpha1.OwnerReference{{Kind: "Agent", Name: "planner", UID: "planner-uid"}},
		},
		Spec:   v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}, SPIFFEID: "spiffe://kognition.dev/workspace/team-a/agent/planner"},
		Status: v1alpha1.AgentIdentityStatus{Phase: v1alpha1.PhaseReady},
	}
	identityBytes, err := json.Marshal(sourceIdentity)
	if err != nil {
		t.Fatal(err)
	}
	storePtr.data[resourceKey("AgentIdentity", "team-a", "agent-identity-planner")] = identityBytes

	// Real Gate3Coordinator over an in-memory store and a real Resolver
	// backed by the fixture's testStore. No AutonomyClass is stored, so
	// the resolver falls back to the synthetic supervised class whose
	// ApprovalPolicy requires approval for every plane, which forces the
	// coordinator to return Pending.
	fakeStore := newGate3FakeStore()
	resolver := runtimeapproval.NewResolver(storePtr)
	keys, err := runtimeapproval.NewStaticBindingKeyStore([]runtimeapproval.BindingKey{{
		Version:   1,
		Epoch:     1,
		Algorithm: runtimeapproval.BindingAlgorithmV1,
		Domain:    runtimeapproval.BindingDomainV1,
		Status:    runtimeapproval.BindingKeyActive,
		Material:  []byte("0123456789abcdef0123456789abcdef"),
	}})
	if err != nil {
		t.Fatalf("NewStaticBindingKeyStore: %v", err)
	}
	coordinator, err := runtimeapproval.NewGate3Coordinator(runtimeapproval.Gate3Config{
		Store:    fakeStore,
		Resolver: resolver,
		Keys:     keys,
		TTL:      time.Minute,
	})
	if err != nil {
		t.Fatalf("NewGate3Coordinator: %v", err)
	}

	// Attach the Gate 3 coordinator and a request builder directly to the
	// server. Both are same-package unexported fields, so no
	// constructor-option round-trip is required for the test.
	server.gate3 = coordinator
	server.gate3Builder = func(_ context.Context, claims *runtimeauth.AgentDelegationSessionCapabilityClaims, identity *v1alpha1.AgentIdentity, invocationID, targetNamespace, targetService, _ string, targetUID string, targetResourceVersion int64, _ string) (runtimeapproval.Gate3Request, bool) {
		return runtimeapproval.Gate3Request{
			Workspace:    claims.Namespace,
			Namespace:    claims.Namespace,
			InvocationID: invocationID,
			Plane:        string(v1alpha1.RuntimeApprovalPlaneCommunication),
			Operation:    "issue-capability",
			Action:       runtimeapproval.ActionExternalCommunication,
			AgentName:    claims.AgentName,
			SessionID:    claims.SessionID,
			Target: v1alpha1.RuntimeApprovalReference{
				Kind:            "AgentService",
				Scope:           "Namespaced",
				Namespace:       targetNamespace,
				Name:            targetService,
				UID:             targetUID,
				ResourceVersion: targetResourceVersion,
			},
			AgentIdentity: *identity,
			AgentInstance: v1alpha1.AgentInstance{
				TypeMeta:   v1alpha1.TypeMeta{Kind: "AgentInstance", APIVersion: v1alpha1.APIVersion},
				ObjectMeta: v1alpha1.ObjectMeta{Namespace: claims.Namespace, Name: claims.InstanceName, UID: claims.InstanceUID, ResourceVersion: 1},
			},
			CanonicalPayload: json.RawMessage(`{"invocation_id":"delegation-1","target_agent_service":"peer-service","target_namespace":"team-a"}`),
		}, true
	}

	// Install an audit sink so we can assert the pending trust decision is
	// recorded before writePendingRuntimeApproval returns 202.
	auditStore := &communicationAuditStore{targetReached: func() bool { return *calls > 0 }}
	server.audit = observability.NewCognitiveAuditLog(auditStore)

	// Issue the source session capability used to request exact capability
	// issuance from the exchange endpoint.
	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.ServeHTTP(response, delegationRequest(t, "/v1/agent-services/capabilities", session, `{"target_namespace":"team-a","target_agent_service":"peer-service","invocation_id":"delegation-1","message":"review this"}`))

	// Contract: HTTP 202 pending tagged transport, target NOT contacted.
	if response.Code != 202 {
		t.Fatalf("status=%d, want 202 Accepted; body=%s", response.Code, response.Body.String())
	}
	if *calls != 0 {
		t.Fatalf("target AgentService contacted before approval: calls=%d", *calls)
	}
	if authorizer.sameCalls != 1 {
		t.Fatalf("authorization checks=%d, want exactly 1 policy check", authorizer.sameCalls)
	}

	var body delegationPendingResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode pending response: %v; raw=%s", err, response.Body.String())
	}
	if body.InvocationID != "delegation-1" {
		t.Fatalf("body.InvocationID=%q, want %q", body.InvocationID, "delegation-1")
	}
	if body.RuntimeApprovalState != string(runtimeapproval.Gate3StatePending) {
		t.Fatalf("body.RuntimeApprovalState=%q, want %q", body.RuntimeApprovalState, runtimeapproval.Gate3StatePending)
	}
	if body.RuntimeApproval == nil {
		t.Fatalf("body.RuntimeApproval is nil; want tagged reference")
	}
	if body.RuntimeApproval.UID != "approval-delegation-1-uid" {
		t.Fatalf("body.RuntimeApproval.UID=%q, want approval-delegation-1-uid", body.RuntimeApproval.UID)
	}
	if body.RuntimeApproval.Kind != "RuntimeApproval" || body.RuntimeApproval.Scope != "Namespaced" {
		t.Fatalf("body.RuntimeApproval reference not fully qualified: %#v", body.RuntimeApproval)
	}
	if body.RuntimeApproval.ResourceVersion == 0 {
		t.Fatalf("body.RuntimeApproval.ResourceVersion=0; want non-zero durable RV")
	}
	if body.RuntimeApprovalExpiresAt == nil || body.RuntimeApprovalExpiresAt.IsZero() {
		t.Fatalf("body.RuntimeApprovalExpiresAt is unset; want non-zero pending TTL")
	}

	// Contract: durable RuntimeApproval persisted with DecisionState=Pending
	// and the tagged reference points at the exact durable object.
	stored, ok := fakeStore.snapshot("delegation-1")
	if !ok {
		t.Fatalf("expected durable RuntimeApproval for delegation-1 to be persisted")
	}
	if stored.Status.DecisionState != v1alpha1.RuntimeApprovalDecisionPending {
		t.Fatalf("stored DecisionState=%q, want Pending", stored.Status.DecisionState)
	}
	if stored.Spec.Workspace != "team-a" || stored.Spec.InvocationID != "delegation-1" {
		t.Fatalf("stored approval binding wrong: %#v", stored.Spec)
	}
	if body.RuntimeApproval.Name != stored.Name || body.RuntimeApproval.Namespace != stored.Namespace ||
		body.RuntimeApproval.UID != stored.UID || body.RuntimeApproval.ResourceVersion != stored.ResourceVersion {
		t.Fatalf("tagged reference %#v does not match persisted approval %s/%s uid=%s rv=%d",
			body.RuntimeApproval, stored.Namespace, stored.Name, stored.UID, stored.ResourceVersion)
	}
	if fakeStore.creates != 1 {
		t.Fatalf("CreateOrGetRuntimeApproval called %d times, want 1", fakeStore.creates)
	}

	if len(auditStore.entries) != 1 {
		t.Fatalf("audit entries=%d, want one pending decision", len(auditStore.entries))
	}
	var pendingDecision observability.TrustDecisionV1
	if err := json.Unmarshal(auditStore.entries[0].Details, &pendingDecision); err != nil {
		t.Fatalf("decode pending audit entry: %v", err)
	}
	if pendingDecision.Plane != "communication" {
		t.Fatalf("pending decision plane=%q, want communication", pendingDecision.Plane)
	}
	if pendingDecision.Outcome != "pending" || pendingDecision.ReasonCode != "approval_required" {
		t.Fatalf("pending decision outcome=%q reason=%q, want pending/approval_required", pendingDecision.Outcome, pendingDecision.ReasonCode)
	}
	if pendingDecision.Execution.Status != "blocked" {
		t.Fatalf("pending decision execution.status=%q, want blocked", pendingDecision.Execution.Status)
	}
	if pendingDecision.Execution.TargetReached == nil || *pendingDecision.Execution.TargetReached {
		t.Fatalf("pending decision target_reached=%v, want false", pendingDecision.Execution.TargetReached)
	}
	if pendingDecision.Approval == nil {
		t.Fatalf("pending decision approval evidence missing")
	}
	if pendingDecision.Approval.DecisionRef == nil || pendingDecision.Approval.DecisionRef.UID != "approval-delegation-1-uid" {
		t.Fatalf("pending decision approval.decisionRef=%#v, want durable RuntimeApproval UID", pendingDecision.Approval.DecisionRef)
	}
}
