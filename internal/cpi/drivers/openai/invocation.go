// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package openai

import (
	"fmt"
	"net/http"

	"github.com/kognition-ai/kognition/internal/modelinvocation"
)

// InvocationContract describes a wire dialect, never a deployment-name family.
func InvocationContract(name string) (modelinvocation.Contract, error) {
	contract := modelinvocation.Contract{
		Name: name, Operation: "chat", Protocol: "openai-chat-completions",
		CompletionLimitParameter: "max_tokens", Temperature: true,
		ToolCalling: true, StructuredOutput: true, DisabledReasoning: true,
		APIVersion: true,
	}
	switch name {
	case "openai-chat-v1":
	case "openai-reasoning-chat-v1", "openai-tool-chat-v1":
		contract.CompletionLimitParameter = "max_completion_tokens"
		contract.Temperature = false
		contract.DisabledReasoningValue = "none"
		contract.RequiredReasoningValue = "medium"
		contract.ToolsRequireReasoningDisabled = name == "openai-tool-chat-v1"
	case "openai-embedding-v1":
		contract = modelinvocation.Contract{Name: name, Operation: "embedding", Protocol: "openai-embeddings", Dimensions: true, APIVersion: true}
	default:
		return modelinvocation.Contract{}, fmt.Errorf("unsupported OpenAI invocation contract %q", name)
	}
	return contract, nil
}

func (d *Driver) InvocationContract(name string) (modelinvocation.Contract, error) {
	return InvocationContract(name)
}

func applyInvocationAPIVersion(request *http.Request, parameters map[string]string) {
	version := parameters["apiVersion"]
	if version == "" {
		version = parameters["api-version"]
	}
	if version != "" {
		query := request.URL.Query()
		query.Set("api-version", version)
		request.URL.RawQuery = query.Encode()
	}
}
