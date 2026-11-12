// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package dispatcher

import (
	"context"
	"fmt"
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
	"github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"go.uber.org/zap"
)

func TestAutoGenProductionDispatcherRealSDK(test *testing.T) {
	image := os.Getenv("KOGNITION_AUTOGEN_TEST_IMAGE")
	if image == "" {
		test.Skip("real SDK image required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	config := externalTestConfig(test)
	var calls atomic.Int32
	provider := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		claims, err := config.ModelSessionCapabilityIssuer.VerifyClaims(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
		if err != nil || claims.InstanceUID != "instance-uid" || claims.AgentName != "agent" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"fixture","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"production SDK answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
	}))
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		test.Fatal(err)
	}
	provider.Listener = listener
	provider.Start()
	defer provider.Close()
	modelURL := fmt.Sprintf("http://host.docker.internal:%d", listener.Addr().(*net.TCPAddr).Port)
	config.ModelGatewayURL = modelURL
	config.ToolGatewayURL, config.MemoryPolicyGatewayURL = modelURL, modelURL
	config.AgentCommunicationGatewayURL = modelURL
	var issuerErr error
	config.AgentDelegationSessionCapabilityIssuer, issuerErr = runtimeauth.NewAgentDelegationSessionCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), 5*time.Minute)
	if issuerErr != nil {
		test.Fatal(issuerErr)
	}
	name := "kognition-runtime-factory-" + uuid.NewString()
	arguments := []string{"run", "--detach", "--rm", "--name", name, "--add-host=host.docker.internal:host-gateway", "--publish", "127.0.0.1::8080"}
	for key, value := range map[string]string{
		"KOGNITION_EXTERNAL_RUNTIME": "true", "KOGNITION_RUNTIME_CLASS": "autogen", "KOGNITION_RUNTIME_AUTH_TOKEN": config.RuntimeAuthToken,
		"KOGNITION_NAMESPACE": "tenant", "KOGNITION_AGENT_NAME": "agent", "KOGNITION_MODEL_PROVIDER": "openai-compatible", "KOGNITION_MODEL_ID": "gpt-4o",
		"KOGNITION_MODEL_GATEWAY_URL": modelURL, "KOGNITION_TOOL_GATEWAY_URL": modelURL, "KOGNITION_MEMORY_SERVICE_URL": modelURL,
		"KOGNITION_AGENT_COMMUNICATION_GATEWAY_URL": modelURL, "KOGNITION_MEMORY_ENABLED": "false",
	} {
		arguments = append(arguments, "--env", key+"="+value)
	}
	arguments = append(arguments, image)
	if output, err := exec.CommandContext(ctx, "docker", arguments...).CombinedOutput(); err != nil {
		test.Fatalf("worker fixture: %v: %s", err, output)
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
	client := &http.Client{Timeout: time.Second}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/readyz", nil)
		request.Header.Set("Authorization", "Bearer "+config.RuntimeAuthToken)
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-ctx.Done():
			test.Fatal("SDK worker not ready")
		case <-ticker.C:
		}
	}
	storage := &stubStore{data: map[string][]byte{
		"Agent/tenant/agent": resourceBytes(test, v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent", Namespace: "tenant", UID: "agent-uid"}, Spec: v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "model"}, Runtime: &v1alpha1.AgentRuntimeSpec{Class: "autogen"}}}),
		"Model/tenant/model": resourceBytes(test, v1alpha1.Model{ObjectMeta: v1alpha1.ObjectMeta{Name: "model", Namespace: "tenant", ResourceVersion: 1}, Spec: v1alpha1.ModelSpec{Provider: "openai-compatible", ModelID: "gpt-4o"}}),
	}}
	dispatcher, err := NewExternalDispatcher(storage, zap.NewNop(), config, map[string]string{"tenant/agent": endpoint + "/invoke"})
	if err != nil {
		test.Fatal(err)
	}
	configureTestRuntimeRecovery(test, dispatcher)
	instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "instance", Namespace: "tenant", UID: "instance-uid"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}, SessionContext: map[string]string{"input": "real production turn", intctrl.ModelCapabilityIdentityNameContextKey: "identity", intctrl.AgentDelegationCapabilityIdentityNameContextKey: "identity"}}}
	instance.Status.SessionID, instance.Status.Phase = dispatcher.sessionID(instance), v1alpha1.PhaseRunning
	storage.data["AgentInstance/tenant/instance"] = resourceBytes(test, instance)
	instance.Status.Phase = v1alpha1.PhasePending
	if _, err := dispatcher.Dispatch(ctx, instance); err != nil {
		test.Fatal(err)
	}
	for {
		status, err := dispatcher.Status(ctx, instance)
		if err != nil {
			test.Fatal(err)
		}
		if status.Completed {
			if status.Output != "production SDK answer" || status.TokenUsage == nil || status.TokenUsage.TotalTokens != 18 || calls.Load() != 1 {
				test.Fatalf("invalid production completion: %+v calls=%d", status, calls.Load())
			}
			break
		}
		select {
		case <-ctx.Done():
			test.Fatal("production contract turn not completed")
		case <-ticker.C:
		}
	}
	if _, err := dispatcher.Dispatch(ctx, instance); err != nil || calls.Load() != 1 {
		test.Fatal("retry repeated paid SDK inference")
	}
	if err := dispatcher.Terminate(ctx, instance, "fixture cleanup"); err != nil {
		test.Fatal(err)
	}
	restarted, err := NewExternalDispatcher(storage, zap.NewNop(), config, map[string]string{"tenant/agent": endpoint + "/invoke"})
	if err != nil {
		test.Fatal(err)
	}
	if err := restarted.SetRuntimeRecovery(dispatcher.recoveryStore, dispatcher.recoveryCipher, 7*24*time.Hour); err != nil {
		test.Fatal(err)
	}
	if err := exec.CommandContext(ctx, "docker", "rm", "--force", name).Run(); err != nil {
		test.Fatal(err)
	}
	recovered, err := restarted.Status(ctx, instance)
	if err != nil || !recovered.Completed || recovered.Output != "production SDK answer" || recovered.TokenUsage == nil || recovered.TokenUsage.TotalTokens != 18 || calls.Load() != 1 {
		test.Fatalf("real SDK controller/worker-loss recovery=%+v err=%v calls=%d", recovered, err, calls.Load())
	}
}
