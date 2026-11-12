// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"text/tabwriter"
)

func TestRunStatus_SingleAgent(t *testing.T) {
	agent := agentStatusInfo{}
	agent.Metadata.Name = "my-agent"
	agent.Spec.Model.Name = "gpt-4"
	replicas := 3
	agent.Spec.Replicas = &replicas
	agent.Status.Phase = "Running"
	agent.Status.Replicas = 2
	agent.Status.LastActive = "2024-01-01T00:00:00Z"

	data, _ := json.Marshal(agent)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer ts.Close()

	oldURL := serverURL
	oldClient := httpClient
	oldFormat := outputFormat
	oldNs := namespace
	defer func() {
		serverURL = oldURL
		httpClient = oldClient
		outputFormat = oldFormat
		namespace = oldNs
	}()

	serverURL = ts.URL
	httpClient = ts.Client()
	outputFormat = ""
	namespace = "default"

	// Capture stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runStatus(nil, []string{"my-agent"})

	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := string(out)
	if !strings.Contains(output, "my-agent") {
		t.Errorf("expected output to contain 'my-agent', got:\n%s", output)
	}
	if !strings.Contains(output, "Running") {
		t.Errorf("expected output to contain 'Running', got:\n%s", output)
	}
	if !strings.Contains(output, "gpt-4") {
		t.Errorf("expected output to contain 'gpt-4', got:\n%s", output)
	}
	if !strings.Contains(output, "2/3") {
		t.Errorf("expected output to contain '2/3' replicas, got:\n%s", output)
	}
}

func TestRunStatus_AllAgents(t *testing.T) {
	a1 := agentStatusInfo{}
	a1.Metadata.Name = "agent-1"
	a1.Spec.Model.Name = "gpt-4"
	a1.Status.Phase = "Running"
	a1.Status.Replicas = 1

	a2 := agentStatusInfo{}
	a2.Metadata.Name = "agent-2"
	a2.Spec.Model.Name = "claude-3"
	a2.Status.Phase = "Pending"
	a2.Status.Replicas = 0

	items1, _ := json.Marshal(a1)
	items2, _ := json.Marshal(a2)

	listResp := fmt.Sprintf(`{"items":[%s,%s]}`, string(items1), string(items2))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(listResp))
	}))
	defer ts.Close()

	oldURL := serverURL
	oldClient := httpClient
	oldFormat := outputFormat
	oldNs := namespace
	defer func() {
		serverURL = oldURL
		httpClient = oldClient
		outputFormat = oldFormat
		namespace = oldNs
	}()

	serverURL = ts.URL
	httpClient = ts.Client()
	outputFormat = ""
	namespace = "default"

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runStatus(nil, []string{})

	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	output := string(out)
	if !strings.Contains(output, "agent-1") {
		t.Errorf("expected output to contain 'agent-1', got:\n%s", output)
	}
	if !strings.Contains(output, "agent-2") {
		t.Errorf("expected output to contain 'agent-2', got:\n%s", output)
	}
}

func TestRunStatus_JSONOutput(t *testing.T) {
	agent := agentStatusInfo{}
	agent.Metadata.Name = "my-agent"
	agent.Spec.Model.Name = "gpt-4"
	agent.Status.Phase = "Running"

	data, _ := json.Marshal(agent)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	defer ts.Close()

	oldURL := serverURL
	oldClient := httpClient
	oldFormat := outputFormat
	oldNs := namespace
	defer func() {
		serverURL = oldURL
		httpClient = oldClient
		outputFormat = oldFormat
		namespace = oldNs
	}()

	serverURL = ts.URL
	httpClient = ts.Client()
	outputFormat = "json"
	namespace = "default"

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := runStatus(nil, []string{"my-agent"})

	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if err != nil {
		t.Fatalf("runStatus() error = %v", err)
	}

	// Should be valid JSON (pretty-printed)
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("expected JSON output, got:\n%s", string(out))
	}
}

func TestRunStatus_ServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer ts.Close()

	oldURL := serverURL
	oldClient := httpClient
	oldFormat := outputFormat
	oldNs := namespace
	defer func() {
		serverURL = oldURL
		httpClient = oldClient
		outputFormat = oldFormat
		namespace = oldNs
	}()

	serverURL = ts.URL
	httpClient = ts.Client()
	outputFormat = ""
	namespace = "default"

	err := runStatus(nil, []string{"missing-agent"})
	if err == nil {
		t.Fatal("expected error from server error response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error to contain '500', got: %v", err)
	}
}

func TestPrintAgentRow_Defaults(t *testing.T) {
	a := &agentStatusInfo{}
	// All fields empty — should use defaults

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	printAgentRow(tw, a)
	tw.Flush()

	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	output := string(out)
	if !strings.Contains(output, "Unknown") {
		t.Errorf("expected 'Unknown' for empty phase, got:\n%s", output)
	}
	if !strings.Contains(output, "<none>") {
		t.Errorf("expected '<none>' for empty model, got:\n%s", output)
	}
	if !strings.Contains(output, "<never>") {
		t.Errorf("expected '<never>' for empty lastActive, got:\n%s", output)
	}
}

func TestPrintAgentRow_WithDesiredReplicas(t *testing.T) {
	a := &agentStatusInfo{}
	a.Metadata.Name = "test"
	a.Status.Phase = "Running"
	a.Status.Replicas = 2
	desired := 3
	a.Spec.Replicas = &desired
	a.Spec.Model.Name = "gpt-4"
	a.Status.LastActive = "2024-06-01T12:00:00Z"

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	printAgentRow(tw, a)
	tw.Flush()

	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	output := string(out)
	if !strings.Contains(output, "2/3") {
		t.Errorf("expected '2/3' for replicas, got:\n%s", output)
	}
}

func TestPrintAgentRow_WithoutDesiredReplicas(t *testing.T) {
	a := &agentStatusInfo{}
	a.Status.Replicas = 5

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	printAgentRow(tw, a)
	tw.Flush()

	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	output := string(out)
	// Without desired, just show actual
	if !strings.Contains(output, "5") {
		t.Errorf("expected '5' for replicas without desired, got:\n%s", output)
	}
	if strings.Contains(output, "5/") {
		t.Errorf("should not contain '5/' without desired replicas, got:\n%s", output)
	}
}

func TestStatusCmd_RequestsCorrectURL(t *testing.T) {
	var requestedPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"metadata":{"name":"x"},"status":{"phase":"Running"}}`))
	}))
	defer ts.Close()

	oldURL := serverURL
	oldClient := httpClient
	oldFormat := outputFormat
	oldNs := namespace
	defer func() {
		serverURL = oldURL
		httpClient = oldClient
		outputFormat = oldFormat
		namespace = oldNs
	}()

	serverURL = ts.URL
	httpClient = ts.Client()
	outputFormat = ""
	namespace = "my-ns"

	// Suppress stdout
	old := os.Stdout
	_, w, _ := os.Pipe()
	os.Stdout = w

	_ = runStatus(nil, []string{"test-agent"})

	w.Close()
	os.Stdout = old

	expected := "/apis/kognition.dev/v1alpha1/namespaces/my-ns/agents/test-agent"
	if requestedPath != expected {
		t.Errorf("requested path = %q, want %q", requestedPath, expected)
	}
}
