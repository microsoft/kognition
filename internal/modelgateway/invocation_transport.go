// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/anthropic"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/bedrock"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/foundry"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/ollama"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/openai"
)

func invocationDrivers(client *http.Client, tokens foundry.TokenProvider) *cpi.Registry {
	registry := cpi.NewRegistry()
	registry.MustRegister(openai.New(client))
	registry.MustRegister(foundry.NewWithTokenProvider(client, tokens))
	registry.MustRegister(anthropic.New(client))
	registry.MustRegister(bedrock.New(client))
	registry.MustRegister(ollama.New(client))
	return registry
}

// decodeCPICompletion rejects envelope features the non-OpenAI adapters cannot encode.
func decodeCPICompletion(payload []byte) (*cpi.CompletionRequest, error) {
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Parameters  any    `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		MaxTokens      *int64          `json:"max_tokens"`
		Temperature    *float64        `json:"temperature"`
		ToolChoice     json.RawMessage `json:"tool_choice"`
		ParallelTools  json.RawMessage `json:"parallel_tool_calls"`
		ResponseFormat json.RawMessage `json:"response_format"`
		User           json.RawMessage `json:"user"`
		Stream         *bool           `json:"stream"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return nil, fmt.Errorf("adapter requires text-only canonical chat messages: %w", err)
	}
	if len(body.ParallelTools) != 0 || len(body.ResponseFormat) != 0 || len(body.User) != 0 ||
		(len(body.ToolChoice) != 0 && string(body.ToolChoice) != `"auto"`) {
		return nil, fmt.Errorf("selected CPI chat adapter does not encode explicit parallel_tool_calls, response_format, user or non-auto tool_choice")
	}
	request := &cpi.CompletionRequest{Model: body.Model, Parameters: map[string]string{}}
	if body.MaxTokens != nil {
		request.Parameters["max_tokens"] = strconv.FormatInt(*body.MaxTokens, 10)
	}
	if body.Temperature != nil {
		request.Parameters["temperature"] = strconv.FormatFloat(*body.Temperature, 'g', -1, 64)
	}
	for _, message := range body.Messages {
		item := cpi.Message{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			if call.Type != "function" || call.ID == "" || call.Function.Name == "" || !json.Valid([]byte(call.Function.Arguments)) {
				return nil, fmt.Errorf("canonical function tool call is invalid")
			}
			item.ToolCalls = append(item.ToolCalls, cpi.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
		}
		request.Messages = append(request.Messages, item)
	}
	for _, tool := range body.Tools {
		if tool.Type != "function" || tool.Function.Name == "" {
			return nil, fmt.Errorf("canonical function tool definition is invalid")
		}
		request.Tools = append(request.Tools, cpi.ToolDefinition{Name: tool.Function.Name, Description: tool.Function.Description, Parameters: tool.Function.Parameters})
	}
	return request, nil
}

func (s *Server) proxyCPI(w http.ResponseWriter, r *http.Request, body []byte, model *api.Model, auth upstreamAuth) (cpi.Usage, []byte, string, error) {
	request, err := decodeCPICompletion(body)
	if err != nil || auth.Invocation == nil {
		writeError(w, http.StatusBadRequest, "an explicit compatible invocation contract is required")
		return cpi.Usage{}, nil, "", fmt.Errorf("decode CPI invocation: %v", err)
	}
	driver, found := s.drivers.Get(auth.Provider)
	if !found {
		writeError(w, http.StatusBadGateway, "model adapter is unavailable")
		return cpi.Usage{}, nil, "", fmt.Errorf("provider adapter is unavailable")
	}
	request.Model, request.Endpoint, request.Creds, request.Invocation = model.Spec.ModelID, model.Spec.Endpoint, auth.Credentials, auth.Invocation
	for key, value := range model.Spec.Parameters {
		request.Parameters[key] = value
	}
	response, err := driver.Complete(r.Context(), request)
	if err != nil {
		s.logger.Warn("provider adapter rejected model invocation", zap.String("provider", auth.Provider), zap.Error(err))
		writeError(w, http.StatusBadGateway, "model provider rejected the request")
		return cpi.Usage{}, nil, "", err
	}
	if response == nil || !response.Usage.Reported {
		writeError(w, http.StatusBadGateway, "model provider response omitted token usage")
		return cpi.Usage{}, nil, "", fmt.Errorf("model provider response omitted token usage")
	}
	message := map[string]any{"role": "assistant", "content": response.Content}
	if len(response.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(response.ToolCalls))
		for _, call := range response.ToolCalls {
			id := call.ID
			if id == "" {
				id = "call-" + uuid.NewString()
			}
			calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]string{"name": call.Name, "arguments": call.Arguments}})
		}
		message["tool_calls"] = calls
	}
	finishReason := response.FinishReason
	if len(response.ToolCalls) > 0 {
		finishReason = "tool_calls"
	} else if finishReason == "" || finishReason == "end_turn" || finishReason == "stop_sequence" {
		finishReason = "stop"
	} else if finishReason == "max_tokens" {
		finishReason = "length"
	}
	encoded, err := json.Marshal(map[string]any{
		"id": "chatcmpl-" + uuid.NewString(), "object": "chat.completion", "created": time.Now().Unix(), "model": model.Spec.ModelID,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": finishReason}},
		"usage":   map[string]int{"prompt_tokens": response.Usage.PromptTokens, "completion_tokens": response.Usage.CompletionTokens, "total_tokens": response.Usage.TotalTokens},
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "model provider response could not be encoded")
		return cpi.Usage{}, nil, "", err
	}
	return response.Usage, encoded, "application/json", nil
}
