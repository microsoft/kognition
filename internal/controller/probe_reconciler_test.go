// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/store"
	"go.uber.org/zap/zaptest"
)

type probeConflictStore struct {
	*memStore
	conflicted bool
}

type persistentProbeConflictStore struct {
	*memStore
	updates int
}

type reasoningSlotConflictStore struct {
	*memStore
	mutate     func(*v1alpha1.AgentInstance)
	conflicted bool
}

type fakeCanaryExecutor struct {
	result  CanaryResult
	err     error
	request CanaryRequest
	calls   int
}

type concurrentCanaryExecutor struct {
	mu            sync.Mutex
	prepared      int
	prepareReady  chan struct{}
	executeCalls  int
	executeReady  chan struct{}
	operationIDs  []string
	providerCalls int
}

func (e *fakeCanaryExecutor) PrepareCanary(_ context.Context, request CanaryRequest) (CanaryRequest, error) {
	request.RequestDigest = canaryRequestDigest(request)
	return request, nil
}

func (e *fakeCanaryExecutor) ExecuteCanary(_ context.Context, request CanaryRequest) (CanaryResult, error) {
	e.calls++
	e.request = request
	return e.result, e.err
}

func (e *concurrentCanaryExecutor) PrepareCanary(ctx context.Context, request CanaryRequest) (CanaryRequest, error) {
	e.mu.Lock()
	e.prepared++
	if e.prepared == 2 {
		close(e.prepareReady)
	}
	e.mu.Unlock()
	select {
	case <-e.prepareReady:
	case <-ctx.Done():
		return CanaryRequest{}, ctx.Err()
	}
	request.RequestDigest = canaryRequestDigest(request)
	return request, nil
}

func (e *concurrentCanaryExecutor) ExecuteCanary(ctx context.Context, request CanaryRequest) (CanaryResult, error) {
	e.mu.Lock()
	e.executeCalls++
	e.operationIDs = append(e.operationIDs, request.OperationID)
	if e.executeCalls == 1 {
		e.providerCalls++
	}
	if e.executeCalls == 2 {
		close(e.executeReady)
	}
	e.mu.Unlock()
	select {
	case <-e.executeReady:
	case <-ctx.Done():
		return CanaryResult{}, ctx.Err()
	}
	return CanaryResult{OperationID: request.OperationID, Outcome: CanaryOutcomePassed}, nil
}

func (s *persistentProbeConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == agentInstanceKind {
		s.updates++
		return store.ErrConflict
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *reasoningSlotConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind != agentInstanceKind || s.conflicted {
		return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
	}
	s.conflicted = true
	var winner v1alpha1.AgentInstance
	if err := json.Unmarshal(data, &winner); err != nil {
		return err
	}
	s.mutate(&winner)
	winnerData, err := json.Marshal(winner)
	if err != nil {
		return err
	}
	if err := s.memStore.Update(ctx, kind, namespace, name, winnerData, resourceVersion); err != nil {
		return err
	}
	return store.ErrConflict
}

func (s *probeConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
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
		action := v1alpha1.RestartActionRestart
		latest.ObjectMeta.Finalizers = append(latest.ObjectMeta.Finalizers, "example.com/foreign")
		latest.Status.Phase = v1alpha1.PhaseDegraded
		latest.Status.SessionID = "concurrent-session"
		latest.Status.RestartCount = 2
		latest.Status.ProbeStatus.LadderStep = 1
		latest.Status.ProbeStatus.LadderAttempts = 2
		latest.Status.ProbeStatus.LastLadderAction = &action
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

func makeProbeAgent(ns, name string, probes *v1alpha1.AgentProbeSpec) v1alpha1.Agent {
	return v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "Agent",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: v1alpha1.AgentSpec{
			Probes: probes,
		},
	}
}

// Traceability coverage for completed TCs:
// TC-0095: HealthProbe detects failure and the instance is marked unhealthy within configured interval.
// TC-0096: ReadinessProbe detects unreachable model endpoint, instance removed from AgentService routing.
// TC-0097: Kill runtime process; Agent reconciler re-creates AgentInstance and resumes.
// TC-0098: ReasoningProbe with known-answer canary passes under normal conditions.
// TC-0099: Corrupt agent prompt causes canary failure; ReasoningProbe detects and emits structured event.
// TC-0100: ReasoningProbe runs at the configured interval (default <= 5 min).

func TestProbeReconciler_InstanceNotFound(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{}
	r := NewProbeReconciler(s, d, zaptest.NewLogger(t))

	res, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "missing", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Fatalf("expected empty result, got %+v", res)
	}
}

