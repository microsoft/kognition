// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/apiserver"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/anthropic"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/bedrock"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/foundry"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/ollama"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/openai"
	"github.com/kognition-ai/kognition/internal/invocation"
	"github.com/kognition-ai/kognition/internal/modelgateway"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/toolexec"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	pgstore "github.com/kognition-ai/kognition/internal/store/postgres"
)

func main() {
	logger := newAPIServerLogger()
	ctx, cancel := newSignalContext()
	err := runAPIServerCommand(ctx, logger, newProductionCPIRegistry(), newProductionRuntime)
	cancel()
	if err != nil {
		logger.Error("API server stopped with error", zap.Error(err))
	}
	_ = logger.Sync()
	if err != nil {
		os.Exit(1)
	}
}

func newAPIServerLogger() *zap.Logger {
	logger, _ := zap.NewProduction()
	return logger
}

func databaseURL() string {
	return envOrDefault("DATABASE_URL", "postgres://kognition:kognition@localhost:5432/kognition?sslmode=disable")
}

func newProductionCPIRegistry() *cpi.Registry {
	return newCPIRegistry(&http.Client{Timeout: 60 * time.Second})
}

func newDatabasePool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	return pgxpool.New(ctx, databaseURL)
}

func newPostgresStore(pool *pgxpool.Pool, logger *zap.Logger) *pgstore.Store {
	return pgstore.New(pool, logger)
}

func newSignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func newAPIServer(config apiserver.Config, st *pgstore.Store, logger *zap.Logger, options ...apiserver.Option) *apiserver.Server {
	return apiserver.New(config, st, logger, options...)
}

type apiServerLifecycle interface {
	Start() error
	Shutdown(context.Context) error
}

type notifyListenerLifecycle interface {
	StartNotifyListener(context.Context) error
	StopNotifyListener(context.Context) error
}

type apiServerRuntime struct {
	server     apiServerLifecycle
	listener   notifyListenerLifecycle
	closePool  func()
	initialize func(context.Context) error
}

type apiServerRuntimeFactory func(context.Context, *zap.Logger, *cpi.Registry, []apiserver.Option) (*apiServerRuntime, error)

func runAPIServerCommand(
	ctx context.Context,
	logger *zap.Logger,
	registry *cpi.Registry,
	newRuntime apiServerRuntimeFactory,
) error {
	options, authEnabled, err := buildServerOptions(ctx, registry)
	if err != nil {
		return fmt.Errorf("configure authentication: %w", err)
	}
	if authEnabled {
		logger.Info("API authentication enabled")
	} else {
		logger.Warn("API authentication is DISABLED; do not expose this server to untrusted networks " +
			"(set KOGNITION_AUTH_ENABLED=true with KOGNITION_OIDC_ISSUER)")
	}
	runtime, err := newRuntime(ctx, logger, registry, options)
	if err != nil {
		return fmt.Errorf("create API server runtime: %w", err)
	}
	if runtime.initialize != nil {
		if err := runtime.initialize(ctx); err != nil {
			runtime.closePool()
			return fmt.Errorf("initialize API server runtime: %w", err)
		}
	}
	return runServerLifecycle(ctx, runtime.server, runtime.listener, runtime.closePool, logger)
}

