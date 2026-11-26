// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package bedrock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kognition-ai/kognition/internal/cpi"
)

func TestCompleteReturnsTypedProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	_, err := New(server.Client()).Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "anthropic.claude-3-haiku",
		Endpoint: server.URL,
		Messages: []cpi.Message{{Role: "user", Content: "hi"}},
	})
	providerErr, ok := cpi.ProviderFailure(err)
	if !ok || providerErr.Provider != "aws-bedrock" || providerErr.Status != http.StatusTooManyRequests || providerErr.Reason != cpi.ReasonRateLimited {
		t.Fatalf("Complete() error = %#v, want aws-bedrock 429 RateLimited", err)
	}
}
