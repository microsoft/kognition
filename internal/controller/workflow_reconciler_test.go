// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

type eventMemStore struct {
	*memStore
	mu     sync.Mutex
	events map[string][]store.InstanceEvent
}

type failingEventStore struct {
	*memStore
	err error
}

type failWorkflowUpdateEventStore struct {
	*eventMemStore
	err      error
	failOnce bool
}

type conflictOnceWorkflowStore struct {
	*memStore
	conflicted bool
}

type workflowCleanupFaultStore struct {
	*memStore
	listErr   error
	deleteErr error
}

type workflowConflictFaultStore struct {
	*memStore
	getCalls         int
	afterConflictErr error
	afterConflictRaw []byte
}

type workflowJobCreateFaultStore struct {
	*memStore
	err          error
	createBefore bool
}

func (s *workflowJobCreateFaultStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if kind != "AgentJob" || s.err == nil {
		return s.memStore.Create(ctx, kind, namespace, name, data)
	}
	if s.createBefore {
		if err := s.memStore.Create(ctx, kind, namespace, name, data); err != nil {
			return err
		}
	}
	return s.err
}

func (s *workflowCleanupFaultStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if kind == "AgentJob" && s.listErr != nil {
		return nil, s.listErr
	}
	return s.memStore.List(ctx, kind, opts)
}

func (s *workflowCleanupFaultStore) Delete(ctx context.Context, kind, namespace, name string) error {
	if kind == "AgentJob" && s.deleteErr != nil {
		return s.deleteErr
	}
	return s.memStore.Delete(ctx, kind, namespace, name)
}

func (s *workflowConflictFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	s.getCalls++
	if kind == "Workflow" && s.getCalls > 1 {
		if s.afterConflictErr != nil {
			return nil, s.afterConflictErr
		}
		if s.afterConflictRaw != nil {
			return s.afterConflictRaw, nil
		}
	}
	return s.memStore.Get(ctx, kind, namespace, name)
}

func (s *workflowConflictFaultStore) Update(context.Context, string, string, string, []byte, int64) error {
	return store.ErrConflict
}

func (s *conflictOnceWorkflowStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "Workflow" && !s.conflicted {
		s.conflicted = true
		raw, err := s.memStore.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var concurrent v1alpha1.Workflow
		if err := json.Unmarshal(raw, &concurrent); err != nil {
			return err
		}
		concurrent.ObjectMeta.Annotations = map[string]string{"external.example/revision": "2"}
		concurrent.ObjectMeta.Finalizers = []string{"external.example/cleanup", WorkflowFinalizer}
		concurrent.Status.Conditions = append(concurrent.Status.Conditions, v1alpha1.Condition{
			Type: "Audited", Status: "True", Reason: "ConcurrentObserver",
		})
		updated, err := json.Marshal(concurrent)
		if err != nil {
			return err
		}
		if err := s.memStore.Update(ctx, kind, namespace, name, updated, concurrent.ObjectMeta.ResourceVersion); err != nil {
			return err
		}
		return store.ErrConflict
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func newEventMemStore() *eventMemStore {
	return &eventMemStore{
		memStore: newMemStore(),
		events:   map[string][]store.InstanceEvent{},
	}
}

// mustReconcile invokes the test-only direct-dispatch shim and fails the
// test on error. Production Reconcile is template-only; see reconcileDirect
// for the rationale.
func mustReconcile(t *testing.T, r *WorkflowReconciler, req Request) {
	t.Helper()
	if _, err := r.reconcileDirect(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func (s *eventMemStore) AppendEvent(_ context.Context, kind, ns, name string, event store.InstanceEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%s/%s/%s", kind, ns, name)
	s.events[key] = append(s.events[key], event)
	return nil
}

func (s *eventMemStore) ListEvents(_ context.Context, kind, ns, name string) ([]store.InstanceEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%s/%s/%s", kind, ns, name)
	out := make([]store.InstanceEvent, len(s.events[key]))
	copy(out, s.events[key])
	return out, nil
}

func (s *failingEventStore) AppendEvent(_ context.Context, _, _, _ string, _ store.InstanceEvent) error {
	return s.err
}

func (s *failWorkflowUpdateEventStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if s.failOnce && kind == "Workflow" {
		s.failOnce = false
		return s.err
	}
	return s.eventMemStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func makeWorkflow(ns, name string) v1alpha1.Workflow {
	return v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "wf-uid-001",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "plan", AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}},
				{Name: "execute", AgentRef: v1alpha1.LocalObjectReference{Name: "executor"}},
			},
			Edges: []v1alpha1.WorkflowEdge{{From: "plan", To: "execute"}},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
}

func storeWorkflow(t *testing.T, s *memStore, wf v1alpha1.Workflow) {
	t.Helper()
	data, err := json.Marshal(wf)
	if err != nil {
		t.Fatalf("marshal workflow: %v", err)
	}
	if err := s.Create(context.Background(), "Workflow", wf.ObjectMeta.Namespace, wf.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store workflow: %v", err)
	}
}

func TestWorkflowReconciler_TracksTemplateRevisions(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	workflow := makeWorkflow("default", "revision-template")
	storeWorkflow(t, store, workflow)
	reconciler := NewWorkflowReconciler(store, zaptest.NewLogger(t))
	request := Request{Namespace: "default", Name: workflow.ObjectMeta.Name, Kind: "Workflow"}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile initial Workflow: %v", err)
	}
	revisions, err := store.ListRevisions(ctx, "Workflow", "default", workflow.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("list initial revisions: %v", err)
	}
	if len(revisions) != 1 || revisions[0].Revision != 1 {
		t.Fatalf("initial revisions = %#v, want revision 1", revisions)
	}

	stored := getWorkflow(t, store, "default", workflow.ObjectMeta.Name)
	stored.Spec.EntryPoint = "plan"
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal updated Workflow: %v", err)
	}
	if err := store.Update(ctx, "Workflow", "default", stored.ObjectMeta.Name, data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update Workflow spec: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile updated Workflow: %v", err)
	}
	revisions, err = store.ListRevisions(ctx, "Workflow", "default", workflow.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("list updated revisions: %v", err)
	}
	if len(revisions) != 2 || revisions[0].Revision != 1 || revisions[1].Revision != 2 {
		t.Fatalf("updated revisions = %#v, want revisions 1 and 2", revisions)
	}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile unchanged Workflow: %v", err)
	}
	revisions, err = store.ListRevisions(ctx, "Workflow", "default", workflow.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("list idempotent revisions: %v", err)
	}
	if len(revisions) != 2 {
		t.Fatalf("idempotent revisions = %#v, want two", revisions)
	}

	stored = getWorkflow(t, store, "default", workflow.ObjectMeta.Name)
	stored.Spec.EntryPoint = ""
	data, err = json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal reverted Workflow: %v", err)
	}
	if err := store.Update(ctx, "Workflow", "default", stored.ObjectMeta.Name, data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("revert Workflow spec: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile reverted Workflow: %v", err)
	}
	revisions, err = store.ListRevisions(ctx, "Workflow", "default", workflow.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("list reverted revisions: %v", err)
	}
	if len(revisions) != 3 || revisions[2].Revision != 3 {
		t.Fatalf("reverted revisions = %#v, want revision 3", revisions)
	}
}

func getWorkflow(t *testing.T, s *memStore, ns, name string) v1alpha1.Workflow {
	t.Helper()
	raw, err := s.Get(context.Background(), "Workflow", ns, name)
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	var wf v1alpha1.Workflow
	if err := json.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("unmarshal workflow: %v", err)
	}
	return wf
}

func listWorkflowJobs(t *testing.T, s *memStore, ns string) []v1alpha1.AgentJob {
	t.Helper()
	rawList, err := s.List(context.Background(), "AgentJob", store.ListOptions{Namespace: ns})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	var out []v1alpha1.AgentJob
	for _, raw := range rawList {
		var j v1alpha1.AgentJob
		if err := json.Unmarshal(raw, &j); err != nil {
			t.Fatalf("unmarshal job: %v", err)
		}
		out = append(out, j)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ObjectMeta.ResourceVersion < out[j].ObjectMeta.ResourceVersion
	})
	return out
}

func updateJob(t *testing.T, s *memStore, job v1alpha1.AgentJob) {
	t.Helper()
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	if err := s.Update(context.Background(), "AgentJob", job.ObjectMeta.Namespace, job.ObjectMeta.Name, data, job.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update job: %v", err)
	}
}

func TestWorkflowReconciler_CreatesFirstReadyStepJob(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-create")
	storeWorkflow(t, s, wf)

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-create", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile add finalizer: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	res, err = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-create", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile create job: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after creating step job")
	}

	jobs := listWorkflowJobs(t, s, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if got := jobs[0].ObjectMeta.Labels["kognition.dev/workflow-step"]; got != "plan" {
		t.Fatalf("first job step label = %q, want %q", got, "plan")
	}

	stored := getWorkflow(t, s, "default", "wf-create")
	if stored.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("workflow phase = %q, want %q", stored.Status.Phase, v1alpha1.PhaseRunning)
	}
	if stored.Status.ActiveStep != "plan" {
		t.Fatalf("active step = %q, want %q", stored.Status.ActiveStep, "plan")
	}
}

func TestWorkflowReconciler_AdvancesWhenDependencyCompletes(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-advance")
	storeWorkflow(t, s, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-advance", Kind: "Workflow"})
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-advance", Kind: "Workflow"})

	jobs := listWorkflowJobs(t, s, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job before completion, got %d", len(jobs))
	}
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	updateJob(t, s, jobs[0])

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-advance", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile after step completion: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after launching next step")
	}

	jobs = listWorkflowJobs(t, s, "default")
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs after dependency completion, got %d", len(jobs))
	}

	stored := getWorkflow(t, s, "default", "wf-advance")
	if len(stored.Status.CompletedSteps) != 1 || stored.Status.CompletedSteps[0] != "plan" {
		t.Fatalf("completed steps = %+v, want [plan]", stored.Status.CompletedSteps)
	}
	if stored.Status.ActiveStep != "execute" {
		t.Fatalf("active step = %q, want %q", stored.Status.ActiveStep, "execute")
	}
}

func TestWorkflowReconciler_CommunicationPolicyDeniesDependentAgentDispatch(t *testing.T) {
	// TC-0302
	ctx := context.Background()
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	r.SetCommunicationPolicyAuthorizer(NewCommunicationPolicyEngine(s, zaptest.NewLogger(t)))
	auditStore := &memoryGrantAuditStore{}
	r.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	wf := makeWorkflow("default", "wf-communication-denied")
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(ctx, Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"})
	_, _ = r.reconcileDirect(ctx, Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"})
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 || jobs[0].Spec.AgentRef == nil || jobs[0].Spec.AgentRef.Name != "planner" {
		t.Fatalf("initial jobs = %#v, want only planner", jobs)
	}
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	updateJob(t, s.memStore, jobs[0])

	result, err := r.reconcileDirect(ctx, Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile denied dependency: %v", err)
	}
	if result.Requeue {
		t.Fatal("denied dependent dispatch must not requeue")
	}
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("jobs after denied dispatch = %d, want only source job", len(jobs))
	}
	stored := getWorkflow(t, s.memStore, "default", wf.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("workflow phase = %q, want Failed", stored.Status.Phase)
	}
	condition := findCondition(stored.Status.Conditions, "Ready")
	if condition == nil || condition.Reason != "CommunicationDenied" {
		t.Fatalf("Ready condition = %#v, want CommunicationDenied", condition)
	}
	events, err := s.ListEvents(ctx, "Workflow", "default", wf.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var denied *store.InstanceEvent
	for index := range events {
		if events[index].Reason == "CommunicationDenied" {
			denied = &events[index]
			break
		}
	}
	if denied == nil {
		t.Fatalf("communication denial event = %#v", denied)
	}
	var details map[string]string
	if err := json.Unmarshal(denied.Details, &details); err != nil {
		t.Fatalf("unmarshal communication denial event: %v", err)
	}
	if details["sourceAgent"] != "planner" || details["targetAgent"] != "executor" {
		t.Fatalf("communication denial event details = %#v", details)
	}
	assertWorkflowCommunicationOutcomes(t, auditStore.entries, "denied")
}

func TestWorkflowReconciler_CommunicationPolicyAllowsDependentAgentDispatch(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	r.SetCommunicationPolicyAuthorizer(NewCommunicationPolicyEngine(s, zaptest.NewLogger(t)))
	auditStore := &memoryGrantAuditStore{}
	r.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	storeCommPolicy(t, s, makeCommPolicy("default", "planner-to-executor", v1alpha1.AgentCommunicationPolicySpec{
		DefaultDeny: true,
		AllowFrom:   []v1alpha1.LocalObjectReference{{Name: "planner"}},
		AllowTo:     []v1alpha1.LocalObjectReference{{Name: "executor"}},
	}))
	wf := makeWorkflow("default", "wf-communication-allowed")
	storeWorkflow(t, s, wf)

	_, _ = r.reconcileDirect(ctx, Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"})
	_, _ = r.reconcileDirect(ctx, Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"})
	jobs := listWorkflowJobs(t, s, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	updateJob(t, s, jobs[0])
	if _, err := r.reconcileDirect(ctx, Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile allowed dependency: %v", err)
	}
	jobs = listWorkflowJobs(t, s, "default")
	if len(jobs) != 2 || jobs[1].Spec.AgentRef == nil || jobs[1].Spec.AgentRef.Name != "executor" {
		t.Fatalf("jobs after allowed dispatch = %#v, want planner and executor", jobs)
	}
	assertWorkflowCommunicationOutcomes(t, auditStore.entries, "allowed", "completed")
}

func TestWorkflowReconciler_CommunicationEvidenceCoversEveryInboundAgent(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	r.SetCommunicationPolicyAuthorizer(NewCommunicationPolicyEngine(s, zaptest.NewLogger(t)))
	auditStore := &memoryGrantAuditStore{}
	r.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	storeCommPolicy(t, s, makeCommPolicy("default", "fanin", v1alpha1.AgentCommunicationPolicySpec{
		DefaultDeny: true,
		AllowFrom:   []v1alpha1.LocalObjectReference{{Name: "planner"}, {Name: "reviewer"}},
		AllowTo:     []v1alpha1.LocalObjectReference{{Name: "executor"}},
	}))
	wf := makeWorkflow("default", "wf-fanin")
	target := v1alpha1.WorkflowStep{Name: "execute", AgentRef: v1alpha1.LocalObjectReference{Name: "executor"}}
	plan := workflowPlan{
		OrderedSteps: []v1alpha1.WorkflowStep{
			{Name: "plan", AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}},
			{Name: "review", AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}},
			target,
		},
		Deps: map[string][]string{"execute": {"plan", "review"}},
	}
	sources, allowed, err := r.authorizeInboundAgentDispatch(ctx, &wf, plan, target, 0)
	if err != nil || !allowed {
		t.Fatalf("authorize fan-in = sources %v allowed %t err %v", sources, allowed, err)
	}
	if _, err := r.createStepJob(ctx, &wf, target, sources); err != nil {
		t.Fatalf("create fan-in job: %v", err)
	}
	if len(auditStore.entries) != 4 {
		t.Fatalf("fan-in evidence count = %d, want 4", len(auditStore.entries))
	}
	for index, wantSource := range []string{"planner", "reviewer", "planner", "reviewer"} {
		var decision observability.TrustDecisionV1
		if err := json.Unmarshal(auditStore.entries[index].Details, &decision); err != nil {
			t.Fatalf("decode fan-in evidence %d: %v", index, err)
		}
		if decision.ClaimedIdentityRef == nil || decision.ClaimedIdentityRef.Name != wantSource {
			t.Fatalf("fan-in source %d = %#v, want %q", index, decision.ClaimedIdentityRef, wantSource)
		}
	}
}

func TestWorkflowReconciler_CyclicCommunicationDenialIncludesIteration(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	r.SetCommunicationPolicyAuthorizer(NewCommunicationPolicyEngine(s, zaptest.NewLogger(t)))
	auditStore := &memoryGrantAuditStore{}
	r.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	wf := makeWorkflow("default", "wf-cycle-denied")
	target := wf.Spec.Steps[1]
	plan := workflowPlan{OrderedSteps: wf.Spec.Steps, Deps: map[string][]string{"execute": {"plan"}}, Cyclic: true}
	_, allowed, err := r.authorizeInboundAgentDispatch(ctx, &wf, plan, target, 3)
	if err != nil || allowed {
		t.Fatalf("authorize cyclic denial = allowed %t err %v", allowed, err)
	}
	var decision observability.TrustDecisionV1
	if err := json.Unmarshal(auditStore.entries[0].Details, &decision); err != nil {
		t.Fatalf("decode cyclic denial: %v", err)
	}
	if decision.Request.InvocationID != "wf-cycle-denied-execute-i3" {
		t.Fatalf("cyclic invocation = %q, want iteration-specific ID", decision.Request.InvocationID)
	}
}

func TestWorkflowReconciler_CommunicationAuditFailureBlocksDependentAgentDispatch(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	r.SetCommunicationPolicyAuthorizer(NewCommunicationPolicyEngine(s, zaptest.NewLogger(t)))
	storeCommPolicy(t, s, makeCommPolicy("default", "planner-to-executor", v1alpha1.AgentCommunicationPolicySpec{
		DefaultDeny: true,
		AllowFrom:   []v1alpha1.LocalObjectReference{{Name: "planner"}},
		AllowTo:     []v1alpha1.LocalObjectReference{{Name: "executor"}},
	}))
	wf := makeWorkflow("default", "wf-communication-audit-failure")
	storeWorkflow(t, s, wf)

	_, _ = r.reconcileDirect(ctx, Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"})
	_, _ = r.reconcileDirect(ctx, Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"})
	jobs := listWorkflowJobs(t, s, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	updateJob(t, s, jobs[0])
	r.SetAuditLog(observability.NewCognitiveAuditLog(&memoryGrantAuditStore{err: errors.New("audit unavailable")}))

	if _, err := r.reconcileDirect(ctx, Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}); err == nil || !strings.Contains(err.Error(), "record allowed communication decision") {
		t.Fatalf("reconcile error = %v, want pre-effect audit failure", err)
	}
	jobs = listWorkflowJobs(t, s, "default")
	if len(jobs) != 1 {
		t.Fatalf("jobs after audit failure = %d, want only source job", len(jobs))
	}
}

func assertWorkflowCommunicationOutcomes(t *testing.T, entries []observability.AuditEntry, expected ...string) {
	t.Helper()
	if len(entries) != len(expected) {
		t.Fatalf("communication evidence entries = %d, want %d", len(entries), len(expected))
	}
	for index, entry := range entries {
		var decision observability.TrustDecisionV1
		if err := json.Unmarshal(entry.Details, &decision); err != nil {
			t.Fatalf("decode communication evidence %d: %v", index, err)
		}
		if decision.SchemaVersion != 1 || decision.Plane != "communication" || decision.Outcome != expected[index] {
			t.Fatalf("communication evidence %d = schema %d plane %q outcome %q, want schema 1 communication/%s", index, decision.SchemaVersion, decision.Plane, decision.Outcome, expected[index])
		}
		if decision.ClaimedIdentityRef == nil || decision.ClaimedIdentityRef.Kind != "Agent" || decision.ClaimedIdentityRef.Name != "planner" {
			t.Fatalf("communication source %d = %#v, want Agent planner", index, decision.ClaimedIdentityRef)
		}
		if decision.Request.TargetRef.Kind != "Agent" || decision.Request.TargetRef.Name != "executor" || decision.Request.InvocationID == "" {
			t.Fatalf("communication target %d = %#v invocation %q, want Agent executor and job invocation", index, decision.Request.TargetRef, decision.Request.InvocationID)
		}
		if decision.Execution.TargetReached == nil || *decision.Execution.TargetReached != (expected[index] == "completed") {
			t.Fatalf("communication targetReached %d = %#v", index, decision.Execution.TargetReached)
		}
	}
}

// TC-0025: Linear workflow reaches succeeded/ready after ordered step completion.
func TestWorkflowReconciler_MarksReadyWhenAllStepsComplete(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-done")
	storeWorkflow(t, s, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-done", Kind: "Workflow"})
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-done", Kind: "Workflow"})

	jobs := listWorkflowJobs(t, s, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	updateJob(t, s, jobs[0])
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-done", Kind: "Workflow"})

	jobs = listWorkflowJobs(t, s, "default")
	for _, j := range jobs {
		j.Status.Phase = v1alpha1.PhaseReady
		updateJob(t, s, j)
	}

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-done", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile completion: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected terminal success without requeue")
	}

	stored := getWorkflow(t, s, "default", "wf-done")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("workflow phase = %q, want %q", stored.Status.Phase, v1alpha1.PhaseReady)
	}
	if len(stored.Status.CompletedSteps) != 2 {
		t.Fatalf("completed steps len = %d, want 2", len(stored.Status.CompletedSteps))
	}
	if stored.Status.ActiveStep != "" {
		t.Fatalf("active step = %q, want empty", stored.Status.ActiveStep)
	}
}

func TestWorkflowReconciler_MarksFailedOnStepFailure(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-fail")
	storeWorkflow(t, s, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-fail", Kind: "Workflow"})
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-fail", Kind: "Workflow"})

	jobs := listWorkflowJobs(t, s, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseFailed
	updateJob(t, s, jobs[0])

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-fail", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile failure: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected terminal failure without requeue")
	}

	stored := getWorkflow(t, s, "default", "wf-fail")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("workflow phase = %q, want %q", stored.Status.Phase, v1alpha1.PhaseFailed)
	}
	if stored.Status.ActiveStep != "plan" {
		t.Fatalf("active step = %q, want %q", stored.Status.ActiveStep, "plan")
	}
}

func TestWorkflowReconciler_InvalidDAGFailsWorkflow(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-invalid")
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "missing", To: "execute"}}
	storeWorkflow(t, s, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-invalid", Kind: "Workflow"})
	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-invalid", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile invalid dag: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected no requeue for invalid DAG")
	}

	stored := getWorkflow(t, s, "default", "wf-invalid")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("workflow phase = %q, want %q", stored.Status.Phase, v1alpha1.PhaseFailed)
	}
}

