// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kognition-ai/kognition/internal/cpi"
)

func TestCompleteAndEmbedReturnTypedProviderErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	creds := &cpi.Credentials{Type: "api-key", Data: map[string]string{"apiKey": "test-key"}}
	driver := New(server.Client())

	_, completeErr := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model: "gpt-4", Endpoint: server.URL, Creds: creds,
		Messages: []cpi.Message{{Role: "user", Content: "hi"}},
	})
	_, embedErr := driver.Embed(context.Background(), &cpi.EmbedRequest{
		Model: "text-embedding-3-small", Endpoint: server.URL, Creds: creds, Inputs: []string{"hi"},
	})
	for name, err := range map[string]error{"Complete": completeErr, "Embed": embedErr} {
		providerErr, ok := cpi.ProviderFailure(err)
		if !ok || providerErr.Provider != "openai-compatible" || providerErr.Status != http.StatusUnauthorized || providerErr.Reason != cpi.ReasonAuthenticationFailed {
			t.Errorf("%s() error = %#v, want openai-compatible 401 AuthenticationFailed", name, err)
		}
	}
}
