// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pluginsdk

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pluginv1alpha1 "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

func TestNewServer_Defaults(t *testing.T) {
	s := NewServer(ServerConfig{Name: "p", Version: "1.0.0"})
	if s.logger == nil {
		t.Error("nil logger should default to no-op")
	}
	if !s.ready {
		t.Error("server should start ready")
	}
	if s.grpcServer == nil {
		t.Error("grpcServer should be initialized")
	}
	if s.GRPCServer() == nil {
		t.Error("GRPCServer() should be non-nil")
	}
}

func TestSetReady(t *testing.T) {
	s := NewServer(ServerConfig{Name: "p", Logger: zap.NewNop()})
	s.SetReady(false)
	if s.ready {
		t.Error("SetReady(false) should clear readiness")
	}
	s.SetReady(true)
	if !s.ready {
		t.Error("SetReady(true) should set readiness")
	}
}

func TestOnRegistered_SetsCallback(t *testing.T) {
	s := NewServer(ServerConfig{Name: "p"})
	if s.onRegistered != nil {
		t.Error("callback should start nil")
	}
	s.OnRegistered(func(string) {})
	if s.onRegistered == nil {
		t.Error("OnRegistered should set the callback")
	}
}

func TestGetPluginInfo(t *testing.T) {
	s := NewServer(ServerConfig{Name: "my-plugin", Version: "2.1.0", Description: "d", Vendor: "acme"})
	info, err := s.GetPluginInfo(context.Background(), &pluginv1alpha1.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("GetPluginInfo: %v", err)
	}
	if info.GetName() != "my-plugin" || info.GetVersion() != "2.1.0" || info.GetVendor() != "acme" {
		t.Errorf("unexpected info: %+v", info)
	}
}

func TestGetPluginCapabilities(t *testing.T) {
	s := NewServer(ServerConfig{
		Name:         "p",
		Capabilities: []Capability{CapabilityRuntime, CapabilityTool},
	})
	caps, err := s.GetPluginCapabilities(context.Background(), &pluginv1alpha1.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities: %v", err)
	}
	if len(caps.GetCapabilities()) != 2 {
		t.Fatalf("caps len = %d, want 2", len(caps.GetCapabilities()))
	}
	if caps.GetCapabilities()[0].GetType() != pluginv1alpha1.PluginCapability_RUNTIME || caps.GetCapabilities()[1].GetType() != pluginv1alpha1.PluginCapability_TOOL {
		t.Errorf("unexpected capabilities: %+v", caps.GetCapabilities())
	}
}

func TestProbe(t *testing.T) {
	s := NewServer(ServerConfig{Name: "p"})

	resp, err := s.Probe(context.Background(), &pluginv1alpha1.PluginProbeRequest{})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !resp.GetReady() {
		t.Error("expected Ready=true by default")
	}

	s.SetReady(false)
	resp, _ = s.Probe(context.Background(), &pluginv1alpha1.PluginProbeRequest{})
	if resp.GetReady() {
		t.Error("expected Ready=false after SetReady(false)")
	}
}

func TestNotifyRegisteredInvokesCallback(t *testing.T) {
	s := NewServer(ServerConfig{Name: "p"})

	var gotVersion string
	s.OnRegistered(func(v string) { gotVersion = v })

	if _, err := s.NotifyRegistered(context.Background(), &pluginv1alpha1.NotifyRegisteredRequest{HostVersion: "9.9.9"}); err != nil {
		t.Fatalf("NotifyRegistered: %v", err)
	}
	if gotVersion != "9.9.9" {
		t.Errorf("callback host version = %q, want 9.9.9", gotVersion)
	}
}

func TestGeneratedPluginClientInteroperatesWithServer(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "kpg-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "plugin.sock")
	server := NewServer(ServerConfig{
		Name:         "protobuf-plugin",
		Version:      "1.0.0",
		Capabilities: []Capability{CapabilityRuntime},
		SocketPath:   socketPath,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.Serve(ctx) }()
	waitForUnixSocket(t, socketPath)

	conn, err := grpc.NewClient(
		"passthrough:///"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		}),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := pluginv1alpha1.NewPluginClient(conn)
	info, err := client.GetPluginInfo(context.Background(), &pluginv1alpha1.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("generated GetPluginInfo: %v", err)
	}
	if info.GetName() != "protobuf-plugin" {
		t.Fatalf("plugin name = %q, want protobuf-plugin", info.GetName())
	}
}

func TestServerServeStopsWhenContextIsCancelled(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "plugin.sock")
	server := NewServer(ServerConfig{Name: "p", SocketPath: socketPath})
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(ctx)
	}()

	waitForUnixSocket(t, socketPath)
	cancel()

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() did not stop after context cancellation")
	}
}

func TestServerStopStopsServing(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "plugin.sock")
	server := NewServer(ServerConfig{Name: "p", SocketPath: socketPath})
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(context.Background())
	}()

	waitForUnixSocket(t, socketPath)
	server.Stop()

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() did not stop after Stop()")
	}
}

func TestServerServeReportsStaleSocketRemovalFailure(t *testing.T) {
	nonEmptyDirectory := filepath.Join(t.TempDir(), "plugin.sock")
	if err := os.Mkdir(nonEmptyDirectory, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nonEmptyDirectory, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	server := NewServer(ServerConfig{Name: "p", SocketPath: nonEmptyDirectory})
	err := server.Serve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pluginsdk: remove stale socket") {
		t.Fatalf("Serve() error = %v, want stale socket removal context", err)
	}
}

func TestServerServeReportsListenFailure(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "missing", "plugin.sock")
	server := NewServer(ServerConfig{Name: "p", SocketPath: socketPath})

	err := server.Serve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pluginsdk: listen on "+socketPath) {
		t.Fatalf("Serve() error = %v, want listen context", err)
	}
}

func TestServerServeReportsGRPCFailure(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "plugin.sock")
	server := NewServer(ServerConfig{Name: "p", SocketPath: socketPath})
	server.Stop()

	err := server.Serve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pluginsdk: serve") {
		t.Fatalf("Serve() error = %v, want gRPC serve context", err)
	}
}

func waitForUnixSocket(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err == nil {
			if err := conn.Close(); err != nil {
				t.Fatalf("close probe connection: %v", err)
			}
			return
		}
	}
	t.Fatalf("server did not listen on %s", socketPath)
}
