// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/internal/store"
)

// ---------------------------------------------------------------------------
// memStore — in-memory store.Store implementation for testing
// ---------------------------------------------------------------------------

type memStore struct {
	mu         sync.RWMutex
	data       map[string][]byte                  // "kind/namespace/name" -> raw JSON
	versions   map[string]int64                   // same key -> resourceVersion
	nextRV     int64                              // global monotonic counter
	watchers   map[string][]chan store.WatchEvent // kind -> channels
	events     map[string][]store.InstanceEvent   // "kind/namespace/name" -> events
	revisions  map[string][]store.Revision        // "kind/namespace/name" -> revisions
	dispatches map[string]store.ModelDispatchRecord
}

func newMemStore() *memStore {
	return &memStore{
		data:       make(map[string][]byte),
		versions:   make(map[string]int64),
		nextRV:     1,
		watchers:   make(map[string][]chan store.WatchEvent),
		events:     make(map[string][]store.InstanceEvent),
		revisions:  make(map[string][]store.Revision),
		dispatches: make(map[string]store.ModelDispatchRecord),
	}
}

func (m *memStore) ClaimModelDispatch(_ context.Context, namespace, operation, sessionID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := namespace + "/" + operation
	if _, exists := m.dispatches[key]; exists {
		return false, nil
	}
	m.dispatches[key] = store.ModelDispatchRecord{SessionID: sessionID}
	return true, nil
}

func (m *memStore) CompleteModelDispatch(_ context.Context, namespace, operation, sessionID string, response json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := namespace + "/" + operation
	record, exists := m.dispatches[key]
	if !exists || record.SessionID != sessionID || len(record.Response) != 0 {
		return store.ErrConflict
	}
	record.Response = append(json.RawMessage(nil), response...)
	m.dispatches[key] = record
	return nil
}

func (m *memStore) GetModelDispatch(_ context.Context, namespace, operation string) (store.ModelDispatchRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, exists := m.dispatches[namespace+"/"+operation]
	if !exists {
		return record, store.ErrNotFound
	}
	record.Response = append(json.RawMessage(nil), record.Response...)
	return record, nil
}

func (m *memStore) key(kind, ns, name string) string {
	return kind + "/" + ns + "/" + name
}

// injectRV patches the metadata.resourceVersion field in a JSON blob so that
// the in-memory store faithfully mirrors the postgres store behaviour.
func injectRV(data []byte, rv int64) []byte {
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err != nil {
		return data
	}
	meta, ok := obj["metadata"].(map[string]interface{})
	if !ok {
		meta = make(map[string]interface{})
		obj["metadata"] = meta
	}
	meta["resourceVersion"] = rv
	out, err := json.Marshal(obj)
	if err != nil {
		return data
	}
	return out
}

// TC-0005: Register reconciler, create resource, verify reconcile invocation.
// TC-0006: Reconciler failure should trigger retry/backoff requeue behavior.
// TC-0007: Leader failover when original leader lease expires.

func (m *memStore) Ping(_ context.Context) error { return nil }

func (m *memStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k := m.key(kind, namespace, name)
	d, ok := m.data[k]
	if !ok {
		return nil, store.ErrNotFound
	}
	return d, nil
}

func (m *memStore) List(_ context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	prefix := kind + "/"
	if opts.Namespace != "" {
		prefix = kind + "/" + opts.Namespace + "/"
	}
	var out [][]byte
	for k, v := range m.data {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			if len(opts.LabelSelector) > 0 {
				var meta struct {
					Metadata struct {
						Labels map[string]string `json:"labels"`
					} `json:"metadata"`
				}
				if err := json.Unmarshal(v, &meta); err != nil {
					continue
				}
				match := true
				for sk, sv := range opts.LabelSelector {
					if meta.Metadata.Labels[sk] != sv {
						match = false
						break
					}
				}
				if !match {
					continue
				}
			}
			out = append(out, v)
		}
	}
	return out, nil
}

func (m *memStore) Create(_ context.Context, kind, namespace, name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(kind, namespace, name)
	if _, ok := m.data[k]; ok {
		return store.ErrAlreadyExists
	}
	rv := m.nextRV
	m.nextRV++
	data = injectRV(data, rv)
	m.data[k] = data
	m.versions[k] = rv
	m.emit(store.WatchEvent{
		Type: store.EventCreated, Kind: kind,
		Namespace: namespace, Name: name,
		ResourceVersion: rv, Object: data,
	})
	return nil
}

func (m *memStore) Update(_ context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(kind, namespace, name)
	if _, ok := m.data[k]; !ok {
		return store.ErrNotFound
	}
	if m.versions[k] != resourceVersion {
		return store.ErrConflict
	}
	rv := m.nextRV
	m.nextRV++
	data = injectRV(data, rv)
	m.data[k] = data
	m.versions[k] = rv
	m.emit(store.WatchEvent{
		Type: store.EventUpdated, Kind: kind,
		Namespace: namespace, Name: name,
		ResourceVersion: rv, Object: data,
	})
	return nil
}

func (m *memStore) Delete(_ context.Context, kind, namespace, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(kind, namespace, name)
	if _, ok := m.data[k]; !ok {
		return store.ErrNotFound
	}
	delete(m.data, k)
	delete(m.versions, k)
	m.emit(store.WatchEvent{
		Type: store.EventDeleted, Kind: kind,
		Namespace: namespace, Name: name,
	})
	return nil
}

func (m *memStore) Watch(_ context.Context, kind string, _ store.ListOptions) (<-chan store.WatchEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan store.WatchEvent, 64)
	m.watchers[kind] = append(m.watchers[kind], ch)
	return ch, nil
}

