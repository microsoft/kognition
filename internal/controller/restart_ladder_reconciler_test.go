// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/store"
	"go.uber.org/zap/zaptest"
)

type restartLadderConflictStore struct {
	*memStore
	conflicted             bool
	concurrentLadderStep   *int32
	concurrentLadderAction *v1alpha1.RestartAction
}

func TestApplyConditionChangePreservesOtherConditions(t *testing.T) {
	foreign := v1alpha1.Condition{Type: "Foreign", Status: "True"}
	ready := v1alpha1.Condition{Type: "Ready", Status: "False"}
	conditions := []v1alpha1.Condition{foreign, ready}
	for _, test := range []struct {
		name          string
		conditionType string
		desired       *v1alpha1.Condition
		wantLen       int
	}{
		{name: "add", conditionType: "New", desired: &v1alpha1.Condition{Type: "New", Status: "True"}, wantLen: 3},
		{name: "replace", conditionType: "Ready", desired: &v1alpha1.Condition{Type: "Ready", Status: "True"}, wantLen: 2},
		{name: "remove", conditionType: "Ready", wantLen: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := applyConditionChange(conditions, test.conditionType, test.desired)
			if len(got) != test.wantLen || got[0] != foreign {
				t.Fatalf("merged conditions = %#v, want %d entries and preserved foreign condition", got, test.wantLen)
			}
			if test.desired != nil && got[len(got)-1] != *test.desired {
				t.Fatalf("desired condition lost: %#v", got)
			}
			if conditions[0] != foreign || conditions[1] != ready {
				t.Fatalf("input conditions were mutated: %#v", conditions)
			}
		})
	}
	got := applyConditionChange(nil, ready.Type, &ready)
	if len(got) != 1 || got[0] != ready {
		t.Fatalf("condition not added to empty input: %#v", got)
	}
}

type restartLadderPersistentConflictStore struct {
	*memStore
	afterConflictErr error
	afterConflictRaw []byte
	instanceGets     int
	updates          int
}

func (s *restartLadderPersistentConflictStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == agentInstanceKind {
		s.instanceGets++
		if s.instanceGets > 1 {
			if s.afterConflictErr != nil {
				return nil, s.afterConflictErr
			}
			if s.afterConflictRaw != nil {
				return s.afterConflictRaw, nil
			}
		}
	}
	return s.memStore.Get(ctx, kind, namespace, name)
}

func (s *restartLadderPersistentConflictStore) Update(context.Context, string, string, string, []byte, int64) error {
	s.updates++
	return fmt.Errorf("wrapped update conflict: %w", store.ErrConflict)
}

func (s *restartLadderConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == agentInstanceKind && !s.conflicted {
		s.conflicted = true
		raw, err := s.memStore.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var latest v1alpha1.AgentInstance
		if err := json.Unmarshal(raw, &latest); err != nil {
			return err
		}
		latest.ObjectMeta.Finalizers = append(latest.ObjectMeta.Finalizers, "example.com/foreign")
		latest.Status.TokenUsage = &v1alpha1.TokenUsage{TotalTokens: 42}
		ready := true
		lastProbeTime := "2026-07-30T20:00:00Z"
		latest.Status.ProbeStatus.Ready = &ready
		latest.Status.ProbeStatus.ConsecutiveReadinessSuccesses = 2
		latest.Status.ProbeStatus.LastProbeTime = &lastProbeTime
		if s.concurrentLadderStep != nil {
			latest.Status.ProbeStatus.LadderStep = *s.concurrentLadderStep
		}
		if s.concurrentLadderAction != nil {
			latest.Status.ProbeStatus.LastLadderAction = s.concurrentLadderAction
		}
		latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{
			Type: string(lifecycle.ConditionReady), Status: string(lifecycle.ConditionTrue), Reason: "ConcurrentRuntime",
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
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func makeAgentWithLadder(ns, name string, probes *v1alpha1.AgentProbeSpec, ladder *v1alpha1.RestartLadder) v1alpha1.Agent {
	a := makeAgent(ns, name, 1)
	a.Spec.Probes = probes
	a.Spec.RestartLadder = ladder
	return a
}

func TestRestartLadder_NoLadder(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{}
	r := NewRestartLadderReconciler(s, d, zaptest.NewLogger(t))

	storeAgent(t, s, makeAgent("ns", "agent1", 1))
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	storeInstance(t, s, inst)

	res, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue, got %v", res.RequeueAfter)
	}
}

func TestRestartLadder_ThresholdNotBreached(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{}
	r := NewRestartLadderReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3},
	}
	ladder := &v1alpha1.RestartLadder{
		Steps: []v1alpha1.RestartLadderStep{
			{Action: v1alpha1.RestartActionRestart, StabilisationSeconds: 30},
		},
	}
	storeAgent(t, s, makeAgentWithLadder("ns", "agent1", probes, ladder))

	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	inst.Status.ProbeStatus = &v1alpha1.ProbeStatus{ConsecutiveHealthFailures: 2}
	storeInstance(t, s, inst)

	res, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue when not breached, got %v", res.RequeueAfter)
	}
	if d.called != 0 {
		t.Fatal("dispatcher should not be called when threshold not breached")
	}
}

