// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package pluginsdk provides a Go SDK for building Kognition sidecar plugins.
// Plugin authors use this package to expose their capabilities (runtime, memory,
// tool, model) as gRPC services over Unix sockets.
//
// EXPERIMENTAL: the Kubernetes controller can host these sidecars when plugin
// discovery is enabled. The API here may change before it is generally available.
//
// Usage:
//
//	srv := pluginsdk.NewServer(pluginsdk.ServerConfig{
//	    SocketPath:  "/var/run/kognition/plugins/my-plugin.sock",
//	    Name:        "my-plugin",
//	    Version:     "1.0.0",
//	    Description: "My custom runtime plugin",
//	    Capabilities: []pluginsdk.Capability{pluginsdk.CapabilityRuntime},
//	})
//	srv.Serve(ctx)
package pluginsdk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	pluginv1alpha1 "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

// Capability identifies a service category this plugin provides.
type Capability int

const (
	CapabilityRuntime   Capability = 1
	CapabilityMemory    Capability = 2
	CapabilityTool      Capability = 3
	CapabilityModel     Capability = 4
	CapabilityKnowledge Capability = 5
)

// ServerConfig configures the plugin gRPC server.
type ServerConfig struct {
	// SocketPath is the Unix socket file the plugin listens on.
	SocketPath string

	// Name is the unique plugin identifier.
	Name string

	// Version is the semantic version of this plugin.
	Version string

	// Description is a human-readable summary.
	Description string

	// Vendor is the plugin author/maintainer.
	Vendor string

	// Capabilities declares which services this plugin provides.
	Capabilities []Capability

	// Logger is the zap logger instance. If nil, a no-op logger is used.
	Logger *zap.Logger
}

// Server is a gRPC server that implements the Plugin identity service
// and allows plugin authors to register their capability handlers.
type Server struct {
	pluginv1alpha1.UnimplementedPluginServer
	cfg           ServerConfig
	grpcServer    *grpc.Server
	logger        *zap.Logger
	ready         bool
	mu            sync.RWMutex
	onRegistered  func(hostVersion string)
	serving       bool
	stopRequested bool
}

// NewServer creates a new plugin server.
func NewServer(cfg ServerConfig) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}

	s := &Server{
		cfg:    cfg,
		logger: logger,
		ready:  true,
	}

	s.grpcServer = grpc.NewServer()
	pluginv1alpha1.RegisterPluginServer(s.grpcServer, s)

	return s
}

// GRPCServer returns the underlying gRPC server for registering additional services.
// Plugin authors use this to add their capability-specific service implementations.
func (s *Server) GRPCServer() *grpc.Server {
	return s.grpcServer
}

// SetReady sets the plugin's readiness state (returned by Probe).
func (s *Server) SetReady(ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = ready
}

// OnRegistered sets a callback invoked when the host notifies registration.
func (s *Server) OnRegistered(fn func(hostVersion string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onRegistered = fn
}

// Serve starts the gRPC server on the configured Unix socket.
// It blocks until ctx is cancelled or a termination signal is received.
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	s.serving = true
	s.stopRequested = false
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.serving = false
		s.mu.Unlock()
	}()

	// Remove stale socket.
	if err := os.Remove(s.cfg.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("pluginsdk: remove stale socket: %w", err)
	}

	var listenConfig net.ListenConfig
	lis, err := listenConfig.Listen(ctx, "unix", s.cfg.SocketPath)
	if err != nil {
		return fmt.Errorf("pluginsdk: listen on %s: %w", s.cfg.SocketPath, err)
	}

	// Handle graceful shutdown.
	shutdownCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		select {
		case <-shutdownCtx.Done():
		case <-sigCh:
			cancel()
		}
		s.logger.Info("shutting down plugin server", zap.String("name", s.cfg.Name))
		s.stop()
	}()

	s.logger.Info("plugin server starting",
		zap.String("name", s.cfg.Name),
		zap.String("socket", s.cfg.SocketPath),
	)

	if err := s.grpcServer.Serve(lis); err != nil {
		s.mu.RLock()
		stopped := s.stopRequested
		s.mu.RUnlock()
		if !errors.Is(err, grpc.ErrServerStopped) || !stopped {
			return fmt.Errorf("pluginsdk: serve: %w", err)
		}
	}
	return nil
}

// Stop gracefully stops the server.
func (s *Server) Stop() {
	s.stop()
}

func (s *Server) stop() {
	s.mu.Lock()
	if s.serving {
		s.stopRequested = true
	}
	s.mu.Unlock()
	s.grpcServer.GracefulStop()
}

func (s *Server) GetPluginInfo(context.Context, *pluginv1alpha1.GetPluginInfoRequest) (*pluginv1alpha1.GetPluginInfoResponse, error) {
	return &pluginv1alpha1.GetPluginInfoResponse{
		Name:        s.cfg.Name,
		Version:     s.cfg.Version,
		Description: s.cfg.Description,
		Vendor:      s.cfg.Vendor,
	}, nil
}

func (s *Server) GetPluginCapabilities(context.Context, *pluginv1alpha1.GetPluginCapabilitiesRequest) (*pluginv1alpha1.GetPluginCapabilitiesResponse, error) {
	capabilities := make([]*pluginv1alpha1.PluginCapability, 0, len(s.cfg.Capabilities))
	for _, capability := range s.cfg.Capabilities {
		capabilities = append(capabilities, &pluginv1alpha1.PluginCapability{Type: pluginv1alpha1.PluginCapability_Type(capability)})
	}
	return &pluginv1alpha1.GetPluginCapabilitiesResponse{Capabilities: capabilities}, nil
}

func (s *Server) Probe(context.Context, *pluginv1alpha1.PluginProbeRequest) (*pluginv1alpha1.PluginProbeResponse, error) {
	s.mu.RLock()
	ready := s.ready
	s.mu.RUnlock()
	return &pluginv1alpha1.PluginProbeResponse{Ready: ready}, nil
}

func (s *Server) NotifyRegistered(_ context.Context, request *pluginv1alpha1.NotifyRegisteredRequest) (*pluginv1alpha1.NotifyRegisteredResponse, error) {
	s.logger.Info("registered with host", zap.String("host_version", request.GetHostVersion()))

	s.mu.RLock()
	fn := s.onRegistered
	s.mu.RUnlock()
	if fn != nil {
		fn(request.GetHostVersion())
	}

	return &pluginv1alpha1.NotifyRegisteredResponse{}, nil
}
