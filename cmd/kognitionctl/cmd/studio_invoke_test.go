// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestDecodeTool_ToleratesKubernetesResourceVersion(t *testing.T) {
	raw := []byte(`{
  "apiVersion":"kognition.dev/v1alpha1",
  "kind":"Tool",
  "metadata":{
    "name":"calculator",
    "namespace":"team-a",
    "resourceVersion":"42",
    "labels":{"tier":"trusted"},
    "annotations":{"owner":"platform"}
  },
  "spec":{"className":"sandbox","protocol":"OpenAPI","endpoint":"https://example.test/calculate"}
}`)

	tool, err := decodeTool(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tool.Name != "calculator" || tool.Namespace != "team-a" {
		t.Errorf("metadata = %#v", tool.ObjectMeta)
	}
	if tool.Labels["tier"] != "trusted" || tool.Annotations["owner"] != "platform" {
		t.Errorf("metadata maps = labels %#v, annotations %#v", tool.Labels, tool.Annotations)
	}
	if tool.Spec.Protocol != v1alpha1.ToolProtocolOpenAPI || tool.Spec.Endpoint != "https://example.test/calculate" {
		t.Errorf("spec = %#v", tool.Spec)
	}
}

func TestStudioInvokeAgentStandalone_ForwardsMessageAndSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/project-b/agents/planner/chat" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["message"] != "Plan the release" || body["sessionID"] != "session-7" {
			t.Errorf("body = %#v", body)
		}
		_, _ = w.Write([]byte(`{"sessionID":"session-7","response":"Ready","usage":{"totalTokens":12}}`))
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	req := studioRequest(http.MethodPost, "/api/invoke/agent/planner?namespace=project-b", "planner", `{"message":"Plan the release","sessionID":"session-7"}`)
	recorder := httptest.NewRecorder()

	handleStudioInvokeAgent(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response["response"] != "Ready" || response["sessionID"] != "session-7" || response["usage"] == nil {
		t.Errorf("response = %#v", response)
	}
}

func TestStudioInvokeAgentK8sUsesCapabilityAwareInstance(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	oldMode, oldClient := k8sMode, k8sDynClient
	k8sMode, k8sDynClient = true, client
	t.Cleanup(func() { k8sMode, k8sDynClient = oldMode, oldClient })

	var created *unstructured.Unstructured
	client.PrependReactor("create", "agentinstances", func(action k8stesting.Action) (bool, runtime.Object, error) {
		created = action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		return true, created, nil
	})
	client.PrependReactor("get", "agents", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kognition.dev/v1alpha1",
			"kind":       "Agent",
			"metadata":   map[string]any{"name": "planner", "namespace": "project-b"},
			"status":     map[string]any{"revisionCount": int64(9)},
		}}, nil
	})
	client.PrependReactor("get", "agentinstances", func(k8stesting.Action) (bool, runtime.Object, error) {
		completed := created.DeepCopy()
		completed.Object["status"] = map[string]any{
			"phase":           "Ready",
			"sessionID":       "runtime-session-9",
			"output":          "Release planned",
			"tokenUsage":      map[string]any{"totalTokens": int64(21)},
			"toolCalls":       int64(1),
			"toolCallsDetail": []any{map[string]any{"name": "calendar"}},
		}
		return true, completed, nil
	})

	req := studioRequest(http.MethodPost, "/api/invoke/agent/planner?namespace=project-b", "planner", `{"message":"Plan the release","conversationID":"conversation-9"}`)
	recorder := httptest.NewRecorder()
	handleStudioInvokeAgent(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response["response"] != "Release planned" || response["sessionID"] != "runtime-session-9" || response["conversationID"] != "conversation-9" {
		t.Errorf("response = %#v", response)
	}
	if response["toolCalls"] != float64(1) || response["toolCallsDetail"] == nil || response["usage"] == nil {
		t.Errorf("response telemetry = %#v", response)
	}
	conversationID, _, _ := unstructured.NestedString(created.Object, "spec", "sessionContext", "conversation_id")
	revision, _, _ := unstructured.NestedInt64(created.Object, "spec", "revision")
	if conversationID != "conversation-9" || revision != 9 || created.GetLabels()["kognition.dev/created-by"] != "studio" {
		t.Errorf("created AgentInstance = %#v", created.Object)
	}
}

