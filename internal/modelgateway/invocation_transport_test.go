// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
)

func TestGatewayUsesCPIContractsForNonOpenAIProviders(t *testing.T) {
	for _, test := range []struct {
		provider string
		contract string
		first    string
		second   string
	}{
		{"anthropic", "anthropic-messages-v1",
			`{"content":[{"type":"tool_use","id":"lookup-id","name":"lookup","input":{"query":"hello"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`,
			`{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`},
		{"aws-bedrock", "bedrock-converse-v1",
			`{"output":{"message":{"content":[{"toolUse":{"toolUseId":"lookup-id","name":"lookup","input":{"query":"hello"}}}]}},"stopReason":"tool_use","usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2}}`,
			`{"output":{"message":{"content":[{"text":"done"}]}},"stopReason":"end_turn","usage":{"inputTokens":2,"outputTokens":1,"totalTokens":3}}`},
		{"ollama", "ollama-chat-v1",
			`{"message":{"content":"","tool_calls":[{"function":{"name":"lookup","arguments":{"query":"hello"}}}]},"prompt_eval_count":1,"eval_count":1}`,
			`{"message":{"content":"done"},"prompt_eval_count":2,"eval_count":1}`},
	} {
		t.Run(test.provider, func(t *testing.T) {
			server, storage, _, issuer, binding, _ := fixture(t)
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") == "Bearer runtime-capability" {
					t.Error("runtime capability leaked to provider")
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if test.provider == "anthropic" && r.Header.Get("x-api-key") != "provider-key" {
					t.Error("provider credential was not resolved by the gateway")
				}
				if test.provider == "aws-bedrock" && !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
					t.Error("Bedrock request was not signed")
				}
				if calls == 2 {
					var messages []map[string]json.RawMessage
					if err := json.Unmarshal(body["messages"], &messages); err != nil || len(messages) != 3 {
						t.Errorf("tool history missing: %s, %v", body["messages"], err)
					}
					if test.provider == "anthropic" || test.provider == "aws-bedrock" {
						if string(messages[2]["role"]) != `"user"` || !strings.Contains(string(messages[2]["content"]), "result") {
							t.Errorf("native tool-result encoding lost: %s", body["messages"])
						}
					} else if string(messages[2]["tool_name"]) != `"lookup"` || messages[1]["tool_calls"] == nil {
						t.Errorf("Ollama tool history lost: %s", body["messages"])
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if calls == 1 {
					_, _ = w.Write([]byte(test.first))
				} else {
					_, _ = w.Write([]byte(test.second))
				}
			}))
			t.Cleanup(upstream.Close)
			var model api.Model
			if err := json.Unmarshal(storage.data["Model/team-a/mock-model"], &model); err != nil {
				t.Fatal(err)
			}
			model.Spec.Provider, model.Spec.Endpoint, model.Spec.ModelID = test.provider, upstream.URL, "production-chat"
			model.Spec.InvocationContract, model.Spec.InvocationProfileRef = test.contract, &api.ModelInvocationProfileReference{Name: "portable-v1"}
			maximum := int64(128)
			profile := api.ModelInvocationProfile{ObjectMeta: api.ObjectMeta{Name: "portable-v1", Namespace: "team-a", UID: "profile-uid"},
				Spec: api.ModelInvocationProfileSpec{Operation: "chat", Defaults: api.InvocationDefaults{MaxCompletionTokens: &maximum}, Requirements: api.InvocationRequirements{ToolCalling: "required"}}}
			put(t, storage, "ModelInvocationProfile", "team-a", profile.Name, profile)
			model.Spec.InvocationProfileRef.SpecDigest = invocationProfileDigest(t, profile.Spec)
			put(t, storage, "Model", "team-a", model.Name, model)
			credential := `{"type":"api-key","apiKey":"provider-key","accessKeyId":"access","secretAccessKey":"secret","region":"us-east-1"}`
			put(t, storage, "SecretRef", "team-a", "model-key", api.SecretRef{ObjectMeta: api.ObjectMeta{Name: "model-key", Namespace: "team-a", Annotations: map[string]string{"kognition.dev/internal-secret-data": credential}}, Spec: api.SecretRefSpec{BackendType: "internal"}})
			resolved, err := modelinvocation.Load(context.Background(), storage, &model)
			if err != nil {
				t.Fatal(err)
			}
			binding.InvocationProfileBinding = &resolved.Binding
			token, err := issuer.Issue(binding)
			if err != nil {
				t.Fatal(err)
			}
			tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]string{"type": "object"}}}}
			messages := []map[string]any{{"role": "user", "content": "lookup hello"}}
			invoke := func(body map[string]any) *httptest.ResponseRecorder {
				t.Helper()
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				request := completionRequest(token)
				request.Body = io.NopCloser(strings.NewReader(string(encoded)))
				result := httptest.NewRecorder()
				server.ServeHTTP(result, request)
				return result
			}
			first := invoke(map[string]any{"messages": messages, "tools": tools})
			if first.Code != http.StatusOK {
				t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
			}
			var completion struct {
				Choices []struct {
					Message map[string]any `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(first.Body.Bytes(), &completion); err != nil {
				t.Fatal(err)
			}
			assistant := completion.Choices[0].Message
			callsJSON, _ := json.Marshal(assistant["tool_calls"])
			var toolCalls []struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(callsJSON, &toolCalls); err != nil || len(toolCalls) != 1 || toolCalls[0].ID == "" {
				t.Fatalf("invalid normalized tool calls: %s, %v", callsJSON, err)
			}
			messages = append(messages, assistant, map[string]any{"role": "tool", "tool_call_id": toolCalls[0].ID, "content": "result"})
			second := invoke(map[string]any{"messages": messages, "tools": tools})
			if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"content":"done"`) || !strings.Contains(second.Body.String(), `"finish_reason":"stop"`) {
				t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
			}
			rejected := invoke(map[string]any{"messages": messages, "parallel_tool_calls": false})
			if rejected.Code != http.StatusBadRequest || calls != 2 {
				t.Fatalf("unencodable settings reached provider: status=%d calls=%d", rejected.Code, calls)
			}
		})
	}
}
