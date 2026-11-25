// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"reflect"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// ---------------------------------------------------------------------------
// revisionMemStore — extends memStore with revision tracking for reconciler tests
// ---------------------------------------------------------------------------

type revisionMemStore struct {
	*memStore
	revisions map[string][]store.Revision // "kind/namespace/name" -> revisions
}

type agentConflictStore struct {
	*revisionMemStore
	conflicted bool
}

type agentInstanceDeleteFaultStore struct {
	*revisionMemStore
	err error
}

type agentIdentityCreateRaceStore struct {
	*revisionMemStore
	staleIdentity []byte
	raced         bool
}

type failOnceDeploymentCleaner struct {
	calls     int
	agentName string
	namespace string
}

func (c *failOnceDeploymentCleaner) CleanupAgentResources(_ context.Context, agentName, namespace string) error {
	c.calls++
	c.agentName = agentName
	c.namespace = namespace
	if c.calls == 1 {
		return errors.New("transient cleanup failure")
	}
	return nil
}

func (s *agentInstanceDeleteFaultStore) Delete(ctx context.Context, kind, namespace, name string) error {
	if kind == agentInstanceKind {
		return s.err
	}
	return s.revisionMemStore.Delete(ctx, kind, namespace, name)
}

func (s *agentIdentityCreateRaceStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	items, err := s.revisionMemStore.List(ctx, kind, opts)
	if kind != agentIdentityKind || !s.raced {
		return items, err
	}
	return nil, err
}

func (s *agentIdentityCreateRaceStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if kind != agentIdentityKind || s.raced {
		return s.revisionMemStore.Create(ctx, kind, namespace, name, data)
	}
	s.raced = true
	if err := s.revisionMemStore.Create(ctx, kind, namespace, name, s.staleIdentity); err != nil {
		return err
	}
	return store.ErrAlreadyExists
}

func (s *agentConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "Agent" && !s.conflicted {
		raw, err := s.revisionMemStore.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var latest v1alpha1.Agent
		if err := json.Unmarshal(raw, &latest); err != nil {
			return err
		}
		latest.ObjectMeta.Finalizers = append(latest.ObjectMeta.Finalizers, "example.com/foreign")
		latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{
			Type: "ToolsReady", Status: "True", Reason: "ConcurrentUpdate",
		})
		updated, err := json.Marshal(latest)
		if err != nil {
			return err
		}
		if err := s.revisionMemStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
			return err
		}
		s.conflicted = true
		return store.ErrConflict
	}
	return s.revisionMemStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func newRevisionMemStore() *revisionMemStore {
	return &revisionMemStore{
		memStore:  newMemStore(),
		revisions: make(map[string][]store.Revision),
	}
}

func (r *revisionMemStore) CreateRevision(_ context.Context, kind, namespace, name string, revision int64, spec, data []byte) error {
	k := kind + "/" + namespace + "/" + name
	r.revisions[k] = append(r.revisions[k], store.Revision{
		Revision:  revision,
		Spec:      spec,
		Data:      data,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	return nil
}

func (r *revisionMemStore) ListRevisions(_ context.Context, kind, namespace, name string) ([]store.Revision, error) {
	k := kind + "/" + namespace + "/" + name
	return r.revisions[k], nil
}

func (r *revisionMemStore) GetRevision(_ context.Context, kind, namespace, name string, rev int64) (*store.Revision, error) {
	k := kind + "/" + namespace + "/" + name
	for _, rv := range r.revisions[k] {
		if rv.Revision == rev {
			return &rv, nil
		}
	}
	return nil, store.ErrNotFound
}

// ---------------------------------------------------------------------------
// Helper to build a valid Agent
// ---------------------------------------------------------------------------

func makeAgent(ns, name string, replicas int32) v1alpha1.Agent {
	return v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "Agent",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "agent-uid-001",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{Inline: "You are a helpful agent."},
			Replicas:     &replicas,
		},
	}
}

func TestAgentReconciler_ConflictPreservesConcurrentSharedState(t *testing.T) {
	ctx := context.Background()
	baseStore := newRevisionMemStore()
	s := &agentConflictStore{revisionMemStore: baseStore}
	reconciler := NewAgentReconciler(s, zaptest.NewLogger(t))

	agent := makeAgent("default", "conflict-agent", 1)
	data, _ := json.Marshal(agent)
	if err := baseStore.Create(ctx, "Agent", "default", "conflict-agent", data); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	result, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "conflict-agent", Kind: "Agent"})
	if err != nil {
		t.Fatalf("reconcile after conflict: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	raw, err := baseStore.Get(ctx, "Agent", "default", "conflict-agent")
	if err != nil {
		t.Fatalf("get updated agent: %v", err)
	}
	var updated v1alpha1.Agent
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal updated agent: %v", err)
	}
	if !hasFinalizer(updated.ObjectMeta.Finalizers, AgentFinalizer) {
		t.Fatal("expected Agent finalizer to be present")
	}
	if !hasFinalizer(updated.ObjectMeta.Finalizers, "example.com/foreign") {
		t.Fatal("concurrent foreign finalizer was lost")
	}
	if condition := findCondition(updated.Status.Conditions, "ToolsReady"); condition == nil || condition.Reason != "ConcurrentUpdate" {
		t.Fatalf("concurrent ToolsReady condition was lost: %#v", condition)
	}
}

// ---------------------------------------------------------------------------
// TC-0017: Valid Agent → reconciler creates AgentInstances → Ready phase
// ---------------------------------------------------------------------------

