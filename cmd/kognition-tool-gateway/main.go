// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command kognition-tool-gateway serves the Kubernetes runtime Tool Gateway.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/gatewaybackend"
	"github.com/kognition-ai/kognition/internal/invocation"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/toolexec"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/toolgateway"
)

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = logger.Sync() }()
	if err := run(logger); err != nil {
		logger.Error("Tool Gateway exited", zap.Error(err))
		os.Exit(1)
	}
}

func run(logger *zap.Logger) error {
	keys, err := runtimeauth.NewConfiguredCapabilityKeyRing(os.Getenv("TOOL_CAPABILITY_HMAC_KEY_ID"), []byte(os.Getenv("TOOL_CAPABILITY_HMAC_KEY")), os.Getenv("TOOL_CAPABILITY_HMAC_PREVIOUS_KEY_ID"), []byte(os.Getenv("TOOL_CAPABILITY_HMAC_PREVIOUS_KEY")))
	if err != nil {
		return fmt.Errorf("configure tool capability key ring: %w", err)
	}
	sessionIssuer, err := runtimeauth.NewToolSessionCapabilityIssuerWithKeyRing(keys, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("configure tool session capability verification: %w", err)
	}
	toolIssuer, err := runtimeauth.NewToolCapabilityIssuerWithKeyRing(keys, time.Minute)
	if err != nil {
		return fmt.Errorf("configure tool capability verification: %w", err)
	}
	controlNamespace := os.Getenv("KOGNITION_TOOL_GATEWAY_NAMESPACE")
	maximumClockSkew, err := runtimeApprovalClockSkewFromEnv()
	if err != nil {
		return err
	}
	backendContext, cancelBackend := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBackend()
	backend, err := gatewaybackend.Open(backendContext, gatewaybackend.Config{
		Mode: os.Getenv("KOGNITION_MODE"), DatabaseURL: os.Getenv("DATABASE_URL"),
		ControlNamespace: controlNamespace, AuditNamespace: controlNamespace,
		Hostname: os.Getenv("HOSTNAME"), Component: "tool-gateway",
		WorkloadUID: os.Getenv("KOGNITION_RUNTIME_APPROVAL_WORKLOAD_UID"), MaximumClockSkew: maximumClockSkew,
	}, logger)
	if err != nil {
		return fmt.Errorf("configure Tool Gateway backend: %w", err)
	}
	defer backend.Close()
	st, claimant := backend.Store, backend.Claimant
	var resilienceCoordinator toolgateway.ResilienceCoordinator
	if backend.PostgresPool != nil {
		resilienceCoordinator, err = toolgateway.NewPostgresResilienceCoordinator(backend.PostgresPool)
	} else {
		resilienceCoordinator, err = toolgateway.NewKubernetesLeaseResilienceCoordinator(backend.KubernetesClient, controlNamespace, envOrDefault("HOSTNAME", "tool-gateway"))
	}
	if err != nil {
		return fmt.Errorf("configure Tool Gateway coordination: %w", err)
	}
	bindingKeys, err := runtimeapproval.BindingKeyStoreFromJSON(os.Getenv(runtimeapproval.BindingKeysEnvironmentVariable))
	if err != nil {
		return fmt.Errorf("configure RuntimeApproval binding keys: %w", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, bindingKeys); err != nil {
		return fmt.Errorf("activate RuntimeApproval binding keys: %w", err)
	}
	gate3Coordinator, err := runtimeapproval.NewGate3Coordinator(runtimeapproval.Gate3Config{
		Store:    st,
		Resolver: runtimeapproval.NewResolver(st),
		Keys:     bindingKeys,
		TTL:      time.Minute,
	})
	if err != nil {
		return fmt.Errorf("configure Gate3 coordinator: %w", err)
	}
	policies := controller.NewInvocationPolicyEngine(
		controller.NewToolGrantEngine(st, logger.Named("tool-grants")),
		controller.NewAgentPolicyReconciler(st, opa.NewEngine(), logger.Named("agent-policies")),
	).WithAgentRBAC(st, controller.NewRBACEngine(st, logger.Named("rbac")))
	quota := controller.NewQuotaEnforcer(st, controller.NewCognitiveQuotaReconciler(st, logger.Named("quota")), logger.Named("quota"))
	executor := toolexec.New(nil)
	dispatcher := runtime.NewToolDispatcher(executor, executor)
	tracePipeline, err := observability.NewTracePipelineFromEnv(context.Background())
	if err != nil {
		return fmt.Errorf("configure OpenTelemetry tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := tracePipeline.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown tracing", zap.Error(err))
		}
	}()
	costStore := backend.Cost
	meteredAuditStore := observability.NewTrustDecisionMetrics(ctrlmetrics.Registry).Wrap(backend.Audit)
	tracePipeline.Tracer.SetFallibleCostSink(func(record observability.CostRecord) error {
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := costStore.RecordCost(writeCtx, record)
		if err != nil {
			logger.Warn("persist cost record", zap.Error(err), zap.String("workspace", record.Workspace), zap.String("trace_id", record.TraceID), zap.String("span_id", record.SpanID))
		}
		return err
	})
	tracer := invocation.NewObservabilityTracer(tracePipeline.Tracer)
	gateway, err := toolgateway.New(st, controller.NewToolGatewayAuthorizer(policies), quota, toolgateway.NewInvocationRunExecutor(st, dispatcher), logger,
		toolgateway.WithLedger(toolgateway.NewStoreLedgerWithAudit(st, observability.NewCognitiveAuditLog(meteredAuditStore))), toolgateway.WithInvocationTracer(tracer), toolgateway.WithResilienceCoordinator(resilienceCoordinator),
		toolgateway.WithGate3Coordinator(gate3Coordinator), toolgateway.WithGate3RequestBuilder(toolgateway.NewGate3RequestBuilder(st)), toolgateway.WithRuntimeApprovalExecutionStore(st), toolgateway.WithClaimant(claimant))
	if err != nil {
		return fmt.Errorf("create Tool Gateway: %w", err)
	}
	server, err := toolgateway.NewServer(st, sessionIssuer, toolIssuer, gateway, logger)
	if err != nil {
		return fmt.Errorf("create Tool Gateway HTTP server: %w", err)
	}
	toolgateway.InitMetrics()
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(ctrlmetrics.Registry, promhttp.HandlerOpts{}))
	mux.Handle("/", server)
	httpServer := &http.Server{Addr: envOrDefault("LISTEN_ADDR", ":8092"), Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("Tool Gateway starting", zap.String("addr", httpServer.Addr))
		errCh <- runtimeauth.ServeGatewayHTTP(ctx, httpServer)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown Tool Gateway: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func runtimeApprovalClockSkewFromEnv() (time.Duration, error) {
	configured := envOrDefault("KOGNITION_RUNTIME_APPROVAL_MAXIMUM_CLOCK_SKEW", "30s")
	skew, err := time.ParseDuration(configured)
	if err != nil || skew < 30*time.Second {
		return 0, fmt.Errorf("KOGNITION_RUNTIME_APPROVAL_MAXIMUM_CLOCK_SKEW must be a duration >= 30s (got %q)", configured)
	}
	return skew, nil
}
