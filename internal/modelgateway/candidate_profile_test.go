// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

func TestGatewayCandidateUsesItsOwnInvocationContract(t *testing.T) {
	server, storage, _, issuer, session, _ := fixture(t)
	var logical api.Model
	if err := json.Unmarshal(storage.data["Model/team-a/mock-model"], &logical); err != nil {
		t.Fatal(err)
	}
	logical.Spec.ClassName = "candidate-class"
	logical.Spec.InvocationProfileRef = &api.ModelInvocationProfileReference{Name: "unselected-profile", SpecDigest: "sha256:" + strings.Repeat("a", 64)}
	logical.Spec.InvocationContract = "openai-chat-v1"
	put(t, storage, "Model", "team-a", logical.Name, logical)

	maximum := int64(128)
	profile := api.ModelInvocationProfile{
		ObjectMeta: api.ObjectMeta{Name: "candidate-profile", Namespace: "team-a", UID: "profile-uid"},
		Spec:       api.ModelInvocationProfileSpec{Operation: "chat", Defaults: api.InvocationDefaults{MaxCompletionTokens: &maximum, Reasoning: "disabled"}},
	}
	put(t, storage, "ModelInvocationProfile", "team-a", profile.Name, profile)
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body["max_completion_tokens"]) != "128" || body["max_tokens"] != nil ||
			string(body["reasoning_effort"]) != `"none"` || string(body["model"]) != `"opaque-candidate"` {
			t.Errorf("candidate inherited the logical Model's dialect: %+v", body)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)
	candidate := logical
	candidate.Name, candidate.UID, candidate.ResourceVersion = "candidate", "candidate-uid", 8
	candidate.Spec.Endpoint, candidate.Spec.ModelID = upstream.URL, "opaque-candidate"
	candidate.Spec.InvocationContract = "openai-tool-chat-v1"
	candidate.Spec.InvocationProfileRef = &api.ModelInvocationProfileReference{Name: profile.Name, SpecDigest: invocationProfileDigest(t, profile.Spec)}
	put(t, storage, "Model", "team-a", candidate.Name, candidate)
	digest, err := runtimeauth.ModelConfigurationDigest(&candidate)
	if err != nil {
		t.Fatal(err)
	}
	binding := api.ModelVersionBinding{
		Namespace: "team-a", ModelName: candidate.Name, ModelUID: candidate.UID, ModelResourceVersion: candidate.ResourceVersion, SpecDigest: digest,
		InvocationProfileUID: profile.UID, InvocationProfileDigest: candidate.Spec.InvocationProfileRef.SpecDigest,
	}
	put(t, storage, "ModelClass", "", "candidate-class", api.ModelClass{
		ObjectMeta: api.ObjectMeta{Name: "candidate-class"},
		Spec:       api.ModelClassSpec{Provisioner: "openai-compatible", VersionCandidates: map[string]api.ModelVersionBinding{"v2": binding}},
		Status:     api.ModelClassStatus{ActiveVersion: "v2", ActiveBinding: &binding},
	})
	invocation, err := modelinvocation.Load(context.Background(), storage, &candidate)
	if err != nil {
		t.Fatal(err)
	}
	session.CandidateBinding, session.InvocationProfileBinding = &binding, &invocation.Binding
	token, err := issuer.Issue(session)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(token))
	if response.Code != http.StatusOK || calls != 1 {
		t.Fatalf("candidate invocation failed: status=%d body=%s calls=%d", response.Code, response.Body.String(), calls)
	}
}
