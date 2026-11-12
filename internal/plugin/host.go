// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package plugin implements the gRPC sidecar plugin transport host.
// It discovers, connects, health-checks, and manages the lifecycle of
// gRPC sidecar plugins (runtimes, memory backends, tools, model providers).
// Plugins are discovered via socket files in a watched directory, following
// the CSI/CNI pattern used in Kubernetes.
//
// The Kubernetes controller owns the production host lifecycle when plugin
// discovery is enabled. Other binaries may use Host directly for local or
// embedded operation.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pluginv1alpha1 "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

// CapabilityType identifies the category of service a plugin provides.
type CapabilityType int

const (
	CapabilityUnspecified CapabilityType = iota
	CapabilityRuntime
	CapabilityMemory
	CapabilityTool
	CapabilityModel
	CapabilityKnowledge
)

func (c CapabilityType) String() string {
	switch c {
	case CapabilityRuntime:
		return "runtime"
	case CapabilityMemory:
		return "memory"
	case CapabilityTool:
		return "tool"
	case CapabilityModel:
		return "model"
	case CapabilityKnowledge:
		return "knowledge"
	default:
		return "unspecified"
	}
}

// PluginInfo holds metadata about a discovered plugin.
type PluginInfo struct {
	Name         string
	Version      string
	Description  string
	Vendor       string
	Capabilities []CapabilityType
	SocketPath   string
}

// PluginState represents the lifecycle state of a connected plugin.
type PluginState int

const (
	StateDisconnected PluginState = iota
	StateConnecting
	StateReady
	StateUnhealthy
)

func (s PluginState) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateReady:
		return "ready"
	case StateUnhealthy:
		return "unhealthy"
	default:
		return "unknown"
	}
}

// PluginConnection manages a single plugin's gRPC connection and state.
type PluginConnection struct {
	Info       PluginInfo
	State      PluginState
	Conn       *grpc.ClientConn
	LastProbe  time.Time
	LastError  error
	registedAt time.Time
	socketFile os.FileInfo
}

func snapshotPluginConnection(connection *PluginConnection) *PluginConnection {
	snapshot := *connection
	snapshot.Info.Capabilities = append([]CapabilityType(nil), connection.Info.Capabilities...)
	return &snapshot
}

// HostConfig configures the plugin host behavior.
type HostConfig struct {
	// SocketDir is the directory to watch for plugin socket files.
	SocketDir string

	// DiscoveryInterval is how often to scan for plugin socket changes.
	DiscoveryInterval time.Duration

	// ProbeInterval is how often to health-check plugins.
	ProbeInterval time.Duration

	// ProbeTimeout is the maximum time to wait for a probe response.
	ProbeTimeout time.Duration

	// ConnectTimeout is the maximum time to wait for initial connection.
	ConnectTimeout time.Duration

	// ReconnectBackoff is the initial backoff duration for reconnection.
	ReconnectBackoff time.Duration

	// MaxReconnectBackoff caps the exponential backoff.
	MaxReconnectBackoff time.Duration

	// HostVersion is reported to plugins on registration.
	HostVersion string
}

// DefaultHostConfig returns a HostConfig with sensible defaults.
func DefaultHostConfig() HostConfig {
	return HostConfig{
		SocketDir:           "/var/run/kognition/plugins",
		DiscoveryInterval:   10 * time.Second,
		ProbeInterval:       10 * time.Second,
		ProbeTimeout:        5 * time.Second,
		ConnectTimeout:      10 * time.Second,
		ReconnectBackoff:    1 * time.Second,
		MaxReconnectBackoff: 30 * time.Second,
		HostVersion:         "0.1.0",
	}
}

// Host discovers, connects to, and manages gRPC sidecar plugins.
type Host struct {
	cfg           HostConfig
	logger        *zap.Logger
	mu            sync.RWMutex
	plugins       map[string]*PluginConnection // keyed by socket path
	connectCtx    context.Context
	cancelConnect context.CancelFunc
	stopCh        chan struct{}
	stopOnce      sync.Once
	started       bool
	stopped       bool
	wg            sync.WaitGroup
	connectWG     sync.WaitGroup
}

