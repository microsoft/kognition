// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package policygateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

type readableMemoryReceipts struct {
	memoryMutationReceiptStore
	receipt store.MemoryMutationReceipt
	err     error
}

func (receipts *readableMemoryReceipts) GetMemoryMutation(context.Context, string, string) (store.MemoryMutationReceipt, bool, error) {
	return receipts.receipt, true, receipts.err
}

func assertStoredMemoryResponse(t *testing.T, receipt store.MemoryMutationReceipt, response *httptest.ResponseRecorder) {
	t.Helper()
	var result struct {
		Version     int    `json:"version"`
		StatusCode  int    `json:"statusCode"`
		Payload     []byte `json:"payload"`
		ContentType string `json:"contentType"`
	}
	if err := json.Unmarshal(receipt.Result, &result); err != nil || result.Version != 2 || result.StatusCode != response.Code || !bytes.Equal(result.Payload, response.Body.Bytes()) || result.ContentType != response.Header().Get("Content-Type") {
		t.Fatalf("stored response does not match delivered response: %s", receipt.Result)
	}
}

func TestServerMemoryReplayValidatesTerminalEvidence(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*readableMemoryReceipts, *captureGate3Resolver, *memoryApprovalExecutionStore)
		status int
		body   string
	}{
		{name: "legacy success", status: 201, body: `{"id":"original"}`},
		{name: "legacy failure", change: func(receipts *readableMemoryReceipts, resolver *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			receipts.receipt.Status = store.MemoryMutationFailed
			receipts.receipt.Result = json.RawMessage(`{"statusCode":422,"body":{"code":"invalid"}}`)
			resolver.result.Approval.Status.ExecutionState = v1alpha1.RuntimeApprovalExecutionFailed
		}, status: 422, body: `{"code":"invalid"}`},
		{name: "binding mismatch", change: func(receipts *readableMemoryReceipts, _ *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			receipts.receipt.Binding = "other"
		}, status: 503},
		{name: "namespace mismatch", change: func(receipts *readableMemoryReceipts, _ *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			receipts.receipt.Namespace = "other"
		}, status: 503},
		{name: "invocation mismatch", change: func(receipts *readableMemoryReceipts, _ *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			receipts.receipt.InvocationID = "other"
		}, status: 503},
		{name: "operation mismatch", change: func(receipts *readableMemoryReceipts, _ *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			receipts.receipt.Operation = "delete"
		}, status: 503},
		{name: "missing completion", change: func(receipts *readableMemoryReceipts, _ *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			receipts.receipt.CompletedAt = nil
		}, status: 503},
		{name: "corrupt result", change: func(receipts *readableMemoryReceipts, _ *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			receipts.receipt.Result = json.RawMessage(`{`)
		}, status: 503},
		{name: "missing body", change: func(receipts *readableMemoryReceipts, _ *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			receipts.receipt.Result = json.RawMessage(`{"statusCode":201}`)
		}, status: 503},
		{name: "missing versioned payload", change: func(receipts *readableMemoryReceipts, _ *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			receipts.receipt.Result = json.RawMessage(`{"version":2,"statusCode":201}`)
		}, status: 503},
		{name: "invalid versioned payload", change: func(receipts *readableMemoryReceipts, _ *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			receipts.receipt.Result = json.RawMessage(`{"version":2,"statusCode":201,"payload":"%%%"}`)
		}, status: 503},
		{name: "unsupported version", change: func(receipts *readableMemoryReceipts, _ *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			receipts.receipt.Result = json.RawMessage(`{"version":3,"statusCode":201,"payload":""}`)
		}, status: 503},
		{name: "unsafe content type", change: func(receipts *readableMemoryReceipts, _ *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			receipts.receipt.Result = json.RawMessage(`{"version":2,"statusCode":201,"payload":"","contentType":"text/plain\r\nInjected: value"}`)
		}, status: 503},
		{name: "contradictory status", change: func(receipts *readableMemoryReceipts, _ *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			receipts.receipt.Status = store.MemoryMutationFailed
		}, status: 503},
		{name: "lookup failure", change: func(receipts *readableMemoryReceipts, _ *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			receipts.err = errors.New("unavailable")
		}, status: 503},
		{name: "unclaimed approval", change: func(_ *readableMemoryReceipts, resolver *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			resolver.result.Approval.Status.ExecutionState = ""
		}, status: 503},
		{name: "recover finalization", change: func(_ *readableMemoryReceipts, resolver *captureGate3Resolver, _ *memoryApprovalExecutionStore) {
			resolver.result.Approval.Status.ExecutionState = v1alpha1.RuntimeApprovalExecutionClaimed
			resolver.result.Approval.Status.FencingToken = 42
		}, status: 201, body: `{"id":"original"}`},
		{name: "finalization failure", change: func(_ *readableMemoryReceipts, resolver *captureGate3Resolver, execution *memoryApprovalExecutionStore) {
			resolver.result.Approval.Status.ExecutionState = v1alpha1.RuntimeApprovalExecutionClaimed
			resolver.result.Approval.Status.FencingToken = 42
			execution.finalizeErr = errors.New("unavailable")
		}, status: 503},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server, _, issuer, binding, upstream, calls := gatewayFixture(t)
			defer upstream.Close()
			resolver := &captureGate3Resolver{result: readyResult()}
			resolver.result.Approval.Status.ExecutionState = v1alpha1.RuntimeApprovalExecutionCompleted
			completedAt := time.Now()
			receipts := &readableMemoryReceipts{receipt: store.MemoryMutationReceipt{
				Namespace: "team-a", InvocationID: "invocation-1", Binding: "binding-tag", Operation: "store",
				Status: store.MemoryMutationSucceeded, CompletedAt: &completedAt, Result: json.RawMessage(`{"statusCode":201,"body":{"id":"original"}}`),
			}}
			execution := &memoryApprovalExecutionStore{}
			if scenario.change != nil {
				scenario.change(receipts, resolver, execution)
			}
			withGate3(server, resolver, &policyAuditStore{})
			WithRuntimeApprovalExecutionStore(execution)(server)
			WithMemoryMutationReceiptStore(receipts)(server)
			capability, err := issuer.Issue(binding)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, storeRequest(t, capability, binding.VolumeHandle))
			if response.Code != scenario.status || (scenario.body != "" && response.Body.String() != scenario.body) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if *calls != 0 || len(receipts.prepared) != 0 || len(receipts.completed) != 0 {
				t.Fatalf("replay attempted execution: calls=%d prepared=%d completed=%d", *calls, len(receipts.prepared), len(receipts.completed))
			}
			if resolver.result.Approval.Status.ExecutionState == v1alpha1.RuntimeApprovalExecutionClaimed {
				if len(execution.events) != 1 || execution.events[0] != "finalize:Succeeded" {
					t.Fatalf("unexpected recovery operations: %v", execution.events)
				}
			} else if len(execution.events) != 0 {
				t.Fatalf("terminal replay changed execution state: %v", execution.events)
			}
		})
	}
}

