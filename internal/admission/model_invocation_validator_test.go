// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestModelInvocationAdmissionRejectsInvalidAndMutableProfiles(t *testing.T) {
	hook := NewModelInvocationValidator()
	for _, spec := range []string{
		`{"operation":"chat"}`,
		`{"operation":"image"}`,
		`{"operation":"chat","defaults":{"maxCompletionTokens":100,"temperature":3}}`,
		`{"operation":"chat","defaults":{"maxCompletionTokens":100},"limits":{"maxCompletionTokens":10}}`,
		`{"operation":"embedding","defaults":{"maxCompletionTokens":100}}`,
		`{"operation":"embedding","parameterHandling":{"unknown":"Drop"}}`,
		`{"operation":"embedding","arbitraryProviderConfig":{}}`,
		`{"operation":"chat","defaults":{"maxCompletionTokens":100,"reasoning":"disabled"},"requirements":{"reasoning":"required"}}`,
	} {
		response := hook.Validate(context.Background(), &Request{Kind: "ModelInvocationProfile", Operation: OperationCreate, Object: []byte(`{"spec":` + spec + `}`)})
		if response.Allowed || response.Reason == "" {
			t.Fatalf("invalid profile accepted or failed without reason: %s", spec)
		}
	}
	maximum := int64(100)
	profile := api.ModelInvocationProfile{Spec: api.ModelInvocationProfileSpec{Operation: "chat", Defaults: api.InvocationDefaults{MaxCompletionTokens: &maximum}}}
	previous, _ := json.Marshal(profile)
	request := &Request{Kind: "ModelInvocationProfile", Operation: OperationCreate, Object: previous}
	if response := hook.Validate(context.Background(), request); !response.Allowed {
		t.Fatal(response.Reason)
	}
	request.Operation = OperationUpdate
	request.OldObject = previous
	if response := hook.Validate(context.Background(), request); !response.Allowed {
		t.Fatal("metadata-only update rejected:", response.Reason)
	}
	maximum = 101
	request.Object, _ = json.Marshal(profile)
	if response := hook.Validate(context.Background(), request); response.Allowed || !strings.Contains(response.Reason, "immutable") {
		t.Fatalf("mutable profile accepted: %+v", response)
	}
}

func TestModelInvocationAdmissionRequiresUnambiguousBinding(t *testing.T) {
	hook := NewModelInvocationValidator()
	for _, spec := range []string{
		`{"invocationProfileRef":{"name":"profile"}}`,
		`{"invocationContract":"openai-chat-v1"}`,
		`{"invocationProfileRef":{"name":"profile"},"invocationContract":"openai-chat-v1","parameters":{"temperature":"0.7"}}`,
	} {
		response := hook.Validate(context.Background(), &Request{Kind: "Model", Operation: OperationCreate, Object: []byte(`{"spec":` + spec + `}`)})
		if response.Allowed {
			t.Fatalf("ambiguous model accepted: %s", spec)
		}
	}
	for _, spec := range []string{`{}`, `{"invocationProfileRef":{"name":"profile","specDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"invocationContract":"openai-chat-v1","parameters":{"apiVersion":"2024-02-01"}}`} {
		response := hook.Validate(context.Background(), &Request{Kind: "Model", Operation: OperationCreate, Object: []byte(`{"spec":` + spec + `}`)})
		if !response.Allowed {
			t.Fatalf("valid model rejected: %s: %s", spec, response.Reason)
		}
	}
}
