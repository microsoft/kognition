// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

type stubGate3Resolver struct {
	result  runtimeapproval.Gate3Result
	results []runtimeapproval.Gate3Result
	err     error
	calls   int
	lastReq runtimeapproval.Gate3Request
}

func (s *stubGate3Resolver) Resolve(_ context.Context, request runtimeapproval.Gate3Request) (runtimeapproval.Gate3Result, error) {
	s.calls++
	s.lastReq = request
	if len(s.results) > 0 {
		index := s.calls - 1
		if index >= len(s.results) {
			index = len(s.results) - 1
		}
		return s.results[index], s.err
	}
	return s.result, s.err
}

func gate3TestBuilder(sentinel string) Gate3RequestBuilder {
	return func(_ context.Context, req Request, tool *v1alpha1.Tool) (runtimeapproval.Gate3Request, bool) {
		return runtimeapproval.Gate3Request{
			Workspace:    tool.Namespace,
			Namespace:    tool.Namespace,
			InvocationID: req.InvocationID,
			Plane:        v1alpha1.RuntimeApprovalPlaneTool,
			Operation:    string(sentinel),
			Action:       "execute",
			AgentName:    req.SubjectName,
		}, true
	}
}

func gate3ReadyResult() runtimeapproval.Gate3Result {
	return runtimeapproval.Gate3Result{
		State: runtimeapproval.Gate3StateReady,
		Approval: v1alpha1.RuntimeApproval{
			ObjectMeta: v1alpha1.ObjectMeta{
				Name:            "invocation-1",
				Namespace:       "team-a",
				UID:             "uid-ready-1",
				ResourceVersion: 7,
			},
			Spec:   v1alpha1.RuntimeApprovalSpec{InvocationID: "invocation-1"},
			Status: v1alpha1.RuntimeApprovalStatus{DecisionState: v1alpha1.RuntimeApprovalDecisionNotRequired},
		},
		ExpiresAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

// gate3ApprovalRef returns the approval reference the Tool Gateway should
// stamp on the tagged response for the fixture returned by
// gate3ReadyResult(). Callers that mutate the underlying approval should
// build their own reference.
func gate3ReadyResultApproval() v1alpha1.RuntimeApproval {
	return gate3ReadyResult().Approval
}

func gate3PendingResult() runtimeapproval.Gate3Result {
	return runtimeapproval.Gate3Result{
		State: runtimeapproval.Gate3StatePending,
		Approval: v1alpha1.RuntimeApproval{
			ObjectMeta: v1alpha1.ObjectMeta{
				Name:            "invocation-1",
				Namespace:       "team-a",
				UID:             "uid-pending-1",
				ResourceVersion: 3,
			},
			Spec: v1alpha1.RuntimeApprovalSpec{InvocationID: "invocation-1"},
		},
		ExpiresAt: time.Date(2030, 6, 7, 8, 9, 10, 0, time.UTC),
	}
}

func TestGatewayGate3ReadyProceedsWithReadyLedgerAnnotation(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	resolver := &stubGate3Resolver{result: gate3ReadyResult()}
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)

	response, err := gateway.Invoke(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("invoke failed: %v", err)
	}
	if response.InvocationID != "invocation-1" || !response.Output["updated"].(bool) {
		t.Fatalf("unexpected response %#v", response)
	}
	// ADR-0014 tagged transport: successful Ready→Finalize execution surfaces
	// the terminal runtime-approval state and the resolved reference so the
	// caller can correlate the completion with the durable approval object.
	if response.RuntimeApprovalState != RuntimeApprovalStateTerminal {
		t.Fatalf("expected terminal runtime approval state, got %q", response.RuntimeApprovalState)
	}
	if response.RuntimeApproval == nil {
		t.Fatalf("expected runtime approval reference on tagged response")
	}
	expectedApproval := gate3ReadyResultApproval()
	if response.RuntimeApproval.Namespace != expectedApproval.ObjectMeta.Namespace || response.RuntimeApproval.Name != expectedApproval.ObjectMeta.Name || response.RuntimeApproval.UID != string(expectedApproval.ObjectMeta.UID) {
		t.Fatalf("unexpected runtime approval reference: %#v", response.RuntimeApproval)
	}
	if got, want := response.RuntimeApprovalExpiresAt, gate3ReadyResult().ExpiresAt; !got.Equal(want) {
		t.Fatalf("expected RuntimeApprovalExpiresAt=%v, got %v", want, got)
	}
	if resolver.calls != 1 {
		t.Fatalf("expected 1 gate3 call, got %d", resolver.calls)
	}
	if executor.calls != 1 || quota.calls != 1 || authorizer.calls != 1 {
		t.Fatalf("downstream not invoked: executor=%d quota=%d authorizer=%d", executor.calls, quota.calls, authorizer.calls)
	}
	if len(ledger.starts) != 1 || len(ledger.completions) != 1 {
		t.Fatalf("expected 1 start and 1 completion, got starts=%d completions=%d", len(ledger.starts), len(ledger.completions))
	}
	ledger.mu.Lock()
	decisions := append([]LedgerEntry(nil), ledger.decisions...)
	ledger.mu.Unlock()
	if len(decisions) != 1 {
		t.Fatalf("expected 1 gate3 ready decision, got %d", len(decisions))
	}
	if decisions[0].Status != "runtime_approval_ready" || decisions[0].ReasonCode != "runtime_approval_not_required" {
		t.Fatalf("unexpected gate3 ready decision entry: %#v", decisions[0])
	}
}

// Per ADR-0014 step ordering (Gate 2 AuthZ before Gate 3 Resolve), Gate 3
// halts execution only after authorization has already run. It must still
// short-circuit before quota is reserved or the executor is contacted.
func TestGatewayGate3PendingHaltsBeforeQuota(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{}}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	pendingResult := gate3PendingResult()
	resolver := &stubGate3Resolver{result: pendingResult}
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)

	response, err := gateway.Invoke(context.Background(), testRequest())
	if !errors.Is(err, ErrRuntimeApprovalPending) {
		t.Fatalf("expected ErrRuntimeApprovalPending, got %v", err)
	}
	// ADR-0014 tagged transport: pending state must be surfaced to the caller
	// alongside the approval reference so the client can poll/retry against
	// the same durable RuntimeApproval object.
	if response.RuntimeApprovalState != RuntimeApprovalStatePending {
		t.Fatalf("expected pending runtime approval state, got %q", response.RuntimeApprovalState)
	}
	if response.RuntimeApproval == nil || response.RuntimeApproval.Namespace != pendingResult.Approval.ObjectMeta.Namespace || response.RuntimeApproval.Name != pendingResult.Approval.ObjectMeta.Name {
		t.Fatalf("expected runtime approval reference on pending response, got %#v", response.RuntimeApproval)
	}
	if !response.RuntimeApprovalExpiresAt.Equal(pendingResult.ExpiresAt) {
		t.Fatalf("expected pending expiry %v, got %v", pendingResult.ExpiresAt, response.RuntimeApprovalExpiresAt)
	}
	if authorizer.calls != 1 {
		t.Fatalf("authorizer must run before gate3: calls=%d", authorizer.calls)
	}
	if quota.calls != 0 || executor.calls != 0 {
		t.Fatalf("downstream invoked: quota=%d executor=%d", quota.calls, executor.calls)
	}
	if len(ledger.starts) != 0 || len(ledger.completions) != 0 {
		t.Fatalf("execution ledger writes should not occur on pending: starts=%d completions=%d", len(ledger.starts), len(ledger.completions))
	}
	ledger.mu.Lock()
	decisions := append([]LedgerEntry(nil), ledger.decisions...)
	ledger.mu.Unlock()
	if len(decisions) != 1 || decisions[0].Status != "runtime_approval_pending" || decisions[0].ReasonCode != "runtime_approval_pending" {
		t.Fatalf("unexpected pending decision entry: %#v", decisions)
	}
}

