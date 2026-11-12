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

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestKnownResources_CoversCanonicalNativeKinds(t *testing.T) {
	for _, kind := range api.RegisteredKinds() {
		if !api.IsGenericResourceKind(kind) {
			continue
		}
		singular := strings.ToLower(kind)
		plural, ok := knownResources[singular]
		if !ok {
			t.Errorf("knownResources missing %s", kind)
			continue
		}
		if got := resourceKind[singular]; got != kind {
			t.Errorf("resourceKind[%q] = %q, want %q", singular, got, kind)
		}
		if got := singularize(plural); got != singular {
			t.Errorf("singularize(%q) = %q, want %q", plural, got, singular)
		}
		if got := pluralize(kind); got != plural {
			t.Errorf("pluralize(%q) = %q, want %q", kind, got, plural)
		}
	}
}

func TestKnownResourcesOmitsRuntimeApproval(t *testing.T) {
	if _, exposed := knownResources["runtimeapproval"]; exposed {
		t.Fatal("RuntimeApproval must not be exposed through generic CLI resources")
	}
	if got := kindFor("runtimeapprovals"); got != "" {
		t.Fatalf("kindFor(runtimeapprovals) = %q, want empty", got)
	}
}

func TestPluralize_KnownResources(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"agent", "agents"},
		{"Agent", "agents"},
		{"AGENT", "agents"},
		{"agentinstance", "agentinstances"},
		{"model", "models"},
		{"modelclass", "modelclasses"},
		{"tool", "tools"},
		{"toolclass", "toolclasses"},
		{"memoryclaim", "memoryclaims"},
		{"memoryclass", "memoryclasses"},
		{"prompt", "prompts"},
		{"autonomyclass", "autonomyclasses"},
		{"trustprofile", "trustprofiles"},
		{"workflow", "workflows"},
		{"evaluation", "evaluations"},
		{"evaluationdataset", "evaluationdatasets"},
		{"evaluationrun", "evaluationruns"},
		{"intentgateway", "intentgateways"},
		{"cognitivequota", "cognitivequotas"},
		{"agentpolicy", "agentpolicies"},
		{"secretref", "secretrefs"},
		{"knowledgebase", "knowledgebases"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := pluralize(tt.input)
			if got != tt.want {
				t.Errorf("pluralize(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPluralize_AlreadyPlural(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"agents", "agents"},
		{"workflows", "workflows"},
		{"evaluations", "evaluations"},
		{"evaluationruns", "evaluationruns"},
		{"tools", "tools"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := pluralize(tt.input)
			if got != tt.want {
				t.Errorf("pluralize(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPluralize_UnknownResource(t *testing.T) {
	got := pluralize("widget")
	if got != "widgets" {
		t.Errorf("pluralize(%q) = %q, want %q", "widget", got, "widgets")
	}
}

func TestSingularize_KnownResources(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"agents", "agent"},
		{"workflows", "workflow"},
		{"evaluations", "evaluation"},
		{"evaluationdatasets", "evaluationdataset"},
		{"evaluationruns", "evaluationrun"},
		{"tools", "tool"},
		{"modelclasses", "modelclass"},
		{"agentpolicies", "agentpolicy"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := singularize(tt.input)
			if got != tt.want {
				t.Errorf("singularize(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSingularize_AlreadySingular(t *testing.T) {
	got := singularize("agent")
	if got != "agent" {
		t.Errorf("singularize(%q) = %q, want %q", "agent", got, "agent")
	}
}

func TestSingularize_UnknownResource(t *testing.T) {
	got := singularize("widgets")
	// Unknown — returned as-is
	if got != "widgets" {
		t.Errorf("singularize(%q) = %q, want %q", "widgets", got, "widgets")
	}
}

func TestKindFor_EvaluationResources(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{input: "evaluation", want: "Evaluation"},
		{input: "evaluations", want: "Evaluation"},
		{input: "evaluationdataset", want: "EvaluationDataset"},
		{input: "evaluationdatasets", want: "EvaluationDataset"},
		{input: "evaluationrun", want: "EvaluationRun"},
		{input: "evaluationruns", want: "EvaluationRun"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got := kindFor(tc.input); got != tc.want {
				t.Fatalf("kindFor(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestAPIURL(t *testing.T) {
	oldURL := serverURL
	defer func() { serverURL = oldURL }()
	serverURL = "http://example.com"

	tests := []struct {
		ns       string
		resource string
		want     string
	}{
		{"default", "agents", "http://example.com/apis/kognition.dev/v1alpha1/namespaces/default/agents"},
		{"kognition", "agent", "http://example.com/apis/kognition.dev/v1alpha1/namespaces/kognition/agents"},
		{"prod", "workflows", "http://example.com/apis/kognition.dev/v1alpha1/namespaces/prod/workflows"},
	}
	for _, tt := range tests {
		t.Run(tt.ns+"/"+tt.resource, func(t *testing.T) {
			got := apiURL(tt.ns, tt.resource)
			if got != tt.want {
				t.Errorf("apiURL(%q, %q) = %q, want %q", tt.ns, tt.resource, got, tt.want)
			}
		})
	}
}

func TestAPIURLName(t *testing.T) {
	oldURL := serverURL
	defer func() { serverURL = oldURL }()
	serverURL = "http://example.com"

	got := apiURLName("default", "agent", "my-agent")
	want := "http://example.com/apis/kognition.dev/v1alpha1/namespaces/default/agents/my-agent"
	if got != want {
		t.Errorf("apiURLName() = %q, want %q", got, want)
	}
}

func TestDoGet_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"kind":"Agent"}`))
	}))
	defer ts.Close()

	oldClient := httpClient
	defer func() { httpClient = oldClient }()
	httpClient = ts.Client()

	body, err := doGet(ts.URL + "/test")
	if err != nil {
		t.Fatalf("doGet() error = %v", err)
	}
	if string(body) != `{"kind":"Agent"}` {
		t.Errorf("doGet() body = %q, want %q", string(body), `{"kind":"Agent"}`)
	}
}

func TestDoGet_ServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found"))
	}))
	defer ts.Close()

	oldClient := httpClient
	defer func() { httpClient = oldClient }()
	httpClient = ts.Client()

	_, err := doGet(ts.URL + "/test")
	if err == nil {
		t.Fatal("doGet() expected error for 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("doGet() error = %v, want to contain '404'", err)
	}
}

func TestDoGet_ConnectionError(t *testing.T) {
	oldClient := httpClient
	defer func() { httpClient = oldClient }()
	httpClient = http.DefaultClient

	_, err := doGet("http://127.0.0.1:1/unreachable")
	if err == nil {
		t.Fatal("doGet() expected error for connection failure")
	}
	if !strings.Contains(err.Error(), "request failed") {
		t.Errorf("doGet() error = %v, want to contain 'request failed'", err)
	}
}

func TestPrintOutput_JSON(t *testing.T) {
	oldFormat := outputFormat
	defer func() { outputFormat = oldFormat }()
	outputFormat = "json"

	// Capture stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	data := []byte(`{"name":"test","value":42}`)
	err := printOutput(data)

	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if err != nil {
		t.Fatalf("printOutput() error = %v", err)
	}

	// Should be valid indented JSON
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\nOutput: %s", err, string(out))
	}
	if parsed["name"] != "test" {
		t.Errorf("expected name=test, got %v", parsed["name"])
	}
}

func TestPrintOutput_YAML(t *testing.T) {
	oldFormat := outputFormat
	defer func() { outputFormat = oldFormat }()
	outputFormat = "" // default is YAML

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	data := []byte(`{"name":"test","value":42}`)
	err := printOutput(data)

	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if err != nil {
		t.Fatalf("printOutput() error = %v", err)
	}

	s := string(out)
	if !strings.Contains(s, "name: test") {
		t.Errorf("expected YAML output containing 'name: test', got:\n%s", s)
	}
}

func TestPrintOutput_InvalidJSON(t *testing.T) {
	oldFormat := outputFormat
	defer func() { outputFormat = oldFormat }()
	outputFormat = "json"

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	data := []byte(`not valid json`)
	err := printOutput(data)

	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if err != nil {
		t.Fatalf("printOutput() error = %v", err)
	}

	// Should fall back to raw output
	if !strings.Contains(string(out), "not valid json") {
		t.Errorf("expected raw fallback, got:\n%s", string(out))
	}
}
