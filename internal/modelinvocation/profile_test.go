// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelinvocation_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/openai"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/store"
)

func integer(value int64) *int64 { return &value }

func profile(contract string) *modelinvocation.Resolved {
	return &modelinvocation.Resolved{
		Binding: api.ModelInvocationProfileBinding{Name: "profile-v1", UID: "profile-uid", Contract: contract},
		Spec: api.ModelInvocationProfileSpec{
			Operation: "chat",
			Defaults:  api.InvocationDefaults{MaxCompletionTokens: integer(512), Reasoning: "disabled"},
			Limits:    api.InvocationLimits{MaxCompletionTokens: integer(1024)},
		},
	}
}

func TestProfileResolvesOmissionsAndRejectsExplicitIncompatibilities(t *testing.T) {
	resolved := profile("openai-tool-chat-v1")
	contract, err := openai.InvocationContract(resolved.Binding.Contract)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := resolved.Prepare(contract, nil, true, false, 0)
	if err != nil || settings["max_completion_tokens"] != "512" || settings["reasoning_effort"] != "none" || settings["temperature"] != "" {
		t.Fatalf("defaults = %#v, %v", settings, err)
	}
	settings, err = resolved.Prepare(contract, nil, true, false, 128)
	if err != nil || settings["max_completion_tokens"] != "128" {
		t.Fatalf("Agent's explicit limit must override the omitted default: %#v, %v", settings, err)
	}
	for _, test := range []struct {
		name     string
		settings map[string]string
		tools    bool
		message  string
	}{
		{"unsupported sampling", map[string]string{"temperature": "0.7"}, false, "temperature is unsupported"},
		{"unknown setting", map[string]string{"made_up": "1"}, false, "unknown invocation parameter"},
		{"invalid limit", map[string]string{"max_tokens": "0"}, false, "positive integer"},
		{"fractional limit", map[string]string{"max_tokens": "3.5"}, false, "positive integer"},
		{"wire integer overflow", map[string]string{"max_tokens": "2147483648"}, false, "positive integer"},
		{"duplicate limits", map[string]string{"max_tokens": "1", "max_completion_tokens": "2"}, false, "conflicting completion"},
		{"hard limit", map[string]string{"max_tokens": "1025"}, false, "hard limit"},
		{"nonfinite sampling", map[string]string{"temperature": "NaN"}, false, "finite"},
		{"tool reasoning", map[string]string{"reasoning": "required"}, true, "explicitly disabled reasoning"},
		{"unsupported effort", map[string]string{"reasoning_effort": "high"}, false, "unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := resolved.Prepare(contract, test.settings, test.tools, false, 0)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v, want %q", err, test.message)
			}
		})
	}
	if _, err := resolved.Prepare(contract, map[string]string{"max_tokens": "256"}, false, false, 128); err == nil {
		t.Fatal("explicit caller limit was silently weakened")
	}
	resolved.Spec.Requirements.Reasoning = "disabled"
	if _, err := resolved.Prepare(contract, map[string]string{"reasoning": "required"}, false, false, 0); err == nil {
		t.Fatal("explicit reasoning overrode its requirement")
	}
}

func TestProfileChatEnvelopePreservesToolsAndStructuredOutput(t *testing.T) {
	resolved := profile("openai-chat-v1")
	contract, _ := openai.InvocationContract(resolved.Binding.Contract)
	payload := []byte(`{"model":"opaque-alias","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup"}}],"tool_choice":{"type":"function","function":{"name":"lookup"}},"parallel_tool_calls":false,"response_format":{"type":"json_schema","json_schema":{"name":"result","schema":{"type":"object"}}}}`)
	prepared, err := resolved.PrepareChatJSON(contract, payload, 0)
	if err != nil {
		t.Fatal(err)
	}
	var actual, original map[string]json.RawMessage
	if json.Unmarshal(prepared, &actual) != nil || json.Unmarshal(payload, &original) != nil {
		t.Fatal("invalid JSON")
	}
	for _, name := range []string{"model", "messages", "tools", "tool_choice", "parallel_tool_calls", "response_format"} {
		var left, right any
		if json.Unmarshal(actual[name], &left) != nil || json.Unmarshal(original[name], &right) != nil {
			t.Fatalf("lost envelope field %s", name)
		}
		leftJSON, _ := json.Marshal(left)
		rightJSON, _ := json.Marshal(right)
		if string(leftJSON) != string(rightJSON) {
			t.Fatalf("%s changed", name)
		}
	}
	for _, invalid := range []string{
		`null`, `[]`, `{"max_tokens":"128"}`, `{"temperature":null}`,
		`{"max_tokens":1,"max_completion_tokens":2}`, `{"stream":true}`,
		`{"top_p":0.8}`, `{"tools":true}`, `{"reasoning_effort":null}`,
		`{"max_tokens":1,"max_tokens":2}`,
	} {
		if _, err := resolved.PrepareChatJSON(contract, []byte(invalid), 0); err == nil {
			t.Fatalf("invalid request accepted: %s", invalid)
		}
	}
}

