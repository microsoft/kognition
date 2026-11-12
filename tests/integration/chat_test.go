// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/apiserver"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"go.uber.org/zap"
)

// TestChat_Success verifies end-to-end chat: agent → model → modelclass → CPI driver.
func TestChat_Success(t *testing.T) {
	h := newTestHarness(t)

	// Seed required resources: ModelClass → Model → Agent (Ready).
	seedResource(t, h, "ModelClass", "", "openai-class", makeModelClass("openai-class", "", "test-provider"))
	seedResource(t, h, "Model", testNS, "gpt4", makeModel("gpt4", testNS, "openai-class", "gpt-4o", "https://api.example.com"))
	agent := makeAgent("helper-bot", testNS, "gpt4", v1alpha1.PhaseReady)
	agent.Spec.Instructions.Inline = "You are a helpful assistant."
	seedResource(t, h, "Agent", testNS, "helper-bot", agent)

	// Configure the mock driver response.
	h.Driver.completionFn = func(_ context.Context, req *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
		if req.Model != "gpt-4o" {
			t.Errorf("expected model gpt-4o, got %s", req.Model)
		}
		// Verify system prompt was passed.
		if len(req.Messages) < 2 || req.Messages[0].Role != "system" {
			t.Errorf("expected system+user messages, got %d messages", len(req.Messages))
		}
		return &cpi.CompletionResponse{
			Content:      "Hello! How can I help?",
			FinishReason: "stop",
			Usage:        cpi.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, Reported: true},
		}, nil
	}

	body := apiserver.ChatRequest{Message: "Hi there"}
	rr := h.doReq(t, http.MethodPost, apiBase+"/agents/helper-bot/chat", body)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp apiserver.ChatResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Response != "Hello! How can I help?" {
		t.Errorf("unexpected response: %s", resp.Response)
	}
	if resp.SessionID == "" {
		t.Error("expected sessionID to be assigned")
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 15 {
		t.Errorf("unexpected usage: %+v", resp.Usage)
	}
}

// TestChat_SessionReuse verifies that a provided sessionID is echoed back.
func TestChat_SessionReuse(t *testing.T) {
	h := newTestHarness(t)
	seedResource(t, h, "ModelClass", "", "mc", makeModelClass("mc", "", "test-provider"))
	seedResource(t, h, "Model", testNS, "m1", makeModel("m1", testNS, "mc", "model-1", "http://x"))
	seedResource(t, h, "Agent", testNS, "a1", makeAgent("a1", testNS, "m1", v1alpha1.PhaseReady))

	body := map[string]string{"message": "hello", "sessionID": "my-session-123"}
	rr := h.doReq(t, http.MethodPost, apiBase+"/agents/a1/chat", body)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp apiserver.ChatResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.SessionID != "my-session-123" {
		t.Errorf("expected session reuse, got %s", resp.SessionID)
	}
}

func TestChat_ModelGrantDeniesByDefaultAndAllowsExactGrant(t *testing.T) {
	h := newTestHarness(t)
	grants := controller.NewToolGrantEngine(h.Store, h.Logger)
	h.Server = apiserver.New(
		apiserver.Config{Addr: ":0"},
		h.Store,
		h.Logger,
		apiserver.WithCPIRegistry(h.Registry),
		apiserver.WithModelAccessAuthorizer(controller.NewInvocationPolicyEngine(grants, nil)),
	)
	seedResource(t, h, "ModelClass", "", "grant-class", makeModelClass("grant-class", "", "test-provider"))
	seedResource(t, h, "Model", testNS, "granted-model", makeModel("granted-model", testNS, "grant-class", "model-id", "https://model.example"))
	seedResource(t, h, "Agent", testNS, "granted-agent", makeAgent("granted-agent", testNS, "granted-model", v1alpha1.PhaseReady))

	denied := h.doReq(t, http.MethodPost, apiBase+"/agents/granted-agent/chat", apiserver.ChatRequest{Message: "hello"})
	if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), "model access denied") {
		t.Fatalf("chat without ModelGrant = %d %s, want 403", denied.Code, denied.Body.String())
	}
	if len(h.Driver.completions) != 0 {
		t.Fatalf("CPI driver received %d requests after denied chat", len(h.Driver.completions))
	}

	grant := v1alpha1.ModelGrant{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ModelGrant"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "granted-agent-model", Namespace: testNS},
		Spec: v1alpha1.ModelGrantSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "granted-agent"},
			ModelRef: v1alpha1.LocalObjectReference{Name: "granted-model"},
		},
	}
	seedResource(t, h, "ModelGrant", testNS, grant.ObjectMeta.Name, grant)

	allowed := h.doReq(t, http.MethodPost, apiBase+"/agents/granted-agent/chat", apiserver.ChatRequest{Message: "hello"})
	if allowed.Code != http.StatusOK {
		t.Fatalf("chat with matching ModelGrant = %d %s, want 200", allowed.Code, allowed.Body.String())
	}
	if len(h.Driver.completions) != 1 {
		t.Fatalf("CPI driver received %d requests after granted chat, want 1", len(h.Driver.completions))
	}
}

