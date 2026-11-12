// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	v1alpha1 "github.com/kognition-ai/kognition/api/v1alpha1"
)

// ProviderHostConfig configures the CPI provider host.
type ProviderHostConfig struct {
	// SocketDir is the directory to watch for CPI provider socket files.
	SocketDir string

	// HealthCheckInterval is how often to health-check providers.
	HealthCheckInterval time.Duration

	// HealthCheckTimeout is the max time to wait for a health response.
	HealthCheckTimeout time.Duration

	// ConnectTimeout is the max time for initial gRPC connection.
	ConnectTimeout time.Duration
}

// DefaultProviderHostConfig returns sensible defaults.
func DefaultProviderHostConfig() ProviderHostConfig {
	return ProviderHostConfig{
		SocketDir:           "/var/run/kognition/cpi",
		HealthCheckInterval: 15 * time.Second,
		HealthCheckTimeout:  5 * time.Second,
		ConnectTimeout:      10 * time.Second,
	}
}

// ProviderConnection tracks a single CPI provider's connection and metadata.
type ProviderConnection struct {
	Info v1alpha1.ProviderEntry
	Conn *grpc.ClientConn
	mu   sync.RWMutex
}

// ProviderHost discovers, connects, registers, and health-checks CPI providers.
type ProviderHost struct {
	cfg       ProviderHostConfig
	logger    *zap.Logger
	mu        sync.RWMutex
	providers map[string]*ProviderConnection // keyed by socket path
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

// NewProviderHost creates a CPI provider host.
func NewProviderHost(cfg ProviderHostConfig, logger *zap.Logger) *ProviderHost {
	return &ProviderHost{
		cfg:       cfg,
		logger:    logger,
		providers: make(map[string]*ProviderConnection),
		stopCh:    make(chan struct{}),
	}
}

// Start begins provider discovery and health monitoring.
func (h *ProviderHost) Start(ctx context.Context) error {
	if err := os.MkdirAll(h.cfg.SocketDir, 0750); err != nil {
		return fmt.Errorf("cpi provider host: create socket dir: %w", err)
	}

	if err := h.discover(); err != nil {
		h.logger.Warn("initial CPI provider discovery failed", zap.Error(err))
	}

	h.wg.Add(2)
	go h.discoveryLoop(ctx)
	go h.healthLoop(ctx)

	return nil
}

// Stop shuts down the provider host and disconnects all providers.
func (h *ProviderHost) Stop() {
	close(h.stopCh)
	h.wg.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()

	for _, pc := range h.providers {
		pc.mu.RLock()
		conn := pc.Conn
		pc.mu.RUnlock()
		if conn != nil {
			_ = conn.Close()
		}
	}
	h.providers = make(map[string]*ProviderConnection)
}

// GetProviders returns a snapshot of all registered providers.
func (h *ProviderHost) GetProviders() []v1alpha1.ProviderEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result := make([]v1alpha1.ProviderEntry, 0, len(h.providers))
	for _, pc := range h.providers {
		pc.mu.RLock()
		result = append(result, cloneProviderEntry(pc.Info))
		pc.mu.RUnlock()
	}
	return result
}

// GetProvidersByDimension returns providers matching the given dimension.
func (h *ProviderHost) GetProvidersByDimension(dimension string) []v1alpha1.ProviderEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var result []v1alpha1.ProviderEntry
	for _, pc := range h.providers {
		pc.mu.RLock()
		if pc.Info.Dimension == dimension {
			result = append(result, cloneProviderEntry(pc.Info))
		}
		pc.mu.RUnlock()
	}
	return result
}

// GetReadyProvider returns the first ready provider with the given dimension and name.
func (h *ProviderHost) GetReadyProvider(dimension, name string) (*v1alpha1.ProviderEntry, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, pc := range h.providers {
		pc.mu.RLock()
		matches := pc.Info.HealthStatus == v1alpha1.ProviderHealthReady &&
			(dimension == "" || pc.Info.Dimension == dimension) &&
			(name == "" || pc.Info.Name == name)
		if matches {
			entry := cloneProviderEntry(pc.Info)
			pc.mu.RUnlock()
			return &entry, nil
		}
		pc.mu.RUnlock()
	}
	return nil, fmt.Errorf("cpi provider host: no ready provider (dimension=%q, name=%q)", dimension, name)
}

