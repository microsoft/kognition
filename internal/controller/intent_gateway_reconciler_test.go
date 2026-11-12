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

type intentGatewayFaultStore struct {
	store.Store
	getKind   string
	getErr    error
	updateErr error
}

type intentGatewayEventRecorder struct {
	updates []string
}

func (*intentGatewayEventRecorder) OnAdd(string)          {}
func (r *intentGatewayEventRecorder) OnUpdate(key string) { r.updates = append(r.updates, key) }
func (*intentGatewayEventRecorder) OnDelete(string)       {}

func (s *intentGatewayFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == s.getKind && s.getErr != nil {
		return nil, s.getErr
	}
	return s.Store.Get(ctx, kind, namespace, name)
}

func (s *intentGatewayFaultStore) Update(context.Context, string, string, string, []byte, int64) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return nil
}

func storeIntentGateway(t *testing.T, memoryStore *memStore, gateway v1alpha1.IntentGateway) {
	t.Helper()
	data, err := json.Marshal(gateway)
	if err != nil {
		t.Fatalf("marshal IntentGateway: %v", err)
	}
	if err := memoryStore.Create(context.Background(), intentGatewayKind, gateway.ObjectMeta.Namespace, gateway.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store IntentGateway: %v", err)
	}
}

func getIntentGateway(t *testing.T, memoryStore *memStore, namespace, name string) v1alpha1.IntentGateway {
	t.Helper()
	raw, err := memoryStore.Get(context.Background(), intentGatewayKind, namespace, name)
	if err != nil {
		t.Fatalf("get IntentGateway: %v", err)
	}
	var gateway v1alpha1.IntentGateway
	if err := json.Unmarshal(raw, &gateway); err != nil {
		t.Fatalf("unmarshal IntentGateway: %v", err)
	}
	return gateway
}

func storeIntentTarget(t *testing.T, memoryStore *memStore, kind, namespace, name string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": name, "namespace": namespace},
	})
	if err != nil {
		t.Fatalf("marshal %s target: %v", kind, err)
	}
	if err := memoryStore.Create(context.Background(), kind, namespace, name, data); err != nil {
		t.Fatalf("store %s target: %v", kind, err)
	}
}

func TestIntentGatewayReconcilerValidatesRoutesAndBecomesReady(t *testing.T) {
	memoryStore := newMemStore()
	storeIntentTarget(t, memoryStore, "Agent", "tenant-a", "planner")
	storeIntentTarget(t, memoryStore, "Workflow", "tenant-a", "triage")
	storeIntentGateway(t, memoryStore, v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "support"},
		Spec: v1alpha1.IntentGatewaySpec{Routes: []v1alpha1.IntentRoute{
			{Pattern: "plan:*", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"}},
			{Pattern: "triage:*", TargetKind: "Workflow", TargetRef: v1alpha1.LocalObjectReference{Name: "triage"}},
		}},
	})
	reconciler := NewIntentGatewayReconciler(memoryStore, zaptest.NewLogger(t))
	request := Request{Kind: intentGatewayKind, Namespace: "tenant-a", Name: "support"}

	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}
	result, err = reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("validate routes: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("result = %+v, want no requeue after routes become ready", result)
	}

	gateway := getIntentGateway(t, memoryStore, "tenant-a", "support")
	if !lifecycle.HasFinalizer(gateway.ObjectMeta.Finalizers, IntentGatewayFinalizer) {
		t.Fatalf("finalizers = %v, want %q", gateway.ObjectMeta.Finalizers, IntentGatewayFinalizer)
	}
	if gateway.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want %q", gateway.Status.Phase, v1alpha1.PhaseReady)
	}
	if gateway.Status.Endpoint != "/apis/kognition.dev/v1alpha1/namespaces/tenant-a/intentgateways/support/goals" {
		t.Fatalf("endpoint = %q", gateway.Status.Endpoint)
	}
	if len(gateway.Status.Conditions) != 1 || gateway.Status.Conditions[0].Reason != "RoutesValid" {
		t.Fatalf("conditions = %+v, want RoutesValid", gateway.Status.Conditions)
	}
}

func TestIntentGatewayReconcilerDoesNotAdvertiseUnavailableEndpoint(t *testing.T) {
	memoryStore := newMemStore()
	storeIntentTarget(t, memoryStore, "Agent", "tenant-a", "planner")
	storeIntentGateway(t, memoryStore, v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "support", Finalizers: []string{IntentGatewayFinalizer}},
		Spec: v1alpha1.IntentGatewaySpec{Routes: []v1alpha1.IntentRoute{{
			Pattern: "plan:*", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"},
		}}},
	})
	reconciler := NewIntentGatewayReconciler(memoryStore, zaptest.NewLogger(t), WithIntentGatewayServingAvailable(false))

	result, err := reconciler.Reconcile(context.Background(), Request{Kind: intentGatewayKind, Namespace: "tenant-a", Name: "support"})
	if err != nil || result != (Result{}) {
		t.Fatalf("reconcile result = %+v, err = %v", result, err)
	}
	gateway := getIntentGateway(t, memoryStore, "tenant-a", "support")
	if gateway.Status.Phase != v1alpha1.PhasePending || gateway.Status.Endpoint != "" || len(gateway.Status.Conditions) != 1 || gateway.Status.Conditions[0].Reason != "ServingUnavailable" {
		t.Fatalf("gateway status = %#v", gateway.Status)
	}
}