func TestRestartLadder_RestartAction(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{sessionID: "new-session"}
	r := NewRestartLadderReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3},
	}
	ladder := &v1alpha1.RestartLadder{
		Steps: []v1alpha1.RestartLadderStep{
			{Action: v1alpha1.RestartActionRestart, StabilisationSeconds: 30},
		},
	}
	storeAgent(t, s, makeAgentWithLadder("ns", "agent1", probes, ladder))

	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	inst.Status.ProbeStatus = &v1alpha1.ProbeStatus{ConsecutiveHealthFailures: 3}
	storeInstance(t, s, inst)

	res, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 30*1e9 { // 30s
		t.Fatalf("expected 30s stabilisation, got %v", res.RequeueAfter)
	}
	if d.called == 0 {
		t.Fatal("expected dispatcher.Dispatch to be called")
	}

	updated := getInstance(t, s, "ns", "inst1")
	if updated.Status.RestartCount != 1 {
		t.Fatalf("expected RestartCount=1, got %d", updated.Status.RestartCount)
	}
	if updated.Status.SessionID != "new-session" {
		t.Fatalf("expected session updated, got %s", updated.Status.SessionID)
	}
}

func TestRestartLadder_RestartConflictPreservesConcurrentSharedState(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	s := &restartLadderConflictStore{memStore: baseStore}
	d := &mockDispatcher{sessionID: "new-session"}
	r := NewRestartLadderReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3},
	}
	ladder := &v1alpha1.RestartLadder{
		Steps: []v1alpha1.RestartLadderStep{{Action: v1alpha1.RestartActionRestart}},
	}
	storeAgent(t, baseStore, makeAgentWithLadder("ns", "agent1", probes, ladder))
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	inst.Status.ProbeStatus = &v1alpha1.ProbeStatus{
		ConsecutiveHealthFailures:    3,
		ConsecutiveReadinessFailures: 3,
		ConsecutiveReasoningFailures: 3,
	}
	storeInstance(t, baseStore, inst)

	if _, err := r.Reconcile(ctx, Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind}); err != nil {
		t.Fatalf("reconcile through persistence conflict: %v", err)
	}
	if d.called != 1 {
		t.Fatalf("dispatch calls = %d, want 1", d.called)
	}

	updated := getInstance(t, baseStore, "ns", "inst1")
	if updated.Status.SessionID != "new-session" || updated.Status.RestartCount != 1 {
		t.Fatalf("restart state not persisted: sessionID=%q restartCount=%d", updated.Status.SessionID, updated.Status.RestartCount)
	}
	if updated.Status.TokenUsage == nil || updated.Status.TokenUsage.TotalTokens != 42 {
		t.Fatalf("concurrent token usage was lost: %#v", updated.Status.TokenUsage)
	}
	if updated.Status.ProbeStatus == nil || updated.Status.ProbeStatus.Ready == nil || !*updated.Status.ProbeStatus.Ready ||
		updated.Status.ProbeStatus.ConsecutiveReadinessSuccesses != 2 || updated.Status.ProbeStatus.LastProbeTime == nil {
		t.Fatalf("concurrent probe result was lost: %#v", updated.Status.ProbeStatus)
	}
	if updated.Status.ProbeStatus.ConsecutiveHealthFailures != 0 ||
		updated.Status.ProbeStatus.ConsecutiveReadinessFailures != 0 ||
		updated.Status.ProbeStatus.ConsecutiveReasoningFailures != 0 {
		t.Fatalf("restart probe reset was not persisted: %#v", updated.Status.ProbeStatus)
	}
	if condition := findCondition(updated.Status.Conditions, string(lifecycle.ConditionReady)); condition == nil || condition.Reason != "ConcurrentRuntime" {
		t.Fatalf("concurrent Ready condition was lost: %#v", condition)
	}
	if !hasFinalizer(updated.ObjectMeta.Finalizers, "example.com/foreign") {
		t.Fatal("concurrent foreign finalizer was lost")
	}
}