func TestGatewayGate3PendingResumesApprovedInvocationExactlyOnce(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	resolver := &stubGate3Resolver{results: []runtimeapproval.Gate3Result{gate3PendingResult(), gate3ReadyResult()}}
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)

	request := testRequest()
	request.InvocationID = "invocation-1"
	if _, err := gateway.Invoke(context.Background(), request); !errors.Is(err, ErrRuntimeApprovalPending) {
		t.Fatalf("expected first invocation to await approval, got %v", err)
	}
	approvedResponse, err := gateway.Invoke(context.Background(), request)
	if err != nil {
		t.Fatalf("approved retry failed: %v", err)
	}
	if !approvedResponse.Output["updated"].(bool) {
		t.Fatalf("unexpected approved response: %#v", approvedResponse)
	}
	replayedResponse, err := gateway.Invoke(context.Background(), request)
	if err != nil || !replayedResponse.Output["updated"].(bool) {
		t.Fatalf("expected stored success replay, response=%#v err=%v", replayedResponse, err)
	}
	if replayedResponse.RuntimeApprovalState != RuntimeApprovalStateTerminal || replayedResponse.RuntimeApproval == nil || !replayedResponse.RuntimeApprovalExpiresAt.Equal(gate3ReadyResult().ExpiresAt) {
		t.Fatalf("stored success replay lost terminal Gate3 tags: %#v", replayedResponse)
	}
	changed := request
	changed.Input = map[string]any{"ticket": "43"}
	if _, err := gateway.Invoke(context.Background(), changed); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected changed input conflict, got %v", err)
	}
	if resolver.calls != 3 || authorizer.calls != 3 {
		t.Fatalf("expected pending, approved, and terminal replay reevaluation, resolver=%d authorizer=%d", resolver.calls, authorizer.calls)
	}
	if quota.calls != 1 || executor.calls != 1 {
		t.Fatalf("expected exactly one effect after approval, quota=%d executor=%d", quota.calls, executor.calls)
	}
}

func TestGatewayGate3RejectedReturnsSentinel(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{}}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	rejected := runtimeapproval.Gate3Result{
		State:     runtimeapproval.Gate3StateRejected,
		Approval:  v1alpha1.RuntimeApproval{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "invocation-1", UID: "uid-rej-1", ResourceVersion: 4}},
		ExpiresAt: time.Date(2030, 2, 3, 0, 0, 0, 0, time.UTC),
	}
	resolver := &stubGate3Resolver{result: rejected}
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)

	response, err := gateway.Invoke(context.Background(), testRequest())
	if !errors.Is(err, ErrRuntimeApprovalRejected) {
		t.Fatalf("expected ErrRuntimeApprovalRejected, got %v", err)
	}
	if response.RuntimeApprovalState != RuntimeApprovalStateTerminal {
		t.Fatalf("expected terminal runtime approval state on rejection, got %q", response.RuntimeApprovalState)
	}
	if response.RuntimeApproval == nil || response.RuntimeApproval.Name != "invocation-1" {
		t.Fatalf("expected runtime approval reference on rejected response, got %#v", response.RuntimeApproval)
	}
	if !response.RuntimeApprovalExpiresAt.Equal(rejected.ExpiresAt) {
		t.Fatalf("expected expiry %v, got %v", rejected.ExpiresAt, response.RuntimeApprovalExpiresAt)
	}
	if authorizer.calls != 1 {
		t.Fatalf("authorizer must run before gate3: calls=%d", authorizer.calls)
	}
	if quota.calls != 0 || executor.calls != 0 {
		t.Fatalf("downstream invoked: quota=%d executor=%d", quota.calls, executor.calls)
	}
	ledger.mu.Lock()
	decisions := append([]LedgerEntry(nil), ledger.decisions...)
	ledger.mu.Unlock()
	if len(decisions) != 1 || decisions[0].Status != "denied" || decisions[0].ReasonCode != "runtime_approval_rejected" {
		t.Fatalf("unexpected rejected decision entry: %#v", decisions)
	}
}

func TestGatewayGate3ExpiredReturnsSentinel(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{}}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	expired := runtimeapproval.Gate3Result{
		State:     runtimeapproval.Gate3StateExpired,
		Approval:  v1alpha1.RuntimeApproval{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "invocation-1", UID: "uid-exp-1", ResourceVersion: 5}},
		ExpiresAt: time.Date(2029, 12, 31, 23, 59, 59, 0, time.UTC),
	}
	resolver := &stubGate3Resolver{result: expired}
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)

	response, err := gateway.Invoke(context.Background(), testRequest())
	if !errors.Is(err, ErrRuntimeApprovalExpired) {
		t.Fatalf("expected ErrRuntimeApprovalExpired, got %v", err)
	}
	if response.RuntimeApprovalState != RuntimeApprovalStateTerminal {
		t.Fatalf("expected terminal runtime approval state on expiry, got %q", response.RuntimeApprovalState)
	}
	if response.RuntimeApproval == nil || response.RuntimeApproval.UID != "uid-exp-1" {
		t.Fatalf("expected runtime approval reference on expired response, got %#v", response.RuntimeApproval)
	}
	if !response.RuntimeApprovalExpiresAt.Equal(expired.ExpiresAt) {
		t.Fatalf("expected expiry %v, got %v", expired.ExpiresAt, response.RuntimeApprovalExpiresAt)
	}
	if authorizer.calls != 1 {
		t.Fatalf("authorizer must run before gate3: calls=%d", authorizer.calls)
	}
	if quota.calls != 0 || executor.calls != 0 {
		t.Fatalf("downstream invoked: quota=%d executor=%d", quota.calls, executor.calls)
	}
	ledger.mu.Lock()
	decisions := append([]LedgerEntry(nil), ledger.decisions...)
	ledger.mu.Unlock()
	if len(decisions) != 1 || decisions[0].Status != "denied" || decisions[0].ReasonCode != "runtime_approval_expired" {
		t.Fatalf("unexpected expired decision entry: %#v", decisions)
	}
}

func TestGatewayGate3BuilderOptOutSkipsResolution(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	resolver := &stubGate3Resolver{result: runtimeapproval.Gate3Result{State: runtimeapproval.Gate3StatePending}}
	optOut := Gate3RequestBuilder(func(context.Context, Request, *v1alpha1.Tool) (runtimeapproval.Gate3Request, bool) {
		return runtimeapproval.Gate3Request{}, false
	})
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(optOut)(gateway)

	response, err := gateway.Invoke(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("expected invocation to proceed, got %v", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver should not be called when builder opts out, calls=%d", resolver.calls)
	}
	if executor.calls != 1 {
		t.Fatalf("executor should run, calls=%d", executor.calls)
	}
	// ADR-0014 tagged transport: when Gate 3 opts out the response must not
	// carry a runtime approval state so clients see an unannotated success.
	if response.RuntimeApprovalState != "" {
		t.Fatalf("expected empty runtime approval state on builder opt-out, got %q", response.RuntimeApprovalState)
	}
	if response.RuntimeApproval != nil {
		t.Fatalf("expected no runtime approval reference on builder opt-out, got %#v", response.RuntimeApproval)
	}
}

func TestGatewayGate3ResolveErrorFailsClosed(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{}}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	boom := errors.New("resolver down")
	resolver := &stubGate3Resolver{err: boom}
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)

	response, err := gateway.Invoke(context.Background(), testRequest())
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("expected wrapped resolver error, got %v", err)
	}
	// ADR-0014 tagged transport: resolver failures fail closed and surface the
	// unavailable state to the caller so they do not treat the outcome as a
	// missing gate.
	if response.RuntimeApprovalState != RuntimeApprovalStateUnavailable {
		t.Fatalf("expected unavailable runtime approval state on resolve error, got %q", response.RuntimeApprovalState)
	}
	if authorizer.calls != 1 {
		t.Fatalf("authorizer must run before gate3: calls=%d", authorizer.calls)
	}
	if quota.calls != 0 || executor.calls != 0 {
		t.Fatalf("downstream invoked: quota=%d executor=%d", quota.calls, executor.calls)
	}
	ledger.mu.Lock()
	decisions := append([]LedgerEntry(nil), ledger.decisions...)
	ledger.mu.Unlock()
	if len(decisions) != 1 || decisions[0].ReasonCode != "runtime_approval_unavailable" {
		t.Fatalf("unexpected gate3 unavailable decision: %#v", decisions)
	}
	if decisions[0].Approval == nil || decisions[0].Approval.Status != "unavailable" || decisions[0].Approval.DecisionRef != nil {
		t.Fatalf("resolver failure must retain unavailable approval evidence: %#v", decisions[0].Approval)
	}
}

