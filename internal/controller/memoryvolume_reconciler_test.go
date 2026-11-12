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
	"github.com/kognition-ai/kognition/internal/store"
)

type memoryVolumeFaultStore struct {
	store.Store
	getErr    error
	updateErr error
}

func (s *memoryVolumeFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.Store.Get(ctx, kind, namespace, name)
}

func (s *memoryVolumeFaultStore) Update(ctx context.Context, kind, namespace, name string, data []byte, version int64) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.Store.Update(ctx, kind, namespace, name, data, version)
}

func storeMemoryVolume(t *testing.T, memoryStore *memStore, volume v1alpha1.MemoryVolume) {
	t.Helper()
	data, err := json.Marshal(volume)
	if err != nil {
		t.Fatalf("marshal MemoryVolume: %v", err)
	}
	if err := memoryStore.Create(context.Background(), memoryVolumeKind, volume.ObjectMeta.Namespace, volume.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store MemoryVolume: %v", err)
	}
}

func getMemoryVolume(t *testing.T, memoryStore *memStore, namespace, name string) v1alpha1.MemoryVolume {
	t.Helper()
	raw, err := memoryStore.Get(context.Background(), memoryVolumeKind, namespace, name)
	if err != nil {
		t.Fatalf("get MemoryVolume: %v", err)
	}
	var volume v1alpha1.MemoryVolume
	if err := json.Unmarshal(raw, &volume); err != nil {
		t.Fatalf("unmarshal MemoryVolume: %v", err)
	}
	return volume
}

func TestMemoryVolumeReconcilerRepairsMissingReadyCondition(t *testing.T) {
	memoryStore := newMemStore()
	storeMemoryVolume(t, memoryStore, v1alpha1.MemoryVolume{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "memory-a"},
		Spec: v1alpha1.MemoryVolumeSpec{
			MemoryClassName: "standard",
			ClaimRef:        &v1alpha1.LocalObjectReference{Name: "claim-a"},
		},
		Status: v1alpha1.MemoryVolumeStatus{Phase: v1alpha1.PhaseBound},
	})
	reconciler := NewMemoryVolumeReconciler(memoryStore, zaptest.NewLogger(t))

	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "memory-a"}); err != nil {
		t.Fatalf("reconcile MemoryVolume: %v", err)
	}
	volume := getMemoryVolume(t, memoryStore, "tenant-a", "memory-a")
	if len(volume.Status.Conditions) != 1 {
		t.Fatalf("conditions = %+v, want repaired Ready condition", volume.Status.Conditions)
	}
	condition := volume.Status.Conditions[0]
	if condition.Type != string(lifecycle.ConditionReady) || condition.Status != string(lifecycle.ConditionTrue) || condition.Reason != "Bound" {
		t.Fatalf("Ready condition = %+v, want True/Bound", condition)
	}
}

func TestMemoryVolumeReconcilerPublishesLifecycleStatus(t *testing.T) {
	for _, test := range []struct {
		name       string
		spec       v1alpha1.MemoryVolumeSpec
		oldPhase   v1alpha1.Phase
		wantPhase  v1alpha1.Phase
		wantStatus string
		wantReason string
		wantText   string
	}{
		{
			name:       "missing memory class fails",
			spec:       v1alpha1.MemoryVolumeSpec{ClaimRef: &v1alpha1.LocalObjectReference{Name: "claim-a"}},
			wantPhase:  v1alpha1.PhaseFailed,
			wantStatus: string(lifecycle.ConditionFalse),
			wantReason: "MissingMemoryClass",
			wantText:   "memoryClassName is required",
		},
		{
			name:       "claim binds volume",
			spec:       v1alpha1.MemoryVolumeSpec{MemoryClassName: "standard", ClaimRef: &v1alpha1.LocalObjectReference{Name: "claim-a"}},
			wantPhase:  v1alpha1.PhaseBound,
			wantStatus: string(lifecycle.ConditionTrue),
			wantReason: "Bound",
			wantText:   `claim "claim-a"`,
		},
		{
			name:       "unbound volume is released",
			spec:       v1alpha1.MemoryVolumeSpec{MemoryClassName: "standard"},
			wantPhase:  v1alpha1.PhaseReleased,
			wantStatus: string(lifecycle.ConditionTrue),
			wantReason: "Unbound",
			wantText:   "no active claim",
		},
		{
			name:       "archived volume stays archived",
			spec:       v1alpha1.MemoryVolumeSpec{MemoryClassName: "standard"},
			oldPhase:   v1alpha1.PhaseArchived,
			wantPhase:  v1alpha1.PhaseArchived,
			wantStatus: string(lifecycle.ConditionTrue),
			wantReason: "Unbound",
			wantText:   "no active claim",
		},
		{
			name:       "released volume is not rebound",
			spec:       v1alpha1.MemoryVolumeSpec{MemoryClassName: "standard", ClaimRef: &v1alpha1.LocalObjectReference{Name: "claim-a"}},
			oldPhase:   v1alpha1.PhaseReleased,
			wantPhase:  v1alpha1.PhaseReleased,
			wantStatus: string(lifecycle.ConditionTrue),
			wantReason: "Bound",
			wantText:   `claim "claim-a"`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			memoryStore := newMemStore()
			storeMemoryVolume(t, memoryStore, v1alpha1.MemoryVolume{
				ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "memory-a"},
				Spec:       test.spec,
				Status:     v1alpha1.MemoryVolumeStatus{Phase: test.oldPhase},
			})
			reconciler := NewMemoryVolumeReconciler(memoryStore, zaptest.NewLogger(t))

			if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "memory-a"}); err != nil {
				t.Fatalf("reconcile MemoryVolume: %v", err)
			}
			volume := getMemoryVolume(t, memoryStore, "tenant-a", "memory-a")
			if volume.Status.Phase != test.wantPhase {
				t.Fatalf("phase = %q, want %q", volume.Status.Phase, test.wantPhase)
			}
			if len(volume.Status.Conditions) != 1 {
				t.Fatalf("conditions = %+v, want one Ready condition", volume.Status.Conditions)
			}
			condition := volume.Status.Conditions[0]
			if condition.Type != string(lifecycle.ConditionReady) || condition.Status != test.wantStatus || condition.Reason != test.wantReason {
				t.Fatalf("Ready condition = %+v, want status %q reason %q", condition, test.wantStatus, test.wantReason)
			}
			if !strings.Contains(condition.Message, test.wantText) {
				t.Fatalf("condition message = %q, want %q context", condition.Message, test.wantText)
			}
		})
	}
}

