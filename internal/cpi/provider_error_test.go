// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClassifyStatus(t *testing.T) {
	tests := map[int]string{
		http.StatusBadRequest:          ReasonInvalidRequest,
		http.StatusUnprocessableEntity: ReasonInvalidRequest,
		http.StatusUnauthorized:        ReasonAuthenticationFailed,
		http.StatusForbidden:           ReasonAuthenticationFailed,
		http.StatusPaymentRequired:     ReasonQuotaOrBilling,
		http.StatusTooManyRequests:     ReasonRateLimited,
		http.StatusNotFound:            ReasonEndpointError,
		http.StatusRequestTimeout:      ReasonEndpointError,
		http.StatusInternalServerError: ReasonEndpointError,
		http.StatusServiceUnavailable:  ReasonEndpointError,
		529:                            ReasonEndpointError,
	}
	for status, want := range tests {
		if got := ClassifyStatus(status); got != want {
			t.Errorf("ClassifyStatus(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestNewProviderErrorPreservesMessageFormatAndFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("slow down"))
	}))
	defer server.Close()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	chat := NewProviderError("anthropic", "", resp)
	if got, want := chat.Error(), "anthropic: HTTP 429: slow down"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if chat.Status != http.StatusTooManyRequests || chat.Reason != ReasonRateLimited || chat.RetryAfter != "12" {
		t.Errorf("fields = %+v, want status 429, RateLimited, Retry-After 12", chat)
	}

	embed := &ProviderError{Provider: "ollama", Operation: "embed", Status: 429, Body: "busy"}
	if got, want := embed.Error(), "ollama: embed HTTP 429: busy"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestNewProviderErrorBoundsBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 4*maxProviderErrorBodyBytes)))
	}))
	defer server.Close()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := NewProviderError("openai-compatible", "", resp); len(got.Body) != maxProviderErrorBodyBytes {
		t.Errorf("body length = %d, want %d", len(got.Body), maxProviderErrorBodyBytes)
	}
}

func TestProviderFailureUnwrapsWrappedErrors(t *testing.T) {
	original := &ProviderError{Provider: "foundry", Status: http.StatusPaymentRequired, Reason: ReasonQuotaOrBilling}
	got, ok := ProviderFailure(fmt.Errorf("dispatch: %w", original))
	if !ok || got != original {
		t.Fatalf("ProviderFailure() = %v, %t; want the wrapped ProviderError", got, ok)
	}
	if _, ok := ProviderFailure(fmt.Errorf("plain")); ok {
		t.Error("ProviderFailure() reported a non-provider error")
	}
}
