// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

type identityFaultStore struct {
	store.Store
	getErr    error
	listErr   error
	updateErr error
}

func (s *identityFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.Store.Get(ctx, kind, namespace, name)
}

func (s *identityFaultStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.Store.List(ctx, kind, opts)
}

func (s *identityFaultStore) Update(ctx context.Context, kind, namespace, name string, data []byte, version int64) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.Store.Update(ctx, kind, namespace, name, data, version)
}

func TestAgentIdentityReconcilerStableIdentityDoesNotWrite(t *testing.T) {
	memoryStore := newMemStore()
	identity := makeAgentIdentity("tenant-a", "planner-id", "planner")
	identity.ObjectMeta.Finalizers = []string{AgentIdentityFinalizer}
	identity.Spec.SPIFFEID = "spiffe://kognition.dev/workspace/tenant-a/agent/planner"
	identity.Status.Phase = v1alpha1.PhaseReady
	storeAgentIdentity(t, memoryStore, identity)
	writeErr := errors.New("unexpected write")
	reconciler := NewAgentIdentityReconciler(&identityFaultStore{Store: memoryStore, updateErr: writeErr}, zaptest.NewLogger(t))

	result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "planner-id"})
	if err != nil || result != (Result{}) {
		t.Fatalf("result = %+v, error = %v, want stable no-op", result, err)
	}
}

type identityStatusReader struct {
	status runtimeauth.WorkloadIdentityStatus
	err    error
	uid    string
	calls  int
}

func (reader *identityStatusReader) AgentStatus(namespace, name, uid string) (runtimeauth.WorkloadIdentityStatus, error) {
	reader.calls++
	if namespace != "tenant-a" || name != "planner" || uid != reader.uid {
		return runtimeauth.WorkloadIdentityStatus{}, runtimeauth.ErrWorkloadIdentityUnavailable
	}
	return reader.status, reader.err
}