func TestProbeReconciler_NonRunningPhaseSkipped(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{}
	r := NewProbeReconciler(s, d, zaptest.NewLogger(t))

	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhasePending)
	storeInstance(t, s, inst)

	res, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue for non-running, got %v", res.RequeueAfter)
	}
	if d.statusCalled != 0 {
		t.Fatal("dispatcher.Status should not be called for non-running instance")
	}
}

func TestProbeReconciler_NoProbeSpec(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}
	r := NewProbeReconciler(s, d, zaptest.NewLogger(t))

	ag := makeProbeAgent("ns", "agent1", nil)
	storeAgent(t, s, ag)
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	storeInstance(t, s, inst)

	res, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no requeue when no probes configured, got %v", res.RequeueAfter)
	}
}

func TestProbeReconciler_HealthProbePassesSetsCondition(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}
	r := NewProbeReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{
			IntervalSeconds:  10,
			FailureThreshold: 3,
		},
	}
	ag := makeProbeAgent("ns", "agent1", probes)
	storeAgent(t, s, ag)
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	storeInstance(t, s, inst)

	res, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 10*time.Second {
		t.Fatalf("expected 10s requeue, got %v", res.RequeueAfter)
	}

	updated := getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus == nil {
		t.Fatal("expected ProbeStatus to be set")
	}
	if updated.Status.ProbeStatus.Healthy == nil || !*updated.Status.ProbeStatus.Healthy {
		t.Fatal("expected Healthy=true")
	}
	cond := findCondition(updated.Status.Conditions, string(lifecycle.ConditionProbeHealthy))
	if cond == nil {
		t.Fatal("expected ProbeHealthy condition")
	}
	if cond.Status != string(lifecycle.ConditionTrue) {
		t.Fatalf("expected condition True, got %s", cond.Status)
	}
}

func TestProbeReconciler_ConflictPreservesConcurrentSharedState(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	s := &probeConflictStore{memStore: baseStore}
	d := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}
	r := NewProbeReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3}}
	storeAgent(t, baseStore, makeProbeAgent("ns", "agent1", probes))
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	inst.Status.ProbeStatus = &v1alpha1.ProbeStatus{}
	storeInstance(t, baseStore, inst)

	if _, err := r.Reconcile(ctx, Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind}); err != nil {
		t.Fatalf("reconcile through persistence conflict: %v", err)
	}
	if d.statusCalled != 1 {
		t.Fatalf("status calls = %d, want 1", d.statusCalled)
	}

	updated := getInstance(t, baseStore, "ns", "inst1")
	if updated.Status.ProbeStatus.Healthy == nil || !*updated.Status.ProbeStatus.Healthy {
		t.Fatalf("probe result not persisted: %#v", updated.Status.ProbeStatus)
	}
	if updated.Status.ProbeStatus.LadderStep != 1 || updated.Status.ProbeStatus.LadderAttempts != 2 || updated.Status.ProbeStatus.LastLadderAction == nil {
		t.Fatalf("concurrent ladder state was lost: %#v", updated.Status.ProbeStatus)
	}
	if updated.Status.Phase != v1alpha1.PhaseDegraded || updated.Status.SessionID != "concurrent-session" || updated.Status.RestartCount != 2 {
		t.Fatalf("concurrent runtime state was lost: %#v", updated.Status)
	}
	if condition := findCondition(updated.Status.Conditions, string(lifecycle.ConditionReady)); condition == nil || condition.Reason != "ConcurrentRuntime" {
		t.Fatalf("concurrent Ready condition was lost: %#v", condition)
	}
	if !hasFinalizer(updated.ObjectMeta.Finalizers, "example.com/foreign") {
		t.Fatal("concurrent foreign finalizer was lost")
	}
}

