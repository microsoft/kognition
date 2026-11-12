// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/store"
)

type recordingMemoryProvisioner struct {
	provisionRequests   []memory.ProvisionRequest
	deprovisionRequests []memory.DeprovisionRequest
	provisionErr        error
	deprovisionErr      error
}

func (p *recordingMemoryProvisioner) Provision(_ context.Context, req memory.ProvisionRequest) error {
	p.provisionRequests = append(p.provisionRequests, req)
	return p.provisionErr
}

func (p *recordingMemoryProvisioner) Deprovision(_ context.Context, req memory.DeprovisionRequest) error {
	p.deprovisionRequests = append(p.deprovisionRequests, req)
	return p.deprovisionErr
}

type concurrentMemoryClaimBindStore struct {
	store.Store
	conflicted    bool
	volumeCreates int
}

type persistentMemoryClaimBindConflictStore struct {
	store.Store
	conflicts     bool
	claimUpdates  int
	volumeCreates int
}

type memoryClassLookupFailureStore struct {
	store.Store
	err error
}

type memoryVolumeLookupFailureStore struct {
	store.Store
	err error
}

type memoryVolumeDeleteFailureStore struct {
	store.Store
	err error
}

type memoryVolumeUpdateFailureStore struct {
	store.Store
	err error
}

func (s *memoryVolumeUpdateFailureStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == memoryVolumeKind {
		return s.err
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *memoryClassLookupFailureStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == memoryClassKind {
		return nil, s.err
	}
	return s.Store.Get(ctx, kind, namespace, name)
}

func (s *memoryVolumeLookupFailureStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == memoryVolumeKind {
		return nil, s.err
	}
	return s.Store.Get(ctx, kind, namespace, name)
}

func (s *memoryVolumeDeleteFailureStore) Delete(ctx context.Context, kind, namespace, name string) error {
	if kind == memoryVolumeKind {
		return s.err
	}
	return s.Store.Delete(ctx, kind, namespace, name)
}

func (s *persistentMemoryClaimBindConflictStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if kind == memoryVolumeKind {
		s.volumeCreates++
	}
	return s.Store.Create(ctx, kind, namespace, name, data)
}

func (s *persistentMemoryClaimBindConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == memoryClaimKind {
		var desired v1alpha1.MemoryClaim
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if s.conflicts && desired.Status.Phase == v1alpha1.PhaseBound {
			s.claimUpdates++
			return fmt.Errorf("injected persistent conflict: %w", store.ErrConflict)
		}
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *concurrentMemoryClaimBindStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if kind == memoryVolumeKind {
		s.volumeCreates++
	}
	return s.Store.Create(ctx, kind, namespace, name, data)
}

func (s *concurrentMemoryClaimBindStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == memoryClaimKind && !s.conflicted {
		var desired v1alpha1.MemoryClaim
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase == v1alpha1.PhaseBound {
			s.conflicted = true
			raw, err := s.Store.Get(ctx, kind, namespace, name)
			if err != nil {
				return err
			}
			var latest v1alpha1.MemoryClaim
			if err := json.Unmarshal(raw, &latest); err != nil {
				return err
			}
			latest.Spec.ConsistencyMode = "strong"
			latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{
				Type: "Audited", Status: string(lifecycle.ConditionTrue), Reason: "ConcurrentAudit",
			})
			updated, err := json.Marshal(latest)
			if err != nil {
				return err
			}
			if err := s.Store.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
				return err
			}
			return fmt.Errorf("injected concurrent conflict: %w", store.ErrConflict)
		}
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func makeMemoryClass(name string, policy v1alpha1.ReclaimPolicy) v1alpha1.MemoryClass {
	return v1alpha1.MemoryClass{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "MemoryClass"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name: name,
			UID:  "mc-uid-" + name,
		},
		Spec: v1alpha1.MemoryClassSpec{
			BackendType:   "pgvector",
			ReclaimPolicy: policy,
		},
	}
}

func makeMemoryClaim(ns, name, className string, memType v1alpha1.MemoryType) v1alpha1.MemoryClaim {
	return v1alpha1.MemoryClaim{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "MemoryClaim"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "claim-uid-" + name,
		},
		Spec: v1alpha1.MemoryClaimSpec{
			MemoryClassName: className,
			Type:            memType,
			Capacity:        "100Mi",
		},
	}
}

func makeMemory(ns, name, claimName string, memType v1alpha1.MemoryType) v1alpha1.Memory {
	return v1alpha1.Memory{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Memory"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "mem-uid-" + name,
		},
		Spec: v1alpha1.MemorySpec{
			ClaimRef: v1alpha1.LocalObjectReference{Name: claimName},
			Type:     memType,
		},
	}
}

func storeMemoryClass(t *testing.T, s *memStore, mc v1alpha1.MemoryClass) {
	t.Helper()
	data, _ := json.Marshal(mc)
	if err := s.Create(context.Background(), "MemoryClass", "", mc.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store memory class: %v", err)
	}
}

func storeMemoryClaim(t *testing.T, s *memStore, claim v1alpha1.MemoryClaim) {
	t.Helper()
	data, _ := json.Marshal(claim)
	if err := s.Create(context.Background(), "MemoryClaim", claim.ObjectMeta.Namespace, claim.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store memory claim: %v", err)
	}
}

func getMemoryClaim(t *testing.T, s *memStore, ns, name string) v1alpha1.MemoryClaim {
	t.Helper()
	raw, err := s.Get(context.Background(), "MemoryClaim", ns, name)
	if err != nil {
		t.Fatalf("get memory claim: %v", err)
	}
	var claim v1alpha1.MemoryClaim
	if err := json.Unmarshal(raw, &claim); err != nil {
		t.Fatalf("unmarshal memory claim: %v", err)
	}
	return claim
}