// NewHost creates a plugin host with the given configuration.
func NewHost(cfg HostConfig, logger *zap.Logger) *Host {
	connectCtx, cancelConnect := context.WithCancel(context.Background())
	return &Host{
		cfg:           cfg,
		logger:        logger,
		plugins:       make(map[string]*PluginConnection),
		connectCtx:    connectCtx,
		cancelConnect: cancelConnect,
		stopCh:        make(chan struct{}),
	}
}

// Start begins plugin discovery and health monitoring.
func (h *Host) Start(ctx context.Context) error {
	if err := validateHostConfig(h.cfg); err != nil {
		return err
	}
	if err := os.MkdirAll(h.cfg.SocketDir, 0750); err != nil {
		return fmt.Errorf("plugin host: create socket dir: %w", err)
	}
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return errors.New("plugin host: cannot start after stop")
	}
	if h.started {
		h.mu.Unlock()
		return errors.New("plugin host: already started")
	}
	h.started = true
	h.cancelConnect()
	h.connectCtx, h.cancelConnect = context.WithCancel(ctx)
	h.wg.Add(2)
	h.mu.Unlock()

	// Initial discovery.
	if err := h.discover(); err != nil {
		h.logger.Warn("initial plugin discovery failed", zap.Error(err))
	}

	// Start background loops.
	go h.discoveryLoop(ctx)
	go h.probeLoop(ctx)

	return nil
}

func validateHostConfig(cfg HostConfig) error {
	if strings.TrimSpace(cfg.SocketDir) == "" {
		return errors.New("plugin host: socket dir is required")
	}
	values := []struct {
		name  string
		value time.Duration
	}{
		{"discovery interval", cfg.DiscoveryInterval},
		{"probe interval", cfg.ProbeInterval},
		{"probe timeout", cfg.ProbeTimeout},
		{"connect timeout", cfg.ConnectTimeout},
		{"reconnect backoff", cfg.ReconnectBackoff},
		{"maximum reconnect backoff", cfg.MaxReconnectBackoff},
	}
	for _, configured := range values {
		if configured.value <= 0 {
			return fmt.Errorf("plugin host: %s must be positive", configured.name)
		}
	}
	if cfg.MaxReconnectBackoff < cfg.ReconnectBackoff {
		return errors.New("plugin host: maximum reconnect backoff must not be less than reconnect backoff")
	}
	return nil
}

// Stop shuts down the plugin host and disconnects all plugins.
func (h *Host) Stop() {
	h.stopOnce.Do(func() {
		close(h.stopCh)
		h.mu.Lock()
		h.stopped = true
		h.cancelConnect()
		h.mu.Unlock()
	})
	h.wg.Wait()
	h.connectWG.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()

	for _, pc := range h.plugins {
		if pc.Conn != nil {
			_ = pc.Conn.Close()
		}
	}
	h.plugins = make(map[string]*PluginConnection)
}

// GetPlugins returns a snapshot of all connected plugins.
func (h *Host) GetPlugins() []PluginConnection {
	h.mu.RLock()
	defer h.mu.RUnlock()

	result := make([]PluginConnection, 0, len(h.plugins))
	for _, pc := range h.plugins {
		result = append(result, *snapshotPluginConnection(pc))
	}
	return result
}

// GetPluginsByCapability returns plugins that provide the given capability.
func (h *Host) GetPluginsByCapability(cap CapabilityType) []PluginConnection {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var result []PluginConnection
	for _, pc := range h.plugins {
		for _, c := range pc.Info.Capabilities {
			if c == cap {
				result = append(result, *snapshotPluginConnection(pc))
				break
			}
		}
	}
	return result
}

