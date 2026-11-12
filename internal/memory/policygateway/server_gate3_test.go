// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package policygateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

// captureGate3Resolver records the request it received and returns a
// pre-programmed result or error. Used to drive ServeHTTP-level Gate 3
// enforcement scenarios.
type captureGate3Resolver struct {
	result  runtimeapproval.Gate3Result
	err     error
	calls   int
	lastReq runtimeapproval.Gate3Request
}

func (c *captureGate3Resolver) Resolve(_ context.Context, request runtimeapproval.Gate3Request) (runtimeapproval.Gate3Result, error) {
	c.calls++
	c.lastReq = request
	return c.result, c.err
}

func readyResult() runtimeapproval.Gate3Result {
	return runtimeapproval.Gate3Result{
		State: runtimeapproval.Gate3StateReady,
		Approval: v1alpha1.RuntimeApproval{
			ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "invocation-1", ResourceVersion: 7},
			Spec: v1alpha1.RuntimeApprovalSpec{
				InvocationID: "invocation-1",
				Binding:      v1alpha1.RuntimeActionBinding{Tag: "binding-tag"},
			},
			Status: v1alpha1.RuntimeApprovalStatus{DecisionState: v1alpha1.RuntimeApprovalDecisionNotRequired},
		},
	}
}

func pendingResult() runtimeapproval.Gate3Result {
	return runtimeapproval.Gate3Result{
		State: runtimeapproval.Gate3StatePending,
		Approval: v1alpha1.RuntimeApproval{
			ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "invocation-1"},
		},
	}
}

func withGate3(server *Server, resolver Gate3Resolver, audit *policyAuditStore) {
	server.gate3 = resolver
	server.gate3Builder = NewGate3RequestBuilder()
	if audit != nil {
		server.auditLog = observability.NewCognitiveAuditLog(audit)
	}
}

func storeRequest(t *testing.T, capability, volume string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/memory/store",
		bytes.NewBufferString(`{"volume_handle":"`+volume+`","content":"hello"}`))
	r.Header.Set("Authorization", "Bearer "+capability)
	r.Header.Set("X-Kognition-Run-ID", "demo-run")
	r.Header.Set(MemoryInvocationIDHeader, "invocation-1")
	return r
}

func deleteRequest(t *testing.T, capability, volume string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodDelete, "/v1/memory/delete/"+volume+"/record-1", nil)
	r.Header.Set("Authorization", "Bearer "+capability)
	r.Header.Set("X-Kognition-Run-ID", "demo-run")
	r.Header.Set(MemoryInvocationIDHeader, "invocation-1")
	return r
}

func TestServerGate3ReadyAllowsWriteAndRecordsAuditLifecycle(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	audit := &policyAuditStore{}
	resolver := &captureGate3Resolver{result: readyResult()}
	resolver.result.Approval.Status.DecisionState = v1alpha1.RuntimeApprovalDecisionApproved
	resolver.result.Approval.UID = "approval-uid"
	withGate3(server, resolver, audit)
	WithRuntimeApprovalExecutionStore(&memoryApprovalExecutionStore{})(server)
	WithMemoryMutationReceiptStore(&memoryMutationReceiptStore{})(server)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := storeRequest(t, capability, binding.VolumeHandle)
	request.Header.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	server.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
	if *upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1", *upstreamCalls)
	}
	if len(audit.entries) != 2 {
		t.Fatalf("audit entries = %d, want 2: %#v", len(audit.entries), audit.entries)
	}
	var authorized, completed observability.TrustDecisionV1
	if err := json.Unmarshal(audit.entries[0].Details, &authorized); err != nil {
		t.Fatalf("unmarshal audit details: %v", err)
	}
	if authorized.Outcome != "allowed" || authorized.ReasonCode != "authorized" || authorized.Execution.Status != "not_started" || authorized.Execution.TargetReached == nil || *authorized.Execution.TargetReached {
		t.Fatalf("audit decision = %s", audit.entries[0].Details)
	}
	if err := json.Unmarshal(audit.entries[1].Details, &completed); err != nil {
		t.Fatalf("unmarshal completed audit details: %v", err)
	}
	if completed.Outcome != "completed" || completed.ReasonCode != "target_completed" || completed.Execution.Status != "completed" || completed.Execution.TargetReached == nil || !*completed.Execution.TargetReached {
		t.Fatalf("completed audit decision = %s", audit.entries[1].Details)
	}
	claims, err := issuer.Verify(capability, binding)
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range []observability.TrustDecisionV1{authorized, completed} {
		if decision.Session == nil || decision.Session.CapabilityID != claims.ID || claims.ID == "" || decision.Session.CapabilityID == binding.InstanceUID {
			t.Fatalf("incorrect capability evidence: %+v", decision.Session)
		}
		if decision.Approval == nil || decision.Approval.Status != "approved" || decision.Approval.DecisionRef == nil || decision.Approval.DecisionRef.UID != "approval-uid" {
			t.Fatalf("incorrect approval evidence: %+v", decision.Approval)
		}
		if decision.Trace == nil || decision.Trace.TraceID != "11111111111111111111111111111111" {
			t.Fatalf("missing trace correlation: %+v", decision.Trace)
		}
	}
}

type memoryApprovalExecutionStore struct {
	store.RuntimeApprovalExecutionStore
	events        []string
	prepareTokens []string
	claimTokens   []string
	prepareErr    error
	claimErr      error
	claimResult   *v1alpha1.RuntimeApproval
	finalizeErr   error
}

