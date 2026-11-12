// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makeAgentService(ns, name, agentRefName string) v1alpha1.AgentService {
	return v1alpha1.AgentService{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "AgentService",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "svc-uid-001",
		},
		Spec: v1alpha1.AgentServiceSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: agentRefName},
		},
	}
}

type agentServiceStatusConflictStore struct {
	*revisionMemStore
	conflicted bool
}

type persistentAgentServiceStatusConflictStore struct {
	*revisionMemStore
	statusUpdates int
}

type agentServiceGetFaultStore struct {
	*revisionMemStore
	kind string
	err  error
}

func (s *agentServiceGetFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == s.kind {
		return nil, s.err
	}
	return s.revisionMemStore.Get(ctx, kind, namespace, name)
}

type capturingAgentServicePublisher struct {
	agent   *v1alpha1.Agent
	deletes int
}

func TestAgentServiceDependencyHandlerRequeuesAffectedServices(t *testing.T) {
	memoryStore := newMemStore()
	for _, service := range []v1alpha1.AgentService{
		makeAgentService("tenant-a", "planner-service", "planner"),
		makeAgentService("tenant-a", "writer-service", "writer"),
		makeAgentService("tenant-b", "other-service", "planner"),
	} {
		data, err := json.Marshal(service)
		if err != nil {
			t.Fatal(err)
		}
		if err := memoryStore.Create(context.Background(), agentServiceKind, service.ObjectMeta.Namespace, service.ObjectMeta.Name, data); err != nil {
			t.Fatal(err)
		}
	}

	agentRecorder := &intentGatewayEventRecorder{}
	agentHandler := NewAgentServiceDependencyHandler(memoryStore, "Agent", agentRecorder, zaptest.NewLogger(t))
	agentHandler.OnUpdate(KeyFunc("tenant-a", "planner"))
	if !reflect.DeepEqual(agentRecorder.updates, []string{KeyFunc("tenant-a", "planner-service")}) {
		t.Fatalf("Agent dependency updates = %v", agentRecorder.updates)
	}

	instanceRecorder := &intentGatewayEventRecorder{}
	instanceHandler := NewAgentServiceDependencyHandler(memoryStore, "AgentInstance", instanceRecorder, zaptest.NewLogger(t))
	instanceHandler.OnDelete(KeyFunc("tenant-a", "planner-r1"))
	got := make(map[string]bool, len(instanceRecorder.updates))
	for _, update := range instanceRecorder.updates {
		got[update] = true
	}
	want := map[string]bool{KeyFunc("tenant-a", "planner-service"): true, KeyFunc("tenant-a", "writer-service"): true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AgentInstance dependency updates = %v, want %v", instanceRecorder.updates, want)
	}
}

func (p *capturingAgentServicePublisher) Ensure(_ context.Context, _ *v1alpha1.AgentService, agent *v1alpha1.Agent) (AgentServiceEndpoint, error) {
	p.agent = agent
	ready := int32(2)
	return AgentServiceEndpoint{URL: "http://revision-runtime.default.svc.cluster.local:8080/invoke", ReadyInstances: &ready}, nil
}

func (p *capturingAgentServicePublisher) Delete(context.Context, *v1alpha1.AgentService) error {
	p.deletes++
	return nil
}

func (s *persistentAgentServiceStatusConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == agentServiceKind {
		var desired v1alpha1.AgentService
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase != "" {
			s.statusUpdates++
			return fmt.Errorf("wrapped status conflict: %w", store.ErrConflict)
		}
	}
	return s.revisionMemStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *agentServiceStatusConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == agentServiceKind && !s.conflicted {
		var desired v1alpha1.AgentService
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase != "" {
			s.conflicted = true
			raw, err := s.revisionMemStore.Get(ctx, kind, namespace, name)
			if err != nil {
				return err
			}
			var latest v1alpha1.AgentService
			if err := json.Unmarshal(raw, &latest); err != nil {
				return err
			}
			latest.ObjectMeta.Labels = map[string]string{"concurrent": "preserved"}
			latest.ObjectMeta.Finalizers = append(latest.ObjectMeta.Finalizers, "example.dev/foreign")
			updated, err := json.Marshal(latest)
			if err != nil {
				return err
			}
			if err := s.revisionMemStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
				return err
			}
			return fmt.Errorf("wrapped status conflict: %w", store.ErrConflict)
		}
	}
	return s.revisionMemStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

