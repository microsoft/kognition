// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/apiserver"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	testNS  = "default"
	apiBase = "/apis/kognition.dev/v1alpha1/namespaces/" + testNS
)

// TestAPI_CreateAgent validates POST creates a resource and returns 201.
func TestAPI_CreateAgent(t *testing.T) {
	h := newTestHarness(t)
	agent := makeAgent("alpha", testNS, "gpt-4", v1alpha1.PhaseReady)

	rec := h.doReq(t, "POST", apiBase+"/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	meta := got["metadata"].(map[string]any)
	if meta["name"] != "alpha" {
		t.Fatalf("expected name alpha, got %v", meta["name"])
	}
	if meta["uid"] == nil || meta["uid"] == "" {
		t.Fatal("uid should be assigned")
	}
}

// TestAPI_CreateDuplicate validates 409 on duplicate creation.
func TestAPI_CreateDuplicate(t *testing.T) {
	h := newTestHarness(t)
	agent := makeAgent("dup", testNS, "gpt-4", v1alpha1.PhaseReady)

	h.doReq(t, "POST", apiBase+"/agents", agent)
	rec := h.doReq(t, "POST", apiBase+"/agents", agent)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAPI_GetAgent validates GET retrieves an existing resource.
func TestAPI_GetAgent(t *testing.T) {
	h := newTestHarness(t)
	agent := makeAgent("beta", testNS, "gpt-4", v1alpha1.PhaseReady)
	h.doReq(t, "POST", apiBase+"/agents", agent)

	rec := h.doReq(t, "GET", apiBase+"/agents/beta", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	meta := got["metadata"].(map[string]any)
	if meta["name"] != "beta" {
		t.Fatalf("expected name beta, got %v", meta["name"])
	}
}

// TestAPI_GetNotFound validates 404 for non-existent resource.
func TestAPI_GetNotFound(t *testing.T) {
	h := newTestHarness(t)
	rec := h.doReq(t, "GET", apiBase+"/agents/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// TestAPI_ListAgents validates LIST returns all resources in namespace.
func TestAPI_ListAgents(t *testing.T) {
	h := newTestHarness(t)
	h.doReq(t, "POST", apiBase+"/agents", makeAgent("a1", testNS, "m", v1alpha1.PhaseReady))
	h.doReq(t, "POST", apiBase+"/agents", makeAgent("a2", testNS, "m", v1alpha1.PhaseReady))

	rec := h.doReq(t, "GET", apiBase+"/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var list map[string]any
	json.Unmarshal(rec.Body.Bytes(), &list)
	items := list["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
}

// TestAPI_UpdateAgent validates PUT with correct resourceVersion.
func TestAPI_UpdateAgent(t *testing.T) {
	h := newTestHarness(t)
	agent := makeAgent("upd", testNS, "gpt-4", v1alpha1.PhaseReady)
	h.doReq(t, "POST", apiBase+"/agents", agent)

	// Mutate and update
	agent.Spec.Instructions.Inline = "Updated instructions"
	agent.ObjectMeta.ResourceVersion = 1
	rec := h.doReq(t, "PUT", apiBase+"/agents/upd", agent)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	meta := got["metadata"].(map[string]any)
	// resourceVersion should be incremented
	rv := meta["resourceVersion"].(float64)
	if rv != 2 {
		t.Fatalf("expected resourceVersion 2, got %v", rv)
	}
}

// TestAPI_UpdateConflict validates 409 on stale resourceVersion.
func TestAPI_UpdateConflict(t *testing.T) {
	h := newTestHarness(t)
	agent := makeAgent("conf", testNS, "gpt-4", v1alpha1.PhaseReady)
	h.doReq(t, "POST", apiBase+"/agents", agent)

	// Try to update with wrong rv
	agent.ObjectMeta.ResourceVersion = 99
	rec := h.doReq(t, "PUT", apiBase+"/agents/conf", agent)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAPI_DeleteAgent validates DELETE removes the resource.
func TestAPI_DeleteAgent(t *testing.T) {
	h := newTestHarness(t)
	h.doReq(t, "POST", apiBase+"/agents", makeAgent("del", testNS, "m", v1alpha1.PhaseReady))

	rec := h.doReq(t, "DELETE", apiBase+"/agents/del", nil)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("expected 200 or 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify gone
	rec = h.doReq(t, "GET", apiBase+"/agents/del", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", rec.Code)
	}
}

// TestAPI_DeleteNotFound validates 404 when deleting non-existent resource.
func TestAPI_DeleteNotFound(t *testing.T) {
	h := newTestHarness(t)
	rec := h.doReq(t, "DELETE", apiBase+"/agents/ghost", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// TestAPI_HealthEndpoints validates /healthz and /readyz.
func TestAPI_HealthEndpoints(t *testing.T) {
	h := newTestHarness(t)

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := h.doReq(t, "GET", path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", path, rec.Code)
		}
	}
}

func TestAPI_AuthenticationProtectsResourcesButNotHealth(t *testing.T) {
	h := newTestHarness(t)
	h.Server = apiserver.New(
		apiserver.Config{Addr: ":0"},
		h.Store,
		h.Logger,
		apiserver.WithCPIRegistry(h.Registry),
		apiserver.WithAuthenticator(auth.NewStaticTokenAuthenticator(map[string]string{"valid-token": "integration-user"}), nil),
	)

	health := httptest.NewRecorder()
	h.Server.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", health.Code)
	}

	resourcePath := apiBase + "/agents"
	missing := httptest.NewRecorder()
	h.Server.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, resourcePath, nil))
	if missing.Code != http.StatusUnauthorized || missing.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("missing token response = %d %q, want 401 Bearer", missing.Code, missing.Header().Get("WWW-Authenticate"))
	}

	invalidRequest := httptest.NewRequest(http.MethodGet, resourcePath, nil)
	invalidRequest.Header.Set("Authorization", "Bearer invalid-token")
	invalid := httptest.NewRecorder()
	h.Server.ServeHTTP(invalid, invalidRequest)
	if invalid.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d, want 401", invalid.Code)
	}

	validRequest := httptest.NewRequest(http.MethodGet, resourcePath, nil)
	validRequest.Header.Set("Authorization", "Bearer valid-token")
	valid := httptest.NewRecorder()
	h.Server.ServeHTTP(valid, validRequest)
	if valid.Code != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200: %s", valid.Code, valid.Body.String())
	}
}

// TestAPI_MultiResourceTypes validates CRUD works for many resource types.
func TestAPI_MultiResourceTypes(t *testing.T) {
	h := newTestHarness(t)

	resources := []struct {
		plural string
		obj    any
	}{
		{"models", makeModel("m1", testNS, "openai", "gpt-4", "https://api.openai.com")},
		{"modelclasses", makeModelClass("openai", testNS, "test-provider")},
		{"tools", makeTool("t1", testNS, "MCP", "http://tool.local")},
		{"intentgateways", makeIntentGateway("gw1", testNS, nil, v1alpha1.PhasePending)},
	}

	for _, tc := range resources {
		rec := h.doReq(t, "POST", apiBase+"/"+tc.plural, tc.obj)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST %s: expected 201, got %d: %s", tc.plural, rec.Code, rec.Body.String())
		}
	}
}

// TestAPI_Revisions validates revision create and list.
func TestAPI_Revisions(t *testing.T) {
	h := newTestHarness(t)
	h.doReq(t, "POST", apiBase+"/agents", makeAgent("rev", testNS, "m", v1alpha1.PhaseReady))

	// Seed a revision directly in store
	h.Store.CreateRevision(context.Background(), "Agent", testNS, "rev", 1, []byte(`{"spec":{}}`), []byte(`{"data":"v1"}`))
	h.Store.CreateRevision(context.Background(), "Agent", testNS, "rev", 2, []byte(`{"spec":{}}`), []byte(`{"data":"v2"}`))

	rec := h.doReq(t, "GET", apiBase+"/agents/rev/revisions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct{ Items []any }
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Items) != 2 {
		t.Fatalf("expected 2 revisions, got %d", len(resp.Items))
	}
}

// TestAPI_Events validates event append and list.
func TestAPI_Events(t *testing.T) {
	h := newTestHarness(t)
	h.doReq(t, "POST", apiBase+"/agentinstances", map[string]any{
		"apiVersion": v1alpha1.APIVersion,
		"kind":       "AgentInstance",
		"metadata":   map[string]any{"name": "inst1"},
		"spec":       map[string]any{},
	})

	// Seed events directly
	h.Store.AppendEvent(context.Background(), "AgentInstance", testNS, "inst1", store.InstanceEvent{
		Timestamp: "2025-01-01T00:00:00Z",
		Type:      "Normal",
		Reason:    "Started",
		Message:   "Instance started",
	})

	rec := h.doReq(t, "GET", apiBase+"/agentinstances/inst1/events", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var evResp struct{ Items []any }
	json.Unmarshal(rec.Body.Bytes(), &evResp)
	if len(evResp.Items) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evResp.Items))
	}
}