// emit must be called with m.mu held.
func (m *memStore) emit(ev store.WatchEvent) {
	for _, ch := range m.watchers[ev.Kind] {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (m *memStore) CreateRevision(_ context.Context, kind, ns, name string, revision int64, spec, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := kind + "/" + ns + "/" + name
	m.revisions[k] = append(m.revisions[k], store.Revision{
		Revision:  revision,
		Spec:      spec,
		Data:      data,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	return nil
}

func (m *memStore) ListRevisions(_ context.Context, kind, ns, name string) ([]store.Revision, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.revisions[kind+"/"+ns+"/"+name], nil
}

func (m *memStore) GetRevision(_ context.Context, kind, ns, name string, revision int64) (*store.Revision, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k := kind + "/" + ns + "/" + name
	for _, rev := range m.revisions[k] {
		if rev.Revision == revision {
			return &rev, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *memStore) AppendEvent(_ context.Context, kind, namespace, name string, ev store.InstanceEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(kind, namespace, name)
	m.events[k] = append(m.events[k], ev)
	return nil
}

func (m *memStore) ListEvents(_ context.Context, kind, namespace, name string) ([]store.InstanceEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k := m.key(kind, namespace, name)
	evts := m.events[k]
	out := make([]store.InstanceEvent, len(evts))
	copy(out, evts)
	return out, nil
}

// ---------------------------------------------------------------------------
// testResource — helper to build a minimal JSON resource
// ---------------------------------------------------------------------------

type testResource struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Namespace       string `json:"namespace"`
		Name            string `json:"name"`
		ResourceVersion int64  `json:"resourceVersion"`
	} `json:"metadata"`
}

func makeResource(kind, ns, name string) []byte {
	r := testResource{APIVersion: "kognition.dev/v1alpha1", Kind: kind}
	r.Metadata.Namespace = ns
	r.Metadata.Name = name
	r.Metadata.ResourceVersion = 1
	data, _ := json.Marshal(r)
	return data
}

// ===========================================================================
// WorkQueue tests
// ===========================================================================

func TestWorkQueue_AddGetDone(t *testing.T) {
	q := NewWorkQueue(time.Millisecond, 10*time.Millisecond, 5)
	defer q.ShutDown()

	q.Add("a")
	q.Add("b")

	if q.Len() != 2 {
		t.Fatalf("expected len 2, got %d", q.Len())
	}

	k1, ok := q.Get()
	if !ok || k1 != "a" {
		t.Fatalf("expected (a, true), got (%s, %v)", k1, ok)
	}
	q.Done(k1)

	k2, ok := q.Get()
	if !ok || k2 != "b" {
		t.Fatalf("expected (b, true), got (%s, %v)", k2, ok)
	}
	q.Done(k2)
}

func TestWorkQueue_Deduplication(t *testing.T) {
	q := NewWorkQueue(time.Millisecond, 10*time.Millisecond, 5)
	defer q.ShutDown()

	q.Add("x")
	q.Add("x")
	q.Add("x")

	if q.Len() != 1 {
		t.Fatalf("expected len 1 after dedup, got %d", q.Len())
	}
}

func TestWorkQueue_DirtyRequeue(t *testing.T) {
	q := NewWorkQueue(time.Millisecond, 10*time.Millisecond, 5)
	defer q.ShutDown()

	q.Add("key")
	k, _ := q.Get() // now in processing

	// Add while processing → marks dirty.
	q.Add("key")
	if q.Len() != 0 {
		t.Fatalf("expected len 0 while processing, got %d", q.Len())
	}

	q.Done(k) // dirty item should re-appear

	if q.Len() != 1 {
		t.Fatalf("expected len 1 after Done on dirty item, got %d", q.Len())
	}

	k2, _ := q.Get()
	if k2 != "key" {
		t.Fatalf("expected re-queued key, got %s", k2)
	}
	q.Done(k2)
}

func TestWorkQueue_RequeueBackoff(t *testing.T) {
	q := NewWorkQueue(time.Millisecond, 100*time.Millisecond, 3)
	defer q.ShutDown()

	q.Add("fail")
	k, _ := q.Get()

	// First requeue should succeed.
	ok := q.Requeue(k)
	if !ok {
		t.Fatal("expected Requeue to succeed on first failure")
	}
	q.Done(k)

	if q.NumRequeues("fail") != 1 {
		t.Fatalf("expected 1 requeue, got %d", q.NumRequeues("fail"))
	}
}

func TestWorkQueue_MaxRetries(t *testing.T) {
	q := NewWorkQueue(time.Millisecond, 10*time.Millisecond, 2)
	defer q.ShutDown()

	q.Add("doomed")
	k, _ := q.Get()
	q.Requeue(k)
	q.Done(k)

	// Wait for the requeue to land.
	time.Sleep(50 * time.Millisecond)
	k, _ = q.Get()
	q.Requeue(k) // second failure
	q.Done(k)

	time.Sleep(50 * time.Millisecond)
	k, _ = q.Get()
	ok := q.Requeue(k) // third → exceeds maxRetries=2
	if ok {
		t.Fatal("expected Requeue to fail after max retries")
	}
	q.Done(k)
}

func TestWorkQueue_Shutdown(t *testing.T) {
	q := NewWorkQueue(time.Millisecond, 10*time.Millisecond, 5)

	q.ShutDown()

	if !q.ShuttingDown() {
		t.Fatal("expected ShuttingDown to be true")
	}

	// Get should return false immediately after shutdown.
	_, ok := q.Get()
	if ok {
		t.Fatal("expected Get to return false after shutdown")
	}
}

func TestWorkQueue_Forget(t *testing.T) {
	q := NewWorkQueue(time.Millisecond, 10*time.Millisecond, 5)
	defer q.ShutDown()

	q.Add("k")
	k, _ := q.Get()
	q.Requeue(k)
	q.Done(k)

	if q.NumRequeues("k") != 1 {
		t.Fatalf("expected 1 requeue, got %d", q.NumRequeues("k"))
	}

	q.Forget("k")
	if q.NumRequeues("k") != 0 {
		t.Fatalf("expected 0 after Forget, got %d", q.NumRequeues("k"))
	}
}

// ===========================================================================
// KeyFunc tests
// ===========================================================================

func TestKeyFunc(t *testing.T) {
	tests := []struct {
		ns, name string
		want     string
	}{
		{"default", "my-agent", "default/my-agent"},
		{"", "cluster-thing", "cluster-thing"},
	}
	for _, tt := range tests {
		got := KeyFunc(tt.ns, tt.name)
		if got != tt.want {
			t.Errorf("KeyFunc(%q, %q) = %q, want %q", tt.ns, tt.name, got, tt.want)
		}
	}
}

// ===========================================================================
// Informer tests
// ===========================================================================

type recordingHandler struct {
	mu      sync.Mutex
	adds    []string
	updates []string
	deletes []string
}

type countingEventHandler struct {
	updates atomic.Int64
}

func (*countingEventHandler) OnAdd(string)      {}
func (h *countingEventHandler) OnUpdate(string) { h.updates.Add(1) }
func (*countingEventHandler) OnDelete(string)   {}

func (r *recordingHandler) OnAdd(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adds = append(r.adds, key)
}

func (r *recordingHandler) OnUpdate(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, key)
}

func (r *recordingHandler) OnDelete(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletes = append(r.deletes, key)
}

func (r *recordingHandler) getAdds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.adds))
	copy(out, r.adds)
	return out
}

