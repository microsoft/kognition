// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package service implements the built-in memory service HTTP handler.
// It exposes the memory.Backend interface over REST/JSON.
package service

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/observability"
)

// Embedder converts text into an embedding vector. When configured on the
// Handler it enables server-side embed-on-write and embed-on-query, keeping
// callers (agent runtimes) free of any embedding dependency.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Handler wraps a memory.Backend and exposes it as HTTP endpoints.
type Handler struct {
	backend       memory.Backend
	embedder      Embedder
	logger        *zap.Logger
	internalToken string
	auditLog      *observability.CognitiveAuditLog
}

// Option configures a Handler.
type Option func(*Handler)

// WithEmbedder enables server-side embedding for store and semantic search.
// When unset, stores persist without embeddings and text queries use BM25.
func WithEmbedder(e Embedder) Option {
	return func(h *Handler) { h.embedder = e }
}

// WithInternalToken restricts every stateful memory endpoint to callers that
// present the configured bearer token. Health remains unauthenticated for
// Kubernetes probes. An empty token preserves the legacy standalone contract.
func WithInternalToken(token string) Option {
	return func(h *Handler) { h.internalToken = token }
}

// WithAuditLog records write-ahead audit entries before memory mutations.
func WithAuditLog(auditLog *observability.CognitiveAuditLog) Option {
	return func(h *Handler) { h.auditLog = auditLog }
}

// NewHandler creates a new memory service handler.
func NewHandler(backend memory.Backend, logger *zap.Logger, opts ...Option) *Handler {
	h := &Handler{backend: backend, logger: logger}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// RegisterRoutes registers all memory service routes on the given mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/memory/store", h.requireInternalToken(h.handleStore))
	mux.HandleFunc("GET /v1/memory/get/{volumeHandle}/{id}", h.requireInternalToken(h.handleGet))
	mux.HandleFunc("POST /v1/memory/list", h.requireInternalToken(h.handleList))
	mux.HandleFunc("POST /v1/memory/search", h.requireInternalToken(h.handleSearch))
	mux.HandleFunc("DELETE /v1/memory/delete/{volumeHandle}/{id}", h.requireInternalToken(h.handleDelete))
	mux.HandleFunc("GET /v1/memory/health", h.handleHealth)
	mux.HandleFunc("POST /v1/memory/provision", h.requireInternalToken(h.handleProvision))
	mux.HandleFunc("POST /v1/memory/deprovision", h.requireInternalToken(h.handleDeprovision))
}

func (h *Handler) requireInternalToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.internalToken != "" {
			const prefix = "Bearer "
			provided := strings.TrimPrefix(r.Header.Get("Authorization"), prefix)
			if !strings.HasPrefix(r.Header.Get("Authorization"), prefix) || subtle.ConstantTimeCompare([]byte(provided), []byte(h.internalToken)) != 1 {
				h.writeError(w, http.StatusUnauthorized, "memory service authorization is required")
				return
			}
		}
		next(w, r)
	}
}

// --- Request/Response types ---

// StoreMemoryRequest is the JSON body for storing a record.
type StoreMemoryRequest struct {
	VolumeHandle string            `json:"volume_handle"`
	ID           string            `json:"id,omitempty"`
	Content      string            `json:"content"`
	Embedding    []float32         `json:"embedding,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Timestamp    *time.Time        `json:"timestamp,omitempty"`
}

// StoreMemoryResponse is returned after storing a record.
type StoreMemoryResponse struct {
	ID string `json:"id"`
}

// ListMemoryRequest is the JSON body for listing records.
type ListMemoryRequest struct {
	VolumeHandle string            `json:"volume_handle"`
	StartTime    *time.Time        `json:"start_time,omitempty"`
	EndTime      *time.Time        `json:"end_time,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Limit        int               `json:"limit,omitempty"`
}

// SearchMemoryRequest is the JSON body for memory search. Vector performs
// semantic search, Query performs lexical search and semantic search when an
// embedder is configured, and providing both enables hybrid search.
type SearchMemoryRequest struct {
	VolumeHandle string            `json:"volume_handle"`
	Vector       []float32         `json:"vector,omitempty"`
	Query        string            `json:"query,omitempty"`
	TopK         int               `json:"top_k"`
	MinScore     float64           `json:"min_score,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// SearchMemoryResult is a single search hit. Score is the backend-native score
// for semantic-only search, normalized BM25 relevance for lexical-only search,
// or normalized reciprocal-rank-fusion confidence for hybrid search. MinScore
// filters source relevance before fusion and the score exposed after fusion.
type SearchMemoryResult struct {
	Record RecordDTO `json:"record"`
	Score  float64   `json:"score"`
}

// RecordDTO is the JSON representation of a memory record.
type RecordDTO struct {
	ID        string            `json:"id"`
	Content   string            `json:"content"`
	Embedding []float32         `json:"embedding,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Timestamp *time.Time        `json:"timestamp,omitempty"`
}

