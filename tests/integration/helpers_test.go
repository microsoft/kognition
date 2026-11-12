// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/apiserver"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/store"
)

// ---------------------------------------------------------------------------
// In-memory store implementation
// ---------------------------------------------------------------------------

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

func newMemStore() *memStore {
	return &memStore{
		resources: make(map[string]memEntry),
		watchers:  make(map[string][]chan store.WatchEvent),
		revisions: make(map[string][]store.Revision),
		events:    make(map[string][]store.InstanceEvent),
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
	return out, nil
}

func (m *memStore) Create(_ context.Context, kind, ns, name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := storeKey(kind, ns, name)
	if _, ok := m.resources[key]; ok {
		return store.ErrAlreadyExists
	}
	// Patch resourceVersion into stored JSON so Get returns consistent data.
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err == nil {
		if meta, ok := obj["metadata"].(map[string]any); ok {
			meta["resourceVersion"] = float64(1)
			obj["metadata"] = meta
			if patched, err := json.Marshal(obj); err == nil {
				data = patched
			}
		}
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

// ---------------------------------------------------------------------------
// Mock CPI Driver
// ---------------------------------------------------------------------------

// mockCPIDriver is a test CPI driver that returns canned completions.
type mockCPIDriver struct {
	name         string
	probeResult  *cpi.HealthResult
	probeErr     error
	completionFn func(ctx context.Context, req *cpi.CompletionRequest) (*cpi.CompletionResponse, error)
	embedFn      func(ctx context.Context, req *cpi.EmbedRequest) (*cpi.EmbedResponse, error)
	completions  []cpi.CompletionRequest
	probes       []probeInvocation
	mu           sync.Mutex
}

type probeInvocation struct {
	model       v1alpha1.ModelSpec
	credentials *cpi.Credentials
}

func newMockCPIDriver(name string) *mockCPIDriver {
	return &mockCPIDriver{
		name: name,
		probeResult: &cpi.HealthResult{
			Available:    true,
			Latency:      "15ms",
			Capabilities: []string{"chat", "embeddings"},
		},
		completionFn: func(_ context.Context, req *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
			return &cpi.CompletionResponse{
				Content:      "Hello! I'm the mock agent.",
				FinishReason: "stop",
				Usage: cpi.Usage{
					PromptTokens:     10,
					CompletionTokens: 8,
					TotalTokens:      18,
					Reported:         true,
				},
			}, nil
		},
		embedFn: func(_ context.Context, req *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
			embeddings := make([][]float64, len(req.Inputs))
			for i := range req.Inputs {
				embeddings[i] = make([]float64, 8)
				for j := range embeddings[i] {
					embeddings[i][j] = float64(i+j) * 0.1
				}
			}
			return &cpi.EmbedResponse{
				Embeddings: embeddings,
				Usage:      cpi.Usage{TotalTokens: len(req.Inputs) * 5},
			}, nil
		},
	}
}

func (d *mockCPIDriver) Name() string { return d.name }

func (d *mockCPIDriver) Validate(_ context.Context, _ *v1alpha1.ModelSpec, _ *v1alpha1.ModelClassSpec) error {
	return nil
}

func (d *mockCPIDriver) Probe(_ context.Context, model *v1alpha1.ModelSpec, credentials *cpi.Credentials) (*cpi.HealthResult, error) {
	d.mu.Lock()
	d.probes = append(d.probes, probeInvocation{
		model:       cloneModelSpec(*model),
		credentials: cloneCredentials(credentials),
	})
	d.mu.Unlock()
	return d.probeResult, d.probeErr
}

func (d *mockCPIDriver) Complete(ctx context.Context, req *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	d.mu.Lock()
	d.completions = append(d.completions, cloneCompletionRequest(*req))
	d.mu.Unlock()
	return d.completionFn(ctx, req)
}

func (d *mockCPIDriver) Embed(ctx context.Context, req *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return d.embedFn(ctx, req)
}

func (d *mockCPIDriver) Invocations() []cpi.CompletionRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([]cpi.CompletionRequest, len(d.completions))
	for i := range d.completions {
		result[i] = cloneCompletionRequest(d.completions[i])
	}
	return result
}

func (d *mockCPIDriver) ProbeInvocations() []probeInvocation {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([]probeInvocation, len(d.probes))
	for i := range d.probes {
		result[i] = probeInvocation{
			model:       cloneModelSpec(d.probes[i].model),
			credentials: cloneCredentials(d.probes[i].credentials),
		}
	}
	return result
}

func cloneCompletionRequest(req cpi.CompletionRequest) cpi.CompletionRequest {
	req.Messages = append([]cpi.Message(nil), req.Messages...)
	req.Parameters = cloneStringMap(req.Parameters)
	req.Tools = append([]cpi.ToolDefinition(nil), req.Tools...)
	req.Creds = cloneCredentials(req.Creds)
	return req
}

func cloneModelSpec(spec v1alpha1.ModelSpec) v1alpha1.ModelSpec {
	spec.Capabilities = append([]string(nil), spec.Capabilities...)
	spec.Parameters = cloneStringMap(spec.Parameters)
	return spec
}

func cloneCredentials(credentials *cpi.Credentials) *cpi.Credentials {
	if credentials == nil {
		return nil
	}
	return &cpi.Credentials{Type: credentials.Type, Data: cloneStringMap(credentials.Data)}
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

// ---------------------------------------------------------------------------
// Mock Memory Backend
// ---------------------------------------------------------------------------

type memBackend struct {
	mu      sync.Mutex
	volumes map[string]bool
	records map[string]map[string]memory.Record // backendHandle -> id -> Record
}

func newMemBackend() *memBackend {
	return &memBackend{
		volumes: make(map[string]bool),
		records: make(map[string]map[string]memory.Record),
	}
}

func (b *memBackend) Provision(_ context.Context, req memory.ProvisionRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.volumes[req.BackendHandle] = true
	b.records[req.BackendHandle] = make(map[string]memory.Record)
	return nil
}

func (b *memBackend) Deprovision(_ context.Context, req memory.DeprovisionRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.volumes, req.BackendHandle)
	delete(b.records, req.BackendHandle)
	return nil
}

func (b *memBackend) Store(_ context.Context, req memory.StoreRequest) (memory.StoreResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	recs, ok := b.records[req.BackendHandle]
	if !ok {
		return memory.StoreResponse{}, fmt.Errorf("volume not provisioned: %s", req.BackendHandle)
	}
	recs[req.ID] = memory.Record{
		ID:        req.ID,
		Content:   req.Content,
		Embedding: req.Embedding,
		Metadata:  req.Metadata,
		Timestamp: req.Timestamp,
	}
	return memory.StoreResponse{ID: req.ID}, nil
}

func (b *memBackend) Get(_ context.Context, req memory.GetRequest) (memory.Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	recs, ok := b.records[req.BackendHandle]
	if !ok {
		return memory.Record{}, memory.ErrRecordNotFound
	}
	rec, ok := recs[req.ID]
	if !ok {
		return memory.Record{}, memory.ErrRecordNotFound
	}
	return rec, nil
}

func (b *memBackend) List(_ context.Context, req memory.ListRequest) ([]memory.Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	recs, ok := b.records[req.BackendHandle]
	if !ok {
		return nil, nil
	}
	var out []memory.Record
	for _, r := range recs {
		out = append(out, r)
	}
	return out, nil
}

func (b *memBackend) Search(_ context.Context, req memory.SearchRequest) ([]memory.SearchResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	recs, ok := b.records[req.BackendHandle]
	if !ok {
		return nil, nil
	}
	// Simple: return all records as results (scored by index position).
	var results []memory.SearchResult
	for _, r := range recs {
		if len(results) >= req.TopK {
			break
		}
		results = append(results, memory.SearchResult{
			Record: r,
			Score:  0.95, // mock high similarity
		})
	}
	return results, nil
}

func (b *memBackend) Delete(_ context.Context, req memory.DeleteRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	recs, ok := b.records[req.BackendHandle]
	if !ok {
		return memory.ErrRecordNotFound
	}
	if _, exists := recs[req.ID]; !exists {
		return memory.ErrRecordNotFound
	}
	delete(recs, req.ID)
	return nil
}

func (b *memBackend) Health(_ context.Context) error { return nil }

// ---------------------------------------------------------------------------
// Test Harness
// ---------------------------------------------------------------------------

// testHarness bundles the server, store, and mock drivers for integration tests.
type testHarness struct {
	Server   *apiserver.Server
	Store    *memStore
	Registry *cpi.Registry
	Driver   *mockCPIDriver
	Memory   *memBackend
	Logger   *zap.Logger
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	ms := newMemStore()
	logger, _ := zap.NewDevelopment()
	registry := cpi.NewRegistry()
	driver := newMockCPIDriver("test-provider")
	if err := registry.Register(driver); err != nil {
		t.Fatalf("register mock driver: %v", err)
	}

	srv := apiserver.New(
		apiserver.Config{Addr: ":0"},
		ms,
		logger,
		apiserver.WithCPIRegistry(registry),
	)

	return &testHarness{
		Server:   srv,
		Store:    ms,
		Registry: registry,
		Driver:   driver,
		Memory:   newMemBackend(),
		Logger:   logger,
	}
}

// doReq performs an HTTP request against the server mux.
func (h *testHarness) doReq(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
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
	h.Server.ServeHTTP(rec, req)
	return rec
}

// doReqRaw performs an HTTP request with raw bytes.
func (h *testHarness) doReqRaw(t *testing.T, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var b io.Reader
	if body != nil {
		b = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, b)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Server.ServeHTTP(rec, req)
	return rec
}

// seedResource creates a resource directly in the store.
func (h *testHarness) seedResource(t *testing.T, kind, ns, name string, obj any) {
	t.Helper()
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal %s/%s/%s: %v", kind, ns, name, err)
	}
	if err := h.Store.Create(context.Background(), kind, ns, name, data); err != nil {
		t.Fatalf("seed %s/%s/%s: %v", kind, ns, name, err)
	}
}

// getResource retrieves and unmarshals a resource from the store.
func (h *testHarness) getResource(t *testing.T, kind, ns, name string, out any) {
	t.Helper()
	data, err := h.Store.Get(context.Background(), kind, ns, name)
	if err != nil {
		t.Fatalf("get %s/%s/%s: %v", kind, ns, name, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("unmarshal %s/%s/%s: %v", kind, ns, name, err)
	}
}

// ---------------------------------------------------------------------------
// Standalone wrappers (used by tests that pass harness as argument)
// ---------------------------------------------------------------------------

func seedResource(t *testing.T, h *testHarness, kind, ns, name string, obj any) {
	h.seedResource(t, kind, ns, name, obj)
}

// ---------------------------------------------------------------------------
// Helper Builders
// ---------------------------------------------------------------------------

func makeAgent(name, namespace, modelRef string, phase v1alpha1.Phase) v1alpha1.Agent {
	return v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "Agent",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			ResourceVersion: 1,
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef: v1alpha1.LocalObjectReference{Name: modelRef},
			Instructions: v1alpha1.AgentInstructions{
				Inline: "You are a helpful assistant.",
			},
		},
		Status: v1alpha1.AgentStatus{
			Phase: phase,
		},
	}
}

