// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dispatcher

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	k8scost "github.com/kognition-ai/kognition/internal/k8s/cost"
	"github.com/kognition-ai/kognition/internal/runtime/recovery"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func externalTestConfig(t *testing.T) Config {
	t.Helper()
	issuer, err := runtimeauth.NewModelSessionCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return Config{RuntimeAuthToken: "test-runtime-token", ModelGatewayURL: "http://model-gateway.test", ModelSessionCapabilityIssuer: issuer}
}

func configureTestRuntimeRecovery(test *testing.T, dispatcher *K8sDispatcher) {
	test.Helper()
	scheme := k8sruntime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		test.Fatal(err)
	}
	storage, err := k8scost.NewConfigMapStore(fake.NewClientBuilder().WithScheme(scheme).Build(), "control")
	if err != nil {
		test.Fatal(err)
	}
	configuration, _ := json.Marshal(map[string]recovery.WorkspaceKeys{"tenant": {Current: "test", Keys: map[string]string{"test": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))}}})
	crypto, err := recovery.NewCipher(configuration)
	if err != nil {
		test.Fatal(err)
	}
	if err := dispatcher.SetRuntimeRecovery(storage, crypto, 7*24*time.Hour); err != nil {
		test.Fatal(err)
	}
}

func TestExternalDispatcherValidatesConfiguration(t *testing.T) {
	for _, endpoint := range []string{"/invoke", "ftp://runtime/invoke", "http://user:password@runtime/invoke", "http://runtime/status", "http://runtime/invoke?token=value", "http://runtime/invoke#fragment", "http://192.0.2.10/invoke", "http://localhost/invoke"} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := NewExternalDispatcher(&stubStore{}, zap.NewNop(), externalTestConfig(t), map[string]string{"tenant/agent": endpoint}); err == nil {
				t.Fatal("accepted invalid endpoint")
			}
		})
	}
	config := externalTestConfig(t)
	config.RuntimeAuthToken = ""
	if _, err := NewExternalDispatcher(&stubStore{}, zap.NewNop(), config, map[string]string{"tenant/agent": "http://runtime/invoke"}); err == nil {
		t.Fatal("accepted missing authentication")
	}
	config = externalTestConfig(t)
	config.ModelSessionCapabilityIssuer = nil
	if _, err := NewExternalDispatcher(&stubStore{}, zap.NewNop(), config, map[string]string{"tenant/agent": "http://runtime/invoke"}); err == nil {
		t.Fatal("accepted missing issuer")
	}
	if _, err := NewExternalDispatcher(&stubStore{}, zap.NewNop(), externalTestConfig(t), map[string]string{"tenant/one": "http://runtime/invoke", "tenant/two": "http://runtime/invoke"}); err == nil {
		t.Fatal("accepted shared runtime")
	}
	for _, endpoint := range []string{"http://127.0.0.1/invoke", "http://[::1]/invoke", "https://runtime.example/invoke"} {
		if _, err := NewExternalDispatcher(&stubStore{}, zap.NewNop(), externalTestConfig(t), map[string]string{"tenant/agent": endpoint}); err != nil {
			t.Fatalf("rejected secure development endpoint %q: %v", endpoint, err)
		}
	}
}

func TestExternalDispatcherRejectsUnmappedPinnedAndMissingAgents(t *testing.T) {
	dispatcher, err := NewExternalDispatcher(&stubStore{}, zap.NewNop(), externalTestConfig(t), map[string]string{"tenant/agent": "https://runtime.example/invoke"})
	if err != nil {
		t.Fatal(err)
	}
	instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "missing"}}}
	if _, err := dispatcher.Dispatch(context.Background(), instance); err == nil {
		t.Fatal("dispatched unmapped Agent")
	}
	if err := dispatcher.Terminate(context.Background(), instance, "cancel"); err == nil {
		t.Fatal("cancelled unmapped Agent")
	}
	status, err := dispatcher.Status(context.Background(), instance)
	if err == nil || status.Alive {
		t.Fatal("unmapped Agent reported alive")
	}
	instance.Spec.AgentRef.Name = "agent"
	if _, err := dispatcher.Dispatch(context.Background(), instance); err == nil {
		t.Fatal("dispatched missing Agent")
	}
	revision := int64(1)
	instance.Spec.AgentRevision = &revision
	if _, err := dispatcher.Dispatch(context.Background(), instance); err == nil {
		t.Fatal("dispatched pinned Agent")
	}
}

