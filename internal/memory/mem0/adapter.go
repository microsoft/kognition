// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package mem0 implements the memory.Backend interface by proxying to
// an external Mem0 service (https://mem0.ai). This adapter translates
// Kognition's memory protocol to Mem0's REST API, enabling agents to
// use Mem0 as a managed memory backend via a MemoryClass configuration.
package mem0

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/memory"
)

const mem0MaxPageSize = 200

// Config holds the parameters needed to connect to a Mem0 service.
type Config struct {
	// BaseURL is the Mem0 API endpoint (e.g. "https://api.mem0.ai" or a
	// self-hosted instance URL).
	BaseURL string

	// APIKey is the authentication token for the Mem0 API.
	APIKey string

	// OrgID is the optional Mem0 organization identifier used to scope memories.
	OrgID string

	// ProjectID is the optional Mem0 project identifier.
	ProjectID string
}

// ConfigFromParameters builds a Config from the MemoryClass parameters map.
func ConfigFromParameters(params map[string]string) Config {
	return Config{
		BaseURL:   params["baseURL"],
		APIKey:    params["apiKey"],
		OrgID:     params["orgId"],
		ProjectID: params["projectId"],
	}
}

// Adapter implements memory.Backend backed by an external Mem0 service.
type Adapter struct {
	client *http.Client
	config Config
	logger *zap.Logger
}

// New creates a new Mem0 memory backend adapter.
func New(cfg Config, logger *zap.Logger) *Adapter {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.mem0.ai"
	}
	return &Adapter{
		client: &http.Client{Timeout: 30 * time.Second},
		config: cfg,
		logger: logger.With(zap.String("backend", "mem0")),
	}
}

// Provision is a no-op for Mem0 — memory spaces are created implicitly on
// first write, scoped by the user_id (mapped from BackendHandle).
func (a *Adapter) Provision(_ context.Context, req memory.ProvisionRequest) error {
	a.logger.Info("provisioned mem0 volume (implicit)", zap.String("handle", req.BackendHandle))
	return nil
}

// Deprovision deletes all memories for the volume's user_id scope.
func (a *Adapter) Deprovision(ctx context.Context, req memory.DeprovisionRequest) error {
	url := fmt.Sprintf("%s/v1/memories/", a.config.BaseURL)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("build deprovision request: %w", err)
	}
	a.setHeaders(httpReq)

	q := httpReq.URL.Query()
	q.Set("user_id", req.BackendHandle)
	httpReq.URL.RawQuery = q.Encode()

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("deprovision mem0 volume: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return a.readError(resp)
	}
	a.logger.Info("deprovisioned mem0 volume", zap.String("handle", req.BackendHandle))
	return nil
}

// Store persists a memory record to Mem0.
func (a *Adapter) Store(ctx context.Context, req memory.StoreRequest) (memory.StoreResponse, error) {
	body := mem0AddRequest{
		Messages: []mem0Message{
			{Role: "user", Content: req.Content},
		},
		UserID:   req.BackendHandle,
		Metadata: req.Metadata,
	}
	if req.ID != "" {
		body.MemoryID = req.ID
	}
	if a.config.OrgID != "" {
		body.OrgID = a.config.OrgID
	}
	if a.config.ProjectID != "" {
		body.ProjectID = a.config.ProjectID
	}

	respBody, err := a.doJSON(ctx, http.MethodPost, "/v1/memories/", body)
	if err != nil {
		return memory.StoreResponse{}, fmt.Errorf("store to mem0: %w", err)
	}

	var result mem0AddResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return memory.StoreResponse{}, fmt.Errorf("unmarshal store response: %w", err)
	}

	id := req.ID
	if len(result.Results) > 0 {
		id = result.Results[0].ID
	}
	return memory.StoreResponse{ID: id}, nil
}

// Get retrieves a single memory record from Mem0 by ID.
func (a *Adapter) Get(ctx context.Context, req memory.GetRequest) (memory.Record, error) {
	path := fmt.Sprintf("/v1/memories/%s/", req.ID)

	respBody, err := a.doJSON(ctx, http.MethodGet, path, nil)
	if err != nil {
		return memory.Record{}, fmt.Errorf("get from mem0: %w", err)
	}

	var m mem0Memory
	if err := json.Unmarshal(respBody, &m); err != nil {
		return memory.Record{}, fmt.Errorf("unmarshal get response: %w", err)
	}

	if m.ID == "" {
		return memory.Record{}, memory.ErrRecordNotFound
	}

	return m.toRecord(), nil
}