func TestRestartLadder_EscalationConflictPreservesHigherConcurrentStep(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	concurrentStep := int32(2)
	concurrentAction := v1alpha1.RestartActionAlert
	s := &restartLadderConflictStore{
		memStore:               baseStore,
		concurrentLadderStep:   &concurrentStep,
		concurrentLadderAction: &concurrentAction,
	}
	d := &mockDispatcher{sessionID: "new-session"}
	r := NewRestartLadderReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3},
	}
	ladder := &v1alpha1.RestartLadder{Steps: []v1alpha1.RestartLadderStep{
		{Action: v1alpha1.RestartActionRestart, MaxAttempts: 1},
		{Action: v1alpha1.RestartActionRestart, MaxAttempts: 1},
		{Action: v1alpha1.RestartActionAlert, MaxAttempts: 1},
	}}
	storeAgent(t, baseStore, makeAgentWithLadder("ns", "agent1", probes, ladder))
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	inst.Status.ProbeStatus = &v1alpha1.ProbeStatus{
		ConsecutiveHealthFailures: 3,
		LadderAttempts:            1,
	}
	storeInstance(t, baseStore, inst)

	if _, err := r.Reconcile(ctx, Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind}); err != nil {
		t.Fatalf("reconcile through escalation conflict: %v", err)
	}
	if d.called != 1 {
		t.Fatalf("dispatch calls = %d, want 1", d.called)
	}
	updated := getInstance(t, baseStore, "ns", "inst1")
	if updated.Status.ProbeStatus == nil || updated.Status.ProbeStatus.LadderStep != concurrentStep {
		t.Fatalf("ladder step = %#v, want concurrent step %d", updated.Status.ProbeStatus, concurrentStep)
	}
	if updated.Status.ProbeStatus.LastLadderAction == nil || *updated.Status.ProbeStatus.LastLadderAction != concurrentAction {
		t.Fatalf("last ladder action = %#v, want concurrent action %q", updated.Status.ProbeStatus.LastLadderAction, concurrentAction)
	}
}

func TestRestartLadder_RecoveryConflictPreservesConcurrentEscalation(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	concurrentStep := int32(2)
	concurrentAction := v1alpha1.RestartActionAlert
	s := &restartLadderConflictStore{
		memStore:               baseStore,
		concurrentLadderStep:   &concurrentStep,
		concurrentLadderAction: &concurrentAction,
	}
	r := NewRestartLadderReconciler(s, &mockDispatcher{}, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3},
	}
	ladder := &v1alpha1.RestartLadder{Steps: []v1alpha1.RestartLadderStep{
		{Action: v1alpha1.RestartActionRestart, MaxAttempts: 1},
		{Action: v1alpha1.RestartActionRestart, MaxAttempts: 1},
		{Action: v1alpha1.RestartActionAlert, MaxAttempts: 1},
	}}
	storeAgent(t, baseStore, makeAgentWithLadder("ns", "agent1", probes, ladder))
	lastAction := v1alpha1.RestartActionRestart
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	inst.Status.ProbeStatus = &v1alpha1.ProbeStatus{
		LadderStep:       1,
		LadderAttempts:   1,
		LastLadderAction: &lastAction,
	}
	storeInstance(t, baseStore, inst)

	if _, err := r.Reconcile(ctx, Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind}); err != nil {
		t.Fatalf("reconcile recovery through escalation conflict: %v", err)
	}
	updated := getInstance(t, baseStore, "ns", "inst1")
	if updated.Status.ProbeStatus == nil || updated.Status.ProbeStatus.LadderStep != concurrentStep {
		t.Fatalf("ladder step = %#v, want concurrent step %d", updated.Status.ProbeStatus, concurrentStep)
	}
	if updated.Status.ProbeStatus.LastLadderAction == nil || *updated.Status.ProbeStatus.LastLadderAction != concurrentAction {
		t.Fatalf("last ladder action = %#v, want concurrent action %q", updated.Status.ProbeStatus.LastLadderAction, concurrentAction)
	}
}