// seedAgentWithRevisionAndInstances creates an Agent with a revision and ready instances.
func seedAgentWithRevisionAndInstances(t *testing.T, ctx context.Context, s *revisionMemStore, ns, name string, revision int64, readyCount int) {
	t.Helper()

	replicas := int32(readyCount)
	agent := makeAgent(ns, name, replicas)
	agent.Status.RevisionCount = revision
	data, _ := json.Marshal(agent)
	if err := s.Create(ctx, "Agent", ns, name, data); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Create the revision record.
	specData, _ := json.Marshal(agent.Spec)
	s.CreateRevision(ctx, "Agent", ns, name, revision, specData, data)

	// Create ready instances.
	for i := 0; i < readyCount; i++ {
		inst := v1alpha1.AgentInstance{
			TypeMeta: v1alpha1.TypeMeta{
				APIVersion: v1alpha1.APIVersion,
				Kind:       "AgentInstance",
			},
			ObjectMeta: v1alpha1.ObjectMeta{
				Name:      name + "-inst-" + string(rune('a'+i)),
				Namespace: ns,
				UID:       "inst-uid-" + string(rune('a'+i)),
				OwnerReferences: []v1alpha1.OwnerReference{
					{
						APIVersion: v1alpha1.APIVersion,
						Kind:       "Agent",
						Name:       name,
						UID:        "agent-uid-001",
					},
				},
			},
			Spec: v1alpha1.AgentInstanceSpec{
				AgentRef: v1alpha1.LocalObjectReference{Name: name},
				Revision: revision,
			},
			Status: v1alpha1.AgentInstanceStatus{
				Phase: v1alpha1.PhaseReady,
			},
		}
		instData, _ := json.Marshal(inst)
		if err := s.Create(ctx, "AgentInstance", ns, inst.ObjectMeta.Name, instData); err != nil {
			t.Fatalf("seed instance %d: %v", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// TC-0061: AgentService → reconcile → resolves Agent → reaches ready instances
// ---------------------------------------------------------------------------

func TestAgentServiceReconciler_ResolvesAgentAndReachesInstances(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewAgentServiceReconciler(s, logger)

	// Seed an Agent with revision 1 and 2 ready instances.
	seedAgentWithRevisionAndInstances(t, ctx, s, "default", "my-agent", 1, 2)

	// Create the AgentService.
	svc := makeAgentService("default", "my-svc", "my-agent")
	data, _ := json.Marshal(svc)
	if err := s.Create(ctx, agentServiceKind, "default", "my-svc", data); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	// First reconcile: adds finalizer.
	res, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "my-svc", Kind: agentServiceKind})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if !res.Requeue {
		t.Error("expected requeue after adding finalizer")
	}

	// Verify finalizer was added.
	raw, _ := s.Get(ctx, agentServiceKind, "default", "my-svc")
	var updated v1alpha1.AgentService
	json.Unmarshal(raw, &updated)
	if !hasFinalizer(updated.ObjectMeta.Finalizers, AgentServiceFinalizer) {
		t.Error("expected finalizer to be present")
	}

	// Second reconcile: resolves agent, counts instances, updates status.
	_, err = reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "my-svc", Kind: agentServiceKind})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	// Verify status.
	raw, _ = s.Get(ctx, agentServiceKind, "default", "my-svc")
	json.Unmarshal(raw, &updated)

	if updated.Status.Phase != v1alpha1.PhaseReady {
		t.Errorf("status.phase = %q, want %q", updated.Status.Phase, v1alpha1.PhaseReady)
	}
	if updated.Status.ActiveRevision != 1 {
		t.Errorf("status.activeRevision = %d, want 1", updated.Status.ActiveRevision)
	}
	if updated.Status.ReadyInstances != 2 {
		t.Errorf("status.readyInstances = %d, want 2", updated.Status.ReadyInstances)
	}
	if updated.Status.Endpoint != "/apis/kognition.dev/v1alpha1/namespaces/default/agentservices/my-svc/invoke" {
		t.Errorf("status.endpoint = %q, want standalone invoke endpoint", updated.Status.Endpoint)
	}
	resourceVersion := updated.ObjectMeta.ResourceVersion
	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "my-svc", Kind: agentServiceKind}); err != nil {
		t.Fatalf("reconcile stable ready service: %v", err)
	}
	raw, err = s.Get(ctx, agentServiceKind, "default", "my-svc")
	if err != nil {
		t.Fatalf("get stable ready service: %v", err)
	}
	updated = v1alpha1.AgentService{}
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal stable ready service: %v", err)
	}
	if updated.ObjectMeta.ResourceVersion != resourceVersion {
		t.Fatalf("resourceVersion = %d after unchanged ready reconcile, want %d", updated.ObjectMeta.ResourceVersion, resourceVersion)
	}
}

