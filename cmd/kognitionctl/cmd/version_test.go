// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunVersionReportsServerHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Fatalf("path = %q, want /healthz", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	previousURL, previousClient := serverURL, httpClient
	serverURL, httpClient = server.URL, server.Client()
	t.Cleanup(func() { serverURL, httpClient = previousURL, previousClient })

	output := captureStdout(t, func() error {
		runVersion(nil, nil)
		return nil
	})
	for _, want := range []string{
		"kognitionctl version " + version,
		"git commit: " + gitCommit,
		"build date: " + buildDate,
		"server:     " + server.URL + " (healthy)",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output = %q, missing %q", output, want)
		}
	}
}

func TestRunVersionReportsUnreachableServer(t *testing.T) {
	previousURL := serverURL
	serverURL = "://invalid"
	t.Cleanup(func() { serverURL = previousURL })

	output := captureStdout(t, func() error {
		runVersion(nil, nil)
		return nil
	})
	if !strings.Contains(output, "server:     unreachable (://invalid)") {
		t.Fatalf("output = %q, want unreachable server status", output)
	}
}