func TestMemoryVolumeReconcilerStableStatusDoesNotWrite(t *testing.T) {
	memoryStore := newMemStore()
	storeMemoryVolume(t, memoryStore, v1alpha1.MemoryVolume{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "memory-a"},
		Spec: v1alpha1.MemoryVolumeSpec{
			MemoryClassName: "standard",
			ClaimRef:        &v1alpha1.LocalObjectReference{Name: "claim-a"},
		},
		Status: v1alpha1.MemoryVolumeStatus{
			Phase: v1alpha1.PhaseBound,
			Conditions: []v1alpha1.Condition{{
				Type:               string(lifecycle.ConditionReady),
				Status:             string(lifecycle.ConditionTrue),
				LastTransitionTime: time.Now().UTC().Truncate(time.Second),
				Reason:             "Bound",
				Message:            `volume bound to claim "claim-a"`,
			}},
		},
	})
	writeErr := errors.New("unexpected write")
	reconciler := NewMemoryVolumeReconciler(&memoryVolumeFaultStore{Store: memoryStore, updateErr: writeErr}, zaptest.NewLogger(t))

	result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "memory-a"})
	if err != nil || result != (Result{}) {
		t.Fatalf("result = %+v, error = %v, want stable no-op", result, err)
	}
}

func TestMemoryVolumeReconcilerStorageBoundaries(t *testing.T) {
	t.Run("not found is complete", func(t *testing.T) {
		reconciler := NewMemoryVolumeReconciler(&memoryVolumeFaultStore{
			Store:  newMemStore(),
			getErr: fmt.Errorf("wrapped missing volume: %w", store.ErrNotFound),
		}, zaptest.NewLogger(t))
		result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "missing"})
		if err != nil || result != (Result{}) {
			t.Fatalf("result = %+v, error = %v, want completed not-found reconciliation", result, err)
		}
	})

	t.Run("get error is wrapped", func(t *testing.T) {
		getErr := errors.New("store unavailable")
		reconciler := NewMemoryVolumeReconciler(&memoryVolumeFaultStore{Store: newMemStore(), getErr: getErr}, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "memory-a"})
		if !errors.Is(err, getErr) || !strings.Contains(err.Error(), "get MemoryVolume") {
			t.Fatalf("error = %v, want wrapped get error", err)
		}
	})

	t.Run("malformed record is rejected", func(t *testing.T) {
		memoryStore := newMemStore()
		if err := memoryStore.Create(context.Background(), memoryVolumeKind, "tenant-a", "broken", []byte(`{"spec":`)); err != nil {
			t.Fatalf("store malformed MemoryVolume: %v", err)
		}
		reconciler := NewMemoryVolumeReconciler(memoryStore, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "broken"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal MemoryVolume") {
			t.Fatalf("error = %v, want unmarshal context", err)
		}
	})

	for _, test := range []struct {
		name        string
		updateErr   error
		wantRequeue bool
	}{
		{name: "wrapped conflict requeues", updateErr: fmt.Errorf("wrapped conflict: %w", store.ErrConflict), wantRequeue: true},
		{name: "update error is wrapped", updateErr: errors.New("write unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			memoryStore := newMemStore()
			storeMemoryVolume(t, memoryStore, v1alpha1.MemoryVolume{
				ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "memory-a"},
				Spec:       v1alpha1.MemoryVolumeSpec{MemoryClassName: "standard"},
			})
			reconciler := NewMemoryVolumeReconciler(&memoryVolumeFaultStore{Store: memoryStore, updateErr: test.updateErr}, zaptest.NewLogger(t))
			result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "memory-a"})
			if test.wantRequeue {
				if err != nil || !result.Requeue {
					t.Fatalf("result = %+v, error = %v, want conflict requeue", result, err)
				}
				return
			}
			if !errors.Is(err, test.updateErr) || !strings.Contains(err.Error(), "update MemoryVolume status") {
				t.Fatalf("error = %v, want wrapped update error", err)
			}
		})
	}
}