func TestAgentServiceReconciler_IgnoresInstancesOwnedByPreviousAgentUID(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	reconciler := NewAgentServiceReconciler(s, zaptest.NewLogger(t))
	seedAgentWithRevisionAndInstances(t, ctx, s, "default", "replaced-agent", 1, 1)

	raw, err := s.Get(ctx, agentInstanceKind, "default", "replaced-agent-inst-a")
	if err != nil {
		t.Fatalf("get seeded instance: %v", err)
	}
	var stale v1alpha1.AgentInstance
	if err := json.Unmarshal(raw, &stale); err != nil {
		t.Fatalf("unmarshal seeded instance: %v", err)
	}
	stale.ObjectMeta.OwnerReferences[0].UID = "previous-agent-uid"
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal stale instance: %v", err)
	}
	if err := s.Update(ctx, agentInstanceKind, "default", stale.ObjectMeta.Name, data, stale.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("replace instance ownership: %v", err)
	}

	svc := makeAgentService("default", "isolated-svc", "replaced-agent")
	data, err = json.Marshal(svc)
	if err != nil {
		t.Fatalf("marshal service: %v", err)
	}
	if err := s.Create(ctx, agentServiceKind, "default", svc.ObjectMeta.Name, data); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	request := Request{Namespace: "default", Name: svc.ObjectMeta.Name, Kind: agentServiceKind}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile service: %v", err)
	}

	raw, err = s.Get(ctx, agentServiceKind, "default", svc.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get reconciled service: %v", err)
	}
	var updated v1alpha1.AgentService
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal reconciled service: %v", err)
	}
	if updated.Status.ReadyInstances != 0 || updated.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("service status = %+v, want no ready instances from previous Agent UID", updated.Status)
	}
}

func TestAgentServiceReconciler_StatusConflictPreservesConcurrentState(t *testing.T) {
	ctx := context.Background()
	baseStore := newRevisionMemStore()
	s := &agentServiceStatusConflictStore{revisionMemStore: baseStore}
	reconciler := NewAgentServiceReconciler(s, zaptest.NewLogger(t))
	seedAgentWithRevisionAndInstances(t, ctx, baseStore, "default", "conflict-agent", 1, 1)

	svc := makeAgentService("default", "conflict-svc", "conflict-agent")
	data, err := json.Marshal(svc)
	if err != nil {
		t.Fatalf("marshal service: %v", err)
	}
	if err := s.Create(ctx, agentServiceKind, "default", svc.ObjectMeta.Name, data); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	request := Request{Namespace: "default", Name: svc.ObjectMeta.Name, Kind: agentServiceKind}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile status conflict: %v", err)
	}

	raw, err := s.Get(ctx, agentServiceKind, "default", svc.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	var updated v1alpha1.AgentService
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatalf("unmarshal service: %v", err)
	}
	if updated.Status.Phase != v1alpha1.PhaseReady || updated.Status.ReadyInstances != 1 {
		t.Fatalf("status = %+v, want one ready instance", updated.Status)
	}
	if updated.ObjectMeta.Labels["concurrent"] != "preserved" {
		t.Fatalf("labels = %v, want concurrent update preserved", updated.ObjectMeta.Labels)
	}
	if !hasFinalizer(updated.ObjectMeta.Finalizers, "example.dev/foreign") {
		t.Fatalf("finalizers = %v, want foreign finalizer preserved", updated.ObjectMeta.Finalizers)
	}
}

func TestAgentServiceReconciler_PersistentStatusConflictIsBounded(t *testing.T) {
	ctx := context.Background()
	baseStore := newRevisionMemStore()
	s := &persistentAgentServiceStatusConflictStore{revisionMemStore: baseStore}
	reconciler := NewAgentServiceReconciler(s, zaptest.NewLogger(t))
	seedAgentWithRevisionAndInstances(t, ctx, baseStore, "default", "conflict-agent", 1, 1)

	svc := makeAgentService("default", "conflict-svc", "conflict-agent")
	svc.ObjectMeta.Finalizers = []string{AgentServiceFinalizer}
	data, err := json.Marshal(svc)
	if err != nil {
		t.Fatalf("marshal service: %v", err)
	}
	if err := s.Create(ctx, agentServiceKind, "default", svc.ObjectMeta.Name, data); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	request := Request{Namespace: "default", Name: svc.ObjectMeta.Name, Kind: agentServiceKind}
	if _, err := reconciler.Reconcile(ctx, request); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("reconcile error = %v, want resource version conflict", err)
	}
	if s.statusUpdates != 5 {
		t.Fatalf("status update attempts = %d, want 5", s.statusUpdates)
	}
}

