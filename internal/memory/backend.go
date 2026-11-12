// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package memory defines the data-plane interface for agent memory backends.
// Implementations provide vector storage, similarity search, and CRUD
// operations on memory records backed by concrete storage engines.
package memory

import (
	"context"
	"errors"
	"time"
)

// ErrRecordNotFound is returned when a requested record does not exist.
var ErrRecordNotFound = errors.New("record not found")

const (
	// TrustedAgentIdentityHeader is set by the policy gateway after it verifies
	// a runtime memory capability and the current AgentIdentity.
	TrustedAgentIdentityHeader = "X-Kognition-Agent-Identity"
	// TrustedWorkspaceHeader is set by the policy gateway from the verified
	// capability namespace so memory audit retention follows Workspace policy.
	TrustedWorkspaceHeader = "X-Kognition-Workspace"
)

// Backend defines the data-plane contract for memory storage engines.
// Each implementation (pgvector, redis, in-memory) operates on a specific
// volume identified by its backendHandle.
type Backend interface {
	// Provision creates the backing schema/collection for a volume.
	Provision(ctx context.Context, req ProvisionRequest) error

	// Deprovision removes the backing schema/collection for a volume.
	Deprovision(ctx context.Context, req DeprovisionRequest) error

	// Store persists a memory record with optional embedding vector.
	Store(ctx context.Context, req StoreRequest) (StoreResponse, error)

	// Get retrieves a single record by ID.
	Get(ctx context.Context, req GetRequest) (Record, error)

	// List retrieves records by filter criteria (time range, metadata).
	List(ctx context.Context, req ListRequest) ([]Record, error)

	// Search performs vector similarity search (k-NN).
	Search(ctx context.Context, req SearchRequest) ([]SearchResult, error)

	// Delete removes a record by ID.
	Delete(ctx context.Context, req DeleteRequest) error

	// Health returns the backend health status.
	Health(ctx context.Context) error
}

// ProvisionRequest contains parameters for provisioning a volume's backing store.
type ProvisionRequest struct {
	// BackendHandle is the provisioner-generated identifier for the volume.
	BackendHandle string

	// EmbeddingDimension is the vector dimension to use (e.g. 1536 for ada-002).
	// If zero, the backend uses its default dimension.
	EmbeddingDimension int

	// Parameters are provisioner-specific configuration pairs.
	Parameters map[string]string
}

// DeprovisionRequest contains parameters for removing a volume's backing store.
type DeprovisionRequest struct {
	BackendHandle string
}

// StoreRequest contains the record to persist.
type StoreRequest struct {
	BackendHandle string
	ID            string
	Content       string
	Embedding     []float32
	Metadata      map[string]string
	Timestamp     time.Time
}

// StoreResponse is returned after persisting a record.
type StoreResponse struct {
	ID string
}

// GetRequest identifies a single record to retrieve.
type GetRequest struct {
	BackendHandle string
	ID            string
}

// ListRequest defines filter criteria for listing records.
type ListRequest struct {
	BackendHandle string

	// TimeRange filters records by timestamp (inclusive).
	StartTime *time.Time
	EndTime   *time.Time

	// Metadata filters by exact key-value match.
	Metadata map[string]string

	// Limit caps the number of returned records. Zero means no limit.
	Limit int
}

// SearchRequest defines a vector similarity search query.
type SearchRequest struct {
	BackendHandle string
	Vector        []float32
	TopK          int
	MinScore      float64

	// Metadata is an optional pre-filter on record metadata.
	Metadata map[string]string
}

// SearchResult is a single result from a similarity search.
type SearchResult struct {
	Record Record
	Score  float64
}

// Record represents a memory record stored in the backend.
type Record struct {
	ID        string
	Content   string
	Embedding []float32
	Metadata  map[string]string
	Timestamp time.Time
}

// DeleteRequest identifies a record to remove.
type DeleteRequest struct {
	BackendHandle string
	ID            string
}
