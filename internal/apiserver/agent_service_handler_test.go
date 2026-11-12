// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/cpi"
)

func TestStandaloneAgentServiceAuthenticatedInvocation(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		token  string
		status int
	}{
		{name: "authorized", token: "gateway-api-token", status: http.StatusOK},
		{name: "dedicated service credential", token: "dedicated-service-token", status: http.StatusOK},
		{name: "runtime credential rejected", token: "runtime-token", status: http.StatusUnauthorized},
		{name: "missing credential", status: http.StatusUnauthorized},
		{name: "unauthorized group", token: "viewer-token", status: http.StatusForbidden},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			memory := newMemStore()
			putAgent(memory, v1alpha1.PhaseReady)
			putModel(memory)
			putModelClass(memory)
			putJSON(memory, "AgentService", "default", "chat-service", v1alpha1.AgentService{
				Spec:   v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}},
				Status: v1alpha1.AgentServiceStatus{Phase: v1alpha1.PhaseReady, ReadyInstances: 1},
			})
			driver := &recordingChatDriver{response: &cpi.CompletionResponse{Content: "delegated response", Usage: cpi.Usage{Reported: true}}}
			registry := cpi.NewRegistry()
			registry.MustRegister(driver)
			authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
				"gateway-api-token": {Subject: "communication-gateway", Groups: []string{"runtime-invokers"}, Method: "token"},
				"viewer-token":      {Subject: "viewer", Groups: []string{"viewers"}, Method: "token"},
			}}
			server := httptest.NewServer(New(Config{}, memory, zap.NewNop(), WithCPIRegistry(registry), WithAuthenticator(authenticator, []string{"runtime-invokers"}), WithAgentServiceToken("dedicated-service-token")))
			defer server.Close()
			request, err := http.NewRequest(http.MethodPost, server.URL+"/apis/kognition.dev/v1alpha1/namespaces/default/agentservices/chat-service/invoke", strings.NewReader(`{"message":"review this","sessionID":"delegation-1"}`))
			if err != nil {
				t.Fatal(err)
			}
			if scenario.token != "" {
				request.Header.Set("Authorization", "Bearer "+scenario.token)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != scenario.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, scenario.status)
			}
			if scenario.status != http.StatusOK {
				if driver.request != nil {
					t.Fatal("unauthorized invocation reached the provider")
				}
				return
			}
			var result ChatResponse
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if result.SessionID != "delegation-1" || result.Response != "delegated response" || driver.request == nil {
				t.Fatalf("delegation lost its session or provider result: %+v", result)
			}
		})
	}
}

func TestAgentServiceInvokeDelegatesToReadyAgentChat(t *testing.T) {
	memory := newMemStore()
	putAgent(memory, v1alpha1.PhaseReady)
	putModel(memory)
	putModelClass(memory)
	putJSON(memory, "AgentService", "default", "chat-service", v1alpha1.AgentService{
		Spec: v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}},
		Status: v1alpha1.AgentServiceStatus{
			Phase: v1alpha1.PhaseReady, ReadyInstances: 1,
			Endpoint: "/apis/kognition.dev/v1alpha1/namespaces/default/agentservices/chat-service/invoke",
		},
	})
	driver := &recordingChatDriver{response: &cpi.CompletionResponse{Content: "service response", Usage: cpi.Usage{Reported: true}}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	server := New(Config{}, memory, zap.NewNop(), WithCPIRegistry(registry))

	request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/agentservices/chat-service/invoke", strings.NewReader(`{"message":"hello"}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "service response") {
		t.Fatalf("response = %d %s, want successful chat", response.Code, response.Body)
	}
	if driver.request == nil {
		t.Fatal("AgentService did not dispatch its referenced Agent")
	}
	events, err := memory.ListEvents(context.Background(), "Agent", "default", "test-agent")
	if err != nil || len(events) != 1 || events[0].Reason != "ChatCompleted" {
		t.Fatalf("agent events = %+v, err = %v", events, err)
	}
}