// TestChat_AgentNotFound returns 404 when agent doesn't exist.
func TestChat_AgentNotFound(t *testing.T) {
	h := newTestHarness(t)
	body := apiserver.ChatRequest{Message: "hi"}
	rr := h.doReq(t, http.MethodPost, apiBase+"/agents/nonexistent/chat", body)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

// TestChat_AgentNotReady returns 503 when agent is not in Ready phase.
func TestChat_AgentNotReady(t *testing.T) {
	h := newTestHarness(t)
	seedResource(t, h, "ModelClass", "", "mc", makeModelClass("mc", "", "test-provider"))
	seedResource(t, h, "Model", testNS, "m1", makeModel("m1", testNS, "mc", "model-1", "http://x"))
	seedResource(t, h, "Agent", testNS, "a1", makeAgent("a1", testNS, "m1", v1alpha1.PhasePending))

	body := apiserver.ChatRequest{Message: "hi"}
	rr := h.doReq(t, http.MethodPost, apiBase+"/agents/a1/chat", body)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestChat_EmptyMessage returns 400.
func TestChat_EmptyMessage(t *testing.T) {
	h := newTestHarness(t)
	body := apiserver.ChatRequest{Message: ""}
	rr := h.doReq(t, http.MethodPost, apiBase+"/agents/a1/chat", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestChat_ModelNotFound returns 422 when model reference is broken.
func TestChat_ModelNotFound(t *testing.T) {
	h := newTestHarness(t)
	seedResource(t, h, "Agent", testNS, "a1", makeAgent("a1", testNS, "missing-model", v1alpha1.PhaseReady))

	body := apiserver.ChatRequest{Message: "hi"}
	rr := h.doReq(t, http.MethodPost, apiBase+"/agents/a1/chat", body)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestChat_DriverError returns 502 when model inference fails.
func TestChat_DriverError(t *testing.T) {
	h := newTestHarness(t)
	seedResource(t, h, "ModelClass", "", "mc", makeModelClass("mc", "", "test-provider"))
	seedResource(t, h, "Model", testNS, "m1", makeModel("m1", testNS, "mc", "model-1", "http://x"))
	seedResource(t, h, "Agent", testNS, "a1", makeAgent("a1", testNS, "m1", v1alpha1.PhaseReady))

	h.Driver.completionFn = func(_ context.Context, _ *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
		return nil, errors.New("upstream timeout")
	}

	body := apiserver.ChatRequest{Message: "hi"}
	rr := h.doReq(t, http.MethodPost, apiBase+"/agents/a1/chat", body)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestChat_NoCPIRegistry returns 503 when no CPI drivers are configured.
func TestChat_NoCPIRegistry(t *testing.T) {
	// Create a harness without CPI registry.
	ms := newMemStore()
	logger, _ := zap.NewDevelopment()
	srv := apiserver.New(apiserver.Config{Addr: ":0"}, ms, logger)
	h := &testHarness{Server: srv, Store: ms, Logger: logger}

	body := apiserver.ChatRequest{Message: "hi"}
	rr := h.doReq(t, http.MethodPost, apiBase+"/agents/a1/chat", body)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rr.Code, rr.Body.String())
	}
}