// ConnectionForSocket returns the connection for a given socket path (for testing).
func (h *ProviderHost) ConnectionForSocket(socketPath string) (*ProviderConnection, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	pc, ok := h.providers[socketPath]
	return pc, ok
}

// discover scans the socket directory for new provider socket files.
func (h *ProviderHost) discover() error {
	entries, err := os.ReadDir(h.cfg.SocketDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cpi provider host: read socket dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".sock") {
			continue
		}

		socketPath := filepath.Join(h.cfg.SocketDir, entry.Name())

		h.mu.RLock()
		_, exists := h.providers[socketPath]
		h.mu.RUnlock()

		if !exists {
			h.logger.Info("discovered CPI provider socket", zap.String("path", socketPath))
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				h.connectAndRegister(socketPath)
			}()
		}
	}

	// Remove stale entries.
	h.mu.Lock()
	for socketPath, pc := range h.providers {
		if _, err := os.Stat(socketPath); errors.Is(err, os.ErrNotExist) {
			pc.mu.Lock()
			name := pc.Info.Name
			conn := pc.Conn
			pc.mu.Unlock()
			h.logger.Info("CPI provider socket removed",
				zap.String("name", name),
				zap.String("path", socketPath))
			if conn != nil {
				_ = conn.Close()
			}
			delete(h.providers, socketPath)
		}
	}
	h.mu.Unlock()

	return nil
}

// connectAndRegister establishes a connection and performs identity registration.
func (h *ProviderHost) connectAndRegister(socketPath string) {
	h.mu.Lock()
	if _, exists := h.providers[socketPath]; exists {
		// Another goroutine already registered this socket (race between
		// concurrent discover() calls). Bail out to avoid connection leak.
		h.mu.Unlock()
		return
	}
	pc := &ProviderConnection{
		Info: v1alpha1.ProviderEntry{
			SocketPath:      socketPath,
			ConnectionState: v1alpha1.ProviderConnectionConnecting,
			HealthStatus:    v1alpha1.ProviderHealthUnknown,
		},
	}
	h.providers[socketPath] = pc
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.ConnectTimeout)
	defer cancel()

	conn, err := grpc.DialContext(ctx, "passthrough:///unix", //nolint:staticcheck // Blocking connection establishment is required for provider readiness.
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(JSONCodec{})),
		grpc.WithBlock(), //nolint:staticcheck // NewClient does not support this compatibility behavior.
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}),
	)
	if err != nil {
		h.logger.Error("failed to connect to CPI provider",
			zap.String("path", socketPath), zap.Error(err))
		pc.mu.Lock()
		pc.Info.ConnectionState = v1alpha1.ProviderConnectionDisconnected
		pc.Info.LastError = err.Error()
		pc.mu.Unlock()
		h.removeFailedConnection(socketPath, pc)
		return
	}

	pc.mu.Lock()
	pc.Conn = conn
	pc.Info.ConnectionState = v1alpha1.ProviderConnectionConnected
	pc.mu.Unlock()

	// Query provider identity.
	if err := h.queryProviderInfo(pc); err != nil {
		h.logger.Error("failed to query CPI provider info",
			zap.String("path", socketPath), zap.Error(err))
		pc.mu.Lock()
		pc.Info.LastError = err.Error()
		pc.mu.Unlock()
		h.removeFailedConnection(socketPath, pc)
		return
	}

	// Query capabilities.
	if err := h.queryProviderCapabilities(pc); err != nil {
		h.logger.Error("failed to query CPI provider capabilities",
			zap.String("path", socketPath), zap.Error(err))
		pc.mu.Lock()
		pc.Info.LastError = err.Error()
		pc.mu.Unlock()
		h.removeFailedConnection(socketPath, pc)
		return
	}

	// Perform initial health check.
	h.healthCheckProvider(pc)

	pc.mu.RLock()
	name := pc.Info.Name
	version := pc.Info.Version
	dimension := pc.Info.Dimension
	capabilities := pc.Info.Capabilities
	health := pc.Info.HealthStatus
	pc.mu.RUnlock()

	h.logger.Info("CPI provider registered",
		zap.String("name", name),
		zap.String("version", version),
		zap.String("dimension", dimension),
		zap.Strings("capabilities", capabilities),
		zap.String("health", string(health)))
}