func TestAgentIdentityReconcilerProjectsVerifiedWorkloadStatus(t *testing.T) {
	ctx := context.Background()
	memoryStore := newMemStore()
	agent := makeAgent("tenant-a", "planner", 1)
	agent.ObjectMeta.UID = "70000000-0000-4000-8000-000000000001"
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Create(ctx, "Agent", "tenant-a", "planner", data); err != nil {
		t.Fatal(err)
	}
	identity := makeAgentIdentity("tenant-a", defaultAgentIdentityName("planner"), "planner")
	identity.ObjectMeta.Finalizers = []string{AgentIdentityFinalizer}
	identity.ObjectMeta.Labels = map[string]string{autoAgentIdentityLabel: "planner"}
	identity.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{APIVersion: v1alpha1.APIVersion, Kind: "Agent", Name: "planner", UID: agent.ObjectMeta.UID}}
	storeAgentIdentity(t, memoryStore, identity)
	spiffeID, err := runtimeauth.AgentIncarnationSPIFFEID("kognition.dev", "tenant-a", "planner", agent.ObjectMeta.UID)
	if err != nil {
		t.Fatal(err)
	}
	reader := &identityStatusReader{uid: agent.ObjectMeta.UID, status: runtimeauth.WorkloadIdentityStatus{
		SPIFFEID: spiffeID, TrustDomain: "kognition.dev", NotBefore: time.Now().Add(-time.Minute).UTC(),
		ExpiresAt: time.Now().Add(time.Hour).UTC(), Generation: 1, Ready: true, Reason: "Ready",
	}}
	reconciler := NewAgentIdentityReconcilerWithWorkloadIdentity(memoryStore, zaptest.NewLogger(t), reader)
	reconcile := func() *v1alpha1.WorkloadIdentityStatus {
		t.Helper()
		result, err := reconciler.Reconcile(ctx, Request{Namespace: "tenant-a", Name: identity.ObjectMeta.Name})
		if err != nil || result.RequeueAfter != 5*time.Second {
			t.Fatalf("result=%+v error=%v", result, err)
		}
		raw, err := memoryStore.Get(ctx, agentIdentityKind, "tenant-a", identity.ObjectMeta.Name)
		identity.Status = v1alpha1.AgentIdentityStatus{}
		if err != nil || json.Unmarshal(raw, identity) != nil {
			t.Fatal("cannot read projected identity status")
		}
		if identity.Status.Phase != v1alpha1.PhaseReady || identity.Spec.SPIFFEID != "spiffe://kognition.dev/workspace/tenant-a/agent/planner" {
			t.Fatal("legacy informational identity changed")
		}
		return identity.Status.WorkloadIdentity
	}
	status := reconcile()
	if status == nil || !status.Ready || status.AgentUID != agent.ObjectMeta.UID || status.SPIFFEID != spiffeID || status.SourceGeneration != 1 {
		t.Fatalf("verified status=%+v", status)
	}
	reader.status.Generation = 2
	reader.status.ExpiresAt = reader.status.ExpiresAt.Add(time.Hour)
	if status = reconcile(); !status.Ready || status.SourceGeneration != 2 || !status.ExpiresAt.Equal(reader.status.ExpiresAt) {
		t.Fatalf("rotated status=%+v", status)
	}
	reader.err = errors.New("private issuer error")
	if status = reconcile(); status.Ready || status.Reason != "IssuerUnavailable" || status.SPIFFEID != "" {
		t.Fatalf("issuer outage retained readiness=%+v", status)
	}
	reader.err = nil
	reader.status.ExpiresAt = time.Now().Add(-time.Second)
	if status = reconcile(); status.Ready || status.Reason != "ExpiredIdentity" {
		t.Fatalf("expired status=%+v", status)
	}
	reader.status.ExpiresAt = time.Now().Add(time.Hour)
	reader.status.SPIFFEID = strings.Replace(spiffeID, "000000000001", "000000000002", 1)
	if status = reconcile(); status.Ready || status.Reason != "InvalidIdentity" {
		t.Fatalf("old incarnation retained readiness=%+v", status)
	}
	reader.status.SPIFFEID = spiffeID
	if status = reconcile(); !status.Ready {
		t.Fatalf("valid source did not recover=%+v", status)
	}
	validStatus := reader.status
	for _, scenario := range []struct {
		name   string
		change func(*runtimeauth.WorkloadIdentityStatus)
		reason string
	}{
		{"future certificate", func(status *runtimeauth.WorkloadIdentityStatus) { status.NotBefore = time.Now().Add(time.Minute) }, "ExpiredIdentity"},
		{"missing generation", func(status *runtimeauth.WorkloadIdentityStatus) { status.Generation = 0 }, "InvalidIdentity"},
		{"generation overflow", func(status *runtimeauth.WorkloadIdentityStatus) { status.Generation = uint64(math.MaxInt64) + 1 }, "InvalidIdentity"},
		{"missing validity", func(status *runtimeauth.WorkloadIdentityStatus) { status.ExpiresAt = time.Time{} }, "InvalidIdentity"},
		{"unready source", func(status *runtimeauth.WorkloadIdentityStatus) {
			status.Ready = false
			status.Reason = "IssuerUnavailable"
		}, "IssuerUnavailable"},
		{"private issuer diagnostics", func(status *runtimeauth.WorkloadIdentityStatus) {
			status.Ready = false
			status.Reason = "private issuer credential details"
		}, "InvalidIdentity"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			reader.status = validStatus
			scenario.change(&reader.status)
			status := reconcile()
			if status.Ready || status.Reason != scenario.reason {
				t.Fatalf("invalid source retained readiness=%+v", status)
			}
		})
	}
	reader.status = validStatus
	if status = reconcile(); !status.Ready {
		t.Fatalf("valid source did not recover after invalid evidence=%+v", status)
	}
	stable := NewAgentIdentityReconcilerWithWorkloadIdentity(&identityFaultStore{Store: memoryStore, updateErr: errors.New("unexpected write")}, zaptest.NewLogger(t), reader)
	if _, err := stable.Reconcile(ctx, Request{Namespace: "tenant-a", Name: identity.ObjectMeta.Name}); err != nil {
		t.Fatalf("unchanged verified status was not a no-op: %v", err)
	}
	reader.calls = 0
	currentAgent, err := getStoredResource[v1alpha1.Agent](ctx, memoryStore, "Agent", "tenant-a", "planner")
	if err != nil {
		t.Fatal(err)
	}
	agent = *currentAgent
	agent.ObjectMeta.UID = "70000000-0000-4000-8000-000000000002"
	data, err = json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Update(ctx, "Agent", "tenant-a", "planner", data, agent.ObjectMeta.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	if status = reconcile(); status.Ready || status.Reason != "AgentBindingInvalid" || reader.calls != 0 || status.AgentUID != "" {
		t.Fatalf("recreated Agent used stale evidence=%+v calls=%d", status, reader.calls)
	}
	legacy := NewAgentIdentityReconciler(memoryStore, zaptest.NewLogger(t))
	if _, err := legacy.Reconcile(ctx, Request{Namespace: "tenant-a", Name: identity.ObjectMeta.Name}); err != nil {
		t.Fatal(err)
	}
	raw, err := memoryStore.Get(ctx, agentIdentityKind, "tenant-a", identity.ObjectMeta.Name)
	identity.Status = v1alpha1.AgentIdentityStatus{}
	if err != nil || json.Unmarshal(raw, identity) != nil || identity.Status.WorkloadIdentity != nil {
		t.Fatal("unconfigured reader retained an unverified certificate claim")
	}
}