func TestAgentReconciler_CreatesInstances(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewAgentReconciler(s, logger)

	// Seed an Agent in the store.
	replicas := int32(2)
	// TC-0011: Agent transitions toward ready state as instances are created.
	// TC-0021: Agent reconciler restores desired replicas when instances are missing.
	agent := makeAgent("default", "my-agent", replicas)
	data, _ := json.Marshal(agent)
	if err := s.Create(ctx, "Agent", "default", "my-agent", data); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// First reconcile: adds finalizer, returns requeue.
	res, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "my-agent", Kind: "Agent"})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if !res.Requeue {
		t.Error("expected requeue after adding finalizer")
	}

	// Verify finalizer was added.
	raw, _ := s.Get(ctx, "Agent", "default", "my-agent")
	var updated v1alpha1.Agent
	json.Unmarshal(raw, &updated)
	if !hasFinalizer(updated.ObjectMeta.Finalizers, AgentFinalizer) {
		t.Error("expected finalizer to be present")
	}

	// Second reconcile: creates revision and instances.
	_, err = reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "my-agent", Kind: "Agent"})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	// Verify AgentInstances were created.
	instances, _ := s.List(ctx, "AgentInstance", store.ListOptions{Namespace: "default"})
	if len(instances) != int(replicas) {
		t.Fatalf("expected %d instances, got %d", replicas, len(instances))
	}

	identityData, err := s.Get(ctx, "AgentIdentity", "default", defaultAgentIdentityName("my-agent"))
	if err != nil {
		t.Fatalf("expected automatic AgentIdentity: %v", err)
	}
	var identity v1alpha1.AgentIdentity
	if err := json.Unmarshal(identityData, &identity); err != nil {
		t.Fatal(err)
	}
	if identity.Spec.AgentRef.Name != "my-agent" || identity.ObjectMeta.Labels[autoAgentIdentityLabel] != "my-agent" {
		t.Fatalf("automatic identity = %#v", identity)
	}

	// Verify each instance references the agent.
	for _, raw := range instances {
		var inst v1alpha1.AgentInstance
		json.Unmarshal(raw, &inst)
		if inst.Spec.AgentRef.Name != "my-agent" {
			t.Errorf("instance agentRef = %q, want %q", inst.Spec.AgentRef.Name, "my-agent")
		}
		if inst.Spec.Revision != 1 {
			t.Errorf("instance revision = %d, want 1", inst.Spec.Revision)
		}
		if len(inst.ObjectMeta.OwnerReferences) != 1 || !inst.ObjectMeta.OwnerReferences[0].Controller {
			t.Fatalf("instance ownerReferences = %#v, want Agent controller owner", inst.ObjectMeta.OwnerReferences)
		}
	}

	// Verify agent status was updated.
	raw, _ = s.Get(ctx, "Agent", "default", "my-agent")
	json.Unmarshal(raw, &updated)
	if updated.Status.Replicas != replicas {
		t.Errorf("status.replicas = %d, want %d", updated.Status.Replicas, replicas)
	}
	if updated.Status.RevisionCount != 1 {
		t.Errorf("status.revisionCount = %d, want 1", updated.Status.RevisionCount)
	}
}

