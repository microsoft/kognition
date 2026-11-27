// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/controlplaneauth"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/gateway"
	"github.com/kognition-ai/kognition/internal/modelgateway"
	cognamespace "github.com/kognition-ai/kognition/internal/namespace"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
	intentgatewayv1alpha1 "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

// Config holds API server configuration.
type Config struct {
	// Addr is the listen address (e.g., ":8080").
	Addr        string
	MetricsAddr string

	// GRPCAddr is the optional gRPC listen address. An empty value disables gRPC.
	GRPCAddr string

	// ReadTimeout is the maximum duration for reading requests.
	ReadTimeout time.Duration

	// WriteTimeout is the maximum duration for writing responses.
	WriteTimeout time.Duration

	// TLSCertFile and TLSKeyFile, when both set, enable HTTPS.
	TLSCertFile string
	TLSKeyFile  string
}

// Option applies optional configuration to the Server.
type Option func(*Server)

// ModelAccessAuthorizer decides whether an agent may invoke a model.
type ModelAccessAuthorizer interface {
	EvaluateModelAccess(context.Context, string, string, string) error
}

// ChatQuotaEnforcer reserves and settles token usage for chat sessions.
type ChatQuotaEnforcer interface {
	CheckSchedulerQuota(context.Context, string) error
	CheckModelQuota(context.Context, string, string) error
	CheckCallBudget(context.Context, string, string, int64, int64) error
	ReserveTokens(context.Context, string, string, int64, int64) error
	Settle(context.Context, string, int64, int64) error
}

// ChatToolInvoker executes a model-requested tool for an Agent principal.
type ChatToolInvoker interface {
	InvokeTool(context.Context, string, string, string, map[string]any) (map[string]any, error)
}

type runtimeApprovalAuthorizer interface {
	AuthorizeRuntimeApproval(context.Context, *auth.Identity, string, string, string) error
}

// WithAdmission sets the admission chain for the server.
func WithAdmission(chain *admission.Chain) Option {
	return func(s *Server) {
		s.admission.AppendChain(chain)
	}
}

func withAdmissionReplacement(chain *admission.Chain) Option {
	return func(s *Server) {
		s.admission = chain
	}
}

// WithApprovalBroker sets the approval broker used by approval endpoints.
func WithApprovalBroker(broker *runtime.ApprovalBroker) Option {
	return func(s *Server) {
		s.approvalBroker = broker
	}
}

// WithCPIRegistry sets the CPI driver registry used by the chat endpoint.
func WithCPIRegistry(registry *cpi.Registry) Option {
	return func(s *Server) {
		s.cpiRegistry = registry
	}
}

// WithModelAccessAuthorizer enforces ModelGrant policy before chat dispatch.
func WithModelAccessAuthorizer(authorizer ModelAccessAuthorizer) Option {
	return func(s *Server) {
		s.modelAccessAuthorizer = authorizer
	}
}

// WithModelGatewayCore routes direct standalone chat authorization and Model
// resolution through the shared Model Gateway core.
func WithModelGatewayCore(core *modelgateway.Core) Option {
	return func(s *Server) {
		s.modelGatewayCore = core
	}
}

// WithChatQuotaEnforcer enables workspace quota accounting for chat sessions.
func WithChatQuotaEnforcer(enforcer ChatQuotaEnforcer) Option {
	return func(s *Server) {
		s.chatQuotaEnforcer = enforcer
	}
}

// WithChatToolInvoker enables secured tool execution from chat completions.
func WithChatToolInvoker(invoker ChatToolInvoker) Option {
	return func(s *Server) {
		s.chatToolInvoker = invoker
	}
}

// WithChatTracing emits Kognition semantic spans for direct Agent chat requests.
func WithChatTracing(tracer *observability.Tracer, collector *observability.Collector) Option {
	return func(s *Server) {
		s.chatTracer = tracer
		s.chatCollector = collector
	}
}

// WithMetricsHandler serves Prometheus metrics on the separate metrics listener.
func WithMetricsHandler(handler http.Handler) Option {
	return func(s *Server) {
		s.metricsHandler = handler
	}
}

// WithAuditLog records API resource mutations in an append-only CognitiveAuditLog.
func WithAuditLog(auditLog *observability.CognitiveAuditLog) Option {
	return func(s *Server) {
		s.auditLog = auditLog
	}
}

// WithAuthenticator enables request authentication. When set, every request
// except the health endpoints must present a valid bearer token; the resulting
// identity is stored in the request context. requiredGroups, when non-empty,
// additionally requires the caller to belong to one of the named groups.
func WithAuthenticator(a auth.Authenticator, requiredGroups []string) Option {
	return func(s *Server) {
		s.authenticator = a
		s.authRequiredGroups = requiredGroups
	}
}

