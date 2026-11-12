// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package bedrock

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
)

func TestParseConverseResponseRequiresValidComponentUsageFields(t *testing.T) {
	for _, usage := range []*converseUsage{{}, {InputTokens: intPtr(1)}, {InputTokens: intPtr(2), OutputTokens: intPtr(1), TotalTokens: intPtr(0)}, {InputTokens: intPtr(2), OutputTokens: intPtr(1), TotalTokens: intPtr(1)}, {InputTokens: intPtr(-1), OutputTokens: intPtr(1)}} {
		if got := parseConverseResponse(&converseResponse{Usage: usage}); got.Usage.Reported {
			t.Fatalf("usage %+v was reported", usage)
		}
	}
}

func TestComplete_MockBedrock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req converseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(req.Messages) == 0 {
			http.Error(w, "no messages", http.StatusBadRequest)
			return
		}

		resp := converseResponse{
			StopReason: "end_turn",
			Usage:      &converseUsage{InputTokens: intPtr(10), OutputTokens: intPtr(5), TotalTokens: intPtr(15)},
		}
		resp.Output.Message = converseMessage{
			Role: "assistant",
			Content: []converseContent{
				{Text: stringPtr("Hello from Bedrock!")},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	driver := New(server.Client())
	resp, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "anthropic.claude-3-haiku",
		Endpoint: server.URL,
		Messages: []cpi.Message{
			{Role: "user", Content: "Hello"},
		},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	if resp.Content != "Hello from Bedrock!" {
		t.Errorf("Content = %q, want %q", resp.Content, "Hello from Bedrock!")
	}
	if resp.FinishReason != "end_turn" {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, "end_turn")
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("Usage.TotalTokens = %d, want 15", resp.Usage.TotalTokens)
	}
}

func TestComplete_WithMultipleTools(t *testing.T) {
	// This test validates the bug fix: all tools must appear in ToolConfig.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req converseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Critical assertion: both tools must be present in ToolConfig.
		if req.ToolConfig == nil {
			t.Error("ToolConfig is nil, expected tools")
			http.Error(w, "no tool config", http.StatusBadRequest)
			return
		}
		if len(req.ToolConfig.Tools) != 2 {
			t.Errorf("ToolConfig.Tools count = %d, want 2", len(req.ToolConfig.Tools))
		}

		// Return a tool_use response.
		resp := converseResponse{StopReason: "tool_use"}
		resp.Output.Message = converseMessage{
			Role: "assistant",
			Content: []converseContent{
				{ToolUse: &toolUse{
					ToolUseID: "tu_1",
					Name:      "get_weather",
					Input:     map[string]any{"city": "Seattle"},
				}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	driver := New(server.Client())
	resp, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "anthropic.claude-3-haiku",
		Endpoint: server.URL,
		Messages: []cpi.Message{{Role: "user", Content: "Weather in Seattle and NYC?"}},
		Tools: []cpi.ToolDefinition{
			{Name: "get_weather", Description: "Get weather for a city", Parameters: map[string]any{"type": "object"}},
			{Name: "get_news", Description: "Get news", Parameters: map[string]any{"type": "object"}},
		},
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
	if resp.FinishReason != "tool_use" {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, "tool_use")
	}
}

func TestComplete_SystemMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req converseRequest
		json.NewDecoder(r.Body).Decode(&req)

		// Verify system messages are extracted into the System field.
		if len(req.System) == 0 {
			t.Error("System field is empty, expected system message")
		} else if req.System[0].Text == nil || *req.System[0].Text != "You are helpful." {
			t.Errorf("System[0].Text = %v, want %q", req.System[0].Text, "You are helpful.")
		}
		// System messages should NOT appear in Messages.
		for _, m := range req.Messages {
			if m.Role == "system" {
				t.Error("system message found in Messages, should be in System field")
			}
		}

		resp := converseResponse{StopReason: "end_turn"}
		resp.Output.Message = converseMessage{Role: "assistant", Content: []converseContent{{Text: stringPtr("ok")}}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	driver := New(server.Client())
	_, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "anthropic.claude-3-haiku",
		Endpoint: server.URL,
		Messages: []cpi.Message{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
}

func TestComplete_MapsToolExchangeAndInferenceConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request converseRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.InferenceConfig == nil || request.InferenceConfig.MaxTokens == nil || *request.InferenceConfig.MaxTokens != 256 {
			t.Errorf("MaxTokens = %v, want 256", request.InferenceConfig)
		}
		if request.InferenceConfig == nil || request.InferenceConfig.Temperature == nil || *request.InferenceConfig.Temperature != 0.25 {
			t.Errorf("Temperature = %v, want 0.25", request.InferenceConfig)
		}
		if len(request.Messages) != 2 {
			t.Fatalf("Messages count = %d, want 2", len(request.Messages))
		}

		assistantContent := request.Messages[0].Content
		if len(assistantContent) != 1 || assistantContent[0].ToolUse == nil {
			t.Fatalf("assistant content = %#v, want one tool use", assistantContent)
		}
		if assistantContent[0].ToolUse.ToolUseID != "call-1" || assistantContent[0].ToolUse.Name != "get_weather" {
			t.Errorf("ToolUse = %#v, want call-1/get_weather", assistantContent[0].ToolUse)
		}

		resultContent := request.Messages[1].Content
		if len(resultContent) != 2 || resultContent[1].ToolResult == nil {
			t.Fatalf("tool result content = %#v, want text and tool result", resultContent)
		}
		if resultContent[1].ToolResult.ToolUseID != "call-1" {
			t.Errorf("ToolResult.ToolUseID = %q, want call-1", resultContent[1].ToolResult.ToolUseID)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":{"message":{"role":"assistant","content":[]}}}`))
	}))
	defer server.Close()

	_, err := New(server.Client()).Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "anthropic.claude-3-haiku",
		Endpoint: server.URL,
		Messages: []cpi.Message{
			{
				Role: "assistant",
				ToolCalls: []cpi.ToolCall{{
					ID:        "call-1",
					Name:      "get_weather",
					Arguments: `{"city":"Seattle"}`,
				}},
			},
			{Role: "tool", Content: "sunny", ToolCallID: "call-1"},
		},
		Parameters: map[string]string{"max_tokens": "256", "temperature": "0.25"},
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
}

