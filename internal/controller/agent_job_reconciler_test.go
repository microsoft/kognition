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

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

type createFaultStore struct {
	*memStore
	createErr error
}

type agentJobBoundaryFaultStore struct {
	*memStore
	getKind    string
	getErr     error
	listKind   string
	listErr    error
	createKind string
	createErr  error
	updateKind string
	updateErr  error
	deleteKind string
	deleteErr  error
}

func (s *agentJobBoundaryFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == s.getKind && s.getErr != nil {
		return nil, s.getErr
	}
	return s.memStore.Get(ctx, kind, namespace, name)
}

func (s *agentJobBoundaryFaultStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if kind == s.listKind && s.listErr != nil {
		return nil, s.listErr
	}
	return s.memStore.List(ctx, kind, opts)
}

func (s *agentJobBoundaryFaultStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if kind == s.createKind && s.createErr != nil {
		return s.createErr
	}
	return s.memStore.Create(ctx, kind, namespace, name, data)
}

func (s *agentJobBoundaryFaultStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == s.updateKind && s.updateErr != nil {
		return s.updateErr
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *agentJobBoundaryFaultStore) Delete(ctx context.Context, kind, namespace, name string) error {
	if kind == s.deleteKind && s.deleteErr != nil {
		return s.deleteErr
	}
	return s.memStore.Delete(ctx, kind, namespace, name)
}

type inlineAgentCreateRaceStore struct {
	*memStore
	traced bool
}

func (s *inlineAgentCreateRaceStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if kind == "Agent" && !s.traced {
		s.traced = true
		if err := s.memStore.Create(ctx, kind, namespace, name, data); err != nil {
			return err
		}
		return store.ErrAlreadyExists
	}
	return s.memStore.Create(ctx, kind, namespace, name, data)
}

type agentJobConflictStore struct {
	*memStore
	conflicted bool
}

type agentJobReadyConflictStore struct {
	*memStore
	conflicted     bool
	completionTime time.Time
}

type agentJobFailedConflictStore struct {
	*memStore
	conflicted     bool
	completionTime time.Time
}

type agentJobPendingConflictStore struct {
	*memStore
	conflicted      bool
	instanceCreates int
}

type persistentAgentJobPendingConflictStore struct {
	*memStore
	updates         int
	instanceCreates int
}

func (s *agentJobPendingConflictStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if kind == agentInstanceKind {
		s.instanceCreates++
	}
	return s.memStore.Create(ctx, kind, namespace, name, data)
}

func (s *agentJobPendingConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "AgentJob" && !s.conflicted {
		var desired v1alpha1.AgentJob
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase == v1alpha1.PhasePending {
			s.conflicted = true
			raw, err := s.memStore.Get(ctx, kind, namespace, name)
			if err != nil {
				return err
			}
			var latest v1alpha1.AgentJob
			if err := json.Unmarshal(raw, &latest); err != nil {
				return err
			}
			latest.Spec.Input = "concurrent-input"
			latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{
				Type: "Audited", Status: "True", Reason: "ConcurrentAudit",
			})
			updated, err := json.Marshal(latest)
			if err != nil {
				return err
			}
			if err := s.memStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
				return err
			}
			return store.ErrConflict
		}
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *persistentAgentJobPendingConflictStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if kind == agentInstanceKind {
		s.instanceCreates++
	}
	return s.memStore.Create(ctx, kind, namespace, name, data)
}

func (s *persistentAgentJobPendingConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "AgentJob" {
		var desired v1alpha1.AgentJob
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase == v1alpha1.PhasePending {
			s.updates++
			return store.ErrConflict
		}
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *agentJobConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "AgentJob" && !s.conflicted {
		s.conflicted = true
		raw, err := s.memStore.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var latest v1alpha1.AgentJob
		if err := json.Unmarshal(raw, &latest); err != nil {
			return err
		}
		latest.ObjectMeta.Finalizers = append(latest.ObjectMeta.Finalizers, "example.com/foreign")
		latest.Status.Output = "concurrent-output"
		latest.Status.Conditions = append(latest.Status.Conditions,
			v1alpha1.Condition{Type: "Ready", Status: "True", Reason: "ConcurrentReady"},
			v1alpha1.Condition{Type: "Audited", Status: "True", Reason: "ConcurrentObserver"},
		)
		updated, err := json.Marshal(latest)
		if err != nil {
			return err
		}
		if err := s.memStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
			return err
		}
		return store.ErrConflict
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *agentJobReadyConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "AgentJob" && !s.conflicted {
		var desired v1alpha1.AgentJob
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase == v1alpha1.PhaseFailed {
			s.conflicted = true
			raw, err := s.memStore.Get(ctx, kind, namespace, name)
			if err != nil {
				return err
			}
			var latest v1alpha1.AgentJob
			if err := json.Unmarshal(raw, &latest); err != nil {
				return err
			}
			latest.Status.Phase = v1alpha1.PhaseReady
			latest.Status.Succeeded = 1
			latest.Status.Failed = 0
			latest.Status.Output = "concurrent-success"
			latest.Status.CompletionTime = &s.completionTime
			latest.Status.Conditions = []v1alpha1.Condition{{
				Type: "Ready", Status: "True", Reason: "Completed",
			}}
			updated, err := json.Marshal(latest)
			if err != nil {
				return err
			}
			if err := s.memStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
				return err
			}
			return store.ErrConflict
		}
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *agentJobFailedConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == "AgentJob" && !s.conflicted {
		var desired v1alpha1.AgentJob
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase == v1alpha1.PhaseReady {
			s.conflicted = true
			raw, err := s.memStore.Get(ctx, kind, namespace, name)
			if err != nil {
				return err
			}
			var latest v1alpha1.AgentJob
			if err := json.Unmarshal(raw, &latest); err != nil {
				return err
			}
			latest.Status.Phase = v1alpha1.PhaseFailed
			latest.Status.Succeeded = 0
			latest.Status.Failed = 1
			latest.Status.Output = ""
			latest.Status.CompletionTime = &s.completionTime
			latest.Status.Conditions = []v1alpha1.Condition{{
				Type: "Ready", Status: "False", Reason: "Failed",
			}}
			updated, err := json.Marshal(latest)
			if err != nil {
				return err
			}
			if err := s.memStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
				return err
			}
			return store.ErrConflict
		}
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *createFaultStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if s.createErr != nil && kind == "AgentInstance" {
		return s.createErr
	}
	return s.memStore.Create(ctx, kind, namespace, name, data)
}

func makeAgentJob(ns, name string) v1alpha1.AgentJob {
	return v1alpha1.AgentJob{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentJob"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "job-uid-001",
		},
		Spec: v1alpha1.AgentJobSpec{
			AgentRef: &v1alpha1.LocalObjectReference{Name: "my-agent"},
		},
		Status: v1alpha1.AgentJobStatus{Phase: v1alpha1.PhasePending},
	}
}

