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

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

const logSecretSentinel = "synthetic-backend-credential-do-not-log"

func assertConfidentialFailureLog(t *testing.T, logs *observer.ObservedLogs, message string, wantFields map[string]any) {
	t.Helper()
	entries := logs.FilterMessage(message).All()
	if len(entries) != 1 {
		t.Fatalf("log %q count = %d, want 1", message, len(entries))
	}
	fields := entries[0].ContextMap()
	for key, want := range wantFields {
		if fields[key] != want {
			t.Fatalf("log field %q = %v, want %v", key, fields[key], want)
		}
	}
	if _, exists := fields["error"]; exists {
		t.Fatal("raw error field was logged")
	}
	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, logSecretSentinel) || strings.Contains(fmt.Sprint(entry.ContextMap()), logSecretSentinel) {
			t.Fatalf("sensitive content reached log %q", entry.Message)
		}
	}
}

func TestControllerRetryLogsOmitRawErrors(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	memory := newMemStore()
	rec := &countingReconciler{err: fmt.Errorf("reconcile backend: %w", errors.New(logSecretSentinel))}
	controller := NewController(ControllerConfig{
		Name: "test-controller", Kind: "Agent", Reconciler: rec,
		BaseDelay: time.Hour, MaxDelay: time.Hour, MaxRetries: 1,
	}, NewInformer(memory, "Agent", logger), logger)
	t.Cleanup(controller.queue.ShutDown)
	const key = "default/agent"
	for range 2 {
		controller.queue.Add(key)
		next, ok := controller.queue.Get()
		if !ok || next != key {
			t.Fatalf("queue item = %q, ok = %t", next, ok)
		}
		controller.processItem(context.Background(), next)
	}
	if rec.callCount.Load() != 2 || controller.queue.NumRequeues(key) != 0 || controller.queue.Len() != 0 {
		t.Fatal("retry exhaustion behavior changed")
	}
	assertConfidentialFailureLog(t, logs, "reconciliation failed, will retry", map[string]any{
		"controller": "test-controller", "key": key, "retries": int64(1),
	})
	assertConfidentialFailureLog(t, logs, "item dropped after max retries", map[string]any{
		"controller": "test-controller", "key": key, "retries": int64(1),
	})
}

func TestExpiredLeaseFailureLogsOmitRawErrors(t *testing.T) {
	for _, failure := range []error{
		errors.New(logSecretSentinel),
		fmt.Errorf("%s: %w", logSecretSentinel, store.ErrConflict),
	} {
		t.Run(failure.Error(), func(t *testing.T) {
			memory := newMemStore()
			const leaseName = "expired-lease"
			record := leaseRecord{
				HolderIdentity: "previous-leader", LeaseDuration: "1s",
				AcquireTime: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
				RenewTime:   time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
			}
			data, err := json.Marshal(wrapLease(leaseName, record))
			if err != nil {
				t.Fatal(err)
			}
			if err := memory.Create(context.Background(), leaseKind, leaseNamespace, leaseName, data); err != nil {
				t.Fatal(err)
			}
			core, logs := observer.New(zap.WarnLevel)
			elector := NewLeaderElector(LeaderElectionConfig{LeaseName: leaseName, Identity: "contender"},
				&faultStore{memStore: memory, updateErr: failure}, zap.New(core))
			if elector.tryAcquire(context.Background()) {
				t.Fatal("failed lease update acquired leadership")
			}
			persisted, err := memory.Get(context.Background(), leaseKind, leaseNamespace, leaseName)
			if err != nil {
				t.Fatal(err)
			}
			var lease leaseWrapper
			if err := json.Unmarshal(persisted, &lease); err != nil {
				t.Fatal(err)
			}
			if lease.Spec.HolderIdentity != record.HolderIdentity {
				t.Fatal("failed takeover changed lease ownership")
			}
			assertConfidentialFailureLog(t, logs, "failed to take over expired lease", map[string]any{
				"lease": leaseName, "identity": "contender",
			})
		})
	}
}

func TestIndexRunReflectionFailureLogsOmitRawErrors(t *testing.T) {
	ctx := context.Background()
	memory := newMemStore()
	storeKB(t, memory, makeKB("default", "docs", "vector"))
	run := makeIndexRun("default", "run", "docs")
	run.Status.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	faults := &faultStore{memStore: memory, updateErr: errors.New(logSecretSentinel)}
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	reconciler := NewKnowledgeBaseIndexRunReconciler(faults, logger)
	stats := IngestStats{Documents: 2, Chunks: 4}
	if reconciler.reflectRunOntoKB(ctx, &run, stats, logger) {
		t.Fatal("failed reflection reported success")
	}
	if getKB(t, memory, "default", "docs").Status.Generation != 0 {
		t.Fatal("failed reflection incremented generation")
	}
	assertConfidentialFailureLog(t, logs, "reflect run onto KB: update failed; retrying", map[string]any{"kb": "docs"})
	faults.updateErr = nil
	for range 2 {
		if !reconciler.reflectRunOntoKB(ctx, &run, stats, logger) {
			t.Fatal("recovery reflection failed")
		}
	}
	kb := getKB(t, memory, "default", "docs")
	if kb.Status.Generation != 1 || kb.Status.DocumentCount != 2 || kb.Status.IndexedChunks != 4 {
		t.Fatalf("reflection was not applied exactly once: %#v", kb.Status)
	}
}

