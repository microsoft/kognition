// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/apiserver"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/runtime"
	"go.uber.org/zap"
)

func newHarnessWithApprovalBroker(t *testing.T) (*testHarness, *runtime.ApprovalBroker) {
	t.Helper()
	ms := newMemStore()
	logger, _ := zap.NewDevelopment()
	registry := cpi.NewRegistry()
	driver := newMockCPIDriver("test-provider")
	if err := registry.Register(driver); err != nil {
		t.Fatalf("register mock driver: %v", err)
	}

	broker := runtime.NewApprovalBroker()
	srv := apiserver.New(
		apiserver.Config{Addr: ":0"},
		ms,
		logger,
		apiserver.WithCPIRegistry(registry),
		apiserver.WithApprovalBroker(broker),
	)

	h := &testHarness{
		Server:   srv,
		Store:    ms,
		Registry: registry,
		Driver:   driver,
		Memory:   newMemBackend(),
		Logger:   logger,
	}
	return h, broker
}

func TestGovernance_ApprovalDecision_NoBroker(t *testing.T) {
	h := newTestHarness(t) // no broker wired

	resp := h.doReq(t, http.MethodPost, "/apis/kognition.dev/v1alpha1/approvals/abc123/decision", map[string]any{
		"state": "approved",
	})

	if resp.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestGovernance_ApprovalDecision_Success(t *testing.T) {
	h, broker := newHarnessWithApprovalBroker(t)

	// Start a waiter in the background
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	doneCh := make(chan runtime.ApprovalDecision, 1)
	go func() {
		d, _ := broker.Wait(ctx, runtime.ApprovalRequest{
			ID:        "req-1",
			Namespace: testNS,
			AgentName: "agent1",
			ToolName:  "tool1",
		})
		doneCh <- d
	}()

	// Give goroutine time to register
	time.Sleep(50 * time.Millisecond)

	resp := h.doReq(t, http.MethodPost, "/apis/kognition.dev/v1alpha1/approvals/req-1/decision", map[string]any{
		"state":  "approved",
		"reason": "looks good",
	})

	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	select {
	case d := <-doneCh:
		if d.State != runtime.ApprovalStateApproved {
			t.Fatalf("expected approved, got %s", d.State)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for decision")
	}
}

func TestGovernance_ApprovalDecision_NotFound(t *testing.T) {
	h, _ := newHarnessWithApprovalBroker(t)

	resp := h.doReq(t, http.MethodPost, "/apis/kognition.dev/v1alpha1/approvals/nonexistent/decision", map[string]any{
		"state": "approved",
	})

	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestGovernance_ApprovalDecision_InvalidState(t *testing.T) {
	h, _ := newHarnessWithApprovalBroker(t)

	resp := h.doReq(t, http.MethodPost, "/apis/kognition.dev/v1alpha1/approvals/req-2/decision", map[string]any{
		"state": "pending",
	})

	// "pending" is not a valid submission state → broker returns validation error → 422
	if resp.Code != http.StatusNotFound && resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 404 or 422, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestGovernance_ApprovalDecision_InvalidJSON(t *testing.T) {
	h, _ := newHarnessWithApprovalBroker(t)

	resp := h.doReqRaw(t, http.MethodPost, "/apis/kognition.dev/v1alpha1/approvals/req-3/decision", []byte("{invalid"))

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
	}
}
