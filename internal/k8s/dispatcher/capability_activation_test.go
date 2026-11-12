// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dispatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

type activationStore struct {
	*stubStore
	observed chan struct{}
	activate chan []byte
	stop     chan struct{}
	active   []byte
}

func (storage *activationStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind != "AgentInstance" {
		return storage.stubStore.Get(ctx, kind, namespace, name)
	}
	select {
	case storage.observed <- struct{}{}:
	case <-storage.stop:
		return storage.active, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case snapshot := <-storage.activate:
		return snapshot, nil
	case <-storage.stop:
		return storage.active, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestDispatchWaitsForActivationForEachCapability(t *testing.T) {
	for _, plane := range []string{"memory", "tool", "model", "knowledge", "delegation"} {
		t.Run(plane, func(t *testing.T) {
			key := []byte("0123456789abcdef0123456789abcdef")
			memoryIssuer, err := runtimeauth.NewMemoryCapabilityIssuer(key, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			toolIssuer, err := runtimeauth.NewToolSessionCapabilityIssuer(key, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			modelIssuer, err := runtimeauth.NewModelSessionCapabilityIssuer(key, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			delegationIssuer, err := runtimeauth.NewAgentDelegationSessionCapabilityIssuer(key, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			knowledgeIssuer, err := runtimeauth.NewKnowledgeSessionCapabilityIssuer(key, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			agent := v1alpha1.Agent{
				ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "tenant-a", UID: "agent-uid"},
				Spec: v1alpha1.AgentSpec{
					ModelRef: v1alpha1.LocalObjectReference{Name: "model"},
					Tools:    []v1alpha1.AgentToolRef{{Ref: v1alpha1.LocalObjectReference{Name: "tool"}}},
					Memory:   &v1alpha1.AgentMemorySpec{Claims: []v1alpha1.LocalObjectReference{{Name: "notes"}}},
				},
			}
			if plane == "knowledge" {
				agent.Spec.KnowledgeBaseClaims = []v1alpha1.KnowledgeBaseClaimRef{{Name: "docs"}}
			}
			service := v1alpha1.AgentService{
				Spec:   v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}},
				Status: v1alpha1.AgentServiceStatus{Phase: v1alpha1.PhaseReady, ReadyInstances: 1, Endpoint: "http://runtime.test:8080/invoke"},
			}
			storage := &activationStore{
				stubStore: &stubStore{data: map[string][]byte{
					"Agent/tenant-a/planner":             resourceBytes(t, agent),
					"AgentIdentity/tenant-a/identity":    resourceBytes(t, v1alpha1.AgentIdentity{ObjectMeta: v1alpha1.ObjectMeta{Name: "identity", Namespace: "tenant-a", UID: "identity-uid"}, Spec: v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}}}),
					"AgentService/tenant-a/planner-api":  resourceBytes(t, service),
					"Model/tenant-a/model":               resourceBytes(t, v1alpha1.Model{ObjectMeta: v1alpha1.ObjectMeta{Name: "model", ResourceVersion: 9}, Spec: v1alpha1.ModelSpec{Provider: "openai-compatible"}}),
					"MemoryClaim/tenant-a/notes":         resourceBytes(t, v1alpha1.MemoryClaim{Status: v1alpha1.MemoryClaimStatus{BoundVolumeName: "notes-volume"}}),
					"MemoryVolume/tenant-a/notes-volume": resourceBytes(t, v1alpha1.MemoryVolume{Spec: v1alpha1.MemoryVolumeSpec{BackendHandle: "notes-backend"}, Status: v1alpha1.MemoryVolumeStatus{Phase: v1alpha1.PhaseBound}}),
					"KnowledgeBaseClaim/tenant-a/docs": resourceBytes(t, v1alpha1.KnowledgeBaseClaim{
						ObjectMeta: v1alpha1.ObjectMeta{Name: "docs", Namespace: "tenant-a", Generation: 1},
						Spec:       v1alpha1.KnowledgeBaseClaimSpec{KnowledgeBaseName: "docs"},
						Status:     v1alpha1.KnowledgeBaseClaimStatus{Phase: v1alpha1.PhaseBound, BoundKnowledgeBase: "docs", ObservedGeneration: 1},
					}),
					"KnowledgeBase/tenant-a/docs":  resourceBytes(t, v1alpha1.KnowledgeBase{ObjectMeta: v1alpha1.ObjectMeta{Name: "docs", Namespace: "tenant-a"}, Spec: v1alpha1.KnowledgeBaseSpec{KnowledgeBaseClassName: "provider"}}),
					"KnowledgeBaseClass//provider": resourceBytes(t, v1alpha1.KnowledgeBaseClass{Spec: v1alpha1.KnowledgeBaseClassSpec{BackendRef: &v1alpha1.KnowledgeBackendReference{APIGroup: "search.knowledge.kognition.dev", Kind: "AzureAISearchBackend", Name: "shared"}}}),
				}},
				observed: make(chan struct{}), activate: make(chan []byte), stop: make(chan struct{}),
			}
			config := Config{RuntimeAuthToken: "test-runtime-token"}
			field := ""
			switch plane {
			case "memory":
				config.MemoryPolicyGatewayURL, config.MemoryCapabilityIssuer = "http://memory.test", memoryIssuer
				field = "memory_capability"
			case "tool":
				config.ToolGatewayURL, config.ToolSessionCapabilityIssuer = "http://tool.test", toolIssuer
				field = "tool_session_capability"
			case "model":
				config.ModelGatewayURL, config.ModelSessionCapabilityIssuer = "http://model.test", modelIssuer
				field = "model_session_capability"
			case "knowledge":
				config.KnowledgeGatewayURL, config.KnowledgeSessionCapabilityIssuer = "http://knowledge.test", knowledgeIssuer
				field = "knowledge_session_capability"
			case "delegation":
				config.AgentCommunicationGatewayURL, config.AgentDelegationSessionCapabilityIssuer = "http://delegation.test", delegationIssuer
				field = "agent_delegation_session_capability"
			}
			dispatcher := NewK8sDispatcher(nil, storage, zap.NewNop(), config)
			delivered := make(chan map[string]string, 1)
			dispatcher.invokeHTTP.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				var body map[string]string
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					return nil, err
				}
				body["authorization"] = request.Header.Get("Authorization")
				delivered <- body
				return &http.Response{StatusCode: http.StatusAccepted, Body: http.NoBody, Header: make(http.Header)}, nil
			})
			instance := &v1alpha1.AgentInstance{
				ObjectMeta: v1alpha1.ObjectMeta{Name: "instance", Namespace: "tenant-a", UID: "instance-uid", Annotations: map[string]string{intctrl.AgentServiceExecutionAnnotation: "planner-api"}},
				Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}, SessionContext: map[string]string{
					"input": "investigate", intctrl.MemoryCapabilityIdentityNameContextKey: "identity", intctrl.ToolCapabilityIdentityNameContextKey: "identity",
					intctrl.ModelCapabilityIdentityNameContextKey: "identity", intctrl.KnowledgeCapabilityIdentityNameContextKey: "identity",
					intctrl.AgentDelegationCapabilityIdentityNameContextKey: "identity",
				}},
			}
			sessionID, err := dispatcher.Dispatch(context.Background(), instance)
			if err != nil {
				t.Fatal(err)
			}
			instance.Status.Phase = v1alpha1.PhaseRunning
			instance.Status.SessionID = sessionID
			storage.active = resourceBytes(t, instance)
			defer close(storage.stop)
			for _, state := range []string{"pending", "wrong-session", "active"} {
				select {
				case <-delivered:
					t.Fatalf("%s capability delivered before matching Running session", plane)
				case <-storage.observed:
				case <-time.After(2 * time.Second):
					t.Fatal("activation was not checked")
				}
				instance.Status.Phase = v1alpha1.PhaseRunning
				instance.Status.SessionID = sessionID
				if state == "pending" {
					instance.Status.Phase = v1alpha1.PhasePending
				} else if state == "wrong-session" {
					instance.Status.SessionID = "previous-session"
				}
				storage.activate <- resourceBytes(t, instance)
			}
			select {
			case body := <-delivered:
				if body["session_id"] != sessionID || body["authorization"] != "Bearer test-runtime-token" || body[field] == "" {
					t.Fatal("runtime received incorrect session, authentication, or missing capability")
				}
				switch plane {
				case "memory":
					_, err = memoryIssuer.Verify(body[field], runtimeauth.MemoryCapabilityBinding{Namespace: "tenant-a", AgentName: "planner", AgentIdentityName: "identity", InstanceName: "instance", InstanceUID: "instance-uid", SessionID: sessionID, VolumeHandle: "notes-backend"})
				case "tool":
					claims, verifyErr := toolIssuer.VerifyClaims(body[field])
					err = verifyErr
					if err == nil && (claims.SessionID != sessionID || claims.InstanceUID != "instance-uid") {
						t.Fatal("incorrect Tool binding")
					}
				case "model":
					claims, verifyErr := modelIssuer.VerifyClaims(body[field])
					err = verifyErr
					if err == nil && (claims.SessionID != sessionID || claims.InstanceUID != "instance-uid" || claims.ModelResourceVersion != 9) {
						t.Fatal("incorrect Model binding")
					}
				case "delegation":
					claims, verifyErr := delegationIssuer.VerifyClaims(body[field])
					err = verifyErr
					if err == nil && (claims.SessionID != sessionID || claims.InstanceUID != "instance-uid") {
						t.Fatal("incorrect delegation binding")
					}
				}
				if err != nil {
					t.Fatalf("invalid delivered capability: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("active session was not delivered")
			}
		})
	}
}