func TestProbeReconciler_PersistentConflictIsBounded(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	s := &persistentProbeConflictStore{memStore: baseStore}
	d := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}
	r := NewProbeReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3}}
	storeAgent(t, baseStore, makeProbeAgent("ns", "agent1", probes))
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	inst.Status.ProbeStatus = &v1alpha1.ProbeStatus{}
	storeInstance(t, baseStore, inst)

	_, err := r.Reconcile(ctx, Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Reconcile() error = %v, want ErrConflict", err)
	}
	if s.updates != 5 {
		t.Fatalf("AgentInstance update attempts = %d, want 5", s.updates)
	}
	if d.statusCalled != 1 {
		t.Fatalf("runtime status calls = %d, want 1", d.statusCalled)
	}
}

func TestProbeReconciler_HealthProbeFailsBelowThreshold(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: false}}
	r := NewProbeReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{
			IntervalSeconds:  10,
			FailureThreshold: 3,
		},
	}
	ag := makeProbeAgent("ns", "agent1", probes)
	storeAgent(t, s, ag)
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	storeInstance(t, s, inst)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus.ConsecutiveHealthFailures != 1 {
		t.Fatalf("expected 1 failure, got %d", updated.Status.ProbeStatus.ConsecutiveHealthFailures)
	}
	if updated.Status.ProbeStatus.Healthy != nil && !*updated.Status.ProbeStatus.Healthy {
		t.Fatal("should not mark unhealthy below threshold")
	}
}

func TestProbeReconciler_HealthProbeReachesThreshold(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: false}}
	r := NewProbeReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{
			IntervalSeconds:  10,
			FailureThreshold: 3,
		},
	}
	ag := makeProbeAgent("ns", "agent1", probes)
	storeAgent(t, s, ag)
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	storeInstance(t, s, inst)

	for i := 0; i < 3; i++ {
		_, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
		if err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	updated := getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus.Healthy == nil || *updated.Status.ProbeStatus.Healthy {
		t.Fatal("expected Healthy=false after reaching threshold")
	}
	cond := findCondition(updated.Status.Conditions, string(lifecycle.ConditionProbeHealthy))
	if cond == nil || cond.Status != string(lifecycle.ConditionFalse) {
		t.Fatal("expected ProbeHealthy=False condition")
	}
}

func TestProbeReconciler_HealthProbeRecovery(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: false}}
	r := NewProbeReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{
			IntervalSeconds:  10,
			FailureThreshold: 3,
		},
	}
	ag := makeProbeAgent("ns", "agent1", probes)
	storeAgent(t, s, ag)
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	storeInstance(t, s, inst)

	for i := 0; i < 3; i++ {
		r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	}

	d.status = RuntimeStatus{Alive: true}
	_, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus.ConsecutiveHealthFailures != 0 {
		t.Fatalf("expected failures reset, got %d", updated.Status.ProbeStatus.ConsecutiveHealthFailures)
	}
	if updated.Status.ProbeStatus.Healthy == nil || !*updated.Status.ProbeStatus.Healthy {
		t.Fatal("expected Healthy=true after recovery")
	}
}

func TestProbeReconciler_ReadinessProbeFailsAndRecovers(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: false}}
	r := NewProbeReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		ReadinessProbe: &v1alpha1.AgentReadinessProbe{
			IntervalSeconds:  15,
			FailureThreshold: 2,
		},
	}
	ag := makeProbeAgent("ns", "agent1", probes)
	storeAgent(t, s, ag)
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	storeInstance(t, s, inst)

	for i := 0; i < 2; i++ {
		r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	}

	updated := getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus.Ready == nil || *updated.Status.ProbeStatus.Ready {
		t.Fatal("expected Ready=false")
	}
	cond := findCondition(updated.Status.Conditions, string(lifecycle.ConditionProbeReady))
	if cond == nil || cond.Status != string(lifecycle.ConditionFalse) {
		t.Fatal("expected ProbeReady=False condition")
	}

	d.status = RuntimeStatus{Alive: true}
	r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})

	updated = getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus.Ready == nil || !*updated.Status.ProbeStatus.Ready {
		t.Fatal("expected Ready=true after recovery")
	}
}