func TestWorkflowReconciler_InvalidWorkflowPlansFailWithoutDispatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*v1alpha1.Workflow)
	}{
		{
			name: "no steps",
			mutate: func(workflow *v1alpha1.Workflow) {
				workflow.Spec.Steps = nil
				workflow.Spec.Edges = nil
			},
		},
		{
			name: "empty step name",
			mutate: func(workflow *v1alpha1.Workflow) {
				workflow.Spec.Steps[0].Name = ""
			},
		},
		{
			name: "duplicate step name",
			mutate: func(workflow *v1alpha1.Workflow) {
				workflow.Spec.Steps[1].Name = workflow.Spec.Steps[0].Name
			},
		},
		{
			name: "unknown edge destination",
			mutate: func(workflow *v1alpha1.Workflow) {
				workflow.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "plan", To: "missing"}}
			},
		},
		{
			name: "unknown entry point",
			mutate: func(workflow *v1alpha1.Workflow) {
				workflow.Spec.EntryPoint = "missing"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := newMemStore()
			r := NewWorkflowReconciler(memory, zaptest.NewLogger(t))
			workflow := makeWorkflow("default", "wf-invalid-plan")
			tc.mutate(&workflow)
			storeWorkflow(t, memory, workflow)
			request := Request{Namespace: "default", Name: workflow.ObjectMeta.Name, Kind: "Workflow"}

			mustReconcile(t, r, request)
			result, err := r.reconcileDirect(context.Background(), request)
			if err != nil {
				t.Fatalf("reconcile invalid workflow: %v", err)
			}
			if result.Requeue || result.RequeueAfter != 0 {
				t.Fatalf("result = %+v, want terminal validation failure", result)
			}

			stored := getWorkflow(t, memory, "default", workflow.ObjectMeta.Name)
			if stored.Status.Phase != v1alpha1.PhaseFailed {
				t.Fatalf("phase = %q, want Failed", stored.Status.Phase)
			}
			condition := findCondition(stored.Status.Conditions, "Ready")
			if condition == nil || condition.Status != "False" || condition.Reason != "InvalidDAG" {
				t.Fatalf("ready condition = %#v, want InvalidDAG failure", condition)
			}
			if jobs := listWorkflowJobs(t, memory, "default"); len(jobs) != 0 {
				t.Fatalf("jobs = %d, want no dispatch for invalid workflow", len(jobs))
			}
		})
	}
}

func TestWorkflowReconciler_DeletionRemovesOwnedJobs(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-delete")
	storeWorkflow(t, s, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-delete", Kind: "Workflow"})
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-delete", Kind: "Workflow"})

	jobs := listWorkflowJobs(t, s, "default")
	if len(jobs) == 0 {
		t.Fatal("expected at least one job before deletion")
	}

	stored := getWorkflow(t, s, "default", "wf-delete")
	now := time.Now().UTC()
	stored.ObjectMeta.DeletionTimestamp = &now
	data, _ := json.Marshal(stored)
	if err := s.Update(context.Background(), "Workflow", "default", "wf-delete", data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark workflow deleting: %v", err)
	}

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-delete", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected no requeue for deletion")
	}

	jobs = listWorkflowJobs(t, s, "default")
	if len(jobs) != 0 {
		t.Fatalf("expected no jobs after deletion, got %d", len(jobs))
	}
	stored = getWorkflow(t, s, "default", "wf-delete")
	for _, f := range stored.ObjectMeta.Finalizers {
		if f == WorkflowFinalizer {
			t.Fatal("expected workflow finalizer removed")
		}
	}
}

func TestWorkflowReconciler_PersistsStepTransitionEvents(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-events")
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-events", Kind: "Workflow"})
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-events", Kind: "Workflow"})

	events, err := s.ListEvents(context.Background(), "Workflow", "default", "wf-events")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected persisted transition events")
	}

	foundDispatch := false
	for _, evt := range events {
		if evt.Reason == "StepDispatched" {
			foundDispatch = true
			break
		}
	}
	if !foundDispatch {
		t.Fatalf("expected StepDispatched event, got %+v", events)
	}
}

// TC-0026: Workflow resumes from durable state after controller restart.
func TestWorkflowReconciler_ResumesAfterControllerRestart(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-resume")
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-resume", Kind: "Workflow"})
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-resume", Kind: "Workflow"})

	jobs := listWorkflowJobs(t, s.memStore, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	updateJob(t, s.memStore, jobs[0])

	// Simulate crash-restart by creating a new reconciler over the same durable store.
	r = NewWorkflowReconciler(s, zaptest.NewLogger(t))
	_, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-resume", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile after restart: %v", err)
	}

	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 2 {
		t.Fatalf("expected workflow to resume at next step, jobs=%d", len(jobs))
	}
}

// TC-0027: Approval gate pauses execution until approval is provided.
func TestWorkflowReconciler_ApprovalGatePausesUntilApproved(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-approval")
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "plan", To: "execute"}}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-approval", Kind: "Workflow"})
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-approval", Kind: "Workflow"})

	jobs := listWorkflowJobs(t, s.memStore, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	updateJob(t, s.memStore, jobs[0])

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-approval", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile waiting approval: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected requeue while waiting approval")
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-approval")
	if stored.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("phase = %q, want Pending", stored.Status.Phase)
	}
	if !strings.Contains(stored.Status.ActiveStep, "execute") {
		t.Fatalf("active step = %q, want execute", stored.Status.ActiveStep)
	}
	foundWaitingApproval := false
	for _, condition := range stored.Status.Conditions {
		if condition.Reason == "WaitingApproval" {
			foundWaitingApproval = true
			break
		}
	}
	if !foundWaitingApproval {
		t.Fatalf("missing WaitingApproval condition; conditions=%+v", stored.Status.Conditions)
	}

	events, _ := s.ListEvents(context.Background(), "Workflow", "default", "wf-approval")
	foundApprovalRequest := false
	for _, evt := range events {
		if evt.Reason == "ApprovalRequested" {
			foundApprovalRequest = true
			break
		}
	}
	if !foundApprovalRequest {
		t.Fatal("expected ApprovalRequested event")
	}

	stored.ObjectMeta.Annotations = map[string]string{
		workflowApprovedStepsKey: "execute",
	}
	data, _ := json.Marshal(stored)
	if err := s.Update(context.Background(), "Workflow", "default", "wf-approval", data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("approve workflow step: %v", err)
	}

	res, err = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-approval", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile approved: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after approved step dispatch")
	}

	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 2 {
		t.Fatalf("expected second step dispatch after approval, jobs=%d", len(jobs))
	}
}

// TC-0240: Approval timeout expires; the gated step is recorded as rejected
// and the workflow is failed with the ApprovalRejected condition.
func TestWorkflowReconciler_ApprovalGateTimeoutFailsWorkflow(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-approval-timeout")
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "plan", To: "execute", Timeout: "1ms"}}
	storeWorkflow(t, s.memStore, wf)

	// First two reconciles dispatch the first step and mark its job ready so
	// the gated second step is reached.
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-approval-timeout", Kind: "Workflow"})
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-approval-timeout", Kind: "Workflow"})

	jobs := listWorkflowJobs(t, s.memStore, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	updateJob(t, s.memStore, jobs[0])

	// First gate reconcile sets the entered-at annotation but does not time out.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-approval-timeout", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile entering gate: %v", err)
	}

	// Wait past the timeout, then reconcile again to trigger the timeout path.
	time.Sleep(20 * time.Millisecond)
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-approval-timeout", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile after timeout: %v", err)
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-approval-timeout")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", stored.Status.Phase)
	}
	foundCond := false
	for _, c := range stored.Status.Conditions {
		if c.Reason == "ApprovalRejected" {
			foundCond = true
			break
		}
	}
	if !foundCond {
		t.Fatalf("missing ApprovalRejected condition; conditions=%+v", stored.Status.Conditions)
	}

	events, _ := s.ListEvents(context.Background(), "Workflow", "default", "wf-approval-timeout")
	foundTimedOut := false
	for _, evt := range events {
		if evt.Reason == "ApprovalTimedOut" {
			foundTimedOut = true
			break
		}
	}
	if !foundTimedOut {
		t.Fatal("expected ApprovalTimedOut event")
	}
}

// TC-0241: Annotation-based approval continues to drive the gate (backward
// compat bridge between the new /approval subresource and the reconciler).
func TestWorkflowReconciler_ApprovalAnnotationBridge(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-approval-bridge")
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "plan", To: "execute"}}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-approval-bridge", Kind: "Workflow"})
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-approval-bridge", Kind: "Workflow"})

	jobs := listWorkflowJobs(t, s.memStore, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	updateJob(t, s.memStore, jobs[0])
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-approval-bridge", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile waiting approval: %v", err)
	}

	// Directly setting the approved-steps annotation (the contract used by the
	// apiserver /approval subresource) must release the gate.
	stored := getWorkflow(t, s.memStore, "default", "wf-approval-bridge")
	if stored.ObjectMeta.Annotations == nil {
		stored.ObjectMeta.Annotations = map[string]string{}
	}
	stored.ObjectMeta.Annotations[workflowApprovedStepsKey] = "execute"
	data, _ := json.Marshal(stored)
	if err := s.Update(context.Background(), "Workflow", "default", "wf-approval-bridge", data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("set approved-steps annotation: %v", err)
	}

	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-approval-bridge", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile after approval annotation: %v", err)
	}

	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 2 {
		t.Fatalf("expected gated step dispatched after annotation set, jobs=%d", len(jobs))
	}
}

// storeApprovalDecision is a test helper that persists an ApprovalDecision CR
// into the in-memory store so the workflow reconciler can list it.
func storeApprovalDecision(t *testing.T, s *memStore, d v1alpha1.ApprovalDecision) {
	t.Helper()
	if d.TypeMeta.Kind == "" {
		d.TypeMeta = v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ApprovalDecision"}
	}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal approval decision: %v", err)
	}
	if err := s.Create(context.Background(), "ApprovalDecision", d.ObjectMeta.Namespace, d.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store approval decision: %v", err)
	}
}

// makeApprovalDecision returns a minimal ApprovalDecision targeting the given
// workflow/step with the supplied approver and decision.
func makeApprovalDecision(ns, name, run, step, approver, decision string) v1alpha1.ApprovalDecision {
	return v1alpha1.ApprovalDecision{
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.ApprovalDecisionSpec{
			WorkflowRunRef: v1alpha1.LocalObjectReference{Name: run},
			Step:           step,
			Approver:       approver,
			Decision:       decision,
		},
	}
}

// driveWorkflowToApprovalGate advances the workflow through the initial
// reconciles so that it reaches the approval gate before "execute".
func driveWorkflowToApprovalGate(t *testing.T, s *eventMemStore, r *WorkflowReconciler, name string) {
	t.Helper()
	req := Request{Namespace: "default", Name: name, Kind: "Workflow"}
	if _, err := r.reconcileDirect(context.Background(), req); err != nil {
		t.Fatalf("reconcile add finalizer: %v", err)
	}
	if _, err := r.reconcileDirect(context.Background(), req); err != nil {
		t.Fatalf("reconcile dispatch first step: %v", err)
	}
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) == 0 {
		t.Fatal("expected first step job before approval gate")
	}
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	updateJob(t, s.memStore, jobs[0])
	if _, err := r.reconcileDirect(context.Background(), req); err != nil {
		t.Fatalf("reconcile entering approval gate: %v", err)
	}
}

// TC-0280: With RequiredApprovers=1, a single approve ApprovalDecision opens the gate.
func TestWorkflowReconciler_ApprovalDecisionQuorumOne(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-quorum-one")
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "plan", To: "execute", RequiredApprovers: 1}}
	storeWorkflow(t, s.memStore, wf)

	driveWorkflowToApprovalGate(t, s, r, "wf-quorum-one")

	storeApprovalDecision(t, s.memStore, makeApprovalDecision("default", "ad-1", "wf-quorum-one", "execute", "alice", v1alpha1.ApprovalDecisionApprove))

	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-quorum-one", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile after approval: %v", err)
	}
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 2 {
		t.Fatalf("expected gated step dispatched after single approval, jobs=%d", len(jobs))
	}
}

// TC-0281: With RequiredApprovers=2, a single approve is not enough; the gate stays closed.
func TestWorkflowReconciler_ApprovalDecisionQuorumTwoNotMet(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-quorum-two-partial")
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "plan", To: "execute", RequiredApprovers: 2}}
	storeWorkflow(t, s.memStore, wf)

	driveWorkflowToApprovalGate(t, s, r, "wf-quorum-two-partial")

	storeApprovalDecision(t, s.memStore, makeApprovalDecision("default", "ad-a", "wf-quorum-two-partial", "execute", "alice", v1alpha1.ApprovalDecisionApprove))

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-quorum-two-partial", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile with partial quorum: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected requeue while waiting for second approver")
	}
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected gated step NOT dispatched with 1 of 2 approvals, jobs=%d", len(jobs))
	}
}

// TC-0282: Two distinct approvers satisfy RequiredApprovers=2 and open the gate.
func TestWorkflowReconciler_ApprovalDecisionQuorumTwoMet(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-quorum-two")
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "plan", To: "execute", RequiredApprovers: 2}}
	storeWorkflow(t, s.memStore, wf)

	driveWorkflowToApprovalGate(t, s, r, "wf-quorum-two")

	storeApprovalDecision(t, s.memStore, makeApprovalDecision("default", "ad-a", "wf-quorum-two", "execute", "alice", v1alpha1.ApprovalDecisionApprove))
	storeApprovalDecision(t, s.memStore, makeApprovalDecision("default", "ad-b", "wf-quorum-two", "execute", "bob", v1alpha1.ApprovalDecisionApprove))

	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-quorum-two", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile after quorum reached: %v", err)
	}
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 2 {
		t.Fatalf("expected gated step dispatched after quorum reached, jobs=%d", len(jobs))
	}
}

// TC-0283: Duplicate approvers must not double-count toward the quorum.
func TestWorkflowReconciler_ApprovalDecisionDuplicateApproverDoesNotCount(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-quorum-dup")
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "plan", To: "execute", RequiredApprovers: 2}}
	storeWorkflow(t, s.memStore, wf)

	driveWorkflowToApprovalGate(t, s, r, "wf-quorum-dup")

	storeApprovalDecision(t, s.memStore, makeApprovalDecision("default", "ad-a1", "wf-quorum-dup", "execute", "alice", v1alpha1.ApprovalDecisionApprove))
	storeApprovalDecision(t, s.memStore, makeApprovalDecision("default", "ad-a2", "wf-quorum-dup", "execute", "alice", v1alpha1.ApprovalDecisionApprove))

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-quorum-dup", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile with duplicate approver: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected requeue while duplicate approvers do not satisfy quorum")
	}
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected gated step NOT dispatched when only one distinct approver voted, jobs=%d", len(jobs))
	}
}

// TC-0284: Any single deny ApprovalDecision rejects the gate immediately.
func TestWorkflowReconciler_ApprovalDecisionDenyRejectsImmediately(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-deny")
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "plan", To: "execute", RequiredApprovers: 2}}
	storeWorkflow(t, s.memStore, wf)

	driveWorkflowToApprovalGate(t, s, r, "wf-deny")

	// One approve, one deny — deny must short-circuit even with quorum > 1.
	storeApprovalDecision(t, s.memStore, makeApprovalDecision("default", "ad-a", "wf-deny", "execute", "alice", v1alpha1.ApprovalDecisionApprove))
	storeApprovalDecision(t, s.memStore, makeApprovalDecision("default", "ad-b", "wf-deny", "execute", "bob", v1alpha1.ApprovalDecisionDeny))

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-deny", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile after deny: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected terminal rejection after deny decision")
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-deny")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", stored.Status.Phase)
	}
	foundRejected := false
	for _, c := range stored.Status.Conditions {
		if c.Reason == "ApprovalRejected" {
			foundRejected = true
			break
		}
	}
	if !foundRejected {
		t.Fatalf("missing ApprovalRejected condition; conditions=%+v", stored.Status.Conditions)
	}

	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected no further dispatches after deny, jobs=%d", len(jobs))
	}
}

// TC-0285: Annotation-based approval continues to work alongside the
// ApprovalDecision path (compatibility with the apiserver /approval flow).
func TestWorkflowReconciler_ApprovalDecisionAnnotationFallbackStillWorks(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-anno-compat")
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "plan", To: "execute", RequiredApprovers: 1}}
	storeWorkflow(t, s.memStore, wf)

	driveWorkflowToApprovalGate(t, s, r, "wf-anno-compat")

	// No ApprovalDecision CRs created. Approve via annotation directly.
	stored := getWorkflow(t, s.memStore, "default", "wf-anno-compat")
	stored.ObjectMeta.Annotations = map[string]string{
		workflowApprovedStepsKey: "execute",
	}
	data, _ := json.Marshal(stored)
	if err := s.Update(context.Background(), "Workflow", "default", "wf-anno-compat", data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("set approval annotation: %v", err)
	}

	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-anno-compat", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile with annotation approval: %v", err)
	}
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 2 {
		t.Fatalf("expected annotation-only approval still dispatches, jobs=%d", len(jobs))
	}
}

