// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"

	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/k8s/callhealth"
	k8sctrl "github.com/kognition-ai/kognition/internal/k8s/controller"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store"
)

type memoryCallHealth struct{}

func (memoryCallHealth) ReplaceModelCallHealth(context.Context, string, []store.ModelCallHealth) error {
	return nil
}

func (memoryCallHealth) ListModelCallHealth(context.Context, time.Duration) ([]store.ModelCallHealth, error) {
	return nil, nil
}

type recordedCalls struct{ outcomes []cpi.CallOutcome }

func (r *recordedCalls) RecordCall(outcome cpi.CallOutcome) { r.outcomes = append(r.outcomes, outcome) }

// failingAddManager fails the nth runnable registration.
type failingAddManager struct {
	startupManager
	failOn int32
	count  atomic.Int32
}

func (m *failingAddManager) Add(runnable ctrlmanager.Runnable) error {
	if m.count.Add(1) == m.failOn {
		return errors.New("registration refused")
	}
	return m.startupManager.Add(runnable)
}

func TestPassiveModelHealthIsDisabledWithoutAHealthStore(t *testing.T) {
	passive := newPassiveModelHealth(nil)
	if passive != nil {
		t.Fatal("passive health must be disabled without a store")
	}
	base := &recordedCalls{}
	if passive.recorder(base) != cpi.CallRecorder(base) || passive.recorder(nil) != nil {
		t.Fatal("a disabled passive health must leave the recorder unchanged")
	}
	if err := passive.register(&startupManager{}, nil, nil, zap.NewNop()); err != nil {
		t.Fatalf("register() on a disabled passive health = %v, want nil", err)
	}
}

func TestPassiveModelHealthRecordsOutcomesAndRegistersItsRunnables(t *testing.T) {
	passive := newPassiveModelHealth(memoryCallHealth{})
	if passive == nil {
		t.Fatal("a store must enable passive health")
	}
	base := &recordedCalls{}
	passive.recorder(base).RecordCall(cpi.CallOutcome{Provider: "anthropic", Operation: cpi.OperationComplete, Model: "claude"})
	if len(base.outcomes) != 1 || len(passive.tracker.Snapshot()) != 2 {
		t.Fatalf("outcomes reached the base recorder %d times and the tracker holds %d streams, want one outcome and the destination and credential streams of its success", len(base.outcomes), len(passive.tracker.Snapshot()))
	}
	if passive.recorder(nil) != cpi.CallRecorder(passive.tracker) {
		t.Fatal("without a base recorder the tracker must be the recorder")
	}

	manager := &startupManager{}
	reconcilers := []managerReconciler{
		stubManagerReconciler{calls: &atomic.Int32{}},
		k8sctrl.NewModelReconcilerAdapter(nil, cpi.NewRegistry(), zap.NewNop()),
		k8sctrl.NewModelClassReconcilerAdapter(nil, cpi.NewRegistry(), zap.NewNop()),
	}
	if err := passive.register(manager, nil, reconcilers, zap.NewNop()); err != nil {
		t.Fatalf("register() error: %v", err)
	}
	var reporter, watcher bool
	for _, runnable := range manager.runnables {
		switch runnable.(type) {
		case *modelhealth.Reporter:
			reporter = true
		case *intctrl.ModelHealthWatcher:
			watcher = true
		}
	}
	if len(manager.runnables) != 2 || !reporter || !watcher {
		t.Fatalf("runnables = %v, want exactly the reporter and the watcher", manager.runnables)
	}
}

func TestPassiveModelHealthRegistrationFailures(t *testing.T) {
	passive := newPassiveModelHealth(memoryCallHealth{})
	withModel := []managerReconciler{k8sctrl.NewModelReconcilerAdapter(nil, cpi.NewRegistry(), zap.NewNop())}
	withBoth := append([]managerReconciler{k8sctrl.NewModelClassReconcilerAdapter(nil, cpi.NewRegistry(), zap.NewNop())}, withModel...)

	if err := passive.register(&startupManager{}, nil, []managerReconciler{stubManagerReconciler{calls: &atomic.Int32{}}}, zap.NewNop()); err == nil || !strings.Contains(err.Error(), "Model reconciler") {
		t.Fatalf("a missing Model reconciler error = %v", err)
	}
	if err := passive.register(&startupManager{}, nil, withModel, zap.NewNop()); err == nil || !strings.Contains(err.Error(), "ModelClass reconciler") {
		t.Fatalf("a missing ModelClass reconciler error = %v", err)
	}
	if err := passive.register(&failingAddManager{failOn: 1}, nil, withBoth, zap.NewNop()); err == nil || !strings.Contains(err.Error(), "reporter") {
		t.Fatalf("a failing reporter registration error = %v", err)
	}
	if err := passive.register(&failingAddManager{failOn: 2}, nil, withBoth, zap.NewNop()); err == nil || !strings.Contains(err.Error(), "watcher") {
		t.Fatalf("a failing watcher registration error = %v", err)
	}
}

func TestPassiveModelHealthRegistersLeaderOnlyConfigMapCollector(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sink, err := callhealth.NewConfigMapStore(fake.NewClientBuilder().WithScheme(scheme).Build(), "kognition-system", "controller", callhealth.WithGarbageCollection())
	if err != nil {
		t.Fatal(err)
	}
	passive := newPassiveModelHealth(sink)
	manager := &startupManager{}
	reconcilers := []managerReconciler{
		k8sctrl.NewModelReconcilerAdapter(nil, cpi.NewRegistry(), zap.NewNop()),
		k8sctrl.NewModelClassReconcilerAdapter(nil, cpi.NewRegistry(), zap.NewNop()),
	}
	if err := passive.register(manager, nil, reconcilers, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if len(manager.runnables) != 3 {
		t.Fatalf("registered %d runnables, want reporter, watcher and collector", len(manager.runnables))
	}
	if collector, ok := manager.runnables[2].(*callhealth.ConfigMapStore); !ok || collector != sink || !collector.NeedLeaderElection() {
		t.Fatal("the registered collector must be the leader-only health store")
	}
	if err := passive.register(&failingAddManager{failOn: 3}, nil, reconcilers, zap.NewNop()); err == nil || !strings.Contains(err.Error(), "collector") {
		t.Fatalf("collector registration failure = %v", err)
	}
}
