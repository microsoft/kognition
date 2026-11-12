// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"

	v1alpha1 "github.com/kognition-ai/kognition/api/v1alpha1"
)

func init() {
	encoding.RegisterCodec(JSONCodec{})
}

// fakeCPIProvider implements Identity and Lifecycle CPI services for testing.
type fakeCPIProvider struct {
	mu           sync.Mutex
	name         string
	version      string
	dimension    string
	capabilities []string
	knowledge    *KnowledgeProviderCapabilities
	healthy      bool
	infoFailures int
	infoCalls    int
	capFailures  int
	capCalls     int

	provisionCalled bool
	provisionReq    *ProvisionRequest
}

func (f *fakeCPIProvider) register(srv *grpc.Server) {
	// Register using generic service descriptors for JSON codec.
	srv.RegisterService(&identityServiceDesc, f)
	srv.RegisterService(&lifecycleServiceDesc, f)
}

// Identity.GetProviderInfo handler
func (f *fakeCPIProvider) GetProviderInfo(ctx context.Context, req *GetProviderInfoRequest) (*GetProviderInfoResponse, error) {
	f.mu.Lock()
	f.infoCalls++
	shouldFail := f.infoCalls <= f.infoFailures
	f.mu.Unlock()
	if shouldFail {
		return nil, errors.New("transient provider info failure")
	}
	return &GetProviderInfoResponse{
		Name:      f.name,
		Version:   f.version,
		Dimension: f.dimension,
	}, nil
}

func TestProviderHost_RetriesTransientIdentityFailure(t *testing.T) {
	dir, err := os.MkdirTemp("", "cpi-retry-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	provider := &fakeCPIProvider{
		name:         "retry-provider",
		version:      "1.0.0",
		dimension:    "model",
		capabilities: []string{"complete"},
		healthy:      true,
		infoFailures: 1,
	}
	socketPath, _ := startFakeProvider(t, dir, "retry-provider", provider)
	host := NewProviderHost(ProviderHostConfig{
		SocketDir:           dir,
		HealthCheckInterval: time.Hour,
		HealthCheckTimeout:  time.Second,
		ConnectTimeout:      time.Second,
	}, zaptest.NewLogger(t))
	t.Cleanup(host.Stop)

	if err := host.discover(); err != nil {
		t.Fatalf("initial discovery: %v", err)
	}
	waitForInfoCalls(t, provider, 1, time.Second)
	waitForProviderAbsent(t, host, socketPath, time.Second)
	if err := host.discover(); err != nil {
		t.Fatalf("retry discovery: %v", err)
	}
	waitForInfoCalls(t, provider, 2, time.Second)
	entry := waitForProviderReady(t, host, socketPath, time.Second)
	if entry.Name != provider.name || entry.HealthStatus != v1alpha1.ProviderHealthReady {
		t.Fatalf("provider entry = %+v, want recovered ready provider", entry)
	}
}

func TestProviderHost_RetriesTransientCapabilityFailure(t *testing.T) {
	dir, err := os.MkdirTemp("", "cpi-cap-retry-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	provider := &fakeCPIProvider{
		name:         "retry-provider",
		version:      "1.0.0",
		dimension:    "model",
		capabilities: []string{"complete"},
		healthy:      true,
		capFailures:  1,
	}
	socketPath, _ := startFakeProvider(t, dir, "retry-provider", provider)
	host := NewProviderHost(ProviderHostConfig{
		SocketDir:           dir,
		HealthCheckInterval: time.Hour,
		HealthCheckTimeout:  time.Second,
		ConnectTimeout:      time.Second,
	}, zaptest.NewLogger(t))
	t.Cleanup(host.Stop)

	if err := host.discover(); err != nil {
		t.Fatalf("initial discovery: %v", err)
	}
	waitForCapabilityCalls(t, provider, 1, time.Second)
	waitForProviderAbsent(t, host, socketPath, time.Second)
	if err := host.discover(); err != nil {
		t.Fatalf("retry discovery: %v", err)
	}
	waitForCapabilityCalls(t, provider, 2, time.Second)
	entry := waitForProviderReady(t, host, socketPath, time.Second)
	if entry.HealthStatus != v1alpha1.ProviderHealthReady || len(entry.Capabilities) != 1 || entry.Capabilities[0] != "complete" {
		t.Fatalf("provider entry = %+v, want recovered capabilities and ready health", entry)
	}
}