func listMemoryVolumes(t *testing.T, s *memStore, ns string) []v1alpha1.MemoryVolume {
	t.Helper()
	rawList, err := s.List(context.Background(), "MemoryVolume", store.ListOptions{Namespace: ns})
	if err != nil {
		t.Fatalf("list memory volumes: %v", err)
	}
	var vols []v1alpha1.MemoryVolume
	for _, raw := range rawList {
		var vol v1alpha1.MemoryVolume
		if err := json.Unmarshal(raw, &vol); err != nil {
			t.Fatalf("unmarshal memory volume: %v", err)
		}
		vols = append(vols, vol)
	}
	return vols
}

func getMemoryVolumeByName(t *testing.T, s *memStore, ns, name string) v1alpha1.MemoryVolume {
	t.Helper()
	raw, err := s.Get(context.Background(), "MemoryVolume", ns, name)
	if err != nil {
		t.Fatalf("get memory volume %s: %v", name, err)
	}
	var vol v1alpha1.MemoryVolume
	if err := json.Unmarshal(raw, &vol); err != nil {
		t.Fatalf("unmarshal memory volume: %v", err)
	}
	return vol
}

func reconcileClaim(t *testing.T, r *MemoryClaimReconciler, ns, name string) {
	t.Helper()
	ctx := context.Background()
	req := Request{Namespace: ns, Name: name, Kind: "MemoryClaim"}
	// Up to 5 reconcile loops to reach a stable state (finalizer + provision + bound).
	for i := 0; i < 5; i++ {
		res, err := r.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("reconcile loop %d: %v", i+1, err)
		}
		if !res.Requeue {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// TC-0035: MemoryClass(pgvector) + MemoryClaim(episodic) → provision + bind
// ---------------------------------------------------------------------------

func TestMemoryClaimReconciler_TC0035_ProvisionAndBind(t *testing.T) {
	s := newMemStore()
	provisioner := &recordingMemoryProvisioner{}
	r := NewMemoryClaimReconciler(s, zaptest.NewLogger(t), WithMemoryBackendProvisioner(provisioner))

	class := makeMemoryClass("fast-pgvector", v1alpha1.ReclaimDelete)
	class.Spec.Parameters = map[string]string{"embeddingDimension": "1536", "index": "hnsw"}
	storeMemoryClass(t, s, class)
	storeMemoryClaim(t, s, makeMemoryClaim("default", "claim-episodic", "fast-pgvector", v1alpha1.MemoryTypeEpisodic))

	reconcileClaim(t, r, "default", "claim-episodic")

	claim := getMemoryClaim(t, s, "default", "claim-episodic")
	if claim.Status.Phase != v1alpha1.PhaseBound {
		t.Fatalf("claim phase = %q, want Bound", claim.Status.Phase)
	}
	if claim.Status.BoundVolumeName == "" {
		t.Fatal("claim BoundVolumeName should not be empty")
	}

	vols := listMemoryVolumes(t, s, "default")
	if len(vols) != 1 {
		t.Fatalf("expected 1 MemoryVolume, got %d", len(vols))
	}
	vol := vols[0]
	if vol.Status.Phase != v1alpha1.PhaseBound {
		t.Fatalf("volume phase = %q, want Bound", vol.Status.Phase)
	}
	if vol.Spec.MemoryClassName != "fast-pgvector" {
		t.Fatalf("volume MemoryClassName = %q, want fast-pgvector", vol.Spec.MemoryClassName)
	}
	if vol.Spec.ClaimRef == nil || vol.Spec.ClaimRef.Name != "claim-episodic" {
		t.Fatalf("volume ClaimRef = %+v, want claim-episodic", vol.Spec.ClaimRef)
	}
	if vol.Spec.BackendHandle == "" {
		t.Fatal("volume BackendHandle should not be empty")
	}
	if len(provisioner.provisionRequests) != 1 {
		t.Fatalf("backend provision calls = %d, want 1", len(provisioner.provisionRequests))
	}
	request := provisioner.provisionRequests[0]
	if request.BackendHandle != vol.Spec.BackendHandle || request.EmbeddingDimension != 1536 || request.Parameters["index"] != "hnsw" {
		t.Fatalf("backend provision request = %+v, want volume handle and class parameters", request)
	}
}

func TestMemoryClaimReconciler_ProvisionFailureRetriesPersistedHandleBeforeBinding(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	provisionErr := errors.New("memory service unavailable")
	provisioner := &recordingMemoryProvisioner{provisionErr: provisionErr}
	r := NewMemoryClaimReconciler(s, zaptest.NewLogger(t), WithMemoryBackendProvisioner(provisioner))
	class := makeMemoryClass("retry-class", v1alpha1.ReclaimDelete)
	claim := makeMemoryClaim("default", "claim-retry", class.ObjectMeta.Name, v1alpha1.MemoryTypeWorking)
	claim.ObjectMeta.Finalizers = []string{MemoryClaimFinalizer}
	claim.Status.Phase = v1alpha1.PhasePending
	storeMemoryClass(t, s, class)
	storeMemoryClaim(t, s, claim)

	request := Request{Namespace: claim.ObjectMeta.Namespace, Name: claim.ObjectMeta.Name, Kind: memoryClaimKind}
	if _, err := r.Reconcile(ctx, request); !errors.Is(err, provisionErr) {
		t.Fatalf("first reconcile error = %v, want wrapped provision error", err)
	}
	storedClaim := getMemoryClaim(t, s, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name)
	if storedClaim.Status.Phase != v1alpha1.PhasePending || storedClaim.Status.BoundVolumeName != "" {
		t.Fatalf("claim after failed provision = %+v, want Pending without binding", storedClaim.Status)
	}
	volumes := listMemoryVolumes(t, s, claim.ObjectMeta.Namespace)
	if len(volumes) != 1 || volumes[0].Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("volumes after failed provision = %+v, want one Pending volume", volumes)
	}
	firstHandle := volumes[0].Spec.BackendHandle

	provisioner.provisionErr = nil
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("retry reconcile: %v", err)
	}
	storedClaim = getMemoryClaim(t, s, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name)
	if storedClaim.Status.Phase != v1alpha1.PhaseBound {
		t.Fatalf("claim phase after retry = %q, want Bound", storedClaim.Status.Phase)
	}
	volumes = listMemoryVolumes(t, s, claim.ObjectMeta.Namespace)
	if len(volumes) != 1 || volumes[0].Spec.BackendHandle != firstHandle || volumes[0].Status.Phase != v1alpha1.PhaseBound {
		t.Fatalf("volume after retry = %+v, want same handle in Bound phase", volumes)
	}
	if len(provisioner.provisionRequests) != 2 || provisioner.provisionRequests[1].BackendHandle != firstHandle {
		t.Fatalf("provision retry requests = %+v, want same persisted handle", provisioner.provisionRequests)
	}
}