func TestRestartLadder_QuarantineAction(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{}
	r := NewRestartLadderReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3},
	}
	ladder := &v1alpha1.RestartLadder{
		Steps: []v1alpha1.RestartLadderStep{
			{Action: v1alpha1.RestartActionQuarantine, StabilisationSeconds: 60},
		},
	}
	storeAgent(t, s, makeAgentWithLadder("ns", "agent1", probes, ladder))

	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	inst.Status.ProbeStatus = &v1alpha1.ProbeStatus{ConsecutiveHealthFailures: 3}
	storeInstance(t, s, inst)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := getInstance(t, s, "ns", "inst1")
	if updated.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("expected PhaseDegraded, got %s", updated.Status.Phase)
	}
	cond := findCondition(updated.Status.Conditions, string(lifecycle.ConditionQuarantined))
	if cond == nil || cond.Status != string(lifecycle.ConditionTrue) {
		t.Fatal("expected Quarantined=True condition")
	}
}

func TestRestartLadder_DegradeModelAction(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{}
	r := NewRestartLadderReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3},
	}
	ladder := &v1alpha1.RestartLadder{
		Steps: []v1alpha1.RestartLadderStep{
			{Action: v1alpha1.RestartActionDegradeModel, StabilisationSeconds: 45},
		},
	}
	storeAgent(t, s, makeAgentWithLadder("ns", "agent1", probes, ladder))

	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	inst.Status.ProbeStatus = &v1alpha1.ProbeStatus{ConsecutiveHealthFailures: 3}
	storeInstance(t, s, inst)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 45*1e9 {
		t.Fatalf("expected 45s stabilisation, got %v", result.RequeueAfter)
	}
	if d.called != 0 {
		t.Fatal("degrading the model should not redispatch the instance")
	}

	updated := getInstance(t, s, "ns", "inst1")
	if updated.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("expected PhaseDegraded, got %s", updated.Status.Phase)
	}
	if updated.Status.ProbeStatus.LadderAttempts != 1 {
		t.Fatalf("expected LadderAttempts=1, got %d", updated.Status.ProbeStatus.LadderAttempts)
	}
	if updated.Status.ProbeStatus.LastLadderAction == nil ||
		*updated.Status.ProbeStatus.LastLadderAction != v1alpha1.RestartActionDegradeModel {
		t.Fatalf("expected LastLadderAction=DegradeModel, got %v", updated.Status.ProbeStatus.LastLadderAction)
	}
	condition := findCondition(updated.Status.Conditions, string(lifecycle.ConditionProbeHealthy))
	if condition == nil || condition.Status != string(lifecycle.ConditionFalse) || condition.Reason != "ModelDegraded" {
		t.Fatalf("expected ProbeHealthy=False/ModelDegraded condition, got %#v", condition)
	}
}

func TestRestartLadder_AlertAction(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{}
	r := NewRestartLadderReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3},
	}
	ladder := &v1alpha1.RestartLadder{
		Steps: []v1alpha1.RestartLadderStep{
			{Action: v1alpha1.RestartActionAlert, StabilisationSeconds: 10},
		},
	}
	storeAgent(t, s, makeAgentWithLadder("ns", "agent1", probes, ladder))

	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	inst.Status.ProbeStatus = &v1alpha1.ProbeStatus{ConsecutiveHealthFailures: 3}
	storeInstance(t, s, inst)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := s.events["AgentInstance/ns/inst1"]
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Reason != "RestartLadderAlert" {
		t.Fatalf("expected RestartLadderAlert reason, got %s", events[0].Reason)
	}
}

