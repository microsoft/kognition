// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestRunApply_RejectsUnsupportedAPIVersionBeforeContact(t *testing.T) {
	for _, kubernetesMode := range []bool{false, true} {
		for _, version := range []string{"kagent.dev/v1alpha1", "kagent.dev/v1alpha2", "kognition.dev/v1beta1", "apps/v1", "7", "null", "{}", "\"\""} {
			t.Run(fmt.Sprintf("kubernetes=%v/version=%s", kubernetesMode, version), func(t *testing.T) {
				requests := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					requests++
					if request.Method == http.MethodGet {
						http.NotFound(w, request)
						return
					}
					w.WriteHeader(http.StatusCreated)
				}))
				defer server.Close()
				restore := configureApplyTest(t, server, fmt.Sprintf("apiVersion: %s\nkind: Agent\nmetadata:\n  namespace: team-a\n  name: planner\n", version))
				defer restore()
				client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
				originalClient := k8sDynClient
				k8sDynClient = client
				defer func() { k8sDynClient = originalClient }()
				k8sMode = kubernetesMode
				err := runApply(nil, nil)
				if err == nil || !strings.Contains(err.Error(), "unsupported apiVersion") {
					t.Errorf("expected explicit unsupported apiVersion rejection, got %v", err)
				}
				if requests != 0 || len(client.Actions()) != 0 {
					t.Fatalf("foreign manifest contacted a resource endpoint: HTTP=%d Kubernetes=%d", requests, len(client.Actions()))
				}
			})
		}
	}
}

func TestRunApply_CreatesAfterNotFound(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		switch r.Method {
		case http.MethodGet:
			if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner" {
				t.Errorf("GET path = %q", r.URL.Path)
			}
			http.NotFound(w, r)
		case http.MethodPost:
			if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents" {
				t.Errorf("POST path = %q", r.URL.Path)
			}
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("content type = %q", got)
			}
			var resource map[string]any
			if err := json.NewDecoder(r.Body).Decode(&resource); err != nil {
				t.Fatal(err)
			}
			if resource["kind"] != "Agent" {
				t.Errorf("kind = %v", resource["kind"])
			}
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	restore := configureApplyTest(t, server, `
apiVersion: kognition.dev/v1alpha1
kind: Agent
metadata:
  name: planner
`)
	defer restore()
	oldNamespace := namespace
	namespace = "team-a"
	t.Cleanup(func() { namespace = oldNamespace })

	output := captureStdout(t, func() error { return runApply(nil, nil) })
	if strings.Join(methods, ",") != "GET,POST" {
		t.Fatalf("methods = %v", methods)
	}
	if !strings.Contains(output, "agent/planner applied") {
		t.Fatalf("output = %q", output)
	}
}