func storeJob(t *testing.T, s *memStore, job v1alpha1.AgentJob) {
	t.Helper()
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	if err := s.Create(context.Background(), "AgentJob", job.ObjectMeta.Namespace, job.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store job: %v", err)
	}
}

func getJob(t *testing.T, s *memStore, ns, name string) v1alpha1.AgentJob {
	t.Helper()
	raw, err := s.Get(context.Background(), "AgentJob", ns, name)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	var job v1alpha1.AgentJob
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatalf("unmarshal job: %v", err)
	}
	return job
}

func listJobInstances(t *testing.T, s *memStore, ns string) []v1alpha1.AgentInstance {
	t.Helper()
	rawList, err := s.List(context.Background(), "AgentInstance", store.ListOptions{Namespace: ns})
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	var instances []v1alpha1.AgentInstance
	for _, raw := range rawList {
		var inst v1alpha1.AgentInstance
		if err := json.Unmarshal(raw, &inst); err != nil {
			t.Fatalf("unmarshal instance: %v", err)
		}
		instances = append(instances, inst)
	}
	return instances
}

func TestAgentJobReconciler_CreatesInstance(t *testing.T) {
	s := newMemStore()
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-create")
	job.ObjectMeta.Annotations = map[string]string{
		observability.AnnotationTraceID:      "0123456789abcdef0123456789abcdef",
		observability.AnnotationSpanID:       "0123456789abcdef",
		observability.AnnotationTraceparent:  "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
		observability.AnnotationWorkflowName: "wf-a",
	}
	storeJob(t, s, job)

	res, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-create", Kind: "AgentJob"})
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	res, err = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-create", Kind: "AgentJob"})
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after creating instance")
	}

	instances := listJobInstances(t, s, "default")
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}
	inst := instances[0]
	if inst.Spec.AgentRef.Name != "my-agent" {
		t.Fatalf("instance agentRef=%q, want my-agent", inst.Spec.AgentRef.Name)
	}
	if len(inst.ObjectMeta.OwnerReferences) == 0 || inst.ObjectMeta.OwnerReferences[0].Kind != "AgentJob" {
		t.Fatalf("expected AgentJob owner reference, got %+v", inst.ObjectMeta.OwnerReferences)
	}
	if got := inst.ObjectMeta.Annotations[observability.AnnotationTraceparent]; got != job.ObjectMeta.Annotations[observability.AnnotationTraceparent] {
		t.Fatalf("instance traceparent annotation = %q, want %q", got, job.ObjectMeta.Annotations[observability.AnnotationTraceparent])
	}

	gotJob := getJob(t, s, "default", "job-create")
	if gotJob.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("expected job phase Pending, got %s", gotJob.Status.Phase)
	}
}

func TestAgentJobReconciler_PropagatesPinnedAgentRevisionAcrossRetries(t *testing.T) {
	s := newMemStore()
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	pinnedRevision := int64(7)
	backoffLimit := int32(1)
	job := makeAgentJob("default", "pinned-job")
	job.Spec.AgentRevision = &pinnedRevision
	job.Spec.BackoffLimit = &backoffLimit
	storeJob(t, s, job)
	request := Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"}

	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("create first instance: %v", err)
	}
	instances := listJobInstances(t, s, "default")
	if len(instances) != 1 || instances[0].Spec.Revision != 1 || instances[0].Spec.AgentRevision == nil || *instances[0].Spec.AgentRevision != pinnedRevision {
		t.Fatalf("first instance = %#v, want attempt 1 at agent revision %d", instances, pinnedRevision)
	}

	instances[0].Status.Phase = v1alpha1.PhaseFailed
	data, err := json.Marshal(instances[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(context.Background(), "AgentInstance", "default", instances[0].ObjectMeta.Name, data, instances[0].ObjectMeta.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("create retry instance: %v", err)
	}
	instances = listJobInstances(t, s, "default")
	if len(instances) != 2 {
		t.Fatalf("instances = %#v, want two attempts", instances)
	}
	for _, instance := range instances {
		if instance.Spec.AgentRevision == nil || *instance.Spec.AgentRevision != pinnedRevision {
			t.Fatalf("instance %#v lost pinned agent revision %d", instance.Spec, pinnedRevision)
		}
	}
}

func TestAgentJobReconciler_FinalizerConflictPreservesConcurrentState(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	s := &agentJobConflictStore{memStore: baseStore}
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-conflict")
	storeJob(t, baseStore, job)

	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "job-conflict", Kind: "AgentJob"})
	if err != nil {
		t.Fatalf("reconcile through finalizer conflict: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	updated := getJob(t, baseStore, "default", "job-conflict")
	if !hasFinalizer(updated.ObjectMeta.Finalizers, AgentJobFinalizer) || !hasFinalizer(updated.ObjectMeta.Finalizers, "example.com/foreign") {
		t.Fatalf("finalizers = %v, want owned and foreign finalizers", updated.ObjectMeta.Finalizers)
	}
	if updated.Status.Output != "concurrent-output" {
		t.Fatalf("concurrent output was lost: %q", updated.Status.Output)
	}
	if condition := findCondition(updated.Status.Conditions, "Audited"); condition == nil || condition.Reason != "ConcurrentObserver" {
		t.Fatalf("concurrent condition was lost: %#v", condition)
	}
	if condition := findCondition(updated.Status.Conditions, "Ready"); condition == nil || condition.Reason != "ConcurrentReady" {
		t.Fatalf("concurrent Ready condition was lost: %#v", condition)
	}
}

func TestAgentJobReconciler_PendingConflictPreservesSingleExecutionAndConcurrentState(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	s := &agentJobPendingConflictStore{memStore: baseStore}
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-pending-conflict")
	job.ObjectMeta.Finalizers = []string{AgentJobFinalizer}
	storeJob(t, baseStore, job)

	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"})
	if err != nil {
		t.Fatalf("reconcile through Pending conflict: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected requeue after creating instance")
	}
	if !s.conflicted {
		t.Fatal("expected Pending conflict fixture to execute")
	}
	if s.instanceCreates != 1 {
		t.Fatalf("AgentInstance create calls = %d, want exactly 1", s.instanceCreates)
	}
	if instances := listJobInstances(t, baseStore, "default"); len(instances) != 1 {
		t.Fatalf("AgentInstances = %d, want 1", len(instances))
	}
	updated := getJob(t, baseStore, "default", job.ObjectMeta.Name)
	if updated.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("phase = %s, want Pending", updated.Status.Phase)
	}
	if updated.Spec.Input != "concurrent-input" {
		t.Fatalf("concurrent input was lost: %q", updated.Spec.Input)
	}
	if condition := findCondition(updated.Status.Conditions, "Audited"); condition == nil || condition.Reason != "ConcurrentAudit" {
		t.Fatalf("concurrent Audited condition was lost: %#v", condition)
	}
}