func TestMemoryClaimReconciler_BoundWithoutVolumeResetsReadiness(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewMemoryClaimReconciler(s, zaptest.NewLogger(t))
	claim := makeMemoryClaim("default", "missing-binding", "memory-class", v1alpha1.MemoryTypeEpisodic)
	claim.ObjectMeta.Finalizers = []string{MemoryClaimFinalizer}
	claim.Status.Phase = v1alpha1.PhaseBound
	claim.Status.Conditions = []v1alpha1.Condition{{
		Type:   string(lifecycle.ConditionReady),
		Status: string(lifecycle.ConditionTrue),
		Reason: "Bound",
	}}
	storeMemoryClaim(t, s, claim)

	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: claim.ObjectMeta.Name, Kind: memoryClaimKind})
	if err != nil {
		t.Fatalf("reconcile malformed bound claim: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected immediate requeue after resetting malformed bound claim")
	}
	updated := getMemoryClaim(t, s, "default", claim.ObjectMeta.Name)
	if updated.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("phase = %q, want Pending", updated.Status.Phase)
	}
	condition := findCondition(updated.Status.Conditions, string(lifecycle.ConditionReady))
	if condition == nil || condition.Status != string(lifecycle.ConditionFalse) || condition.Reason != "VolumeMissing" {
		t.Fatalf("Ready condition = %#v, want False/VolumeMissing", condition)
	}
	if !hasFinalizer(updated.ObjectMeta.Finalizers, MemoryClaimFinalizer) {
		t.Fatal("MemoryClaim finalizer was removed while repairing binding")
	}
}

func TestMemoryClaimReconciler_MissingBoundVolumeIsReprovisioned(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewMemoryClaimReconciler(s, zaptest.NewLogger(t))
	storeMemoryClass(t, s, makeMemoryClass("memory-class", v1alpha1.ReclaimDelete))
	claim := makeMemoryClaim("default", "replacement", "memory-class", v1alpha1.MemoryTypeEpisodic)
	claim.ObjectMeta.Finalizers = []string{MemoryClaimFinalizer}
	claim.Status.Phase = v1alpha1.PhaseBound
	claim.Status.BoundVolumeName = "deleted-volume"
	storeMemoryClaim(t, s, claim)

	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: claim.ObjectMeta.Name, Kind: memoryClaimKind})
	if err != nil {
		t.Fatalf("reconcile missing bound volume: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected immediate requeue after detecting missing volume")
	}
	updated := getMemoryClaim(t, s, "default", claim.ObjectMeta.Name)
	if updated.Status.Phase != v1alpha1.PhasePending || updated.Status.BoundVolumeName != "" {
		t.Fatalf("claim status = %#v, want Pending with cleared binding", updated.Status)
	}
	condition := findCondition(updated.Status.Conditions, string(lifecycle.ConditionReady))
	if condition == nil || condition.Status != string(lifecycle.ConditionFalse) || condition.Reason != "VolumeMissing" {
		t.Fatalf("Ready condition = %#v, want False/VolumeMissing", condition)
	}

	result, err = r.Reconcile(ctx, Request{Namespace: "default", Name: claim.ObjectMeta.Name, Kind: memoryClaimKind})
	if err != nil {
		t.Fatalf("reconcile replacement volume: %v", err)
	}
	if result.Requeue {
		t.Fatal("unexpected requeue after replacement volume was provisioned")
	}
	updated = getMemoryClaim(t, s, "default", claim.ObjectMeta.Name)
	if updated.Status.Phase != v1alpha1.PhaseBound || updated.Status.BoundVolumeName != "default-replacement" {
		t.Fatalf("claim status = %#v, want Bound to replacement volume", updated.Status)
	}
	volume := getMemoryVolumeByName(t, s, "default", updated.Status.BoundVolumeName)
	if volume.Status.Phase != v1alpha1.PhaseBound || volume.Spec.ClaimRef == nil || volume.Spec.ClaimRef.Name != claim.ObjectMeta.Name {
		t.Fatalf("replacement volume = %#v, want Bound to claim", volume)
	}
}