// ---------------------------------------------------------------------------
// TC-0062: RevisionPin → traffic stays at pinned revision even when newer exists
// ---------------------------------------------------------------------------

func TestAgentServiceReconciler_RevisionPin(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewAgentServiceReconciler(s, logger)

	// Seed an Agent with revision 2 (latest) but also revision 1 present.
	replicas := int32(1)
	agent := makeAgent("default", "pinned-agent", replicas)
	agent.Status.RevisionCount = 2
	data, _ := json.Marshal(agent)
	s.Create(ctx, "Agent", "default", "pinned-agent", data)

	// Create both revisions.
	specData, _ := json.Marshal(agent.Spec)
	s.CreateRevision(ctx, "Agent", "default", "pinned-agent", 1, specData, data)
	s.CreateRevision(ctx, "Agent", "default", "pinned-agent", 2, specData, data)

	// Create an instance at revision 1 (ready).
	inst := v1alpha1.AgentInstance{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentInstance"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "pinned-agent-inst-a",
			Namespace: "default",
			UID:       "inst-uid-pin-a",
			OwnerReferences: []v1alpha1.OwnerReference{
				{APIVersion: v1alpha1.APIVersion, Kind: "Agent", Name: "pinned-agent", UID: "agent-uid-001"},
			},
		},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "pinned-agent"},
			Revision: 1,
		},
		Status: v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhaseReady},
	}
	instData, _ := json.Marshal(inst)
	s.Create(ctx, "AgentInstance", "default", "pinned-agent-inst-a", instData)

	// Create an instance at revision 2 (ready) — should NOT count.
	inst2 := inst
	inst2.ObjectMeta.Name = "pinned-agent-inst-b"
	inst2.ObjectMeta.UID = "inst-uid-pin-b"
	inst2.Spec.Revision = 2
	instData2, _ := json.Marshal(inst2)
	s.Create(ctx, "AgentInstance", "default", "pinned-agent-inst-b", instData2)

	// Create an AgentService pinned to revision 1.
	pinnedRev := int64(1)
	svc := makeAgentService("default", "pinned-svc", "pinned-agent")
	svc.Spec.RevisionPin = &pinnedRev
	svcData, _ := json.Marshal(svc)
	s.Create(ctx, agentServiceKind, "default", "pinned-svc", svcData)

	// Reconcile: finalizer.
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "pinned-svc", Kind: agentServiceKind})
	// Reconcile: status update.
	_, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "pinned-svc", Kind: agentServiceKind})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Verify: activeRevision is 1, not 2.
	raw, _ := s.Get(ctx, agentServiceKind, "default", "pinned-svc")
	var updated v1alpha1.AgentService
	json.Unmarshal(raw, &updated)

	if updated.Status.ActiveRevision != 1 {
		t.Errorf("status.activeRevision = %d, want 1 (pinned)", updated.Status.ActiveRevision)
	}
	// Only the revision-1 instance counts as ready.
	if updated.Status.ReadyInstances != 1 {
		t.Errorf("status.readyInstances = %d, want 1 (only rev 1 instance)", updated.Status.ReadyInstances)
	}
	if updated.Status.Phase != v1alpha1.PhaseReady {
		t.Errorf("status.phase = %q, want %q", updated.Status.Phase, v1alpha1.PhaseReady)
	}
}