func TestAgentJobReconciler_PendingConflictRetriesAreBounded(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	s := &persistentAgentJobPendingConflictStore{memStore: baseStore}
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-pending-persistent-conflict")
	job.ObjectMeta.Finalizers = []string{AgentJobFinalizer}
	storeJob(t, baseStore, job)

	_, err := r.Reconcile(ctx, Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Reconcile() error = %v, want ErrConflict", err)
	}
	if s.updates != 5 {
		t.Fatalf("AgentJob update attempts = %d, want 5", s.updates)
	}
	if s.instanceCreates != 1 {
		t.Fatalf("AgentInstance create calls = %d, want exactly 1", s.instanceCreates)
	}
	if instances := listJobInstances(t, baseStore, "default"); len(instances) != 1 {
		t.Fatalf("AgentInstances = %d, want 1", len(instances))
	}
}

func TestAgentJobReconciler_FailureConflictPreservesConcurrentSuccess(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	completionTime := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	s := &agentJobReadyConflictStore{memStore: baseStore, completionTime: completionTime}
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-terminal-conflict")
	job.ObjectMeta.Finalizers = []string{AgentJobFinalizer}
	storeJob(t, baseStore, job)
	storeInstance(t, baseStore, v1alpha1.AgentInstance{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: agentInstanceKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name: "job-terminal-conflict-r1", Namespace: "default", UID: "instance-uid",
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion, Kind: "AgentJob", Name: job.ObjectMeta.Name, UID: job.ObjectMeta.UID,
			}},
		},
		Status: v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhaseFailed},
	})

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"}); err != nil {
		t.Fatalf("reconcile through terminal conflict: %v", err)
	}
	if !s.conflicted {
		t.Fatal("expected terminal conflict fixture to execute")
	}
	updated := getJob(t, baseStore, "default", job.ObjectMeta.Name)
	if updated.Status.Phase != v1alpha1.PhaseReady || updated.Status.Succeeded != 1 || updated.Status.Failed != 0 {
		t.Fatalf("terminal counts = phase %q, succeeded %d, failed %d; want Ready, 1, 0", updated.Status.Phase, updated.Status.Succeeded, updated.Status.Failed)
	}
	if updated.Status.Output != "concurrent-success" {
		t.Fatalf("output = %q, want concurrent-success", updated.Status.Output)
	}
	if updated.Status.CompletionTime == nil || !updated.Status.CompletionTime.Equal(completionTime) {
		t.Fatalf("completion time = %v, want %v", updated.Status.CompletionTime, completionTime)
	}
	if condition := findCondition(updated.Status.Conditions, "Ready"); condition == nil || condition.Status != "True" || condition.Reason != "Completed" {
		t.Fatalf("Ready condition = %#v, want concurrent success", condition)
	}
}

func TestAgentJobReconciler_SuccessConflictPreservesConcurrentFailure(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	completionTime := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	s := &agentJobFailedConflictStore{memStore: baseStore, completionTime: completionTime}
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-terminal-failure-conflict")
	job.ObjectMeta.Finalizers = []string{AgentJobFinalizer}
	storeJob(t, baseStore, job)
	storeInstance(t, baseStore, v1alpha1.AgentInstance{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: agentInstanceKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name: "job-terminal-failure-conflict-r1", Namespace: "default", UID: "instance-uid",
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion, Kind: "AgentJob", Name: job.ObjectMeta.Name, UID: job.ObjectMeta.UID,
			}},
		},
		Status: v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhaseReady, Output: "stale-success"},
	})

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"}); err != nil {
		t.Fatalf("reconcile through terminal conflict: %v", err)
	}
	if !s.conflicted {
		t.Fatal("expected terminal conflict fixture to execute")
	}
	updated := getJob(t, baseStore, "default", job.ObjectMeta.Name)
	if updated.Status.Phase != v1alpha1.PhaseFailed || updated.Status.Succeeded != 0 || updated.Status.Failed != 1 {
		t.Fatalf("terminal counts = phase %q, succeeded %d, failed %d; want Failed, 0, 1", updated.Status.Phase, updated.Status.Succeeded, updated.Status.Failed)
	}
	if updated.Status.Output != "" {
		t.Fatalf("output = %q, want concurrent failure output", updated.Status.Output)
	}
	if updated.Status.CompletionTime == nil || !updated.Status.CompletionTime.Equal(completionTime) {
		t.Fatalf("completion time = %v, want %v", updated.Status.CompletionTime, completionTime)
	}
	if condition := findCondition(updated.Status.Conditions, "Ready"); condition == nil || condition.Status != "False" || condition.Reason != "Failed" {
		t.Fatalf("Ready condition = %#v, want concurrent failure", condition)
	}
}