func TestServerMemoryReplayPreservesWireResponse(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		for _, body := range []string{"{ \"id\": \"original\" }\n", `"JSON string"`, "plain text\n", ""} {
			t.Run(method+"/"+body, func(t *testing.T) {
				server, _, issuer, binding, upstream, calls := gatewayFixture(t)
				defer upstream.Close()
				resolver := &captureGate3Resolver{result: readyResult()}
				audit := &policyAuditStore{}
				withGate3(server, resolver, audit)
				receipts := &readableMemoryReceipts{err: errors.New("not reading yet")}
				WithMemoryMutationReceiptStore(&receipts.memoryMutationReceiptStore)(server)
				WithRuntimeApprovalExecutionStore(&memoryApprovalExecutionStore{})(server)
				server.httpClient.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
					*calls++
					return &http.Response{StatusCode: 201, Body: io.NopCloser(bytes.NewBufferString(body)), Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
				})
				capability, err := issuer.Issue(binding)
				if err != nil {
					t.Fatal(err)
				}
				request := func() *http.Request {
					if method == http.MethodDelete {
						return deleteRequest(t, capability, binding.VolumeHandle)
					}
					return storeRequest(t, capability, binding.VolumeHandle)
				}
				first := httptest.NewRecorder()
				server.ServeHTTP(first, request())
				if first.Code != 201 || len(receipts.completed) != 1 {
					t.Fatalf("initial response: %d %s", first.Code, first.Body.String())
				}
				receipts.receipt, receipts.err = receipts.completed[0], nil
				WithMemoryMutationReceiptStore(receipts)(server)
				resolver.result.Approval.Status.ExecutionState = v1alpha1.RuntimeApprovalExecutionCompleted
				replayed := httptest.NewRecorder()
				server.ServeHTTP(replayed, request())
				if replayed.Code != first.Code || replayed.Body.String() != body || replayed.Header().Get("Content-Type") != first.Header().Get("Content-Type") || *calls != 1 {
					t.Fatalf("replay differs: %d %q calls=%d", replayed.Code, replayed.Body.String(), *calls)
				}
				if len(audit.entries) != 4 {
					t.Fatalf("audit entries=%d, want 4", len(audit.entries))
				}
				var decision observability.TrustDecisionV1
				if err := json.Unmarshal(audit.entries[3].Details, &decision); err != nil || decision.Outcome != "replayed" || decision.ReasonCode != "replay_detected" || decision.Execution.Status != "not_started" || decision.Execution.TargetReached == nil || *decision.Execution.TargetReached {
					t.Fatalf("incorrect replay evidence: %s", audit.entries[3].Details)
				}
			})
		}
	}
}