func newProductionRuntime(ctx context.Context, logger *zap.Logger, registry *cpi.Registry, options []apiserver.Option) (*apiServerRuntime, error) {
	pool, err := newDatabasePool(ctx, databaseURL())
	if err != nil {
		return nil, err
	}
	st := newPostgresStore(pool, logger.Named("store"))
	metricsRegistry := prometheus.NewRegistry()
	callTracker := modelhealth.NewTracker()
	registry.Observe(cpi.Recorders(observability.NewCPICallMetrics(metricsRegistry), callTracker))
	usageWriter := observability.NewModelUsageMetrics(metricsRegistry).Wrap(st)
	auditStore := observability.NewTrustDecisionMetrics(metricsRegistry).Wrap(st)
	bindingKeys, err := runtimeapproval.BindingKeyStoreFromJSON(os.Getenv(runtimeapproval.BindingKeysEnvironmentVariable))
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("configure RuntimeApproval binding keys: %w", err)
	}
	policyEvaluator := controller.NewAgentPolicyReconciler(st, opa.NewEngine(), logger.Named("runtime-agent-policy"))
	invocationPolicies := controller.NewInvocationPolicyEngine(
		controller.NewToolGrantEngine(st, logger.Named("model-grant-engine")),
		policyEvaluator,
	)
	modelGatewayCore, err := modelgateway.NewCore(st, invocationPolicies)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("create standalone Model Gateway core: %w", err)
	}
	quotaReconciler := controller.NewCognitiveQuotaReconciler(st, logger.Named("chat-quota"))
	quotaEnforcer := controller.NewQuotaEnforcer(st, quotaReconciler, logger.Named("chat-quota"))
	toolExecutor := toolexec.New(nil)
	tracePipeline, err := observability.NewTracePipelineFromEnv(ctx)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("configure OpenTelemetry tracing: %w", err)
	}
	if tracePipeline.Enabled() {
		logger.Info("OpenTelemetry trace export enabled", zap.String("endpoint", tracePipeline.Config.Endpoint), zap.String("protocol", string(tracePipeline.Config.Protocol)))
	}
	tracePipeline.Tracer.SetFallibleCostSink(func(record observability.CostRecord) error {
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var err error
		if record.OperationType == observability.CostOpModelInference {
			_, err = usageWriter.ApplyUsageEvent(writeCtx, record)
		} else {
			err = st.RecordCost(writeCtx, record)
		}
		if err != nil {
			logger.Warn("persist cost record", zap.Error(err), zap.String("workspace", record.Workspace), zap.String("trace_id", record.TraceID), zap.String("span_id", record.SpanID))
		}
		return err
	})
	invocationTracer := invocation.NewObservabilityTracer(tracePipeline.Tracer)
	toolInvoker, err := controller.NewToolGatewayInvoker(
		st,
		invocationPolicies.WithAgentRBAC(st, controller.NewRBACEngine(st, logger.Named("chat-rbac"))),
		quotaEnforcer,
		runtime.NewToolDispatcher(toolExecutor, toolExecutor),
		logger.Named("chat-tool-invoker"),
		invocationTracer,
	)
	if err != nil {
		pool.Close()
		shutdownTracePipeline(tracePipeline, logger)
		return nil, fmt.Errorf("create tool gateway: %w", err)
	}
	options = append(options,
		apiserver.WithModelAccessAuthorizer(invocationPolicies),
		apiserver.WithModelGatewayCore(modelGatewayCore),
		apiserver.WithChatQuotaEnforcer(quotaEnforcer),
		apiserver.WithChatToolInvoker(toolInvoker),
		apiserver.WithMetricsHandler(promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{})),
		apiserver.WithAuditLog(observability.NewCognitiveAuditLog(auditStore)),
	)
	options = append(options, apiserver.WithChatTracing(tracePipeline.Tracer, tracePipeline.Collector))
	startReporter, stopReporter := modelHealthReporting(modelhealth.NewReporter(callTracker, st, modelhealth.NewReporterID(), logger))
	return &apiServerRuntime{
		server:   newAPIServer(serverConfigFromEnv(), st, logger.Named("apiserver"), options...),
		listener: st,
		initialize: func(ctx context.Context) error {
			if err := st.ActivateRuntimeApprovalBindingKeys(ctx, bindingKeys); err != nil {
				return err
			}
			startReporter(ctx)
			return nil
		},
		closePool: func() {
			stopReporter()
			shutdownTracePipeline(tracePipeline, logger)
			pool.Close()
		},
	}, nil
}

// modelHealthReporting ties the passive model health reporter to the runtime's
// lifecycle: start runs when the runtime is initialized and stop, which is a no-op
// if start never ran, when its resources are closed. runServerLifecycle closes
// them only after the server has drained its requests, and the reporter keeps
// publishing until then (see Reporter.RunInBackground).
func modelHealthReporting(reporter *modelhealth.Reporter) (start func(context.Context), stop func()) {
	stopReporter := func() {}
	start = func(ctx context.Context) { stopReporter = reporter.RunInBackground(ctx) }
	stop = func() { stopReporter() }
	return start, stop
}

func serverConfigFromEnv() apiserver.Config {
	return apiserver.Config{
		Addr:         envOrDefault("LISTEN_ADDR", ":8080"),
		MetricsAddr:  envOrDefault("KOGNITION_METRICS_ADDR", "127.0.0.1:9090"),
		GRPCAddr:     os.Getenv("KOGNITION_GRPC_ADDR"),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		TLSCertFile:  os.Getenv("KOGNITION_TLS_CERT_FILE"),
		TLSKeyFile:   os.Getenv("KOGNITION_TLS_KEY_FILE"),
	}
}

func newCPIRegistry(httpClient *http.Client) *cpi.Registry {
	registry := cpi.NewRegistry()
	registry.MustRegister(openai.New(httpClient))
	registry.MustRegister(foundry.New(httpClient))
	registry.MustRegister(anthropic.New(httpClient))
	registry.MustRegister(ollama.New(httpClient))
	registry.MustRegister(bedrock.New(httpClient))
	return registry
}