// GetReadyPlugin returns the first ready plugin with the given capability and name.
// If name is empty, returns the first ready plugin with the capability.
func (h *Host) GetReadyPlugin(cap CapabilityType, name string) (*PluginConnection, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var matches []*PluginConnection
	for _, pc := range h.plugins {
		if pc.State != StateReady {
			continue
		}
		if name != "" && pc.Info.Name != name {
			continue
		}
		for _, c := range pc.Info.Capabilities {
			if c == cap {
				matches = append(matches, pc)
				break
			}
		}
	}
	if len(matches) > 0 {
		sort.Slice(matches, func(i, j int) bool {
			if matches[i].Info.Name != matches[j].Info.Name {
				return matches[i].Info.Name < matches[j].Info.Name
			}
			return matches[i].Info.SocketPath < matches[j].Info.SocketPath
		})
		return snapshotPluginConnection(matches[0]), nil
	}
	return nil, fmt.Errorf("plugin host: no ready plugin found (capability=%s, name=%q)", cap, name)
}

// ConnectionForSocket returns the connection for a given socket path (for testing).
func (h *Host) ConnectionForSocket(socketPath string) (*PluginConnection, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	pc, ok := h.plugins[socketPath]
	if !ok {
		return nil, false
	}
	return snapshotPluginConnection(pc), true
}

// discover scans the socket directory for new plugin socket files.
func (h *Host) discover() error {
	entries, err := os.ReadDir(h.cfg.SocketDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("plugin host: read socket dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".sock") {
			continue
		}

		socketPath := filepath.Join(h.cfg.SocketDir, entry.Name())
		socketFile, err := entry.Info()
		if err != nil {
			h.logger.Warn("stat plugin socket", zap.String("path", socketPath), zap.Error(err))
			continue
		}

		h.mu.Lock()
		connected, exists := h.plugins[socketPath]
		replaced := exists && connected.socketFile != nil && !os.SameFile(connected.socketFile, socketFile)
		var candidate *PluginConnection
		var staleConn *grpc.ClientConn
		if !exists || replaced {
			candidate = &PluginConnection{
				Info:       PluginInfo{SocketPath: socketPath},
				State:      StateConnecting,
				socketFile: socketFile,
			}
			h.plugins[socketPath] = candidate
			if replaced {
				staleConn = connected.Conn
			}
		} else if connected.socketFile == nil {
			connected.socketFile = socketFile
		}
		h.mu.Unlock()

		if staleConn != nil {
			_ = staleConn.Close()
		}
		if !exists || replaced {
			if replaced {
				h.logger.Info("plugin socket replaced", zap.String("path", socketPath))
			}
			h.logger.Info("discovered plugin socket", zap.String("path", socketPath))
			h.connectPluginAsyncIfCurrent(socketPath, candidate, h.reconnectBackoff())
		}
	}

	// Clean up removed sockets.
	h.mu.Lock()
	for socketPath, pc := range h.plugins {
		if _, err := os.Stat(socketPath); errors.Is(err, os.ErrNotExist) {
			h.logger.Info("plugin socket removed", zap.String("name", pc.Info.Name), zap.String("path", socketPath))
			if pc.Conn != nil {
				_ = pc.Conn.Close()
			}
			delete(h.plugins, socketPath)
		}
	}
	h.mu.Unlock()

	return nil
}

// connectPlugin establishes a gRPC connection to a plugin sidecar.
func (h *Host) connectPlugin(socketPath string) {
	h.connectPluginIfCurrent(socketPath, nil, h.reconnectBackoff())
}

func (h *Host) connectPluginAsyncIfCurrent(socketPath string, current *PluginConnection, backoff time.Duration) {
	h.mu.Lock()
	if h.stopped || h.plugins[socketPath] != current {
		h.mu.Unlock()
		return
	}
	h.connectWG.Add(1)
	h.mu.Unlock()

	go func() {
		defer h.connectWG.Done()
		h.connectPluginIfCurrent(socketPath, current, backoff)
	}()
}

