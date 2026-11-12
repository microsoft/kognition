// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TC-0244: `kognitionctl workflow logs <run>` prints a per-step timeline derived
// from the WorkflowRun status and its owned AgentJobs.
func TestRunWorkflowLogs_TimelineTable(t *testing.T) {
	started := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	finished := started.Add(750 * time.Millisecond)

	runDoc := map[string]any{
		"metadata": map[string]any{"name": "run-1", "namespace": "default"},
		"status": map[string]any{
			"phase": "Ready",
			"stepStatuses": []map[string]any{
				{"name": "step-a", "phase": "Ready", "attempts": 1, "startedAt": started.Format(time.RFC3339Nano)},
				{"name": "step-b", "phase": "Ready", "attempts": 1, "startedAt": started.Format(time.RFC3339Nano)},
			},
		},
	}
	jobA := map[string]any{
		"metadata": map[string]any{
			"name":            "job-a",
			"namespace":       "default",
			"annotations":     map[string]string{"kognition.dev/step-name": "step-a"},
			"ownerReferences": []map[string]any{{"kind": "WorkflowRun", "name": "run-1"}},
		},
		"status": map[string]any{"phase": "Ready", "completionTime": finished.Format(time.RFC3339Nano)},
	}
	jobB := map[string]any{
		"metadata": map[string]any{
			"name":            "job-b",
			"namespace":       "default",
			"annotations":     map[string]string{"kognition.dev/step-name": "step-b"},
			"ownerReferences": []map[string]any{{"kind": "WorkflowRun", "name": "run-1"}},
		},
		"status": map[string]any{"phase": "Ready", "completionTime": finished.Format(time.RFC3339Nano)},
	}

	runBody, _ := json.Marshal(runDoc)
	listBody, _ := json.Marshal(map[string]any{"items": []any{jobA, jobB}})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if strings.HasSuffix(r.URL.Path, "/workflowruns/run-1") {
			w.Write(runBody)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/agentjobs") {
			w.Write(listBody)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	oldURL, oldClient, oldFmt, oldNs, oldK8s := serverURL, httpClient, outputFormat, namespace, k8sMode
	defer func() {
		serverURL, httpClient, outputFormat, namespace, k8sMode = oldURL, oldClient, oldFmt, oldNs, oldK8s
	}()
	serverURL = ts.URL
	httpClient = ts.Client()
	outputFormat = ""
	namespace = "default"
	k8sMode = false

	old := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw

	err := runWorkflowLogs(workflowLogsCmd, []string{"run-1"})

	pw.Close()
	os.Stdout = old
	out, _ := io.ReadAll(pr)

	if err != nil {
		t.Fatalf("runWorkflowLogs error = %v", err)
	}

	output := string(out)
	for _, want := range []string{"STEP", "PHASE", "DURATION", "step-a", "step-b", "Ready", "750ms"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, output)
		}
	}
}