type memoryMutationReceiptStore struct {
	store.MemoryMutationReceiptStore
	prepared    []store.MemoryMutationReceipt
	completed   []store.MemoryMutationReceipt
	released    []string
	existing    *store.MemoryMutationReceipt
	prepareErr  error
	completeErr error
}

func (s *memoryMutationReceiptStore) PrepareMemoryMutation(_ context.Context, receipt store.MemoryMutationReceipt) (bool, store.MemoryMutationReceipt, error) {
	s.prepared = append(s.prepared, receipt)
	if s.existing != nil && s.prepareErr == nil {
		if s.existing.Namespace != receipt.Namespace || s.existing.InvocationID != receipt.InvocationID ||
			s.existing.Binding != receipt.Binding || s.existing.Operation != receipt.Operation ||
			s.existing.Status != store.MemoryMutationPrepared || s.existing.PreparationToken != receipt.PreparationToken ||
			s.existing.PreparationDeadline == nil || receipt.PreparationDeadline == nil ||
			!s.existing.PreparationDeadline.Equal(*receipt.PreparationDeadline) {
			return false, store.MemoryMutationReceipt{}, store.ErrConflict
		}
		return true, *s.existing, nil
	}
	return s.prepareErr == nil, receipt, s.prepareErr
}

func (s *memoryMutationReceiptStore) ReleasePreparedMemoryMutation(_ context.Context, namespace, invocationID, token string) (bool, error) {
	s.released = append(s.released, namespace+"/"+invocationID+"/"+token)
	return true, nil
}

func (s *memoryMutationReceiptStore) CompleteMemoryMutation(_ context.Context, receipt store.MemoryMutationReceipt) error {
	s.completed = append(s.completed, receipt)
	return s.completeErr
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (s *memoryApprovalExecutionStore) PrepareRuntimeApproval(_ context.Context, namespace, invocationID string, expectedResourceVersion int64, token string, _ time.Time) (v1alpha1.RuntimeApproval, error) {
	s.events = append(s.events, "prepare")
	s.prepareTokens = append(s.prepareTokens, token)
	if s.prepareErr != nil {
		return v1alpha1.RuntimeApproval{}, s.prepareErr
	}
	return v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: namespace, Name: invocationID, ResourceVersion: expectedResourceVersion + 1},
		Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: invocationID},
	}, nil
}

func (s *memoryApprovalExecutionStore) ClaimRuntimeApprovalExecution(_ context.Context, namespace, invocationID string, expectedResourceVersion int64, token string, claimant v1alpha1.RuntimeApprovalClaimant) (v1alpha1.RuntimeApproval, error) {
	s.events = append(s.events, "claim")
	s.claimTokens = append(s.claimTokens, token)
	if s.claimErr != nil {
		return v1alpha1.RuntimeApproval{}, s.claimErr
	}
	if s.claimResult != nil {
		return *s.claimResult, nil
	}
	return v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: namespace, Name: invocationID, ResourceVersion: expectedResourceVersion + 1},
		Spec:       v1alpha1.RuntimeApprovalSpec{InvocationID: invocationID},
		Status: v1alpha1.RuntimeApprovalStatus{
			ExecutionState: v1alpha1.RuntimeApprovalExecutionClaimed,
			FencingToken:   42,
			Claimant:       &claimant,
		},
	}, nil
}

func (s *memoryApprovalExecutionStore) AbortRuntimeApprovalBeforeDispatch(context.Context, string, string, int64, string, string) (v1alpha1.RuntimeApproval, error) {
	s.events = append(s.events, "abort")
	return v1alpha1.RuntimeApproval{}, nil
}

func (s *memoryApprovalExecutionStore) FinalizeRuntimeApprovalExecution(_ context.Context, _, _ string, _ int64, fencingToken int64, disposition string) (v1alpha1.RuntimeApproval, error) {
	s.events = append(s.events, "finalize:"+disposition)
	if fencingToken != 42 {
		return v1alpha1.RuntimeApproval{}, errors.New("unexpected fencing token")
	}
	if s.finalizeErr != nil {
		return v1alpha1.RuntimeApproval{}, s.finalizeErr
	}
	return v1alpha1.RuntimeApproval{}, nil
}

func TestServerGate3ReadyClaimsBeforeMemoryMutationAndFinalizesSuccess(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	resolver := &captureGate3Resolver{result: readyResult()}
	executionStore := &memoryApprovalExecutionStore{}
	receiptStore := &memoryMutationReceiptStore{}
	withGate3(server, resolver, nil)
	WithRuntimeApprovalExecutionStore(executionStore)(server)
	WithMemoryMutationReceiptStore(receiptStore)(server)
	server.httpClient.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		executionStore.events = append(executionStore.events, "upstream")
		return http.DefaultTransport.RoundTrip(request)
	})

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := storeRequest(t, capability, binding.VolumeHandle)
	request.Header.Set(MemoryInvocationIDHeader, "invocation-1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if *upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1", *upstreamCalls)
	}
	want := []string{"prepare", "claim", "upstream", "finalize:Succeeded"}
	if !reflect.DeepEqual(executionStore.events, want) {
		t.Fatalf("execution events = %#v, want %#v", executionStore.events, want)
	}
	if len(receiptStore.completed) != 1 || receiptStore.completed[0].Status != store.MemoryMutationSucceeded {
		t.Fatalf("completed receipts = %#v, want one succeeded receipt", receiptStore.completed)
	}
	assertStoredMemoryResponse(t, receiptStore.completed[0], response)
	if receiptStore.completed[0].ErrorMessage != "" {
		t.Fatalf("completed receipt error = %q, want empty", receiptStore.completed[0].ErrorMessage)
	}
}