func TestAgentServiceReconciler_PinnedRevisionPublishesSnapshotWorkload(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	replicas := int32(1)
	current := makeAgent("default", "pinned-agent", replicas)
	current.Status.RevisionCount = 2
	current.Spec.Instructions.Inline = "current instructions"
	currentData, err := json.Marshal(current)
	if err != nil {
		t.Fatalf("marshal current Agent: %v", err)
	}
	if err := s.Create(ctx, "Agent", "default", "pinned-agent", currentData); err != nil {
		t.Fatalf("store current Agent: %v", err)
	}
	pinned := current
	pinned.Spec.Instructions.Inline = "pinned instructions"
	pinnedData, err := json.Marshal(pinned)
	if err != nil {
		t.Fatalf("marshal pinned Agent revision: %v", err)
	}
	if err := s.CreateRevision(ctx, "Agent", "default", "pinned-agent", 1, nil, pinnedData); err != nil {
		t.Fatalf("store pinned revision: %v", err)
	}
	if err := s.CreateRevision(ctx, "Agent", "default", "pinned-agent", 2, nil, currentData); err != nil {
		t.Fatalf("store current revision: %v", err)
	}
	revision := int64(1)
	svc := makeAgentService("default", "pinned-service", "pinned-agent")
	svc.Spec.RevisionPin = &revision
	svc.ObjectMeta.Finalizers = []string{AgentServiceFinalizer}
	svcData, err := json.Marshal(svc)
	if err != nil {
		t.Fatalf("marshal AgentService: %v", err)
	}
	if err := s.Create(ctx, agentServiceKind, "default", svc.ObjectMeta.Name, svcData); err != nil {
		t.Fatalf("store AgentService: %v", err)
	}
	publisher := &capturingAgentServicePublisher{}
	reconciler := NewAgentServiceReconciler(s, zaptest.NewLogger(t), WithAgentServiceEndpointPublisher(publisher))

	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: svc.ObjectMeta.Name, Kind: agentServiceKind}); err != nil {
		t.Fatalf("reconcile pinned service: %v", err)
	}
	if publisher.agent == nil || publisher.agent.Spec.Instructions.Inline != "pinned instructions" {
		t.Fatalf("published Agent = %#v, want pinned revision spec", publisher.agent)
	}
	if publisher.agent.ObjectMeta.UID != current.ObjectMeta.UID || publisher.agent.Status.RevisionCount != current.Status.RevisionCount {
		t.Fatalf("published Agent identity/status = %#v, want current lifecycle metadata", publisher.agent)
	}
	storedRaw, err := s.Get(ctx, agentServiceKind, "default", svc.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get AgentService: %v", err)
	}
	var stored v1alpha1.AgentService
	if err := json.Unmarshal(storedRaw, &stored); err != nil {
		t.Fatalf("unmarshal AgentService: %v", err)
	}
	if stored.Status.Phase != v1alpha1.PhaseReady || stored.Status.ReadyInstances != 2 || stored.Status.ActiveRevision != 1 {
		t.Fatalf("pinned AgentService status = %#v, want publisher readiness for revision 1", stored.Status)
	}
}

func TestAgentServiceReconciler_WithdrawsPublishedEndpointWhenAgentDisappears(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	svc := makeAgentService("default", "orphan-service", "missing-agent")
	svc.ObjectMeta.Finalizers = []string{AgentServiceFinalizer}
	svcData, err := json.Marshal(svc)
	if err != nil {
		t.Fatalf("marshal AgentService: %v", err)
	}
	if err := s.Create(ctx, agentServiceKind, "default", svc.ObjectMeta.Name, svcData); err != nil {
		t.Fatalf("store AgentService: %v", err)
	}
	publisher := &capturingAgentServicePublisher{}
	reconciler := NewAgentServiceReconciler(s, zaptest.NewLogger(t), WithAgentServiceEndpointPublisher(publisher))

	result, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: svc.ObjectMeta.Name, Kind: agentServiceKind})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("reconcile missing Agent = %#v, %v; want pending requeue", result, err)
	}
	if publisher.deletes != 1 {
		t.Fatalf("publisher deletes = %d, want 1", publisher.deletes)
	}
	raw, err := s.Get(ctx, agentServiceKind, "default", svc.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get pending AgentService: %v", err)
	}
	var stored v1alpha1.AgentService
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal pending AgentService: %v", err)
	}
	resourceVersion := stored.ObjectMeta.ResourceVersion
	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: svc.ObjectMeta.Name, Kind: agentServiceKind}); err != nil {
		t.Fatalf("repeat reconcile missing Agent: %v", err)
	}
	raw, err = s.Get(ctx, agentServiceKind, "default", svc.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get repeated pending AgentService: %v", err)
	}
	stored = v1alpha1.AgentService{}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal repeated pending AgentService: %v", err)
	}
	if stored.ObjectMeta.ResourceVersion != resourceVersion {
		t.Fatalf("resourceVersion = %d after unchanged missing-Agent reconcile, want %d", stored.ObjectMeta.ResourceVersion, resourceVersion)
	}
}

