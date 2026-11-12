// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/apiserver"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/openai"
)

func reconcileModel(t *testing.T, h *testHarness, name string) v1alpha1.Model {
	t.Helper()
	reconciler := controller.NewModelReconciler(h.Store, h.Registry, h.Logger)
	if _, err := reconciler.Reconcile(context.Background(), controller.Request{
		Kind:      "Model",
		Namespace: testNS,
		Name:      name,
	}); err != nil {
		t.Fatalf("reconcile Model %q: %v", name, err)
	}

	var model v1alpha1.Model
	h.getResource(t, "Model", testNS, name, &model)
	return model
}

// TC-0056: A healthy registered provider reports model availability and capabilities.
func TestModelsCPI_HealthyProviderReportsAvailability(t *testing.T) {
	h := newTestHarness(t)
	h.Driver.probeResult = &cpi.HealthResult{
		Available:    true,
		Reason:       "ProbeOK",
		Message:      "provider accepted the credentials",
		Capabilities: []string{"chat", "embeddings"},
	}
	h.seedResource(t, "ModelClass", "", "healthy-class", makeModelClass("healthy-class", "", "test-provider"))
	h.seedResource(t, "SecretRef", testNS, "model-credentials", internalSecretRef("model-credentials", "valid-key"))
	h.seedResource(t, "Model", testNS, "healthy-model", v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "healthy-model", Namespace: testNS},
		Spec: v1alpha1.ModelSpec{
			ClassName:     "healthy-class",
			Provider:      "test-provider",
			ModelID:       "test-chat-model",
			Endpoint:      "https://models.example.test",
			CredentialRef: &v1alpha1.LocalObjectReference{Name: "model-credentials"},
		},
	})

	model := reconcileModel(t, h, "healthy-model")
	if !model.Status.Available || model.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("model status = available %v, phase %q; want available Ready", model.Status.Available, model.Status.Phase)
	}
	if len(model.Status.Capabilities) != 2 || model.Status.Capabilities[0] != "chat" || model.Status.Capabilities[1] != "embeddings" {
		t.Fatalf("capabilities = %v, want [chat embeddings]", model.Status.Capabilities)
	}
	probes := h.Driver.ProbeInvocations()
	if len(probes) != 1 || probes[0].credentials == nil || probes[0].credentials.Data["apiKey"] != "valid-key" {
		t.Fatalf("probe credentials = %+v, want resolved API key", probes)
	}
}

// TC-0057: An invalid model endpoint persists an unavailable status and diagnostic.
func TestModelsCPI_InvalidEndpointReportsUnavailable(t *testing.T) {
	h := newTestHarness(t)
	registry := cpi.NewRegistry()
	registry.MustRegister(openai.New(nil))
	h.Registry = registry
	h.seedResource(t, "ModelClass", "", "openai-class", makeModelClass("openai-class", "", "openai-compatible"))
	h.seedResource(t, "Model", testNS, "unavailable-model", v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "unavailable-model", Namespace: testNS},
		Spec: v1alpha1.ModelSpec{
			ClassName: "openai-class",
			Provider:  "openai-compatible",
			ModelID:   "test-chat-model",
			Endpoint:  "not-a-valid-url",
		},
	})

	model := reconcileModel(t, h, "unavailable-model")
	if model.Status.Available || model.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("model status = available %v, phase %q; want unavailable Failed", model.Status.Available, model.Status.Phase)
	}
	ready := findCondition(model.Status.Conditions, "Ready")
	if ready == nil || ready.Reason != "ValidationFailed" || ready.Message == "" {
		t.Fatalf("Ready condition = %+v, want ValidationFailed diagnostic", ready)
	}
}

