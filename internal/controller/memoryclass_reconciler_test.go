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

type memoryClassFaultStore struct {
	store.Store
	getErr    error
	updateErr error
}

func (s *memoryClassFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.Store.Get(ctx, kind, namespace, name)
}

func (s *memoryClassFaultStore) Update(ctx context.Context, kind, namespace, name string, data []byte, version int64) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.Store.Update(ctx, kind, namespace, name, data, version)
}

func storeReconciledMemoryClass(t *testing.T, memoryStore *memStore, memoryClass v1alpha1.MemoryClass) {
	t.Helper()
	data, err := json.Marshal(memoryClass)
	if err != nil {
		t.Fatalf("marshal MemoryClass: %v", err)
	}
	if err := memoryStore.Create(context.Background(), memoryClassKind, "", memoryClass.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store MemoryClass: %v", err)
	}
}

func getReconciledMemoryClass(t *testing.T, memoryStore *memStore, name string) v1alpha1.MemoryClass {
	t.Helper()
	raw, err := memoryStore.Get(context.Background(), memoryClassKind, "", name)
	if err != nil {
		t.Fatalf("get MemoryClass: %v", err)
	}
	var memoryClass v1alpha1.MemoryClass
	if err := json.Unmarshal(raw, &memoryClass); err != nil {
		t.Fatalf("unmarshal MemoryClass: %v", err)
	}
	return memoryClass
}

func TestMemoryClassReconcilerRepairsMissingReadyCondition(t *testing.T) {
	memoryStore := newMemStore()
	storeReconciledMemoryClass(t, memoryStore, v1alpha1.MemoryClass{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "standard"},
		Spec:       v1alpha1.MemoryClassSpec{BackendType: "inmemory"},
		Status: v1alpha1.MemoryClassStatus{
			Phase:   v1alpha1.PhaseReady,
			Message: `memory class registered (backend "inmemory")`,
		},
	})
	reconciler := NewMemoryClassReconciler(memoryStore, zaptest.NewLogger(t))

	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "ignored", Name: "standard"}); err != nil {
		t.Fatalf("reconcile MemoryClass: %v", err)
	}
	memoryClass := getReconciledMemoryClass(t, memoryStore, "standard")
	if len(memoryClass.Status.Conditions) != 1 {
		t.Fatalf("conditions = %+v, want repaired Ready condition", memoryClass.Status.Conditions)
	}
	condition := memoryClass.Status.Conditions[0]
	if condition.Type != string(lifecycle.ConditionReady) || condition.Status != string(lifecycle.ConditionTrue) || condition.Reason != "Registered" {
		t.Fatalf("Ready condition = %+v, want True/Registered", condition)
	}
}

func TestMemoryClassReconcilerPublishesValidationStatus(t *testing.T) {
	for _, test := range []struct {
		name       string
		backend    string
		wantPhase  v1alpha1.Phase
		wantStatus string
		wantReason string
		wantText   string
	}{
		{
			name:       "configured backend is ready",
			backend:    "pgvector",
			wantPhase:  v1alpha1.PhaseReady,
			wantStatus: string(lifecycle.ConditionTrue),
			wantReason: "Registered",
			wantText:   `backend "pgvector"`,
		},
		{
			name:       "blank backend fails",
			backend:    "  ",
			wantPhase:  v1alpha1.PhaseFailed,
			wantStatus: string(lifecycle.ConditionFalse),
			wantReason: "MissingBackendType",
			wantText:   "spec.backendType is required",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			memoryStore := newMemStore()
			storeReconciledMemoryClass(t, memoryStore, v1alpha1.MemoryClass{
				ObjectMeta: v1alpha1.ObjectMeta{Name: "standard"},
				Spec:       v1alpha1.MemoryClassSpec{BackendType: test.backend},
			})
			reconciler := NewMemoryClassReconciler(memoryStore, zaptest.NewLogger(t))

			if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "standard"}); err != nil {
				t.Fatalf("reconcile MemoryClass: %v", err)
			}
			memoryClass := getReconciledMemoryClass(t, memoryStore, "standard")
			if memoryClass.Status.Phase != test.wantPhase || !strings.Contains(memoryClass.Status.Message, test.wantText) {
				t.Fatalf("status = %+v, want phase %q with %q context", memoryClass.Status, test.wantPhase, test.wantText)
			}
			if len(memoryClass.Status.Conditions) != 1 {
				t.Fatalf("conditions = %+v, want one Ready condition", memoryClass.Status.Conditions)
			}
			condition := memoryClass.Status.Conditions[0]
			if condition.Type != string(lifecycle.ConditionReady) || condition.Status != test.wantStatus || condition.Reason != test.wantReason {
				t.Fatalf("Ready condition = %+v, want status %q reason %q", condition, test.wantStatus, test.wantReason)
			}
		})
	}
}