// TestGatewayGate3NotResolvedWhenAuthorizationDenied confirms ADR-0014 step
// ordering: Gate 3 must not be resolved (and therefore no RuntimeApproval
// side effect can occur) when Gate 2 AuthZ denies the request.
func TestGatewayGate3NotResolvedWhenAuthorizationDenied(t *testing.T) {
	authorizer := &gatewayAuthorizer{err: errors.New("grant missing")}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{}}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	resolver := &stubGate3Resolver{result: runtimeapproval.Gate3Result{State: runtimeapproval.Gate3StatePending}}
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)

	_, err := gateway.Invoke(context.Background(), testRequest())
	if err == nil {
		t.Fatalf("expected authorization error, got nil")
	}
	if authorizer.calls != 1 {
		t.Fatalf("authorizer must run: calls=%d", authorizer.calls)
	}
	if resolver.calls != 0 {
		t.Fatalf("gate3 must not resolve after authorization denial: calls=%d", resolver.calls)
	}
	if quota.calls != 0 || executor.calls != 0 {
		t.Fatalf("downstream invoked after authorization denial: quota=%d executor=%d", quota.calls, executor.calls)
	}
	if len(ledger.decisions) != 1 || ledger.decisions[0].Approval == nil || ledger.decisions[0].Approval.Status != "not_evaluated" || ledger.decisions[0].Approval.DecisionRef != nil {
		t.Fatalf("approval must be unevaluated after authorization denial: %#v", ledger.decisions)
	}
}

// fakeRuntimeApprovalExecutionStore records calls to the execution store
// contract and returns approvals with progressively advanced resource
// versions so tests can assert Prepare → Claim → Finalize ordering.
type fakeRuntimeApprovalExecutionStore struct {
	prepareCalls  []prepareCall
	claimCalls    []claimCall
	finalizeCalls []finalizeCall
	abortCalls    []abortCall

	prepareErr  error
	claimErr    error
	finalizeErr error
	abortErr    error

	fencingToken int64
	current      v1alpha1.RuntimeApproval
	getErr       error
}

type prepareCall struct {
	namespace    string
	invocationID string
	expectedRV   int64
	token        string
	deadline     time.Time
}

type claimCall struct {
	namespace    string
	invocationID string
	expectedRV   int64
	token        string
	claimant     v1alpha1.RuntimeApprovalClaimant
}

type abortCall struct {
	namespace    string
	invocationID string
	expectedRV   int64
	token        string
	reason       string
}

type finalizeCall struct {
	namespace    string
	invocationID string
	expectedRV   int64
	fencingToken int64
	disposition  string
}

func (f *fakeRuntimeApprovalExecutionStore) PrepareRuntimeApproval(_ context.Context, ns, invID string, expectedRV int64, token string, deadline time.Time) (v1alpha1.RuntimeApproval, error) {
	f.prepareCalls = append(f.prepareCalls, prepareCall{ns, invID, expectedRV, token, deadline})
	if f.prepareErr != nil {
		return v1alpha1.RuntimeApproval{}, f.prepareErr
	}
	return v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Name: invID, Namespace: ns, ResourceVersion: expectedRV + 1},
		Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: invID},
	}, nil
}

func (f *fakeRuntimeApprovalExecutionStore) ClaimRuntimeApprovalExecution(_ context.Context, ns, invID string, expectedRV int64, token string, claimant v1alpha1.RuntimeApprovalClaimant) (v1alpha1.RuntimeApproval, error) {
	f.claimCalls = append(f.claimCalls, claimCall{ns, invID, expectedRV, token, claimant})
	if f.claimErr != nil {
		return v1alpha1.RuntimeApproval{}, f.claimErr
	}
	if f.fencingToken == 0 {
		f.fencingToken = 1
	}
	f.current = v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Name: invID, Namespace: ns, ResourceVersion: expectedRV + 1},
		Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: invID},
		Status: v1alpha1.RuntimeApprovalStatus{
			ExecutionState: v1alpha1.RuntimeApprovalExecutionClaimed,
			FencingToken:   f.fencingToken,
			Claimant:       &claimant,
		},
	}
	return f.current, nil
}

func (f *fakeRuntimeApprovalExecutionStore) FinalizeRuntimeApprovalExecution(_ context.Context, ns, invID string, expectedRV, fencingToken int64, disposition string) (v1alpha1.RuntimeApproval, error) {
	f.finalizeCalls = append(f.finalizeCalls, finalizeCall{ns, invID, expectedRV, fencingToken, disposition})
	if f.finalizeErr != nil {
		return v1alpha1.RuntimeApproval{}, f.finalizeErr
	}
	return v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Name: invID, Namespace: ns, ResourceVersion: expectedRV + 1},
		Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: invID},
	}, nil
}

// Remaining methods are unused by these tests and return not-implemented.
func (f *fakeRuntimeApprovalExecutionStore) CreateOrGetRuntimeApproval(context.Context, v1alpha1.RuntimeApproval, json.RawMessage, time.Duration) (bool, v1alpha1.RuntimeApproval, error) {
	return false, v1alpha1.RuntimeApproval{}, errors.New("not implemented")
}

func (f *fakeRuntimeApprovalExecutionStore) GetRuntimeApprovalByInvocation(context.Context, string, string) (v1alpha1.RuntimeApproval, error) {
	if f.getErr != nil {
		return v1alpha1.RuntimeApproval{}, f.getErr
	}
	return f.current, nil
}

func (f *fakeRuntimeApprovalExecutionStore) DecideRuntimeApproval(context.Context, string, string, store.RuntimeApprovalDecision) (v1alpha1.RuntimeApproval, error) {
	return v1alpha1.RuntimeApproval{}, errors.New("not implemented")
}

func (f *fakeRuntimeApprovalExecutionStore) ExpireRuntimeApprovalDecision(context.Context, string, string, int64) (v1alpha1.RuntimeApproval, error) {
	return v1alpha1.RuntimeApproval{}, errors.New("not implemented")
}

func (f *fakeRuntimeApprovalExecutionStore) AcknowledgeRuntimeApprovalDecisionAudit(context.Context, string, string, int64, string) (v1alpha1.RuntimeApproval, error) {
	return v1alpha1.RuntimeApproval{}, errors.New("not implemented")
}