func TestServerGate3ResumesExactPreparedMemoryMutationReceipt(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	deadline := time.Date(2026, time.March, 12, 10, 5, 0, 0, time.UTC)
	executionStore := &memoryApprovalExecutionStore{}
	receiptStore := &memoryMutationReceiptStore{existing: &store.MemoryMutationReceipt{
		Namespace:           "team-a",
		InvocationID:        "invocation-1",
		Binding:             "binding-tag",
		Operation:           "store",
		Status:              store.MemoryMutationPrepared,
		PreparationToken:    "original-token",
		PreparationDeadline: &deadline,
	}}
	result := readyResult()
	result.Approval.Status.PreparationToken = "original-token"
	result.Approval.Status.PreparationDeadline = &deadline
	withGate3(server, &captureGate3Resolver{result: result}, nil)
	WithRuntimeApprovalExecutionStore(executionStore)(server)
	WithMemoryMutationReceiptStore(receiptStore)(server)
	WithPreparationTokenFactory(func() (string, error) { return "replacement-token", nil })(server)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := storeRequest(t, capability, binding.VolumeHandle)
	request.Header.Set(MemoryInvocationIDHeader, "invocation-1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if *upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1", *upstreamCalls)
	}
	if got, want := receiptStore.prepared[0].PreparationToken, "original-token"; got != want {
		t.Fatalf("prepared receipt token = %q, want %q", got, want)
	}
	if len(executionStore.prepareTokens) != 0 {
		t.Fatalf("runtime approval preparation tokens = %#v, want none for resumed preparation", executionStore.prepareTokens)
	}
	if got, want := executionStore.claimTokens, []string{"original-token"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime approval claim tokens = %#v, want %#v", got, want)
	}
}

func TestServerGate3ResumedPreparationFailureDoesNotReleaseReceipt(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	deadline := time.Date(2026, time.March, 12, 10, 5, 0, 0, time.UTC)
	result := readyResult()
	result.Approval.Status.PreparationToken = "original-token"
	result.Approval.Status.PreparationDeadline = &deadline
	executionStore := &memoryApprovalExecutionStore{claimErr: errors.New("already claimed")}
	receiptStore := &memoryMutationReceiptStore{existing: &store.MemoryMutationReceipt{
		Namespace:           "team-a",
		InvocationID:        "invocation-1",
		Binding:             "binding-tag",
		Operation:           "store",
		Status:              store.MemoryMutationPrepared,
		PreparationToken:    "original-token",
		PreparationDeadline: &deadline,
	}}
	withGate3(server, &captureGate3Resolver{result: result}, nil)
	WithRuntimeApprovalExecutionStore(executionStore)(server)
	WithMemoryMutationReceiptStore(receiptStore)(server)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, storeRequest(t, capability, binding.VolumeHandle))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if *upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", *upstreamCalls)
	}
	if len(receiptStore.released) != 0 {
		t.Fatalf("released receipts = %#v, want none for resumed preparation", receiptStore.released)
	}
}

func TestServerGate3RuntimePreparationFailureRetainsReceipt(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	executionStore := &memoryApprovalExecutionStore{prepareErr: errors.New("runtime approval unavailable")}
	receiptStore := &memoryMutationReceiptStore{}
	withGate3(server, &captureGate3Resolver{result: readyResult()}, nil)
	WithRuntimeApprovalExecutionStore(executionStore)(server)
	WithMemoryMutationReceiptStore(receiptStore)(server)
	WithPreparationTokenFactory(func() (string, error) { return "owned-preparation-token", nil })(server)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, storeRequest(t, capability, binding.VolumeHandle))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if *upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", *upstreamCalls)
	}
	if len(receiptStore.released) != 0 {
		t.Fatalf("released receipts = %#v, want none after ambiguous RuntimeApproval preparation", receiptStore.released)
	}
}

func TestServerGate3RejectsIncompleteDurablePreparationIdentity(t *testing.T) {
	for _, mutate := range []func(*v1alpha1.RuntimeApprovalStatus){
		func(status *v1alpha1.RuntimeApprovalStatus) { status.PreparationToken = "orphan-token" },
		func(status *v1alpha1.RuntimeApprovalStatus) {
			deadline := time.Date(2026, time.March, 12, 10, 5, 0, 0, time.UTC)
			status.PreparationDeadline = &deadline
		},
	} {
		server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
		result := readyResult()
		mutate(&result.Approval.Status)
		executionStore := &memoryApprovalExecutionStore{}
		receiptStore := &memoryMutationReceiptStore{}
		withGate3(server, &captureGate3Resolver{result: result}, nil)
		WithRuntimeApprovalExecutionStore(executionStore)(server)
		WithMemoryMutationReceiptStore(receiptStore)(server)

		capability, err := issuer.Issue(binding)
		if err != nil {
			upstream.Close()
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, storeRequest(t, capability, binding.VolumeHandle))
		upstream.Close()

		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
		}
		if *upstreamCalls != 0 || len(receiptStore.prepared) != 0 || len(executionStore.events) != 0 {
			t.Fatalf("incomplete preparation reached side effects: upstream=%d receipts=%d events=%#v", *upstreamCalls, len(receiptStore.prepared), executionStore.events)
		}
	}
}

