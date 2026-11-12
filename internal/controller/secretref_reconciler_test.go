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

type secretRefFaultStore struct {
	store.Store
	getErr    error
	updateErr error
}

func (s *secretRefFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.Store.Get(ctx, kind, namespace, name)
}

func (s *secretRefFaultStore) Update(ctx context.Context, kind, namespace, name string, data []byte, version int64) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.Store.Update(ctx, kind, namespace, name, data, version)
}

func storeSecretRef(t *testing.T, memoryStore *memStore, secretRef v1alpha1.SecretRef) {
	t.Helper()
	data, err := json.Marshal(secretRef)
	if err != nil {
		t.Fatalf("marshal SecretRef: %v", err)
	}
	if err := memoryStore.Create(context.Background(), secretRefKind, secretRef.ObjectMeta.Namespace, secretRef.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store SecretRef: %v", err)
	}
}

func getSecretRef(t *testing.T, memoryStore *memStore, namespace, name string) v1alpha1.SecretRef {
	t.Helper()
	raw, err := memoryStore.Get(context.Background(), secretRefKind, namespace, name)
	if err != nil {
		t.Fatalf("get SecretRef: %v", err)
	}
	var secretRef v1alpha1.SecretRef
	if err := json.Unmarshal(raw, &secretRef); err != nil {
		t.Fatalf("unmarshal SecretRef: %v", err)
	}
	return secretRef
}

func TestSecretRefReconcilerRepairsMissingReadyCondition(t *testing.T) {
	memoryStore := newMemStore()
	storeSecretRef(t, memoryStore, v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "model-key"},
		Spec: v1alpha1.SecretRefSpec{
			BackendType: "internal",
			BackendRef:  "secret/openai",
			Key:         "api-key",
		},
		Status: v1alpha1.SecretRefStatus{
			Phase:   v1alpha1.PhaseReady,
			Message: `secret reference validated (backend "internal")`,
			Synced:  true,
		},
	})
	reconciler := NewSecretRefReconciler(memoryStore, zaptest.NewLogger(t))

	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "model-key"}); err != nil {
		t.Fatalf("reconcile SecretRef: %v", err)
	}
	secretRef := getSecretRef(t, memoryStore, "tenant-a", "model-key")
	if len(secretRef.Status.Conditions) != 1 {
		t.Fatalf("conditions = %+v, want repaired Ready condition", secretRef.Status.Conditions)
	}
	condition := secretRef.Status.Conditions[0]
	if condition.Type != string(lifecycle.ConditionReady) || condition.Status != string(lifecycle.ConditionTrue) || condition.Reason != "Synced" {
		t.Fatalf("Ready condition = %+v, want True/Synced", condition)
	}
}

func TestSecretRefReconcilerPublishesValidationStatus(t *testing.T) {
	for _, test := range []struct {
		name       string
		spec       v1alpha1.SecretRefSpec
		wantPhase  v1alpha1.Phase
		wantSynced bool
		wantStatus string
		wantReason string
		wantText   string
	}{
		{
			name: "valid legacy fields",
			spec: v1alpha1.SecretRefSpec{
				Provider:  "internal",
				Path:      "secret/openai",
				LegacyKey: "api-key",
			},
			wantPhase:  v1alpha1.PhaseReady,
			wantSynced: true,
			wantStatus: string(lifecycle.ConditionTrue),
			wantReason: "Synced",
			wantText:   `backend "internal"`,
		},
		{
			name:       "invalid missing backend",
			spec:       v1alpha1.SecretRefSpec{BackendRef: "secret/openai", Key: "api-key"},
			wantPhase:  v1alpha1.PhaseFailed,
			wantSynced: false,
			wantStatus: string(lifecycle.ConditionFalse),
			wantReason: "ValidationFailed",
			wantText:   "backendType is required",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			memoryStore := newMemStore()
			storeSecretRef(t, memoryStore, v1alpha1.SecretRef{
				ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "model-key"},
				Spec:       test.spec,
			})
			reconciler := NewSecretRefReconciler(memoryStore, zaptest.NewLogger(t))

			if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "model-key"}); err != nil {
				t.Fatalf("reconcile SecretRef: %v", err)
			}
			secretRef := getSecretRef(t, memoryStore, "tenant-a", "model-key")
			if secretRef.Status.Phase != test.wantPhase || secretRef.Status.Synced != test.wantSynced {
				t.Fatalf("status = %+v, want phase %q synced %t", secretRef.Status, test.wantPhase, test.wantSynced)
			}
			if secretRef.Status.LastSyncTime == nil {
				t.Fatal("lastSyncTime was not recorded")
			}
			if !strings.Contains(secretRef.Status.Message, test.wantText) {
				t.Fatalf("message = %q, want %q context", secretRef.Status.Message, test.wantText)
			}
			if len(secretRef.Status.Conditions) != 1 {
				t.Fatalf("conditions = %+v, want one Ready condition", secretRef.Status.Conditions)
			}
			condition := secretRef.Status.Conditions[0]
			if condition.Type != string(lifecycle.ConditionReady) || condition.Status != test.wantStatus || condition.Reason != test.wantReason {
				t.Fatalf("Ready condition = %+v, want status %q reason %q", condition, test.wantStatus, test.wantReason)
			}
		})
	}
}

