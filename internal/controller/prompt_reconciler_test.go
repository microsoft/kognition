// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type promptAgentConflictStore struct {
	*revisionMemStore
	conflicted bool
}

type promptAgentUpdateFailureStore struct {
	*revisionMemStore
	failNextAgentUpdate bool
}

type latestOnlyPromptRevisionStore struct {
	*revisionMemStore
}

type parentUpdatingPromptRevisionStore struct {
	*revisionMemStore
}

type promptGetFaultStore struct {
	*revisionMemStore
	err error
}

func (s *promptGetFaultStore) Get(context.Context, string, string, string) ([]byte, error) {
	return nil, s.err
}

func (s *parentUpdatingPromptRevisionStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	err := s.revisionMemStore.Update(ctx, kind, namespace, name, data, resourceVersion)
	if err != nil {
		return fmt.Errorf("wrapped prompt update: %w", err)
	}
	return nil
}

func (s *parentUpdatingPromptRevisionStore) CreateRevision(ctx context.Context, kind, namespace, name string, revision int64, spec, data []byte) error {
	if err := s.revisionMemStore.CreateRevision(ctx, kind, namespace, name, revision, spec, data); err != nil {
		return err
	}
	raw, err := s.Get(ctx, kind, namespace, name)
	if err != nil {
		return err
	}
	var prompt v1alpha1.Prompt
	if err := json.Unmarshal(raw, &prompt); err != nil {
		return err
	}
	prompt.ObjectMeta.Annotations = map[string]string{"kognition.dev/revision-count": "1"}
	updated, err := json.Marshal(prompt)
	if err != nil {
		return err
	}
	return s.Update(ctx, kind, namespace, name, updated, prompt.ObjectMeta.ResourceVersion)
}

func (s *latestOnlyPromptRevisionStore) ListRevisions(ctx context.Context, kind, namespace, name string) ([]store.Revision, error) {
	revisions, err := s.revisionMemStore.ListRevisions(ctx, kind, namespace, name)
	if err != nil || len(revisions) == 0 {
		return revisions, err
	}
	return revisions[len(revisions)-1:], nil
}

var errPromptAgentUpdate = errors.New("transient agent update failure")

func (s *promptAgentUpdateFailureStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "Agent" && s.failNextAgentUpdate {
		s.failNextAgentUpdate = false
		return errPromptAgentUpdate
	}
	return s.revisionMemStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *promptAgentConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "Agent" && !s.conflicted {
		raw, err := s.revisionMemStore.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var latest v1alpha1.Agent
		if err := json.Unmarshal(raw, &latest); err != nil {
			return err
		}
		replicas := int32(4)
		latest.ObjectMeta.Labels = map[string]string{"concurrent": "preserved"}
		latest.Spec.Replicas = &replicas
		latest.Status.Phase = v1alpha1.PhaseRunning
		latest.Status.Replicas = 4
		latest.Status.ReadyReplicas = 3
		latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{
			Type: "ConcurrentReady", Status: "True", Reason: "ConcurrentUpdate",
		})
		updated, err := json.Marshal(latest)
		if err != nil {
			return err
		}
		if err := s.revisionMemStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
			return err
		}
		s.conflicted = true
		return fmt.Errorf("wrapped agent conflict: %w", store.ErrConflict)
	}
	return s.revisionMemStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func makePrompt(ns, name, content string) v1alpha1.Prompt {
	return v1alpha1.Prompt{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Prompt"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "prompt-uid-001",
		},
		Spec: v1alpha1.PromptSpec{Content: content},
	}
}

func makePromptRefAgent(ns, name, promptName string) v1alpha1.Agent {
	one := int32(1)
	return v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "agent-uid-prompt-ref",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef: v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{
				Ref: &v1alpha1.LocalObjectReference{Name: promptName},
			},
			Replicas: &one,
		},
	}
}

func TestPromptReconciler_WrappedNotFoundIsComplete(t *testing.T) {
	s := &promptGetFaultStore{
		revisionMemStore: newRevisionMemStore(),
		err:              fmt.Errorf("wrapped missing prompt: %w", store.ErrNotFound),
	}
	r := NewPromptReconciler(s, zaptest.NewLogger(t))
	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "missing", Kind: "Prompt"})
	if err != nil || result != (Result{}) {
		t.Fatalf("result = %+v, error = %v, want completed not-found reconciliation", result, err)
	}
}

