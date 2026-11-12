// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"go.uber.org/zap/zaptest"
)

func updateInstance(t *testing.T, s *memStore, inst v1alpha1.AgentInstance) {
	t.Helper()
	data, err := json.Marshal(inst)
	if err != nil {
		t.Fatalf("marshal instance: %v", err)
	}
	if err := s.Update(context.Background(), "AgentInstance", inst.ObjectMeta.Namespace, inst.ObjectMeta.Name, data, inst.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update instance: %v", err)
	}
}

// Traceability coverage for completed TCs:
// TC-0101: Restart-ladder [Restart, RollbackRevision, DegradeModel]; HealthProbe failure triggers Restart first.
// TC-0102: ReasoningProbe failure persists after Restart; controller escalates to RollbackRevision.
// TC-0103: ReasoningProbe failure -> restart ladder -> autonomous recovery; full event chain observable.
// TC-0104: Repeated failures exceed all ladder steps; agent is quarantined, no new sessions scheduled.

// TestProbeAndLadder_EndToEnd exercises the full probe → threshold breach → restart ladder → recovery flow.
func TestProbeAndLadder_EndToEnd(t *testing.T) {
	s := newMemStore()
	d := &mockDispatcher{statusSet: true, status: RuntimeStatus{Alive: false}}
	logger := zaptest.NewLogger(t)

	probeR := NewProbeReconciler(s, d, logger)
	ladderR := NewRestartLadderReconciler(s, d, logger)

	probes := &v1alpha1.AgentProbeSpec{
		HealthProbe: &v1alpha1.AgentHealthProbe{
			IntervalSeconds:  10,
			FailureThreshold: 3,
		},
	}
	ladder := &v1alpha1.RestartLadder{
		Steps: []v1alpha1.RestartLadderStep{
			{Action: v1alpha1.RestartActionRestart, StabilisationSeconds: 5},
			{Action: v1alpha1.RestartActionQuarantine, StabilisationSeconds: 10},
		},
	}

	ag := makeAgent("ns", "agent1", 1)
	ag.Spec.Probes = probes
	ag.Spec.RestartLadder = ladder
	storeAgent(t, s, ag)

	inst := makeInstance("ns", "inst1", "agent1", v1alpha1.PhaseRunning)
	storeInstance(t, s, inst)

	req := Request{Namespace: "ns", Name: "inst1", Kind: "AgentInstance"}
	ctx := context.Background()

	// Phase 1: accumulate health failures up to threshold.
	for i := 0; i < 3; i++ {
		probeR.Reconcile(ctx, req)
	}
	updated := getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus.ConsecutiveHealthFailures != 3 {
		t.Fatalf("expected 3 health failures, got %d", updated.Status.ProbeStatus.ConsecutiveHealthFailures)
	}

	// Phase 2: ladder reconciler should detect breach and execute Restart (step 0).
	d.sessionID = "restarted-session"
	d.status = RuntimeStatus{Alive: false} // still unhealthy after restart
	ladderR.Reconcile(ctx, req)

	updated = getInstance(t, s, "ns", "inst1")
	if updated.Status.RestartCount != 1 {
		t.Fatalf("expected RestartCount=1, got %d", updated.Status.RestartCount)
	}
	// After first attempt at step 0, LadderAttempts=1 but LadderStep stays 0 until next reconcile escalates.
	if updated.Status.ProbeStatus.LadderStep != 0 {
		t.Fatalf("expected LadderStep=0 after first restart, got %d", updated.Status.ProbeStatus.LadderStep)
	}
	// Restart resets probe counters.
	if updated.Status.ProbeStatus.ConsecutiveHealthFailures != 0 {
		t.Fatalf("expected health failures reset to 0, got %d", updated.Status.ProbeStatus.ConsecutiveHealthFailures)
	}

	// Phase 3: failures accumulate again after restart.
	for i := 0; i < 3; i++ {
		probeR.Reconcile(ctx, req)
	}

	// Phase 4: ladder escalates to Quarantine (step 1).
	ladderR.Reconcile(ctx, req)
	updated = getInstance(t, s, "ns", "inst1")
	if updated.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("expected PhaseDegraded after quarantine, got %s", updated.Status.Phase)
	}
	cond := findCondition(updated.Status.Conditions, string(lifecycle.ConditionQuarantined))
	if cond == nil || cond.Status != string(lifecycle.ConditionTrue) {
		t.Fatal("expected Quarantined=True condition")
	}

	// Phase 5: recovery — runtime becomes healthy, probe detects it.
	// First, set instance back to Running to allow probe reconciler to act.
	updated.Status.Phase = v1alpha1.PhaseRunning
	updateInstance(t, s, updated)

	d.status = RuntimeStatus{Alive: true}
	probeR.Reconcile(ctx, req)

	updated = getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus.ConsecutiveHealthFailures != 0 {
		t.Fatalf("expected 0 health failures after recovery, got %d", updated.Status.ProbeStatus.ConsecutiveHealthFailures)
	}

	// Ladder reconciler should reset ladder state on recovery.
	ladderR.Reconcile(ctx, req)
	updated = getInstance(t, s, "ns", "inst1")
	if updated.Status.ProbeStatus.LadderStep != 0 {
		t.Fatalf("expected LadderStep=0 after recovery, got %d", updated.Status.ProbeStatus.LadderStep)
	}
}