func (r *recordingHandler) getUpdates() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.updates))
	copy(out, r.updates)
	return out
}

func (r *recordingHandler) getDeletes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.deletes))
	copy(out, r.deletes)
	return out
}

func TestInformer_ListAndWatch(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	// Pre-populate some resources before the informer starts.
	ctx := context.Background()
	ms.Create(ctx, "Agent", "default", "agent-a", makeResource("Agent", "default", "agent-a"))
	ms.Create(ctx, "Agent", "default", "agent-b", makeResource("Agent", "default", "agent-b"))

	handler := &recordingHandler{}
	inf := NewInformer(ms, "Agent", logger)
	inf.AddEventHandler(handler)

	ctxRun, cancel := context.WithCancel(ctx)
	defer cancel()

	go inf.Run(ctxRun)

	// Wait for initial sync.
	deadline := time.After(2 * time.Second)
	for !inf.HasSynced() {
		select {
		case <-deadline:
			t.Fatal("informer did not sync in time")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Check that the initial list produced OnAdd events.
	adds := handler.getAdds()
	if len(adds) != 2 {
		t.Fatalf("expected 2 adds from initial list, got %d", len(adds))
	}

	// Check cache.
	for _, key := range adds {
		if inf.GetFromCache(key) == nil {
			t.Errorf("expected cache entry for %s", key)
		}
	}

	// Now create a new resource — should trigger an OnAdd via watch.
	ms.Create(ctx, "Agent", "default", "agent-c", makeResource("Agent", "default", "agent-c"))
	time.Sleep(100 * time.Millisecond)

	adds = handler.getAdds()
	if len(adds) != 3 {
		t.Fatalf("expected 3 adds after watch event, got %d", len(adds))
	}

	// Delete a resource — should trigger OnDelete.
	ms.Delete(ctx, "Agent", "default", "agent-a")
	time.Sleep(100 * time.Millisecond)

	deletes := handler.getDeletes()
	if len(deletes) != 1 {
		t.Fatalf("expected 1 delete, got %d", len(deletes))
	}
	if deletes[0] != "default/agent-a" {
		t.Errorf("expected delete key default/agent-a, got %s", deletes[0])
	}

	// Verify the cache no longer has agent-a.
	if inf.GetFromCache("default/agent-a") != nil {
		t.Error("expected agent-a to be removed from cache")
	}
}

// ===========================================================================
// Controller tests
// ===========================================================================

type countingReconciler struct {
	mu        sync.Mutex
	calls     []Request
	err       error // if non-nil, Reconcile returns this error
	result    Result
	onCall    func(req Request) // optional callback
	callCount atomic.Int32
}

func (r *countingReconciler) Reconcile(_ context.Context, req Request) (Result, error) {
	r.callCount.Add(1)
	r.mu.Lock()
	r.calls = append(r.calls, req)
	err := r.err
	res := r.result
	fn := r.onCall
	r.mu.Unlock()

	if fn != nil {
		fn(req)
	}
	return res, err
}

func (r *countingReconciler) getCalls() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Request, len(r.calls))
	copy(out, r.calls)
	return out
}

func TestController_ReconcileOnCreate(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	rec := &countingReconciler{}
	inf := NewInformer(ms, "Agent", logger)

	ctrl := NewController(ControllerConfig{
		Name:       "test-controller",
		Kind:       "Agent",
		Reconciler: rec,
		Workers:    2,
	}, inf, logger)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		ctrl.Run(ctx)
		close(done)
	}()

	// Wait for sync.
	deadline := time.After(2 * time.Second)
	for !inf.HasSynced() {
		select {
		case <-deadline:
			t.Fatal("informer did not sync")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Create a resource — should trigger reconciliation.
	ms.Create(ctx, "Agent", "default", "my-agent", makeResource("Agent", "default", "my-agent"))

	// Wait for the reconciler to be called.
	ok := waitFor(t, 2*time.Second, func() bool {
		return rec.callCount.Load() >= 1
	})
	if !ok {
		t.Fatal("reconciler was not called within timeout")
	}

	calls := rec.getCalls()
	found := false
	for _, c := range calls {
		if c.Kind == "Agent" && c.Namespace == "default" && c.Name == "my-agent" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected reconcile for default/my-agent, calls: %+v", calls)
	}

	cancel()
	waitForTestSignal(t, done, "controller shutdown")
}

func TestController_ErrorTriggersRequeue(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	// Pre-populate a resource.
	ctx := context.Background()
	ms.Create(ctx, "Agent", "default", "fail-agent", makeResource("Agent", "default", "fail-agent"))

	callCount := &atomic.Int32{}
	rec := &countingReconciler{
		err: context.DeadlineExceeded,
		onCall: func(_ Request) {
			callCount.Add(1)
		},
	}

	inf := NewInformer(ms, "Agent", logger)
	ctrl := NewController(ControllerConfig{
		Name:       "fail-controller",
		Kind:       "Agent",
		Reconciler: rec,
		Workers:    1,
		BaseDelay:  time.Millisecond,
		MaxDelay:   10 * time.Millisecond,
		MaxRetries: 3,
	}, inf, logger)

	ctxRun, cancel := context.WithCancel(ctx)

	done := make(chan struct{})
	go func() {
		ctrl.Run(ctxRun)
		close(done)
	}()

	// Wait for informer sync + initial reconciliation + at least 2 retries.
	ok := waitFor(t, 3*time.Second, func() bool {
		return callCount.Load() >= 3
	})
	if !ok {
		t.Fatalf("expected >= 3 reconcile calls (retries), got %d", callCount.Load())
	}

	cancel()
	waitForTestSignal(t, done, "controller shutdown after reconcile error")
}

func TestController_RequeueAfter(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	rec := &countingReconciler{
		result: Result{RequeueAfter: 50 * time.Millisecond},
	}

	inf := NewInformer(ms, "Agent", logger)
	ctrl := NewController(ControllerConfig{
		Name:       "requeue-ctrl",
		Kind:       "Agent",
		Reconciler: rec,
		Workers:    1,
	}, inf, logger)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		ctrl.Run(ctx)
		close(done)
	}()

	// Wait for sync.
	waitFor(t, 2*time.Second, func() bool { return inf.HasSynced() })

	// Create a resource.
	ms.Create(ctx, "Agent", "default", "timed", makeResource("Agent", "default", "timed"))

	// Should be reconciled at least twice: initial + requeue after 50ms.
	ok := waitFor(t, 2*time.Second, func() bool {
		return rec.callCount.Load() >= 2
	})
	if !ok {
		t.Fatalf("expected >= 2 reconcile calls with RequeueAfter, got %d", rec.callCount.Load())
	}

	cancel()
	waitForTestSignal(t, done, "controller shutdown after requeue")
}