func TestServerGate3MemoryMutationFailureRetainsUpstreamResult(t *testing.T) {
	server, _, issuer, binding, upstream, _ := gatewayFixture(t)
	defer upstream.Close()
	executionStore := &memoryApprovalExecutionStore{}
	receiptStore := &memoryMutationReceiptStore{}
	withGate3(server, &captureGate3Resolver{result: readyResult()}, nil)
	WithRuntimeApprovalExecutionStore(executionStore)(server)
	WithMemoryMutationReceiptStore(receiptStore)(server)
	server.httpClient.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnprocessableEntity,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"code":"invalid_memory"}`)),
		}, nil
	})

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := storeRequest(t, capability, binding.VolumeHandle)
	request.Header.Set(MemoryInvocationIDHeader, "invocation-1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity || response.Body.String() != `{"code":"invalid_memory"}` {
		t.Fatalf("response status=%d body=%s", response.Code, response.Body.String())
	}
	if len(receiptStore.completed) != 1 || receiptStore.completed[0].Status != store.MemoryMutationFailed {
		t.Fatalf("completed receipts = %#v, want one failed receipt", receiptStore.completed)
	}
	completed := receiptStore.completed[0]
	assertStoredMemoryResponse(t, completed, response)
	if got, want := completed.ErrorMessage, "memory service returned HTTP 422"; got != want {
		t.Fatalf("completed receipt error = %q, want %q", got, want)
	}
	wantEvents := []string{"prepare", "claim", "finalize:Failed"}
	if !reflect.DeepEqual(executionStore.events, wantEvents) {
		t.Fatalf("execution events = %#v, want %#v", executionStore.events, wantEvents)
	}
}

func TestServerGate3DeleteClaimsBeforeMutationAndPersistsSuccess(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	executionStore := &memoryApprovalExecutionStore{}
	receiptStore := &memoryMutationReceiptStore{}
	withGate3(server, &captureGate3Resolver{result: readyResult()}, nil)
	WithRuntimeApprovalExecutionStore(executionStore)(server)
	WithMemoryMutationReceiptStore(receiptStore)(server)
	server.httpClient.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		executionStore.events = append(executionStore.events, "upstream")
		return http.DefaultTransport.RoundTrip(request)
	})

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, deleteRequest(t, capability, binding.VolumeHandle))

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if *upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1", *upstreamCalls)
	}
	wantEvents := []string{"prepare", "claim", "upstream", "finalize:Succeeded"}
	if !reflect.DeepEqual(executionStore.events, wantEvents) {
		t.Fatalf("execution events = %#v, want %#v", executionStore.events, wantEvents)
	}
	if len(receiptStore.completed) != 1 || receiptStore.completed[0].Operation != "delete" || receiptStore.completed[0].Status != store.MemoryMutationSucceeded {
		t.Fatalf("completed receipts = %#v, want one succeeded delete", receiptStore.completed)
	}
}

func TestServerGate3DeleteFailurePersistsTerminalOutcome(t *testing.T) {
	server, _, issuer, binding, upstream, _ := gatewayFixture(t)
	defer upstream.Close()
	executionStore := &memoryApprovalExecutionStore{}
	receiptStore := &memoryMutationReceiptStore{}
	audit := &policyAuditStore{}
	withGate3(server, &captureGate3Resolver{result: readyResult()}, audit)
	WithRuntimeApprovalExecutionStore(executionStore)(server)
	WithMemoryMutationReceiptStore(receiptStore)(server)
	server.httpClient.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnprocessableEntity,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"code":"delete_failed"}`)),
		}, nil
	})

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, deleteRequest(t, capability, binding.VolumeHandle))

	if response.Code != http.StatusUnprocessableEntity || response.Body.String() != `{"code":"delete_failed"}` {
		t.Fatalf("response status=%d body=%s", response.Code, response.Body.String())
	}
	if len(receiptStore.completed) != 1 || receiptStore.completed[0].Operation != "delete" || receiptStore.completed[0].Status != store.MemoryMutationFailed {
		t.Fatalf("completed receipts = %#v, want one failed delete", receiptStore.completed)
	}
	completed := receiptStore.completed[0]
	assertStoredMemoryResponse(t, completed, response)
	if got, want := completed.ErrorMessage, "memory service returned HTTP 422"; got != want {
		t.Fatalf("completed receipt error = %q, want %q", got, want)
	}
	wantEvents := []string{"prepare", "claim", "finalize:Failed"}
	if !reflect.DeepEqual(executionStore.events, wantEvents) {
		t.Fatalf("execution events = %#v, want %#v", executionStore.events, wantEvents)
	}
	if len(audit.entries) != 2 {
		t.Fatalf("audit entries = %d, want 2: %#v", len(audit.entries), audit.entries)
	}
	var failed observability.TrustDecisionV1
	if err := json.Unmarshal(audit.entries[1].Details, &failed); err != nil {
		t.Fatalf("unmarshal failed audit details: %v", err)
	}
	if failed.Outcome != "failed" || failed.ReasonCode != "target_failed" || failed.Execution.Status != "failed" || failed.Execution.TargetReached == nil || !*failed.Execution.TargetReached {
		t.Fatalf("failed audit decision = %s", audit.entries[1].Details)
	}
}

