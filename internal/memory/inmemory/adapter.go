// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package inmemory implements the memory.Backend interface using in-process
// data structures. It provides a zero-dependency, zero-config memory backend
// suitable for development, testing, and lightweight production workloads.
// Vector similarity search uses brute-force cosine similarity.
package inmemory

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/kognition-ai/kognition/internal/memory"
)

// Adapter implements memory.Backend with in-process storage.
type Adapter struct {
	mu      sync.RWMutex
	volumes map[string]*volume // backendHandle -> volume
}

// volume holds the records for a single provisioned memory volume.
type volume struct {
	records map[string]memory.Record // recordID -> Record
}

// New creates a new in-memory backend adapter.
func New() *Adapter {
	return &Adapter{
		volumes: make(map[string]*volume),
	}
}

func (a *Adapter) Provision(_ context.Context, req memory.ProvisionRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.volumes[req.BackendHandle]; exists {
		// Already provisioned – idempotent.
		return nil
	}
	a.volumes[req.BackendHandle] = &volume{
		records: make(map[string]memory.Record),
	}
	return nil
}

func (a *Adapter) Deprovision(_ context.Context, req memory.DeprovisionRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.volumes, req.BackendHandle)
	return nil
}

func (a *Adapter) Store(_ context.Context, req memory.StoreRequest) (memory.StoreResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	vol, ok := a.volumes[req.BackendHandle]
	if !ok {
		return memory.StoreResponse{}, fmt.Errorf("volume %s not provisioned", req.BackendHandle)
	}

	ts := req.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	// Clone metadata to avoid aliasing.
	meta := make(map[string]string, len(req.Metadata))
	for k, v := range req.Metadata {
		meta[k] = v
	}

	// Clone embedding.
	var emb []float32
	if len(req.Embedding) > 0 {
		emb = make([]float32, len(req.Embedding))
		copy(emb, req.Embedding)
	}

	vol.records[req.ID] = memory.Record{
		ID:        req.ID,
		Content:   req.Content,
		Embedding: emb,
		Metadata:  meta,
		Timestamp: ts.Truncate(time.Millisecond),
	}
	return memory.StoreResponse{ID: req.ID}, nil
}

func (a *Adapter) Get(_ context.Context, req memory.GetRequest) (memory.Record, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	vol, ok := a.volumes[req.BackendHandle]
	if !ok {
		return memory.Record{}, fmt.Errorf("volume %s not provisioned", req.BackendHandle)
	}

	rec, exists := vol.records[req.ID]
	if !exists {
		return memory.Record{}, memory.ErrRecordNotFound
	}
	return cloneRecord(rec), nil
}

func (a *Adapter) List(_ context.Context, req memory.ListRequest) ([]memory.Record, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	vol, ok := a.volumes[req.BackendHandle]
	if !ok {
		return nil, fmt.Errorf("volume %s not provisioned", req.BackendHandle)
	}

	var results []memory.Record
	for _, rec := range vol.records {
		if req.StartTime != nil && rec.Timestamp.Before(*req.StartTime) {
			continue
		}
		if req.EndTime != nil && rec.Timestamp.After(*req.EndTime) {
			continue
		}
		if !matchMetadata(rec.Metadata, req.Metadata) {
			continue
		}
		results = append(results, cloneRecord(rec))
	}

	// Sort by timestamp descending (newest first).
	sort.Slice(results, func(i, j int) bool {
		return results[i].Timestamp.After(results[j].Timestamp)
	})

	if req.Limit > 0 && len(results) > req.Limit {
		results = results[:req.Limit]
	}
	return results, nil
}

func (a *Adapter) Search(_ context.Context, req memory.SearchRequest) ([]memory.SearchResult, error) {
	if len(req.Vector) == 0 {
		return nil, fmt.Errorf("search vector must not be empty")
	}

	a.mu.RLock()
	defer a.mu.RUnlock()

	vol, ok := a.volumes[req.BackendHandle]
	if !ok {
		return nil, fmt.Errorf("volume %s not provisioned", req.BackendHandle)
	}

	topK := req.TopK
	if topK <= 0 {
		topK = 10
	}

	var candidates []memory.SearchResult
	for _, rec := range vol.records {
		if len(rec.Embedding) == 0 {
			continue
		}
		if !matchMetadata(rec.Metadata, req.Metadata) {
			continue
		}
		score := cosineSimilarity(req.Vector, rec.Embedding)
		if req.MinScore > 0 && score < req.MinScore {
			continue
		}
		candidates = append(candidates, memory.SearchResult{
			Record: cloneRecord(rec),
			Score:  score,
		})
	}

	// Sort by score descending.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})

	if len(candidates) > topK {
		candidates = candidates[:topK]
	}
	return candidates, nil
}

func (a *Adapter) Delete(_ context.Context, req memory.DeleteRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	vol, ok := a.volumes[req.BackendHandle]
	if !ok {
		return fmt.Errorf("volume %s not provisioned", req.BackendHandle)
	}

	if _, exists := vol.records[req.ID]; !exists {
		return fmt.Errorf("record %s: %w", req.ID, memory.ErrRecordNotFound)
	}
	delete(vol.records, req.ID)
	return nil
}

func (a *Adapter) Health(_ context.Context) error {
	return nil
}

// --- Helpers ---

func cloneRecord(rec memory.Record) memory.Record {
	clone := rec
	clone.Embedding = append([]float32(nil), rec.Embedding...)
	clone.Metadata = make(map[string]string, len(rec.Metadata))
	for key, value := range rec.Metadata {
		clone.Metadata[key] = value
	}
	return clone
}

// matchMetadata returns true if the record metadata contains all filter pairs.
func matchMetadata(recMeta, filterMeta map[string]string) bool {
	for k, v := range filterMeta {
		if recMeta[k] != v {
			return false
		}
	}
	return true
}

// cosineSimilarity computes the cosine similarity between two vectors.
// Returns 0 if either vector has zero magnitude.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