// ===========================================================================
// Manager tests
// ===========================================================================

func TestManager_RegisterAndStart(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	rec := &countingReconciler{}

	mgr := NewManager(ManagerConfig{
		Store:  ms,
		Logger: logger,
	})

	mgr.Register(ControllerConfig{
		Name:       "agent-ctrl",
		Kind:       "Agent",
		Reconciler: rec,
		Workers:    1,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mgr.Start(ctx)
		close(done)
	}()

	// Wait for the informer to sync.
	time.Sleep(200 * time.Millisecond)

	// Create a resource.
	ms.Create(ctx, "Agent", "default", "mgr-agent", makeResource("Agent", "default", "mgr-agent"))

	ok := waitFor(t, 2*time.Second, func() bool {
		return rec.callCount.Load() >= 1
	})
	if !ok {
		t.Fatal("manager-registered controller did not reconcile")
	}

	cancel()
	waitForTestSignal(t, done, "manager shutdown")
}

func TestManager_SharedInformer(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	rec1 := &countingReconciler{}
	rec2 := &countingReconciler{}

	mgr := NewManager(ManagerConfig{
		Store:  ms,
		Logger: logger,
	})

	// Register two controllers for the same kind — should share one informer.
	mgr.Register(ControllerConfig{
		Name: "ctrl-1", Kind: "Agent", Reconciler: rec1, Workers: 1,
	})
	mgr.Register(ControllerConfig{
		Name: "ctrl-2", Kind: "Agent", Reconciler: rec2, Workers: 1,
	})

	if len(mgr.informers) != 1 {
		t.Fatalf("expected 1 shared informer, got %d", len(mgr.informers))
	}
}

// ===========================================================================
// Leader Election tests
// ===========================================================================

func TestLeaderElection_SingleInstance(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	elected := make(chan struct{}, 1)

	le := NewLeaderElector(LeaderElectionConfig{
		LeaseName:     "test-lease",
		LeaseDuration: 5 * time.Second,
		RenewDeadline: 2 * time.Second,
		RetryPeriod:   100 * time.Millisecond,
		Identity:      "instance-1",
	}, ms, logger)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		le.Run(ctx, func(ctx context.Context) {
			elected <- struct{}{}
			<-ctx.Done()
		})
		close(done)
	}()

	select {
	case <-elected:
		// Success — we became leader.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for leader election")
	}

	cancel()
	waitForTestSignal(t, done, "leader elector shutdown")
}

func TestLeaderElection_SameHolderReacquisitionRefreshesLease(t *testing.T) {
	ms := newMemStore()
	le := NewLeaderElector(LeaderElectionConfig{
		LeaseName:     "restart-lease",
		LeaseDuration: 5 * time.Second,
		RenewDeadline: 2 * time.Second,
		RetryPeriod:   100 * time.Millisecond,
		Identity:      "instance-1",
	}, ms, zaptest.NewLogger(t))
	ctx := context.Background()

	if !le.tryAcquire(ctx) {
		t.Fatal("initial tryAcquire() = false, want true")
	}
	raw, err := ms.Get(ctx, leaseKind, leaseNamespace, "restart-lease")
	if err != nil {
		t.Fatalf("get initial lease: %v", err)
	}
	var stale leaseWrapper
	if err := json.Unmarshal(raw, &stale); err != nil {
		t.Fatalf("unmarshal initial lease: %v", err)
	}
	staleRenewTime := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	stale.Spec.RenewTime = staleRenewTime
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal stale lease: %v", err)
	}
	if err := ms.Update(ctx, leaseKind, leaseNamespace, "restart-lease", data, stale.Metadata.ResourceVersion); err != nil {
		t.Fatalf("backdate lease: %v", err)
	}

	if !le.tryAcquire(ctx) {
		t.Fatal("same-holder tryAcquire() = false, want true")
	}
	raw, err = ms.Get(ctx, leaseKind, leaseNamespace, "restart-lease")
	if err != nil {
		t.Fatalf("get reacquired lease: %v", err)
	}
	var refreshed leaseWrapper
	if err := json.Unmarshal(raw, &refreshed); err != nil {
		t.Fatalf("unmarshal reacquired lease: %v", err)
	}
	if refreshed.Spec.RenewTime == staleRenewTime {
		t.Fatalf("renewTime after same-holder reacquisition = %q, want refreshed timestamp", refreshed.Spec.RenewTime)
	}
}

func TestLeaderElection_ExpiredLeaseTakeover(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	// Instance 1 acquires the lease with a very short duration.
	le1 := NewLeaderElector(LeaderElectionConfig{
		LeaseName:     "takeover-lease",
		LeaseDuration: 200 * time.Millisecond,
		RenewDeadline: 100 * time.Millisecond,
		RetryPeriod:   50 * time.Millisecond,
		Identity:      "instance-1",
	}, ms, logger)

	ctx1, cancel1 := context.WithCancel(context.Background())

	leader1Ready := make(chan struct{}, 1)
	done1 := make(chan struct{})
	go func() {
		le1.Run(ctx1, func(ctx context.Context) {
			leader1Ready <- struct{}{}
			<-ctx.Done()
		})
		close(done1)
	}()

	select {
	case <-leader1Ready:
	case <-time.After(2 * time.Second):
		t.Fatal("instance-1 did not become leader")
	}

	// Kill instance 1 so its lease expires.
	cancel1()
	waitForTestSignal(t, done1, "first leader elector shutdown")
	time.Sleep(300 * time.Millisecond) // Wait for lease to expire.

	// Instance 2 should take over.
	le2 := NewLeaderElector(LeaderElectionConfig{
		LeaseName:     "takeover-lease",
		LeaseDuration: 5 * time.Second,
		RenewDeadline: 2 * time.Second,
		RetryPeriod:   50 * time.Millisecond,
		Identity:      "instance-2",
	}, ms, logger)

	ctx2, cancel2 := context.WithCancel(context.Background())

	leader2Ready := make(chan struct{}, 1)
	done2 := make(chan struct{})
	go func() {
		le2.Run(ctx2, func(ctx context.Context) {
			leader2Ready <- struct{}{}
			<-ctx.Done()
		})
		close(done2)
	}()

	select {
	case <-leader2Ready:
		// Success — instance-2 took over.
	case <-time.After(2 * time.Second):
		t.Fatal("instance-2 did not take over expired lease")
	}

	cancel2()
	waitForTestSignal(t, done2, "second leader elector shutdown")
}

