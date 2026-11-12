// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestRunGet_StandaloneListRendersTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"items":[{"kind":"Agent","metadata":{"namespace":"team-a","name":"planner","creationTimestamp":"2026-03-12T10:00:00Z"}}]}`))
	}))
	defer server.Close()

	output := captureGetOutput(t, server, "", []string{"agent"})
	for _, want := range []string{"NAMESPACE", "team-a", "planner", "Agent", "2026-03-12"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}
}

func TestRunGet_StandaloneNamedResourceUsesStructuredOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"kind":"Agent","metadata":{"name":"planner"}}`))
	}))
	defer server.Close()

	output := captureGetOutput(t, server, "json", []string{"agent", "planner"})
	if !strings.Contains(output, `"name": "planner"`) {
		t.Fatalf("output = %s", output)
	}
}

func TestRunGet_PropagatesHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "backend unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()

	err := runGet(nil, []string{"agents"})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("error = %v, want HTTP 503", err)
	}
}

func TestPrintTable_InvalidListFallsBackToStructuredOutput(t *testing.T) {
	oldFormat := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = oldFormat }()

	output := captureStdout(t, func() error {
		return printTable("agent", []byte(`not-json`))
	})
	if !strings.Contains(output, "not-json") {
		t.Fatalf("output = %s", output)
	}
}

func TestShortenAge(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", want: "<unknown>"},
		{name: "timestamp", in: "2026-03-12T10:00:00Z", want: "2026-03-12"},
		{name: "date", in: "2026-03-12", want: "2026-03-12"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shortenAge(test.in); got != test.want {
				t.Fatalf("shortenAge(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func captureGetOutput(t *testing.T, server *httptest.Server, format string, args []string) string {
	t.Helper()
	restore := configureGetTest(server)
	defer restore()

	outputFormat = format
	return captureStdout(t, func() error { return runGet(nil, args) })
}

func configureGetTest(server *httptest.Server) func() {
	oldURL, oldClient := serverURL, httpClient
	oldFormat, oldNamespace, oldK8sMode := outputFormat, namespace, k8sMode
	serverURL, httpClient = server.URL, server.Client()
	outputFormat, namespace, k8sMode = "", "team-a", false
	return func() {
		serverURL, httpClient = oldURL, oldClient
		outputFormat, namespace, k8sMode = oldFormat, oldNamespace, oldK8sMode
	}
}

func captureStdout(t *testing.T, run func() error) string {
	t.Helper()
	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer

	runErr := run()
	_ = writer.Close()
	os.Stdout = oldStdout
	output, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	return string(output)
}
