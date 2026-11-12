// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package openai

import (
	"context"
	"encoding/json"
	"errors"
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

type failingTransport struct {
	err error
}

func (t failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

func TestComplete_MockOpenAI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			// Verify auth header is set.
			if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
				t.Errorf("expected Bearer test-key, got %q", auth)
			}

			var req chatRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if req.Model != "gpt-4" {
				t.Errorf("expected model gpt-4, got %s", req.Model)
			}

			resp := chatCompletionResponse{
				Choices: []chatChoice{{
					Message:      chatMessage{Role: "assistant", Content: "Hello from OpenAI!"},
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
		Model:    "gpt-4",
		Endpoint: server.URL,
		Creds:    &cpi.Credentials{Type: "api-key", Data: map[string]string{"apiKey": "test-key"}},
		Messages: []cpi.Message{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "Hello"},
		},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	if resp.Content != "Hello from OpenAI!" {
		t.Errorf("Content = %q, want %q", resp.Content, "Hello from OpenAI!")
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, "stop")
	}
	if resp.Usage.PromptTokens != 10 {
		t.Errorf("Usage.PromptTokens = %d, want 10", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 5 {
		t.Errorf("Usage.CompletionTokens = %d, want 5", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("Usage.TotalTokens = %d, want 15", resp.Usage.TotalTokens)
	}
}

func TestProbe_MockOpenAI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"gpt-4"}]}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	driver := New(server.Client())
	ctx := context.Background()

	result, err := driver.Probe(ctx, &v1alpha1.ModelSpec{
		ModelID:  "gpt-4",
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
}

func TestProbe_AuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	driver := New(server.Client())
	result, err := driver.Probe(context.Background(), &v1alpha1.ModelSpec{
		ModelID:  "gpt-4",
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

func TestComplete_WithToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{
			Choices: []chatChoice{{
				Message: chatMessage{
					Role: "assistant",
					ToolCalls: []chatToolCall{{
						ID:   "call_123",
						Type: "function",
						Function: chatFunctionCall{
							Name:      "get_weather",
							Arguments: `{"city":"Seattle"}`,
						},
					}},
				},
				FinishReason: "tool_calls",
			}},
			Usage: &chatUsage{PromptTokens: intPtr(12), CompletionTokens: intPtr(8), TotalTokens: intPtr(20)},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	driver := New(server.Client())
	resp, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "gpt-4",
		Endpoint: server.URL,
		Messages: []cpi.Message{{Role: "user", Content: "Weather in Seattle?"}},
		Tools: []cpi.ToolDefinition{{
			Name:        "get_weather",
			Description: "Get weather",
			Parameters:  map[string]any{"type": "object"},
		}},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "call_123" {
		t.Errorf("ToolCall.ID = %q, want %q", resp.ToolCalls[0].ID, "call_123")
	}
	if resp.ToolCalls[0].Name != "get_weather" {
		t.Errorf("ToolCall.Name = %q, want %q", resp.ToolCalls[0].Name, "get_weather")
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, "tool_calls")
	}
}

func TestEmbed_MockOpenAI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		resp := embeddingResponse{
			Data: []struct {
				Embedding []float64 `json:"embedding"`
			}{
				{Embedding: []float64{0.1, 0.2, 0.3}},
				{Embedding: []float64{0.4, 0.5, 0.6}},
			},
			Usage: struct {
				PromptTokens *int `json:"prompt_tokens"`
				TotalTokens  *int `json:"total_tokens"`
			}{PromptTokens: intPtr(8), TotalTokens: intPtr(8)},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	driver := New(server.Client())
	resp, err := driver.Embed(context.Background(), &cpi.EmbedRequest{
		Model:    "text-embedding-ada-002",
		Endpoint: server.URL,
		Inputs:   []string{"hello", "world"},
	})
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}

	if len(resp.Embeddings) != 2 {
		t.Fatalf("expected 2 embeddings, got %d", len(resp.Embeddings))
	}
	if resp.Embeddings[0][0] != 0.1 {
		t.Errorf("Embeddings[0][0] = %f, want 0.1", resp.Embeddings[0][0])
	}
	if resp.Usage.PromptTokens != 8 {
		t.Errorf("Usage.PromptTokens = %d, want 8", resp.Usage.PromptTokens)
	}
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
			model:   &v1alpha1.ModelSpec{ModelID: "gpt-4"},
			wantErr: true,
		},
		{
			name:    "missing modelID",
			model:   &v1alpha1.ModelSpec{Endpoint: "https://api.openai.com"},
			wantErr: true,
		},
		{
			name:    "invalid endpoint URL",
			model:   &v1alpha1.ModelSpec{ModelID: "gpt-4", Endpoint: "not-a-url"},
			wantErr: true,
		},
		{
			name:    "valid",
			model:   &v1alpha1.ModelSpec{ModelID: "gpt-4", Endpoint: "https://api.openai.com"},
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

func TestComplete_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	}))
	defer server.Close()

	driver := New(server.Client())
	_, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "gpt-4",
		Endpoint: server.URL,
		Messages: []cpi.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for HTTP 429, got nil")
	}
}