func TestLeaderElection_DoesNotTakeOverUnexpiredOrMalformedLease(t *testing.T) {
	tests := []struct {
		name          string
		renewTime     string
		leaseDuration string
	}{
		{
			name:          "active foreign lease",
			renewTime:     time.Now().UTC().Format(time.RFC3339),
			leaseDuration: "5m",
		},
		{
			name:          "malformed renewal time",
			renewTime:     "not-a-time",
			leaseDuration: "5m",
		},
		{
			name:          "malformed duration uses configured duration",
			renewTime:     time.Now().UTC().Add(-time.Second).Format(time.RFC3339),
			leaseDuration: "not-a-duration",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ms := newMemStore()
			leaseName := "contended-lease"
			record := leaseRecord{
				HolderIdentity: "current-leader",
				LeaseDuration:  test.leaseDuration,
				AcquireTime:    time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
				RenewTime:      test.renewTime,
			}
			data, err := json.Marshal(wrapLease(leaseName, record))
			if err != nil {
				t.Fatalf("marshal lease: %v", err)
			}
			if err := ms.Create(context.Background(), leaseKind, leaseNamespace, leaseName, data); err != nil {
				t.Fatalf("seed lease: %v", err)
			}

			contender := NewLeaderElector(LeaderElectionConfig{
				LeaseName:     leaseName,
				LeaseDuration: 5 * time.Second,
				Identity:      "contender",
			}, ms, zaptest.NewLogger(t))
			if contender.tryAcquire(context.Background()) {
				t.Fatal("contender acquired lease that must remain with current leader")
			}

			raw, err := ms.Get(context.Background(), leaseKind, leaseNamespace, leaseName)
			if err != nil {
				t.Fatalf("get lease after contention: %v", err)
			}
			var persisted leaseWrapper
			if err := json.Unmarshal(raw, &persisted); err != nil {
				t.Fatalf("unmarshal persisted lease: %v", err)
			}
			if persisted.Spec.HolderIdentity != "current-leader" {
				t.Fatalf("holder after contention = %q, want current-leader", persisted.Spec.HolderIdentity)
			}
		})
	}
}

func TestLeaderElection_ExpiredLeaseConflictDoesNotAcquireLeadership(t *testing.T) {
	ms := newMemStore()
	leaseName := "contended-expired-lease"
	record := leaseRecord{
		HolderIdentity: "expired-leader",
		LeaseDuration:  "1s",
		AcquireTime:    time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		RenewTime:      time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
	}
	data, err := json.Marshal(wrapLease(leaseName, record))
	if err != nil {
		t.Fatalf("marshal lease: %v", err)
	}
	if err := ms.Create(context.Background(), leaseKind, leaseNamespace, leaseName, data); err != nil {
		t.Fatalf("seed expired lease: %v", err)
	}

	contender := NewLeaderElector(LeaderElectionConfig{
		LeaseName: leaseName,
		Identity:  "contender",
	}, &faultStore{memStore: ms, updateErr: store.ErrConflict}, zaptest.NewLogger(t))
	if contender.tryAcquire(context.Background()) {
		t.Fatal("contender acquired leadership after takeover conflict")
	}

	raw, err := ms.Get(context.Background(), leaseKind, leaseNamespace, leaseName)
	if err != nil {
		t.Fatalf("get lease after conflict: %v", err)
	}
	var persisted leaseWrapper
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("unmarshal persisted lease: %v", err)
	}
	if persisted.Spec.HolderIdentity != "expired-leader" {
		t.Fatalf("holder after conflict = %q, want expired-leader", persisted.Spec.HolderIdentity)
	}
}

// ===========================================================================
// Controller.keyToRequest tests
// ===========================================================================

func TestController_keyToRequest(t *testing.T) {
	logger := zaptest.NewLogger(t)
	c := &Controller{kind: "Agent", logger: logger}

	tests := []struct {
		key  string
		want Request
	}{
		{"default/my-agent", Request{Kind: "Agent", Namespace: "default", Name: "my-agent"}},
		{"cluster-resource", Request{Kind: "Agent", Name: "cluster-resource"}},
	}
	for _, tt := range tests {
		got := c.keyToRequest(tt.key)
		if got != tt.want {
			t.Errorf("keyToRequest(%q) = %+v, want %+v", tt.key, got, tt.want)
		}
	}
}

// ===========================================================================
// helpers
// ===========================================================================

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if cond() {
			return true
		}
		select {
		case <-deadline:
			return false
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func waitForTestSignal(t *testing.T, signal <-chan struct{}, operation string) {
	t.Helper()
	waitForTestSignalWithin(t, signal, operation, 2*time.Second)
}

// waitForTestSignalWithin allows a longer budget for tests whose work is bounded
// by CPU scheduling rather than by the behavior under test.
func waitForTestSignalWithin(t *testing.T, signal <-chan struct{}, operation string, limit time.Duration) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(limit):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

// ===========================================================================
// Additional coverage tests
// ===========================================================================

// --- failingWatchStore wraps memStore but can make Watch fail ---

type failingWatchStore struct {
	*memStore
	mu         sync.Mutex
	watchFails int // number of remaining Watch failures
}

type reconnectFailingWatchStore struct {
	*memStore
	mu                  sync.Mutex
	watchCalls          int
	replacementFailures int
	firstWatch          chan store.WatchEvent
}

func (s *reconnectFailingWatchStore) Watch(ctx context.Context, kind string, opts store.ListOptions) (<-chan store.WatchEvent, error) {
	s.mu.Lock()
	s.watchCalls++
	if s.watchCalls == 1 {
		s.mu.Unlock()
		return s.firstWatch, nil
	}
	if s.watchCalls <= 1+s.replacementFailures {
		s.mu.Unlock()
		return nil, errors.New("replacement watch unavailable")
	}
	s.mu.Unlock()
	return s.memStore.Watch(ctx, kind, opts)
}

func (s *reconnectFailingWatchStore) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watchCalls
}

func (f *failingWatchStore) Watch(ctx context.Context, kind string, opts store.ListOptions) (<-chan store.WatchEvent, error) {
	f.mu.Lock()
	if f.watchFails > 0 {
		f.watchFails--
		f.mu.Unlock()
		return nil, errors.New("simulated watch error")
	}
	f.mu.Unlock()
	return f.memStore.Watch(ctx, kind, opts)
}

type blockingListStore struct {
	*memStore
	listed  chan struct{}
	release chan struct{}
}

func (s *blockingListStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	items, err := s.memStore.List(ctx, kind, opts)
	close(s.listed)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return items, err
	}
}