func TestAgentServiceInvokeUsesPinnedRevisionSnapshot(t *testing.T) {
	memory := newMemStore()
	current := v1alpha1.Agent{Spec: v1alpha1.AgentSpec{
		ModelRef:     v1alpha1.LocalObjectReference{Name: "test-model"},
		Instructions: v1alpha1.AgentInstructions{Inline: "current prompt"},
	}, Status: v1alpha1.AgentStatus{Phase: v1alpha1.PhaseReady}}
	putJSON(memory, "Agent", "default", "test-agent", current)
	pinned := current
	pinned.Spec.Instructions.Inline = "pinned revision prompt"
	pinnedData, err := json.Marshal(pinned)
	if err != nil {
		t.Fatalf("marshal pinned revision: %v", err)
	}
	if err := memory.CreateRevision(context.Background(), "Agent", "default", "test-agent", 1, nil, pinnedData); err != nil {
		t.Fatalf("store pinned revision: %v", err)
	}
	putModel(memory)
	putModelClass(memory)
	revision := int64(1)
	putJSON(memory, "AgentService", "default", "pinned-service", v1alpha1.AgentService{
		Spec:   v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}, RevisionPin: &revision},
		Status: v1alpha1.AgentServiceStatus{Phase: v1alpha1.PhaseReady, ReadyInstances: 1},
	})
	driver := &recordingChatDriver{response: &cpi.CompletionResponse{Content: "revision response", Usage: cpi.Usage{Reported: true}}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	server := New(Config{}, memory, zap.NewNop(), WithCPIRegistry(registry))

	request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/agentservices/pinned-service/invoke", strings.NewReader(`{"message":"hello"}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || driver.request == nil {
		t.Fatalf("response = %d %s, request = %#v", response.Code, response.Body, driver.request)
	}
	if len(driver.request.Messages) < 2 || driver.request.Messages[0].Content != "pinned revision prompt" {
		t.Fatalf("completion messages = %#v, want pinned revision system prompt", driver.request.Messages)
	}
}

func TestDirectAgentChatIgnoresRevisionSelectionHeaders(t *testing.T) {
	memory := newMemStore()
	current := v1alpha1.Agent{Spec: v1alpha1.AgentSpec{
		ModelRef:     v1alpha1.LocalObjectReference{Name: "test-model"},
		Instructions: v1alpha1.AgentInstructions{Inline: "current prompt"},
	}, Status: v1alpha1.AgentStatus{Phase: v1alpha1.PhaseReady}}
	putJSON(memory, "Agent", "default", "test-agent", current)
	pinned := current
	pinned.Spec.Instructions.Inline = "pinned revision prompt"
	pinnedData, err := json.Marshal(pinned)
	if err != nil {
		t.Fatalf("marshal pinned revision: %v", err)
	}
	if err := memory.CreateRevision(context.Background(), "Agent", "default", "test-agent", 1, nil, pinnedData); err != nil {
		t.Fatalf("store pinned revision: %v", err)
	}
	putModel(memory)
	putModelClass(memory)
	driver := &recordingChatDriver{response: &cpi.CompletionResponse{Content: "direct response", Usage: cpi.Usage{Reported: true}}}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	server := New(Config{}, memory, zap.NewNop(), WithCPIRegistry(registry))

	request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/agents/test-agent/chat", strings.NewReader(`{"message":"hello"}`))
	request.Header.Set("X-Kognition-Agent-Revision", "1")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || driver.request == nil {
		t.Fatalf("response = %d %s, request = %#v", response.Code, response.Body, driver.request)
	}
	if len(driver.request.Messages) < 2 || driver.request.Messages[0].Content != "current prompt" {
		t.Fatalf("completion messages = %#v, want current system prompt", driver.request.Messages)
	}
}

func TestAgentServiceInvokeRejectsUnavailablePinnedRevision(t *testing.T) {
	memory := newMemStore()
	putAgent(memory, v1alpha1.PhaseReady)
	revision := int64(7)
	putJSON(memory, "AgentService", "default", "missing-revision", v1alpha1.AgentService{
		Spec:   v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}, RevisionPin: &revision},
		Status: v1alpha1.AgentServiceStatus{Phase: v1alpha1.PhaseReady, ReadyInstances: 1},
	})
	server := New(Config{}, memory, zap.NewNop(), WithCPIRegistry(cpi.NewRegistry()))

	request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/agentservices/missing-revision/invoke", strings.NewReader(`{"message":"hello"}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "revision 7 not found") {
		t.Fatalf("response = %d %s, want unavailable pinned revision", response.Code, response.Body)
	}
}

func TestAgentServiceInvokeRejectsMissingOrUnreadyService(t *testing.T) {
	memory := newMemStore()
	putJSON(memory, "AgentService", "default", "pending", v1alpha1.AgentService{
		Spec:   v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "test-agent"}},
		Status: v1alpha1.AgentServiceStatus{Phase: v1alpha1.PhasePending},
	})
	server := New(Config{}, memory, zap.NewNop(), WithCPIRegistry(cpi.NewRegistry()))
	for _, test := range []struct {
		name       string
		service    string
		wantStatus int
	}{
		{name: "missing", service: "missing", wantStatus: http.StatusNotFound},
		{name: "unready", service: "pending", wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/agentservices/"+test.service+"/invoke", strings.NewReader(`{"message":"hello"}`))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("response = %d %s, want %d", response.Code, response.Body, test.wantStatus)
			}
		})
	}
}
