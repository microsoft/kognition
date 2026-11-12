// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type workspaceFaultStore struct {
	store.Store
	getErrKind  string
	getErr      error
	listErrKind string
	listErr     error
	createErr   error
	updateErr   error
}

func (s *workspaceFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if s.getErr != nil && (s.getErrKind == "" || s.getErrKind == kind) {
		return nil, s.getErr
	}
	return s.Store.Get(ctx, kind, namespace, name)
}

func (s *workspaceFaultStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if s.listErr != nil && (s.listErrKind == "" || s.listErrKind == kind) {
		return nil, s.listErr
	}
	return s.Store.List(ctx, kind, opts)
}

func (s *workspaceFaultStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if s.createErr != nil {
		return s.createErr
	}
	return s.Store.Create(ctx, kind, namespace, name, data)
}

func (s *workspaceFaultStore) Update(ctx context.Context, kind, namespace, name string, data []byte, version int64) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.Store.Update(ctx, kind, namespace, name, data, version)
}

func TestWorkspaceReconcilerStableWorkspaceDoesNotWrite(t *testing.T) {
	memoryStore := newMemStore()
	workspace := makeWorkspace("", "tenant-a")
	workspace.ObjectMeta.Finalizers = []string{WorkspaceFinalizer}
	workspace.Status.Phase = v1alpha1.PhaseReady
	storeWorkspace(t, memoryStore, workspace)
	storeCommPolicy(t, memoryStore, makeCommPolicy("tenant-a", "tenant-a-default-deny", v1alpha1.AgentCommunicationPolicySpec{DefaultDeny: true}))
	writeErr := errors.New("unexpected write")
	reconciler := NewWorkspaceReconciler(&workspaceFaultStore{Store: memoryStore, updateErr: writeErr}, zaptest.NewLogger(t))

	result, err := reconciler.Reconcile(context.Background(), Request{Name: "tenant-a"})
	if err != nil || result != (Result{}) {
		t.Fatalf("result = %+v, error = %v, want stable no-op", result, err)
	}
}

func TestWorkspaceReconcilerUsesMetadataNamespaceForOwnedResources(t *testing.T) {
	memoryStore := newMemStore()
	workspace := makeWorkspace("governance-system", "governance-sample")
	workspace.ObjectMeta.Finalizers = []string{WorkspaceFinalizer}
	storeWorkspace(t, memoryStore, workspace)
	reconciler := NewWorkspaceReconciler(memoryStore, zaptest.NewLogger(t))

	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "governance-system", Name: "governance-sample"}); err != nil {
		t.Fatalf("reconcile namespaced Workspace: %v", err)
	}
	policyName := "governance-sample-default-deny"
	if _, err := memoryStore.Get(context.Background(), "AgentCommunicationPolicy", "governance-system", policyName); err != nil {
		t.Fatalf("get policy from Workspace namespace: %v", err)
	}
	if _, err := memoryStore.Get(context.Background(), "AgentCommunicationPolicy", "governance-sample", policyName); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("policy unexpectedly created in Workspace-name namespace: %v", err)
	}
}