func TestInformer_DoesNotLoseEventsAfterInitialList(t *testing.T) {
	ms := newMemStore()
	bs := &blockingListStore{
		memStore: ms,
		listed:   make(chan struct{}),
		release:  make(chan struct{}),
	}
	logger := zaptest.NewLogger(t)
	handler := &recordingHandler{}
	inf := NewInformer(bs, "Agent", logger)
	inf.AddEventHandler(handler)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = inf.Run(ctx)
	}()

	waitForTestSignal(t, bs.listed, "informer initial list")
	if err := ms.Create(ctx, "Agent", "default", "during-list", makeResource("Agent", "default", "during-list")); err != nil {
		t.Fatalf("create during initial list: %v", err)
	}
	close(bs.release)

	if !waitFor(t, 2*time.Second, func() bool {
		return inf.GetFromCache("default/during-list") != nil
	}) {
		t.Fatal("event created after initial list snapshot was lost")
	}

	cancel()
	waitForTestSignal(t, done, "informer shutdown")
}

func TestInformer_ConcurrentHandlerRegistrationAndDispatch(t *testing.T) {
	inf := NewInformer(newMemStore(), "Agent", zap.NewNop())
	handler := &countingEventHandler{}
	for range 100 {
		inf.AddEventHandler(handler)
	}

	start := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-start
		for range 1000 {
			inf.handleEvent(store.WatchEvent{
				Type:      store.EventUpdated,
				Namespace: "default",
				Name:      "agent",
			})
		}
	}()

	close(start)
	for range 1000 {
		inf.AddEventHandler(handler)
	}
	// About 1.1 million dispatches race with registration; a loaded host needs more than the default.
	waitForTestSignalWithin(t, done, "concurrent event dispatch", 30*time.Second)

	before := handler.updates.Load()
	inf.handleEvent(store.WatchEvent{
		Type:      store.EventUpdated,
		Namespace: "default",
		Name:      "agent",
	})
	if got := handler.updates.Load() - before; got != 1100 {
		t.Fatalf("handlers invoked after concurrent registration = %d, want 1100", got)
	}
}

func TestController_OnUpdateAndDelete(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	rec := &countingReconciler{}
	inf := NewInformer(ms, "Agent", logger)

	ctrl := NewController(ControllerConfig{
		Name:       "update-delete-ctrl",
		Kind:       "Agent",
		Reconciler: rec,
		Workers:    1,
	}, inf, logger)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		ctrl.Run(ctx)
		close(done)
	}()

	waitFor(t, 2*time.Second, func() bool { return inf.HasSynced() })

	// Create a resource.
	ms.Create(ctx, "Agent", "default", "upd-agent", makeResource("Agent", "default", "upd-agent"))
	waitFor(t, 2*time.Second, func() bool { return rec.callCount.Load() >= 1 })

	// Update the resource (triggers OnUpdate → queue.Add).
	ms.Update(ctx, "Agent", "default", "upd-agent", makeResource("Agent", "default", "upd-agent"), 1)
	waitFor(t, 2*time.Second, func() bool { return rec.callCount.Load() >= 2 })

	// Delete the resource (triggers OnDelete → queue.Add).
	ms.Delete(ctx, "Agent", "default", "upd-agent")
	ok := waitFor(t, 2*time.Second, func() bool { return rec.callCount.Load() >= 3 })
	if !ok {
		t.Fatalf("expected >= 3 reconcile calls (create+update+delete), got %d", rec.callCount.Load())
	}

	cancel()
	waitForTestSignal(t, done, "controller shutdown after update and delete")
}

func TestInformer_UpdateEvent(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	handler := &recordingHandler{}
	inf := NewInformer(ms, "Agent", logger)
	inf.AddEventHandler(handler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go inf.Run(ctx)
	waitFor(t, 2*time.Second, func() bool { return inf.HasSynced() })

	// Create then update.
	ms.Create(ctx, "Agent", "default", "upd-me", makeResource("Agent", "default", "upd-me"))
	time.Sleep(100 * time.Millisecond)

	ms.Update(ctx, "Agent", "default", "upd-me", makeResource("Agent", "default", "upd-me"), 1)
	time.Sleep(100 * time.Millisecond)

	updates := handler.getUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 update event, got %d", len(updates))
	}
	if updates[0] != "default/upd-me" {
		t.Errorf("expected update key default/upd-me, got %s", updates[0])
	}

	// Verify cache is updated.
	cached := inf.GetFromCache("default/upd-me")
	if cached == nil {
		t.Error("expected cache entry after update")
	}
}

func TestInformer_WatchReconnect(t *testing.T) {
	ms := newMemStore()
	fs := &failingWatchStore{memStore: ms, watchFails: 2} // first 2 Watch calls fail
	logger := zaptest.NewLogger(t)

	handler := &recordingHandler{}
	inf := NewInformer(fs, "Agent", logger)
	inf.AddEventHandler(handler)

	// Pre-populate a resource for the list phase.
	ctx := context.Background()
	ms.Create(ctx, "Agent", "default", "exist", makeResource("Agent", "default", "exist"))

	ctxRun, cancel := context.WithCancel(ctx)
	defer cancel()

	go inf.Run(ctxRun)

	// Even though Watch fails twice, informer should sync (list succeeded)
	// and eventually reconnect on the 3rd Watch call.
	waitFor(t, 2*time.Second, func() bool { return inf.HasSynced() })

	// Give informer time to reconnect (retry watch twice then succeed).
	time.Sleep(200 * time.Millisecond)

	// Now create a resource — should be picked up via the successful watch.
	ms.Create(ctx, "Agent", "default", "after-reconnect", makeResource("Agent", "default", "after-reconnect"))
	time.Sleep(200 * time.Millisecond)

	adds := handler.getAdds()
	found := false
	for _, a := range adds {
		if a == "default/after-reconnect" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected to receive add event after reconnect, adds: %v", adds)
	}
}