func TestAgentReconciler_WaitsForAutomaticIdentityReadiness(t *testing.T) {
	ctx := context.Background()
	memoryStore := newRevisionMemStore()
	agentReconciler := NewAgentReconciler(memoryStore, zaptest.NewLogger(t))
	identityReconciler := NewAgentIdentityReconciler(memoryStore, zaptest.NewLogger(t))
	agent := makeAgent("default", "identity-gated", 1)
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Create(ctx, "Agent", "default", agent.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	request := Request{Namespace: "default", Name: agent.ObjectMeta.Name, Kind: "Agent"}
	if result, err := agentReconciler.Reconcile(ctx, request); err != nil || !result.Requeue {
		t.Fatalf("add Agent finalizer result = %+v, err = %v", result, err)
	}
	if _, err := agentReconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("create automatic identity and replica: %v", err)
	}
	instances, _ := memoryStore.List(ctx, agentInstanceKind, store.ListOptions{Namespace: "default"})
	if len(instances) != 1 {
		t.Fatalf("instances before identity readiness = %d, want 1", len(instances))
	}
	var pendingInstance v1alpha1.AgentInstance
	if err := json.Unmarshal(instances[0], &pendingInstance); err != nil {
		t.Fatal(err)
	}
	if pendingInstance.ObjectMeta.Annotations[requireAgentIdentityAnnotation] != "true" || pendingInstance.ObjectMeta.Annotations[autoAgentIdentityNameAnnotation] != defaultAgentIdentityName(agent.ObjectMeta.Name) {
		t.Fatalf("automatic instance annotations = %#v", pendingInstance.ObjectMeta.Annotations)
	}
	identityRequest := Request{Namespace: "default", Name: defaultAgentIdentityName(agent.ObjectMeta.Name), Kind: agentIdentityKind}
	if _, err := identityReconciler.Reconcile(ctx, identityRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := identityReconciler.Reconcile(ctx, identityRequest); err != nil {
		t.Fatal(err)
	}
}

func TestAgentReconciler_RejectsManualIdentityAtAutomaticIdentityName(t *testing.T) {
	ctx := context.Background()
	memoryStore := newRevisionMemStore()
	reconciler := NewAgentReconciler(memoryStore, zaptest.NewLogger(t))
	agent := makeAgent("default", "planner", 0)
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Create(ctx, "Agent", "default", agent.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	manualIdentity := v1alpha1.AgentIdentity{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: agentIdentityKind},
		ObjectMeta: v1alpha1.ObjectMeta{Name: defaultAgentIdentityName("planner"), Namespace: "default"},
		Spec:       v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}},
	}
	data, err = json.Marshal(manualIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Create(ctx, agentIdentityKind, "default", manualIdentity.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	request := Request{Namespace: "default", Name: "planner", Kind: "Agent"}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err == nil || !strings.Contains(err.Error(), "reserved for automatic provisioning") {
		t.Fatalf("manual identity collision error = %v", err)
	}
}

func TestAgentIdentityDependencyHandlerRequeuesNamespaceAgents(t *testing.T) {
	memoryStore := newMemStore()
	for _, agent := range []v1alpha1.Agent{
		makeAgent("tenant-a", "planner", 1),
		makeAgent("tenant-a", "writer", 1),
		makeAgent("tenant-b", "other", 1),
	} {
		data, err := json.Marshal(agent)
		if err != nil {
			t.Fatal(err)
		}
		if err := memoryStore.Create(context.Background(), "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, data); err != nil {
			t.Fatal(err)
		}
	}
	recorder := &intentGatewayEventRecorder{}
	handler := NewAgentIdentityDependencyHandler(memoryStore, recorder, zaptest.NewLogger(t))
	handler.OnDelete(KeyFunc("tenant-a", "agent-identity-planner"))
	want := map[string]bool{KeyFunc("tenant-a", "planner"): true, KeyFunc("tenant-a", "writer"): true}
	got := make(map[string]bool, len(recorder.updates))
	for _, update := range recorder.updates {
		got[update] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("identity dependency updates = %v, want %v", recorder.updates, want)
	}
}

func TestAgentReconciler_WaitsForTerminatingAutomaticIdentityBeforeSameNameRecreation(t *testing.T) {
	ctx := context.Background()
	store := newRevisionMemStore()
	reconciler := NewAgentReconciler(store, zaptest.NewLogger(t))
	current := makeAgent("default", "planner", 0)
	current.ObjectMeta.UID = "current-agent-uid"
	data, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, "Agent", "default", current.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	deletingAt := time.Now().UTC()
	staleIdentity := v1alpha1.AgentIdentity{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: agentIdentityKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              defaultAgentIdentityName("planner"),
			Namespace:         "default",
			DeletionTimestamp: &deletingAt,
			Labels:            map[string]string{autoAgentIdentityLabel: "planner"},
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       "Agent",
				Name:       "planner",
				UID:        "previous-agent-uid",
			}},
		},
		Spec: v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}},
	}
	data, err = json.Marshal(staleIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, agentIdentityKind, "default", staleIdentity.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	request := Request{Namespace: "default", Name: "planner", Kind: "Agent"}
	if result, err := reconciler.Reconcile(ctx, request); err != nil || !result.Requeue {
		t.Fatalf("add Agent finalizer result = %+v, err = %v", result, err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err == nil || !strings.Contains(err.Error(), "is deleting") {
		t.Fatalf("reconcile with terminating identity error = %v", err)
	}
	if err := store.Delete(ctx, agentIdentityKind, "default", staleIdentity.ObjectMeta.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile after identity cleanup: %v", err)
	}
	identityData, err := store.Get(ctx, agentIdentityKind, "default", defaultAgentIdentityName("planner"))
	if err != nil {
		t.Fatal(err)
	}
	var identity v1alpha1.AgentIdentity
	if err := json.Unmarshal(identityData, &identity); err != nil {
		t.Fatal(err)
	}
	if !identityOwnedByAgent(&identity, &current) {
		t.Fatalf("replacement identity ownerReferences = %#v, want current Agent UID", identity.ObjectMeta.OwnerReferences)
	}
}

func TestAgentReconciler_RejectsStaleAutomaticIdentityAfterCreateRace(t *testing.T) {
	ctx := context.Background()
	baseStore := newRevisionMemStore()
	current := makeAgent("default", "planner", 0)
	current.ObjectMeta.UID = "current-agent-uid"
	data, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := baseStore.Create(ctx, "Agent", "default", current.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	staleIdentity := v1alpha1.AgentIdentity{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: agentIdentityKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      defaultAgentIdentityName("planner"),
			Namespace: "default",
			Labels:    map[string]string{autoAgentIdentityLabel: "planner"},
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       "Agent",
				Name:       "planner",
				UID:        "previous-agent-uid",
			}},
		},
		Spec: v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}},
	}
	data, err = json.Marshal(staleIdentity)
	if err != nil {
		t.Fatal(err)
	}
	store := &agentIdentityCreateRaceStore{revisionMemStore: baseStore, staleIdentity: data}
	reconciler := NewAgentReconciler(store, zaptest.NewLogger(t))
	request := Request{Namespace: "default", Name: "planner", Kind: "Agent"}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err == nil || !strings.Contains(err.Error(), "previous Agent instance") {
		t.Fatalf("reconcile with stale create-race identity error = %v", err)
	}
	if !store.raced {
		t.Fatal("automatic identity create race was not exercised")
	}
}

// ---------------------------------------------------------------------------
// TC-0019: Update spec → new revision + new instances + old drained
// ---------------------------------------------------------------------------

