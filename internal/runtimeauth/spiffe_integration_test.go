// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package runtimeauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/autogen"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
)

func TestSPIREWorkloadIdentityAcceptance(test *testing.T) {
	address := os.Getenv("KOGNITION_SPIRE_TEST_SOCKET")
	if address == "" {
		test.Skip("isolated SPIRE Workload API fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	source, err := NewWorkloadIdentity(ctx, WorkloadIdentityConfig{SocketAddress: address, ExpectedID: testIncarnationID, TrustDomain: "kognition.dev"})
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	if os.Getenv("KOGNITION_SPIRE_TEST_GATEWAY") == "true" {
		gatewayID := "spiffe://kognition.dev/gateway/model"
		gatewaySource, err := NewWorkloadIdentity(ctx, WorkloadIdentityConfig{SocketAddress: address, ExpectedID: gatewayID, TrustDomain: "kognition.dev"})
		if err != nil {
			test.Fatal(err)
		}
		defer func() { _ = gatewaySource.Close() }()
		certificate, err := gatewaySource.GetX509SVID()
		if err != nil {
			test.Fatal(err)
		}
		var requests atomic.Int32
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Header.Get("Authorization") != "Bearer fixture-scoped-capability" || request.URL.Path != "/v1/chat/completions" {
				test.Error("real SPIRE proxy changed capability or operation")
			}
			requests.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"id":"fixture","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"verified-real-spire"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
		}))
		server.TLS = tlsconfig.MTLSServerConfig(gatewaySource, gatewaySource, tlsconfig.AuthorizeID(source.expected))
		server.TLS.Certificates = []tls.Certificate{{Certificate: [][]byte{certificate.Certificates[0].Raw}, PrivateKey: certificate.PrivateKey}}
		server.StartTLS()
		defer server.Close()
		endpoints := map[string]WorkloadGatewayEndpoint{}
		for plane := range workloadOperations {
			endpoints[plane] = WorkloadGatewayEndpoint{URL: server.URL, PeerID: gatewayID}
		}
		proxy, err := NewWorkloadProxy(source, endpoints)
		if err != nil {
			test.Fatal(err)
		}
		defer proxy.Close()
		request := httptest.NewRequest(http.MethodPost, "/model/v1/chat/completions", strings.NewReader(`{}`))
		request.Header.Set("Authorization", "Bearer fixture-scoped-capability")
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "verified-real-spire") {
			test.Fatal("workload proxy did not traverse real SPIRE mTLS")
		}
		if os.Getenv("KOGNITION_SPIRE_TEST_AUTOGEN") == "true" {
			proxyServer := httptest.NewServer(proxy)
			defer proxyServer.Close()
			runSPIREAutoGenWorker(test, ctx, proxyServer.URL)
			if requests.Load() != 2 {
				test.Fatal("actual SDK turn did not traverse the real SPIRE gateway exactly once")
			}
		}
	}
	initial := source.Status()
	if !initial.Ready || initial.ExpiresAt.Sub(initial.NotBefore) > 2*time.Minute {
		test.Fatalf("real issuer identity not ready/bounded: %+v", initial)
	}
	first, err := source.GetX509SVID()
	if err != nil {
		test.Fatal(err)
	}
	serial := first.Certificates[0].SerialNumber.String()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := source.GetX509SVID()
		if err == nil && current.Certificates[0].SerialNumber.String() != serial && source.Status().Generation > initial.Generation {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			test.Fatal("real SPIRE workload certificate did not rotate")
		}
	}
	wrong, err := NewWorkloadIdentity(ctx, WorkloadIdentityConfig{SocketAddress: address, ExpectedID: "spiffe://kognition.dev/workspace/tenant/agent/worker/incarnation/70000000-0000-4000-8000-000000000002", TrustDomain: "kognition.dev"})
	if err == nil {
		_ = wrong.Close()
		test.Fatal("real SPIRE identity accepted another Agent incarnation")
	}
	if err := source.Close(); err != nil || source.Status().Ready {
		test.Fatal("closed real SPIRE source remained ready")
	}
}

func runSPIREAutoGenWorker(test *testing.T, ctx context.Context, proxyURL string) {
	test.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		test.Fatal(err)
	}
	address := listener.Addr().String()
	_, port, err := net.SplitHostPort(address)
	_ = listener.Close()
	if err != nil {
		test.Fatal(err)
	}
	configuration := map[string]string{
		"KOGNITION_RUNTIME_CLASS": "autogen", "KOGNITION_AGENT_NAME": "worker", "KOGNITION_NAMESPACE": "tenant",
		"KOGNITION_MODEL_PROVIDER": "openai-compatible", "KOGNITION_MODEL_ID": "gpt-4o",
		"KOGNITION_WORKLOAD_IDENTITY_MODE": "spire", "KOGNITION_WORKLOAD_PROXY_URL": proxyURL,
		"KOGNITION_MODEL_GATEWAY_URL": proxyURL + "/model", "KOGNITION_TOOL_GATEWAY_URL": proxyURL + "/tool",
		"KOGNITION_MEMORY_SERVICE_URL": proxyURL + "/memory", "KOGNITION_AGENT_COMMUNICATION_GATEWAY_URL": proxyURL + "/communication",
		"KOGNITION_MEMORY_ENABLED": "false",
	}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		test.Fatal(err)
	}
	controlToken := uuid.NewString()
	workerContext, stop := context.WithCancel(ctx)
	defer stop()
	worker := exec.CommandContext(workerContext, "python", "/app/main.py")
	worker.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/tmp", "PYTHONUNBUFFERED=1", "KOGNITION_EXTERNAL_RUNTIME=true", "KOGNITION_RUNTIME_AUTH_TOKEN=" + controlToken, "KOGNITION_PORT=" + port}
	for key, value := range configuration {
		worker.Env = append(worker.Env, key+"="+value)
	}
	if err := worker.Start(); err != nil {
		test.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- worker.Wait() }()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			test.Error("fixture worker did not terminate")
		}
	}()
	client, err := autogen.NewHTTPAgentClient(autogen.HTTPClientConfig{
		Endpoint: "http://" + address, AgentName: "worker", Namespace: "tenant", RuntimeConfiguration: string(encoded), AuthToken: controlToken,
		Capabilities: func(context.Context, string) (map[string]string, error) {
			return map[string]string{"model_session_capability": "fixture-scoped-capability"}, nil
		},
	})
	if err != nil {
		test.Fatal(err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for client.Health(ctx) != nil {
		select {
		case <-ctx.Done():
			test.Fatal("SPIRE SDK worker did not become ready")
		case <-ticker.C:
		}
	}
	adapter := autogen.New(client)
	session, err := adapter.CreateSession(ctx, runtime.CreateSessionRequest{AgentName: "worker", Namespace: "tenant", InitialCredits: 16})
	if err != nil {
		test.Fatal(err)
	}
	stream, err := adapter.Step(ctx, runtime.StepRequest{SessionID: session.SessionID, Input: "verify workload identity transport"})
	if err != nil {
		test.Fatal(err)
	}
	defer stream.Close()
	found := false
	for event := range stream.Events {
		if event.Error != nil {
			test.Fatal("SPIRE SDK worker emitted a runtime error")
		}
		if event.Output != nil && event.Output.Content == "verified-real-spire" {
			found = true
		}
	}
	if !found {
		test.Fatal("actual SDK output did not traverse the loopback SPIRE transport")
	}
}
