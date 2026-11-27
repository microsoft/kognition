// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package bedrock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestProbe_ReportsRequestBuildFailure(t *testing.T) {
	result, err := New(nil).Probe(context.Background(), &v1alpha1.ModelSpec{ModelID: "anthropic.claude-3-haiku", Endpoint: "http://[::1"}, nil)
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	if result.Available || result.Reason != "RequestBuildFailed" {
		t.Fatalf("Probe() = %+v, want unavailable RequestBuildFailed", result)
	}
}

func TestProbe_ModelRejectionMatchingIsCaseAndApostropheInsensitive(t *testing.T) {
	for _, message := range []string{
		"The PROVIDED model identifier is INVALID.",
		"Invocation of model ID x with on-demand throughput isn’t supported.",
		"This action doesn’t support the model that you provided.",
	} {
		t.Run(message, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"message":"` + message + `"}`))
			}))
			defer server.Close()

			result, err := New(server.Client()).Probe(context.Background(), &v1alpha1.ModelSpec{ModelID: "m", Endpoint: server.URL}, nil)
			if err != nil {
				t.Fatalf("Probe() error: %v", err)
			}
			if result.Available || result.Reason != "ModelUnavailable" {
				t.Fatalf("Probe() = %+v, want unavailable ModelUnavailable", result)
			}
		})
	}
}

func TestProbe_TruncatesModelRejectionMessageOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("é", 2*maxProbeMessageSize)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"The provided model identifier is invalid. ` + long + `"}`))
	}))
	defer server.Close()

	result, err := New(server.Client()).Probe(context.Background(), &v1alpha1.ModelSpec{ModelID: "m", Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	detail := strings.TrimPrefix(result.Message, "HTTP 400 from Bedrock: ")
	if got := len([]rune(detail)); got != maxProbeMessageSize || strings.ContainsRune(detail, '\uFFFD') {
		t.Fatalf("detail has %d runes (replacement char: %t), want %d valid runes", got, strings.ContainsRune(detail, '\uFFFD'), maxProbeMessageSize)
	}
}

func TestProbe_ThrottledWithoutRetryAfterOmitsHint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	result, err := New(server.Client()).Probe(context.Background(), &v1alpha1.ModelSpec{ModelID: "m", Endpoint: server.URL}, nil)
	if err != nil {
		t.Fatalf("Probe() error: %v", err)
	}
	if result.Reason != "RateLimited" || strings.Contains(result.Message, "Retry-After") {
		t.Fatalf("Probe() = %+v, want RateLimited without a Retry-After hint", result)
	}
}

func TestProbe_DistinguishesModelNotReadyFromThrottling(t *testing.T) {
	tests := []struct {
		errorType  string
		wantReason string
		wantDetail string
	}{
		{"ModelNotReadyException", "ModelUnavailable", "model is not ready"},
		{"ModelNotReadyException:http://example.com", "ModelUnavailable", "model is not ready"},
		{"com.amazonaws.bedrock#ModelNotReadyException", "ModelUnavailable", "model is not ready"},
		{"ThrottlingException", "RateLimited", "throttled"},
	}
	for _, test := range tests {
		t.Run(test.errorType, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Amzn-Errortype", test.errorType)
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer server.Close()

			result, err := New(server.Client()).Probe(context.Background(), &v1alpha1.ModelSpec{
				ModelID: "m", Endpoint: server.URL,
			}, nil)
			if err != nil {
				t.Fatalf("Probe() error: %v", err)
			}
			if result.Available || result.Reason != test.wantReason || !strings.Contains(result.Message, test.wantDetail) {
				t.Fatalf("Probe() = %+v, want unavailable %s with %q", result, test.wantReason, test.wantDetail)
			}
			if test.wantReason == "RateLimited" && !strings.Contains(result.Message, "Retry-After: 7") {
				t.Fatalf("Message = %q, want Retry-After hint", result.Message)
			}
		})
	}
}