func TestProbe_Healthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Probe sends a minimal converse request; return 200.
		resp := converseResponse{StopReason: "end_turn"}
		resp.Output.Message = converseMessage{Role: "assistant", Content: []converseContent{{Text: stringPtr("hi")}}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	driver := New(server.Client())
	result, err := driver.Probe(context.Background(), &v1alpha1.ModelSpec{
		ModelID:  "anthropic.claude-3-haiku",
		Endpoint: server.URL,
	}, &cpi.Credentials{
		Type: "aws",
		Data: map[string]string{"accessKeyId": "AK", "secretAccessKey": "SK", "region": "us-east-1"},
	})
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
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	driver := New(server.Client())
	result, err := driver.Probe(context.Background(), &v1alpha1.ModelSpec{
		ModelID:  "anthropic.claude-3-haiku",
		Endpoint: server.URL,
	}, nil)
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	if result.Available {
		t.Errorf("expected Available=false for 403")
	}
	if result.Reason != "AuthenticationFailed" {
		t.Errorf("Reason = %q, want %q", result.Reason, "AuthenticationFailed")
	}
}

func TestEmbed_NotSupported(t *testing.T) {
	driver := New(nil)
	_, err := driver.Embed(context.Background(), &cpi.EmbedRequest{
		Model:  "titan-embed",
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
			model:   &v1alpha1.ModelSpec{Endpoint: "https://bedrock-runtime.us-east-1.amazonaws.com"},
			wantErr: true,
		},
		{
			name:    "missing endpoint",
			model:   &v1alpha1.ModelSpec{ModelID: "anthropic.claude-3-haiku"},
			wantErr: true,
		},
		{
			name:    "valid",
			model:   &v1alpha1.ModelSpec{ModelID: "anthropic.claude-3-haiku", Endpoint: "https://bedrock-runtime.us-east-1.amazonaws.com"},
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

func TestBuildConverseRequest_ToolConfig(t *testing.T) {
	// Unit test for buildConverseRequest to directly verify the ToolConfig bug fix.
	req := &cpi.CompletionRequest{
		Model: "test-model",
		Messages: []cpi.Message{
			{Role: "user", Content: "hi"},
		},
		Tools: []cpi.ToolDefinition{
			{Name: "tool_a", Description: "A", Parameters: map[string]any{"type": "object"}},
			{Name: "tool_b", Description: "B", Parameters: map[string]any{"type": "object"}},
			{Name: "tool_c", Description: "C", Parameters: map[string]any{"type": "object"}},
		},
	}

	cr := buildConverseRequest(req)

	if cr.ToolConfig == nil {
		t.Fatal("ToolConfig is nil")
	}
	if len(cr.ToolConfig.Tools) != 3 {
		t.Fatalf("ToolConfig.Tools count = %d, want 3", len(cr.ToolConfig.Tools))
	}
	names := make([]string, 3)
	for i, ts := range cr.ToolConfig.Tools {
		names[i] = ts.ToolSpec.Name
	}
	if names[0] != "tool_a" || names[1] != "tool_b" || names[2] != "tool_c" {
		t.Errorf("Tool names = %v, want [tool_a tool_b tool_c]", names)
	}
}

func TestBuildConverseRequest_SkipsMalformedToolCallArguments(t *testing.T) {
	req := &cpi.CompletionRequest{
		Messages: []cpi.Message{{
			Role: "assistant",
			ToolCalls: []cpi.ToolCall{
				{ID: "invalid", Name: "broken", Arguments: `{"city":`},
				{ID: "valid", Name: "weather", Arguments: `{"city":"Seattle"}`},
			},
		}},
	}

	cr := buildConverseRequest(req)

	if len(cr.Messages) != 1 {
		t.Fatalf("Messages count = %d, want 1", len(cr.Messages))
	}
	content := cr.Messages[0].Content
	if len(content) != 1 || content[0].ToolUse == nil {
		t.Fatalf("Content = %#v, want one valid tool use", content)
	}
	if content[0].ToolUse.ToolUseID != "valid" || content[0].ToolUse.Name != "weather" {
		t.Errorf("ToolUse = %#v, want valid weather call", content[0].ToolUse)
	}
}