// ProvisionRequest is the JSON body for provisioning a volume.
type ProvisionRequest struct {
	VolumeHandle       string            `json:"volume_handle"`
	EmbeddingDimension int               `json:"embedding_dimension,omitempty"`
	Parameters         map[string]string `json:"parameters,omitempty"`
}

// DeprovisionRequest is the JSON body for deprovisioning a volume.
type DeprovisionRequest struct {
	VolumeHandle string `json:"volume_handle"`
}

// ErrorResponse is returned on errors.
type ErrorResponse struct {
	Error string `json:"error"`
}

// --- Handlers ---

func (h *Handler) handleStore(w http.ResponseWriter, r *http.Request) {
	var req StoreMemoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.VolumeHandle == "" {
		h.writeError(w, http.StatusBadRequest, "volume_handle is required")
		return
	}

	ts := time.Now()
	if req.Timestamp != nil {
		ts = *req.Timestamp
	}

	id := req.ID
	if id == "" {
		id = uuid.NewString()
	}

	// Embed-on-write (best-effort): when an embedder is configured and the
	// caller did not supply a precomputed vector, embed the content so the
	// record is retrievable by semantic search. On failure the raw content is
	// still persisted for keyword/BM25 fallback.
	embedding := req.Embedding
	if len(embedding) == 0 && req.Content != "" && h.embedder != nil {
		if vec, err := h.embedder.Embed(r.Context(), req.Content); err != nil {
			h.logger.Warn("embed-on-write failed; storing without embedding", zap.Error(err))
		} else {
			embedding = vec
		}
	}
	if err := h.recordAudit(r.Context(), req.VolumeHandle, id, "store", h.trustedAuditWorkspace(r, req.VolumeHandle), h.trustedAuditIdentity(r)); err != nil {
		h.logger.Error("memory store audit failed", zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "audit durability failed")
		return
	}

	resp, err := h.backend.Store(r.Context(), memory.StoreRequest{
		BackendHandle: req.VolumeHandle,
		ID:            id,
		Content:       req.Content,
		Embedding:     embedding,
		Metadata:      req.Metadata,
		Timestamp:     ts,
	})
	if err != nil {
		h.logger.Error("store failed", zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "store operation failed")
		return
	}

	h.writeJSON(w, http.StatusCreated, StoreMemoryResponse{ID: resp.ID})
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	volumeHandle := r.PathValue("volumeHandle")
	id := r.PathValue("id")
	if volumeHandle == "" || id == "" {
		h.writeError(w, http.StatusBadRequest, "volume_handle and id are required")
		return
	}

	rec, err := h.backend.Get(r.Context(), memory.GetRequest{
		BackendHandle: volumeHandle,
		ID:            id,
	})
	if err != nil {
		if errors.Is(err, memory.ErrRecordNotFound) {
			h.writeError(w, http.StatusNotFound, "record not found")
			return
		}
		h.logger.Error("get failed", zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "get operation failed")
		return
	}

	h.writeJSON(w, http.StatusOK, recordToDTO(rec))
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	var req ListMemoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.VolumeHandle == "" {
		h.writeError(w, http.StatusBadRequest, "volume_handle is required")
		return
	}

	records, err := h.backend.List(r.Context(), memory.ListRequest{
		BackendHandle: req.VolumeHandle,
		StartTime:     req.StartTime,
		EndTime:       req.EndTime,
		Metadata:      req.Metadata,
		Limit:         req.Limit,
	})
	if err != nil {
		h.logger.Error("list failed", zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "list operation failed")
		return
	}

	dtos := make([]RecordDTO, len(records))
	for i, rec := range records {
		dtos[i] = recordToDTO(rec)
	}
	h.writeJSON(w, http.StatusOK, dtos)
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	var req SearchMemoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.VolumeHandle == "" {
		h.writeError(w, http.StatusBadRequest, "volume_handle is required")
		return
	}

	// Resolve the semantic query when possible. A text query remains usable
	// without an embedder (or during an embedding outage) through BM25 recall.
	vector := req.Vector
	if len(vector) == 0 && req.Query == "" {
		h.writeError(w, http.StatusBadRequest, "vector or query is required")
		return
	}
	if len(vector) == 0 && h.embedder != nil {
		vec, err := h.embedder.Embed(r.Context(), req.Query)
		if err != nil {
			h.logger.Warn("embed-on-query failed; using lexical fallback", zap.Error(err))
		} else {
			vector = vec
		}
	}

	var semanticResults []memory.SearchResult
	if len(vector) > 0 {
		var err error
		semanticResults, err = h.backend.Search(r.Context(), memory.SearchRequest{
			BackendHandle: req.VolumeHandle,
			Vector:        vector,
			TopK:          req.TopK,
			MinScore:      req.MinScore,
			Metadata:      req.Metadata,
		})
		if err != nil {
			if req.Query == "" {
				h.logger.Error("search failed", zap.Error(err))
				h.writeError(w, http.StatusInternalServerError, "search operation failed")
				return
			}
			h.logger.Warn("semantic search failed; using lexical fallback", zap.Error(err))
		}
	}

	var lexicalResults []memory.SearchResult
	if req.Query != "" {
		records, listErr := h.backend.List(r.Context(), memory.ListRequest{
			BackendHandle: req.VolumeHandle,
			Metadata:      req.Metadata,
		})
		if listErr != nil {
			h.logger.Error("lexical search failed", zap.Error(listErr))
			h.writeError(w, http.StatusInternalServerError, "search operation failed")
			return
		}
		lexicalResults = rankBM25(records, req.Query, req.TopK, req.MinScore)
	}

	results := fuseSearchResults(semanticResults, lexicalResults, req.TopK)
	results = filterMinScore(results, req.MinScore)
	dtos := make([]SearchMemoryResult, len(results))
	for i, res := range results {
		dtos[i] = SearchMemoryResult{
			Record: recordToDTO(res.Record),
			Score:  res.Score,
		}
	}
	h.writeJSON(w, http.StatusOK, dtos)
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	volumeHandle := r.PathValue("volumeHandle")
	id := r.PathValue("id")
	if volumeHandle == "" || id == "" {
		h.writeError(w, http.StatusBadRequest, "volume_handle and id are required")
		return
	}

	if err := h.recordAudit(r.Context(), volumeHandle, id, "delete", h.trustedAuditWorkspace(r, volumeHandle), h.trustedAuditIdentity(r)); err != nil {
		h.logger.Error("memory delete audit failed", zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "audit durability failed")
		return
	}
	err := h.backend.Delete(r.Context(), memory.DeleteRequest{
		BackendHandle: volumeHandle,
		ID:            id,
	})
	if err != nil {
		if errors.Is(err, memory.ErrRecordNotFound) {
			h.writeError(w, http.StatusNotFound, "record not found")
			return
		}
		h.logger.Error("delete failed", zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "delete operation failed")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) recordAudit(ctx context.Context, volumeHandle, recordID, verb, workspace, agentIdentity string) error {
	if h.auditLog == nil {
		return nil
	}
	details, err := json.Marshal(map[string]string{"volumeHandle": volumeHandle, "recordID": recordID})
	if err != nil {
		return err
	}
	return h.auditLog.RecordBeforeExec(ctx, observability.AuditEntry{
		ID:            uuid.NewString(),
		Workspace:     workspace,
		AgentIdentity: agentIdentity,
		EventType:     observability.AuditMemoryOp,
		SubjectKind:   "MemoryVolume",
		SubjectRef:    volumeHandle + "/" + recordID,
		Verb:          verb,
		Details:       details,
	})
}

func (h *Handler) trustedAuditIdentity(request *http.Request) string {
	if h.internalToken == "" {
		return ""
	}
	return request.Header.Get(memory.TrustedAgentIdentityHeader)
}

func (h *Handler) trustedAuditWorkspace(request *http.Request, fallback string) string {
	if h.internalToken == "" {
		return fallback
	}
	if workspace := request.Header.Get(memory.TrustedWorkspaceHeader); workspace != "" {
		return workspace
	}
	return fallback
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := h.backend.Health(r.Context()); err != nil {
		h.writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleProvision(w http.ResponseWriter, r *http.Request) {
	var req ProvisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.VolumeHandle == "" {
		h.writeError(w, http.StatusBadRequest, "volume_handle is required")
		return
	}

	err := h.backend.Provision(r.Context(), memory.ProvisionRequest{
		BackendHandle:      req.VolumeHandle,
		EmbeddingDimension: req.EmbeddingDimension,
		Parameters:         req.Parameters,
	})
	if err != nil {
		h.logger.Error("provision failed", zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "provision operation failed")
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) handleDeprovision(w http.ResponseWriter, r *http.Request) {
	var req DeprovisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.VolumeHandle == "" {
		h.writeError(w, http.StatusBadRequest, "volume_handle is required")
		return
	}

	err := h.backend.Deprovision(r.Context(), memory.DeprovisionRequest{
		BackendHandle: req.VolumeHandle,
	})
	if err != nil {
		h.logger.Error("deprovision failed", zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "deprovision operation failed")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Helpers ---

func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("failed to write response", zap.Error(err))
	}
}

func (h *Handler) writeError(w http.ResponseWriter, status int, msg string) {
	h.writeJSON(w, status, ErrorResponse{Error: msg})
}

func recordToDTO(rec memory.Record) RecordDTO {
	dto := RecordDTO{
		ID:        rec.ID,
		Content:   rec.Content,
		Embedding: rec.Embedding,
		Metadata:  rec.Metadata,
	}
	if !rec.Timestamp.IsZero() {
		t := rec.Timestamp
		dto.Timestamp = &t
	}
	return dto
}