func TestIntentGatewayDependencyHandlerRequeuesReferencingGateways(t *testing.T) {
	memoryStore := newMemStore()
	storeIntentGateway(t, memoryStore, v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "agent-route"},
		Spec: v1alpha1.IntentGatewaySpec{Routes: []v1alpha1.IntentRoute{{
			TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"},
		}}},
	})
	storeIntentGateway(t, memoryStore, v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "workflow-route"},
		Spec: v1alpha1.IntentGatewaySpec{Routes: []v1alpha1.IntentRoute{{
			TargetKind: "Workflow", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"},
		}}},
	})
	storeIntentGateway(t, memoryStore, v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-b", Name: "other-namespace"},
		Spec: v1alpha1.IntentGatewaySpec{Routes: []v1alpha1.IntentRoute{{
			TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"},
		}}},
	})

	recorder := &intentGatewayEventRecorder{}
	handler := NewIntentGatewayDependencyHandler(memoryStore, "Agent", recorder, zaptest.NewLogger(t))
	handler.OnDelete(KeyFunc("tenant-a", "planner"))

	if len(recorder.updates) != 1 || recorder.updates[0] != KeyFunc("tenant-a", "agent-route") {
		t.Fatalf("queued gateways = %v, want [tenant-a/agent-route]", recorder.updates)
	}
}

func TestIntentGatewayReconcilerRejectsInvalidProtocol(t *testing.T) {
	memoryStore := newMemStore()
	storeIntentGateway(t, memoryStore, v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "invalid", Finalizers: []string{IntentGatewayFinalizer}},
		Spec:       v1alpha1.IntentGatewaySpec{Protocol: "grpc"},
	})
	reconciler := NewIntentGatewayReconciler(memoryStore, zaptest.NewLogger(t))
	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "invalid"}); err != nil {
		t.Fatalf("reconcile invalid protocol: %v", err)
	}
	gateway := getIntentGateway(t, memoryStore, "tenant-a", "invalid")
	if gateway.Status.Phase != v1alpha1.PhasePending || gateway.Status.Endpoint != "" || len(gateway.Status.Conditions) != 1 || gateway.Status.Conditions[0].Reason != "InvalidGatewayContract" {
		t.Fatalf("gateway status = %#v", gateway.Status)
	}
}

func TestIntentGatewayReconcilerMarksInvalidRoutesPending(t *testing.T) {
	for _, test := range []struct {
		name        string
		route       v1alpha1.IntentRoute
		wantMessage string
	}{
		{
			name:        "missing target name",
			route:       v1alpha1.IntentRoute{Pattern: "plan:*", TargetKind: "Agent"},
			wantMessage: "targetRef.name is required",
		},
		{
			name:        "blank route pattern",
			route:       v1alpha1.IntentRoute{Pattern: " ", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"}},
			wantMessage: "route pattern is required",
		},
		{
			name:        "invalid glob route pattern",
			route:       v1alpha1.IntentRoute{Pattern: "[", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"}},
			wantMessage: "invalid pattern",
		},
		{
			name:        "unsupported target kind",
			route:       v1alpha1.IntentRoute{Pattern: "plan:*", TargetKind: "Service", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"}},
			wantMessage: `unsupported targetKind "Service"`,
		},
		{
			name:        "missing target",
			route:       v1alpha1.IntentRoute{Pattern: "plan:*", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"}},
			wantMessage: `Agent "planner" not found`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			memoryStore := newMemStore()
			storeIntentGateway(t, memoryStore, v1alpha1.IntentGateway{
				ObjectMeta: v1alpha1.ObjectMeta{
					Namespace:  "tenant-a",
					Name:       "support",
					Finalizers: []string{IntentGatewayFinalizer},
				},
				Spec: v1alpha1.IntentGatewaySpec{Routes: []v1alpha1.IntentRoute{test.route}},
			})
			reconciler := NewIntentGatewayReconciler(memoryStore, zaptest.NewLogger(t))

			result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "support"})
			if err != nil {
				t.Fatalf("reconcile invalid route: %v", err)
			}
			if result.RequeueAfter != 10*time.Second {
				t.Fatalf("requeue after = %v, want 10s", result.RequeueAfter)
			}
			gateway := getIntentGateway(t, memoryStore, "tenant-a", "support")
			if gateway.Status.Phase != v1alpha1.PhasePending {
				t.Fatalf("phase = %q, want %q", gateway.Status.Phase, v1alpha1.PhasePending)
			}
			if len(gateway.Status.Conditions) != 1 || gateway.Status.Conditions[0].Reason != "RouteTargetNotFound" {
				t.Fatalf("conditions = %+v, want RouteTargetNotFound", gateway.Status.Conditions)
			}
			if message := gateway.Status.Conditions[0].Message; !strings.Contains(message, test.wantMessage) {
				t.Fatalf("condition message = %q, want %q context", message, test.wantMessage)
			}
		})
	}
}

func TestIntentGatewayReconcilerDeletionRemovesOnlyOwnedFinalizer(t *testing.T) {
	memoryStore := newMemStore()
	deletionTime := time.Now().UTC()
	storeIntentGateway(t, memoryStore, v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{
			Namespace:         "tenant-a",
			Name:              "support",
			DeletionTimestamp: &deletionTime,
			Finalizers:        []string{"external.example/cleanup", IntentGatewayFinalizer},
		},
	})
	reconciler := NewIntentGatewayReconciler(memoryStore, zaptest.NewLogger(t))

	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "support"}); err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}
	gateway := getIntentGateway(t, memoryStore, "tenant-a", "support")
	if lifecycle.HasFinalizer(gateway.ObjectMeta.Finalizers, IntentGatewayFinalizer) {
		t.Fatalf("finalizers = %v, still contains owned finalizer", gateway.ObjectMeta.Finalizers)
	}
	if !lifecycle.HasFinalizer(gateway.ObjectMeta.Finalizers, "external.example/cleanup") {
		t.Fatalf("finalizers = %v, unrelated finalizer was removed", gateway.ObjectMeta.Finalizers)
	}
}