func TestMemoryClaimReconciler_BoundVolumeLookupFailurePreservesBinding(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	lookupErr := errors.New("volume backend unavailable")
	r := NewMemoryClaimReconciler(&memoryVolumeLookupFailureStore{Store: baseStore, err: lookupErr}, zaptest.NewLogger(t))
	claim := makeMemoryClaim("default", "bound-claim", "memory-class", v1alpha1.MemoryTypeEpisodic)
	claim.ObjectMeta.Finalizers = []string{MemoryClaimFinalizer}
	claim.Status.Phase = v1alpha1.PhaseBound
	claim.Status.BoundVolumeName = "bound-volume"
	storeMemoryClaim(t, baseStore, claim)

	result, err := r.Reconcile(ctx, Request{Namespace: claim.ObjectMeta.Namespace, Name: claim.ObjectMeta.Name, Kind: memoryClaimKind})
	if !errors.Is(err, lookupErr) {
		t.Fatalf("Reconcile() error = %v, want volume lookup error", err)
	}
	if result != (Result{}) {
		t.Fatalf("Reconcile() result = %#v, want zero result", result)
	}
	stored := getMemoryClaim(t, baseStore, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseBound || stored.Status.BoundVolumeName != claim.Status.BoundVolumeName {
		t.Fatalf("claim binding = %#v, want original Bound state preserved", stored.Status)
	}
}

func TestMemoryClaimReconciler_BindConflictPreservesProvisionAndConcurrentState(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	conflictStore := &concurrentMemoryClaimBindStore{Store: baseStore}
	r := NewMemoryClaimReconciler(conflictStore, zaptest.NewLogger(t))
	storeMemoryClass(t, baseStore, makeMemoryClass("conflict-class", v1alpha1.ReclaimDelete))
	claim := makeMemoryClaim("default", "conflict-claim", "conflict-class", v1alpha1.MemoryTypeEpisodic)
	claim.ObjectMeta.Finalizers = []string{MemoryClaimFinalizer}
	claim.Status.Phase = v1alpha1.PhasePending
	storeMemoryClaim(t, baseStore, claim)

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: claim.ObjectMeta.Name, Kind: memoryClaimKind}); err != nil {
		t.Fatalf("provision after bind conflict: %v", err)
	}
	if !conflictStore.conflicted {
		t.Fatal("expected bind conflict fixture to execute")
	}
	if conflictStore.volumeCreates != 1 {
		t.Fatalf("MemoryVolume creates = %d, want exactly 1", conflictStore.volumeCreates)
	}

	stored := getMemoryClaim(t, baseStore, "default", claim.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseBound || stored.Status.BoundVolumeName != "default-conflict-claim" {
		t.Fatalf("claim binding was not persisted: %+v", stored.Status)
	}
	if stored.Spec.ConsistencyMode != "strong" {
		t.Fatalf("concurrent spec change was lost: %+v", stored.Spec)
	}
	condition := lifecycle.GetCondition(apiToLifecycleConditions(stored.Status.Conditions), lifecycle.ConditionType("Audited"))
	if condition == nil || condition.Reason != "ConcurrentAudit" {
		t.Fatalf("Audited condition = %#v, want concurrent condition preserved", condition)
	}
}

func TestMemoryClaimReconciler_BindConflictRetriesAreBounded(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	conflictStore := &persistentMemoryClaimBindConflictStore{Store: baseStore, conflicts: true}
	r := NewMemoryClaimReconciler(conflictStore, zaptest.NewLogger(t))
	storeMemoryClass(t, baseStore, makeMemoryClass("persistent-conflict-class", v1alpha1.ReclaimDelete))
	claim := makeMemoryClaim("default", "persistent-conflict-claim", "persistent-conflict-class", v1alpha1.MemoryTypeEpisodic)
	claim.ObjectMeta.Finalizers = []string{MemoryClaimFinalizer}
	claim.Status.Phase = v1alpha1.PhasePending
	storeMemoryClaim(t, baseStore, claim)

	_, err := r.Reconcile(ctx, Request{Namespace: "default", Name: claim.ObjectMeta.Name, Kind: memoryClaimKind})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Reconcile() error = %v, want ErrConflict", err)
	}
	if conflictStore.claimUpdates != 5 {
		t.Fatalf("claim update attempts = %d, want 5", conflictStore.claimUpdates)
	}
	if conflictStore.volumeCreates != 1 {
		t.Fatalf("MemoryVolume creates = %d, want exactly 1", conflictStore.volumeCreates)
	}

	conflictStore.conflicts = false
	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: claim.ObjectMeta.Name, Kind: memoryClaimKind})
	if err != nil {
		t.Fatalf("Reconcile() after conflict cleared: %v", err)
	}
	if result.Requeue {
		t.Fatal("unexpected requeue after adopting provisioned volume")
	}
	stored := getMemoryClaim(t, baseStore, "default", claim.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseBound || stored.Status.BoundVolumeName != "default-persistent-conflict-claim" {
		t.Fatalf("claim binding after retry = %#v, want Bound to provisioned volume", stored.Status)
	}
}

