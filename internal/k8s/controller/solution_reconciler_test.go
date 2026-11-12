// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"testing"

	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

type apiAssignedChildIdentityClient struct{ client.Client }

func (c *apiAssignedChildIdentityClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	object.SetUID(types.UID("api-" + object.GetName()))
	return c.Client.Create(ctx, object, options...)
}

func TestSolutionReconcilerAdapter_ModelChangePreservesChildIdentityAndState(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	solution := &k8s.Solution{
		ObjectMeta: metav1.ObjectMeta{Name: "model-update", Namespace: "demo", UID: "solution-uid"},
		Spec: k8s.SolutionSpec{
			Goal:     "Summarize supplied information",
			ModelRef: &k8s.LocalObjectReference{Name: "previous-chat"},
		},
	}
	kubernetes := &apiAssignedChildIdentityClient{Client: fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(solution).
		WithStatusSubresource(&k8s.Solution{}, &k8s.Agent{}, &k8s.MemoryClaim{}, &k8s.Tool{}, &k8s.AgentPolicy{}, &k8s.Workflow{}).
		Build()}
	storage := NewStoreAdapter(kubernetes)
	adapter := NewSolutionReconcilerAdapter(storage, zaptest.NewLogger(t))
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(solution)}
	for range 2 {
		if _, err := adapter.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	claimKey := client.ObjectKey{Namespace: "demo", Name: "model-update-memory"}
	var claim k8s.MemoryClaim
	if err := kubernetes.Get(ctx, claimKey, &claim); err != nil {
		t.Fatal(err)
	}
	claimUID := claim.UID
	claim.Finalizers = []string{"kognition.dev/memoryclaim-cleanup"}
	claim.Annotations = map[string]string{"operator.example/retained": "keep"}
	if err := kubernetes.Update(ctx, &claim); err != nil {
		t.Fatal(err)
	}
	claim.Status.Phase = "Bound"
	claim.Status.BoundVolumeName = "existing-bound-volume"
	if err := kubernetes.Status().Update(ctx, &claim); err != nil {
		t.Fatal(err)
	}
	agentKey := client.ObjectKey{Namespace: "demo", Name: "model-update-agent"}
	var agent k8s.Agent
	if err := kubernetes.Get(ctx, agentKey, &agent); err != nil {
		t.Fatal(err)
	}
	agentUID := agent.UID
	if err := kubernetes.Get(ctx, request.NamespacedName, solution); err != nil {
		t.Fatal(err)
	}
	solution.Spec.ModelRef.Name = "gpt-6-luna"
	if err := kubernetes.Update(ctx, solution); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := kubernetes.Get(ctx, request.NamespacedName, solution); err != nil {
		t.Fatal(err)
	}
	if solution.Status.Phase != "Ready" || solution.Status.ActiveRevision != 2 {
		t.Fatalf("changed Solution did not become Ready at revision 2: %+v", solution.Status)
	}
	if err := kubernetes.Get(ctx, agentKey, &agent); err != nil {
		t.Fatal(err)
	}
	if agent.UID != agentUID || agent.Spec.ModelRef.Name != "gpt-6-luna" {
		t.Fatalf("child model changed by replacing its identity: UID=%s model=%s", agent.UID, agent.Spec.ModelRef.Name)
	}
	if err := kubernetes.Get(ctx, claimKey, &claim); err != nil {
		t.Fatal(err)
	}
	if claim.UID != claimUID || claim.Status.Phase != "Bound" || claim.Status.BoundVolumeName != "existing-bound-volume" ||
		len(claim.Finalizers) != 1 || claim.Finalizers[0] != "kognition.dev/memoryclaim-cleanup" || claim.Annotations["operator.example/retained"] != "keep" {
		t.Fatalf("model-only update changed bound child identity/state: %+v", claim)
	}
	revision, err := storage.GetRevision(ctx, "Solution", "demo", "model-update", 1)
	if err != nil {
		t.Fatal(err)
	}
	var previous k8s.SolutionSpec
	if err := json.Unmarshal(revision.Spec, &previous); err != nil {
		t.Fatal(err)
	}
	if previous.ModelRef == nil || previous.ModelRef.Name != "previous-chat" {
		t.Fatal("model update rewrote the immutable prior Solution revision")
	}
}

func TestSolutionReconcilerAdapter_DeploysOnceAndRemainsIdempotent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	solution := &k8s.Solution{
		ObjectMeta: metav1.ObjectMeta{Name: "operator", Namespace: "demo", UID: types.UID("solution-uid")},
		Spec:       k8s.SolutionSpec{Goal: "Help operators"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(solution).
		WithStatusSubresource(&k8s.Solution{}, &k8s.Agent{}).
		Build()
	storeAdapter := NewStoreAdapter(k8sClient)
	adapter := NewSolutionReconcilerAdapter(storeAdapter, zaptest.NewLogger(t))
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(solution)}

	result, err := adapter.Reconcile(context.Background(), req)
	if err != nil || !result.Requeue { //nolint:staticcheck // Verify controller-runtime 1.x immediate requeue compatibility.
		t.Fatalf("finalizer reconcile = (%+v, %v), want requeue without error", result, err)
	}
	if _, err := adapter.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("deployment reconcile: %v", err)
	}

	var deployed k8s.Solution
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &deployed); err != nil {
		t.Fatalf("get solution: %v", err)
	}
	revisions, err := storeAdapter.ListRevisions(context.Background(), "Solution", "demo", "operator")
	if err != nil || deployed.Status.ActiveRevision != 1 || len(revisions) != 1 || revisions[0].Revision != 1 {
		t.Fatalf("deployed solution = status %#v revisions %+v err %v", deployed.Status, revisions, err)
	}
	var agent k8s.Agent
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: "operator-agent"}, &agent); err != nil {
		t.Fatalf("get compiled agent: %v", err)
	}

	if _, err := adapter.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("idempotent reconcile: %v", err)
	}
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &deployed); err != nil {
		t.Fatalf("get reconciled solution: %v", err)
	}
	revisions, err = storeAdapter.ListRevisions(context.Background(), "Solution", "demo", "operator")
	if err != nil || deployed.Status.ActiveRevision != 1 || len(revisions) != 1 {
		t.Fatalf("idempotent reconcile created another revision: status %#v revisions %+v err %v", deployed.Status, revisions, err)
	}
}