func TestAgentIdentityReconcilerRepairsStaleIdentity(t *testing.T) {
	memoryStore := newMemStore()
	identity := makeAgentIdentity("tenant-a", "planner-id", "planner")
	identity.ObjectMeta.Finalizers = []string{AgentIdentityFinalizer}
	identity.Spec.SPIFFEID = "spiffe://legacy.example/planner"
	storeAgentIdentity(t, memoryStore, identity)
	reconciler := NewAgentIdentityReconciler(memoryStore, zaptest.NewLogger(t))

	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "planner-id"}); err != nil {
		t.Fatalf("reconcile AgentIdentity: %v", err)
	}
	raw, err := memoryStore.Get(context.Background(), agentIdentityKind, "tenant-a", "planner-id")
	if err != nil {
		t.Fatalf("get AgentIdentity: %v", err)
	}
	if err := json.Unmarshal(raw, &identity); err != nil {
		t.Fatalf("unmarshal AgentIdentity: %v", err)
	}
	wantSPIFFEID := "spiffe://kognition.dev/workspace/tenant-a/agent/planner"
	if identity.Spec.SPIFFEID != wantSPIFFEID || identity.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("identity = %+v, want SPIFFE ID %q and Ready phase", identity, wantSPIFFEID)
	}
}

func TestAgentIdentityReconcilerManagesFinalizerLifecycle(t *testing.T) {
	t.Run("adds finalizer before issuing identity", func(t *testing.T) {
		memoryStore := newMemStore()
		storeAgentIdentity(t, memoryStore, makeAgentIdentity("tenant-a", "planner-id", "planner"))
		reconciler := NewAgentIdentityReconciler(memoryStore, zaptest.NewLogger(t))

		result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "planner-id"})
		if err != nil || !result.Requeue {
			t.Fatalf("result = %+v, error = %v, want requeue after finalizer write", result, err)
		}
		raw, err := memoryStore.Get(context.Background(), agentIdentityKind, "tenant-a", "planner-id")
		if err != nil {
			t.Fatalf("get AgentIdentity: %v", err)
		}
		var identity v1alpha1.AgentIdentity
		if err := json.Unmarshal(raw, &identity); err != nil {
			t.Fatalf("unmarshal AgentIdentity: %v", err)
		}
		if len(identity.ObjectMeta.Finalizers) != 1 || identity.ObjectMeta.Finalizers[0] != AgentIdentityFinalizer {
			t.Fatalf("finalizers = %v, want identity cleanup finalizer", identity.ObjectMeta.Finalizers)
		}
		if identity.Spec.SPIFFEID != "" || identity.Status.Phase != "" {
			t.Fatalf("identity was issued before finalizer persistence: %+v", identity)
		}
	})

	t.Run("removes owned finalizer during deletion", func(t *testing.T) {
		memoryStore := newMemStore()
		identity := makeAgentIdentity("tenant-a", "planner-id", "planner")
		now := time.Now().UTC()
		identity.ObjectMeta.DeletionTimestamp = &now
		identity.ObjectMeta.Finalizers = []string{"example.test/other", AgentIdentityFinalizer}
		storeAgentIdentity(t, memoryStore, identity)
		reconciler := NewAgentIdentityReconciler(memoryStore, zaptest.NewLogger(t))

		if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "planner-id"}); err != nil {
			t.Fatalf("reconcile deleting AgentIdentity: %v", err)
		}
		raw, err := memoryStore.Get(context.Background(), agentIdentityKind, "tenant-a", "planner-id")
		if err != nil {
			t.Fatalf("get AgentIdentity: %v", err)
		}
		if err := json.Unmarshal(raw, &identity); err != nil {
			t.Fatalf("unmarshal AgentIdentity: %v", err)
		}
		if len(identity.ObjectMeta.Finalizers) != 1 || identity.ObjectMeta.Finalizers[0] != "example.test/other" {
			t.Fatalf("finalizers = %v, want unrelated finalizer preserved", identity.ObjectMeta.Finalizers)
		}
	})

	t.Run("deletion without owned finalizer is stable", func(t *testing.T) {
		memoryStore := newMemStore()
		identity := makeAgentIdentity("tenant-a", "planner-id", "planner")
		now := time.Now().UTC()
		identity.ObjectMeta.DeletionTimestamp = &now
		storeAgentIdentity(t, memoryStore, identity)
		writeErr := errors.New("unexpected write")
		reconciler := NewAgentIdentityReconciler(&identityFaultStore{Store: memoryStore, updateErr: writeErr}, zaptest.NewLogger(t))

		result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "planner-id"})
		if err != nil || result != (Result{}) {
			t.Fatalf("result = %+v, error = %v, want stable deletion no-op", result, err)
		}
	})
}