func TestServerGate3UnknownTargetCompletionLeavesClaimedWithoutTerminalReceipt(t *testing.T) {
	for _, operation := range []string{"store", "delete"} {
		t.Run(operation, func(t *testing.T) {
			server, _, issuer, binding, upstream, _ := gatewayFixture(t)
			defer upstream.Close()
			executionStore := &memoryApprovalExecutionStore{}
			receiptStore := &memoryMutationReceiptStore{}
			withGate3(server, &captureGate3Resolver{result: readyResult()}, nil)
			WithRuntimeApprovalExecutionStore(executionStore)(server)
			WithMemoryMutationReceiptStore(receiptStore)(server)
			server.httpClient.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("connection reset after request write")
			})

			capability, err := issuer.Issue(binding)
			if err != nil {
				t.Fatal(err)
			}
			var request *http.Request
			if operation == "delete" {
				request = deleteRequest(t, capability, binding.VolumeHandle)
			} else {
				request = storeRequest(t, capability, binding.VolumeHandle)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)

			if response.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusBadGateway)
			}
			if got, want := executionStore.events, []string{"prepare", "claim"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("execution events = %#v, want %#v", got, want)
			}
			if len(receiptStore.completed) != 0 || len(receiptStore.released) != 0 {
				t.Fatalf("unknown completion became terminal or released: completed=%#v released=%#v", receiptStore.completed, receiptStore.released)
			}
		})
	}
}

func TestServerGate3ReceiptCompletionFailureLeavesApprovalClaimed(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	executionStore := &memoryApprovalExecutionStore{}
	receiptStore := &memoryMutationReceiptStore{completeErr: errors.New("receipt unavailable")}
	withGate3(server, &captureGate3Resolver{result: readyResult()}, nil)
	WithRuntimeApprovalExecutionStore(executionStore)(server)
	WithMemoryMutationReceiptStore(receiptStore)(server)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := storeRequest(t, capability, binding.VolumeHandle)
	request.Header.Set(MemoryInvocationIDHeader, "invocation-1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d body=%s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if *upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1", *upstreamCalls)
	}
	wantEvents := []string{"prepare", "claim"}
	if !reflect.DeepEqual(executionStore.events, wantEvents) {
		t.Fatalf("execution events = %#v, want %#v", executionStore.events, wantEvents)
	}
	if len(receiptStore.completed) != 1 || receiptStore.completed[0].Status != store.MemoryMutationSucceeded {
		t.Fatalf("completion attempts = %#v, want one succeeded receipt attempt", receiptStore.completed)
	}
}

func TestServerGate3AmbiguousCompletionRetryDoesNotDuplicateMutation(t *testing.T) {
	for _, operation := range []string{"store", "delete"} {
		t.Run(operation, func(t *testing.T) {
			server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
			defer upstream.Close()
			resolver := &captureGate3Resolver{result: readyResult()}
			executionStore := &memoryApprovalExecutionStore{}
			receiptStore := &memoryMutationReceiptStore{completeErr: errors.New("receipt unavailable")}
			withGate3(server, resolver, nil)
			WithRuntimeApprovalExecutionStore(executionStore)(server)
			WithMemoryMutationReceiptStore(receiptStore)(server)
			tokenIndex := 0
			WithPreparationTokenFactory(func() (string, error) {
				tokenIndex++
				return fmt.Sprintf("preparation-token-%d", tokenIndex), nil
			})(server)

			capability, err := issuer.Issue(binding)
			if err != nil {
				t.Fatal(err)
			}
			newRequest := func() *http.Request {
				if operation == "delete" {
					return deleteRequest(t, capability, binding.VolumeHandle)
				}
				return storeRequest(t, capability, binding.VolumeHandle)
			}
			firstResponse := httptest.NewRecorder()
			server.ServeHTTP(firstResponse, newRequest())
			if firstResponse.Code != http.StatusServiceUnavailable {
				t.Fatalf("first status = %d, want %d body=%s", firstResponse.Code, http.StatusServiceUnavailable, firstResponse.Body.String())
			}

			preparedReceipt := receiptStore.prepared[0]
			receiptStore.existing = &preparedReceipt
			resolver.result = readyResult()
			resolver.result.Approval.Status.ExecutionState = v1alpha1.RuntimeApprovalExecutionClaimed
			secondResponse := httptest.NewRecorder()
			server.ServeHTTP(secondResponse, newRequest())

			if secondResponse.Code != http.StatusServiceUnavailable {
				t.Fatalf("retry status = %d, want %d", secondResponse.Code, http.StatusServiceUnavailable)
			}
			if *upstreamCalls != 1 {
				t.Fatalf("upstream calls = %d, want exactly 1", *upstreamCalls)
			}
			if len(receiptStore.completed) != 1 {
				t.Fatalf("completion attempts = %d, want 1", len(receiptStore.completed))
			}
			if len(receiptStore.released) != 0 {
				t.Fatalf("released receipts = %#v, want none", receiptStore.released)
			}
		})
	}
}

