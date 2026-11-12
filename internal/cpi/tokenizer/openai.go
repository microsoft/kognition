// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package tokenizer

import (
	"fmt"

	"github.com/kognition-ai/kognition/internal/cpi"
	tiktoken "github.com/pkoukk/tiktoken-go"
)

type openAIChatFormat struct {
	encodingName     string
	tokensPerMessage int
}

var openAIChatFormatByModel = map[string]openAIChatFormat{
	"gpt-3.5-turbo-0613":     {encodingName: "cl100k_base", tokensPerMessage: 3},
	"gpt-3.5-turbo-16k-0613": {encodingName: "cl100k_base", tokensPerMessage: 3},
	"gpt-4-0314":             {encodingName: "cl100k_base", tokensPerMessage: 3},
	"gpt-4-0613":             {encodingName: "cl100k_base", tokensPerMessage: 3},
	"gpt-4-32k-0314":         {encodingName: "cl100k_base", tokensPerMessage: 3},
	"gpt-4-32k-0613":         {encodingName: "cl100k_base", tokensPerMessage: 3},
}

// CountOpenAIChatInput applies the exact chat framing for a versioned model.
// Unknown models and non-canary request shapes fail closed.
func CountOpenAIChatInput(req *cpi.CompletionRequest) (int, error) {
	if req == nil {
		return 0, fmt.Errorf("completion request is required")
	}
	format, ok := openAIChatFormatByModel[req.Model]
	if !ok {
		return 0, fmt.Errorf("exact tokenizer unavailable for model %q", req.Model)
	}
	if len(req.Tools) != 0 {
		return 0, fmt.Errorf("exact tokenization unavailable for requests with tools")
	}

	encoding, err := tiktoken.GetEncoding(format.encodingName)
	if err != nil {
		return 0, fmt.Errorf("load exact tokenizer for model %q: %w", req.Model, err)
	}

	count := 3 // Every reply is primed with <|start|>assistant<|message|>.
	for _, message := range req.Messages {
		if len(message.ToolCalls) != 0 || message.ToolCallID != "" {
			return 0, fmt.Errorf("exact tokenization unavailable for tool messages")
		}
		switch message.Role {
		case "system", "user", "assistant":
		default:
			return 0, fmt.Errorf("exact tokenization unavailable for role %q", message.Role)
		}
		count += format.tokensPerMessage
		count += len(encoding.EncodeOrdinary(message.Role))
		count += len(encoding.EncodeOrdinary(message.Content))
	}

	return count, nil
}