func TestProbeReconciler_ReasoningProbeInvokesCanaryAndPassesExactOutcome(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: false}}
	r := NewProbeReconciler(s, d, zaptest.NewLogger(t))
	executor := &fakeCanaryExecutor{result: CanaryResult{
		OperationID:            "canary-operation",
		ExpectedBytes:          20,
		ActualBytes:            20,
		ExpectedSHA256:         "sha256:expected",
		ActualSHA256:           "sha256:actual",
		PromptTokens:           31,
		CompletionTokens:       7,
		TotalTokens:            38,
		ProviderDurationMillis: 125,
		TraceID:                "0123456789abcdef0123456789abcdef",
		SpanID:                 "0123456789abcdef",
		Outcome:                CanaryOutcomePassed,
	}}
	r.SetCanaryExecutor(executor)

	probes := &v1alpha1.AgentProbeSpec{
		ReasoningProbe: &v1alpha1.AgentReasoningProbe{
			CanaryTask:       "Return exactly KOGNITION_CANARY_OK",
			ExpectedOutcome:  "KOGNITION_CANARY_OK",
			IntervalSeconds:  10,
			TimeoutSeconds:   7,
			MaxTokens:        23,
			MaxInputTokens:   97,
			FailureThreshold: 2,
		},
	}
	ag := makeProbeAgent("ns", "agent1", probes)
	storeAgent(t, s, ag)
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	storeInstance(t, s, inst)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	updated := getInstance(t, s, "ns", "inst1")
	if executor.calls != 1 {
		t.Fatalf("ExecuteCanary() calls = %d, want 1", executor.calls)
	}
	if executor.request.CanaryTask != "Return exactly KOGNITION_CANARY_OK" || executor.request.ExpectedOutcome != "KOGNITION_CANARY_OK" {
		t.Fatalf("ExecuteCanary() request = %+v", executor.request)
	}
	if executor.request.Timeout != 7*time.Second || executor.request.MaxOutputTokens != 23 || executor.request.MaxInputTokens != 97 {
		t.Fatalf("ExecuteCanary() bounds = timeout %v, maxInputTokens %d, maxOutputTokens %d", executor.request.Timeout, executor.request.MaxInputTokens, executor.request.MaxOutputTokens)
	}
	if updated.Status.ProbeStatus.LastReasoningResult == nil || updated.Status.ProbeStatus.LastReasoningResult.OperationID != "canary-operation" || updated.Status.ProbeStatus.LastReasoningResult.TotalTokens != 38 {
		t.Fatalf("LastReasoningResult = %#v, want metadata-only result", updated.Status.ProbeStatus.LastReasoningResult)
	}
	if updated.Status.ProbeStatus.ReasoningHealthy == nil || !*updated.Status.ProbeStatus.ReasoningHealthy {
		t.Fatal("expected ReasoningHealthy=true after canary pass")
	}
	cond := findCondition(updated.Status.Conditions, string(lifecycle.ConditionReasoningHealthy))
	if cond == nil || cond.Status != string(lifecycle.ConditionTrue) {
		t.Fatal("expected ReasoningHealthy=True condition after canary pass")
	}
}

func TestProbeReconciler_ReasoningProbeRunsOncePerCanonicalSlot(t *testing.T) {
	s := newMemStore()
	now := time.Date(2026, time.March, 1, 12, 0, 35, 0, time.UTC)
	r := NewProbeReconciler(s, &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }
	executor := &fakeCanaryExecutor{result: CanaryResult{Outcome: CanaryOutcomePassed}}
	r.SetCanaryExecutor(executor)
	probes := &v1alpha1.AgentProbeSpec{ReasoningProbe: &v1alpha1.AgentReasoningProbe{
		CanaryTask: "canary", ExpectedOutcome: "ok", IntervalSeconds: 10,
	}}
	storeAgent(t, s, makeProbeAgent("ns", "agent1", probes))
	instance := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	instance.ObjectMeta.CreationTimestamp = now.Add(-35 * time.Second)
	storeInstance(t, s, instance)

	for attempt := 0; attempt < 2; attempt++ {
		if _, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind}); err != nil {
			t.Fatalf("Reconcile() attempt %d error = %v", attempt+1, err)
		}
	}
	if executor.calls != 1 {
		t.Fatalf("ExecuteCanary() calls in one slot = %d, want 1", executor.calls)
	}
	if got, want := executor.request.ScheduleSlot, instance.ObjectMeta.CreationTimestamp.Add(30*time.Second); !got.Equal(want) {
		t.Fatalf("canonical slot = %s, want %s", got, want)
	}

	now = now.Add(25 * time.Second)
	if _, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind}); err != nil {
		t.Fatalf("Reconcile() after missed intervals error = %v", err)
	}
	if executor.calls != 2 {
		t.Fatalf("ExecuteCanary() calls after missed intervals = %d, want 2", executor.calls)
	}
	if got, want := executor.request.ScheduleSlot, instance.ObjectMeta.CreationTimestamp.Add(60*time.Second); !got.Equal(want) {
		t.Fatalf("coalesced slot = %s, want %s", got, want)
	}
}