func TestAgentJobReconciler_SucceedsWhenInstanceReady(t *testing.T) {
	s := newMemStore()
	// TC-0023: AgentJob drives one-shot execution and records success.
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-success")
	storeJob(t, s, job)

	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-success", Kind: "AgentJob"})
	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-success", Kind: "AgentJob"})

	instances := listJobInstances(t, s, "default")
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}
	inst := instances[0]
	startTime := time.Now().UTC().Add(-3 * time.Second)
	inst.Status.Phase = v1alpha1.PhaseReady
	inst.Status.StartTime = &startTime
	inst.Status.TokenUsage = &v1alpha1.TokenUsage{PromptTokens: 90, CompletionTokens: 30, TotalTokens: 120}
	inst.Status.ToolCalls = 2
	inst.Status.Output = "completed output"
	data, _ := json.Marshal(inst)
	if err := s.Update(context.Background(), "AgentInstance", inst.ObjectMeta.Namespace, inst.ObjectMeta.Name, data, inst.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update instance to ready: %v", err)
	}

	res, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-success", Kind: "AgentJob"})
	if err != nil {
		t.Fatalf("reconcile success: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected terminal success without requeue")
	}

	gotJob := getJob(t, s, "default", "job-success")
	if gotJob.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected job phase Ready, got %s", gotJob.Status.Phase)
	}
	if gotJob.Status.Succeeded != 1 {
		t.Fatalf("expected succeeded=1, got %d", gotJob.Status.Succeeded)
	}
	if gotJob.Status.CompletionTime == nil {
		t.Fatal("expected completionTime to be set")
	}
	if gotJob.Status.StartTime == nil || !gotJob.Status.StartTime.Equal(startTime) {
		t.Fatalf("job startTime = %v, want %v", gotJob.Status.StartTime, startTime)
	}
	if gotJob.Status.TokenUsage == nil || gotJob.Status.TokenUsage.TotalTokens != 120 {
		t.Fatalf("job tokenUsage = %+v, want copied runtime telemetry", gotJob.Status.TokenUsage)
	}
	if gotJob.Status.ToolCalls != 2 || gotJob.Status.Output != "completed output" {
		t.Fatalf("job tool/output telemetry = toolCalls %d output %q", gotJob.Status.ToolCalls, gotJob.Status.Output)
	}
}

func TestAgentJobReconciler_PrefersSucceededInstanceWithOutput(t *testing.T) {
	s := newMemStore()
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-output-selection")
	storeJob(t, s, job)

	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-output-selection", Kind: "AgentJob"})
	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-output-selection", Kind: "AgentJob"})

	instances := listJobInstances(t, s, "default")
	if len(instances) != 1 {
		t.Fatalf("expected 1 created instance, got %d", len(instances))
	}
	firstStart := time.Now().UTC().Add(-4 * time.Second)
	instances[0].Status.Phase = v1alpha1.PhaseReady
	instances[0].Status.StartTime = &firstStart
	instances[0].Status.TokenUsage = &v1alpha1.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	instances[0].Status.ToolCalls = 1
	instances[0].Status.Output = ""
	data, _ := json.Marshal(instances[0])
	if err := s.Update(context.Background(), "AgentInstance", instances[0].ObjectMeta.Namespace, instances[0].ObjectMeta.Name, data, instances[0].ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update first ready instance: %v", err)
	}

	secondStart := time.Now().UTC().Add(-2 * time.Second)
	second := instances[0]
	second.ObjectMeta.Name = "job-output-selection-with-output"
	second.ObjectMeta.ResourceVersion = 0
	second.Status.StartTime = &secondStart
	second.Status.TokenUsage = &v1alpha1.TokenUsage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}
	second.Status.ToolCalls = 3
	second.Status.Output = "preferred output"
	data, _ = json.Marshal(second)
	if err := s.Create(context.Background(), "AgentInstance", second.ObjectMeta.Namespace, second.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store second ready instance: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-output-selection", Kind: "AgentJob"}); err != nil {
		t.Fatalf("reconcile success: %v", err)
	}

	gotJob := getJob(t, s, "default", "job-output-selection")
	if gotJob.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected job phase Ready, got %s", gotJob.Status.Phase)
	}
	if gotJob.Status.Output != "preferred output" {
		t.Fatalf("job output = %q, want preferred output", gotJob.Status.Output)
	}
	if gotJob.Status.StartTime == nil || !gotJob.Status.StartTime.Equal(secondStart) {
		t.Fatalf("job startTime = %v, want %v", gotJob.Status.StartTime, secondStart)
	}
	if gotJob.Status.TokenUsage == nil || gotJob.Status.TokenUsage.TotalTokens != 30 || gotJob.Status.ToolCalls != 3 {
		t.Fatalf("job telemetry = tokenUsage %+v toolCalls %d, want selected output instance telemetry", gotJob.Status.TokenUsage, gotJob.Status.ToolCalls)
	}
}

func TestAgentJobReconciler_RetriesUntilBackoffLimitThenFails(t *testing.T) {
	s := newMemStore()
	// TC-0024: AgentJob retries failures up to backoffLimit then marks failed.
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-fail")
	limit := int32(1)
	job.Spec.BackoffLimit = &limit
	storeJob(t, s, job)

	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-fail", Kind: "AgentJob"})
	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-fail", Kind: "AgentJob"})

	instances := listJobInstances(t, s, "default")
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}
	first := instances[0]
	first.Status.Phase = v1alpha1.PhaseFailed
	data, _ := json.Marshal(first)
	if err := s.Update(context.Background(), "AgentInstance", first.ObjectMeta.Namespace, first.ObjectMeta.Name, data, first.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark first failed: %v", err)
	}

	// Reconcile after first failure should create retry instance.
	res, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-fail", Kind: "AgentJob"})
	if err != nil {
		t.Fatalf("reconcile retry: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after retry instance creation")
	}

	instances = listJobInstances(t, s, "default")
	if len(instances) != 2 {
		t.Fatalf("expected 2 instances after retry, got %d", len(instances))
	}

	// Fail second instance -> terminal failure (> backoffLimit).
	var second v1alpha1.AgentInstance
	if instances[0].ObjectMeta.Name == first.ObjectMeta.Name {
		second = instances[1]
	} else {
		second = instances[0]
	}
	second.Status.Phase = v1alpha1.PhaseFailed
	data, _ = json.Marshal(second)
	if err := s.Update(context.Background(), "AgentInstance", second.ObjectMeta.Namespace, second.ObjectMeta.Name, data, second.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark second failed: %v", err)
	}

	res, err = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-fail", Kind: "AgentJob"})
	if err != nil {
		t.Fatalf("reconcile terminal failure: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected terminal failure without requeue")
	}

	gotJob := getJob(t, s, "default", "job-fail")
	if gotJob.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected job phase Failed, got %s", gotJob.Status.Phase)
	}
	if gotJob.Status.Failed != 2 {
		t.Fatalf("expected failed=2, got %d", gotJob.Status.Failed)
	}
	if gotJob.Status.CompletionTime == nil {
		t.Fatal("expected completionTime to be set on failure")
	}
}