func (f *fakeRuntimeApprovalExecutionStore) AbortRuntimeApprovalBeforeDispatch(_ context.Context, ns, invID string, expectedRV int64, token, reason string) (v1alpha1.RuntimeApproval, error) {
	f.abortCalls = append(f.abortCalls, abortCall{ns, invID, expectedRV, token, reason})
	if f.abortErr != nil {
		return v1alpha1.RuntimeApproval{}, f.abortErr
	}
	return v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Name: invID, Namespace: ns, ResourceVersion: expectedRV + 1},
		Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: invID},
	}, nil
}

func (f *fakeRuntimeApprovalExecutionStore) MarkRuntimeApprovalClaimUncertain(context.Context, string, string, int64, int64, string) (v1alpha1.RuntimeApproval, error) {
	return v1alpha1.RuntimeApproval{}, errors.New("not implemented")
}

func (f *fakeRuntimeApprovalExecutionStore) ReconcileRuntimeApprovalExecution(context.Context, string, string, int64, string, string, string, string) (v1alpha1.RuntimeApproval, error) {
	return v1alpha1.RuntimeApproval{}, errors.New("not implemented")
}

func (f *fakeRuntimeApprovalExecutionStore) ReapRuntimeApprovalPreparation(context.Context, string, string, int64, string) (v1alpha1.RuntimeApproval, error) {
	return v1alpha1.RuntimeApproval{}, errors.New("not implemented")
}

// TestGatewayGate3ReadyPreparesAndClaimsBeforeExecute asserts ADR-0014
// Prepare→Claim ordering: Prepare is called with the resolved approval's
// resource version, then Claim is called with the resource version returned
// by Prepare and the same preparation token.
func TestGatewayGate3ReadyPreparesAndClaimsBeforeExecute(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	execStore := &fakeRuntimeApprovalExecutionStore{}
	var storedGateway *gatewayStore
	executor := &gatewayExecutor{output: map[string]any{"updated": true}, before: func() {
		if len(execStore.claimCalls) != 1 {
			t.Fatalf("target contacted before successful Claim: calls=%d", len(execStore.claimCalls))
		}
		if len(storedGateway.settledReservations) != 0 {
			t.Fatalf("reservation settled before target return: %#v", storedGateway.settledReservations)
		}
	}}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)
	storedGateway = gateway.store.(*gatewayStore)
	resolver := &stubGate3Resolver{result: gate3ReadyResult()}
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	if _, err := gateway.Invoke(context.Background(), testRequest()); err != nil {
		t.Fatalf("invoke failed: %v", err)
	}

	if len(execStore.prepareCalls) != 1 {
		t.Fatalf("expected 1 Prepare call, got %d", len(execStore.prepareCalls))
	}
	prep := execStore.prepareCalls[0]
	if prep.namespace != "team-a" || prep.invocationID != "invocation-1" {
		t.Fatalf("unexpected Prepare target: %+v", prep)
	}
	if prep.expectedRV != 7 {
		t.Fatalf("Prepare must use resolved approval RV=7, got %d", prep.expectedRV)
	}
	if prep.token == "" {
		t.Fatalf("Prepare must supply a preparation token")
	}

	if len(execStore.claimCalls) != 1 {
		t.Fatalf("expected 1 Claim call, got %d", len(execStore.claimCalls))
	}
	claim := execStore.claimCalls[0]
	if claim.expectedRV != prep.expectedRV+1 {
		t.Fatalf("Claim must use RV returned by Prepare (%d), got %d", prep.expectedRV+1, claim.expectedRV)
	}
	if claim.token != prep.token {
		t.Fatalf("Claim must reuse preparation token %q, got %q", prep.token, claim.token)
	}
	if executor.calls != 1 {
		t.Fatalf("executor must run after Claim, calls=%d", executor.calls)
	}
}

func TestGatewayGate3ClaimUsesInjectedWorkloadUID(t *testing.T) {
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_WORKLOAD_UID", "pod-uid-0123456789")
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway := testGateway(t, authorizer, &gatewayQuota{}, executor, &gatewayLedger{}, 7)
	execStore := &fakeRuntimeApprovalExecutionStore{}
	WithGate3Coordinator(&stubGate3Resolver{result: gate3ReadyResult()})(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)
	if err := gateway.ensureClaimantDefaults(); err != nil {
		t.Fatalf("default claimant: %v", err)
	}

	if _, err := gateway.Invoke(context.Background(), testRequest()); err != nil {
		t.Fatalf("invoke failed: %v", err)
	}

	if len(execStore.claimCalls) != 1 {
		t.Fatalf("claim calls = %d, want 1", len(execStore.claimCalls))
	}
	if got := execStore.claimCalls[0].claimant.WorkloadUID; got != "pod-uid-0123456789" {
		t.Fatalf("claim workload UID = %q, want injected Pod UID", got)
	}
}

func TestGatewayGate3ReadyPersistsSharedPreparationBeforeClaim(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	resolver := &stubGate3Resolver{result: gate3ReadyResult()}
	execStore := &fakeRuntimeApprovalExecutionStore{}
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	token := "preparation-token-00000000000001"
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)
	WithClock(func() time.Time { return now })(gateway)
	WithPreparationTokenFactory(func() (string, error) { return token, nil })(gateway)

	if _, err := gateway.Invoke(context.Background(), testRequest()); err != nil {
		t.Fatalf("invoke failed: %v", err)
	}

	gatewayStore := gateway.store.(*gatewayStore)
	if len(gatewayStore.preparedReservations) != 1 {
		t.Fatalf("prepared reservations = %d, want 1", len(gatewayStore.preparedReservations))
	}
	if len(gatewayStore.preparedReceipts) != 1 {
		t.Fatalf("prepared receipts = %d, want 1", len(gatewayStore.preparedReceipts))
	}
	reservation := gatewayStore.preparedReservations[0]
	preparedReceipt := gatewayStore.preparedReceipts[0]
	if reservation.PreparationToken != token || preparedReceipt.PreparationToken != token || execStore.prepareCalls[0].token != token || execStore.claimCalls[0].token != token {
		t.Fatalf("preparation token was not shared: reservation=%q receipt=%q prepare=%q claim=%q", reservation.PreparationToken, preparedReceipt.PreparationToken, execStore.prepareCalls[0].token, execStore.claimCalls[0].token)
	}
	wantDeadline := now.Add(runtimeapproval.MaxPreparationDeadline)
	if !reservation.PreparationDeadline.Equal(wantDeadline) || preparedReceipt.PreparationDeadline == nil || !preparedReceipt.PreparationDeadline.Equal(wantDeadline) || !execStore.prepareCalls[0].deadline.Equal(wantDeadline) {
		t.Fatalf("preparation deadline was not shared: reservation=%v receipt=%v approval=%v", reservation.PreparationDeadline, preparedReceipt.PreparationDeadline, execStore.prepareCalls[0].deadline)
	}
	if len(gatewayStore.settledReservations) != 1 || gatewayStore.settledReservations[0].PreparationToken != token {
		t.Fatalf("settled reservations = %#v, want one settlement with shared token", gatewayStore.settledReservations)
	}
	if len(gatewayStore.releasedReservations) != 0 || len(gatewayStore.cleanupCalls) != 0 {
		t.Fatalf("claimed preparation must not be released: reservations=%#v cleanup=%v", gatewayStore.releasedReservations, gatewayStore.cleanupCalls)
	}
	completed := gatewayStore.receipts[reservation.Namespace+"/"+reservation.InvocationID]
	if completed.Status != store.ToolInvocationSucceeded {
		t.Fatalf("receipt status = %q, want %q", completed.Status, store.ToolInvocationSucceeded)
	}
}

