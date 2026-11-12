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

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

// ---------------------------------------------------------------------------
// mockDispatcher — controllable RuntimeDispatcher for tests
// ---------------------------------------------------------------------------

type mockDispatcher struct {
	sessionID    string
	err          error
	called       int
	lastInstance *v1alpha1.AgentInstance
	status       RuntimeStatus
	statusErr    error
	statusCalled int
	statusSet    bool
	terminateErr error
	terminations int
	termReason   string
}

type recordingInstanceQuota struct {
	checkErr      error
	reserveErr    error
	budgetErr     error
	runtimeErr    error
	settleErr     error
	checks        int
	reserves      int
	budgets       int
	runtimeChecks int
	settles       int
	namespace     string
	budgetAgent   string
	reservationID string
	inputCeiling  int64
	outputCeiling int64
	usedTokens    int64
	actualInput   int64
	actualOutput  int64
}

type instanceModelAuthorizerStub struct {
	err       error
	calls     int
	namespace string
	agent     string
	model     string
}

func (a *instanceModelAuthorizerStub) EvaluateModelAccess(_ context.Context, namespace, agent, model string) error {
	a.calls++
	a.namespace = namespace
	a.agent = agent
	a.model = model
	return a.err
}

func (q *recordingInstanceQuota) CheckCallBudget(_ context.Context, namespace, agent string, _, _ int64) error {
	q.budgets++
	q.namespace = namespace
	q.budgetAgent = agent
	return q.budgetErr
}

func (q *recordingInstanceQuota) CheckSchedulerQuota(_ context.Context, namespace string) error {
	q.checks++
	q.namespace = namespace
	return q.checkErr
}

func (q *recordingInstanceQuota) CheckRuntimeBudget(_ context.Context, namespace, _ string, usedTokens int64) error {
	q.runtimeChecks++
	q.namespace = namespace
	q.usedTokens = usedTokens
	return q.runtimeErr
}

func (q *recordingInstanceQuota) ReserveTokens(_ context.Context, namespace, reservationID string, inputCeiling, outputCeiling int64) error {
	q.reserves++
	q.namespace = namespace
	q.reservationID = reservationID
	q.inputCeiling = inputCeiling
	q.outputCeiling = outputCeiling
	return q.reserveErr
}

func (q *recordingInstanceQuota) Settle(_ context.Context, reservationID string, actualInput, actualOutput int64) error {
	q.settles++
	q.reservationID = reservationID
	q.actualInput = actualInput
	q.actualOutput = actualOutput
	return q.settleErr
}

func (m *mockDispatcher) Dispatch(_ context.Context, instance *v1alpha1.AgentInstance) (string, error) {
	m.called++
	if instance != nil {
		copyInst := *instance
		copyInst.Spec = instance.Spec
		copyInst.Spec.SessionContext = cloneStringMap(instance.Spec.SessionContext)
		m.lastInstance = &copyInst
	}
	return m.sessionID, m.err
}

func (m *mockDispatcher) Status(_ context.Context, _ *v1alpha1.AgentInstance) (RuntimeStatus, error) {
	m.statusCalled++
	if m.statusErr != nil {
		return RuntimeStatus{}, m.statusErr
	}
	if !m.statusSet {
		return RuntimeStatus{Alive: true}, nil
	}
	return m.status, nil
}

func (m *mockDispatcher) Terminate(_ context.Context, _ *v1alpha1.AgentInstance, reason string) error {
	m.terminations++
	m.termReason = reason
	return m.terminateErr
}

type placementDispatcher struct {
	mockDispatcher
	replicas         []RuntimeReplica
	preferredReplica string
}

func (d *placementDispatcher) RuntimeReplicas(_ context.Context, _ *v1alpha1.AgentInstance) ([]RuntimeReplica, error) {
	return d.replicas, nil
}

func (d *placementDispatcher) DispatchTo(ctx context.Context, instance *v1alpha1.AgentInstance, preferredReplica string) (string, error) {
	d.preferredReplica = preferredReplica
	return d.Dispatch(ctx, instance)
}

type recordingScheduler struct {
	request ScheduleRequest
	result  ScheduleResult
}

func (s *recordingScheduler) Schedule(_ context.Context, request ScheduleRequest) (ScheduleResult, error) {
	s.request = request
	return s.result, nil
}

func TestAgentInstanceReconciler_DispatchUsesSchedulerPreferredReplica(t *testing.T) {
	instance := makeInstance("default", "task-1", "agent-a", v1alpha1.PhasePending)
	dispatcher := &placementDispatcher{
		mockDispatcher: mockDispatcher{sessionID: "session-1"},
		replicas: []RuntimeReplica{
			{Name: "agent-a-1", AgentName: "agent-a", Ready: true},
			{Name: "agent-a-2", AgentName: "agent-a", Ready: true},
		},
	}
	scheduler := &recordingScheduler{result: ScheduleResult{
		Assigned:         true,
		AgentName:        "agent-a",
		PreferredReplica: "agent-a-2",
	}}
	reconciler := &AgentInstanceReconciler{dispatcher: dispatcher, scheduler: scheduler}

	sessionID, err := reconciler.dispatch(context.Background(), &instance, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("dispatch() error = %v", err)
	}
	if sessionID != "session-1" {
		t.Fatalf("dispatch() session ID = %q, want session-1", sessionID)
	}
	if scheduler.request.AgentName != "agent-a" || len(scheduler.request.Replicas) != 2 {
		t.Fatalf("Schedule() request = %+v, want agent-a with two replicas", scheduler.request)
	}
	if dispatcher.preferredReplica != "agent-a-2" {
		t.Fatalf("DispatchTo() preferred replica = %q, want agent-a-2", dispatcher.preferredReplica)
	}
	if instance.Spec.AgentRef.Name != "agent-a" || dispatcher.lastInstance.Spec.AgentRef.Name != "agent-a" {
		t.Fatalf("AgentRef changed during placement: source=%q dispatched=%q", instance.Spec.AgentRef.Name, dispatcher.lastInstance.Spec.AgentRef.Name)
	}
}

func TestAgentInstanceReconciler_DispatchRejectsCrossAgentPlacement(t *testing.T) {
	instance := makeInstance("default", "task-1", "agent-a", v1alpha1.PhasePending)
	dispatcher := &placementDispatcher{
		mockDispatcher: mockDispatcher{sessionID: "session-1"},
		replicas: []RuntimeReplica{
			{Name: "agent-a-1", AgentName: "agent-a", Ready: true},
		},
	}
	scheduler := &recordingScheduler{result: ScheduleResult{
		Assigned:         true,
		AgentName:        "agent-b",
		PreferredReplica: "agent-b-1",
	}}
	reconciler := &AgentInstanceReconciler{dispatcher: dispatcher, scheduler: scheduler}

	if _, err := reconciler.dispatch(context.Background(), &instance, zaptest.NewLogger(t)); err != nil {
		t.Fatalf("dispatch() error = %v", err)
	}
	if dispatcher.preferredReplica != "" {
		t.Fatalf("DispatchTo() preferred replica = %q, want service fallback", dispatcher.preferredReplica)
	}
	if instance.Spec.AgentRef.Name != "agent-a" || dispatcher.lastInstance.Spec.AgentRef.Name != "agent-a" {
		t.Fatalf("AgentRef changed during rejected placement: source=%q dispatched=%q", instance.Spec.AgentRef.Name, dispatcher.lastInstance.Spec.AgentRef.Name)
	}
}

type faultStore struct {
	*memStore
	getErr    error
	updateErr error
}

type instanceConflictStore struct {
	*memStore
	updates int
}

type instanceLifecycleConflictStore struct {
	*memStore
	conflicted bool
}

type updateFailOnceStore struct {
	*memStore
	failures int
}

func (s *instanceLifecycleConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
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
		latest.Status.SessionID = "concurrent-session"
		concurrentStart := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
		latest.Status.StartTime = &concurrentStart
		latest.Status.DispatchAttempts = 4
		latest.Status.TokenUsage = &v1alpha1.TokenUsage{PromptTokens: 7, CompletionTokens: 5, TotalTokens: 12}
		latest.Status.ToolCalls = 2
		latest.Status.ToolCallsDetail = []v1alpha1.ToolCallDetail{{Name: "concurrent-tool"}}
		latest.Status.Output = "concurrent output"
		latest.Status.Conditions = lifecycleToAPIConditions(lifecycle.SetCondition(
			apiToLifecycleConditions(latest.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionUnknown,
			"ConcurrentSession",
			"replacement session is running",
		))
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

func (s *updateFailOnceStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == agentInstanceKind && s.failures < 5 {
		s.failures++
		return store.ErrConflict
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *instanceConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	s.updates++
	if kind == agentInstanceKind && s.updates == 2 {
		raw, err := s.memStore.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var latest v1alpha1.AgentInstance
		if err := json.Unmarshal(raw, &latest); err != nil {
			return err
		}
		latest.ObjectMeta.Finalizers = append(latest.ObjectMeta.Finalizers, "example.com/foreign")
		latest.Status.ProbeStatus = &v1alpha1.ProbeStatus{ConsecutiveHealthFailures: 2}
		latest.Status.RestartCount = 3
		latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{
			Type: "ProbeHealthy", Status: "False", Reason: "ConcurrentProbe",
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

func (s *faultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.memStore.Get(ctx, kind, namespace, name)
}

func (s *faultStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

// ---------------------------------------------------------------------------
// Helper to build a valid AgentInstance
// ---------------------------------------------------------------------------

func makeInstance(ns, name, agentName string, phase v1alpha1.Phase) v1alpha1.AgentInstance {
	return v1alpha1.AgentInstance{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "AgentInstance",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			UID:               "inst-uid-001",
			CreationTimestamp: time.Now().UTC(),
			OwnerReferences: []v1alpha1.OwnerReference{
				{
					APIVersion: v1alpha1.APIVersion,
					Kind:       "Agent",
					Name:       agentName,
					UID:        "agent-uid-001",
				},
			},
		},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef:       v1alpha1.LocalObjectReference{Name: agentName},
			Revision:       1,
			SessionContext: map[string]string{"input": "test request"},
		},
		Status: v1alpha1.AgentInstanceStatus{
			Phase: phase,
		},
	}
}

func TestAgentInstanceReconciler_AliveWarmReplicaTransitionsToReady(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))

	inst := makeInstance("default", "warm-replica", "my-agent", v1alpha1.PhaseRunning)
	inst.Spec.SessionContext = nil
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, inst)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "warm-replica", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Fatalf("Reconcile() result = %+v, want no requeue", result)
	}

	got := getInstance(t, s, "default", "warm-replica")
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %s, want Ready", got.Status.Phase)
	}
	var ready *v1alpha1.Condition
	for index := range got.Status.Conditions {
		if got.Status.Conditions[index].Type == "Ready" {
			ready = &got.Status.Conditions[index]
			break
		}
	}
	if ready == nil || ready.Status != "True" || ready.Reason != "RuntimeReady" {
		t.Fatalf("Ready condition = %+v, want RuntimeReady=True", ready)
	}
}

func TestAgentInstanceReconciler_ProvisioningWarmReplicaWaitsForAvailability(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true, Pending: true}}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	inst := makeInstance("default", "warm-replica", "my-agent", v1alpha1.PhaseRunning)
	inst.Spec.SessionContext = nil
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, inst)
	request := Request{Namespace: "default", Name: "warm-replica", Kind: "AgentInstance"}

	result, err := r.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got := getInstance(t, s, "default", "warm-replica"); got.Status.Phase != v1alpha1.PhaseRunning || result.RequeueAfter <= 0 {
		t.Fatalf("provisioning warm replica = %+v, result = %+v; want Running and another availability poll", got.Status, result)
	}

	dispatcher.status.Pending = false
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := getInstance(t, s, "default", "warm-replica"); got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("available warm replica phase = %s, want Ready", got.Status.Phase)
	}
}