func TestAgentJobReconciler_FailsTerminalPendingInstanceCondition(t *testing.T) {
	s := newMemStore()
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-model-denied")
	storeJob(t, s, job)

	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-model-denied", Kind: "AgentJob"})
	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-model-denied", Kind: "AgentJob"})

	instances := listJobInstances(t, s, "default")
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}
	inst := instances[0]
	inst.Status.Phase = v1alpha1.PhasePending
	inst.Status.Conditions = []v1alpha1.Condition{{
		Type:    "Ready",
		Status:  "False",
		Reason:  "ModelAccessDenied",
		Message: "authorize model: no ModelGrant permits this agent",
	}}
	data, _ := json.Marshal(inst)
	if err := s.Update(context.Background(), "AgentInstance", inst.ObjectMeta.Namespace, inst.ObjectMeta.Name, data, inst.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update instance to terminal pending condition: %v", err)
	}

	res, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-model-denied", Kind: "AgentJob"})
	if err != nil {
		t.Fatalf("reconcile terminal pending condition: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected terminal failure without requeue")
	}

	gotJob := getJob(t, s, "default", "job-model-denied")
	if gotJob.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected job phase Failed, got %s", gotJob.Status.Phase)
	}
	if gotJob.Status.Failed != 1 {
		t.Fatalf("expected failed=1, got %d", gotJob.Status.Failed)
	}
	if gotJob.Status.Output != "authorize model: no ModelGrant permits this agent" {
		t.Fatalf("job output = %q", gotJob.Status.Output)
	}
	if condition := findCondition(gotJob.Status.Conditions, "Ready"); condition == nil || condition.Reason != "Failed" || condition.Message != gotJob.Status.Output {
		t.Fatalf("Ready condition = %#v", condition)
	}
}

func TestAgentJobReconciler_InvalidSpecFails(t *testing.T) {
	s := newMemStore()
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-invalid")
	job.Spec.AgentRef = nil
	job.Spec.AgentSpec = nil
	storeJob(t, s, job)

	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-invalid", Kind: "AgentJob"})
	res, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-invalid", Kind: "AgentJob"})
	if err != nil {
		t.Fatalf("reconcile invalid spec: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected terminal failure for invalid spec")
	}

	gotJob := getJob(t, s, "default", "job-invalid")
	if gotJob.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected job phase Failed, got %s", gotJob.Status.Phase)
	}
	if gotJob.Status.Failed != 1 {
		t.Fatalf("expected failed=1, got %d", gotJob.Status.Failed)
	}
}

func TestAgentJobReconciler_DeletionRemovesOwnedInstances(t *testing.T) {
	s := newMemStore()
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-delete")
	storeJob(t, s, job)

	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-delete", Kind: "AgentJob"})
	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-delete", Kind: "AgentJob"})

	instances := listJobInstances(t, s, "default")
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance before deletion, got %d", len(instances))
	}

	stored := getJob(t, s, "default", "job-delete")
	now := time.Now().UTC()
	stored.ObjectMeta.DeletionTimestamp = &now
	data, _ := json.Marshal(stored)
	if err := s.Update(context.Background(), "AgentJob", "default", "job-delete", data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("set deletion timestamp: %v", err)
	}

	res, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-delete", Kind: "AgentJob"})
	if err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected no requeue on deletion handling")
	}

	instances = listJobInstances(t, s, "default")
	if len(instances) != 0 {
		t.Fatalf("expected 0 instances after deletion, got %d", len(instances))
	}

	after := getJob(t, s, "default", "job-delete")
	for _, f := range after.ObjectMeta.Finalizers {
		if f == AgentJobFinalizer {
			t.Fatal("expected job finalizer removed")
		}
	}
}

func TestAgentJobReconciler_InlineAgentSpecCreatesInlineAgentAndInstance(t *testing.T) {
	s := newMemStore()
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-inline")
	job.Spec.AgentRef = nil
	job.Spec.AgentSpec = &v1alpha1.AgentSpec{
		ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
		Instructions: v1alpha1.AgentInstructions{Inline: "inline instructions"},
	}
	storeJob(t, s, job)

	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-inline", Kind: "AgentJob"})
	res, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-inline", Kind: "AgentJob"})
	if err != nil {
		t.Fatalf("reconcile inline: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after inline instance creation")
	}

	inlineAgentName := "job-inline-inline"
	rawAgent, err := s.Get(context.Background(), "Agent", "default", inlineAgentName)
	if err != nil {
		t.Fatalf("expected inline agent to exist: %v", err)
	}
	var agent v1alpha1.Agent
	if err := json.Unmarshal(rawAgent, &agent); err != nil {
		t.Fatalf("unmarshal inline agent: %v", err)
	}
	if agent.Spec.Instructions.Inline != "inline instructions" {
		t.Fatalf("expected inline instructions, got %q", agent.Spec.Instructions.Inline)
	}

	instances := listJobInstances(t, s, "default")
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}
	if instances[0].Spec.AgentRef.Name != inlineAgentName {
		t.Fatalf("expected instance agentRef=%q, got %q", inlineAgentName, instances[0].Spec.AgentRef.Name)
	}
}

