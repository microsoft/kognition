// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package foundry

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

const driverName = "foundry"

// Driver implements the CPI for Azure AI Foundry (formerly Azure AI Studio).
// Supports both API key and Managed Identity (Entra token) authentication.
// Uses the OpenAI-compatible wire format that Foundry model deployments expose.
type Driver struct {
	client *http.Client
	tokens TokenProvider
}

// New creates a Foundry CPI driver.
func New(client *http.Client) *Driver {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &Driver{client: client, tokens: newAzureTokenProvider()}
}

// NewWithTokenProvider creates a Foundry CPI driver with a custom token provider (for testing).
func NewWithTokenProvider(client *http.Client, tp TokenProvider) *Driver {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &Driver{client: client, tokens: tp}
}

func (d *Driver) Name() string { return driverName }

func (d *Driver) CountInputTokens(req *cpi.CompletionRequest) (int, error) {
	if req == nil {
		return 0, fmt.Errorf("foundry: completion request is required")
	}
	count, err := cpitokenizer.CountOpenAIChatInput(req)
	if err != nil {
		return 0, fmt.Errorf("foundry: count input tokens: %w", err)
	}
	return count, nil
}

func (d *Driver) Validate(_ context.Context, model *v1alpha1.ModelSpec, _ *v1alpha1.ModelClassSpec) error {
	if model.Endpoint == "" {
		return fmt.Errorf("foundry: endpoint is required")
	}
	parsed, err := url.Parse(model.Endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("foundry: endpoint must be a valid URL")
	}
	if model.ModelID == "" {
		return fmt.Errorf("foundry: modelID is required (deployment name)")
	}
	return nil
}

func (d *Driver) Probe(ctx context.Context, model *v1alpha1.ModelSpec, creds *cpi.Credentials) (*cpi.HealthResult, error) {
	start := time.Now()

	// AI Foundry PROJECT endpoints (e.g. https://<resource>.services.ai.azure.com/api/projects/<project>)
	// do not expose `/info`; that path returns 401 even with a valid token. For
	// those, the only safe non-side-effecting probe is to verify we can mint a
	// bearer token via applyAuth — if Azure AD hands us a token, the credential
	// is well-formed and the model will be reachable on the actual call path.
	if strings.Contains(model.Endpoint, "/api/projects/") {
		if creds == nil || creds.Type != "azure-identity" {
			return &cpi.HealthResult{
				Available: false,
				Latency:   time.Since(start).String(),
				Reason:    "ProbeUnsupported",
				Message:   "Foundry project endpoints can only be probed with Azure identity credentials",
			}, nil
		}
		probeReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, model.Endpoint, nil)
		if err := d.applyAuth(ctx, probeReq, creds); err != nil {
			return &cpi.HealthResult{
				Available: false,
				Latency:   time.Since(start).String(),
				Reason:    "AuthenticationFailed",
				Message:   err.Error(),
			}, nil
		}
		return &cpi.HealthResult{
			Available:    true,
			Latency:      time.Since(start).String(),
			Reason:       "Healthy",
			Message:      "Foundry project endpoint (token acquired; /info not probed)",
			Capabilities: []string{"chat"},
		}, nil
	}

	// Foundry deployments expose /info or respond to GET on the base endpoint.
	endpoint := model.Endpoint + "/info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return &cpi.HealthResult{
			Available: false,
			Reason:    "RequestBuildFailed",
			Message:   err.Error(),
		}, nil
	}
	if err := d.applyAuth(ctx, req, creds); err != nil {
		return &cpi.HealthResult{
			Available: false,
			Reason:    "AuthenticationFailed",
			Message:   err.Error(),
		}, nil
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
			Message:   fmt.Sprintf("HTTP %d from info endpoint", resp.StatusCode),
		}, nil
	}

	// Some Foundry deployments return 404 for /info but are still functional.
	// Fall back to checking if the chat completions endpoint accepts OPTIONS/HEAD.
	if resp.StatusCode == http.StatusNotFound {
		return &cpi.HealthResult{
			Available:    true,
			Latency:      time.Since(start).String(),
			Reason:       "Healthy",
			Message:      "deployment reachable (info endpoint not available)",
			Capabilities: []string{"chat"},
		}, nil
	}

	if resp.StatusCode >= 400 {
		return &cpi.HealthResult{
			Available: false,
			Latency:   time.Since(start).String(),
			Reason:    "EndpointError",
			Message:   fmt.Sprintf("HTTP %d from info endpoint", resp.StatusCode),
		}, nil
	}

	return &cpi.HealthResult{
		Available:    true,
		Latency:      time.Since(start).String(),
		Reason:       "Healthy",
		Message:      "Foundry deployment healthy",
		Capabilities: []string{"chat"},
	}, nil
}