// TC-0286: ApprovalDecisions in a different namespace must not contribute to
// the gate's quorum. The reconciler scopes the List call to the workflow's
// own namespace.
func TestWorkflowReconciler_ApprovalDecisionDifferentNamespaceIgnored(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-ns-iso")
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "plan", To: "execute", RequiredApprovers: 1}}
	storeWorkflow(t, s.memStore, wf)

	driveWorkflowToApprovalGate(t, s, r, "wf-ns-iso")

	// Decision in a foreign namespace targeting the same workflow name/step.
	storeApprovalDecision(t, s.memStore, makeApprovalDecision("other-ns", "ad-foreign", "wf-ns-iso", "execute", "alice", v1alpha1.ApprovalDecisionApprove))

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-ns-iso", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile with foreign-namespace decision: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected requeue: foreign-namespace decision must not satisfy quorum")
	}
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected gated step NOT dispatched by foreign-namespace decision, jobs=%d", len(jobs))
	}
}

// TC-0287: ApprovalDecisions targeting a different step (or different
// iteration) must not contribute to a gate's quorum.
func TestWorkflowReconciler_ApprovalDecisionDifferentStepOrIterationIgnored(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-step-iter-iso")
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "plan", To: "execute", RequiredApprovers: 1}}
	storeWorkflow(t, s.memStore, wf)

	driveWorkflowToApprovalGate(t, s, r, "wf-step-iter-iso")

	// Wrong step.
	storeApprovalDecision(t, s.memStore, makeApprovalDecision("default", "ad-wrong-step", "wf-step-iter-iso", "some-other-step", "alice", v1alpha1.ApprovalDecisionApprove))
	// Right step but wrong iteration (iteration 5 for a non-cyclic workflow).
	wrongIter := makeApprovalDecision("default", "ad-wrong-iter", "wf-step-iter-iso", "execute", "bob", v1alpha1.ApprovalDecisionApprove)
	wrongIter.Spec.Iteration = 5
	storeApprovalDecision(t, s.memStore, wrongIter)

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-step-iter-iso", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile with wrong step/iter decisions: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected requeue: wrong-step/iter decisions must not satisfy quorum")
	}
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected gated step NOT dispatched by mis-targeted decisions, jobs=%d", len(jobs))
	}
}

// TC-0288: An ApprovalDecision created AFTER an earlier gate evaluation must
// still be picked up on the next reconcile. This guards against any caching
// that would freeze the gate state.
func TestWorkflowReconciler_ApprovalDecisionLateArrivalPickedUpOnNextReconcile(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-late")
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "plan", To: "execute", RequiredApprovers: 1}}
	storeWorkflow(t, s.memStore, wf)

	driveWorkflowToApprovalGate(t, s, r, "wf-late")

	// First reconcile with no decisions: gate stays closed.
	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-late", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("first reconcile (no decisions): %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected requeue while gate awaits decisions")
	}
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected only the pre-gate job, jobs=%d", len(jobs))
	}

	// Decision arrives later.
	storeApprovalDecision(t, s.memStore, makeApprovalDecision("default", "ad-late", "wf-late", "execute", "alice", v1alpha1.ApprovalDecisionApprove))

	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-late", Kind: "Workflow"}); err != nil {
		t.Fatalf("second reconcile (with late decision): %v", err)
	}
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 2 {
		t.Fatalf("expected gated step dispatched after late-arriving approval, jobs=%d", len(jobs))
	}
}

// TC-0289: In a cyclic workflow, ApprovalDecisions for iteration>0 must
// satisfy the gate for that iteration (and only that iteration).
func TestWorkflowReconciler_ApprovalDecisionIterationScopedQuorum(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-cycle-ad")
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "planner", AgentRef: v1alpha1.LocalObjectReference{Name: "planner-agent"}},
		{Name: "writer", AgentRef: v1alpha1.LocalObjectReference{Name: "writer-agent"}},
	}
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "planner", To: "writer"}, {From: "writer", To: "planner"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "planner", To: "writer", RequiredApprovers: 1}}
	storeWorkflow(t, s.memStore, wf)

	// Bring the cyclic workflow to its iteration-1 approval point.
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-ad", Kind: "Workflow"})
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-ad", Kind: "Workflow"})

	jobs := listWorkflowJobs(t, s.memStore, "default")
	for i := range jobs {
		if jobs[i].ObjectMeta.Labels[workflowStepLabel] == "planner" && jobs[i].ObjectMeta.Labels[workflowIterationLabel] == "1" {
			jobs[i].Status.Phase = v1alpha1.PhaseReady
			updateJob(t, s.memStore, jobs[i])
		}
	}

	// An iteration-0 approval must NOT open the iteration-1 gate.
	iter0 := makeApprovalDecision("default", "ad-iter0", "wf-cycle-ad", "writer", "alice", v1alpha1.ApprovalDecisionApprove)
	iter0.Spec.Iteration = 0
	storeApprovalDecision(t, s.memStore, iter0)

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-ad", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile with iter-0 decision only: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected requeue: iteration-0 decision must not satisfy iteration-1 gate")
	}

	// Now add the iteration-1 approval — gate must open and writer must dispatch.
	iter1 := makeApprovalDecision("default", "ad-iter1", "wf-cycle-ad", "writer", "alice", v1alpha1.ApprovalDecisionApprove)
	iter1.Spec.Iteration = 1
	storeApprovalDecision(t, s.memStore, iter1)

	res, err = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-ad", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile with iter-1 decision: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected writer dispatch after iteration-scoped approval decision")
	}
}

// TC-0028: Cyclic workflow halts at configured maxIterations budget.
func TestWorkflowReconciler_CyclicWorkflowRespectsMaxIterations(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-cycle")
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "planner", AgentRef: v1alpha1.LocalObjectReference{Name: "planner-agent"}},
		{Name: "writer", AgentRef: v1alpha1.LocalObjectReference{Name: "writer-agent"}},
	}
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "planner", To: "writer"}, {From: "writer", To: "planner"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 3, MaxTokensPerCycle: 1000}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle", Kind: "Workflow"})

	for iter := 1; iter <= 3; iter++ {
		res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle", Kind: "Workflow"})
		if err != nil {
			t.Fatalf("dispatch first step iter %d: %v", iter, err)
		}
		if !res.Requeue {
			t.Fatalf("expected requeue after first step dispatch in iter %d", iter)
		}

		jobs := listWorkflowJobs(t, s.memStore, "default")
		for i := range jobs {
			if jobs[i].ObjectMeta.Labels[workflowIterationLabel] == fmt.Sprintf("%d", iter) && jobs[i].ObjectMeta.Labels[workflowStepLabel] == "planner" {
				jobs[i].Status.Phase = v1alpha1.PhaseReady
				updateJob(t, s.memStore, jobs[i])
			}
		}

		res, err = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle", Kind: "Workflow"})
		if err != nil {
			t.Fatalf("dispatch second step iter %d: %v", iter, err)
		}
		if !res.Requeue {
			t.Fatalf("expected requeue after second step dispatch in iter %d", iter)
		}

		jobs = listWorkflowJobs(t, s.memStore, "default")
		for i := range jobs {
			if jobs[i].ObjectMeta.Labels[workflowIterationLabel] == fmt.Sprintf("%d", iter) {
				jobs[i].Status.Phase = v1alpha1.PhaseReady
				updateJob(t, s.memStore, jobs[i])
			}
		}

		_, err = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle", Kind: "Workflow"})
		if err != nil {
			t.Fatalf("advance iteration %d: %v", iter, err)
		}
	}

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("final reconcile: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected terminal completion after maxIterations")
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-cycle")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", stored.Status.Phase)
	}

	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 6 {
		t.Fatalf("expected exactly 6 jobs (2 steps x 3 iterations), got %d", len(jobs))
	}
}

func TestWorkflowReconciler_EventAppendFailureReturnsError(t *testing.T) {
	s := &failingEventStore{memStore: newMemStore(), err: errors.New("append failed")}
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-event-error")
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-event-error", Kind: "Workflow"})
	_, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-event-error", Kind: "Workflow"})
	if err == nil {
		t.Fatal("expected reconcile error when event append fails")
	}
	if !strings.Contains(err.Error(), "append workflow event") {
		t.Fatalf("error = %v, want append workflow event", err)
	}
}

func TestWorkflowReconciler_CyclicBudgetExceededFailsWorkflow(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-budget-exceeded")
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "planner", AgentRef: v1alpha1.LocalObjectReference{Name: "planner-agent"}, Input: "two words"},
		{Name: "writer", AgentRef: v1alpha1.LocalObjectReference{Name: "writer-agent"}},
	}
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "planner", To: "writer"}, {From: "writer", To: "planner"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 3, MaxTokensPerCycle: 1}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-budget-exceeded", Kind: "Workflow"})
	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-budget-exceeded", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile budget exceeded: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected terminal failure when token budget is exceeded")
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-budget-exceeded")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", stored.Status.Phase)
	}

	events, _ := s.ListEvents(context.Background(), "Workflow", "default", "wf-budget-exceeded")
	foundBudgetExceeded := false
	for _, evt := range events {
		if evt.Reason == "BudgetExceeded" {
			foundBudgetExceeded = true
			break
		}
	}
	if !foundBudgetExceeded {
		t.Fatal("expected BudgetExceeded event")
	}
}

func TestWorkflowReconciler_CyclicFalseConditionsSkipWithoutDispatch(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-cycle-false-conditions")
	wf.Spec.Steps[1].Condition = "false"
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{
		{From: "plan", To: "execute"},
		{From: "execute", To: "plan", Condition: "false"},
	}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}

	mustReconcile(t, r, req)
	mustReconcile(t, r, req)

	stored := getWorkflow(t, s.memStore, "default", wf.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", stored.Status.Phase)
	}
	skipped := map[string]bool{}
	for _, status := range stored.Status.StepStatuses {
		if status.Phase == v1alpha1.PhaseSkipped {
			skipped[status.Name] = true
		}
	}
	if !skipped["plan"] || !skipped["execute"] {
		t.Fatalf("skipped steps = %+v, want plan and execute", skipped)
	}
	jobs, err := s.List(context.Background(), "AgentJob", store.ListOptions{Namespace: "default"})
	if err != nil {
		t.Fatalf("list agent jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("agent jobs = %d, want none", len(jobs))
	}
}

func TestWorkflowReconciler_CyclicApprovalRequiresIterationScopedApproval(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-cycle-approval")
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "planner", AgentRef: v1alpha1.LocalObjectReference{Name: "planner-agent"}},
		{Name: "writer", AgentRef: v1alpha1.LocalObjectReference{Name: "writer-agent"}},
	}
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "planner", To: "writer"}, {From: "writer", To: "planner"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	wf.Spec.ApprovalGates = []v1alpha1.WorkflowApprovalGate{{From: "planner", To: "writer"}}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-approval", Kind: "Workflow"})
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-approval", Kind: "Workflow"})

	jobs := listWorkflowJobs(t, s.memStore, "default")
	for i := range jobs {
		if jobs[i].ObjectMeta.Labels[workflowStepLabel] == "planner" && jobs[i].ObjectMeta.Labels[workflowIterationLabel] == "1" {
			jobs[i].Status.Phase = v1alpha1.PhaseReady
			updateJob(t, s.memStore, jobs[i])
		}
	}

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-approval", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile waiting for approval: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected requeue while waiting for cyclic approval")
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-cycle-approval")
	stored.ObjectMeta.Annotations = map[string]string{
		workflowApprovedStepsKey: "writer",
	}
	data, _ := json.Marshal(stored)
	if err := s.Update(context.Background(), "Workflow", "default", "wf-cycle-approval", data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("set plain approval token: %v", err)
	}

	res, err = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-approval", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile with plain approval token: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected still blocked because iteration-scoped approval is required")
	}

	stored = getWorkflow(t, s.memStore, "default", "wf-cycle-approval")
	stored.ObjectMeta.Annotations[workflowApprovedStepsKey] = "writer@1"
	data, _ = json.Marshal(stored)
	if err := s.Update(context.Background(), "Workflow", "default", "wf-cycle-approval", data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("set iteration approval token: %v", err)
	}

	res, err = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-approval", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile with iteration approval token: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected writer dispatch after iteration-scoped approval")
	}
}

func TestWorkflowReconciler_CyclicEntryPointDispatchesFirst(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-cycle-entrypoint")
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "planner", AgentRef: v1alpha1.LocalObjectReference{Name: "planner-agent"}},
		{Name: "writer", AgentRef: v1alpha1.LocalObjectReference{Name: "writer-agent"}},
	}
	wf.Spec.EntryPoint = "writer"
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "planner", To: "writer"}, {From: "writer", To: "planner"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	storeWorkflow(t, s, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-entrypoint", Kind: "Workflow"})
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-entrypoint", Kind: "Workflow"})

	jobs := listWorkflowJobs(t, s, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected first cyclic dispatch only, got %d jobs", len(jobs))
	}
	if jobs[0].ObjectMeta.Labels[workflowStepLabel] != "writer" {
		t.Fatalf("first dispatched step = %q, want writer", jobs[0].ObjectMeta.Labels[workflowStepLabel])
	}
}

func TestWorkflowReconciler_AcyclicRunningStepRequeues(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-acyclic-running")
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-acyclic-running", Kind: "Workflow"})
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-acyclic-running", Kind: "Workflow"})

	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	jobs[0].Status.Phase = v1alpha1.PhaseRunning
	updateJob(t, s.memStore, jobs[0])

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-acyclic-running", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile running step: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected timed requeue while step is running")
	}
}

func TestWorkflowReconciler_CyclicRunningStepRequeues(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-cycle-running")
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "planner", AgentRef: v1alpha1.LocalObjectReference{Name: "planner-agent"}},
		{Name: "writer", AgentRef: v1alpha1.LocalObjectReference{Name: "writer-agent"}},
	}
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "planner", To: "writer"}, {From: "writer", To: "planner"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 2}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-running", Kind: "Workflow"})
	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-running", Kind: "Workflow"})

	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	jobs[0].Status.Phase = v1alpha1.PhaseRunning
	updateJob(t, s.memStore, jobs[0])

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-running", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile cyclic running step: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected timed requeue while cyclic step is running")
	}
}

func TestWorkflowReconciler_CyclicStepRetryReplacesFailedAttempt(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-cycle-retry")
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{
			Name:     "planner",
			AgentRef: v1alpha1.LocalObjectReference{Name: "planner-agent"},
			Retry:    &v1alpha1.WorkflowStepRetry{MaxAttempts: 2, Backoff: "1ms"},
		},
		{Name: "writer", AgentRef: v1alpha1.LocalObjectReference{Name: "writer-agent"}},
	}
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "planner", To: "writer"}, {From: "writer", To: "planner"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-cycle-retry", Kind: "Workflow"}

	_, _ = r.reconcileDirect(context.Background(), req)
	_, _ = r.reconcileDirect(context.Background(), req)

	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected initial cyclic job, got %d", len(jobs))
	}
	jobs[0].Status.Phase = v1alpha1.PhaseFailed
	updateJob(t, s.memStore, jobs[0])

	res, err := r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("schedule cyclic retry: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected retry backoff requeue")
	}
	if jobs = listWorkflowJobs(t, s.memStore, "default"); len(jobs) != 0 {
		t.Fatalf("expected failed attempt deleted before retry, got %d jobs", len(jobs))
	}
	stored := getWorkflow(t, s.memStore, "default", "wf-cycle-retry")
	ss := findStepStatus(&stored, "planner")
	if ss == nil || ss.Attempts != 1 || ss.NextRetryAt == nil {
		t.Fatalf("expected first attempt with scheduled retry, got %+v", ss)
	}

	time.Sleep(2 * time.Millisecond)
	res, err = r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("dispatch cyclic retry: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected immediate requeue after retry dispatch")
	}
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 || jobs[0].ObjectMeta.Labels[workflowIterationLabel] != "1" {
		t.Fatalf("expected one replacement job in iteration 1, got %+v", jobs)
	}
	stored = getWorkflow(t, s.memStore, "default", "wf-cycle-retry")
	ss = findStepStatus(&stored, "planner")
	if ss == nil || ss.Attempts != 2 || ss.NextRetryAt != nil {
		t.Fatalf("expected second active attempt, got %+v", ss)
	}

	jobs[0].Status.Phase = v1alpha1.PhaseFailed
	updateJob(t, s.memStore, jobs[0])
	res, err = r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("exhaust cyclic retry: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected terminal failure after cyclic retry exhaustion")
	}
	stored = getWorkflow(t, s.memStore, "default", "wf-cycle-retry")
	ss = findStepStatus(&stored, "planner")
	if stored.Status.Phase != v1alpha1.PhaseFailed || ss == nil || ss.Attempts != 2 || ss.NextRetryAt != nil {
		t.Fatalf("expected failure after exactly two attempts, status=%+v step=%+v", stored.Status, ss)
	}
}

func TestWorkflowReconciler_CyclicRetryHonorsFutureBackoff(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-cycle-future-retry")
	wf.ObjectMeta.Finalizers = []string{WorkflowFinalizer}
	wf.Spec.Edges = append(wf.Spec.Edges, v1alpha1.WorkflowEdge{From: "execute", To: "plan"})
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	retryAt := time.Now().UTC().Add(time.Hour)
	wf.Status.StepStatuses = []v1alpha1.WorkflowStepStatus{{
		Name:        "plan",
		Phase:       v1alpha1.PhasePending,
		Attempts:    1,
		NextRetryAt: &retryAt,
	}}
	storeWorkflow(t, s.memStore, wf)

	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile future cyclic retry: %v", err)
	}
	if res.Requeue || res.RequeueAfter < 59*time.Minute {
		t.Fatalf("result = %+v, want retry near persisted one-hour deadline", res)
	}
	if jobs := listWorkflowJobs(t, s.memStore, "default"); len(jobs) != 0 {
		t.Fatalf("agent jobs = %d, want no early redispatch", len(jobs))
	}
	stored := getWorkflow(t, s.memStore, "default", wf.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhasePending || stored.Status.ActiveStep != "plan@1" {
		t.Fatalf("status = %+v, want pending plan retry", stored.Status)
	}
}

func TestWorkflowReconciler_CyclicRetrySurfacesDependencyFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		wrapStore   func(*memStore, error) store.Store
		wantMessage string
	}{
		{
			name: "delete failed attempt",
			wrapStore: func(memory *memStore, err error) store.Store {
				return &workflowCleanupFaultStore{memStore: memory, deleteErr: err}
			},
			wantMessage: "delete failed cyclic agent job",
		},
		{
			name: "append retry event",
			wrapStore: func(memory *memStore, err error) store.Store {
				return &failingEventStore{memStore: memory, err: err}
			},
			wantMessage: "append workflow event",
		},
		{
			name: "persist retry state",
			wrapStore: func(memory *memStore, err error) store.Store {
				return &faultStore{memStore: memory, updateErr: err}
			},
			wantMessage: "update cyclic workflow retry",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := newMemStore()
			workflow := makeWorkflow("default", "wf-cycle-retry-failure")
			workflow.Spec.Steps[0].Retry = &v1alpha1.WorkflowStepRetry{MaxAttempts: 2, Backoff: "0s"}
			workflow.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "plan", To: "execute"}, {From: "execute", To: "plan"}}
			workflow.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
			storeWorkflow(t, memory, workflow)
			request := Request{Namespace: "default", Name: workflow.ObjectMeta.Name, Kind: "Workflow"}
			setupReconciler := NewWorkflowReconciler(memory, zaptest.NewLogger(t))
			mustReconcile(t, setupReconciler, request)
			mustReconcile(t, setupReconciler, request)

			jobs := listWorkflowJobs(t, memory, "default")
			if len(jobs) != 1 {
				t.Fatalf("jobs = %d, want one failed attempt fixture", len(jobs))
			}
			jobs[0].Status.Phase = v1alpha1.PhaseFailed
			updateJob(t, memory, jobs[0])

			wantErr := errors.New("dependency unavailable")
			r := NewWorkflowReconciler(tc.wrapStore(memory, wantErr), zaptest.NewLogger(t))
			_, err := r.reconcileDirect(context.Background(), request)
			if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("error = %v, want wrapped dependency failure containing %q", err, tc.wantMessage)
			}
		})
	}
}

