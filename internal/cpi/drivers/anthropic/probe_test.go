// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestProbe_DefaultsToAnthropicEndpoint(t *testing.T) {
	var requested string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requested = r.URL.String()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
	})}

	result, err := New(client).Probe(context.Background(), &v1alpha1.ModelSpec{ModelID: "claude-3-sonnet"}, nil)
	if err != nil || !result.Available {
		t.Fatalf("Probe() = %+v, %v; want healthy", result, err)
	}
	if want := defaultBaseURL + countTokensPath; requested != want {
		t.Fatalf("requested %q, want %q", requested, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProbe_DoesNotFallBackOnDefinitiveCountTokensFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadRequest} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				w.WriteHeader(status)
			}))
			defer server.Close()

			if _, err := New(server.Client()).Probe(context.Background(), &v1alpha1.ModelSpec{ModelID: "claude-3-sonnet", Endpoint: server.URL}, nil); err != nil {
				t.Fatalf("Probe() error: %v", err)
			}
			if len(paths) != 1 || paths[0] != countTokensPath {
				t.Fatalf("requests = %v, want only %s (no billable fallback)", paths, countTokensPath)
			}
		})
	}
}

func TestProbe_ReportsRequestBuildFailure(t *testing.T) {
	result, err := New(nil).Probe(context.Background(), &v1alpha1.ModelSpec{ModelID: "claude-3-sonnet", Endpoint: "http://[::1"}, nil)
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	if result.Available || result.Reason != "RequestBuildFailed" {
		t.Fatalf("Probe() = %+v, want unavailable RequestBuildFailed", result)
	}
}

func TestProbe_UsesInvocationProfileAPIVersion(t *testing.T) {
	var versions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		versions = append(versions, r.Header.Get("anthropic-version"))
		if r.URL.Path == countTokensPath {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	_, err := New(server.Client()).Probe(context.Background(), &v1alpha1.ModelSpec{
		ModelID:              "claude-3-sonnet",
		Endpoint:             server.URL,
		Parameters:           map[string]string{"apiVersion": "2099-01-01"},
		InvocationProfileRef: &v1alpha1.ModelInvocationProfileReference{Name: "profile", SpecDigest: "sha256:test"},
	}, nil)
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	if len(versions) != 2 || versions[0] != "2099-01-01" || versions[1] != "2099-01-01" {
		t.Fatalf("anthropic-version headers = %v, want the profile API version on both the count_tokens and fallback requests", versions)
	}
}

func TestProbe_TruncatesBillingMessageOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("é", 2*maxProbeMessageSize)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"credit balance ` + long + `"}}`))
	}))
	defer server.Close()

	result, err := New(server.Client()).Probe(context.Background(), &v1alpha1.ModelSpec{ModelID: "claude-3-sonnet", Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	if result.Available || result.Reason != "QuotaOrBillingError" {
		t.Fatalf("Probe() = %+v, want unavailable QuotaOrBillingError", result)
	}
	detail := strings.TrimPrefix(result.Message, "HTTP 400 from Anthropic: ")
	if got := len([]rune(detail)); got != maxProbeMessageSize || strings.ContainsRune(detail, '\uFFFD') {
		t.Fatalf("detail has %d runes (replacement char: %t), want %d valid runes", got, strings.ContainsRune(detail, '\uFFFD'), maxProbeMessageSize)
	}
}

func TestProbe_RateLimitedWithoutRetryAfterOmitsHint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	result, err := New(server.Client()).Probe(context.Background(), &v1alpha1.ModelSpec{ModelID: "claude-3-sonnet", Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	if result.Reason != "RateLimited" || strings.Contains(result.Message, "Retry-After") {
		t.Fatalf("Probe() = %+v, want RateLimited without a Retry-After hint", result)
	}
}
