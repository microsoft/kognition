// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kognition-ai/kognition/internal/cpi"
)

func TestCompleteAndEmbedReturnTypedProviderErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	driver := New(server.Client())

	_, completeErr := driver.Complete(context.Background(), &cpi.CompletionRequest{Model: "llama3", Endpoint: server.URL})
	_, embedErr := driver.Embed(context.Background(), &cpi.EmbedRequest{Model: "nomic-embed-text", Endpoint: server.URL, Inputs: []string{"hi"}})
	for name, err := range map[string]error{"Complete": completeErr, "Embed": embedErr} {
		providerErr, ok := cpi.ProviderFailure(err)
		if !ok || providerErr.Provider != "ollama" || providerErr.Status != http.StatusServiceUnavailable || providerErr.Reason != cpi.ReasonEndpointError {
			t.Errorf("%s() error = %#v, want ollama 503 EndpointError", name, err)
		}
	}
}