func TestMemoryClaimReconciler_ExistingVolumeMustBelongToClaim(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewMemoryClaimReconciler(s, zaptest.NewLogger(t))
	storeMemoryClass(t, s, makeMemoryClass("collision-class", v1alpha1.ReclaimDelete))
	claim := makeMemoryClaim("default", "collision-claim", "collision-class", v1alpha1.MemoryTypeEpisodic)
	claim.ObjectMeta.Finalizers = []string{MemoryClaimFinalizer}
	claim.Status.Phase = v1alpha1.PhasePending
	storeMemoryClaim(t, s, claim)

	volume := v1alpha1.MemoryVolume{
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "default-collision-claim",
			Namespace: "default",
			OwnerReferences: []v1alpha1.OwnerReference{{
				Kind: memoryClaimKind, Name: "other-claim", UID: "other-claim-uid",
			}},
		},
		Spec: v1alpha1.MemoryVolumeSpec{
			ClaimRef: &v1alpha1.LocalObjectReference{Name: "collision-claim"},
		},
	}
	volumeData, err := json.Marshal(volume)
	if err != nil {
		t.Fatalf("marshal colliding volume: %v", err)
	}
	if err := s.Create(ctx, memoryVolumeKind, "default", volume.ObjectMeta.Name, volumeData); err != nil {
		t.Fatalf("store colliding volume: %v", err)
	}

	_, err = r.Reconcile(ctx, Request{Namespace: "default", Name: claim.ObjectMeta.Name, Kind: memoryClaimKind})
	wantErr := `memory volume "default-collision-claim" already exists but is not owned by claim`
	if err == nil || err.Error() != wantErr {
		t.Fatalf("Reconcile() error = %v, want %q", err, wantErr)
	}
	stored := getMemoryClaim(t, s, "default", claim.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhasePending || stored.Status.BoundVolumeName != "" {
		t.Fatalf("claim adopted colliding volume: %#v", stored.Status)
	}
}

// ---------------------------------------------------------------------------
// TC-0036: Memory API type CRUD — one Memory per MemoryType
// ---------------------------------------------------------------------------

func TestMemory_TC0036_CRUDAllTypes(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()

	allTypes := []v1alpha1.MemoryType{
		v1alpha1.MemoryTypeEpisodic,
		v1alpha1.MemoryTypeSemantic,
		v1alpha1.MemoryTypeWorking,
		v1alpha1.MemoryTypeProcedural,
		v1alpha1.MemoryTypeShared,
	}

	for _, memType := range allTypes {
		name := "mem-" + string(memType)
		mem := makeMemory("default", name, "claim-"+string(memType), memType)
		data, _ := json.Marshal(mem)
		if err := s.Create(ctx, "Memory", "default", name, data); err != nil {
			t.Fatalf("create memory %s: %v", name, err)
		}
	}

	for _, memType := range allTypes {
		name := "mem-" + string(memType)
		raw, err := s.Get(ctx, "Memory", "default", name)
		if err != nil {
			t.Fatalf("get memory %s: %v", name, err)
		}
		var got v1alpha1.Memory
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal memory %s: %v", name, err)
		}
		if got.Spec.Type != memType {
			t.Errorf("memory %s type = %q, want %q", name, got.Spec.Type, memType)
		}
		if got.Spec.ClaimRef.Name != "claim-"+string(memType) {
			t.Errorf("memory %s claimRef = %q, want claim-%s", name, got.Spec.ClaimRef.Name, memType)
		}
		if got.TypeMeta.Kind != "Memory" {
			t.Errorf("memory %s kind = %q, want Memory", name, got.TypeMeta.Kind)
		}
	}
}

// ---------------------------------------------------------------------------
// TC-0037: ReclaimPolicy=Delete → volume removed on claim deletion
// ---------------------------------------------------------------------------

func TestMemoryClaimReconciler_TC0037_ReclaimDelete(t *testing.T) {
	s := newMemStore()
	provisioner := &recordingMemoryProvisioner{}
	r := NewMemoryClaimReconciler(s, zaptest.NewLogger(t), WithMemoryBackendProvisioner(provisioner))

	storeMemoryClass(t, s, makeMemoryClass("delete-class", v1alpha1.ReclaimDelete))
	storeMemoryClaim(t, s, makeMemoryClaim("default", "claim-delete", "delete-class", v1alpha1.MemoryTypeWorking))

	// Provision the claim.
	reconcileClaim(t, r, "default", "claim-delete")

	claim := getMemoryClaim(t, s, "default", "claim-delete")
	if claim.Status.Phase != v1alpha1.PhaseBound {
		t.Fatalf("claim phase = %q, want Bound before deletion test", claim.Status.Phase)
	}
	volName := claim.Status.BoundVolumeName

	// Trigger deletion by setting DeletionTimestamp.
	now := time.Now()
	claim.ObjectMeta.DeletionTimestamp = &now
	data, _ := json.Marshal(claim)
	if err := s.Update(context.Background(), "MemoryClaim", "default", "claim-delete", data, claim.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark claim for deletion: %v", err)
	}

	// Reconcile to handle deletion.
	ctx := context.Background()
	req := Request{Namespace: "default", Name: "claim-delete", Kind: "MemoryClaim"}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}

	// Volume must be deleted.
	if _, err := s.Get(ctx, "MemoryVolume", "default", volName); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected MemoryVolume to be deleted, got err = %v", err)
	}
	if len(provisioner.deprovisionRequests) != 1 || provisioner.deprovisionRequests[0].BackendHandle == "" {
		t.Fatalf("backend deprovision requests = %+v, want bound volume handle", provisioner.deprovisionRequests)
	}
}