// TC-0032: Prompt version history remains retrievable across updates.
func TestPromptReconciler_ComputesHashAndVersionHistory(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	r := NewPromptReconciler(s, zaptest.NewLogger(t))

	prompt := makePrompt("default", "assistant", "hello world")
	data, _ := json.Marshal(prompt)
	if err := s.Create(ctx, "Prompt", "default", "assistant", data); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile prompt (1): %v", err)
	}

	raw, _ := s.Get(ctx, "Prompt", "default", "assistant")
	var got v1alpha1.Prompt
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal prompt: %v", err)
	}
	if got.Status.Version != 1 {
		t.Fatalf("version = %d, want 1", got.Status.Version)
	}
	if got.Status.Hash == "" {
		t.Fatal("expected non-empty prompt hash")
	}

	revs, _ := s.ListRevisions(ctx, "Prompt", "default", "assistant")
	if len(revs) != 1 {
		t.Fatalf("revision count = %d, want 1", len(revs))
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile prompt (idempotent): %v", err)
	}
	revs, _ = s.ListRevisions(ctx, "Prompt", "default", "assistant")
	if len(revs) != 1 {
		t.Fatalf("revision count after idempotent reconcile = %d, want 1", len(revs))
	}

	raw, _ = s.Get(ctx, "Prompt", "default", "assistant")
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal prompt after idempotent reconcile: %v", err)
	}
	oldHash := got.Status.Hash

	got.Spec.Content = "hello world updated"
	data, _ = json.Marshal(got)
	if err := s.Update(ctx, "Prompt", "default", "assistant", data, got.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update prompt content: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile prompt (changed): %v", err)
	}

	raw, _ = s.Get(ctx, "Prompt", "default", "assistant")
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal changed prompt: %v", err)
	}
	if got.Status.Version != 2 {
		t.Fatalf("version = %d, want 2", got.Status.Version)
	}
	if got.Status.Hash == oldHash {
		t.Fatal("expected prompt hash to change after content update")
	}

	revs, _ = s.ListRevisions(ctx, "Prompt", "default", "assistant")
	if len(revs) != 2 {
		t.Fatalf("revision count after content change = %d, want 2", len(revs))
	}
}

func TestPromptReconciler_RetriesStatusAfterRevisionUpdatesParent(t *testing.T) {
	ctx := context.Background()
	s := &parentUpdatingPromptRevisionStore{revisionMemStore: newRevisionMemStore()}
	r := NewPromptReconciler(s, zaptest.NewLogger(t))

	prompt := makePrompt("default", "assistant", "hello world")
	data, _ := json.Marshal(prompt)
	if err := s.Create(ctx, "Prompt", "default", "assistant", data); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile prompt: %v", err)
	}

	raw, err := s.Get(ctx, "Prompt", "default", "assistant")
	if err != nil {
		t.Fatalf("get prompt: %v", err)
	}
	var got v1alpha1.Prompt
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal prompt: %v", err)
	}
	if got.Status.Version != 1 || got.Status.Hash == "" {
		t.Fatalf("status = %#v, want version 1 with hash", got.Status)
	}
	if got.ObjectMeta.Annotations["kognition.dev/revision-count"] != "1" {
		t.Fatalf("revision annotation was not preserved: %#v", got.ObjectMeta.Annotations)
	}
}

func TestPromptReconciler_IncrementsFromLatestRevisionNumber(t *testing.T) {
	ctx := context.Background()
	s := &latestOnlyPromptRevisionStore{revisionMemStore: newRevisionMemStore()}
	r := NewPromptReconciler(s, zaptest.NewLogger(t))

	prompt := makePrompt("default", "assistant", "new content")
	data, _ := json.Marshal(prompt)
	if err := s.Create(ctx, "Prompt", "default", "assistant", data); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	if err := s.CreateRevision(ctx, "Prompt", "default", "assistant", 5, []byte(`{"content":"old content"}`), nil); err != nil {
		t.Fatalf("seed latest revision: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile prompt: %v", err)
	}
	revisions := s.revisionMemStore.revisions["Prompt/default/assistant"]
	if got := revisions[len(revisions)-1].Revision; got != 6 {
		t.Fatalf("new revision = %d, want 6", got)
	}
}