func TestAgentReconciler_SpecChange_CreatesNewRevision(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewAgentReconciler(s, logger)

	// Seed an Agent.
	replicas := int32(1)
	// TC-0013: Spec update creates a new immutable revision and drains old instances.
	// TC-0014: Existing revision snapshots remain immutable and non-mutated by spec updates.
	// TC-0199: Update Agent spec, verify reconciler detects drift and triggers rolling update.
	agent := makeAgent("default", "rev-agent", replicas)
	data, _ := json.Marshal(agent)
	s.Create(ctx, "Agent", "default", "rev-agent", data)

	// Reconcile to steady state (finalizer + revision 1 + 1 instance).
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "rev-agent", Kind: "Agent"})
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "rev-agent", Kind: "Agent"})

	// Verify revision 1 instance exists.
	instances, _ := s.List(ctx, "AgentInstance", store.ListOptions{Namespace: "default"})
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance after initial reconcile, got %d", len(instances))
	}

	var inst v1alpha1.AgentInstance
	json.Unmarshal(instances[0], &inst)
	if inst.Spec.Revision != 1 {
		t.Fatalf("expected revision 1, got %d", inst.Spec.Revision)
	}

	// Mutate the agent spec (change instructions).
	raw, _ := s.Get(ctx, "Agent", "default", "rev-agent")
	json.Unmarshal(raw, &agent)
	agent.Spec.Instructions.Inline = "Updated instructions"
	data, _ = json.Marshal(agent)
	s.Update(ctx, "Agent", "default", "rev-agent", data, agent.ObjectMeta.ResourceVersion)

	// Reconcile again — should create revision 2 and drain stale instances.
	_, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "rev-agent", Kind: "Agent"})
	if err != nil {
		t.Fatalf("reconcile after spec change: %v", err)
	}

	// Verify instances are now at revision 2.
	instances, _ = s.List(ctx, "AgentInstance", store.ListOptions{Namespace: "default"})
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance after revision change, got %d", len(instances))
	}

	json.Unmarshal(instances[0], &inst)
	if inst.Spec.Revision != 2 {
		t.Errorf("expected instance at revision 2, got %d", inst.Spec.Revision)
	}

	// Verify revision was recorded.
	revisions, _ := s.ListRevisions(ctx, "Agent", "default", "rev-agent")
	if len(revisions) != 2 {
		t.Errorf("expected 2 revisions, got %d", len(revisions))
	}
}

// ---------------------------------------------------------------------------
// Deletion: reconciler removes instances and finalizer
// ---------------------------------------------------------------------------

func TestAgentReconciler_Deletion_CleansUp(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewAgentReconciler(s, logger)

	// Seed and reconcile to steady state.
	replicas := int32(2)
	// TC-0012: Finalizer-mediated cleanup on delete.
	// TC-0200: Delete Agent, verify finalizer cleanup and cascaded AgentInstance deletion.
	agent := makeAgent("default", "del-agent", replicas)
	data, _ := json.Marshal(agent)
	s.Create(ctx, "Agent", "default", "del-agent", data)
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "del-agent", Kind: "Agent"})
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "del-agent", Kind: "Agent"})

	// Set deletion timestamp.
	raw, _ := s.Get(ctx, "Agent", "default", "del-agent")
	json.Unmarshal(raw, &agent)
	now := time.Now().UTC()
	agent.ObjectMeta.DeletionTimestamp = &now
	data, _ = json.Marshal(agent)
	s.Update(ctx, "Agent", "default", "del-agent", data, agent.ObjectMeta.ResourceVersion)

	// Reconcile deletion.
	_, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "del-agent", Kind: "Agent"})
	if err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}

	// Verify all instances deleted.
	instances, _ := s.List(ctx, "AgentInstance", store.ListOptions{Namespace: "default"})
	if len(instances) != 0 {
		t.Errorf("expected 0 instances after deletion, got %d", len(instances))
	}

	// Verify finalizer removed.
	raw, _ = s.Get(ctx, "Agent", "default", "del-agent")
	var afterDel v1alpha1.Agent
	json.Unmarshal(raw, &afterDel)
	if hasFinalizer(afterDel.ObjectMeta.Finalizers, AgentFinalizer) {
		t.Error("expected finalizer to be removed after deletion")
	}

	if _, err := s.Get(ctx, agentIdentityKind, "default", defaultAgentIdentityName("del-agent")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("automatic identity after deletion = %v, want not found", err)
	}
	if err := s.Delete(ctx, "Agent", "default", "del-agent"); err != nil {
		t.Fatalf("remove finalized Agent: %v", err)
	}

	// A same-name Agent must receive a fresh identity rather than inheriting
	// an object associated with the deleted Agent's UID.
	recreated := makeAgent("default", "del-agent", 1)
	recreated.ObjectMeta.UID = "agent-uid-recreated"
	data, _ = json.Marshal(recreated)
	if err := s.Create(ctx, "Agent", "default", recreated.ObjectMeta.Name, data); err != nil {
		t.Fatalf("recreate Agent: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: recreated.ObjectMeta.Name, Kind: "Agent"}); err != nil {
		t.Fatalf("reconcile recreated Agent finalizer: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: recreated.ObjectMeta.Name, Kind: "Agent"}); err != nil {
		t.Fatalf("reconcile recreated Agent identity: %v", err)
	}
	identityData, err := s.Get(ctx, agentIdentityKind, "default", defaultAgentIdentityName("del-agent"))
	if err != nil {
		t.Fatalf("get recreated Agent identity: %v", err)
	}
	var recreatedIdentity v1alpha1.AgentIdentity
	if err := json.Unmarshal(identityData, &recreatedIdentity); err != nil {
		t.Fatal(err)
	}
	if !identityOwnedByAgent(&recreatedIdentity, &recreated) {
		t.Fatalf("recreated identity ownerReferences = %#v, want new Agent UID", recreatedIdentity.ObjectMeta.OwnerReferences)
	}
}

func TestAgentReconciler_DeletionPreservesManualAgentIdentity(t *testing.T) {
	ctx := context.Background()
	store := newRevisionMemStore()
	reconciler := NewAgentReconciler(store, zaptest.NewLogger(t))
	agent := makeAgent("default", "planner", 0)
	agent.ObjectMeta.Finalizers = []string{AgentFinalizer}
	deletingAt := time.Now().UTC()
	agent.ObjectMeta.DeletionTimestamp = &deletingAt
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, "Agent", "default", agent.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	manualIdentity := v1alpha1.AgentIdentity{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: agentIdentityKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "manual-planner-identity",
			Namespace: "default",
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       "Agent",
				Name:       "planner",
				UID:        agent.ObjectMeta.UID,
			}},
		},
		Spec: v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}},
	}
	data, err = json.Marshal(manualIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, agentIdentityKind, "default", manualIdentity.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "planner", Kind: "Agent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, agentIdentityKind, "default", manualIdentity.ObjectMeta.Name); err != nil {
		t.Fatalf("manual AgentIdentity was deleted: %v", err)
	}
}