func (d *Driver) Complete(ctx context.Context, req *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	prepared, err := cpi.PrepareCompletion(req, d)
	if err != nil {
		return nil, fmt.Errorf("foundry: invocation: %w", err)
	}
	req = prepared
	body := buildChatRequest(req)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("foundry: marshal request: %w", err)
	}
	if req.Invocation == nil {
		payload, err = PrepareChatRequest(payload, req.Model)
		if err != nil {
			return nil, fmt.Errorf("foundry: prepare legacy request: %w", err)
		}
	}

	endpoint, err := ChatEndpoint(req.Endpoint, req.Model)
	if err != nil {
		return nil, fmt.Errorf("foundry: build chat endpoint: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("foundry: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := d.applyAuth(ctx, httpReq, req.Creds); err != nil {
		return nil, err
	}

	// Foundry deployments may require api-version query parameter.
	// Accept both "api-version" (kebab-case) and "apiVersion" (camelCase).
	if v, ok := req.Parameters["api-version"]; ok {
		q := httpReq.URL.Query()
		q.Set("api-version", v)
		httpReq.URL.RawQuery = q.Encode()
	} else if v, ok := req.Parameters["apiVersion"]; ok {
		q := httpReq.URL.Query()
		q.Set("api-version", v)
		httpReq.URL.RawQuery = q.Encode()
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("foundry: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("foundry: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var chatResp chatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("foundry: decode response: %w", err)
	}

	return parseChatResponse(&chatResp), nil
}

func (d *Driver) Embed(ctx context.Context, req *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	prepared, err := cpi.PrepareEmbedding(req, d)
	if err != nil {
		return nil, fmt.Errorf("foundry: invocation: %w", err)
	}
	req = prepared
	body := map[string]any{
		"model": req.Model,
		"input": req.Inputs,
	}
	if req.Invocation != nil && req.Parameters["dimensions"] != "" {
		dimensions, err := strconv.Atoi(req.Parameters["dimensions"])
		if err != nil {
			return nil, fmt.Errorf("foundry: embedding dimensions: %w", err)
		}
		body["dimensions"] = dimensions
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("foundry: marshal embed request: %w", err)
	}

	endpoint, err := foundryEmbeddingEndpoint(req.Endpoint, req.Model)
	if err != nil {
		return nil, fmt.Errorf("foundry: build embed endpoint: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("foundry: build embed request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if err := d.applyAuth(ctx, httpReq, req.Creds); err != nil {
		return nil, err
	}
	if v, ok := req.Parameters["api-version"]; ok {
		q := httpReq.URL.Query()
		q.Set("api-version", v)
		httpReq.URL.RawQuery = q.Encode()
	} else if v, ok := req.Parameters["apiVersion"]; ok {
		q := httpReq.URL.Query()
		q.Set("api-version", v)
		httpReq.URL.RawQuery = q.Encode()
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("foundry: embed request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("foundry: embed HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var embedResp embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("foundry: decode embed response: %w", err)
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

func foundryEmbeddingEndpoint(baseEndpoint, deployment string) (string, error) {
	parsed, err := url.Parse(baseEndpoint)
	if err != nil {
		return "", err
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.Contains(path, "/openai/deployments/") &&
		!strings.HasSuffix(path, "/models") &&
		!strings.Contains(path, "/api/projects/") {
		rawPath := strings.TrimRight(parsed.EscapedPath(), "/") +
			"/openai/deployments/" + url.PathEscape(deployment) + "/embeddings"
		path, err = url.PathUnescape(rawPath)
		if err != nil {
			return "", err
		}
		parsed.Path = path
		parsed.RawPath = rawPath
		return parsed.String(), nil
	}
	rawPath := strings.TrimRight(parsed.EscapedPath(), "/") + "/embeddings"
	path, err = url.PathUnescape(rawPath)
	if err != nil {
		return "", err
	}
	parsed.Path = path
	parsed.RawPath = rawPath
	return parsed.String(), nil
}

// ChatEndpoint builds the OpenAI-compatible chat-completions URL for a
// Foundry deployment. Azure AI Foundry project endpoints are management
// aliases; inference runs through the corresponding Cognitive Services
// deployment endpoint.
func ChatEndpoint(baseEndpoint, deployment string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseEndpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid Foundry endpoint")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if strings.Contains(strings.ToLower(parsed.Host), ".services.ai.azure.com") && strings.Contains(path, "/api/projects/") {
		parsed.Host = strings.Replace(parsed.Host, ".services.ai.azure.com", ".cognitiveservices.azure.com", 1)
		parsed.Path = "/openai/deployments/" + url.PathEscape(deployment) + "/chat/completions"
		parsed.RawPath = "/openai/deployments/" + url.PathEscape(deployment) + "/chat/completions"
		return parsed.String(), nil
	}
	if strings.Contains(path, "/openai/deployments/") {
		parsed.Path = path + "/chat/completions"
		if parsed.RawPath != "" {
			parsed.RawPath = strings.TrimRight(parsed.RawPath, "/") + "/chat/completions"
		}
		return parsed.String(), nil
	}
	if strings.Contains(strings.ToLower(parsed.Host), ".cognitiveservices.azure.com") || strings.Contains(strings.ToLower(parsed.Host), ".openai.azure.com") {
		parsed.Path = path + "/openai/deployments/" + url.PathEscape(deployment) + "/chat/completions"
		parsed.RawPath = parsed.Path
		return parsed.String(), nil
	}
	parsed.Path = path + "/chat/completions"
	if parsed.RawPath != "" {
		parsed.RawPath = strings.TrimRight(parsed.RawPath, "/") + "/chat/completions"
	}
	return parsed.String(), nil
}

// --- Wire types (same as OpenAI format) ---

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

func (d *Driver) applyAuth(ctx context.Context, req *http.Request, creds *cpi.Credentials) error {
	if creds == nil {
		return nil
	}
	switch creds.Type {
	case "api-key":
		// Foundry supports both Bearer and api-key header.
		if key, ok := creds.Data["apiKey"]; ok {
			req.Header.Set("api-key", key)
		}
	case "azure-identity":
		token, err := d.tokens.GetToken(ctx, creds)
		if err != nil {
			return fmt.Errorf("foundry: azure identity auth: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
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
