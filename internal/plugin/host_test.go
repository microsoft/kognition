// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package plugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	pluginv1alpha1 "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

func TestCapabilityTypeString(t *testing.T) {
	cases := map[CapabilityType]string{
		CapabilityUnspecified: "unspecified",
		CapabilityRuntime:     "runtime",
		CapabilityMemory:      "memory",
		CapabilityTool:        "tool",
		CapabilityModel:       "model",
		CapabilityKnowledge:   "knowledge",
		CapabilityType(99):    "unspecified",
	}
	for ct, want := range cases {
		if got := ct.String(); got != want {
			t.Errorf("CapabilityType(%d).String() = %q, want %q", ct, got, want)
		}
	}
}

func TestPluginStateString(t *testing.T) {
	cases := map[PluginState]string{
		StateDisconnected: "disconnected",
		StateConnecting:   "connecting",
		StateReady:        "ready",
		StateUnhealthy:    "unhealthy",
		PluginState(99):   "unknown",
	}
	for st, want := range cases {
		if got := st.String(); got != want {
			t.Errorf("PluginState(%d).String() = %q, want %q", st, got, want)
		}
	}
}

func TestCapabilityTypeFromProto(t *testing.T) {
	cases := map[pluginv1alpha1.PluginCapability_Type]CapabilityType{
		pluginv1alpha1.PluginCapability_TYPE_UNSPECIFIED: CapabilityUnspecified,
		pluginv1alpha1.PluginCapability_RUNTIME:          CapabilityRuntime,
		pluginv1alpha1.PluginCapability_MEMORY:           CapabilityMemory,
		pluginv1alpha1.PluginCapability_TOOL:             CapabilityTool,
		pluginv1alpha1.PluginCapability_MODEL:            CapabilityModel,
		pluginv1alpha1.PluginCapability_KNOWLEDGE:        CapabilityKnowledge,
		pluginv1alpha1.PluginCapability_Type(99):         CapabilityUnspecified,
	}
	for capability, want := range cases {
		if got := capabilityTypeFromProto(capability); got != want {
			t.Errorf("capabilityTypeFromProto(%d) = %v, want %v", capability, got, want)
		}
		if got := capabilityTypeFromWire(int(capability)); got != want {
			t.Errorf("capabilityTypeFromWire(%d) = %v, want %v", capability, got, want)
		}
	}
}

func TestDefaultHostConfig(t *testing.T) {
	cfg := DefaultHostConfig()
	if cfg.SocketDir == "" {
		t.Error("SocketDir should have a default")
	}
	if cfg.DiscoveryInterval <= 0 || cfg.ProbeInterval <= 0 || cfg.ProbeTimeout <= 0 || cfg.ConnectTimeout <= 0 {
		t.Error("timeouts should be positive")
	}
	if cfg.MaxReconnectBackoff < cfg.ReconnectBackoff {
		t.Error("MaxReconnectBackoff should be >= ReconnectBackoff")
	}
}

func TestHost_StartRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*HostConfig)
	}{
		{name: "empty socket directory", mutate: func(cfg *HostConfig) { cfg.SocketDir = "" }},
		{name: "zero discovery interval", mutate: func(cfg *HostConfig) { cfg.DiscoveryInterval = 0 }},
		{name: "zero probe interval", mutate: func(cfg *HostConfig) { cfg.ProbeInterval = 0 }},
		{name: "backoff range", mutate: func(cfg *HostConfig) { cfg.MaxReconnectBackoff = cfg.ReconnectBackoff / 2 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultHostConfig()
			tt.mutate(&cfg)
			if err := NewHost(cfg, zap.NewNop()).Start(context.Background()); err == nil {
				t.Fatal("Start accepted invalid configuration")
			}
		})
	}
}

// newHostWithPlugins builds a Host and injects connections directly, bypassing
// discovery/gRPC, to exercise the selection logic in isolation.
func newHostWithPlugins(conns map[string]*PluginConnection) *Host {
	h := NewHost(DefaultHostConfig(), zap.NewNop())
	h.plugins = conns
	return h
}