// TC-0031: Prompt changes mark referencing agents with DriftDetected until re-reconciled.
func TestPromptReconciler_MarksReferencingAgentsAsDriftDetected(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	r := NewPromptReconciler(s, zaptest.NewLogger(t))

	prompt := makePrompt("default", "assistant", "base content")
	data, _ := json.Marshal(prompt)
	if err := s.Create(ctx, "Prompt", "default", "assistant", data); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}

	agent := makePromptRefAgent("default", "agent-a", "assistant")
	data, _ = json.Marshal(agent)
	if err := s.Create(ctx, "Agent", "default", "agent-a", data); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile prompt initial: %v", err)
	}

	raw, _ := s.Get(ctx, "Agent", "default", "agent-a")
	var initialAgent v1alpha1.Agent
	if err := json.Unmarshal(raw, &initialAgent); err != nil {
		t.Fatalf("unmarshal initial agent: %v", err)
	}
	for _, c := range initialAgent.Status.Conditions {
		if c.Type == "DriftDetected" && c.Status == "True" {
			t.Fatalf("did not expect DriftDetected=True on initial prompt version: %+v", c)
		}
	}

	raw, _ = s.Get(ctx, "Prompt", "default", "assistant")
	var storedPrompt v1alpha1.Prompt
	if err := json.Unmarshal(raw, &storedPrompt); err != nil {
		t.Fatalf("unmarshal prompt: %v", err)
	}
	storedPrompt.Spec.Content = "changed content"
	data, _ = json.Marshal(storedPrompt)
	if err := s.Update(ctx, "Prompt", "default", "assistant", data, storedPrompt.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update prompt: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile prompt changed: %v", err)
	}

	raw, _ = s.Get(ctx, "Agent", "default", "agent-a")
	var updatedAgent v1alpha1.Agent
	if err := json.Unmarshal(raw, &updatedAgent); err != nil {
		t.Fatalf("unmarshal updated agent: %v", err)
	}

	found := false
	for _, c := range updatedAgent.Status.Conditions {
		if c.Type == "DriftDetected" {
			found = true
			if c.Status != "True" {
				t.Fatalf("DriftDetected status = %s, want True", c.Status)
			}
			if c.Reason != "PromptChanged" {
				t.Fatalf("DriftDetected reason = %s, want PromptChanged", c.Reason)
			}
		}
	}
	if !found {
		t.Fatal("expected DriftDetected condition to be set")
	}
}

func TestPromptReconciler_DriftPropagationSkipsMalformedAndUnrelatedAgents(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	r := NewPromptReconciler(s, zaptest.NewLogger(t))
	prompt := makePrompt("default", "assistant", "base content")
	data, err := json.Marshal(prompt)
	if err != nil {
		t.Fatalf("marshal prompt: %v", err)
	}
	if err := s.Create(ctx, "Prompt", prompt.ObjectMeta.Namespace, prompt.ObjectMeta.Name, data); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	if _, err := r.Reconcile(ctx, Request{Namespace: prompt.ObjectMeta.Namespace, Name: prompt.ObjectMeta.Name, Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile initial prompt: %v", err)
	}

	referencing := makePromptRefAgent("default", "referencing", prompt.ObjectMeta.Name)
	unrelated := makePromptRefAgent("default", "unrelated", "other-prompt")
	for _, agent := range []v1alpha1.Agent{referencing, unrelated} {
		data, err := json.Marshal(agent)
		if err != nil {
			t.Fatalf("marshal Agent %s: %v", agent.ObjectMeta.Name, err)
		}
		if err := s.Create(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, data); err != nil {
			t.Fatalf("seed Agent %s: %v", agent.ObjectMeta.Name, err)
		}
	}
	if err := s.Create(ctx, "Agent", "default", "malformed", []byte(`{"spec":`)); err != nil {
		t.Fatalf("seed malformed Agent: %v", err)
	}

	raw, err := s.Get(ctx, "Prompt", prompt.ObjectMeta.Namespace, prompt.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get prompt: %v", err)
	}
	var storedPrompt v1alpha1.Prompt
	if err := json.Unmarshal(raw, &storedPrompt); err != nil {
		t.Fatalf("unmarshal prompt: %v", err)
	}
	storedPrompt.Spec.Content = "changed content"
	data, err = json.Marshal(storedPrompt)
	if err != nil {
		t.Fatalf("marshal changed prompt: %v", err)
	}
	if err := s.Update(ctx, "Prompt", storedPrompt.ObjectMeta.Namespace, storedPrompt.ObjectMeta.Name, data, storedPrompt.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update prompt: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: prompt.ObjectMeta.Namespace, Name: prompt.ObjectMeta.Name, Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile changed prompt: %v", err)
	}
	for _, tc := range []struct {
		name      string
		wantDrift bool
	}{
		{name: referencing.ObjectMeta.Name, wantDrift: true},
		{name: unrelated.ObjectMeta.Name, wantDrift: false},
	} {
		raw, err := s.Get(ctx, "Agent", "default", tc.name)
		if err != nil {
			t.Fatalf("get Agent %s: %v", tc.name, err)
		}
		var agent v1alpha1.Agent
		if err := json.Unmarshal(raw, &agent); err != nil {
			t.Fatalf("unmarshal Agent %s: %v", tc.name, err)
		}
		condition := findCondition(agent.Status.Conditions, "DriftDetected")
		if got := condition != nil && condition.Status == "True"; got != tc.wantDrift {
			t.Fatalf("Agent %s DriftDetected = %#v, want true = %t", tc.name, condition, tc.wantDrift)
		}
	}
}

func TestPromptReconciler_RetriesDriftPropagationAfterAgentUpdateFailure(t *testing.T) {
	ctx := context.Background()
	baseStore := newRevisionMemStore()
	s := &promptAgentUpdateFailureStore{revisionMemStore: baseStore}
	r := NewPromptReconciler(s, zaptest.NewLogger(t))

	prompt := makePrompt("default", "assistant", "base content")
	data, _ := json.Marshal(prompt)
	if err := baseStore.Create(ctx, "Prompt", "default", "assistant", data); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	agent := makePromptRefAgent("default", "agent-a", "assistant")
	data, _ = json.Marshal(agent)
	if err := baseStore.Create(ctx, "Agent", "default", "agent-a", data); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile initial prompt: %v", err)
	}

	raw, _ := baseStore.Get(ctx, "Prompt", "default", "assistant")
	var storedPrompt v1alpha1.Prompt
	if err := json.Unmarshal(raw, &storedPrompt); err != nil {
		t.Fatalf("unmarshal prompt: %v", err)
	}
	storedPrompt.Spec.Content = "changed content"
	data, _ = json.Marshal(storedPrompt)
	if err := baseStore.Update(ctx, "Prompt", "default", "assistant", data, storedPrompt.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update prompt: %v", err)
	}

	s.failNextAgentUpdate = true
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err == nil {
		t.Fatal("expected transient agent update failure")
	}
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err != nil {
		t.Fatalf("retry changed prompt: %v", err)
	}

	raw, _ = baseStore.Get(ctx, "Agent", "default", "agent-a")
	var got v1alpha1.Agent
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal agent: %v", err)
	}
	condition := findCondition(got.Status.Conditions, "DriftDetected")
	if condition == nil || condition.Status != "True" || condition.Reason != "PromptChanged" {
		t.Fatalf("DriftDetected condition = %#v, want True/PromptChanged", condition)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile completed propagation: %v", err)
	}
	raw, _ = baseStore.Get(ctx, "Agent", "default", "agent-a")
	var stable v1alpha1.Agent
	if err := json.Unmarshal(raw, &stable); err != nil {
		t.Fatalf("unmarshal stable agent: %v", err)
	}
	if stable.ObjectMeta.ResourceVersion != got.ObjectMeta.ResourceVersion {
		t.Fatalf("agent resource version changed from %d to %d after propagation completed", got.ObjectMeta.ResourceVersion, stable.ObjectMeta.ResourceVersion)
	}
}