func TestAgentInstanceReconciler_AuthorizesPinnedModelAsOriginalAgent(t *testing.T) {
	s := newMemStore()
	currentAgent := makeAgent("default", "planner", 1)
	currentAgent.Spec.ModelRef.Name = "current-model"
	currentData, err := json.Marshal(currentAgent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), "Agent", "default", "planner", currentData); err != nil {
		t.Fatal(err)
	}
	pinnedRevision := int64(2)
	pinnedAgent := currentAgent
	pinnedAgent.Spec.ModelRef.Name = "pinned-model"
	pinnedData, err := json.Marshal(pinnedAgent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRevision(context.Background(), "Agent", "default", "planner", pinnedRevision, nil, pinnedData); err != nil {
		t.Fatal(err)
	}
	instance := makeInstance("default", "pinned-instance", "planner", v1alpha1.PhasePending)
	instance.Spec.AgentRevision = &pinnedRevision
	storeInstance(t, s, instance)
	dispatcher := &mockDispatcher{}
	authorizer := &instanceModelAuthorizerStub{}
	reconciler := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	reconciler.SetModelAuthorizer(authorizer)

	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: "AgentInstance"}); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: "AgentInstance"}); err != nil {
		t.Fatalf("dispatch pinned instance: %v", err)
	}
	if authorizer.agent != "planner" || authorizer.model != "pinned-model" {
		t.Fatalf("authorization = agent %q model %q, want planner/pinned-model", authorizer.agent, authorizer.model)
	}
}

func TestAgentInstanceReconciler_RunningInvocationTimeoutFailsSession(t *testing.T) {
	s := newMemStore()
	storeQuotaTestAgent(t, s, "default", "my-agent", 128)
	agent := getStoredAgent(t, s, "default", "my-agent")
	agent.Spec.Runtime = &v1alpha1.AgentRuntimeSpec{Timeout: "50ms"}
	storeUpdatedAgent(t, s, agent)
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true}}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))

	inst := makeInstance("default", "stale-session", "my-agent", v1alpha1.PhaseRunning)
	startedAt := time.Now().UTC().Add(-time.Second)
	inst.Status.StartTime = &startedAt
	inst.Status.SessionID = "stale-runtime-session"
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, inst)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "stale-session", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Fatalf("Reconcile() result = %+v, want no requeue", result)
	}
	if dispatcher.statusCalled != 1 {
		t.Fatalf("dispatcher status calls = %d, want 1", dispatcher.statusCalled)
	}

	got := getInstance(t, s, "default", "stale-session")
	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
	ready := findCondition(got.Status.Conditions, string(lifecycle.ConditionReady))
	if ready == nil || ready.Reason != "SessionTimeoutExceeded" || !strings.Contains(ready.Message, "50ms") {
		t.Fatalf("Ready condition = %+v, want SessionTimeoutExceeded with timeout", ready)
	}
	if dispatcher.terminations != 1 || dispatcher.termReason != "session timeout exceeded" {
		t.Fatalf("runtime terminations = %d reason = %q, want timed-out session termination", dispatcher.terminations, dispatcher.termReason)
	}
}

// TC-0091: reported runtime usage at the per-session limit throttles the
// session through the public reconciliation boundary and settles exact usage.
func TestAgentInstanceReconciler_RuntimeQuotaExceededTerminatesSession(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{
		Alive: true,
		TokenUsage: &v1alpha1.TokenUsage{
			PromptTokens:     60,
			CompletionTokens: 40,
			TotalTokens:      100,
		},
	}}
	quota := &recordingInstanceQuota{runtimeErr: fmt.Errorf("%w: agent my-agent used 100 tokens, limit 100", ErrBudgetExceeded)}
	reconciler := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	reconciler.SetQuotaEnforcer(quota)

	instance := makeInstance("default", "quota-exceeded", "my-agent", v1alpha1.PhaseRunning)
	instance.Status.SessionID = "runtime-session"
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)

	result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Fatalf("Reconcile() result = %+v, want terminal result", result)
	}

	got := getInstance(t, s, "default", instance.ObjectMeta.Name)
	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
	budget := findCondition(got.Status.Conditions, string(lifecycle.ConditionBudgetExceeded))
	if budget == nil || budget.Status != string(lifecycle.ConditionTrue) || budget.Reason != "RuntimeTokenBudgetExceeded" {
		t.Fatalf("BudgetExceeded condition = %+v, want RuntimeTokenBudgetExceeded=True", budget)
	}
	ready := findCondition(got.Status.Conditions, string(lifecycle.ConditionReady))
	if ready == nil || ready.Status != string(lifecycle.ConditionFalse) || ready.Reason != "RuntimeTokenBudgetExceeded" {
		t.Fatalf("Ready condition = %+v, want RuntimeTokenBudgetExceeded=False", ready)
	}
	if got.Status.TokenUsage == nil || got.Status.TokenUsage.TotalTokens != 100 {
		t.Fatalf("token usage = %+v, want persisted total 100", got.Status.TokenUsage)
	}
	if quota.runtimeChecks != 1 || quota.usedTokens != 100 || quota.settles != 1 || quota.actualInput != 60 || quota.actualOutput != 40 {
		t.Fatalf("quota calls = %+v, want one 100-token check and exact 60/40 settlement", quota)
	}
	if dispatcher.terminations != 1 || dispatcher.termReason != "token budget exceeded" {
		t.Fatalf("runtime terminations = %d reason = %q, want one quota termination", dispatcher.terminations, dispatcher.termReason)
	}
	events, err := s.ListEvents(context.Background(), agentInstanceKind, "default", instance.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != 1 || events[0].Type != "Warning" || events[0].Reason != "RuntimeTokenBudgetExceeded" || !strings.Contains(events[0].Message, "100 tokens") {
		t.Fatalf("quota exhaustion events = %+v, want one structured warning", events)
	}
}

func TestAgentInstanceReconciler_RuntimeQuotaConditionSurvivesStatusConflict(t *testing.T) {
	baseStore := newMemStore()
	conflictStore := &instanceLifecycleConflictStore{memStore: baseStore}
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{
		Alive:      true,
		TokenUsage: &v1alpha1.TokenUsage{PromptTokens: 60, CompletionTokens: 40, TotalTokens: 100},
	}}
	quota := &recordingInstanceQuota{runtimeErr: fmt.Errorf("%w: token limit reached", ErrBudgetExceeded)}
	reconciler := NewAgentInstanceReconcilerWithDispatcher(conflictStore, dispatcher, zaptest.NewLogger(t))
	reconciler.SetQuotaEnforcer(quota)
	instance := makeInstance("default", "quota-conflict", "my-agent", v1alpha1.PhaseRunning)
	instance.Status.SessionID = "runtime-session"
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, baseStore, instance)

	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	got := getInstance(t, baseStore, "default", instance.ObjectMeta.Name)
	if budget := findCondition(got.Status.Conditions, string(lifecycle.ConditionBudgetExceeded)); budget != nil {
		t.Fatalf("stale BudgetExceeded condition crossed session conflict = %+v", budget)
	}
	if got.Status.SessionID != "concurrent-session" || got.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("concurrent lifecycle = %+v, want new running session preserved", got.Status)
	}
	if got.Status.StartTime == nil || !got.Status.StartTime.Equal(time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)) ||
		got.Status.TokenUsage == nil || got.Status.TokenUsage.TotalTokens != 12 ||
		got.Status.ToolCalls != 2 || len(got.Status.ToolCallsDetail) != 1 || got.Status.ToolCallsDetail[0].Name != "concurrent-tool" ||
		got.Status.Output != "concurrent output" {
		t.Fatalf("stale session data crossed conflict boundary: %+v", got.Status)
	}
	if ready := findCondition(got.Status.Conditions, string(lifecycle.ConditionReady)); ready == nil || ready.Reason != "ConcurrentSession" {
		t.Fatalf("concurrent Ready condition was lost: %+v", ready)
	}
}

func TestAgentInstanceReconciler_RuntimeQuotaWarningIsIdempotentAcrossRetry(t *testing.T) {
	baseStore := newMemStore()
	failingStore := &updateFailOnceStore{memStore: baseStore}
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{
		Alive:      true,
		TokenUsage: &v1alpha1.TokenUsage{PromptTokens: 60, CompletionTokens: 40, TotalTokens: 100},
	}}
	quota := &recordingInstanceQuota{runtimeErr: fmt.Errorf("%w: token limit reached", ErrBudgetExceeded)}
	reconciler := NewAgentInstanceReconcilerWithDispatcher(failingStore, dispatcher, zaptest.NewLogger(t))
	reconciler.SetQuotaEnforcer(quota)
	instance := makeInstance("default", "quota-warning-retry", "my-agent", v1alpha1.PhaseRunning)
	instance.Status.SessionID = "runtime-session"
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, baseStore, instance)

	request := Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind}
	if _, err := reconciler.Reconcile(context.Background(), request); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("first Reconcile() error = %v, want conflict after warning append", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("retry Reconcile() error = %v", err)
	}
	events, err := baseStore.ListEvents(context.Background(), agentInstanceKind, "default", instance.ObjectMeta.Name)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != 1 || events[0].Reason != "RuntimeTokenBudgetExceeded" {
		t.Fatalf("quota warning events = %+v, want exactly one warning across retry", events)
	}
}

func TestAgentInstanceReconciler_RuntimeQuotaCheckFailureKeepsSessionRunning(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{
		Alive:      true,
		TokenUsage: &v1alpha1.TokenUsage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20},
	}}
	quotaErr := errors.New("limit store unavailable")
	quota := &recordingInstanceQuota{runtimeErr: quotaErr}
	reconciler := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	reconciler.SetQuotaEnforcer(quota)

	instance := makeInstance("default", "quota-check-error", "my-agent", v1alpha1.PhaseRunning)
	instance.Status.SessionID = "runtime-session"
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)

	_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind})
	if !errors.Is(err, quotaErr) {
		t.Fatalf("Reconcile() error = %v, want quota check error", err)
	}
	got := getInstance(t, s, "default", instance.ObjectMeta.Name)
	if got.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("phase = %s, want Running after retryable quota check failure", got.Status.Phase)
	}
	if findCondition(got.Status.Conditions, string(lifecycle.ConditionBudgetExceeded)) != nil {
		t.Fatalf("unexpected BudgetExceeded condition after check failure: %+v", got.Status.Conditions)
	}
	if quota.runtimeChecks != 1 || quota.settles != 0 {
		t.Fatalf("quota calls = %+v, want one check and no settlement", quota)
	}
}

func TestAgentInstanceReconciler_RuntimeQuotaUnderLimitContinuesPolling(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{
		Alive:      true,
		TokenUsage: &v1alpha1.TokenUsage{PromptTokens: 12, CompletionTokens: 8},
	}}
	quota := &recordingInstanceQuota{}
	reconciler := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	reconciler.SetQuotaEnforcer(quota)

	instance := makeInstance("default", "quota-under-limit", "my-agent", v1alpha1.PhaseRunning)
	instance.Status.SessionID = "runtime-session"
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)

	result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("Reconcile() result = %+v, want continued runtime polling", result)
	}
	got := getInstance(t, s, "default", instance.ObjectMeta.Name)
	if got.Status.Phase != v1alpha1.PhaseRunning || got.Status.TokenUsage == nil {
		t.Fatalf("status = %+v, want Running with observed usage", got.Status)
	}
	if quota.runtimeChecks != 1 || quota.usedTokens != 20 || quota.settles != 0 {
		t.Fatalf("quota calls = %+v, want one 20-token check and no settlement", quota)
	}
}

func TestAgentInstanceReconciler_RuntimeQuotaTerminationFailureDoesNotSettle(t *testing.T) {
	s := newMemStore()
	terminateErr := errors.New("runtime cancel unavailable")
	dispatcher := &mockDispatcher{statusSet: true, terminateErr: terminateErr, status: RuntimeStatus{
		Alive:      true,
		TokenUsage: &v1alpha1.TokenUsage{PromptTokens: 60, CompletionTokens: 40, TotalTokens: 100},
	}}
	quota := &recordingInstanceQuota{runtimeErr: fmt.Errorf("%w: token limit reached", ErrBudgetExceeded)}
	reconciler := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	reconciler.SetQuotaEnforcer(quota)
	instance := makeInstance("default", "quota-cancel-failed", "my-agent", v1alpha1.PhaseRunning)
	instance.Status.SessionID = "runtime-session"
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)

	_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind})
	if !errors.Is(err, terminateErr) {
		t.Fatalf("Reconcile() error = %v, want termination error", err)
	}
	got := getInstance(t, s, "default", instance.ObjectMeta.Name)
	if got.Status.Phase != v1alpha1.PhaseRunning || quota.settles != 0 {
		t.Fatalf("status = %+v quota = %+v, want running and unsettled", got.Status, quota)
	}
}