func TestRunApply_UpdatesWithCurrentResourceVersion(t *testing.T) {
	oldNamespace := namespace
	namespace = "flag-namespace"
	t.Cleanup(func() { namespace = oldNamespace })
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner" {
			t.Errorf("path = %q", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"metadata":{"resourceVersion":"42"}}`))
		case http.MethodPut:
			var resource map[string]any
			if err := json.NewDecoder(r.Body).Decode(&resource); err != nil {
				t.Fatal(err)
			}
			metadata, _ := resource["metadata"].(map[string]any)
			if metadata["resourceVersion"] != "42" {
				t.Errorf("resourceVersion = %v", metadata["resourceVersion"])
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	restore := configureApplyTest(t, server, `
kind: Agent
metadata:
  namespace: team-a
  name: planner
`)
	defer restore()

	output := captureStdout(t, func() error { return runApply(nil, nil) })
	if strings.Join(methods, ",") != "GET,PUT" {
		t.Fatalf("methods = %v", methods)
	}
	if !strings.Contains(output, "agent/planner applied") {
		t.Fatalf("output = %q", output)
	}
}

// TC-0143: The single-file five-agent scenario depends on applying every YAML document in source order.
func TestRunApply_AppliesEveryDocumentInOrder(t *testing.T) {
	var writes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			http.NotFound(w, r)
		case http.MethodPost:
			var resource map[string]any
			if err := json.NewDecoder(r.Body).Decode(&resource); err != nil {
				t.Fatal(err)
			}
			metadata, _ := resource["metadata"].(map[string]any)
			writes = append(writes, fmt.Sprintf("%s/%s", resource["kind"], metadata["name"]))
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	restore := configureApplyTest(t, server, `
---
kind: Agent
metadata:
  namespace: team-a
  name: researcher
---
# Empty documents and comments are ignored.
---
kind: Workflow
metadata:
  namespace: team-a
  name: content-pipeline
`)
	defer restore()

	output := captureStdout(t, func() error { return runApply(nil, nil) })
	if got := strings.Join(writes, ","); got != "Agent/researcher,Workflow/content-pipeline" {
		t.Fatalf("writes = %q, want both documents in source order", got)
	}
	for _, want := range []string{"agent/researcher applied", "workflow/content-pipeline applied"} {
		if !strings.Contains(output, want) {
			t.Errorf("output = %q, want %q", output, want)
		}
	}
}

func TestRunApply_KubernetesCreatesAndUpdatesResources(t *testing.T) {
	for _, test := range []struct {
		name       string
		existing   *unstructured.Unstructured
		wantAction string
	}{
		{name: "create", wantAction: "create"},
		{
			name: "update",
			existing: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "kognition.dev/v1alpha1",
				"kind":       "Agent",
				"metadata": map[string]any{
					"name": "planner", "namespace": "team-a", "resourceVersion": "42",
				},
			}},
			wantAction: "update",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetDiscoveryCache(t)
			primeDiscoveryCache(map[string]bool{"agents": true}, nil)
			agentsGVR := schema.GroupVersionResource{Group: "kognition.dev", Version: "v1alpha1", Resource: "agents"}
			scheme := runtime.NewScheme()
			scheme.AddKnownTypeWithName(agentsGVR.GroupVersion().WithKind("Agent"), &unstructured.Unstructured{})
			scheme.AddKnownTypeWithName(agentsGVR.GroupVersion().WithKind("AgentList"), &unstructured.UnstructuredList{})
			objects := []runtime.Object{}
			if test.existing != nil {
				objects = append(objects, test.existing)
			}
			client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
				scheme,
				map[schema.GroupVersionResource]string{agentsGVR: "AgentList"},
				objects...,
			)
			k8sDynClient = client

			server := httptest.NewServer(http.NotFoundHandler())
			defer server.Close()
			restore := configureApplyTest(t, server, "kind: Agent\nmetadata:\n  namespace: team-a\n  name: planner\n")
			defer restore()
			k8sMode = true

			output := captureStdout(t, func() error { return runApply(nil, nil) })
			if !strings.Contains(output, "agent/planner applied") {
				t.Fatalf("output = %q", output)
			}
			actions := client.Actions()
			if got := actions[len(actions)-1].GetVerb(); got != test.wantAction {
				t.Fatalf("last action = %q, want %q; actions = %#v", got, test.wantAction, actions)
			}
			if test.wantAction == "update" {
				updated, ok := actions[len(actions)-1].(interface{ GetObject() runtime.Object })
				if !ok || updated.GetObject().(*unstructured.Unstructured).GetResourceVersion() != "42" {
					t.Fatalf("update did not preserve resourceVersion: %#v", actions[len(actions)-1])
				}
			}
		})
	}
}

func TestRunApply_KubernetesPropagatesMutationFailures(t *testing.T) {
	for _, test := range []struct {
		name     string
		existing *unstructured.Unstructured
		verb     string
	}{
		{name: "create", verb: "create"},
		{
			name: "update",
			existing: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "kognition.dev/v1alpha1",
				"kind":       "Agent",
				"metadata": map[string]any{
					"name": "planner", "namespace": "team-a", "resourceVersion": "42",
				},
			}},
			verb: "update",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetDiscoveryCache(t)
			primeDiscoveryCache(map[string]bool{"agents": true}, nil)
			agentsGVR := schema.GroupVersionResource{Group: "kognition.dev", Version: "v1alpha1", Resource: "agents"}
			scheme := runtime.NewScheme()
			scheme.AddKnownTypeWithName(agentsGVR.GroupVersion().WithKind("Agent"), &unstructured.Unstructured{})
			scheme.AddKnownTypeWithName(agentsGVR.GroupVersion().WithKind("AgentList"), &unstructured.UnstructuredList{})
			objects := []runtime.Object{}
			if test.existing != nil {
				objects = append(objects, test.existing)
			}
			client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
				scheme,
				map[schema.GroupVersionResource]string{agentsGVR: "AgentList"},
				objects...,
			)
			client.PrependReactor(test.verb, "agents", func(ktesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("mutation rejected")
			})
			k8sDynClient = client

			server := httptest.NewServer(http.NotFoundHandler())
			defer server.Close()
			restore := configureApplyTest(t, server, "kind: Agent\nmetadata:\n  namespace: team-a\n  name: planner\n")
			defer restore()
			k8sMode = true

			err := runApply(nil, nil)
			if err == nil || !strings.Contains(err.Error(), "mutation rejected") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRunApply_PropagatesStandaloneMutationTransportFailures(t *testing.T) {
	for _, test := range []struct {
		name        string
		getStatus   int
		getResponse string
	}{
		{name: "create", getStatus: http.StatusNotFound},
		{name: "update", getStatus: http.StatusOK, getResponse: `{"metadata":{"resourceVersion":"42"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.NotFoundHandler())
			defer server.Close()
			restore := configureApplyTest(t, server, "kind: Agent\nmetadata:\n  name: planner\n")
			defer restore()
			requests := 0
			httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				if requests == 1 {
					return &http.Response{
						StatusCode: test.getStatus,
						Body:       io.NopCloser(strings.NewReader(test.getResponse)),
					}, nil
				}
				return nil, errors.New("transport unavailable")
			})}

			err := runApply(nil, nil)
			if err == nil || !strings.Contains(err.Error(), "transport unavailable") || requests != 2 {
				t.Fatalf("error = %v, requests = %d", err, requests)
			}
		})
	}
}