func TestAgentIdentityReconcilerStorageBoundaries(t *testing.T) {
	t.Run("not found is complete", func(t *testing.T) {
		reconciler := NewAgentIdentityReconciler(&identityFaultStore{
			Store:  newMemStore(),
			getErr: fmt.Errorf("wrapped missing identity: %w", store.ErrNotFound),
		}, zaptest.NewLogger(t))
		result, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "missing"})
		if err != nil || result != (Result{}) {
			t.Fatalf("result = %+v, error = %v, want completed not-found reconciliation", result, err)
		}
	})

	t.Run("get error is wrapped", func(t *testing.T) {
		getErr := errors.New("store unavailable")
		reconciler := NewAgentIdentityReconciler(&identityFaultStore{Store: newMemStore(), getErr: getErr}, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "planner-id"})
		if !errors.Is(err, getErr) || !strings.Contains(err.Error(), "get agent identity") {
			t.Fatalf("error = %v, want wrapped get error", err)
		}
	})

	t.Run("malformed record is rejected", func(t *testing.T) {
		memoryStore := newMemStore()
		if err := memoryStore.Create(context.Background(), agentIdentityKind, "tenant-a", "broken", []byte(`{"spec":`)); err != nil {
			t.Fatalf("store malformed AgentIdentity: %v", err)
		}
		reconciler := NewAgentIdentityReconciler(memoryStore, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "broken"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal agent identity") {
			t.Fatalf("error = %v, want unmarshal context", err)
		}
	})

	for _, test := range []struct {
		name        string
		prepare     func(*v1alpha1.AgentIdentity)
		wantContext string
	}{
		{name: "finalizer write error is wrapped", wantContext: "add finalizer"},
		{
			name: "identity write error is wrapped",
			prepare: func(identity *v1alpha1.AgentIdentity) {
				identity.ObjectMeta.Finalizers = []string{AgentIdentityFinalizer}
			},
			wantContext: "ensure spiffe id",
		},
		{
			name: "finalizer removal error is wrapped",
			prepare: func(identity *v1alpha1.AgentIdentity) {
				now := time.Now().UTC()
				identity.ObjectMeta.DeletionTimestamp = &now
				identity.ObjectMeta.Finalizers = []string{AgentIdentityFinalizer}
			},
			wantContext: "remove finalizer",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			memoryStore := newMemStore()
			identity := makeAgentIdentity("tenant-a", "planner-id", "planner")
			if test.prepare != nil {
				test.prepare(identity)
			}
			storeAgentIdentity(t, memoryStore, identity)
			writeErr := errors.New("write unavailable")
			reconciler := NewAgentIdentityReconciler(&identityFaultStore{Store: memoryStore, updateErr: writeErr}, zaptest.NewLogger(t))

			_, err := reconciler.Reconcile(context.Background(), Request{Namespace: "tenant-a", Name: "planner-id"})
			if !errors.Is(err, writeErr) || !strings.Contains(err.Error(), test.wantContext) {
				t.Fatalf("error = %v, want wrapped write error with %q context", err, test.wantContext)
			}
		})
	}
}

