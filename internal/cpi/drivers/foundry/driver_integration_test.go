// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package foundry_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/foundry"
)

// These tests exercise the Foundry driver against a real Azure AI Foundry deployment.
// Run with:
//   FOUNDRY_ENDPOINT=https://... FOUNDRY_API_KEY=... FOUNDRY_MODEL_ID=... go test -tags=integration -v -run TestIntegration ./internal/cpi/drivers/foundry/
//
// Environment variables:
//   FOUNDRY_ENDPOINT  - The base URL of the Foundry deployment (e.g., https://<resource>.openai.azure.com/openai/deployments/<deployment>)
//   FOUNDRY_API_KEY   - The API key for the deployment
//   FOUNDRY_MODEL_ID  - The deployment/model name
//   FOUNDRY_API_VERSION - (optional) API version, defaults to "2024-02-01"

func getTestConfig(t *testing.T) (endpoint string, creds *cpi.Credentials, modelID, apiVersion string) {
	t.Helper()
	endpoint = os.Getenv("FOUNDRY_ENDPOINT")
	modelID = os.Getenv("FOUNDRY_MODEL_ID")
	apiVersion = os.Getenv("FOUNDRY_API_VERSION")
	if endpoint == "" || modelID == "" {
		t.Skip("FOUNDRY_ENDPOINT and FOUNDRY_MODEL_ID must be set")
	}
	if apiVersion == "" {
		apiVersion = "2024-12-01-preview"
	}

	// Prefer FOUNDRY_TOKEN (azure-identity) over FOUNDRY_API_KEY.
	if token := os.Getenv("FOUNDRY_TOKEN"); token != "" {
		creds = &cpi.Credentials{Type: "azure-identity", Data: map[string]string{"token": token}}
		return
	}
	if apiKey := os.Getenv("FOUNDRY_API_KEY"); apiKey != "" {
		creds = &cpi.Credentials{Type: "api-key", Data: map[string]string{"apiKey": apiKey}}
		return
	}
	t.Skip("FOUNDRY_TOKEN or FOUNDRY_API_KEY must be set")
	return
}

func TestIntegration_Validate(t *testing.T) {
	endpoint, _, modelID, _ := getTestConfig(t)

	drv := foundry.New(nil)

	err := drv.Validate(context.Background(), &v1alpha1.ModelSpec{
		Endpoint: endpoint,
		ModelID:  modelID,
	}, nil)
	if err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	t.Log("Validate: OK")
}

func TestIntegration_Probe(t *testing.T) {
	endpoint, creds, modelID, _ := getTestConfig(t)

	drv := foundry.New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := drv.Probe(ctx, &v1alpha1.ModelSpec{
		Endpoint: endpoint,
		ModelID:  modelID,
	}, creds)
	if err != nil {
		t.Fatalf("Probe returned error: %v", err)
	}

	t.Logf("Probe result: Available=%v Latency=%s Reason=%s Message=%s Capabilities=%v",
		result.Available, result.Latency, result.Reason, result.Message, result.Capabilities)

	if !result.Available {
		t.Errorf("expected model to be available, got reason=%s message=%s", result.Reason, result.Message)
	}
}