func TestAgentReconciler_DeletionRetriesDeploymentCleanupBeforeRemovingFinalizer(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	cleaner := &failOnceDeploymentCleaner{}
	reconciler := NewAgentReconciler(s, zaptest.NewLogger(t), WithDeploymentCleaner(cleaner))
	agent := makeAgent("default", "cleanup-retry", 1)
	agent.ObjectMeta.Finalizers = []string{AgentFinalizer}
	now := time.Now().UTC()
	agent.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("marshal agent: %v", err)
	}
	if err := s.Create(ctx, "Agent", "default", agent.ObjectMeta.Name, data); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	instance := v1alpha1.AgentInstance{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: agentInstanceKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "cleanup-retry-instance",
			Namespace: "default",
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       "Agent",
				Name:       agent.ObjectMeta.Name,
				UID:        agent.ObjectMeta.UID,
			}},
		},
	}
	instanceData, err := json.Marshal(instance)
	if err != nil {
		t.Fatalf("marshal instance: %v", err)
	}
	if err := s.Create(ctx, agentInstanceKind, "default", instance.ObjectMeta.Name, instanceData); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: agent.ObjectMeta.Name, Kind: "Agent"}); err == nil {
		t.Fatal("first deletion reconcile succeeded despite cleaner failure")
	}
	if _, err := s.Get(ctx, agentInstanceKind, "default", instance.ObjectMeta.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("owned instance Get() error = %v after partial cleanup, want ErrNotFound", err)
	}
	storedRaw, err := s.Get(ctx, "Agent", "default", agent.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get agent after failed cleanup: %v", err)
	}
	var stored v1alpha1.Agent
	if err := json.Unmarshal(storedRaw, &stored); err != nil {
		t.Fatalf("unmarshal agent after failed cleanup: %v", err)
	}
	if !hasFinalizer(stored.ObjectMeta.Finalizers, AgentFinalizer) {
		t.Fatal("Agent finalizer was removed after failed deployment cleanup")
	}
	if cleaner.agentName != agent.ObjectMeta.Name || cleaner.namespace != agent.ObjectMeta.Namespace {
		t.Fatalf("cleanup target = %s/%s, want %s/%s", cleaner.namespace, cleaner.agentName, agent.ObjectMeta.Namespace, agent.ObjectMeta.Name)
	}

	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: agent.ObjectMeta.Name, Kind: "Agent"}); err != nil {
		t.Fatalf("retry deletion reconcile: %v", err)
	}
	if cleaner.calls != 2 {
		t.Fatalf("deployment cleanup calls = %d, want 2", cleaner.calls)
	}
	storedRaw, err = s.Get(ctx, "Agent", "default", agent.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get agent after successful cleanup: %v", err)
	}
	stored = v1alpha1.Agent{}
	if err := json.Unmarshal(storedRaw, &stored); err != nil {
		t.Fatalf("unmarshal agent after successful cleanup: %v", err)
	}
	if hasFinalizer(stored.ObjectMeta.Finalizers, AgentFinalizer) {
		t.Fatal("Agent finalizer remained after successful deployment cleanup")
	}
}

// ---------------------------------------------------------------------------
// Agent not found — no error
// ---------------------------------------------------------------------------

func TestAgentReconciler_NotFound(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewAgentReconciler(s, logger)

	_, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "nonexistent", Kind: "Agent"})
	if err != nil {
		t.Fatalf("expected no error for missing agent, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Scale down: reduce replicas → excess instances deleted
// ---------------------------------------------------------------------------

func TestAgentReconciler_ScaleDown(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewAgentReconciler(s, logger)

	// Seed with 3 replicas and reconcile to steady state.
	agent := makeAgent("default", "scale-agent", 3)
	data, _ := json.Marshal(agent)
	s.Create(ctx, "Agent", "default", "scale-agent", data)
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "scale-agent", Kind: "Agent"}) // finalizer
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "scale-agent", Kind: "Agent"}) // create instances

	// Verify 3 instances exist.
	instances, _ := s.List(ctx, "AgentInstance", store.ListOptions{Namespace: "default"})
	if len(instances) != 3 {
		t.Fatalf("expected 3 instances, got %d", len(instances))
	}

	// Scale down to 1 replica.
	raw, _ := s.Get(ctx, "Agent", "default", "scale-agent")
	json.Unmarshal(raw, &agent)
	one := int32(1)
	agent.Spec.Replicas = &one
	data, _ = json.Marshal(agent)
	s.Update(ctx, "Agent", "default", "scale-agent", data, agent.ObjectMeta.ResourceVersion)

	// Reconcile the scale-down.
	_, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "scale-agent", Kind: "Agent"})
	if err != nil {
		t.Fatalf("reconcile scale-down: %v", err)
	}

	// Verify only 1 instance remains.
	instances, _ = s.List(ctx, "AgentInstance", store.ListOptions{Namespace: "default"})
	if len(instances) != 1 {
		t.Errorf("expected 1 instance after scale-down, got %d", len(instances))
	}
}

// ---------------------------------------------------------------------------
// Idempotency: reconcile unchanged state → no new revision
// ---------------------------------------------------------------------------