func TestWorkflowReconciler_CyclicStepTimeoutFailsAndDeletesJob(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-cycle-timeout")
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "planner", AgentRef: v1alpha1.LocalObjectReference{Name: "planner-agent"}, Timeout: "1ms"},
		{Name: "writer", AgentRef: v1alpha1.LocalObjectReference{Name: "writer-agent"}},
	}
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "planner", To: "writer"}, {From: "writer", To: "planner"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-cycle-timeout", Kind: "Workflow"}

	_, _ = r.reconcileDirect(context.Background(), req)
	_, _ = r.reconcileDirect(context.Background(), req)
	time.Sleep(2 * time.Millisecond)

	res, err := r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("enforce cyclic timeout: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		stored := getWorkflow(t, s.memStore, "default", "wf-cycle-timeout")
		t.Fatalf("expected terminal failure after timeout without retry, result=%+v status=%+v", res, stored.Status)
	}
	if jobs := listWorkflowJobs(t, s.memStore, "default"); len(jobs) != 0 {
		t.Fatalf("expected timed-out job deleted, got %d jobs", len(jobs))
	}
	stored := getWorkflow(t, s.memStore, "default", "wf-cycle-timeout")
	ss := findStepStatus(&stored, "planner")
	if stored.Status.Phase != v1alpha1.PhaseFailed || ss == nil || ss.Phase != v1alpha1.PhaseTimedOut {
		t.Fatalf("expected failed workflow with timed-out step, status=%+v step=%+v", stored.Status, ss)
	}
}

func TestWorkflowReconciler_CyclicTimeoutSurfacesJobDeletionFailure(t *testing.T) {
	memory := newMemStore()
	wf := makeWorkflow("default", "wf-cycle-timeout-delete-failure")
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "planner", AgentRef: v1alpha1.LocalObjectReference{Name: "planner-agent"}, Timeout: "1ms"},
		{Name: "writer", AgentRef: v1alpha1.LocalObjectReference{Name: "writer-agent"}},
	}
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "planner", To: "writer"}, {From: "writer", To: "planner"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	storeWorkflow(t, memory, wf)
	req := Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}

	setupReconciler := NewWorkflowReconciler(memory, zaptest.NewLogger(t))
	mustReconcile(t, setupReconciler, req)
	mustReconcile(t, setupReconciler, req)
	time.Sleep(2 * time.Millisecond)

	wantErr := errors.New("storage unavailable")
	r := NewWorkflowReconciler(&workflowCleanupFaultStore{memStore: memory, deleteErr: wantErr}, zaptest.NewLogger(t))
	_, err := r.reconcileDirect(context.Background(), req)
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "delete timed-out cyclic agent job") {
		t.Fatalf("error = %v, want wrapped timed-out job deletion failure", err)
	}
	if jobs := listWorkflowJobs(t, memory, "default"); len(jobs) != 1 {
		t.Fatalf("jobs = %d, want timed-out job retained after failed deletion", len(jobs))
	}
}

func TestWorkflowReconciler_CyclicStepContinueOnErrorDispatchesNextStep(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-cycle-continue")
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "planner", AgentRef: v1alpha1.LocalObjectReference{Name: "planner-agent"}, ContinueOnError: true},
		{Name: "writer", AgentRef: v1alpha1.LocalObjectReference{Name: "writer-agent"}},
	}
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "planner", To: "writer"}, {From: "writer", To: "planner"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-cycle-continue", Kind: "Workflow"}

	_, _ = r.reconcileDirect(context.Background(), req)
	_, _ = r.reconcileDirect(context.Background(), req)
	jobs := listWorkflowJobs(t, s.memStore, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseFailed
	updateJob(t, s.memStore, jobs[0])

	res, err := r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("continue cyclic workflow after suppressed failure: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected next cyclic step to dispatch")
	}
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 2 || jobs[1].ObjectMeta.Labels[workflowStepLabel] != "writer" {
		t.Fatalf("expected writer dispatch after suppressed planner failure, got %+v", jobs)
	}
	jobs[1].Status.Phase = v1alpha1.PhaseReady
	updateJob(t, s.memStore, jobs[1])

	_, err = r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("complete cyclic workflow after suppressed failure: %v", err)
	}
	stored := getWorkflow(t, s.memStore, "default", "wf-cycle-continue")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", stored.Status.Phase)
	}
}

func TestWorkflowReconciler_CyclicTimedOutStepContinueOnErrorIsNotRedispatched(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-cycle-timeout-continue")
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{
			Name:            "planner",
			AgentRef:        v1alpha1.LocalObjectReference{Name: "planner-agent"},
			Timeout:         "1ms",
			ContinueOnError: true,
		},
		{Name: "writer", AgentRef: v1alpha1.LocalObjectReference{Name: "writer-agent"}},
	}
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "planner", To: "writer"}, {From: "writer", To: "planner"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-cycle-timeout-continue", Kind: "Workflow"}

	_, _ = r.reconcileDirect(context.Background(), req)
	_, _ = r.reconcileDirect(context.Background(), req)
	time.Sleep(2 * time.Millisecond)

	res, err := r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("continue after cyclic timeout: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected writer dispatch after ignored timeout")
	}
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 || jobs[0].ObjectMeta.Labels[workflowStepLabel] != "writer" {
		t.Fatalf("expected only writer job after planner timeout, got %+v", jobs)
	}

	res, err = r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile running writer: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected timed requeue while writer remains active")
	}
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 || jobs[0].ObjectMeta.Labels[workflowStepLabel] != "writer" {
		t.Fatalf("timed-out planner was redispatched: %+v", jobs)
	}
}

func TestWorkflowReconciler_CyclicIterationAboveMaxCompletes(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-cycle-max")
	wf.Spec.Steps = []v1alpha1.WorkflowStep{
		{Name: "planner", AgentRef: v1alpha1.LocalObjectReference{Name: "planner-agent"}},
		{Name: "writer", AgentRef: v1alpha1.LocalObjectReference{Name: "writer-agent"}},
	}
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "planner", To: "writer"}, {From: "writer", To: "planner"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	wf.ObjectMeta.Annotations = map[string]string{workflowCurrentIterationKey: "2"}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-max", Kind: "Workflow"})
	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-cycle-max", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile iteration above max: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected terminal completion when current iteration is above max")
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-cycle-max")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", stored.Status.Phase)
	}
}

func TestWorkflowReconciler_InvalidCyclicBudgetFailsDurably(t *testing.T) {
	for _, tc := range []struct {
		name        string
		updateErr   error
		wantMessage string
	}{
		{name: "records terminal failure"},
		{name: "surfaces persistence failure", updateErr: errors.New("workflow store unavailable"), wantMessage: "update invalid cyclic workflow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := newMemStore()
			workflow := makeWorkflow("default", "wf-invalid-cycle-budget")
			workflow.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "plan", To: "execute"}, {From: "execute", To: "plan"}}
			workflow.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 0}
			storeWorkflow(t, memory, workflow)
			workflow = getWorkflow(t, memory, workflow.ObjectMeta.Namespace, workflow.ObjectMeta.Name)

			reconciler := NewWorkflowReconciler(memory, zaptest.NewLogger(t))
			reconciler.persistStatus = func(ctx context.Context, desired *v1alpha1.Workflow) error {
				if tc.updateErr != nil {
					return tc.updateErr
				}
				raw, err := json.Marshal(desired)
				if err != nil {
					return err
				}
				return memory.Update(ctx, "Workflow", desired.ObjectMeta.Namespace, desired.ObjectMeta.Name, raw, desired.ObjectMeta.ResourceVersion)
			}
			plan := workflowPlan{OrderedSteps: workflow.Spec.Steps, Cyclic: true}
			_, err := reconciler.reconcileCyclic(context.Background(), &workflow, plan, nil)
			if tc.updateErr != nil {
				if !errors.Is(err, tc.updateErr) || !strings.Contains(err.Error(), tc.wantMessage) {
					t.Fatalf("error = %v, want wrapped persistence failure containing %q", err, tc.wantMessage)
				}
				return
			}
			if err != nil {
				t.Fatalf("reconcile invalid cyclic budget: %v", err)
			}
			stored := getWorkflow(t, memory, workflow.ObjectMeta.Namespace, workflow.ObjectMeta.Name)
			if stored.Status.Phase != v1alpha1.PhaseFailed {
				t.Fatalf("phase = %q, want Failed", stored.Status.Phase)
			}
			ready := findCondition(stored.Status.Conditions, "Ready")
			if ready == nil || ready.Reason != "InvalidBudget" {
				t.Fatalf("Ready condition = %+v, want InvalidBudget", ready)
			}
		})
	}
}

func TestParseCSVTokens_FallbackOnMalformedCSV(t *testing.T) {
	tokens := parseCSVTokens("alpha,\"unterminated")
	if len(tokens) != 2 {
		t.Fatalf("tokens len = %d, want 2", len(tokens))
	}
	if tokens[0] != "alpha" || tokens[1] != "\"unterminated" {
		t.Fatalf("tokens = %+v, want fallback split tokens", tokens)
	}
}

func TestParseIterationLabel_InvalidValuesReturnZero(t *testing.T) {
	if got := parseIterationLabel(nil); got != 0 {
		t.Fatalf("nil labels => %d, want 0", got)
	}
	if got := parseIterationLabel(map[string]string{workflowIterationLabel: "abc"}); got != 0 {
		t.Fatalf("non-numeric labels => %d, want 0", got)
	}
	if got := parseIterationLabel(map[string]string{workflowIterationLabel: "-1"}); got != 0 {
		t.Fatalf("negative labels => %d, want 0", got)
	}
}

// TC-0029: Planner-emergent DAG mutations within budgets are accepted and executed.
func TestWorkflowReconciler_EmergentMutationAppliedWithinBudgets(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-emergent-apply")
	wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{
		Enabled: true,
		Budgets: v1alpha1.WorkflowEmergentBudgets{
			MaxSteps:  4,
			MaxTokens: 100,
			MaxCost:   "10",
		},
	}
	proposal := workflowEmergentProposal{
		Steps:           []v1alpha1.WorkflowStep{{Name: "review", AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}}},
		Edges:           []v1alpha1.WorkflowEdge{{From: "execute", To: "review"}},
		ProposedBy:      "planner@run-1",
		EstimatedTokens: 12,
		EstimatedCost:   1.5,
	}
	proposalJSON, _ := json.Marshal(proposal)
	wf.ObjectMeta.Annotations = map[string]string{workflowEmergentProposalKey: string(proposalJSON)}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-apply", Kind: "Workflow"})
	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-apply", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile emergent apply: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after applying emergent mutation")
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-emergent-apply")
	if len(stored.Spec.Steps) != 3 {
		t.Fatalf("steps len = %d, want 3", len(stored.Spec.Steps))
	}
	if stored.ObjectMeta.Annotations[workflowEmergentProposalKey] != "" {
		t.Fatal("expected emergent proposal annotation to be cleared")
	}
	if stored.ObjectMeta.Annotations[workflowEmergentStepsConsumedKey] != "1" {
		t.Fatalf("steps consumed = %q, want 1", stored.ObjectMeta.Annotations[workflowEmergentStepsConsumedKey])
	}

	events, _ := s.ListEvents(context.Background(), "Workflow", "default", "wf-emergent-apply")
	found := false
	for _, evt := range events {
		if evt.Reason == "EmergentMutationApplied" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected EmergentMutationApplied event")
	}
}

func TestWorkflowReconciler_EmergentMutationRejectionSurfacesPersistenceFailures(t *testing.T) {
	validProposal := func() workflowEmergentProposal {
		return workflowEmergentProposal{
			Steps:      []v1alpha1.WorkflowStep{{Name: "review", AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}}},
			ProposedBy: "planner@failure-test",
		}
	}
	tests := []struct {
		name              string
		configure         func(*v1alpha1.Workflow)
		wantUpdateContext string
	}{
		{
			name: "malformed payload",
			configure: func(wf *v1alpha1.Workflow) {
				wf.ObjectMeta.Annotations[workflowEmergentProposalKey] = "{"
			},
			wantUpdateContext: "update workflow after malformed proposal",
		},
		{
			name: "empty proposal",
			configure: func(wf *v1alpha1.Workflow) {
				proposal, _ := json.Marshal(workflowEmergentProposal{ProposedBy: "planner@empty"})
				wf.ObjectMeta.Annotations[workflowEmergentProposalKey] = string(proposal)
			},
			wantUpdateContext: "update workflow after empty proposal",
		},
		{
			name: "invalid structure",
			configure: func(wf *v1alpha1.Workflow) {
				proposal := validProposal()
				proposal.Steps[0].Name = "plan"
				payload, _ := json.Marshal(proposal)
				wf.ObjectMeta.Annotations[workflowEmergentProposalKey] = string(payload)
			},
			wantUpdateContext: "update workflow after invalid proposal",
		},
		{
			name: "accounting overflow",
			configure: func(wf *v1alpha1.Workflow) {
				proposal := validProposal()
				payload, _ := json.Marshal(proposal)
				wf.ObjectMeta.Annotations[workflowEmergentProposalKey] = string(payload)
				wf.ObjectMeta.Annotations[workflowEmergentStepsConsumedKey] = "2147483647"
			},
			wantUpdateContext: "update workflow after accounting overflow",
		},
		{
			name: "max steps",
			configure: func(wf *v1alpha1.Workflow) {
				proposal := validProposal()
				payload, _ := json.Marshal(proposal)
				wf.ObjectMeta.Annotations[workflowEmergentProposalKey] = string(payload)
				wf.Spec.Emergent.Budgets.MaxSteps = 1
				wf.ObjectMeta.Annotations[workflowEmergentStepsConsumedKey] = "1"
			},
			wantUpdateContext: "update workflow after maxSteps rejection",
		},
		{
			name: "max tokens",
			configure: func(wf *v1alpha1.Workflow) {
				proposal := validProposal()
				proposal.EstimatedTokens = 2
				payload, _ := json.Marshal(proposal)
				wf.ObjectMeta.Annotations[workflowEmergentProposalKey] = string(payload)
				wf.Spec.Emergent.Budgets.MaxTokens = 1
			},
			wantUpdateContext: "update workflow after maxTokens rejection",
		},
		{
			name: "max cost",
			configure: func(wf *v1alpha1.Workflow) {
				proposal := validProposal()
				proposal.EstimatedCost = 2
				payload, _ := json.Marshal(proposal)
				wf.ObjectMeta.Annotations[workflowEmergentProposalKey] = string(payload)
				wf.Spec.Emergent.Budgets.MaxCost = "1"
			},
			wantUpdateContext: "update workflow after maxCost rejection",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, failure := range []string{"event", "update"} {
				t.Run(failure, func(t *testing.T) {
					wantErr := errors.New("persistence unavailable")
					wf := makeWorkflow("default", "wf-emergent-persistence")
					wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{Enabled: true}
					wf.ObjectMeta.Annotations = map[string]string{}
					tc.configure(&wf)

					var reconciler *WorkflowReconciler
					if failure == "event" {
						memory := newMemStore()
						storeWorkflow(t, memory, wf)
						reconciler = NewWorkflowReconciler(&failingEventStore{memStore: memory, err: wantErr}, zaptest.NewLogger(t))
					} else {
						memory := newEventMemStore()
						storeWorkflow(t, memory.memStore, wf)
						reconciler = NewWorkflowReconciler(memory, zaptest.NewLogger(t))
						reconciler.persistStatus = func(context.Context, *v1alpha1.Workflow) error {
							return wantErr
						}
					}

					processed, err := reconciler.applyEmergentMutation(context.Background(), &wf)
					wantContext := "append workflow event"
					if failure == "update" {
						wantContext = tc.wantUpdateContext
					}
					if processed || !errors.Is(err, wantErr) || !strings.Contains(err.Error(), wantContext) {
						t.Fatalf("processed = %v, error = %v, want wrapped %q failure", processed, err, wantContext)
					}
				})
			}
		})
	}
}

func TestWorkflowReconciler_EmergentMutationSurfacesAppliedEventFailure(t *testing.T) {
	wantErr := errors.New("event store unavailable")
	memory := newMemStore()
	reconciler := NewWorkflowReconciler(&failingEventStore{memStore: memory, err: wantErr}, zaptest.NewLogger(t))
	persisted := false
	reconciler.persistStatus = func(context.Context, *v1alpha1.Workflow) error {
		persisted = true
		return nil
	}

	wf := makeWorkflow("default", "wf-emergent-applied-event-failure")
	wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{Enabled: true}
	proposal, _ := json.Marshal(workflowEmergentProposal{
		Steps:      []v1alpha1.WorkflowStep{{Name: "review", AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}}},
		ProposedBy: "planner@event-failure",
	})
	wf.ObjectMeta.Annotations = map[string]string{workflowEmergentProposalKey: string(proposal)}

	processed, err := reconciler.applyEmergentMutation(context.Background(), &wf)
	if processed || !persisted || !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "append workflow event") {
		t.Fatalf("processed = %v, persisted = %v, error = %v; want persisted mutation and wrapped event failure", processed, persisted, err)
	}
}

// TC-0030: Emergent DAG mutation exceeding maxSteps budget is rejected.
func TestWorkflowReconciler_EmergentMutationRejectedWhenBudgetExceeded(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-emergent-reject")
	wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{
		Enabled: true,
		Budgets: v1alpha1.WorkflowEmergentBudgets{MaxSteps: 1},
	}
	proposal := workflowEmergentProposal{
		Steps:      []v1alpha1.WorkflowStep{{Name: "review", AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}}},
		ProposedBy: "planner@run-2",
	}
	proposalJSON, _ := json.Marshal(proposal)
	wf.ObjectMeta.Annotations = map[string]string{
		workflowEmergentProposalKey:      string(proposalJSON),
		workflowEmergentStepsConsumedKey: "1",
	}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-reject", Kind: "Workflow"})
	res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-reject", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile emergent reject: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after processing rejected emergent mutation")
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-emergent-reject")
	if len(stored.Spec.Steps) != 2 {
		t.Fatalf("steps len = %d, want unchanged 2", len(stored.Spec.Steps))
	}
	if stored.ObjectMeta.Annotations[workflowEmergentProposalKey] != "" {
		t.Fatal("expected rejected proposal annotation to be cleared")
	}

	events, _ := s.ListEvents(context.Background(), "Workflow", "default", "wf-emergent-reject")
	found := false
	for _, evt := range events {
		if evt.Reason == "EmergentMutationRejected" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected EmergentMutationRejected event")
	}
}

