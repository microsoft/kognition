// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
)

const (
	driverName     = "anthropic"
	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
)

// Driver implements the CPI for the Anthropic Messages API.
type Driver struct {
	client *http.Client
}

// New creates an Anthropic CPI driver.
func New(client *http.Client) *Driver {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &Driver{client: client}
}

func (d *Driver) Name() string { return driverName }

func (d *Driver) Validate(_ context.Context, model *v1alpha1.ModelSpec, _ *v1alpha1.ModelClassSpec) error {
	if model.ModelID == "" {
		return fmt.Errorf("anthropic: modelID is required")
	}
	if model.Endpoint != "" {
		parsed, err := url.Parse(model.Endpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("anthropic: endpoint must be a valid URL if specified")
		}
	}
	return nil
}

func (d *Driver) Probe(ctx context.Context, model *v1alpha1.ModelSpec, creds *cpi.Credentials) (*cpi.HealthResult, error) {
	start := time.Now()

	// Send a minimal messages request with max_tokens=1 to validate connectivity.
	baseURL := model.Endpoint
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	body := messagesRequest{
		Model:     model.ModelID,
		MaxTokens: 1,
		Messages:  []message{{Role: "user", Content: "hi"}},
	}
	payload, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return &cpi.HealthResult{
			Available: false,
			Reason:    "RequestBuildFailed",
			Message:   err.Error(),
		}, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", apiVersion)
	if model.InvocationProfileRef != nil {
		req.Header.Set("anthropic-version", invocationAPIVersion(model.Parameters))
	}
	applyAuth(req, creds)

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

	latency := time.Since(start).String()
	if reason, message := classifyProbeStatus(resp); reason != "" {
		return &cpi.HealthResult{
			Available: false,
			Latency:   latency,
			Reason:    reason,
			Message:   message,
		}, nil
	}

	return &cpi.HealthResult{
		Available:    true,
		Latency:      latency,
		Reason:       "Healthy",
		Message:      "Anthropic API reachable",
		Capabilities: []string{"chat"},
	}, nil
}

// classifyProbeStatus maps the probe response to an unhealthy reason and message.
// It returns an empty reason when the endpoint is alive: any 2xx, or 400 (a
// validation error on the minimal probe request).
func classifyProbeStatus(resp *http.Response) (reason, message string) {
	status := resp.StatusCode
	switch {
	case status >= 200 && status < 300, status == http.StatusBadRequest:
		return "", ""
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return "AuthenticationFailed", fmt.Sprintf("HTTP %d", status)
	case status == http.StatusTooManyRequests:
		message = fmt.Sprintf("HTTP %d from Anthropic: rate limited", status)
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			message += fmt.Sprintf(" (Retry-After: %s)", retryAfter)
		}
		return "RateLimited", message
	default:
		return "EndpointError", fmt.Sprintf("HTTP %d from Anthropic", status)
	}
}

func (d *Driver) Complete(ctx context.Context, req *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	prepared, err := cpi.PrepareCompletion(req, d)
	if err != nil {
		return nil, fmt.Errorf("anthropic: invocation: %w", err)
	}
	req = prepared
	baseURL := req.Endpoint
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	body := buildMessagesRequest(req)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", apiVersion)
	if req.Invocation != nil {
		httpReq.Header.Set("anthropic-version", invocationAPIVersion(req.Parameters))
	}
	applyAuth(httpReq, req.Creds)

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var msgResp messagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&msgResp); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}

	return parseMessagesResponse(&msgResp), nil
}

func (d *Driver) Embed(_ context.Context, _ *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return nil, fmt.Errorf("anthropic: embedding not supported")
}

// --- Anthropic wire types ---

type messagesRequest struct {
	Model       string    `json:"model"`
	MaxTokens   int       `json:"max_tokens"`
	Messages    []message `json:"messages"`
	System      string    `json:"system,omitempty"`
	Tools       []tool    `json:"tools,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []contentBlock
}

type contentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

type tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"input_schema"`
}

type messagesResponse struct {
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      *messageUsage  `json:"usage"`
}

type messageUsage struct {
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
}

// --- Helpers ---

func applyAuth(req *http.Request, creds *cpi.Credentials) {
	if creds == nil {
		return
	}
	if key, ok := creds.Data["apiKey"]; ok {
		req.Header.Set("x-api-key", key)
	}
}

func buildMessagesRequest(req *cpi.CompletionRequest) messagesRequest {
	mr := messagesRequest{
		Model:     req.Model,
		MaxTokens: 4096,
	}
	if req.Invocation != nil && req.Parameters["temperature"] != "" {
		if value, err := strconv.ParseFloat(req.Parameters["temperature"], 64); err == nil {
			mr.Temperature = &value
		}
	}

	if v, ok := req.Parameters["max_tokens"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			mr.MaxTokens = n
		}
	}

	for _, m := range req.Messages {
		if m.Role == "system" {
			mr.System = m.Content
			continue
		}
		if req.Invocation != nil {
			if m.ToolCallID != "" {
				mr.Messages = append(mr.Messages, message{
					Role: "user", Content: []contentBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content}},
				})
				continue
			}
			if len(m.ToolCalls) > 0 {
				var blocks []contentBlock
				if m.Content != "" {
					blocks = append(blocks, contentBlock{Type: "text", Text: m.Content})
				}
				for _, call := range m.ToolCalls {
					var input any
					if json.Unmarshal([]byte(call.Arguments), &input) == nil {
						blocks = append(blocks, contentBlock{Type: "tool_use", ID: call.ID, Name: call.Name, Input: input})
					}
				}
				mr.Messages = append(mr.Messages, message{Role: m.Role, Content: blocks})
				continue
			}
		}
		mr.Messages = append(mr.Messages, message{
			Role:    m.Role,
			Content: m.Content,
		})
	}

	for _, t := range req.Tools {
		mr.Tools = append(mr.Tools, tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}

	return mr
}

func parseMessagesResponse(resp *messagesResponse) *cpi.CompletionResponse {
	cr := &cpi.CompletionResponse{
		FinishReason: resp.StopReason,
	}
	if resp.Usage != nil && resp.Usage.InputTokens != nil && resp.Usage.OutputTokens != nil {
		if usage, valid := cpi.NormalizeCompletionUsage(*resp.Usage.InputTokens, *resp.Usage.OutputTokens, nil); valid {
			cr.Usage = usage
		}
	}

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			cr.Content += block.Text
		case "tool_use":
			args, _ := json.Marshal(block.Input)
			cr.ToolCalls = append(cr.ToolCalls, cpi.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: string(args),
			})
		}
	}

	return cr
}