func TestProbeReconciler_ReasoningProbeConflictDoesNotInvokeProvider(t *testing.T) {
	baseStore := newMemStore()
	s := &persistentProbeConflictStore{memStore: baseStore}
	r := NewProbeReconciler(s, &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}, zaptest.NewLogger(t))
	executor := &fakeCanaryExecutor{result: CanaryResult{Outcome: CanaryOutcomePassed}}
	r.SetCanaryExecutor(executor)
	probes := &v1alpha1.AgentProbeSpec{ReasoningProbe: &v1alpha1.AgentReasoningProbe{
		CanaryTask: "canary", ExpectedOutcome: "ok", IntervalSeconds: 10,
	}}
	storeAgent(t, baseStore, makeProbeAgent("ns", "agent1", probes))
	storeInstance(t, baseStore, makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning))

	result, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("ownership conflict must requeue")
	}
	if executor.calls != 0 {
		t.Fatalf("ExecuteCanary() calls after ownership conflict = %d, want 0", executor.calls)
	}
}

func TestProbeReconciler_ReasoningProbeConflictJoinsSameSlotWinner(t *testing.T) {
	baseStore := newMemStore()
	now := time.Date(2026, time.March, 1, 12, 0, 35, 0, time.UTC)
	s := &reasoningSlotConflictStore{memStore: baseStore, mutate: func(instance *v1alpha1.AgentInstance) {
		instance.Status.ProbeStatus.ReasoningOperationID = "winner-operation"
		instance.Status.ProbeStatus.ReasoningRequestDigest = "winner-digest"
		instance.Status.ProbeStatus.ReasoningBindings = []v1alpha1.CanaryResourceBinding{{Kind: "Model", Name: "winner-model", ResourceVersion: "7"}}
	}}
	r := NewProbeReconciler(s, &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }
	executor := &fakeCanaryExecutor{result: CanaryResult{OperationID: "winner-operation", Outcome: CanaryOutcomePassed}}
	r.SetCanaryExecutor(executor)
	probes := &v1alpha1.AgentProbeSpec{ReasoningProbe: &v1alpha1.AgentReasoningProbe{CanaryTask: "canary", ExpectedOutcome: "ok", IntervalSeconds: 10}}
	storeAgent(t, baseStore, makeProbeAgent("ns", "agent1", probes))
	instance := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	instance.ObjectMeta.CreationTimestamp = now.Add(-35 * time.Second)
	storeInstance(t, baseStore, instance)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if executor.calls != 1 || executor.request.OperationID != "winner-operation" || executor.request.RequestDigest != "winner-digest" {
		t.Fatalf("joined request = calls:%d operation:%q digest:%q", executor.calls, executor.request.OperationID, executor.request.RequestDigest)
	}
	if len(executor.request.Bindings) != 1 || executor.request.Bindings[0].Name != "winner-model" {
		t.Fatalf("joined bindings = %#v, want persisted winner binding", executor.request.Bindings)
	}
	updated := getInstance(t, baseStore, "ns", "inst1")
	if updated.Status.ProbeStatus.LastReasoningResult == nil || updated.Status.ProbeStatus.LastReasoningResult.OperationID != "winner-operation" {
		t.Fatalf("terminal reasoning result = %#v, want joined operation result", updated.Status.ProbeStatus.LastReasoningResult)
	}
}