func makeModel(name, namespace, className, modelID, endpoint string) v1alpha1.Model {
	return v1alpha1.Model{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "Model",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			ResourceVersion: 1,
		},
		Spec: v1alpha1.ModelSpec{
			ClassName:  className,
			ModelID:    modelID,
			Endpoint:   endpoint,
			Parameters: map[string]string{"temperature": "0.7"},
		},
	}
}

func makeModelClass(name, namespace, provisioner string) v1alpha1.ModelClass {
	return v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "ModelClass",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			ResourceVersion: 1,
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: provisioner,
		},
	}
}

func makeIntentGateway(name, namespace string, routes []v1alpha1.IntentRoute, phase v1alpha1.Phase) v1alpha1.IntentGateway {
	return v1alpha1.IntentGateway{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "IntentGateway",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			ResourceVersion: 1,
		},
		Spec: v1alpha1.IntentGatewaySpec{
			Routes: routes,
		},
		Status: v1alpha1.IntentGatewayStatus{
			Phase: phase,
		},
	}
}

func makeTool(name, namespace, protocol, endpoint string) v1alpha1.Tool {
	return v1alpha1.Tool{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "Tool",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			ResourceVersion: 1,
		},
		Spec: v1alpha1.ToolSpec{
			Protocol:    v1alpha1.ToolProtocol(protocol),
			Endpoint:    endpoint,
			Description: "Test tool " + name,
		},
	}
}