func TestProviderHost_RetriesAfterDialFailure(t *testing.T) {
	dir, err := os.MkdirTemp("", "cpi-dial-retry-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "late-provider.sock")
	host := NewProviderHost(ProviderHostConfig{
		SocketDir:           dir,
		HealthCheckInterval: time.Hour,
		HealthCheckTimeout:  time.Second,
		ConnectTimeout:      time.Second,
	}, zaptest.NewLogger(t))
	t.Cleanup(host.Stop)

	host.connectAndRegister(socketPath)
	if _, ok := host.ConnectionForSocket(socketPath); ok {
		t.Fatal("failed dial left a provider entry that would block retry")
	}
	provider := &fakeCPIProvider{
		name:         "late-provider",
		version:      "1.0.0",
		dimension:    "model",
		capabilities: []string{"complete"},
		healthy:      true,
	}
	startFakeProvider(t, dir, "late-provider", provider)
	if err := host.discover(); err != nil {
		t.Fatalf("retry discovery: %v", err)
	}
	waitForInfoCalls(t, provider, 1, time.Second)
	if _, ok := host.ConnectionForSocket(socketPath); !ok {
		t.Fatal("provider was not registered after recovering from dial failure")
	}
}

func waitForProviderAbsent(t *testing.T, host *ProviderHost, socketPath string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, ok := host.ConnectionForSocket(socketPath); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("failed provider connection was not released for retry")
}

func waitForProviderReady(t *testing.T, host *ProviderHost, socketPath string, timeout time.Duration) v1alpha1.ProviderEntry {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last v1alpha1.ProviderEntry
	for time.Now().Before(deadline) {
		connection, ok := host.ConnectionForSocket(socketPath)
		if ok {
			connection.mu.RLock()
			last = connection.Info
			connection.mu.RUnlock()
			if last.HealthStatus == v1alpha1.ProviderHealthReady {
				return last
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("provider did not become ready; last entry = %+v", last)
	return v1alpha1.ProviderEntry{}
}

func waitForInfoCalls(t *testing.T, provider *fakeCPIProvider, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		provider.mu.Lock()
		calls := provider.infoCalls
		provider.mu.Unlock()
		if calls >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GetProviderInfo calls did not reach %d", want)
}

func waitForCapabilityCalls(t *testing.T, provider *fakeCPIProvider, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		provider.mu.Lock()
		calls := provider.capCalls
		provider.mu.Unlock()
		if calls >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GetProviderCapabilities calls did not reach %d", want)
}

// Identity.GetProviderCapabilities handler
func (f *fakeCPIProvider) GetProviderCapabilities(ctx context.Context, req *GetProviderCapabilitiesRequest) (*GetProviderCapabilitiesResponse, error) {
	f.mu.Lock()
	f.capCalls++
	shouldFail := f.capCalls <= f.capFailures
	f.mu.Unlock()
	if shouldFail {
		return nil, errors.New("transient provider capabilities failure")
	}
	return &GetProviderCapabilitiesResponse{
		Capabilities: f.capabilities,
		Knowledge:    f.knowledge,
	}, nil
}

// Lifecycle.HealthCheck handler
func (f *fakeCPIProvider) HealthCheck(ctx context.Context, req *HealthCheckRequest) (*HealthCheckResponse, error) {
	f.mu.Lock()
	h := f.healthy
	f.mu.Unlock()
	return &HealthCheckResponse{
		Ready:   h,
		Message: "test provider",
	}, nil
}

// Lifecycle.Provision handler
func (f *fakeCPIProvider) Provision(ctx context.Context, req *ProvisionRequest) (*ProvisionResponse, error) {
	f.mu.Lock()
	f.provisionCalled = true
	f.provisionReq = req
	f.mu.Unlock()
	return &ProvisionResponse{
		ResourceID:     "prov-123",
		ConnectionInfo: map[string]string{"endpoint": "http://localhost:8080"},
	}, nil
}

// gRPC service descriptors for JSON codec (no protoc).
var identityServiceDesc = grpc.ServiceDesc{
	ServiceName: "kognition.v1alpha1.Identity",
	HandlerType: (*identityServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "GetProviderInfo",
			Handler:    handleGetProviderInfo,
		},
		{
			MethodName: "GetProviderCapabilities",
			Handler:    handleGetProviderCapabilities,
		},
	},
}

var lifecycleServiceDesc = grpc.ServiceDesc{
	ServiceName: "kognition.v1alpha1.Lifecycle",
	HandlerType: (*lifecycleServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "HealthCheck",
			Handler:    handleHealthCheck,
		},
		{
			MethodName: "Provision",
			Handler:    handleProvision,
		},
	},
}

type identityServer interface {
	GetProviderInfo(context.Context, *GetProviderInfoRequest) (*GetProviderInfoResponse, error)
	GetProviderCapabilities(context.Context, *GetProviderCapabilitiesRequest) (*GetProviderCapabilitiesResponse, error)
}

type lifecycleServer interface {
	HealthCheck(context.Context, *HealthCheckRequest) (*HealthCheckResponse, error)
	Provision(context.Context, *ProvisionRequest) (*ProvisionResponse, error)
}

func handleGetProviderInfo(srv interface{}, ctx context.Context, dec func(interface{}) error, _ grpc.UnaryServerInterceptor) (interface{}, error) {
	req := &GetProviderInfoRequest{}
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(identityServer).GetProviderInfo(ctx, req)
}

func handleGetProviderCapabilities(srv interface{}, ctx context.Context, dec func(interface{}) error, _ grpc.UnaryServerInterceptor) (interface{}, error) {
	req := &GetProviderCapabilitiesRequest{}
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(identityServer).GetProviderCapabilities(ctx, req)
}

func handleHealthCheck(srv interface{}, ctx context.Context, dec func(interface{}) error, _ grpc.UnaryServerInterceptor) (interface{}, error) {
	req := &HealthCheckRequest{}
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(lifecycleServer).HealthCheck(ctx, req)
}

func handleProvision(srv interface{}, ctx context.Context, dec func(interface{}) error, _ grpc.UnaryServerInterceptor) (interface{}, error) {
	req := &ProvisionRequest{}
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(lifecycleServer).Provision(ctx, req)
}

// startFakeProvider starts a gRPC server on a Unix socket and returns its path.
func startFakeProvider(t *testing.T, dir string, name string, provider *fakeCPIProvider) (string, *grpc.Server) {
	t.Helper()

	socketPath := filepath.Join(dir, name+".sock")
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on %s: %v", socketPath, err)
	}

	srv := grpc.NewServer(grpc.ForceServerCodec(JSONCodec{}))
	provider.register(srv)

	go func() {
		_ = srv.Serve(lis) // Server stop during test teardown is expected.
	}()

	t.Cleanup(func() { srv.GracefulStop() })

	return socketPath, srv
}

// TC-0188: Register provider via IdentityRPC → verify ProviderRegistry records it
// with correct dimension and capabilities.
func TestProviderHost_Registration(t *testing.T) {
	dir := t.TempDir()

	provider := &fakeCPIProvider{
		name:         "openai-provider",
		version:      "1.0.0",
		dimension:    "model",
		capabilities: []string{"provision", "healthcheck", "complete"},
		healthy:      true,
	}

	startFakeProvider(t, dir, "openai", provider)

	logger := zaptest.NewLogger(t)
	host := NewProviderHost(ProviderHostConfig{
		SocketDir:           dir,
		HealthCheckInterval: 1 * time.Hour, // No auto-health for this test
		HealthCheckTimeout:  5 * time.Second,
		ConnectTimeout:      5 * time.Second,
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := host.Start(ctx); err != nil {
		t.Fatalf("start provider host: %v", err)
	}
	defer host.Stop()

	// Wait for registration to complete.
	var entries []v1alpha1.ProviderEntry
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries = host.GetProviders()
		if len(entries) > 0 && entries[0].Name == "openai-provider" &&
			entries[0].ConnectionState == v1alpha1.ProviderConnectionConnected &&
			entries[0].HealthStatus == v1alpha1.ProviderHealthReady {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if len(entries) == 0 {
		t.Fatal("expected at least 1 registered provider, got 0")
	}

	entry := entries[0]
	if entry.Name != "openai-provider" {
		t.Errorf("name = %q, want %q", entry.Name, "openai-provider")
	}
	if entry.Version != "1.0.0" {
		t.Errorf("version = %q, want %q", entry.Version, "1.0.0")
	}
	if entry.Dimension != "model" {
		t.Errorf("dimension = %q, want %q", entry.Dimension, "model")
	}
	if len(entry.Capabilities) != 3 {
		t.Errorf("capabilities = %v, want 3 items", entry.Capabilities)
	}
	if entry.ConnectionState != v1alpha1.ProviderConnectionConnected {
		t.Errorf("connection state = %q, want %q", entry.ConnectionState, v1alpha1.ProviderConnectionConnected)
	}
	if entry.HealthStatus != v1alpha1.ProviderHealthReady {
		t.Errorf("health = %q, want %q", entry.HealthStatus, v1alpha1.ProviderHealthReady)
	}
}

// TC-0189: Call LifecycleRPC.Provision → verify provider receives request and reports success.
func TestProviderHost_Provision(t *testing.T) {
	dir := t.TempDir()

	provider := &fakeCPIProvider{
		name:         "gcp-vertex",
		version:      "2.1.0",
		dimension:    "model",
		capabilities: []string{"provision", "deprovision"},
		healthy:      true,
	}

	startFakeProvider(t, dir, "vertex", provider)

	logger := zaptest.NewLogger(t)
	host := NewProviderHost(ProviderHostConfig{
		SocketDir:           dir,
		HealthCheckInterval: 1 * time.Hour,
		HealthCheckTimeout:  5 * time.Second,
		ConnectTimeout:      5 * time.Second,
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := host.Start(ctx); err != nil {
		t.Fatalf("start provider host: %v", err)
	}
	defer host.Stop()

	// Wait for registration.
	socketPath := filepath.Join(dir, "vertex.sock")
	var pc *ProviderConnection
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var ok bool
		pc, ok = host.ConnectionForSocket(socketPath)
		if ok {
			pc.mu.RLock()
			connReady := pc.Conn != nil
			registered := connReady && pc.Info.Name == "gcp-vertex"
			pc.mu.RUnlock()
			if registered {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	if pc == nil {
		t.Fatal("provider not registered in time")
	}
	pc.mu.RLock()
	connNil := pc.Conn == nil
	pc.mu.RUnlock()
	if connNil {
		t.Fatal("provider not registered in time")
	}

	// Call Provision via the gRPC connection.
	req := &ProvisionRequest{
		ResourceKind: "Model",
		ResourceName: "gpt-4",
		Namespace:    "default",
		Parameters:   map[string]string{"region": "us-east1"},
	}
	resp := &ProvisionResponse{}
	err := pc.Conn.Invoke(ctx, "/kognition.v1alpha1.Lifecycle/Provision", req, resp,
		grpc.ForceCodec(JSONCodec{}))
	if err != nil {
		t.Fatalf("Provision call failed: %v", err)
	}

	if resp.ResourceID != "prov-123" {
		t.Errorf("resource_id = %q, want %q", resp.ResourceID, "prov-123")
	}
	if resp.ConnectionInfo["endpoint"] != "http://localhost:8080" {
		t.Errorf("connection_info = %v, want endpoint", resp.ConnectionInfo)
	}
	provider.mu.Lock()
	called := provider.provisionCalled
	provider.mu.Unlock()
	if !called {
		t.Error("expected Provision to be called on the provider")
	}
}

// TC-0190: Kill registered provider → verify HealthCheck detects failure → status updated.
func TestProviderHost_HealthDegradation(t *testing.T) {
	dir := t.TempDir()

	provider := &fakeCPIProvider{
		name:         "memory-provider",
		version:      "1.0.0",
		dimension:    "memory",
		capabilities: []string{"provision", "healthcheck"},
		healthy:      true,
	}

	_, srv := startFakeProvider(t, dir, "memory", provider)

	logger := zaptest.NewLogger(t)
	host := NewProviderHost(ProviderHostConfig{
		SocketDir:           dir,
		HealthCheckInterval: 200 * time.Millisecond,
		HealthCheckTimeout:  2 * time.Second,
		ConnectTimeout:      5 * time.Second,
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := host.Start(ctx); err != nil {
		t.Fatalf("start provider host: %v", err)
	}
	defer host.Stop()

	// Wait for registration and initial Ready status.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries := host.GetProviders()
		if len(entries) > 0 && entries[0].HealthStatus == v1alpha1.ProviderHealthReady {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	entries := host.GetProviders()
	if len(entries) == 0 || entries[0].HealthStatus != v1alpha1.ProviderHealthReady {
		t.Fatal("provider did not become Ready")
	}

	// Kill the provider server to simulate failure.
	srv.Stop()

	// Wait for health check to detect failure.
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries = host.GetProviders()
		if len(entries) > 0 && entries[0].HealthStatus == v1alpha1.ProviderHealthUnavailable {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	entries = host.GetProviders()
	if len(entries) == 0 {
		t.Fatal("expected provider entry after failure")
	}
	if entries[0].HealthStatus != v1alpha1.ProviderHealthUnavailable {
		t.Errorf("health = %q, want %q", entries[0].HealthStatus, v1alpha1.ProviderHealthUnavailable)
	}
}

// TestProviderHost_GetProvidersByDimension verifies dimension filtering.
func TestProviderHost_GetProvidersByDimension(t *testing.T) {
	dir := t.TempDir()

	modelProvider := &fakeCPIProvider{
		name: "model-p", version: "1.0.0", dimension: "model",
		capabilities: []string{"provision"}, healthy: true,
	}
	toolProvider := &fakeCPIProvider{
		name: "tool-p", version: "1.0.0", dimension: "tool",
		capabilities: []string{"provision"}, healthy: true,
	}

	startFakeProvider(t, dir, "model-p", modelProvider)
	startFakeProvider(t, dir, "tool-p", toolProvider)

	logger := zaptest.NewLogger(t)
	host := NewProviderHost(ProviderHostConfig{
		SocketDir:           dir,
		HealthCheckInterval: 1 * time.Hour,
		HealthCheckTimeout:  5 * time.Second,
		ConnectTimeout:      5 * time.Second,
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := host.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer host.Stop()

	// Wait for both providers to be fully registered (Name set).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		providers := host.GetProviders()
		if len(providers) >= 2 {
			allNamed := true
			for _, p := range providers {
				if p.Name == "" {
					allNamed = false
					break
				}
			}
			if allNamed {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	models := host.GetProvidersByDimension("model")
	if len(models) != 1 || models[0].Name != "model-p" {
		t.Errorf("GetProvidersByDimension(model) = %v, want [model-p]", models)
	}

	tools := host.GetProvidersByDimension("tool")
	if len(tools) != 1 || tools[0].Name != "tool-p" {
		t.Errorf("GetProvidersByDimension(tool) = %v, want [tool-p]", tools)
	}
}

// TestProviderHost_SocketRemoval verifies that removing a socket removes the provider.
func TestProviderHost_SocketRemoval(t *testing.T) {
	dir := t.TempDir()

	provider := &fakeCPIProvider{
		name: "ephemeral", version: "1.0.0", dimension: "tool",
		capabilities: []string{"provision"}, healthy: true,
	}

	socketPath, srv := startFakeProvider(t, dir, "ephemeral", provider)

	logger := zaptest.NewLogger(t)
	host := NewProviderHost(ProviderHostConfig{
		SocketDir:           dir,
		HealthCheckInterval: 1 * time.Hour,
		HealthCheckTimeout:  5 * time.Second,
		ConnectTimeout:      5 * time.Second,
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := host.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer host.Stop()

	// Wait for registration.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(host.GetProviders()) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if len(host.GetProviders()) == 0 {
		t.Fatal("provider not registered")
	}

	// Stop server and remove socket.
	srv.Stop()
	os.Remove(socketPath)

	// Trigger discovery to detect removal.
	host.discover()

	if len(host.GetProviders()) != 0 {
		t.Error("expected provider to be removed after socket deletion")
	}
}

// TestProviderHost_GetReadyProvider verifies ready provider lookup by dimension and name.
func TestProviderHost_GetReadyProvider(t *testing.T) {
	dir := t.TempDir()

	provider := &fakeCPIProvider{
		name: "my-model", version: "1.0.0", dimension: "model",
		capabilities: []string{"complete"}, healthy: true,
	}

	startFakeProvider(t, dir, "my-model", provider)

	logger := zaptest.NewLogger(t)
	host := NewProviderHost(ProviderHostConfig{
		SocketDir:           dir,
		HealthCheckInterval: 1 * time.Hour,
		HealthCheckTimeout:  5 * time.Second,
		ConnectTimeout:      5 * time.Second,
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := host.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer host.Stop()

	// Wait for registration and readiness.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		providers := host.GetProviders()
		if len(providers) > 0 && providers[0].Name != "" && providers[0].HealthStatus == v1alpha1.ProviderHealthReady {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Match by dimension.
	entry, err := host.GetReadyProvider("model", "")
	if err != nil {
		t.Fatalf("GetReadyProvider(model, '') failed: %v", err)
	}
	if entry.Name != "my-model" {
		t.Errorf("name = %q, want %q", entry.Name, "my-model")
	}

	// Match by name.
	entry, err = host.GetReadyProvider("", "my-model")
	if err != nil {
		t.Fatalf("GetReadyProvider('', my-model) failed: %v", err)
	}
	if entry.Dimension != "model" {
		t.Errorf("dimension = %q, want %q", entry.Dimension, "model")
	}

	// No match.
	_, err = host.GetReadyProvider("memory", "")
	if err == nil {
		t.Error("expected error for non-existent dimension, got nil")
	}

	_, err = host.GetReadyProvider("model", "nonexistent")
	if err == nil {
		t.Error("expected error for non-existent name, got nil")
	}
}

// TestProviderHost_UnhealthyProvider verifies that a provider reporting not-ready
// is recorded as Unavailable without an RPC error.
func TestProviderHost_UnhealthyProvider(t *testing.T) {
	dir := t.TempDir()

	provider := &fakeCPIProvider{
		name: "sick-provider", version: "1.0.0", dimension: "tool",
		capabilities: []string{"provision"}, healthy: false, // reports not-ready
	}

	startFakeProvider(t, dir, "sick", provider)

	logger := zaptest.NewLogger(t)
	host := NewProviderHost(ProviderHostConfig{
		SocketDir:           dir,
		HealthCheckInterval: 1 * time.Hour,
		HealthCheckTimeout:  5 * time.Second,
		ConnectTimeout:      5 * time.Second,
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := host.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer host.Stop()

	// Wait for registration.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries := host.GetProviders()
		if len(entries) > 0 && entries[0].Name == "sick-provider" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	entries := host.GetProviders()
	if len(entries) == 0 {
		t.Fatal("provider not registered")
	}

	entry := entries[0]
	if entry.HealthStatus != v1alpha1.ProviderHealthUnavailable {
		t.Errorf("health = %q, want %q", entry.HealthStatus, v1alpha1.ProviderHealthUnavailable)
	}
	if entry.ConnectionState != v1alpha1.ProviderConnectionConnected {
		t.Errorf("connection = %q, want %q", entry.ConnectionState, v1alpha1.ProviderConnectionConnected)
	}
	if entry.LastError != "test provider" {
		t.Errorf("lastError = %q, want %q", entry.LastError, "test provider")
	}

	// GetReadyProvider should NOT find this provider.
	_, err := host.GetReadyProvider("tool", "sick-provider")
	if err == nil {
		t.Error("expected error for unhealthy provider, got nil")
	}
}

// TestProviderHost_DuplicateDiscovery verifies that concurrent discover() calls
// for the same socket do not create duplicate connections.
func TestProviderHost_DuplicateDiscovery(t *testing.T) {
	dir := t.TempDir()

	provider := &fakeCPIProvider{
		name: "stable", version: "1.0.0", dimension: "model",
		capabilities: []string{"provision"}, healthy: true,
	}

	startFakeProvider(t, dir, "stable", provider)

	logger := zaptest.NewLogger(t)
	host := NewProviderHost(ProviderHostConfig{
		SocketDir:           dir,
		HealthCheckInterval: 1 * time.Hour,
		HealthCheckTimeout:  5 * time.Second,
		ConnectTimeout:      5 * time.Second,
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := host.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer host.Stop()

	// Wait for initial registration.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(host.GetProviders()) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Call discover() multiple times — should not create duplicates.
	for i := 0; i < 5; i++ {
		host.discover()
	}

	entries := host.GetProviders()
	if len(entries) != 1 {
		t.Errorf("expected 1 provider, got %d", len(entries))
	}
}