func TestPromptReconciler_AgentConflictPreservesConcurrentState(t *testing.T) {
	ctx := context.Background()
	baseStore := newRevisionMemStore()
	s := &promptAgentConflictStore{revisionMemStore: baseStore}
	r := NewPromptReconciler(s, zaptest.NewLogger(t))

	prompt := makePrompt("default", "assistant", "base content")
	data, _ := json.Marshal(prompt)
	if err := baseStore.Create(ctx, "Prompt", "default", "assistant", data); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	agent := makePromptRefAgent("default", "agent-a", "assistant")
	data, _ = json.Marshal(agent)
	if err := baseStore.Create(ctx, "Agent", "default", "agent-a", data); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile initial prompt: %v", err)
	}
	raw, err := baseStore.Get(ctx, "Prompt", "default", "assistant")
	if err != nil {
		t.Fatalf("get prompt: %v", err)
	}
	var storedPrompt v1alpha1.Prompt
	if err := json.Unmarshal(raw, &storedPrompt); err != nil {
		t.Fatalf("unmarshal prompt: %v", err)
	}
	storedPrompt.Spec.Content = "changed content"
	data, _ = json.Marshal(storedPrompt)
	if err := baseStore.Update(ctx, "Prompt", "default", "assistant", data, storedPrompt.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update prompt: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile changed prompt: %v", err)
	}
	raw, err = baseStore.Get(ctx, "Agent", "default", "agent-a")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	var stored v1alpha1.Agent
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal agent: %v", err)
	}
	if !s.conflicted {
		t.Fatal("expected Agent update conflict")
	}
	if stored.ObjectMeta.Labels["concurrent"] != "preserved" || stored.Spec.Replicas == nil || *stored.Spec.Replicas != 4 {
		t.Fatalf("concurrent metadata or spec lost: metadata=%#v spec=%#v", stored.ObjectMeta, stored.Spec)
	}
	if stored.Status.Phase != v1alpha1.PhaseRunning || stored.Status.Replicas != 4 || stored.Status.ReadyReplicas != 3 {
		t.Fatalf("concurrent status lost: %#v", stored.Status)
	}
	var driftDetected, concurrentReady bool
	for _, condition := range stored.Status.Conditions {
		driftDetected = driftDetected || condition.Type == "DriftDetected" && condition.Status == "True"
		concurrentReady = concurrentReady || condition.Type == "ConcurrentReady" && condition.Status == "True"
	}
	if !driftDetected || !concurrentReady {
		t.Fatalf("merged conditions = %#v, want DriftDetected and ConcurrentReady", stored.Status.Conditions)
	}
}