// TC-0244b: JSON output mode emits a parseable document with run and jobsByStep keys.
func TestRunWorkflowLogs_JSONOutput(t *testing.T) {
	runDoc := map[string]any{
		"metadata": map[string]any{"name": "run-j", "namespace": "default"},
		"status": map[string]any{
			"phase":        "Ready",
			"stepStatuses": []map[string]any{{"name": "s1", "phase": "Ready", "attempts": 1}},
		},
	}
	job := map[string]any{
		"metadata": map[string]any{
			"name":            "j1",
			"namespace":       "default",
			"annotations":     map[string]string{"kognition.dev/step-name": "s1"},
			"ownerReferences": []map[string]any{{"kind": "WorkflowRun", "name": "run-j"}},
		},
		"status": map[string]any{"phase": "Ready"},
	}
	runBody, _ := json.Marshal(runDoc)
	listBody, _ := json.Marshal(map[string]any{"items": []any{job}})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if strings.HasSuffix(r.URL.Path, "/workflowruns/run-j") {
			w.Write(runBody)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/agentjobs") {
			w.Write(listBody)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	oldURL, oldClient, oldFmt, oldNs, oldK8s := serverURL, httpClient, outputFormat, namespace, k8sMode
	defer func() {
		serverURL, httpClient, outputFormat, namespace, k8sMode = oldURL, oldClient, oldFmt, oldNs, oldK8s
	}()
	serverURL, httpClient, outputFormat, namespace, k8sMode = ts.URL, ts.Client(), "json", "default", false

	old := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	err := runWorkflowLogs(workflowLogsCmd, []string{"run-j"})
	pw.Close()
	os.Stdout = old
	out, _ := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("runWorkflowLogs: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, string(out))
	}
	if _, ok := got["run"]; !ok {
		t.Fatalf("JSON missing 'run' key: %s", string(out))
	}
	if _, ok := got["jobsByStep"]; !ok {
		t.Fatalf("JSON missing 'jobsByStep' key: %s", string(out))
	}
}

// TC-0244c: HTTP error on WorkflowRun fetch surfaces a wrapped error.
func TestRunWorkflowLogs_HTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	oldURL, oldClient, oldFmt, oldNs, oldK8s := serverURL, httpClient, outputFormat, namespace, k8sMode
	defer func() {
		serverURL, httpClient, outputFormat, namespace, k8sMode = oldURL, oldClient, oldFmt, oldNs, oldK8s
	}()
	serverURL, httpClient, outputFormat, namespace, k8sMode = ts.URL, ts.Client(), "", "default", false

	err := runWorkflowLogs(workflowLogsCmd, []string{"missing"})
	if err == nil {
		t.Fatalf("expected error on 500 response")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error should mention run name, got: %v", err)
	}
}

// TC-0244d: Run with zero matching jobs still renders timeline with "-" placeholders.
func TestRunWorkflowLogs_ZeroJobsAndNilStarted(t *testing.T) {
	runDoc := map[string]any{
		"metadata": map[string]any{"name": "run-z", "namespace": "default"},
		"status": map[string]any{
			"phase":        "Pending",
			"stepStatuses": []map[string]any{{"name": "only-step", "phase": "Pending", "attempts": 0}},
		},
	}
	wrongOwnerJob := map[string]any{
		"metadata": map[string]any{
			"name":            "stray",
			"namespace":       "default",
			"annotations":     map[string]string{"kognition.dev/step-name": "only-step"},
			"ownerReferences": []map[string]any{{"kind": "Workflow", "name": "run-z"}},
		},
		"status": map[string]any{"phase": "Ready"},
	}
	runBody, _ := json.Marshal(runDoc)
	listBody, _ := json.Marshal(map[string]any{"items": []any{wrongOwnerJob}})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if strings.HasSuffix(r.URL.Path, "/workflowruns/run-z") {
			w.Write(runBody)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/agentjobs") {
			w.Write(listBody)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	oldURL, oldClient, oldFmt, oldNs, oldK8s := serverURL, httpClient, outputFormat, namespace, k8sMode
	defer func() {
		serverURL, httpClient, outputFormat, namespace, k8sMode = oldURL, oldClient, oldFmt, oldNs, oldK8s
	}()
	serverURL, httpClient, outputFormat, namespace, k8sMode = ts.URL, ts.Client(), "", "default", false

	old := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw
	err := runWorkflowLogs(workflowLogsCmd, []string{"run-z"})
	pw.Close()
	os.Stdout = old
	out, _ := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("runWorkflowLogs: %v", err)
	}
	output := string(out)
	if !strings.Contains(output, "only-step") {
		t.Fatalf("expected step row in output, got:\n%s", output)
	}
	// Wrong-kind owner must be filtered → no finished timestamp, only "-".
	if strings.Count(output, "-") < 3 {
		t.Fatalf("expected '-' placeholders for started/finished/duration, got:\n%s", output)
	}
}