// WithControlPlaneAdminGroups separately authorizes standalone policy management.
func WithControlPlaneAdminGroups(groups []string) Option {
	return func(s *Server) {
		s.controlPlaneAdminGroups = append([]string(nil), groups...)
	}
}

func WithWorkflowExecutorPrincipals(principals []api.ControlPlaneRoleSubject) Option {
	return func(s *Server) {
		s.workflowExecutorPrincipals = append([]api.ControlPlaneRoleSubject(nil), principals...)
	}
}

func WithWorkflowExecutorClock(clock func(context.Context) (time.Time, error)) Option {
	return func(s *Server) { s.workflowExecutorClock = clock }
}

func WithWorkflowExecutorInitiation(service *runtimeapproval.WorkflowExecutorInitiationService, target func(context.Context, runtimeapproval.WorkflowExecutorAction) error) Option {
	return func(s *Server) { s.workflowExecutorInitiation, s.workflowExecutorInitiationTarget = service, target }
}

func WithAgentServiceToken(token string) Option {
	return func(s *Server) {
		s.agentServiceToken = token
	}
}

// Server is the kognition API server.
type Server struct {
	config                Config
	logger                *zap.Logger
	store                 APIServerStore
	admission             *admission.Chain
	approvalBroker        *runtime.ApprovalBroker
	cpiRegistry           *cpi.Registry
	modelAccessAuthorizer ModelAccessAuthorizer
	modelGatewayCore      *modelgateway.Core
	chatQuotaEnforcer     ChatQuotaEnforcer
	chatToolInvoker       ChatToolInvoker
	chatTracer            *observability.Tracer
	chatCollector         *observability.Collector
	auditLog              *observability.CognitiveAuditLog
	costStore             observability.CostStore
	metricsHandler        http.Handler
	metricsServer         *http.Server
	rateLimiter           *RateLimiter
	mux                   *http.ServeMux
	handler               http.Handler
	srv                   *http.Server
	grpcMu                sync.Mutex
	grpcServer            *grpc.Server
	grpcListener          net.Listener

	authenticator                    auth.Authenticator
	authRequiredGroups               []string
	controlPlaneAdminGroups          []string
	workflowExecutorPrincipals       []api.ControlPlaneRoleSubject
	workflowExecutorClock            func(context.Context) (time.Time, error)
	workflowExecutorInitiation       *runtimeapproval.WorkflowExecutorInitiationService
	workflowExecutorInitiationTarget func(context.Context, runtimeapproval.WorkflowExecutorAction) error
	agentServiceToken                string
	runtimeApprovals                 store.RuntimeApprovalExecutionStore
	approvalAuthorizer               runtimeApprovalAuthorizer
}

type APIServerStore interface {
	store.Store
	store.UpdateResultStore
}