func TestAgentInstanceReconciler_InconsistentRuntimeUsageDoesNotSettle(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{
		Alive:      true,
		TokenUsage: &v1alpha1.TokenUsage{TotalTokens: 100},
	}}
	quota := &recordingInstanceQuota{}
	reconciler := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	reconciler.SetQuotaEnforcer(quota)
	instance := makeInstance("default", "quota-invalid-usage", "my-agent", v1alpha1.PhaseRunning)
	instance.Status.SessionID = "runtime-session"
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)

	_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind})
	if err == nil || !strings.Contains(err.Error(), "runtime token usage is inconsistent") {
		t.Fatalf("Reconcile() error = %v, want inconsistent usage error", err)
	}
	if quota.runtimeChecks != 0 || quota.settles != 0 || dispatcher.terminations != 0 {
		t.Fatalf("quota = %+v dispatcher = %+v, want no enforcement from inconsistent usage", quota, dispatcher)
	}
}

func TestParseAgentInvocationTimeoutAcceptsDurationAndBareSeconds(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{name: "duration", raw: "45s", want: 45 * time.Second},
		{name: "bare seconds", raw: "45", want: 45 * time.Second},
		{name: "fractional seconds", raw: "0.5", want: 500 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseAgentInvocationTimeout(test.raw)
			if err != nil {
				t.Fatalf("parseAgentInvocationTimeout() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("parseAgentInvocationTimeout() = %s, want %s", got, test.want)
			}
		})
	}
}

func storeInstance(t *testing.T, s *memStore, inst v1alpha1.AgentInstance) {
	t.Helper()
	data, err := json.Marshal(inst)
	if err != nil {
		t.Fatalf("marshal instance: %v", err)
	}
	if err := s.Create(context.Background(), "AgentInstance", inst.ObjectMeta.Namespace, inst.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store instance: %v", err)
	}
}

func getInstance(t *testing.T, s *memStore, ns, name string) v1alpha1.AgentInstance {
	t.Helper()
	raw, err := s.Get(context.Background(), "AgentInstance", ns, name)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	var inst v1alpha1.AgentInstance
	if err := json.Unmarshal(raw, &inst); err != nil {
		t.Fatalf("unmarshal instance: %v", err)
	}
	return inst
}

func storeQuotaTestAgent(t *testing.T, s *memStore, namespace, name string, maxTokens int64) {
	t.Helper()
	agent := v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1alpha1.AgentSpec{
			Instructions: v1alpha1.AgentInstructions{Inline: "rules"},
			ModelRef:     v1alpha1.LocalObjectReference{Name: "test-model"},
			Runtime:      &v1alpha1.AgentRuntimeSpec{MaxTokens: &maxTokens},
		},
	}
	storeUpdatedAgent(t, s, agent)
}

func getStoredAgent(t *testing.T, s *memStore, namespace, name string) v1alpha1.Agent {
	t.Helper()
	raw, err := s.Get(context.Background(), "Agent", namespace, name)
	if err != nil {
		t.Fatal(err)
	}
	var agent v1alpha1.Agent
	if err := json.Unmarshal(raw, &agent); err != nil {
		t.Fatal(err)
	}
	return agent
}

func storeUpdatedAgent(t *testing.T, s *memStore, agent v1alpha1.Agent) {
	t.Helper()
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, data); err == nil {
		return
	}
	if err := s.Update(context.Background(), "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, data, agent.ObjectMeta.ResourceVersion); err != nil {
		t.Fatal(err)
	}
}

func TestAgentInstanceReconciler_ReservesAndSettlesRuntimeQuota(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	storeQuotaTestAgent(t, s, "default", "my-agent", 128)
	dispatcher := &mockDispatcher{
		sessionID: "runtime-session",
		statusSet: true,
		status: RuntimeStatus{Alive: true, Completed: true, TokenUsage: &v1alpha1.TokenUsage{
			PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18,
		}},
	}
	quota := &recordingInstanceQuota{}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	r.SetQuotaEnforcer(quota)
	instance := makeInstance("default", "quota-instance", "my-agent", v1alpha1.PhasePending)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)
	request := Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind}

	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("dispatch reconcile: %v", err)
	}
	if quota.checks != 1 || quota.reserves != 1 || quota.reservationID != "agentinstance:inst-uid-001" || quota.inputCeiling != 5 || quota.outputCeiling != 128 {
		t.Fatalf("reservation = %+v, want one reservation at 5/128", quota)
	}
	if dispatcher.called != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", dispatcher.called)
	}
	quota.settleErr = errors.New("quota store unavailable")
	if _, err := r.Reconcile(ctx, request); !errors.Is(err, quota.settleErr) {
		t.Fatalf("completion reconcile error = %v, want settlement error", err)
	}
	if got := getInstance(t, s, "default", instance.ObjectMeta.Name); got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase after settlement failure = %q, want persisted Ready", got.Status.Phase)
	}
	quota.settleErr = nil
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("terminal settlement retry: %v", err)
	}
	if quota.settles != 2 || quota.actualInput != 11 || quota.actualOutput != 7 || dispatcher.called != 1 {
		t.Fatalf("quota=%+v dispatcher calls=%d, want retried exact settlement without redispatch", quota, dispatcher.called)
	}
}

func TestAgentInstanceReconciler_QuotaDenialPreventsDispatch(t *testing.T) {
	s := newMemStore()
	storeQuotaTestAgent(t, s, "default", "my-agent", 128)
	dispatcher := &mockDispatcher{}
	quota := &recordingInstanceQuota{checkErr: errors.New("quota exhausted")}
	auditStore := &memoryGrantAuditStore{}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	r.SetQuotaEnforcer(quota)
	r.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	instance := makeInstance("default", "quota-denied", "my-agent", v1alpha1.PhasePending)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind})
	if err != nil {
		t.Fatalf("quota denial reconcile: %v", err)
	}
	if result.RequeueAfter != 30*time.Second || dispatcher.called != 0 || quota.reserves != 0 {
		t.Fatalf("result=%+v dispatcher=%d quota=%+v", result, dispatcher.called, quota)
	}
	condition := findCondition(getInstance(t, s, "default", instance.ObjectMeta.Name).Status.Conditions, string(lifecycle.ConditionReady))
	if condition == nil || condition.Reason != "QuotaExceeded" {
		t.Fatalf("Ready condition = %+v, want QuotaExceeded", condition)
	}
	if len(auditStore.entries) != 1 {
		t.Fatalf("model evidence count = %d, want 1", len(auditStore.entries))
	}
	var decision observability.TrustDecisionV1
	if err := json.Unmarshal(auditStore.entries[0].Details, &decision); err != nil {
		t.Fatalf("decode quota evidence: %v", err)
	}
	if decision.SchemaVersion != 1 || decision.Plane != "model" || decision.Outcome != "denied" || decision.ReasonCode != "quota_exhausted" || decision.Request.TargetRef.Name != "test-model" || decision.Execution.TargetReached == nil || *decision.Execution.TargetReached {
		t.Fatalf("quota evidence = %#v", decision)
	}
	if strings.Contains(string(auditStore.entries[0].Details), "quota exhausted") {
		t.Fatalf("quota evidence leaked provider detail: %s", auditStore.entries[0].Details)
	}
}

// TC-0091: reject a configured session ceiling above the per-agent limit
// before runtime dispatch or token reservation.
func TestAgentInstanceReconciler_PerSessionCeilingDenialPreventsDispatch(t *testing.T) {
	s := newMemStore()
	storeQuotaTestAgent(t, s, "default", "my-agent", 128)
	dispatcher := &mockDispatcher{}
	quota := &recordingInstanceQuota{budgetErr: fmt.Errorf("%w: configured ceiling exceeds per-agent limit", ErrBudgetExceeded)}
	reconciler := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	reconciler.SetQuotaEnforcer(quota)
	instance := makeInstance("default", "ceiling-denied", "my-agent", v1alpha1.PhasePending)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)

	result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 30*time.Second || dispatcher.called != 0 || quota.reserves != 0 || quota.budgets != 1 {
		t.Fatalf("result=%+v dispatcher=%d quota=%+v, want pre-dispatch ceiling denial", result, dispatcher.called, quota)
	}
	condition := findCondition(getInstance(t, s, "default", instance.ObjectMeta.Name).Status.Conditions, string(lifecycle.ConditionReady))
	if condition == nil || condition.Reason != "QuotaExceeded" {
		t.Fatalf("Ready condition = %+v, want QuotaExceeded", condition)
	}
}

func TestAgentInstanceReconciler_QuotaDenialAuditFailurePreventsDispatch(t *testing.T) {
	s := newMemStore()
	storeQuotaTestAgent(t, s, "default", "my-agent", 128)
	dispatcher := &mockDispatcher{}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	r.SetQuotaEnforcer(&recordingInstanceQuota{checkErr: errors.New("quota exhausted")})
	r.SetAuditLog(observability.NewCognitiveAuditLog(&memoryGrantAuditStore{err: errors.New("audit unavailable")}))
	instance := makeInstance("default", "quota-audit-failed", "my-agent", v1alpha1.PhasePending)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind})
	if err == nil || !strings.Contains(err.Error(), "persist quota-denied model evidence") {
		t.Fatalf("reconcile error = %v, want audit persistence failure", err)
	}
	if dispatcher.called != 0 {
		t.Fatalf("dispatcher calls = %d, want 0", dispatcher.called)
	}
}

func TestAgentInstanceReconciler_ModelDenialPreventsWarmRuntimeMaterialization(t *testing.T) {
	s := newMemStore()
	agent := v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "my-agent", Namespace: "default"},
		Spec:       v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "restricted-model"}},
	}
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), "Agent", "default", agent.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	dispatcher := &mockDispatcher{}
	quota := &recordingInstanceQuota{}
	authorizer := &instanceModelAuthorizerStub{err: errors.New("model grant revoked")}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	r.SetQuotaEnforcer(quota)
	r.SetModelAuthorizer(authorizer)
	instance := makeInstance("default", "model-denied", "my-agent", v1alpha1.PhasePending)
	instance.Spec.SessionContext = nil
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind})
	if err != nil {
		t.Fatalf("model denial reconcile: %v", err)
	}
	if result.RequeueAfter != 30*time.Second || dispatcher.called != 0 || quota.checks != 0 || quota.reserves != 0 {
		t.Fatalf("result=%+v dispatcher=%d quota=%+v", result, dispatcher.called, quota)
	}
	if authorizer.calls != 1 || authorizer.namespace != "default" || authorizer.agent != "my-agent" || authorizer.model != "restricted-model" {
		t.Fatalf("authorizer = %+v", authorizer)
	}
	condition := findCondition(getInstance(t, s, "default", instance.ObjectMeta.Name).Status.Conditions, string(lifecycle.ConditionReady))
	if condition == nil || condition.Reason != "ModelAccessDenied" {
		t.Fatalf("Ready condition = %+v, want ModelAccessDenied", condition)
	}
}