func TestGatewayGate3PreparedRetryReusesExactPreparationIdentity(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway := testGateway(t, authorizer, &gatewayQuota{}, executor, &gatewayLedger{}, 7)
	gatewayStore := gateway.store.(*gatewayStore)
	deadline := time.Date(2026, time.September, 5, 12, 5, 0, 0, time.UTC)
	token := "preparation-token-00000000000001"
	request := testRequest()
	request.InvocationID = "invocation-1"
	receipt := gateway.newReceipt(request, &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: "team-a", ResourceVersion: 7}})
	receipt.Status = store.ToolInvocationPrepared
	receipt.PreparationToken = token
	receipt.PreparationDeadline = &deadline
	gatewayStore.receipts = map[string]store.ToolInvocationReceipt{"team-a/invocation-1": receipt}
	ready := gate3ReadyResult()
	ready.Approval.Status.ExecutionState = v1alpha1.RuntimeApprovalExecutionNotStarted
	ready.Approval.Status.PreparationToken = token
	ready.Approval.Status.PreparationDeadline = &deadline
	execStore := &fakeRuntimeApprovalExecutionStore{}
	WithGate3Coordinator(&stubGate3Resolver{result: ready})(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)
	WithPreparationTokenFactory(func() (string, error) { return "must-not-be-generated", nil })(gateway)

	if _, err := gateway.Invoke(context.Background(), request); err != nil {
		t.Fatalf("prepared retry failed: %v", err)
	}
	if len(execStore.prepareCalls) != 0 {
		t.Fatalf("prepared retry called PrepareRuntimeApproval %d times", len(execStore.prepareCalls))
	}
	if len(execStore.claimCalls) != 1 || execStore.claimCalls[0].token != token {
		t.Fatalf("claim calls = %#v, want exact token %q", execStore.claimCalls, token)
	}
	if len(gatewayStore.preparedReservations) != 1 || gatewayStore.preparedReservations[0].PreparationToken != token || !gatewayStore.preparedReservations[0].PreparationDeadline.Equal(deadline) {
		t.Fatalf("reservation did not resume exact preparation: %#v", gatewayStore.preparedReservations)
	}
	if executor.calls != 1 {
		t.Fatalf("target calls = %d, want 1", executor.calls)
	}
}

func TestGatewayGate3PendingReceiptResumesRuntimeApprovalPreparation(t *testing.T) {
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway := testGateway(t, &gatewayAuthorizer{}, &gatewayQuota{}, executor, &gatewayLedger{}, 7)
	gatewayStore := gateway.store.(*gatewayStore)
	deadline := time.Date(2026, time.September, 5, 12, 5, 0, 0, time.UTC)
	token := "preparation-token-00000000000001"
	first := gate3ReadyResult()
	second := gate3ReadyResult()
	second.Approval.ObjectMeta.ResourceVersion = 8
	second.Approval.Status.ExecutionState = v1alpha1.RuntimeApprovalExecutionNotStarted
	second.Approval.Status.PreparationToken = token
	second.Approval.Status.PreparationDeadline = &deadline
	execStore := &fakeRuntimeApprovalExecutionStore{}
	WithGate3Coordinator(&stubGate3Resolver{results: []runtimeapproval.Gate3Result{first, second}})(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)
	WithPreparationTokenFactory(func() (string, error) { return token, nil })(gateway)
	gatewayStore.prepareReservationErr = errors.New("reservation unavailable")

	request := testRequest()
	if _, err := gateway.Invoke(context.Background(), request); err == nil {
		t.Fatal("first reservation preparation failure was accepted")
	}
	gatewayStore.prepareReservationErr = nil
	if _, err := gateway.Invoke(context.Background(), request); err != nil {
		t.Fatalf("prepared RuntimeApproval retry failed: %v", err)
	}
	if len(execStore.prepareCalls) != 1 {
		t.Fatalf("PrepareRuntimeApproval calls = %d, want original call only", len(execStore.prepareCalls))
	}
	if len(execStore.claimCalls) != 1 || execStore.claimCalls[0].token != token {
		t.Fatalf("claim calls = %#v, want resumed token %q", execStore.claimCalls, token)
	}
	if executor.calls != 1 {
		t.Fatalf("target calls = %d, want 1", executor.calls)
	}
}

func TestGatewayGate3OwnershipVerificationFailureBlocksTarget(t *testing.T) {
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway := testGateway(t, &gatewayAuthorizer{}, &gatewayQuota{}, executor, &gatewayLedger{}, 7)
	execStore := &fakeRuntimeApprovalExecutionStore{getErr: errors.New("claim lookup unavailable")}
	WithGate3Coordinator(&stubGate3Resolver{result: gate3ReadyResult()})(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	response, err := gateway.Invoke(context.Background(), testRequest())
	if err == nil || response.RuntimeApprovalState != RuntimeApprovalStateUnavailable {
		t.Fatalf("response=%#v error=%v, want unavailable ownership check", response, err)
	}
	if executor.calls != 0 || len(execStore.finalizeCalls) != 0 {
		t.Fatalf("ownership verification failure reached target: target=%d finalize=%d", executor.calls, len(execStore.finalizeCalls))
	}
}

func TestGatewayGate3PreparedRetryClaimFailureRetainsExactOwner(t *testing.T) {
	executor := &gatewayExecutor{}
	gateway := testGateway(t, &gatewayAuthorizer{}, &gatewayQuota{}, executor, &gatewayLedger{}, 7)
	gatewayStore := gateway.store.(*gatewayStore)
	deadline := time.Date(2026, time.September, 5, 12, 5, 0, 0, time.UTC)
	token := "preparation-token-00000000000001"
	request := testRequest()
	request.InvocationID = "invocation-1"
	receipt := gateway.newReceipt(request, &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: "team-a", ResourceVersion: 7}})
	receipt.Status = store.ToolInvocationPrepared
	receipt.PreparationToken = token
	receipt.PreparationDeadline = &deadline
	gatewayStore.receipts = map[string]store.ToolInvocationReceipt{"team-a/invocation-1": receipt}
	ready := gate3ReadyResult()
	ready.Approval.Status.ExecutionState = v1alpha1.RuntimeApprovalExecutionNotStarted
	ready.Approval.Status.PreparationToken = token
	ready.Approval.Status.PreparationDeadline = &deadline
	execStore := &fakeRuntimeApprovalExecutionStore{claimErr: errors.New("claim outcome unavailable")}
	WithGate3Coordinator(&stubGate3Resolver{result: ready})(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	if _, err := gateway.Invoke(context.Background(), request); err == nil {
		t.Fatal("prepared retry Claim failure was accepted")
	}
	stored := gatewayStore.receipts["team-a/invocation-1"]
	if stored.Status != store.ToolInvocationPrepared || stored.PreparationToken != token || stored.PreparationDeadline == nil || !stored.PreparationDeadline.Equal(deadline) {
		t.Fatalf("prepared owner changed: %#v", stored)
	}
	if len(execStore.abortCalls) != 0 || len(gatewayStore.cleanupCalls) != 0 || executor.calls != 0 {
		t.Fatalf("ambiguous resumed Claim changed ownership: abort=%d cleanup=%v target=%d", len(execStore.abortCalls), gatewayStore.cleanupCalls, executor.calls)
	}
}