func TestResolveAgentIdentityStorageAndTenantBoundaries(t *testing.T) {
	t.Run("resolution remains namespace scoped", func(t *testing.T) {
		memoryStore := newMemStore()
		storeAgentIdentity(t, memoryStore, makeAgentIdentity("tenant-b", "planner-b", "planner"))
		storeAgentIdentity(t, memoryStore, makeAgentIdentity("tenant-a", "planner-a", "planner"))

		identity, err := ResolveAgentIdentity(context.Background(), memoryStore, "tenant-a", "planner")
		if err != nil {
			t.Fatalf("resolve AgentIdentity: %v", err)
		}
		if identity.ObjectMeta.Namespace != "tenant-a" || identity.ObjectMeta.Name != "planner-a" {
			t.Fatalf("resolved identity = %s/%s, want tenant-a/planner-a", identity.ObjectMeta.Namespace, identity.ObjectMeta.Name)
		}
	})

	t.Run("malformed identities are skipped", func(t *testing.T) {
		memoryStore := newMemStore()
		if err := memoryStore.Create(context.Background(), agentIdentityKind, "tenant-a", "broken", []byte(`{"spec":`)); err != nil {
			t.Fatalf("store malformed AgentIdentity: %v", err)
		}
		storeAgentIdentity(t, memoryStore, makeAgentIdentity("tenant-a", "planner-id", "planner"))

		identity, err := ResolveAgentIdentity(context.Background(), memoryStore, "tenant-a", "planner")
		if err != nil || identity.ObjectMeta.Name != "planner-id" {
			t.Fatalf("identity = %+v, error = %v, want valid identity after malformed record", identity, err)
		}
	})

	t.Run("list error is wrapped", func(t *testing.T) {
		listErr := errors.New("list unavailable")
		_, err := ResolveAgentIdentity(context.Background(), &identityFaultStore{Store: newMemStore(), listErr: listErr}, "tenant-a", "planner")
		if !errors.Is(err, listErr) || !strings.Contains(err.Error(), "list identities") {
			t.Fatalf("error = %v, want wrapped list error", err)
		}
	})

	t.Run("multiple active identities are rejected", func(t *testing.T) {
		memoryStore := newMemStore()
		storeAgentIdentity(t, memoryStore, makeAgentIdentity("tenant-a", "planner-a", "planner"))
		storeAgentIdentity(t, memoryStore, makeAgentIdentity("tenant-a", "planner-b", "planner"))
		_, err := ResolveAgentIdentity(context.Background(), memoryStore, "tenant-a", "planner")
		if !errors.Is(err, ErrAgentIdentityAmbiguous) {
			t.Fatalf("error = %v, want ErrAgentIdentityAmbiguous", err)
		}
	})
}

func TestResolveAuthorizationAgentIdentityRejectsManualIdentity(t *testing.T) {
	ctx := context.Background()
	memoryStore := newMemStore()
	agent := makeAgent("tenant-a", "planner", 1)
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Create(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	identity := makeAgentIdentity("tenant-a", "planner-manual", "planner")
	identity.Spec.SPIFFEID = "spiffe://kognition.dev/workspace/tenant-a/agent/planner"
	identity.Status.Phase = v1alpha1.PhaseReady
	storeAgentIdentity(t, memoryStore, identity)

	_, err = ResolveAuthorizationAgentIdentity(ctx, memoryStore, "tenant-a", "planner")
	if !errors.Is(err, ErrAgentIdentityNotFound) {
		t.Fatalf("error = %v, want ErrAgentIdentityNotFound", err)
	}
}