func TestAgentInstanceReconciler_RecordsTerminalTrustOutcomeOnce(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	for _, profile := range []v1alpha1.TrustProfile{
		{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "my-agent-trust", Namespace: "default"},
			Spec:       v1alpha1.TrustProfileSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "my-agent"}, InitialScore: 50},
		},
		{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "other-agent-trust", Namespace: "default"},
			Spec:       v1alpha1.TrustProfileSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "other-agent"}, InitialScore: 60},
		},
	} {
		data, err := json.Marshal(profile)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Create(ctx, trustProfileKind, "default", profile.ObjectMeta.Name, data); err != nil {
			t.Fatal(err)
		}
	}
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true, Completed: true}}
	trust := NewTrustProfileReconciler(s, zaptest.NewLogger(t))
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	r.SetTrustRecorder(trust)
	instance := makeInstance("default", "trust-instance", "my-agent", v1alpha1.PhaseRunning)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)
	request := Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind}

	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("complete instance: %v", err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("repeat terminal reconcile: %v", err)
	}
	profile, err := trust.GetTrustProfile(ctx, "default", "my-agent-trust")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Status.CurrentScore != 51 || profile.Status.TotalEvaluations != 1 || len(profile.Status.ScoreHistory) != 1 {
		t.Fatalf("matching trust status = %+v, want score 51 and one evaluation", profile.Status)
	}
	if profile.Status.ScoreHistory[0].Reason != "agentinstance:inst-uid-001:success" {
		t.Fatalf("score history reason = %q, want stable instance event ID", profile.Status.ScoreHistory[0].Reason)
	}
	other, err := trust.GetTrustProfile(ctx, "default", "other-agent-trust")
	if err != nil {
		t.Fatal(err)
	}
	if other.Status.CurrentScore != 0 || other.Status.TotalEvaluations != 0 {
		t.Fatalf("unrelated trust status changed: %+v", other.Status)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TC-0020: AgentInstances transition into running lifecycle phase.
func TestAgentInstanceReconciler_PendingToRunning(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{sessionID: "session-abc"}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "my-agent-12345678", "my-agent", v1alpha1.PhasePending)
	storeInstance(t, s, inst)

	ctx := context.Background()

	// First reconcile: adds finalizer.
	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "my-agent-12345678", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	// Verify finalizer was added.
	afterFinalizer := getInstance(t, s, "default", "my-agent-12345678")
	if len(afterFinalizer.ObjectMeta.Finalizers) != 1 || afterFinalizer.ObjectMeta.Finalizers[0] != AgentInstanceFinalizer {
		t.Fatalf("expected finalizer %q, got %v", AgentInstanceFinalizer, afterFinalizer.ObjectMeta.Finalizers)
	}

	// Second reconcile: dispatches to runtime, transitions to Running.
	result, err = r.Reconcile(ctx, Request{Namespace: "default", Name: "my-agent-12345678", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("unexpected requeue")
	}

	got := getInstance(t, s, "default", "my-agent-12345678")
	if got.Status.Phase != v1alpha1.PhaseRunning {
		t.Errorf("expected phase Running, got %s", got.Status.Phase)
	}
	if got.Status.SessionID != "session-abc" {
		t.Errorf("expected sessionID session-abc, got %s", got.Status.SessionID)
	}
	if got.Status.StartTime == nil {
		t.Error("expected StartTime to be set")
	}
	if dispatcher.called != 1 {
		t.Errorf("expected dispatcher called once, got %d", dispatcher.called)
	}

	// Verify Ready=True condition was set.
	var foundReady bool
	for _, c := range got.Status.Conditions {
		if c.Type == "Ready" {
			foundReady = true
			if c.Status != "True" {
				t.Errorf("expected Ready condition True, got %s", c.Status)
			}
			if c.Reason != "Dispatched" {
				t.Errorf("expected reason Dispatched, got %s", c.Reason)
			}
		}
	}
	if !foundReady {
		t.Error("expected Ready condition to be set")
	}
}

// TC-0033: SecretRef-backed values are resolved and injected at dispatch time.
func TestAgentInstanceReconciler_InjectsResolvedSecretsAtDispatch(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-secret-value")

	s := newMemStore()
	dispatcher := &mockDispatcher{sessionID: "session-secret"}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	agent := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "my-agent", Namespace: "default", UID: "agent-uid-001"},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt-4"},
			Instructions: v1alpha1.AgentInstructions{Inline: "You are helpful."},
			Tools:        []v1alpha1.AgentToolRef{{Ref: v1alpha1.LocalObjectReference{Name: "web-search"}}},
		},
	}
	model := v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "gpt-4", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			ClassName:     "openai",
			ModelID:       "gpt-4o",
			CredentialRef: &v1alpha1.LocalObjectReference{Name: "openai-key"},
		},
	}
	tool := v1alpha1.Tool{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "web-search", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			ClassName:  "process-networked",
			Endpoint:   "https://tools.internal/web-search",
			SecretRefs: []v1alpha1.ToolSecretRef{{Name: "web-search-key", Key: "API_KEY"}},
		},
	}
	modelSecret := v1alpha1.SecretRef{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "SecretRef"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "openai-key", Namespace: "default"},
		Spec:       v1alpha1.SecretRefSpec{BackendType: "env", BackendRef: "OPENAI_API_KEY", Key: "api-key"},
	}
	toolSecret := v1alpha1.SecretRef{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "SecretRef"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:        "web-search-key",
			Namespace:   "default",
			Annotations: map[string]string{internalSecretDataAnnotation: `{"api-key":"internal-secret-value"}`},
		},
		Spec: v1alpha1.SecretRefSpec{BackendType: "internal", BackendRef: "tool/web-search", Key: "api-key"},
	}
	identity := makeAgentIdentity("default", defaultAgentIdentityName(agent.ObjectMeta.Name), agent.ObjectMeta.Name)
	identity.Spec.SPIFFEID = "spiffe://kognition.dev/workspace/default/agent/my-agent"
	identity.Status.Phase = v1alpha1.PhaseReady
	identity.ObjectMeta.Labels = map[string]string{autoAgentIdentityLabel: agent.ObjectMeta.Name}
	identity.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{
		APIVersion: v1alpha1.APIVersion,
		Kind:       "Agent",
		Name:       agent.ObjectMeta.Name,
		UID:        agent.ObjectMeta.UID,
		Controller: true,
	}}

	for _, tc := range []struct {
		kind string
		name string
		obj  any
	}{
		{kind: "Agent", name: agent.ObjectMeta.Name, obj: agent},
		{kind: "Model", name: model.ObjectMeta.Name, obj: model},
		{kind: "Tool", name: tool.ObjectMeta.Name, obj: tool},
		{kind: "SecretRef", name: modelSecret.ObjectMeta.Name, obj: modelSecret},
		{kind: "SecretRef", name: toolSecret.ObjectMeta.Name, obj: toolSecret},
		{kind: "AgentIdentity", name: identity.ObjectMeta.Name, obj: identity},
	} {
		data, err := json.Marshal(tc.obj)
		if err != nil {
			t.Fatalf("marshal %s %s: %v", tc.kind, tc.name, err)
		}
		if err := s.Create(context.Background(), tc.kind, "default", tc.name, data); err != nil {
			t.Fatalf("store %s %s: %v", tc.kind, tc.name, err)
		}
	}

	inst := makeInstance("default", "my-agent-secret", "my-agent", v1alpha1.PhasePending)
	storeInstance(t, s, inst)

	ctx := context.Background()
	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "my-agent-secret", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error adding finalizer: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	result, err = r.Reconcile(ctx, Request{Namespace: "default", Name: "my-agent-secret", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected dispatch error: %v", err)
	}
	if result.Requeue {
		t.Fatal("unexpected requeue on successful dispatch")
	}
	if dispatcher.lastInstance == nil {
		t.Fatal("expected dispatcher to receive an instance")
	}
	if got := dispatcher.lastInstance.Spec.SessionContext["model.api-key"]; got != "env-secret-value" {
		t.Fatalf("model secret = %q, want %q", got, "env-secret-value")
	}
	if got := dispatcher.lastInstance.Spec.SessionContext["tool.web-search.API_KEY"]; got != "internal-secret-value" {
		t.Fatalf("tool secret = %q, want %q", got, "internal-secret-value")
	}

	persisted := getInstance(t, s, "default", "my-agent-secret")
	if got := persisted.Spec.SessionContext["input"]; got != "test request" {
		t.Fatalf("persisted input = %q, want original input", got)
	}
	if _, ok := persisted.Spec.SessionContext["model.api-key"]; ok {
		t.Fatalf("persisted session context contains injected model secret: %+v", persisted.Spec.SessionContext)
	}
	if _, ok := persisted.Spec.SessionContext["tool.web-search.API_KEY"]; ok {
		t.Fatalf("persisted session context contains injected tool secret: %+v", persisted.Spec.SessionContext)
	}
	if persisted.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("expected phase Running, got %s", persisted.Status.Phase)
	}
}

func TestAgentInstanceReconciler_InjectsReadyIdentityOnlyIntoDispatchCopy(t *testing.T) {
	s := newMemStore()
	identity := makeAgentIdentity("default", defaultAgentIdentityName("my-agent"), "my-agent")
	identity.Spec.SPIFFEID = "spiffe://kognition.dev/workspace/default/agent/my-agent"
	identity.Status.Phase = v1alpha1.PhaseReady
	storeAgentIdentity(t, s, identity)
	dispatcher := &mockDispatcher{sessionID: "session-identity"}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	r.secrets = nil
	instance := makeInstance("default", "identity-instance", "my-agent", v1alpha1.PhasePending)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind}); err != nil {
		t.Fatalf("reconcile identity dispatch: %v", err)
	}
	if dispatcher.lastInstance == nil || dispatcher.lastInstance.Spec.SessionContext[AgentIdentityContextKey] != identity.Spec.SPIFFEID {
		t.Fatalf("dispatch context = %+v, want SPIFFE URI", dispatcher.lastInstance)
	}
	stored := getInstance(t, s, "default", instance.ObjectMeta.Name)
	if stored.Spec.SessionContext[AgentIdentityContextKey] != "" {
		t.Fatalf("stored session context persisted identity: %+v", stored.Spec.SessionContext)
	}
}

func TestAgentInstanceReconciler_ReplicaPlacementPreservesAgentIdentityAndAccounting(t *testing.T) {
	s := newMemStore()
	maxTokens := int64(128)
	agent := makeTestAgent("tenant-a", "researcher", "", false)
	agent.Spec.ModelRef.Name = "research-model"
	agent.Spec.Runtime = &v1alpha1.AgentRuntimeSpec{MaxTokens: &maxTokens}
	storeAgent(t, s, agent)
	identity := makeAgentIdentity("tenant-a", defaultAgentIdentityName(agent.ObjectMeta.Name), agent.ObjectMeta.Name)
	identity.Spec.SPIFFEID = "spiffe://kognition.dev/workspace/tenant-a/agent/researcher"
	identity.Status.Phase = v1alpha1.PhaseReady
	identity.ObjectMeta.Labels = map[string]string{autoAgentIdentityLabel: agent.ObjectMeta.Name}
	identity.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{
		APIVersion: v1alpha1.APIVersion,
		Kind:       "Agent",
		Name:       agent.ObjectMeta.Name,
		UID:        agent.ObjectMeta.UID,
		Controller: true,
	}}
	storeAgentIdentity(t, s, identity)
	dispatcher := &placementDispatcher{
		mockDispatcher: mockDispatcher{sessionID: "placed-session"},
		replicas:       []RuntimeReplica{{Name: "researcher-7", AgentName: "researcher", Ready: true}},
	}
	scheduler := &recordingScheduler{result: ScheduleResult{
		Assigned: true, AgentName: "researcher", PreferredReplica: "researcher-7",
	}}
	quota := &recordingInstanceQuota{}
	authorizer := &instanceModelAuthorizerStub{}
	reconciler := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	reconciler.secrets = nil
	reconciler.SetScheduler(scheduler)
	reconciler.SetQuotaEnforcer(quota)
	reconciler.SetModelAuthorizer(authorizer)
	reconciler.SetRequireAutomaticIdentity(true)
	instance := makeInstance("tenant-a", "research-session", "researcher", v1alpha1.PhasePending)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)

	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if dispatcher.preferredReplica != "researcher-7" {
		t.Fatalf("preferred replica = %q, want researcher-7", dispatcher.preferredReplica)
	}
	if scheduler.request.AgentName != "researcher" || scheduler.request.Namespace != "tenant-a" || scheduler.request.SessionID != "agentinstance:inst-uid-001" {
		t.Fatalf("schedule request = %+v, want original Agent identity and reservation", scheduler.request)
	}
	if dispatcher.lastInstance == nil || dispatcher.lastInstance.Spec.AgentRef.Name != "researcher" {
		t.Fatalf("dispatch instance = %+v, want original AgentRef", dispatcher.lastInstance)
	}
	dispatchContext := dispatcher.lastInstance.Spec.SessionContext
	for _, key := range []string{
		MemoryCapabilityIdentityNameContextKey,
		ToolCapabilityIdentityNameContextKey,
		ModelCapabilityIdentityNameContextKey,
		AgentDelegationCapabilityIdentityNameContextKey,
	} {
		if dispatchContext[key] != identity.ObjectMeta.Name {
			t.Fatalf("dispatch capability identity %s = %q, want %q", key, dispatchContext[key], identity.ObjectMeta.Name)
		}
	}
	if dispatchContext[AgentIdentityContextKey] != identity.Spec.SPIFFEID {
		t.Fatalf("dispatch SPIFFE ID = %q, want %q", dispatchContext[AgentIdentityContextKey], identity.Spec.SPIFFEID)
	}
	if authorizer.calls != 1 || authorizer.namespace != "tenant-a" || authorizer.agent != "researcher" || authorizer.model != "research-model" {
		t.Fatalf("model authorization = %+v, want original Agent and model", authorizer)
	}
	if quota.checks != 1 || quota.budgets != 1 || quota.reserves != 1 || quota.namespace != "tenant-a" || quota.budgetAgent != "researcher" || quota.reservationID != "agentinstance:inst-uid-001" {
		t.Fatalf("quota accounting = %+v, want original namespace, Agent, and reservation", quota)
	}
	persisted := getInstance(t, s, "tenant-a", instance.ObjectMeta.Name)
	if persisted.Spec.AgentRef.Name != "researcher" {
		t.Fatalf("persisted AgentRef = %q, want researcher", persisted.Spec.AgentRef.Name)
	}
	for _, key := range []string{
		AgentIdentityContextKey,
		MemoryCapabilityIdentityNameContextKey,
		ToolCapabilityIdentityNameContextKey,
		ModelCapabilityIdentityNameContextKey,
		AgentDelegationCapabilityIdentityNameContextKey,
	} {
		if persisted.Spec.SessionContext[key] != "" {
			t.Fatalf("persisted session context contains ephemeral identity %s: %#v", key, persisted.Spec.SessionContext)
		}
	}
}