func TestAgentJobReconciler_InlineAgentCreateRaceAcceptsOwnedWinner(t *testing.T) {
	ctx := context.Background()
	s := &inlineAgentCreateRaceStore{memStore: newMemStore()}
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-inline-race")
	job.Spec.AgentRef = nil
	job.Spec.AgentSpec = &v1alpha1.AgentSpec{
		ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
		Instructions: v1alpha1.AgentInstructions{Inline: "inline instructions"},
	}
	storeJob(t, s.memStore, job)

	request := Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	result, err := r.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("reconcile create race: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected requeue after inline instance creation")
	}
	if !s.traced {
		t.Fatal("inline Agent create race was not exercised")
	}

	rawAgent, err := s.Get(ctx, "Agent", "default", inlineAgentNameForJob(&job))
	if err != nil {
		t.Fatalf("get winning inline Agent: %v", err)
	}
	var winningAgent v1alpha1.Agent
	if err := json.Unmarshal(rawAgent, &winningAgent); err != nil {
		t.Fatalf("unmarshal winning inline Agent: %v", err)
	}
	if len(winningAgent.ObjectMeta.OwnerReferences) != 1 ||
		winningAgent.ObjectMeta.OwnerReferences[0].Kind != "AgentJob" ||
		winningAgent.ObjectMeta.OwnerReferences[0].Name != job.ObjectMeta.Name ||
		winningAgent.ObjectMeta.OwnerReferences[0].UID != job.ObjectMeta.UID {
		t.Fatalf("winning inline Agent owner references = %+v, want AgentJob %s/%s", winningAgent.ObjectMeta.OwnerReferences, job.ObjectMeta.Namespace, job.ObjectMeta.Name)
	}
	if instances := listJobInstances(t, s.memStore, "default"); len(instances) != 1 {
		t.Fatalf("AgentInstances after create race = %d, want 1", len(instances))
	}
}

func TestAgentJobReconciler_DeletionRemovesInlineAgentOnly(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-inline-delete")
	job.Spec.AgentRef = nil
	job.Spec.AgentSpec = &v1alpha1.AgentSpec{
		ModelRef: v1alpha1.LocalObjectReference{Name: "gpt-4"},
	}
	storeJob(t, s, job)
	if err := s.Create(ctx, "Agent", "default", "unrelated", makeResource("Agent", "default", "unrelated")); err != nil {
		t.Fatalf("create unrelated agent: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"}); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"}); err != nil {
		t.Fatalf("create inline resources: %v", err)
	}

	stored := getJob(t, s, "default", job.ObjectMeta.Name)
	now := time.Now().UTC()
	stored.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal deleting job: %v", err)
	}
	if err := s.Update(ctx, "AgentJob", "default", job.ObjectMeta.Name, data, stored.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("set deletion timestamp: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"}); err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}
	if _, err := s.Get(ctx, "Agent", "default", inlineAgentNameForJob(&job)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("inline Agent Get() error = %v, want ErrNotFound", err)
	}
	if _, err := s.Get(ctx, "Agent", "default", "unrelated"); err != nil {
		t.Fatalf("unrelated Agent was deleted: %v", err)
	}
	if instances := listJobInstances(t, s, "default"); len(instances) != 0 {
		t.Fatalf("AgentInstances after deletion = %d, want 0", len(instances))
	}
	if got := getJob(t, s, "default", job.ObjectMeta.Name); hasFinalizer(got.ObjectMeta.Finalizers, AgentJobFinalizer) {
		t.Fatal("AgentJob cleanup finalizer was not removed")
	}
}

func TestAgentJobReconciler_InlineAgentCollisionPreservesForeignAgent(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-inline-collision")
	job.Spec.AgentRef = nil
	job.Spec.AgentSpec = &v1alpha1.AgentSpec{
		ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
		Instructions: v1alpha1.AgentInstructions{Inline: "job instructions"},
	}
	storeJob(t, s, job)
	foreignAgent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      inlineAgentNameForJob(&job),
			Namespace: job.ObjectMeta.Namespace,
			UID:       "foreign-agent-uid",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "foreign-model"},
			Instructions: v1alpha1.AgentInstructions{Inline: "foreign instructions"},
		},
	}
	foreignData, err := json.Marshal(foreignAgent)
	if err != nil {
		t.Fatalf("marshal foreign Agent: %v", err)
	}
	if err := s.Create(ctx, "Agent", "default", foreignAgent.ObjectMeta.Name, foreignData); err != nil {
		t.Fatalf("create foreign Agent: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"}); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"}); err != nil {
		t.Fatalf("reject inline Agent collision: %v", err)
	}
	failed := getJob(t, s, "default", job.ObjectMeta.Name)
	if failed.Status.Phase != v1alpha1.PhaseFailed || failed.Status.Failed != 1 {
		t.Fatalf("job status = %+v, want terminal collision failure", failed.Status)
	}
	var readyReason string
	for _, condition := range failed.Status.Conditions {
		if condition.Type == "Ready" {
			readyReason = condition.Reason
		}
	}
	if readyReason != "InvalidSpec" {
		t.Fatalf("Ready condition reason = %q, want InvalidSpec", readyReason)
	}
	if instances := listJobInstances(t, s, "default"); len(instances) != 0 {
		t.Fatalf("AgentInstances after collision = %d, want 0", len(instances))
	}

	now := time.Now().UTC()
	failed.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(failed)
	if err != nil {
		t.Fatalf("marshal deleting job: %v", err)
	}
	if err := s.Update(ctx, "AgentJob", "default", job.ObjectMeta.Name, data, failed.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("set deletion timestamp: %v", err)
	}
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"}); err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}
	raw, err := s.Get(ctx, "Agent", "default", foreignAgent.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("foreign Agent was deleted: %v", err)
	}
	var preserved v1alpha1.Agent
	if err := json.Unmarshal(raw, &preserved); err != nil {
		t.Fatalf("unmarshal preserved Agent: %v", err)
	}
	if preserved.ObjectMeta.UID != foreignAgent.ObjectMeta.UID || preserved.Spec.Instructions.Inline != "foreign instructions" {
		t.Fatalf("foreign Agent changed: %+v", preserved)
	}
	if got := getJob(t, s, "default", job.ObjectMeta.Name); hasFinalizer(got.ObjectMeta.Finalizers, AgentJobFinalizer) {
		t.Fatal("AgentJob cleanup finalizer was not removed")
	}
}

func TestAgentJobReconciler_CompletionPolicyAnyFailsOnFirstFailure(t *testing.T) {
	s := newMemStore()
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-any")
	limit := int32(5)
	job.Spec.BackoffLimit = &limit
	job.Spec.CompletionPolicy = v1alpha1.CompletionPolicyAny
	storeJob(t, s, job)

	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-any", Kind: "AgentJob"})
	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-any", Kind: "AgentJob"})

	instances := listJobInstances(t, s, "default")
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}
	inst := instances[0]
	inst.Status.Phase = v1alpha1.PhaseFailed
	data, _ := json.Marshal(inst)
	if err := s.Update(context.Background(), "AgentInstance", inst.ObjectMeta.Namespace, inst.ObjectMeta.Name, data, inst.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	res, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-any", Kind: "AgentJob"})
	if err != nil {
		t.Fatalf("reconcile terminal any-failure: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatal("expected terminal failure for CompletionPolicyAny")
	}

	gotJob := getJob(t, s, "default", "job-any")
	if gotJob.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected job phase Failed, got %s", gotJob.Status.Phase)
	}
	instances = listJobInstances(t, s, "default")
	if len(instances) != 1 {
		t.Fatalf("expected no retry instance for CompletionPolicyAny, got %d instances", len(instances))
	}
}

