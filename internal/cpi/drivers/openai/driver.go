// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	cpitokenizer "github.com/kognition-ai/kognition/internal/cpi/tokenizer"
)

// buildEndpoint joins an OpenAI-compatible base URL with an API suffix
// like "models", "chat/completions", or "embeddings". The suffix MUST NOT
// include a leading slash or a "/v1" prefix. If the user-supplied base URL
// already ends with "/v1" (or "/v1/"), the function avoids producing a
// duplicated "/v1/v1" path.
func buildEndpoint(base, suffix string) string {
	b := strings.TrimRight(base, "/")
	if strings.HasSuffix(b, "/v1") {
		return b + "/" + suffix
	}
	return b + "/v1/" + suffix
}

const driverName = "openai-compatible"

// Driver implements the CPI for any OpenAI-compatible API endpoint
// (OpenAI, Azure OpenAI, vLLM, LocalAI, LM Studio, etc.).
type Driver struct {
	client *http.Client
}

// New creates an openai-compatible CPI driver with the given HTTP client.
// If client is nil, a default client with 30s timeout is used.
func New(client *http.Client) *Driver {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Driver{client: client}
}

func (d *Driver) Name() string { return driverName }

func (d *Driver) CountInputTokens(req *cpi.CompletionRequest) (int, error) {
	if req == nil {
		return 0, fmt.Errorf("openai-compatible: completion request is required")
	}
	count, err := cpitokenizer.CountOpenAIChatInput(req)
	if err != nil {
		return 0, fmt.Errorf("openai-compatible: count input tokens: %w", err)
	}
	return count, nil
}

func (d *Driver) Validate(_ context.Context, model *v1alpha1.ModelSpec, _ *v1alpha1.ModelClassSpec) error {
	if model.Endpoint == "" {
		return fmt.Errorf("openai-compatible: endpoint is required")
	}
	parsed, err := url.Parse(model.Endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("openai-compatible: endpoint must be a valid URL with scheme and host")
	}
	if model.ModelID == "" {
		return fmt.Errorf("openai-compatible: modelID is required")
	}
	return nil
}

func (d *Driver) Probe(ctx context.Context, model *v1alpha1.ModelSpec, creds *cpi.Credentials) (*cpi.HealthResult, error) {
	start := time.Now()

	// Send a minimal models list request to validate connectivity.
	endpoint := buildEndpoint(model.Endpoint, "models")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return &cpi.HealthResult{
			Available: false,
			Reason:    "RequestBuildFailed",
			Message:   err.Error(),
		}, nil
	}
	applyAuth(req, creds)
	if model.InvocationProfileRef != nil {
		applyInvocationAPIVersion(req, model.Parameters)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return &cpi.HealthResult{
			Available: false,
			Latency:   time.Since(start).String(),
			Reason:    "EndpointUnreachable",
			Message:   err.Error(),
		}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &cpi.HealthResult{
			Available: false,
			Latency:   time.Since(start).String(),
			Reason:    "AuthenticationFailed",
			Message:   fmt.Sprintf("HTTP %d from models endpoint", resp.StatusCode),
		}, nil
	}

	if resp.StatusCode >= 400 {
		return &cpi.HealthResult{
			Available: false,
			Latency:   time.Since(start).String(),
			Reason:    "EndpointError",
			Message:   fmt.Sprintf("HTTP %d from models endpoint", resp.StatusCode),
		}, nil
	}

	return &cpi.HealthResult{
		Available:    true,
		Latency:      time.Since(start).String(),
		Reason:       "Healthy",
		Message:      "models endpoint responded successfully",
		Capabilities: []string{"chat"},
	}, nil
}

func (d *Driver) Complete(ctx context.Context, req *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	prepared, err := cpi.PrepareCompletion(req, d)
	if err != nil {
		return nil, fmt.Errorf("openai-compatible: invocation: %w", err)
	}
	req = prepared
	body := buildChatRequest(req)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai-compatible: marshal request: %w", err)
	}

	endpoint := buildEndpoint(req.Endpoint, "chat/completions")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openai-compatible: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	applyAuth(httpReq, req.Creds)
	if req.Invocation != nil {
		applyInvocationAPIVersion(httpReq, req.Parameters)
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-compatible: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, cpi.NewProviderError("openai-compatible", "", resp)
	}

	var chatResp chatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("openai-compatible: decode response: %w", err)
	}

	return parseChatResponse(&chatResp), nil
}