func TestIntentGatewayReconcilerStorageBoundaries(t *testing.T) {
	t.Run("wrapped not found is complete", func(t *testing.T) {
		reconciler := NewIntentGatewayReconciler(&intentGatewayFaultStore{
			Store:   newMemStore(),
			getKind: intentGatewayKind,
			getErr:  fmt.Errorf("wrapped missing gateway: %w", store.ErrNotFound),
		}, zaptest.NewLogger(t))
		result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "missing"})
		if err != nil || result != (Result{}) {
			t.Fatalf("result = %+v, error = %v, want completed not-found reconciliation", result, err)
		}
	})

	t.Run("get error is wrapped", func(t *testing.T) {
		getErr := errors.New("store unavailable")
		reconciler := NewIntentGatewayReconciler(&intentGatewayFaultStore{
			Store:   newMemStore(),
			getKind: intentGatewayKind,
			getErr:  getErr,
		}, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "support"})
		if !errors.Is(err, getErr) || !strings.Contains(err.Error(), "get intent gateway") {
			t.Fatalf("error = %v, want wrapped gateway read error", err)
		}
	})

	t.Run("malformed gateway is rejected", func(t *testing.T) {
		memoryStore := newMemStore()
		if err := memoryStore.Create(context.Background(), intentGatewayKind, "tenant-a", "broken", []byte(`{"spec":`)); err != nil {
			t.Fatalf("store malformed IntentGateway: %v", err)
		}
		reconciler := NewIntentGatewayReconciler(&intentGatewayFaultStore{
			Store:   memoryStore,
			getKind: "Agent",
			getErr:  fmt.Errorf("wrapped missing route target: %w", store.ErrNotFound),
		}, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "broken"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal intent gateway") {
			t.Fatalf("error = %v, want unmarshal context", err)
		}
	})

	t.Run("finalizer update error is wrapped", func(t *testing.T) {
		memoryStore := newMemStore()
		storeIntentGateway(t, memoryStore, v1alpha1.IntentGateway{
			ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Name: "support"},
		})
		updateErr := errors.New("write unavailable")
		reconciler := NewIntentGatewayReconciler(&intentGatewayFaultStore{
			Store:     memoryStore,
			updateErr: updateErr,
		}, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "support"})
		if !errors.Is(err, updateErr) || !strings.Contains(err.Error(), "add finalizer") {
			t.Fatalf("error = %v, want wrapped finalizer update error", err)
		}
	})

	t.Run("ready gateway becomes pending when target disappears", func(t *testing.T) {
		memoryStore := newMemStore()
		storeIntentGateway(t, memoryStore, v1alpha1.IntentGateway{
			ObjectMeta: v1alpha1.ObjectMeta{
				Namespace:  "tenant-a",
				Name:       "support",
				Finalizers: []string{IntentGatewayFinalizer},
			},
			Spec: v1alpha1.IntentGatewaySpec{Routes: []v1alpha1.IntentRoute{{
				Pattern: "plan:*", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "deleted-agent"},
			}}},
			Status: v1alpha1.IntentGatewayStatus{Phase: v1alpha1.PhaseReady},
		})
		reconciler := NewIntentGatewayReconciler(memoryStore, zaptest.NewLogger(t))
		result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "support"})
		if err != nil || result.RequeueAfter != 10*time.Second {
			t.Fatalf("result = %+v, error = %v, want pending requeue", result, err)
		}
		gateway := getIntentGateway(t, memoryStore, "tenant-a", "support")
		if gateway.Status.Phase != v1alpha1.PhasePending || gateway.Status.Endpoint != "" || len(gateway.Status.Conditions) != 1 || gateway.Status.Conditions[0].Reason != "RouteTargetNotFound" {
			t.Fatalf("gateway status = %#v", gateway.Status)
		}
	})
}