func TestRestartLadder_RollbackAction(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{sessionID: "rolled-back"}
	r := NewRestartLadderReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3},
	}
	ladder := &v1alpha1.RestartLadder{
		Steps: []v1alpha1.RestartLadderStep{
			{Action: v1alpha1.RestartActionRollbackRevision, StabilisationSeconds: 60},
		},
	}
	storeAgent(t, s, makeAgentWithLadder("ns", "agent1", probes, ladder))

	// Add revisions so rollback can find a previous one.
	s.revisions["Agent/ns/agent1"] = []store.Revision{
		{Revision: 1, Spec: []byte(`{}`), CreatedAt: "2024-01-01T00:00:00Z"},
		{Revision: 2, Spec: []byte(`{}`), CreatedAt: "2024-01-02T00:00:00Z"},
	}

	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	inst.Status.ProbeStatus = &v1alpha1.ProbeStatus{ConsecutiveHealthFailures: 3}
	storeInstance(t, s, inst)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.called == 0 {
		t.Fatal("expected dispatch for rollback")
	}

	updated := getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus.RolledBackToRevision == nil {
		t.Fatal("expected RolledBackToRevision to be set")
	}
	if *updated.Status.ProbeStatus.RolledBackToRevision != 1 {
		t.Fatalf("expected RolledBackToRevision=1, got %d", *updated.Status.ProbeStatus.RolledBackToRevision)
	}
}

func TestRestartLadder_RecoveryResetsLadder(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{}
	r := NewRestartLadderReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3},
	}
	ladder := &v1alpha1.RestartLadder{
		Steps: []v1alpha1.RestartLadderStep{
			{Action: v1alpha1.RestartActionRestart},
		},
	}
	storeAgent(t, s, makeAgentWithLadder("ns", "agent1", probes, ladder))

	// Instance that was mid-ladder but has now recovered (failures below threshold).
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseDegraded)
	action := v1alpha1.RestartActionRestart
	inst.Status.ProbeStatus = &v1alpha1.ProbeStatus{
		ConsecutiveHealthFailures: 1, // below threshold of 3
		LadderStep:                1,
		LadderAttempts:            1,
		LastLadderAction:          &action,
	}
	storeInstance(t, s, inst)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus.LadderStep != 0 {
		t.Fatalf("expected LadderStep=0, got %d", updated.Status.ProbeStatus.LadderStep)
	}
	if updated.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("expected PhaseRunning after recovery, got %s", updated.Status.Phase)
	}
}

func TestRestartLadder_RecoveryAfterFirstStepResetsLadder(t *testing.T) {
	s := newMemStore()
	r := NewRestartLadderReconciler(s, &mockDispatcher{}, zaptest.NewLogger(t))
	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3},
	}
	ladder := &v1alpha1.RestartLadder{
		Steps: []v1alpha1.RestartLadderStep{{Action: v1alpha1.RestartActionQuarantine}},
	}
	storeAgent(t, s, makeAgentWithLadder("ns", "agent1", probes, ladder))

	action := v1alpha1.RestartActionQuarantine
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseDegraded)
	inst.Status.ProbeStatus = &v1alpha1.ProbeStatus{
		ConsecutiveHealthFailures: 1,
		LadderAttempts:            1,
		LastLadderAction:          &action,
	}
	inst.Status.Conditions = []v1alpha1.Condition{{
		Type: string(lifecycle.ConditionQuarantined), Status: string(lifecycle.ConditionTrue), Reason: "QuarantinedByLadder",
	}}
	storeInstance(t, s, inst)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind}); err != nil {
		t.Fatalf("reconcile recovered instance: %v", err)
	}

	updated := getInstance(t, s, "ns", "inst1")
	if updated.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("phase = %s, want Running", updated.Status.Phase)
	}
	if updated.Status.ProbeStatus.LadderAttempts != 0 || updated.Status.ProbeStatus.LastLadderAction != nil {
		t.Fatalf("ladder state was not reset: %#v", updated.Status.ProbeStatus)
	}
	if condition := findCondition(updated.Status.Conditions, string(lifecycle.ConditionQuarantined)); condition != nil {
		t.Fatalf("quarantine condition was not removed: %#v", condition)
	}
}

