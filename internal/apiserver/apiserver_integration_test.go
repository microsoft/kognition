// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package apiserver_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// These tests exercise the live API server through HTTP.
// Run with:
//
//	APISERVER_URL=http://localhost:8080 go test -tags=integration -v -run TestLive ./internal/apiserver/
//
// Prerequisites:
//   - A port-forward (or direct route) to the kognition-apiserver is active.
//   - At least one Agent resource exists in the target namespace.
//
// Environment variables:
//
//	APISERVER_URL - Base URL of the API server (default: http://localhost:8080)
//	TEST_NAMESPACE - Namespace to query (default: "default")
//	TEST_AGENT     - Agent name for chat test (default: "foundry-hello")

func baseURL(t *testing.T) string {
	t.Helper()
	// Live tests require an explicit target. When APISERVER_URL is unset there is
	// no server to talk to (e.g. in CI or a plain `go test -tags integration`
	// run), so skip rather than hard-fail with connection-refused.
	url := os.Getenv("APISERVER_URL")
	if url == "" {
		t.Skip("APISERVER_URL not set; skipping live API server test " +
			"(set APISERVER_URL to a running kognition-apiserver to enable)")
	}
	return strings.TrimRight(url, "/")
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func TestLive_Healthz(t *testing.T) {
	resp, err := http.Get(baseURL(t) + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("expected body 'ok', got %q", string(body))
	}
	t.Log("GET /healthz → 200 ok")
}

func TestLive_Readyz(t *testing.T) {
	resp, err := http.Get(baseURL(t) + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("expected body 'ok', got %q", string(body))
	}
	t.Log("GET /readyz → 200 ok")
}

func TestLive_ListAgents(t *testing.T) {
	ns := envOrDefault("TEST_NAMESPACE", "default")
	url := fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/namespaces/%s/agents", baseURL(t), ns)

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET agents: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Kind  string `json:"kind"`
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if result.Kind != "AgentList" {
		t.Errorf("expected kind AgentList, got %q", result.Kind)
	}
	if len(result.Items) == 0 {
		t.Fatal("expected at least one agent, got empty list")
	}

	for _, ag := range result.Items {
		t.Logf("  agent=%s phase=%s", ag.Metadata.Name, ag.Status.Phase)
	}
}

func TestLive_GetAgent(t *testing.T) {
	ns := envOrDefault("TEST_NAMESPACE", "default")
	agent := envOrDefault("TEST_AGENT", "foundry-hello")
	url := fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/namespaces/%s/agents/%s", baseURL(t), ns, agent)

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET agent: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if result.Kind != "Agent" {
		t.Errorf("expected kind Agent, got %q", result.Kind)
	}
	if result.Metadata.Name != agent {
		t.Errorf("expected name %q, got %q", agent, result.Metadata.Name)
	}
	t.Logf("GET agent %s → phase=%s", agent, result.Status.Phase)
}

func TestLive_ChatEndpoint(t *testing.T) {
	ns := envOrDefault("TEST_NAMESPACE", "default")
	agent := envOrDefault("TEST_AGENT", "foundry-hello")
	url := fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/namespaces/%s/agents/%s/chat",
		baseURL(t), ns, agent)

	payload := `{"message":"Say hello in exactly one word"}`
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post(url, "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST chat: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	// We accept either 200 (success) or 502 (model auth failure) as evidence
	// the full pipeline executed. A 503 or 404 would indicate the runtime isn't wired.
	switch resp.StatusCode {
	case http.StatusOK:
		var chatResp struct {
			SessionID string `json:"sessionID"`
			Response  string `json:"response"`
		}
		if err := json.Unmarshal(body, &chatResp); err != nil {
			t.Fatalf("decode chat response: %v", err)
		}
		t.Logf("Chat success: sessionID=%s response=%q", chatResp.SessionID, chatResp.Response)
	case http.StatusBadGateway:
		// 502 = the runtime invoked the CPI driver which called the model but auth failed.
		// This still proves the full pipeline is wired correctly.
		t.Logf("Chat got 502 (model auth issue — pipeline is wired): %s", body)
	default:
		t.Fatalf("unexpected status %d: %s", resp.StatusCode, body)
	}
}