func TestAgentInstanceReconciler_UserCreatedInstancePrefersAutomaticIdentityForCapabilities(t *testing.T) {
	s := newMemStore()
	agent := makeTestAgent("default", "my-agent", "", false)
	agent.Spec.Tools = []v1alpha1.AgentToolRef{{Ref: v1alpha1.LocalObjectReference{Name: "calculator"}}}
	storeAgent(t, s, agent)
	identity := makeAgentIdentity("default", defaultAgentIdentityName("my-agent"), "my-agent")
	identity.Spec.SPIFFEID = "spiffe://kognition.dev/workspace/default/agent/my-agent"
	identity.Status.Phase = v1alpha1.PhaseReady
	identity.ObjectMeta.Labels = map[string]string{autoAgentIdentityLabel: "my-agent"}
	identity.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{
		APIVersion: v1alpha1.APIVersion,
		Kind:       "Agent",
		Name:       agent.ObjectMeta.Name,
		UID:        agent.ObjectMeta.UID,
		Controller: true,
	}}
	storeAgentIdentity(t, s, identity)
	dispatcher := &mockDispatcher{sessionID: "user-session"}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	r.secrets = nil
	instance := makeInstance("default", "user-created-instance", "my-agent", v1alpha1.PhasePending)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	instance.ObjectMeta.Annotations = nil
	storeInstance(t, s, instance)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind}); err != nil {
		t.Fatal(err)
	}
	if dispatcher.lastInstance == nil {
		t.Fatal("dispatcher did not receive the user-created instance")
	}
	context := dispatcher.lastInstance.Spec.SessionContext
	if context[AgentIdentityContextKey] != identity.Spec.SPIFFEID || context[ToolCapabilityIdentityNameContextKey] != identity.ObjectMeta.Name {
		t.Fatalf("dispatch context = %#v, want automatic identity and Tool capability binding", context)
	}
	persisted := getInstance(t, s, "default", instance.ObjectMeta.Name)
	if persisted.Spec.SessionContext[ToolCapabilityIdentityNameContextKey] != "" {
		t.Fatalf("persisted session context contains ephemeral capability identity: %#v", persisted.Spec.SessionContext)
	}
}

func TestAgentInstanceReconciler_DeclaredIdentityMustBeReady(t *testing.T) {
	s := newMemStore()
	agent := makeTestAgent("default", "my-agent", "", false)
	storeAgent(t, s, agent)
	identity := makeAgentIdentity("default", defaultAgentIdentityName("my-agent"), "my-agent")
	identity.ObjectMeta.Labels = map[string]string{autoAgentIdentityLabel: "my-agent"}
	identity.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{
		APIVersion: v1alpha1.APIVersion,
		Kind:       "Agent",
		Name:       agent.ObjectMeta.Name,
		UID:        agent.ObjectMeta.UID,
	}}
	storeAgentIdentity(t, s, identity)
	dispatcher := &mockDispatcher{}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	r.secrets = nil
	instance := makeInstance("default", "unready-identity", "my-agent", v1alpha1.PhasePending)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	instance.ObjectMeta.Annotations = map[string]string{
		requireAgentIdentityAnnotation:  "true",
		autoAgentIdentityNameAnnotation: defaultAgentIdentityName("my-agent"),
	}
	storeInstance(t, s, instance)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind})
	if err != nil {
		t.Fatalf("reconcile unready identity: %v", err)
	}
	if result.RequeueAfter == 0 || dispatcher.called != 0 {
		t.Fatalf("result=%+v dispatcher calls=%d, want identity wait without dispatch", result, dispatcher.called)
	}
	condition := findCondition(getInstance(t, s, "default", instance.ObjectMeta.Name).Status.Conditions, string(lifecycle.ConditionReady))
	if condition == nil || condition.Reason != "IdentityPending" || !strings.Contains(condition.Message, "AgentIdentity") {
		t.Fatalf("Ready condition = %+v, want IdentityPending", condition)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind}); err != nil {
			t.Fatalf("identity wait reconcile %d: %v", attempt, err)
		}
	}
	stored := getInstance(t, s, "default", instance.ObjectMeta.Name)
	if stored.Status.DispatchAttempts != 0 || stored.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("identity wait state = %#v, want pending with zero dispatch attempts", stored.Status)
	}
}

func TestAgentInstanceReconciler_ControllerReplicaAddsEphemeralCapabilityIdentity(t *testing.T) {
	s := newMemStore()
	agent := makeTestAgent("default", "my-agent", "", false)
	storeAgent(t, s, agent)
	identity := makeAgentIdentity("default", defaultAgentIdentityName("my-agent"), "my-agent")
	identity.Spec.SPIFFEID = "spiffe://kognition.dev/workspace/default/agent/my-agent"
	identity.Status.Phase = v1alpha1.PhaseReady
	identity.ObjectMeta.Labels = map[string]string{autoAgentIdentityLabel: "my-agent"}
	identity.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{
		APIVersion: v1alpha1.APIVersion,
		Kind:       "Agent",
		Name:       agent.ObjectMeta.Name,
		UID:        agent.ObjectMeta.UID,
	}}
	storeAgentIdentity(t, s, identity)
	dispatcher := &mockDispatcher{sessionID: "managed-session"}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	r.secrets = nil
	instance := makeInstance("default", "managed-identity-instance", "my-agent", v1alpha1.PhasePending)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	instance.ObjectMeta.Annotations = map[string]string{
		requireAgentIdentityAnnotation:  "true",
		autoAgentIdentityNameAnnotation: defaultAgentIdentityName("my-agent"),
	}
	storeInstance(t, s, instance)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind}); err != nil {
		t.Fatal(err)
	}
	if dispatcher.lastInstance == nil || dispatcher.lastInstance.Spec.SessionContext[MemoryCapabilityIdentityNameContextKey] != identity.ObjectMeta.Name {
		t.Fatalf("dispatch context = %+v, want ephemeral managed identity name", dispatcher.lastInstance)
	}
	persisted := getInstance(t, s, "default", instance.ObjectMeta.Name)
	if persisted.Spec.SessionContext[MemoryCapabilityIdentityNameContextKey] != "" {
		t.Fatalf("persisted session context contains capability identity: %+v", persisted.Spec.SessionContext)
	}
}

func TestAgentInstanceReconciler_ControllerReplicaWaitsForTerminatingIdentity(t *testing.T) {
	s := newMemStore()
	agent := makeTestAgent("default", "my-agent", "", false)
	storeAgent(t, s, agent)
	identity := makeAgentIdentity("default", defaultAgentIdentityName("my-agent"), "my-agent")
	identity.Spec.SPIFFEID = "spiffe://kognition.dev/workspace/default/agent/my-agent"
	identity.Status.Phase = v1alpha1.PhaseReady
	identity.ObjectMeta.Labels = map[string]string{autoAgentIdentityLabel: "my-agent"}
	identity.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{
		APIVersion: v1alpha1.APIVersion,
		Kind:       "Agent",
		Name:       agent.ObjectMeta.Name,
		UID:        agent.ObjectMeta.UID,
	}}
	deletingAt := time.Now().UTC()
	identity.ObjectMeta.DeletionTimestamp = &deletingAt
	storeAgentIdentity(t, s, identity)
	dispatcher := &mockDispatcher{}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	r.secrets = nil
	instance := makeInstance("default", "terminating-identity", "my-agent", v1alpha1.PhasePending)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	instance.ObjectMeta.Annotations = map[string]string{
		requireAgentIdentityAnnotation:  "true",
		autoAgentIdentityNameAnnotation: defaultAgentIdentityName("my-agent"),
	}
	storeInstance(t, s, instance)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("terminating identity result = %+v, err = %v", result, err)
	}
	stored := getInstance(t, s, "default", instance.ObjectMeta.Name)
	if dispatcher.called != 0 || stored.Status.DispatchAttempts != 0 {
		t.Fatalf("terminating identity state = dispatcher calls %d, status %#v", dispatcher.called, stored.Status)
	}
}

func TestAgentInstanceReconciler_ControllerReplicaRejectsReadyManualIdentity(t *testing.T) {
	s := newMemStore()
	manualIdentity := makeAgentIdentity("default", defaultAgentIdentityName("my-agent"), "my-agent")
	manualIdentity.Spec.SPIFFEID = "spiffe://kognition.dev/workspace/default/agent/my-agent"
	manualIdentity.Status.Phase = v1alpha1.PhaseReady
	storeAgentIdentity(t, s, manualIdentity)
	storeAgent(t, s, makeTestAgent("default", "my-agent", "", false))
	dispatcher := &mockDispatcher{}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	r.secrets = nil
	instance := makeInstance("default", "manual-identity-managed-replica", "my-agent", v1alpha1.PhasePending)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	instance.ObjectMeta.Annotations = map[string]string{
		requireAgentIdentityAnnotation:  "true",
		autoAgentIdentityNameAnnotation: defaultAgentIdentityName("my-agent"),
	}
	storeInstance(t, s, instance)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("manual identity managed replica result = %+v, err = %v", result, err)
	}
	if dispatcher.called != 0 {
		t.Fatalf("dispatcher calls = %d, want 0", dispatcher.called)
	}
}

func TestAgentInstanceReconciler_LegacyInstanceIgnoresUnreadyIdentity(t *testing.T) {
	s := newMemStore()
	storeAgentIdentity(t, s, makeAgentIdentity("default", "my-agent-id", "my-agent"))
	dispatcher := &mockDispatcher{sessionID: "legacy-session"}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	r.secrets = nil
	instance := makeInstance("default", "legacy-unready-identity", "my-agent", v1alpha1.PhasePending)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind}); err != nil {
		t.Fatal(err)
	}
	stored := getInstance(t, s, "default", instance.ObjectMeta.Name)
	if dispatcher.called != 1 || stored.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("legacy instance state = dispatcher calls %d, status %#v", dispatcher.called, stored.Status)
	}
}

func TestAgentInstanceReconciler_RequiredIdentityProtectsUnannotatedInstance(t *testing.T) {
	for _, identityState := range []string{"missing", "manual", "pending", "ready"} {
		t.Run(identityState, func(t *testing.T) {
			storage := newMemStore()
			agent := makeTestAgent("default", "my-agent", "", false)
			storeAgent(t, storage, agent)
			if identityState != "missing" {
				identity := makeAgentIdentity("default", defaultAgentIdentityName("my-agent"), "my-agent")
				identity.Spec.SPIFFEID = "spiffe://kognition.dev/workspace/default/agent/my-agent"
				identity.Status.Phase = v1alpha1.PhaseReady
				if identityState != "manual" {
					identity.ObjectMeta.Labels = map[string]string{autoAgentIdentityLabel: agent.ObjectMeta.Name}
					identity.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{
						APIVersion: v1alpha1.APIVersion, Kind: "Agent", Name: agent.ObjectMeta.Name, UID: agent.ObjectMeta.UID, Controller: true,
					}}
				}
				if identityState == "pending" {
					identity.Status.Phase = v1alpha1.PhasePending
				}
				storeAgentIdentity(t, storage, identity)
			}
			dispatcher := &mockDispatcher{sessionID: "managed-session"}
			reconciler := NewAgentInstanceReconcilerWithDispatcher(storage, dispatcher, zaptest.NewLogger(t))
			reconciler.SetRequireAutomaticIdentity(true)
			reconciler.secrets = nil
			instance := makeInstance("default", "unannotated-instance", "my-agent", v1alpha1.PhasePending)
			instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
			storeInstance(t, storage, instance)
			result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name})
			if err != nil {
				t.Fatal(err)
			}
			persisted := getInstance(t, storage, "default", instance.ObjectMeta.Name)
			if identityState == "ready" {
				if dispatcher.called != 1 || persisted.Status.Phase != v1alpha1.PhaseRunning {
					t.Fatalf("ready managed identity did not dispatch: calls=%d phase=%s", dispatcher.called, persisted.Status.Phase)
				}
			} else if dispatcher.called != 0 || result.RequeueAfter == 0 || persisted.Status.DispatchAttempts != 0 || persisted.Status.Phase != v1alpha1.PhasePending {
				t.Fatalf("identity gate failed: calls=%d result=%+v status=%+v", dispatcher.called, result, persisted.Status)
			}
		})
	}
}

