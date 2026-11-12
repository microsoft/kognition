// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

import (
	"context"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
)

// Driver is the in-process Capability Provider Interface for model backends.
// Each built-in provider (openai, foundry, anthropic, ollama, aws-bedrock)
// implements this interface to validate, health-check, and invoke models.
//
// This is the "in-tree plugin" phase (analogous to K8s in-tree volume plugins
// before CSI). In MVP-5, this interface will be wrapped by a gRPC SPI (US-0065).
type Driver interface {
	// Name returns the provisioner name this driver handles (e.g., "openai-compatible").
	Name() string

	// Validate checks that the given Model + ModelClass configuration is valid
	// (correct parameters, endpoint reachable format, credential shape).
	Validate(ctx context.Context, model *v1alpha1.ModelSpec, class *v1alpha1.ModelClassSpec) error

	// Probe performs a health check against the model endpoint and returns
	// availability status, latency, and capabilities.
	Probe(ctx context.Context, model *v1alpha1.ModelSpec, creds *Credentials) (*HealthResult, error)

	// Complete sends a chat completion request and returns the response.
	Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)

	// Embed sends an embedding request and returns the vectors.
	Embed(ctx context.Context, req *EmbedRequest) (*EmbedResponse, error)
}

// ExactTokenCounter is an optional CPI capability for adapters that can
// exactly tokenize the serialized input sent to a specific provider model.
type ExactTokenCounter interface {
	CountInputTokens(req *CompletionRequest) (int, error)
}

// Credentials holds resolved credential material for a model.
type Credentials struct {
	// Type is the credential type (e.g., "api-key", "azure-identity", "aws-sigv4").
	Type string

	// Data holds credential fields (e.g., "apiKey", "clientId", "region").
	Data map[string]string
}

// HealthResult is returned by Driver.Probe().
type HealthResult struct {
	Available    bool
	Latency      string
	Reason       string
	Message      string
	Capabilities []string
}

// CompletionRequest is the unified request format for chat completions.
type CompletionRequest struct {
	Model               string
	Endpoint            string
	Creds               *Credentials
	Messages            []Message
	Parameters          map[string]string
	Tools               []ToolDefinition
	Invocation          *modelinvocation.Resolved
	MaxCompletionTokens int64
}

// Message represents a chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
}

// ToolCall represents a tool invocation requested by the model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDefinition describes a tool available for the model to call.
type ToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// CompletionResponse is the unified response format.
type CompletionResponse struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
}

// Usage reports token consumption.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Reported         bool
}

// NormalizeCompletionUsage validates provider token counters and derives an
// omitted total. A supplied total must exactly match its components.
func NormalizeCompletionUsage(promptTokens, completionTokens int, totalTokens *int) (Usage, bool) {
	if promptTokens < 0 || completionTokens < 0 || promptTokens > int(^uint(0)>>1)-completionTokens {
		return Usage{}, false
	}
	componentTotal := promptTokens + completionTokens
	total := componentTotal
	if totalTokens != nil {
		total = *totalTokens
	}
	if total < 0 || total != componentTotal {
		return Usage{}, false
	}
	return Usage{PromptTokens: promptTokens, CompletionTokens: completionTokens, TotalTokens: total, Reported: true}, true
}

func NormalizeEmbeddingUsage(promptTokens, totalTokens *int) Usage {
	if promptTokens == nil || totalTokens == nil {
		return Usage{}
	}
	usage, _ := NormalizeCompletionUsage(*promptTokens, 0, totalTokens)
	return usage
}

// EmbedRequest is the unified request format for embeddings.
type EmbedRequest struct {
	Model      string
	Endpoint   string
	Creds      *Credentials
	Inputs     []string
	Parameters map[string]string
	Invocation *modelinvocation.Resolved
}

// EmbedResponse contains embedding vectors.
type EmbedResponse struct {
	Embeddings [][]float64
	Usage      Usage
}