func TestMemoryClaimReconciler_DeprovisionFailureKeepsVolumeAndFinalizer(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	deprovisionErr := errors.New("backend cleanup unavailable")
	provisioner := &recordingMemoryProvisioner{}
	r := NewMemoryClaimReconciler(s, zaptest.NewLogger(t), WithMemoryBackendProvisioner(provisioner))
	class := makeMemoryClass("delete-class", v1alpha1.ReclaimDelete)
	claim := makeMemoryClaim("default", "claim-cleanup-retry", class.ObjectMeta.Name, v1alpha1.MemoryTypeWorking)
	storeMemoryClass(t, s, class)
	storeMemoryClaim(t, s, claim)
	reconcileClaim(t, r, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name)

	claim = getMemoryClaim(t, s, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name)
	volumeName := claim.Status.BoundVolumeName
	now := time.Now().UTC()
	claim.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("marshal deleting claim: %v", err)
	}
	if err := s.Update(ctx, memoryClaimKind, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name, data, claim.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark claim for deletion: %v", err)
	}
	provisioner.deprovisionErr = deprovisionErr

	if _, err := r.Reconcile(ctx, Request{Namespace: claim.ObjectMeta.Namespace, Name: claim.ObjectMeta.Name, Kind: memoryClaimKind}); !errors.Is(err, deprovisionErr) {
		t.Fatalf("deletion reconcile error = %v, want wrapped deprovision error", err)
	}
	if _, err := s.Get(ctx, memoryVolumeKind, claim.ObjectMeta.Namespace, volumeName); err != nil {
		t.Fatalf("MemoryVolume removed after failed backend cleanup: %v", err)
	}
	storedClaim := getMemoryClaim(t, s, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name)
	if !hasString(storedClaim.ObjectMeta.Finalizers, MemoryClaimFinalizer) || storedClaim.Status.Phase != v1alpha1.PhaseBound {
		t.Fatalf("claim changed after failed backend cleanup: %+v", storedClaim)
	}
}

func TestMemoryClaimReconciler_DeletionDefaultsToDeleteWhenClassIsMissing(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	r := NewMemoryClaimReconciler(s, zaptest.NewLogger(t))
	class := makeMemoryClass("removed-class", v1alpha1.ReclaimRetain)
	claim := makeMemoryClaim("default", "claim-missing-class", class.ObjectMeta.Name, v1alpha1.MemoryTypeWorking)
	claim.ObjectMeta.Finalizers = []string{"example.com/foreign"}
	storeMemoryClass(t, s, class)
	storeMemoryClaim(t, s, claim)
	reconcileClaim(t, r, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name)

	claim = getMemoryClaim(t, s, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name)
	volumeName := claim.Status.BoundVolumeName
	if err := s.Delete(ctx, memoryClassKind, "", class.ObjectMeta.Name); err != nil {
		t.Fatalf("delete MemoryClass: %v", err)
	}
	now := time.Now().UTC()
	claim.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("marshal deleting claim: %v", err)
	}
	if err := s.Update(ctx, memoryClaimKind, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name, data, claim.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark claim for deletion: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: claim.ObjectMeta.Namespace, Name: claim.ObjectMeta.Name, Kind: memoryClaimKind}); err != nil {
		t.Fatalf("reconcile deletion without MemoryClass: %v", err)
	}
	if _, err := s.Get(ctx, memoryVolumeKind, claim.ObjectMeta.Namespace, volumeName); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("MemoryVolume lookup error = %v, want ErrNotFound", err)
	}
	stored := getMemoryClaim(t, s, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseReleased {
		t.Fatalf("claim phase = %q, want Released", stored.Status.Phase)
	}
	if hasString(stored.ObjectMeta.Finalizers, MemoryClaimFinalizer) {
		t.Fatal("MemoryClaim cleanup finalizer was not removed")
	}
	if !hasString(stored.ObjectMeta.Finalizers, "example.com/foreign") {
		t.Fatal("foreign finalizer was removed")
	}
}

func TestMemoryClaimReconciler_DeletionStopsWhenVolumeDeleteFails(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	class := makeMemoryClass("delete-class", v1alpha1.ReclaimDelete)
	claim := makeMemoryClaim("default", "claim-delete-error", class.ObjectMeta.Name, v1alpha1.MemoryTypeWorking)
	storeMemoryClass(t, baseStore, class)
	storeMemoryClaim(t, baseStore, claim)
	reconcileClaim(t, NewMemoryClaimReconciler(baseStore, zaptest.NewLogger(t)), claim.ObjectMeta.Namespace, claim.ObjectMeta.Name)

	claim = getMemoryClaim(t, baseStore, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name)
	volumeName := claim.Status.BoundVolumeName
	now := time.Now().UTC()
	claim.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("marshal deleting claim: %v", err)
	}
	if err := baseStore.Update(ctx, memoryClaimKind, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name, data, claim.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark claim for deletion: %v", err)
	}

	deleteErr := errors.New("memory volume delete unavailable")
	r := NewMemoryClaimReconciler(&memoryVolumeDeleteFailureStore{Store: baseStore, err: deleteErr}, zaptest.NewLogger(t))
	if _, err := r.Reconcile(ctx, Request{Namespace: claim.ObjectMeta.Namespace, Name: claim.ObjectMeta.Name, Kind: memoryClaimKind}); !errors.Is(err, deleteErr) {
		t.Fatalf("Reconcile() error = %v, want wrapped delete error", err)
	}
	if _, err := baseStore.Get(ctx, memoryVolumeKind, claim.ObjectMeta.Namespace, volumeName); err != nil {
		t.Fatalf("MemoryVolume changed after failed deletion: %v", err)
	}
	stored := getMemoryClaim(t, baseStore, claim.ObjectMeta.Namespace, claim.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseBound {
		t.Fatalf("claim phase = %q, want Bound", stored.Status.Phase)
	}
	if !hasString(stored.ObjectMeta.Finalizers, MemoryClaimFinalizer) {
		t.Fatal("MemoryClaim finalizer was removed after failed volume deletion")
	}
}