func TestExternalDispatcherRejectsUnsupportedConfigurationBeforeDelivery(t *testing.T) {
	for _, testCase := range []struct {
		spec    v1alpha1.AgentSpec
		storage *stubStore
	}{
		{spec: v1alpha1.AgentSpec{Runtime: &v1alpha1.AgentRuntimeSpec{Class: "custom"}}, storage: &stubStore{}},
		{
			spec: v1alpha1.AgentSpec{KnowledgeBaseClaims: []v1alpha1.KnowledgeBaseClaimRef{{Name: "private-knowledge"}}},
			storage: &stubStore{data: map[string][]byte{
				"KnowledgeBaseClaim/tenant/private-knowledge": resourceBytes(t, v1alpha1.KnowledgeBaseClaim{Spec: v1alpha1.KnowledgeBaseClaimSpec{KnowledgeBaseName: "private-knowledge"}}),
				"KnowledgeBase/tenant/private-knowledge":      resourceBytes(t, v1alpha1.KnowledgeBase{ObjectMeta: v1alpha1.ObjectMeta{Name: "private-knowledge", Namespace: "tenant"}}),
			}},
		},
	} {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		}))
		dispatcher, err := NewExternalDispatcher(testCase.storage, zap.NewNop(), externalTestConfig(t), map[string]string{"tenant/agent": server.URL + "/invoke"})
		if err != nil {
			t.Fatal(err)
		}
		configuration, err := dispatcher.externalConfiguration(context.Background(), server.URL+"/invoke", "tenant", &v1alpha1.Agent{Spec: testCase.spec}, &v1alpha1.Model{})
		server.Close()
		if err == nil || configuration != "" || requests.Load() != 0 {
			t.Fatal("unsupported configuration reached external runtime")
		}
	}
}

