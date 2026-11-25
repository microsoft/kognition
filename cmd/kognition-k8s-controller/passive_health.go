// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"errors"
	"fmt"

	"go.uber.org/zap"
	ctrl "sigs.k8s.io/controller-runtime"

	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	k8scallhealth "github.com/kognition-ai/kognition/internal/k8s/callhealth"
	k8sctrl "github.com/kognition-ai/kognition/internal/k8s/controller"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store"
)

// passiveModelHealth wires passive model health into the Kubernetes controller:
// it records the outcomes of the controller's own model calls, publishes them,
// lets the Model controller degrade Models whose real traffic is failing, and
// queues Models whose status no longer matches the evidence. The methods of a
// nil value do nothing.
type passiveModelHealth struct {
	sink    store.ModelCallHealthStore
	tracker *modelhealth.Tracker
}

func newPassiveModelHealth(sink store.ModelCallHealthStore) *passiveModelHealth {
	if sink == nil {
		return nil
	}
	return &passiveModelHealth{sink: sink, tracker: modelhealth.NewTracker()}
}

// recorder adds the passive health tracker to base, which may be nil.
func (p *passiveModelHealth) recorder(base cpi.CallRecorder) cpi.CallRecorder {
	if p == nil {
		return base
	}
	return cpi.Recorders(base, p.tracker)
}

// register configures the Model reconciler among reconcilers and adds the
// reporter, watcher and ConfigMap collector to the manager. Like the controllers,
// they run only on the elected leader.
func (p *passiveModelHealth) register(mgr ctrl.Manager, st store.Store, reconcilers []managerReconciler, logger *zap.Logger) error {
	if p == nil {
		return nil
	}
	var modelAdapter *k8sctrl.ModelReconcilerAdapter
	var modelClassAdapter *k8sctrl.ModelClassReconcilerAdapter
	for _, reconciler := range reconcilers {
		switch adapter := reconciler.(type) {
		case *k8sctrl.ModelReconcilerAdapter:
			modelAdapter = adapter
		case *k8sctrl.ModelClassReconcilerAdapter:
			modelClassAdapter = adapter
		}
	}
	if modelAdapter == nil {
		return errors.New("register passive model health: the Model reconciler is not part of the assembly")
	}
	if modelClassAdapter == nil {
		return errors.New("register passive model health: the ModelClass reconciler is not part of the assembly")
	}
	modelAdapter.ConfigureCallHealth(p.sink)
	modelClassAdapter.ConfigureCallHealth(p.sink)
	if err := mgr.Add(modelhealth.NewReporter(p.tracker, p.sink, modelhealth.NewReporterID(), logger)); err != nil {
		return fmt.Errorf("register model call health reporter: %w", err)
	}
	if err := mgr.Add(intctrl.NewModelHealthWatcher(st, p.sink, modelAdapter.Enqueue, logger)); err != nil {
		return fmt.Errorf("register model health watcher: %w", err)
	}
	if collector, ok := p.sink.(*k8scallhealth.ConfigMapStore); ok {
		if err := mgr.Add(collector); err != nil {
			return fmt.Errorf("register model call health collector: %w", err)
		}
	}
	return nil
}