func TestSecretRefReconcilerStableStatusDoesNotWrite(t *testing.T) {
	memoryStore := newMemStore()
	transitionTime := time.Now().UTC().Truncate(time.Second)
	storeSecretRef(t, memoryStore, v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "model-key"},
		Spec: v1alpha1.SecretRefSpec{
			BackendType: "internal",
			BackendRef:  "secret/openai",
			Key:         "api-key",
		},
		Status: v1alpha1.SecretRefStatus{
			Phase:   v1alpha1.PhaseReady,
			Message: `secret reference validated (backend "internal")`,
			Synced:  true,
			Conditions: []v1alpha1.Condition{{
				Type:               string(lifecycle.ConditionReady),
				Status:             string(lifecycle.ConditionTrue),
				LastTransitionTime: transitionTime,
				Reason:             "Synced",
				Message:            `secret reference validated (backend "internal")`,
			}},
		},
	})
	writeErr := errors.New("unexpected write")
	reconciler := NewSecretRefReconciler(&secretRefFaultStore{Store: memoryStore, updateErr: writeErr}, zaptest.NewLogger(t))

	result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "model-key"})
	if err != nil || result != (Result{}) {
		t.Fatalf("result = %+v, error = %v, want stable no-op", result, err)
	}
}

func TestSecretRefReconcilerStorageBoundaries(t *testing.T) {
	t.Run("wrapped not found is complete", func(t *testing.T) {
		reconciler := NewSecretRefReconciler(&secretRefFaultStore{
			Store:  newMemStore(),
			getErr: fmt.Errorf("wrapped missing SecretRef: %w", store.ErrNotFound),
		}, zaptest.NewLogger(t))
		result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "missing"})
		if err != nil || result != (Result{}) {
			t.Fatalf("result = %+v, error = %v, want completed not-found reconciliation", result, err)
		}
	})

	t.Run("get error is wrapped", func(t *testing.T) {
		getErr := errors.New("store unavailable")
		reconciler := NewSecretRefReconciler(&secretRefFaultStore{Store: newMemStore(), getErr: getErr}, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "model-key"})
		if !errors.Is(err, getErr) || !strings.Contains(err.Error(), "get SecretRef") {
			t.Fatalf("error = %v, want wrapped get error", err)
		}
	})

	t.Run("malformed record is rejected", func(t *testing.T) {
		memoryStore := newMemStore()
		if err := memoryStore.Create(context.Background(), secretRefKind, "tenant-a", "broken", []byte(`{"spec":`)); err != nil {
			t.Fatalf("store malformed SecretRef: %v", err)
		}
		reconciler := NewSecretRefReconciler(memoryStore, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "broken"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal SecretRef") {
			t.Fatalf("error = %v, want unmarshal context", err)
		}
	})

	for _, test := range []struct {
		name        string
		updateErr   error
		wantRequeue bool
		wantWrapped bool
	}{
		{name: "wrapped conflict requeues", updateErr: fmt.Errorf("wrapped conflict: %w", store.ErrConflict), wantRequeue: true},
		{name: "update error is wrapped", updateErr: errors.New("write unavailable"), wantWrapped: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			memoryStore := newMemStore()
			storeSecretRef(t, memoryStore, v1alpha1.SecretRef{
				ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "model-key"},
				Spec:       v1alpha1.SecretRefSpec{BackendType: "internal", BackendRef: "secret/openai", Key: "api-key"},
			})
			reconciler := NewSecretRefReconciler(&secretRefFaultStore{Store: memoryStore, updateErr: test.updateErr}, zaptest.NewLogger(t))
			result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "model-key"})
			if test.wantRequeue {
				if err != nil || !result.Requeue {
					t.Fatalf("result = %+v, error = %v, want conflict requeue", result, err)
				}
				return
			}
			if !test.wantWrapped || !errors.Is(err, test.updateErr) || !strings.Contains(err.Error(), "update SecretRef status") {
				t.Fatalf("error = %v, want wrapped update error", err)
			}
		})
	}
}