func TestStudioInvokeAgentStandalone_ReportsBackendFailures(t *testing.T) {
	t.Run("invalid backend URL", func(t *testing.T) {
		oldURL := serverURL
		serverURL = "://invalid"
		t.Cleanup(func() { serverURL = oldURL })
		recorder := httptest.NewRecorder()
		studioInvokeAgentStandalone(recorder, studioRequest(http.MethodPost, "/api/invoke/agent/planner", "planner", ""), "default", "planner", "hello", "")
		if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "build request") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		oldURL, oldClient := serverURL, httpClient
		serverURL = "http://backend.invalid"
		httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, context.DeadlineExceeded
		})}
		t.Cleanup(func() { serverURL, httpClient = oldURL, oldClient })
		recorder := httptest.NewRecorder()
		studioInvokeAgentStandalone(recorder, studioRequest(http.MethodPost, "/api/invoke/agent/planner", "planner", ""), "default", "planner", "hello", "")
		if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "invoke agent") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("backend rejection", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "model disabled", http.StatusForbidden)
		}))
		defer server.Close()
		restore := configureGetTest(server)
		defer restore()
		recorder := httptest.NewRecorder()
		studioInvokeAgentStandalone(recorder, studioRequest(http.MethodPost, "/api/invoke/agent/planner", "planner", ""), "default", "planner", "hello", "")
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "agent returned 403: model disabled") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})
}

func TestStudioInvokeAgentRuntimeDirect_CompletesGeneratedTurn(t *testing.T) {
	var sessionMu sync.Mutex
	var runtimeSessionID string
	var gotTraceparent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer controller-token" {
			http.Error(w, "missing runtime authorization", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/invoke":
			gotTraceparent = r.Header.Get("traceparent")
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
				return
			}
			if payload["message"] != "Plan the release" || payload["conversation_id"] != "conversation-7" {
				t.Errorf("invoke payload = %#v", payload)
			}
			if !strings.HasPrefix(payload["session_id"], "studio-") || payload["session_id"] == "frontend-session" {
				t.Errorf("runtime session_id = %q, want fresh Studio turn", payload["session_id"])
			}
			sessionMu.Lock()
			runtimeSessionID = payload["session_id"]
			sessionMu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == "/status":
			sessionMu.Lock()
			currentSessionID := runtimeSessionID
			sessionMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"phase":             "Completed",
				"session_id":        currentSessionID,
				"output":            "Release planned",
				"token_usage":       map[string]any{"totalTokens": 21},
				"tool_calls":        1,
				"tool_calls_detail": []map[string]any{{"name": "calendar"}},
			})
		default:
			t.Errorf("unexpected runtime request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	k8sMode = true
	oldCoreClient := k8sCoreClient
	k8sCoreClient = fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-planner", Namespace: "project-b", Labels: map[string]string{"kognition.dev/agent-name": "planner", "kognition.dev/agent-namespace": "project-b", "app.kubernetes.io/managed-by": "kognition"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Env: []corev1.EnvVar{{Name: "KOGNITION_RUNTIME_AUTH_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "runtime-auth"}, Key: "runtime-control-token"}}}}}}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-auth", Namespace: "project-b"}, Data: map[string][]byte{"runtime-control-token": []byte("controller-token")}},
	)
	t.Cleanup(func() { k8sCoreClient = oldCoreClient })
	seedAgentPortForward(t, server, "project-b", "planner")
	req := studioRequest(http.MethodPost, "/api/invoke/agent/planner?namespace=project-b", "planner", `{"message":"Plan the release","sessionID":"frontend-session","conversationID":"conversation-7"}`)
	recorder := httptest.NewRecorder()

	studioInvokeAgentRuntimeDirect(recorder, req, "project-b", "planner", "Plan the release", "conversation-7")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response["response"] != "Release planned" || response["conversationID"] != "conversation-7" || response["sessionID"] != runtimeSessionID {
		t.Errorf("response = %#v", response)
	}
	if response["usage"] == nil || response["toolCalls"] != float64(1) || response["toolCallsDetail"] == nil {
		t.Errorf("response telemetry = %#v", response)
	}
	if gotTraceparent != "" {
		t.Fatalf("traceparent header = %q, want runtime-owned conversation trace root", gotTraceparent)
	}
}

