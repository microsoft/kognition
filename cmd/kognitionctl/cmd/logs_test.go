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

func TestRunLogs_FiltersInstancesAndPrintsEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/kognition.dev/v1alpha1/namespaces/team-a/agentinstances":
			_, _ = w.Write([]byte(`{"items":[
  {"metadata":{"name":"planner-a"},"spec":{"agentRef":{"name":"PLANNER"},"revision":3},"status":{"phase":"Running","sessionID":"abcdefghijk","output":"first result","startTime":"2026-01-02T03:04:05Z"}},
  "malformed",
  {"metadata":{"name":"other-a"},"spec":{"agentRef":{"name":"other"},"revision":1},"status":{"phase":"Ready"}}
]}`))
		case "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner/events":
			_, _ = w.Write([]byte(`[{"type":"Normal","reason":"Dispatched","message":"instance started","timestamp":"2026-01-02T03:04:06Z"}]`))
		default:
			t.Errorf("unexpected backend path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	restore := configureGetTest(server)
	defer restore()
	previousInstance := logsInstance
	logsInstance = ""
	defer func() { logsInstance = previousInstance }()

	output := captureStdout(t, func() error { return runLogs(nil, []string{"planner"}) })
	for _, want := range []string{"planner-a", "Running", "abcdefgh", "first result", "--- Events ---", "Dispatched: instance started (Normal)"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "other-a") {
		t.Fatalf("output included unrelated instance:\n%s", output)
	}
}

func TestRunLogs_SpecificInstancePrintsRuntimeDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agentinstances/planner-a" {
			t.Errorf("backend path = %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{
  "metadata":{"name":"planner-a"},
  "spec":{"agentRef":{"name":"planner"},"revision":4},
  "status":{"phase":"Ready","sessionID":"session-4","startTime":"2026-01-02T03:04:05Z","output":"completed output","tokenUsage":{"promptTokens":10,"completionTokens":6,"totalTokens":16},"toolCalls":2}
}`))
	}))
	defer server.Close()
	restore := configureGetTest(server)
	defer restore()
	previousInstance := logsInstance
	logsInstance = "planner-a"
	defer func() { logsInstance = previousInstance }()

	output := captureStdout(t, func() error { return runLogs(nil, []string{"planner"}) })
	for _, want := range []string{
		"Instance:  planner-a",
		"Agent:     planner",
		"Revision:  4",
		"Phase:     Ready",
		"Tokens:    10 prompt / 6 completion / 16 total",
		"ToolCalls: 2",
		"--- Output ---",
		"completed output",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestRunLogs_ListErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "backend failure", status: http.StatusServiceUnavailable, body: "unavailable", want: "server error (503)"},
		{name: "malformed list", status: http.StatusOK, body: "{", want: "parse instance list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			restore := configureGetTest(server)
			defer restore()
			previousInstance := logsInstance
			logsInstance = ""
			defer func() { logsInstance = previousInstance }()

			err := runLogs(nil, []string{"planner"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runLogs() error = %v, want message containing %q", err, tc.want)
			}
		})
	}
}

func TestRunLogs_NoMatchingInstancesWritesDiagnostic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"other-a"},"spec":{"agentRef":{"name":"other"}}}]}`))
	}))
	defer server.Close()
	restore := configureGetTest(server)
	defer restore()
	previousInstance := logsInstance
	logsInstance = ""
	defer func() { logsInstance = previousInstance }()

	diagnostic := captureStderr(t, func() error { return runLogs(nil, []string{"planner"}) })
	if !strings.Contains(diagnostic, `No instances found for agent "planner" in namespace "team-a"`) {
		t.Fatalf("stderr = %q, want no-instance diagnostic", diagnostic)
	}
}

func TestRunLogs_JSONOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"planner-json"},"spec":{"agentRef":{"name":"planner"},"revision":2},"status":{"phase":"Ready"}}]}`))
	}))
	defer server.Close()
	restore := configureGetTest(server)
	defer restore()
	previousInstance := logsInstance
	logsInstance = ""
	outputFormat = "json"
	defer func() { logsInstance = previousInstance }()

	output := captureStdout(t, func() error { return runLogs(nil, []string{"planner"}) })
	if !strings.Contains(output, `"name":"planner-json"`) || !strings.Contains(output, `"phase":"Ready"`) {
		t.Fatalf("JSON output = %s", output)
	}
}

func TestShowInstanceLogs_EmptyAndRawOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/kognition.dev/v1alpha1/namespaces/team-a/agentinstances/empty":
			_, _ = w.Write([]byte(`{"metadata":{"name":"empty"},"spec":{"agentRef":{"name":"planner"},"revision":1},"status":{"phase":"Pending"}}`))
		case "/apis/kognition.dev/v1alpha1/namespaces/team-a/agentinstances/raw":
			_, _ = w.Write([]byte("plain runtime output"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	restore := configureGetTest(server)
	defer restore()

	emptyOutput := captureStdout(t, func() error { return showInstanceLogs("empty") })
	if !strings.Contains(emptyOutput, "(no output yet)") {
		t.Fatalf("empty instance output = %s", emptyOutput)
	}
	rawOutput := captureStdout(t, func() error { return showInstanceLogs("raw") })
	if !strings.Contains(rawOutput, "plain runtime output") {
		t.Fatalf("raw instance output = %s", rawOutput)
	}
}

func captureStderr(t *testing.T, run func() error) string {
	t.Helper()
	previous := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	runErr := run()
	_ = writer.Close()
	os.Stderr = previous
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