func (h *Host) connectPluginIfCurrent(socketPath string, current *PluginConnection, backoff time.Duration) {
	var socketFile os.FileInfo
	if current != nil {
		socketFile = current.socketFile
	}
	if latest, err := os.Stat(socketPath); err == nil {
		socketFile = latest
	}
	candidate := &PluginConnection{
		Info:       PluginInfo{SocketPath: socketPath},
		State:      StateConnecting,
		socketFile: socketFile,
	}
	h.mu.Lock()
	if h.stopped || (current != nil && h.plugins[socketPath] != current) {
		h.mu.Unlock()
		return
	}
	h.connectWG.Add(1)
	h.plugins[socketPath] = candidate
	connectCtx := h.connectCtx
	h.mu.Unlock()
	defer h.connectWG.Done()

	ctx, cancel := context.WithTimeout(connectCtx, h.cfg.ConnectTimeout)
	defer cancel()

	conn, err := DialPlugin(ctx, socketPath)
	if err != nil {
		h.logger.Error("failed to connect to plugin", zap.String("path", socketPath), zap.Error(err))
		h.updateConnectionIfCurrent(socketPath, candidate, func(pc *PluginConnection) {
			pc.State = StateDisconnected
			pc.LastError = err
		})
		h.scheduleReconnect(socketPath, candidate, backoff)
		return
	}

	// Query plugin identity.
	info, err := h.queryPluginInfo(ctx, conn)
	if err != nil {
		h.logger.Error("failed to query plugin info", zap.String("path", socketPath), zap.Error(err))
		_ = conn.Close()
		h.updateConnectionIfCurrent(socketPath, candidate, func(pc *PluginConnection) {
			pc.State = StateDisconnected
			pc.LastError = err
		})
		h.scheduleReconnect(socketPath, candidate, backoff)
		return
	}
	info.SocketPath = socketPath

	// Query capabilities.
	caps, err := h.queryPluginCapabilities(ctx, conn)
	if err != nil {
		h.logger.Error("failed to query plugin capabilities", zap.String("path", socketPath), zap.Error(err))
		_ = conn.Close()
		h.updateConnectionIfCurrent(socketPath, candidate, func(pc *PluginConnection) {
			pc.State = StateDisconnected
			pc.LastError = err
		})
		h.scheduleReconnect(socketPath, candidate, backoff)
		return
	}
	info.Capabilities = caps

	// Notify registration before publishing any health state. Some plugins use
	// this handshake to complete initialization.
	if err := h.notifyRegistered(ctx, conn); err != nil {
		h.logger.Error("failed to notify plugin registration", zap.String("path", socketPath), zap.Error(err))
		_ = conn.Close()
		h.updateConnectionIfCurrent(socketPath, candidate, func(pc *PluginConnection) {
			pc.Info = info
			pc.State = StateDisconnected
			pc.LastError = err
		})
		h.scheduleReconnect(socketPath, candidate, backoff)
		return
	}

	// Probe readiness.
	ready, err := h.probePlugin(ctx, conn)
	if err != nil || !ready {
		h.logger.Warn("plugin not ready", zap.String("name", info.Name), zap.Error(err))
		replacement := &PluginConnection{
			Info:       info,
			State:      StateUnhealthy,
			Conn:       conn,
			LastProbe:  time.Now(),
			LastError:  err,
			socketFile: candidate.socketFile,
		}
		if !h.replaceConnectionIfCurrent(socketPath, candidate, replacement) {
			_ = conn.Close()
		}
		return
	}

	replacement := &PluginConnection{
		Info:       info,
		State:      StateReady,
		Conn:       conn,
		LastProbe:  time.Now(),
		registedAt: time.Now(),
		socketFile: candidate.socketFile,
	}
	if !h.replaceConnectionIfCurrent(socketPath, candidate, replacement) {
		_ = conn.Close()
		return
	}

	h.logger.Info("plugin registered",
		zap.String("name", info.Name),
		zap.String("version", info.Version),
		zap.String("path", socketPath),
	)
}

