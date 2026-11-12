// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"sync"

	"github.com/kognition-ai/kognition/internal/store"
)

// memStore is a minimal in-memory store.Store for admission tests.
type memStore struct {
	mu   sync.RWMutex
	data map[string][]byte
}

func newMemStore() *memStore {
	return &memStore{data: make(map[string][]byte)}
}

func (m *memStore) key(kind, ns, name string) string {
	return kind + "/" + ns + "/" + name
}

func (m *memStore) Ping(_ context.Context) error { return nil }

func (m *memStore) Get(_ context.Context, kind, ns, name string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.data[m.key(kind, ns, name)]
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
			out = append(out, v)
		}
	}
	return out, nil
}

func (m *memStore) Create(_ context.Context, kind, ns, name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(kind, ns, name)
	if _, ok := m.data[k]; ok {
		return store.ErrAlreadyExists
	}
	m.data[k] = data
	return nil
}

func (m *memStore) Update(_ context.Context, kind, ns, name string, data []byte, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(kind, ns, name)
	if _, ok := m.data[k]; !ok {
		return store.ErrNotFound
	}
	m.data[k] = data
	return nil
}

func (m *memStore) Delete(_ context.Context, kind, ns, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(kind, ns, name)
	if _, ok := m.data[k]; !ok {
		return store.ErrNotFound
	}
	delete(m.data, k)
	return nil
}

func (m *memStore) Watch(_ context.Context, _ string, _ store.ListOptions) (<-chan store.WatchEvent, error) {
	return make(chan store.WatchEvent), nil
}

func (m *memStore) CreateRevision(_ context.Context, _, _, _ string, _ int64, _, _ []byte) error {
	return nil
}

func (m *memStore) ListRevisions(_ context.Context, _, _, _ string) ([]store.Revision, error) {
	return nil, nil
}

func (m *memStore) GetRevision(_ context.Context, _, _, _ string, _ int64) (*store.Revision, error) {
	return nil, store.ErrNotFound
}

func (m *memStore) AppendEvent(_ context.Context, _, _, _ string, _ store.InstanceEvent) error {
	return nil
}

func (m *memStore) ListEvents(_ context.Context, _, _, _ string) ([]store.InstanceEvent, error) {
	return nil, nil
}