func TestProbeReconciler_ReasoningProbeConflictSkipsNewerSlot(t *testing.T) {
	baseStore := newMemStore()
	now := time.Date(2026, time.March, 1, 12, 0, 35, 0, time.UTC)
	s := &reasoningSlotConflictStore{memStore: baseStore, mutate: func(instance *v1alpha1.AgentInstance) {
		newer := now.Add(10 * time.Second).Format(time.RFC3339Nano)
		instance.Status.ProbeStatus.LastReasoningScheduleSlot = &newer
		instance.Status.ProbeStatus.ReasoningOperationID = "newer-operation"
	}}
	r := NewProbeReconciler(s, &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }
	executor := &fakeCanaryExecutor{result: CanaryResult{Outcome: CanaryOutcomePassed}}
	r.SetCanaryExecutor(executor)
	probes := &v1alpha1.AgentProbeSpec{ReasoningProbe: &v1alpha1.AgentReasoningProbe{CanaryTask: "canary", ExpectedOutcome: "ok", IntervalSeconds: 10}}
	storeAgent(t, baseStore, makeProbeAgent("ns", "agent1", probes))
	instance := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	instance.ObjectMeta.CreationTimestamp = now.Add(-35 * time.Second)
	storeInstance(t, baseStore, instance)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("Reconcile() = (%+v, %v), want requeue after newer winner", result, err)
	}
	if executor.calls != 0 {
		t.Fatalf("ExecuteCanary() calls = %d, want no call for newer winner", executor.calls)
	}
}

func TestProbeReconciler_ReasoningProbeFinishesOlderSlotBeforeAcquiringLatest(t *testing.T) {
	s := newMemStore()
	now := time.Date(2026, time.March, 1, 12, 0, 35, 0, time.UTC)
	r := NewProbeReconciler(s, &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }
	executor := &fakeCanaryExecutor{result: CanaryResult{OperationID: "older-operation", Outcome: CanaryOutcomePassed}}
	r.SetCanaryExecutor(executor)
	probes := &v1alpha1.AgentProbeSpec{ReasoningProbe: &v1alpha1.AgentReasoningProbe{CanaryTask: "canary", ExpectedOutcome: "ok", IntervalSeconds: 10}}
	storeAgent(t, s, makeProbeAgent("ns", "agent1", probes))
	instance := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	instance.ObjectMeta.CreationTimestamp = now.Add(-35 * time.Second)
	older := instance.ObjectMeta.CreationTimestamp.Add(10 * time.Second)
	olderText := older.Format(time.RFC3339Nano)
	instance.Status.ProbeStatus = &v1alpha1.ProbeStatus{
		LastReasoningScheduleSlot: &olderText,
		ReasoningOperationID:      "older-operation",
		ReasoningRequestDigest:    "older-digest",
	}
	storeInstance(t, s, instance)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if executor.calls != 1 || executor.request.OperationID != "older-operation" || !executor.request.ScheduleSlot.Equal(older) {
		t.Fatalf("executed request = calls:%d operation:%q slot:%s, want unfinished older slot", executor.calls, executor.request.OperationID, executor.request.ScheduleSlot)
	}
}

func TestProbeReconciler_ConcurrentReasoningProbesShareOneSlotAndProviderCall(t *testing.T) {
	s := newMemStore()
	now := time.Date(2026, time.March, 1, 12, 0, 35, 0, time.UTC)
	executor := &concurrentCanaryExecutor{prepareReady: make(chan struct{}), executeReady: make(chan struct{})}
	reconcilers := []*ProbeReconciler{
		NewProbeReconciler(s, &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}, zaptest.NewLogger(t)),
		NewProbeReconciler(s, &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}, zaptest.NewLogger(t)),
	}
	for _, reconciler := range reconcilers {
		reconciler.now = func() time.Time { return now }
		reconciler.SetCanaryExecutor(executor)
	}
	probes := &v1alpha1.AgentProbeSpec{ReasoningProbe: &v1alpha1.AgentReasoningProbe{CanaryTask: "canary", ExpectedOutcome: "ok", IntervalSeconds: 10}}
	storeAgent(t, s, makeProbeAgent("ns", "agent1", probes))
	instance := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	instance.ObjectMeta.CreationTimestamp = now.Add(-35 * time.Second)
	storeInstance(t, s, instance)

	request := Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind}
	errCh := make(chan error, len(reconcilers))
	for _, reconciler := range reconcilers {
		go func() {
			_, err := reconciler.Reconcile(context.Background(), request)
			errCh <- err
		}()
	}
	for range reconcilers {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent Reconcile() error = %v", err)
		}
	}

	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.executeCalls != 2 || executor.providerCalls != 1 {
		t.Fatalf("concurrent calls = executor:%d provider:%d, want executor:2 provider:1", executor.executeCalls, executor.providerCalls)
	}
	if len(executor.operationIDs) != 2 || executor.operationIDs[0] == "" || executor.operationIDs[0] != executor.operationIDs[1] {
		t.Fatalf("concurrent operation IDs = %#v, want one non-empty persisted identity", executor.operationIDs)
	}
}

