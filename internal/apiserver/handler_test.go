// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/store"
)

type memStore struct {
	mu        sync.RWMutex
	resources map[string]memEntry
	watchers  map[string][]chan store.WatchEvent
	revisions map[string][]store.Revision
	events    map[string][]store.InstanceEvent
}

type memEntry struct {
	data    []byte
	version int64
}

type memoryAuditStore struct {
	mu      sync.Mutex
	entries []observability.AuditEntry
	filter  observability.AuditFilter
	err     error
}

type memoryCostStore struct {
	*memStore
	query  observability.CostQuery
	result *observability.CostQueryResult
	err    error
}

func (s *memoryCostStore) RecordCost(context.Context, observability.CostRecord) error { return nil }
func (s *memoryCostStore) RecordCostBatch(context.Context, []observability.CostRecord) error {
	return nil
}
func (s *memoryCostStore) QueryCosts(_ context.Context, query observability.CostQuery) (*observability.CostQueryResult, error) {
	s.query = query
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}
func (s *memoryCostStore) GetTotalCost(context.Context, string, time.Time, time.Time) (observability.CostUnit, error) {
	return 0, nil
}

func (s *memoryAuditStore) Append(_ context.Context, entry observability.AuditEntry) error {
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	return nil
}

func (s *memoryAuditStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	for _, entry := range entries {
		if err := s.Append(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *memoryAuditStore) Query(_ context.Context, filter observability.AuditFilter) ([]observability.AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filter = filter
	return append([]observability.AuditEntry(nil), s.entries...), nil
}

func newMemStore() *memStore {
	return &memStore{
		resources: make(map[string]memEntry),
		watchers:  make(map[string][]chan store.WatchEvent),
		revisions: make(map[string][]store.Revision),
		events:    make(map[string][]store.InstanceEvent),
	}
}

func TestCostAggregationRouteScopesWorkspaceAndParsesFilters(t *testing.T) {
	costs := &memoryCostStore{memStore: newMemStore(), result: &observability.CostQueryResult{Aggregations: []observability.CostAggregation{{Workspace: "team-a", TotalCostUnits: 95}}, TotalCount: 1}}
	server := newBareServer(costs, zap.NewNop())
	response := doReq(t, server, http.MethodGet, "/apis/kognition.dev/v1alpha1/namespaces/team-a/costrecords?agentIdentity=spiffe%3A%2F%2Fplanner&solution=research&solutionRevision=2&start=2026-08-21T10%3A00%3A00Z&end=2026-08-21T11%3A00%3A00Z&groupBy=agent%2Cmodel&limit=20&offset=5", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if costs.query.Workspace != "team-a" || costs.query.AgentIdentity != "spiffe://planner" || costs.query.Solution != "research" || costs.query.SolutionRev == nil || *costs.query.SolutionRev != 2 || costs.query.Start.IsZero() || costs.query.End.IsZero() || len(costs.query.GroupBy) != 2 || costs.query.GroupBy[0] != observability.GroupByAgent || costs.query.GroupBy[1] != observability.GroupByModel || costs.query.Limit != 20 || costs.query.Offset != 5 {
		t.Fatalf("cost query = %#v", costs.query)
	}
	response = doReq(t, server, http.MethodGet, "/apis/kognition.dev/v1alpha1/namespaces/team-a/costrecords?groupBy=unknown", nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid group status = %d", response.Code)
	}
}

func TestTrustDecisionRouteQueriesNormalizedAuditEntries(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	auditStore := &memoryAuditStore{}
	for _, decision := range []observability.TrustDecisionV1{
		{SchemaVersion: 1, ID: "older", Timestamp: now.Add(-time.Minute), Plane: "tool", Outcome: "allowed", ReasonCode: "authorized", Workspace: "team-a", Request: observability.TrustDecisionRequest{Operation: "invoke", TargetRef: observability.TrustDecisionResourceRef{Kind: "Tool", Name: "weather"}}, Authorization: observability.TrustDecisionAuthorization{Identity: "allowed", Role: "allowed", Grant: "allowed", Policy: "allowed"}, Execution: observability.TrustDecisionExecution{Status: "not_started"}},
		{SchemaVersion: 1, ID: "newer", Timestamp: now, Plane: "tool", Outcome: "allowed", ReasonCode: "authorized", Workspace: "team-a", Request: observability.TrustDecisionRequest{Operation: "invoke", TargetRef: observability.TrustDecisionResourceRef{Kind: "Tool", Name: "weather"}}, Authorization: observability.TrustDecisionAuthorization{Identity: "allowed", Role: "allowed", Grant: "allowed", Policy: "allowed"}, Execution: observability.TrustDecisionExecution{Status: "not_started"}},
	} {
		details, err := json.Marshal(decision)
		if err != nil {
			t.Fatal(err)
		}
		auditStore.entries = append(auditStore.entries, observability.AuditEntry{ID: decision.ID, Timestamp: decision.Timestamp, Workspace: decision.Workspace, AgentIdentity: "agent-a", SessionID: "session-a", EventType: observability.AuditToolCall, Details: details})
	}
	server := newBareServer(newMemStore(), zap.NewNop(), WithAuditLog(observability.NewCognitiveAuditLog(auditStore)))
	response := doReq(t, server, http.MethodGet, "/apis/kognition.dev/v1alpha1/namespaces/team-a/trust-decisions?agentIdentity=agent-a&sessionId=session-a&plane=tool&outcome=allowed&limit=1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var result observability.TrustDecisionQueryResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.APIVersion != observability.TrustDecisionAPIVersion || len(result.Items) != 1 || result.Items[0].ID != "newer" || !result.Truncated {
		t.Fatalf("result = %#v", result)
	}
	if auditStore.filter.Workspace != "team-a" || auditStore.filter.AgentIdentity != "agent-a" || auditStore.filter.SessionID != "session-a" || auditStore.filter.Plane != "tool" || auditStore.filter.Outcome != "allowed" || auditStore.filter.Limit != 2 || !auditStore.filter.Descending {
		t.Fatalf("audit filter = %#v", auditStore.filter)
	}
	invalid := doReq(t, server, http.MethodGet, "/apis/kognition.dev/v1alpha1/namespaces/team-a/trust-decisions?limit=501", nil)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d: %s", invalid.Code, invalid.Body.String())
	}
}

func storeKey(kind, ns, name string) string {
	return kind + "/" + ns + "/" + name
}

func (m *memStore) Ping(_ context.Context) error { return nil }

func (m *memStore) Get(_ context.Context, kind, ns, name string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.resources[storeKey(kind, ns, name)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return e.data, nil
}

func (m *memStore) List(_ context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	prefix := kind + "/"
	if opts.Namespace != "" {
		prefix = kind + "/" + opts.Namespace + "/"
	}
	var out [][]byte
	for k, e := range m.resources {
		if strings.HasPrefix(k, prefix) {
			out = append(out, e.data)
		}
	}
	// Apply offset-based pagination to match store contract (limit+1 pattern).
	if opts.Limit > 0 {
		offset := 0
		if opts.Continue != "" {
			if raw, err := base64.RawURLEncoding.DecodeString(opts.Continue); err == nil {
				offset, _ = strconv.Atoi(string(raw))
			}
		}
		if offset >= len(out) {
			return nil, nil
		}
		end := offset + opts.Limit + 1
		if end > len(out) {
			end = len(out)
		}
		out = out[offset:end]
	}
	return out, nil
}

func (m *memStore) Create(_ context.Context, kind, ns, name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := storeKey(kind, ns, name)
	if _, ok := m.resources[key]; ok {
		return store.ErrAlreadyExists
	}
	m.resources[key] = memEntry{data: data, version: 1}
	m.notifyLocked(store.WatchEvent{
		Type:            store.EventCreated,
		Kind:            kind,
		Namespace:       ns,
		Name:            name,
		ResourceVersion: 1,
		Object:          data,
	})
	return nil
}

func (m *memStore) Update(_ context.Context, kind, ns, name string, data []byte, rv int64) error {
	_, err := m.UpdateAndGet(context.Background(), kind, ns, name, data, rv)
	return err
}

func (m *memStore) UpdateAndGet(_ context.Context, kind, ns, name string, data []byte, rv int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := storeKey(kind, ns, name)
	e, ok := m.resources[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	if e.version != rv {
		return nil, store.ErrConflict
	}
	newVersion := e.version + 1

	// Patch resourceVersion in the stored JSON data (mirrors Postgres jsonb_set behavior).
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err == nil {
		if meta, ok := obj["metadata"].(map[string]any); ok {
			meta["resourceVersion"] = float64(newVersion)
			obj["metadata"] = meta
			if patched, err := json.Marshal(obj); err == nil {
				data = patched
			}
		}
	}

	m.resources[key] = memEntry{data: data, version: newVersion}
	m.notifyLocked(store.WatchEvent{
		Type:            store.EventUpdated,
		Kind:            kind,
		Namespace:       ns,
		Name:            name,
		ResourceVersion: newVersion,
		Object:          data,
	})
	return data, nil
}

type interleavingUpdateStore struct {
	*memStore
}

func (s *interleavingUpdateStore) UpdateAndGet(ctx context.Context, kind, ns, name string, data []byte, rv int64) ([]byte, error) {
	firstWrite, err := s.memStore.UpdateAndGet(ctx, kind, ns, name, data, rv)
	if err != nil {
		return nil, err
	}

	var later map[string]any
	if err := json.Unmarshal(firstWrite, &later); err != nil {
		return nil, err
	}
	later["spec"] = map[string]any{"model": "later-writer-model"}
	laterData, err := json.Marshal(later)
	if err != nil {
		return nil, err
	}
	if _, err := s.memStore.UpdateAndGet(ctx, kind, ns, name, laterData, rv+1); err != nil {
		return nil, err
	}
	return firstWrite, nil
}

type scriptedWatchStore struct {
	*memStore
	events []store.WatchEvent
}

func (s *scriptedWatchStore) Watch(context.Context, string, store.ListOptions) (<-chan store.WatchEvent, error) {
	ch := make(chan store.WatchEvent, len(s.events))
	for _, event := range s.events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

type deleteConflictStore struct {
	*memStore
	conflicted bool
}

func (s *deleteConflictStore) UpdateAndGet(ctx context.Context, kind, ns, name string, data []byte, rv int64) ([]byte, error) {
	if !s.conflicted {
		s.conflicted = true

		current, err := s.memStore.Get(ctx, kind, ns, name)
		if err != nil {
			return nil, err
		}
		var concurrent map[string]any
		if err := json.Unmarshal(current, &concurrent); err != nil {
			return nil, err
		}
		meta := concurrent["metadata"].(map[string]any)
		meta["annotations"] = map[string]any{"updated-by": "controller"}
		concurrentData, err := json.Marshal(concurrent)
		if err != nil {
			return nil, err
		}
		if _, err := s.memStore.UpdateAndGet(ctx, kind, ns, name, concurrentData, rv); err != nil {
			return nil, err
		}
		return nil, store.ErrConflict
	}

	return s.memStore.UpdateAndGet(ctx, kind, ns, name, data, rv)
}

type persistentDeleteConflictStore struct {
	*memStore
	attempts int
}

func (s *persistentDeleteConflictStore) UpdateAndGet(context.Context, string, string, string, []byte, int64) ([]byte, error) {
	s.attempts++
	return nil, store.ErrConflict
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("request body read failed")
}

type deleteConflictRefreshStore struct {
	*memStore
	refreshData []byte
	refreshErr  error
	getCalls    int
}

func (s *deleteConflictRefreshStore) Get(ctx context.Context, kind, ns, name string) ([]byte, error) {
	s.getCalls++
	if s.getCalls > 1 {
		if s.refreshErr != nil {
			return nil, s.refreshErr
		}
		return s.refreshData, nil
	}
	return s.memStore.Get(ctx, kind, ns, name)
}

func (*deleteConflictRefreshStore) UpdateAndGet(context.Context, string, string, string, []byte, int64) ([]byte, error) {
	return nil, store.ErrConflict
}

type approvalConflictStore struct {
	*memStore
	conflicted bool
}

func (s *approvalConflictStore) Update(ctx context.Context, kind, ns, name string, data []byte, rv int64) error {
	if kind == "WorkflowRun" && !s.conflicted {
		s.conflicted = true

		current, err := s.memStore.Get(ctx, kind, ns, name)
		if err != nil {
			return err
		}
		var concurrent map[string]any
		if err := json.Unmarshal(current, &concurrent); err != nil {
			return err
		}
		meta := concurrent["metadata"].(map[string]any)
		annotations, _ := meta["annotations"].(map[string]any)
		if annotations == nil {
			annotations = map[string]any{}
			meta["annotations"] = annotations
		}
		annotations["kognition.dev/approvers.execute"] = "bob"
		concurrent["status"] = map[string]any{
			"approvalRecords": []any{map[string]any{
				"step": "execute", "decision": "approve", "approver": "bob",
			}},
		}
		concurrentData, err := json.Marshal(concurrent)
		if err != nil {
			return err
		}
		if _, err := s.memStore.UpdateAndGet(ctx, kind, ns, name, concurrentData, rv); err != nil {
			return err
		}
		return store.ErrConflict
	}

	return s.memStore.Update(ctx, kind, ns, name, data, rv)
}

type workflowGetErrorStore struct {
	*memStore
	err error
}

func (s *workflowGetErrorStore) Get(ctx context.Context, kind, ns, name string) ([]byte, error) {
	if kind == "Workflow" {
		return nil, s.err
	}
	return s.memStore.Get(ctx, kind, ns, name)
}

func (m *memStore) Delete(_ context.Context, kind, ns, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := storeKey(kind, ns, name)
	e, ok := m.resources[key]
	if !ok {
		return store.ErrNotFound
	}
	delete(m.resources, key)
	m.notifyLocked(store.WatchEvent{
		Type:      store.EventDeleted,
		Kind:      kind,
		Namespace: ns,
		Name:      name,
		Object:    e.data,
	})
	return nil
}

func (m *memStore) CreateControlPlaneRoleBinding(_ context.Context, ns, name, roleName string, data []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.resources[storeKey("ControlPlaneRole", ns, roleName)]; !ok {
		return nil, store.ErrNotFound
	}
	key := storeKey("ControlPlaneRoleBinding", ns, name)
	if _, ok := m.resources[key]; ok {
		return nil, store.ErrAlreadyExists
	}
	m.resources[key] = memEntry{data: data, version: 1}
	return data, nil
}

func (m *memStore) UpdateControlPlaneRoleBinding(_ context.Context, ns, name, roleName string, data []byte, rv int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.resources[storeKey("ControlPlaneRole", ns, roleName)]; !ok {
		return nil, store.ErrNotFound
	}
	key := storeKey("ControlPlaneRoleBinding", ns, name)
	entry, ok := m.resources[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	if entry.version != rv {
		return nil, store.ErrConflict
	}
	entry.version++
	var object map[string]any
	if err := json.Unmarshal(data, &object); err == nil {
		if metadata, ok := object["metadata"].(map[string]any); ok {
			metadata["resourceVersion"] = float64(entry.version)
			if patched, err := json.Marshal(object); err == nil {
				data = patched
			}
		}
	}
	entry.data = data
	m.resources[key] = entry
	m.notifyLocked(store.WatchEvent{
		Type:            store.EventUpdated,
		Kind:            "ControlPlaneRoleBinding",
		Namespace:       ns,
		Name:            name,
		ResourceVersion: entry.version,
		Object:          data,
	})
	return data, nil
}

func (m *memStore) DeleteControlPlaneRole(_ context.Context, ns, name string, rv int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := storeKey("ControlPlaneRole", ns, name)
	role, ok := m.resources[key]
	if !ok {
		return store.ErrNotFound
	}
	if role.version != rv {
		return store.ErrConflict
	}
	for resourceKey, entry := range m.resources {
		if !strings.HasPrefix(resourceKey, storeKey("ControlPlaneRoleBinding", ns, "")) {
			continue
		}
		var binding v1alpha1.ControlPlaneRoleBinding
		if json.Unmarshal(entry.data, &binding) == nil && binding.Spec.RoleRef.Name == name {
			return store.ErrConflict
		}
	}
	delete(m.resources, key)
	return nil
}

func (m *memStore) DeleteControlPlaneRoleBinding(_ context.Context, ns, name string, rv int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := storeKey("ControlPlaneRoleBinding", ns, name)
	binding, ok := m.resources[key]
	if !ok {
		return store.ErrNotFound
	}
	if binding.version != rv {
		return store.ErrConflict
	}
	delete(m.resources, key)
	return nil
}

func (m *memStore) Watch(_ context.Context, kind string, _ store.ListOptions) (<-chan store.WatchEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan store.WatchEvent, 64)
	m.watchers[kind] = append(m.watchers[kind], ch)
	return ch, nil
}

func (m *memStore) notifyLocked(evt store.WatchEvent) {
	for _, ch := range m.watchers[evt.Kind] {
		select {
		case ch <- evt:
		default:
		}
	}
}

func (m *memStore) CreateRevision(_ context.Context, kind, ns, name string, revision int64, spec, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := storeKey(kind, ns, name)
	m.revisions[key] = append(m.revisions[key], store.Revision{
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
	return m.revisions[storeKey(kind, ns, name)], nil
}

func (m *memStore) GetRevision(_ context.Context, kind, ns, name string, revision int64) (*store.Revision, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.revisions[storeKey(kind, ns, name)] {
		if r.Revision == revision {
			return &r, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *memStore) AppendEvent(_ context.Context, kind, ns, name string, event store.InstanceEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := storeKey(kind, ns, name)
	m.events[key] = append(m.events[key], event)
	return nil
}

func (m *memStore) ListEvents(_ context.Context, kind, ns, name string) ([]store.InstanceEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.events[storeKey(kind, ns, name)], nil
}

// newTestServer creates a test server backed by the in-memory store.
func newTestServer(t *testing.T) (*Server, *memStore) {
	t.Helper()
	ms := newMemStore()
	logger, _ := zap.NewDevelopment()
	srv := newBareServer(ms, logger)
	return srv, ms
}

func newBareServer(st APIServerStore, logger *zap.Logger, opts ...Option) *Server {
	opts = append([]Option{withAdmissionReplacement(admission.NewChain(nil, nil))}, opts...)
	return New(Config{Addr: ":0"}, st, logger, opts...)
}

func newProductionTestServer(t *testing.T) (*Server, *memStore) {
	t.Helper()
	ms := newMemStore()
	logger, _ := zap.NewDevelopment()
	return New(Config{Addr: ":0"}, ms, logger), ms
}

func doReq(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		b = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, b)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

func doReqRaw(t *testing.T, srv *Server, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var b io.Reader
	if body != nil {
		b = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, b)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode JSON: %v (body: %s)", err, rec.Body.String())
	}
	return out
}

func makeWatchAgentResource(name string, version int64) []byte {
	data, _ := json.Marshal(map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Agent",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       "default",
			"resourceVersion": version,
		},
		"spec": map[string]any{"model": "gpt-4"},
	})
	return data
}

// TC-0001: Full CRUD lifecycle — create, get, update (resourceVersion increments), delete, 404.
func TestCRUDLifecycle(t *testing.T) {
	srv, _ := newTestServer(t)

	// 1. Create
	agent := map[string]any{
		"metadata": map[string]any{"name": "test-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	created := decodeJSON(t, rec)
	meta := created["metadata"].(map[string]any)
	if meta["uid"] == "" {
		t.Fatal("create: uid not assigned")
	}
	if meta["resourceVersion"].(float64) != 1 {
		t.Fatalf("create: want resourceVersion 1, got %v", meta["resourceVersion"])
	}
	if meta["namespace"] != "default" {
		t.Fatalf("create: want namespace 'default', got %v", meta["namespace"])
	}
	if created["kind"] != "Agent" {
		t.Fatalf("create: want kind 'Agent', got %v", created["kind"])
	}

	// 2. Get
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/test-agent", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	gotSpec := got["spec"].(map[string]any)
	if gotSpec["model"] != "gpt-4" {
		t.Fatalf("get: spec mismatch: %v", gotSpec)
	}

	// 3. Update
	created["spec"] = map[string]any{"model": "claude-4"}
	rec = doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/test-agent", created)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	updated := decodeJSON(t, rec)
	updatedMeta := updated["metadata"].(map[string]any)
	if updatedMeta["resourceVersion"].(float64) != 2 {
		t.Fatalf("update: want resourceVersion 2, got %v", updatedMeta["resourceVersion"])
	}

	// Verify the update persisted.
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/test-agent", nil)
	got = decodeJSON(t, rec)
	if got["spec"].(map[string]any)["model"] != "claude-4" {
		t.Fatalf("update: spec not persisted")
	}

	// 4. Delete
	rec = doReq(t, srv, "DELETE", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/test-agent", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d", rec.Code)
	}

	// 5. Verify 404 after delete
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/test-agent", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: want 404, got %d", rec.Code)
	}
}

func TestAgentCRUDWritesDurableAuditEntries(t *testing.T) {
	memory := newMemStore()
	auditStore := &memoryAuditStore{}
	server := newBareServer(memory, zap.NewNop(), WithAuditLog(observability.NewCognitiveAuditLog(auditStore)))
	path := "/apis/kognition.dev/v1alpha1/namespaces/default/agents"

	created := doReq(t, server, http.MethodPost, path, map[string]any{
		"metadata": map[string]any{"name": "audited-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", created.Code, created.Body.String())
	}
	updatedAgent := decodeJSON(t, created)
	updatedAgent["spec"] = map[string]any{"model": "gpt-4.1"}
	updated := doReq(t, server, http.MethodPut, path+"/audited-agent", updatedAgent)
	if updated.Code != http.StatusOK {
		t.Fatalf("update status = %d: %s", updated.Code, updated.Body.String())
	}
	deleted := doReq(t, server, http.MethodDelete, path+"/audited-agent", nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d: %s", deleted.Code, deleted.Body.String())
	}

	entries, err := auditStore.Query(context.Background(), observability.AuditFilter{})
	if err != nil {
		t.Fatalf("query audit entries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("audit entries = %#v, want create/update/delete", entries)
	}
	for index, verb := range []string{"create", "update", "delete"} {
		entry := entries[index]
		if entry.Workspace != "default" || entry.EventType != observability.AuditSpecChange || entry.Verb != verb || entry.SubjectKind != "Agent" || entry.SubjectRef != "default/audited-agent" {
			t.Fatalf("audit entry %d = %#v, want Agent %s", index, entry, verb)
		}
	}
}

func TestResourceAuditAttributesVerifiedCaller(t *testing.T) {
	auditStore := &memoryAuditStore{}
	server := newBareServer(newMemStore(), zap.NewNop(), WithAuditLog(observability.NewCognitiveAuditLog(auditStore)))
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{
		Subject:  "caller-id",
		Username: "caller@example.com",
		Method:   "oidc",
	})

	if err := server.recordResourceAudit(ctx, "ToolGrant", "team-a", "weather-access", "create"); err != nil {
		t.Fatalf("record resource audit: %v", err)
	}
	if len(auditStore.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(auditStore.entries))
	}
	entry := auditStore.entries[0]
	if entry.AgentIdentity != "caller-id" {
		t.Fatalf("audit actor = %q, want verified subject %q", entry.AgentIdentity, "caller-id")
	}
	if entry.SubjectRef != "team-a/weather-access" {
		t.Fatalf("audit subject = %q, want mutated resource", entry.SubjectRef)
	}
}

func TestAgentCreateFailsBeforeMutationWhenAuditWriteFails(t *testing.T) {
	memory := newMemStore()
	server := newBareServer(memory, zap.NewNop(), WithAuditLog(observability.NewCognitiveAuditLog(&memoryAuditStore{err: errors.New("audit unavailable")})))
	response := doReq(t, server, http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/agents", map[string]any{
		"metadata": map[string]any{"name": "blocked-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	})
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("create status = %d, want 500: %s", response.Code, response.Body.String())
	}
	if _, err := memory.Get(context.Background(), "Agent", "default", "blocked-agent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Agent was created after an audit failure: %v", err)
	}
}

func TestClusterScopedResourceIgnoresNamespacePath(t *testing.T) {
	memory := newMemStore()
	auditStore := &memoryAuditStore{}
	srv := newBareServer(memory, zap.NewNop(), WithAuditLog(observability.NewCognitiveAuditLog(auditStore)))
	resource := map[string]any{
		"metadata": map[string]any{"name": "shared-class", "namespace": "team-a"},
		"spec":     map[string]any{"provisioner": "foundry"},
	}

	created := doReq(t, srv, http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/team-a/modelclasses", resource)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", created.Code, created.Body.String())
	}
	stored, err := memory.Get(context.Background(), "ModelClass", "", "shared-class")
	if err != nil {
		t.Fatalf("cluster-scoped storage get: %v", err)
	}
	var storedObject map[string]any
	if err := json.Unmarshal(stored, &storedObject); err != nil {
		t.Fatalf("unmarshal stored class: %v", err)
	}
	storedMeta, _ := storedObject["metadata"].(map[string]any)
	if got := storedMeta["namespace"]; got != "" {
		t.Fatalf("stored namespace = %v, want empty", got)
	}

	got := doReq(t, srv, http.MethodGet, "/apis/kognition.dev/v1alpha1/namespaces/team-b/modelclasses/shared-class", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("cross-namespace get status = %d: %s", got.Code, got.Body.String())
	}
	listed := doReq(t, srv, http.MethodGet, "/apis/kognition.dev/v1alpha1/namespaces/team-b/modelclasses", nil)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), "shared-class") {
		t.Fatalf("cross-namespace list status = %d: %s", listed.Code, listed.Body.String())
	}

	current := decodeJSON(t, got)
	current["spec"] = map[string]any{"provisioner": "foundry", "latencyClass": "low"}
	updated := doReq(t, srv, http.MethodPut, "/apis/kognition.dev/v1alpha1/namespaces/team-b/modelclasses/shared-class", current)
	if updated.Code != http.StatusOK {
		t.Fatalf("cross-namespace update status = %d: %s", updated.Code, updated.Body.String())
	}
	deleted := doReq(t, srv, http.MethodDelete, "/apis/kognition.dev/v1alpha1/namespaces/team-b/modelclasses/shared-class", nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("cross-namespace delete status = %d: %s", deleted.Code, deleted.Body.String())
	}
	if len(auditStore.entries) != 3 {
		t.Fatalf("audit entries = %d, want create/update/delete", len(auditStore.entries))
	}
	for index, verb := range []string{"create", "update", "delete"} {
		entry := auditStore.entries[index]
		if entry.Workspace != "cluster" || entry.EventType != observability.AuditSpecChange || entry.Verb != verb || entry.SubjectKind != "ModelClass" || entry.SubjectRef != "/shared-class" {
			t.Fatalf("audit entry %d = %#v, want cluster-scoped ModelClass %s", index, entry, verb)
		}
	}
}

// TC-0002: Watch delivers ADDED event within 2s of a create.
func TestWatchDeliversEvent(t *testing.T) {
	srv, _ := newTestServer(t)

	// Start a watch in a goroutine.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/apis/kognition.dev/v1alpha1/watch/namespaces/default/agents", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	var watchBody string
	done := make(chan struct{})
	go func() {
		srv.mux.ServeHTTP(rec, req)
		watchBody = rec.Body.String()
		close(done)
	}()

	// Give watch time to subscribe.
	time.Sleep(50 * time.Millisecond)

	// Create a resource.
	agent := map[string]any{
		"metadata": map[string]any{"name": "watch-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	createRec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", createRec.Code)
	}

	// Wait a bit for the event to be written, then cancel.
	time.Sleep(200 * time.Millisecond)
	cancel()
	waitForWatchCompletion(t, done)

	if !strings.Contains(watchBody, "ADDED") {
		t.Fatalf("watch: expected ADDED event, got: %s", watchBody)
	}
	if !strings.Contains(watchBody, "watch-agent") {
		t.Fatalf("watch: expected resource name in event, got: %s", watchBody)
	}
}

// TC-0004: Concurrent update with stale resourceVersion returns 409 Conflict.
func TestOptimisticConcurrencyConflict(t *testing.T) {
	srv, _ := newTestServer(t)

	// Create a resource.
	agent := map[string]any{
		"metadata": map[string]any{"name": "conflict-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}
	created := decodeJSON(t, rec)

	// First update succeeds (resourceVersion=1 → 2).
	created["spec"] = map[string]any{"model": "claude-4"}
	rec = doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/conflict-agent", created)
	if rec.Code != http.StatusOK {
		t.Fatalf("first update: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Second update with stale resourceVersion=1 should fail with 409.
	stale := map[string]any{
		"metadata": map[string]any{
			"name":            "conflict-agent",
			"resourceVersion": float64(1), // stale!
		},
		"spec": map[string]any{"model": "gemini"},
	}
	rec = doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/conflict-agent", stale)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale update: want 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify error body.
	errBody := decodeJSON(t, rec)
	if errBody["code"].(float64) != 409 {
		t.Fatalf("error code: want 409, got %v", errBody["code"])
	}
}

// TestCreateDuplicate verifies that creating the same resource twice returns 409.
func TestCreateDuplicate(t *testing.T) {
	srv, _ := newTestServer(t)

	agent := map[string]any{
		"metadata": map[string]any{"name": "dup-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}

	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: want 201, got %d", rec.Code)
	}

	rec = doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDeleteNotFound verifies deleting a non-existent resource returns 404.
func TestDeleteNotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := doReq(t, srv, "DELETE", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/no-such-agent", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing: want 404, got %d", rec.Code)
	}
}

// TestListEmpty verifies that listing an empty namespace returns an empty items array.
func TestListEmpty(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", rec.Code)
	}
	body := decodeJSON(t, rec)
	items := body["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("list: want 0 items, got %d", len(items))
	}
	if body["kind"] != "AgentList" {
		t.Fatalf("list: want kind 'AgentList', got %v", body["kind"])
	}
}

// TestHealthEndpoints verifies /healthz and /readyz return 200.
func TestHealthEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := doReq(t, srv, "GET", path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: want 200, got %d", path, rec.Code)
		}
	}
}

// TestGetNotFound verifies that GET for a non-existent resource returns 404.
func TestGetNotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/no-such-agent", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get missing: want 404, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["kind"] != "Status" {
		t.Fatalf("error kind: want 'Status', got %v", body["kind"])
	}
	if body["code"].(float64) != 404 {
		t.Fatalf("error code: want 404, got %v", body["code"])
	}
}

// TestUpdateNotFound verifies that PUT on a non-existent resource returns 404.
func TestUpdateNotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	obj := map[string]any{
		"metadata": map[string]any{
			"name":            "ghost-agent",
			"resourceVersion": float64(1),
		},
		"spec": map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/ghost-agent", obj)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update missing: want 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateInvalidJSON verifies that a malformed JSON body returns 400.
func TestCreateInvalidJSON(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest("POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", strings.NewReader("{bad json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON create: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateMissingName verifies that creating without metadata.name returns 400.
func TestCreateMissingName(t *testing.T) {
	srv, _ := newTestServer(t)

	obj := map[string]any{
		"metadata": map[string]any{},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", obj)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing name: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if !strings.Contains(body["message"].(string), "metadata.name") {
		t.Fatalf("error message should mention metadata.name, got: %v", body["message"])
	}
}

// TestCreateEmptyBody verifies that an empty POST body returns 400.
func TestCreateEmptyBody(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest("POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateNoMetadataBlock verifies that creating without a metadata object returns 400.
func TestCreateNoMetadataBlock(t *testing.T) {
	srv, _ := newTestServer(t)

	obj := map[string]any{
		"spec": map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", obj)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no metadata: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUpdateMissingMetadata verifies that PUT without a metadata block returns 400.
func TestUpdateMissingMetadata(t *testing.T) {
	srv, _ := newTestServer(t)

	obj := map[string]any{
		"spec": map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/some-agent", obj)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing metadata: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUpdateMissingResourceVersion verifies that PUT without resourceVersion returns 400.
func TestUpdateMissingResourceVersion(t *testing.T) {
	srv, _ := newTestServer(t)

	// First create the resource.
	agent := map[string]any{
		"metadata": map[string]any{"name": "rv-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup create: want 201, got %d", rec.Code)
	}

	// Update without resourceVersion.
	update := map[string]any{
		"metadata": map[string]any{"name": "rv-agent"},
		"spec":     map[string]any{"model": "claude-4"},
	}
	rec = doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/rv-agent", update)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing rv: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if !strings.Contains(body["message"].(string), "resourceVersion") {
		t.Fatalf("error should mention resourceVersion, got: %v", body["message"])
	}
}

// TestUpdateInvalidResourceVersion verifies that PUT with rv=0 or non-numeric rv returns 400.
func TestUpdateInvalidResourceVersion(t *testing.T) {
	srv, _ := newTestServer(t)

	tests := []struct {
		name string
		rv   any
	}{
		{"zero", float64(0)},
		{"negative", float64(-1)},
		{"string", "abc"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obj := map[string]any{
				"metadata": map[string]any{
					"name":            "test-agent",
					"resourceVersion": tc.rv,
				},
				"spec": map[string]any{"model": "gpt-4"},
			}
			rec := doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/test-agent", obj)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("rv=%v: want 400, got %d: %s", tc.rv, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestUpdateInvalidJSON verifies that a malformed JSON body on PUT returns 400.
func TestUpdateInvalidJSON(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest("PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/some-agent", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON update: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- Admission integration helpers ---

// testValidatingHook is a validating hook for handler integration tests.
type testValidatingHook struct {
	name     string
	kind     string
	validate func(ctx context.Context, req *admission.Request) *admission.Response
}

func (h *testValidatingHook) Name() string { return h.name }
func (h *testValidatingHook) Handles(kind string, op admission.Operation) bool {
	return kind == h.kind
}
func (h *testValidatingHook) Validate(ctx context.Context, req *admission.Request) *admission.Response {
	return h.validate(ctx, req)
}

// testMutatingHook is a mutating hook for handler integration tests.
type testMutatingHook struct {
	name   string
	kind   string
	mutate func(ctx context.Context, req *admission.Request) *admission.Response
}

func (h *testMutatingHook) Name() string { return h.name }
func (h *testMutatingHook) Handles(kind string, op admission.Operation) bool {
	return kind == h.kind
}
func (h *testMutatingHook) Mutate(ctx context.Context, req *admission.Request) *admission.Response {
	return h.mutate(ctx, req)
}

func newTestServerWithAdmission(t *testing.T, chain *admission.Chain) (*Server, *memStore) {
	t.Helper()
	ms := newMemStore()
	logger, _ := zap.NewDevelopment()
	srv := New(Config{Addr: ":0"}, ms, logger, withAdmissionReplacement(chain))
	return srv, ms
}

// TC-0008: Register validating hook that rejects Agents without modelRef; verify 422 rejection.
func TestAdmission_ValidatingRejectsCreate(t *testing.T) {
	hook := &testValidatingHook{
		name: "require-model-ref",
		kind: "Agent",
		validate: func(_ context.Context, req *admission.Request) *admission.Response {
			var obj map[string]any
			json.Unmarshal(req.Object, &obj)
			spec, _ := obj["spec"].(map[string]any)
			if spec == nil || spec["modelRef"] == nil || spec["modelRef"] == "" {
				return &admission.Response{Allowed: false, Reason: "spec.modelRef is required"}
			}
			return &admission.Response{Allowed: true}
		},
	}

	chain := admission.NewChain(nil, []admission.ValidatingHook{hook})
	srv, _ := newTestServerWithAdmission(t, chain)

	// Agent without modelRef → 422
	agent := map[string]any{
		"metadata": map[string]any{"name": "bad-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if !strings.Contains(body["message"].(string), "spec.modelRef is required") {
		t.Fatalf("error should mention modelRef, got: %v", body["message"])
	}

	// Agent with modelRef → 201
	good := map[string]any{
		"metadata": map[string]any{"name": "good-agent"},
		"spec":     map[string]any{"modelRef": "models/gpt-4"},
	}
	rec = doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", good)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TC-0009: Register mutating hook that injects default timeoutSeconds; verify persisted spec has it.
func TestAdmission_MutatingInjectsDefault(t *testing.T) {
	hook := &testMutatingHook{
		name: "default-timeout",
		kind: "Agent",
		mutate: func(_ context.Context, req *admission.Request) *admission.Response {
			var obj map[string]any
			json.Unmarshal(req.Object, &obj)
			spec, _ := obj["spec"].(map[string]any)
			if spec == nil {
				spec = make(map[string]any)
			}
			if _, ok := spec["timeoutSeconds"]; !ok {
				spec["timeoutSeconds"] = float64(30)
				obj["spec"] = spec
				patched, _ := json.Marshal(obj)
				return &admission.Response{Allowed: true, Patch: patched}
			}
			return &admission.Response{Allowed: true}
		},
	}

	chain := admission.NewChain([]admission.MutatingHook{hook}, nil)
	srv, _ := newTestServerWithAdmission(t, chain)

	agent := map[string]any{
		"metadata": map[string]any{"name": "timeout-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET the resource and verify timeoutSeconds was injected.
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/timeout-agent", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: want 200, got %d", rec.Code)
	}
	got := decodeJSON(t, rec)
	spec := got["spec"].(map[string]any)
	if spec["timeoutSeconds"] != float64(30) {
		t.Fatalf("want timeoutSeconds=30, got %v", spec["timeoutSeconds"])
	}
	// Original field should still be there.
	if spec["model"] != "gpt-4" {
		t.Fatalf("model field lost after mutation")
	}
}

// TC-0010: Chain mutating + validating hooks end-to-end through the HTTP API.
func TestAdmission_MutatingThenValidatingEndToEnd(t *testing.T) {
	mutator := &testMutatingHook{
		name: "default-timeout",
		kind: "Agent",
		mutate: func(_ context.Context, req *admission.Request) *admission.Response {
			var obj map[string]any
			json.Unmarshal(req.Object, &obj)
			spec, _ := obj["spec"].(map[string]any)
			if spec == nil {
				spec = make(map[string]any)
			}
			if _, ok := spec["timeoutSeconds"]; !ok {
				spec["timeoutSeconds"] = float64(30)
				obj["spec"] = spec
				patched, _ := json.Marshal(obj)
				return &admission.Response{Allowed: true, Patch: patched}
			}
			return &admission.Response{Allowed: true}
		},
	}

	validator := &testValidatingHook{
		name: "require-model-ref",
		kind: "Agent",
		validate: func(_ context.Context, req *admission.Request) *admission.Response {
			var obj map[string]any
			json.Unmarshal(req.Object, &obj)
			spec, _ := obj["spec"].(map[string]any)
			if spec == nil || spec["modelRef"] == nil || spec["modelRef"] == "" {
				return &admission.Response{Allowed: false, Reason: "spec.modelRef is required"}
			}
			return &admission.Response{Allowed: true}
		},
	}

	chain := admission.NewChain(
		[]admission.MutatingHook{mutator},
		[]admission.ValidatingHook{validator},
	)
	srv, _ := newTestServerWithAdmission(t, chain)

	// Case 1: Agent with modelRef → mutator injects timeout, validator passes → 201
	agent := map[string]any{
		"metadata": map[string]any{"name": "chain-agent"},
		"spec":     map[string]any{"modelRef": "models/gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("case 1 create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify mutation persisted.
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/chain-agent", nil)
	got := decodeJSON(t, rec)
	spec := got["spec"].(map[string]any)
	if spec["timeoutSeconds"] != float64(30) {
		t.Fatalf("case 1: want timeoutSeconds=30, got %v", spec["timeoutSeconds"])
	}
	if spec["modelRef"] != "models/gpt-4" {
		t.Fatalf("case 1: modelRef lost")
	}

	// Case 2: Agent without modelRef → mutator injects timeout, validator rejects → 422
	bad := map[string]any{
		"metadata": map[string]any{"name": "bad-chain-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec = doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", bad)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("case 2: want 422, got %d: %s", rec.Code, rec.Body.String())
	}

	// Case 3: Update path also runs admission.
	// First create a valid agent without chain (need to update the existing one).
	created := decodeJSON(t, doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/chain-agent", nil))
	created["spec"] = map[string]any{"modelRef": "models/claude-4"}
	rec = doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/chain-agent", created)
	if rec.Code != http.StatusOK {
		t.Fatalf("case 3 update: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify update mutation persisted.
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/chain-agent", nil)
	got = decodeJSON(t, rec)
	spec = got["spec"].(map[string]any)
	if spec["timeoutSeconds"] != float64(30) {
		t.Fatalf("case 3: want timeoutSeconds=30 after update, got %v", spec["timeoutSeconds"])
	}
	if spec["modelRef"] != "models/claude-4" {
		t.Fatalf("case 3: want modelRef=models/claude-4, got %v", spec["modelRef"])
	}
}

func TestDefaultAdmission_RejectsInvalidSecretRefCreate(t *testing.T) {
	srv, _ := newProductionTestServer(t)

	secretRef := map[string]any{
		"metadata": map[string]any{"name": "bad-secretref"},
		"spec": map[string]any{
			"backendType": "internal",
			"backendRef":  "secret/app",
		},
	}

	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/secretrefs", secretRef)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if !strings.Contains(body["message"].(string), "secretRef.spec.key is required") {
		t.Fatalf("expected key validation error, got %v", body["message"])
	}
}

func TestDefaultAdmission_RejectsUnsupportedSecretRefBackend(t *testing.T) {
	srv, _ := newProductionTestServer(t)

	secretRef := map[string]any{
		"metadata": map[string]any{"name": "vault-secretref"},
		"spec": map[string]any{
			"backendType": "vault",
			"backendRef":  "kv/team/app",
			"key":         "token",
		},
	}

	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/secretrefs", secretRef)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if !strings.Contains(body["message"].(string), `secretRef.spec.backendType "vault" is not supported`) {
		t.Fatalf("expected unsupported backend error, got %v", body["message"])
	}
}

func TestDefaultAdmission_AcceptsAndUpdatesValidSecretRef(t *testing.T) {
	srv, _ := newProductionTestServer(t)

	secretRef := map[string]any{
		"metadata": map[string]any{"name": "good-secretref"},
		"spec": map[string]any{
			"provider":  "env",
			"path":      "OPENAI_API_KEY",
			"secretKey": "token",
		},
	}

	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/secretrefs", secretRef)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	created := decodeJSON(t, rec)

	spec := created["spec"].(map[string]any)
	spec["backendType"] = "env"
	spec["backendRef"] = "OPENAI_API_KEY"
	spec["key"] = "token"
	delete(spec, "provider")
	delete(spec, "path")
	delete(spec, "secretKey")
	created["spec"] = spec

	rec = doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/secretrefs/good-secretref", created)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	updated := decodeJSON(t, rec)
	updatedSpec := updated["spec"].(map[string]any)
	if updatedSpec["backendType"] != "env" || updatedSpec["backendRef"] != "OPENAI_API_KEY" || updatedSpec["key"] != "token" {
		t.Fatalf("updated secretRef spec = %+v, want env backend persisted", updatedSpec)
	}
}

func TestWithAdmission_PreservesDefaultSecretRefValidation(t *testing.T) {
	hook := &testValidatingHook{
		name: "require-model-ref",
		kind: "Agent",
		validate: func(_ context.Context, req *admission.Request) *admission.Response {
			var obj map[string]any
			json.Unmarshal(req.Object, &obj)
			spec, _ := obj["spec"].(map[string]any)
			if spec == nil || spec["modelRef"] == nil || spec["modelRef"] == "" {
				return &admission.Response{Allowed: false, Reason: "spec.modelRef is required"}
			}
			return &admission.Response{Allowed: true}
		},
	}

	chain := admission.NewChain(nil, []admission.ValidatingHook{hook})
	ms := newMemStore()
	srv := New(Config{Addr: ":0"}, ms, zap.NewNop(), WithAdmission(chain))

	secretRef := map[string]any{
		"metadata": map[string]any{"name": "bad-secretref"},
		"spec": map[string]any{
			"backendType": "internal",
			"backendRef":  "secret/app",
		},
	}

	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/secretrefs", secretRef)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if !strings.Contains(body["message"].(string), "secretRef.spec.key is required") {
		t.Fatalf("expected default SecretRef validation error, got %v", body["message"])
	}

	agent := map[string]any{
		"metadata": map[string]any{"name": "good-agent"},
		"spec": map[string]any{
			"modelRef":     map[string]any{"name": "gpt-4"},
			"instructions": map[string]any{"inline": "Test agent."},
		},
	}
	rec = doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("agent create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDefaultAdmission_RejectsInvalidToolSecretRefCreate(t *testing.T) {
	srv, _ := newProductionTestServer(t)

	tool := map[string]any{
		"metadata": map[string]any{"name": "bad-tool"},
		"spec": map[string]any{
			"className":  "process-networked",
			"secretRefs": []any{map[string]any{"key": "API_KEY"}},
		},
	}

	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/tools", tool)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if !strings.Contains(body["message"].(string), "tool.spec.secretRefs[0].name is required") {
		t.Fatalf("expected tool secretRef validation error, got %v", body["message"])
	}
}

func TestSecretRefAPIResponses_RedactInternalSecretAnnotation(t *testing.T) {
	srv, ms := newTestServer(t)

	secretRef := map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "SecretRef",
		"metadata": map[string]any{
			"name":        "internal-secretref",
			"namespace":   "default",
			"annotations": map[string]any{"kognition.dev/internal-secret-data": `{"api-key":"super-secret"}`, "team": "core"},
		},
		"spec": map[string]any{
			"backendType": "internal",
			"backendRef":  "secret/internal-secretref",
			"key":         "api-key",
		},
	}

	data, err := json.Marshal(secretRef)
	if err != nil {
		t.Fatalf("marshal secretRef: %v", err)
	}
	if err := ms.Create(context.Background(), "SecretRef", "default", "internal-secretref", data); err != nil {
		t.Fatalf("seed secretRef: %v", err)
	}
	if err := ms.CreateRevision(context.Background(), "SecretRef", "default", "internal-secretref", 1, data, data); err != nil {
		t.Fatalf("seed secretRef revision: %v", err)
	}

	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/secretrefs/internal-secretref", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	meta := body["metadata"].(map[string]any)
	annotations := meta["annotations"].(map[string]any)
	if _, ok := annotations["kognition.dev/internal-secret-data"]; ok {
		t.Fatal("get leaked internal secret annotation")
	}
	if annotations["team"] != "core" {
		t.Fatalf("get annotations = %+v, want non-secret annotations preserved", annotations)
	}

	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/secretrefs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body = decodeJSON(t, rec)
	items := body["items"].([]any)
	listMeta := items[0].(map[string]any)["metadata"].(map[string]any)
	listAnnotations := listMeta["annotations"].(map[string]any)
	if _, ok := listAnnotations["kognition.dev/internal-secret-data"]; ok {
		t.Fatal("list leaked internal secret annotation")
	}

	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/secretrefs/internal-secretref/revisions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list revisions: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body = decodeJSON(t, rec)
	revisionItems := body["items"].([]any)
	revisionDataEncoded := revisionItems[0].(map[string]any)["data"].(string)
	revisionDataBytes, err := base64.StdEncoding.DecodeString(revisionDataEncoded)
	if err != nil {
		t.Fatalf("decode revision data: %v", err)
	}
	var revisionData map[string]any
	if err := json.Unmarshal(revisionDataBytes, &revisionData); err != nil {
		t.Fatalf("unmarshal revision data: %v", err)
	}
	revisionMeta := revisionData["metadata"].(map[string]any)
	revisionAnnotations := revisionMeta["annotations"].(map[string]any)
	if _, ok := revisionAnnotations["kognition.dev/internal-secret-data"]; ok {
		t.Fatal("revision list leaked internal secret annotation")
	}

	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/secretrefs/internal-secretref/revisions/1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get revision: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body = decodeJSON(t, rec)
	revisionData = body["data"].(map[string]any)
	revisionMeta = revisionData["metadata"].(map[string]any)
	revisionAnnotations = revisionMeta["annotations"].(map[string]any)
	if _, ok := revisionAnnotations["kognition.dev/internal-secret-data"]; ok {
		t.Fatal("get revision leaked internal secret annotation")
	}
}

// TestListWithItems verifies list returns created resources.
func TestListWithItems(t *testing.T) {
	srv, _ := newTestServer(t)

	// Create two agents.
	for _, name := range []string{"agent-a", "agent-b"} {
		obj := map[string]any{
			"metadata": map[string]any{"name": name},
			"spec":     map[string]any{"model": "gpt-4"},
		}
		rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", obj)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: want 201, got %d", name, rec.Code)
		}
	}

	// List.
	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", rec.Code)
	}
	body := decodeJSON(t, rec)
	items := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("list: want 2 items, got %d", len(items))
	}
	if body["kind"] != "AgentList" {
		t.Fatalf("list kind: want 'AgentList', got %v", body["kind"])
	}
	if body["apiVersion"] != "kognition.dev/v1alpha1" {
		t.Fatalf("list apiVersion: want 'kognition.dev/v1alpha1', got %v", body["apiVersion"])
	}
}

// TestNamespaceIsolation verifies resources are isolated between namespaces.
func TestNamespaceIsolation(t *testing.T) {
	srv, _ := newTestServer(t)

	// Create agent in namespace "ns-a".
	agent := map[string]any{
		"metadata": map[string]any{"name": "isolated-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/ns-a/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create in ns-a: want 201, got %d", rec.Code)
	}

	// GET from ns-a succeeds.
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/ns-a/agents/isolated-agent", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get from ns-a: want 200, got %d", rec.Code)
	}

	// GET from ns-b fails with 404.
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/ns-b/agents/isolated-agent", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get from ns-b: want 404, got %d", rec.Code)
	}

	// LIST in ns-b returns empty.
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/ns-b/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list ns-b: want 200, got %d", rec.Code)
	}
	body := decodeJSON(t, rec)
	items := body["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("list ns-b: want 0 items, got %d", len(items))
	}
}

// TestWatchMultipleEventTypes verifies watch receives ADDED, MODIFIED, and DELETED events.
func TestWatchMultipleEventTypes(t *testing.T) {
	srv, _ := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/apis/kognition.dev/v1alpha1/watch/namespaces/default/agents", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	var watchBody string
	done := make(chan struct{})
	go func() {
		srv.mux.ServeHTTP(rec, req)
		watchBody = rec.Body.String()
		close(done)
	}()

	// Give watch time to subscribe.
	time.Sleep(50 * time.Millisecond)

	// Create.
	agent := map[string]any{
		"metadata": map[string]any{"name": "multi-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	createRec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", createRec.Code)
	}
	created := decodeJSON(t, createRec)

	time.Sleep(50 * time.Millisecond)

	// Update.
	created["spec"] = map[string]any{"model": "claude-4"}
	updateRec := doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/multi-agent", created)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update: want 200, got %d: %s", updateRec.Code, updateRec.Body.String())
	}

	time.Sleep(50 * time.Millisecond)

	// Delete.
	deleteRec := doReq(t, srv, "DELETE", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/multi-agent", nil)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d", deleteRec.Code)
	}

	// Wait for events, then cancel.
	time.Sleep(200 * time.Millisecond)
	cancel()
	waitForWatchCompletion(t, done)

	if !strings.Contains(watchBody, "ADDED") {
		t.Fatalf("watch: expected ADDED, got: %s", watchBody)
	}
	if !strings.Contains(watchBody, "MODIFIED") {
		t.Fatalf("watch: expected MODIFIED, got: %s", watchBody)
	}
	if !strings.Contains(watchBody, "DELETED") {
		t.Fatalf("watch: expected DELETED, got: %s", watchBody)
	}
}

func waitForWatchCompletion(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled watch request to complete")
	}
}

func TestWatchRequiresRelistWhenHistoryIsLost(t *testing.T) {
	watchStore := &scriptedWatchStore{
		memStore: newMemStore(),
		events:   []store.WatchEvent{{Type: store.EventResyncRequired}},
	}
	srv := New(Config{Addr: ":0"}, watchStore, zap.NewNop())

	rec := doReq(t, srv, http.MethodGet, "/apis/kognition.dev/v1alpha1/watch/namespaces/default/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("watch: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	want := "data: {\"type\":\"ERROR\",\"error\":\"watch history lost; relist required\"}\n\n"
	if rec.Body.String() != want {
		t.Fatalf("watch body = %q, want %q", rec.Body.String(), want)
	}
}

func TestWatchFiltersEventsFromOtherNamespaces(t *testing.T) {
	watchStore := &scriptedWatchStore{
		memStore: newMemStore(),
		events: []store.WatchEvent{
			{
				Type:      store.EventCreated,
				Namespace: "other",
				Object:    makeWatchAgentResource("other-agent", 1),
			},
			{
				Type:      store.EventCreated,
				Namespace: "default",
				Object:    makeWatchAgentResource("default-agent", 1),
			},
		},
	}
	srv := New(Config{Addr: ":0"}, watchStore, zap.NewNop())

	rec := doReq(t, srv, http.MethodGet, "/apis/kognition.dev/v1alpha1/watch/namespaces/default/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("watch: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "other-agent") {
		t.Fatalf("watch leaked event from another namespace: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "default-agent") {
		t.Fatalf("watch omitted event from requested namespace: %s", rec.Body.String())
	}
}

// TestResolveKindMapping verifies plural-to-kind mapping for several resource types.
func TestResolveKindMapping(t *testing.T) {
	tests := []struct {
		plural string
		kind   string
	}{
		{"agents", "Agent"},
		{"workflows", "Workflow"},
		{"memoryclasses", "MemoryClass"},
		{"memories", "Memory"},
		{"knowledgebases", "KnowledgeBase"},
		{"knowledgebaseindexruns", "KnowledgeBaseIndexRun"},
		{"knowledgebaseindexschedules", "KnowledgeBaseIndexSchedule"},
		{"toolclasses", "ToolClass"},
		{"agentpolicies", "AgentPolicy"},
		{"evaluations", "Evaluation"},
		{"evaluationdatasets", "EvaluationDataset"},
		{"evaluationruns", "EvaluationRun"},
		{"crossnamespacegrants", "CrossNamespaceGrant"},
		{"unknownplural", ""},
	}

	for _, tc := range tests {
		t.Run(tc.plural, func(t *testing.T) {
			got := resolveKind(tc.plural)
			if got != tc.kind {
				t.Fatalf("resolveKind(%q): want %q, got %q", tc.plural, tc.kind, got)
			}
		})
	}
}

func TestPluralMappingsCoverCanonicalRegistryExactly(t *testing.T) {
	want := make(map[string]struct{}, len(v1alpha1.KindRegistry))
	for kind := range v1alpha1.KindRegistry {
		if v1alpha1.IsGenericResourceKind(kind) {
			want[kind] = struct{}{}
		}
	}

	for plural, kind := range pluralToKind {
		if _, ok := want[kind]; !ok {
			t.Errorf("plural %q maps to non-canonical kind %q", plural, kind)
			continue
		}
		delete(want, kind)
	}
	if len(want) != 0 {
		t.Errorf("canonical kinds without standalone resource routes: %v", want)
	}
}

func TestRuntimeApprovalIsNotExposedThroughGenericRoutes(t *testing.T) {
	if got := resolveKind("runtimeapprovals"); got != "" {
		t.Fatalf("resolveKind(runtimeapprovals) = %q, want no generic route", got)
	}
}

func TestUnknownResourceRouteReturnsNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doReq(t, srv, http.MethodGet, "/apis/kognition.dev/v1alpha1/namespaces/default/notresources", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown resource: want 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDefaultAdmissionRejectsInvalidAgent(t *testing.T) {
	srv, _ := newProductionTestServer(t)
	obj := map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Agent",
		"metadata":   map[string]any{"name": "invalid-agent"},
		"spec":       map[string]any{},
	}
	rec := doReq(t, srv, http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/agents", obj)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid Agent: want 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestResolveKindEmpty verifies resolveKind handles empty string gracefully.
func TestResolveKindEmpty(t *testing.T) {
	got := resolveKind("")
	if got != "" {
		t.Fatalf("resolveKind(\"\"): want empty, got %q", got)
	}
}

// --- US-0004 Tests ---

// TestFinalizerBlocksDelete verifies that delete sets deletionTimestamp when finalizers are present.
func TestFinalizerBlocksDelete(t *testing.T) {
	srv, _ := newTestServer(t)

	agent := map[string]any{
		"metadata": map[string]any{
			"name":       "fin-agent",
			"finalizers": []any{"cleanup.kognition.dev"},
		},
		"spec": map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// DELETE should soft-delete (set deletionTimestamp)
	rec = doReq(t, srv, "DELETE", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/fin-agent", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET should still succeed and have deletionTimestamp
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/fin-agent", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get after soft-delete: want 200, got %d", rec.Code)
	}
	obj := decodeJSON(t, rec)
	meta := obj["metadata"].(map[string]any)
	if meta["deletionTimestamp"] == nil {
		t.Fatal("expected deletionTimestamp to be set")
	}
}

func TestFinalizerDeleteRetriesConflictWithoutLosingConcurrentMetadata(t *testing.T) {
	ms := newMemStore()
	srv := newBareServer(&deleteConflictStore{memStore: ms}, zap.NewNop())

	agent := map[string]any{
		"metadata": map[string]any{
			"name":       "conflicted-delete-agent",
			"finalizers": []any{"cleanup.kognition.dev"},
		},
		"spec": map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = doReq(t, srv, http.MethodDelete, "/apis/kognition.dev/v1alpha1/namespaces/default/agents/conflicted-delete-agent", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	obj := decodeJSON(t, rec)
	meta := obj["metadata"].(map[string]any)
	if meta["deletionTimestamp"] == nil {
		t.Fatal("expected deletionTimestamp after conflict retry")
	}
	annotations := meta["annotations"].(map[string]any)
	if annotations["updated-by"] != "controller" {
		t.Fatalf("annotations = %+v, want concurrent metadata preserved", annotations)
	}
	if meta["resourceVersion"] != float64(3) {
		t.Fatalf("resourceVersion = %v, want 3 after concurrent update and retry", meta["resourceVersion"])
	}
}

func TestFinalizerDeleteStopsAfterPersistentConflicts(t *testing.T) {
	conflictStore := &persistentDeleteConflictStore{memStore: newMemStore()}
	srv := newBareServer(conflictStore, zap.NewNop())

	agent := map[string]any{
		"metadata": map[string]any{
			"name":       "contended-delete-agent",
			"finalizers": []any{"cleanup.kognition.dev"},
		},
		"spec": map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = doReq(t, srv, http.MethodDelete, "/apis/kognition.dev/v1alpha1/namespaces/default/agents/contended-delete-agent", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if conflictStore.attempts != 10 {
		t.Fatalf("update attempts = %d, want 10", conflictStore.attempts)
	}

	persisted, err := conflictStore.Get(context.Background(), "Agent", "default", "contended-delete-agent")
	if err != nil {
		t.Fatalf("get resource after conflicts: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(persisted, &obj); err != nil {
		t.Fatalf("decode persisted resource: %v", err)
	}
	meta := obj["metadata"].(map[string]any)
	if _, exists := meta["deletionTimestamp"]; exists {
		t.Fatalf("persisted metadata = %+v, want no deletionTimestamp", meta)
	}
}

func TestDeleteSoftDeleteResponse_RedactsSecretRefInternalAnnotation(t *testing.T) {
	srv, _ := newTestServer(t)

	secretRef := map[string]any{
		"metadata": map[string]any{
			"name":       "fin-secretref",
			"finalizers": []any{"cleanup.kognition.dev"},
			"annotations": map[string]any{
				"kognition.dev/internal-secret-data": `{"api-key":"super-secret"}`,
				"team":                               "core",
			},
		},
		"spec": map[string]any{
			"backendType": "internal",
			"backendRef":  "secret/fin-secretref",
			"key":         "api-key",
		},
	}

	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/secretrefs", secretRef)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = doReq(t, srv, "DELETE", "/apis/kognition.dev/v1alpha1/namespaces/default/secretrefs/fin-secretref", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	meta := body["metadata"].(map[string]any)
	annotations := meta["annotations"].(map[string]any)
	if _, ok := annotations["kognition.dev/internal-secret-data"]; ok {
		t.Fatal("delete response leaked internal secret annotation")
	}
	if annotations["team"] != "core" {
		t.Fatalf("delete response annotations = %+v, want non-secret annotations preserved", annotations)
	}
}

// TestDeleteWithoutFinalizers verifies immediate deletion when no finalizers.
func TestDeleteWithoutFinalizers(t *testing.T) {
	srv, _ := newTestServer(t)

	agent := map[string]any{
		"metadata": map[string]any{"name": "no-fin-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}

	rec = doReq(t, srv, "DELETE", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/no-fin-agent", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET should now 404
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/no-fin-agent", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: want 404, got %d", rec.Code)
	}
}

func TestDeleteAutomaticAgentIdentityRequiresControllerAdmission(t *testing.T) {
	validator := admission.NewAgentIdentityValidator("controller")
	srv, memory := newTestServerWithAdmission(t, admission.NewChain(nil, []admission.ValidatingHook{validator}))
	identity := v1alpha1.AgentIdentity{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-identity-planner", Namespace: "default", Labels: map[string]string{"kognition.dev/auto-created-for-agent": "planner"}},
		Spec:       v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}},
	}
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.Create(context.Background(), "AgentIdentity", "default", identity.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodDelete, "/apis/kognition.dev/v1alpha1/namespaces/default/agentidentities/agent-identity-planner", nil)
	recorder := httptest.NewRecorder()
	srv.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tenant delete status = %d, want %d: %s", recorder.Code, http.StatusUnprocessableEntity, recorder.Body.String())
	}
	request = request.WithContext(auth.WithIdentity(request.Context(), &auth.Identity{Username: "controller", Method: "token"}))
	recorder = httptest.NewRecorder()
	srv.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("controller delete status = %d, want %d: %s", recorder.Code, http.StatusNoContent, recorder.Body.String())
	}
}

// TestAutoDeleteOnFinalizerRemoval verifies resource is deleted when last finalizer is removed.
func TestAutoDeleteOnFinalizerRemoval(t *testing.T) {
	srv, _ := newTestServer(t)

	agent := map[string]any{
		"metadata": map[string]any{
			"name":       "auto-del-agent",
			"finalizers": []any{"cleanup.kognition.dev"},
		},
		"spec": map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}

	// Soft-delete
	doReq(t, srv, "DELETE", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/auto-del-agent", nil)

	// Get the resource to get current resourceVersion
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/auto-del-agent", nil)
	obj := decodeJSON(t, rec)

	// Remove the finalizer
	meta := obj["metadata"].(map[string]any)
	meta["finalizers"] = []any{}
	rec = doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/auto-del-agent", obj)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("update to remove finalizer: want 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET should now 404 (auto-deleted)
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/auto-del-agent", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after auto-delete: want 404, got %d", rec.Code)
	}
}

func TestAutoDeleteOnFinalizerRemovalReportsDeleteFailure(t *testing.T) {
	ms := newMemStore()
	es := &errStore{memStore: ms, deleteErr: errors.New("delete unavailable")}
	srv := newBareServer(es, zap.NewNop())

	agent := map[string]any{
		"metadata": map[string]any{
			"name":       "failed-auto-delete-agent",
			"finalizers": []any{"cleanup.kognition.dev"},
		},
		"spec": map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = doReq(t, srv, http.MethodDelete, "/apis/kognition.dev/v1alpha1/namespaces/default/agents/failed-auto-delete-agent", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("soft delete: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	obj := decodeJSON(t, rec)
	obj["metadata"].(map[string]any)["finalizers"] = []any{}

	rec = doReq(t, srv, http.MethodPut, "/apis/kognition.dev/v1alpha1/namespaces/default/agents/failed-auto-delete-agent", obj)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("finalizer removal: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := ms.Get(context.Background(), "Agent", "default", "failed-auto-delete-agent"); err != nil {
		t.Fatalf("resource should remain after failed delete: %v", err)
	}
}

func TestAPIUpdateDefersWorkflowRevisionCreation(t *testing.T) {
	srv, ms := newTestServer(t)

	workflow := map[string]any{
		"metadata": map[string]any{"name": "rev-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/workflows", workflow)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}
	created := decodeJSON(t, rec)

	// Update spec
	created["spec"] = map[string]any{"model": "claude-4"}
	rec = doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/workflows/rev-agent", created)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Workflow reconciliation owns immutable history in both modes.
	ms.mu.RLock()
	revs := ms.revisions[storeKey("Workflow", "default", "rev-agent")]
	ms.mu.RUnlock()
	if len(revs) != 0 {
		t.Fatalf("API-created revisions = %#v, want workflow reconciler-owned history", revs)
	}
}

func TestAPIUpdateAppliesAdmissionBeforeWorkflowReconciliation(t *testing.T) {
	hook := &testMutatingHook{
		name: "resolve-model",
		kind: "Workflow",
		mutate: func(_ context.Context, req *admission.Request) *admission.Response {
			if req.Operation != admission.OperationUpdate {
				return &admission.Response{Allowed: true}
			}
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &admission.Response{Allowed: false, Reason: err.Error()}
			}
			spec := obj["spec"].(map[string]any)
			spec["model"] = "admitted-model"
			patched, _ := json.Marshal(obj)
			return &admission.Response{Allowed: true, Patch: patched}
		},
	}
	srv, _ := newTestServerWithAdmission(t, admission.NewChain([]admission.MutatingHook{hook}, nil))

	workflow := map[string]any{
		"metadata": map[string]any{"name": "admitted-rev-agent"},
		"spec":     map[string]any{"model": "initial-model"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/workflows", workflow)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	created := decodeJSON(t, rec)
	created["spec"] = map[string]any{"model": "submitted-model"}
	rec = doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/workflows/admitted-rev-agent", created)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	updated := decodeJSON(t, rec)
	if got := updated["spec"].(map[string]any)["model"]; got != "admitted-model" {
		t.Fatalf("admitted model: want admitted-model, got %v", got)
	}
}

func TestUpdateUsesItsOwnPersistedWrite(t *testing.T) {
	ms := newMemStore()
	srv := newBareServer(&interleavingUpdateStore{memStore: ms}, zap.NewNop())
	workflow := map[string]any{
		"metadata": map[string]any{"name": "concurrent-rev-agent"},
		"spec":     map[string]any{"model": "initial-model"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/workflows", workflow)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	created := decodeJSON(t, rec)
	created["spec"] = map[string]any{"model": "first-writer-model"}
	rec = doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/workflows/concurrent-rev-agent", created)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	response := decodeJSON(t, rec)
	if got := response["spec"].(map[string]any)["model"]; got != "first-writer-model" {
		t.Fatalf("response model: want first-writer-model, got %v", got)
	}
	responseMeta := response["metadata"].(map[string]any)
	if got := int64(responseMeta["resourceVersion"].(float64)); got != 2 {
		t.Fatalf("response resourceVersion: want 2, got %d", got)
	}

}

// TestNoRevisionOnMetadataOnlyChange verifies no revision on non-spec changes.
func TestNoRevisionOnMetadataOnlyChange(t *testing.T) {
	srv, ms := newTestServer(t)

	agent := map[string]any{
		"metadata": map[string]any{"name": "no-rev-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}
	created := decodeJSON(t, rec)

	// Update only metadata (add a label), keep spec the same
	meta := created["metadata"].(map[string]any)
	meta["labels"] = map[string]any{"env": "test"}
	rec = doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/no-rev-agent", created)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	ms.mu.RLock()
	revs := ms.revisions[storeKey("Agent", "default", "no-rev-agent")]
	ms.mu.RUnlock()
	if len(revs) != 0 {
		t.Fatalf("want 0 revisions, got %d", len(revs))
	}
}

func TestAPIUpdateDefersControllerOwnedRevisionKinds(t *testing.T) {
	srv, ms := newTestServer(t)
	for _, kind := range []struct {
		resource string
		kind     string
	}{
		{resource: "agents", kind: "Agent"},
		{resource: "prompts", kind: "Prompt"},
		{resource: "solutions", kind: "Solution"},
	} {
		t.Run(kind.kind, func(t *testing.T) {
			object := map[string]any{
				"metadata": map[string]any{"name": "controller-owned-revision"},
				"spec":     map[string]any{"value": "before"},
			}
			rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/"+kind.resource, object)
			if rec.Code != http.StatusCreated {
				t.Fatalf("create: want 201, got %d: %s", rec.Code, rec.Body.String())
			}
			updated := decodeJSON(t, rec)
			updated["spec"] = map[string]any{"value": "after"}
			rec = doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/"+kind.resource+"/controller-owned-revision", updated)
			if rec.Code != http.StatusOK {
				t.Fatalf("update: want 200, got %d: %s", rec.Code, rec.Body.String())
			}
			ms.mu.RLock()
			revisions := ms.revisions[storeKey(kind.kind, "default", "controller-owned-revision")]
			ms.mu.RUnlock()
			if len(revisions) != 0 {
				t.Fatalf("API-created revisions = %#v, want controller-owned history only", revisions)
			}
		})
	}
}

// TestListRevisionsAPI verifies the revision list endpoint.
func TestListRevisionsAPI(t *testing.T) {
	srv, memory := newTestServer(t)
	putJSON(memory, "Workflow", "default", "list-rev-agent", v1alpha1.Workflow{})
	if err := memory.CreateRevision(context.Background(), "Workflow", "default", "list-rev-agent", 1, []byte(`{"model":"gpt-4"}`), nil); err != nil {
		t.Fatalf("create revision 1: %v", err)
	}
	if err := memory.CreateRevision(context.Background(), "Workflow", "default", "list-rev-agent", 2, []byte(`{"model":"claude-4"}`), nil); err != nil {
		t.Fatalf("create revision 2: %v", err)
	}

	// List revisions
	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/workflows/list-rev-agent/revisions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list revisions: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	items := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("want 2 revisions, got %d", len(items))
	}
}

// TestGetRevisionAPI verifies the single revision endpoint.
func TestGetRevisionAPI(t *testing.T) {
	srv, memory := newTestServer(t)
	putJSON(memory, "Workflow", "default", "get-rev-agent", v1alpha1.Workflow{})
	if err := memory.CreateRevision(context.Background(), "Workflow", "default", "get-rev-agent", 1, []byte(`{"model":"gpt-4"}`), nil); err != nil {
		t.Fatalf("create revision: %v", err)
	}

	// List revisions to get the revision number
	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/workflows/get-rev-agent/revisions", nil)
	listBody := decodeJSON(t, rec)
	items := listBody["items"].([]any)
	if len(items) == 0 {
		t.Fatal("expected at least 1 revision")
	}
	revNum := items[0].(map[string]any)["revision"].(float64)

	// Get the specific revision
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/workflows/get-rev-agent/revisions/"+strconv.FormatInt(int64(revNum), 10), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get revision: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["kind"] != "Revision" {
		t.Fatalf("want kind Revision, got %v", body["kind"])
	}
}

// TestGetRevisionNotFound verifies 404 for non-existent revision.
func TestGetRevisionNotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	agent := map[string]any{
		"metadata": map[string]any{"name": "rev-404-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)

	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/rev-404-agent/revisions/999", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get non-existent revision: want 404, got %d", rec.Code)
	}
}

func TestDiffRevisionsAPI(t *testing.T) {
	srv, memory := newTestServer(t)
	putJSON(memory, "Agent", "default", "diff-agent", v1alpha1.Agent{})
	fromSpec := []byte(`{"model":"gpt-4","runtime":{"maxTokens":128}}`)
	toSpec := []byte(`{"model":"claude-4","runtime":{"maxTokens":256},"tools":["search"]}`)
	if err := memory.CreateRevision(context.Background(), "Agent", "default", "diff-agent", 2, fromSpec, nil); err != nil {
		t.Fatalf("create source revision: %v", err)
	}
	if err := memory.CreateRevision(context.Background(), "Agent", "default", "diff-agent", 3, toSpec, nil); err != nil {
		t.Fatalf("create target revision: %v", err)
	}

	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/diff-agent/revisions/diff?from=2&to=3", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("diff revisions: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["kind"] != "RevisionDiff" || body["fromRevision"] != float64(2) || body["toRevision"] != float64(3) {
		t.Fatalf("diff response identity = %#v", body)
	}
	patch, ok := body["patch"].([]any)
	if !ok || len(patch) != 3 {
		t.Fatalf("patch = %#v, want three operations", body["patch"])
	}
	paths := make(map[string]bool)
	for _, item := range patch {
		operation := item.(map[string]any)
		paths[operation["path"].(string)] = true
	}
	for _, path := range []string{"/model", "/runtime/maxTokens", "/tools"} {
		if !paths[path] {
			t.Errorf("patch paths = %#v, missing %q", paths, path)
		}
	}
}

func TestDiffRevisionsAPIRejectsInvalidOrMissingRevisions(t *testing.T) {
	srv, memory := newTestServer(t)
	putJSON(memory, "Agent", "default", "diff-errors", v1alpha1.Agent{})
	if err := memory.CreateRevision(context.Background(), "Agent", "default", "diff-errors", 1, []byte(`{"model":"gpt-4"}`), nil); err != nil {
		t.Fatalf("create revision: %v", err)
	}

	for _, test := range []struct {
		name string
		path string
		code int
	}{
		{name: "missing from", path: "/apis/kognition.dev/v1alpha1/namespaces/default/agents/diff-errors/revisions/diff?to=1", code: http.StatusBadRequest},
		{name: "same revision", path: "/apis/kognition.dev/v1alpha1/namespaces/default/agents/diff-errors/revisions/diff?from=1&to=1", code: http.StatusBadRequest},
		{name: "missing revision", path: "/apis/kognition.dev/v1alpha1/namespaces/default/agents/diff-errors/revisions/diff?from=1&to=2", code: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := doReq(t, srv, "GET", test.path, nil)
			if rec.Code != test.code {
				t.Fatalf("diff revisions: want %d, got %d: %s", test.code, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRevisionEndpointsHideHistoryAfterParentDeletion(t *testing.T) {
	srv, memory := newTestServer(t)
	putJSON(memory, "Agent", "default", "deleted-agent", v1alpha1.Agent{})
	for _, revision := range []int64{1, 2} {
		if err := memory.CreateRevision(context.Background(), "Agent", "default", "deleted-agent", revision, []byte(fmt.Sprintf(`{"model":"revision-%d"}`, revision)), nil); err != nil {
			t.Fatalf("create revision %d: %v", revision, err)
		}
	}
	if err := memory.Delete(context.Background(), "Agent", "default", "deleted-agent"); err != nil {
		t.Fatalf("delete parent: %v", err)
	}

	for _, path := range []string{
		"/apis/kognition.dev/v1alpha1/namespaces/default/agents/deleted-agent/revisions",
		"/apis/kognition.dev/v1alpha1/namespaces/default/agents/deleted-agent/revisions/1",
		"/apis/kognition.dev/v1alpha1/namespaces/default/agents/deleted-agent/revisions/diff?from=1&to=2",
	} {
		rec := doReq(t, srv, "GET", path, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s: want 404, got %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestGetRevisionAPIRejectsMalformedRevision(t *testing.T) {
	srv, memory := newTestServer(t)
	putJSON(memory, "Agent", "default", "rev-agent", v1alpha1.Agent{})
	for _, revision := range []string{"0", "1junk", "invalid"} {
		t.Run(revision, func(t *testing.T) {
			rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/rev-agent/revisions/"+revision, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("get revision %q: want 400, got %d: %s", revision, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestDeleteAlreadyDeleting verifies that a second DELETE on a soft-deleted resource
// returns 200 with the resource as-is (idempotent).
func TestDeleteAlreadyDeleting(t *testing.T) {
	srv, _ := newTestServer(t)

	agent := map[string]any{
		"metadata": map[string]any{
			"name":       "already-del-agent",
			"finalizers": []any{"cleanup.kognition.dev"},
		},
		"spec": map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}

	// First DELETE — soft-delete sets deletionTimestamp.
	rec = doReq(t, srv, "DELETE", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/already-del-agent", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("first delete: want 200, got %d", rec.Code)
	}
	first := decodeJSON(t, rec)
	firstMeta := first["metadata"].(map[string]any)
	if firstMeta["deletionTimestamp"] == nil {
		t.Fatal("expected deletionTimestamp after first delete")
	}

	// Second DELETE — should return 200 with resource unchanged.
	rec = doReq(t, srv, "DELETE", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/already-del-agent", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("second delete: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	second := decodeJSON(t, rec)
	secondMeta := second["metadata"].(map[string]any)
	if secondMeta["deletionTimestamp"] != firstMeta["deletionTimestamp"] {
		t.Fatalf("deletionTimestamp should be unchanged: want %v, got %v",
			firstMeta["deletionTimestamp"], secondMeta["deletionTimestamp"])
	}
}

// TestGetRevisionBadRequest verifies that invalid revision parameters return 400.
func TestGetRevisionBadRequest(t *testing.T) {
	srv, _ := newTestServer(t)

	agent := map[string]any{
		"metadata": map[string]any{"name": "rev-bad-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)

	tests := []struct {
		name string
		rev  string
	}{
		{"non-integer", "abc"},
		{"zero", "0"},
		{"negative", "-1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/rev-bad-agent/revisions/"+tc.rev, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("rev=%s: want 400, got %d: %s", tc.rev, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestListRevisionsEmpty verifies that listing revisions for a resource with no spec changes
// returns an empty items array.
func TestListRevisionsEmpty(t *testing.T) {
	srv, _ := newTestServer(t)

	agent := map[string]any{
		"metadata": map[string]any{"name": "no-revs-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}

	// List revisions — no spec changes, so should be empty.
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/no-revs-agent/revisions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list revisions: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	items := body["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("want 0 revisions, got %d", len(items))
	}
}

// --- Error-injecting store for internal error path tests ---

// errStore wraps memStore and can inject errors per method.
type errStore struct {
	*memStore
	pingErr           error
	getErr            error
	listErr           error
	createErr         error
	updateErr         error
	deleteErr         error
	watchErr          error
	listRevisionsErr  error
	createRevisionErr error
	getRevisionErr    error
	listEventsErr     error
	appendEventErr    error
}

func (e *errStore) Ping(ctx context.Context) error {
	if e.pingErr != nil {
		return e.pingErr
	}
	return e.memStore.Ping(ctx)
}

func (e *errStore) Get(ctx context.Context, kind, ns, name string) ([]byte, error) {
	if e.getErr != nil {
		return nil, e.getErr
	}
	return e.memStore.Get(ctx, kind, ns, name)
}

func (e *errStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if e.listErr != nil {
		return nil, e.listErr
	}
	return e.memStore.List(ctx, kind, opts)
}

func (e *errStore) Create(ctx context.Context, kind, ns, name string, data []byte) error {
	if e.createErr != nil {
		return e.createErr
	}
	return e.memStore.Create(ctx, kind, ns, name, data)
}

func (e *errStore) Update(ctx context.Context, kind, ns, name string, data []byte, rv int64) error {
	if e.updateErr != nil {
		return e.updateErr
	}
	return e.memStore.Update(ctx, kind, ns, name, data, rv)
}

func (e *errStore) UpdateAndGet(ctx context.Context, kind, ns, name string, data []byte, rv int64) ([]byte, error) {
	if e.updateErr != nil {
		return nil, e.updateErr
	}
	return e.memStore.UpdateAndGet(ctx, kind, ns, name, data, rv)
}

func (e *errStore) Delete(ctx context.Context, kind, ns, name string) error {
	if e.deleteErr != nil {
		return e.deleteErr
	}
	return e.memStore.Delete(ctx, kind, ns, name)
}

func (e *errStore) Watch(ctx context.Context, kind string, opts store.ListOptions) (<-chan store.WatchEvent, error) {
	if e.watchErr != nil {
		return nil, e.watchErr
	}
	return e.memStore.Watch(ctx, kind, opts)
}

func (e *errStore) ListRevisions(ctx context.Context, kind, ns, name string) ([]store.Revision, error) {
	if e.listRevisionsErr != nil {
		return nil, e.listRevisionsErr
	}
	return e.memStore.ListRevisions(ctx, kind, ns, name)
}

func (e *errStore) CreateRevision(ctx context.Context, kind, ns, name string, revision int64, spec, data []byte) error {
	if e.createRevisionErr != nil {
		return e.createRevisionErr
	}
	return e.memStore.CreateRevision(ctx, kind, ns, name, revision, spec, data)
}

func (e *errStore) GetRevision(ctx context.Context, kind, ns, name string, revision int64) (*store.Revision, error) {
	if e.getRevisionErr != nil {
		return nil, e.getRevisionErr
	}
	return e.memStore.GetRevision(ctx, kind, ns, name, revision)
}

func (e *errStore) ListEvents(ctx context.Context, kind, ns, name string) ([]store.InstanceEvent, error) {
	if e.listEventsErr != nil {
		return nil, e.listEventsErr
	}
	return e.memStore.ListEvents(ctx, kind, ns, name)
}

func (e *errStore) AppendEvent(ctx context.Context, kind, ns, name string, event store.InstanceEvent) error {
	if e.appendEventErr != nil {
		return e.appendEventErr
	}
	return e.memStore.AppendEvent(ctx, kind, ns, name, event)
}

func newErrTestServer(t *testing.T) (*Server, *errStore) {
	t.Helper()
	es := &errStore{memStore: newMemStore()}
	logger, _ := zap.NewDevelopment()
	srv := newBareServer(es, logger)
	return srv, es
}

// --- Tests for internal error paths ---

func TestReadyzFailure(t *testing.T) {
	srv, es := newErrTestServer(t)
	es.pingErr = errors.New("connection refused")

	rec := doReq(t, srv, "GET", "/readyz", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz: want 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListStoreError(t *testing.T) {
	srv, es := newErrTestServer(t)
	es.listErr = errors.New("db connection pool exhausted")

	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("list: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["message"] != "internal error" {
		t.Fatalf("message: want 'internal error', got %v", body["message"])
	}
}

func TestGetStoreError(t *testing.T) {
	srv, es := newErrTestServer(t)
	es.getErr = errors.New("timeout")

	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/my-agent", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("get: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMutationEndpointsRejectUnreadableBodies(t *testing.T) {
	for _, tc := range []struct {
		name           string
		method         string
		path           string
		approvalBroker bool
	}{
		{name: "create", method: http.MethodPost, path: "/apis/kognition.dev/v1alpha1/namespaces/default/agents"},
		{name: "update", method: http.MethodPut, path: "/apis/kognition.dev/v1alpha1/namespaces/default/agents/unreadable"},
		{name: "approval decision", method: http.MethodPost, path: "/apis/kognition.dev/v1alpha1/approvals/approval-1/decision", approvalBroker: true},
		{name: "workflow approval", method: http.MethodPost, path: "/apis/kognition.dev/v1alpha1/namespaces/default/workflowruns/run-1/approval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			if tc.approvalBroker {
				srv = New(Config{Addr: ":0"}, newMemStore(), zap.NewNop(), WithApprovalBroker(runtime.NewApprovalBroker()))
			}
			req := httptest.NewRequest(tc.method, tc.path, failingReader{})
			rec := httptest.NewRecorder()
			srv.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if body := decodeJSON(t, rec); body["message"] != "failed to read request body" {
				t.Fatalf("message = %v, want request body error", body["message"])
			}
		})
	}
}

func TestCreateStoreError(t *testing.T) {
	srv, es := newErrTestServer(t)
	es.createErr = errors.New("disk full")

	agent := map[string]any{
		"metadata": map[string]any{"name": "fail-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("create: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateStoreError(t *testing.T) {
	srv, es := newErrTestServer(t)
	agent := map[string]any{
		"metadata": map[string]any{"name": "up-agent"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}
	es.updateErr = errors.New("deadlock detected")
	updated := map[string]any{
		"metadata": map[string]any{"name": "up-agent", "resourceVersion": float64(1)},
		"spec":     map[string]any{"model": "gpt-5"},
	}
	rec = doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/up-agent", updated)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("update: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateGetOldObjectError(t *testing.T) {
	srv, es := newErrTestServer(t)
	agent := map[string]any{
		"metadata": map[string]any{"name": "old-fail"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}
	es.getErr = errors.New("connection reset")
	updated := map[string]any{
		"metadata": map[string]any{"name": "old-fail", "resourceVersion": float64(1)},
		"spec":     map[string]any{"model": "gpt-5"},
	}
	rec = doReq(t, srv, "PUT", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/old-fail", updated)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("update old-obj fetch: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteGetStoreError(t *testing.T) {
	srv, es := newErrTestServer(t)
	es.getErr = errors.New("network partition")

	rec := doReq(t, srv, "DELETE", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/any-agent", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("delete-get: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteRejectsMalformedStoredResource(t *testing.T) {
	memory := newMemStore()
	if err := memory.Create(context.Background(), "Agent", "default", "malformed", []byte("{")); err != nil {
		t.Fatalf("seed malformed resource: %v", err)
	}
	srv := New(Config{Addr: ":0"}, memory, zap.NewNop())
	rec := doReq(t, srv, http.MethodDelete, "/apis/kognition.dev/v1alpha1/namespaces/default/agents/malformed", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
}

func TestDeleteConflictRefreshFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		refreshData []byte
		refreshErr  error
	}{
		{name: "read failure", refreshErr: errors.New("refresh failed")},
		{name: "malformed resource", refreshData: []byte("{")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := newMemStore()
			data, err := json.Marshal(map[string]any{
				"metadata": map[string]any{
					"name":            "conflicted",
					"namespace":       "default",
					"resourceVersion": 1,
					"finalizers":      []any{"cleanup.kognition.dev"},
				},
			})
			if err != nil {
				t.Fatalf("marshal resource: %v", err)
			}
			if err := memory.Create(context.Background(), "Agent", "default", "conflicted", data); err != nil {
				t.Fatalf("seed resource: %v", err)
			}
			faults := &deleteConflictRefreshStore{memStore: memory, refreshData: tc.refreshData, refreshErr: tc.refreshErr}
			srv := New(Config{Addr: ":0"}, faults, zap.NewNop())
			rec := doReq(t, srv, http.MethodDelete, "/apis/kognition.dev/v1alpha1/namespaces/default/agents/conflicted", nil)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
			}
		})
	}
}

func TestDeleteReportsNotFoundWhenResourceDisappears(t *testing.T) {
	memory := newMemStore()
	data := []byte(`{"metadata":{"name":"vanished","namespace":"default","resourceVersion":1}}`)
	if err := memory.Create(context.Background(), "Agent", "default", "vanished", data); err != nil {
		t.Fatalf("seed resource: %v", err)
	}
	srv := New(Config{Addr: ":0"}, &errStore{memStore: memory, deleteErr: store.ErrNotFound}, zap.NewNop())
	rec := doReq(t, srv, http.MethodDelete, "/apis/kognition.dev/v1alpha1/namespaces/default/agents/vanished", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestDeleteStoreError(t *testing.T) {
	srv, es := newErrTestServer(t)

	// Create a resource without finalizers so delete goes to the immediate path.
	agent := map[string]any{
		"metadata": map[string]any{"name": "del-fail"},
		"spec":     map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}

	// Now inject delete error.
	es.deleteErr = errors.New("permission denied")

	rec = doReq(t, srv, "DELETE", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/del-fail", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("delete: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteWithFinalizersUpdateError(t *testing.T) {
	srv, es := newErrTestServer(t)

	// Create a resource with finalizers.
	agent := map[string]any{
		"metadata": map[string]any{
			"name":       "fin-del-fail",
			"finalizers": []any{"cleanup.kognition.dev"},
		},
		"spec": map[string]any{"model": "gpt-4"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", agent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}

	// Inject update error — the delete-with-finalizers path does an Update.
	es.updateErr = errors.New("serialization failure")

	rec = doReq(t, srv, "DELETE", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/fin-del-fail", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("delete-update: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWatchStoreError(t *testing.T) {
	srv, es := newErrTestServer(t)
	es.watchErr = errors.New("watch capacity exceeded")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/kognition.dev/v1alpha1/watch/namespaces/default/agents", nil)
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("watch-err: want 500, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "watch setup failed") {
		t.Fatalf("watch-err: want 'watch setup failed' in body, got: %s", body)
	}
	if strings.Contains(body, "watch capacity exceeded") {
		t.Fatal("watch-err: internal error leaked to client")
	}
}

func TestListRevisionsStoreError(t *testing.T) {
	srv, es := newErrTestServer(t)
	putJSON(es.memStore, "Agent", "default", "any", v1alpha1.Agent{})
	es.listRevisionsErr = errors.New("query timeout")

	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/any/revisions", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("list-revisions: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGetRevisionStoreError(t *testing.T) {
	srv, es := newErrTestServer(t)
	putJSON(es.memStore, "Agent", "default", "any", v1alpha1.Agent{})
	es.getRevisionErr = errors.New("storage unavailable")

	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/any/revisions/1", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("get-revision-err: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["message"] != "internal error" {
		t.Fatalf("get-revision-err: want 'internal error', got %v", body["message"])
	}
}

func TestHandleListPagination(t *testing.T) {
	srv, ms := newTestServer(t)

	// Create 5 agents.
	for i := 0; i < 5; i++ {
		name := "agent-" + strconv.Itoa(i)
		agent := map[string]any{
			"kind":       "Agent",
			"apiVersion": "kognition.dev/v1alpha1",
			"metadata":   map[string]any{"name": name, "namespace": "default"},
			"spec":       map[string]any{"model": "gpt-4"},
		}
		data, _ := json.Marshal(agent)
		ms.mu.Lock()
		ms.resources[storeKey("Agent", "default", name)] = memEntry{data: data, version: 1}
		ms.mu.Unlock()
	}

	// Request with limit=2 should return 2 items and a continue token.
	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents?limit=2", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	items := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	metadata := body["metadata"].(map[string]any)
	cont, ok := metadata["continue"].(string)
	if !ok || cont == "" {
		t.Fatal("expected continue token in metadata")
	}

	// Use the continue token to fetch the next page.
	rec2 := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents?limit=2&continue="+cont, nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("page2: want 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	body2 := decodeJSON(t, rec2)
	items2 := body2["items"].([]any)
	if len(items2) != 2 {
		t.Fatalf("page2: expected 2 items, got %d", len(items2))
	}

	// Third page should have only 1 item and no continue token.
	meta2 := body2["metadata"].(map[string]any)
	cont2 := meta2["continue"].(string)
	rec3 := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents?limit=2&continue="+cont2, nil)
	if rec3.Code != http.StatusOK {
		t.Fatalf("page3: want 200, got %d: %s", rec3.Code, rec3.Body.String())
	}
	body3 := decodeJSON(t, rec3)
	items3 := body3["items"].([]any)
	if len(items3) != 1 {
		t.Fatalf("page3: expected 1 item, got %d", len(items3))
	}
	meta3 := body3["metadata"].(map[string]any)
	if _, hasContinue := meta3["continue"]; hasContinue {
		t.Fatal("page3: expected no continue token on last page")
	}
}

func TestHandleListPaginationInvalidLimit(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents?limit=abc", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleListPaginationInvalidContinue(t *testing.T) {
	srv, _ := newTestServer(t)

	tests := []string{
		"?limit=2&continue=not-base64!",
		"?limit=2&continue=" + base64.RawURLEncoding.EncodeToString([]byte("not-an-offset")),
		"?limit=2&continue=" + base64.RawURLEncoding.EncodeToString([]byte("-1")),
		"?continue=" + base64.RawURLEncoding.EncodeToString([]byte("2")),
	}
	for _, query := range tests {
		rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents"+query, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("query %q: want 400, got %d: %s", query, rec.Code, rec.Body.String())
		}
	}
}

func TestHandleListNoPagination(t *testing.T) {
	srv, ms := newTestServer(t)

	// Create 3 agents without pagination (no limit param).
	for i := 0; i < 3; i++ {
		name := "agent-" + strconv.Itoa(i)
		agent := map[string]any{
			"kind":       "Agent",
			"apiVersion": "kognition.dev/v1alpha1",
			"metadata":   map[string]any{"name": name, "namespace": "default"},
			"spec":       map[string]any{"model": "gpt-4"},
		}
		data, _ := json.Marshal(agent)
		ms.mu.Lock()
		ms.resources[storeKey("Agent", "default", name)] = memEntry{data: data, version: 1}
		ms.mu.Unlock()
	}

	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	items := body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	metadata := body["metadata"].(map[string]any)
	if _, hasContinue := metadata["continue"]; hasContinue {
		t.Fatal("expected no continue token when no limit specified")
	}
}

// noFlushResponseWriter is an http.ResponseWriter that does NOT implement http.Flusher.
type noFlushResponseWriter struct {
	code   int
	header http.Header
	body   bytes.Buffer
}

func (w *noFlushResponseWriter) Header() http.Header         { return w.header }
func (w *noFlushResponseWriter) Write(b []byte) (int, error) { return w.body.Write(b) }
func (w *noFlushResponseWriter) WriteHeader(code int)        { w.code = code }

func TestWatchFlusherNotSupported(t *testing.T) {
	srv, _ := newErrTestServer(t)

	w := &noFlushResponseWriter{header: http.Header{}}
	req := httptest.NewRequest("GET", "/apis/kognition.dev/v1alpha1/watch/namespaces/default/agents", nil)
	srv.mux.ServeHTTP(w, req)

	if w.code != http.StatusInternalServerError {
		t.Fatalf("watch-no-flusher: want 500, got %d: %s", w.code, w.body.String())
	}
	if !strings.Contains(w.body.String(), "streaming not supported") {
		t.Fatalf("want 'streaming not supported', got %s", w.body.String())
	}
}

func TestMemoryRecordRTBFEndpoint_CascadesAndReturnsResult(t *testing.T) {
	srv, ms := newTestServer(t)

	recA := map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "MemoryRecord",
		"metadata": map[string]any{
			"name":            "rec-a",
			"namespace":       "default",
			"resourceVersion": float64(1),
		},
		"spec": map[string]any{
			"memoryRef": map[string]any{"name": "mem-rtbf"},
			"content":   "seed",
			"provenance": map[string]any{
				"sourceAgentIdentity": "agent-1",
				"sessionId":           "sess-1",
				"reasoningStepId":     "step-1",
			},
		},
	}
	recB := map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "MemoryRecord",
		"metadata": map[string]any{
			"name":            "rec-b",
			"namespace":       "default",
			"resourceVersion": float64(1),
		},
		"spec": map[string]any{
			"memoryRef": map[string]any{"name": "mem-rtbf"},
			"content":   "derived",
			"provenance": map[string]any{
				"sourceAgentIdentity":    "agent-1",
				"sessionId":              "sess-1",
				"reasoningStepId":        "step-2",
				"derivedFromRecordNames": []any{"rec-a"},
			},
		},
	}

	bA, _ := json.Marshal(recA)
	bB, _ := json.Marshal(recB)
	if err := ms.Create(context.Background(), "MemoryRecord", "default", "rec-a", bA); err != nil {
		t.Fatalf("create rec-a: %v", err)
	}
	if err := ms.Create(context.Background(), "MemoryRecord", "default", "rec-b", bB); err != nil {
		t.Fatalf("create rec-b: %v", err)
	}

	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/memoryrecords/rec-a/rtbf", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rtbf: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	tombstoned, ok := body["tombstonedRecords"].([]any)
	if !ok || len(tombstoned) != 2 {
		t.Fatalf("expected 2 tombstoned records, got %+v", body["tombstonedRecords"])
	}
}

func TestMemoryRecordRTBFEndpoint_Returns500WhenAuditAppendFails(t *testing.T) {
	srv, es := newErrTestServer(t)
	es.appendEventErr = errors.New("append failed")

	recA := map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "MemoryRecord",
		"metadata": map[string]any{
			"name":            "rec-a",
			"namespace":       "default",
			"resourceVersion": float64(1),
		},
		"spec": map[string]any{
			"memoryRef": map[string]any{"name": "mem-rtbf"},
			"content":   "seed",
			"provenance": map[string]any{
				"sourceAgentIdentity": "agent-1",
				"sessionId":           "sess-1",
				"reasoningStepId":     "step-1",
			},
		},
	}
	bA, _ := json.Marshal(recA)
	if err := es.Create(context.Background(), "MemoryRecord", "default", "rec-a", bA); err != nil {
		t.Fatalf("create rec-a: %v", err)
	}

	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/memoryrecords/rec-a/rtbf", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("rtbf audit fail: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestToInt64Variants(t *testing.T) {
	tests := []struct {
		name string
		val  any
		want int64
		ok   bool
	}{
		{"float64", float64(42), 42, true},
		{"int64", int64(7), 7, true},
		{"json.Number valid", json.Number("99"), 99, true},
		{"json.Number invalid", json.Number("not-a-number"), 0, false},
		{"string unsupported", "123", 0, false},
		{"nil", nil, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := toInt64(tc.val)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("toInt64(%v): got (%d, %v), want (%d, %v)", tc.val, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TC: List events — empty, then with appended events.
func TestListEventsStoreError(t *testing.T) {
	srv, es := newErrTestServer(t)
	es.listEventsErr = errors.New("events table locked")

	rec := doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agentinstances/any/events", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("list-events: want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListEventsAPI(t *testing.T) {
	srv, ms := newTestServer(t)
	// Create an agent instance first.
	instance := map[string]any{
		"metadata": map[string]any{"name": "run-1"},
		"spec":     map[string]any{"agentRef": "my-agent"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agentinstances", instance)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}

	// List events — should be empty.
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agentinstances/run-1/events", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list events: want 200, got %d", rec.Code)
	}
	body := decodeJSON(t, rec)
	items := body["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("want 0 events, got %d", len(items))
	}

	// Append an event directly via store.
	ms.AppendEvent(context.Background(), "AgentInstance", "default", "run-1", store.InstanceEvent{
		Timestamp: "2024-01-01T00:00:00Z",
		Type:      "Normal",
		Reason:    "Started",
		Message:   "Instance started",
	})

	// List events — should have 1 event.
	rec = doReq(t, srv, "GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agentinstances/run-1/events", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list events: want 200, got %d", rec.Code)
	}
	body = decodeJSON(t, rec)
	items = body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("want 1 event, got %d", len(items))
	}
	ev := items[0].(map[string]any)
	if ev["reason"] != "Started" {
		t.Fatalf("want reason=Started, got %v", ev["reason"])
	}
}

func TestSanitizeSecretRefFallbacksAndPublicAnnotations(t *testing.T) {
	unchanged := [][]byte{
		nil,
		[]byte("{"),
		[]byte(`{"metadata":{}}`),
		[]byte(`{"metadata":{"annotations":{"public":"value"}}}`),
	}
	for _, input := range unchanged {
		if got := sanitizeResourceForAPI("SecretRef", input); !bytes.Equal(got, input) {
			t.Fatalf("sanitizeResourceForAPI changed %q to %q", input, got)
		}
	}

	input := []byte(`{"metadata":{"annotations":{"kognition.dev/internal-secret-data":"secret","public":"value"}}}`)
	var got map[string]any
	if err := json.Unmarshal(sanitizeResourceForAPI("SecretRef", input), &got); err != nil {
		t.Fatalf("decode sanitized SecretRef: %v", err)
	}
	annotations := got["metadata"].(map[string]any)["annotations"].(map[string]any)
	if annotations["public"] != "value" {
		t.Fatalf("annotations = %+v, want public annotation preserved", annotations)
	}
	if _, exists := annotations[internalSecretDataAnnotation]; exists {
		t.Fatalf("annotations = %+v, internal annotation was not removed", annotations)
	}
}

// TC-0053: Submit approval via API and verify blocked request resumes approved.
func TestApprovalDecisionAPI_Approved(t *testing.T) {
	ms := newMemStore()
	logger, _ := zap.NewDevelopment()
	broker := runtime.NewApprovalBroker()
	srv := New(Config{Addr: ":0"}, ms, logger, WithApprovalBroker(broker))

	resultCh := make(chan runtime.ApprovalDecision, 1)
	errCh := make(chan error, 1)
	go func() {
		decision, err := broker.Wait(context.Background(), runtime.ApprovalRequest{ID: "approval-53"})
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- decision
	}()

	time.Sleep(10 * time.Millisecond)

	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/approvals/approval-53/decision", map[string]any{
		"state":  "approved",
		"reason": "looks good",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("submit approval: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case err := <-errCh:
		t.Fatalf("wait returned error: %v", err)
	case decision := <-resultCh:
		if decision.State != runtime.ApprovalStateApproved {
			t.Fatalf("decision state = %q, want %q", decision.State, runtime.ApprovalStateApproved)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for approval decision")
	}
}

func TestGenericAutonomyReviewCRUDRejectsStatusWrites(t *testing.T) {
	memory := newMemStore()
	server := New(Config{}, memory, zap.NewNop())
	createPath := "/apis/kognition.dev/v1alpha1/namespaces/default/autonomyreviews"
	create := doReq(t, server, http.MethodPost, createPath, map[string]any{
		"metadata": map[string]any{"name": "forged-review"},
		"spec":     map[string]any{"agentRef": map[string]any{"name": "agent"}, "actionDescription": "promotion"},
		"status":   map[string]any{"decision": "Approved", "decidedBy": "attacker"},
	})
	if create.Code != http.StatusUnprocessableEntity || !strings.Contains(create.Body.String(), "controller-owned") {
		t.Fatalf("create response = %d %s, want controller-owned denial", create.Code, create.Body)
	}
	pendingFixture := v1alpha1.AutonomyReview{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "pending-review", Namespace: "default", ResourceVersion: 1},
		Spec: v1alpha1.AutonomyReviewSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}, ActionDescription: "promotion",
		},
		Status: v1alpha1.AutonomyReviewStatus{Decision: "Pending"},
	}
	pendingData, err := json.Marshal(pendingFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.Create(context.Background(), "AutonomyReview", "default", "pending-review", pendingData); err != nil {
		t.Fatal(err)
	}
	raw, err := memory.Get(context.Background(), "AutonomyReview", "default", "pending-review")
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	object["status"] = map[string]any{"decision": "Approved", "decidedBy": "attacker"}
	update := doReq(t, server, http.MethodPut, createPath+"/pending-review", object)
	if update.Code != http.StatusUnprocessableEntity || !strings.Contains(update.Body.String(), "controller-owned") {
		t.Fatalf("update response = %d %s, want controller-owned denial", update.Code, update.Body)
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	object["spec"].(map[string]any)["trustProfileRef"] = "another-profile"
	retarget := doReq(t, server, http.MethodPut, createPath+"/pending-review", object)
	if retarget.Code != http.StatusUnprocessableEntity || !strings.Contains(retarget.Body.String(), "spec is immutable") {
		t.Fatalf("retarget response = %d %s, want immutable-spec denial", retarget.Code, retarget.Body)
	}
}

// TC-0054: Submit rejection via API and verify blocked request resumes rejected.
func TestApprovalDecisionAPI_Rejected(t *testing.T) {
	ms := newMemStore()
	logger, _ := zap.NewDevelopment()
	broker := runtime.NewApprovalBroker()
	srv := New(Config{Addr: ":0"}, ms, logger, WithApprovalBroker(broker))

	resultCh := make(chan runtime.ApprovalDecision, 1)
	errCh := make(chan error, 1)
	go func() {
		decision, err := broker.Wait(context.Background(), runtime.ApprovalRequest{ID: "approval-54"})
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- decision
	}()

	time.Sleep(10 * time.Millisecond)

	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/approvals/approval-54/decision", map[string]any{
		"state":  "rejected",
		"reason": "denied by policy",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("submit rejection: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case err := <-errCh:
		t.Fatalf("wait returned error: %v", err)
	case decision := <-resultCh:
		if decision.State != runtime.ApprovalStateRejected {
			t.Fatalf("decision state = %q, want %q", decision.State, runtime.ApprovalStateRejected)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for rejection decision")
	}
}

func TestApprovalDecisionAPI_RejectsDecisionAfterApprovalClosed(t *testing.T) {
	broker := runtime.NewApprovalBroker()
	srv := New(Config{Addr: ":0"}, newMemStore(), zap.NewNop(), WithApprovalBroker(broker))

	resultCh := make(chan runtime.ApprovalDecision, 1)
	go func() {
		decision, _ := broker.Wait(context.Background(), runtime.ApprovalRequest{ID: "approval-closed"})
		resultCh <- decision
	}()

	deadline := time.Now().Add(time.Second)
	for {
		response := doReq(t, srv, http.MethodPost, "/apis/kognition.dev/v1alpha1/approvals/approval-closed/decision", map[string]any{
			"state": "approved",
		})
		if response.Code == http.StatusOK {
			break
		}
		if response.Code != http.StatusNotFound || time.Now().After(deadline) {
			t.Fatalf("first decision: want 200, got %d: %s", response.Code, response.Body.String())
		}
		time.Sleep(time.Millisecond)
	}
	<-resultCh

	response := doReq(t, srv, http.MethodPost, "/apis/kognition.dev/v1alpha1/approvals/approval-closed/decision", map[string]any{
		"state": "rejected",
	})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "already closed") {
		t.Fatalf("second decision = %d %s, want 409 already closed", response.Code, response.Body)
	}
}

func TestApprovalDecisionAPI_ValidatesRequests(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		srv, _ := newTestServer(t)
		rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/approvals/test/decision", map[string]any{
			"state": "approved",
		})
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusNotImplemented, rec.Body.String())
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		logger, _ := zap.NewDevelopment()
		srv := New(Config{Addr: ":0"}, newMemStore(), logger, WithApprovalBroker(runtime.NewApprovalBroker()))
		rec := doReqRaw(t, srv, "POST", "/apis/kognition.dev/v1alpha1/approvals/test/decision", []byte("{"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	})

	t.Run("invalid state", func(t *testing.T) {
		logger, _ := zap.NewDevelopment()
		srv := New(Config{Addr: ":0"}, newMemStore(), logger, WithApprovalBroker(runtime.NewApprovalBroker()))
		rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/approvals/test/decision", map[string]any{
			"state": "pending",
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
		}
	})

	t.Run("not found", func(t *testing.T) {
		logger, _ := zap.NewDevelopment()
		srv := New(Config{Addr: ":0"}, newMemStore(), logger, WithApprovalBroker(runtime.NewApprovalBroker()))
		rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/approvals/missing/decision", map[string]any{
			"state": "approved",
		})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusNotFound, rec.Body.String())
		}
	})
}

// seedWorkflowForApproval seeds a Workflow with an approval gate on the given
// step into the in-memory store. If requiredApprovers > 0, the gate enforces
// quorum.
func seedWorkflowForApproval(t *testing.T, ms *memStore, ns, name, step string, requiredApprovers int32) {
	t.Helper()
	wfObj := map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Workflow",
		"metadata":   map[string]any{"namespace": ns, "name": name, "resourceVersion": 1},
		"spec": map[string]any{
			"approvalGates": []any{
				map[string]any{"from": "plan", "to": step, "requiredApprovers": requiredApprovers},
			},
		},
	}
	data, _ := json.Marshal(wfObj)
	if err := ms.Create(context.Background(), "Workflow", ns, name, data); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
}

// seedWorkflowRunForApproval seeds a WorkflowRun referencing the given workflow.
func seedWorkflowRunForApproval(t *testing.T, ms *memStore, ns, name, workflowName string) {
	t.Helper()
	runObj := map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "WorkflowRun",
		"metadata":   map[string]any{"namespace": ns, "name": name, "resourceVersion": 1},
		"spec":       map[string]any{"workflowRef": map[string]any{"name": workflowName}},
	}
	data, _ := json.Marshal(runObj)
	if err := ms.Create(context.Background(), "WorkflowRun", ns, name, data); err != nil {
		t.Fatalf("seed workflowrun: %v", err)
	}
}

// TC-0238: POST to /approval subresource with decision=approve persists the
// approved-steps annotation and an audit record on the WorkflowRun.
func TestWorkflowRunApproval_OpensGate(t *testing.T) {
	srv, ms := newTestServer(t)
	seedWorkflowForApproval(t, ms, "default", "wf-1", "execute", 0)
	seedWorkflowRunForApproval(t, ms, "default", "run-1", "wf-1")

	rec := doReq(t, srv, "POST",
		"/apis/kognition.dev/v1alpha1/namespaces/default/workflowruns/run-1/approval",
		map[string]any{"step": "execute", "decision": "approve", "approver": "alice", "comment": "lgtm"})
	if rec.Code != http.StatusOK {
		t.Fatalf("approval response: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	data, err := ms.Get(context.Background(), "WorkflowRun", "default", "run-1")
	if err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	meta := stored["metadata"].(map[string]any)
	annotations, _ := meta["annotations"].(map[string]any)
	if got, _ := annotations["kognition.dev/approved-steps"].(string); got != "execute" {
		t.Fatalf("approved-steps annotation = %q, want %q", got, "execute")
	}
	status, _ := stored["status"].(map[string]any)
	records, _ := status["approvalRecords"].([]any)
	if len(records) != 1 {
		t.Fatalf("approvalRecords len = %d, want 1", len(records))
	}
	rec0 := records[0].(map[string]any)
	if rec0["step"] != "execute" || rec0["decision"] != "approve" || rec0["approver"] != "alice" {
		t.Fatalf("approval record = %+v", rec0)
	}
}

// TC-0239: requiredApprovers=2 — the first distinct approver does not open the
// gate; the second distinct approver does.
func TestWorkflowRunApproval_RequiredApproversQuorum(t *testing.T) {
	srv, ms := newTestServer(t)
	seedWorkflowForApproval(t, ms, "default", "wf-2", "execute", 2)
	seedWorkflowRunForApproval(t, ms, "default", "run-2", "wf-2")

	rec := doReq(t, srv, "POST",
		"/apis/kognition.dev/v1alpha1/namespaces/default/workflowruns/run-2/approval",
		map[string]any{"step": "execute", "decision": "approve", "approver": "alice"})
	if rec.Code != http.StatusOK {
		t.Fatalf("first approval: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	data, _ := ms.Get(context.Background(), "WorkflowRun", "default", "run-2")
	var stored map[string]any
	_ = json.Unmarshal(data, &stored)
	meta := stored["metadata"].(map[string]any)
	annotations, _ := meta["annotations"].(map[string]any)
	if got, _ := annotations["kognition.dev/approved-steps"].(string); got != "" {
		t.Fatalf("after 1/2 approvers approved-steps = %q, want empty", got)
	}
	if got, _ := annotations["kognition.dev/approvers.execute"].(string); got != "alice" {
		t.Fatalf("approvers annotation = %q, want alice", got)
	}

	rec = doReq(t, srv, "POST",
		"/apis/kognition.dev/v1alpha1/namespaces/default/workflowruns/run-2/approval",
		map[string]any{"step": "execute", "decision": "approve", "approver": "alice"})
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate approval: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	data, _ = ms.Get(context.Background(), "WorkflowRun", "default", "run-2")
	_ = json.Unmarshal(data, &stored)
	annotations = stored["metadata"].(map[string]any)["annotations"].(map[string]any)
	if got, _ := annotations["kognition.dev/approved-steps"].(string); got != "" {
		t.Fatalf("duplicate approver opened gate: approved-steps = %q", got)
	}
	if got, _ := annotations["kognition.dev/approvers.execute"].(string); got != "alice" {
		t.Fatalf("duplicate approver annotation = %q, want alice", got)
	}

	rec = doReq(t, srv, "POST",
		"/apis/kognition.dev/v1alpha1/namespaces/default/workflowruns/run-2/approval",
		map[string]any{"step": "execute", "decision": "approve", "approver": "bob"})
	if rec.Code != http.StatusOK {
		t.Fatalf("second approval: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	data, _ = ms.Get(context.Background(), "WorkflowRun", "default", "run-2")
	_ = json.Unmarshal(data, &stored)
	meta = stored["metadata"].(map[string]any)
	annotations, _ = meta["annotations"].(map[string]any)
	if got, _ := annotations["kognition.dev/approved-steps"].(string); got != "execute" {
		t.Fatalf("after 2/2 approvers approved-steps = %q, want execute", got)
	}
	status, _ := stored["status"].(map[string]any)
	records, _ := status["approvalRecords"].([]any)
	if len(records) != 3 {
		t.Fatalf("approvalRecords len = %d, want 3", len(records))
	}
}

func TestWorkflowRunApproval_RetriesConflictWithoutLosingConcurrentVote(t *testing.T) {
	ms := newMemStore()
	srv := New(Config{Addr: ":0"}, &approvalConflictStore{memStore: ms}, zap.NewNop())
	seedWorkflowForApproval(t, ms, "default", "wf-conflict", "execute", 2)
	seedWorkflowRunForApproval(t, ms, "default", "run-conflict", "wf-conflict")

	rec := doReq(t, srv, http.MethodPost,
		"/apis/kognition.dev/v1alpha1/namespaces/default/workflowruns/run-conflict/approval",
		map[string]any{"step": "execute", "decision": "approve", "approver": "alice"})
	if rec.Code != http.StatusOK {
		t.Fatalf("approval response: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	data, err := ms.Get(context.Background(), "WorkflowRun", "default", "run-conflict")
	if err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("unmarshal workflowrun: %v", err)
	}
	meta := stored["metadata"].(map[string]any)
	annotations := meta["annotations"].(map[string]any)
	if got := annotations["kognition.dev/approvers.execute"]; got != "bob,alice" {
		t.Fatalf("approvers annotation = %q, want bob,alice", got)
	}
	if got := annotations["kognition.dev/approved-steps"]; got != "execute" {
		t.Fatalf("approved-steps annotation = %q, want execute", got)
	}
	records := stored["status"].(map[string]any)["approvalRecords"].([]any)
	if len(records) != 2 {
		t.Fatalf("approvalRecords len = %d, want 2", len(records))
	}
	if got := records[0].(map[string]any)["approver"]; got != "bob" {
		t.Fatalf("first audit approver = %q, want bob", got)
	}
	if got := records[1].(map[string]any)["approver"]; got != "alice" {
		t.Fatalf("second audit approver = %q, want alice", got)
	}
	if meta["resourceVersion"] != float64(3) {
		t.Fatalf("resourceVersion = %v, want 3 after concurrent vote and retry", meta["resourceVersion"])
	}
}

func TestWorkflowRunApproval_FailsClosedWhenWorkflowIsCorrupt(t *testing.T) {
	ms := newMemStore()
	if err := ms.Create(context.Background(), "Workflow", "default", "wf-corrupt", []byte("{")); err != nil {
		t.Fatalf("seed corrupt workflow: %v", err)
	}
	seedWorkflowRunForApproval(t, ms, "default", "run-corrupt", "wf-corrupt")
	srv := New(Config{Addr: ":0"}, ms, zap.NewNop())

	rec := doReq(t, srv, http.MethodPost,
		"/apis/kognition.dev/v1alpha1/namespaces/default/workflowruns/run-corrupt/approval",
		map[string]any{"step": "execute", "decision": "approve", "approver": "alice"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("approval response: want 500, got %d: %s", rec.Code, rec.Body.String())
	}

	data, err := ms.Get(context.Background(), "WorkflowRun", "default", "run-corrupt")
	if err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("unmarshal workflowrun: %v", err)
	}
	if _, ok := stored["status"]; ok {
		t.Fatalf("failed approval persisted status: %#v", stored["status"])
	}
	meta := stored["metadata"].(map[string]any)
	if _, ok := meta["annotations"]; ok {
		t.Fatalf("failed approval persisted annotations: %#v", meta["annotations"])
	}
	if meta["resourceVersion"] != float64(1) {
		t.Fatalf("resourceVersion = %v, want unchanged version 1", meta["resourceVersion"])
	}
}

func TestWorkflowRunApproval_RequiresIdentityForQuorum(t *testing.T) {
	srv, ms := newTestServer(t)
	seedWorkflowForApproval(t, ms, "default", "wf-identity", "execute", 2)
	seedWorkflowRunForApproval(t, ms, "default", "run-identity", "wf-identity")

	rec := doReq(t, srv, "POST",
		"/apis/kognition.dev/v1alpha1/namespaces/default/workflowruns/run-identity/approval",
		map[string]any{"step": "execute", "decision": "approve"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("anonymous quorum approval: want 400, got %d: %s", rec.Code, rec.Body.String())
	}

	data, err := ms.Get(context.Background(), "WorkflowRun", "default", "run-identity")
	if err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("unmarshal workflowrun: %v", err)
	}
	if _, ok := stored["status"]; ok {
		t.Fatalf("rejected approval persisted status: %#v", stored["status"])
	}
	annotations, _ := stored["metadata"].(map[string]any)["annotations"].(map[string]any)
	if len(annotations) != 0 {
		t.Fatalf("rejected approval persisted annotations: %#v", annotations)
	}
}

func TestWorkflowRunApproval_UsesAuthenticatedApprover(t *testing.T) {
	srv, ms := newTestServer(t)
	seedWorkflowForApproval(t, ms, "default", "wf-auth", "execute", 2)
	seedWorkflowRunForApproval(t, ms, "default", "run-auth", "wf-auth")

	body, err := json.Marshal(map[string]any{
		"step": "execute", "decision": "approve", "approver": "forged-user",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	request := httptest.NewRequest("POST",
		"/apis/kognition.dev/v1alpha1/namespaces/default/workflowruns/run-auth/approval",
		bytes.NewReader(body))
	request = request.WithContext(auth.WithIdentity(request.Context(), &auth.Identity{
		Username: "verified-user", Method: "oidc",
	}))
	response := httptest.NewRecorder()
	srv.mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("approval response: want 200, got %d: %s", response.Code, response.Body.String())
	}
	responseBody := decodeJSON(t, response)
	if got := responseBody["approver"]; got != "verified-user" {
		t.Fatalf("response approver = %q, want verified-user", got)
	}

	data, err := ms.Get(context.Background(), "WorkflowRun", "default", "run-auth")
	if err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("unmarshal workflowrun: %v", err)
	}
	annotations := stored["metadata"].(map[string]any)["annotations"].(map[string]any)
	if got := annotations["kognition.dev/approvers.execute"]; got != "verified-user" {
		t.Fatalf("approvers annotation = %q, want verified-user", got)
	}
	records := stored["status"].(map[string]any)["approvalRecords"].([]any)
	record := records[0].(map[string]any)
	if got := record["approver"]; got != "verified-user" {
		t.Fatalf("audit approver = %q, want verified-user", got)
	}

	seedWorkflowRunForApproval(t, ms, "default", "run-auth-reject", "wf-auth")
	rejectBody, err := json.Marshal(map[string]any{
		"step": "execute", "decision": "reject", "approver": "forged-user",
	})
	if err != nil {
		t.Fatalf("marshal rejection: %v", err)
	}
	rejectRequest := httptest.NewRequest("POST",
		"/apis/kognition.dev/v1alpha1/namespaces/default/workflowruns/run-auth-reject/approval",
		bytes.NewReader(rejectBody))
	rejectRequest = rejectRequest.WithContext(auth.WithIdentity(rejectRequest.Context(), &auth.Identity{
		Username: "verified-user", Method: "oidc",
	}))
	rejectResponse := httptest.NewRecorder()
	srv.mux.ServeHTTP(rejectResponse, rejectRequest)
	if rejectResponse.Code != http.StatusOK {
		t.Fatalf("rejection response: want 200, got %d: %s", rejectResponse.Code, rejectResponse.Body.String())
	}
	if got := decodeJSON(t, rejectResponse)["approver"]; got != "verified-user" {
		t.Fatalf("rejection response approver = %q, want verified-user", got)
	}

	rejectedData, err := ms.Get(context.Background(), "WorkflowRun", "default", "run-auth-reject")
	if err != nil {
		t.Fatalf("get rejected workflowrun: %v", err)
	}
	var rejected map[string]any
	if err := json.Unmarshal(rejectedData, &rejected); err != nil {
		t.Fatalf("unmarshal rejected workflowrun: %v", err)
	}
	rejectedAnnotations := rejected["metadata"].(map[string]any)["annotations"].(map[string]any)
	if got := rejectedAnnotations["kognition.dev/rejected-steps"]; got != "execute" {
		t.Fatalf("rejected-steps annotation = %q, want execute", got)
	}
	rejectedRecords := rejected["status"].(map[string]any)["approvalRecords"].([]any)
	if got := rejectedRecords[0].(map[string]any)["approver"]; got != "verified-user" {
		t.Fatalf("rejection audit approver = %q, want verified-user", got)
	}
}

func TestWorkflowRunApproval_ValidatesRequestsAndStoreFailures(t *testing.T) {
	const path = "/apis/kognition.dev/v1alpha1/namespaces/default/workflowruns/run-errors/approval"

	t.Run("invalid JSON", func(t *testing.T) {
		srv, _ := newTestServer(t)
		response := doReqRaw(t, srv, http.MethodPost, path, []byte("{"))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body.String())
		}
	})

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{name: "missing step", body: map[string]any{"decision": "approve"}},
		{name: "invalid decision", body: map[string]any{"step": "execute", "decision": "defer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServer(t)
			response := doReq(t, srv, http.MethodPost, path, tc.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body.String())
			}
		})
	}

	t.Run("missing run", func(t *testing.T) {
		srv, _ := newTestServer(t)
		response := doReq(t, srv, http.MethodPost, path, map[string]any{"step": "execute", "decision": "reject"})
		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusNotFound, response.Body.String())
		}
	})

	t.Run("corrupt run", func(t *testing.T) {
		memory := newMemStore()
		if err := memory.Create(context.Background(), "WorkflowRun", "default", "run-errors", []byte("{")); err != nil {
			t.Fatalf("seed corrupt run: %v", err)
		}
		srv := New(Config{Addr: ":0"}, memory, zap.NewNop())
		response := doReq(t, srv, http.MethodPost, path, map[string]any{"step": "execute", "decision": "reject"})
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusInternalServerError, response.Body.String())
		}
	})

	t.Run("get failure", func(t *testing.T) {
		srv := New(Config{Addr: ":0"}, &errStore{memStore: newMemStore(), getErr: errors.New("read failed")}, zap.NewNop())
		response := doReq(t, srv, http.MethodPost, path, map[string]any{"step": "execute", "decision": "reject"})
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusInternalServerError, response.Body.String())
		}
	})

	for _, tc := range []struct {
		name       string
		updateErr  error
		wantStatus int
	}{
		{name: "update failure", updateErr: errors.New("write failed"), wantStatus: http.StatusInternalServerError},
		{name: "conflict exhaustion", updateErr: store.ErrConflict, wantStatus: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := newMemStore()
			seedWorkflowRunForApproval(t, memory, "default", "run-errors", "missing-workflow")
			srv := New(Config{Addr: ":0"}, &errStore{memStore: memory, updateErr: tc.updateErr}, zap.NewNop())
			response := doReq(t, srv, http.MethodPost, path, map[string]any{"step": "execute", "decision": "approve"})
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.wantStatus, response.Body.String())
			}
		})
	}
}

func TestWorkflowRunApproval_IncompleteApprovalConfigurationUsesSingleApproval(t *testing.T) {
	const path = "/apis/kognition.dev/v1alpha1/namespaces/default/workflowruns/run-fallback/approval"

	for _, tc := range []struct {
		name     string
		run      map[string]any
		workflow map[string]any
	}{
		{
			name: "run has no spec",
			run:  map[string]any{"metadata": map[string]any{"namespace": "default", "name": "run-fallback", "resourceVersion": 1}},
		},
		{
			name: "run has no workflow reference",
			run: map[string]any{
				"metadata": map[string]any{"namespace": "default", "name": "run-fallback", "resourceVersion": 1},
				"spec":     map[string]any{},
			},
		},
		{
			name: "workflow reference has no name",
			run: map[string]any{
				"metadata": map[string]any{"namespace": "default", "name": "run-fallback", "resourceVersion": 1},
				"spec":     map[string]any{"workflowRef": map[string]any{}},
			},
		},
		{
			name: "run metadata has no namespace",
			run: map[string]any{
				"metadata": map[string]any{"name": "run-fallback", "resourceVersion": 1},
				"spec":     map[string]any{"workflowRef": map[string]any{"name": "wf-fallback"}},
			},
		},
		{
			name: "workflow has no spec",
			run: map[string]any{
				"metadata": map[string]any{"namespace": "default", "name": "run-fallback", "resourceVersion": 1},
				"spec":     map[string]any{"workflowRef": map[string]any{"name": "wf-fallback"}},
			},
			workflow: map[string]any{"metadata": map[string]any{"namespace": "default", "name": "wf-fallback"}},
		},
		{
			name: "workflow has no matching valid gate",
			run: map[string]any{
				"metadata": map[string]any{"namespace": "default", "name": "run-fallback", "resourceVersion": 1},
				"spec":     map[string]any{"workflowRef": map[string]any{"name": "wf-fallback"}},
			},
			workflow: map[string]any{
				"metadata": map[string]any{"namespace": "default", "name": "wf-fallback"},
				"spec": map[string]any{"approvalGates": []any{
					"invalid",
					map[string]any{"to": "another-step", "requiredApprovers": 2},
					map[string]any{"to": "execute", "requiredApprovers": "invalid"},
				}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory := newMemStore()
			runData, err := json.Marshal(tc.run)
			if err != nil {
				t.Fatalf("marshal run: %v", err)
			}
			if err := memory.Create(context.Background(), "WorkflowRun", "default", "run-fallback", runData); err != nil {
				t.Fatalf("seed run: %v", err)
			}
			if tc.workflow != nil {
				workflowData, err := json.Marshal(tc.workflow)
				if err != nil {
					t.Fatalf("marshal workflow: %v", err)
				}
				if err := memory.Create(context.Background(), "Workflow", "default", "wf-fallback", workflowData); err != nil {
					t.Fatalf("seed workflow: %v", err)
				}
			}

			srv := New(Config{Addr: ":0"}, memory, zap.NewNop())
			response := doReq(t, srv, http.MethodPost, path, map[string]any{"step": "execute", "decision": "approve"})
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
			}
		})
	}
}

func TestWorkflowRunApproval_FailsClosedOnWorkflowReadError(t *testing.T) {
	memory := newMemStore()
	seedWorkflowRunForApproval(t, memory, "default", "run-workflow-error", "wf-error")
	srv := New(Config{Addr: ":0"}, &workflowGetErrorStore{memStore: memory, err: errors.New("workflow read failed")}, zap.NewNop())

	response := doReq(t, srv, http.MethodPost,
		"/apis/kognition.dev/v1alpha1/namespaces/default/workflowruns/run-workflow-error/approval",
		map[string]any{"step": "execute", "decision": "approve"})
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusInternalServerError, response.Body.String())
	}
}

// fakeDriver implements cpi.Driver and always returns an error from Complete.
type fakeDriver struct {
	err error
}

func (d *fakeDriver) Name() string { return "fake" }
func (d *fakeDriver) Validate(_ context.Context, _ *v1alpha1.ModelSpec, _ *v1alpha1.ModelClassSpec) error {
	return nil
}
func (d *fakeDriver) Probe(_ context.Context, _ *v1alpha1.ModelSpec, _ *cpi.Credentials) (*cpi.HealthResult, error) {
	return &cpi.HealthResult{Available: true}, nil
}
func (d *fakeDriver) Complete(_ context.Context, _ *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	return nil, d.err
}
func (d *fakeDriver) Embed(_ context.Context, _ *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return nil, nil
}

func TestChatHandler_ErrorMasking(t *testing.T) {
	logCore, observedLogs := observer.New(zap.ErrorLevel)
	logger := zap.New(logCore)

	// Set up a fake CPI driver that returns a sensitive error.
	sensitiveErr := "secret connection string: host=db.internal password=xyz"
	reg := cpi.NewRegistry()
	if err := reg.Register(&fakeDriver{err: errors.New(sensitiveErr)}); err != nil {
		t.Fatal(err)
	}

	ms := &memStore{
		resources: make(map[string]memEntry),
		events:    make(map[string][]store.InstanceEvent),
	}

	srv := New(Config{Addr: ":0"}, ms, logger, WithCPIRegistry(reg))

	// Seed Agent (phase=Ready, modelRef="test-model").
	agent := v1alpha1.Agent{}
	agent.Kind = "Agent"
	agent.APIVersion = "kognition.dev/v1alpha1"
	agent.Name = "test-agent"
	agent.Namespace = "default"
	agent.Spec.ModelRef.Name = "test-model"
	agent.Status.Phase = v1alpha1.PhaseReady
	agentData, _ := json.Marshal(agent)
	ms.resources[storeKey("Agent", "default", "test-agent")] = memEntry{data: agentData, version: 1}

	// Seed Model (className="test-class", modelID="gpt-4").
	model := v1alpha1.Model{}
	model.Kind = "Model"
	model.APIVersion = "kognition.dev/v1alpha1"
	model.Name = "test-model"
	model.Namespace = "default"
	model.Spec.ClassName = "test-class"
	model.Spec.ModelID = "gpt-4"
	modelData, _ := json.Marshal(model)
	ms.resources[storeKey("Model", "default", "test-model")] = memEntry{data: modelData, version: 1}

	// Seed ModelClass (provisioner="fake").
	mc := v1alpha1.ModelClass{}
	mc.Kind = "ModelClass"
	mc.APIVersion = "kognition.dev/v1alpha1"
	mc.Name = "test-class"
	mc.Namespace = "default"
	mc.Spec.Provisioner = "fake"
	mcData, _ := json.Marshal(mc)
	ms.resources[storeKey("ModelClass", "", "test-class")] = memEntry{data: mcData, version: 1}

	// POST to chat endpoint.
	reqBody := `{"message":"hello"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/test-agent/chat", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	srv.mux.ServeHTTP(rec, req)

	// Must return 502.
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", rec.Code, rec.Body.String())
	}

	// Body must contain generic error message.
	respBody := rec.Body.String()
	if !strings.Contains(respBody, "model inference failed") {
		t.Fatalf("response should contain 'model inference failed', got: %s", respBody)
	}

	// Body must NOT contain sensitive information.
	if strings.Contains(respBody, "secret connection string") {
		t.Fatalf("response leaks sensitive error details: %s", respBody)
	}
	if strings.Contains(respBody, "password=xyz") {
		t.Fatalf("response leaks password: %s", respBody)
	}
	for _, entry := range observedLogs.All() {
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), sensitiveErr) || strings.Contains(string(encoded), "password=xyz") {
			t.Fatalf("log entry leaks sensitive provider error: %s", encoded)
		}
	}
	events, err := ms.ListEvents(context.Background(), "Agent", "default", "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Reason != "ChatFailed" {
		t.Fatalf("ChatFailed events = %+v", events)
	}
	if events[0].Message != modelInferenceFailedMessage || strings.Contains(events[0].Message, "password=xyz") {
		t.Fatalf("durable event leaks provider error: %+v", events[0])
	}
}