func TestRunApply_ReadsExplicitAndImplicitStdin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	for _, test := range []struct {
		name      string
		applyFile string
	}{
		{name: "explicit", applyFile: "-"},
		{name: "implicit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdin, err := os.CreateTemp(t.TempDir(), "manifest-*.yaml")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stdin.WriteString("kind: Agent\nmetadata:\n  name: planner\n"); err != nil {
				t.Fatal(err)
			}
			if _, err := stdin.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()

			oldStdin, oldApplyFile := os.Stdin, applyFile
			os.Stdin, applyFile = stdin, test.applyFile
			t.Cleanup(func() { os.Stdin, applyFile = oldStdin, oldApplyFile })
			restore := configureGetTest(server)
			defer restore()

			output := captureStdout(t, func() error { return runApply(nil, nil) })
			if !strings.Contains(output, "agent/planner applied") {
				t.Fatalf("output = %q", output)
			}
		})
	}
}

func TestRunApply_ReportsStdinReadFailure(t *testing.T) {
	stdin, err := os.CreateTemp(t.TempDir(), "closed-stdin")
	if err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	oldStdin, oldApplyFile := os.Stdin, applyFile
	os.Stdin, applyFile = stdin, "-"
	t.Cleanup(func() { os.Stdin, applyFile = oldStdin, oldApplyFile })

	err = runApply(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "read stdin") {
		t.Fatalf("error = %v, want stdin read failure", err)
	}
}

func TestRunApply_ExistenceCheckFailureDoesNotCreate(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want only GET", r.Method)
		}
		http.Error(w, "storage unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()

	restore := configureApplyTest(t, server, `
kind: Agent
metadata:
  namespace: team-a
  name: planner
`)
	defer restore()

	err := runApply(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "storage unavailable") {
		t.Fatalf("error = %v, want existence-check HTTP 500", err)
	}
	if requestCount != 1 {
		t.Fatalf("request count = %d, want 1", requestCount)
	}
}

func configureApplyTest(t *testing.T, server *httptest.Server, manifest string) func() {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resource.yaml")
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	restore := configureGetTest(server)
	oldApplyFile := applyFile
	applyFile = path
	return func() {
		applyFile = oldApplyFile
		restore()
	}
}

func TestRunApply_RejectsInvalidManifest(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{name: "empty input", want: "empty input"},
		{name: "malformed YAML", manifest: "metadata: [", want: "parse resource"},
		{name: "missing identity", manifest: "kind: Agent", want: "resource must have kind and metadata.name"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("invalid manifest reached API server")
			}))
			defer server.Close()

			restore := configureApplyTest(t, server, test.manifest)
			defer restore()

			err := runApply(nil, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRunApply_PropagatesInputAndTransportFailures(t *testing.T) {
	t.Run("unreadable file", func(t *testing.T) {
		oldApplyFile := applyFile
		applyFile = filepath.Join(t.TempDir(), "missing.yaml")
		defer func() { applyFile = oldApplyFile }()

		err := runApply(nil, nil)
		if err == nil || !strings.Contains(err.Error(), "read file") {
			t.Fatalf("error = %v, want read file failure", err)
		}
	})

	t.Run("API transport", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		restore := configureApplyTest(t, server, "kind: Agent\nmetadata:\n  name: planner\n")
		server.Close()
		defer restore()

		err := runApply(nil, nil)
		if err == nil {
			t.Fatal("expected transport failure")
		}
	})
}

func TestRunApply_RejectsMalformedExistingResource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"metadata":`))
	}))
	defer server.Close()

	restore := configureApplyTest(t, server, "kind: Agent\nmetadata:\n  name: planner\n")
	defer restore()

	err := runApply(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "parse existing resource") {
		t.Fatalf("error = %v, want existing resource parse failure", err)
	}
}

func TestRunApply_PropagatesCreateAndUpdateFailures(t *testing.T) {
	tests := []struct {
		name        string
		getResponse string
		getStatus   int
		writeMethod string
	}{
		{name: "create", getStatus: http.StatusNotFound, writeMethod: http.MethodPost},
		{name: "update", getStatus: http.StatusOK, getResponse: `{"metadata":{"resourceVersion":"9"}}`, writeMethod: http.MethodPut},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var methods []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				methods = append(methods, r.Method)
				if r.Method == http.MethodGet {
					w.WriteHeader(test.getStatus)
					_, _ = w.Write([]byte(test.getResponse))
					return
				}
				http.Error(w, "admission denied", http.StatusUnprocessableEntity)
			}))
			defer server.Close()

			restore := configureApplyTest(t, server, "kind: Agent\nmetadata:\n  name: planner\n")
			defer restore()

			err := runApply(nil, nil)
			if err == nil || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "admission denied") {
				t.Fatalf("error = %v, want write rejection", err)
			}
			if got := strings.Join(methods, ","); got != "GET,"+test.writeMethod {
				t.Fatalf("methods = %s, want GET,%s", got, test.writeMethod)
			}
		})
	}
}