func TestStudioInvokeAgentRuntimeDirect_ReportsRuntimeFailures(t *testing.T) {
	tests := []struct {
		name         string
		invokeStatus int
		statusStatus int
		phase        string
		runtimeError string
		wantStatus   int
		wantError    string
	}{
		{name: "invoke rejected", invokeStatus: http.StatusServiceUnavailable, wantStatus: http.StatusServiceUnavailable, wantError: "agent returned 503: runtime unavailable"},
		{name: "status rejected", invokeStatus: http.StatusAccepted, statusStatus: http.StatusBadGateway, wantStatus: http.StatusBadGateway, wantError: "status returned 502: status unavailable"},
		{name: "runtime failed", invokeStatus: http.StatusAccepted, statusStatus: http.StatusOK, phase: "Failed", runtimeError: "model unavailable", wantStatus: http.StatusBadGateway, wantError: "model unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var runtimeSessionID string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/invoke":
					var payload map[string]string
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
						return
					}
					runtimeSessionID = payload["session_id"]
					w.WriteHeader(tt.invokeStatus)
					if tt.invokeStatus >= 400 {
						_, _ = w.Write([]byte("runtime unavailable"))
					}
				case "/status":
					w.WriteHeader(tt.statusStatus)
					if tt.statusStatus >= 400 {
						_, _ = w.Write([]byte("status unavailable"))
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"phase": tt.phase, "session_id": runtimeSessionID, "error": tt.runtimeError})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			restore := configureGetTest(server)
			defer restore()
			k8sMode = true
			seedAgentPortForward(t, server, "project-b", "planner")
			req := studioRequest(http.MethodPost, "/api/invoke/agent/planner?namespace=project-b", "planner", `{"message":"Plan the release"}`)
			recorder := httptest.NewRecorder()

			studioInvokeAgentRuntimeDirect(recorder, req, "project-b", "planner", "Plan the release", "")

			if recorder.Code != tt.wantStatus || !strings.Contains(recorder.Body.String(), tt.wantError) {
				t.Fatalf("status = %d, body = %s, want status %d containing %q", recorder.Code, recorder.Body.String(), tt.wantStatus, tt.wantError)
			}
		})
	}
}

func TestStudioInvokeAgentRuntimeDirect_RetriesUnusableStatusAndDefaultsFailureMessage(t *testing.T) {
	var runtimeSessionID string
	statusCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/invoke":
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
				return
			}
			runtimeSessionID = payload["session_id"]
			w.WriteHeader(http.StatusAccepted)
		case "/status":
			statusCalls++
			switch statusCalls {
			case 1:
				_, _ = w.Write([]byte("not-json"))
			case 2:
				_ = json.NewEncoder(w).Encode(map[string]any{"phase": "Completed", "session_id": "previous-turn"})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"phase": "Failed", "session_id": runtimeSessionID})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	restore := configureGetTest(server)
	defer restore()
	k8sMode = true
	seedAgentPortForward(t, server, "project-b", "planner")
	recorder := httptest.NewRecorder()

	req := studioRequest(http.MethodPost, "/api/invoke/agent/planner?namespace=project-b", "planner", `{"message":"Plan"}`)
	studioInvokeAgentRuntimeDirect(recorder, req, "project-b", "planner", "Plan", "")

	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "agent run failed") || statusCalls != 3 {
		t.Fatalf("status = %d, calls = %d, body = %s", recorder.Code, statusCalls, recorder.Body.String())
	}
}