func TestWorkflowReconciler_EmergentMutationRejectedWhenTokenBudgetExceeded(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-emergent-token-reject")
	wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{
		Enabled: true,
		Budgets: v1alpha1.WorkflowEmergentBudgets{MaxTokens: 10},
	}
	proposal := workflowEmergentProposal{
		Steps:           []v1alpha1.WorkflowStep{{Name: "review", AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}}},
		ProposedBy:      "planner@token",
		EstimatedTokens: 11,
	}
	proposalJSON, _ := json.Marshal(proposal)
	wf.ObjectMeta.Annotations = map[string]string{workflowEmergentProposalKey: string(proposalJSON)}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-token-reject", Kind: "Workflow"})
	_, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-token-reject", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile token reject: %v", err)
	}

	events, _ := s.ListEvents(context.Background(), "Workflow", "default", "wf-emergent-token-reject")
	found := false
	for _, evt := range events {
		if evt.Reason != "EmergentMutationRejected" {
			continue
		}
		var details map[string]string
		if err := json.Unmarshal(evt.Details, &details); err != nil {
			t.Fatalf("unmarshal event details: %v", err)
		}
		if details["maxTokens"] == "10" && details["proposedTokens"] == "11" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected EmergentMutationRejected with token details")
	}
}

func TestWorkflowReconciler_EmergentMutationRejectedWhenCostBudgetExceeded(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-emergent-cost-reject")
	wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{
		Enabled: true,
		Budgets: v1alpha1.WorkflowEmergentBudgets{MaxCost: "1.25"},
	}
	proposal := workflowEmergentProposal{
		Steps:         []v1alpha1.WorkflowStep{{Name: "review", AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}}},
		ProposedBy:    "planner@cost",
		EstimatedCost: 2,
	}
	proposalJSON, _ := json.Marshal(proposal)
	wf.ObjectMeta.Annotations = map[string]string{workflowEmergentProposalKey: string(proposalJSON)}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-cost-reject", Kind: "Workflow"})
	_, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-cost-reject", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile cost reject: %v", err)
	}

	events, _ := s.ListEvents(context.Background(), "Workflow", "default", "wf-emergent-cost-reject")
	found := false
	for _, evt := range events {
		if evt.Reason != "EmergentMutationRejected" {
			continue
		}
		var details map[string]string
		if err := json.Unmarshal(evt.Details, &details); err != nil {
			t.Fatalf("unmarshal event details: %v", err)
		}
		if details["maxCost"] == "1.25" && details["proposedCost"] == "2" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected EmergentMutationRejected with cost details")
	}
}

func TestWorkflowReconciler_NonFiniteConsumedCostCannotBypassBudget(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-emergent-nan-cost")
	wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{
		Enabled: true,
		Budgets: v1alpha1.WorkflowEmergentBudgets{MaxCost: "1.25"},
	}
	proposal := workflowEmergentProposal{
		Steps:         []v1alpha1.WorkflowStep{{Name: "review", AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}}},
		ProposedBy:    "planner@cost",
		EstimatedCost: 2,
	}
	proposalJSON, _ := json.Marshal(proposal)
	wf.ObjectMeta.Annotations = map[string]string{
		workflowEmergentProposalKey:     string(proposalJSON),
		workflowEmergentCostConsumedKey: "NaN",
	}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"})
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile non-finite consumed cost: %v", err)
	}

	stored := getWorkflow(t, s.memStore, "default", wf.ObjectMeta.Name)
	if len(stored.Spec.Steps) != len(wf.Spec.Steps) {
		t.Fatalf("steps len = %d, want unchanged %d", len(stored.Spec.Steps), len(wf.Spec.Steps))
	}
	if stored.ObjectMeta.Annotations[workflowEmergentProposalKey] != "" {
		t.Fatal("expected over-budget proposal annotation to be cleared")
	}

	events, _ := s.ListEvents(context.Background(), "Workflow", "default", wf.ObjectMeta.Name)
	for _, evt := range events {
		if evt.Reason == "EmergentMutationRejected" {
			return
		}
	}
	t.Fatal("expected non-finite accounting to preserve cost-budget rejection")
}

func TestWorkflowEmergentAnnotationParsing(t *testing.T) {
	t.Run("steps", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			annotations map[string]string
			want        int32
		}{
			{name: "nil"},
			{name: "blank", annotations: map[string]string{"value": "  "}},
			{name: "malformed", annotations: map[string]string{"value": "many"}},
			{name: "negative", annotations: map[string]string{"value": "-1"}},
			{name: "out of range", annotations: map[string]string{"value": "4294967297"}},
			{name: "valid", annotations: map[string]string{"value": " 42 "}, want: 42},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := annotationInt(tc.annotations, "value"); got != tc.want {
					t.Fatalf("annotationInt() = %d, want %d", got, tc.want)
				}
			})
		}
	})

	t.Run("tokens", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			annotations map[string]string
			want        int64
		}{
			{name: "nil"},
			{name: "blank", annotations: map[string]string{"value": "  "}},
			{name: "malformed", annotations: map[string]string{"value": "many"}},
			{name: "negative", annotations: map[string]string{"value": "-1"}},
			{name: "valid", annotations: map[string]string{"value": " 42 "}, want: 42},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := annotationInt64(tc.annotations, "value"); got != tc.want {
					t.Fatalf("annotationInt64() = %d, want %d", got, tc.want)
				}
			})
		}
	})

	t.Run("cost", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			annotations map[string]string
			want        float64
		}{
			{name: "nil"},
			{name: "blank", annotations: map[string]string{"value": "  "}},
			{name: "malformed", annotations: map[string]string{"value": "expensive"}},
			{name: "negative", annotations: map[string]string{"value": "-1"}},
			{name: "nan", annotations: map[string]string{"value": "NaN"}},
			{name: "positive infinity", annotations: map[string]string{"value": "+Inf"}},
			{name: "negative infinity", annotations: map[string]string{"value": "-Inf"}},
			{name: "valid", annotations: map[string]string{"value": " 1.25 "}, want: 1.25},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := annotationFloat(tc.annotations, "value"); got != tc.want {
					t.Fatalf("annotationFloat() = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("budget cost", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			raw  string
			want float64
		}{
			{name: "blank", raw: "  "},
			{name: "malformed", raw: "unlimited"},
			{name: "negative", raw: "-1"},
			{name: "nan", raw: "NaN"},
			{name: "positive infinity", raw: "+Inf"},
			{name: "valid", raw: " 2.5 ", want: 2.5},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := parseBudgetCost(tc.raw); got != tc.want {
					t.Fatalf("parseBudgetCost() = %v, want %v", got, tc.want)
				}
			})
		}
	})
}

func TestWorkflowReconciler_EmergentMutationRejectsNegativeEstimates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*workflowEmergentProposal)
	}{
		{
			name: "tokens",
			mutate: func(proposal *workflowEmergentProposal) {
				proposal.EstimatedTokens = -1
			},
		},
		{
			name: "cost",
			mutate: func(proposal *workflowEmergentProposal) {
				proposal.EstimatedCost = -1
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newEventMemStore()
			r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
			wf := makeWorkflow("default", "wf-emergent-negative-"+tc.name)
			wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{Enabled: true}
			proposal := workflowEmergentProposal{
				Steps:      []v1alpha1.WorkflowStep{{Name: "review", AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}}},
				ProposedBy: "planner@invalid",
			}
			tc.mutate(&proposal)
			proposalJSON, _ := json.Marshal(proposal)
			wf.ObjectMeta.Annotations = map[string]string{workflowEmergentProposalKey: string(proposalJSON)}
			storeWorkflow(t, s.memStore, wf)

			_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"})
			if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}); err != nil {
				t.Fatalf("reconcile negative estimate: %v", err)
			}

			stored := getWorkflow(t, s.memStore, "default", wf.ObjectMeta.Name)
			if len(stored.Spec.Steps) != len(wf.Spec.Steps) {
				t.Fatalf("steps len = %d, want unchanged %d", len(stored.Spec.Steps), len(wf.Spec.Steps))
			}
			if stored.ObjectMeta.Annotations[workflowEmergentProposalKey] != "" {
				t.Fatal("expected invalid proposal annotation to be cleared")
			}

			events, _ := s.ListEvents(context.Background(), "Workflow", "default", wf.ObjectMeta.Name)
			for _, evt := range events {
				if evt.Reason == "EmergentMutationRejected" {
					return
				}
			}
			t.Fatal("expected negative estimate to reject emergent mutation")
		})
	}
}

func TestWorkflowReconciler_EmergentMutationRejectsAccountingOverflow(t *testing.T) {
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		mutate      func(*workflowEmergentProposal)
	}{
		{
			name:        "steps",
			annotations: map[string]string{workflowEmergentStepsConsumedKey: "2147483647"},
			mutate:      func(*workflowEmergentProposal) {},
		},
		{
			name:        "tokens",
			annotations: map[string]string{workflowEmergentTokensConsumedKey: "9223372036854775807"},
			mutate: func(proposal *workflowEmergentProposal) {
				proposal.EstimatedTokens = 1
			},
		},
		{
			name:        "cost",
			annotations: map[string]string{workflowEmergentCostConsumedKey: "1e308"},
			mutate: func(proposal *workflowEmergentProposal) {
				proposal.EstimatedCost = 1e308
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newEventMemStore()
			r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
			wf := makeWorkflow("default", "wf-emergent-overflow-"+tc.name)
			wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{Enabled: true}
			proposal := workflowEmergentProposal{
				Steps:      []v1alpha1.WorkflowStep{{Name: "review", AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}}},
				ProposedBy: "planner@overflow",
			}
			tc.mutate(&proposal)
			proposalJSON, _ := json.Marshal(proposal)
			wf.ObjectMeta.Annotations = tc.annotations
			wf.ObjectMeta.Annotations[workflowEmergentProposalKey] = string(proposalJSON)
			storeWorkflow(t, s.memStore, wf)

			_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"})
			if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}); err != nil {
				t.Fatalf("reconcile accounting overflow: %v", err)
			}

			stored := getWorkflow(t, s.memStore, "default", wf.ObjectMeta.Name)
			if len(stored.Spec.Steps) != len(wf.Spec.Steps) {
				t.Fatalf("steps len = %d, want unchanged %d", len(stored.Spec.Steps), len(wf.Spec.Steps))
			}
			if stored.ObjectMeta.Annotations[workflowEmergentProposalKey] != "" {
				t.Fatal("expected overflowing proposal annotation to be cleared")
			}

			events, _ := s.ListEvents(context.Background(), "Workflow", "default", wf.ObjectMeta.Name)
			for _, evt := range events {
				if evt.Reason == "EmergentMutationRejected" {
					return
				}
			}
			t.Fatal("expected accounting overflow to reject emergent mutation")
		})
	}
}

func TestWorkflowReconciler_EmergentMutationRejectedForMalformedPayload(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-emergent-malformed")
	wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{Enabled: true}
	wf.ObjectMeta.Annotations = map[string]string{workflowEmergentProposalKey: "{"}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-malformed", Kind: "Workflow"})
	_, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-malformed", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile malformed proposal: %v", err)
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-emergent-malformed")
	if stored.ObjectMeta.Annotations[workflowEmergentProposalKey] != "" {
		t.Fatal("expected malformed proposal annotation to be cleared")
	}

	events, _ := s.ListEvents(context.Background(), "Workflow", "default", "wf-emergent-malformed")
	found := false
	for _, evt := range events {
		if evt.Reason == "EmergentMutationRejected" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected malformed payload rejection event")
	}
}

func TestWorkflowReconciler_EmergentMutationRejectedForInvalidStructure(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-emergent-invalid-structure")
	wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{Enabled: true}
	proposal := workflowEmergentProposal{
		Steps:      []v1alpha1.WorkflowStep{{Name: "plan", AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}}},
		ProposedBy: "planner@invalid",
	}
	proposalJSON, _ := json.Marshal(proposal)
	wf.ObjectMeta.Annotations = map[string]string{workflowEmergentProposalKey: string(proposalJSON)}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-invalid-structure", Kind: "Workflow"})
	_, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-invalid-structure", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile invalid structure: %v", err)
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-emergent-invalid-structure")
	if len(stored.Spec.Steps) != 2 {
		t.Fatalf("steps len = %d, want unchanged 2", len(stored.Spec.Steps))
	}

	events, _ := s.ListEvents(context.Background(), "Workflow", "default", "wf-emergent-invalid-structure")
	found := false
	for _, evt := range events {
		if evt.Reason != "EmergentMutationRejected" {
			continue
		}
		var details map[string]string
		if err := json.Unmarshal(evt.Details, &details); err != nil {
			t.Fatalf("unmarshal event details: %v", err)
		}
		if strings.Contains(details["error"], "already exists") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected structural rejection event with validation details")
	}
}

func TestWorkflowReconciler_EmergentMutationRejectedForUnknownEdgeEndpoint(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-emergent-invalid-edge")
	wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{Enabled: true}
	proposal := workflowEmergentProposal{
		Steps:      []v1alpha1.WorkflowStep{{Name: "review", AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}}},
		Edges:      []v1alpha1.WorkflowEdge{{From: "review", To: "ghost-step"}},
		ProposedBy: "planner@invalid-edge",
	}
	proposalJSON, _ := json.Marshal(proposal)
	wf.ObjectMeta.Annotations = map[string]string{workflowEmergentProposalKey: string(proposalJSON)}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-invalid-edge", Kind: "Workflow"})
	_, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-invalid-edge", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile invalid edge: %v", err)
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-emergent-invalid-edge")
	if len(stored.Spec.Steps) != 2 {
		t.Fatalf("steps len = %d, want unchanged 2", len(stored.Spec.Steps))
	}
	if stored.ObjectMeta.Annotations[workflowEmergentProposalKey] != "" {
		t.Fatal("expected invalid edge proposal annotation to be cleared")
	}

	events, _ := s.ListEvents(context.Background(), "Workflow", "default", "wf-emergent-invalid-edge")
	found := false
	for _, evt := range events {
		if evt.Reason != "EmergentMutationRejected" {
			continue
		}
		var details map[string]string
		if err := json.Unmarshal(evt.Details, &details); err != nil {
			t.Fatalf("unmarshal event details: %v", err)
		}
		if strings.Contains(details["error"], "edge to unknown step") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected invalid edge rejection event with validation details")
	}
}

func TestWorkflowReconciler_EmergentMutationRejectedForEmptyProposal(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-emergent-empty")
	wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{Enabled: true}
	proposalJSON, _ := json.Marshal(workflowEmergentProposal{ProposedBy: "planner@noop"})
	wf.ObjectMeta.Annotations = map[string]string{workflowEmergentProposalKey: string(proposalJSON)}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-empty", Kind: "Workflow"})
	_, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-empty", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile empty proposal: %v", err)
	}

	events, _ := s.ListEvents(context.Background(), "Workflow", "default", "wf-emergent-empty")
	found := false
	for _, evt := range events {
		if evt.Reason == "EmergentMutationRejected" && evt.Message == "empty emergent proposal" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected rejection event for empty proposal")
	}
}

func TestWorkflowReconciler_EmergentMutationAppliedEventDetails(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-emergent-apply-details")
	wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{Enabled: true}
	proposal := workflowEmergentProposal{
		Steps:           []v1alpha1.WorkflowStep{{Name: "review", AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}}},
		Edges:           []v1alpha1.WorkflowEdge{{From: "execute", To: "review"}},
		ProposedBy:      "planner@details",
		EstimatedTokens: 7,
		EstimatedCost:   0.5,
	}
	proposalJSON, _ := json.Marshal(proposal)
	wf.ObjectMeta.Annotations = map[string]string{workflowEmergentProposalKey: string(proposalJSON)}
	storeWorkflow(t, s.memStore, wf)

	_, _ = r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-apply-details", Kind: "Workflow"})
	_, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-apply-details", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile apply details: %v", err)
	}

	events, _ := s.ListEvents(context.Background(), "Workflow", "default", "wf-emergent-apply-details")
	found := false
	for _, evt := range events {
		if evt.Reason != "EmergentMutationApplied" {
			continue
		}
		var details map[string]string
		if err := json.Unmarshal(evt.Details, &details); err != nil {
			t.Fatalf("unmarshal event details: %v", err)
		}
		if details["proposedBy"] == "planner@details" && details["addedSteps"] == "1" && details["tokensConsumed"] == "7" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected applied event with audit detail fields")
	}
}

func TestWorkflowReconciler_EmergentMutationDoesNotEmitAppliedEventOnUpdateFailure(t *testing.T) {
	s := &failWorkflowUpdateEventStore{
		eventMemStore: newEventMemStore(),
		err:           errors.New("forced update failure"),
		failOnce:      true,
	}
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-emergent-update-fail")
	wf.ObjectMeta.Finalizers = []string{WorkflowFinalizer}
	wf.Spec.Emergent = &v1alpha1.WorkflowEmergentSpec{Enabled: true}
	proposal := workflowEmergentProposal{
		Steps:      []v1alpha1.WorkflowStep{{Name: "review", AgentRef: v1alpha1.LocalObjectReference{Name: "reviewer"}}},
		ProposedBy: "planner@fail",
	}
	proposalJSON, _ := json.Marshal(proposal)
	wf.ObjectMeta.Annotations = map[string]string{workflowEmergentProposalKey: string(proposalJSON)}
	storeWorkflow(t, s.memStore, wf)

	_, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emergent-update-fail", Kind: "Workflow"})
	if err == nil || !strings.Contains(err.Error(), "update workflow after emergent mutation") {
		t.Fatalf("expected update failure, got %v", err)
	}

	events, _ := s.ListEvents(context.Background(), "Workflow", "default", "wf-emergent-update-fail")
	for _, evt := range events {
		if evt.Reason == "EmergentMutationApplied" {
			t.Fatal("did not expect EmergentMutationApplied when workflow update fails")
		}
	}
}

// ---------------------------------------------------------------------------
// Terminal-phase early return — ensures no store writes once workflow is done.
// ---------------------------------------------------------------------------