func TestAgentInstanceReconciler_LegacyInstanceWithoutIdentityStillDispatches(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{sessionID: "legacy-session"}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))
	r.secrets = nil
	instance := makeInstance("default", "missing-identity", "my-agent", v1alpha1.PhasePending)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name, Kind: agentInstanceKind}); err != nil {
		t.Fatal(err)
	}
	stored := getInstance(t, s, "default", instance.ObjectMeta.Name)
	if dispatcher.called != 1 || stored.Status.DispatchAttempts != 0 || stored.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("legacy identity state = dispatcher calls %d, status %#v", dispatcher.called, stored.Status)
	}
}

func TestAgentInstanceReconciler_UnreadableDispatchDependencyRetriesThenFails(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{sessionID: "must-not-dispatch"}
	r := NewAgentInstanceReconcilerWithConfig(s, dispatcher, zaptest.NewLogger(t), AgentInstanceReconcilerConfig{
		DispatchRetryLimit:  1,
		DispatchBackoffBase: time.Second,
	})
	if err := s.Create(context.Background(), "Agent", "default", "unreadable", []byte("{")); err != nil {
		t.Fatalf("store malformed Agent: %v", err)
	}

	inst := makeInstance("default", "unreadable-agent", "unreadable", v1alpha1.PhasePending)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, inst)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "unreadable-agent"})
	if err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("first Reconcile() RequeueAfter = %v, want %v", result.RequeueAfter, time.Second)
	}
	retrying := getInstance(t, s, "default", "unreadable-agent")
	if retrying.Status.Phase != v1alpha1.PhasePending || retrying.Status.DispatchAttempts != 1 {
		t.Fatalf("status after retry = %+v, want Pending with one attempt", retrying.Status)
	}
	if condition := findCondition(retrying.Status.Conditions, "Ready"); condition == nil || condition.Reason != "DispatchRetrying" {
		t.Fatalf("Ready condition after retry = %#v, want DispatchRetrying", condition)
	}

	result, err = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "unreadable-agent"})
	if err != nil {
		t.Fatalf("terminal Reconcile() error = %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("terminal Reconcile() result = %+v, want no requeue", result)
	}
	failed := getInstance(t, s, "default", "unreadable-agent")
	if failed.Status.Phase != v1alpha1.PhaseFailed || failed.Status.DispatchAttempts != 2 {
		t.Fatalf("terminal status = %+v, want Failed after two attempts", failed.Status)
	}
	if condition := findCondition(failed.Status.Conditions, "Ready"); condition == nil || condition.Reason != "DispatchFailed" {
		t.Fatalf("terminal Ready condition = %#v, want DispatchFailed", condition)
	}
	if dispatcher.called != 0 {
		t.Fatalf("dispatcher calls = %d, want 0 when preparation fails", dispatcher.called)
	}
	if got := failed.Spec.SessionContext["input"]; got != "test request" || len(failed.Spec.SessionContext) != 1 {
		t.Fatalf("persisted session context = %+v, want only original input", failed.Spec.SessionContext)
	}
}

func TestAgentInstanceReconciler_UsageAccountingUncertaintyDoesNotRedispatch(t *testing.T) {
	s := newMemStore()
	storeAgent(t, s, makeAgent("default", "my-agent", 1))
	dispatcher := &mockDispatcher{err: fmt.Errorf("%w: store unavailable", ErrDispatchAccountingUncertain)}
	quota := &recordingInstanceQuota{}
	r := NewAgentInstanceReconcilerWithConfig(s, dispatcher, zaptest.NewLogger(t), AgentInstanceReconcilerConfig{DispatchRetryLimit: 5, DispatchBackoffBase: time.Second})
	r.SetQuotaEnforcer(quota)
	instance := makeInstance("default", "uncertain-accounting", "my-agent", v1alpha1.PhasePending)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, instance)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name})
	if err != nil {
		t.Fatal(err)
	}
	if result.Requeue || result.RequeueAfter != 0 || dispatcher.called != 1 {
		t.Fatalf("result=%+v dispatches=%d, want terminal single dispatch", result, dispatcher.called)
	}
	stored := getInstance(t, s, "default", instance.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseFailed || stored.Status.DispatchAttempts != 1 {
		t.Fatalf("status = %+v, want Failed after one uncertain attempt", stored.Status)
	}
	if condition := findCondition(stored.Status.Conditions, "Ready"); condition == nil || condition.Reason != "UsageAccountingUncertain" {
		t.Fatalf("Ready condition = %#v", condition)
	}
	if quota.settles != 0 {
		t.Fatalf("quota settlements = %d, want reservation retained", quota.settles)
	}
	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name}); err != nil {
		t.Fatal(err)
	}
	if dispatcher.called != 1 || quota.settles != 0 {
		t.Fatalf("second reconcile dispatches=%d settlements=%d, want no replay/refund", dispatcher.called, quota.settles)
	}
}

func TestAgentInstanceReconciler_StatusUncertaintyRetainsReservation(t *testing.T) {
	storage := newMemStore()
	dispatcher := &mockDispatcher{statusErr: fmt.Errorf("%w: lost runtime result", ErrDispatchAccountingUncertain)}
	quota := &recordingInstanceQuota{}
	reconciler := NewAgentInstanceReconcilerWithDispatcher(storage, dispatcher, zaptest.NewLogger(t))
	reconciler.SetQuotaEnforcer(quota)
	instance := makeInstance("default", "uncertain-status", "my-agent", v1alpha1.PhaseRunning)
	instance.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, storage, instance)
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: instance.ObjectMeta.Name}); err != nil {
			t.Fatal(err)
		}
	}
	stored := getInstance(t, storage, "default", instance.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseFailed || !hasUsageAccountingUncertainCondition(stored.Status.Conditions) {
		t.Fatalf("status = %+v", stored.Status)
	}
	if quota.settles != 0 || dispatcher.called != 0 || dispatcher.statusCalled != 1 {
		t.Fatalf("settles=%d dispatches=%d polls=%d", quota.settles, dispatcher.called, dispatcher.statusCalled)
	}
}

func TestAgentInstanceReconciler_DispatchPreparationRecoversAfterDependencyRepair(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{sessionID: "recovered-session"}
	r := NewAgentInstanceReconcilerWithConfig(s, dispatcher, zaptest.NewLogger(t), AgentInstanceReconcilerConfig{
		DispatchRetryLimit:  2,
		DispatchBackoffBase: time.Second,
	})
	if err := s.Create(context.Background(), "Agent", "default", "repairable", []byte("{")); err != nil {
		t.Fatalf("store malformed Agent: %v", err)
	}
	inst := makeInstance("default", "repairable-agent", "repairable", v1alpha1.PhasePending)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, inst)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "repairable-agent"})
	if err != nil {
		t.Fatalf("preparation failure Reconcile() error = %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("preparation failure RequeueAfter = %v, want %v", result.RequeueAfter, time.Second)
	}

	if err := s.Delete(context.Background(), "Agent", "default", "repairable"); err != nil {
		t.Fatalf("delete malformed Agent: %v", err)
	}
	agent := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "repairable", Namespace: "default"},
	}
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("marshal repaired Agent: %v", err)
	}
	if err := s.Create(context.Background(), "Agent", "default", "repairable", data); err != nil {
		t.Fatalf("store repaired Agent: %v", err)
	}

	result, err = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "repairable-agent"})
	if err != nil {
		t.Fatalf("recovery Reconcile() error = %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("recovery Reconcile() result = %+v, want no requeue", result)
	}
	recovered := getInstance(t, s, "default", "repairable-agent")
	if recovered.Status.Phase != v1alpha1.PhaseRunning || recovered.Status.SessionID != "recovered-session" {
		t.Fatalf("recovered status = %+v, want Running recovered-session", recovered.Status)
	}
	if recovered.Status.DispatchAttempts != 0 {
		t.Fatalf("DispatchAttempts after recovery = %d, want 0", recovered.Status.DispatchAttempts)
	}
	if dispatcher.called != 1 {
		t.Fatalf("dispatcher calls = %d, want 1 after dependency repair", dispatcher.called)
	}
}

func TestAgentInstanceReconciler_DispatchFailure(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{err: errors.New("runtime unavailable")}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithConfig(s, dispatcher, logger, AgentInstanceReconcilerConfig{DispatchRetryLimit: 1})

	inst := makeInstance("default", "fail-inst", "my-agent", v1alpha1.PhasePending)
	storeInstance(t, s, inst)

	ctx := context.Background()

	// First reconcile: adds finalizer.
	firstResult, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "fail-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error adding finalizer: %v", err)
	}
	if !firstResult.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	// Second reconcile: first retry.
	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "fail-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatal("expected requeue-after on retry")
	}

	// Third reconcile: retry budget exhausted, transitions to Failed.
	result, err = r.Reconcile(ctx, Request{Namespace: "default", Name: "fail-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Fatal("unexpected requeue on terminal failure")
	}

	got := getInstance(t, s, "default", "fail-inst")
	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Errorf("expected phase Failed, got %s", got.Status.Phase)
	}

	// Verify Ready=False condition with DispatchFailed reason.
	var foundReady bool
	for _, c := range got.Status.Conditions {
		if c.Type == "Ready" {
			foundReady = true
			if c.Status != "False" {
				t.Errorf("expected Ready condition False, got %s", c.Status)
			}
			if c.Reason != "DispatchFailed" {
				t.Errorf("expected reason DispatchFailed, got %s", c.Reason)
			}
			if c.Message != "failed to dispatch to runtime after 2 attempts: runtime unavailable" {
				t.Errorf("expected message 'failed to dispatch to runtime after 2 attempts: runtime unavailable', got %s", c.Message)
			}
		}
	}
	if !foundReady {
		t.Error("expected Ready condition to be set on failure")
	}
}

func TestAgentInstanceReconciler_RunningIsStable(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{sessionID: "session-xyz"}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "running-inst", "my-agent", v1alpha1.PhaseRunning)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	inst.Status.SessionID = "session-xyz"
	now := time.Now().UTC()
	inst.Status.StartTime = &now
	storeInstance(t, s, inst)
	before := getInstance(t, s, "default", "running-inst")

	ctx := context.Background()
	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "running-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatal("expected requeue after running status poll")
	}
	// Dispatcher should not be called for already-running instances.
	if dispatcher.called != 0 {
		t.Errorf("expected dispatcher not called, got %d", dispatcher.called)
	}
	if dispatcher.statusCalled != 1 {
		t.Errorf("expected status polled once, got %d", dispatcher.statusCalled)
	}
	after := getInstance(t, s, "default", "running-inst")
	if after.ObjectMeta.ResourceVersion != before.ObjectMeta.ResourceVersion {
		t.Fatalf("unchanged running poll advanced resource version from %d to %d", before.ObjectMeta.ResourceVersion, after.ObjectMeta.ResourceVersion)
	}
}

func TestAgentInstanceReconciler_Deletion(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{sessionID: "session-del"}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "del-inst", "my-agent", v1alpha1.PhaseRunning)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	now := time.Now().UTC()
	inst.ObjectMeta.DeletionTimestamp = &now
	storeInstance(t, s, inst)

	ctx := context.Background()
	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "del-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("unexpected requeue")
	}

	got := getInstance(t, s, "default", "del-inst")
	if len(got.ObjectMeta.Finalizers) != 0 {
		t.Errorf("expected finalizers removed, got %v", got.ObjectMeta.Finalizers)
	}
}

