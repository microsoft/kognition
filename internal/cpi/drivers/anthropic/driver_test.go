// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package anthropic

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

func TestParseMessagesResponseRequiresValidComponentUsageFields(t *testing.T) {
	for _, usage := range []*messageUsage{{}, {InputTokens: intPtr(1)}, {InputTokens: intPtr(-1), OutputTokens: intPtr(1)}, {InputTokens: intPtr(int(^uint(0) >> 1)), OutputTokens: intPtr(1)}} {
		if got := parseMessagesResponse(&messagesResponse{Usage: usage}); got.Usage.Reported {
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

func TestComplete_MockAnthropic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			// Verify Anthropic-specific headers.
			if v := r.Header.Get("anthropic-version"); v != apiVersion {
				t.Errorf("anthropic-version = %q, want %q", v, apiVersion)
			}
			if key := r.Header.Get("x-api-key"); key != "test-key" {
				t.Errorf("x-api-key = %q, want %q", key, "test-key")
			}

			var req messagesRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if req.Model != "claude-3-sonnet" {
				t.Errorf("model = %q, want %q", req.Model, "claude-3-sonnet")
			}
			if req.System != "You are helpful." {
				t.Errorf("system = %q, want %q", req.System, "You are helpful.")
			}

			resp := messagesResponse{
				Content: []contentBlock{
					{Type: "text", Text: "Hello from Anthropic!"},
				},
				StopReason: "end_turn",
				Usage:      &messageUsage{InputTokens: intPtr(12), OutputTokens: intPtr(6)},
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
		Model:    "claude-3-sonnet",
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

	if resp.Content != "Hello from Anthropic!" {
		t.Errorf("Content = %q, want %q", resp.Content, "Hello from Anthropic!")
	}
	if resp.FinishReason != "end_turn" {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, "end_turn")
	}
	if resp.Usage.PromptTokens != 12 {
		t.Errorf("Usage.PromptTokens = %d, want 12", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 6 {
		t.Errorf("Usage.CompletionTokens = %d, want 6", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != 18 {
		t.Errorf("Usage.TotalTokens = %d, want 18", resp.Usage.TotalTokens)
	}
}

func TestProbe_MockAnthropic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" && r.Method == http.MethodPost {
			// Return 200 for the probe message.
			resp := messagesResponse{
				Content:    []contentBlock{{Type: "text", Text: "hi"}},
				StopReason: "end_turn",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	driver := New(server.Client())
	result, err := driver.Probe(context.Background(), &v1alpha1.ModelSpec{
		ModelID:  "claude-3-sonnet",
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
		ModelID:  "claude-3-sonnet",
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
		resp := messagesResponse{
			Content: []contentBlock{
				{Type: "text", Text: "Let me check the weather."},
				{
					Type:  "tool_use",
					ID:    "toolu_01",
					Name:  "get_weather",
					Input: map[string]any{"city": "Seattle"},
				},
			},
			StopReason: "tool_use",
			Usage:      &messageUsage{InputTokens: intPtr(15), OutputTokens: intPtr(10)},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	driver := New(server.Client())
	resp, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "claude-3-sonnet",
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

	if resp.Content != "Let me check the weather." {
		t.Errorf("Content = %q, want %q", resp.Content, "Let me check the weather.")
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "toolu_01" {
		t.Errorf("ToolCall.ID = %q, want %q", resp.ToolCalls[0].ID, "toolu_01")
	}
	if resp.ToolCalls[0].Name != "get_weather" {
		t.Errorf("ToolCall.Name = %q, want %q", resp.ToolCalls[0].Name, "get_weather")
	}
	if resp.FinishReason != "tool_use" {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, "tool_use")
	}
}

func TestEmbed_NotSupported(t *testing.T) {
	driver := New(nil)
	_, err := driver.Embed(context.Background(), &cpi.EmbedRequest{
		Model:  "text-embedding",
		Inputs: []string{"hello"},
	})
	if err == nil {
		t.Fatal("expected error for unsupported Embed(), got nil")
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
			name:    "missing modelID",
			model:   &v1alpha1.ModelSpec{},
			wantErr: true,
		},
		{
			name:    "valid with default endpoint",
			model:   &v1alpha1.ModelSpec{ModelID: "claude-3-sonnet"},
			wantErr: false,
		},
		{
			name:    "valid with custom endpoint",
			model:   &v1alpha1.ModelSpec{ModelID: "claude-3-sonnet", Endpoint: "https://my-proxy.example.com"},
			wantErr: false,
		},
		{
			name:    "invalid endpoint URL",
			model:   &v1alpha1.ModelSpec{ModelID: "claude-3-sonnet", Endpoint: "not-a-url"},
			wantErr: true,
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
		http.Error(w, `{"error":"overloaded"}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()

	driver := New(server.Client())
	_, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "claude-3-sonnet",
		Endpoint: server.URL,
		Messages: []cpi.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for HTTP 503, got nil")
	}
}

func TestComplete_TransportFailure(t *testing.T) {
	driver := New(&http.Client{Transport: failingTransport{err: errors.New("connection refused")}})

	_, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "claude-3-sonnet",
		Endpoint: "https://anthropic.invalid",
		Messages: []cpi.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "anthropic: request failed") {
		t.Fatalf("Complete() error = %v, want request failure", err)
	}
}

func TestComplete_MalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":`))
	}))
	defer server.Close()

	_, err := New(server.Client()).Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "claude-3-sonnet",
		Endpoint: server.URL,
		Messages: []cpi.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "anthropic: decode response") {
		t.Fatalf("Complete() error = %v, want decode failure", err)
	}
}

func TestComplete_MapsMaxTokens(t *testing.T) {
	tests := []struct {
		name       string
		parameters map[string]string
		want       int
	}{
		{name: "valid value", parameters: map[string]string{"max_tokens": "256"}, want: 256},
		{name: "malformed value uses default", parameters: map[string]string{"max_tokens": "many"}, want: 4096},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request messagesRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				if request.MaxTokens != test.want {
					t.Errorf("MaxTokens = %d, want %d", request.MaxTokens, test.want)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"content":[]}`))
			}))
			defer server.Close()

			_, err := New(server.Client()).Complete(context.Background(), &cpi.CompletionRequest{
				Model:      "claude-3-sonnet",
				Endpoint:   server.URL,
				Parameters: test.parameters,
			})
			if err != nil {
				t.Fatalf("Complete() error: %v", err)
			}
		})
	}
}