func TestServerGate3FinalizationFailureRetryDoesNotDuplicateMutation(t *testing.T) {
	for _, operation := range []string{"store", "delete"} {
		t.Run(operation, func(t *testing.T) {
			server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
			defer upstream.Close()
			resolver := &captureGate3Resolver{result: readyResult()}
			executionStore := &memoryApprovalExecutionStore{finalizeErr: errors.New("finalization unavailable")}
			receiptStore := &memoryMutationReceiptStore{}
			withGate3(server, resolver, nil)
			WithRuntimeApprovalExecutionStore(executionStore)(server)
			WithMemoryMutationReceiptStore(receiptStore)(server)

			capability, err := issuer.Issue(binding)
			if err != nil {
				t.Fatal(err)
			}
			newRequest := func() *http.Request {
				if operation == "delete" {
					return deleteRequest(t, capability, binding.VolumeHandle)
				}
				return storeRequest(t, capability, binding.VolumeHandle)
			}
			firstResponse := httptest.NewRecorder()
			server.ServeHTTP(firstResponse, newRequest())
			if firstResponse.Code != http.StatusServiceUnavailable {
				t.Fatalf("first status = %d, want %d body=%s", firstResponse.Code, http.StatusServiceUnavailable, firstResponse.Body.String())
			}

			terminalReceipt := receiptStore.completed[0]
			receiptStore.existing = &terminalReceipt
			resolver.result = readyResult()
			resolver.result.Approval.Status.ExecutionState = v1alpha1.RuntimeApprovalExecutionClaimed
			secondResponse := httptest.NewRecorder()
			server.ServeHTTP(secondResponse, newRequest())

			if secondResponse.Code != http.StatusServiceUnavailable {
				t.Fatalf("retry status = %d, want %d", secondResponse.Code, http.StatusServiceUnavailable)
			}
			if *upstreamCalls != 1 {
				t.Fatalf("upstream calls = %d, want exactly 1", *upstreamCalls)
			}
			if len(receiptStore.completed) != 1 {
				t.Fatalf("completed receipts = %d, want 1", len(receiptStore.completed))
			}
		})
	}
}

func TestServerGate3ClaimFailureRetainsPreparationWithoutMemoryMutation(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	resolver := &captureGate3Resolver{result: readyResult()}
	executionStore := &memoryApprovalExecutionStore{claimErr: errors.New("claim conflict")}
	receiptStore := &memoryMutationReceiptStore{}
	withGate3(server, resolver, nil)
	WithRuntimeApprovalExecutionStore(executionStore)(server)
	WithMemoryMutationReceiptStore(receiptStore)(server)
	WithPreparationTokenFactory(func() (string, error) { return "preparation-token", nil })(server)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := storeRequest(t, capability, binding.VolumeHandle)
	request.Header.Set(MemoryInvocationIDHeader, "invocation-1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if *upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", *upstreamCalls)
	}
	want := []string{"prepare", "claim"}
	if !reflect.DeepEqual(executionStore.events, want) {
		t.Fatalf("execution events = %#v, want %#v", executionStore.events, want)
	}
	if len(receiptStore.released) != 0 {
		t.Fatalf("released receipts = %#v, want none after ambiguous claim", receiptStore.released)
	}
	if len(receiptStore.completed) != 0 {
		t.Fatalf("completed receipts = %#v, want none", receiptStore.completed)
	}
}

func TestServerGate3NonClaimedResultStopsBeforeMemoryMutation(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	failed := readyResult().Approval
	failed.Status.ExecutionState = v1alpha1.RuntimeApprovalExecutionFailed
	failed.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionApprovalExpiredBeforeClaim
	executionStore := &memoryApprovalExecutionStore{claimResult: &failed}
	receiptStore := &memoryMutationReceiptStore{}
	withGate3(server, &captureGate3Resolver{result: readyResult()}, nil)
	WithRuntimeApprovalExecutionStore(executionStore)(server)
	WithMemoryMutationReceiptStore(receiptStore)(server)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, storeRequest(t, capability, binding.VolumeHandle))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if *upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", *upstreamCalls)
	}
	if len(receiptStore.completed) != 0 || len(executionStore.events) != 2 {
		t.Fatalf("non-claimed result reached completion: receipts=%#v events=%#v", receiptStore.completed, executionStore.events)
	}
}

func TestServerGate3WrongClaimantIncarnationStopsBeforeMemoryMutation(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	server.claimant = v1alpha1.RuntimeApprovalClaimant{
		WorkloadUID:       "memory-gateway-pod",
		ContainerID:       "container-current",
		ProcessStartNonce: "process-current-0001",
	}
	wrongClaimant := server.claimant
	wrongClaimant.ContainerID = "container-previous"
	claimed := readyResult().Approval
	claimed.Status.ExecutionState = v1alpha1.RuntimeApprovalExecutionClaimed
	claimed.Status.FencingToken = 42
	claimed.Status.Claimant = &wrongClaimant
	executionStore := &memoryApprovalExecutionStore{claimResult: &claimed}
	receiptStore := &memoryMutationReceiptStore{}
	withGate3(server, &captureGate3Resolver{result: readyResult()}, nil)
	WithRuntimeApprovalExecutionStore(executionStore)(server)
	WithMemoryMutationReceiptStore(receiptStore)(server)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, storeRequest(t, capability, binding.VolumeHandle))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if *upstreamCalls != 0 || len(receiptStore.completed) != 0 {
		t.Fatalf("wrong claimant reached mutation: upstream=%d receipts=%#v", *upstreamCalls, receiptStore.completed)
	}
}