// TC-0058: Models route independently to the provider registered for each model.
func TestModelsCPI_RoutesModelsToIndependentProviders(t *testing.T) {
	h := newTestHarness(t)
	second := newMockCPIDriver("second-provider")
	h.Driver.completionFn = func(_ context.Context, _ *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
		return &cpi.CompletionResponse{Content: "first provider response", Usage: cpi.Usage{Reported: true}}, nil
	}
	second.completionFn = func(_ context.Context, _ *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
		return &cpi.CompletionResponse{Content: "second provider response", Usage: cpi.Usage{Reported: true}}, nil
	}
	if err := h.Registry.Register(second); err != nil {
		t.Fatalf("register second provider: %v", err)
	}
	h.seedResource(t, "ModelClass", "", "first-class", makeModelClass("first-class", "", "test-provider"))
	h.seedResource(t, "ModelClass", "", "second-class", makeModelClass("second-class", "", "second-provider"))

	models := []v1alpha1.Model{
		{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "first-model", Namespace: testNS},
			Spec: v1alpha1.ModelSpec{
				ClassName: "first-class",
				Provider:  "test-provider",
				ModelID:   "first",
				Endpoint:  "https://first.example.test",
			},
		},
		{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "second-model", Namespace: testNS},
			Spec: v1alpha1.ModelSpec{
				ClassName: "second-class",
				Provider:  "second-provider",
				ModelID:   "second",
				Endpoint:  "https://second.example.test",
			},
		},
	}
	for _, model := range models {
		h.seedResource(t, "Model", testNS, model.ObjectMeta.Name, model)
		got := reconcileModel(t, h, model.ObjectMeta.Name)
		if !got.Status.Available {
			t.Fatalf("model %q was unavailable", model.ObjectMeta.Name)
		}
	}

	firstProbes := h.Driver.ProbeInvocations()
	if len(firstProbes) != 1 || firstProbes[0].model.ModelID != "first" {
		t.Fatalf("first provider probes = %+v, want only model first", firstProbes)
	}
	secondProbes := second.ProbeInvocations()
	if len(secondProbes) != 1 || secondProbes[0].model.ModelID != "second" {
		t.Fatalf("second provider probes = %+v, want only model second", secondProbes)
	}

	h.seedResource(t, "Agent", testNS, "first-agent", makeAgent("first-agent", testNS, "first-model", v1alpha1.PhaseReady))
	h.seedResource(t, "Agent", testNS, "second-agent", makeAgent("second-agent", testNS, "second-model", v1alpha1.PhaseReady))

	wants := map[string]string{
		"first-agent":  "first provider response",
		"second-agent": "second provider response",
	}
	for agentName, want := range wants {
		response := h.doReq(t, http.MethodPost, apiBase+"/agents/"+agentName+"/chat", apiserver.ChatRequest{Message: "hello"})
		if response.Code != http.StatusOK {
			t.Fatalf("chat with %s returned %d: %s", agentName, response.Code, response.Body.String())
		}
		var chat apiserver.ChatResponse
		if err := json.Unmarshal(response.Body.Bytes(), &chat); err != nil {
			t.Fatalf("unmarshal %s chat response: %v", agentName, err)
		}
		if chat.Response != want {
			t.Fatalf("%s response = %q, want %q", agentName, chat.Response, want)
		}
	}
	if calls := h.Driver.Invocations(); len(calls) != 1 || calls[0].Model != "first" {
		t.Fatalf("first provider completions = %+v, want only model first", calls)
	}
	if calls := second.Invocations(); len(calls) != 1 || calls[0].Model != "second" {
		t.Fatalf("second provider completions = %+v, want only model second", calls)
	}
}

func internalSecretRef(name, apiKey string) v1alpha1.SecretRef {
	data, _ := json.Marshal(map[string]string{"type": "api-key", "apiKey": apiKey})
	return v1alpha1.SecretRef{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "SecretRef"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:        name,
			Namespace:   testNS,
			Annotations: map[string]string{"kognition.dev/internal-secret-data": string(data)},
		},
		Spec: v1alpha1.SecretRefSpec{
			BackendType: "internal",
			BackendRef:  "secret/" + name,
			Key:         "apiKey",
		},
	}
}

func findCondition(conditions []v1alpha1.Condition, conditionType string) *v1alpha1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
