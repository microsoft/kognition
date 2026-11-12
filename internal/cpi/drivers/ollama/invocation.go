// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ollama

import (
	"fmt"

	"github.com/kognition-ai/kognition/internal/modelinvocation"
)

func (d *Driver) InvocationContract(name string) (modelinvocation.Contract, error) {
	switch name {
	case "ollama-chat-v1":
		return modelinvocation.Contract{
			Name: name, Operation: "chat", Protocol: "ollama-chat",
			CompletionLimitParameter: "max_tokens", Temperature: true, ToolCalling: true,
		}, nil
	case "ollama-embedding-v1":
		return modelinvocation.Contract{Name: name, Operation: "embedding", Protocol: "ollama-embeddings"}, nil
	default:
		return modelinvocation.Contract{}, fmt.Errorf("unsupported Ollama invocation contract %q", name)
	}
}
