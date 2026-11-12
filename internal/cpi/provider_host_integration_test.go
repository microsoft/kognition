// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package cpi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc/encoding"

	v1alpha1 "github.com/kognition-ai/kognition/api/v1alpha1"
)

func init() {
	encoding.RegisterCodec(JSONCodec{})
}

// TestProviderHost_FullLifecycle is an integration test that exercises the complete
// lifecycle: Start → discover → register → Ready → health degrades → Unavailable →
// recover → Ready again → socket removal → cleanup.
func TestProviderHost_FullLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := t.TempDir()

	provider := &fakeCPIProvider{
		name: "lifecycle-test", version: "2.0.0", dimension: "tool",
		capabilities: []string{"provision", "deprovision"}, healthy: true,
	}

	socketPath, srv := startFakeProvider(t, dir, "lifecycle-test", provider)

	logger := zaptest.NewLogger(t)
	host := NewProviderHost(ProviderHostConfig{
		SocketDir:           dir,
		HealthCheckInterval: 200 * time.Millisecond,
		HealthCheckTimeout:  5 * time.Second,
		ConnectTimeout:      5 * time.Second,
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Phase 1: Start and register ---
	if err := host.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop()

	waitForProviders(t, host, 1, 10*time.Second)
	waitForHealth(t, host, socketPath, v1alpha1.ProviderHealthReady, 10*time.Second)

	entries := host.GetProviders()
	if entries[0].Name != "lifecycle-test" {
		t.Errorf("name = %q, want %q", entries[0].Name, "lifecycle-test")
	}
	if entries[0].Version != "2.0.0" {
		t.Errorf("version = %q, want %q", entries[0].Version, "2.0.0")
	}
	if entries[0].HealthStatus != v1alpha1.ProviderHealthReady {
		t.Errorf("health = %q, want %q", entries[0].HealthStatus, v1alpha1.ProviderHealthReady)
	}
	if entries[0].ConnectionState != v1alpha1.ProviderConnectionConnected {
		t.Errorf("connection = %q, want %q", entries[0].ConnectionState, v1alpha1.ProviderConnectionConnected)
	}

	// GetReadyProvider should find it.
	entry, err := host.GetReadyProvider("tool", "lifecycle-test")
	if err != nil {
		t.Fatalf("GetReadyProvider: %v", err)
	}
	if entry.Name != "lifecycle-test" {
		t.Fatalf("ready provider name = %q, want %q", entry.Name, "lifecycle-test")
	}

	// --- Phase 2: Provider goes unhealthy ---
	provider.mu.Lock()
	provider.healthy = false
	provider.mu.Unlock()

	// Wait for the health loop to detect the change.
	waitForHealth(t, host, socketPath, v1alpha1.ProviderHealthUnavailable, 5*time.Second)

	// GetReadyProvider should NOT find it now.
	_, err = host.GetReadyProvider("tool", "lifecycle-test")
	if err == nil {
		t.Error("expected error for unhealthy provider, got nil")
	}

	// --- Phase 3: Provider recovers ---
	provider.mu.Lock()
	provider.healthy = true
	provider.mu.Unlock()

	waitForHealth(t, host, socketPath, v1alpha1.ProviderHealthReady, 5*time.Second)

	// GetReadyProvider should find it again.
	entry, err = host.GetReadyProvider("tool", "lifecycle-test")
	if err != nil {
		t.Fatalf("GetReadyProvider after recovery: %v", err)
	}
	if entry.HealthStatus != v1alpha1.ProviderHealthReady {
		t.Errorf("health after recovery = %q, want %q", entry.HealthStatus, v1alpha1.ProviderHealthReady)
	}

	// --- Phase 4: Socket removal ---
	srv.Stop()
	os.Remove(socketPath)

	// Trigger discovery to detect removal.
	host.discover()

	if len(host.GetProviders()) != 0 {
		t.Error("expected provider to be removed after socket deletion")
	}

	// --- Phase 5: New provider appears dynamically ---
	provider2 := &fakeCPIProvider{
		name: "dynamic", version: "1.0.0", dimension: "memory",
		capabilities: []string{"read", "write"}, healthy: true,
	}

	startFakeProvider(t, dir, "dynamic", provider2)

	// Trigger discovery.
	host.discover()

	waitForProviders(t, host, 1, 10*time.Second)

	entries = host.GetProviders()
	if entries[0].Name != "dynamic" {
		t.Errorf("dynamic provider name = %q, want %q", entries[0].Name, "dynamic")
	}
	if entries[0].Dimension != "memory" {
		t.Errorf("dynamic provider dimension = %q, want %q", entries[0].Dimension, "memory")
	}
}

// TestProviderHost_MultipleProviders verifies registration of multiple providers
// with different dimensions and correct filtering.
func TestProviderHost_MultipleProviders(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dir := t.TempDir()

	toolProvider := &fakeCPIProvider{
		name: "my-tool", version: "1.0.0", dimension: "tool",
		capabilities: []string{"provision"}, healthy: true,
	}
	modelProvider := &fakeCPIProvider{
		name: "my-model", version: "2.0.0", dimension: "model",
		capabilities: []string{"complete", "embed"}, healthy: true,
	}
	memProvider := &fakeCPIProvider{
		name: "my-memory", version: "1.1.0", dimension: "memory",
		capabilities: []string{"read", "write"}, healthy: true,
	}

	startFakeProvider(t, dir, "tool", toolProvider)
	startFakeProvider(t, dir, "model", modelProvider)
	startFakeProvider(t, dir, "memory", memProvider)

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
		t.Fatalf("Start: %v", err)
	}
	defer host.Stop()

	waitForProviders(t, host, 3, 10*time.Second)

	// Filter by dimension.
	toolEntries := host.GetProvidersByDimension("tool")
	if len(toolEntries) != 1 {
		t.Fatalf("tool dimension: got %d, want 1", len(toolEntries))
	}
	if toolEntries[0].Name != "my-tool" {
		t.Errorf("tool provider name = %q, want %q", toolEntries[0].Name, "my-tool")
	}

	modelEntries := host.GetProvidersByDimension("model")
	if len(modelEntries) != 1 {
		t.Fatalf("model dimension: got %d, want 1", len(modelEntries))
	}
	if modelEntries[0].Name != "my-model" {
		t.Errorf("model provider name = %q, want %q", modelEntries[0].Name, "my-model")
	}

	memEntries := host.GetProvidersByDimension("memory")
	if len(memEntries) != 1 {
		t.Fatalf("memory dimension: got %d, want 1", len(memEntries))
	}
	if memEntries[0].Name != "my-memory" {
		t.Errorf("memory provider name = %q, want %q", memEntries[0].Name, "my-memory")
	}

	// GetReadyProvider by dimension.
	entry, err := host.GetReadyProvider("model", "")
	if err != nil {
		t.Fatalf("GetReadyProvider(model): %v", err)
	}
	if entry.Name != "my-model" {
		t.Errorf("ready model name = %q, want %q", entry.Name, "my-model")
	}
}

// --- Test Helpers ---

// waitForProviders polls until the expected number of providers are fully registered (Name set).
func waitForProviders(t *testing.T, host *ProviderHost, count int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		providers := host.GetProviders()
		if len(providers) >= count {
			allNamed := true
			for _, p := range providers {
				if p.Name == "" {
					allNamed = false
					break
				}
			}
			if allNamed {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d providers (got %d)", count, len(host.GetProviders()))
}

// waitForHealth polls until a specific provider reaches the expected health status.
func waitForHealth(t *testing.T, host *ProviderHost, socketPath string, expected v1alpha1.ProviderHealthStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pc, ok := host.ConnectionForSocket(socketPath)
		if ok {
			pc.mu.RLock()
			health := pc.Info.HealthStatus
			pc.mu.RUnlock()
			if health == expected {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for health %q on socket %s", expected, filepath.Base(socketPath))
}
