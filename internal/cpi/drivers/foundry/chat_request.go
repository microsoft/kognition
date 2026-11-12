// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package foundry

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PrepareChatRequest adapts legacy chat fields for GPT-5/6 reasoning deployments.
// These deployments use completion-token limits and model-default sampling.
func PrepareChatRequest(payload []byte, deployment string) ([]byte, error) {
	name := strings.ToLower(deployment)
	if !strings.HasPrefix(name, "gpt-5") && !strings.HasPrefix(name, "gpt-6") {
		return payload, nil
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, fmt.Errorf("decode reasoning completion request: %w", err)
	}
	if request == nil {
		return nil, fmt.Errorf("reasoning completion request must be an object")
	}
	if effort, configured := request["reasoning_effort"]; strings.HasPrefix(name, "gpt-6") && (!configured || string(effort) == "null") {
		// GPT-6 function tools require non-reasoning Chat Completions requests.
		request["reasoning_effort"] = json.RawMessage(`"none"`)
	}
	if limit, exists := request["max_tokens"]; exists {
		if _, duplicate := request["max_completion_tokens"]; duplicate {
			return nil, fmt.Errorf("specify only one of max_tokens and max_completion_tokens")
		}
		request["max_completion_tokens"] = limit
		delete(request, "max_tokens")
	}
	delete(request, "temperature")
	return json.Marshal(request)
}
