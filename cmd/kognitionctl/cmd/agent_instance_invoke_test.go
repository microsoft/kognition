// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestInvokeInteractiveAgentK8sExplainsAForbiddenChatStep(t *testing.T) {
	forbid := func(resource string) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: "kognition.dev", Resource: resource}, "researcher", errors.New(`User "alice" cannot do that`))
	}
	tests := []struct {
		name      string
		denyGet   string
		denyMake  bool
		wantParts []string
	}{
		{name: "reading the Agent", denyGet: "agents", wantParts: []string{"get agents", "team-a", "auth can-i get agents"}},
		{name: "creating the AgentInstance", denyMake: true, wantParts: []string{"create agentinstances", "team-a", "auth can-i create agentinstances", "ADR-0031", "AgentInstance"}},
		{name: "reading the AgentInstance back", denyGet: "agentinstances", wantParts: []string{"get agentinstances", "team-a", "auth can-i get agentinstances"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
			previousClient := k8sDynClient
			k8sDynClient = client
			t.Cleanup(func() { k8sDynClient = previousClient })
			client.PrependReactor("get", "agents", func(k8stesting.Action) (bool, runtime.Object, error) {
				if test.denyGet == "agents" {
					return true, nil, forbid("agents")
				}
				return true, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kognition.dev/v1alpha1", "kind": "Agent", "metadata": map[string]any{"name": "researcher", "namespace": "team-a"}}}, nil
			})
			client.PrependReactor("create", "agentinstances", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if test.denyMake {
					return true, nil, forbid("agentinstances")
				}
				return true, action.(k8stesting.CreateAction).GetObject(), nil
			})
			client.PrependReactor("get", "agentinstances", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, forbid("agentinstances")
			})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := invokeInteractiveAgentK8s(ctx, "team-a", "researcher", "hello", "", "cli")
			if err == nil {
				t.Fatal("a forbidden chat step succeeded")
			}
			for _, part := range test.wantParts {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error does not mention %q:\n%v", part, err)
				}
			}
			if !apierrors.IsForbidden(err) {
				t.Errorf("the original Forbidden error was lost: %v", err)
			}
		})
	}

	t.Run("other failures are not rewritten", func(t *testing.T) {
		client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
		previousClient := k8sDynClient
		k8sDynClient = client
		t.Cleanup(func() { k8sDynClient = previousClient })
		client.PrependReactor("get", "agents", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("connection refused")
		})
		_, err := invokeInteractiveAgentK8s(context.Background(), "team-a", "researcher", "hello", "", "cli")
		if err == nil || strings.Contains(err.Error(), "auth can-i") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestInvokeInteractiveAgentK8sCreatesCapabilityAwareInstance(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	previousClient := k8sDynClient
	k8sDynClient = client
	t.Cleanup(func() { k8sDynClient = previousClient })

	var created *unstructured.Unstructured
	client.PrependReactor("create", "agentinstances", func(action k8stesting.Action) (bool, runtime.Object, error) {
		created = action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		return true, created, nil
	})
	client.PrependReactor("get", "agents", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kognition.dev/v1alpha1",
			"kind":       "Agent",
			"metadata":   map[string]any{"name": "researcher", "namespace": "team-a"},
			"status":     map[string]any{"revisionCount": int64(7)},
		}}, nil
	})
	statusReads := 0
	client.PrependReactor("get", "agentinstances", func(k8stesting.Action) (bool, runtime.Object, error) {
		completed := created.DeepCopy()
		statusReads++
		if statusReads == 1 {
			completed.Object["status"] = map[string]any{
				"phase": "Pending",
				"conditions": []any{
					map[string]any{"type": "Ready", "status": "False", "reason": "IdentityPending"},
					map[string]any{"type": "Ready", "status": "True", "reason": "ModelAccessDenied"},
					map[string]any{"type": "Other", "status": "False", "reason": "ModelAccessDenied"},
				},
			}
			return true, completed, nil
		}
		completed.Object["status"] = map[string]any{
			"phase":           "Ready",
			"sessionID":       "runtime-session-7",
			"output":          "analysis complete",
			"tokenUsage":      map[string]any{"promptTokens": int64(8), "completionTokens": int64(3)},
			"toolCalls":       int64(1),
			"toolCallsDetail": []any{map[string]any{"name": "analyze-dataset-research-tool"}},
		}
		return true, completed, nil
	})

	result, err := invokeInteractiveAgentK8s(context.Background(), "team-a", "researcher", "analyze data", "conversation-7", "studio")
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "analysis complete" || result.SessionID != "runtime-session-7" || result.ToolCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
	if created.GetNamespace() != "team-a" || created.GetLabels()["kognition.dev/created-by"] != "studio" {
		t.Fatalf("metadata = %#v", created.Object["metadata"])
	}
	agentName, _, _ := unstructured.NestedString(created.Object, "spec", "agentRef", "name")
	input, _, _ := unstructured.NestedString(created.Object, "spec", "sessionContext", "input")
	conversationID, _, _ := unstructured.NestedString(created.Object, "spec", "sessionContext", "conversation_id")
	revision, _, _ := unstructured.NestedInt64(created.Object, "spec", "revision")
	if agentName != "researcher" || input != "analyze data" || conversationID != "conversation-7" || revision != 7 {
		t.Fatalf("spec = %#v", created.Object["spec"])
	}
}

func TestInvokeInteractiveAgentK8sReturnsControllerFailure(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		phase  string
		reason string
	}{
		{name: "terminal failure", phase: "Failed", reason: "InvocationFailed"},
		{name: "pending model denial", phase: "Pending", reason: "ModelAccessDenied"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
			previousClient := k8sDynClient
			k8sDynClient = client
			t.Cleanup(func() { k8sDynClient = previousClient })

			client.PrependReactor("get", "agents", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "kognition.dev/v1alpha1",
					"kind":       "Agent",
					"metadata":   map[string]any{"name": "researcher", "namespace": "team-a"},
					"status":     map[string]any{"revisionCount": int64(1)},
				}}, nil
			})
			client.PrependReactor("get", "agentinstances", func(action k8stesting.Action) (bool, runtime.Object, error) {
				get := action.(k8stesting.GetAction)
				return true, &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "kognition.dev/v1alpha1",
					"kind":       "AgentInstance",
					"metadata": map[string]any{
						"name":      get.GetName(),
						"namespace": "team-a",
					},
					"status": map[string]any{
						"phase": testCase.phase,
						"conditions": []any{map[string]any{
							"type": "Ready", "status": "False", "reason": testCase.reason, "message": "model grant denied",
							"lastTransitionTime": metav1.Now().Format("2006-01-02T15:04:05Z"),
						}},
					},
				}}, nil
			})

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := invokeInteractiveAgentK8s(ctx, "team-a", "researcher", "analyze data", "", "cli")
			if err == nil || !strings.Contains(err.Error(), "model grant denied") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
