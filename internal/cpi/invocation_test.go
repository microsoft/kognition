// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/anthropic"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/bedrock"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/foundry"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/ollama"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/openai"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
)

func TestInvocationContractsEncodeOpaqueAliasesAcrossProviders(t *testing.T) {
	for _, test := range []struct {
		provider string
		contract string
		driver   func(*http.Client) cpi.Driver
		path     string
		reply    string
	}{
		{"openai", "openai-chat-v1", func(c *http.Client) cpi.Driver { return openai.New(c) }, "/v1/chat/completions", `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`},
		{"foundry reasoning", "openai-tool-chat-v1", func(c *http.Client) cpi.Driver { return foundry.New(c) }, "/chat/completions", `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`},
		{"anthropic", "anthropic-messages-v1", func(c *http.Client) cpi.Driver { return anthropic.New(c) }, "/v1/messages", `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`},
		{"bedrock", "bedrock-converse-v1", func(c *http.Client) cpi.Driver { return bedrock.New(c) }, "/model/production-chat/converse", `{"output":{"message":{"content":[{"text":"ok"}]}},"usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2}}`},
		{"ollama", "ollama-chat-v1", func(c *http.Client) cpi.Driver { return ollama.New(c) }, "/api/chat", `{"message":{"content":"ok"},"prompt_eval_count":1,"eval_count":1}`},
	} {
		t.Run(test.provider, func(t *testing.T) {
			maximum := int64(128)
			resolved := &modelinvocation.Resolved{
				Binding: api.ModelInvocationProfileBinding{Contract: test.contract},
				Spec:    api.ModelInvocationProfileSpec{Operation: "chat", Defaults: api.InvocationDefaults{MaxCompletionTokens: &maximum}},
			}
			if test.contract == "openai-tool-chat-v1" {
				resolved.Spec.Defaults.Reasoning = "disabled"
			}
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != test.path {
					t.Errorf("path = %s", r.URL.Path)
				}
				if (test.provider == "openai" || test.provider == "foundry reasoning") && r.URL.Query().Get("api-version") != "2024-12-01-preview" {
					t.Errorf("declared API version was not transmitted: %s", r.URL.RawQuery)
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				switch test.provider {
				case "foundry reasoning":
					if string(body["max_completion_tokens"]) != "128" || string(body["reasoning_effort"]) != `"none"` || body["max_tokens"] != nil || body["temperature"] != nil {
						t.Errorf("reasoning payload = %+v", body)
					}
				case "bedrock":
					var config map[string]int
					if json.Unmarshal(body["inferenceConfig"], &config) != nil || config["maxTokens"] != 128 {
						t.Errorf("Bedrock inference config = %s", body["inferenceConfig"])
					}
				case "ollama":
					var options map[string]int
					if json.Unmarshal(body["options"], &options) != nil || options["num_predict"] != 128 {
						t.Errorf("Ollama options = %s", body["options"])
					}
				default:
					if string(body["max_tokens"]) != "128" {
						t.Errorf("token limit = %s", body["max_tokens"])
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.reply))
			}))
			defer upstream.Close()
			driver := test.driver(upstream.Client())
			parameters := map[string]string{}
			if test.provider == "openai" || test.provider == "foundry reasoning" {
				parameters["apiVersion"] = "2024-12-01-preview"
			}
			response, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
				Model: "production-chat", Endpoint: upstream.URL, Invocation: resolved,
				Messages:   []cpi.Message{{Role: "user", Content: "hi"}},
				Parameters: parameters,
			})
			if err != nil || response.Content != "ok" || !response.Usage.Reported || calls != 1 {
				t.Fatalf("response=%+v error=%v calls=%d", response, err, calls)
			}
			if _, err := driver.(cpi.InvocationContractProvider).InvocationContract("unimplemented-protocol-v1"); err == nil {
				t.Fatal("unsupported contract accepted")
			}
		})
	}
}