func TestAgentInstanceReconciler_DeletionConflictPreservesConcurrentState(t *testing.T) {
	baseStore := newMemStore()
	conflictStore := &instanceConflictStore{memStore: baseStore, updates: 1}
	r := NewAgentInstanceReconcilerWithDispatcher(conflictStore, &mockDispatcher{}, zaptest.NewLogger(t))

	inst := makeInstance("default", "conflicted-deletion", "my-agent", v1alpha1.PhaseRunning)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	now := time.Now().UTC()
	inst.ObjectMeta.DeletionTimestamp = &now
	storeInstance(t, baseStore, inst)

	result, err := r.Reconcile(context.Background(), Request{
		Namespace: "default",
		Name:      inst.ObjectMeta.Name,
		Kind:      agentInstanceKind,
	})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result != (Result{}) {
		t.Fatalf("Reconcile() result = %+v, want empty result", result)
	}
	if conflictStore.updates != 3 {
		t.Fatalf("update attempts = %d, want conflict plus retry", conflictStore.updates-1)
	}

	got := getInstance(t, baseStore, "default", inst.ObjectMeta.Name)
	if hasFinalizer(got.ObjectMeta.Finalizers, AgentInstanceFinalizer) {
		t.Fatalf("owned finalizer was not removed: %v", got.ObjectMeta.Finalizers)
	}
	if !hasFinalizer(got.ObjectMeta.Finalizers, "example.com/foreign") {
		t.Fatalf("concurrent foreign finalizer was lost: %v", got.ObjectMeta.Finalizers)
	}
	if got.Status.ProbeStatus == nil || got.Status.ProbeStatus.ConsecutiveHealthFailures != 2 {
		t.Fatalf("concurrent probe status was lost: %#v", got.Status.ProbeStatus)
	}
	if got.Status.RestartCount != 3 {
		t.Fatalf("concurrent restart count was lost: %d", got.Status.RestartCount)
	}
}

func TestAgentInstanceReconciler_NotFound(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	ctx := context.Background()
	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "nonexistent", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("unexpected requeue")
	}
}

// TC-0022: AgentInstance status reports token usage and tool-call counts.
func TestAgentInstanceReconciler_TokenUsageReported(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{
		sessionID: "session-tok",
		statusSet: true,
		status: RuntimeStatus{
			Alive: true,
			TokenUsage: &v1alpha1.TokenUsage{
				PromptTokens:     100,
				CompletionTokens: 50,
				TotalTokens:      150,
			},
			ToolCalls:       3,
			ToolCallsDetail: []v1alpha1.ToolCallDetail{{Name: "calculator", Result: "391"}},
		},
	}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "tok-inst", "my-agent", v1alpha1.PhaseRunning)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	inst.Status.SessionID = "session-tok"
	now := time.Now().UTC()
	inst.Status.StartTime = &now
	inst.Status.TokenUsage = &v1alpha1.TokenUsage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}
	inst.Status.ToolCalls = 3
	inst.Status.ToolCallsDetail = []v1alpha1.ToolCallDetail{{Name: "calculator", Result: "391"}}
	storeInstance(t, s, inst)

	ctx := context.Background()

	// Reconcile running instance — should preserve token usage fields.
	_, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "tok-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getInstance(t, s, "default", "tok-inst")
	if got.Status.TokenUsage == nil {
		t.Fatal("expected token usage to be preserved")
	}
	if got.Status.TokenUsage.TotalTokens != 150 {
		t.Errorf("expected total tokens 150, got %d", got.Status.TokenUsage.TotalTokens)
	}
	if got.Status.ToolCalls != 3 {
		t.Errorf("expected tool calls 3, got %d", got.Status.ToolCalls)
	}
}

func TestAgentInstanceReconciler_UnknownPhaseResetsToPending(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{sessionID: "session-unk"}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "unk-inst", "my-agent", v1alpha1.Phase("SomeUnknownPhase"))
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s, inst)

	ctx := context.Background()

	// Reconcile: unknown phase should reset to Pending and requeue.
	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "unk-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected requeue after unknown phase reset")
	}

	got := getInstance(t, s, "default", "unk-inst")
	if got.Status.Phase != v1alpha1.PhasePending {
		t.Errorf("expected phase Pending, got %s", got.Status.Phase)
	}
}

func TestAgentInstanceReconciler_DeletionWithoutFinalizer(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "del-nofin", "my-agent", v1alpha1.PhaseRunning)
	now := time.Now().UTC()
	inst.ObjectMeta.DeletionTimestamp = &now
	// No finalizer present.
	storeInstance(t, s, inst)

	ctx := context.Background()

	// Reconcile: deletion path with no finalizer should be a no-op update.
	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "del-nofin", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Fatal("unexpected requeue")
	}
}

func TestAgentInstanceReconciler_GetError(t *testing.T) {
	getErr := errors.New("store unavailable")
	s := &faultStore{memStore: newMemStore(), getErr: getErr}
	dispatcher := &mockDispatcher{}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "broken", Kind: "AgentInstance"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, getErr) {
		t.Fatalf("expected wrapped get error, got %v", err)
	}
	if dispatcher.called != 0 {
		t.Fatalf("expected dispatcher not called, got %d", dispatcher.called)
	}
}

func TestAgentInstanceReconciler_UnmarshalError(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, &mockDispatcher{}, logger)

	if err := s.Create(context.Background(), "AgentInstance", "default", "bad-json", []byte("{")); err != nil {
		t.Fatalf("seed invalid json: %v", err)
	}

	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "bad-json", Kind: "AgentInstance"})
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
	if !strings.Contains(err.Error(), "unmarshal agent instance") {
		t.Fatalf("expected unmarshal context, got %v", err)
	}
}

func TestAgentInstanceReconciler_AddFinalizerUpdateError(t *testing.T) {
	updateErr := errors.New("write failed")
	s := &faultStore{memStore: newMemStore(), updateErr: updateErr}
	dispatcher := &mockDispatcher{}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "finalizer-fail", "my-agent", v1alpha1.PhasePending)
	storeInstance(t, s.memStore, inst)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "finalizer-fail", Kind: "AgentInstance"})
	if err == nil {
		t.Fatal("expected add finalizer error")
	}
	if !errors.Is(err, updateErr) {
		t.Fatalf("expected wrapped update error, got %v", err)
	}
	if !strings.Contains(err.Error(), "add finalizer") {
		t.Fatalf("expected add finalizer context, got %v", err)
	}
	if dispatcher.called != 0 {
		t.Fatalf("expected dispatcher not called, got %d", dispatcher.called)
	}
}

func TestAgentInstanceReconciler_UpdateConflictAfterDispatch(t *testing.T) {
	s := &faultStore{memStore: newMemStore()}
	dispatcher := &mockDispatcher{sessionID: "session-conflict"}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "conflict-after-dispatch", "my-agent", v1alpha1.PhasePending)
	storeInstance(t, s.memStore, inst)

	// First reconcile adds finalizer.
	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "conflict-after-dispatch", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error adding finalizer: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	// Second reconcile dispatches successfully but fails to persist due to conflict.
	s.updateErr = store.ErrConflict
	_, err = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "conflict-after-dispatch", Kind: "AgentInstance"})
	if err == nil {
		t.Fatal("expected update conflict error")
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected wrapped conflict error, got %v", err)
	}
	if !strings.Contains(err.Error(), "update after dispatch") {
		t.Fatalf("expected update-after-dispatch context, got %v", err)
	}
	if dispatcher.called != 1 {
		t.Fatalf("expected dispatcher called once, got %d", dispatcher.called)
	}

	// Since persist failed, state remains Pending in the store.
	got := getInstance(t, s.memStore, "default", "conflict-after-dispatch")
	if got.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("expected stored phase Pending after conflict, got %s", got.Status.Phase)
	}
}

func TestAgentInstanceReconciler_DispatchConflictPreservesConcurrentSharedState(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	s := &instanceConflictStore{memStore: baseStore}
	dispatcher := &mockDispatcher{sessionID: "session-preserved"}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))

	inst := makeInstance("default", "shared-conflict", "my-agent", v1alpha1.PhasePending)
	storeInstance(t, baseStore, inst)
	request := Request{Namespace: "default", Name: "shared-conflict", Kind: agentInstanceKind}

	if result, err := r.Reconcile(ctx, request); err != nil || !result.Requeue {
		t.Fatalf("add finalizer: result=%+v err=%v", result, err)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("dispatch after conflict: %v", err)
	}

	got := getInstance(t, baseStore, "default", "shared-conflict")
	if got.Status.Phase != v1alpha1.PhaseRunning || got.Status.SessionID != "session-preserved" {
		t.Fatalf("dispatch state not persisted: phase=%s sessionID=%q", got.Status.Phase, got.Status.SessionID)
	}
	if got.Status.ProbeStatus == nil || got.Status.ProbeStatus.ConsecutiveHealthFailures != 2 {
		t.Fatalf("concurrent probe status was lost: %#v", got.Status.ProbeStatus)
	}
	if got.Status.RestartCount != 3 {
		t.Fatalf("concurrent restart count was lost: %d", got.Status.RestartCount)
	}
	if condition := findCondition(got.Status.Conditions, "ProbeHealthy"); condition == nil || condition.Reason != "ConcurrentProbe" {
		t.Fatalf("concurrent probe condition was lost: %#v", condition)
	}
	if !hasFinalizer(got.ObjectMeta.Finalizers, "example.com/foreign") {
		t.Fatal("concurrent foreign finalizer was lost")
	}
}

func TestAgentInstanceReconciler_RunningConflictPreservesUnchangedLifecycleFields(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	s := &instanceLifecycleConflictStore{memStore: baseStore}
	dispatcher := &mockDispatcher{
		statusSet: true,
		status: RuntimeStatus{
			Alive:      true,
			TokenUsage: &v1alpha1.TokenUsage{TotalTokens: 42},
			ToolCalls:  3,
		},
	}
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, zaptest.NewLogger(t))

	inst := makeInstance("default", "running-shared-conflict", "my-agent", v1alpha1.PhaseRunning)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	inst.Status.SessionID = "original-session"
	storeInstance(t, baseStore, inst)

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: inst.ObjectMeta.Name, Kind: agentInstanceKind}); err != nil {
		t.Fatalf("reconcile running instance after conflict: %v", err)
	}
	if !s.conflicted {
		t.Fatal("expected conflict fixture to execute")
	}

	got := getInstance(t, baseStore, "default", inst.ObjectMeta.Name)
	if got.Status.SessionID != "concurrent-session" || got.Status.DispatchAttempts != 4 {
		t.Fatalf("concurrent lifecycle state was lost: sessionID=%q attempts=%d", got.Status.SessionID, got.Status.DispatchAttempts)
	}
	if got.Status.TokenUsage == nil || got.Status.TokenUsage.TotalTokens != 12 || got.Status.ToolCalls != 2 {
		t.Fatalf("concurrent session usage was overwritten: tokenUsage=%+v toolCalls=%d", got.Status.TokenUsage, got.Status.ToolCalls)
	}
}

func TestAgentInstanceReconciler_DispatchRetriesThenFails(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{err: errors.New("runtime unavailable")}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithConfig(s, dispatcher, logger, AgentInstanceReconcilerConfig{
		DispatchRetryLimit:  3,
		DispatchBackoffBase: 10 * time.Millisecond,
		DispatchBackoffMax:  40 * time.Millisecond,
	})

	inst := makeInstance("default", "retry-inst", "my-agent", v1alpha1.PhasePending)
	storeInstance(t, s, inst)

	ctx := context.Background()

	first, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "retry-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error adding finalizer: %v", err)
	}
	if !first.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	second, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "retry-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error on first retry attempt: %v", err)
	}
	if second.RequeueAfter != 10*time.Millisecond {
		t.Fatalf("expected first retry backoff 10ms, got %v", second.RequeueAfter)
	}

	third, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "retry-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error on second retry attempt: %v", err)
	}
	if third.RequeueAfter != 20*time.Millisecond {
		t.Fatalf("expected second retry backoff 20ms, got %v", third.RequeueAfter)
	}

	fourth, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "retry-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error on third retry attempt: %v", err)
	}
	if fourth.RequeueAfter != 40*time.Millisecond {
		t.Fatalf("expected third retry backoff 40ms, got %v", fourth.RequeueAfter)
	}

	fifth, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "retry-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error on terminal failure attempt: %v", err)
	}
	if fifth.Requeue || fifth.RequeueAfter > 0 {
		t.Fatal("expected terminal failure without requeue")
	}

	got := getInstance(t, s, "default", "retry-inst")
	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected phase Failed after retry budget exhaustion, got %s", got.Status.Phase)
	}
	if got.Status.DispatchAttempts != 4 {
		t.Fatalf("expected dispatch attempts 4, got %d", got.Status.DispatchAttempts)
	}
	if dispatcher.called != 4 {
		t.Fatalf("expected dispatcher called 4 times, got %d", dispatcher.called)
	}
}

