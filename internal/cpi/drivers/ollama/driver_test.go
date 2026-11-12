// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ollama

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

// TC-0198: Register ollama driver for local testing; verify Complete() returns a
// valid response against a running Ollama instance.
func TestComplete_MockOllama(t *testing.T) {
	// Mock Ollama server that responds to /api/chat.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var req chatRequestBody
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			// Validate the incoming request structure.
			if req.Model != "llama3" {
				t.Errorf("expected model llama3, got %s", req.Model)
			}
			if req.Stream != false {
				t.Errorf("expected stream=false")
			}

			resp := chatResponse{
				Message: ollamaMessage{
					Role:    "assistant",
					Content: "Go interfaces define a set of method signatures.",
				},
				Done:            true,
				PromptEvalCount: intPtr(15),
				EvalCount:       intPtr(9),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"models":[{"name":"llama3"}]}`))

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	driver := New(server.Client())

	ctx := context.Background()

	// Test Complete().
	completionResp, err := driver.Complete(ctx, &cpi.CompletionRequest{
		Model:    "llama3",
		Endpoint: server.URL,
		Messages: []cpi.Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "What are Go interfaces?"},
		},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	if completionResp.Content != "Go interfaces define a set of method signatures." {
		t.Errorf("Content = %q, want %q", completionResp.Content, "Go interfaces define a set of method signatures.")
	}
	if completionResp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", completionResp.FinishReason, "stop")
	}
	if completionResp.Usage.PromptTokens != 15 {
		t.Errorf("Usage.PromptTokens = %d, want 15", completionResp.Usage.PromptTokens)
	}
	if completionResp.Usage.CompletionTokens != 9 {
		t.Errorf("Usage.CompletionTokens = %d, want 9", completionResp.Usage.CompletionTokens)
	}
	if completionResp.Usage.TotalTokens != 24 {
		t.Errorf("Usage.TotalTokens = %d, want 24", completionResp.Usage.TotalTokens)
	}
}

func intPtr(value int) *int { return &value }

func TestCompleteRequiresValidUsageCounters(t *testing.T) {
	for _, response := range []chatResponse{{}, {PromptEvalCount: intPtr(1)}, {PromptEvalCount: intPtr(-1), EvalCount: intPtr(1)}, {PromptEvalCount: intPtr(int(^uint(0) >> 1)), EvalCount: intPtr(1)}} {
		if got := usageFromChatResponse(response); got.Reported {
			t.Fatalf("usage %+v was reported", response)
		}
	}
}

func TestProbe_MockOllama(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"models":[{"name":"llama3"}]}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	driver := New(server.Client())

	ctx := context.Background()
	result, err := driver.Probe(ctx, &v1alpha1.ModelSpec{
		ModelID:  "llama3",
		Endpoint: server.URL,
	}, nil)
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

func TestComplete_WithToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatResponse{
			Message: ollamaMessage{
				Role:    "assistant",
				Content: "",
				ToolCalls: []ollamaToolCall{
					{Function: struct {
						Name      string `json:"name"`
						Arguments any    `json:"arguments"`
					}{
						Name:      "get_weather",
						Arguments: map[string]any{"city": "Seattle"},
					}},
				},
			},
			Done: true,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	driver := New(server.Client())

	resp, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "llama3",
		Endpoint: server.URL,
		Messages: []cpi.Message{{Role: "user", Content: "What's the weather in Seattle?"}},
		Tools: []cpi.ToolDefinition{{
			Name:        "get_weather",
			Description: "Get current weather",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "get_weather" {
		t.Errorf("ToolCall.Name = %q, want %q", resp.ToolCalls[0].Name, "get_weather")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		model   v1alpha1.ModelSpec
		wantErr string
	}{
		{name: "valid default endpoint", model: v1alpha1.ModelSpec{ModelID: "llama3"}},
		{name: "valid custom endpoint", model: v1alpha1.ModelSpec{ModelID: "llama3", Endpoint: "http://ollama.local:11434"}},
		{name: "missing model", model: v1alpha1.ModelSpec{}, wantErr: "modelID is required"},
		{name: "invalid endpoint", model: v1alpha1.ModelSpec{ModelID: "llama3", Endpoint: "localhost:11434"}, wantErr: "endpoint must be a valid URL"},
	}

	driver := New(nil)
	if driver.Name() != "ollama" {
		t.Fatalf("Name() = %q, want ollama", driver.Name())
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := driver.Validate(context.Background(), &test.model, nil)
			if test.wantErr == "" && err != nil {
				t.Fatalf("Validate() error: %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestProbeClassifiesEndpointFailures(t *testing.T) {
	t.Run("provider status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}))
		defer server.Close()

		result, err := New(server.Client()).Probe(context.Background(), &v1alpha1.ModelSpec{Endpoint: server.URL}, nil)
		if err != nil || result.Available || result.Reason != "EndpointError" || !strings.Contains(result.Message, "HTTP 503") {
			t.Fatalf("Probe() = %+v, %v", result, err)
		}
	})

	t.Run("transport error", func(t *testing.T) {
		client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})}
		result, err := New(client).Probe(context.Background(), &v1alpha1.ModelSpec{Endpoint: "http://ollama.local"}, nil)
		if err != nil || result.Available || result.Reason != "EndpointUnreachable" || !strings.Contains(result.Message, "connection refused") {
			t.Fatalf("Probe() = %+v, %v", result, err)
		}
	})
}

func TestCompleteReportsProviderAndDecodeErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantErr    string
	}{
		{name: "provider error", statusCode: http.StatusBadGateway, body: "model unavailable", wantErr: "ollama: HTTP 502: model unavailable"},
		{name: "invalid response", statusCode: http.StatusOK, body: "not-json", wantErr: "ollama: decode response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			_, err := New(server.Client()).Complete(context.Background(), &cpi.CompletionRequest{Model: "llama3", Endpoint: server.URL})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Complete() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestCompleteReportsTransportError(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset")
	})}
	_, err := New(client).Complete(context.Background(), &cpi.CompletionRequest{Model: "llama3", Endpoint: "http://ollama.local"})
	if err == nil || !strings.Contains(err.Error(), "ollama: request failed:") || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("Complete() error = %v", err)
	}
}

func TestEmbedBatchesInputsInOrder(t *testing.T) {
	var prompts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/embeddings" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode embed request: %v", err)
		}
		if body.Model != "nomic-embed-text" {
			t.Errorf("model = %q, want nomic-embed-text", body.Model)
		}
		prompts = append(prompts, body.Prompt)
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float64{float64(len(prompts)), 0.5}})
	}))
	defer server.Close()

	response, err := New(server.Client()).Embed(context.Background(), &cpi.EmbedRequest{
		Model:    "nomic-embed-text",
		Endpoint: server.URL,
		Inputs:   []string{"first", "second"},
	})
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
	if strings.Join(prompts, ",") != "first,second" {
		t.Fatalf("prompts = %v, want [first second]", prompts)
	}
	if len(response.Embeddings) != 2 || response.Embeddings[0][0] != 1 || response.Embeddings[1][0] != 2 {
		t.Fatalf("embeddings = %v", response.Embeddings)
	}
}

func TestEmbedReportsProviderAndDecodeErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantErr    string
	}{
		{name: "provider error", statusCode: http.StatusTooManyRequests, body: "busy", wantErr: "ollama: embed HTTP 429: busy"},
		{name: "invalid response", statusCode: http.StatusOK, body: "not-json", wantErr: "ollama: decode embed response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			_, err := New(server.Client()).Embed(context.Background(), &cpi.EmbedRequest{Model: "embed", Endpoint: server.URL, Inputs: []string{"input"}})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Embed() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestEmbedReportsTransportError(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset")
	})}
	_, err := New(client).Embed(context.Background(), &cpi.EmbedRequest{
		Model:    "embed",
		Endpoint: "http://ollama.local",
		Inputs:   []string{"input"},
	})
	if err == nil || !strings.Contains(err.Error(), "ollama: embed request failed:") || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("Embed() error = %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