func TestWorkspaceReconcilerFinalizerLifecycle(t *testing.T) {
	t.Run("persists finalizer before creating policy", func(t *testing.T) {
		memoryStore := newMemStore()
		storeWorkspace(t, memoryStore, makeWorkspace("", "tenant-a"))
		createErr := errors.New("policy creation must not run")
		reconciler := NewWorkspaceReconciler(&workspaceFaultStore{Store: memoryStore, createErr: createErr}, zaptest.NewLogger(t))

		result, err := reconciler.Reconcile(context.Background(), Request{Name: "tenant-a"})
		if err != nil || !result.Requeue {
			t.Fatalf("result = %+v, error = %v, want requeue after finalizer write", result, err)
		}
		workspace := getStoredWorkspace(t, memoryStore, "tenant-a")
		if len(workspace.ObjectMeta.Finalizers) != 1 || workspace.ObjectMeta.Finalizers[0] != WorkspaceFinalizer {
			t.Fatalf("finalizers = %v, want workspace cleanup finalizer", workspace.ObjectMeta.Finalizers)
		}
	})

	t.Run("removes only owned finalizer during deletion", func(t *testing.T) {
		memoryStore := newMemStore()
		workspace := makeWorkspace("", "tenant-a")
		now := time.Now().UTC()
		workspace.ObjectMeta.DeletionTimestamp = &now
		workspace.ObjectMeta.Finalizers = []string{"example.test/other", WorkspaceFinalizer}
		storeWorkspace(t, memoryStore, workspace)
		reconciler := NewWorkspaceReconciler(memoryStore, zaptest.NewLogger(t))

		if _, err := reconciler.Reconcile(context.Background(), Request{Name: "tenant-a"}); err != nil {
			t.Fatalf("reconcile deleting Workspace: %v", err)
		}
		workspace = getStoredWorkspace(t, memoryStore, "tenant-a")
		if len(workspace.ObjectMeta.Finalizers) != 1 || workspace.ObjectMeta.Finalizers[0] != "example.test/other" {
			t.Fatalf("finalizers = %v, want unrelated finalizer preserved", workspace.ObjectMeta.Finalizers)
		}
	})

	t.Run("deletion without owned finalizer is stable", func(t *testing.T) {
		memoryStore := newMemStore()
		workspace := makeWorkspace("", "tenant-a")
		now := time.Now().UTC()
		workspace.ObjectMeta.DeletionTimestamp = &now
		storeWorkspace(t, memoryStore, workspace)
		writeErr := errors.New("unexpected write")
		reconciler := NewWorkspaceReconciler(&workspaceFaultStore{Store: memoryStore, updateErr: writeErr}, zaptest.NewLogger(t))

		result, err := reconciler.Reconcile(context.Background(), Request{Name: "tenant-a"})
		if err != nil || result != (Result{}) {
			t.Fatalf("result = %+v, error = %v, want stable deletion no-op", result, err)
		}
	})
}