func TestAgentReconciler_Idempotent(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewAgentReconciler(s, logger)

	// Seed and reconcile to steady state.
	agent := makeAgent("default", "idem-agent", 1)
	data, _ := json.Marshal(agent)
	s.Create(ctx, "Agent", "default", "idem-agent", data)
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "idem-agent", Kind: "Agent"}) // finalizer
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "idem-agent", Kind: "Agent"}) // create

	// Record revision count.
	revisions, _ := s.ListRevisions(ctx, "Agent", "default", "idem-agent")
	revCount := len(revisions)

	// Reconcile again with no changes.
	_, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "idem-agent", Kind: "Agent"})
	if err != nil {
		t.Fatalf("idempotent reconcile: %v", err)
	}

	// Verify no new revision was created.
	revisions, _ = s.ListRevisions(ctx, "Agent", "default", "idem-agent")
	if len(revisions) != revCount {
		t.Errorf("expected %d revisions (idempotent), got %d", revCount, len(revisions))
	}

	// Verify instance count unchanged.
	instances, _ := s.List(ctx, "AgentInstance", store.ListOptions{Namespace: "default"})
	if len(instances) != 1 {
		t.Errorf("expected 1 instance (idempotent), got %d", len(instances))
	}
}

func TestAgentReconciler_UsesNextRevisionAfterLegacyHistory(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	agent := makeAgent("default", "legacy-agent", 1)
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("marshal Agent: %v", err)
	}
	if err := s.Create(ctx, "Agent", "default", "legacy-agent", data); err != nil {
		t.Fatalf("create Agent: %v", err)
	}
	if err := s.CreateRevision(ctx, "Agent", "default", "legacy-agent", 5, []byte(`{"spec":{"modelRef":{"name":"legacy"}}}`), nil); err != nil {
		t.Fatalf("create legacy revision: %v", err)
	}
	reconciler := NewAgentReconciler(s, zaptest.NewLogger(t))
	request := Request{Namespace: "default", Name: "legacy-agent", Kind: "Agent"}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile revised Agent: %v", err)
	}

	revisions, err := s.ListRevisions(ctx, "Agent", "default", "legacy-agent")
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	if len(revisions) != 2 || revisions[0].Revision != 5 || revisions[1].Revision != 6 {
		t.Fatalf("revisions = %#v, want legacy revision 5 followed by revision 6", revisions)
	}
}

func TestAgentReconciler_PromptChangeRollsRevisionAndClearsDrift(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	agentReconciler := NewAgentReconciler(s, zaptest.NewLogger(t))
	promptReconciler := NewPromptReconciler(s, zaptest.NewLogger(t))

	prompt := makePrompt("default", "assistant", "prompt-v1")
	data, _ := json.Marshal(prompt)
	if err := s.Create(ctx, "Prompt", "default", "assistant", data); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	if _, err := promptReconciler.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile prompt (v1): %v", err)
	}

	agent := makePromptRefAgent("default", "prompt-agent", "assistant")
	data, _ = json.Marshal(agent)
	if err := s.Create(ctx, "Agent", "default", "prompt-agent", data); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	if _, err := agentReconciler.Reconcile(ctx, Request{Namespace: "default", Name: "prompt-agent", Kind: "Agent"}); err != nil {
		t.Fatalf("reconcile agent (finalizer): %v", err)
	}
	if _, err := agentReconciler.Reconcile(ctx, Request{Namespace: "default", Name: "prompt-agent", Kind: "Agent"}); err != nil {
		t.Fatalf("reconcile agent (steady): %v", err)
	}

	revs, _ := s.ListRevisions(ctx, "Agent", "default", "prompt-agent")
	if len(revs) != 1 {
		t.Fatalf("agent revision count after initial reconcile = %d, want 1", len(revs))
	}

	raw, _ := s.Get(ctx, "Prompt", "default", "assistant")
	var storedPrompt v1alpha1.Prompt
	if err := json.Unmarshal(raw, &storedPrompt); err != nil {
		t.Fatalf("unmarshal prompt: %v", err)
	}
	storedPrompt.Spec.Content = "prompt-v2"
	data, _ = json.Marshal(storedPrompt)
	if err := s.Update(ctx, "Prompt", "default", "assistant", data, storedPrompt.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update prompt: %v", err)
	}

	if _, err := promptReconciler.Reconcile(ctx, Request{Namespace: "default", Name: "assistant", Kind: "Prompt"}); err != nil {
		t.Fatalf("reconcile prompt (v2): %v", err)
	}

	raw, _ = s.Get(ctx, "Agent", "default", "prompt-agent")
	var drifted v1alpha1.Agent
	if err := json.Unmarshal(raw, &drifted); err != nil {
		t.Fatalf("unmarshal drifted agent: %v", err)
	}
	if c := findCondition(drifted.Status.Conditions, "DriftDetected"); c == nil || c.Status != "True" {
		t.Fatalf("expected DriftDetected=True after prompt change, got %+v", c)
	}

	if _, err := agentReconciler.Reconcile(ctx, Request{Namespace: "default", Name: "prompt-agent", Kind: "Agent"}); err != nil {
		t.Fatalf("reconcile agent after prompt change: %v", err)
	}

	revs, _ = s.ListRevisions(ctx, "Agent", "default", "prompt-agent")
	if len(revs) != 2 {
		t.Fatalf("agent revision count after prompt change = %d, want 2", len(revs))
	}

	raw, _ = s.Get(ctx, "Agent", "default", "prompt-agent")
	var synced v1alpha1.Agent
	if err := json.Unmarshal(raw, &synced); err != nil {
		t.Fatalf("unmarshal synced agent: %v", err)
	}
	if c := findCondition(synced.Status.Conditions, "DriftDetected"); c == nil || c.Status != "False" || c.Reason != "PromptInSync" {
		t.Fatalf("expected DriftDetected=False PromptInSync after agent reconcile, got %+v", c)
	}
}

