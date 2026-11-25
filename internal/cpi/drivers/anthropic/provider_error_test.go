// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/cpi"
)

func TestCompleteClassifiesBillingFailuresFromTheResponseBody(t *testing.T) {
	envelope := func(message string) string {
		encoded, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "invalid_request_error", "message": message}})
		return string(encoded)
	}
	padded := envelope(strings.Repeat("x", 2*1024) + " Your credit balance is too low.")
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"exhausted credit", http.StatusBadRequest, envelope("Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing."), cpi.ReasonQuotaOrBilling},
		{"workspace spend limit", http.StatusBadRequest, envelope("You have reached your workspace's spend limit for this month."), cpi.ReasonQuotaOrBilling},
		{"usage limit", http.StatusBadRequest, envelope("You have reached your specified API usage limits."), cpi.ReasonQuotaOrBilling},
		{"marker in a different case", http.StatusBadRequest, envelope("BILLING problem"), cpi.ReasonQuotaOrBilling},
		{"malformed request", http.StatusBadRequest, envelope("messages: text content blocks must be non-empty"), cpi.ReasonInvalidRequest},
		{"unknown model", http.StatusBadRequest, envelope("model: claude-x is not a valid model"), cpi.ReasonInvalidRequest},
		{"not JSON", http.StatusBadRequest, "credit balance is too low", cpi.ReasonInvalidRequest},
		{"empty body", http.StatusBadRequest, "", cpi.ReasonInvalidRequest},
		{"message beyond the kept body", http.StatusBadRequest, padded, cpi.ReasonInvalidRequest},
		{"payment required", http.StatusPaymentRequired, envelope("billing_error"), cpi.ReasonQuotaOrBilling},
		{"markers only apply to 400", http.StatusTooManyRequests, envelope("Your credit balance is too low"), cpi.ReasonRateLimited},
		{"server error with a marker", http.StatusInternalServerError, envelope("billing system unavailable"), cpi.ReasonEndpointError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			_, err := New(server.Client()).Complete(context.Background(), &cpi.CompletionRequest{
				Model:    "claude-3-sonnet",
				Endpoint: server.URL,
				Messages: []cpi.Message{{Role: "user", Content: "hi"}},
			})
			providerErr, ok := cpi.ProviderFailure(err)
			if !ok || providerErr.Reason != tc.want || providerErr.Status != tc.status {
				t.Fatalf("Complete() error = %#v, want HTTP %d with reason %s", err, tc.status, tc.want)
			}
			if want := fmt.Sprintf("anthropic: HTTP %d: ", tc.status); !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("message = %q, want the historical %q prefix", err.Error(), want)
			}
		})
	}
}

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