// New creates a new API server with the given configuration.
func New(cfg Config, st APIServerStore, logger *zap.Logger, opts ...Option) *Server {
	mux := http.NewServeMux()
	mutatingHooks, validatingHooks := admission.ProductionHooks(st, opa.NewEngine())
	validatingHooks = append(validatingHooks, cognamespace.NewProductionCrossNamespaceHook(st))

	s := &Server{
		config:      cfg,
		logger:      logger,
		store:       st,
		admission:   admission.NewChain(mutatingHooks, validatingHooks),
		rateLimiter: NewRateLimiter(10, 20), // 10 req/s per gateway, burst of 20
		mux:         mux,
	}
	if auditStore, ok := st.(observability.AuditStore); ok {
		s.auditLog = observability.NewCognitiveAuditLog(auditStore)
	}
	if costStore, ok := st.(observability.CostStore); ok {
		s.costStore = costStore
	}
	if runtimeApprovals, ok := st.(store.RuntimeApprovalExecutionStore); ok {
		s.runtimeApprovals = runtimeApprovals
	}
	s.approvalAuthorizer = controlplaneauth.New(st)

	for _, opt := range opts {
		opt(s)
	}

	s.registerRoutes()

	var handler http.Handler = mux
	if s.authenticator != nil {
		handler = auth.Middleware(s.authenticator, auth.MiddlewareConfig{
			SkipPaths:      map[string]bool{"/healthz": true, "/readyz": true},
			RequiredGroups: s.authRequiredGroups,
		})(handler)
	}
	if s.agentServiceToken != "" {
		handler = s.agentServiceAuthentication(handler)
	}
	s.handler = requestid.Middleware(handler)
	s.srv = &http.Server{
		Addr:         cfg.Addr,
		Handler:      s.handler,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}
	if s.metricsHandler != nil {
		addr := cfg.MetricsAddr
		if addr == "" {
			addr = "127.0.0.1:9090"
		}
		metricsMux := http.NewServeMux()
		metricsMux.Handle("GET /metrics", s.metricsHandler)
		s.metricsServer = &http.Server{Addr: addr, Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	}

	return s
}

// registerRoutes sets up API routes.
func (s *Server) registerRoutes() {
	// Health endpoints
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)

	s.mux.HandleFunc("POST /internal/workflow-execution/namespaces/{namespace}/bindings", s.handleWorkflowBindingCreate)
	s.mux.HandleFunc("GET /internal/workflow-execution/namespaces/{namespace}/bindings/{name}", s.handleWorkflowBindingGet)
	s.mux.HandleFunc("PUT /internal/workflow-execution/namespaces/{namespace}/bindings/{name}", s.handleWorkflowBindingUpdate)
	s.mux.HandleFunc("DELETE /internal/workflow-execution/namespaces/{namespace}/bindings/{name}", s.handleWorkflowBindingRevoke)
	s.mux.HandleFunc("POST /internal/workflow-execution/namespaces/{namespace}/runs", s.handleAcceptedWorkflowRun)
	s.mux.HandleFunc("POST /internal/workflow-execution/namespaces/{namespace}/initiation-capabilities", s.handleWorkflowExecutorCapability)
	s.mux.HandleFunc("POST /internal/workflow-execution/namespaces/{namespace}/initiations/resolve", s.handleWorkflowExecutorInitiationResolve)
	s.mux.HandleFunc("POST /internal/workflow-execution/namespaces/{namespace}/initiations/dispatch", s.handleWorkflowExecutorInitiationDispatch)

	// Resource CRUD — pattern: /apis/{group}/{version}/namespaces/{namespace}/{resource}
	s.mux.HandleFunc("GET /apis/kognition.dev/v1alpha1/namespaces/{namespace}/{resource}", s.handleList)
	s.mux.HandleFunc("GET /apis/kognition.dev/v1alpha1/namespaces/{namespace}/{resource}/{name}", s.handleGet)
	s.mux.HandleFunc("POST /apis/kognition.dev/v1alpha1/namespaces/{namespace}/{resource}", s.handleCreate)
	s.mux.HandleFunc("PUT /apis/kognition.dev/v1alpha1/namespaces/{namespace}/{resource}/{name}", s.handleUpdate)
	s.mux.HandleFunc("DELETE /apis/kognition.dev/v1alpha1/namespaces/{namespace}/{resource}/{name}", s.handleDelete)

	// Watch endpoint (long-poll / SSE)
	s.mux.HandleFunc("GET /apis/kognition.dev/v1alpha1/watch/namespaces/{namespace}/{resource}", s.handleWatch)

	// Revision history endpoints
	s.mux.HandleFunc("GET /apis/kognition.dev/v1alpha1/namespaces/{namespace}/{resource}/{name}/revisions", s.handleListRevisions)
	s.mux.HandleFunc("GET /apis/kognition.dev/v1alpha1/namespaces/{namespace}/{resource}/{name}/revisions/diff", s.handleDiffRevisions)
	s.mux.HandleFunc("GET /apis/kognition.dev/v1alpha1/namespaces/{namespace}/{resource}/{name}/revisions/{revision}", s.handleGetRevision)

	// Instance event log endpoint
	s.mux.HandleFunc("GET /apis/kognition.dev/v1alpha1/namespaces/{namespace}/{resource}/{name}/events", s.handleListEvents)
	// Cost aggregation endpoint
	s.mux.HandleFunc("GET /apis/kognition.dev/v1alpha1/namespaces/{namespace}/costrecords", s.handleQueryCosts)
	s.mux.HandleFunc("GET /apis/kognition.dev/v1alpha1/namespaces/{namespace}/trust-decisions", s.handleQueryTrustDecisions)

	// MemoryRecord RTBF endpoint
	s.mux.HandleFunc("POST /apis/kognition.dev/v1alpha1/namespaces/{namespace}/memoryrecords/{name}/rtbf", s.handleMemoryRecordRTBF)

	// Approval decision endpoint
	s.mux.HandleFunc("POST /apis/kognition.dev/v1alpha1/approvals/{approvalID}/decision", s.handleSubmitApprovalDecision)
	s.mux.HandleFunc("POST /apis/kognition.dev/v1alpha1/namespaces/{namespace}/runtimeapprovals/{name}/decision", s.handleRuntimeApprovalDecision)
	s.mux.HandleFunc("POST /apis/kognition.dev/v1alpha1/namespaces/{namespace}/runtimeapprovals/{name}/reconcile", s.handleRuntimeApprovalReconciliation)

	for _, resource := range []string{"controlplaneroles", "controlplanerolebindings"} {
		s.mux.HandleFunc("GET /apis/kognition.dev/v1alpha1/namespaces/{namespace}/"+resource, s.handleControlPlanePolicyList)
		s.mux.HandleFunc("GET /apis/kognition.dev/v1alpha1/namespaces/{namespace}/"+resource+"/{name}", s.handleControlPlanePolicyGet)
		s.mux.HandleFunc("POST /apis/kognition.dev/v1alpha1/namespaces/{namespace}/"+resource, s.handleControlPlanePolicyCreate)
		s.mux.HandleFunc("PUT /apis/kognition.dev/v1alpha1/namespaces/{namespace}/"+resource+"/{name}", s.handleControlPlanePolicyUpdate)
		s.mux.HandleFunc("DELETE /apis/kognition.dev/v1alpha1/namespaces/{namespace}/"+resource+"/{name}", s.handleControlPlanePolicyDelete)
	}

	// WorkflowRun approval subresource
	s.mux.HandleFunc("POST /apis/kognition.dev/v1alpha1/namespaces/{namespace}/workflowruns/{name}/approval", s.handleWorkflowRunApproval)
	// AutonomyReview decision subresource
	s.mux.HandleFunc("POST /apis/kognition.dev/v1alpha1/namespaces/{namespace}/autonomyreviews/{name}/decision", s.handleAutonomyReviewDecision)

	// IntentGateway goal submission endpoint (with rate limiting)
	s.mux.HandleFunc("POST /apis/kognition.dev/v1alpha1/namespaces/{namespace}/intentgateways/{name}/goals", s.handleSubmitGoal)

	// Agent chat endpoint
	s.mux.HandleFunc("POST /apis/kognition.dev/v1alpha1/namespaces/{namespace}/agents/{name}/chat", s.handleChat)
	// AgentService invocation endpoint
	s.mux.HandleFunc("POST /apis/kognition.dev/v1alpha1/namespaces/{namespace}/agentservices/{name}/invoke", s.handleAgentServiceInvoke)
}