func TestIntegration_Complete(t *testing.T) {
	endpoint, creds, modelID, apiVersion := getTestConfig(t)

	drv := foundry.New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := drv.Complete(ctx, &cpi.CompletionRequest{
		Model:    modelID,
		Endpoint: endpoint,
		Creds:    creds,
		Messages: []cpi.Message{
			{Role: "system", Content: "You are a helpful assistant. Reply concisely."},
			{Role: "user", Content: "What is 2+2? Reply with just the number."},
		},
		Parameters: map[string]string{
			"api-version": apiVersion,
			"temperature": "0",
			"max_tokens":  "10",
		},
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	t.Logf("Complete response:")
	t.Logf("  Content: %q", resp.Content)
	t.Logf("  FinishReason: %s", resp.FinishReason)
	t.Logf("  Usage: prompt=%d completion=%d total=%d",
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	t.Logf("  ToolCalls: %d", len(resp.ToolCalls))

	if resp.Content == "" {
		t.Error("expected non-empty content in response")
	}
	if resp.Usage.TotalTokens == 0 {
		t.Error("expected non-zero total tokens")
	}
}

func TestIntegration_Complete_WithTools(t *testing.T) {
	endpoint, creds, modelID, apiVersion := getTestConfig(t)

	drv := foundry.New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := drv.Complete(ctx, &cpi.CompletionRequest{
		Model:    modelID,
		Endpoint: endpoint,
		Creds:    creds,
		Messages: []cpi.Message{
			{Role: "system", Content: "You are a helpful assistant. Use the available tools when appropriate."},
			{Role: "user", Content: "What is the weather in Seattle right now?"},
		},
		Parameters: map[string]string{
			"api-version": apiVersion,
			"temperature": "0",
		},
		Tools: []cpi.ToolDefinition{
			{
				Name:        "get_weather",
				Description: "Get the current weather for a location",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"location": map[string]any{
							"type":        "string",
							"description": "City name",
						},
					},
					"required": []string{"location"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Complete with tools failed: %v", err)
	}

	t.Logf("Complete (with tools) response:")
	t.Logf("  Content: %q", resp.Content)
	t.Logf("  FinishReason: %s", resp.FinishReason)
	t.Logf("  ToolCalls: %d", len(resp.ToolCalls))
	for i, tc := range resp.ToolCalls {
		t.Logf("    [%d] ID=%s Name=%s Args=%s", i, tc.ID, tc.Name, tc.Arguments)
	}

	if len(resp.ToolCalls) == 0 {
		t.Log("WARNING: model did not call the tool (may depend on model capability)")
	} else {
		if resp.ToolCalls[0].Name != "get_weather" {
			t.Errorf("expected tool call to get_weather, got %s", resp.ToolCalls[0].Name)
		}
	}
}

func TestIntegration_RoundTrip_ToolResponse(t *testing.T) {
	endpoint, creds, modelID, apiVersion := getTestConfig(t)

	drv := foundry.New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Step 1: Ask model a question that requires a tool call.
	resp1, err := drv.Complete(ctx, &cpi.CompletionRequest{
		Model:    modelID,
		Endpoint: endpoint,
		Creds:    creds,
		Messages: []cpi.Message{
			{Role: "system", Content: "You are a helpful assistant. Always use the calculator tool for math."},
			{Role: "user", Content: "What is 15 * 7?"},
		},
		Parameters: map[string]string{
			"api-version": apiVersion,
			"temperature": "0",
		},
		Tools: []cpi.ToolDefinition{
			{
				Name:        "calculator",
				Description: "Performs arithmetic calculations",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"expression": map[string]any{
							"type":        "string",
							"description": "Math expression to evaluate",
						},
					},
					"required": []string{"expression"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Step 1 (initial request) failed: %v", err)
	}

	t.Logf("Step 1 - FinishReason: %s, ToolCalls: %d, Content: %q",
		resp1.FinishReason, len(resp1.ToolCalls), resp1.Content)

	if len(resp1.ToolCalls) == 0 {
		t.Log("Model answered directly without tool call — skipping tool response round-trip")
		return
	}

	// Step 2: Feed the tool result back.
	tc := resp1.ToolCalls[0]
	resp2, err := drv.Complete(ctx, &cpi.CompletionRequest{
		Model:    modelID,
		Endpoint: endpoint,
		Creds:    creds,
		Messages: []cpi.Message{
			{Role: "system", Content: "You are a helpful assistant. Always use the calculator tool for math."},
			{Role: "user", Content: "What is 15 * 7?"},
			{Role: "assistant", Content: "", ToolCalls: []cpi.ToolCall{tc}},
			{Role: "tool", Content: "105", ToolCallID: tc.ID},
		},
		Parameters: map[string]string{
			"api-version": apiVersion,
			"temperature": "0",
		},
		Tools: []cpi.ToolDefinition{
			{
				Name:        "calculator",
				Description: "Performs arithmetic calculations",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"expression": map[string]any{
							"type":        "string",
							"description": "Math expression to evaluate",
						},
					},
					"required": []string{"expression"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Step 2 (tool response) failed: %v", err)
	}

	t.Logf("Step 2 - Content: %q, FinishReason: %s", resp2.Content, resp2.FinishReason)
	fmt.Fprintf(os.Stderr, "\n=== FULL ROUND-TRIP SUCCESS ===\n")
	fmt.Fprintf(os.Stderr, "Model processed tool result and responded: %s\n", resp2.Content)
}
