// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kognition-ai/kognition/internal/cpi"
)

func TestCompleteReturnsTypedProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	_, err := New(server.Client()).Complete(context.Background(), &cpi.CompletionRequest{
		Model:    "claude-3-sonnet",
		Endpoint: server.URL,
		Messages: []cpi.Message{{Role: "user", Content: "hi"}},
	})
	providerErr, ok := cpi.ProviderFailure(err)
	if !ok || providerErr.Provider != "anthropic" || providerErr.Status != http.StatusTooManyRequests ||
		providerErr.Reason != cpi.ReasonRateLimited || providerErr.RetryAfter != "9" {
		t.Fatalf("Complete() error = %#v, want anthropic 429 RateLimited with Retry-After 9", err)
	}
}