func TestProbeReconciler_ReasoningProbeRejectsInvalidScheduleState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1alpha1.AgentInstance)
	}{
		{name: "missing creation timestamp", mutate: func(instance *v1alpha1.AgentInstance) {
			instance.ObjectMeta.CreationTimestamp = time.Time{}
		}},
		{name: "malformed persisted slot", mutate: func(instance *v1alpha1.AgentInstance) {
			slot := "not-a-timestamp"
			instance.Status.ProbeStatus = &v1alpha1.ProbeStatus{LastReasoningScheduleSlot: &slot}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newMemStore()
			r := NewProbeReconciler(s, &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}, zaptest.NewLogger(t))
			executor := &fakeCanaryExecutor{result: CanaryResult{Outcome: CanaryOutcomePassed}}
			r.SetCanaryExecutor(executor)
			probes := &v1alpha1.AgentProbeSpec{ReasoningProbe: &v1alpha1.AgentReasoningProbe{
				CanaryTask: "canary", ExpectedOutcome: "ok", IntervalSeconds: 10,
			}}
			storeAgent(t, s, makeProbeAgent("ns", "agent1", probes))
			instance := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
			test.mutate(&instance)
			storeInstance(t, s, instance)

			if _, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind}); err == nil {
				t.Fatal("Reconcile() error = nil, want invalid schedule error")
			}
			if executor.calls != 0 {
				t.Fatalf("ExecuteCanary() calls for invalid schedule = %d, want 0", executor.calls)
			}
		})
	}
}

func TestProbeReconciler_ReasoningProbeDistinguishesMismatchFromExecutionFailure(t *testing.T) {
	tests := []struct {
		name       string
		result     CanaryResult
		err        error
		wantReason string
	}{
		{
			name:       "outcome mismatch",
			result:     CanaryResult{Outcome: CanaryOutcomeMismatch},
			wantReason: "ReasoningOutcomeMismatch",
		},
		{
			name:       "execution failure",
			err:        errors.New("provider transport failed"),
			wantReason: "ReasoningExecutionFailed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newMemStore()
			r := NewProbeReconciler(s, &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}, zaptest.NewLogger(t))
			now := time.Now().UTC()
			r.now = func() time.Time { return now }
			r.SetCanaryExecutor(&fakeCanaryExecutor{result: test.result, err: test.err})
			probes := &v1alpha1.AgentProbeSpec{ReasoningProbe: &v1alpha1.AgentReasoningProbe{
				CanaryTask:       "Return exactly KOGNITION_CANARY_OK",
				ExpectedOutcome:  "KOGNITION_CANARY_OK",
				IntervalSeconds:  10,
				FailureThreshold: 2,
			}}
			storeAgent(t, s, makeProbeAgent("ns", "agent1", probes))
			instance := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
			instance.ObjectMeta.CreationTimestamp = now
			storeInstance(t, s, instance)

			for attempt := 0; attempt < 2; attempt++ {
				if _, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: agentInstanceKind}); err != nil {
					t.Fatalf("Reconcile() attempt %d error = %v", attempt+1, err)
				}
				now = now.Add(10 * time.Second)
			}

			updated := getInstance(t, s, "ns", "inst1")
			if updated.Status.ProbeStatus.ConsecutiveReasoningFailures != 2 {
				t.Fatalf("consecutive reasoning failures = %d, want 2", updated.Status.ProbeStatus.ConsecutiveReasoningFailures)
			}
			if updated.Status.ProbeStatus.ReasoningHealthy == nil || *updated.Status.ProbeStatus.ReasoningHealthy {
				t.Fatal("expected ReasoningHealthy=false at failure threshold")
			}
			condition := findCondition(updated.Status.Conditions, string(lifecycle.ConditionReasoningHealthy))
			if condition == nil || condition.Reason != test.wantReason {
				t.Fatalf("ReasoningHealthy condition = %#v, want reason %q", condition, test.wantReason)
			}
		})
	}
}

func TestProbeReconciler_ShortestInterval(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}
	r := NewProbeReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{
			IntervalSeconds: 20,
		},
		ReadinessProbe: &v1alpha1.AgentReadinessProbe{
			IntervalSeconds: 5,
		},
	}
	ag := makeProbeAgent("ns", "agent1", probes)
	storeAgent(t, s, ag)
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	storeInstance(t, s, inst)

	res, err := r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 5*time.Second {
		t.Fatalf("expected 5s (shortest), got %v", res.RequeueAfter)
	}
}