func TestRestartLadder_ReconcileBoundaries(t *testing.T) {
	t.Run("wrapped instance not found", func(t *testing.T) {
		r := NewRestartLadderReconciler(&faultStore{
			memStore: newMemStore(),
			getErr:   fmt.Errorf("wrapped missing instance: %w", store.ErrNotFound),
		}, &mockDispatcher{}, zaptest.NewLogger(t))
		result, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "missing"})
		if err != nil || result.Requeue || result.RequeueAfter != 0 {
			t.Fatalf("result = %+v, error = %v; want no-op", result, err)
		}
	})

	t.Run("instance read failure", func(t *testing.T) {
		wantErr := errors.New("instance store unavailable")
		r := NewRestartLadderReconciler(&faultStore{memStore: newMemStore(), getErr: wantErr}, &mockDispatcher{}, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst"})
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "get agent instance") {
			t.Fatalf("error = %v, want wrapped instance read failure", err)
		}
	})

	t.Run("malformed instance", func(t *testing.T) {
		memory := newMemStore()
		if err := memory.Create(context.Background(), agentInstanceKind, "ns", "inst", []byte("{")); err != nil {
			t.Fatalf("store malformed instance: %v", err)
		}
		r := NewRestartLadderReconciler(memory, &mockDispatcher{}, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal agent instance") {
			t.Fatalf("error = %v, want malformed instance failure", err)
		}
	})

	for _, tc := range []struct {
		name     string
		phase    v1alpha1.Phase
		agentRef string
	}{
		{name: "inactive instance", phase: v1alpha1.PhasePending, agentRef: "agent1"},
		{name: "missing agent reference", phase: v1alpha1.PhaseRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := newMemStore()
			storeInstance(t, memory, makeInstance("ns", "inst", tc.agentRef, tc.phase))
			r := NewRestartLadderReconciler(memory, &mockDispatcher{}, zaptest.NewLogger(t))
			result, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst"})
			if err != nil || result.Requeue || result.RequeueAfter != 0 {
				t.Fatalf("result = %+v, error = %v; want no-op", result, err)
			}
		})
	}

	t.Run("parent agent unavailable", func(t *testing.T) {
		memory := newMemStore()
		storeInstance(t, memory, makeInstance("ns", "inst", "missing", v1alpha1.PhaseRunning))
		r := NewRestartLadderReconcilerWithConfig(memory, &mockDispatcher{}, zaptest.NewLogger(t), RestartLadderReconcilerConfig{
			DefaultStabilisation: 17 * time.Second,
		})
		result, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst"})
		if err != nil || result.RequeueAfter != 17*time.Second {
			t.Fatalf("result = %+v, error = %v; want 17s requeue", result, err)
		}
	})

	t.Run("malformed parent agent", func(t *testing.T) {
		memory := newMemStore()
		storeInstance(t, memory, makeInstance("ns", "inst", "agent1", v1alpha1.PhaseRunning))
		if err := memory.Create(context.Background(), "Agent", "ns", "agent1", []byte("{")); err != nil {
			t.Fatalf("store malformed agent: %v", err)
		}
		r := NewRestartLadderReconciler(memory, &mockDispatcher{}, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal agent") {
			t.Fatalf("error = %v, want malformed agent failure", err)
		}
	})

	t.Run("recovery update failure", func(t *testing.T) {
		memory := newMemStore()
		probes := &v1alpha1.AgentProbeSpec{HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3}}
		ladder := &v1alpha1.RestartLadder{Steps: []v1alpha1.RestartLadderStep{{Action: v1alpha1.RestartActionRestart}}}
		storeAgent(t, memory, makeAgentWithLadder("ns", "agent1", probes, ladder))
		instance := makeInstance("ns", "inst", "agent1", v1alpha1.PhaseDegraded)
		instance.Status.ProbeStatus = &v1alpha1.ProbeStatus{ConsecutiveHealthFailures: 1, LadderAttempts: 1}
		storeInstance(t, memory, instance)
		wantErr := errors.New("update unavailable")
		r := NewRestartLadderReconciler(&faultStore{memStore: memory, updateErr: wantErr}, &mockDispatcher{}, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst"})
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "update instance after recovery") {
			t.Fatalf("error = %v, want wrapped recovery update failure", err)
		}
	})

	t.Run("failed action still counts attempt", func(t *testing.T) {
		memory := newMemStore()
		probes := &v1alpha1.AgentProbeSpec{HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 1}}
		ladder := &v1alpha1.RestartLadder{Steps: []v1alpha1.RestartLadderStep{{Action: v1alpha1.RestartActionRestart}}}
		storeAgent(t, memory, makeAgentWithLadder("ns", "agent1", probes, ladder))
		instance := makeInstance("ns", "inst", "agent1", v1alpha1.PhaseRunning)
		instance.Status.ProbeStatus = &v1alpha1.ProbeStatus{ConsecutiveHealthFailures: 1}
		storeInstance(t, memory, instance)
		dispatchErr := errors.New("runtime unavailable")
		r := NewRestartLadderReconciler(memory, &mockDispatcher{err: dispatchErr}, zaptest.NewLogger(t))
		result, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst"})
		if err != nil || result.RequeueAfter != time.Minute {
			t.Fatalf("result = %+v, error = %v; want default stabilization requeue", result, err)
		}
		updated := getInstance(t, memory, "ns", "inst")
		if updated.Status.ProbeStatus.LadderAttempts != 1 || updated.Status.ProbeStatus.LastLadderAction == nil {
			t.Fatalf("failed action was not counted: %#v", updated.Status.ProbeStatus)
		}
	})

	t.Run("exhausted escalation update failure", func(t *testing.T) {
		memory := newMemStore()
		probes := &v1alpha1.AgentProbeSpec{HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 1}}
		ladder := &v1alpha1.RestartLadder{Steps: []v1alpha1.RestartLadderStep{{Action: v1alpha1.RestartActionAlert, MaxAttempts: 1}}}
		storeAgent(t, memory, makeAgentWithLadder("ns", "agent1", probes, ladder))
		instance := makeInstance("ns", "inst", "agent1", v1alpha1.PhaseRunning)
		instance.Status.ProbeStatus = &v1alpha1.ProbeStatus{ConsecutiveHealthFailures: 1, LadderAttempts: 1}
		storeInstance(t, memory, instance)
		wantErr := errors.New("update unavailable")
		r := NewRestartLadderReconciler(&faultStore{memStore: memory, updateErr: wantErr}, &mockDispatcher{}, zaptest.NewLogger(t))
		_, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst"})
		if !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want exhausted escalation update failure", err)
		}
	})
}