func TestAgentJobReconciler_CreateJobInstanceError(t *testing.T) {
	createErr := errors.New("create failed")
	s := &createFaultStore{memStore: newMemStore(), createErr: createErr}
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-create-error")
	storeJob(t, s.memStore, job)

	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-create-error", Kind: "AgentJob"})
	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-create-error", Kind: "AgentJob"})
	if err == nil {
		t.Fatal("expected create job instance error")
	}
	if !errors.Is(err, createErr) || !strings.Contains(err.Error(), "create job instance") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgentJobReconciler_HandlesMissingAndMalformedJobs(t *testing.T) {
	ctx := context.Background()
	r := NewAgentJobReconciler(newMemStore(), zaptest.NewLogger(t))
	if result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "missing", Kind: "AgentJob"}); err != nil || result != (Result{}) {
		t.Fatalf("missing job result = %+v, error = %v", result, err)
	}

	base := newMemStore()
	if err := base.Create(ctx, "AgentJob", "default", "malformed", []byte("{")); err != nil {
		t.Fatalf("store malformed job: %v", err)
	}
	r = NewAgentJobReconciler(base, zaptest.NewLogger(t))
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "malformed", Kind: "AgentJob"}); err == nil || !strings.Contains(err.Error(), "unmarshal agentjob") {
		t.Fatalf("malformed job error = %v", err)
	}

	wantErr := errors.New("read failed")
	r = NewAgentJobReconciler(&agentJobBoundaryFaultStore{memStore: base, getKind: "AgentJob", getErr: wantErr}, zaptest.NewLogger(t))
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "malformed", Kind: "AgentJob"}); !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "get agentjob") {
		t.Fatalf("job read error = %v", err)
	}
}

func TestAgentJobReconciler_SurfacesLifecyclePersistenceFailures(t *testing.T) {
	wantErr := errors.New("dependency failed")
	for _, tc := range []struct {
		name        string
		prepare     func(*testing.T, *memStore, *v1alpha1.AgentJob)
		fault       func(*agentJobBoundaryFaultStore)
		wantMessage string
	}{
		{
			name: "add finalizer",
			fault: func(s *agentJobBoundaryFaultStore) {
				s.updateKind, s.updateErr = "AgentJob", wantErr
			},
			wantMessage: "add finalizer",
		},
		{
			name: "list instances",
			prepare: func(_ *testing.T, _ *memStore, job *v1alpha1.AgentJob) {
				job.ObjectMeta.Finalizers = []string{AgentJobFinalizer}
			},
			fault: func(s *agentJobBoundaryFaultStore) {
				s.listKind, s.listErr = agentInstanceKind, wantErr
			},
			wantMessage: "list owned instances",
		},
		{
			name: "complete job",
			prepare: func(t *testing.T, base *memStore, job *v1alpha1.AgentJob) {
				job.ObjectMeta.Finalizers = []string{AgentJobFinalizer}
				instance := makeInstance(job.ObjectMeta.Namespace, job.ObjectMeta.Name+"-r1", job.Spec.AgentRef.Name, v1alpha1.PhaseReady)
				instance.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{Kind: "AgentJob", Name: job.ObjectMeta.Name, UID: job.ObjectMeta.UID}}
				data, err := json.Marshal(instance)
				if err != nil {
					t.Fatalf("marshal ready instance: %v", err)
				}
				if err := base.Create(context.Background(), agentInstanceKind, job.ObjectMeta.Namespace, instance.ObjectMeta.Name, data); err != nil {
					t.Fatalf("store ready instance: %v", err)
				}
			},
			fault: func(s *agentJobBoundaryFaultStore) {
				s.updateKind, s.updateErr = "AgentJob", wantErr
			},
			wantMessage: "update completed job",
		},
		{
			name: "persist invalid spec",
			prepare: func(_ *testing.T, _ *memStore, job *v1alpha1.AgentJob) {
				job.ObjectMeta.Finalizers = []string{AgentJobFinalizer}
				job.Spec.AgentRef = nil
			},
			fault: func(s *agentJobBoundaryFaultStore) {
				s.updateKind, s.updateErr = "AgentJob", wantErr
			},
			wantMessage: "update invalid-spec job",
		},
		{
			name: "persist pending dispatch",
			prepare: func(_ *testing.T, _ *memStore, job *v1alpha1.AgentJob) {
				job.ObjectMeta.Finalizers = []string{AgentJobFinalizer}
			},
			fault: func(s *agentJobBoundaryFaultStore) {
				s.updateKind, s.updateErr = "AgentJob", wantErr
			},
			wantMessage: "update pending job",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newMemStore()
			job := makeAgentJob("default", "job-"+strings.ReplaceAll(tc.name, " ", "-"))
			if tc.prepare != nil {
				tc.prepare(t, base, &job)
			}
			storeJob(t, base, job)
			faults := &agentJobBoundaryFaultStore{memStore: base}
			tc.fault(faults)
			r := NewAgentJobReconciler(faults, zaptest.NewLogger(t))
			_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"})
			if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("error = %v, want %q wrapping dependency error", err, tc.wantMessage)
			}
		})
	}
}