func TestAgentInstanceReconciler_DispatchBackoffCappedAtMax(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{err: errors.New("runtime unavailable")}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithConfig(s, dispatcher, logger, AgentInstanceReconcilerConfig{
		DispatchRetryLimit:  5,
		DispatchBackoffBase: 10 * time.Millisecond,
		DispatchBackoffMax:  15 * time.Millisecond,
	})

	inst := makeInstance("default", "retry-cap-inst", "my-agent", v1alpha1.PhasePending)
	storeInstance(t, s, inst)

	ctx := context.Background()

	first, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "retry-cap-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error adding finalizer: %v", err)
	}
	if !first.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	second, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "retry-cap-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error on first retry attempt: %v", err)
	}
	if second.RequeueAfter != 10*time.Millisecond {
		t.Fatalf("expected first retry backoff 10ms, got %v", second.RequeueAfter)
	}

	third, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "retry-cap-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error on second retry attempt: %v", err)
	}
	if third.RequeueAfter != 15*time.Millisecond {
		t.Fatalf("expected capped retry backoff 15ms, got %v", third.RequeueAfter)
	}

	fourth, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "retry-cap-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error on third retry attempt: %v", err)
	}
	if fourth.RequeueAfter != 15*time.Millisecond {
		t.Fatalf("expected capped retry backoff to stay 15ms, got %v", fourth.RequeueAfter)
	}
}

func TestAgentInstanceReconciler_ConfigDefaultsApplied(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)

	rDefault := NewAgentInstanceReconcilerWithDispatcher(s, &mockDispatcher{}, logger)
	if rDefault.config.DispatchRetryLimit != 3 {
		t.Fatalf("expected default DispatchRetryLimit=3, got %d", rDefault.config.DispatchRetryLimit)
	}
	if rDefault.config.DispatchBackoffBase != time.Second {
		t.Fatalf("expected default DispatchBackoffBase=1s, got %v", rDefault.config.DispatchBackoffBase)
	}
	if rDefault.config.DispatchBackoffMax != 30*time.Second {
		t.Fatalf("expected default DispatchBackoffMax=30s, got %v", rDefault.config.DispatchBackoffMax)
	}
	if rDefault.config.RunningPollInterval != 10*time.Second {
		t.Fatalf("expected default RunningPollInterval=10s, got %v", rDefault.config.RunningPollInterval)
	}

	rConfigured := NewAgentInstanceReconcilerWithConfig(s, &mockDispatcher{}, logger, AgentInstanceReconcilerConfig{})
	if rConfigured.config.DispatchRetryLimit != 3 ||
		rConfigured.config.DispatchBackoffBase != time.Second ||
		rConfigured.config.DispatchBackoffMax != 30*time.Second ||
		rConfigured.config.RunningPollInterval != 10*time.Second {
		t.Fatal("expected defaults to be applied for zero-valued custom config")
	}
}

func TestAgentInstanceReconciler_DispatchRetryUpdateError(t *testing.T) {
	updateErr := errors.New("write failed")
	s := &faultStore{memStore: newMemStore()}
	dispatcher := &mockDispatcher{err: errors.New("runtime unavailable")}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithConfig(s, dispatcher, logger, AgentInstanceReconcilerConfig{DispatchRetryLimit: 3})

	inst := makeInstance("default", "retry-update-fail", "my-agent", v1alpha1.PhasePending)
	storeInstance(t, s.memStore, inst)

	first, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "retry-update-fail", Kind: "AgentInstance"})
	if err != nil || !first.Requeue {
		t.Fatalf("expected finalizer pass with requeue, got result=%+v err=%v", first, err)
	}

	s.updateErr = updateErr
	_, err = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "retry-update-fail", Kind: "AgentInstance"})
	if err == nil {
		t.Fatal("expected retry update error")
	}
	if !errors.Is(err, updateErr) || !strings.Contains(err.Error(), "update after dispatch retry failure") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgentInstanceReconciler_DispatchFailureUpdateError(t *testing.T) {
	updateErr := errors.New("write failed")
	s := &faultStore{memStore: newMemStore()}
	dispatcher := &mockDispatcher{err: errors.New("runtime unavailable")}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithConfig(s, dispatcher, logger, AgentInstanceReconcilerConfig{DispatchRetryLimit: 0})

	inst := makeInstance("default", "terminal-update-fail", "my-agent", v1alpha1.PhasePending)
	storeInstance(t, s.memStore, inst)

	first, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "terminal-update-fail", Kind: "AgentInstance"})
	if err != nil || !first.Requeue {
		t.Fatalf("expected finalizer pass with requeue, got result=%+v err=%v", first, err)
	}

	// Force terminal on second dispatch by reducing limit after constructor defaults.
	r.config.DispatchRetryLimit = 0
	s.updateErr = updateErr
	_, err = r.Reconcile(context.Background(), Request{Namespace: "default", Name: "terminal-update-fail", Kind: "AgentInstance"})
	if err == nil {
		t.Fatal("expected dispatch terminal update error")
	}
	if !errors.Is(err, updateErr) || !strings.Contains(err.Error(), "update after dispatch failure") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgentInstanceReconciler_RunningStatusErrorUpdateError(t *testing.T) {
	updateErr := errors.New("write failed")
	s := &faultStore{memStore: newMemStore(), updateErr: updateErr}
	dispatcher := &mockDispatcher{statusErr: errors.New("runtime timeout")}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "status-error-update-fail", "my-agent", v1alpha1.PhaseRunning)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s.memStore, inst)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "status-error-update-fail", Kind: "AgentInstance"})
	if err == nil {
		t.Fatal("expected runtime status update error")
	}
	if !errors.Is(err, updateErr) || !strings.Contains(err.Error(), "update after runtime status error") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgentInstanceReconciler_RunningCompletionUpdateError(t *testing.T) {
	updateErr := errors.New("write failed")
	s := &faultStore{memStore: newMemStore(), updateErr: updateErr}
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true, Completed: true}}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "completion-update-fail", "my-agent", v1alpha1.PhaseRunning)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s.memStore, inst)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "completion-update-fail", Kind: "AgentInstance"})
	if err == nil {
		t.Fatal("expected completion update error")
	}
	if !errors.Is(err, updateErr) || !strings.Contains(err.Error(), "update after completion") {
		t.Fatalf("unexpected error: %v", err)
	}
	s.updateErr = nil
	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "completion-update-fail", Kind: "AgentInstance"}); err != nil {
		t.Fatalf("retry Reconcile() error = %v", err)
	}
	stored := getInstance(t, s.memStore, "default", "completion-update-fail")
	if stored.Status.Phase != v1alpha1.PhaseReady || dispatcher.statusCalled != 2 {
		t.Fatalf("retry status=%+v polls=%d, want Ready after replayed completion", stored.Status, dispatcher.statusCalled)
	}
}

func TestAgentInstanceReconciler_RunningInactiveUpdateError(t *testing.T) {
	updateErr := errors.New("write failed")
	s := &faultStore{memStore: newMemStore(), updateErr: updateErr}
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: false, Completed: false}}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "inactive-update-fail", "my-agent", v1alpha1.PhaseRunning)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s.memStore, inst)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "inactive-update-fail", Kind: "AgentInstance"})
	if err == nil {
		t.Fatal("expected inactive-session update error")
	}
	if !errors.Is(err, updateErr) || !strings.Contains(err.Error(), "update after inactive session") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgentInstanceReconciler_RunningStatusUpdateError(t *testing.T) {
	updateErr := errors.New("write failed")
	s := &faultStore{memStore: newMemStore(), updateErr: updateErr}
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: true, Completed: false, ToolCalls: 1}}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "running-update-fail", "my-agent", v1alpha1.PhaseRunning)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	storeInstance(t, s.memStore, inst)

	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "running-update-fail", Kind: "AgentInstance"})
	if err == nil {
		t.Fatal("expected running update error")
	}
	if !errors.Is(err, updateErr) || !strings.Contains(err.Error(), "update running status") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgentInstanceReconciler_RunningCompletedTransitionsToReady(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{
		statusSet: true,
		status: RuntimeStatus{
			Alive:     true,
			Completed: true,
			TokenUsage: &v1alpha1.TokenUsage{
				PromptTokens:     10,
				CompletionTokens: 5,
				TotalTokens:      15,
			},
			ToolCalls: 4,
		},
	}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "ready-inst", "my-agent", v1alpha1.PhaseRunning)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	inst.Status.SessionID = "session-ready"
	now := time.Now().UTC()
	inst.Status.StartTime = &now
	storeInstance(t, s, inst)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "ready-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Fatal("expected no requeue on completion")
	}

	got := getInstance(t, s, "default", "ready-inst")
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected phase Ready, got %s", got.Status.Phase)
	}
	if got.Status.TokenUsage == nil || got.Status.TokenUsage.TotalTokens != 15 {
		t.Fatalf("expected token usage updated from runtime status, got %+v", got.Status.TokenUsage)
	}
	if got.Status.ToolCalls != 4 {
		t.Fatalf("expected tool calls 4, got %d", got.Status.ToolCalls)
	}
}

func TestAgentInstanceReconciler_RunningCompletedWhenNotAliveTransitionsToReady(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{
		statusSet: true,
		status: RuntimeStatus{
			Alive:     false,
			Completed: true,
			ToolCalls: 2,
		},
	}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "ready-not-alive-inst", "my-agent", v1alpha1.PhaseRunning)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	inst.Status.SessionID = "session-ready-not-alive"
	now := time.Now().UTC()
	inst.Status.StartTime = &now
	storeInstance(t, s, inst)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "ready-not-alive-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Fatal("expected no requeue on completion")
	}

	got := getInstance(t, s, "default", "ready-not-alive-inst")
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected phase Ready, got %s", got.Status.Phase)
	}
}

func TestAgentInstanceReconciler_RunningNotAliveTransitionsToFailed(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: false}}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "not-alive-inst", "my-agent", v1alpha1.PhaseRunning)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	inst.Status.SessionID = "session-not-alive"
	now := time.Now().UTC()
	inst.Status.StartTime = &now
	storeInstance(t, s, inst)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "not-alive-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Fatal("expected no requeue on inactive session")
	}

	got := getInstance(t, s, "default", "not-alive-inst")
	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected phase Failed, got %s", got.Status.Phase)
	}

	var foundReady bool
	for _, c := range got.Status.Conditions {
		if c.Type == "Ready" {
			foundReady = true
			if c.Status != "False" {
				t.Fatalf("expected Ready=False, got %s", c.Status)
			}
			if c.Reason != "SessionNotAlive" {
				t.Fatalf("expected reason SessionNotAlive, got %s", c.Reason)
			}
		}
	}
	if !foundReady {
		t.Fatal("expected Ready condition")
	}
}

func TestAgentInstanceReconciler_RunningNotAliveIncludesRuntimeError(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: false, Error: "Missing credentials: OPENAI_API_KEY"}}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "err-inst", "my-agent", v1alpha1.PhaseRunning)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	inst.Status.SessionID = "session-err"
	now := time.Now().UTC()
	inst.Status.StartTime = &now
	storeInstance(t, s, inst)

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "err-inst", Kind: "AgentInstance"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getInstance(t, s, "default", "err-inst")
	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected phase Failed, got %s", got.Status.Phase)
	}
	var msg string
	for _, c := range got.Status.Conditions {
		if c.Type == "Ready" {
			msg = c.Message
		}
	}
	if !strings.Contains(msg, "Missing credentials: OPENAI_API_KEY") {
		t.Fatalf("expected condition message to include runtime error, got %q", msg)
	}
}

func TestAgentInstanceReconciler_RunningStatusErrorTransitionsToFailed(t *testing.T) {
	s := newMemStore()
	dispatcher := &mockDispatcher{statusErr: errors.New("runtime timeout")}
	logger := zaptest.NewLogger(t)
	r := NewAgentInstanceReconcilerWithDispatcher(s, dispatcher, logger)

	inst := makeInstance("default", "status-fail-inst", "my-agent", v1alpha1.PhaseRunning)
	inst.ObjectMeta.Finalizers = []string{AgentInstanceFinalizer}
	inst.Status.SessionID = "session-status-fail"
	now := time.Now().UTC()
	inst.Status.StartTime = &now
	storeInstance(t, s, inst)

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "status-fail-inst", Kind: "AgentInstance"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Fatal("expected no requeue on runtime status failure")
	}

	got := getInstance(t, s, "default", "status-fail-inst")
	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected phase Failed, got %s", got.Status.Phase)
	}
}