func TestAgentConditionUpdateFailureLogsOmitRawErrors(t *testing.T) {
	for _, resource := range []string{"KnowledgeBase", "Tool"} {
		t.Run(resource, func(t *testing.T) {
			ctx := context.Background()
			memory := newMemStore()
			required := makeTestAgent("default", "required", "claim", true)
			optional := makeTestAgent("default", "optional", "claim", false)
			if resource == "Tool" {
				required.Spec.Tools = []v1alpha1.AgentToolRef{{Ref: v1alpha1.LocalObjectReference{Name: "search"}, Required: true}}
				optional.Spec.Tools = []v1alpha1.AgentToolRef{{Ref: v1alpha1.LocalObjectReference{Name: "search"}}}
			}
			storeAgent(t, memory, required)
			storeAgent(t, memory, optional)
			storeKBClaim(t, memory, makeTestKBClaim("default", "claim", "docs"))
			faults := &faultStore{memStore: memory, updateErr: fmt.Errorf("update backend: %w", errors.New(logSecretSentinel))}
			core, logs := observer.New(zap.InfoLevel)
			logger := zap.New(core)
			var update func() error
			message := "failed to update agent KB condition"
			conditionType := "KnowledgeBaseReady"
			if resource == "KnowledgeBase" {
				kb := makeKB("default", "docs", "vector")
				reconciler := NewKnowledgeBaseReconciler(faults, logger)
				update = func() error { return reconciler.updateAgentKBConditions(ctx, &kb, false, logger) }
			} else {
				tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Namespace: "default", Name: "search"}}
				reconciler := NewToolReconciler(faults, logger)
				update = func() error { return reconciler.updateAgentToolConditions(ctx, &tool) }
				message = "failed to update agent tool condition"
				conditionType = "ToolsReady"
			}
			if err := update(); err != nil {
				t.Fatalf("condition update changed failure behavior: %v", err)
			}
			assertConfidentialFailureLog(t, logs, message, map[string]any{"agent": "required"})
			faults.updateErr = nil
			if err := update(); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"required", "optional"} {
				raw, err := memory.Get(ctx, "Agent", "default", name)
				if err != nil {
					t.Fatal(err)
				}
				var agent v1alpha1.Agent
				if err := json.Unmarshal(raw, &agent); err != nil {
					t.Fatal(err)
				}
				condition := restartLadderCondition(agent.Status.Conditions, conditionType)
				if name == "required" && (condition == nil || condition.Status != "False") ||
					name == "optional" && condition != nil {
					t.Fatalf("condition update changed gating for %q: %#v", name, condition)
				}
			}
		})
	}
}

func TestRestartLadderFailureLogsOmitRawErrors(t *testing.T) {
	memory := newMemStore()
	probes := &v1alpha1.AgentProbeSpec{HealthProbe: &v1alpha1.AgentHealthProbe{FailureThreshold: 1}}
	ladder := &v1alpha1.RestartLadder{Steps: []v1alpha1.RestartLadderStep{{Action: v1alpha1.RestartActionRestart}}}
	storeAgent(t, memory, makeAgentWithLadder("default", "agent", probes, ladder))
	instance := makeInstance("default", "instance", "agent", v1alpha1.PhaseRunning)
	instance.Status.ProbeStatus = &v1alpha1.ProbeStatus{ConsecutiveHealthFailures: 1}
	storeInstance(t, memory, instance)
	dispatcher := &mockDispatcher{err: errors.Join(errors.New(logSecretSentinel), errors.New("remote runtime failed"))}
	core, logs := observer.New(zap.InfoLevel)
	reconciler := NewRestartLadderReconciler(memory, dispatcher, zap.New(core))
	result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "default", Name: "instance"})
	if err != nil || result.RequeueAfter != time.Minute || dispatcher.called != 1 {
		t.Fatalf("ladder failure behavior changed: result=%+v error=%v calls=%d", result, err, dispatcher.called)
	}
	updated := getInstance(t, memory, "default", "instance")
	if updated.Status.ProbeStatus.LadderAttempts != 1 || updated.Status.ProbeStatus.LastLadderAction == nil ||
		*updated.Status.ProbeStatus.LastLadderAction != v1alpha1.RestartActionRestart {
		t.Fatal("failed action did not count the attempt")
	}
	assertConfidentialFailureLog(t, logs, "ladder action failed", map[string]any{
		"namespace": "default", "name": "instance", "action": string(v1alpha1.RestartActionRestart),
	})
}