func TestHost_GetPlugins(t *testing.T) {
	h := newHostWithPlugins(map[string]*PluginConnection{
		"/s/a.sock": {Info: PluginInfo{Name: "a", Capabilities: []CapabilityType{CapabilityRuntime}}, State: StateReady},
		"/s/b.sock": {Info: PluginInfo{Name: "b"}, State: StateUnhealthy},
	})
	plugins := h.GetPlugins()
	if got := len(plugins); got != 2 {
		t.Errorf("GetPlugins len = %d, want 2", got)
	}
	for index := range plugins {
		if plugins[index].Info.Name == "a" {
			plugins[index].Info.Capabilities[0] = CapabilityTool
		}
	}
	if snapshot := h.GetPlugins(); snapshot[0].Info.Name == "a" && snapshot[0].Info.Capabilities[0] != CapabilityRuntime || snapshot[1].Info.Name == "a" && snapshot[1].Info.Capabilities[0] != CapabilityRuntime {
		t.Fatal("GetPlugins returned host-owned capability storage")
	}
	if got := len(NewHost(DefaultHostConfig(), zap.NewNop()).GetPlugins()); got != 0 {
		t.Errorf("empty host GetPlugins len = %d, want 0", got)
	}
}

func TestHost_GetPluginsByCapability(t *testing.T) {
	h := newHostWithPlugins(map[string]*PluginConnection{
		"/s/rt.sock":  {Info: PluginInfo{Name: "rt", Capabilities: []CapabilityType{CapabilityRuntime}}, State: StateReady},
		"/s/mem.sock": {Info: PluginInfo{Name: "mem", Capabilities: []CapabilityType{CapabilityMemory, CapabilityTool}}, State: StateReady},
	})
	runtimePlugins := h.GetPluginsByCapability(CapabilityRuntime)
	if got := len(runtimePlugins); got != 1 {
		t.Errorf("runtime plugins = %d, want 1", got)
	}
	runtimePlugins[0].Info.Capabilities[0] = CapabilityTool
	if got := h.GetPluginsByCapability(CapabilityRuntime); len(got) != 1 || got[0].Info.Capabilities[0] != CapabilityRuntime {
		t.Fatalf("GetPluginsByCapability returned host-owned capability storage: %+v", got)
	}
	if got := len(h.GetPluginsByCapability(CapabilityTool)); got != 1 {
		t.Errorf("tool plugins = %d, want 1", got)
	}
	if got := len(h.GetPluginsByCapability(CapabilityModel)); got != 0 {
		t.Errorf("model plugins = %d, want 0", got)
	}
}

func TestHost_GetReadyPlugin(t *testing.T) {
	h := newHostWithPlugins(map[string]*PluginConnection{
		"/s/ready.sock":     {Info: PluginInfo{Name: "ready", Capabilities: []CapabilityType{CapabilityRuntime}}, State: StateReady},
		"/s/unhealthy.sock": {Info: PluginInfo{Name: "sick", Capabilities: []CapabilityType{CapabilityRuntime}}, State: StateUnhealthy},
	})

	// Found by capability.
	pc, err := h.GetReadyPlugin(CapabilityRuntime, "")
	if err != nil || pc == nil || pc.Info.Name != "ready" {
		t.Fatalf("GetReadyPlugin(runtime,\"\") = %v, %v; want ready plugin", pc, err)
	}
	pc.Info.Name = "caller-mutated"
	pc.Info.Capabilities[0] = CapabilityTool
	pc.State = StateUnhealthy
	pc, err = h.GetReadyPlugin(CapabilityRuntime, "ready")
	if err != nil || pc.Info.Name != "ready" || pc.Info.Capabilities[0] != CapabilityRuntime || pc.State != StateReady {
		t.Fatalf("GetReadyPlugin returned host-owned state: %v, %v", pc, err)
	}

	// Name filter that matches.
	if _, err := h.GetReadyPlugin(CapabilityRuntime, "ready"); err != nil {
		t.Errorf("GetReadyPlugin(runtime,ready) unexpected error: %v", err)
	}

	// Name filter that does not match any ready plugin.
	if _, err := h.GetReadyPlugin(CapabilityRuntime, "nope"); err == nil {
		t.Error("expected error for unknown name")
	}

	// Non-ready plugins are skipped: no ready Memory plugin exists.
	if _, err := h.GetReadyPlugin(CapabilityMemory, ""); err == nil {
		t.Error("expected error when no ready plugin has the capability")
	}
}