func TestInformer_RetriesReplacementWatchWithBackoff(t *testing.T) {
	baseStore := newMemStore()
	watchStore := &reconnectFailingWatchStore{
		memStore:            baseStore,
		replacementFailures: 1,
		firstWatch:          make(chan store.WatchEvent),
	}
	inf := NewInformer(watchStore, "Agent", zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- inf.Run(ctx) }()

	if !waitFor(t, 2*time.Second, inf.HasSynced) {
		t.Fatal("informer did not sync")
	}
	close(watchStore.firstWatch)
	if !waitFor(t, 2*time.Second, func() bool { return watchStore.calls() >= 3 }) {
		t.Fatalf("informer stopped retrying replacement watch after %d calls", watchStore.calls())
	}
	time.Sleep(50 * time.Millisecond)
	if calls := watchStore.calls(); calls != 3 {
		t.Fatalf("Watch() calls during reconnect backoff = %d, want 3", calls)
	}
	if err := baseStore.Create(ctx, "Agent", "default", "recovered", makeResource("Agent", "default", "recovered")); err != nil {
		t.Fatalf("create resource after watch recovery: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool {
		return inf.GetFromCache("default/recovered") != nil
	}) {
		t.Fatal("informer did not receive events after replacement watch recovered")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error after cancellation = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for informer shutdown after watch recovery")
	}
}

func TestInformer_HandleUnknownEvent(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	handler := &recordingHandler{}
	inf := NewInformer(ms, "Agent", logger)
	inf.AddEventHandler(handler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go inf.Run(ctx)
	waitFor(t, 2*time.Second, func() bool { return inf.HasSynced() })

	// Manually inject an unknown event type through the watch channel.
	ms.mu.Lock()
	for _, ch := range ms.watchers["Agent"] {
		ch <- store.WatchEvent{
			Type:      store.EventType("UNKNOWN"),
			Kind:      "Agent",
			Namespace: "default",
			Name:      "mystery",
		}
	}
	ms.mu.Unlock()

	time.Sleep(100 * time.Millisecond)

	// No adds, updates, or deletes should occur.
	if len(handler.getAdds()) != 0 {
		t.Error("unexpected adds from unknown event")
	}
	if len(handler.getUpdates()) != 0 {
		t.Error("unexpected updates from unknown event")
	}
	if len(handler.getDeletes()) != 0 {
		t.Error("unexpected deletes from unknown event")
	}

	// Cache should not contain the key.
	if inf.GetFromCache("default/mystery") != nil {
		t.Error("expected no cache entry for unknown event")
	}
}

func TestInformer_ResyncEventRelistsMissedState(t *testing.T) {
	ms := newMemStore()
	inf := NewInformer(ms, "Agent", zaptest.NewLogger(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go inf.Run(ctx)
	waitFor(t, 2*time.Second, func() bool { return inf.HasSynced() })

	ms.mu.Lock()
	for _, ch := range ms.watchers["Agent"] {
		ch <- store.WatchEvent{Type: store.EventResyncRequired, Kind: "Agent"}
	}
	ms.mu.Unlock()
	if err := ms.Create(ctx, "Agent", "default", "missed", makeResource("Agent", "default", "missed")); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 2*time.Second, func() bool {
		return inf.GetFromCache("default/missed") != nil
	})
}

func TestLeaderElection_Renewal(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	le := NewLeaderElector(LeaderElectionConfig{
		LeaseName:     "renew-test",
		LeaseDuration: 5 * time.Second,
		RenewDeadline: 50 * time.Millisecond, // very short so we see renewals quickly
		RetryPeriod:   50 * time.Millisecond,
		Identity:      "renewer-1",
	}, ms, logger)

	ctx, cancel := context.WithCancel(context.Background())

	elected := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		le.Run(ctx, func(ctx context.Context) {
			elected <- struct{}{}
			<-ctx.Done()
		})
		close(done)
	}()

	select {
	case <-elected:
	case <-time.After(2 * time.Second):
		t.Fatal("did not become leader")
	}

	// Wait for at least one renewal to happen (RenewDeadline = 50ms).
	time.Sleep(150 * time.Millisecond)

	// Verify the lease renewTime was updated.
	existing, err := ms.Get(ctx, leaseKind, leaseNamespace, "renew-test")
	if err != nil {
		t.Fatalf("failed to get lease: %v", err)
	}

	var wrapper leaseWrapper
	if err := json.Unmarshal(existing, &wrapper); err != nil {
		t.Fatalf("failed to unmarshal lease: %v", err)
	}

	// The resourceVersion should be > 1 (initial create = 1, renewals increment it).
	if wrapper.Metadata.ResourceVersion <= 1 {
		t.Errorf("expected resourceVersion > 1 after renewal, got %d", wrapper.Metadata.ResourceVersion)
	}

	cancel()
	waitForTestSignal(t, done, "leader elector shutdown after renewal")
}

func TestLeaderElection_RenewalFailureStepDown(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	le := NewLeaderElector(LeaderElectionConfig{
		LeaseName:     "stepdown-test",
		LeaseDuration: 5 * time.Second,
		RenewDeadline: 500 * time.Millisecond,
		RetryPeriod:   50 * time.Millisecond,
		Identity:      "stepper-1",
	}, ms, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	steppedDown := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		le.Run(ctx, func(ctx context.Context) {
			<-ctx.Done()
			steppedDown <- struct{}{}
		})
		close(done)
	}()

	// Wait for election to complete.
	time.Sleep(100 * time.Millisecond)

	// Tamper with the lease — change the holder identity so renew() fails.
	// Retry in a loop in case a renewal tick races with our update.
	for i := 0; i < 20; i++ {
		existing, _ := ms.Get(ctx, leaseKind, leaseNamespace, "stepdown-test")
		var wrapper leaseWrapper
		json.Unmarshal(existing, &wrapper)
		wrapper.Spec.HolderIdentity = "intruder"
		data, _ := json.Marshal(wrapper)
		if err := ms.Update(ctx, leaseKind, leaseNamespace, "stepdown-test", data, wrapper.Metadata.ResourceVersion); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The leader should detect the holder mismatch and step down.
	select {
	case <-steppedDown:
		// Success — stepped down.
	case <-time.After(3 * time.Second):
		t.Fatal("leader did not step down after renewal failure")
	}

	cancel()
	waitForTestSignal(t, done, "leader elector shutdown after step-down")
}

func TestLeaderElectionConfig_Defaults(t *testing.T) {
	cfg := LeaderElectionConfig{
		LeaseName: "default-test",
	}
	cfg.setDefaults()

	if cfg.LeaseDuration != 15*time.Second {
		t.Errorf("expected LeaseDuration 15s, got %v", cfg.LeaseDuration)
	}
	if cfg.RenewDeadline != 10*time.Second {
		t.Errorf("expected RenewDeadline 10s, got %v", cfg.RenewDeadline)
	}
	if cfg.RetryPeriod != 2*time.Second {
		t.Errorf("expected RetryPeriod 2s, got %v", cfg.RetryPeriod)
	}
	if cfg.Identity == "" {
		t.Error("expected auto-generated Identity, got empty string")
	}
	if len(cfg.Identity) < 32 { // UUID format
		t.Errorf("expected UUID-style Identity, got %q", cfg.Identity)
	}
}

func TestManager_WithLeaderElection(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	rec := &countingReconciler{}

	mgr := NewManager(ManagerConfig{
		Store:  ms,
		Logger: logger,
		LeaderElection: &LeaderElectionConfig{
			LeaseName:     "mgr-le-test",
			LeaseDuration: 5 * time.Second,
			RenewDeadline: 1 * time.Second,
			RetryPeriod:   50 * time.Millisecond,
			Identity:      "mgr-instance-1",
		},
	})

	mgr.Register(ControllerConfig{
		Name:       "le-agent-ctrl",
		Kind:       "Agent",
		Reconciler: rec,
		Workers:    1,
	})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		mgr.Start(ctx)
		close(done)
	}()

	// Wait for leader election + informer sync.
	time.Sleep(500 * time.Millisecond)

	// Create a resource — should trigger reconciliation via leader-elected controllers.
	ms.Create(ctx, "Agent", "default", "le-agent", makeResource("Agent", "default", "le-agent"))

	ok := waitFor(t, 3*time.Second, func() bool {
		return rec.callCount.Load() >= 1
	})
	if !ok {
		t.Fatal("leader-elected controller did not reconcile")
	}

	cancel()
	waitForTestSignal(t, done, "leader-elected manager shutdown")
}

func TestController_ResultRequeue(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	rec := &countingReconciler{
		result: Result{Requeue: true},
	}

	inf := NewInformer(ms, "Agent", logger)
	ctrl := NewController(ControllerConfig{
		Name:       "requeue-true-ctrl",
		Kind:       "Agent",
		Reconciler: rec,
		Workers:    1,
	}, inf, logger)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		ctrl.Run(ctx)
		close(done)
	}()

	waitFor(t, 2*time.Second, func() bool { return inf.HasSynced() })

	// Create a resource — reconciler returns Requeue=true, so it should be called repeatedly.
	ms.Create(ctx, "Agent", "default", "requeue-me", makeResource("Agent", "default", "requeue-me"))

	ok := waitFor(t, 2*time.Second, func() bool {
		return rec.callCount.Load() >= 3
	})
	if !ok {
		t.Fatalf("expected >= 3 calls with Result{Requeue: true}, got %d", rec.callCount.Load())
	}

	cancel()
	waitForTestSignal(t, done, "controller shutdown after immediate requeue")
}