func TestWorkspaceReconcilerStorageBoundaries(t *testing.T) {
	t.Run("not found is complete", func(t *testing.T) {
		reconciler := NewWorkspaceReconciler(newMemStore(), zaptest.NewLogger(t))
		result, err := reconciler.Reconcile(context.Background(), Request{Name: "missing"})
		if err != nil || result != (Result{}) {
			t.Fatalf("result = %+v, error = %v, want completed not-found reconciliation", result, err)
		}
	})

	t.Run("malformed workspace is rejected", func(t *testing.T) {
		memoryStore := newMemStore()
		if err := memoryStore.Create(context.Background(), workspaceKind, "", "broken", []byte(`{"spec":`)); err != nil {
			t.Fatalf("store malformed Workspace: %v", err)
		}
		reconciler := NewWorkspaceReconciler(memoryStore, zaptest.NewLogger(t))
		_, err := reconciler.Reconcile(context.Background(), Request{Name: "broken"})
		if err == nil || !strings.Contains(err.Error(), "unmarshal workspace") {
			t.Fatalf("error = %v, want unmarshal context", err)
		}
	})

	t.Run("unsupported policy kind fails closed", func(t *testing.T) {
		memoryStore := readyWorkspaceStore(t, "tenant-a", false)
		reconciler := NewWorkspaceReconciler(&workspaceFaultStore{
			Store:      memoryStore,
			getErrKind: "AgentCommunicationPolicy",
			getErr:     errors.New("unsupported kind AgentCommunicationPolicy"),
		}, zaptest.NewLogger(t))

		if _, err := reconciler.Reconcile(context.Background(), Request{Name: "tenant-a"}); err == nil || !strings.Contains(err.Error(), "check default policy") {
			t.Fatalf("error = %v, want fail-closed policy lookup error", err)
		}
	})

	t.Run("policy creation race is idempotent", func(t *testing.T) {
		memoryStore := readyWorkspaceStore(t, "tenant-a", false)
		reconciler := NewWorkspaceReconciler(&workspaceFaultStore{Store: memoryStore, createErr: store.ErrAlreadyExists}, zaptest.NewLogger(t))

		if _, err := reconciler.Reconcile(context.Background(), Request{Name: "tenant-a"}); err != nil {
			t.Fatalf("reconcile concurrent policy creation: %v", err)
		}
	})

	for _, test := range []struct {
		name        string
		prepare     func(*v1alpha1.Workspace, *memStore)
		wantContext string
	}{
		{name: "finalizer write error is wrapped", wantContext: "add finalizer"},
		{
			name: "finalizer removal error is wrapped",
			prepare: func(workspace *v1alpha1.Workspace, _ *memStore) {
				now := time.Now().UTC()
				workspace.ObjectMeta.DeletionTimestamp = &now
				workspace.ObjectMeta.Finalizers = []string{WorkspaceFinalizer}
			},
			wantContext: "remove finalizer",
		},
		{
			name: "status write error is wrapped",
			prepare: func(workspace *v1alpha1.Workspace, memoryStore *memStore) {
				workspace.ObjectMeta.Finalizers = []string{WorkspaceFinalizer}
				storeCommPolicy(t, memoryStore, makeCommPolicy("tenant-a", "tenant-a-default-deny", v1alpha1.AgentCommunicationPolicySpec{DefaultDeny: true}))
			},
			wantContext: "update workspace status",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			memoryStore := newMemStore()
			workspace := makeWorkspace("", "tenant-a")
			if test.prepare != nil {
				test.prepare(workspace, memoryStore)
			}
			storeWorkspace(t, memoryStore, workspace)
			writeErr := errors.New("write unavailable")
			reconciler := NewWorkspaceReconciler(&workspaceFaultStore{Store: memoryStore, updateErr: writeErr}, zaptest.NewLogger(t))

			_, err := reconciler.Reconcile(context.Background(), Request{Name: "tenant-a"})
			if !errors.Is(err, writeErr) || !strings.Contains(err.Error(), test.wantContext) {
				t.Fatalf("error = %v, want wrapped write error with %q context", err, test.wantContext)
			}
		})
	}

	for _, test := range []struct {
		name        string
		configure   func(*workspaceFaultStore)
		wantContext string
	}{
		{
			name: "workspace get error is wrapped",
			configure: func(faults *workspaceFaultStore) {
				faults.getErrKind = workspaceKind
				faults.getErr = errors.New("get unavailable")
			},
			wantContext: "get workspace",
		},
		{
			name: "policy get error is wrapped",
			configure: func(faults *workspaceFaultStore) {
				faults.getErrKind = "AgentCommunicationPolicy"
				faults.getErr = errors.New("policy lookup unavailable")
			},
			wantContext: "check default policy",
		},
		{
			name: "policy create error is wrapped",
			configure: func(faults *workspaceFaultStore) {
				faults.createErr = errors.New("create unavailable")
			},
			wantContext: "create default policy",
		},
		{
			name: "agent list error is wrapped",
			configure: func(faults *workspaceFaultStore) {
				faults.listErrKind = "Agent"
				faults.listErr = errors.New("list unavailable")
			},
			wantContext: "list agents",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			memoryStore := readyWorkspaceStore(t, "tenant-a", test.wantContext == "list agents")
			faults := &workspaceFaultStore{Store: memoryStore}
			test.configure(faults)
			reconciler := NewWorkspaceReconciler(faults, zaptest.NewLogger(t))

			_, err := reconciler.Reconcile(context.Background(), Request{Name: "tenant-a"})
			if err == nil || !strings.Contains(err.Error(), test.wantContext) {
				t.Fatalf("error = %v, want %q context", err, test.wantContext)
			}
		})
	}
}