func TestStudioInvokeAgentRuntimeDirect_ReportsPortForwardResolutionFailure(t *testing.T) {
	oldMode, oldCoreClient := k8sMode, k8sCoreClient
	k8sMode, k8sCoreClient = true, nil
	t.Cleanup(func() { k8sMode, k8sCoreClient = oldMode, oldCoreClient })
	pfCacheMu.Lock()
	delete(pfCache, "project-b/planner")
	delete(pfInflight, "project-b/planner")
	pfCacheMu.Unlock()
	recorder := httptest.NewRecorder()

	req := studioRequest(http.MethodPost, "/api/invoke/agent/planner?namespace=project-b", "planner", `{"message":"Plan"}`)
	studioInvokeAgentRuntimeDirect(recorder, req, "project-b", "planner", "Plan", "")

	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "kubernetes client not initialized") {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestStudioInvokeAgent_RejectsInvalidRequestsBeforeBackend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid request reached backend")
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "malformed JSON", body: `{`, want: "parse body"},
		{name: "missing message", body: `{"message":"  "}`, want: "message is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := studioRequest(http.MethodPost, "/api/invoke/agent/planner", "planner", test.body)
			recorder := httptest.NewRecorder()
			handleStudioInvokeAgent(recorder, req)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), test.want) {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestStudioInvokeWorkflow_CreatesLabeledRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/project-b/workflowruns" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var obj map[string]any
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			t.Fatal(err)
		}
		meta, _ := obj["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		spec, _ := obj["spec"].(map[string]any)
		workflowRef, _ := spec["workflowRef"].(map[string]any)
		parameters, _ := spec["parameters"].(map[string]any)
		if !strings.HasPrefix(meta["name"].(string), "release-flow-studio-") || meta["namespace"] != "project-b" {
			t.Errorf("metadata = %#v", meta)
		}
		if labels["kognition.dev/created-by"] != "studio" || labels["kognition.dev/workflow"] != "Release-Flow" {
			t.Errorf("labels = %#v", labels)
		}
		if workflowRef["name"] != "Release-Flow" || spec["input"] != "ship" || parameters["region"] != "west" {
			t.Errorf("spec = %#v", spec)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"kind":"WorkflowRun","metadata":{"name":"created-run"}}`))
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	req := studioRequest(http.MethodPost, "/api/invoke/workflow/Release-Flow?namespace=project-b", "Release-Flow", `{"input":"ship","parameters":{"region":"west"}}`)
	recorder := httptest.NewRecorder()

	handleStudioInvokeWorkflow(recorder, req)

	if recorder.Code != http.StatusCreated || !strings.Contains(recorder.Body.String(), `"created-run"`) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestExtractOpenAPIRequestSchema_MatchesEndpointAndPreferredVerb(t *testing.T) {
	doc := map[string]any{
		"paths": map[string]any{
			"/fallback": map[string]any{
				"post": map[string]any{
					"requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"title": "fallback"}}}},
				},
			},
			"/calculate": map[string]any{
				"put": map[string]any{
					"summary":     "Short summary",
					"description": "Detailed calculation",
					"requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}},
				},
			},
		},
	}

	schema, description := extractOpenAPIRequestSchema(doc, "https://example.test/calculate")
	if schema["type"] != "object" || description != "Detailed calculation" {
		t.Errorf("schema = %#v, description = %q", schema, description)
	}
}

func TestIntrospectMCPSchema_SelectsToolByName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request = %s, content type = %q", r.Method, r.Header.Get("Content-Type"))
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["method"] != "tools/list" {
			t.Errorf("request = %#v", request)
		}
		_, _ = w.Write([]byte(`{"result":{"tools":[
  {"name":"other","description":"Other tool","inputSchema":{"title":"wrong"}},
  {"name":"calculator","description":"Calculate values","inputSchema":{"type":"object"}}
]}}`))
	}))
	defer server.Close()
	resetClusterProxyClient(t)

	tool := v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "calculator"},
		Spec:       v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolMCP, Endpoint: server.URL},
	}
	schema, description, raw, err := introspectMCPSchema(context.Background(), tool)
	if err != nil {
		t.Fatal(err)
	}
	if schema["type"] != "object" || description != "Calculate values" {
		t.Errorf("schema = %#v, description = %q", schema, description)
	}
	matched, _ := raw.(map[string]any)
	if matched["name"] != "calculator" {
		t.Errorf("raw = %#v", raw)
	}
}

func TestIntrospectOpenAPISchema_SupportsDocumentAndLegacyShape(t *testing.T) {
	tests := []struct {
		name      string
		response  string
		path      string
		wantTitle string
		wantDesc  string
	}{
		{
			name: "OpenAPI document",
			path: "/calculate",
			response: `{"openapi":"3.0.0","paths":{"/calculate":{"post":{
  "summary":"Calculate values",
  "requestBody":{"content":{"application/json":{"schema":{"title":"request"}}}}
}}}}`,
			wantTitle: "request",
			wantDesc:  "Calculate values",
		},
		{
			name:      "legacy shape",
			path:      "/legacy",
			response:  `{"inputSchema":{"title":"legacy"},"description":"Legacy tool"}`,
			wantTitle: "legacy",
			wantDesc:  "Legacy tool",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Accept") != "application/json" {
					t.Errorf("request = %s, accept = %q", r.Method, r.Header.Get("Accept"))
				}
				_, _ = w.Write([]byte(test.response))
			}))
			defer server.Close()
			resetClusterProxyClient(t)

			tool := v1alpha1.Tool{Spec: v1alpha1.ToolSpec{
				Protocol: v1alpha1.ToolProtocolOpenAPI,
				Endpoint: server.URL + test.path,
			}}
			schema, description, raw, err := introspectOpenAPISchema(context.Background(), tool)
			if err != nil {
				t.Fatal(err)
			}
			if schema["title"] != test.wantTitle || description != test.wantDesc || raw == nil {
				t.Errorf("schema = %#v, description = %q, raw = %#v", schema, description, raw)
			}
		})
	}
}

func TestStudioToolSchema_FallsBackToDeclaredSchema(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis/kognition.dev/v1alpha1/namespaces/team-a/tools/calculator" {
			_, _ = w.Write([]byte(`{
  "kind":"Tool",
  "metadata":{"name":"calculator","namespace":"team-a","resourceVersion":"9"},
  "spec":{
    "className":"sandbox",
    "protocol":"OpenAPI",
    "description":"Declared calculator",
    "endpoint":"` + server.URL + `/schema",
    "inputSchema":{"type":"object","required":["expression"]}
  }
}`))
			return
		}
		http.Error(w, "schema backend unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	resetClusterProxyClient(t)
	req := studioRequest(http.MethodGet, "/api/tools/calculator/schema", "calculator", "")
	recorder := httptest.NewRecorder()

	handleStudioToolSchema(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	schema, _ := response["inputSchema"].(map[string]any)
	if response["source"] != "declared" || schema["type"] != "object" {
		t.Errorf("response = %#v", response)
	}
	if warning, _ := response["warning"].(string); !strings.Contains(warning, "503") {
		t.Errorf("warning = %q", warning)
	}
}

func TestStudioInvokeTool_OpenAPIForwardsInput(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/kognition.dev/v1alpha1/namespaces/team-a/tools/calculator":
			_, _ = w.Write([]byte(`{
  "apiVersion":"kognition.dev/v1alpha1",
  "kind":"Tool",
  "metadata":{"name":"calculator","namespace":"team-a"},
	"spec":{"className":"sandbox","protocol":"OpenAPI","endpoint":"` + server.URL + `/calculate","metadata":{"allowPrivateNetwork":"true"}}
}`))
		case "/calculate":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
				t.Errorf("request = %s, content type = %q, accept = %q", r.Method, r.Header.Get("Content-Type"), r.Header.Get("Accept"))
			}
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input["expression"] != "6*7" {
				t.Errorf("input = %#v", input)
			}
			_, _ = w.Write([]byte(`{"value":42}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	resetClusterProxyClient(t)
	req := studioRequest(http.MethodPost, "/api/invoke/tool/calculator?namespace=team-a", "calculator", `{"input":{"expression":"6*7"},"timeoutSeconds":301}`)
	recorder := httptest.NewRecorder()

	handleStudioInvokeTool(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	output, _ := response["output"].(map[string]any)
	if output["value"] != float64(42) || response["executionMode"] != "sandbox" || response["protocol"] != "OpenAPI" {
		t.Errorf("response = %#v", response)
	}
}

func TestStudioInvokeTool_MCPUsesToolNameAndArguments(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/kognition.dev/v1alpha1/namespaces/default/tools/search":
			_, _ = w.Write([]byte(`{
  "apiVersion":"kognition.dev/v1alpha1",
  "kind":"Tool",
  "metadata":{"name":"search","namespace":"default"},
	"spec":{"className":"sandbox","protocol":"MCP","endpoint":"` + server.URL + `/mcp","metadata":{"allowPrivateNetwork":"true"}}
}`))
		case "/mcp":
			var request struct {
				JSONRPC string `json:"jsonrpc"`
				ID      any    `json:"id"`
				Method  string `json:"method"`
				Params  struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"params"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			switch request.Method {
			case "initialize":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      request.ID,
					"result": map[string]any{
						"protocolVersion": "2025-03-26",
						"capabilities":    map[string]any{},
						"serverInfo":      map[string]any{"name": "test", "version": "1"},
					},
				})
			case "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
			case "tools/call":
				if request.JSONRPC != "2.0" || request.Params.Name != "search" || request.Params.Arguments["query"] != "release" {
					t.Errorf("request = %#v", request)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      request.ID,
					"result": map[string]any{
						"content": []any{map[string]any{"type": "text", "text": "found"}},
					},
				})
			default:
				t.Errorf("unexpected MCP method %q", request.Method)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	resetClusterProxyClient(t)
	req := studioRequest(http.MethodPost, "/api/invoke/tool/search?namespace=", "search", `{"input":{"query":"release"},"timeoutSeconds":2}`)
	recorder := httptest.NewRecorder()

	handleStudioInvokeTool(recorder, req)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"text":"found"`) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestStudioInvokeTool_ReturnsStructuredBackendError(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tools/calculator") {
			_, _ = w.Write([]byte(`{
  "apiVersion":"kognition.dev/v1alpha1",
  "kind":"Tool",
  "metadata":{"name":"calculator","namespace":"default"},
	"spec":{"className":"sandbox","protocol":"OpenAPI","endpoint":"` + server.URL + `/calculate","metadata":{"allowPrivateNetwork":"true"}}
}`))
			return
		}
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	resetClusterProxyClient(t)
	req := studioRequest(http.MethodPost, "/api/invoke/tool/calculator", "calculator", `{}`)
	recorder := httptest.NewRecorder()
	handleStudioInvokeTool(recorder, req)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response["code"] != "ToolHTTPError" || response["retryable"] != true || response["protocol"] != "OpenAPI" {
		t.Errorf("response = %#v", response)
	}
}

func TestStudioInvokeTool_RejectsMalformedAndUnsupportedRequests(t *testing.T) {
	t.Run("malformed body", func(t *testing.T) {
		req := studioRequest(http.MethodPost, "/api/invoke/tool/calculator", "calculator", `{`)
		recorder := httptest.NewRecorder()
		handleStudioInvokeTool(recorder, req)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "parse body") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("unsupported protocol", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{
  "apiVersion":"kognition.dev/v1alpha1",
  "kind":"Tool",
  "metadata":{"name":"browser","namespace":"default"},
  "spec":{"className":"sandbox","protocol":"Browser"}
}`))
		}))
		defer server.Close()
		restore := configureGetTest(server)
		defer restore()

		req := studioRequest(http.MethodPost, "/api/invoke/tool/browser", "browser", "")
		recorder := httptest.NewRecorder()
		handleStudioInvokeTool(recorder, req)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "only OpenAPI and MCP") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})
}