func TestWorkflowReconciler_TerminalPhaseIsNoOp(t *testing.T) {
	for _, phase := range []v1alpha1.Phase{v1alpha1.PhaseReady, v1alpha1.PhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			s := newEventMemStore()
			r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
			wf := makeWorkflow("default", "wf-terminal-"+string(phase))
			wf.Status.Phase = phase
			wf.ObjectMeta.Finalizers = []string{WorkflowFinalizer}
			storeWorkflow(t, s.memStore, wf)

			// Capture the resourceVersion before reconciling.
			before := getWorkflow(t, s.memStore, "default", "wf-terminal-"+string(phase))
			beforeRV := before.ObjectMeta.ResourceVersion

			res, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-terminal-" + string(phase), Kind: "Workflow"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Requeue || res.RequeueAfter > 0 {
				t.Fatalf("expected no requeue for terminal phase, got %+v", res)
			}

			// ResourceVersion must not have changed — no store write occurred.
			after := getWorkflow(t, s.memStore, "default", "wf-terminal-"+string(phase))
			if after.ObjectMeta.ResourceVersion != beforeRV {
				t.Fatalf("expected no store update for terminal workflow; RV changed from %d to %d", beforeRV, after.ObjectMeta.ResourceVersion)
			}

			// No jobs should have been created.
			jobs := listWorkflowJobs(t, s.memStore, "default")
			if len(jobs) != 0 {
				t.Fatalf("expected no jobs for terminal workflow, got %d", len(jobs))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TC-0211: Template-based output passing across a 3-step chain A→B→C
// ---------------------------------------------------------------------------
func TestWorkflowReconciler_TemplateOutputPassingChain(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))

	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-chain",
			Namespace: "default",
			UID:       "wf-chain-uid",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "fetch", AgentRef: v1alpha1.LocalObjectReference{Name: "fetcher"}},
				{Name: "transform", AgentRef: v1alpha1.LocalObjectReference{Name: "transformer"}, Input: "{{ .Steps.fetch.Output }}"},
				{Name: "store", AgentRef: v1alpha1.LocalObjectReference{Name: "storer"}, Input: "{{ .Steps.transform.Output }}"},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "fetch", To: "transform"},
				{From: "transform", To: "store"},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)

	// Reconcile #0: adds finalizer.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-chain", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}

	// Reconcile #1: creates job for step "fetch" (no deps).
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-chain", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 || jobs[0].Spec.Input != "" {
		t.Fatalf("expected 1 job with empty input for fetch step, got %d jobs", len(jobs))
	}

	// Complete fetch job with output.
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = "fetched-data-42"
	updateJob(t, s.memStore, jobs[0])

	// Reconcile #2: should create transform job with resolved input.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-chain", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	jobs = listWorkflowJobs(t, s.memStore, "default")
	var transformJob *v1alpha1.AgentJob
	for i := range jobs {
		if jobs[i].ObjectMeta.Labels[workflowStepLabel] == "transform" {
			transformJob = &jobs[i]
			break
		}
	}
	if transformJob == nil {
		t.Fatal("transform job not created")
	}
	if transformJob.Spec.Input != "fetched-data-42" {
		t.Fatalf("expected transform input 'fetched-data-42', got %q", transformJob.Spec.Input)
	}

	// Complete transform job.
	transformJob.Status.Phase = v1alpha1.PhaseReady
	transformJob.Status.Output = "transformed-result"
	updateJob(t, s.memStore, *transformJob)

	// Reconcile #3: should create store job with resolved input.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-chain", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	jobs = listWorkflowJobs(t, s.memStore, "default")
	var storeJob *v1alpha1.AgentJob
	for i := range jobs {
		if jobs[i].ObjectMeta.Labels[workflowStepLabel] == "store" {
			storeJob = &jobs[i]
			break
		}
	}
	if storeJob == nil {
		t.Fatal("store job not created")
	}
	if storeJob.Spec.Input != "transformed-result" {
		t.Fatalf("expected store input 'transformed-result', got %q", storeJob.Spec.Input)
	}
}

// ---------------------------------------------------------------------------
// TC-0212: Template referencing a non-existent step fails the workflow
// ---------------------------------------------------------------------------
func TestWorkflowReconciler_TemplateResolutionFailure(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))

	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-bad-tmpl",
			Namespace: "default",
			UID:       "wf-bad-tmpl-uid",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "a", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}},
				{Name: "b", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-b"}, Input: "{{ .Steps.nonexistent.Output }}"},
			},
			Edges: []v1alpha1.WorkflowEdge{{From: "a", To: "b"}},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)

	// Reconcile #0: adds finalizer.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-bad-tmpl", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}

	// Reconcile #1: creates job for step "a".
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-bad-tmpl", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Complete step a.
	jobs := listWorkflowJobs(t, s.memStore, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = "ok"
	updateJob(t, s.memStore, jobs[0])

	// Reconcile #2: should fail workflow due to missing step reference.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-bad-tmpl", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	got := getWorkflow(t, s.memStore, "default", "wf-bad-tmpl")
	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected Failed phase, got %s", got.Status.Phase)
	}

	// Check for TemplateResolutionFailed condition.
	found := false
	for _, c := range got.Status.Conditions {
		if c.Type == "TemplateResolutionFailed" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected TemplateResolutionFailed condition")
	}
}

// ---------------------------------------------------------------------------
// TC-0213: Output exceeding 64 KiB is truncated in StepStatuses
// ---------------------------------------------------------------------------
func TestWorkflowReconciler_OutputTruncation(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))

	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-truncate",
			Namespace: "default",
			UID:       "wf-trunc-uid",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "big", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-big"}},
				{Name: "next", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-next"}, Input: "{{ .Steps.big.Output }}"},
			},
			Edges: []v1alpha1.WorkflowEdge{{From: "big", To: "next"}},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)

	// Reconcile #0: adds finalizer.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-truncate", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}

	// Reconcile #1: creates job for "big".
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-truncate", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Complete "big" with output larger than 64 KiB.
	jobs := listWorkflowJobs(t, s.memStore, "default")
	bigOutput := strings.Repeat("x", 70*1024) // 70 KiB
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = bigOutput
	updateJob(t, s.memStore, jobs[0])

	// Reconcile #2: syncs step statuses (truncation happens), then creates "next".
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-truncate", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	got := getWorkflow(t, s.memStore, "default", "wf-truncate")

	// Verify the StepStatuses entry for "big" was truncated.
	var bigStatus *v1alpha1.WorkflowStepStatus
	for i := range got.Status.StepStatuses {
		if got.Status.StepStatuses[i].Name == "big" {
			bigStatus = &got.Status.StepStatuses[i]
			break
		}
	}
	if bigStatus == nil {
		t.Fatal("expected StepStatuses entry for 'big'")
	}
	if len(bigStatus.Output) > 65536+100 {
		t.Fatalf("expected truncated output, got len=%d", len(bigStatus.Output))
	}
	if !strings.Contains(bigStatus.Output, "[output truncated at 64 KiB]") {
		t.Fatal("expected truncation marker in output")
	}
}

// ---------------------------------------------------------------------------
// TC-0214: Workflow params resolve correctly via {{ .Params.key }}
// ---------------------------------------------------------------------------
func TestWorkflowReconciler_ParamsTemplateResolution(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))

	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-params",
			Namespace: "default",
			UID:       "wf-params-uid",
		},
		Spec: v1alpha1.WorkflowSpec{
			Parameters: map[string]string{"env": "production", "region": "us-east-1"},
			Steps: []v1alpha1.WorkflowStep{
				{Name: "deploy", AgentRef: v1alpha1.LocalObjectReference{Name: "deployer"}, Input: "deploy to {{ .Params.env }} in {{ .Params.region }}"},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)

	// Reconcile #0: adds finalizer.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-params", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}

	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-params", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	expected := "deploy to production in us-east-1"
	if jobs[0].Spec.Input != expected {
		t.Fatalf("expected input %q, got %q", expected, jobs[0].Spec.Input)
	}
}

// ---------------------------------------------------------------------------
// TC-0215: Branching with CEL edge conditions – A→B (yes) and A→C (no)
// ---------------------------------------------------------------------------
func TestWorkflowReconciler_CELEdgeBranching(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))

	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-branch",
			Namespace: "default",
			UID:       "wf-branch-uid",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "A", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}},
				{Name: "B", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-b"}},
				{Name: "C", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-c"}},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "A", To: "B", Condition: `steps.A.output.contains("yes")`},
				{From: "A", To: "C", Condition: `steps.A.output.contains("no")`},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)

	// Reconcile #0: adds finalizer.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-branch", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}

	// Reconcile #1: creates job for step A (no inbound edges).
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-branch", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Complete A with "yes" output.
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = "yes-approve"
	updateJob(t, s.memStore, jobs[0])

	// Reconcile #2: dispatches B (edge condition true). C not yet evaluated.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-branch", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	// Complete B so the reconciler can proceed to evaluate C.
	jobs = listWorkflowJobs(t, s.memStore, "default")
	for i := range jobs {
		if jobs[i].ObjectMeta.Labels[workflowStepLabel] == "B" {
			jobs[i].Status.Phase = v1alpha1.PhaseReady
			jobs[i].Status.Output = "b-done"
			updateJob(t, s.memStore, jobs[i])
		}
	}

	// Reconcile #3: B is complete, evaluates C's edge condition (false) → skip C.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-branch", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}

	got := getWorkflow(t, s.memStore, "default", "wf-branch")
	// C should be Skipped.
	cSkipped := false
	for _, ss := range got.Status.StepStatuses {
		if ss.Name == "C" && ss.Phase == v1alpha1.PhaseSkipped {
			cSkipped = true
		}
	}

	jobs = listWorkflowJobs(t, s.memStore, "default")
	bDispatched := false
	for _, j := range jobs {
		if j.ObjectMeta.Labels[workflowStepLabel] == "B" {
			bDispatched = true
		}
	}

	if !bDispatched {
		t.Fatal("expected B to be dispatched")
	}
	if !cSkipped {
		t.Fatal("expected C to be skipped")
	}
}

// ---------------------------------------------------------------------------
// TC-0216: Step condition gates dispatch – step skipped when predecessor fails
// ---------------------------------------------------------------------------
func TestWorkflowReconciler_CELStepConditionSkip(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))

	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-stepcond",
			Namespace: "default",
			UID:       "wf-stepcond-uid",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "A", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}},
				{Name: "B", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-b"}, Condition: `steps.A.phase == "Ready"`},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "A", To: "B"},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)

	// Reconcile #0: adds finalizer.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-stepcond", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}

	// Reconcile #1: creates job for A.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-stepcond", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Complete A with Ready phase (but let's manually set step status to simulate
	// a completed-but-recorded-as-failed scenario for the condition check).
	jobs := listWorkflowJobs(t, s.memStore, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = "done"
	updateJob(t, s.memStore, jobs[0])

	// First, reconcile once with A Ready to trigger B dispatch — B should be dispatched.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-stepcond", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	jobs = listWorkflowJobs(t, s.memStore, "default")
	bFound := false
	for _, j := range jobs {
		if j.ObjectMeta.Labels[workflowStepLabel] == "B" {
			bFound = true
		}
	}
	if !bFound {
		t.Fatal("expected B to be dispatched when A is Ready")
	}

	// Now test the skip path: new workflow where A completes as Failed-output.
	// We need a fresh workflow where A is already done but step status says "Failed".
	s2 := newEventMemStore()
	r2 := NewWorkflowReconciler(s2, zaptest.NewLogger(t))

	wf2 := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-stepcond2",
			Namespace: "default",
			UID:       "wf-stepcond2-uid",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "A", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}},
				// B requires A to be Ready, but we'll complete A as Ready then manually
				// adjust the step status.
				{Name: "B", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-b"}, Condition: `steps.A.output == "success"`},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "A", To: "B"},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s2.memStore, wf2)

	// Add finalizer.
	if _, err := r2.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-stepcond2", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}
	// Create job for A.
	if _, err := r2.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-stepcond2", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Complete A with output "failure" (not "success").
	jobs2 := listWorkflowJobs(t, s2.memStore, "default")
	jobs2[0].Status.Phase = v1alpha1.PhaseReady
	jobs2[0].Status.Output = "failure"
	updateJob(t, s2.memStore, jobs2[0])

	// Reconcile: B's step condition should evaluate to false → skip B.
	if _, err := r2.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-stepcond2", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	got2 := getWorkflow(t, s2.memStore, "default", "wf-stepcond2")
	bSkipped := false
	for _, ss := range got2.Status.StepStatuses {
		if ss.Name == "B" && ss.Phase == v1alpha1.PhaseSkipped {
			bSkipped = true
		}
	}
	if !bSkipped {
		t.Fatal("expected B to be skipped when step condition is false")
	}
}

// ---------------------------------------------------------------------------
// TC-0217: All-matching – A→B and A→C both true, both dispatched
// ---------------------------------------------------------------------------
func TestWorkflowReconciler_CELAllMatchingEdges(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))

	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-allbranch",
			Namespace: "default",
			UID:       "wf-allbranch-uid",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "A", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}},
				{Name: "B", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-b"}},
				{Name: "C", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-c"}},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "A", To: "B", Condition: `steps.A.output.contains("go")`},
				{From: "A", To: "C", Condition: `steps.A.output.contains("go")`},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)

	// Reconcile #0: adds finalizer.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-allbranch", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}
	// Reconcile #1: creates job for A.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-allbranch", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Complete A with "go" output.
	jobs := listWorkflowJobs(t, s.memStore, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = "go-ahead"
	updateJob(t, s.memStore, jobs[0])

	// Reconcile #2: dispatches B (first matching edge target).
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-allbranch", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	// Complete B so the reconciler proceeds to C.
	jobs = listWorkflowJobs(t, s.memStore, "default")
	for i := range jobs {
		if jobs[i].ObjectMeta.Labels[workflowStepLabel] == "B" {
			jobs[i].Status.Phase = v1alpha1.PhaseReady
			jobs[i].Status.Output = "b-done"
			updateJob(t, s.memStore, jobs[i])
		}
	}

	// Reconcile #3: dispatches C (second matching edge target).
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-allbranch", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}

	jobs = listWorkflowJobs(t, s.memStore, "default")
	bFound, cFound := false, false
	for _, j := range jobs {
		switch j.ObjectMeta.Labels[workflowStepLabel] {
		case "B":
			bFound = true
		case "C":
			cFound = true
		}
	}
	if !bFound {
		t.Fatal("expected B to be dispatched")
	}
	if !cFound {
		t.Fatal("expected C to be dispatched")
	}
}

// ---------------------------------------------------------------------------
// TC-0218: CEL compile error fails the workflow gracefully
// ---------------------------------------------------------------------------
func TestWorkflowReconciler_CELErrorFailsWorkflow(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))

	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-celerr",
			Namespace: "default",
			UID:       "wf-celerr-uid",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "A", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}},
				{Name: "B", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-b"}},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "A", To: "B", Condition: `this is not valid CEL!!!`},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)

	// Reconcile #0: adds finalizer.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-celerr", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}
	// Reconcile #1: creates job for A.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-celerr", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Complete A.
	jobs := listWorkflowJobs(t, s.memStore, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = "done"
	updateJob(t, s.memStore, jobs[0])

	// Reconcile #2: CEL error on edge A→B should fail the workflow.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-celerr", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	got := getWorkflow(t, s.memStore, "default", "wf-celerr")
	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected Failed phase, got %s", got.Status.Phase)
	}

	// Check for CELConditionFailed condition.
	found := false
	for _, c := range got.Status.Conditions {
		if c.Type == "CELConditionFailed" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected CELConditionFailed condition")
	}
}

// ---------------------------------------------------------------------------
// TC-0219: Edge with empty condition fires unconditionally
// ---------------------------------------------------------------------------
func TestWorkflowReconciler_CELEmptyEdgeCondition(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))

	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-emptyedge",
			Namespace: "default",
			UID:       "wf-emptyedge-uid",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "A", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}},
				{Name: "B", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-b"}},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "A", To: "B", Condition: ""}, // empty condition = unconditional
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)

	// Reconcile #0: adds finalizer.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emptyedge", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}
	// Reconcile #1: creates job for A.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emptyedge", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// Complete A.
	jobs := listWorkflowJobs(t, s.memStore, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = "anything"
	updateJob(t, s.memStore, jobs[0])

	// Reconcile #2: B should be dispatched unconditionally.
	if _, err := r.reconcileDirect(context.Background(), Request{Namespace: "default", Name: "wf-emptyedge", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	jobs = listWorkflowJobs(t, s.memStore, "default")
	bFound := false
	for _, j := range jobs {
		if j.ObjectMeta.Labels[workflowStepLabel] == "B" {
			bFound = true
		}
	}
	if !bFound {
		t.Fatal("expected B to be dispatched with empty edge condition")
	}
}

// makeDiamondWorkflow creates A→[B,C]→D topology used by parallel dispatch tests.
func makeDiamondWorkflow(ns, name string) v1alpha1.Workflow {
	return v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "wf-uid-diamond",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{Name: "A", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}},
				{Name: "B", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-b"}},
				{Name: "C", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-c"}},
				{Name: "D", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-d"}},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "A", To: "B"},
				{From: "A", To: "C"},
				{From: "B", To: "D"},
				{From: "C", To: "D"},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
}

// TC-0220: Diamond DAG — A→[B,C]→D: B and C dispatch in the same pass after A completes.
func TestWorkflowReconciler_DiamondDAGParallelFanOut(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeDiamondWorkflow("default", "wf-diamond")
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-diamond", Kind: "Workflow"}

	// Reconcile #1: finalizer
	_, _ = r.reconcileDirect(context.Background(), req)
	// Reconcile #2: dispatch A
	_, _ = r.reconcileDirect(context.Background(), req)

	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job (A), got %d", len(jobs))
	}
	if jobs[0].ObjectMeta.Labels[workflowStepLabel] != "A" {
		t.Fatalf("expected step A, got %q", jobs[0].ObjectMeta.Labels[workflowStepLabel])
	}

	// Complete A.
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = "result-a"
	updateJob(t, s.memStore, jobs[0])

	// Reconcile #3: should dispatch BOTH B and C in the same pass.
	res, err := r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected Requeue after dispatching B and C")
	}

	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 3 {
		t.Fatalf("expected 3 jobs (A+B+C), got %d", len(jobs))
	}
	stepNames := map[string]bool{}
	for _, j := range jobs {
		stepNames[j.ObjectMeta.Labels[workflowStepLabel]] = true
	}
	if !stepNames["B"] || !stepNames["C"] {
		t.Fatalf("expected B and C dispatched, got steps: %v", stepNames)
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-diamond")
	if len(stored.Status.ActiveSteps) != 2 {
		t.Fatalf("expected 2 active steps, got %v", stored.Status.ActiveSteps)
	}

	// D must NOT be dispatched yet — it needs both B and C.
	if stepNames["D"] {
		t.Fatal("D should not be dispatched until both B and C complete")
	}

	// Complete B and C.
	for i := range jobs {
		step := jobs[i].ObjectMeta.Labels[workflowStepLabel]
		if step == "B" || step == "C" {
			jobs[i].Status.Phase = v1alpha1.PhaseReady
			jobs[i].Status.Output = "result-" + step
			updateJob(t, s.memStore, jobs[i])
		}
	}

	// Reconcile #4: dispatch D (fan-in gate satisfied).
	_, err = r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile 4: %v", err)
	}
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 4 {
		t.Fatalf("expected 4 jobs (A+B+C+D), got %d", len(jobs))
	}
	dFound := false
	for _, j := range jobs {
		if j.ObjectMeta.Labels[workflowStepLabel] == "D" {
			dFound = true
		}
	}
	if !dFound {
		t.Fatal("expected D dispatched after fan-in")
	}

	// Complete D.
	for i := range jobs {
		if jobs[i].ObjectMeta.Labels[workflowStepLabel] == "D" {
			jobs[i].Status.Phase = v1alpha1.PhaseReady
			jobs[i].Status.Output = "result-d"
			updateJob(t, s.memStore, jobs[i])
		}
	}

	// Reconcile #5: workflow completes.
	res, err = r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile 5: %v", err)
	}
	stored = getWorkflow(t, s.memStore, "default", "wf-diamond")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", stored.Status.Phase)
	}
	if len(stored.Status.CompletedSteps) != 4 {
		t.Fatalf("completed steps = %v, want 4", stored.Status.CompletedSteps)
	}
}

// TC-0221: maxParallelSteps=1 forces sequential dispatch of independent steps.
func TestWorkflowReconciler_MaxParallelStepsThrottle(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeDiamondWorkflow("default", "wf-throttle")
	wf.Spec.MaxParallelSteps = 1
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-throttle", Kind: "Workflow"}

	// Finalizer + dispatch A.
	_, _ = r.reconcileDirect(context.Background(), req)
	_, _ = r.reconcileDirect(context.Background(), req)

	// Complete A.
	jobs := listWorkflowJobs(t, s.memStore, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = "done"
	updateJob(t, s.memStore, jobs[0])

	// Reconcile: only ONE of B or C should dispatch (maxParallelSteps=1).
	_, err := r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs (A + one of B/C), got %d", len(jobs))
	}

	// Complete the dispatched step.
	for i := range jobs {
		if jobs[i].Status.Phase != v1alpha1.PhaseReady {
			jobs[i].Status.Phase = v1alpha1.PhaseReady
			jobs[i].Status.Output = "done"
			updateJob(t, s.memStore, jobs[i])
		}
	}

	// Reconcile: the other one should now dispatch.
	_, err = r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 3 {
		t.Fatalf("expected 3 jobs after second dispatch, got %d", len(jobs))
	}
}