func TestExternalDispatcherAuthenticatedLifecycle(t *testing.T) {
	invoked := make(chan map[string]string, 1)
	cancelled := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-runtime-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]string
		switch request.URL.Path {
		case "/configuration-check":
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["KOGNITION_NAMESPACE"] != "tenant" || body["KOGNITION_AGENT_NAME"] != "agent" {
				writer.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			writer.WriteHeader(http.StatusNoContent)
		case "/invoke":
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			invoked <- body
			writer.WriteHeader(http.StatusAccepted)
		case "/status":
			_ = json.NewEncoder(writer).Encode(map[string]any{"phase": "Completed", "session_id": request.URL.Query().Get("session_id"), "agent_name": "agent", "output": "done", "token_usage": map[string]int{"total_tokens": 7}})
		case "/cancel":
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			cancelled <- body["session_id"]
			_ = json.NewEncoder(writer).Encode(map[string]any{"accepted": true, "session_id": body["session_id"]})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "instance", Namespace: "tenant", UID: "instance-uid"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}, SessionContext: map[string]string{"input": "hello", intctrl.ModelCapabilityIdentityNameContextKey: "identity", intctrl.MemoryCapabilityIdentityNameContextKey: "identity", intctrl.ToolCapabilityIdentityNameContextKey: "identity", intctrl.KnowledgeCapabilityIdentityNameContextKey: "identity", intctrl.AgentDelegationCapabilityIdentityNameContextKey: "identity"}}}
	storage := &stubStore{data: map[string][]byte{
		"Agent/tenant/agent":               resourceBytes(t, v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent", Namespace: "tenant", UID: "agent-uid"}, Spec: v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "model"}, Tools: []v1alpha1.AgentToolRef{{Ref: v1alpha1.LocalObjectReference{Name: "tool"}}}, Memory: &v1alpha1.AgentMemorySpec{Claims: []v1alpha1.LocalObjectReference{{Name: "notes"}}}, KnowledgeBaseClaims: []v1alpha1.KnowledgeBaseClaimRef{{Name: "docs"}}}}),
		"AgentIdentity/tenant/identity":    resourceBytes(t, v1alpha1.AgentIdentity{ObjectMeta: v1alpha1.ObjectMeta{Name: "identity", Namespace: "tenant", UID: "identity-uid"}, Spec: v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}}}),
		"Model/tenant/model":               resourceBytes(t, v1alpha1.Model{ObjectMeta: v1alpha1.ObjectMeta{Name: "model", ResourceVersion: 1}, Spec: v1alpha1.ModelSpec{Provider: "openai-compatible"}}),
		"MemoryClaim/tenant/notes":         resourceBytes(t, v1alpha1.MemoryClaim{Status: v1alpha1.MemoryClaimStatus{BoundVolumeName: "notes-volume"}}),
		"MemoryVolume/tenant/notes-volume": resourceBytes(t, v1alpha1.MemoryVolume{Spec: v1alpha1.MemoryVolumeSpec{BackendHandle: "notes-backend"}, Status: v1alpha1.MemoryVolumeStatus{Phase: v1alpha1.PhaseBound}}),
		"KnowledgeBaseClaim/tenant/docs": resourceBytes(t, v1alpha1.KnowledgeBaseClaim{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "docs", Namespace: "tenant", Generation: 1},
			Spec:       v1alpha1.KnowledgeBaseClaimSpec{KnowledgeBaseName: "docs"},
			Status:     v1alpha1.KnowledgeBaseClaimStatus{Phase: v1alpha1.PhaseBound, BoundKnowledgeBase: "docs", ObservedGeneration: 1},
		}),
		"KnowledgeBase/tenant/docs":    resourceBytes(t, v1alpha1.KnowledgeBase{ObjectMeta: v1alpha1.ObjectMeta{Name: "docs", Namespace: "tenant"}, Spec: v1alpha1.KnowledgeBaseSpec{KnowledgeBaseClassName: "provider"}}),
		"KnowledgeBaseClass//provider": resourceBytes(t, v1alpha1.KnowledgeBaseClass{Spec: v1alpha1.KnowledgeBaseClassSpec{BackendRef: &v1alpha1.KnowledgeBackendReference{APIGroup: "search.knowledge.kognition.dev", Kind: "AzureAISearchBackend", Name: "shared"}}}),
	}}
	config := externalTestConfig(t)
	key := []byte("0123456789abcdef0123456789abcdef")
	var err error
	config.MemoryCapabilityIssuer, err = runtimeauth.NewMemoryCapabilityIssuer(key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	config.ToolSessionCapabilityIssuer, err = runtimeauth.NewToolSessionCapabilityIssuer(key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	config.AgentDelegationSessionCapabilityIssuer, err = runtimeauth.NewAgentDelegationSessionCapabilityIssuer(key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	config.KnowledgeSessionCapabilityIssuer, err = runtimeauth.NewKnowledgeSessionCapabilityIssuer(key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	config.MemoryPolicyGatewayURL, config.ToolGatewayURL, config.AgentCommunicationGatewayURL = "http://memory.test", "http://tool.test", "http://delegation.test"
	config.KnowledgeGatewayURL = "http://knowledge.test"
	dispatcher, err := NewExternalDispatcher(storage, zap.NewNop(), config, map[string]string{"tenant/agent": server.URL + "/invoke"})
	if err != nil {
		t.Fatal(err)
	}
	if replicas, err := dispatcher.RuntimeReplicas(context.Background(), instance); err != nil || len(replicas) != 0 {
		t.Fatal("external runtime attempted Kubernetes replica discovery")
	}
	instance.Status.SessionID = dispatcher.sessionID(instance)
	instance.Status.Phase = v1alpha1.PhaseRunning
	storage.data["AgentInstance/tenant/instance"] = resourceBytes(t, instance)
	instance.Status.Phase = v1alpha1.PhasePending
	sessionID, err := dispatcher.Dispatch(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-invoked:
		if body["runtime_configuration"] == "" {
			t.Fatal("missing runtime configuration binding")
		}
		claims, err := dispatcher.config.ModelSessionCapabilityIssuer.VerifyClaims(body["model_session_capability"])
		if err != nil || claims.SessionID != sessionID || claims.InstanceUID != "instance-uid" {
			t.Fatal("incorrect delivered capability")
		}
		if _, err := config.MemoryCapabilityIssuer.Verify(body["memory_capability"], runtimeauth.MemoryCapabilityBinding{Namespace: "tenant", AgentName: "agent", AgentIdentityName: "identity", InstanceName: "instance", InstanceUID: "instance-uid", SessionID: sessionID, VolumeHandle: "notes-backend"}); err != nil {
			t.Fatal("incorrect Memory capability")
		}
		toolClaims, err := config.ToolSessionCapabilityIssuer.VerifyClaims(body["tool_session_capability"])
		if err != nil || toolClaims.SessionID != sessionID || toolClaims.InstanceUID != "instance-uid" {
			t.Fatal("incorrect Tool capability")
		}
		knowledgeClaims, err := config.KnowledgeSessionCapabilityIssuer.VerifyClaims(body["knowledge_session_capability"])
		if err != nil || knowledgeClaims.SessionID != sessionID || knowledgeClaims.InstanceUID != "instance-uid" || len(knowledgeClaims.KnowledgeBases) != 1 || knowledgeClaims.KnowledgeBases[0] != "docs" {
			t.Fatal("incorrect Knowledge capability")
		}
		delegationClaims, err := config.AgentDelegationSessionCapabilityIssuer.VerifyClaims(body["agent_delegation_session_capability"])
		if err != nil || delegationClaims.SessionID != sessionID || delegationClaims.InstanceUID != "instance-uid" {
			t.Fatal("incorrect delegation capability")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runtime was not invoked")
	}
	dispatcher.invocationsMu.Lock()
	control := dispatcher.invocations[sessionID]
	dispatcher.invocationsMu.Unlock()
	if control != nil {
		<-control.done
	}
	status, err := dispatcher.Status(context.Background(), instance)
	if err != nil || !status.Completed || status.Output != "done" || status.TokenUsage.TotalTokens != 7 {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if err := dispatcher.Terminate(context.Background(), instance, "test cancellation"); err != nil {
		t.Fatal(err)
	}
	if got := <-cancelled; got != sessionID {
		t.Fatal("cancelled incorrect session")
	}
}

func TestExternalDispatcherAutoGenSelectsRuntimeContract(t *testing.T) {
	var created atomic.Int32
	var legacyInvoked atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-runtime-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case request.URL.Path == "/configuration-check":
			writer.WriteHeader(http.StatusNoContent)
		case request.URL.Path == "/sessions":
			var binding map[string]string
			if err := json.NewDecoder(request.Body).Decode(&binding); err != nil || binding["agent_name"] != "agent" || binding["namespace"] != "tenant" || binding["instance_uid"] != "instance-uid" || binding["runtime_configuration"] == "" {
				t.Error("contract session lost controller ownership binding")
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			created.Add(1)
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]string{"session_id": binding["session_id"]})
		case request.URL.Path == "/invoke":
			legacyInvoked.Add(1)
			writer.WriteHeader(http.StatusAccepted)
		case strings.HasSuffix(request.URL.Path, "/checkpoint"):
			_, _ = writer.Write([]byte(`{"version":1,"sdk_state":{"type":"AssistantAgentState"}}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "instance", Namespace: "tenant", UID: "instance-uid"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}, SessionContext: map[string]string{"input": "work", intctrl.ModelCapabilityIdentityNameContextKey: "identity", intctrl.ToolCapabilityIdentityNameContextKey: "identity", intctrl.AgentDelegationCapabilityIdentityNameContextKey: "identity"}}}
	storage := &stubStore{data: map[string][]byte{
		"Agent/tenant/agent": resourceBytes(t, v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent", Namespace: "tenant", UID: "agent-uid"}, Spec: v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "model"}, Runtime: &v1alpha1.AgentRuntimeSpec{Class: "autogen"}}}),
		"Model/tenant/model": resourceBytes(t, v1alpha1.Model{ObjectMeta: v1alpha1.ObjectMeta{Name: "model", Namespace: "tenant", ResourceVersion: 1}, Spec: v1alpha1.ModelSpec{Provider: "openai-compatible", ModelID: "gpt-4o"}}),
	}}
	config := externalTestConfig(t)
	config.MemoryPolicyGatewayURL, config.ToolGatewayURL, config.AgentCommunicationGatewayURL = "http://memory.test", "http://tool.test", "http://delegation.test"
	key := []byte("0123456789abcdef0123456789abcdef")
	var err error
	config.ToolSessionCapabilityIssuer, err = runtimeauth.NewToolSessionCapabilityIssuer(key, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	config.AgentDelegationSessionCapabilityIssuer, err = runtimeauth.NewAgentDelegationSessionCapabilityIssuer(key, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewExternalDispatcher(storage, zap.NewNop(), config, map[string]string{"tenant/agent": server.URL + "/invoke"})
	if err != nil {
		t.Fatal(err)
	}
	configureTestRuntimeRecovery(t, dispatcher)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sessionID, err := dispatcher.Dispatch(ctx, instance)
	if err != nil {
		t.Fatal(err)
	}
	if sessionID == "" || created.Load() != 1 || legacyInvoked.Load() != 0 {
		t.Fatalf("autogen did not select Runtime contract: session=%q create=%d legacy=%d", sessionID, created.Load(), legacyInvoked.Load())
	}
}

func TestExternalDispatcherAutoGenOwnedLifecycle(t *testing.T) {
	created := make(chan struct{}, 1)
	stepped := make(chan map[string]string, 1)
	var turns atomic.Int32
	var phase atomic.Bool
	var cancelled atomic.Bool
	var ownerSession atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-runtime-token" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]string
		switch {
		case request.URL.Path == "/configuration-check":
			writer.WriteHeader(http.StatusNoContent)
		case request.URL.Path == "/sessions/restore":
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{"session_id": ownerSession.Load()})
		case request.URL.Path == "/sessions":
			if json.NewDecoder(request.Body).Decode(&body) != nil || body["instance_uid"] != "instance-uid" {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			ownerSession.Store(body["session_id"])
			created <- struct{}{}
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]string{"session_id": body["session_id"]})
		case strings.HasSuffix(request.URL.Path, "/step"):
			if json.NewDecoder(request.Body).Decode(&body) != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			turns.Add(1)
			stepped <- body
			phase.Store(true)
			writer.WriteHeader(http.StatusAccepted)
		case strings.HasSuffix(request.URL.Path, "/status"):
			value := "Ready"
			if phase.Load() {
				value = "Completed"
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"phase": value, "session_id": ownerSession.Load(), "agent_name": "agent", "instance_uid": "instance-uid", "output": "done", "token_usage": map[string]int{"prompt_tokens": 4, "completion_tokens": 3, "total_tokens": 7}})
		case strings.HasSuffix(request.URL.Path, "/cancel"):
			cancelled.Store(true)
			_ = json.NewEncoder(writer).Encode(map[string]bool{"accepted": true})
		case strings.HasSuffix(request.URL.Path, "/release"):
			cancelled.Store(true)
			_ = json.NewEncoder(writer).Encode(map[string]interface{}{"accepted": true, "session_id": ownerSession.Load()})
		case strings.HasSuffix(request.URL.Path, "/checkpoint"):
			_, _ = writer.Write([]byte(`{"version":1,"sdk_state":{"type":"AssistantAgentState"}}`))
		default:
			t.Error("AutoGen lifecycle used a legacy or unknown worker operation")
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "instance", Namespace: "tenant", UID: "instance-uid"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}, SessionContext: map[string]string{"input": "work", intctrl.ModelCapabilityIdentityNameContextKey: "identity", intctrl.AgentDelegationCapabilityIdentityNameContextKey: "identity"}}}
	storage := &stubStore{data: map[string][]byte{
		"Agent/tenant/agent": resourceBytes(t, v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent", Namespace: "tenant", UID: "agent-uid"}, Spec: v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "model"}, Runtime: &v1alpha1.AgentRuntimeSpec{Class: "autogen"}}}),
		"Model/tenant/model": resourceBytes(t, v1alpha1.Model{ObjectMeta: v1alpha1.ObjectMeta{Name: "model", Namespace: "tenant", ResourceVersion: 1}, Spec: v1alpha1.ModelSpec{Provider: "openai-compatible", ModelID: "gpt-4o"}}),
	}}
	config := externalTestConfig(t)
	config.MemoryPolicyGatewayURL, config.ToolGatewayURL, config.AgentCommunicationGatewayURL = "http://memory.test", "http://tool.test", "http://delegation.test"
	var err error
	config.AgentDelegationSessionCapabilityIssuer, err = runtimeauth.NewAgentDelegationSessionCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewExternalDispatcher(storage, zap.NewNop(), config, map[string]string{"tenant/agent": server.URL + "/invoke"})
	if err != nil {
		t.Fatal(err)
	}
	configureTestRuntimeRecovery(t, dispatcher)
	instance.Status.SessionID, instance.Status.Phase = dispatcher.sessionID(instance), v1alpha1.PhaseRunning
	storage.data["AgentInstance/tenant/instance"] = resourceBytes(t, instance)
	instance.Status.Phase = v1alpha1.PhasePending
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sessionID, err := dispatcher.Dispatch(ctx, instance)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-created:
	case <-ctx.Done():
		t.Fatal("contract session was not created")
	}
	select {
	case body := <-stepped:
		claims, err := config.ModelSessionCapabilityIssuer.VerifyClaims(body["model_session_capability"])
		if err != nil || claims.SessionID != sessionID || claims.InstanceUID != "instance-uid" || body["turn_id"] != sessionID || body["message"] != "work" {
			t.Fatal("contract turn lost immutable control-plane binding")
		}
	case <-ctx.Done():
		t.Fatal("contract session was not stepped")
	}
	var status intctrl.RuntimeStatus
	for {
		status, err = dispatcher.Status(ctx, instance)
		if err != nil || status.Completed {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("durable terminal completion did not arrive")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err != nil || !status.Completed || status.Output != "done" || status.TokenUsage == nil || status.TokenUsage.TotalTokens != 7 {
		t.Fatalf("contract completion=%+v error=%v", status, err)
	}
	if repeated, err := dispatcher.Dispatch(ctx, instance); err != nil || repeated != sessionID || turns.Load() != 1 {
		t.Fatal("dispatch retry repeated the governed turn")
	}
	if err := dispatcher.Terminate(ctx, instance, "test cleanup"); err != nil || !cancelled.Load() {
		t.Fatalf("contract cancellation=%v", err)
	}
	restarted, err := NewExternalDispatcher(storage, zap.NewNop(), config, map[string]string{"tenant/agent": server.URL + "/invoke"})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.SetRuntimeRecovery(dispatcher.recoveryStore, dispatcher.recoveryCipher, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.Status(ctx, instance)
	if err != nil || !recovered.Completed || recovered.Output != "done" || recovered.TokenUsage.TotalTokens != 7 || turns.Load() != 1 {
		t.Fatalf("durable controller restart recovery=%+v err=%v", recovered, err)
	}
	wrong := *instance
	wrong.ObjectMeta = instance.ObjectMeta
	wrong.ObjectMeta.UID = "other-incarnation"
	if status, err := restarted.Status(ctx, &wrong); !errors.Is(err, intctrl.ErrDispatchAccountingUncertain) || status.Completed {
		t.Fatal("wrong instance incarnation recovered encrypted state")
	}
	if _, err := dispatcher.recoveryStore.ExpireRuntimeRecovery(ctx, time.Now().Add(8*24*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	if status, err := restarted.Status(ctx, instance); !errors.Is(err, intctrl.ErrDispatchAccountingUncertain) || status.Completed {
		t.Fatal("retired ciphertext replayed terminal recovery")
	}
}

func TestRuntimeControlTLSUsesOnlyOperatorTrust(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writer.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	dispatcher, err := NewExternalDispatcher(&stubStore{}, zap.NewNop(), externalTestConfig(t), map[string]string{"tenant/agent": server.URL + "/invoke"})
	if err != nil {
		t.Fatal(err)
	}
	if response, err := dispatcher.http.Get(server.URL); err == nil {
		_ = response.Body.Close()
		t.Fatal("untrusted worker certificate was accepted")
	}
	path := filepath.Join(t.TempDir(), "operator-ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	dispatcher.config.RuntimeCAFile = path
	client, err := dispatcher.runtimeControlClient()
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	dispatcher.config.RuntimeCAFile = filepath.Join(t.TempDir(), "missing.pem")
	if _, err := dispatcher.runtimeControlClient(); err == nil {
		t.Fatal("missing operator trust silently fell back")
	}
}

func TestExternalDispatcherBindsRuntimeEngine(t *testing.T) {
	for _, class := range []string{"", "default", "native", "autogen"} {
		t.Run(class, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var configuration map[string]string
				if err := json.NewDecoder(request.Body).Decode(&configuration); err != nil {
					t.Error(err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				expected := ""
				if class == "autogen" {
					expected = class
				}
				if configuration["KOGNITION_RUNTIME_CLASS"] != expected || request.Header.Get("Authorization") != "Bearer test-runtime-token" {
					writer.WriteHeader(http.StatusPreconditionFailed)
					return
				}
				writer.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			dispatcher, err := NewExternalDispatcher(&stubStore{}, zap.NewNop(), externalTestConfig(t), map[string]string{"tenant/agent": server.URL + "/invoke"})
			if err != nil {
				t.Fatal(err)
			}
			agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent"}, Spec: v1alpha1.AgentSpec{Runtime: &v1alpha1.AgentRuntimeSpec{Class: class}}}
			if _, err := dispatcher.externalConfiguration(context.Background(), server.URL+"/invoke", "tenant", agent, &v1alpha1.Model{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExternalDispatcherStatusUncertainty(t *testing.T) {
	for _, scenario := range []struct {
		name, body string
		code       int
	}{
		{"missing session", `{}`, 404},
		{"foreign session", `{"phase":"Completed","agent_name":"agent","session_id":"foreign","output":"secret"}`, 200},
		{"foreign agent", `{"phase":"Completed","agent_name":"foreign","session_id":"session"}`, 200},
		{"unknown phase", `{"phase":"Ready","agent_name":"agent","session_id":"session"}`, 200},
		{"invalid JSON", `{`, 200},
		{"oversized", strings.Repeat(" ", (1<<20)+1), 200},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(scenario.code)
				_, _ = writer.Write([]byte(scenario.body))
			}))
			defer server.Close()
			dispatcher, err := NewExternalDispatcher(&stubStore{}, zap.NewNop(), externalTestConfig(t), map[string]string{"tenant/agent": server.URL + "/invoke"})
			if err != nil {
				t.Fatal(err)
			}
			instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}, SessionContext: map[string]string{"input": "work"}}, Status: v1alpha1.AgentInstanceStatus{SessionID: "session"}}
			status, err := dispatcher.Status(context.Background(), instance)
			if !errors.Is(err, intctrl.ErrDispatchAccountingUncertain) || status.Output != "" || status.Completed {
				t.Fatalf("status=%+v err=%v", status, err)
			}
		})
	}
}

func TestExternalDispatcherCancellationBeforeActivation(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	dispatcher, err := NewExternalDispatcher(&stubStore{}, zap.NewNop(), externalTestConfig(t), map[string]string{"tenant/agent": server.URL + "/invoke"})
	if err != nil {
		t.Fatal(err)
	}
	instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "instance", Namespace: "tenant"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}, SessionContext: map[string]string{"input": "work"}}, Status: v1alpha1.AgentInstanceStatus{SessionID: "session"}}
	dispatcher.invokeExternal(instance, server.URL+"/invoke", "session", "{}", "", "", "capability", "", "")
	status, err := dispatcher.Status(context.Background(), instance)
	if err != nil || !status.Alive || status.Completed {
		t.Fatalf("pending status=%+v err=%v", status, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := dispatcher.Terminate(ctx, instance, "cancel"); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("unactivated invocation reached runtime")
	}
	_, err = dispatcher.Status(ctx, instance)
	if !errors.Is(err, intctrl.ErrDispatchAccountingUncertain) {
		t.Fatalf("lost session status=%v", err)
	}
}

func TestExternalDispatcherDuplicateDispatchPreservesCancellation(t *testing.T) {
	for _, activated := range []bool{false, true} {
		name := "pending activation"
		if activated {
			name = "in-flight delivery"
		}
		t.Run(name, func(t *testing.T) {
			instance := &v1alpha1.AgentInstance{
				ObjectMeta: v1alpha1.ObjectMeta{Name: "instance", Namespace: "tenant", UID: "instance-uid"},
				Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}, SessionContext: map[string]string{
					"input": "work", intctrl.ModelCapabilityIdentityNameContextKey: "identity",
				}},
			}
			storage := &stubStore{data: map[string][]byte{
				"Agent/tenant/agent": resourceBytes(t, v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent", Namespace: "tenant"}, Spec: v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "model"}}}),
				"Model/tenant/model": resourceBytes(t, v1alpha1.Model{ObjectMeta: v1alpha1.ObjectMeta{Name: "model", ResourceVersion: 1}, Spec: v1alpha1.ModelSpec{Provider: "openai-compatible"}}),
			}}
			dispatcher, err := NewExternalDispatcher(storage, zap.NewNop(), externalTestConfig(t), map[string]string{"tenant/agent": "https://runtime.test/invoke"})
			if err != nil {
				t.Fatal(err)
			}
			instance.Status.SessionID = dispatcher.sessionID(instance)
			if activated {
				instance.Status.Phase = v1alpha1.PhaseRunning
				storage.data["AgentInstance/tenant/instance"] = resourceBytes(t, instance)
			}
			instance.Status.Phase = v1alpha1.PhasePending
			dispatcher.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/configuration-check" {
					return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"accepted":true,"session_id":"` + instance.Status.SessionID + `"}`))}, nil
			})
			entered := make(chan struct{}, 2)
			var attempts atomic.Int32
			dispatcher.invokeHTTP.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				attempts.Add(1)
				entered <- struct{}{}
				<-request.Context().Done()
				return nil, request.Context().Err()
			})
			sessionID, err := dispatcher.Dispatch(context.Background(), instance)
			if err != nil {
				t.Fatal(err)
			}
			dispatcher.invocationsMu.Lock()
			original := dispatcher.invocations[sessionID]
			dispatcher.invocationsMu.Unlock()
			t.Cleanup(func() {
				original.cancel()
				<-original.done
			})
			if activated {
				select {
				case <-entered:
				case <-time.After(2 * time.Second):
					t.Fatal("first delivery did not start")
				}
			}
			duplicate, err := dispatcher.Dispatch(context.Background(), instance)
			if err != nil || duplicate != sessionID {
				t.Fatalf("duplicate session=%q err=%v", duplicate, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := dispatcher.Terminate(ctx, instance, "cancel duplicate dispatch"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-original.done:
			default:
				t.Fatal("cancellation returned while the original delivery was still pending")
			}
			var expected int32
			if activated {
				expected = 1
			}
			if attempts.Load() != expected {
				t.Fatalf("delivery attempts=%d, want %d", attempts.Load(), expected)
			}
		})
	}
}