func TestStudioInvokeTool_ReportsRequestAndToolLoadingErrors(t *testing.T) {
	t.Run("body too large", func(t *testing.T) {
		req := studioRequest(http.MethodPost, "/api/invoke/tool/calculator", "calculator", strings.Repeat("x", (1<<20)+1))
		recorder := httptest.NewRecorder()
		handleStudioInvokeTool(recorder, req)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "request body too large") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantError  string
	}{
		{name: "tool lookup failure", status: http.StatusNotFound, body: "missing", wantStatus: http.StatusNotFound, wantError: "missing"},
		{name: "invalid tool resource", status: http.StatusOK, body: `{`, wantStatus: http.StatusInternalServerError, wantError: "decode tool"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			restore := configureGetTest(server)
			defer restore()

			req := studioRequest(http.MethodPost, "/api/invoke/tool/calculator", "calculator", `{}`)
			recorder := httptest.NewRecorder()
			handleStudioInvokeTool(recorder, req)
			if recorder.Code != test.wantStatus || !strings.Contains(recorder.Body.String(), test.wantError) {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestRegisterStudioInvokeAPI_RegistersEveryEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	registerStudioInvokeAPI(mux)

	tests := []struct {
		method  string
		path    string
		pattern string
	}{
		{method: http.MethodPost, path: "/api/invoke/agent/test", pattern: "POST /api/invoke/agent/{name}"},
		{method: http.MethodPost, path: "/api/invoke/workflow/test", pattern: "POST /api/invoke/workflow/{name}"},
		{method: http.MethodPost, path: "/api/invoke/tool/test", pattern: "POST /api/invoke/tool/{name}"},
		{method: http.MethodGet, path: "/api/tools/test/schema", pattern: "GET /api/tools/{name}/schema"},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader("{"))
		_, pattern := mux.Handler(request)
		if pattern != test.pattern {
			t.Errorf("%s %s matched pattern %q, want %q", test.method, test.path, pattern, test.pattern)
		}
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/tools/calculator" {
			t.Errorf("backend path = %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{
  "kind":"Tool",
  "metadata":{"name":"calculator","namespace":"team-a"},
  "spec":{"protocol":"OpenAPI","endpoint":"https://example.test/calculate","inputSchema":{"type":"object"}}
}`))
	}))
	defer backend.Close()
	restore := configureGetTest(backend)
	defer restore()

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/tools/calculator/schema?namespace=team-a", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"source":"declared"`) {
		t.Fatalf("schema route did not reach tool-schema handler: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestStudioInvokeHandlers_ReportBackendFailures(t *testing.T) {
	t.Run("standalone agent rejection", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "model unavailable", http.StatusServiceUnavailable)
		}))
		defer server.Close()
		restore := configureGetTest(server)
		defer restore()

		recorder := httptest.NewRecorder()
		handleStudioInvokeAgent(recorder, studioRequest(http.MethodPost, "/api/invoke/agent/test", "test", `{"message":"hello"}`))
		if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "model unavailable") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("workflow malformed body", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handleStudioInvokeWorkflow(recorder, studioRequest(http.MethodPost, "/api/invoke/workflow/test", "test", "{"))
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "parse body") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("workflow create rejection", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "quota exceeded", http.StatusTooManyRequests)
		}))
		defer server.Close()
		restore := configureGetTest(server)
		defer restore()

		recorder := httptest.NewRecorder()
		handleStudioInvokeWorkflow(recorder, studioRequest(http.MethodPost, "/api/invoke/workflow/test", "test", `{}`))
		if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "server error (429): quota exceeded") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})
}