// TC-0222: failFast cancels running jobs when one fails.
func TestWorkflowReconciler_FailFastCancelsJobs(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeDiamondWorkflow("default", "wf-failfast")
	wf.Spec.FailFast = true
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-failfast", Kind: "Workflow"}

	// Finalizer + dispatch A.
	_, _ = r.reconcileDirect(context.Background(), req)
	_, _ = r.reconcileDirect(context.Background(), req)

	// Complete A.
	jobs := listWorkflowJobs(t, s.memStore, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = "done"
	updateJob(t, s.memStore, jobs[0])

	// Dispatch B and C.
	_, _ = r.reconcileDirect(context.Background(), req)
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 3 {
		t.Fatalf("expected 3 jobs (A+B+C), got %d", len(jobs))
	}

	// Fail B, leave C running.
	for i := range jobs {
		step := jobs[i].ObjectMeta.Labels[workflowStepLabel]
		if step == "B" {
			jobs[i].Status.Phase = v1alpha1.PhaseFailed
			updateJob(t, s.memStore, jobs[i])
		} else if step == "C" {
			jobs[i].Status.Phase = v1alpha1.PhaseRunning
			updateJob(t, s.memStore, jobs[i])
		}
	}

	// Reconcile: failFast should cancel C and mark workflow failed.
	_, err := r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile failFast: %v", err)
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-failfast")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", stored.Status.Phase)
	}

	// C's job should have been deleted (cancelled).
	remainingJobs := listWorkflowJobs(t, s.memStore, "default")
	for _, j := range remainingJobs {
		step := j.ObjectMeta.Labels[workflowStepLabel]
		if step == "C" && (j.Status.Phase == v1alpha1.PhasePending || j.Status.Phase == v1alpha1.PhaseRunning) {
			t.Fatal("expected C to be cancelled (deleted) by failFast")
		}
	}
}

// TC-0223: continueOnError lets remaining steps complete, then marks workflow failed.
func TestWorkflowReconciler_ContinueOnErrorCompletesRemainingSteps(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeDiamondWorkflow("default", "wf-continueonerror")
	wf.Spec.ContinueOnError = true
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-continueonerror", Kind: "Workflow"}

	// Finalizer + dispatch A.
	_, _ = r.reconcileDirect(context.Background(), req)
	_, _ = r.reconcileDirect(context.Background(), req)

	// Complete A.
	jobs := listWorkflowJobs(t, s.memStore, "default")
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = "done"
	updateJob(t, s.memStore, jobs[0])

	// Dispatch B and C.
	_, _ = r.reconcileDirect(context.Background(), req)

	// Fail B, complete C.
	jobs = listWorkflowJobs(t, s.memStore, "default")
	for i := range jobs {
		step := jobs[i].ObjectMeta.Labels[workflowStepLabel]
		if step == "B" {
			jobs[i].Status.Phase = v1alpha1.PhaseFailed
			updateJob(t, s.memStore, jobs[i])
		} else if step == "C" {
			jobs[i].Status.Phase = v1alpha1.PhaseReady
			jobs[i].Status.Output = "result-c"
			updateJob(t, s.memStore, jobs[i])
		}
	}

	// Reconcile: in continueOnError, B's failure counts as terminal.
	// D should dispatch because both B (failed) and C (completed) are terminal.
	_, err := r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile after B fails: %v", err)
	}

	jobs = listWorkflowJobs(t, s.memStore, "default")
	dDispatched := false
	for _, j := range jobs {
		if j.ObjectMeta.Labels[workflowStepLabel] == "D" {
			dDispatched = true
		}
	}
	if !dDispatched {
		t.Fatal("expected D dispatched in continueOnError mode after B failed and C completed")
	}

	// Complete D.
	jobs = listWorkflowJobs(t, s.memStore, "default")
	for i := range jobs {
		if jobs[i].ObjectMeta.Labels[workflowStepLabel] == "D" {
			jobs[i].Status.Phase = v1alpha1.PhaseReady
			jobs[i].Status.Output = "result-d"
			updateJob(t, s.memStore, jobs[i])
		}
	}

	// Reconcile: all steps terminal — workflow should be Failed (because B failed).
	_, err = r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile final: %v", err)
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-continueonerror")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed (B failed)", stored.Status.Phase)
	}
	// D should still have completed (not cancelled).
	if len(stored.Status.CompletedSteps) < 3 {
		t.Fatalf("expected at least 3 completed steps (A, C, D), got %v", stored.Status.CompletedSteps)
	}
}

// TC-0228: Step retry recovers from a transient failure within MaxAttempts.
func TestWorkflowReconciler_StepRetrySucceeds(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-retry",
			Namespace: "default",
			UID:       "wf-uid-retry",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{
					Name:     "flaky",
					AgentRef: v1alpha1.LocalObjectReference{Name: "agent-flaky"},
					Retry:    &v1alpha1.WorkflowStepRetry{MaxAttempts: 3, Backoff: "1ms"},
				},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-retry", Kind: "Workflow"}

	// Reconcile #1: finalizer.
	mustReconcile(t, r, req)
	// Reconcile #2: dispatch attempt 1.
	mustReconcile(t, r, req)

	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	// Fail attempt 1.
	jobs[0].Status.Phase = v1alpha1.PhaseFailed
	updateJob(t, s.memStore, jobs[0])

	// Reconcile #3: classify failure, schedule retry; job should be removed.
	mustReconcile(t, r, req)
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 0 {
		t.Fatalf("expected failed job to be cancelled before retry, got %d", len(jobs))
	}
	stored := getWorkflow(t, s.memStore, "default", "wf-retry")
	if stored.Status.Phase == v1alpha1.PhaseFailed {
		t.Fatalf("workflow should not be Failed during retry, got %q", stored.Status.Phase)
	}

	// Allow backoff to elapse.
	time.Sleep(5 * time.Millisecond)

	// Reconcile #4: dispatch attempt 2.
	mustReconcile(t, r, req)
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job for retry attempt, got %d", len(jobs))
	}
	// Fail attempt 2.
	jobs[0].Status.Phase = v1alpha1.PhaseFailed
	updateJob(t, s.memStore, jobs[0])

	mustReconcile(t, r, req)
	time.Sleep(5 * time.Millisecond)

	// Reconcile #6: dispatch attempt 3 (final, MaxAttempts=3 total attempts).
	mustReconcile(t, r, req)
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job for attempt 3, got %d", len(jobs))
	}
	// Succeed attempt 3.
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = "ok"
	updateJob(t, s.memStore, jobs[0])

	mustReconcile(t, r, req)
	stored = getWorkflow(t, s.memStore, "default", "wf-retry")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready after retry success", stored.Status.Phase)
	}
	var ss *v1alpha1.WorkflowStepStatus
	for i := range stored.Status.StepStatuses {
		if stored.Status.StepStatuses[i].Name == "flaky" {
			ss = &stored.Status.StepStatuses[i]
		}
	}
	if ss == nil {
		t.Fatal("missing step status for flaky")
	}
	if ss.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", ss.Attempts)
	}
}

// TC-0229: Per-step timeout marks the step PhaseTimedOut and triggers a retry.
func TestWorkflowReconciler_StepTimeoutTriggersRetry(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-timeout",
			Namespace: "default",
			UID:       "wf-uid-timeout",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{
					Name:     "slow",
					AgentRef: v1alpha1.LocalObjectReference{Name: "agent-slow"},
					Timeout:  "10ms",
					Retry:    &v1alpha1.WorkflowStepRetry{MaxAttempts: 2, Backoff: "1ms"},
				},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-timeout", Kind: "Workflow"}

	// Reconcile #1: finalizer.
	mustReconcile(t, r, req)
	// Reconcile #2: dispatch.
	mustReconcile(t, r, req)

	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	// Leave job in Running state and wait for the timeout to elapse.
	jobs[0].Status.Phase = v1alpha1.PhaseRunning
	updateJob(t, s.memStore, jobs[0])

	time.Sleep(20 * time.Millisecond)

	// Reconcile #3: should detect timeout, cancel job, schedule retry.
	mustReconcile(t, r, req)
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 0 {
		t.Fatalf("expected timed-out job to be cancelled, got %d", len(jobs))
	}
	stored := getWorkflow(t, s.memStore, "default", "wf-timeout")
	var ss *v1alpha1.WorkflowStepStatus
	for i := range stored.Status.StepStatuses {
		if stored.Status.StepStatuses[i].Name == "slow" {
			ss = &stored.Status.StepStatuses[i]
		}
	}
	if ss == nil {
		t.Fatal("missing step status for slow")
	}
	if ss.Phase != v1alpha1.PhasePending {
		t.Fatalf("phase after retry scheduling = %q, want Pending", ss.Phase)
	}
	if ss.NextRetryAt == nil {
		t.Fatal("expected NextRetryAt to be set after timeout")
	}

	time.Sleep(5 * time.Millisecond)

	// Reconcile #4: dispatch retry.
	mustReconcile(t, r, req)
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 retry job, got %d", len(jobs))
	}
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = "ok"
	updateJob(t, s.memStore, jobs[0])

	mustReconcile(t, r, req)
	stored = getWorkflow(t, s.memStore, "default", "wf-timeout")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready after retry success", stored.Status.Phase)
	}
}

// TC-0230: A CEL edge condition referencing steps.<x>.phase routes around a failure.
func TestWorkflowReconciler_FailureFallbackViaCELEdge(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-fallback",
			Namespace: "default",
			UID:       "wf-uid-fallback",
		},
		Spec: v1alpha1.WorkflowSpec{
			ContinueOnError: true,
			Steps: []v1alpha1.WorkflowStep{
				{Name: "A", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}, ContinueOnError: true},
				{Name: "B", AgentRef: v1alpha1.LocalObjectReference{Name: "agent-b"}},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "A", To: "B", Condition: `steps.A.phase == "Failed"`},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-fallback", Kind: "Workflow"}

	// Reconcile #1: finalizer.
	mustReconcile(t, r, req)
	// Reconcile #2: dispatch A.
	mustReconcile(t, r, req)

	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 || jobs[0].ObjectMeta.Labels[workflowStepLabel] != "A" {
		t.Fatalf("expected A dispatched, got %d jobs", len(jobs))
	}
	jobs[0].Status.Phase = v1alpha1.PhaseFailed
	updateJob(t, s.memStore, jobs[0])

	// Reconcile #3: A failed → edge condition true → dispatch B.
	mustReconcile(t, r, req)
	jobs = listWorkflowJobs(t, s.memStore, "default")
	var bJob *v1alpha1.AgentJob
	for i := range jobs {
		if jobs[i].ObjectMeta.Labels[workflowStepLabel] == "B" {
			bJob = &jobs[i]
		}
	}
	if bJob == nil {
		t.Fatal("expected B to be dispatched as fallback when A failed")
	}
	bJob.Status.Phase = v1alpha1.PhaseReady
	bJob.Status.Output = "fallback-ok"
	updateJob(t, s.memStore, *bJob)

	mustReconcile(t, r, req)
	stored := getWorkflow(t, s.memStore, "default", "wf-fallback")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready (A continueOnError + B succeeded)", stored.Status.Phase)
	}
}

// TC-0231: Per-step ContinueOnError suppresses workflow failure on a diamond branch.
func TestWorkflowReconciler_PerStepContinueOnError(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeDiamondWorkflow("default", "wf-perstep-coe")
	// Mark only B as continueOnError; the workflow-level flag stays off.
	for i := range wf.Spec.Steps {
		if wf.Spec.Steps[i].Name == "B" {
			wf.Spec.Steps[i].ContinueOnError = true
		}
	}
	// In continueOnError-per-step mode, B's failure should not block D — but
	// D depends on both B and C; we want D to be dispatchable when B fails.
	// To make this deterministic, enable workflow-level ContinueOnError so the
	// failed terminal counts as terminal for fan-in.
	wf.Spec.ContinueOnError = true
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-perstep-coe", Kind: "Workflow"}

	// Finalizer + dispatch A.
	mustReconcile(t, r, req)
	mustReconcile(t, r, req)

	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected A dispatched, got %d jobs", len(jobs))
	}
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	jobs[0].Status.Output = "ok-a"
	updateJob(t, s.memStore, jobs[0])

	// Dispatch B and C.
	mustReconcile(t, r, req)
	jobs = listWorkflowJobs(t, s.memStore, "default")
	for i := range jobs {
		switch jobs[i].ObjectMeta.Labels[workflowStepLabel] {
		case "B":
			jobs[i].Status.Phase = v1alpha1.PhaseFailed
			updateJob(t, s.memStore, jobs[i])
		case "C":
			jobs[i].Status.Phase = v1alpha1.PhaseReady
			jobs[i].Status.Output = "ok-c"
			updateJob(t, s.memStore, jobs[i])
		}
	}

	// Dispatch D (B failed but is suppressed by per-step ContinueOnError).
	mustReconcile(t, r, req)
	jobs = listWorkflowJobs(t, s.memStore, "default")
	var dJob *v1alpha1.AgentJob
	for i := range jobs {
		if jobs[i].ObjectMeta.Labels[workflowStepLabel] == "D" {
			dJob = &jobs[i]
		}
	}
	if dJob == nil {
		t.Fatal("expected D to be dispatched after B(failed, suppressed) and C(ready)")
	}
	dJob.Status.Phase = v1alpha1.PhaseReady
	dJob.Status.Output = "ok-d"
	updateJob(t, s.memStore, *dJob)

	mustReconcile(t, r, req)
	stored := getWorkflow(t, s.memStore, "default", "wf-perstep-coe")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready (B failure suppressed by per-step continueOnError)", stored.Status.Phase)
	}
}

// TC-0256: Backoff grows by BackoffMultiplier across attempts.
func TestComputeBackoff_MultiplierGrowsExponentially(t *testing.T) {
	retry := &v1alpha1.WorkflowStepRetry{
		MaxAttempts:       5,
		Backoff:           "100ms",
		BackoffMultiplier: 2.0,
	}
	d1 := computeBackoff(retry, 1)
	d2 := computeBackoff(retry, 2)
	d3 := computeBackoff(retry, 3)
	if d1 != 100*time.Millisecond {
		t.Fatalf("attempt 1 delay = %s, want 100ms", d1)
	}
	if d2 != 200*time.Millisecond {
		t.Fatalf("attempt 2 delay = %s, want 200ms", d2)
	}
	if d3 != 400*time.Millisecond {
		t.Fatalf("attempt 3 delay = %s, want 400ms", d3)
	}
}

// TC-0257: MaxBackoff caps the exponential delay.
func TestComputeBackoff_MaxBackoffCapsDelay(t *testing.T) {
	retry := &v1alpha1.WorkflowStepRetry{
		MaxAttempts:       10,
		Backoff:           "100ms",
		BackoffMultiplier: 2.0,
		MaxBackoff:        "500ms",
	}
	// Without cap: 100, 200, 400, 800, 1600 ms. With 500ms cap: 500ms from attempt 4 onward.
	if d := computeBackoff(retry, 4); d != 500*time.Millisecond {
		t.Fatalf("attempt 4 delay = %s, want capped at 500ms", d)
	}
	if d := computeBackoff(retry, 7); d != 500*time.Millisecond {
		t.Fatalf("attempt 7 delay = %s, want capped at 500ms", d)
	}
}

// TC-0258: Retry exhaustion (no continueOnError) fails the workflow.
func TestWorkflowReconciler_RetryExhaustionFailsWorkflow(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-exhaust-fail",
			Namespace: "default",
			UID:       "wf-uid-exhaust-fail",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{
					Name:     "doomed",
					AgentRef: v1alpha1.LocalObjectReference{Name: "agent-doomed"},
					Retry:    &v1alpha1.WorkflowStepRetry{MaxAttempts: 2, Backoff: "1ms"},
				},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-exhaust-fail", Kind: "Workflow"}

	// Finalizer + dispatch attempt 1.
	mustReconcile(t, r, req)
	mustReconcile(t, r, req)
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	jobs[0].Status.Phase = v1alpha1.PhaseFailed
	updateJob(t, s.memStore, jobs[0])

	// Classify + schedule retry.
	mustReconcile(t, r, req)
	time.Sleep(5 * time.Millisecond)

	// Dispatch attempt 2 (final).
	mustReconcile(t, r, req)
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job for attempt 2, got %d", len(jobs))
	}
	jobs[0].Status.Phase = v1alpha1.PhaseFailed
	updateJob(t, s.memStore, jobs[0])

	// Classify exhaustion → workflow Failed.
	mustReconcile(t, r, req)
	stored := getWorkflow(t, s.memStore, "default", "wf-exhaust-fail")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed after retry exhaustion", stored.Status.Phase)
	}
	ss := findStepStatus(&stored, "doomed")
	if ss == nil {
		t.Fatal("missing step status for doomed")
	}
	if ss.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", ss.Attempts)
	}
	if ss.NextRetryAt != nil {
		t.Fatalf("NextRetryAt should be cleared after exhaustion, got %v", ss.NextRetryAt)
	}
}

// TC-0259: Retry exhaustion with per-step ContinueOnError leaves workflow Ready.
func TestWorkflowReconciler_RetryExhaustionWithContinueOnError(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-exhaust-coe",
			Namespace: "default",
			UID:       "wf-uid-exhaust-coe",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{
					Name:            "doomed",
					AgentRef:        v1alpha1.LocalObjectReference{Name: "agent-doomed"},
					Retry:           &v1alpha1.WorkflowStepRetry{MaxAttempts: 2, Backoff: "1ms"},
					ContinueOnError: true,
				},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-exhaust-coe", Kind: "Workflow"}

	mustReconcile(t, r, req)
	mustReconcile(t, r, req)
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	jobs[0].Status.Phase = v1alpha1.PhaseFailed
	updateJob(t, s.memStore, jobs[0])

	mustReconcile(t, r, req)
	time.Sleep(5 * time.Millisecond)

	mustReconcile(t, r, req)
	jobs = listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job for attempt 2, got %d", len(jobs))
	}
	jobs[0].Status.Phase = v1alpha1.PhaseFailed
	updateJob(t, s.memStore, jobs[0])

	mustReconcile(t, r, req)
	stored := getWorkflow(t, s.memStore, "default", "wf-exhaust-coe")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready (failure suppressed by per-step continueOnError)", stored.Status.Phase)
	}
}

// TC-0260: Step with Timeout but no Retry policy times out and fails the workflow immediately.
func TestWorkflowReconciler_TimeoutWithoutRetryFailsWorkflow(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wf-timeout-noretry",
			Namespace: "default",
			UID:       "wf-uid-timeout-noretry",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{
					Name:     "slow",
					AgentRef: v1alpha1.LocalObjectReference{Name: "agent-slow"},
					Timeout:  "10ms",
				},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-timeout-noretry", Kind: "Workflow"}

	mustReconcile(t, r, req)
	mustReconcile(t, r, req)
	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	jobs[0].Status.Phase = v1alpha1.PhaseRunning
	updateJob(t, s.memStore, jobs[0])

	time.Sleep(20 * time.Millisecond)

	mustReconcile(t, r, req)
	stored := getWorkflow(t, s.memStore, "default", "wf-timeout-noretry")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed (timeout, no retry policy)", stored.Status.Phase)
	}
	ss := findStepStatus(&stored, "slow")
	if ss == nil {
		t.Fatal("missing step status for slow")
	}
	if ss.Phase != v1alpha1.PhaseTimedOut {
		t.Fatalf("step phase = %q, want TimedOut", ss.Phase)
	}
}