func TestAgentReconciler_PromptResolveFailedSetsDriftCondition(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	reconciler := NewAgentReconciler(s, zaptest.NewLogger(t))

	agent := makePromptRefAgent("default", "missing-prompt-agent", "does-not-exist")
	data, _ := json.Marshal(agent)
	if err := s.Create(ctx, "Agent", "default", "missing-prompt-agent", data); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "missing-prompt-agent", Kind: "Agent"}); err != nil {
		t.Fatalf("reconcile agent (finalizer): %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "missing-prompt-agent", Kind: "Agent"}); err != nil {
		t.Fatalf("reconcile agent (missing prompt): %v", err)
	}

	raw, _ := s.Get(ctx, "Agent", "default", "missing-prompt-agent")
	var updated v1alpha1.Agent
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal updated agent: %v", err)
	}

	c := findCondition(updated.Status.Conditions, "DriftDetected")
	if c == nil {
		t.Fatal("expected DriftDetected condition when prompt resolution fails")
	}
	if c.Status != "True" {
		t.Fatalf("DriftDetected status = %q, want True", c.Status)
	}
	if c.Reason != "PromptResolveFailed" {
		t.Fatalf("DriftDetected reason = %q, want PromptResolveFailed", c.Reason)
	}
}

// ---------------------------------------------------------------------------
// computePhase three-state logic
// ---------------------------------------------------------------------------

func TestComputePhase_ThreeStates(t *testing.T) {
	rec := &AgentReconciler{}
	tests := []struct {
		name           string
		readyCount     int32
		desiredReplica int32
		totalInstances int
		want           v1alpha1.Phase
	}{
		{"no instances yet → Pending", 0, 2, 0, v1alpha1.PhasePending},
		{"instances exist but none ready → Running", 0, 2, 2, v1alpha1.PhaseRunning},
		{"partial readiness → Running", 1, 2, 2, v1alpha1.PhaseRunning},
		{"all ready → Ready", 2, 2, 2, v1alpha1.PhaseReady},
		{"over-provisioned ready → Ready", 3, 2, 3, v1alpha1.PhaseReady},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rec.computePhase(tc.readyCount, tc.desiredReplica, tc.totalInstances)
			if got != tc.want {
				t.Errorf("computePhase(%d, %d, %d) = %q, want %q",
					tc.readyCount, tc.desiredReplica, tc.totalInstances, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Regression: Failed AgentInstances at the current revision must be replaced
// so the Agent can recover from transient dispatch errors (e.g. controller
// cache race on parent Agent lookup at startup). Without this, an Agent that
// hit a temporary dispatch failure would stay pod-less forever, and Studio
// preview would surface `no pods found for agent` on every attempt.
// ---------------------------------------------------------------------------

func disableFailedInstanceBackoff(t *testing.T) {
	t.Helper()
	prev := failedInstanceReplaceBackoff
	failedInstanceReplaceBackoff = 0
	t.Cleanup(func() { failedInstanceReplaceBackoff = prev })
}

func TestAgentReconciler_BacksOffReplacingFreshFailedInstance(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	reconciler := NewAgentReconciler(s, zaptest.NewLogger(t))

	agent := makeAgent("default", "backoff-agent", 1)
	data, _ := json.Marshal(agent)
	if err := s.Create(ctx, "Agent", "default", "backoff-agent", data); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	req := Request{Namespace: "default", Name: "backoff-agent", Kind: "Agent"}
	for i := 0; i < 2; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
	}

	rawList, _ := s.List(ctx, "AgentInstance", store.ListOptions{Namespace: "default"})
	if len(rawList) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(rawList))
	}
	var inst v1alpha1.AgentInstance
	if err := json.Unmarshal(rawList[0], &inst); err != nil {
		t.Fatalf("unmarshal instance: %v", err)
	}
	inst.Status.Phase = v1alpha1.PhaseFailed
	data, _ = json.Marshal(inst)
	if err := s.Update(ctx, "AgentInstance", "default", inst.ObjectMeta.Name, data, inst.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark instance failed: %v", err)
	}

	res, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile after failure: %v", err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > failedInstanceReplaceBackoff {
		t.Fatalf("RequeueAfter = %v, want within (0, %v]", res.RequeueAfter, failedInstanceReplaceBackoff)
	}
	rawList, _ = s.List(ctx, "AgentInstance", store.ListOptions{Namespace: "default"})
	if len(rawList) != 1 {
		t.Fatalf("instances = %d, want the failed one kept during backoff", len(rawList))
	}
	var kept v1alpha1.AgentInstance
	_ = json.Unmarshal(rawList[0], &kept)
	if kept.ObjectMeta.Name != inst.ObjectMeta.Name {
		t.Fatalf("failed instance was replaced during backoff: %q -> %q", inst.ObjectMeta.Name, kept.ObjectMeta.Name)
	}
}

