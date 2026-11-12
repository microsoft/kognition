// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunWatch_RendersStreamAsTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/watch/namespaces/team-a/agents" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte("\n{\"type\":\"ADDED\",\"object\":{\"kind\":\"Agent\",\"metadata\":{\"name\":\"planner\"}}}\nnot-json\n"))
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()

	output := captureStdout(t, func() error { return runWatch(nil, []string{"agent"}) })
	if !strings.Contains(output, "ADDED\tAgent/planner") || !strings.Contains(output, "not-json") {
		t.Fatalf("output = %q", output)
	}
}

func TestRunWatch_RendersJSONEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{\"type\":\"DELETED\",\"object\":{\"metadata\":{\"name\":\"planner\"}}}\n"))
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	outputFormat = "json"

	output := captureStdout(t, func() error { return runWatch(nil, []string{"agents"}) })
	if !strings.Contains(output, `"type": "DELETED"`) || !strings.Contains(output, `"name": "planner"`) {
		t.Fatalf("output = %s", output)
	}
}

func TestRunWatch_PropagatesHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "watch unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()

	err := runWatch(nil, []string{"agent"})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("error = %v, want HTTP 503", err)
	}
}

func TestRunWatch_PropagatesScannerFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 70*1024) + "\n"))
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()

	err := runWatch(nil, []string{"agent"})
	if err == nil || !strings.Contains(err.Error(), "watch stream error") {
		t.Fatalf("error = %v, want scanner failure", err)
	}
}