func (d *Driver) Embed(ctx context.Context, req *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	prepared, err := cpi.PrepareEmbedding(req, d)
	if err != nil {
		return nil, fmt.Errorf("openai-compatible: invocation: %w", err)
	}
	req = prepared
	body := map[string]any{
		"model": req.Model,
		"input": req.Inputs,
	}
	if req.Invocation != nil && req.Parameters["dimensions"] != "" {
		dimensions, err := strconv.Atoi(req.Parameters["dimensions"])
		if err != nil {
			return nil, fmt.Errorf("openai-compatible: embedding dimensions: %w", err)
		}
		body["dimensions"] = dimensions
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai-compatible: marshal embed request: %w", err)
	}

	endpoint := buildEndpoint(req.Endpoint, "embeddings")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openai-compatible: build embed request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	applyAuth(httpReq, req.Creds)
	if req.Invocation != nil {
		applyInvocationAPIVersion(httpReq, req.Parameters)
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-compatible: embed request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, cpi.NewProviderError("openai-compatible", "embed", resp)
	}

	var embedResp embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("openai-compatible: decode embed response: %w", err)
	}

	embeddings := make([][]float64, len(embedResp.Data))
	for i, d := range embedResp.Data {
		embeddings[i] = d.Embedding
	}

	return &cpi.EmbedResponse{
		Embeddings: embeddings,
		Usage:      cpi.NormalizeEmbeddingUsage(embedResp.Usage.PromptTokens, embedResp.Usage.TotalTokens),
	}, nil
}

// --- OpenAI wire types ---

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	Tools               []chatTool    `json:"tools,omitempty"`
	Temperature         *float64      `json:"temperature,omitempty"`
	MaxTokens           *int          `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int          `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string        `json:"reasoning_effort,omitempty"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatCompletionResponse struct {
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage"`
}

type chatUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	TotalTokens      *int `json:"total_tokens"`
}

type chatChoice struct {
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type embeddingResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens *int `json:"prompt_tokens"`
		TotalTokens  *int `json:"total_tokens"`
	} `json:"usage"`
}

// --- Helpers ---

func applyAuth(req *http.Request, creds *cpi.Credentials) {
	if creds == nil {
		return
	}
	switch creds.Type {
	case "api-key":
		if key, ok := creds.Data["apiKey"]; ok {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	case "azure-identity":
		if token, ok := creds.Data["token"]; ok {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if apiKey, ok := creds.Data["api-key"]; ok {
			req.Header.Set("api-key", apiKey)
		}
	}
}

func buildChatRequest(req *cpi.CompletionRequest) chatRequest {
	cr := chatRequest{
		Model:    req.Model,
		Messages: make([]chatMessage, len(req.Messages)),
	}
	if req.Invocation != nil {
		if value := req.Parameters["max_completion_tokens"]; value != "" {
			if number, err := strconv.Atoi(value); err == nil {
				cr.MaxCompletionTokens = &number
			}
		}
		cr.ReasoningEffort = req.Parameters["reasoning_effort"]
	}

	for i, m := range req.Messages {
		cm := chatMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			cm.ToolCalls = append(cm.ToolCalls, chatToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: chatFunctionCall{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			})
		}
		cr.Messages[i] = cm
	}

	for _, t := range req.Tools {
		cr.Tools = append(cr.Tools, chatTool{
			Type: "function",
			Function: chatFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}

	if v, ok := req.Parameters["temperature"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cr.Temperature = &f
		}
	}
	if v, ok := req.Parameters["max_tokens"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			cr.MaxTokens = &n
		}
	}

	return cr
}

func parseChatResponse(resp *chatCompletionResponse) *cpi.CompletionResponse {
	cr := &cpi.CompletionResponse{}
	if resp.Usage != nil && resp.Usage.PromptTokens != nil && resp.Usage.CompletionTokens != nil {
		if usage, valid := cpi.NormalizeCompletionUsage(*resp.Usage.PromptTokens, *resp.Usage.CompletionTokens, resp.Usage.TotalTokens); valid {
			cr.Usage = usage
		}
	}

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		cr.Content = choice.Message.Content
		cr.FinishReason = choice.FinishReason
		for _, tc := range choice.Message.ToolCalls {
			cr.ToolCalls = append(cr.ToolCalls, cpi.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
	}

	return cr
}