func TestExternalDispatcherRetriesOnlyDefinitiveBusy(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		firstStatus  int
		networkError bool
		attempts     int32
	}{
		{name: "busy", firstStatus: http.StatusConflict, attempts: 2},
		{name: "rejected", firstStatus: http.StatusPreconditionFailed, attempts: 1},
		{name: "ambiguous", networkError: true, attempts: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "instance", Namespace: "tenant"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}, SessionContext: map[string]string{"input": "work"}}, Status: v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhaseRunning, SessionID: "session"}}
			storage := &stubStore{data: map[string][]byte{"AgentInstance/tenant/instance": resourceBytes(t, instance)}}
			dispatcher, err := NewExternalDispatcher(storage, zap.NewNop(), externalTestConfig(t), map[string]string{"tenant/agent": "https://runtime.test/invoke"})
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var attempts atomic.Int32
			dispatcher.invokeHTTP.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				statusCode := http.StatusAccepted
				if attempts.Add(1) == 1 {
					close(entered)
					select {
					case <-release:
					case <-request.Context().Done():
						return nil, request.Context().Err()
					}
					if scenario.networkError {
						return nil, errors.New("response lost")
					}
					statusCode = scenario.firstStatus
				}
				return &http.Response{StatusCode: statusCode, Body: http.NoBody, Header: make(http.Header)}, nil
			})
			dispatcher.invokeExternal(instance, "https://runtime.test/invoke", "session", "{}", "", "", "capability", "", "")
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("invocation did not start")
			}
			dispatcher.invocationsMu.Lock()
			control := dispatcher.invocations["session"]
			dispatcher.invocationsMu.Unlock()
			defer control.cancel()
			close(release)
			select {
			case <-control.done:
			case <-time.After(5 * time.Second):
				t.Fatal("invocation did not finish")
			}
			if attempts.Load() != scenario.attempts {
				t.Fatalf("attempts=%d", attempts.Load())
			}
		})
	}
}

