//go:build integration

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/foundry"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/native"
)

// This suite requires explicit paid-provider opt-in. It never creates cloud or
// Kubernetes resources. Local resource/authorization fixtures are not cluster acceptance.
func TestFoundryInvocationProfilesLive(t *testing.T) {
	if os.Getenv("KOGNITION_FOUNDRY_LIVE_TESTS") != "true" {
		t.Skip("set KOGNITION_FOUNDRY_LIVE_TESTS=true to authorize bounded paid inference")
	}
	for _, key := range []string{
		"MS_FOUNDRY_INFERENCE_ENDPOINT", "MS_FOUNDRY_MODEL", "MS_FOUNDRY_INVOCATION_CONTRACT",
		"MS_FOUNDRY_API_VERSION", "MS_FOUNDRY_EMBEDDING_MODEL", "MS_FOUNDRY_EMBEDDING_API_VERSION",
		"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET", "KOGNITION_FOUNDRY_LIVE_PYTHON",
	} {
		if os.Getenv(key) == "" {
			t.Fatalf("live profile validation requires %s", key)
		}
	}
	endpoint, err := url.Parse(os.Getenv("MS_FOUNDRY_INFERENCE_ENDPOINT"))
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil ||
		!strings.HasSuffix(endpoint.Hostname(), ".cognitiveservices.azure.com") ||
		endpoint.RawQuery != "" || strings.Trim(endpoint.Path, "/") != "" {
		t.Fatal("live tests require a Cognitive Services HTTPS resource root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	transport := &liveInvocationTransport{upstream: http.DefaultTransport, host: endpoint.Host, maximum: 8}
	t.Cleanup(func() { t.Logf("LIVE_USAGE %s", transport.summary()) })
	client := &http.Client{Transport: transport, Timeout: 70 * time.Second}
	driver := foundry.New(client)
	credentials := &cpi.Credentials{Type: "azure-identity", Data: map[string]string{
		"type": "azure-identity", "credentialType": "client-secret",
		"tenantId": os.Getenv("AZURE_TENANT_ID"), "clientId": os.Getenv("AZURE_CLIENT_ID"),
		"clientSecret": os.Getenv("AZURE_CLIENT_SECRET"),
	}}
	server, storage, authorizer, issuer, binding, _ := fixture(t)
	server.client = client
	server.drivers = invocationDrivers(client, server.tokens)
	const namespace = "team-a"
	maximum := int64(512)
	profile := api.ModelInvocationProfile{
		ObjectMeta: api.ObjectMeta{Name: "live-profile-v1", Namespace: namespace, UID: "live-profile-uid", ResourceVersion: 1},
		Spec: api.ModelInvocationProfileSpec{
			Operation:    "chat",
			Defaults:     api.InvocationDefaults{MaxCompletionTokens: &maximum, Reasoning: "disabled"},
			Limits:       api.InvocationLimits{MaxCompletionTokens: &maximum},
			Requirements: api.InvocationRequirements{ToolCalling: "required", Reasoning: "disabled"},
		},
	}
	put(t, storage, "ModelInvocationProfile", namespace, profile.Name, profile)
	model := api.Model{
		ObjectMeta: api.ObjectMeta{Name: "mock-model", Namespace: namespace, UID: "live-model-uid", ResourceVersion: 7},
		Spec: api.ModelSpec{
			Provider: "foundry", ClassName: "live-foundry", ModelID: os.Getenv("MS_FOUNDRY_MODEL"), Endpoint: endpoint.String(),
			CredentialRef:        &api.LocalObjectReference{Name: "model-key"},
			InvocationContract:   os.Getenv("MS_FOUNDRY_INVOCATION_CONTRACT"),
			InvocationProfileRef: &api.ModelInvocationProfileReference{Name: profile.Name, SpecDigest: invocationProfileDigest(t, profile.Spec)},
			Parameters:           map[string]string{"apiVersion": os.Getenv("MS_FOUNDRY_API_VERSION")},
		},
	}
	put(t, storage, "Model", namespace, model.Name, model)
	modelClass := api.ModelClass{ObjectMeta: api.ObjectMeta{Name: model.Spec.ClassName}, Spec: api.ModelClassSpec{Provisioner: "foundry"}}
	put(t, storage, "ModelClass", "", modelClass.Name, modelClass)
	credentialJSON, err := json.Marshal(credentials.Data)
	if err != nil {
		t.Fatal(err)
	}
	put(t, storage, "SecretRef", namespace, "model-key", api.SecretRef{
		ObjectMeta: api.ObjectMeta{Name: "model-key", Namespace: namespace, Annotations: map[string]string{"kognition.dev/internal-secret-data": string(credentialJSON)}},
		Spec:       api.SecretRefSpec{BackendType: "internal"},
	})
	invocation, err := cpi.ResolveInvocation(ctx, storage, driver, &model)
	if err != nil {
		t.Fatal(err)
	}
	binding.InvocationProfileBinding = &invocation.Binding
	issue := func(t *testing.T) string {
		t.Helper()
		capability, err := issuer.Issue(binding)
		if err != nil {
			t.Fatal(err)
		}
		return capability
	}
	invoke := func(t *testing.T, capability string, body map[string]any, expected int) liveChatCompletion {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := completionRequest(capability).WithContext(ctx)
		request.Body = io.NopCloser(bytes.NewReader(payload))
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != expected {
			t.Fatalf("gateway status=%d, want %d: %s", recorder.Code, expected, recorder.Body.String())
		}
		if expected != http.StatusOK {
			return liveChatCompletion{}
		}
		var completion liveChatCompletion
		if err := json.Unmarshal(recorder.Body.Bytes(), &completion); err != nil || len(completion.Choices) != 1 {
			t.Fatalf("invalid chat response shape: %v", err)
		}
		if completion.Usage.TotalTokens <= 0 || completion.Usage.TotalTokens != completion.Usage.PromptTokens+completion.Usage.CompletionTokens {
			t.Fatal("provider usage counters are missing or inconsistent")
		}
		return completion
	}
	run := func(name string, test func(*testing.T)) {
		t.Helper()
		if !t.Run(name, test) {
			t.Fatalf("stopping live suite after %s failed; no automatic compatibility retries", name)
		}
	}
	t.Logf("endpoint=%s deployment=%s contract=%s identity=client-secret profileDigest=%s; maximum eight model requests, 512 completion tokens each", endpoint.Host, model.Spec.ModelID, model.Spec.InvocationContract, invocation.Binding.SpecDigest)
	run("CPIChat", func(t *testing.T) {
		request, err := cpi.BindCompletion(ctx, storage, driver, &model, &cpi.CompletionRequest{
			Model: model.Spec.ModelID, Endpoint: model.Spec.Endpoint, Creds: credentials,
			Parameters: model.Spec.Parameters, Messages: []cpi.Message{{Role: "user", Content: "Return exactly READY."}},
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := driver.Complete(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(result.Content) != "READY" || !result.Usage.Reported || result.Usage.TotalTokens <= 0 {
			t.Fatalf("unexpected CPI completion content=%q usage=%+v", result.Content, result.Usage)
		}
		t.Logf("output=%q prompt=%d completion=%d total=%d", result.Content, result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.TotalTokens)
	})
	run("GatewayChat", func(t *testing.T) {
		result := invoke(t, issue(t), map[string]any{
			"model":    "untrusted-alias-must-not-route",
			"messages": []map[string]string{{"role": "user", "content": "Return exactly READY."}},
		}, http.StatusOK)
		if result.Choices[0].Message.Content != "READY" {
			t.Fatalf("unexpected gateway output %q", result.Choices[0].Message.Content)
		}
		t.Logf("output=%q usage=%+v", result.Choices[0].Message.Content, result.Usage)
	})
	run("GatewayToolRoundTrip", func(t *testing.T) {
		messages := []map[string]any{{"role": "user", "content": "Call sample_lookup with topic kognition. After its result, return exactly the verification_code from that result."}}
		tools := []map[string]any{{
			"type": "function", "function": map[string]any{
				"name": "sample_lookup", "description": "Returns the sample verification code for the requested topic.",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"topic": map[string]string{"type": "string"}}, "required": []string{"topic"}, "additionalProperties": false},
			},
		}}
		capability := issue(t)
		first := invoke(t, capability, map[string]any{
			"messages": messages, "tools": tools, "tool_choice": map[string]any{"type": "function", "function": map[string]string{"name": "sample_lookup"}},
		}, http.StatusOK)
		message := first.Choices[0].Message
		if len(message.ToolCalls) != 1 || message.ToolCalls[0].ID == "" || message.ToolCalls[0].Function.Name != "sample_lookup" {
			t.Fatal("provider did not produce the required function call")
		}
		call := message.ToolCalls[0]
		var arguments map[string]string
		if json.Unmarshal([]byte(call.Function.Arguments), &arguments) != nil || arguments["topic"] != "kognition" {
			t.Fatal("provider function arguments did not match the requested topic")
		}
		messages = append(messages,
			map[string]any{"role": "assistant", "content": message.Content, "tool_calls": message.ToolCalls},
			map[string]any{"role": "tool", "tool_call_id": call.ID, "content": `{"verification_code":"KOGNITION_TOOL_OK_7429"}`},
		)
		second := invoke(t, capability, map[string]any{"messages": messages, "tools": tools, "tool_choice": "none"}, http.StatusOK)
		if strings.TrimSpace(second.Choices[0].Message.Content) != "KOGNITION_TOOL_OK_7429" {
			t.Fatalf("tool result was not consumed: %q", second.Choices[0].Message.Content)
		}
		t.Logf("tool=%s arguments=%s result=%q firstUsage=%+v secondUsage=%+v", call.Function.Name, call.Function.Arguments, second.Choices[0].Message.Content, first.Usage, second.Usage)
	})
	run("GatewayStructuredOutput", func(t *testing.T) {
		result := invoke(t, issue(t), map[string]any{
			"messages": []map[string]string{{"role": "user", "content": "Return the verification status ready as the required JSON object."}},
			"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{
				"name": "verification", "strict": true,
				"schema": map[string]any{"type": "object", "properties": map[string]any{"status": map[string]any{"type": "string", "enum": []string{"ready"}}}, "required": []string{"status"}, "additionalProperties": false},
			}},
		}, http.StatusOK)
		var value map[string]string
		if err := json.Unmarshal([]byte(result.Choices[0].Message.Content), &value); err != nil || len(value) != 1 || value["status"] != "ready" {
			t.Fatalf("structured output violates the required schema: %q", result.Choices[0].Message.Content)
		}
		t.Logf("output=%s usage=%+v", result.Choices[0].Message.Content, result.Usage)
	})
	run("NativeRuntime", func(t *testing.T) {
		registry := cpi.NewRegistry()
		registry.MustRegister(driver)
		agent := &api.Agent{ObjectMeta: api.ObjectMeta{Name: "live-native", Namespace: namespace}, Spec: api.AgentSpec{Instructions: api.AgentInstructions{Inline: "Return exactly READY."}}}
		adapter := native.New(native.Config{Store: storage, Registry: registry, Model: &model, ModelClass: &modelClass, Credentials: credentials, Agent: agent})
		session, err := adapter.CreateSession(ctx, runtime.CreateSessionRequest{Namespace: namespace, AgentName: agent.Name, InitialCredits: 32})
		if err != nil {
			t.Fatal(err)
		}
		stream, err := adapter.Step(ctx, runtime.StepRequest{SessionID: session.SessionID, Input: "Verify the live deployment."})
		if err != nil {
			t.Fatal(err)
		}
		content := ""
		for event := range stream.Events {
			if event.Error != nil {
				t.Fatal(event.Error.Message)
			}
			if event.Output != nil && event.Output.IsFinal {
				content = event.Output.Content
			}
		}
		if strings.TrimSpace(content) != "READY" {
			t.Fatalf("native runtime output=%q", content)
		}
		checkpoint, err := adapter.Checkpoint(ctx, session.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.Resume(ctx, checkpoint.Token); err != nil {
			t.Fatal(err)
		}
		t.Logf("output=%q; profiled checkpoint restored without another inference", content)
	})
	run("PythonSDKThroughGateway", func(t *testing.T) {
		gateway := httptest.NewServer(server)
		t.Cleanup(gateway.Close)
		capability := issue(t)
		script := `
import json, os, sys
sys.path.insert(0, sys.argv[1])
from config import AgentConfig
from providers import create_model, model_runtime_session_capability
assert not os.environ.get("AZURE_CLIENT_SECRET")
values = json.load(sys.stdin)
config = AgentConfig(agent_name="planner", model_provider="openai-compatible",
    model_id="opaque-logical-model", model_gateway_url=values["url"],
    model_invocation_profile=True, timeout_seconds=60)
assert config.max_tokens is None and config.temperature is None
with model_runtime_session_capability(values["capability"]):
    response = create_model(config).invoke("Return exactly READY.")
assert response.content.strip() == "READY"
assert response.usage_metadata["total_tokens"] > 0
print(json.dumps({"output":response.content,"usage":response.usage_metadata,
    "provider_credentials_in_runtime":False}))
`
		root, err := filepath.Abs(filepath.Join("..", "..", "runtime", "agent"))
		if err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(ctx, os.Getenv("KOGNITION_FOUNDRY_LIVE_PYTHON"), "-c", script, root)
		input, err := json.Marshal(map[string]string{"url": gateway.URL, "capability": capability})
		if err != nil {
			t.Fatal(err)
		}
		command.Stdin = bytes.NewReader(input)
		for _, variable := range os.Environ() {
			name, _, _ := strings.Cut(variable, "=")
			if !strings.HasPrefix(name, "AZURE_") && !strings.HasPrefix(name, "MS_FOUNDRY_") && !strings.HasPrefix(name, "FOUNDRY_") {
				command.Env = append(command.Env, variable)
			}
		}
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("credential-isolated Python SDK execution failed: %v\n%s", err, output)
		}
		t.Log(strings.TrimSpace(string(output)))
	})
	run("ProfiledEmbeddings", func(t *testing.T) {
		dimensions := int64(1536)
		embeddingProfile := profile
		embeddingProfile.Name, embeddingProfile.UID = "live-embedding-v1", "embedding-profile-uid"
		embeddingProfile.Spec = api.ModelInvocationProfileSpec{Operation: "embedding", Defaults: api.InvocationDefaults{Dimensions: &dimensions}}
		put(t, storage, "ModelInvocationProfile", namespace, embeddingProfile.Name, embeddingProfile)
		embeddingModel := model
		embeddingModel.Spec.ModelID = os.Getenv("MS_FOUNDRY_EMBEDDING_MODEL")
		embeddingModel.Spec.InvocationContract = "openai-embedding-v1"
		embeddingModel.Spec.InvocationProfileRef = &api.ModelInvocationProfileReference{Name: embeddingProfile.Name, SpecDigest: invocationProfileDigest(t, embeddingProfile.Spec)}
		embeddingModel.Spec.Parameters = map[string]string{"apiVersion": os.Getenv("MS_FOUNDRY_EMBEDDING_API_VERSION")}
		resolved, err := cpi.ResolveInvocation(ctx, storage, driver, &embeddingModel)
		if err != nil {
			t.Fatal(err)
		}
		response, err := driver.Embed(ctx, &cpi.EmbedRequest{Model: embeddingModel.Spec.ModelID, Endpoint: embeddingModel.Spec.Endpoint, Creds: credentials,
			Invocation: resolved, Parameters: embeddingModel.Spec.Parameters, Inputs: []string{"Kognition invocation profile live verification."}})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Embeddings) != 1 || len(response.Embeddings[0]) != 1536 || response.Usage.TotalTokens <= 0 {
			t.Fatal("embedding dimensions or token usage do not match the declared profile")
		}
		for _, value := range response.Embeddings[0] {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				t.Fatal("provider returned a nonfinite embedding")
			}
		}
		t.Logf("deployment=%s dimensions=%d usage=%+v", embeddingModel.Spec.ModelID, len(response.Embeddings[0]), response.Usage)
	})
	run("RejectInvalidSettingsBeforeProvider", func(t *testing.T) {
		before := transport.count()
		capability := issue(t)
		for _, extra := range []map[string]any{
			{"temperature": 0.7}, {"unknown_setting": true}, {"max_tokens": 513},
			{"max_tokens": 1, "max_completion_tokens": 2}, {"reasoning_effort": "medium"},
		} {
			extra["messages"] = []map[string]string{{"role": "user", "content": "Must not reach the provider."}}
			invoke(t, capability, extra, http.StatusBadRequest)
		}
		authorizer.err = errors.New("live test grant revoked")
		invoke(t, capability, map[string]any{"messages": []map[string]string{{"role": "user", "content": "Must not reach the provider."}}}, http.StatusForbidden)
		authorizer.err = nil
		replacement := profile
		replacement.UID = "recreated-profile-uid"
		put(t, storage, "ModelInvocationProfile", namespace, profile.Name, replacement)
		invoke(t, capability, map[string]any{"messages": []map[string]string{{"role": "user", "content": "Must not reach the provider."}}}, http.StatusForbidden)
		put(t, storage, "ModelInvocationProfile", namespace, profile.Name, profile)
		if transport.count() != before {
			t.Fatal("invalid intent or stale authorization reached the live provider")
		}
		t.Log("five invalid settings, revoked model access and replaced profile rejected with zero provider calls")
	})
}