func TestRestartLadder_ConflictBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name             string
		afterConflictErr error
		afterConflictRaw []byte
		wantErr          error
		wantMessage      string
		wantUpdates      int
	}{
		{
			name:             "instance deleted during wrapped conflict",
			afterConflictErr: fmt.Errorf("wrapped missing instance: %w", store.ErrNotFound),
			wantUpdates:      1,
		},
		{
			name:             "reread failure",
			afterConflictErr: errors.New("read unavailable"),
			wantMessage:      "re-read after conflict",
			wantUpdates:      1,
		},
		{
			name:             "malformed concurrent instance",
			afterConflictRaw: []byte("{"),
			wantMessage:      "unmarshal after conflict",
			wantUpdates:      1,
		},
		{
			name:        "retry exhaustion",
			wantErr:     store.ErrConflict,
			wantMessage: "update instance after ladder action",
			wantUpdates: 5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := newMemStore()
			probes := &v1alpha1.AgentProbeSpec{HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 1}}
			ladder := &v1alpha1.RestartLadder{Steps: []v1alpha1.RestartLadderStep{{Action: v1alpha1.RestartActionQuarantine}}}
			storeAgent(t, memory, makeAgentWithLadder("ns", "agent1", probes, ladder))
			instance := makeInstance("ns", "inst", "agent1", v1alpha1.PhaseRunning)
			instance.Status.ProbeStatus = &v1alpha1.ProbeStatus{ConsecutiveHealthFailures: 1}
			storeInstance(t, memory, instance)
			faults := &restartLadderPersistentConflictStore{
				memStore:         memory,
				afterConflictErr: tc.afterConflictErr,
				afterConflictRaw: tc.afterConflictRaw,
			}
			r := NewRestartLadderReconciler(faults, &mockDispatcher{}, zaptest.NewLogger(t))

			_, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst"})
			if tc.wantMessage == "" && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantMessage != "" && (err == nil || !strings.Contains(err.Error(), tc.wantMessage)) {
				t.Fatalf("error = %v, want message containing %q", err, tc.wantMessage)
			}
			if faults.updates != tc.wantUpdates {
				t.Fatalf("update attempts = %d, want %d", faults.updates, tc.wantUpdates)
			}
		})
	}
}
