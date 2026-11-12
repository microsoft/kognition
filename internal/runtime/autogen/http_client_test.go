// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package autogen_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/autogen"
)

func bridgeConfiguration() string {
	return `{"KOGNITION_RUNTIME_CLASS":"autogen","KOGNITION_AGENT_NAME":"worker","KOGNITION_NAMESPACE":"tenant","KOGNITION_MODEL_GATEWAY_URL":"http://model:8093"}`
}

func TestHTTPAgentClientBoundLifecycle(test *testing.T) {
	createdID := "70000000-0000-4000-8000-000000000001"
	restoredID := "70000000-0000-4000-8000-000000000002"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer runtime-control" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/sessions":
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["namespace"] != "tenant" || body["agent_name"] != "worker" || body["runtime_configuration"] != bridgeConfiguration() {
				test.Error("session was not bound to trusted configuration")
			}
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]string{"session_id": createdID})
		case "/sessions/" + createdID + "/step":
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["model_session_capability"] != "model-turn" || body["turn_id"] == "" || body["endpoint"] != "" || body["api_key"] != "" {
				test.Error("turn forwarded untrusted metadata or omitted its capability")
			}
			writer.WriteHeader(http.StatusAccepted)
		case "/sessions/" + createdID + "/status":
			_, _ = writer.Write([]byte(`{"phase":"Completed","output":"answer","tool_calls_detail":[{"name":"registered_tool","call_id":"worker-call","args":{"query":"value"},"result":"result","error":null}]}`))
		case "/sessions/" + createdID + "/checkpoint":
			_, _ = writer.Write([]byte(`{"version":1,"agent_name":"worker","namespace":"tenant","sdk_state":{"type":"AssistantAgentState"}}`))
		case "/sessions/restore":
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]string{"session_id": restoredID})
		case "/sessions/" + createdID + "/cancel":
			_, _ = writer.Write([]byte(`{"accepted":true}`))
		case "/readyz":
			_, _ = writer.Write([]byte(`{"status":"ok"}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := autogen.NewHTTPAgentClient(autogen.HTTPClientConfig{
		Endpoint: server.URL, AgentName: "worker", Namespace: "tenant", RuntimeConfiguration: bridgeConfiguration(), AuthToken: "runtime-control",
		Capabilities: func(context.Context, string) (map[string]string, error) {
			return map[string]string{"model_session_capability": "model-turn", "endpoint": "https://untrusted", "api_key": "must-not-forward"}, nil
		},
	})
	if err != nil {
		test.Fatal(err)
	}
	adapter := autogen.New(client)
	session, err := adapter.CreateSession(context.Background(), runtime.CreateSessionRequest{AgentName: "worker", Namespace: "tenant", Context: map[string]string{"model_endpoint": "https://untrusted"}})
	if err != nil {
		test.Fatal(err)
	}
	stream, err := adapter.Step(context.Background(), runtime.StepRequest{SessionID: session.SessionID, Input: "question"})
	if err != nil {
		test.Fatal(err)
	}
	var events []runtime.StepEvent
	for event := range stream.Events {
		events = append(events, event)
	}
	if len(events) != 4 || events[0].ToolCall == nil || events[1].ToolResult == nil || events[0].ToolCall.CallID != events[1].ToolResult.CallID || events[2].Output == nil || events[2].Output.Content != "answer" {
		test.Fatalf("HTTP event translation failed: %+v", events)
	}
	checkpoint, err := adapter.Checkpoint(context.Background(), session.SessionID)
	if err != nil || strings.Contains(string(checkpoint.Token), "runtime-control") || strings.Contains(string(checkpoint.Token), "model-turn") {
		test.Fatal("checkpoint failed or persisted credentials")
	}
	resumed, err := adapter.Resume(context.Background(), checkpoint.Token)
	if err != nil || resumed.SessionID != restoredID {
		test.Fatalf("resume=%+v error=%v", resumed, err)
	}
	if err := client.Health(context.Background()); err != nil {
		test.Fatal(err)
	}
	if _, err := adapter.Cancel(context.Background(), runtime.CancelRequest{SessionID: session.SessionID}); err != nil {
		test.Fatal(err)
	}
	if _, err := client.CreateAgent(context.Background(), autogen.CreateAgentRequest{AgentName: "other", Namespace: "tenant"}); err == nil {
		test.Fatal("accepted another Agent principal")
	}
	if _, err := client.GetAgentState(context.Background(), "unknown"); err == nil {
		test.Fatal("accepted an unowned session")
	}
}

func TestHTTPAgentClientRejectsUnsafeEndpointAndRedirect(test *testing.T) {
	configuration := autogen.HTTPClientConfig{AgentName: "worker", Namespace: "tenant", RuntimeConfiguration: bridgeConfiguration(), AuthToken: "runtime-control", Capabilities: func(context.Context, string) (map[string]string, error) { return nil, nil }}
	for _, endpoint := range []string{"http://localhost:8080", "http://remote:8080", "https://user:password@worker", "https://worker?override=1", "https://worker/path"} {
		configuration.Endpoint = endpoint
		if _, err := autogen.NewHTTPAgentClient(configuration); err == nil {
			test.Fatalf("accepted unsafe endpoint %s", endpoint)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "https://untrusted.example/", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	configuration.Endpoint = server.URL
	client, err := autogen.NewHTTPAgentClient(configuration)
	if err != nil {
		test.Fatal(err)
	}
	if err := client.Health(context.Background()); err == nil {
		test.Fatal("followed a worker redirect")
	}
}
