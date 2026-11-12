// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunDelete_StandaloneDeletesNamedResource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()

	output := captureStdout(t, func() error {
		return runDelete(nil, []string{"agent", "planner"})
	})
	if !strings.Contains(output, `agent "planner" deleted`) {
		t.Fatalf("output = %q", output)
	}
}

func TestRunDelete_PropagatesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "deletion blocked", http.StatusConflict)
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()

	err := runDelete(nil, []string{"agent", "planner"})
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "deletion blocked") {
		t.Fatalf("error = %v, want HTTP 409 response", err)
	}
}

func TestRunDescribe_DefaultsToYAMLAndRestoresFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"kind":"Agent","metadata":{"name":"planner"}}`))
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()

	output := captureStdout(t, func() error {
		return runDescribe(nil, []string{"agent", "planner"})
	})
	if !strings.Contains(output, "kind: Agent") || !strings.Contains(output, "name: planner") {
		t.Fatalf("output = %s", output)
	}
	if outputFormat != "" {
		t.Fatalf("outputFormat = %q, want restored default", outputFormat)
	}
}

func TestRunEvents_RendersEnvelopeAsTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner/events" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"items":[{"type":"Normal","reason":"Ready","message":"agent is ready","timestamp":"2026-03-12T10:00:00Z"}]}`))
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()

	output := captureStdout(t, func() error {
		return runEvents(nil, []string{"agent", "planner"})
	})
	for _, want := range []string{"TYPE", "REASON", "MESSAGE", "TIMESTAMP", "Normal", "Ready", "agent is ready", "2026-03-12T10:00:00Z"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}
}

func TestRunEvents_RendersRawArrayAndCreatedAt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"type":"Warning","reason":"Retrying","message":"retry scheduled","createdAt":"2026-03-12T11:00:00Z"}]`))
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()

	output := captureStdout(t, func() error {
		return runEvents(nil, []string{"agents", "planner"})
	})
	for _, want := range []string{"Warning", "Retrying", "retry scheduled", "2026-03-12T11:00:00Z"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}
}

func TestRunEvents_StructuredOutputPropagatesHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "event store unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	outputFormat = "json"

	err := runEvents(nil, []string{"agent", "planner"})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("error = %v, want HTTP 503", err)
	}
}