func TestValidateWorkspaceBoundaryContracts(t *testing.T) {
	ctx := context.Background()
	if err := ValidateWorkspaceBoundary(ctx, newMemStore(), ""); err == nil || !strings.Contains(err.Error(), "namespace is required") {
		t.Fatalf("empty namespace error = %v", err)
	}
	if err := ValidateWorkspaceBoundary(ctx, newMemStore(), "missing"); err == nil || !strings.Contains(err.Error(), `workspace "missing" does not exist`) {
		t.Fatalf("missing workspace error = %v", err)
	}

	memoryStore := newMemStore()
	storeWorkspace(t, memoryStore, makeWorkspace("", "tenant-a"))
	if err := ValidateWorkspaceBoundary(ctx, memoryStore, "tenant-a"); err != nil {
		t.Fatalf("validate existing workspace: %v", err)
	}
	getErr := errors.New("lookup unavailable")
	if err := ValidateWorkspaceBoundary(ctx, &workspaceFaultStore{Store: memoryStore, getErr: getErr}, "tenant-a"); !errors.Is(err, getErr) {
		t.Fatalf("lookup error = %v, want original store error", err)
	}
}

func TestValidateCrossWorkspaceAccessContracts(t *testing.T) {
	ctx := context.Background()
	listErr := errors.New("unexpected list")
	if err := ValidateCrossWorkspaceAccess(ctx, &workspaceFaultStore{Store: newMemStore(), listErr: listErr}, "tenant-a", "tenant-a", "Tool", "search"); err != nil {
		t.Fatalf("same-workspace access: %v", err)
	}

	memoryStore := newMemStore()
	if err := memoryStore.Create(ctx, "CrossNamespaceGrant", "tenant-b", "broken", []byte(`{"spec":`)); err != nil {
		t.Fatalf("store malformed grant: %v", err)
	}
	storeCrossNamespaceGrant(t, memoryStore, makeCrossNamespaceGrant("tenant-b", "allow-search", "tenant-b", "tenant-a", "Tool", "search"))
	if err := ValidateCrossWorkspaceAccess(ctx, memoryStore, "tenant-a", "tenant-b", "Tool", "search"); err != nil {
		t.Fatalf("explicitly granted access: %v", err)
	}
	if err := ValidateCrossWorkspaceAccess(ctx, memoryStore, "tenant-c", "tenant-b", "Tool", "search"); err == nil || !strings.Contains(err.Error(), "cross-workspace access denied") {
		t.Fatalf("ungranted access error = %v", err)
	}

	wildcardStore := newMemStore()
	storeCrossNamespaceGrant(t, wildcardStore, makeCrossNamespaceGrant("tenant-b", "allow-all", "tenant-b", "*", "Tool", "*"))
	if err := ValidateCrossWorkspaceAccess(ctx, wildcardStore, "tenant-a", "tenant-b", "Tool", "calculator"); err != nil {
		t.Fatalf("wildcard-granted access: %v", err)
	}
	listErr = errors.New("grant list unavailable")
	err := ValidateCrossWorkspaceAccess(ctx, &workspaceFaultStore{Store: memoryStore, listErr: listErr}, "tenant-a", "tenant-b", "Tool", "search")
	if !errors.Is(err, listErr) || !strings.Contains(err.Error(), "list cross-namespace grants") {
		t.Fatalf("list error = %v, want wrapped store error", err)
	}
}

func readyWorkspaceStore(t *testing.T, name string, withPolicy bool) *memStore {
	t.Helper()
	memoryStore := newMemStore()
	workspace := makeWorkspace("", name)
	workspace.ObjectMeta.Finalizers = []string{WorkspaceFinalizer}
	workspace.Status.Phase = v1alpha1.PhaseReady
	storeWorkspace(t, memoryStore, workspace)
	if withPolicy {
		storeCommPolicy(t, memoryStore, makeCommPolicy(name, name+"-default-deny", v1alpha1.AgentCommunicationPolicySpec{DefaultDeny: true}))
	}
	return memoryStore
}

func getStoredWorkspace(t *testing.T, memoryStore *memStore, name string) *v1alpha1.Workspace {
	t.Helper()
	raw, err := memoryStore.Get(context.Background(), workspaceKind, "", name)
	if err != nil {
		t.Fatalf("get Workspace: %v", err)
	}
	var workspace v1alpha1.Workspace
	if err := json.Unmarshal(raw, &workspace); err != nil {
		t.Fatalf("unmarshal Workspace: %v", err)
	}
	return &workspace
}