func TestExternalDispatcherRecoversStatusWithoutLocalInvocation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/status" || request.Header.Get("Authorization") != "Bearer test-runtime-token" {
			t.Error("recovery made an unexpected request")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"agent_name": "agent", "session_id": "session", "phase": "Completed", "output": "retained result", "token_usage": map[string]int{"total_tokens": 7}})
	}))
	defer server.Close()
	dispatcher, err := NewExternalDispatcher(&stubStore{}, zap.NewNop(), externalTestConfig(t), map[string]string{"tenant/agent": server.URL + "/invoke"})
	if err != nil {
		t.Fatal(err)
	}
	instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}, SessionContext: map[string]string{"input": "work"}}, Status: v1alpha1.AgentInstanceStatus{SessionID: "session"}}
	status, err := dispatcher.Status(context.Background(), instance)
	if err != nil || !status.Completed || status.Output != "retained result" || status.TokenUsage == nil || status.TokenUsage.TotalTokens != 7 {
		t.Fatalf("recovered status=%+v err=%v", status, err)
	}
	instance.Spec.SessionContext["input"] = ""
	status, err = dispatcher.Status(context.Background(), instance)
	if err != nil || !status.Alive || status.Completed || status.Output != "" || status.TokenUsage != nil {
		t.Fatalf("warm status=%+v err=%v", status, err)
	}
}