// List retrieves memories filtered by user_id (BackendHandle) and optional metadata.
func (a *Adapter) List(ctx context.Context, req memory.ListRequest) ([]memory.Record, error) {
	records := make([]memory.Record, 0)
	seenPages := make(map[string]struct{})
	for page := 1; ; page++ {
		result, err := a.listPage(ctx, req.BackendHandle, page)
		if err != nil {
			return nil, err
		}
		pageSignature := memoryPageSignature(result.Results)
		if _, seen := seenPages[pageSignature]; seen {
			return records, nil
		}
		seenPages[pageSignature] = struct{}{}

		for _, m := range result.Results {
			rec := m.toRecord()
			if req.StartTime != nil && rec.Timestamp.Before(*req.StartTime) {
				continue
			}
			if req.EndTime != nil && rec.Timestamp.After(*req.EndTime) {
				continue
			}
			if !matchesMetadata(rec.Metadata, req.Metadata) {
				continue
			}
			records = append(records, rec)
			if req.Limit > 0 && len(records) == req.Limit {
				return records, nil
			}
		}

		if result.Next == "" || len(result.Results) == 0 {
			return records, nil
		}
	}
}

func memoryPageSignature(memories []mem0Memory) string {
	var signature strings.Builder
	for _, m := range memories {
		signature.WriteString(m.ID)
		signature.WriteByte(0)
	}
	return signature.String()
}

func (a *Adapter) listPage(ctx context.Context, backendHandle string, page int) (mem0ListResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, a.config.BaseURL+"/v1/memories/", nil)
	if err != nil {
		return mem0ListResponse{}, fmt.Errorf("build list request: %w", err)
	}
	a.setHeaders(httpReq)

	q := httpReq.URL.Query()
	q.Set("user_id", backendHandle)
	q.Set("page", fmt.Sprintf("%d", page))
	q.Set("page_size", fmt.Sprintf("%d", mem0MaxPageSize))
	httpReq.URL.RawQuery = q.Encode()

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return mem0ListResponse{}, fmt.Errorf("list from mem0: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return mem0ListResponse{}, a.readError(resp)
	}

	var result mem0ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return mem0ListResponse{}, fmt.Errorf("unmarshal list response: %w", err)
	}
	return result, nil
}

// Search rejects vector search because Mem0 V1 accepts text queries but the
// backend contract supplies only vectors. The service falls back to BM25 for
// text queries by listing the complete volume.
func (a *Adapter) Search(context.Context, memory.SearchRequest) ([]memory.SearchResult, error) {
	return nil, errors.New("mem0: vector search is not supported by the v1 API")
}

// Delete removes a memory record from Mem0.
func (a *Adapter) Delete(ctx context.Context, req memory.DeleteRequest) error {
	path := fmt.Sprintf("/v1/memories/%s/", req.ID)

	respBody, err := a.doJSON(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return fmt.Errorf("delete from mem0: %w", err)
	}
	_ = respBody
	return nil
}

// Health checks connectivity to the Mem0 service.
func (a *Adapter) Health(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, a.config.BaseURL+"/v1/memories/", nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}
	a.setHeaders(httpReq)
	q := httpReq.URL.Query()
	q.Set("page_size", "1")
	q.Set("user_id", "__health_check__")
	httpReq.URL.RawQuery = q.Encode()

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("mem0 health check failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("mem0: invalid API key")
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("mem0: service returned %d", resp.StatusCode)
	}
	return nil
}

// --- internal helpers ---

func (a *Adapter) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if a.config.APIKey != "" {
		req.Header.Set("Authorization", "Token "+a.config.APIKey)
	}
}

func (a *Adapter) doJSON(ctx context.Context, method, path string, body any) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, a.config.BaseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	a.setHeaders(httpReq)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, memory.ErrRecordNotFound
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mem0 API error %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

func (a *Adapter) readError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("mem0 API error %d: %s", resp.StatusCode, string(body))
}

func matchesMetadata(record, filter map[string]string) bool {
	for k, v := range filter {
		if record[k] != v {
			return false
		}
	}
	return true
}
