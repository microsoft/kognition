// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package foundry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
)

func intPtr(value int) *int { return &value }

var _ cpi.ExactTokenCounter = (*Driver)(nil)

func TestCountInputTokens(t *testing.T) {
	driver := New(nil)
	request := &cpi.CompletionRequest{
		Model:      "gpt-4-0613",
		Messages:   []cpi.Message{{Role: "user", Content: "health check"}},
		Parameters: map[string]string{"max_tokens": "16"},
	}

	count, err := driver.CountInputTokens(request)
	if err != nil {
		t.Fatalf("CountInputTokens() error: %v", err)
	}
	if count <= 0 {
		t.Fatalf("CountInputTokens() = %d, want a positive count", count)
	}

	request.Model = "production-deployment"
	if _, err := driver.CountInputTokens(request); err == nil || !strings.Contains(err.Error(), "exact tokenizer unavailable") {
		t.Fatalf("CountInputTokens() error = %v, want tokenizer unavailable", err)
	}
}

func TestCompleteReasoningDeployment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if _, exists := request["max_tokens"]; exists {
			t.Error("reasoning deployment received unsupported max_tokens")
		}
		if got := string(request["max_completion_tokens"]); got != "256" {
			t.Errorf("max_completion_tokens = %s, want 256", got)
		}
		if _, exists := request["temperature"]; exists {
			t.Error("reasoning deployment received unsupported temperature")
		}
		if got := string(request["reasoning_effort"]); got != `"none"` {
			t.Errorf("reasoning_effort = %s, want none for GPT-6 function tools", got)
		}
		if string(request["tools"]) == "" {
			t.Error("tool definitions were lost")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ready"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	}))
	defer server.Close()

	response, err := New(server.Client()).Complete(context.Background(), &cpi.CompletionRequest{
		Model:      "gpt-6-luna",
		Endpoint:   server.URL,
		Messages:   []cpi.Message{{Role: "user", Content: "ready"}},
		Parameters: map[string]string{"max_tokens": "256", "temperature": "0.7"},
		Tools:      []cpi.ToolDefinition{{Name: "lookup", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != "ready" || !response.Usage.Reported {
		t.Fatalf("completion = %+v, want content and reported usage", response)
	}
}

func TestParseChatResponseRequiresComponentUsageFields(t *testing.T) {
	for _, usage := range []*chatUsage{
		{},
		{PromptTokens: intPtr(1)},
		{PromptTokens: intPtr(2), CompletionTokens: intPtr(1), TotalTokens: intPtr(0)},
		{PromptTokens: intPtr(2), CompletionTokens: intPtr(1), TotalTokens: intPtr(1)},
	} {
		if got := parseChatResponse(&chatCompletionResponse{Usage: usage}); got.Usage.Reported {
			t.Fatalf("usage %+v was reported", usage)
		}
	}
}

func TestComplete_MockFoundry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			// Verify auth header.
			if key := r.Header.Get("api-key"); key != "test-key" {
				t.Errorf("api-key header = %q, want %q", key, "test-key")
			}

			var req chatRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if req.Model != "gpt-4o" {
				t.Errorf("model = %q, want %q", req.Model, "gpt-4o")
			}

			resp := chatCompletionResponse{
				Choices: []chatChoice{{
					Message:      chatMessage{Role: "assistant", Content: "Hello from Foundry!"},
					FinishReason: "stop",
				}},
				Usage: &chatUsage{PromptTokens: intPtr(10), CompletionTokens: intPtr(5), TotalTokens: intPtr(15)},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	driver := New(server.Client())
	ctx := context.Background()

	resp, err := driver.Complete(ctx, &cpi.CompletionRequest{
		Model:    "gpt-4o",
		Endpoint: server.URL,
		Creds:    &cpi.Credentials{Type: "api-key", Data: map[string]string{"apiKey": "test-key"}},
		Messages: []cpi.Message{
			{Role: "user", Content: "Hello"},
		},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	if resp.Content != "Hello from Foundry!" {
		t.Errorf("Content = %q, want %q", resp.Content, "Hello from Foundry!")
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, "stop")
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("Usage.TotalTokens = %d, want 15", resp.Usage.TotalTokens)
	}
}

func TestChatEndpoint(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   string
		deployment string
		want       string
	}{
		{"generic endpoint", "https://foundry.example", "gpt-4o", "https://foundry.example/chat/completions"},
		{"Cognitive Services endpoint", "https://resource.cognitiveservices.azure.com/", "gpt-4o", "https://resource.cognitiveservices.azure.com/openai/deployments/gpt-4o/chat/completions"},
		{"project endpoint", "https://resource.services.ai.azure.com/api/projects/sample", "gpt-4.1", "https://resource.cognitiveservices.azure.com/openai/deployments/gpt-4.1/chat/completions"},
		{"deployment endpoint", "https://resource.openai.azure.com/openai/deployments/gpt-4o", "ignored", "https://resource.openai.azure.com/openai/deployments/gpt-4o/chat/completions"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ChatEndpoint(test.endpoint, test.deployment)
			if err != nil || got != test.want {
				t.Fatalf("ChatEndpoint() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestComplete_WithApiVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify api-version query parameter is set.
		if v := r.URL.Query().Get("api-version"); v != "2024-02-15-preview" {
			t.Errorf("api-version = %q, want %q", v, "2024-02-15-preview")
		}

		resp := chatCompletionResponse{
			Choices: []chatChoice{{
				Message:      chatMessage{Role: "assistant", Content: "ok"},
				FinishReason: "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	driver := New(server.Client())
	_, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model:      "gpt-4o",
		Endpoint:   server.URL,
		Messages:   []cpi.Message{{Role: "user", Content: "hi"}},
		Parameters: map[string]string{"api-version": "2024-02-15-preview"},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
}

func TestComplete_SerializesConversationAndParameters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("api-version"); got != "2024-10-21" {
			t.Errorf("api-version = %q, want %q", got, "2024-10-21")
		}

		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if request.Temperature == nil || *request.Temperature != 0.25 {
			t.Errorf("temperature = %v, want 0.25", request.Temperature)
		}
		if request.MaxTokens == nil || *request.MaxTokens != 128 {
			t.Errorf("max_tokens = %v, want 128", request.MaxTokens)
		}
		if len(request.Messages) != 2 {
			t.Fatalf("messages = %d, want 2", len(request.Messages))
		}
		toolCall := request.Messages[0].ToolCalls
		if len(toolCall) != 1 || toolCall[0].ID != "call-1" || toolCall[0].Function.Name != "lookup" || toolCall[0].Function.Arguments != `{"id":"42"}` {
			t.Fatalf("assistant tool calls = %#v", toolCall)
		}
		if request.Messages[1].ToolCallID != "call-1" || request.Messages[1].Content != "result" {
			t.Fatalf("tool result message = %#v", request.Messages[1])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	_, err := New(server.Client()).Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "gpt-4o",
		Endpoint: server.URL,
		Messages: []cpi.Message{
			{
				Role: "assistant",
				ToolCalls: []cpi.ToolCall{{
					ID:        "call-1",
					Name:      "lookup",
					Arguments: `{"id":"42"}`,
				}},
			},
			{Role: "tool", Content: "result", ToolCallID: "call-1"},
		},
		Parameters: map[string]string{
			"apiVersion":  "2024-10-21",
			"temperature": "0.25",
			"max_tokens":  "128",
		},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
}

func TestProbe_InfoEndpointHealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/info" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"model_name":"gpt-4o","model_version":"2024-05-01"}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	driver := New(server.Client())
	result, err := driver.Probe(context.Background(), &v1alpha1.ModelSpec{
		ModelID:  "gpt-4o",
		Endpoint: server.URL,
	}, &cpi.Credentials{Type: "api-key", Data: map[string]string{"apiKey": "test-key"}})
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	if !result.Available {
		t.Errorf("expected Available=true, got false: %s", result.Message)
	}
	if result.Reason != "Healthy" {
		t.Errorf("Reason = %q, want %q", result.Reason, "Healthy")
	}
	if result.Message != "Foundry deployment healthy" {
		t.Errorf("Message = %q, want %q", result.Message, "Foundry deployment healthy")
	}
}

func TestProbe_InfoEndpoint404_StillHealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Some Foundry deployments don't expose /info.
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	driver := New(server.Client())
	result, err := driver.Probe(context.Background(), &v1alpha1.ModelSpec{
		ModelID:  "gpt-4o",
		Endpoint: server.URL,
	}, nil)
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	if !result.Available {
		t.Errorf("expected Available=true for 404 fallback, got false")
	}
}

func TestProbe_AuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	driver := New(server.Client())
	result, err := driver.Probe(context.Background(), &v1alpha1.ModelSpec{
		ModelID:  "gpt-4o",
		Endpoint: server.URL,
	}, nil)
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	if result.Available {
		t.Errorf("expected Available=false for 401")
	}
	if result.Reason != "AuthenticationFailed" {
		t.Errorf("Reason = %q, want %q", result.Reason, "AuthenticationFailed")
	}
}

func TestProbe_ProjectEndpointCredentialModes(t *testing.T) {
	model := &v1alpha1.ModelSpec{
		ModelID:  "gpt-4o",
		Endpoint: "https://example.services.ai.azure.com/api/projects/example",
	}
	tests := []struct {
		name          string
		creds         *cpi.Credentials
		tokenErr      error
		wantAvailable bool
		wantReason    string
	}{
		{name: "azure identity", creds: &cpi.Credentials{Type: "azure-identity"}, wantAvailable: true, wantReason: "Healthy"},
		{name: "azure identity failure", creds: &cpi.Credentials{Type: "azure-identity"}, tokenErr: errors.New("token unavailable"), wantReason: "AuthenticationFailed"},
		{name: "API key", creds: &cpi.Credentials{Type: "api-key", Data: map[string]string{"apiKey": "key"}}, wantReason: "ProbeUnsupported"},
		{name: "no credentials", wantReason: "ProbeUnsupported"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			driver := NewWithTokenProvider(nil, &mockTokenProvider{token: "test-token", err: tc.tokenErr})
			result, err := driver.Probe(context.Background(), model, tc.creds)
			if err != nil {
				t.Fatalf("Probe() error: %v", err)
			}
			if result.Available != tc.wantAvailable || result.Reason != tc.wantReason {
				t.Fatalf("Probe() = available %t, reason %q; want available %t, reason %q", result.Available, result.Reason, tc.wantAvailable, tc.wantReason)
			}
		})
	}
}

func TestEmbed_MockFoundry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		validPaths := map[string]bool{
			"/openai/deployments/text-embedding-3-large/embeddings": true,
			"/models/embeddings":               true,
			"/api/projects/example/embeddings": true,
		}
		if !validPaths[r.URL.Path] {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if got := r.URL.Query().Get("api-version"); got != "2024-02-01" {
			http.Error(w, "missing api-version", http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Expect-Trace") == "true" && r.URL.Query().Get("trace") != "true" {
			http.Error(w, "existing query parameter was not preserved", http.StatusBadRequest)
			return
		}
		resp := embeddingResponse{
			Data: []struct {
				Embedding []float64 `json:"embedding"`
			}{
				{Embedding: []float64{0.1, 0.2}},
			},
			Usage: struct {
				PromptTokens *int `json:"prompt_tokens"`
				TotalTokens  *int `json:"total_tokens"`
			}{PromptTokens: intPtr(4), TotalTokens: intPtr(4)},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	tests := []struct {
		name     string
		endpoint string
		trace    bool
	}{
		{name: "resource root", endpoint: server.URL},
		{name: "deployment scoped", endpoint: server.URL + "/openai/deployments/text-embedding-3-large/?trace=true", trace: true},
		{name: "models endpoint", endpoint: server.URL + "/models"},
		{name: "project endpoint", endpoint: server.URL + "/api/projects/example"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := *server.Client()
			if tc.trace {
				client.Transport = headerTransport{base: client.Transport, key: "X-Expect-Trace", value: "true"}
			}
			driver := New(&client)
			resp, err := driver.Embed(context.Background(), &cpi.EmbedRequest{
				Model:      "text-embedding-3-large",
				Endpoint:   tc.endpoint,
				Inputs:     []string{"test"},
				Parameters: map[string]string{"api-version": "2024-02-01"},
			})
			if err != nil {
				t.Fatalf("Embed() error: %v", err)
			}
			if len(resp.Embeddings) != 1 {
				t.Fatalf("expected 1 embedding, got %d", len(resp.Embeddings))
			}
			if resp.Embeddings[0][0] != 0.1 {
				t.Errorf("Embeddings[0][0] = %f, want 0.1", resp.Embeddings[0][0])
			}
		})
	}
}

func TestFoundryEmbeddingEndpoint_EscapesDeploymentSegmentOnce(t *testing.T) {
	endpoint, err := foundryEmbeddingEndpoint("https://example.openai.azure.com", "embedding/model")
	if err != nil {
		t.Fatalf("foundryEmbeddingEndpoint() error: %v", err)
	}
	if endpoint != "https://example.openai.azure.com/openai/deployments/embedding%2Fmodel/embeddings" {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

func TestFoundryEmbeddingEndpoint_PreservesEscapedScopedPath(t *testing.T) {
	endpoint, err := foundryEmbeddingEndpoint("https://example.openai.azure.com/openai/deployments/embedding%2Fmodel", "ignored")
	if err != nil {
		t.Fatalf("foundryEmbeddingEndpoint() error: %v", err)
	}
	if endpoint != "https://example.openai.azure.com/openai/deployments/embedding%2Fmodel/embeddings" {
		t.Fatalf("endpoint = %q", endpoint)
	}
}

type headerTransport struct {
	base  http.RoundTripper
	key   string
	value string
}

type failingTransport struct {
	err error
}

func (t failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set(t.key, t.value)
	return t.base.RoundTrip(req)
}

func TestValidate(t *testing.T) {
	driver := New(nil)
	ctx := context.Background()

	tests := []struct {
		name    string
		model   *v1alpha1.ModelSpec
		wantErr bool
	}{
		{
			name:    "missing endpoint",
			model:   &v1alpha1.ModelSpec{ModelID: "gpt-4o"},
			wantErr: true,
		},
		{
			name:    "missing modelID",
			model:   &v1alpha1.ModelSpec{Endpoint: "https://my-foundry.openai.azure.com"},
			wantErr: true,
		},
		{
			name:    "invalid endpoint URL",
			model:   &v1alpha1.ModelSpec{ModelID: "gpt-4o", Endpoint: "not-a-url"},
			wantErr: true,
		},
		{
			name:    "valid",
			model:   &v1alpha1.ModelSpec{ModelID: "gpt-4o", Endpoint: "https://my-foundry.openai.azure.com"},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := driver.Validate(ctx, tc.model, nil)
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestComplete_WithToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{
			Choices: []chatChoice{{
				Message: chatMessage{
					Role: "assistant",
					ToolCalls: []chatToolCall{{
						ID:   "call_abc",
						Type: "function",
						Function: chatFunctionCall{
							Name:      "search",
							Arguments: `{"query":"weather"}`,
						},
					}},
				},
				FinishReason: "tool_calls",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	driver := New(server.Client())
	resp, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "gpt-4o",
		Endpoint: server.URL,
		Messages: []cpi.Message{{Role: "user", Content: "search for weather"}},
		Tools: []cpi.ToolDefinition{{
			Name:        "search",
			Description: "Search the web",
			Parameters:  map[string]any{"type": "object"},
		}},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "search" {
		t.Errorf("ToolCall.Name = %q, want %q", resp.ToolCalls[0].Name, "search")
	}
}

// mockTokenProvider implements TokenProvider for unit tests.
type mockTokenProvider struct {
	token string
	err   error
}

func (m *mockTokenProvider) GetToken(_ context.Context, _ *cpi.Credentials) (string, error) {
	return m.token, m.err
}

func TestComplete_AzureIdentityDynamicToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer dynamic-token-xyz" {
			t.Errorf("Authorization = %q, want %q", auth, "Bearer dynamic-token-xyz")
		}

		resp := chatCompletionResponse{
			Choices: []chatChoice{{
				Message:      chatMessage{Role: "assistant", Content: "ok"},
				FinishReason: "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	mock := &mockTokenProvider{token: "dynamic-token-xyz"}
	driver := NewWithTokenProvider(server.Client(), mock)
	_, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "gpt-4o",
		Endpoint: server.URL,
		Creds:    &cpi.Credentials{Type: "azure-identity", Data: map[string]string{"clientId": "abc-123"}},
		Messages: []cpi.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
}

func TestComplete_AzureIdentityTokenError(t *testing.T) {
	mock := &mockTokenProvider{err: fmt.Errorf("credential unavailable")}
	driver := NewWithTokenProvider(nil, mock)
	_, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "gpt-4o",
		Endpoint: "https://example.com",
		Creds:    &cpi.Credentials{Type: "azure-identity", Data: map[string]string{"clientId": "abc-123"}},
		Messages: []cpi.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error when token provider fails")
	}
}

func TestProbe_AzureIdentityDynamicToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer probe-token" {
			t.Errorf("Authorization = %q, want %q", auth, "Bearer probe-token")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"model_name":"gpt-4o"}`))
	}))
	defer server.Close()

	mock := &mockTokenProvider{token: "probe-token"}
	driver := NewWithTokenProvider(server.Client(), mock)
	result, err := driver.Probe(context.Background(), &v1alpha1.ModelSpec{
		ModelID:  "gpt-4o",
		Endpoint: server.URL,
	}, &cpi.Credentials{Type: "azure-identity", Data: map[string]string{"clientId": "xyz"}})
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	if !result.Available {
		t.Errorf("expected Available=true, got false: %s", result.Message)
	}
}

func TestProbe_ClassifiesEndpointFailures(t *testing.T) {
	tests := []struct {
		name       string
		client     *http.Client
		statusCode int
		wantReason string
	}{
		{
			name:       "unreachable",
			client:     &http.Client{Transport: failingTransport{err: errors.New("connection refused")}},
			wantReason: "EndpointUnreachable",
		},
		{name: "server error", statusCode: http.StatusServiceUnavailable, wantReason: "EndpointError"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := test.client
			endpoint := "https://foundry.example"
			if test.statusCode != 0 {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(test.statusCode)
				}))
				defer server.Close()
				client = server.Client()
				endpoint = server.URL
			}

			result, err := New(client).Probe(context.Background(), &v1alpha1.ModelSpec{
				ModelID:  "gpt-4o",
				Endpoint: endpoint,
			}, nil)
			if err != nil {
				t.Fatalf("Probe() error: %v", err)
			}
			if result.Available || result.Reason != test.wantReason {
				t.Fatalf("Probe() = available %t, reason %q; want unavailable %q", result.Available, result.Reason, test.wantReason)
			}
		})
	}
}

func TestComplete_PropagatesResponseFailures(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       string
	}{
		{name: "HTTP error", statusCode: http.StatusTooManyRequests, body: "quota exceeded", want: "HTTP 429: quota exceeded"},
		{name: "malformed response", statusCode: http.StatusOK, body: `{"choices":`, want: "decode response"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			_, err := New(server.Client()).Complete(context.Background(), &cpi.CompletionRequest{
				Model:    "gpt-4o",
				Endpoint: server.URL,
				Messages: []cpi.Message{{Role: "user", Content: "hello"}},
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Complete() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestEmbed_PropagatesResponseFailures(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		want       string
	}{
		{name: "HTTP error", statusCode: http.StatusBadRequest, body: "invalid input", want: "embed HTTP 400: invalid input"},
		{name: "malformed response", statusCode: http.StatusOK, body: `{"data":`, want: "decode embed response"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			_, err := New(server.Client()).Embed(context.Background(), &cpi.EmbedRequest{
				Model:    "text-embedding-3-large",
				Endpoint: server.URL,
				Inputs:   []string{"hello"},
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Embed() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompleteAndEmbed_PropagateTransportFailures(t *testing.T) {
	driver := New(&http.Client{Transport: failingTransport{err: errors.New("connection reset")}})
	tests := []struct {
		name string
		call func() error
		want string
	}{
		{
			name: "completion",
			call: func() error {
				_, err := driver.Complete(context.Background(), &cpi.CompletionRequest{Model: "gpt-4o", Endpoint: "https://foundry.example"})
				return err
			},
			want: "foundry: request failed",
		},
		{
			name: "embedding",
			call: func() error {
				_, err := driver.Embed(context.Background(), &cpi.EmbedRequest{Model: "embedding", Endpoint: "https://foundry.example"})
				return err
			},
			want: "foundry: embed request failed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "connection reset") {
				t.Fatalf("error = %v, want wrapped transport failure", err)
			}
		})
	}
}