func TestProfileValidatesOperationAndCapabilities(t *testing.T) {
	resolved := profile("openai-tool-chat-v1")
	resolved.Spec.Requirements.ToolCalling = "required"
	resolved.Spec.Requirements.StructuredOutput = "required"
	if err := resolved.ValidateContract(modelinvocation.Contract{Name: resolved.Binding.Contract, Operation: "chat"}); err == nil {
		t.Fatal("capability requirements were ignored")
	}
	contract, _ := openai.InvocationContract("openai-embedding-v1")
	resolved.Binding.Contract = contract.Name
	if err := resolved.ValidateContract(contract); err == nil {
		t.Fatal("chat profile accepted an embedding contract")
	}
	resolved.Spec = api.ModelInvocationProfileSpec{Operation: "embedding", Defaults: api.InvocationDefaults{Dimensions: integer(1536)}}
	settings, err := resolved.Prepare(contract, nil, false, false, 0)
	if err != nil || settings["dimensions"] != "1536" {
		t.Fatalf("embedding settings = %#v, %v", settings, err)
	}
	if _, err := resolved.Prepare(contract, map[string]string{"temperature": "0.1"}, false, false, 0); err == nil {
		t.Fatal("embedding accepted chat settings")
	}
}

type profileStore struct {
	store.Store
	profile api.ModelInvocationProfile
}

func (s *profileStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	if kind != "ModelInvocationProfile" || namespace != s.profile.Namespace || name != s.profile.Name {
		return nil, store.ErrNotFound
	}
	return json.Marshal(s.profile)
}

func TestProfileBindingTracksSpecAndIncarnation(t *testing.T) {
	model := &api.Model{ObjectMeta: api.ObjectMeta{Name: "model", Namespace: "team-a"}, Spec: api.ModelSpec{
		InvocationProfileRef: &api.ModelInvocationProfileReference{Name: "profile-v1"},
		InvocationContract:   "openai-chat-v1",
	}}
	storage := &profileStore{profile: api.ModelInvocationProfile{
		ObjectMeta: api.ObjectMeta{Name: "profile-v1", Namespace: "team-a", UID: "first"},
		Spec:       profile("openai-chat-v1").Spec,
	}}
	model.Spec.InvocationProfileRef.SpecDigest, _ = modelinvocation.Digest(storage.profile.Spec)
	first, err := modelinvocation.Load(context.Background(), storage, model)
	if err != nil {
		t.Fatal(err)
	}
	storage.profile.UID = "replacement"
	second, err := modelinvocation.Load(context.Background(), storage, model)
	if err != nil || modelinvocation.EqualBinding(&first.Binding, &second.Binding) {
		t.Fatalf("profile replacement retained a session binding: %v", err)
	}
	storage.profile.Spec.Defaults.MaxCompletionTokens = integer(513)
	if _, err := modelinvocation.Load(context.Background(), storage, model); err == nil {
		t.Fatal("a new invocation adopted a different spec behind the reviewed profile name")
	}
	storage.profile.Namespace = "another-team"
	if _, err := modelinvocation.Load(context.Background(), storage, model); err == nil {
		t.Fatal("cross-namespace profile resolved")
	}
	model.Spec.Parameters = map[string]string{"temperature": "0.7"}
	if err := modelinvocation.ValidateModel(model.Spec); err == nil {
		t.Fatal("ambiguous profile and legacy generation parameters accepted")
	}
}