// TestGatewayGate3ReadyFinalizesSucceededOnExecutorSuccess asserts that
// after successful execution the gateway calls Finalize with the fencing
// token stamped by Claim and disposition=Succeeded.
func TestGatewayGate3ReadyFinalizesSucceededOnExecutorSuccess(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	resolver := &stubGate3Resolver{result: gate3ReadyResult()}
	execStore := &fakeRuntimeApprovalExecutionStore{fencingToken: 42}
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	if _, err := gateway.Invoke(context.Background(), testRequest()); err != nil {
		t.Fatalf("invoke failed: %v", err)
	}

	if len(execStore.finalizeCalls) != 1 {
		t.Fatalf("expected 1 Finalize call, got %d", len(execStore.finalizeCalls))
	}
	fin := execStore.finalizeCalls[0]
	if fin.disposition != runtimeapproval.ExecutionDispositionSucceeded {
		t.Fatalf("expected disposition=%q, got %q", runtimeapproval.ExecutionDispositionSucceeded, fin.disposition)
	}
	if fin.fencingToken != 42 {
		t.Fatalf("Finalize must use fencing token from Claim (42), got %d", fin.fencingToken)
	}
	if fin.expectedRV != 9 {
		// Prepare returned RV=8; Claim returned RV=9. Finalize must reference the claimed RV.
		t.Fatalf("Finalize must use claim RV=9, got %d", fin.expectedRV)
	}
}

// TestGatewayGate3ReadyFinalizesFailedOnExecutorFailure asserts that after
// executor error the gateway still calls Finalize but with
// disposition=Failed so the durable ledger records the terminal outcome.
func TestGatewayGate3ReadyFinalizesFailedOnExecutorFailure(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	execErr := errors.New("upstream boom")
	executor := &gatewayExecutor{err: execErr}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	resolver := &stubGate3Resolver{result: gate3ReadyResult()}
	execStore := &fakeRuntimeApprovalExecutionStore{}
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	request := testRequest()
	if _, err := gateway.Invoke(context.Background(), request); !errors.Is(err, execErr) {
		t.Fatalf("expected wrapped executor error, got %v", err)
	}

	if len(execStore.finalizeCalls) != 1 {
		t.Fatalf("expected 1 Finalize call, got %d", len(execStore.finalizeCalls))
	}
	if got := execStore.finalizeCalls[0].disposition; got != runtimeapproval.ExecutionDispositionFailed {
		t.Fatalf("expected disposition=%q, got %q", runtimeapproval.ExecutionDispositionFailed, got)
	}
	if len(gateway.store.(*gatewayStore).settledReservations) != 1 {
		t.Fatalf("target error must settle reservation after return")
	}
	if _, retryErr := gateway.Invoke(context.Background(), request); retryErr == nil || !strings.Contains(retryErr.Error(), "previous tool invocation failed") {
		t.Fatalf("retry error = %v, want stored target failure", retryErr)
	}
	if executor.calls != 1 {
		t.Fatalf("retry duplicated target effect: calls=%d", executor.calls)
	}
}

func TestGatewayGate3PreparationFailuresPreserveDurableOwner(t *testing.T) {
	tests := []struct {
		name              string
		configure         func(*gatewayStore, *fakeRuntimeApprovalExecutionStore, *gatewayLedger)
		wantReceiptStatus store.ToolInvocationReceiptStatus
		wantPrepared      bool
	}{
		{
			name: "runtime approval preparation",
			configure: func(_ *gatewayStore, approvals *fakeRuntimeApprovalExecutionStore, _ *gatewayLedger) {
				approvals.prepareErr = errors.New("prepare unavailable")
			},
			wantReceiptStatus: store.ToolInvocationPending,
		},
		{
			name: "reservation preparation",
			configure: func(st *gatewayStore, _ *fakeRuntimeApprovalExecutionStore, _ *gatewayLedger) {
				st.prepareReservationErr = errors.New("reservation unavailable")
			},
			wantReceiptStatus: store.ToolInvocationPending,
		},
		{
			name: "receipt preparation",
			configure: func(st *gatewayStore, _ *fakeRuntimeApprovalExecutionStore, _ *gatewayLedger) {
				st.prepareReceiptErr = errors.New("receipt unavailable")
			},
			wantReceiptStatus: store.ToolInvocationPending,
		},
		{
			name: "audit start",
			configure: func(_ *gatewayStore, _ *fakeRuntimeApprovalExecutionStore, ledger *gatewayLedger) {
				ledger.startErr = errors.New("audit unavailable")
			},
			wantPrepared: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
			executor := &gatewayExecutor{}
			ledger := &gatewayLedger{}
			gateway := testGateway(t, authorizer, &gatewayQuota{}, executor, ledger, 7)
			gatewayStore := gateway.store.(*gatewayStore)
			execStore := &fakeRuntimeApprovalExecutionStore{}
			test.configure(gatewayStore, execStore, ledger)
			WithGate3Coordinator(&stubGate3Resolver{result: gate3ReadyResult()})(gateway)
			WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
			WithRuntimeApprovalExecutionStore(execStore)(gateway)

			response, err := gateway.Invoke(context.Background(), testRequest())
			if err == nil || response.RuntimeApprovalState != RuntimeApprovalStateUnavailable {
				t.Fatalf("response=%#v error=%v, want unavailable failure", response, err)
			}
			if executor.calls != 0 || len(execStore.finalizeCalls) != 0 {
				t.Fatalf("pre-target failure dispatched: target=%d finalize=%d", executor.calls, len(execStore.finalizeCalls))
			}
			stored, found := gatewayStore.receipts["team-a/invocation-1"]
			if test.wantPrepared {
				if !found || stored.Status != store.ToolInvocationPrepared {
					t.Fatalf("receipt = (%#v, %t), want prepared", stored, found)
				}
			} else if test.wantReceiptStatus != "" && (!found || stored.Status != test.wantReceiptStatus) {
				t.Fatalf("receipt = (%#v, %t), want %q", stored, found, test.wantReceiptStatus)
			}
			if test.name != "audit start" && (len(execStore.abortCalls) != 0 || len(gatewayStore.cleanupCalls) != 0) {
				t.Fatalf("ambiguous failure released owner: abort=%d cleanup=%v", len(execStore.abortCalls), gatewayStore.cleanupCalls)
			}
			if test.name == "audit start" {
				if len(execStore.abortCalls) != 1 || !slices.Equal(gatewayStore.cleanupCalls, []string{"reservation", "receipt"}) || found {
					t.Fatalf("audit-start cleanup abort=%d cleanup=%v receiptFound=%t", len(execStore.abortCalls), gatewayStore.cleanupCalls, found)
				}
			}
		})
	}
}

func TestGatewayGate3QuotaExhaustionAbortsPreparationWithoutDispatch(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	executor := &gatewayExecutor{}
	gateway := testGateway(t, authorizer, &gatewayQuota{}, executor, &gatewayLedger{}, 7)
	gatewayStore := gateway.store.(*gatewayStore)
	gatewayStore.prepareReservationErr = store.ErrQuotaExceeded
	execStore := &fakeRuntimeApprovalExecutionStore{}
	WithGate3Coordinator(&stubGate3Resolver{result: gate3ReadyResult()})(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	response, err := gateway.Invoke(context.Background(), testRequest())
	if !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("Invoke() error = %v, want quota exceeded", err)
	}
	if response.RuntimeApprovalState != RuntimeApprovalStateTerminal {
		t.Fatalf("runtime approval state = %q, want terminal", response.RuntimeApprovalState)
	}
	if executor.calls != 0 {
		t.Fatalf("quota-exhausted invocation dispatched target %d times", executor.calls)
	}
	if len(execStore.abortCalls) != 1 || !slices.Equal(gatewayStore.cleanupCalls, []string{"reservation"}) {
		t.Fatalf("quota-exhausted cleanup abort=%d cleanup=%v", len(execStore.abortCalls), gatewayStore.cleanupCalls)
	}
	receipt := gatewayStore.receipts["team-a/invocation-1"]
	if receipt.Status != store.ToolInvocationFailed {
		t.Fatalf("quota-exhausted receipt status = %q, want failed", receipt.Status)
	}
}