func (h *Host) reconnectBackoff() time.Duration {
	if h.cfg.ReconnectBackoff > 0 {
		return h.cfg.ReconnectBackoff
	}
	return time.Second
}

func (h *Host) scheduleReconnect(socketPath string, current *PluginConnection, backoff time.Duration) {
	h.mu.Lock()
	if !h.started || h.stopped || h.plugins[socketPath] != current {
		h.mu.Unlock()
		return
	}
	connectCtx := h.connectCtx
	h.connectWG.Add(1)
	h.mu.Unlock()

	go func() {
		defer h.connectWG.Done()
		timer := time.NewTimer(backoff)
		defer timer.Stop()
		select {
		case <-connectCtx.Done():
			return
		case <-timer.C:
		}

		nextBackoff := backoff * 2
		if max := h.cfg.MaxReconnectBackoff; max > 0 && nextBackoff > max {
			nextBackoff = max
		}
		h.connectPluginIfCurrent(socketPath, current, nextBackoff)
	}()
}

func (h *Host) updateConnectionIfCurrent(socketPath string, current *PluginConnection, update func(*PluginConnection)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.plugins[socketPath] == current {
		update(current)
	}
}

func (h *Host) replaceConnectionIfCurrent(socketPath string, current, replacement *PluginConnection) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.plugins[socketPath] != current {
		return false
	}
	h.plugins[socketPath] = replacement
	return true
}

// queryPluginInfo calls GetPluginInfo on the plugin.
func (h *Host) queryPluginInfo(ctx context.Context, conn *grpc.ClientConn) (PluginInfo, error) {
	resp, err := pluginv1alpha1.NewPluginClient(conn).GetPluginInfo(ctx, &pluginv1alpha1.GetPluginInfoRequest{})
	if err != nil {
		return PluginInfo{}, err
	}
	return PluginInfo{
		Name:        resp.GetName(),
		Version:     resp.GetVersion(),
		Description: resp.GetDescription(),
		Vendor:      resp.GetVendor(),
	}, nil
}

// queryPluginCapabilities calls GetPluginCapabilities on the plugin.
func (h *Host) queryPluginCapabilities(ctx context.Context, conn *grpc.ClientConn) ([]CapabilityType, error) {
	resp, err := pluginv1alpha1.NewPluginClient(conn).GetPluginCapabilities(ctx, &pluginv1alpha1.GetPluginCapabilitiesRequest{})
	if err != nil {
		return nil, err
	}
	caps := make([]CapabilityType, 0, len(resp.GetCapabilities()))
	for _, capability := range resp.GetCapabilities() {
		caps = append(caps, capabilityTypeFromProto(capability.GetType()))
	}
	return caps, nil
}

func capabilityTypeFromProto(capability pluginv1alpha1.PluginCapability_Type) CapabilityType {
	switch capability {
	case pluginv1alpha1.PluginCapability_RUNTIME:
		return CapabilityRuntime
	case pluginv1alpha1.PluginCapability_MEMORY:
		return CapabilityMemory
	case pluginv1alpha1.PluginCapability_TOOL:
		return CapabilityTool
	case pluginv1alpha1.PluginCapability_MODEL:
		return CapabilityModel
	case pluginv1alpha1.PluginCapability_KNOWLEDGE:
		return CapabilityKnowledge
	default:
		return CapabilityUnspecified
	}
}

// probePlugin calls Probe on the plugin.
func (h *Host) probePlugin(ctx context.Context, conn *grpc.ClientConn) (bool, error) {
	resp, err := pluginv1alpha1.NewPluginClient(conn).Probe(ctx, &pluginv1alpha1.PluginProbeRequest{})
	if err != nil {
		return false, err
	}
	return resp.GetReady(), nil
}

// notifyRegistered calls NotifyRegistered on the plugin.
func (h *Host) notifyRegistered(ctx context.Context, conn *grpc.ClientConn) error {
	_, err := pluginv1alpha1.NewPluginClient(conn).NotifyRegistered(ctx, &pluginv1alpha1.NotifyRegisteredRequest{HostVersion: h.cfg.HostVersion})
	return err
}