func TestServerGate3ReceiptPreparationFailureStopsBeforeClaimAndMutation(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	resolver := &captureGate3Resolver{result: readyResult()}
	executionStore := &memoryApprovalExecutionStore{}
	receiptStore := &memoryMutationReceiptStore{prepareErr: errors.New("receipt unavailable")}
	now := time.Date(2026, time.March, 12, 10, 0, 0, 0, time.UTC)
	withGate3(server, resolver, nil)
	WithRuntimeApprovalExecutionStore(executionStore)(server)
	WithMemoryMutationReceiptStore(receiptStore)(server)
	WithClock(func() time.Time { return now })(server)
	WithPreparationTokenFactory(func() (string, error) { return "preparation-token", nil })(server)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := storeRequest(t, capability, binding.VolumeHandle)
	request.Header.Set(MemoryInvocationIDHeader, "invocation-1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if *upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", *upstreamCalls)
	}
	if len(executionStore.events) != 0 {
		t.Fatalf("execution events = %#v, want none", executionStore.events)
	}
	if len(receiptStore.prepared) != 1 {
		t.Fatalf("prepared receipts = %d, want 1", len(receiptStore.prepared))
	}
	receipt := receiptStore.prepared[0]
	if receipt.Namespace != "team-a" || receipt.InvocationID != "invocation-1" || receipt.Operation != "store" || receipt.Binding != "binding-tag" {
		t.Fatalf("prepared receipt identity = %#v", receipt)
	}
	if receipt.PreparationToken != "preparation-token" || receipt.PreparationDeadline == nil || !receipt.PreparationDeadline.Equal(now.Add(runtimeapproval.MaxPreparationDeadline)) {
		t.Fatalf("prepared receipt preparation = %#v", receipt)
	}
}

func TestServerGate3PendingReturns202AndDeniedAuditOnly(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	audit := &policyAuditStore{}
	resolver := &captureGate3Resolver{result: pendingResult()}
	withGate3(server, resolver, audit)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, storeRequest(t, capability, binding.VolumeHandle))

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", response.Code, response.Body.String())
	}
	if *upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", *upstreamCalls)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1: %#v", len(audit.entries), audit.entries)
	}
	var decision map[string]any
	if err := json.Unmarshal(audit.entries[0].Details, &decision); err != nil {
		t.Fatalf("unmarshal audit details: %v", err)
	}
	if decision["outcome"] != "denied" || decision["reasonCode"] != "runtime_approval_pending" {
		t.Fatalf("audit decision = %s", audit.entries[0].Details)
	}
}

func TestServerGate3RejectedReturns403AndDeniedAudit(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	audit := &policyAuditStore{}
	resolver := &captureGate3Resolver{result: runtimeapproval.Gate3Result{
		State: runtimeapproval.Gate3StateRejected,
		Approval: v1alpha1.RuntimeApproval{
			ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "invocation-1"},
		},
	}}
	withGate3(server, resolver, audit)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, storeRequest(t, capability, binding.VolumeHandle))

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
	if *upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", *upstreamCalls)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	var decision map[string]any
	if err := json.Unmarshal(audit.entries[0].Details, &decision); err != nil {
		t.Fatalf("unmarshal audit details: %v", err)
	}
	if decision["outcome"] != "denied" || decision["reasonCode"] != "runtime_approval_rejected" {
		t.Fatalf("audit decision = %s", audit.entries[0].Details)
	}
}

func TestServerGate3ExpiredReturns403(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	audit := &policyAuditStore{}
	resolver := &captureGate3Resolver{result: runtimeapproval.Gate3Result{
		State: runtimeapproval.Gate3StateExpired,
		Approval: v1alpha1.RuntimeApproval{
			ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "invocation-1"},
		},
	}}
	withGate3(server, resolver, audit)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, storeRequest(t, capability, binding.VolumeHandle))

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
	if *upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", *upstreamCalls)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	var decision map[string]any
	if err := json.Unmarshal(audit.entries[0].Details, &decision); err != nil {
		t.Fatalf("unmarshal audit details: %v", err)
	}
	if decision["reasonCode"] != "runtime_approval_expired" {
		t.Fatalf("audit reasonCode = %v", decision["reasonCode"])
	}
}

func TestServerGate3ResolverErrorReturns503(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	audit := &policyAuditStore{}
	resolver := &captureGate3Resolver{err: errors.New("coordinator down")}
	withGate3(server, resolver, audit)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, storeRequest(t, capability, binding.VolumeHandle))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if *upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", *upstreamCalls)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	var decision map[string]any
	if err := json.Unmarshal(audit.entries[0].Details, &decision); err != nil {
		t.Fatalf("unmarshal audit details: %v", err)
	}
	if decision["reasonCode"] != "runtime_approval_unavailable" {
		t.Fatalf("audit reasonCode = %v", decision["reasonCode"])
	}
	approval := decision["approval"].(map[string]any)
	if approval["status"] != "unavailable" || approval["decisionRef"] != nil || resolver.calls != 1 {
		t.Fatalf("resolver failure must retain unavailable approval evidence: %s", audit.entries[0].Details)
	}
}