func TestIsThresholdBreached(t *testing.T) {
	spec := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 3},
	}

	ps := &v1alpha1.ProbeStatus{ConsecutiveHealthFailures: 2}
	if IsThresholdBreached(ps, spec) {
		t.Fatal("should not be breached at 2/3")
	}

	ps.ConsecutiveHealthFailures = 3
	if !IsThresholdBreached(ps, spec) {
		t.Fatal("should be breached at 3/3")
	}

	if IsThresholdBreached(nil, spec) {
		t.Fatal("nil ps should not breach")
	}
	if IsThresholdBreached(ps, nil) {
		t.Fatal("nil spec should not breach")
	}

	// Reasoning probe threshold (default 2).
	reasoningSpec := &v1alpha1.AgentProbeSpec{
		ReasoningProbe: &v1alpha1.AgentReasoningProbe{FailureThreshold: 2},
	}
	rps := &v1alpha1.ProbeStatus{ConsecutiveReasoningFailures: 1}
	if IsThresholdBreached(rps, reasoningSpec) {
		t.Fatal("reasoning should not be breached at 1/2")
	}
	rps.ConsecutiveReasoningFailures = 2
	if !IsThresholdBreached(rps, reasoningSpec) {
		t.Fatal("reasoning should be breached at 2/2")
	}
}
func TestProbeReconciler_HealthProbeSuccessThreshold(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: false}}
	r := NewProbeReconciler(s, d, zaptest.NewLogger(t))

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{
			IntervalSeconds:  10,
			FailureThreshold: 2,
			SuccessThreshold: 3,
		},
	}
	ag := makeProbeAgent("ns", "agent1", probes)
	storeAgent(t, s, ag)
	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	storeInstance(t, s, inst)

	// Drive to unhealthy.
	for i := 0; i < 2; i++ {
		r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	}
	updated := getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus.Healthy == nil || *updated.Status.ProbeStatus.Healthy {
		t.Fatal("expected Healthy=false after reaching failure threshold")
	}

	// Switch to healthy runtime. First two successes should NOT flip Healthy back.
	d.status = RuntimeStatus{Alive: true}
	for i := 0; i < 2; i++ {
		r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
		updated = getInstance(t, s, "ns", "inst1")
		if updated.Status.ProbeStatus.Healthy != nil && *updated.Status.ProbeStatus.Healthy {
			t.Fatalf("expected Healthy still false after %d successes (threshold=3)", i+1)
		}
		if updated.Status.ProbeStatus.ConsecutiveHealthSuccesses != int32(i+1) {
			t.Fatalf("expected ConsecutiveHealthSuccesses=%d, got %d", i+1, updated.Status.ProbeStatus.ConsecutiveHealthSuccesses)
		}
	}

	// Third success should flip Healthy=true.
	r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	updated = getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus.Healthy == nil || !*updated.Status.ProbeStatus.Healthy {
		t.Fatal("expected Healthy=true after reaching success threshold")
	}
	if updated.Status.ProbeStatus.ConsecutiveHealthSuccesses != 0 {
		t.Fatalf("expected ConsecutiveHealthSuccesses reset to 0, got %d", updated.Status.ProbeStatus.ConsecutiveHealthSuccesses)
	}

	// Verify a failure while recovering resets success counter.
	d.status = RuntimeStatus{Alive: false}
	// Drive to unhealthy again.
	for i := 0; i < 2; i++ {
		r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	}
	// One success, then fail again.
	d.status = RuntimeStatus{Alive: true}
	r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	updated = getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus.ConsecutiveHealthSuccesses != 1 {
		t.Fatalf("expected ConsecutiveHealthSuccesses=1, got %d", updated.Status.ProbeStatus.ConsecutiveHealthSuccesses)
	}

	d.status = RuntimeStatus{Alive: false}
	r.Reconcile(context.Background(), Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"})
	updated = getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus.ConsecutiveHealthSuccesses != 0 {
		t.Fatalf("expected ConsecutiveHealthSuccesses reset to 0 after failure, got %d", updated.Status.ProbeStatus.ConsecutiveHealthSuccesses)
	}
}
