// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package driver

import (
	"context"
	"strconv"
	"sync"

	"github.com/kognition-ai/kognition/internal/collaboration"
)

// MemoryLedger is a process-local Ledger for tests and single-process tooling.
// It encodes each state so it enforces the same bounds as a durable store.
type MemoryLedger struct {
	mu  sync.Mutex
	raw []byte
	rev int
}

// NewMemoryLedger returns an empty MemoryLedger.
func NewMemoryLedger() *MemoryLedger { return &MemoryLedger{} }

// Load implements Ledger.
func (m *MemoryLedger) Load(context.Context) (collaboration.State, Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.raw == nil {
		return collaboration.State{}, "", ErrNotFound
	}
	st, err := collaboration.Decode(m.raw)
	return st, Version(strconv.Itoa(m.rev)), err
}

// Create implements Ledger.
func (m *MemoryLedger) Create(_ context.Context, st collaboration.State) (Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.raw != nil {
		return "", ErrExists
	}
	raw, err := collaboration.Encode(st)
	if err != nil {
		return "", err
	}
	m.raw, m.rev = raw, 1
	return Version(strconv.Itoa(m.rev)), nil
}

// Save implements Ledger.
func (m *MemoryLedger) Save(_ context.Context, st collaboration.State, expect Version) (Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.raw == nil {
		return "", ErrNotFound
	}
	if expect != Version(strconv.Itoa(m.rev)) {
		return "", ErrConflict
	}
	raw, err := collaboration.Encode(st)
	if err != nil {
		return "", err
	}
	m.raw = raw
	m.rev++
	return Version(strconv.Itoa(m.rev)), nil
}
