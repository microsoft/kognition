// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

func TestTrustProfileIndexReconcilerSelectsProfile(t *testing.T) {
	agent, profile := trustProfileIndexFixtures("profile-a", "profile-a-uid")
	k8sClient := newFakeClient(t, agent, profile)
	reconciler := NewTrustProfileIndexReconciler(k8sClient)

	if _, err := reconciler.Reconcile(context.Background(), trustProfileIndexRequest(profile)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	index := &k8s.TrustProfileIndex{}
	key := client.ObjectKey{Namespace: profile.Namespace, Name: TrustProfileIndexName(agent.UID)}
	if err := k8sClient.Get(context.Background(), key, index); err != nil {
		t.Fatalf("get TrustProfileIndex: %v", err)
	}
	if index.Spec.AgentUID != string(agent.UID) || index.Spec.SelectedProfileName != profile.Name || index.Spec.SelectedProfileUID != string(profile.UID) {
		t.Fatalf("TrustProfileIndex spec = %+v", index.Spec)
	}
	if len(index.OwnerReferences) != 1 || index.OwnerReferences[0].UID != agent.UID || index.OwnerReferences[0].Kind != "Agent" || index.OwnerReferences[0].Controller == nil || !*index.OwnerReferences[0].Controller {
		t.Fatalf("TrustProfileIndex owner references = %+v", index.OwnerReferences)
	}
	storedProfile := &k8s.TrustProfile{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(profile), storedProfile); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(storedProfile, TrustProfileIndexFinalizer) {
		t.Fatal("selected TrustProfile lacks index finalizer")
	}
}

func TestTrustProfileIndexReconcilerPreservesCreateWinner(t *testing.T) {
	agent, loser := trustProfileIndexFixtures("profile-b", "profile-b-uid")
	_, selected := trustProfileIndexFixtures("profile-a", "profile-a-uid")
	winner := &k8s.TrustProfileIndex{
		ObjectMeta: metav1.ObjectMeta{Namespace: loser.Namespace, Name: TrustProfileIndexName(agent.UID)},
		Spec:       k8s.TrustProfileIndexSpec{AgentUID: string(agent.UID), SelectedProfileName: selected.Name, SelectedProfileUID: string(selected.UID)},
	}
	k8sClient := newFakeClient(t, agent, selected, loser, winner)
	reconciler := NewTrustProfileIndexReconciler(k8sClient)

	if _, err := reconciler.Reconcile(context.Background(), trustProfileIndexRequest(loser)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	storedIndex := &k8s.TrustProfileIndex{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(winner), storedIndex); err != nil {
		t.Fatal(err)
	}
	if storedIndex.Spec.SelectedProfileUID != "profile-a-uid" {
		t.Fatalf("winning profile UID = %q", storedIndex.Spec.SelectedProfileUID)
	}
	storedLoser := &k8s.TrustProfile{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(loser), storedLoser); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(storedLoser, TrustProfileIndexFinalizer) {
		t.Fatal("losing TrustProfile retained index finalizer")
	}
}

func TestTrustProfileIndexReconcilerRejectsStaleAgentIncarnation(t *testing.T) {
	agent, profile := trustProfileIndexFixtures("profile-a", "profile-a-uid")
	staleAgentUID := types.UID("previous-agent-uid")
	profile.Spec.AgentIncarnationUID = string(staleAgentUID)
	profile.Finalizers = []string{TrustProfileIndexFinalizer}
	index := &k8s.TrustProfileIndex{
		ObjectMeta: metav1.ObjectMeta{Namespace: profile.Namespace, Name: TrustProfileIndexName(staleAgentUID)},
		Spec:       k8s.TrustProfileIndexSpec{AgentUID: string(staleAgentUID), SelectedProfileName: profile.Name, SelectedProfileUID: string(profile.UID)},
	}
	k8sClient := newFakeClient(t, agent, profile, index)
	reconciler := NewTrustProfileIndexReconciler(k8sClient)

	if _, err := reconciler.Reconcile(context.Background(), trustProfileIndexRequest(profile)); err == nil {
		t.Fatal("Reconcile() error = nil, want stale incarnation rejection")
	}
	storedIndex := &k8s.TrustProfileIndex{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(index), storedIndex); client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("TrustProfileIndex lookup error = %v, want not found", err)
	}
	storedProfile := &k8s.TrustProfile{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(profile), storedProfile); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(storedProfile, TrustProfileIndexFinalizer) {
		t.Fatal("stale TrustProfile retained index finalizer")
	}
}

func TestTrustProfileIndexReconcilerReleasesSelectionWhenAgentIsAbsent(t *testing.T) {
	_, profile := trustProfileIndexFixtures("profile-a", "profile-a-uid")
	profile.Finalizers = []string{TrustProfileIndexFinalizer}
	index := &k8s.TrustProfileIndex{
		ObjectMeta: metav1.ObjectMeta{Namespace: profile.Namespace, Name: TrustProfileIndexName(types.UID(profile.Spec.AgentIncarnationUID))},
		Spec:       k8s.TrustProfileIndexSpec{AgentUID: profile.Spec.AgentIncarnationUID, SelectedProfileName: profile.Name, SelectedProfileUID: string(profile.UID)},
	}
	k8sClient := newFakeClient(t, profile, index)
	reconciler := NewTrustProfileIndexReconciler(k8sClient)

	if _, err := reconciler.Reconcile(context.Background(), trustProfileIndexRequest(profile)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	storedIndex := &k8s.TrustProfileIndex{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(index), storedIndex); client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("TrustProfileIndex lookup error = %v, want not found", err)
	}
	storedProfile := &k8s.TrustProfile{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(profile), storedProfile); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(storedProfile, TrustProfileIndexFinalizer) {
		t.Fatal("orphaned TrustProfile retained index finalizer")
	}
}

func TestTrustProfileIndexReconcilerTransitionsIndexBeforeDeletion(t *testing.T) {
	agent, selected := trustProfileIndexFixtures("profile-a", "profile-a-uid")
	now := metav1.Now()
	selected.Finalizers = []string{TrustProfileIndexFinalizer}
	selected.DeletionTimestamp = &now
	_, replacement := trustProfileIndexFixtures("profile-b", "profile-b-uid")
	index := &k8s.TrustProfileIndex{
		ObjectMeta: metav1.ObjectMeta{Namespace: selected.Namespace, Name: TrustProfileIndexName(agent.UID)},
		Spec:       k8s.TrustProfileIndexSpec{AgentUID: string(agent.UID), SelectedProfileName: selected.Name, SelectedProfileUID: string(selected.UID)},
	}
	k8sClient := newFakeClient(t, agent, selected, replacement, index)
	reconciler := NewTrustProfileIndexReconciler(k8sClient)

	if _, err := reconciler.Reconcile(context.Background(), trustProfileIndexRequest(selected)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	storedIndex := &k8s.TrustProfileIndex{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(index), storedIndex); err != nil {
		t.Fatal(err)
	}
	if storedIndex.Spec.SelectedProfileName != replacement.Name || storedIndex.Spec.SelectedProfileUID != string(replacement.UID) {
		t.Fatalf("transitioned TrustProfileIndex spec = %+v", storedIndex.Spec)
	}
	storedReplacement := &k8s.TrustProfile{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(replacement), storedReplacement); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(storedReplacement, TrustProfileIndexFinalizer) {
		t.Fatal("replacement TrustProfile lacks index finalizer")
	}
	storedSelected := &k8s.TrustProfile{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(selected), storedSelected); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	} else if err == nil && controllerutil.ContainsFinalizer(storedSelected, TrustProfileIndexFinalizer) {
		t.Fatal("deleting TrustProfile retained index finalizer")
	}
}

func TestTrustProfileIndexReconcilerDeletesIndexBeforeLastProfile(t *testing.T) {
	agent, selected := trustProfileIndexFixtures("profile-a", "profile-a-uid")
	now := metav1.Now()
	selected.Finalizers = []string{TrustProfileIndexFinalizer}
	selected.DeletionTimestamp = &now
	index := &k8s.TrustProfileIndex{
		ObjectMeta: metav1.ObjectMeta{Namespace: selected.Namespace, Name: TrustProfileIndexName(agent.UID)},
		Spec:       k8s.TrustProfileIndexSpec{AgentUID: string(agent.UID), SelectedProfileName: selected.Name, SelectedProfileUID: string(selected.UID)},
	}
	k8sClient := newFakeClient(t, agent, selected, index)
	reconciler := NewTrustProfileIndexReconciler(k8sClient)

	if _, err := reconciler.Reconcile(context.Background(), trustProfileIndexRequest(selected)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	storedIndex := &k8s.TrustProfileIndex{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(index), storedIndex); client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("TrustProfileIndex lookup error = %v, want not found", err)
	}
}

func TestTrustProfileIndexReconcilerDeletesIndexWhoseSelectedProfileIsAbsent(t *testing.T) {
	agent, profile := trustProfileIndexFixtures("profile-a", "profile-a-uid")
	index := &k8s.TrustProfileIndex{
		ObjectMeta: metav1.ObjectMeta{Namespace: profile.Namespace, Name: TrustProfileIndexName(agent.UID)},
		Spec:       k8s.TrustProfileIndexSpec{AgentUID: string(agent.UID), SelectedProfileName: profile.Name, SelectedProfileUID: string(profile.UID)},
	}
	k8sClient := newFakeClient(t, agent, index)
	reconciler := NewTrustProfileIndexReconciler(k8sClient)

	if _, err := reconciler.Reconcile(context.Background(), trustProfileIndexRequest(profile)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	storedIndex := &k8s.TrustProfileIndex{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(index), storedIndex); client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("TrustProfileIndex lookup error = %v, want not found", err)
	}
}

func TestTrustProfileIndexReconcilerRepairsMismatchedSelectedProfileUID(t *testing.T) {
	agent, profile := trustProfileIndexFixtures("profile-a", "current-profile-uid")
	index := &k8s.TrustProfileIndex{
		ObjectMeta: metav1.ObjectMeta{Namespace: profile.Namespace, Name: TrustProfileIndexName(agent.UID)},
		Spec:       k8s.TrustProfileIndexSpec{AgentUID: string(agent.UID), SelectedProfileName: profile.Name, SelectedProfileUID: "deleted-profile-uid"},
	}
	k8sClient := newFakeClient(t, agent, profile, index)
	reconciler := NewTrustProfileIndexReconciler(k8sClient)

	if _, err := reconciler.Reconcile(context.Background(), trustProfileIndexRequest(profile)); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), trustProfileIndexRequest(profile)); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}

	storedIndex := &k8s.TrustProfileIndex{}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(index), storedIndex); err != nil {
		t.Fatal(err)
	}
	if storedIndex.Spec.SelectedProfileName != profile.Name || storedIndex.Spec.SelectedProfileUID != string(profile.UID) {
		t.Fatalf("repaired TrustProfileIndex spec = %+v", storedIndex.Spec)
	}
}

func trustProfileIndexFixtures(profileName string, profileUID types.UID) (*k8s.Agent, *k8s.TrustProfile) {
	agent := &k8s.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "agent-a", UID: "agent-a-uid"}}
	profile := &k8s.TrustProfile{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: profileName, UID: profileUID},
		Spec: k8s.TrustProfileSpec{
			AgentRef:            k8s.LocalObjectReference{Name: agent.Name},
			AgentIncarnationUID: string(agent.UID),
		},
	}
	return agent, profile
}

func trustProfileIndexRequest(profile *k8s.TrustProfile) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: profile.Namespace, Name: profile.Name}}
}