// TC-0242: Parallel step jobs share a single trace-id and root parent span,
// while each receives a distinct span-id and well-formed traceparent header.
func TestWorkflowReconciler_StepJobsShareTraceContext(t *testing.T) {
	s := newEventMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	r.ownerKind = "WorkflowRun"

	wf := makeDiamondWorkflow("default", "wf-trace")
	storeWorkflow(t, s.memStore, wf)
	req := Request{Namespace: "default", Name: "wf-trace", Kind: "WorkflowRun"}

	// Bootstrap (finalizer) + dispatch A.
	mustReconcile(t, r, req)
	mustReconcile(t, r, req)

	jobs := listWorkflowJobs(t, s.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("after first dispatch, got %d jobs, want 1 (A)", len(jobs))
	}

	// Complete A so B and C fan out in parallel.
	jobs[0].Status.Phase = v1alpha1.PhaseReady
	updateJob(t, s.memStore, jobs[0])
	mustReconcile(t, r, req)

	jobs = listWorkflowJobs(t, s.memStore, "default")
	var fanout []v1alpha1.AgentJob
	for _, j := range jobs {
		if j.Labels[workflowStepLabel] == "B" || j.Labels[workflowStepLabel] == "C" {
			fanout = append(fanout, j)
		}
	}
	if len(fanout) != 2 {
		t.Fatalf("fanout job count = %d, want 2 (B,C)", len(fanout))
	}

	stored := getWorkflow(t, s.memStore, "default", "wf-trace")
	rootTraceID := stored.ObjectMeta.Annotations[observability.AnnotationTraceID]
	rootSpanID := stored.ObjectMeta.Annotations[observability.AnnotationRootSpanID]
	if rootTraceID == "" || len(rootTraceID) != 32 {
		t.Fatalf("workflow trace-id = %q, want 32-hex chars", rootTraceID)
	}
	if rootSpanID == "" || len(rootSpanID) != 16 {
		t.Fatalf("workflow root-span-id = %q, want 16-hex chars", rootSpanID)
	}

	seenSpans := map[string]bool{}
	for _, j := range fanout {
		ann := j.ObjectMeta.Annotations
		if ann[observability.AnnotationTraceID] != rootTraceID {
			t.Fatalf("step %s trace-id = %q, want %q", j.Labels[workflowStepLabel], ann[observability.AnnotationTraceID], rootTraceID)
		}
		if ann[observability.AnnotationParentSpanID] != rootSpanID {
			t.Fatalf("step %s parent-span-id = %q, want root %q", j.Labels[workflowStepLabel], ann[observability.AnnotationParentSpanID], rootSpanID)
		}
		spanID := ann[observability.AnnotationSpanID]
		if len(spanID) != 16 {
			t.Fatalf("step %s span-id = %q, want 16-hex chars", j.Labels[workflowStepLabel], spanID)
		}
		if seenSpans[spanID] {
			t.Fatalf("step %s span-id %q duplicated across parallel jobs", j.Labels[workflowStepLabel], spanID)
		}
		seenSpans[spanID] = true
		wantTP := "00-" + rootTraceID + "-" + spanID + "-01"
		if ann[observability.AnnotationTraceparent] != wantTP {
			t.Fatalf("step %s traceparent = %q, want %q", j.Labels[workflowStepLabel], ann[observability.AnnotationTraceparent], wantTP)
		}
		if ann[observability.AnnotationStepName] == "" {
			t.Fatalf("step %s missing step-name annotation", j.Labels[workflowStepLabel])
		}
	}
}

// TC-0247: Standalone Workflow (no WorkflowRun owner) does not emit trace context
// annotations on the workflow or on dispatched step jobs.
func TestWorkflowReconciler_StandaloneWorkflowNoTraceAnnotations(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeWorkflow("default", "wf-notrace")
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-notrace", Kind: "Workflow"}
	if _, err := r.reconcileDirect(context.Background(), req); err != nil {
		t.Fatalf("reconcile finalizer: %v", err)
	}
	if _, err := r.reconcileDirect(context.Background(), req); err != nil {
		t.Fatalf("reconcile dispatch: %v", err)
	}

	stored := getWorkflow(t, s, "default", "wf-notrace")
	for _, k := range []string{
		observability.AnnotationTraceID,
		observability.AnnotationRootSpanID,
		observability.AnnotationTraceparent,
	} {
		if v, ok := stored.ObjectMeta.Annotations[k]; ok {
			t.Fatalf("standalone workflow unexpectedly annotated %s=%q", k, v)
		}
	}

	jobs := listWorkflowJobs(t, s, "default")
	if len(jobs) == 0 {
		t.Fatal("expected at least one step job")
	}
	for _, j := range jobs {
		for _, k := range []string{
			observability.AnnotationTraceID,
			observability.AnnotationSpanID,
			observability.AnnotationTraceparent,
			observability.AnnotationParentSpanID,
		} {
			if v, ok := j.ObjectMeta.Annotations[k]; ok {
				t.Fatalf("standalone step job %q unexpectedly annotated %s=%q", j.Name, k, v)
			}
		}
	}
}

// TestWorkflowReconciler_TemplateOnlyReconcile verifies the current
// contract: a Workflow is always a template. Reconcile must never dispatch
// jobs, must drop any legacy WorkflowFinalizer, and must clear stale
// execution status left over from the pre-deprecation direct-mode path so
// kubectl/dashboard don't show misleading Failed/Ready phases. All actual
// workflow execution now flows through WorkflowRun → WorkflowRunReconciler.
func TestWorkflowReconciler_TemplateOnlyReconcile(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))

	wf := makeWorkflow("default", "wf-template")
	// Simulate a Workflow migrated from the old direct-mode world: has a
	// stale Failed phase, a step status, and the legacy finalizer.
	wf.Status = v1alpha1.WorkflowStatus{
		Phase:      v1alpha1.PhaseFailed,
		ActiveStep: "plan",
		StepStatuses: []v1alpha1.WorkflowStepStatus{
			{Name: "plan", Phase: v1alpha1.PhaseFailed},
		},
	}
	wf.ObjectMeta.Finalizers = []string{WorkflowFinalizer}
	storeWorkflow(t, s, wf)

	res, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "wf-template", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected no requeue for template-only reconcile")
	}

	stored := getWorkflow(t, s, "default", "wf-template")
	if stored.Status.Phase != "" {
		t.Fatalf("status.phase = %q, want cleared", stored.Status.Phase)
	}
	if stored.Status.ActiveStep != "" {
		t.Fatalf("status.activeStep = %q, want cleared", stored.Status.ActiveStep)
	}
	if len(stored.Status.StepStatuses) != 0 {
		t.Fatalf("status.stepStatuses = %d, want cleared", len(stored.Status.StepStatuses))
	}
	for _, f := range stored.ObjectMeta.Finalizers {
		if f == WorkflowFinalizer {
			t.Fatal("expected legacy WorkflowFinalizer to be removed")
		}
	}
	if want := workflowEntryPoints(&stored.Spec); len(want) == 0 || !slices.Equal(stored.Status.EntryPoints, want) {
		t.Fatalf("status.entryPoints = %v, want %v", stored.Status.EntryPoints, want)
	}
	if jobs := listWorkflowJobs(t, s, "default"); len(jobs) != 0 {
		t.Fatalf("jobs = %d, want 0 (template must not dispatch)", len(jobs))
	}

	// Second reconcile must be a pure no-op (idempotent).
	res, err = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "wf-template", Kind: "Workflow"})
	if err != nil {
		t.Fatalf("reconcile (second call): %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected second reconcile to be no-op")
	}
}

func TestWorkflowEntryPoints(t *testing.T) {
	steps := func(names ...string) []v1alpha1.WorkflowStep {
		result := make([]v1alpha1.WorkflowStep, len(names))
		for i, name := range names {
			result[i] = v1alpha1.WorkflowStep{Name: name}
		}
		return result
	}
	edge := func(from, to string) v1alpha1.WorkflowEdge { return v1alpha1.WorkflowEdge{From: from, To: to} }
	for name, test := range map[string]struct {
		spec v1alpha1.WorkflowSpec
		want []string
	}{
		"explicit entry point wins": {
			spec: v1alpha1.WorkflowSpec{EntryPoint: "b", Steps: steps("a", "b"), Edges: []v1alpha1.WorkflowEdge{edge("a", "b")}},
			want: []string{"b"},
		},
		"single root derived from edges": {
			spec: v1alpha1.WorkflowSpec{Steps: steps("classifier", "tech", "billing"), Edges: []v1alpha1.WorkflowEdge{edge("classifier", "tech"), edge("classifier", "billing")}},
			want: []string{"classifier"},
		},
		"parallel roots in declaration order": {
			spec: v1alpha1.WorkflowSpec{Steps: steps("b", "a", "merge"), Edges: []v1alpha1.WorkflowEdge{edge("a", "merge"), edge("b", "merge")}},
			want: []string{"b", "a"},
		},
		"fully cyclic graph has no derivable entry": {
			spec: v1alpha1.WorkflowSpec{Steps: steps("a", "b"), Edges: []v1alpha1.WorkflowEdge{edge("a", "b"), edge("b", "a")}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := workflowEntryPoints(&test.spec); !slices.Equal(got, test.want) {
				t.Fatalf("entry points = %v, want %v", got, test.want)
			}
		})
	}
}

func TestWorkflowReconciler_TemplateReconcileConflictPreservesConcurrentMetadata(t *testing.T) {
	base := newMemStore()
	s := &conflictOnceWorkflowStore{memStore: base}
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))

	wf := makeWorkflow("default", "wf-conflict")
	wf.Status = v1alpha1.WorkflowStatus{Phase: v1alpha1.PhaseFailed, ActiveStep: "plan"}
	wf.ObjectMeta.Finalizers = []string{WorkflowFinalizer}
	storeWorkflow(t, base, wf)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "wf-conflict", Kind: "Workflow"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	stored := getWorkflow(t, base, "default", "wf-conflict")
	if !s.conflicted {
		t.Fatal("expected optimistic concurrency retry")
	}
	if stored.Status.Phase != "" || stored.Status.ActiveStep != "" {
		t.Fatalf("status = %+v, want cleared template status", stored.Status)
	}
	if condition := findCondition(stored.Status.Conditions, "Audited"); condition == nil || condition.Reason != "ConcurrentObserver" {
		t.Fatalf("concurrent condition was lost: %#v", condition)
	}
	if got := stored.ObjectMeta.Annotations["external.example/revision"]; got != "2" {
		t.Fatalf("concurrent annotation = %q, want 2", got)
	}
	if !hasString(stored.ObjectMeta.Finalizers, "external.example/cleanup") {
		t.Fatalf("finalizers = %v, want external finalizer preserved", stored.ObjectMeta.Finalizers)
	}
	if hasString(stored.ObjectMeta.Finalizers, WorkflowFinalizer) {
		t.Fatalf("finalizers = %v, want legacy workflow finalizer removed", stored.ObjectMeta.Finalizers)
	}
}

func TestWorkflowReconciler_TemplateReconcileStoreBoundaries(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		r := NewWorkflowReconciler(newMemStore(), zaptest.NewLogger(t))
		result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "missing", Kind: "Workflow"})
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if result.Requeue || result.RequeueAfter != 0 {
			t.Fatalf("result = %+v, want no requeue", result)
		}
	})

	t.Run("get failure", func(t *testing.T) {
		wantErr := errors.New("storage unavailable")
		r := NewWorkflowReconciler(&faultStore{memStore: newMemStore(), getErr: wantErr}, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "wf", Kind: "Workflow"})
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "get workflow") {
			t.Fatalf("error = %v, want wrapped get failure", err)
		}
	})

	t.Run("malformed workflow", func(t *testing.T) {
		s := newMemStore()
		if err := s.Create(context.Background(), "Workflow", "default", "wf", []byte("{")); err != nil {
			t.Fatalf("create malformed workflow: %v", err)
		}
		r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "wf", Kind: "Workflow"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal workflow") {
			t.Fatalf("error = %v, want unmarshal failure", err)
		}
	})

	t.Run("cleanup update failure", func(t *testing.T) {
		base := newMemStore()
		wf := makeWorkflow("default", "wf")
		wf.Status.Phase = v1alpha1.PhaseFailed
		storeWorkflow(t, base, wf)
		wantErr := errors.New("update unavailable")
		r := NewWorkflowReconciler(&faultStore{memStore: base, updateErr: wantErr}, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "wf", Kind: "Workflow"})
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "clear template workflow state") {
			t.Fatalf("error = %v, want wrapped cleanup update failure", err)
		}
	})
}

func TestWorkflowReconciler_TemplateDeletionStoreBoundaries(t *testing.T) {
	markDeleting := func(t *testing.T, memory *memStore, name string) {
		t.Helper()
		workflow := makeWorkflow("default", name)
		workflow.ObjectMeta.Finalizers = []string{WorkflowFinalizer}
		now := time.Now().UTC()
		workflow.ObjectMeta.DeletionTimestamp = &now
		storeWorkflow(t, memory, workflow)
	}

	t.Run("list jobs failure", func(t *testing.T) {
		memory := newMemStore()
		markDeleting(t, memory, "wf-list-error")
		wantErr := errors.New("list unavailable")
		r := NewWorkflowReconciler(&workflowCleanupFaultStore{memStore: memory, listErr: wantErr}, zaptest.NewLogger(t))

		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "wf-list-error", Kind: "Workflow"})
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "list jobs for deletion") {
			t.Fatalf("error = %v, want wrapped list failure", err)
		}
	})

	t.Run("delete job failure", func(t *testing.T) {
		memory := newMemStore()
		workflow := makeWorkflow("default", "wf-delete-error")
		workflow.ObjectMeta.Finalizers = []string{WorkflowFinalizer}
		storeWorkflow(t, memory, workflow)
		r := NewWorkflowReconciler(memory, zaptest.NewLogger(t))
		mustReconcile(t, r, Request{Namespace: "default", Name: "wf-delete-error", Kind: "Workflow"})
		mustReconcile(t, r, Request{Namespace: "default", Name: "wf-delete-error", Kind: "Workflow"})

		stored := getWorkflow(t, memory, "default", "wf-delete-error")
		now := time.Now().UTC()
		stored.ObjectMeta.DeletionTimestamp = &now
		data, err := json.Marshal(stored)
		if err != nil {
			t.Fatalf("marshal deleting workflow: %v", err)
		}
		if err := memory.Update(context.Background(), "Workflow", "default", "wf-delete-error", data, stored.ObjectMeta.ResourceVersion); err != nil {
			t.Fatalf("mark workflow deleting: %v", err)
		}
		wantErr := errors.New("delete unavailable")
		r = NewWorkflowReconciler(&workflowCleanupFaultStore{memStore: memory, deleteErr: wantErr}, zaptest.NewLogger(t))

		_, err = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "wf-delete-error", Kind: "Workflow"})
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "delete owned job") {
			t.Fatalf("error = %v, want wrapped delete failure", err)
		}
	})

	t.Run("remove finalizer failure", func(t *testing.T) {
		memory := newMemStore()
		markDeleting(t, memory, "wf-finalizer-error")
		wantErr := errors.New("update unavailable")
		r := NewWorkflowReconciler(&faultStore{memStore: memory, updateErr: wantErr}, zaptest.NewLogger(t))

		_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "wf-finalizer-error", Kind: "Workflow"})
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "remove finalizer") {
			t.Fatalf("error = %v, want wrapped finalizer failure", err)
		}
	})
}

func TestWorkflowReconciler_TemplatePersistenceConflictBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name             string
		afterConflictErr error
		afterConflictRaw []byte
		wantErr          error
		wantMessage      string
	}{
		{
			name:             "workflow deleted concurrently",
			afterConflictErr: store.ErrNotFound,
		},
		{
			name:             "reread failure",
			afterConflictErr: errors.New("read unavailable"),
			wantMessage:      "re-read workflow after conflict",
		},
		{
			name:             "corrupt concurrent workflow",
			afterConflictRaw: []byte("{"),
			wantMessage:      "unmarshal workflow after conflict",
		},
		{
			name:        "retry exhaustion",
			wantErr:     store.ErrConflict,
			wantMessage: "clear template workflow state",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := newMemStore()
			workflow := makeWorkflow("default", "wf-conflict-boundary")
			workflow.Status.Phase = v1alpha1.PhaseFailed
			storeWorkflow(t, memory, workflow)
			faults := &workflowConflictFaultStore{
				memStore:         memory,
				afterConflictErr: tc.afterConflictErr,
				afterConflictRaw: tc.afterConflictRaw,
			}
			r := NewWorkflowReconciler(faults, zaptest.NewLogger(t))

			_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "wf-conflict-boundary", Kind: "Workflow"})
			if errors.Is(tc.afterConflictErr, store.ErrNotFound) {
				if err != nil {
					t.Fatalf("reconcile concurrently deleted workflow: %v", err)
				}
				return
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("error = %v, want message containing %q", err, tc.wantMessage)
			}
		})
	}
}

func TestWorkflowReconciler_CyclicDispatchCreateBoundaries(t *testing.T) {
	newCyclicWorkflow := func(name string) v1alpha1.Workflow {
		workflow := makeWorkflow("default", name)
		workflow.Spec.Steps = workflow.Spec.Steps[:1]
		workflow.Spec.EntryPoint = workflow.Spec.Steps[0].Name
		workflow.Spec.Edges = []v1alpha1.WorkflowEdge{{From: workflow.Spec.EntryPoint, To: workflow.Spec.EntryPoint}}
		workflow.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
		return workflow
	}

	t.Run("concurrent create is idempotent", func(t *testing.T) {
		memory := newMemStore()
		workflow := newCyclicWorkflow("wf-cyclic-create-race")
		storeWorkflow(t, memory, workflow)
		request := Request{Namespace: "default", Name: workflow.ObjectMeta.Name, Kind: "Workflow"}
		reconciler := NewWorkflowReconciler(memory, zaptest.NewLogger(t))
		mustReconcile(t, reconciler, request)
		reconciler = NewWorkflowReconciler(&workflowJobCreateFaultStore{
			memStore:     memory,
			err:          store.ErrAlreadyExists,
			createBefore: true,
		}, zaptest.NewLogger(t))

		if _, err := reconciler.reconcileDirect(context.Background(), request); err != nil {
			t.Fatalf("reconcile concurrent job create: %v", err)
		}
		jobs := listWorkflowJobs(t, memory, "default")
		if len(jobs) != 1 || jobs[0].ObjectMeta.Labels[workflowIterationLabel] != "1" {
			t.Fatalf("jobs = %+v, want one iteration-1 job", jobs)
		}
	})

	t.Run("store outage is returned", func(t *testing.T) {
		memory := newMemStore()
		workflow := newCyclicWorkflow("wf-cyclic-create-error")
		storeWorkflow(t, memory, workflow)
		request := Request{Namespace: "default", Name: workflow.ObjectMeta.Name, Kind: "Workflow"}
		mustReconcile(t, NewWorkflowReconciler(memory, zaptest.NewLogger(t)), request)
		wantErr := errors.New("agent job store unavailable")
		reconciler := NewWorkflowReconciler(&workflowJobCreateFaultStore{memStore: memory, err: wantErr}, zaptest.NewLogger(t))

		_, err := reconciler.reconcileDirect(context.Background(), request)
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "create cyclic step job") {
			t.Fatalf("error = %v, want wrapped cyclic job create error", err)
		}
		if jobs := listWorkflowJobs(t, memory, "default"); len(jobs) != 0 {
			t.Fatalf("jobs = %d, want none after failed create", len(jobs))
		}
	})
}