func TestStudioToolSchema_UsesLiveMCPAndReportsUnsupportedProtocol(t *testing.T) {
	t.Run("live MCP schema", func(t *testing.T) {
		var server *httptest.Server
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/tools/calculator"):
				_, _ = w.Write([]byte(`{"metadata":{"name":"calculator"},"spec":{"protocol":"MCP","endpoint":"` + server.URL + `/mcp"}}`))
			case r.URL.Path == "/mcp":
				_, _ = w.Write([]byte(`{"result":{"tools":[{"name":"only-tool","description":"Live schema","inputSchema":{"type":"object"}}]}}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		restore := configureGetTest(server)
		defer restore()
		resetClusterProxyClient(t)

		recorder := httptest.NewRecorder()
		handleStudioToolSchema(recorder, studioRequest(http.MethodGet, "/api/tools/calculator/schema", "calculator", ""))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"source":"live"`) || !strings.Contains(recorder.Body.String(), "Live schema") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("unsupported protocol", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"metadata":{"name":"browser"},"spec":{"protocol":"Browser"}}`))
		}))
		defer server.Close()
		restore := configureGetTest(server)
		defer restore()

		recorder := httptest.NewRecorder()
		handleStudioToolSchema(recorder, studioRequest(http.MethodGet, "/api/tools/browser/schema", "browser", ""))
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"source":"none"`) || !strings.Contains(recorder.Body.String(), "not supported") {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
	})
}

func TestSchemaIntrospection_RejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name     string
		protocol v1alpha1.ToolProtocol
		status   int
		body     string
		want     string
	}{
		{name: "MCP HTTP error", protocol: v1alpha1.ToolProtocolMCP, status: http.StatusBadGateway, body: "offline", want: "MCP server returned 502"},
		{name: "MCP malformed JSON", protocol: v1alpha1.ToolProtocolMCP, status: http.StatusOK, body: "{", want: "decode tools/list"},
		{name: "MCP JSON-RPC error", protocol: v1alpha1.ToolProtocolMCP, status: http.StatusOK, body: `{"error":{"code":-32601,"message":"missing"}}`, want: "MCP error -32601"},
		{name: "MCP empty list", protocol: v1alpha1.ToolProtocolMCP, status: http.StatusOK, body: `{"result":{"tools":[]}}`, want: "returned no tools"},
		{name: "OpenAPI HTTP error", protocol: v1alpha1.ToolProtocolOpenAPI, status: http.StatusUnauthorized, body: "denied", want: "OpenAPI GET returned 401"},
		{name: "OpenAPI malformed JSON", protocol: v1alpha1.ToolProtocolOpenAPI, status: http.StatusOK, body: "{", want: "decode schema response"},
		{name: "OpenAPI missing shape", protocol: v1alpha1.ToolProtocolOpenAPI, status: http.StatusOK, body: `{}`, want: "no OpenAPI paths"},
		{name: "OpenAPI missing request schema", protocol: v1alpha1.ToolProtocolOpenAPI, status: http.StatusOK, body: `{"openapi":"3.0.0","paths":{"/test":{"post":{}}}}`, want: "no matching operation"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			resetClusterProxyClient(t)
			tool := v1alpha1.Tool{Spec: v1alpha1.ToolSpec{Protocol: test.protocol, Endpoint: server.URL + "/test"}}

			var err error
			if test.protocol == v1alpha1.ToolProtocolMCP {
				_, _, _, err = introspectMCPSchema(context.Background(), tool)
			} else {
				_, _, _, err = introspectOpenAPISchema(context.Background(), tool)
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}

	for _, protocol := range []v1alpha1.ToolProtocol{v1alpha1.ToolProtocolMCP, v1alpha1.ToolProtocolOpenAPI} {
		t.Run(string(protocol)+" empty endpoint", func(t *testing.T) {
			tool := v1alpha1.Tool{Spec: v1alpha1.ToolSpec{Protocol: protocol}}
			var err error
			if protocol == v1alpha1.ToolProtocolMCP {
				_, _, _, err = introspectMCPSchema(context.Background(), tool)
			} else {
				_, _, _, err = introspectOpenAPISchema(context.Background(), tool)
			}
			if err == nil || !strings.Contains(err.Error(), "endpoint is empty") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestExtractOpenAPIRequestSchema_RejectsIncompleteDocuments(t *testing.T) {
	documents := []map[string]any{
		{},
		{"paths": map[string]any{"/test": "invalid"}},
		{"paths": map[string]any{"/test": map[string]any{"parameters": []any{}}}},
	}
	for i, document := range documents {
		schema, description := extractOpenAPIRequestSchema(document, "https://example.test/test")
		if schema != nil || description != "" {
			t.Errorf("document %d returned schema %#v, description %q", i, schema, description)
		}
	}
}

func resetClusterProxyClient(t *testing.T) {
	t.Helper()
	clusterProxyOnce = sync.Once{}
	clusterProxyClient = nil
	t.Cleanup(func() {
		clusterProxyOnce = sync.Once{}
		clusterProxyClient = nil
	})
}

func studioRequest(method, target, name, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.SetPathValue("name", name)
	return req
}