func (h *ProviderHost) removeFailedConnection(socketPath string, failed *ProviderConnection) {
	h.mu.Lock()
	if h.providers[socketPath] == failed {
		delete(h.providers, socketPath)
	}
	h.mu.Unlock()

	failed.mu.Lock()
	failed.Info.ConnectionState = v1alpha1.ProviderConnectionDisconnected
	conn := failed.Conn
	failed.Conn = nil
	failed.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// queryProviderInfo calls Identity.GetProviderInfo on the provider.
func (h *ProviderHost) queryProviderInfo(pc *ProviderConnection) error {
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.HealthCheckTimeout)
	defer cancel()

	req := &GetProviderInfoRequest{}
	resp := &GetProviderInfoResponse{}

	err := pc.Conn.Invoke(ctx, "/kognition.v1alpha1.Identity/GetProviderInfo", req, resp)
	if err != nil {
		return fmt.Errorf("GetProviderInfo: %w", err)
	}

	pc.mu.Lock()
	pc.Info.Name = resp.Name
	pc.Info.Version = resp.Version
	pc.Info.Dimension = resp.Dimension
	pc.Info.RegisteredAt = time.Now()
	pc.mu.Unlock()

	return nil
}

// queryProviderCapabilities calls Identity.GetProviderCapabilities on the provider.
func (h *ProviderHost) queryProviderCapabilities(pc *ProviderConnection) error {
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.HealthCheckTimeout)
	defer cancel()

	req := &GetProviderCapabilitiesRequest{}
	resp := &GetProviderCapabilitiesResponse{}

	err := pc.Conn.Invoke(ctx, "/kognition.v1alpha1.Identity/GetProviderCapabilities", req, resp)
	if err != nil {
		return fmt.Errorf("GetProviderCapabilities: %w", err)
	}

	pc.mu.Lock()
	pc.Info.Capabilities = resp.Capabilities
	pc.Info.Knowledge = knowledgeCapabilitiesFromWire(resp.Knowledge)
	pc.mu.Unlock()

	return nil
}

// healthCheckProvider performs a single HealthCheck call.
func (h *ProviderHost) healthCheckProvider(pc *ProviderConnection) {
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.HealthCheckTimeout)
	defer cancel()

	req := &HealthCheckRequest{}
	resp := &HealthCheckResponse{}

	err := pc.Conn.Invoke(ctx, "/kognition.v1alpha1.Lifecycle/HealthCheck", req, resp)

	pc.mu.Lock()
	defer pc.mu.Unlock()

	pc.Info.LastHealthCheck = time.Now()

	if err != nil {
		pc.Info.HealthStatus = v1alpha1.ProviderHealthUnavailable
		pc.Info.LastError = err.Error()
		return
	}

	if resp.Ready {
		pc.Info.HealthStatus = v1alpha1.ProviderHealthReady
		pc.Info.LastError = ""
	} else {
		pc.Info.HealthStatus = v1alpha1.ProviderHealthUnavailable
		pc.Info.LastError = resp.Message
	}
}

// discoveryLoop periodically scans for new provider sockets.
func (h *ProviderHost) discoveryLoop(ctx context.Context) {
	defer h.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-h.stopCh:
			return
		case <-ticker.C:
			if err := h.discover(); err != nil {
				h.logger.Warn("CPI provider discovery error", zap.Error(err))
			}
		}
	}
}

// healthLoop periodically health-checks all connected providers.
func (h *ProviderHost) healthLoop(ctx context.Context) {
	defer h.wg.Done()

	ticker := time.NewTicker(h.cfg.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.healthCheckAll()
		}
	}
}

// healthCheckAll probes all connected providers.
func (h *ProviderHost) healthCheckAll() {
	h.mu.RLock()
	providers := make([]*ProviderConnection, 0, len(h.providers))
	for _, pc := range h.providers {
		providers = append(providers, pc)
	}
	h.mu.RUnlock()

	for _, pc := range providers {
		pc.mu.RLock()
		connected := pc.Info.ConnectionState == v1alpha1.ProviderConnectionConnected
		pc.mu.RUnlock()

		if connected {
			h.healthCheckProvider(pc)
		}
	}
}

// JSONCodec implements grpc encoding.Codec using JSON serialization for CPI calls.
type JSONCodec struct{}

func (JSONCodec) Marshal(v interface{}) ([]byte, error)      { return json.Marshal(v) }
func (JSONCodec) Unmarshal(data []byte, v interface{}) error { return json.Unmarshal(data, v) }
func (JSONCodec) Name() string                               { return "json" }