func TestAgentServiceReconciler_WithdrawsEndpointWhenPinnedRevisionBecomesUnavailable(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	seedAgentWithRevisionAndInstances(t, ctx, s, "default", "revision-agent", 1, 1)
	revision := int64(1)
	svc := makeAgentService("default", "revision-service", "revision-agent")
	svc.Spec.RevisionPin = &revision
	svc.ObjectMeta.Finalizers = []string{AgentServiceFinalizer}
	svcData, err := json.Marshal(svc)
	if err != nil {
		t.Fatalf("marshal AgentService: %v", err)
	}
	if err := s.Create(ctx, agentServiceKind, "default", svc.ObjectMeta.Name, svcData); err != nil {
		t.Fatalf("store AgentService: %v", err)
	}
	publisher := &capturingAgentServicePublisher{}
	reconciler := NewAgentServiceReconciler(s, zaptest.NewLogger(t), WithAgentServiceEndpointPublisher(publisher))
	request := Request{Namespace: "default", Name: svc.ObjectMeta.Name, Kind: agentServiceKind}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("publish pinned revision: %v", err)
	}
	if publisher.agent == nil {
		t.Fatal("expected pinned endpoint to be published")
	}
	raw, err := s.Get(ctx, agentServiceKind, "default", svc.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get published AgentService: %v", err)
	}
	var stored v1alpha1.AgentService
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal published AgentService: %v", err)
	}
	missingRevision := int64(99)
	stored.Spec.RevisionPin = &missingRevision
	updated, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal unavailable revision AgentService: %v", err)
	}
	if err := s.Update(ctx, agentServiceKind, "default", stored.ObjectMeta.Name, updated, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("pin unavailable revision: %v", err)
	}

	result, err := reconciler.Reconcile(ctx, request)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("reconcile unavailable pinned revision = %#v, %v; want pending requeue", result, err)
	}
	if publisher.deletes != 1 {
		t.Fatalf("publisher deletes = %d, want stale endpoint withdrawn", publisher.deletes)
	}
	raw, err = s.Get(ctx, agentServiceKind, "default", svc.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("get unavailable revision AgentService: %v", err)
	}
	stored = v1alpha1.AgentService{}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal unavailable revision AgentService: %v", err)
	}
	if stored.Status.Phase != v1alpha1.PhasePending || stored.Status.Endpoint != "" || stored.Status.ActiveRevision != 0 || stored.Status.ReadyInstances != 0 {
		t.Fatalf("unavailable revision status = %#v, want withdrawn pending endpoint", stored.Status)
	}
}

// ---------------------------------------------------------------------------
// TC-0063: DNS-like name resolution returns correct endpoint
// ---------------------------------------------------------------------------

func TestAgentServiceReconciler_StandaloneInvokeEndpoint(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewAgentServiceReconciler(s, logger)

	// Seed Agent with revision and instances in workspace "production".
	seedAgentWithRevisionAndInstances(t, ctx, s, "production", "chat-agent", 3, 1)

	// Create the AgentService in the "production" namespace.
	svc := makeAgentService("production", "chat-service", "chat-agent")
	svcData, _ := json.Marshal(svc)
	s.Create(ctx, agentServiceKind, "production", "chat-service", svcData)

	// Reconcile: finalizer + status.
	reconciler.Reconcile(ctx, Request{Namespace: "production", Name: "chat-service", Kind: agentServiceKind})
	_, err := reconciler.Reconcile(ctx, Request{Namespace: "production", Name: "chat-service", Kind: agentServiceKind})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Verify DNS-like endpoint.
	raw, _ := s.Get(ctx, agentServiceKind, "production", "chat-service")
	var updated v1alpha1.AgentService
	json.Unmarshal(raw, &updated)

	expectedEndpoint := "/apis/kognition.dev/v1alpha1/namespaces/production/agentservices/chat-service/invoke"
	if updated.Status.Endpoint != expectedEndpoint {
		t.Errorf("status.endpoint = %q, want %q", updated.Status.Endpoint, expectedEndpoint)
	}
}

// ---------------------------------------------------------------------------
// Deletion: reconciler removes finalizer on deletion
// ---------------------------------------------------------------------------