func TestMemoryClaimReconciler_DeletionStopsWhenReclaimPolicyLookupFails(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	storeMemoryClass(t, baseStore, makeMemoryClass("retain-class", v1alpha1.ReclaimRetain))
	storeMemoryClaim(t, baseStore, makeMemoryClaim("default", "claim-policy-error", "retain-class", v1alpha1.MemoryTypeWorking))
	reconcileClaim(t, NewMemoryClaimReconciler(baseStore, zaptest.NewLogger(t)), "default", "claim-policy-error")

	claim := getMemoryClaim(t, baseStore, "default", "claim-policy-error")
	volumeName := claim.Status.BoundVolumeName
	now := time.Now().UTC()
	claim.ObjectMeta.DeletionTimestamp = &now
	data, _ := json.Marshal(claim)
	if err := baseStore.Update(ctx, memoryClaimKind, "default", claim.ObjectMeta.Name, data, claim.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark claim for deletion: %v", err)
	}

	lookupErr := errors.New("memory class store unavailable")
	r := NewMemoryClaimReconciler(&memoryClassLookupFailureStore{Store: baseStore, err: lookupErr}, zaptest.NewLogger(t))
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: claim.ObjectMeta.Name, Kind: memoryClaimKind}); !errors.Is(err, lookupErr) {
		t.Fatalf("reconcile error = %v, want wrapped lookup error", err)
	}
	if _, err := baseStore.Get(ctx, memoryVolumeKind, "default", volumeName); err != nil {
		t.Fatalf("retained volume was changed after policy lookup failure: %v", err)
	}
	updated := getMemoryClaim(t, baseStore, "default", claim.ObjectMeta.Name)
	if !hasString(updated.ObjectMeta.Finalizers, MemoryClaimFinalizer) {
		t.Fatal("claim finalizer was removed after policy lookup failure")
	}
	if updated.Status.Phase != v1alpha1.PhaseBound {
		t.Fatalf("claim phase = %q, want Bound after policy lookup failure", updated.Status.Phase)
	}
}

func TestMemoryClaimReconciler_DeletionStopsWhenRetainTransitionFails(t *testing.T) {
	ctx := context.Background()
	baseStore := newMemStore()
	storeMemoryClass(t, baseStore, makeMemoryClass("retain-class", v1alpha1.ReclaimRetain))
	storeMemoryClaim(t, baseStore, makeMemoryClaim("default", "claim-retain-error", "retain-class", v1alpha1.MemoryTypeWorking))
	reconcileClaim(t, NewMemoryClaimReconciler(baseStore, zaptest.NewLogger(t)), "default", "claim-retain-error")

	claim := getMemoryClaim(t, baseStore, "default", "claim-retain-error")
	volumeName := claim.Status.BoundVolumeName
	now := time.Now().UTC()
	claim.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("marshal deleting claim: %v", err)
	}
	if err := baseStore.Update(ctx, memoryClaimKind, "default", claim.ObjectMeta.Name, data, claim.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark claim for deletion: %v", err)
	}

	transitionErr := errors.New("memory volume store unavailable")
	r := NewMemoryClaimReconciler(&memoryVolumeUpdateFailureStore{Store: baseStore, err: transitionErr}, zaptest.NewLogger(t))
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: claim.ObjectMeta.Name, Kind: memoryClaimKind}); !errors.Is(err, transitionErr) {
		t.Fatalf("Reconcile() error = %v, want wrapped transition error", err)
	}

	storedClaim := getMemoryClaim(t, baseStore, "default", claim.ObjectMeta.Name)
	if !hasString(storedClaim.ObjectMeta.Finalizers, MemoryClaimFinalizer) {
		t.Fatal("claim finalizer was removed after retained volume transition failure")
	}
	if storedClaim.Status.Phase != v1alpha1.PhaseBound {
		t.Fatalf("claim phase = %q, want Bound after retained volume transition failure", storedClaim.Status.Phase)
	}
	volume := getMemoryVolumeByName(t, baseStore, "default", volumeName)
	if volume.Status.Phase != v1alpha1.PhaseBound || volume.Spec.ClaimRef == nil {
		t.Fatalf("volume changed after failed retain transition: %+v", volume)
	}
}

func TestMemoryClaimReconciler_DeletionStopsForUnsupportedReclaimPolicy(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	class := makeMemoryClass("invalid-class", v1alpha1.ReclaimPolicy("Recycle"))
	storeMemoryClass(t, s, class)
	storeMemoryClaim(t, s, makeMemoryClaim("default", "claim-invalid-policy", class.ObjectMeta.Name, v1alpha1.MemoryTypeWorking))
	r := NewMemoryClaimReconciler(s, zaptest.NewLogger(t))
	reconcileClaim(t, r, "default", "claim-invalid-policy")

	claim := getMemoryClaim(t, s, "default", "claim-invalid-policy")
	volumeName := claim.Status.BoundVolumeName
	now := time.Now().UTC()
	claim.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("marshal deleting claim: %v", err)
	}
	if err := s.Update(ctx, memoryClaimKind, "default", claim.ObjectMeta.Name, data, claim.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark claim for deletion: %v", err)
	}

	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: claim.ObjectMeta.Name, Kind: memoryClaimKind}); err == nil {
		t.Fatal("Reconcile() error = nil, want unsupported reclaim policy error")
	}
	stored := getMemoryClaim(t, s, "default", claim.ObjectMeta.Name)
	if !hasString(stored.ObjectMeta.Finalizers, MemoryClaimFinalizer) {
		t.Fatal("claim finalizer was removed for unsupported reclaim policy")
	}
	if stored.Status.Phase != v1alpha1.PhaseBound {
		t.Fatalf("claim phase = %q, want Bound for unsupported reclaim policy", stored.Status.Phase)
	}
	volume := getMemoryVolumeByName(t, s, "default", volumeName)
	if volume.Status.Phase != v1alpha1.PhaseBound || volume.Spec.ClaimRef == nil {
		t.Fatalf("volume changed for unsupported reclaim policy: %+v", volume)
	}
}