func TestMemoryClassReconcilerStableStatusDoesNotWrite(t *testing.T) {
	memoryStore := newMemStore()
	message := `memory class registered (backend "inmemory")`
	storeReconciledMemoryClass(t, memoryStore, v1alpha1.MemoryClass{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "standard"},
		Spec:       v1alpha1.MemoryClassSpec{BackendType: "inmemory"},
		Status: v1alpha1.MemoryClassStatus{
			Phase:   v1alpha1.PhaseReady,
			Message: message,
			Conditions: []v1alpha1.Condition{{
				Type:               string(lifecycle.ConditionReady),
				Status:             string(lifecycle.ConditionTrue),
				LastTransitionTime: time.Now().UTC().Truncate(time.Second),
				Reason:             "Registered",
				Message:            message,
			}},
		},
	})
	writeErr := errors.New("unexpected write")
	reconciler := NewMemoryClassReconciler(&memoryClassFaultStore{Store: memoryStore, updateErr: writeErr}, zaptest.NewLogger(t))

	result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "standard"})
	if err != nil || result != (Result{}) {
		t.Fatalf("result = %+v, error = %v, want stable no-op", result, err)
	}
}

func TestMemoryClassReconcilerStorageBoundaries(t *testing.T) {
	t.Run("not found is complete", func(t *testing.T) {
		reconciler := NewMemoryClassReconciler(&memoryClassFaultStore{
			Store:  newMemStore(),
			getErr: fmt.Errorf("wrapped missing class: %w", store.ErrNotFound),
		}, zaptest.NewLogger(t))
		result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "missing"})
		if err != nil || result != (Result{}) {
			t.Fatalf("result = %+v, error = %v, want completed not-found reconciliation", result, err)
		}
	})

	t.Run("get error is wrapped", func(t *testing.T) {
		getErr := errors.New("store unavailable")
		reconciler := NewMemoryClassReconciler(&memoryClassFaultStore{Store: newMemStore(), getErr: getErr}, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Name: "standard"})
		if !errors.Is(err, getErr) || !strings.Contains(err.Error(), "get MemoryClass") {
			t.Fatalf("error = %v, want wrapped get error", err)
		}
	})

	t.Run("malformed record is rejected", func(t *testing.T) {
		memoryStore := newMemStore()
		if err := memoryStore.Create(context.Background(), memoryClassKind, "", "broken", []byte(`{"spec":`)); err != nil {
			t.Fatalf("store malformed MemoryClass: %v", err)
		}
		reconciler := NewMemoryClassReconciler(memoryStore, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Name: "broken"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal MemoryClass") {
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
			storeReconciledMemoryClass(t, memoryStore, v1alpha1.MemoryClass{
				ObjectMeta: v1alpha1.ObjectMeta{Name: "standard"},
				Spec:       v1alpha1.MemoryClassSpec{BackendType: "inmemory"},
			})
			reconciler := NewMemoryClassReconciler(&memoryClassFaultStore{Store: memoryStore, updateErr: test.updateErr}, zaptest.NewLogger(t))
			result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "standard"})
			if test.wantRequeue {
				if err != nil || !result.Requeue {
					t.Fatalf("result = %+v, error = %v, want conflict requeue", result, err)
				}
				return
			}
			if !errors.Is(err, test.updateErr) || !strings.Contains(err.Error(), "update MemoryClass status") {
				t.Fatalf("error = %v, want wrapped update error", err)
			}
		})
	}
}