func TestAgentServiceReconciler_Deletion(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewAgentServiceReconciler(s, logger)

	// Seed Agent and service.
	seedAgentWithRevisionAndInstances(t, ctx, s, "default", "del-agent", 1, 1)
	svc := makeAgentService("default", "del-svc", "del-agent")
	svcData, _ := json.Marshal(svc)
	s.Create(ctx, agentServiceKind, "default", "del-svc", svcData)

	// Reconcile to steady state.
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "del-svc", Kind: agentServiceKind})
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "del-svc", Kind: agentServiceKind})

	// Set deletion timestamp.
	raw, _ := s.Get(ctx, agentServiceKind, "default", "del-svc")
	var updated v1alpha1.AgentService
	json.Unmarshal(raw, &updated)
	now := time.Now().UTC()
	updated.ObjectMeta.DeletionTimestamp = &now
	data, _ := json.Marshal(updated)
	s.Update(ctx, agentServiceKind, "default", "del-svc", data, updated.ObjectMeta.ResourceVersion)

	// Reconcile deletion.
	_, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "del-svc", Kind: agentServiceKind})
	if err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}

	// Verify finalizer removed.
	raw, _ = s.Get(ctx, agentServiceKind, "default", "del-svc")
	var afterDeletion v1alpha1.AgentService
	json.Unmarshal(raw, &afterDeletion)
	if hasFinalizer(afterDeletion.ObjectMeta.Finalizers, AgentServiceFinalizer) {
		t.Error("expected finalizer to be removed after deletion")
	}
}

// ---------------------------------------------------------------------------
// Empty AgentRef.Name: service enters pending state
// ---------------------------------------------------------------------------

func TestAgentServiceReconciler_EmptyAgentRef(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewAgentServiceReconciler(s, logger)

	// Create service with empty agentRef name.
	svc := makeAgentService("default", "empty-ref-svc", "")
	svcData, _ := json.Marshal(svc)
	s.Create(ctx, agentServiceKind, "default", "empty-ref-svc", svcData)

	// Reconcile: finalizer.
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "empty-ref-svc", Kind: agentServiceKind})
	// Reconcile: empty ref → pending.
	res, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "empty-ref-svc", Kind: agentServiceKind})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected requeue after empty agentRef")
	}

	raw, _ := s.Get(ctx, agentServiceKind, "default", "empty-ref-svc")
	var updated v1alpha1.AgentService
	json.Unmarshal(raw, &updated)
	if updated.Status.Phase != v1alpha1.PhasePending {
		t.Errorf("status.phase = %q, want %q", updated.Status.Phase, v1alpha1.PhasePending)
	}
}

// ---------------------------------------------------------------------------
// Pinned revision not found: service enters pending state
// ---------------------------------------------------------------------------

func TestAgentServiceReconciler_PinnedRevisionNotFound(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewAgentServiceReconciler(s, logger)

	// Seed an Agent with revision 1 only.
	seedAgentWithRevisionAndInstances(t, ctx, s, "default", "rev-agent", 1, 1)

	// Create service pinned to revision 99 (does not exist).
	pinnedRev := int64(99)
	svc := makeAgentService("default", "bad-pin-svc", "rev-agent")
	svc.Spec.RevisionPin = &pinnedRev
	svcData, _ := json.Marshal(svc)
	s.Create(ctx, agentServiceKind, "default", "bad-pin-svc", svcData)

	// Reconcile: finalizer.
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "bad-pin-svc", Kind: agentServiceKind})
	// Reconcile: pinned revision not found → pending.
	res, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "bad-pin-svc", Kind: agentServiceKind})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected requeue after pinned revision not found")
	}

	raw, _ := s.Get(ctx, agentServiceKind, "default", "bad-pin-svc")
	var updated v1alpha1.AgentService
	json.Unmarshal(raw, &updated)
	if updated.Status.Phase != v1alpha1.PhasePending {
		t.Errorf("status.phase = %q, want %q", updated.Status.Phase, v1alpha1.PhasePending)
	}
}

// ---------------------------------------------------------------------------
// ListRevisions fallback: RevisionCount==0, no pin → falls back to ListRevisions
// ---------------------------------------------------------------------------

