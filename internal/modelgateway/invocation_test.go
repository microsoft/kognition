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

func invocationProfileDigest(t *testing.T, spec api.ModelInvocationProfileSpec) string {
	t.Helper()
	digest, err := modelinvocation.Digest(spec)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestGatewayEnforcesInvocationProfileAndExactIncarnation(t *testing.T) {
	server, storage, _, issuer, binding, _ := fixture(t)
	maximum := int64(128)
	profile := api.ModelInvocationProfile{
		ObjectMeta: api.ObjectMeta{Name: "profile-v1", Namespace: "team-a", UID: "profile-uid"},
		Spec:       api.ModelInvocationProfileSpec{Operation: "chat", Defaults: api.InvocationDefaults{MaxCompletionTokens: &maximum, Reasoning: "disabled"}},
	}
	put(t, storage, "ModelInvocationProfile", "team-a", profile.Name, profile)
	var model api.Model
	if err := json.Unmarshal(storage.data["Model/team-a/mock-model"], &model); err != nil {
		t.Fatal(err)
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if string(payload["max_completion_tokens"]) != "128" || payload["max_tokens"] != nil || payload["temperature"] != nil ||
			string(payload["reasoning_effort"]) != `"none"` || string(payload["model"]) != `"production-chat"` {
			t.Errorf("invalid prepared payload: %+v", payload)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	model.Spec.ModelID = "production-chat"
	model.Spec.Endpoint = upstream.URL
	model.Spec.InvocationProfileRef = &api.ModelInvocationProfileReference{Name: profile.Name, SpecDigest: invocationProfileDigest(t, profile.Spec)}
	model.Spec.InvocationContract = "openai-tool-chat-v1"
	put(t, storage, "Model", "team-a", model.Name, model)
	resolved, err := modelinvocation.Load(context.Background(), storage, &model)
	if err != nil {
		t.Fatal(err)
	}

	binding.InvocationProfileBinding = &resolved.Binding
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		body   string
		status int
	}{
		{`{"model":"untrusted","messages":[{"role":"user","content":"hi"}]}`, http.StatusOK},
		{`{"messages":[],"temperature":0.7}`, http.StatusBadRequest},
		{`{"messages":[],"unknown_parameter":true}`, http.StatusBadRequest},
		{`{"messages":[],"max_tokens":1,"max_completion_tokens":2}`, http.StatusBadRequest},
	} {
		response := httptest.NewRecorder()
		request := completionRequest(capability)
		request.Body = io.NopCloser(strings.NewReader(test.body))
		server.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("status=%d body=%s for %s", response.Code, response.Body.String(), test.body)
		}
	}
	if calls != 1 {
		t.Fatalf("invalid requests reached upstream: %d calls", calls)
	}
	for _, replacement := range []api.ModelInvocationProfile{
		{ObjectMeta: api.ObjectMeta{Name: profile.Name, Namespace: "team-a", UID: "replacement"}, Spec: profile.Spec},
		{ObjectMeta: profile.ObjectMeta, Spec: api.ModelInvocationProfileSpec{Operation: "chat", Defaults: api.InvocationDefaults{MaxCompletionTokens: &maximum}}},
	} {
		put(t, storage, "ModelInvocationProfile", "team-a", profile.Name, replacement)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, completionRequest(capability))
		if response.Code != http.StatusForbidden {
			t.Fatalf("changed profile retained capability: %d %s", response.Code, response.Body.String())
		}
	}
	delete(storage.data, "ModelInvocationProfile/team-a/"+profile.Name)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(capability))
	if response.Code != http.StatusForbidden || calls != 1 {
		t.Fatalf("deleted profile retained capability: status=%d calls=%d", response.Code, calls)
	}
}