func TestOperations_PreserveContextCancellation(t *testing.T) {
	driver := New(http.DefaultClient)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "complete",
			call: func() error {
				_, err := driver.Complete(ctx, &cpi.CompletionRequest{
					Model:    "gpt-4",
					Endpoint: "https://example.invalid",
				})
				return err
			},
		},
		{
			name: "embed",
			call: func() error {
				_, err := driver.Embed(ctx, &cpi.EmbedRequest{
					Model:    "text-embedding-3-small",
					Endpoint: "https://example.invalid",
				})
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("operation error = %v, want context cancellation", err)
			}
		})
	}
}

func TestOperations_RejectMalformedSuccessResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"incomplete":`))
	}))
	defer server.Close()

	driver := New(server.Client())
	tests := []struct {
		name       string
		call       func() error
		wantPrefix string
	}{
		{
			name: "complete",
			call: func() error {
				_, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
					Model:    "gpt-4",
					Endpoint: server.URL,
				})
				return err
			},
			wantPrefix: "openai-compatible: decode response:",
		},
		{
			name: "embed",
			call: func() error {
				_, err := driver.Embed(context.Background(), &cpi.EmbedRequest{
					Model:    "text-embedding-3-small",
					Endpoint: server.URL,
				})
				return err
			},
			wantPrefix: "openai-compatible: decode embed response:",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil || !strings.HasPrefix(err.Error(), tc.wantPrefix) {
				t.Fatalf("operation error = %v, want prefix %q", err, tc.wantPrefix)
			}
		})
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
			endpoint := "https://openai.example"
			if test.statusCode != 0 {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(test.statusCode)
				}))
				defer server.Close()
				client = server.Client()
				endpoint = server.URL
			}

			result, err := New(client).Probe(context.Background(), &v1alpha1.ModelSpec{
				ModelID:  "gpt-4",
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

func TestEmbed_PreservesHTTPErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte("unsupported dimensions"))
	}))
	defer server.Close()

	_, err := New(server.Client()).Embed(context.Background(), &cpi.EmbedRequest{
		Model:    "text-embedding-3-small",
		Endpoint: server.URL,
		Inputs:   []string{"hello"},
	})
	if err == nil || !strings.Contains(err.Error(), "embed HTTP 422: unsupported dimensions") {
		t.Fatalf("Embed() error = %v, want upstream status and body", err)
	}
}

func TestEmbed_AzureIdentityAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer entra-token" {
			t.Errorf("Authorization = %q, want Bearer entra-token", got)
		}
		if got := r.Header.Get("api-key"); got != "azure-api-key" {
			t.Errorf("api-key = %q, want azure-api-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"usage":{"prompt_tokens":0,"total_tokens":0}}`))
	}))
	defer server.Close()

	_, err := New(server.Client()).Embed(context.Background(), &cpi.EmbedRequest{
		Model:    "text-embedding-3-small",
		Endpoint: server.URL,
		Inputs:   []string{"hello"},
		Creds: &cpi.Credentials{Type: "azure-identity", Data: map[string]string{
			"token":   "entra-token",
			"api-key": "azure-api-key",
		}},
	})
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
}

func TestComplete_MapsGenerationParameters(t *testing.T) {
	tests := []struct {
		name            string
		parameters      map[string]string
		wantTemperature bool
		wantMaxTokens   bool
	}{
		{
			name:            "valid values",
			parameters:      map[string]string{"temperature": "0.25", "max_tokens": "256"},
			wantTemperature: true,
			wantMaxTokens:   true,
		},
		{
			name:       "malformed values omitted",
			parameters: map[string]string{"temperature": "warm", "max_tokens": "many"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request chatRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				if (request.Temperature != nil) != test.wantTemperature {
					t.Errorf("Temperature present = %t, want %t", request.Temperature != nil, test.wantTemperature)
				}
				if request.Temperature != nil && *request.Temperature != 0.25 {
					t.Errorf("Temperature = %v, want 0.25", *request.Temperature)
				}
				if (request.MaxTokens != nil) != test.wantMaxTokens {
					t.Errorf("MaxTokens present = %t, want %t", request.MaxTokens != nil, test.wantMaxTokens)
				}
				if request.MaxTokens != nil && *request.MaxTokens != 256 {
					t.Errorf("MaxTokens = %v, want 256", *request.MaxTokens)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[]}`))
			}))
			defer server.Close()

			_, err := New(server.Client()).Complete(context.Background(), &cpi.CompletionRequest{
				Model:      "gpt-4",
				Endpoint:   server.URL,
				Parameters: test.parameters,
			})
			if err != nil {
				t.Fatalf("Complete() error: %v", err)
			}
		})
	}
}