func TestExternalDispatcherRejectsRedirects(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		leaked.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	dispatcher, err := NewExternalDispatcher(&stubStore{}, zap.NewNop(), externalTestConfig(t), map[string]string{"tenant/agent": server.URL + "/invoke"})
	if err != nil {
		t.Fatal(err)
	}
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent"}}
	if _, err := dispatcher.externalConfiguration(context.Background(), server.URL+"/invoke", "tenant", agent, &v1alpha1.Model{}); err == nil {
		t.Fatal("accepted configuration redirect")
	}
	instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}, SessionContext: map[string]string{"input": "work"}}, Status: v1alpha1.AgentInstanceStatus{SessionID: "session"}}
	if _, err := dispatcher.Status(context.Background(), instance); !errors.Is(err, intctrl.ErrDispatchAccountingUncertain) {
		t.Fatalf("status redirect=%v", err)
	}
	if err := dispatcher.Terminate(context.Background(), instance, "cancel"); err == nil {
		t.Fatal("accepted cancel redirect")
	}
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/invoke", strings.NewReader("sensitive payload"))
	request.Header.Set("Authorization", "Bearer test-runtime-token")
	response, err := dispatcher.invokeHTTP.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect || leaked.Load() != 0 {
		t.Fatal("followed credential-bearing redirect")
	}
}
