// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dispatcher

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
)

func ollamaGatewayDispatcher(t *testing.T, withProfile bool) (*K8sDispatcher, *v1alpha1.Agent) {
	t.Helper()
	completion := int64(512)
	limit := int64(1024)
	profile := v1alpha1.ModelInvocationProfile{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "ollama-chat", Namespace: "default", UID: "profile-uid"},
		Spec: v1alpha1.ModelInvocationProfileSpec{
			Operation: "chat",
			Defaults:  v1alpha1.InvocationDefaults{MaxCompletionTokens: &completion},
			Limits:    v1alpha1.InvocationLimits{MaxCompletionTokens: &limit},
		},
	}
	model := v1alpha1.Model{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "local-chat", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			Provider: "ollama", ModelID: "qwen2.5:1.5b", Endpoint: "http://ollama.providers.svc:11434",
		},
	}
	data := map[string][]byte{}
	if withProfile {
		digest, err := modelinvocation.Digest(profile.Spec)
		if err != nil {
			t.Fatal(err)
		}
		model.Spec.InvocationProfileRef = &v1alpha1.ModelInvocationProfileReference{Name: profile.Name, SpecDigest: digest}
		model.Spec.InvocationContract = "ollama-chat-v1"
		data["ModelInvocationProfile/default/ollama-chat"] = resourceBytes(t, profile)
	}
	data["Model/default/local-chat"] = resourceBytes(t, model)
	dispatcher := &K8sDispatcher{
		store:  &stubStore{data: data},
		logger: zap.NewNop(),
		config: Config{ModelGatewayURL: "http://model-gateway.kognition-system:8093"},
	}
	agent := &v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "local-agent", Namespace: "default"},
		Spec:       v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "local-chat"}},
	}
	return dispatcher, agent
}

func TestRuntimeEnvServesProfiledOllamaThroughModelGateway(t *testing.T) {
	dispatcher, agent := ollamaGatewayDispatcher(t, true)
	envs := envToMap(dispatcher.runtimeEnv(context.Background(), agent))
	if envs["KOGNITION_MODEL_PROVIDER"] != "openai-compatible" || envs["KOGNITION_MODEL_INVOCATION_PROFILE"] != "true" || envs["KOGNITION_MODEL_GATEWAY_URL"] == "" {
		t.Fatalf("gateway runtime environment = %#v", envs)
	}
	for _, name := range []string{"KOGNITION_MODEL_ENDPOINT", "KOGNITION_API_KEY"} {
		if _, present := envs[name]; present {
			t.Fatalf("gateway runtime must not receive %s", name)
		}
	}
}

func TestRuntimeEnvRejectsUnprofiledOllamaThroughModelGateway(t *testing.T) {
	dispatcher, agent := ollamaGatewayDispatcher(t, false)
	_, err := dispatcher.runtimeEnv(context.Background(), agent)
	if err == nil || !strings.Contains(err.Error(), "explicit ModelInvocationProfile") {
		t.Fatalf("unprofiled non-OpenAI provider error = %v", err)
	}
}

func TestRequireModelGatewayProvider(t *testing.T) {
	profiled := &v1alpha1.Model{Spec: v1alpha1.ModelSpec{InvocationProfileRef: &v1alpha1.ModelInvocationProfileReference{Name: "p"}}}
	for _, test := range []struct {
		name     string
		provider string
		model    *v1alpha1.Model
		allowed  bool
	}{
		{"openai-compatible without profile", "openai-compatible", &v1alpha1.Model{}, true},
		{"foundry without profile", "foundry", &v1alpha1.Model{}, true},
		{"ollama with profile", "ollama", profiled, true},
		{"anthropic with profile", "anthropic", profiled, true},
		{"ollama without profile", "ollama", &v1alpha1.Model{}, false},
		{"empty provider with profile", "", profiled, false},
		{"empty provider without profile", "", &v1alpha1.Model{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := requireModelGatewayProvider(test.provider, test.model); (err == nil) != test.allowed {
				t.Fatalf("requireModelGatewayProvider(%q) = %v, allowed %v", test.provider, err, test.allowed)
			}
		})
	}
}
