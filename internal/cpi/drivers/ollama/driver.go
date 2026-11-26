// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ollama

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

	"github.com/google/uuid"
	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
)

const (
	driverName     = "ollama"
	defaultBaseURL = "http://localhost:11434"
)

// Driver implements the CPI for local Ollama instances.
// Uses the Ollama native HTTP API (not OpenAI-compatible endpoint).
type Driver struct {
	client *http.Client
}

// New creates an Ollama CPI driver.
func New(client *http.Client) *Driver {
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	return &Driver{client: client}
}

func (d *Driver) Name() string { return driverName }

func (d *Driver) Validate(_ context.Context, model *v1alpha1.ModelSpec, _ *v1alpha1.ModelClassSpec) error {
	if model.ModelID == "" {
		return fmt.Errorf("ollama: modelID is required (e.g., llama3, mistral)")
	}
	if model.Endpoint != "" {
		parsed, err := url.Parse(model.Endpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("ollama: endpoint must be a valid URL if specified")
		}
	}
	return nil
}

func (d *Driver) Probe(ctx context.Context, model *v1alpha1.ModelSpec, _ *cpi.Credentials) (*cpi.HealthResult, error) {
	start := time.Now()

	baseURL := model.Endpoint
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	// Hit the /api/tags endpoint to verify Ollama is running.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return &cpi.HealthResult{
			Available: false,
			Reason:    "RequestBuildFailed",
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

	if resp.StatusCode != http.StatusOK {
		return &cpi.HealthResult{
			Available: false,
			Latency:   time.Since(start).String(),
			Reason:    "EndpointError",
			Message:   fmt.Sprintf("HTTP %d from /api/tags", resp.StatusCode),
		}, nil
	}

	return &cpi.HealthResult{
		Available:    true,
		Latency:      time.Since(start).String(),
		Reason:       "Healthy",
		Message:      "Ollama instance reachable",
		Capabilities: []string{"chat"},
	}, nil
}

func (d *Driver) Complete(ctx context.Context, req *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	prepared, err := cpi.PrepareCompletion(req, d)
	if err != nil {
		return nil, fmt.Errorf("ollama: invocation: %w", err)
	}
	req = prepared
	baseURL := req.Endpoint
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	body := buildChatRequest(req)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("ollama: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, cpi.NewProviderError("ollama", "", resp)
	}

	var chatResp chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, fmt.Errorf("ollama: decode response: %w", err)
	}

	cr := &cpi.CompletionResponse{
		Content:      chatResp.Message.Content,
		FinishReason: "stop",
	}

	for _, tc := range chatResp.Message.ToolCalls {
		args, _ := json.Marshal(tc.Function.Arguments)
		call := cpi.ToolCall{
			Name:      tc.Function.Name,
			Arguments: string(args),
		}
		if req.Invocation != nil {
			call.ID = "call-" + uuid.NewString()
		}
		cr.ToolCalls = append(cr.ToolCalls, call)
	}

	cr.Usage = usageFromChatResponse(chatResp)

	return cr, nil
}

func usageFromChatResponse(response chatResponse) cpi.Usage {
	if response.PromptEvalCount == nil || response.EvalCount == nil {
		return cpi.Usage{}
	}
	usage, _ := cpi.NormalizeCompletionUsage(*response.PromptEvalCount, *response.EvalCount, nil)
	return usage
}

func (d *Driver) Embed(ctx context.Context, req *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	prepared, err := cpi.PrepareEmbedding(req, d)
	if err != nil {
		return nil, fmt.Errorf("ollama: invocation: %w", err)
	}
	req = prepared
	baseURL := req.Endpoint
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	// Ollama embedding API accepts a single input at a time.
	allEmbeddings := make([][]float64, 0, len(req.Inputs))
	for _, input := range req.Inputs {
		body := map[string]any{
			"model":  req.Model,
			"prompt": input,
		}
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("ollama: marshal embed request: %w", err)
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/embeddings", bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("ollama: build embed request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := d.client.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("ollama: embed request failed: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			providerErr := cpi.NewProviderError("ollama", "embed", resp)
			_ = resp.Body.Close()
			return nil, providerErr
		}

		var embedResp struct {
			Embedding []float64 `json:"embedding"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("ollama: decode embed response: %w", err)
		}
		_ = resp.Body.Close()
		allEmbeddings = append(allEmbeddings, embedResp.Embedding)
	}

	return &cpi.EmbedResponse{Embeddings: allEmbeddings}, nil
}

// --- Ollama wire types ---

type chatRequestBody struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"`
}

type ollamaTool struct {
	Type     string         `json:"type"`
	Function ollamaFunction `json:"function"`
}

type ollamaFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

type ollamaToolCall struct {
	Function struct {
		Name      string `json:"name"`
		Arguments any    `json:"arguments"`
	} `json:"function"`
}

type chatResponse struct {
	Message         ollamaMessage `json:"message"`
	Done            bool          `json:"done"`
	PromptEvalCount *int          `json:"prompt_eval_count"`
	EvalCount       *int          `json:"eval_count"`
}

// --- Helpers ---

func buildChatRequest(req *cpi.CompletionRequest) chatRequestBody {
	cr := chatRequestBody{
		Model:  req.Model,
		Stream: false, // Non-streaming for CPI
	}
	if req.Invocation != nil {
		cr.Options = map[string]any{}
		if value, err := strconv.Atoi(req.Parameters["max_tokens"]); err == nil {
			cr.Options["num_predict"] = value
		}
		if value := req.Parameters["temperature"]; value != "" {
			if number, err := strconv.ParseFloat(value, 64); err == nil {
				cr.Options["temperature"] = number
			}
		}
	}

	toolNames := map[string]string{}
	for _, m := range req.Messages {
		item := ollamaMessage{
			Role:    m.Role,
			Content: m.Content,
		}
		if req.Invocation != nil {
			for _, call := range m.ToolCalls {
				var arguments any
				if json.Unmarshal([]byte(call.Arguments), &arguments) == nil {
					value := ollamaToolCall{}
					value.Function.Name, value.Function.Arguments = call.Name, arguments
					item.ToolCalls = append(item.ToolCalls, value)
					toolNames[call.ID] = call.Name
				}
			}
			if m.ToolCallID != "" {
				item.ToolName = toolNames[m.ToolCallID]
			}
		}
		cr.Messages = append(cr.Messages, item)
	}

	for _, t := range req.Tools {
		cr.Tools = append(cr.Tools, ollamaTool{
			Type: "function",
			Function: ollamaFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}

	return cr
}