func TestHost_GetReadyPluginSelectionIsDeterministic(t *testing.T) {
	h := newHostWithPlugins(map[string]*PluginConnection{
		"/s/z.sock": {Info: PluginInfo{Name: "zeta", SocketPath: "/s/z.sock", Capabilities: []CapabilityType{CapabilityRuntime}}, State: StateReady},
		"/s/b.sock": {Info: PluginInfo{Name: "alpha", SocketPath: "/s/b.sock", Capabilities: []CapabilityType{CapabilityRuntime}}, State: StateReady},
		"/s/a.sock": {Info: PluginInfo{Name: "alpha", SocketPath: "/s/a.sock", Capabilities: []CapabilityType{CapabilityRuntime}}, State: StateReady},
	})
	for range 100 {
		plugin, err := h.GetReadyPlugin(CapabilityRuntime, "")
		if err != nil {
			t.Fatalf("GetReadyPlugin: %v", err)
		}
		if plugin.Info.Name != "alpha" || plugin.Info.SocketPath != "/s/a.sock" {
			t.Fatalf("selected plugin = %+v, want alpha at /s/a.sock", plugin.Info)
		}
	}
}

func TestHost_ConnectionForSocket(t *testing.T) {
	h := newHostWithPlugins(map[string]*PluginConnection{
		"/s/a.sock": {
			Info:  PluginInfo{Name: "a", Capabilities: []CapabilityType{CapabilityRuntime}},
			State: StateReady,
		},
	})
	connection, ok := h.ConnectionForSocket("/s/a.sock")
	if !ok {
		t.Error("expected hit for known socket")
	}
	if _, ok := h.ConnectionForSocket("/s/missing.sock"); ok {
		t.Error("expected miss for unknown socket")
	}

	connection.Info.Name = "caller-mutated"
	connection.Info.Capabilities[0] = CapabilityTool
	connection.State = StateUnhealthy

	snapshot, ok := h.ConnectionForSocket("/s/a.sock")
	if !ok || snapshot.Info.Name != "a" || snapshot.Info.Capabilities[0] != CapabilityRuntime || snapshot.State != StateReady {
		t.Fatalf("host state escaped through accessor: %+v, present = %v", snapshot, ok)
	}
}

func TestHost_Discover_NonexistentDir(t *testing.T) {
	h := NewHost(HostConfig{SocketDir: filepath.Join(t.TempDir(), "does-not-exist")}, zap.NewNop())
	if err := h.discover(); err != nil {
		t.Errorf("discover on missing dir should be nil, got %v", err)
	}
	if len(h.GetPlugins()) != 0 {
		t.Error("no plugins expected")
	}
}

func TestHost_Discover_IgnoresNonSocketEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notasocket.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0750); err != nil {
		t.Fatal(err)
	}
	h := NewHost(HostConfig{SocketDir: dir}, zap.NewNop())
	if err := h.discover(); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(h.GetPlugins()) != 0 {
		t.Errorf("non-.sock entries should be ignored, got %d plugins", len(h.GetPlugins()))
	}
}