// TC-0131 verifies Workspace quota inheritance and bounded Solution overrides
// through the Kubernetes store adapter and deployed child resources.
func TestSolutionReconcilerAdapter_InheritsWorkspaceQuotaAndAppliesOverride(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-quota", Namespace: "demo"},
		Spec: k8s.CognitiveQuotaSpec{
			Hard: map[string]int64{"agents": 4, "requestsPerMinute": 60},
			TokenBudget: &k8s.TokenBudget{
				MaxInputTokens:  4000,
				MaxOutputTokens: 2000,
				MaxTotalTokens:  5000,
			},
		},
	}
	solution := &k8s.Solution{
		ObjectMeta: metav1.ObjectMeta{Name: "governed", Namespace: "demo", UID: types.UID("solution-uid")},
		Spec: k8s.SolutionSpec{
			Goal: "Review compliance evidence",
			Overrides: &k8s.SolutionOverrides{
				Hard: map[string]int64{"agents": 3},
				TokenBudget: &k8s.TokenBudget{
					MaxInputTokens:  3000,
					MaxOutputTokens: 9000,
					MaxTotalTokens:  4500,
				},
			},
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(quota, solution).
		WithStatusSubresource(&k8s.Solution{}, &k8s.Agent{}).
		Build()
	adapter := NewSolutionReconcilerAdapter(NewStoreAdapter(k8sClient), zaptest.NewLogger(t))
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(solution)}

	if result, err := adapter.Reconcile(context.Background(), req); err != nil || !result.Requeue { //nolint:staticcheck // Verify controller-runtime 1.x immediate requeue compatibility.
		t.Fatalf("finalizer reconcile = (%+v, %v), want requeue without error", result, err)
	}
	if _, err := adapter.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("deployment reconcile: %v", err)
	}

	var agent k8s.Agent
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: "governed-agent"}, &agent); err != nil {
		t.Fatalf("get compiled agent: %v", err)
	}
	if agent.Spec.Runtime == nil || agent.Spec.Runtime.MaxTokens == nil || *agent.Spec.Runtime.MaxTokens != 4500 {
		t.Fatalf("compiled runtime maxTokens = %#v, want 4500", agent.Spec.Runtime)
	}
	var inheritedHard map[string]int64
	if err := json.Unmarshal([]byte(agent.Annotations["kognition.dev/workspace-quota"]), &inheritedHard); err != nil {
		t.Fatalf("decode inherited Workspace quota: %v", err)
	}
	if inheritedHard["agents"] != 3 || inheritedHard["requestsPerMinute"] != 60 {
		t.Fatalf("effective hard quota = %v, want overridden agents=3 and inherited requestsPerMinute=60", inheritedHard)
	}
}

func TestSolutionReconcilerAdapter_AgentWatchUsesSolutionLabel(t *testing.T) {
	adapter := NewSolutionReconcilerAdapter(NewStoreAdapter(newFakeClient(t)), zaptest.NewLogger(t))
	agent := &k8s.Agent{ObjectMeta: metav1.ObjectMeta{Name: "operator-agent", Namespace: "demo", Labels: map[string]string{
		"kognition.dev/solution": "operator",
	}}}

	requests := adapter.agentToSolution(context.Background(), agent)
	if len(requests) != 1 {
		t.Fatalf("agentToSolution() = %#v, want one request", requests)
	}
	if requests[0].NamespacedName != (types.NamespacedName{Namespace: "demo", Name: "operator"}) {
		t.Fatalf("agentToSolution() = %#v, want demo/operator", requests)
	}

	unlabeled := &k8s.Agent{ObjectMeta: metav1.ObjectMeta{Name: "standalone-agent", Namespace: "demo"}}
	if requests := adapter.agentToSolution(context.Background(), unlabeled); len(requests) != 0 {
		t.Fatalf("agentToSolution() for unlabeled Agent = %#v, want no requests", requests)
	}
}

func TestSolutionReconcilerAdapter_QuotaWatchMapsNamespaceSolutions(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	quota := &k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "demo"}}
	first := &k8s.Solution{ObjectMeta: metav1.ObjectMeta{Name: "operator", Namespace: "demo"}}
	second := &k8s.Solution{ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: "demo"}}
	otherNamespace := &k8s.Solution{ObjectMeta: metav1.ObjectMeta{Name: "operator", Namespace: "other"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(quota, first, second, otherNamespace).Build()
	adapter := NewSolutionReconcilerAdapter(NewStoreAdapter(k8sClient), zaptest.NewLogger(t))
	adapter.client = k8sClient

	requests := adapter.quotaToSolutions(context.Background(), quota)
	want := map[types.NamespacedName]bool{
		client.ObjectKeyFromObject(first):  true,
		client.ObjectKeyFromObject(second): true,
	}
	if len(requests) != len(want) {
		t.Fatalf("quotaToSolutions() = %#v, want %d demo Solutions", requests, len(want))
	}
	for _, request := range requests {
		if !want[request.NamespacedName] {
			t.Fatalf("quotaToSolutions() returned unexpected request %#v", request)
		}
	}
}