func buildServerOptions(ctx context.Context, registry *cpi.Registry) ([]apiserver.Option, bool, error) {
	options := []apiserver.Option{apiserver.WithCPIRegistry(registry)}
	serviceToken := os.Getenv("KOGNITION_AGENT_SERVICE_TOKEN")
	if serviceToken != "" {
		if os.Getenv("KOGNITION_AUTH_ENABLED") != "true" || len(serviceToken) < 32 || serviceToken == os.Getenv("KOGNITION_BOOTSTRAP_TOKEN") || serviceToken == os.Getenv("KOGNITION_RUNTIME_AUTH_TOKEN") {
			return nil, true, fmt.Errorf("AgentService credential requires enabled authentication, at least 32 characters, and separation from bootstrap and runtime credentials")
		}
		options = append(options, apiserver.WithAgentServiceToken(serviceToken))
	}
	if os.Getenv("KOGNITION_AUTH_ENABLED") != "true" {
		return options, false, nil
	}
	authenticator, requiredGroups, err := buildAuthenticator(ctx)
	if err != nil {
		return nil, true, err
	}
	return append(options,
		apiserver.WithAuthenticator(authenticator, requiredGroups),
		apiserver.WithControlPlaneAdminGroups(splitCSV(os.Getenv("KOGNITION_CONTROL_PLANE_ADMIN_GROUPS"))),
	), true, nil
}

func runServerLifecycle(
	ctx context.Context,
	server apiServerLifecycle,
	listener notifyListenerLifecycle,
	closePool func(),
	logger *zap.Logger,
) error {
	defer closePool()
	if err := listener.StartNotifyListener(ctx); err != nil {
		logger.Warn("failed to start pg_notify listener; watch will only see writes from this process", zap.Error(err))
	}

	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Start() }()
	var lifecycleErr error
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			lifecycleErr = fmt.Errorf("start API server: %w", err)
		}
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		lifecycleErr = errors.Join(lifecycleErr, fmt.Errorf("shutdown API server: %w", err))
	}
	listenerCtx, listenerCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer listenerCancel()
	if err := listener.StopNotifyListener(listenerCtx); err != nil {
		lifecycleErr = errors.Join(lifecycleErr, fmt.Errorf("stop notify listener: %w", err))
	}
	return lifecycleErr
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func shutdownTracePipeline(pipeline *observability.TracePipeline, logger *zap.Logger) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pipeline.Shutdown(shutdownCtx); err != nil {
		logger.Error("OpenTelemetry trace exporter shutdown error", zap.Error(err))
	}
}

// buildAuthenticator assembles the request authenticator from environment
// configuration: an optional bootstrap static token (KOGNITION_BOOTSTRAP_TOKEN)
// followed by an OIDC provider (KOGNITION_OIDC_ISSUER + KOGNITION_OIDC_AUDIENCE).
// At least one must be configured when authentication is enabled.
func buildAuthenticator(ctx context.Context) (auth.Authenticator, []string, error) {
	var chain auth.Chain

	if bt := os.Getenv("KOGNITION_BOOTSTRAP_TOKEN"); bt != "" {
		subject := envOrDefault("KOGNITION_BOOTSTRAP_SUBJECT", "bootstrap")
		chain = append(chain, auth.NewStaticTokenAuthenticator(map[string]string{bt: subject}))
	}

	if issuer := os.Getenv("KOGNITION_OIDC_ISSUER"); issuer != "" {
		oidcAuth, err := auth.NewOIDCAuthenticator(ctx, []auth.ProviderConfig{{
			IssuerURL:     issuer,
			Audience:      os.Getenv("KOGNITION_OIDC_AUDIENCE"),
			UsernameClaim: os.Getenv("KOGNITION_OIDC_USERNAME_CLAIM"),
			GroupsClaim:   os.Getenv("KOGNITION_OIDC_GROUPS_CLAIM"),
		}})
		if err != nil {
			return nil, nil, err
		}
		chain = append(chain, oidcAuth)
	}

	if len(chain) == 0 {
		return nil, nil, fmt.Errorf("KOGNITION_AUTH_ENABLED=true but neither " +
			"KOGNITION_OIDC_ISSUER nor KOGNITION_BOOTSTRAP_TOKEN is set")
	}
	return chain, splitCSV(os.Getenv("KOGNITION_OIDC_REQUIRED_GROUPS")), nil
}

// splitCSV parses a comma-separated list into a trimmed, non-empty slice.
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
