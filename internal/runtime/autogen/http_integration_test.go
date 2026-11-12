// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package autogen_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kognition-ai/kognition/internal/conformance"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/autogen"
)

func TestHTTPAgentClientRealSDKContainer(test *testing.T) {
	image := os.Getenv("KOGNITION_AUTOGEN_TEST_IMAGE")
	if image == "" {
		test.Skip("KOGNITION_AUTOGEN_TEST_IMAGE required for real worker acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var mu sync.Mutex
	var histories []map[string]interface{}
	modelToken := uuid.NewString()
	provider := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+modelToken {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		var input map[string]interface{}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		histories = append(histories, input)
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"fixture","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"real container answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
	}))
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		test.Fatal(err)
	}
	provider.Listener = listener
	provider.Start()
	defer provider.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	modelURL := fmt.Sprintf("http://host.docker.internal:%d", port)
	configuration := map[string]string{
		"KOGNITION_RUNTIME_CLASS": "autogen", "KOGNITION_AGENT_NAME": "worker", "KOGNITION_NAMESPACE": "tenant",
		"KOGNITION_MODEL_PROVIDER": "openai-compatible", "KOGNITION_MODEL_ID": "gpt-4o",
		"KOGNITION_MODEL_GATEWAY_URL": modelURL, "KOGNITION_TOOL_GATEWAY_URL": modelURL,
		"KOGNITION_MEMORY_SERVICE_URL": modelURL, "KOGNITION_AGENT_COMMUNICATION_GATEWAY_URL": modelURL,
		"KOGNITION_MEMORY_ENABLED": "false",
	}
	configurationBytes, err := json.Marshal(configuration)
	if err != nil {
		test.Fatal(err)
	}
	name := "kognition-backlog-autogen-" + uuid.NewString()
	controlToken := uuid.NewString()
	arguments := []string{"run", "--detach", "--rm", "--name", name, "--add-host=host.docker.internal:host-gateway", "--publish", "127.0.0.1::8080", "--env", "KOGNITION_EXTERNAL_RUNTIME=true", "--env", "KOGNITION_RUNTIME_AUTH_TOKEN=" + controlToken}
	for key, value := range configuration {
		arguments = append(arguments, "--env", key+"="+value)
	}
	arguments = append(arguments, image)
	if output, err := exec.CommandContext(ctx, "docker", arguments...).CombinedOutput(); err != nil {
		test.Fatalf("start fixture worker: %v: %s", err, output)
	}
	test.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "docker", "rm", "--force", name).Run()
	})
	output, err := exec.CommandContext(ctx, "docker", "port", name, "8080/tcp").Output()
	if err != nil {
		test.Fatal(err)
	}
	endpoint := "http://" + strings.TrimSpace(string(output))
	client, err := autogen.NewHTTPAgentClient(autogen.HTTPClientConfig{
		Endpoint: endpoint, AgentName: "worker", Namespace: "tenant", RuntimeConfiguration: string(configurationBytes), AuthToken: controlToken,
		Capabilities: func(context.Context, string) (map[string]string, error) {
			return map[string]string{"model_session_capability": modelToken}, nil
		},
	})
	if err != nil {
		test.Fatal(err)
	}
	for client.Health(ctx) != nil {
		select {
		case <-ctx.Done():
			test.Fatal("real SDK worker did not become ready")
		case <-time.After(100 * time.Millisecond):
		}
	}
	adapter := autogen.New(client)
	session, err := adapter.CreateSession(ctx, runtime.CreateSessionRequest{AgentName: "worker", Namespace: "tenant", InitialCredits: 16})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := adapter.CreateSession(ctx, runtime.CreateSessionRequest{AgentName: "worker", Namespace: "other-tenant"}); err == nil {
		test.Fatal("bound worker accepted a foreign namespace")
	}
	emptyCheckpoint, err := adapter.Checkpoint(ctx, session.SessionID)
	if err != nil {
		test.Fatal(err)
	}
	emptyResumed, err := adapter.Resume(ctx, emptyCheckpoint.Token)
	if err != nil {
		test.Fatal(err)
	}
	step := func(sessionID, question string) {
		test.Helper()
		stream, err := adapter.Step(ctx, runtime.StepRequest{SessionID: sessionID, Input: question})
		if err != nil {
			test.Fatal(err)
		}
		defer stream.Close()
		found := false
		for event := range stream.Events {
			if event.Error != nil {
				test.Fatalf("real worker emitted error: %+v", event.Error)
			}
			if event.Output != nil && event.Output.Content == "real container answer" {
				found = true
			}
		}
		if !found {
			test.Fatal("real SDK worker did not return its model response")
		}
	}
	step(emptyResumed.SessionID, "original container turn")
	checkpoint, err := adapter.Checkpoint(ctx, emptyResumed.SessionID)
	if err != nil {
		test.Fatal(err)
	}
	if strings.Contains(string(checkpoint.Token), controlToken) || strings.Contains(string(checkpoint.Token), modelToken) {
		test.Fatal("real worker checkpoint retained credentials")
	}
	resumed, err := adapter.Resume(ctx, checkpoint.Token)
	if err != nil {
		test.Fatal(err)
	}
	step(resumed.SessionID, "restored container turn")
	mu.Lock()
	providerCalls := len(histories)
	var restored []byte
	if providerCalls == 2 {
		restored, err = json.Marshal(histories[1])
	}
	mu.Unlock()
	if providerCalls != 2 {
		test.Fatalf("provider calls=%d, expected two actual SDK turns", providerCalls)
	}
	if err != nil || !strings.Contains(string(restored), "original container turn") || !strings.Contains(string(restored), "restored container turn") {
		test.Fatal("fresh HTTP session did not restore actual SDK history")
	}
	test.Run("BoundPrincipalConformance", func(test *testing.T) {
		conformance.RunBoundRuntimeSuite(test, func(*testing.T) runtime.Runtime {
			return autogen.New(client)
		}, "worker", "tenant")
	})
}
