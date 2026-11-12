// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package gateway serves the Kubernetes IntentGateway HTTP endpoint.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/kognition-ai/kognition/internal/auth"
	coregateway "github.com/kognition-ai/kognition/internal/gateway"
	"github.com/kognition-ai/kognition/internal/store"
	intentgatewayv1alpha1 "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

// Config configures the Kubernetes IntentGateway HTTP server.
type Config struct {
	Addr               string
	GRPCAddr           string
	ReadTimeout        time.Duration
	WriteTimeout       time.Duration
	Authenticator      auth.Authenticator
	AuthRequiredGroups []string
}

// Option configures a Server.
type Option func(*Server)

// WithAPIKeyResolver supplies a deployment-specific API key resolver.
func WithAPIKeyResolver(resolver coregateway.APIKeyResolver) Option {
	return func(server *Server) {
		server.credentials = resolver
	}
}

// WithRateLimiter supplies the shared goal-submission rate limiter.
func WithRateLimiter(rateLimiter *coregateway.RateLimiter) Option {
	return func(server *Server) {
		server.rateLimiter = rateLimiter
	}
}

// Server exposes only IntentGateway goal submission and health endpoints.
type Server struct {
	config       Config
	store        store.Store
	logger       *zap.Logger
	credentials  coregateway.APIKeyResolver
	rateLimiter  *coregateway.RateLimiter
	service      *coregateway.Service
	mux          *http.ServeMux
	httpServer   *http.Server
	grpcMu       sync.Mutex
	grpcServer   *grpc.Server
	grpcListener net.Listener
}

// New creates a Kubernetes IntentGateway HTTP server.
func New(config Config, st store.Store, logger *zap.Logger, options ...Option) *Server {
	server := &Server{
		config:      config,
		store:       st,
		logger:      logger,
		credentials: coregateway.NewStoreAPIKeyResolver(st, nil),
		rateLimiter: coregateway.NewRateLimiter(10, 20),
		mux:         http.NewServeMux(),
	}
	for _, option := range options {
		option(server)
	}
	server.service = coregateway.NewService(st, server.credentials, server.rateLimiter)
	server.registerRoutes()
	var handler http.Handler = server.mux
	if server.config.Authenticator != nil {
		handler = auth.Middleware(server.config.Authenticator, auth.MiddlewareConfig{
			SkipPaths:      map[string]bool{"/healthz": true, "/readyz": true},
			RequiredGroups: server.config.AuthRequiredGroups,
		})(handler)
	}
	server.httpServer = &http.Server{
		Addr:         config.Addr,
		Handler:      handler,
		ReadTimeout:  config.ReadTimeout,
		WriteTimeout: config.WriteTimeout,
	}
	return server
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux.HandleFunc("POST /apis/kognition.dev/v1alpha1/namespaces/{namespace}/intentgateways/{name}/goals", s.handleSubmitGoal)
}

// Start begins serving HTTP requests.
func (s *Server) Start() error {
	if err := s.startGRPC(); err != nil {
		return err
	}
	s.logger.Info("starting Kubernetes IntentGateway server", zap.String("addr", s.config.Addr))
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.rateLimiter.Stop()
	return errors.Join(s.shutdownGRPC(ctx), s.httpServer.Shutdown(ctx))
}

func (s *Server) startGRPC() error {
	if s.config.GRPCAddr == "" {
		return nil
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", s.config.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen gRPC server: %w", err)
	}
	grpcServer := s.newGRPCServer()
	s.grpcMu.Lock()
	s.grpcListener = listener
	s.grpcServer = grpcServer
	s.grpcMu.Unlock()
	s.logger.Info("starting Kubernetes IntentGateway gRPC server", zap.String("addr", s.config.GRPCAddr))
	go func() {
		if err := grpcServer.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			s.logger.Error("Kubernetes IntentGateway gRPC stopped with error", zap.Error(err))
		}
	}()
	return nil
}

func (s *Server) newGRPCServer() *grpc.Server {
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(auth.GRPCUnaryInterceptor(s.config.Authenticator, s.config.AuthRequiredGroups)))
	intentgatewayv1alpha1.RegisterIntentGatewayServer(grpcServer, coregateway.NewGRPCServer(s.service))
	return grpcServer
}

func (s *Server) shutdownGRPC(ctx context.Context) error {
	s.grpcMu.Lock()
	grpcServer := s.grpcServer
	s.grpcServer = nil
	s.grpcListener = nil
	s.grpcMu.Unlock()
	if grpcServer == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		grpcServer.Stop()
		<-done
		return ctx.Err()
	}
}

// ServeHTTP enables httptest-based contract tests.
func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	s.httpServer.Handler.ServeHTTP(writer, request)
}

func (s *Server) handleHealthz(writer http.ResponseWriter, _ *http.Request) {
	writer.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(writer, "ok")
}

func (s *Server) handleReadyz(writer http.ResponseWriter, request *http.Request) {
	if err := s.store.Ping(request.Context()); err != nil {
		s.logger.Warn("IntentGateway readiness check failed", zap.Error(err))
		http.Error(writer, "not ready", http.StatusServiceUnavailable)
		return
	}
	writer.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(writer, "ok")
}

func (s *Server) handleSubmitGoal(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "failed to read request body")
		return
	}
	if len(body) == 0 {
		writeError(writer, http.StatusBadRequest, "request body is required")
		return
	}
	var submission coregateway.GoalSubmission
	if err := json.Unmarshal(body, &submission); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid JSON body")
		return
	}
	response, submissionErr := s.service.SubmitGoal(request.Context(), coregateway.SubmissionRequest{
		Namespace:   request.PathValue("namespace"),
		GatewayName: request.PathValue("name"),
		Submission:  submission,
		HeaderValue: request.Header.Get,
	})
	if submissionErr != nil {
		s.writeSubmissionError(writer, submissionErr)
		return
	}
	writeJSON(writer, http.StatusCreated, response)
}

func (s *Server) writeSubmissionError(writer http.ResponseWriter, submissionErr *coregateway.SubmissionError) {
	if submissionErr.Kind == coregateway.ErrorInternal && submissionErr.Err != nil {
		s.logger.Error("IntentGateway goal submission failed", zap.Error(submissionErr.Err))
	}
	switch submissionErr.Kind {
	case coregateway.ErrorInvalidRequest:
		writeError(writer, http.StatusBadRequest, submissionErr.Message)
	case coregateway.ErrorNotFound:
		writeError(writer, http.StatusNotFound, submissionErr.Message)
	case coregateway.ErrorUnavailable:
		writeError(writer, http.StatusServiceUnavailable, submissionErr.Message)
	case coregateway.ErrorUnauthorized:
		writer.Header().Set("WWW-Authenticate", "ApiKey")
		writeError(writer, http.StatusUnauthorized, submissionErr.Message)
	case coregateway.ErrorRateLimited:
		writeError(writer, http.StatusTooManyRequests, submissionErr.Message)
	case coregateway.ErrorNoRoute:
		writeError(writer, http.StatusUnprocessableEntity, submissionErr.Message)
	default:
		writeError(writer, http.StatusInternalServerError, submissionErr.Message)
	}
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Status",
		"status":     "Failure",
		"message":    message,
		"code":       status,
	})
}