// ---------------------------------------------------------------------------
// TC-0150: Full lifecycle — Pending→Bound→(Released|Retained|Archived) per policy
// ---------------------------------------------------------------------------

func TestMemoryClaimReconciler_TC0150_FullLifecycle(t *testing.T) {
	policies := []struct {
		policy       v1alpha1.ReclaimPolicy
		wantVolPhase v1alpha1.Phase
	}{
		{v1alpha1.ReclaimDelete, ""}, // volume absent
		{v1alpha1.ReclaimRetain, v1alpha1.PhaseRetained},
		{v1alpha1.ReclaimArchive, v1alpha1.PhaseArchived},
	}

	for _, tc := range policies {
		t.Run(string(tc.policy), func(t *testing.T) {
			s := newMemStore()
			r := NewMemoryClaimReconciler(s, zaptest.NewLogger(t))
			ctx := context.Background()

			className := "class-" + string(tc.policy)
			claimName := "claim-" + string(tc.policy)

			storeMemoryClass(t, s, makeMemoryClass(className, tc.policy))
			storeMemoryClaim(t, s, makeMemoryClaim("default", claimName, className, v1alpha1.MemoryTypeEpisodic))

			// Phase 1: Pending
			claim := getMemoryClaim(t, s, "default", claimName)
			if claim.Status.Phase != "" && claim.Status.Phase != v1alpha1.PhasePending {
				t.Fatalf("initial phase = %q, want empty or Pending", claim.Status.Phase)
			}

			// Phase 2: Bound
			reconcileClaim(t, r, "default", claimName)
			claim = getMemoryClaim(t, s, "default", claimName)
			if claim.Status.Phase != v1alpha1.PhaseBound {
				t.Fatalf("after provision phase = %q, want Bound", claim.Status.Phase)
			}
			volName := claim.Status.BoundVolumeName

			// Phase 3: Mark for deletion
			now := time.Now()
			claim.ObjectMeta.DeletionTimestamp = &now
			data, _ := json.Marshal(claim)
			if err := s.Update(ctx, "MemoryClaim", "default", claimName, data, claim.ObjectMeta.ResourceVersion); err != nil {
				t.Fatalf("mark deletion: %v", err)
			}

			req := Request{Namespace: "default", Name: claimName, Kind: "MemoryClaim"}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("reconcile deletion: %v", err)
			}

			claim = getMemoryClaim(t, s, "default", claimName)
			if claim.Status.Phase != v1alpha1.PhaseReleased {
				t.Fatalf("after deletion phase = %q, want Released", claim.Status.Phase)
			}

			// Phase 4: Verify volume disposition
			switch tc.policy {
			case v1alpha1.ReclaimDelete:
				_, err := s.Get(ctx, "MemoryVolume", "default", volName)
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("[Delete] expected volume gone, err = %v", err)
				}

			case v1alpha1.ReclaimRetain:
				vol := getMemoryVolumeByName(t, s, "default", volName)
				if vol.Status.Phase != v1alpha1.PhaseRetained {
					t.Fatalf("[Retain] volume phase = %q, want Retained", vol.Status.Phase)
				}
				if vol.Spec.ClaimRef != nil {
					t.Fatalf("[Retain] volume ClaimRef should be nil after retain, got %+v", vol.Spec.ClaimRef)
				}

			case v1alpha1.ReclaimArchive:
				vol := getMemoryVolumeByName(t, s, "default", volName)
				if vol.Status.Phase != v1alpha1.PhaseArchived {
					t.Fatalf("[Archive] volume phase = %q, want Archived", vol.Status.Phase)
				}
			}
		})
	}
}

// TestMemoryClaimReconciler_MemoryClassNotFoundIdempotent verifies that when a
// MemoryClass is missing, the condition is set once and subsequent reconciles do
// NOT write to the store again (preventing pg_notify loops).
func TestMemoryClaimReconciler_MemoryClassNotFoundIdempotent(t *testing.T) {
	s := newMemStore()
	r := NewMemoryClaimReconciler(s, zaptest.NewLogger(t))

	// Create a claim referencing a non-existent class.
	storeMemoryClaim(t, s, makeMemoryClaim("default", "orphan-claim", "missing-class", v1alpha1.MemoryTypeEpisodic))

	ctx := context.Background()
	req := Request{Namespace: "default", Name: "orphan-claim", Kind: "MemoryClaim"}

	// First reconcile: adds finalizer.
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after finalizer add")
	}

	// Second reconcile: sets phase Pending, requeues.
	res, err = r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if !res.Requeue {
		t.Fatal("expected requeue after setting Pending")
	}

	// Third reconcile: MemoryClass not found → sets condition + RequeueAfter.
	res, err = r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("expected RequeueAfter=30s, got %v", res.RequeueAfter)
	}

	// Capture resourceVersion after the condition was written.
	claim := getMemoryClaim(t, s, "default", "orphan-claim")
	rvAfterCondition := claim.ObjectMeta.ResourceVersion

	// Fourth reconcile: same state, should NOT write to store.
	res, err = r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 4: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("expected RequeueAfter=30s on repeat, got %v", res.RequeueAfter)
	}

	after := getMemoryClaim(t, s, "default", "orphan-claim")
	if after.ObjectMeta.ResourceVersion != rvAfterCondition {
		t.Fatalf("expected no store write on repeated MemoryClassNotFound; RV changed from %d to %d",
			rvAfterCondition, after.ObjectMeta.ResourceVersion)
	}
}