func TestServerGate3NotEvaluatedAfterGrantDenial(t *testing.T) {
	server, memoryStore, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	audit := &policyAuditStore{}
	resolver := &captureGate3Resolver{err: errors.New("must not resolve")}
	withGate3(server, resolver, audit)
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Delete(context.Background(), "MemoryGrant", binding.Namespace, "planner-memory"); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, storeRequest(t, capability, binding.VolumeHandle))
	if response.Code != http.StatusForbidden || resolver.calls != 0 || *upstreamCalls != 0 || len(audit.entries) != 1 {
		t.Fatalf("status=%d resolver=%d upstream=%d audit=%d", response.Code, resolver.calls, *upstreamCalls, len(audit.entries))
	}
	var decision observability.TrustDecisionV1
	if err := json.Unmarshal(audit.entries[0].Details, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Approval == nil || decision.Approval.Status != "not_evaluated" || decision.Approval.DecisionRef != nil || decision.Execution.TargetReached == nil || *decision.Execution.TargetReached {
		t.Fatalf("incorrect short-circuit evidence: %s", audit.entries[0].Details)
	}
}

func TestServerGate3SkippedForReadOnlyGet(t *testing.T) {
	server, memoryStore, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	// Seed an object so the upstream fixture returns 200 rather than 404.
	_ = memoryStore
	audit := &policyAuditStore{}
	// Pending resolver: if Gate 3 is invoked for a ReadOnly path this test fails.
	resolver := &captureGate3Resolver{result: pendingResult()}
	withGate3(server, resolver, audit)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/memory/get/"+binding.VolumeHandle+"/item-1", nil)
	r.Header.Set("Authorization", "Bearer "+capability)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, r)

	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0 for ReadOnly", resolver.calls)
	}
	if *upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1", *upstreamCalls)
	}
	if len(audit.entries) != 2 {
		t.Fatalf("audit entries = %d, want 2", len(audit.entries))
	}
	var authorized, completed observability.TrustDecisionV1
	if err := json.Unmarshal(audit.entries[0].Details, &authorized); err != nil {
		t.Fatalf("unmarshal audit details: %v", err)
	}
	if authorized.Outcome != "allowed" || authorized.ReasonCode != "authorized" || authorized.Execution.TargetReached == nil || *authorized.Execution.TargetReached {
		t.Fatalf("ReadOnly audit decision = %s", audit.entries[0].Details)
	}
	if err := json.Unmarshal(audit.entries[1].Details, &completed); err != nil {
		t.Fatalf("unmarshal completed audit details: %v", err)
	}
	if completed.Outcome != "completed" || completed.ReasonCode != "target_completed" || completed.Execution.Status != "completed" || completed.Execution.TargetReached == nil || !*completed.Execution.TargetReached {
		t.Fatalf("ReadOnly completed audit decision = %s", audit.entries[1].Details)
	}
}

func TestServerGate3UsesCallerSuppliedInvocationIDHeader(t *testing.T) {
	server, _, issuer, binding, upstream, _ := gatewayFixture(t)
	defer upstream.Close()
	resolver := &captureGate3Resolver{result: readyResult()}
	withGate3(server, resolver, nil)
	WithRuntimeApprovalExecutionStore(&memoryApprovalExecutionStore{})(server)
	WithMemoryMutationReceiptStore(&memoryMutationReceiptStore{})(server)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	r := storeRequest(t, capability, binding.VolumeHandle)
	r.Header.Set(MemoryInvocationIDHeader, "caller-supplied-id")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, r)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if resolver.lastReq.InvocationID != "caller-supplied-id" {
		t.Fatalf("invocation id = %q, want %q", resolver.lastReq.InvocationID, "caller-supplied-id")
	}
}

func TestServerGate3CanonicalizationFailureStopsBeforeMutation(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	resolver := &captureGate3Resolver{result: readyResult()}
	withGate3(server, resolver, nil)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"backend-a","volume_handle":"backend-a","content":"hello"}`))
	request.Header.Set("Authorization", "Bearer "+capability)
	request.Header.Set(MemoryInvocationIDHeader, "invocation-1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if resolver.calls != 0 || *upstreamCalls != 0 {
		t.Fatalf("canonicalization failure reached resolver or target: resolver=%d upstream=%d", resolver.calls, *upstreamCalls)
	}
}

func TestServerGate3ReadyWithoutExecutionStoreStopsBeforeMutation(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	withGate3(server, &captureGate3Resolver{result: readyResult()}, nil)

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, storeRequest(t, capability, binding.VolumeHandle))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if *upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", *upstreamCalls)
	}
}

func TestServerGate3RequiresCallerSuppliedInvocationID(t *testing.T) {
	for _, operation := range []string{"store", "delete"} {
		t.Run(operation, func(t *testing.T) {
			server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
			defer upstream.Close()
			resolver := &captureGate3Resolver{result: readyResult()}
			withGate3(server, resolver, nil)

			capability, err := issuer.Issue(binding)
			if err != nil {
				t.Fatal(err)
			}
			var request *http.Request
			if operation == "delete" {
				request = deleteRequest(t, capability, binding.VolumeHandle)
			} else {
				request = storeRequest(t, capability, binding.VolumeHandle)
			}
			request.Header.Del(MemoryInvocationIDHeader)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body.String())
			}
			if resolver.calls != 0 {
				t.Fatalf("resolver calls = %d, want 0", resolver.calls)
			}
			if *upstreamCalls != 0 {
				t.Fatalf("upstream calls = %d, want 0", *upstreamCalls)
			}
		})
	}
}