// discoveryLoop periodically scans for new plugin sockets.
func (h *Host) discoveryLoop(ctx context.Context) {
	defer h.wg.Done()

	ticker := time.NewTicker(h.cfg.DiscoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-h.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.discover(); err != nil {
				h.logger.Warn("plugin discovery scan failed", zap.Error(err))
			}
		}
	}
}

// probeLoop periodically health-checks all connected plugins.
func (h *Host) probeLoop(ctx context.Context) {
	defer h.wg.Done()

	ticker := time.NewTicker(h.cfg.ProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-h.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.probeAll(ctx)
		}
	}
}

// probeAll health-checks every registered plugin.
func (h *Host) probeAll(ctx context.Context) {
	h.mu.RLock()
	paths := make([]string, 0, len(h.plugins))
	for path := range h.plugins {
		paths = append(paths, path)
	}
	h.mu.RUnlock()

	for _, path := range paths {
		h.mu.RLock()
		pc, ok := h.plugins[path]
		var conn *grpc.ClientConn
		if ok {
			conn = pc.Conn
		}
		h.mu.RUnlock()
		if !ok || conn == nil {
			continue
		}

		probeCtx, cancel := context.WithTimeout(ctx, h.cfg.ProbeTimeout)
		ready, err := h.probePlugin(probeCtx, conn)
		cancel()

		h.mu.Lock()
		if h.plugins[path] != pc {
			h.mu.Unlock()
			continue
		}
		pc.LastProbe = time.Now()
		if err != nil {
			h.logger.Warn("plugin connection lost",
				zap.String("name", pc.Info.Name),
				zap.Error(err),
			)
			pc.State = StateDisconnected
			pc.Conn = nil
			pc.LastError = err
			h.mu.Unlock()
			_ = conn.Close()
			h.scheduleReconnect(path, pc, h.reconnectBackoff())
			continue
		}
		if !ready {
			if pc.State == StateReady {
				h.logger.Warn("plugin became unhealthy",
					zap.String("name", pc.Info.Name),
				)
			}
			pc.State = StateUnhealthy
			pc.LastError = nil
		} else {
			if pc.State == StateUnhealthy {
				h.logger.Info("plugin recovered",
					zap.String("name", pc.Info.Name),
				)
			}
			pc.State = StateReady
			pc.LastError = nil
		}
		h.mu.Unlock()
	}
}

// Reconnect attempts to re-establish connection to a disconnected plugin.
func (h *Host) Reconnect(socketPath string) {
	h.mu.Lock()
	pc, ok := h.plugins[socketPath]
	if !ok {
		h.mu.Unlock()
		return
	}
	conn := pc.Conn
	candidate := &PluginConnection{
		Info:       PluginInfo{SocketPath: socketPath},
		State:      StateConnecting,
		socketFile: pc.socketFile,
	}
	h.plugins[socketPath] = candidate
	h.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}

	h.connectPluginIfCurrent(socketPath, candidate, h.reconnectBackoff())
}

// DialPlugin is a helper to create a gRPC client connection to a Unix socket.
// Used internally and exposed for testing.
func DialPlugin(ctx context.Context, socketPath string) (*grpc.ClientConn, error) {
	return grpc.DialContext(ctx, "passthrough:///plugin.sock", //nolint:staticcheck // DialPlugin preserves blocking grpc 1.x connection semantics.
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(), //nolint:staticcheck // DialPlugin preserves blocking grpc 1.x connection semantics.
	)
}

// ListenPlugin creates a net.Listener on a Unix socket (for plugin processes).
func ListenPlugin(socketPath string) (net.Listener, error) {
	// Remove stale socket file if it exists.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	var listenConfig net.ListenConfig
	return listenConfig.Listen(context.Background(), "unix", socketPath)
}
