// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

type autoscalerListErrorClient struct {
	client.Client
}

func (c *autoscalerListErrorClient) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("list unavailable")
}

func TestModelAutoscalerAgentWatchEnqueuesOnlyAffectedAutoscalers(t *testing.T) {
	writer := &k8s.ModelAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "writer-scale", Namespace: "team-a"},
		Spec:       k8s.ModelAutoscalerSpec{AgentRef: k8s.LocalObjectReference{Name: "writer"}},
	}
	reader := &k8s.ModelAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "reader-scale", Namespace: "team-a"},
		Spec:       k8s.ModelAutoscalerSpec{AgentRef: k8s.LocalObjectReference{Name: "reader"}},
	}
	adapter := &ModelAutoscalerReconcilerAdapter{client: newFakeClient(t, writer, reader), logger: testLogger(t)}

	requests := adapter.agentToAutoscalers(context.Background(), &k8s.Agent{ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: "team-a"}})
	if len(requests) != 1 || requests[0].Name != "writer-scale" || requests[0].Namespace != "team-a" {
		t.Fatalf("agent watch requests = %+v, want team-a/writer-scale", requests)
	}
}

func TestModelAutoscalerModelWatchEnqueuesOnlySameNamespace(t *testing.T) {
	first := &k8s.ModelAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "team-a"}}
	second := &k8s.ModelAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "team-a"}}
	otherNamespace := &k8s.ModelAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "team-b"}}
	adapter := &ModelAutoscalerReconcilerAdapter{client: newFakeClient(t, first, second, otherNamespace), logger: testLogger(t)}

	requests := adapter.namespacedDependencyToAutoscalers(context.Background(), &k8s.Model{ObjectMeta: metav1.ObjectMeta{Name: "shared-model", Namespace: "team-a"}})
	if len(requests) != 2 {
		t.Fatalf("model watch enqueued %d autoscalers, want 2", len(requests))
	}
	seen := map[string]bool{}
	for _, request := range requests {
		seen[request.Namespace+"/"+request.Name] = true
	}
	if !seen["team-a/first"] || !seen["team-a/second"] || seen["team-b/other"] {
		t.Fatalf("model watch requests = %+v, want only team-a autoscalers", requests)
	}
}

func TestModelAutoscalerModelClassWatchEnqueuesAcrossNamespaces(t *testing.T) {
	first := &k8s.ModelAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "first", Namespace: "team-a"}}
	second := &k8s.ModelAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "second", Namespace: "team-b"}}
	adapter := &ModelAutoscalerReconcilerAdapter{client: newFakeClient(t, first, second), logger: testLogger(t)}

	requests := adapter.modelClassToAutoscalers(context.Background(), &k8s.ModelClass{ObjectMeta: metav1.ObjectMeta{Name: "fallback"}})
	if len(requests) != 2 {
		t.Fatalf("model class watch enqueued %d autoscalers, want 2", len(requests))
	}
	seen := map[string]bool{}
	for _, request := range requests {
		seen[request.Namespace+"/"+request.Name] = true
	}
	if !seen["team-a/first"] || !seen["team-b/second"] {
		t.Fatalf("model class watch requests = %+v, want autoscalers in both namespaces", requests)
	}
}

func TestModelAutoscalerWatchesReturnNoRequestsWhenListFails(t *testing.T) {
	adapter := &ModelAutoscalerReconcilerAdapter{client: &autoscalerListErrorClient{}, logger: testLogger(t)}

	agentRequests := adapter.agentToAutoscalers(context.Background(), &k8s.Agent{ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: "team-a"}})
	if agentRequests != nil {
		t.Fatalf("agent watch requests = %+v, want nil after list failure", agentRequests)
	}

	modelRequests := adapter.namespacedDependencyToAutoscalers(context.Background(), &k8s.Model{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "team-a"}})
	if modelRequests != nil {
		t.Fatalf("model watch requests = %+v, want nil after list failure", modelRequests)
	}
}