func TestHost_Discover_ConnectsPluginSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "discovered.sock")
	listener, err := ListenPlugin(socketPath)
	if err != nil {
		t.Fatalf("ListenPlugin: %v", err)
	}
	server := pluginsdk.NewServer(pluginsdk.ServerConfig{
		Name:         "discovered-runtime",
		Version:      "1.0.0",
		Capabilities: []pluginsdk.Capability{pluginsdk.CapabilityRuntime},
	})
	go func() {
		_ = server.GRPCServer().Serve(listener)
	}()
	t.Cleanup(server.Stop)

	cfg := DefaultHostConfig()
	cfg.SocketDir = filepath.Dir(socketPath)
	cfg.ConnectTimeout = 2 * time.Second
	h := NewHost(cfg, zap.NewNop())
	t.Cleanup(h.Stop)
	if err := h.discover(); err != nil {
		t.Fatalf("discover: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pc, ok := h.ConnectionForSocket(socketPath)
		if ok && pc.State == StateReady {
			if pc.Info.Name != "discovered-runtime" {
				t.Fatalf("discovered plugin name = %q", pc.Info.Name)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	pc, _ := h.ConnectionForSocket(socketPath)
	t.Fatalf("discovered plugin did not become ready: %+v", pc)
}

func TestHost_Discover_CleansUpRemovedSockets(t *testing.T) {
	dir := t.TempDir()
	h := NewHost(HostConfig{SocketDir: dir}, zap.NewNop())
	// Inject a plugin whose socket file no longer exists.
	stale := filepath.Join(dir, "gone.sock")
	h.plugins[stale] = &PluginConnection{
		Info:      PluginInfo{Name: "gone", SocketPath: stale},
		State:     StateReady,
		LastProbe: time.Now(),
	}
	if err := h.discover(); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if _, ok := h.ConnectionForSocket(stale); ok {
		t.Error("stale plugin should have been cleaned up")
	}
}

func TestHost_ConnectProbeAndReconnectLifecycle(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "runtime.sock")
	listener, err := ListenPlugin(socketPath)
	if err != nil {
		t.Fatalf("ListenPlugin: %v", err)
	}

	server := pluginsdk.NewServer(pluginsdk.ServerConfig{
		Name:         "native-runtime",
		Version:      "1.2.3",
		Description:  "test runtime",
		Vendor:       "kognition",
		Capabilities: []pluginsdk.Capability{pluginsdk.CapabilityRuntime, pluginsdk.CapabilityTool},
	})
	registered := make(chan string, 2)
	server.OnRegistered(func(hostVersion string) { registered <- hostVersion })
	go func() {
		_ = server.GRPCServer().Serve(listener)
	}()
	t.Cleanup(server.Stop)

	cfg := DefaultHostConfig()
	cfg.ConnectTimeout = 2 * time.Second
	cfg.ProbeTimeout = time.Second
	cfg.HostVersion = "9.8.7"
	h := NewHost(cfg, zap.NewNop())
	t.Cleanup(h.Stop)

	h.connectPlugin(socketPath)
	pc, ok := h.ConnectionForSocket(socketPath)
	if !ok || pc.State != StateReady || pc.Conn == nil {
		t.Fatalf("connected plugin = %+v, present = %v", pc, ok)
	}
	if pc.Info.Name != "native-runtime" || pc.Info.Version != "1.2.3" || pc.Info.Description != "test runtime" || pc.Info.Vendor != "kognition" {
		t.Fatalf("plugin info = %+v", pc.Info)
	}
	if len(pc.Info.Capabilities) != 2 || pc.Info.Capabilities[0] != CapabilityRuntime || pc.Info.Capabilities[1] != CapabilityTool {
		t.Fatalf("plugin capabilities = %+v", pc.Info.Capabilities)
	}
	select {
	case got := <-registered:
		if got != cfg.HostVersion {
			t.Fatalf("registered host version = %q, want %q", got, cfg.HostVersion)
		}
	case <-time.After(time.Second):
		t.Fatal("plugin registration callback was not invoked")
	}

	server.SetReady(false)
	h.probeAll(context.Background())
	if pc, _ = h.ConnectionForSocket(socketPath); pc.State != StateUnhealthy || pc.LastProbe.IsZero() {
		t.Fatalf("plugin after failed readiness probe = %+v", pc)
	}
	server.SetReady(true)
	h.probeAll(context.Background())
	if pc, _ = h.ConnectionForSocket(socketPath); pc.State != StateReady || pc.LastError != nil {
		t.Fatalf("plugin after recovery = %+v", pc)
	}
	if err := pc.Conn.Close(); err != nil {
		t.Fatalf("close plugin connection: %v", err)
	}
	h.probeAll(context.Background())
	if pc, _ = h.ConnectionForSocket(socketPath); pc.State != StateDisconnected || pc.Conn != nil || pc.LastError == nil {
		t.Fatalf("plugin after transport failure = %+v", pc)
	}

	h.Reconnect(socketPath)
	if pc, _ = h.ConnectionForSocket(socketPath); pc.State != StateReady || pc.Conn == nil {
		t.Fatalf("reconnected plugin = %+v", pc)
	}
}

func TestHost_InitiallyUnreadyPluginRecoversWithoutReconnect(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "kognition-plugin-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "runtime.sock")
	listener, err := ListenPlugin(socketPath)
	if err != nil {
		t.Fatalf("ListenPlugin: %v", err)
	}

	server := pluginsdk.NewServer(pluginsdk.ServerConfig{
		Name:         "warming-runtime",
		Version:      "1.0.0",
		Capabilities: []pluginsdk.Capability{pluginsdk.CapabilityRuntime},
	})
	registered := make(chan string, 1)
	server.OnRegistered(func(hostVersion string) { registered <- hostVersion })
	server.SetReady(false)
	go func() { _ = server.GRPCServer().Serve(listener) }()
	t.Cleanup(server.Stop)

	cfg := DefaultHostConfig()
	cfg.ConnectTimeout = 2 * time.Second
	h := NewHost(cfg, zap.NewNop())
	t.Cleanup(h.Stop)
	h.connectPlugin(socketPath)

	unready, ok := h.ConnectionForSocket(socketPath)
	if !ok || unready.State != StateUnhealthy || unready.Conn == nil {
		t.Fatalf("initially unready plugin = %+v, present = %v", unready, ok)
	}
	if unready.Info.Name != "warming-runtime" || unready.LastProbe.IsZero() {
		t.Fatalf("initially unready plugin metadata = %+v", unready)
	}
	select {
	case hostVersion := <-registered:
		if hostVersion != cfg.HostVersion {
			t.Fatalf("registered host version = %q, want %q", hostVersion, cfg.HostVersion)
		}
	case <-time.After(time.Second):
		t.Fatal("initially unready plugin did not complete registration")
	}

	server.SetReady(true)
	h.probeAll(context.Background())
	recovered, ok := h.ConnectionForSocket(socketPath)
	if !ok || recovered.Conn != unready.Conn || recovered.State != StateReady || recovered.LastError != nil {
		t.Fatalf("recovered plugin = %+v, present = %v; want ready snapshot of same transport", recovered, ok)
	}
}

func TestHost_StartStopAndFailedConnection(t *testing.T) {
	cfg := DefaultHostConfig()
	cfg.SocketDir = filepath.Join(t.TempDir(), "plugins")
	cfg.ProbeInterval = time.Hour
	cfg.ConnectTimeout = 20 * time.Millisecond
	h := NewHost(cfg, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	if err := h.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.Start(ctx); err == nil {
		t.Fatal("second Start() succeeded")
	}
	cancel()
	h.Stop()
	if err := h.Start(context.Background()); err == nil {
		t.Fatal("Start() after Stop() succeeded")
	}
	if _, err := os.Stat(cfg.SocketDir); err != nil {
		t.Fatalf("socket directory after Start: %v", err)
	}

	failed := NewHost(cfg, zap.NewNop())
	missingSocket := filepath.Join(cfg.SocketDir, "missing.sock")
	failed.connectPlugin(missingSocket)
	pc, ok := failed.ConnectionForSocket(missingSocket)
	if !ok || pc.State != StateDisconnected || pc.LastError == nil {
		t.Fatalf("failed connection = %+v, present = %v", pc, ok)
	}
	failed.Reconnect(filepath.Join(cfg.SocketDir, "unknown.sock"))
}

func TestHost_ConcurrentStartAndStop(t *testing.T) {
	for range 50 {
		cfg := DefaultHostConfig()
		cfg.SocketDir = filepath.Join(t.TempDir(), "plugins")
		cfg.ConnectTimeout = time.Millisecond
		if err := os.MkdirAll(cfg.SocketDir, 0750); err != nil {
			t.Fatalf("create socket directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(cfg.SocketDir, "candidate.sock"), nil, 0600); err != nil {
			t.Fatalf("create socket candidate: %v", err)
		}
		h := NewHost(cfg, zap.NewNop())
		startDone := make(chan error, 1)
		stopDone := make(chan struct{})
		go func() { startDone <- h.Start(context.Background()) }()
		go func() {
			h.Stop()
			close(stopDone)
		}()
		select {
		case err := <-startDone:
			if err != nil && !strings.Contains(err.Error(), "cannot start after stop") {
				t.Fatalf("concurrent Start error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent Start did not return")
		}
		select {
		case <-stopDone:
		case <-time.After(time.Second):
			t.Fatal("concurrent Stop did not return")
		}
		if plugins := h.GetPlugins(); len(plugins) != 0 {
			t.Fatalf("stopped host retained plugins: %+v", plugins)
		}
	}
}

func TestHost_AutomaticallyReconnectsFailedPlugin(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "kognition-plugin-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "runtime.sock")
	if err := os.WriteFile(socketPath, nil, 0600); err != nil {
		t.Fatalf("create stale socket: %v", err)
	}

	cfg := DefaultHostConfig()
	cfg.SocketDir = socketDir
	cfg.ConnectTimeout = 50 * time.Millisecond
	cfg.ReconnectBackoff = 10 * time.Millisecond
	cfg.MaxReconnectBackoff = 20 * time.Millisecond
	cfg.ProbeInterval = time.Hour
	h := NewHost(cfg, zap.NewNop())
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(h.Stop)

	deadline := time.Now().Add(time.Second)
	for {
		pc, ok := pluginSnapshotForSocket(h, socketPath)
		if ok && pc.State == StateDisconnected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("plugin did not record initial connection failure: %+v", pc)
		}
		time.Sleep(10 * time.Millisecond)
	}

	listener, err := ListenPlugin(socketPath)
	if err != nil {
		t.Fatalf("ListenPlugin: %v", err)
	}
	server := pluginsdk.NewServer(pluginsdk.ServerConfig{
		Name:         "restarted-runtime",
		Version:      "1.0.0",
		Capabilities: []pluginsdk.Capability{pluginsdk.CapabilityRuntime},
	})
	go func() { _ = server.GRPCServer().Serve(listener) }()
	t.Cleanup(server.Stop)

	deadline = time.Now().Add(2 * time.Second)
	for {
		pc, ok := pluginSnapshotForSocket(h, socketPath)
		if ok && pc.State == StateReady && pc.Info.Name == "restarted-runtime" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("plugin did not reconnect automatically: %+v", pc)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHost_ReconnectsWhenSocketIsReplacedAtSamePath(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "kognition-plugin-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "runtime.sock")
	startServer := func(name string) *pluginsdk.Server {
		listener, err := ListenPlugin(socketPath)
		if err != nil {
			t.Fatalf("ListenPlugin: %v", err)
		}
		server := pluginsdk.NewServer(pluginsdk.ServerConfig{
			Name:         name,
			Version:      "1.0.0",
			Capabilities: []pluginsdk.Capability{pluginsdk.CapabilityRuntime},
		})
		go func() { _ = server.GRPCServer().Serve(listener) }()
		t.Cleanup(server.Stop)
		return server
	}

	server := startServer("original-runtime")
	cfg := DefaultHostConfig()
	cfg.SocketDir = filepath.Dir(socketPath)
	cfg.DiscoveryInterval = 10 * time.Millisecond
	cfg.ProbeInterval = time.Hour
	cfg.ProbeTimeout = 50 * time.Millisecond
	cfg.ConnectTimeout = 100 * time.Millisecond
	cfg.ReconnectBackoff = 10 * time.Millisecond
	cfg.MaxReconnectBackoff = 20 * time.Millisecond
	h := NewHost(cfg, zap.NewNop())
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(h.Stop)
	waitForPluginState(t, h, socketPath, StateReady, "original-runtime")

	t.Run("preserved file identity", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows cannot change Unix socket timestamps")
		}
		original, ok := h.ConnectionForSocket(socketPath)
		if !ok {
			t.Fatal("original plugin connection missing")
		}
		originalSocket, err := os.Stat(socketPath)
		if err != nil {
			t.Fatal(err)
		}
		// Exercise a new file version without relying on filesystem inode reuse.
		modified := originalSocket.ModTime().Add(time.Second)
		if err := os.Chtimes(socketPath, modified, modified); err != nil {
			t.Fatal(err)
		}
		updatedSocket, err := os.Stat(socketPath)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(originalSocket, updatedSocket) || originalSocket.ModTime().Equal(updatedSocket.ModTime()) {
			t.Fatal("socket fixture must preserve file identity while changing its modification time")
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			current, ok := h.ConnectionForSocket(socketPath)
			if ok && current.State == StateReady && current.Info.Name == "original-runtime" && current.Conn != original.Conn {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("plugin did not reconnect after socket file version changed: %+v", current)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	server.Stop()
	server = startServer("restarted-runtime")
	waitForPluginState(t, h, socketPath, StateReady, "restarted-runtime")
}

func TestHost_ConcurrentAccessorsProbeAndReconnect(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "kognition-plugin-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "runtime.sock")
	listener, err := ListenPlugin(socketPath)
	if err != nil {
		t.Fatalf("ListenPlugin: %v", err)
	}
	server := pluginsdk.NewServer(pluginsdk.ServerConfig{
		Name:         "concurrent-runtime",
		Version:      "1.0.0",
		Capabilities: []pluginsdk.Capability{pluginsdk.CapabilityRuntime},
	})
	go func() { _ = server.GRPCServer().Serve(listener) }()
	t.Cleanup(server.Stop)

	cfg := DefaultHostConfig()
	cfg.ConnectTimeout = time.Second
	cfg.ProbeTimeout = time.Second
	h := NewHost(cfg, zap.NewNop())
	h.connectPlugin(socketPath)
	t.Cleanup(h.Stop)
	waitForPluginState(t, h, socketPath, StateReady, "concurrent-runtime")

	var readers sync.WaitGroup
	readers.Add(2)
	for range 2 {
		go func() {
			defer readers.Done()
			for range 50 {
				if connection, ok := h.ConnectionForSocket(socketPath); ok {
					connection.State = StateUnhealthy
					connection.Info.Capabilities = nil
				}
				if connection, err := h.GetReadyPlugin(CapabilityRuntime, "concurrent-runtime"); err == nil {
					connection.Info.Name = "caller-mutated"
				}
			}
		}()
	}
	for index := range 20 {
		server.SetReady(index%2 == 0)
		h.probeAll(context.Background())
		if index%5 == 0 {
			h.Reconnect(socketPath)
		}
	}
	readers.Wait()
	server.SetReady(true)
	h.probeAll(context.Background())
	waitForPluginState(t, h, socketPath, StateReady, "concurrent-runtime")
}

func TestHost_RegistrationFailureDoesNotPublishReadyPlugin(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "kognition-plugin-")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "reject.sock")
	listener, err := ListenPlugin(socketPath)
	if err != nil {
		t.Fatalf("ListenPlugin: %v", err)
	}
	server, registrationAttempted := newRegistrationRejectingServer()
	go func() { _ = server.Serve(listener) }()

	cfg := DefaultHostConfig()
	cfg.SocketDir = socketDir
	cfg.DiscoveryInterval = time.Hour
	cfg.ProbeInterval = time.Hour
	cfg.ConnectTimeout = 100 * time.Millisecond
	cfg.ReconnectBackoff = 10 * time.Millisecond
	cfg.MaxReconnectBackoff = 20 * time.Millisecond
	h := NewHost(cfg, zap.NewNop())
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(h.Stop)
	select {
	case <-registrationAttempted:
	case <-time.After(time.Second):
		t.Fatal("host did not attempt plugin registration")
	}
	if _, err := h.GetReadyPlugin(CapabilityRuntime, "rejecting-runtime"); err == nil {
		t.Fatal("plugin with rejected registration was published as ready")
	}

	server.Stop()
	listener, err = ListenPlugin(socketPath)
	if err != nil {
		t.Fatalf("restart listener: %v", err)
	}
	recovered := pluginsdk.NewServer(pluginsdk.ServerConfig{
		Name:         "recovered-runtime",
		Version:      "1.0.0",
		Capabilities: []pluginsdk.Capability{pluginsdk.CapabilityRuntime},
	})
	go func() { _ = recovered.GRPCServer().Serve(listener) }()
	t.Cleanup(recovered.Stop)
	waitForPluginState(t, h, socketPath, StateReady, "recovered-runtime")
}

func TestHost_ScheduledReconnectDoesNotReplaceNewerConnection(t *testing.T) {
	cfg := DefaultHostConfig()
	h := NewHost(cfg, zap.NewNop())
	h.started = true

	socketPath := filepath.Join(t.TempDir(), "runtime.sock")
	failed := &PluginConnection{
		Info:  PluginInfo{Name: "failed", SocketPath: socketPath},
		State: StateDisconnected,
	}
	newer := &PluginConnection{
		Info:  PluginInfo{Name: "newer", SocketPath: socketPath},
		State: StateReady,
	}
	h.plugins[socketPath] = failed

	h.scheduleReconnect(socketPath, failed, 20*time.Millisecond)
	h.mu.Lock()
	h.plugins[socketPath] = newer
	h.mu.Unlock()
	done := make(chan struct{})
	go func() {
		h.connectWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scheduled reconnect")
	}

	got, ok := h.ConnectionForSocket(socketPath)
	if !ok || got.Info.Name != newer.Info.Name || got.State != newer.State {
		t.Fatalf("connection after stale reconnect = %+v, present = %v; want newer connection", got, ok)
	}
}

func TestHost_StopCancelsPendingReconnect(t *testing.T) {
	h := NewHost(DefaultHostConfig(), zap.NewNop())
	h.started = true

	socketPath := filepath.Join(t.TempDir(), "runtime.sock")
	failed := &PluginConnection{
		Info:  PluginInfo{Name: "failed", SocketPath: socketPath},
		State: StateDisconnected,
	}
	h.plugins[socketPath] = failed
	h.scheduleReconnect(socketPath, failed, time.Hour)

	stopped := make(chan struct{})
	go func() {
		h.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel pending reconnect")
	}
	if _, ok := h.ConnectionForSocket(socketPath); ok {
		t.Fatal("stopped host retained failed plugin connection")
	}
}

func pluginSnapshotForSocket(h *Host, socketPath string) (PluginConnection, bool) {
	for _, plugin := range h.GetPlugins() {
		if plugin.Info.SocketPath == socketPath {
			return plugin, true
		}
	}
	return PluginConnection{}, false
}

func waitForPluginState(t *testing.T, host *Host, socketPath string, state PluginState, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		plugin, ok := pluginSnapshotForSocket(host, socketPath)
		if ok && plugin.State == state && plugin.Info.Name == name {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	plugin, _ := pluginSnapshotForSocket(host, socketPath)
	t.Fatalf("plugin did not reach state %s with name %q: %+v", state, name, plugin)
}

type registrationRejectingPlugin struct {
	pluginv1alpha1.UnimplementedPluginServer
	registrationAttempted chan struct{}
	once                  sync.Once
}

func newRegistrationRejectingServer() (*grpc.Server, <-chan struct{}) {
	plugin := &registrationRejectingPlugin{registrationAttempted: make(chan struct{})}
	server := grpc.NewServer()
	pluginv1alpha1.RegisterPluginServer(server, plugin)
	return server, plugin.registrationAttempted
}

func (p *registrationRejectingPlugin) GetPluginInfo(context.Context, *pluginv1alpha1.GetPluginInfoRequest) (*pluginv1alpha1.GetPluginInfoResponse, error) {
	return &pluginv1alpha1.GetPluginInfoResponse{Name: "rejecting-runtime", Version: "1.0.0"}, nil
}

func (p *registrationRejectingPlugin) GetPluginCapabilities(context.Context, *pluginv1alpha1.GetPluginCapabilitiesRequest) (*pluginv1alpha1.GetPluginCapabilitiesResponse, error) {
	return &pluginv1alpha1.GetPluginCapabilitiesResponse{Capabilities: []*pluginv1alpha1.PluginCapability{{Type: pluginv1alpha1.PluginCapability_RUNTIME}}}, nil
}

func (p *registrationRejectingPlugin) Probe(context.Context, *pluginv1alpha1.PluginProbeRequest) (*pluginv1alpha1.PluginProbeResponse, error) {
	return &pluginv1alpha1.PluginProbeResponse{Ready: true}, nil
}

func (p *registrationRejectingPlugin) NotifyRegistered(context.Context, *pluginv1alpha1.NotifyRegisteredRequest) (*pluginv1alpha1.NotifyRegisteredResponse, error) {
	p.once.Do(func() { close(p.registrationAttempted) })
	return nil, errors.New("registration rejected")
}