func TestAgentReconciler_ReplacesFailedInstances(t *testing.T) {
	disableFailedInstanceBackoff(t)
	ctx := context.Background()
	s := newRevisionMemStore()
	reconciler := NewAgentReconciler(s, zaptest.NewLogger(t))

	// Seed and drive to steady state (finalizer + create instances).
	agent := makeAgent("default", "recover-agent", 1)
	data, _ := json.Marshal(agent)
	if err := s.Create(ctx, "Agent", "default", "recover-agent", data); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "recover-agent", Kind: "Agent"}); err != nil {
		t.Fatalf("reconcile 1 (finalizer): %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "recover-agent", Kind: "Agent"}); err != nil {
		t.Fatalf("reconcile 2 (create instances): %v", err)
	}

	// Verify one instance exists at revision 1.
	rawList, _ := s.List(ctx, "AgentInstance", store.ListOptions{Namespace: "default"})
	if len(rawList) != 1 {
		t.Fatalf("expected 1 instance after initial reconcile, got %d", len(rawList))
	}
	var initial v1alpha1.AgentInstance
	if err := json.Unmarshal(rawList[0], &initial); err != nil {
		t.Fatalf("unmarshal instance: %v", err)
	}
	initialName := initial.ObjectMeta.Name

	// Simulate dispatcher failure: mark the instance PhaseFailed at the same revision.
	initial.Status.Phase = v1alpha1.PhaseFailed
	initial.Status.DispatchAttempts = 4
	data, _ = json.Marshal(initial)
	if err := s.Update(ctx, "AgentInstance", "default", initialName, data, initial.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark instance failed: %v", err)
	}

	// Reconcile again — the Failed instance must be deleted AND a fresh one created.
	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "recover-agent", Kind: "Agent"}); err != nil {
		t.Fatalf("reconcile 3 (recover): %v", err)
	}

	// Failed instance must be gone; a new instance at revision 1 must exist.
	rawList, _ = s.List(ctx, "AgentInstance", store.ListOptions{Namespace: "default"})
	if len(rawList) != 1 {
		t.Fatalf("expected 1 instance after recovery, got %d", len(rawList))
	}
	var replaced v1alpha1.AgentInstance
	if err := json.Unmarshal(rawList[0], &replaced); err != nil {
		t.Fatalf("unmarshal replaced instance: %v", err)
	}
	if replaced.ObjectMeta.Name == initialName {
		t.Fatalf("expected replaced instance to have a new name, still got %q", initialName)
	}
	if replaced.Status.Phase == v1alpha1.PhaseFailed {
		t.Fatalf("replaced instance should not start in Failed phase, got %q", replaced.Status.Phase)
	}
	if replaced.Spec.Revision != 1 {
		t.Fatalf("replaced instance revision = %d, want 1", replaced.Spec.Revision)
	}
}

func TestAgentReconciler_InstanceReplacementSurfacesDeletionFailures(t *testing.T) {
	disableFailedInstanceBackoff(t)
	for _, tc := range []struct {
		name        string
		prepare     func(*testing.T, *revisionMemStore, v1alpha1.AgentInstance)
		wantMessage string
	}{
		{
			name: "stale revision",
			prepare: func(t *testing.T, s *revisionMemStore, _ v1alpha1.AgentInstance) {
				t.Helper()
				raw, err := s.Get(context.Background(), "Agent", "default", "replacement-failure")
				if err != nil {
					t.Fatalf("get Agent: %v", err)
				}
				var agent v1alpha1.Agent
				if err := json.Unmarshal(raw, &agent); err != nil {
					t.Fatalf("unmarshal Agent: %v", err)
				}
				agent.Spec.Instructions.Inline = "updated instructions"
				data, _ := json.Marshal(agent)
				if err := s.Update(context.Background(), "Agent", "default", agent.ObjectMeta.Name, data, agent.ObjectMeta.ResourceVersion); err != nil {
					t.Fatalf("update Agent: %v", err)
				}
			},
			wantMessage: "delete stale instance",
		},
		{
			name: "failed instance",
			prepare: func(t *testing.T, s *revisionMemStore, instance v1alpha1.AgentInstance) {
				t.Helper()
				instance.Status.Phase = v1alpha1.PhaseFailed
				data, _ := json.Marshal(instance)
				if err := s.Update(context.Background(), agentInstanceKind, "default", instance.ObjectMeta.Name, data, instance.ObjectMeta.ResourceVersion); err != nil {
					t.Fatalf("mark instance failed: %v", err)
				}
			},
			wantMessage: "delete failed instance",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			memory := newRevisionMemStore()
			reconciler := NewAgentReconciler(memory, zaptest.NewLogger(t))
			agent := makeAgent("default", "replacement-failure", 1)
			data, _ := json.Marshal(agent)
			if err := memory.Create(ctx, "Agent", "default", agent.ObjectMeta.Name, data); err != nil {
				t.Fatalf("seed Agent: %v", err)
			}
			if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: agent.ObjectMeta.Name, Kind: "Agent"}); err != nil {
				t.Fatalf("add finalizer: %v", err)
			}
			if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: agent.ObjectMeta.Name, Kind: "Agent"}); err != nil {
				t.Fatalf("create initial instance: %v", err)
			}
			rawInstances, err := memory.List(ctx, agentInstanceKind, store.ListOptions{Namespace: "default"})
			if err != nil || len(rawInstances) != 1 {
				t.Fatalf("initial instances = %d, err = %v, want one", len(rawInstances), err)
			}
			var instance v1alpha1.AgentInstance
			if err := json.Unmarshal(rawInstances[0], &instance); err != nil {
				t.Fatalf("unmarshal initial instance: %v", err)
			}
			tc.prepare(t, memory, instance)

			wantErr := errors.New("storage unavailable")
			faultStore := &agentInstanceDeleteFaultStore{revisionMemStore: memory, err: wantErr}
			_, err = NewAgentReconciler(faultStore, zaptest.NewLogger(t)).Reconcile(ctx, Request{Namespace: "default", Name: agent.ObjectMeta.Name, Kind: "Agent"})
			if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("error = %v, want wrapped deletion failure containing %q", err, tc.wantMessage)
			}
			rawInstances, err = memory.List(ctx, agentInstanceKind, store.ListOptions{Namespace: "default"})
			if err != nil || len(rawInstances) != 1 {
				t.Fatalf("instances after failed replacement = %d, err = %v, want original only", len(rawInstances), err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helper
// ---------------------------------------------------------------------------

func hasFinalizer(finalizers []string, f string) bool {
	for _, fin := range finalizers {
		if fin == f {
			return true
		}
	}
	return false
}

func findCondition(conditions []v1alpha1.Condition, condType string) *v1alpha1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