func TestGatewayGate3SettlementFailureAfterTargetPreservesClaimAndPreparedReceipt(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway := testGateway(t, authorizer, &gatewayQuota{}, executor, &gatewayLedger{}, 7)
	gatewayStore := gateway.store.(*gatewayStore)
	settlementErr := errors.New("reservation settlement unavailable")
	gatewayStore.settleReservationErr = settlementErr
	execStore := &fakeRuntimeApprovalExecutionStore{fencingToken: 42}
	WithGate3Coordinator(&stubGate3Resolver{result: gate3ReadyResult()})(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	request := testRequest()
	response, err := gateway.Invoke(context.Background(), request)
	if !errors.Is(err, settlementErr) {
		t.Fatalf("Invoke() error = %v, want settlement failure", err)
	}
	if response.RuntimeApprovalState != RuntimeApprovalStateUnavailable {
		t.Fatalf("runtime approval state = %q, want unavailable", response.RuntimeApprovalState)
	}
	if executor.calls != 1 {
		t.Fatalf("target calls = %d, want 1", executor.calls)
	}
	if len(execStore.finalizeCalls) != 0 {
		t.Fatalf("finalize calls = %d, want 0 while settlement is unresolved", len(execStore.finalizeCalls))
	}
	stored := gatewayStore.receipts["team-a/invocation-1"]
	if stored.Status != store.ToolInvocationPrepared {
		t.Fatalf("receipt status = %q, want %q", stored.Status, store.ToolInvocationPrepared)
	}
	if len(gatewayStore.releasedReservations) != 0 || len(gatewayStore.cleanupCalls) != 0 {
		t.Fatalf("claimed preparation was released: reservations=%#v cleanup=%v", gatewayStore.releasedReservations, gatewayStore.cleanupCalls)
	}

	if _, retryErr := gateway.Invoke(context.Background(), request); !errors.Is(retryErr, ErrToolInvocationInProgress) {
		t.Fatalf("retry error = %v, want ErrToolInvocationInProgress", retryErr)
	}
	if executor.calls != 1 {
		t.Fatalf("retry duplicated target effect: calls=%d", executor.calls)
	}
}

func TestGatewayGate3MissingSettlementPreservesClaimAndPreparedReceipt(t *testing.T) {
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway := testGateway(t, &gatewayAuthorizer{}, &gatewayQuota{}, executor, &gatewayLedger{}, 7)
	gatewayStore := gateway.store.(*gatewayStore)
	settled := false
	gatewayStore.settleReservationOK = &settled
	execStore := &fakeRuntimeApprovalExecutionStore{}
	WithGate3Coordinator(&stubGate3Resolver{result: gate3ReadyResult()})(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	response, err := gateway.Invoke(context.Background(), testRequest())
	if err == nil || response.RuntimeApprovalState != RuntimeApprovalStateUnavailable {
		t.Fatalf("response=%#v error=%v, want unavailable missing settlement", response, err)
	}
	if executor.calls != 1 || len(execStore.finalizeCalls) != 0 {
		t.Fatalf("missing settlement ownership target=%d finalize=%d", executor.calls, len(execStore.finalizeCalls))
	}
	if stored := gatewayStore.receipts["team-a/invocation-1"]; stored.Status != store.ToolInvocationPrepared {
		t.Fatalf("receipt status = %q, want prepared", stored.Status)
	}
}

func TestGatewayGate3ReceiptCompletionFailurePreservesFinalizedApprovalOwner(t *testing.T) {
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway := testGateway(t, &gatewayAuthorizer{}, &gatewayQuota{}, executor, &gatewayLedger{}, 7)
	gatewayStore := gateway.store.(*gatewayStore)
	completionErr := errors.New("receipt completion unavailable")
	gatewayStore.completeReceiptErr = completionErr
	execStore := &fakeRuntimeApprovalExecutionStore{}
	WithGate3Coordinator(&stubGate3Resolver{result: gate3ReadyResult()})(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	request := testRequest()
	if _, err := gateway.Invoke(context.Background(), request); !errors.Is(err, completionErr) {
		t.Fatalf("Invoke() error = %v, want receipt completion failure", err)
	}
	if executor.calls != 1 || len(execStore.finalizeCalls) != 1 || len(gatewayStore.settledReservations) != 1 {
		t.Fatalf("ownership order target=%d finalize=%d settle=%d", executor.calls, len(execStore.finalizeCalls), len(gatewayStore.settledReservations))
	}
	if stored := gatewayStore.receipts["team-a/invocation-1"]; stored.Status != store.ToolInvocationPrepared {
		t.Fatalf("receipt status = %q, want prepared", stored.Status)
	}
	gatewayStore.completeReceiptErr = nil
	if _, retryErr := gateway.Invoke(context.Background(), request); !errors.Is(retryErr, ErrToolInvocationInProgress) {
		t.Fatalf("retry error = %v, want ErrToolInvocationInProgress", retryErr)
	}
	if executor.calls != 1 {
		t.Fatalf("retry duplicated target effect: calls=%d", executor.calls)
	}
}

func TestGatewayGate3CompletionAuditFailureLeavesSucceededReceiptReplayOwner(t *testing.T) {
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	ledger := &gatewayLedger{completionErr: errors.New("audit completion unavailable")}
	gateway := testGateway(t, &gatewayAuthorizer{}, &gatewayQuota{}, executor, ledger, 7)
	execStore := &fakeRuntimeApprovalExecutionStore{}
	WithGate3Coordinator(&stubGate3Resolver{result: gate3ReadyResult()})(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	request := testRequest()
	response, err := gateway.Invoke(context.Background(), request)
	if err == nil || response.ErrorCode != "ToolAuditCompletionFailed" || response.RuntimeApprovalState != RuntimeApprovalStateUnavailable {
		t.Fatalf("response=%#v error=%v, want visible completion-audit failure", response, err)
	}
	stored := gateway.store.(*gatewayStore).receipts["team-a/invocation-1"]
	if stored.Status != store.ToolInvocationSucceeded {
		t.Fatalf("receipt status = %q, want succeeded replay owner", stored.Status)
	}
	replayed, retryErr := gateway.Invoke(context.Background(), request)
	if retryErr != nil || replayed.Output["updated"] != true || executor.calls != 1 {
		t.Fatalf("retry response=%#v error=%v target calls=%d", replayed, retryErr, executor.calls)
	}
}

func TestGatewayGate3ClaimFailurePreservesPreparedOwner(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	resolver := &stubGate3Resolver{result: gate3ReadyResult()}
	claimErr := errors.New("claim conflict")
	execStore := &fakeRuntimeApprovalExecutionStore{claimErr: claimErr}
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	_, err := gateway.Invoke(context.Background(), testRequest())
	if !errors.Is(err, claimErr) {
		t.Fatalf("expected wrapped claim error, got %v", err)
	}

	if len(execStore.prepareCalls) != 1 {
		t.Fatalf("expected 1 Prepare call, got %d", len(execStore.prepareCalls))
	}
	if len(execStore.claimCalls) != 1 {
		t.Fatalf("expected 1 Claim call, got %d", len(execStore.claimCalls))
	}
	if executor.calls != 0 {
		t.Fatalf("executor must not run when Claim fails; got %d calls", executor.calls)
	}
	if len(execStore.finalizeCalls) != 0 {
		t.Fatalf("Finalize must not be called when Claim fails; got %d", len(execStore.finalizeCalls))
	}
	if len(execStore.abortCalls) != 0 {
		t.Fatalf("ambiguous Claim failure must not abort preparation; got %d calls", len(execStore.abortCalls))
	}
	gatewayStore := gateway.store.(*gatewayStore)
	if len(gatewayStore.cleanupCalls) != 0 || len(gatewayStore.releasedReservations) != 0 {
		t.Fatalf("ambiguous Claim failure released ownership: cleanup=%v reservations=%#v", gatewayStore.cleanupCalls, gatewayStore.releasedReservations)
	}
	if stored := gatewayStore.receipts["team-a/invocation-1"]; stored.Status != store.ToolInvocationPrepared {
		t.Fatalf("receipt status = %q, want prepared owner", stored.Status)
	}
	if len(ledger.completions) != 0 || executor.calls != 0 {
		t.Fatalf("claim failure completion=%d target=%d", len(ledger.completions), executor.calls)
	}
}

func TestGatewayGate3ReadyRetainsReceiptWhenReservationReleaseIsAmbiguous(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	ledger := &gatewayLedger{startErr: errors.New("audit unavailable")}
	executor := &gatewayExecutor{}
	gateway := testGateway(t, authorizer, &gatewayQuota{}, executor, ledger, 7)
	gatewayStore := gateway.store.(*gatewayStore)
	gatewayStore.releaseReservationErr = errors.New("reservation release unavailable")
	execStore := &fakeRuntimeApprovalExecutionStore{}
	WithGate3Coordinator(&stubGate3Resolver{result: gate3ReadyResult()})(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	if _, err := gateway.Invoke(context.Background(), testRequest()); !errors.Is(err, ledger.startErr) {
		t.Fatalf("Invoke() error = %v, want audit failure", err)
	}
	if executor.calls != 0 || len(execStore.claimCalls) != 0 || len(execStore.abortCalls) != 1 {
		t.Fatalf("pre-claim failure dispatched or skipped abort: executor=%d claim=%d abort=%d", executor.calls, len(execStore.claimCalls), len(execStore.abortCalls))
	}
	if got, want := gatewayStore.cleanupCalls, []string{"reservation"}; !slices.Equal(got, want) {
		t.Fatalf("cleanup calls = %v, want %v", got, want)
	}
	stored := gatewayStore.receipts["team-a/invocation-1"]
	if stored.Status != store.ToolInvocationPrepared {
		t.Fatalf("receipt status = %q, want prepared ownership preserved", stored.Status)
	}
}

func TestGatewayGate3FinalizeFailureIsUnavailableAndPreservesPreparedReceipt(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	resolver := &stubGate3Resolver{result: gate3ReadyResult()}
	execStore := &fakeRuntimeApprovalExecutionStore{finalizeErr: errors.New("fencing token mismatch"), fencingToken: 42}
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	request := testRequest()
	resp, err := gateway.Invoke(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "fencing token mismatch") {
		t.Fatalf("Finalize failure was not caller-visible: %v", err)
	}
	if resp.Output["updated"] != true {
		t.Fatalf("expected executor output preserved on success, got %#v", resp.Output)
	}
	if len(execStore.finalizeCalls) != 1 {
		t.Fatalf("expected 1 Finalize attempt, got %d", len(execStore.finalizeCalls))
	}
	if execStore.finalizeCalls[0].disposition != runtimeapproval.ExecutionDispositionSucceeded {
		t.Fatalf("expected Finalize disposition=Succeeded, got %q", execStore.finalizeCalls[0].disposition)
	}
	if execStore.finalizeCalls[0].fencingToken != 42 {
		t.Fatalf("Finalize must carry the fencing token from Claim (42), got %d", execStore.finalizeCalls[0].fencingToken)
	}
	if resp.RuntimeApprovalState != RuntimeApprovalStateUnavailable || len(ledger.completions) != 0 {
		t.Fatalf("response=%#v completions=%#v, want unavailable without completion", resp, ledger.completions)
	}
	stored := gateway.store.(*gatewayStore).receipts["team-a/invocation-1"]
	if stored.Status != store.ToolInvocationPrepared {
		t.Fatalf("receipt status = %q, want prepared", stored.Status)
	}
	if _, retryErr := gateway.Invoke(context.Background(), request); !errors.Is(retryErr, ErrToolInvocationInProgress) {
		t.Fatalf("retry error = %v, want ErrToolInvocationInProgress", retryErr)
	}
	if executor.calls != 1 {
		t.Fatalf("retry duplicated target effect: calls=%d", executor.calls)
	}
}

func TestGatewayGate3FinalizeFailureAfterTargetErrorPreservesClaimedOwner(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	execErr := errors.New("upstream boom")
	executor := &gatewayExecutor{err: execErr}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	resolver := &stubGate3Resolver{result: gate3ReadyResult()}
	execStore := &fakeRuntimeApprovalExecutionStore{finalizeErr: errors.New("fencing token mismatch")}
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	_, err := gateway.Invoke(context.Background(), testRequest())
	if !errors.Is(err, execErr) || !strings.Contains(err.Error(), "fencing token mismatch") {
		t.Fatalf("expected target and Finalize failures, got %v", err)
	}
	if len(execStore.finalizeCalls) != 1 {
		t.Fatalf("expected 1 Finalize attempt, got %d", len(execStore.finalizeCalls))
	}
	if execStore.finalizeCalls[0].disposition != runtimeapproval.ExecutionDispositionFailed {
		t.Fatalf("expected Finalize disposition=Failed, got %q", execStore.finalizeCalls[0].disposition)
	}
	if len(ledger.completions) != 0 || executor.calls != 1 {
		t.Fatalf("completion=%#v target calls=%d, want claimed owner and one effect", ledger.completions, executor.calls)
	}
}

// TestGatewayGate3ReadyAbortsWhenPreparationExpired asserts that a Claim
// rejection shaped like a preparation-window expiration still triggers
// AbortRuntimeApprovalBeforeDispatch. The reaper handles orphaned prepared
// approvals via the deadline, but a live-process abort short-circuits the
// wait and moves the durable state to Aborted immediately.
func TestGatewayGate3PreparationExpiredClaimPreservesPreparedOwner(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	resolver := &stubGate3Resolver{result: gate3ReadyResult()}
	claimErr := errors.New("preparation window expired")
	execStore := &fakeRuntimeApprovalExecutionStore{claimErr: claimErr}
	WithGate3Coordinator(resolver)(gateway)
	WithGate3RequestBuilder(gate3TestBuilder("update"))(gateway)
	WithRuntimeApprovalExecutionStore(execStore)(gateway)

	_, err := gateway.Invoke(context.Background(), testRequest())
	if !errors.Is(err, claimErr) {
		t.Fatalf("expected wrapped claim error, got %v", err)
	}
	if len(execStore.abortCalls) != 0 {
		t.Fatalf("ambiguous expired Claim must preserve preparation; abort calls=%d", len(execStore.abortCalls))
	}
	if executor.calls != 0 {
		t.Fatalf("executor must not run when preparation expired; got %d calls", executor.calls)
	}
	if len(execStore.finalizeCalls) != 0 {
		t.Fatalf("Finalize must not be called when Claim fails; got %d", len(execStore.finalizeCalls))
	}

	// Prepare deadline must be bounded by MaxPreparationDeadline so a
	// hung claim cannot leave the approval prepared indefinitely.
	prep := execStore.prepareCalls[0]
	upper := time.Now().Add(runtimeapproval.MaxPreparationDeadline).Add(1 * time.Second)
	if prep.deadline.After(upper) {
		t.Fatalf("preparation deadline %v must be within MaxPreparationDeadline of now", prep.deadline)
	}
}