func TestAgentServiceReconciler_ListRevisionsFallback(t *testing.T) {
	ctx := context.Background()
	s := newRevisionMemStore()
	logger := zaptest.NewLogger(t)
	reconciler := NewAgentServiceReconciler(s, logger)

	// Create Agent with RevisionCount = 0 but revisions exist in the store.
	replicas := int32(1)
	agent := makeAgent("default", "fallback-agent", replicas)
	agent.Status.RevisionCount = 0 // Force fallback path.
	data, _ := json.Marshal(agent)
	s.Create(ctx, "Agent", "default", "fallback-agent", data)

	// Manually create revision records (simulates store having revisions but agent status not updated).
	specData, _ := json.Marshal(agent.Spec)
	s.CreateRevision(ctx, "Agent", "default", "fallback-agent", 1, specData, data)
	s.CreateRevision(ctx, "Agent", "default", "fallback-agent", 2, specData, data)

	// Create a ready instance at revision 2 (the expected latest).
	inst := v1alpha1.AgentInstance{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentInstance"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "fallback-agent-inst-a",
			Namespace: "default",
			UID:       "inst-uid-fb-a",
			OwnerReferences: []v1alpha1.OwnerReference{
				{APIVersion: v1alpha1.APIVersion, Kind: "Agent", Name: "fallback-agent", UID: "agent-uid-001"},
			},
		},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "fallback-agent"},
			Revision: 2,
		},
		Status: v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhaseRunning},
	}
	instData, _ := json.Marshal(inst)
	s.Create(ctx, "AgentInstance", "default", "fallback-agent-inst-a", instData)

	// Create the AgentService (no pin).
	svc := makeAgentService("default", "fallback-svc", "fallback-agent")
	svcData, _ := json.Marshal(svc)
	s.Create(ctx, agentServiceKind, "default", "fallback-svc", svcData)

	// Reconcile: finalizer + status.
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "fallback-svc", Kind: agentServiceKind})
	_, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "fallback-svc", Kind: agentServiceKind})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	raw, _ := s.Get(ctx, agentServiceKind, "default", "fallback-svc")
	var updated v1alpha1.AgentService
	json.Unmarshal(raw, &updated)

	if updated.Status.ActiveRevision != 2 {
		t.Errorf("status.activeRevision = %d, want 2 (from ListRevisions fallback)", updated.Status.ActiveRevision)
	}
	if updated.Status.ReadyInstances != 1 {
		t.Errorf("status.readyInstances = %d, want 1", updated.Status.ReadyInstances)
	}
	if updated.Status.Phase != v1alpha1.PhaseReady {
		t.Errorf("status.phase = %q, want %q", updated.Status.Phase, v1alpha1.PhaseReady)
	}
}

// ---------------------------------------------------------------------------
// Agent not found: service enters pending state with condition
// ---------------------------------------------------------------------------

func TestAgentServiceReconciler_AgentNotFound(t *testing.T) {
	ctx := context.Background()
	baseStore := newRevisionMemStore()
	s := &agentServiceGetFaultStore{
		revisionMemStore: baseStore,
		kind:             "Agent",
		err:              fmt.Errorf("wrapped missing agent: %w", store.ErrNotFound),
	}
	logger := zaptest.NewLogger(t)
	reconciler := NewAgentServiceReconciler(s, logger)

	// Create service referencing a non-existent agent.
	svc := makeAgentService("default", "orphan-svc", "missing-agent")
	svcData, _ := json.Marshal(svc)
	s.Create(ctx, agentServiceKind, "default", "orphan-svc", svcData)

	// Reconcile: finalizer.
	reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "orphan-svc", Kind: agentServiceKind})
	// Reconcile: agent not found → pending.
	res, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: "orphan-svc", Kind: agentServiceKind})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected requeue after agent not found")
	}

	// Verify status is pending.
	raw, _ := s.Get(ctx, agentServiceKind, "default", "orphan-svc")
	var updated v1alpha1.AgentService
	json.Unmarshal(raw, &updated)
	if updated.Status.Phase != v1alpha1.PhasePending {
		t.Errorf("status.phase = %q, want %q", updated.Status.Phase, v1alpha1.PhasePending)
	}
}

func TestAgentServiceReconciler_WrappedNotFoundIsComplete(t *testing.T) {
	s := &agentServiceGetFaultStore{
		revisionMemStore: newRevisionMemStore(),
		kind:             agentServiceKind,
		err:              fmt.Errorf("wrapped missing service: %w", store.ErrNotFound),
	}
	reconciler := NewAgentServiceReconciler(s, zaptest.NewLogger(t))
	result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: "missing", Kind: agentServiceKind})
	if err != nil || result != (Result{}) {
		t.Fatalf("result = %+v, error = %v, want completed not-found reconciliation", result, err)
	}
}