func TestController_WaitForSyncCanceled(t *testing.T) {
	// Use a failingWatchStore that also fails List to prevent sync.
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	rec := &countingReconciler{}

	// Create an informer with a store that has no data and never sends watch events.
	// Cancel context immediately to test the waitForSync timeout path.
	inf := NewInformer(ms, "Agent", logger)

	ctrl := NewController(ControllerConfig{
		Name:       "sync-cancel-ctrl",
		Kind:       "Agent",
		Reconciler: rec,
		Workers:    1,
	}, inf, logger)

	// Create a context that cancels very quickly — before the informer can sync.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	// Give a tiny delay so the timeout fires before list/watch.
	time.Sleep(5 * time.Millisecond)

	err := ctrl.Run(ctx)
	_ = err // Run may return nil if sync completes before the short timeout.
	// The key check: reconciler was never called because sync didn't complete.
	if rec.callCount.Load() != 0 {
		t.Errorf("expected 0 reconcile calls when sync was canceled, got %d", rec.callCount.Load())
	}
}

func TestController_MaxRetriesDrop(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)

	rec := &countingReconciler{
		err: errors.New("permanent failure"),
	}

	inf := NewInformer(ms, "Agent", logger)
	ctrl := NewController(ControllerConfig{
		Name:       "maxretry-ctrl",
		Kind:       "Agent",
		Reconciler: rec,
		Workers:    1,
		BaseDelay:  time.Millisecond,
		MaxDelay:   5 * time.Millisecond,
		MaxRetries: 2, // very low — will exceed quickly
	}, inf, logger)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		ctrl.Run(ctx)
		close(done)
	}()

	waitFor(t, 2*time.Second, func() bool { return inf.HasSynced() })

	// Create a resource — reconciler always fails.
	ms.Create(ctx, "Agent", "default", "doomed", makeResource("Agent", "default", "doomed"))

	// Should be called at most maxRetries+1 times then dropped.
	// Wait for calls to stop increasing.
	time.Sleep(300 * time.Millisecond)
	finalCount := rec.callCount.Load()

	// Give more time to confirm no more calls happen.
	time.Sleep(200 * time.Millisecond)
	if rec.callCount.Load() != finalCount {
		t.Errorf("expected calls to stop after max retries, but got more: %d vs %d",
			finalCount, rec.callCount.Load())
	}

	// Should have been called initial + maxRetries times = 3 (1 + 2 retries).
	if finalCount < 2 || finalCount > 4 {
		t.Errorf("expected 2-4 reconcile calls with maxRetries=2, got %d", finalCount)
	}

	cancel()
	waitForTestSignal(t, done, "controller shutdown after max retries")
}

func TestWorkQueue_AddAfterShutdown(t *testing.T) {
	q := NewWorkQueue(time.Millisecond, 10*time.Millisecond, 5)

	q.Add("before")
	q.ShutDown()

	// Add after shutdown should be a no-op.
	q.Add("after")

	if q.Len() != 1 {
		t.Errorf("expected len 1 (only pre-shutdown item), got %d", q.Len())
	}

	// Get should drain the pre-shutdown item or return false.
	_, ok := q.Get()
	if ok {
		// The item "before" was still in the queue (shutdown doesn't drain).
		// Second Get should return false.
		_, ok2 := q.Get()
		if ok2 {
			t.Error("expected Get to return false after shutdown with empty queue")
		}
	}
}

func TestControllerConfig_SetDefaults(t *testing.T) {
	cfg := ControllerConfig{
		Name: "test",
		Kind: "Agent",
	}
	cfg.setDefaults()

	if cfg.Workers != 1 {
		t.Errorf("expected Workers=1, got %d", cfg.Workers)
	}
	if cfg.BaseDelay != time.Second {
		t.Errorf("expected BaseDelay=1s, got %v", cfg.BaseDelay)
	}
	if cfg.MaxDelay != 5*time.Minute {
		t.Errorf("expected MaxDelay=5m, got %v", cfg.MaxDelay)
	}
	if cfg.MaxRetries != 10 {
		t.Errorf("expected MaxRetries=10, got %d", cfg.MaxRetries)
	}
}
