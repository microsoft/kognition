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

func TestNewRefinedProviderError(t *testing.T) {
	// refined answers with status and body, and builds the error from that response.
	refined := func(t *testing.T, status int, body, operation string, refine ReasonRefiner) *ProviderError {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		defer server.Close()
		resp, err := http.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		return NewRefinedProviderError("anthropic", operation, resp, refine)
	}

	t.Run("a refined reason replaces the status reason and keeps the message", func(t *testing.T) {
		var gotStatus int
		var gotBody string
		got := refined(t, http.StatusBadRequest, "no credit", "", func(status int, body []byte) string {
			gotStatus, gotBody = status, string(body)
			return ReasonQuotaOrBilling
		})
		if gotStatus != http.StatusBadRequest || gotBody != "no credit" {
			t.Errorf("refiner saw %d %q, want 400 \"no credit\"", gotStatus, gotBody)
		}
		if got.Reason != ReasonQuotaOrBilling || got.Status != http.StatusBadRequest || got.Error() != "anthropic: HTTP 400: no credit" {
			t.Errorf("error = %+v (%q), want the refined reason with an unchanged status and message", got, got.Error())
		}
	})
	t.Run("an empty refinement keeps the status reason", func(t *testing.T) {
		got := refined(t, http.StatusBadRequest, "bad", "", func(int, []byte) string { return "" })
		if got.Reason != ReasonInvalidRequest {
			t.Errorf("reason = %q, want %q", got.Reason, ReasonInvalidRequest)
		}
	})
	t.Run("a nil refiner is NewProviderError", func(t *testing.T) {
		got := refined(t, http.StatusTooManyRequests, "slow", "embed", nil)
		if got.Reason != ReasonRateLimited || got.Operation != "embed" {
			t.Errorf("error = %+v, want a RateLimited embed error", got)
		}
	})
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