func TestAgentJobReconciler_DeletionSurfacesDependencyFailures(t *testing.T) {
	wantErr := errors.New("cleanup failed")
	for _, tc := range []struct {
		name        string
		prepare     func(*testing.T, *memStore, *v1alpha1.AgentJob)
		fault       func(*agentJobBoundaryFaultStore)
		wantMessage string
	}{
		{
			name: "list instances",
			fault: func(s *agentJobBoundaryFaultStore) {
				s.listKind, s.listErr = agentInstanceKind, wantErr
			},
			wantMessage: "list instances for deletion",
		},
		{
			name: "delete instance",
			prepare: func(t *testing.T, base *memStore, job *v1alpha1.AgentJob) {
				instance := makeInstance(job.ObjectMeta.Namespace, job.ObjectMeta.Name+"-r1", job.Spec.AgentRef.Name, v1alpha1.PhaseRunning)
				instance.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{Kind: "AgentJob", Name: job.ObjectMeta.Name, UID: job.ObjectMeta.UID}}
				data, err := json.Marshal(instance)
				if err != nil {
					t.Fatalf("marshal owned instance: %v", err)
				}
				if err := base.Create(context.Background(), agentInstanceKind, job.ObjectMeta.Namespace, instance.ObjectMeta.Name, data); err != nil {
					t.Fatalf("store owned instance: %v", err)
				}
			},
			fault: func(s *agentJobBoundaryFaultStore) {
				s.deleteKind, s.deleteErr = agentInstanceKind, wantErr
			},
			wantMessage: "delete job instance",
		},
		{
			name: "get inline agent",
			prepare: func(_ *testing.T, _ *memStore, job *v1alpha1.AgentJob) {
				job.Spec.AgentRef = nil
				job.Spec.AgentSpec = &v1alpha1.AgentSpec{}
			},
			fault: func(s *agentJobBoundaryFaultStore) {
				s.getKind, s.getErr = "Agent", wantErr
			},
			wantMessage: "get inline agent",
		},
		{
			name: "remove finalizer",
			fault: func(s *agentJobBoundaryFaultStore) {
				s.updateKind, s.updateErr = "AgentJob", wantErr
			},
			wantMessage: "remove finalizer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newMemStore()
			job := makeAgentJob("default", "job-delete-"+strings.ReplaceAll(tc.name, " ", "-"))
			job.ObjectMeta.Finalizers = []string{AgentJobFinalizer}
			now := time.Now().UTC()
			job.ObjectMeta.DeletionTimestamp = &now
			if tc.prepare != nil {
				tc.prepare(t, base, &job)
			}
			storeJob(t, base, job)
			faults := &agentJobBoundaryFaultStore{memStore: base}
			tc.fault(faults)
			r := NewAgentJobReconciler(faults, zaptest.NewLogger(t))
			_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"})
			if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("error = %v, want %q wrapping cleanup error", err, tc.wantMessage)
			}
		})
	}
}

func TestAgentJobReconciler_InlineAgentFailuresBecomeInvalidSpec(t *testing.T) {
	wantErr := errors.New("inline agent dependency failed")
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, *memStore, *agentJobBoundaryFaultStore, *v1alpha1.AgentJob)
	}{
		{
			name: "read failure",
			prepare: func(_ *testing.T, _ *memStore, faults *agentJobBoundaryFaultStore, _ *v1alpha1.AgentJob) {
				faults.getKind, faults.getErr = "Agent", wantErr
			},
		},
		{
			name: "create failure",
			prepare: func(_ *testing.T, _ *memStore, faults *agentJobBoundaryFaultStore, _ *v1alpha1.AgentJob) {
				faults.createKind, faults.createErr = "Agent", wantErr
			},
		},
		{
			name: "malformed existing agent",
			prepare: func(t *testing.T, base *memStore, _ *agentJobBoundaryFaultStore, job *v1alpha1.AgentJob) {
				if err := base.Create(context.Background(), "Agent", job.ObjectMeta.Namespace, inlineAgentNameForJob(job), []byte("{")); err != nil {
					t.Fatalf("store malformed inline agent: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newMemStore()
			faults := &agentJobBoundaryFaultStore{memStore: base}
			job := makeAgentJob("default", "job-inline-"+strings.ReplaceAll(tc.name, " ", "-"))
			job.ObjectMeta.Finalizers = []string{AgentJobFinalizer}
			job.Spec.AgentRef = nil
			job.Spec.AgentSpec = &v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "model"}}
			tc.prepare(t, base, faults, &job)
			storeJob(t, base, job)
			r := NewAgentJobReconciler(faults, zaptest.NewLogger(t))
			if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: job.ObjectMeta.Name, Kind: "AgentJob"}); err != nil {
				t.Fatalf("reconcile inline failure: %v", err)
			}
			stored := getJob(t, base, "default", job.ObjectMeta.Name)
			if stored.Status.Phase != v1alpha1.PhaseFailed || stored.Status.Failed != 1 {
				t.Fatalf("status = %+v, want terminal invalid-spec failure", stored.Status)
			}
		})
	}
}

func TestAgentJobReconciler_UpdateRunningJobError(t *testing.T) {
	updateErr := errors.New("write failed")
	s := &faultStore{memStore: newMemStore()}
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-running-update-error")
	storeJob(t, s.memStore, job)

	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-running-update-error", Kind: "AgentJob"})
	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-running-update-error", Kind: "AgentJob"})

	s.updateErr = updateErr
	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-running-update-error", Kind: "AgentJob"})
	if err == nil {
		t.Fatal("expected update running job error")
	}
	if !errors.Is(err, updateErr) || !strings.Contains(err.Error(), "update running job") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgentJobReconciler_UpdateFailedJobError(t *testing.T) {
	updateErr := errors.New("write failed")
	s := &faultStore{memStore: newMemStore()}
	r := NewAgentJobReconciler(s, zaptest.NewLogger(t))
	job := makeAgentJob("default", "job-failed-update-error")
	limit := int32(0)
	job.Spec.BackoffLimit = &limit
	storeJob(t, s.memStore, job)

	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-failed-update-error", Kind: "AgentJob"})
	_, _ = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-failed-update-error", Kind: "AgentJob"})

	instances := listJobInstances(t, s.memStore, "default")
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}
	inst := instances[0]
	inst.Status.Phase = v1alpha1.PhaseFailed
	data, _ := json.Marshal(inst)
	if err := s.memStore.Update(context.Background(), "AgentInstance", inst.ObjectMeta.Namespace, inst.ObjectMeta.Name, data, inst.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	s.updateErr = updateErr
	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "job-failed-update-error", Kind: "AgentJob"})
	if err == nil {
		t.Fatal("expected update failed job error")
	}
	if !errors.Is(err, updateErr) || !strings.Contains(err.Error(), "update failed job") {
		t.Fatalf("unexpected error: %v", err)
	}
}