type liveChatCompletion struct {
	Choices []struct {
		Message struct {
			Content   string             `json:"content"`
			ToolCalls []liveFunctionCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type liveFunctionCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type liveInvocationTransport struct {
	upstream         http.RoundTripper
	host             string
	maximum          int
	mu               sync.Mutex
	requests         int
	promptTokens     int
	completionTokens int
}

func (s *liveInvocationTransport) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func (s *liveInvocationTransport) summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	encoded, _ := json.Marshal(map[string]int{"requests": s.requests, "promptTokens": s.promptTokens, "completionTokens": s.completionTokens, "totalTokens": s.promptTokens + s.completionTokens})
	return string(encoded)
}

func (s *liveInvocationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != s.host || request.Method != http.MethodPost ||
		(!strings.HasSuffix(request.URL.Path, "/chat/completions") && !strings.HasSuffix(request.URL.Path, "/embeddings")) {
		return nil, fmt.Errorf("unexpected live-test inference destination")
	}
	if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") || request.Header.Get("api-key") != "" {
		return nil, fmt.Errorf("live inference must use Entra bearer authentication, not resource keys")
	}
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	request.Body = io.NopCloser(bytes.NewReader(payload))
	var parameters map[string]json.RawMessage
	if err := json.Unmarshal(payload, &parameters); err != nil {
		return nil, err
	}
	if strings.HasSuffix(request.URL.Path, "/chat/completions") {
		var maximum int
		if json.Unmarshal(parameters["max_completion_tokens"], &maximum) != nil || maximum < 1 || maximum > 512 ||
			parameters["temperature"] != nil || parameters["max_tokens"] != nil || string(parameters["reasoning_effort"]) != `"none"` {
			return nil, fmt.Errorf("live chat request violated the declared bounded nonreasoning contract")
		}
	}
	s.mu.Lock()
	if s.requests >= s.maximum {
		s.mu.Unlock()
		return nil, fmt.Errorf("live test exceeded its eight-request budget")
	}
	s.requests++
	s.mu.Unlock()
	response, err := s.upstream.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRequestBytes+1))
	_ = response.Body.Close()
	if err != nil || len(body) > maxRequestBytes {
		return nil, fmt.Errorf("live provider response exceeded the bounded test payload")
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	var usage struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &usage) == nil {
		s.mu.Lock()
		s.promptTokens += usage.Usage.PromptTokens
		s.completionTokens += usage.Usage.CompletionTokens
		s.mu.Unlock()
	}
	return response, nil
}
