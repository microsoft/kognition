// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package tokenizer

import (
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/cpi"
)

func TestCountOpenAIChatInput(t *testing.T) {
	request := &cpi.CompletionRequest{
		Model: "gpt-4-0613",
		Messages: []cpi.Message{
			{Role: "system", Content: "Return exactly KOGNITION_CANARY_OK"},
			{Role: "user", Content: "health check"},
		},
	}

	count, err := CountOpenAIChatInput(request)
	if err != nil {
		t.Fatalf("CountOpenAIChatInput() error: %v", err)
	}
	if count != 22 {
		t.Fatalf("CountOpenAIChatInput() = %d, want 22", count)
	}
}

func TestCountOpenAIChatInputRejectsUnknownModel(t *testing.T) {
	_, err := CountOpenAIChatInput(&cpi.CompletionRequest{Model: "production-deployment"})
	if err == nil || !strings.Contains(err.Error(), "exact tokenizer unavailable") {
		t.Fatalf("CountOpenAIChatInput() error = %v, want tokenizer unavailable", err)
	}
}