// Start begins serving requests.
func (s *Server) Start() error {
	if err := s.startGRPC(); err != nil {
		return err
	}
	if s.metricsServer != nil {
		var listenConfig net.ListenConfig
		listener, err := listenConfig.Listen(context.Background(), "tcp", s.metricsServer.Addr)
		if err != nil {
			return fmt.Errorf("listen metrics server: %w", err)
		}
		defer func() { _ = s.metricsServer.Close() }()
		go func() {
			if err := s.metricsServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.logger.Error("metrics server failed", zap.Error(err))
				_ = s.srv.Close()
			}
		}()
	}
	if s.config.TLSCertFile != "" && s.config.TLSKeyFile != "" {
		s.logger.Info("starting API server (TLS)", zap.String("addr", s.config.Addr))
		return s.srv.ListenAndServeTLS(s.config.TLSCertFile, s.config.TLSKeyFile)
	}
	s.logger.Info("starting API server", zap.String("addr", s.config.Addr))
	return s.srv.ListenAndServe()
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("shutting down API server")
	s.rateLimiter.Stop()
	var metricsErr error
	if s.metricsServer != nil {
		metricsErr = s.metricsServer.Shutdown(ctx)
	}
	return errors.Join(s.shutdownGRPC(ctx), s.srv.Shutdown(ctx), metricsErr)
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
	s.logger.Info("starting API server gRPC", zap.String("addr", s.config.GRPCAddr))
	go func() {
		if err := grpcServer.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			s.logger.Error("API server gRPC stopped with error", zap.Error(err))
		}
	}()
	return nil
}

func (s *Server) newGRPCServer() *grpc.Server {
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(auth.GRPCUnaryInterceptor(s.authenticator, s.authRequiredGroups)))
	intentgatewayv1alpha1.RegisterIntentGatewayServer(grpcServer, gateway.NewGRPCServer(gateway.NewService(s.store, s, s.rateLimiter)))
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

// ServeHTTP implements http.Handler, delegating to the internal mux.
// This enables httptest-based integration testing without starting a listener.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// handleHealthz responds with server health.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "ok")
}

// handleReadyz responds with readiness status.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		s.logger.Warn("readiness check failed", zap.Error(err), requestid.ZapField(r.Context()))
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "ok")
}
