// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/memory/service"
)

const memoryServiceResponseLimit = 64 * 1024

// MemoryBackendProvisioner manages backend handles represented by MemoryVolumes.
type MemoryBackendProvisioner interface {
	Provision(context.Context, memory.ProvisionRequest) error
	Deprovision(context.Context, memory.DeprovisionRequest) error
}

type memoryServiceProvisioner struct {
	endpoint      string
	client        *http.Client
	internalToken string
}

// NewMemoryServiceProvisioner creates an HTTP provisioner. An empty endpoint disables provisioning.
func NewMemoryServiceProvisioner(endpoint string, client *http.Client, internalTokens ...string) MemoryBackendProvisioner {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return nil
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	token := ""
	if len(internalTokens) > 0 {
		token = internalTokens[0]
	}
	return &memoryServiceProvisioner{endpoint: endpoint, client: client, internalToken: token}
}

func (p *memoryServiceProvisioner) Provision(ctx context.Context, req memory.ProvisionRequest) error {
	return p.post(ctx, "/v1/memory/provision", service.ProvisionRequest{
		VolumeHandle:       req.BackendHandle,
		EmbeddingDimension: req.EmbeddingDimension,
		Parameters:         req.Parameters,
	})
}

func (p *memoryServiceProvisioner) Deprovision(ctx context.Context, req memory.DeprovisionRequest) error {
	return p.post(ctx, "/v1/memory/deprovision", service.DeprovisionRequest{VolumeHandle: req.BackendHandle})
}

func (p *memoryServiceProvisioner) post(ctx context.Context, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode memory service request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build memory service request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.internalToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.internalToken)
	}
	response, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("call memory service: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, memoryServiceResponseLimit))
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(response.Body, memoryServiceResponseLimit))
	var errorResponse service.ErrorResponse
	if json.Unmarshal(raw, &errorResponse) == nil && errorResponse.Error != "" {
		return fmt.Errorf("memory service returned %s: %s", response.Status, errorResponse.Error)
	}
	return fmt.Errorf("memory service returned %s", response.Status)
}
