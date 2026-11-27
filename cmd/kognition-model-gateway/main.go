// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command kognition-model-gateway authorizes runtime model requests before
// proxying OpenAI-compatible chat completions to the configured provider.
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
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/gatewaybackend"
	"github.com/kognition-ai/kognition/internal/modelgateway"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = logger.Sync() }()
	if err := run(logger); err != nil {
		logger.Error("Model Gateway exited", zap.Error(err))
		os.Exit(1)
	}
}

func run(logger *zap.Logger) error {
	keys, err := runtimeauth.NewConfiguredCapabilityKeyRing(os.Getenv("MODEL_CAPABILITY_HMAC_KEY_ID"), []byte(os.Getenv("MODEL_CAPABILITY_HMAC_KEY")), os.Getenv("MODEL_CAPABILITY_HMAC_PREVIOUS_KEY_ID"), []byte(os.Getenv("MODEL_CAPABILITY_HMAC_PREVIOUS_KEY")))
	if err != nil {
		return fmt.Errorf("configure model capability key ring: %w", err)
	}
	issuer, err := runtimeauth.NewModelSessionCapabilityIssuerWithKeyRing(keys, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("configure model capability verification: %w", err)
	}
	costNamespace := os.Getenv("KOGNITION_COST_NAMESPACE")
	backendContext, cancelBackend := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBackend()
	backend, err := gatewaybackend.Open(backendContext, gatewaybackend.Config{
		Mode: os.Getenv("KOGNITION_MODE"), DatabaseURL: os.Getenv("DATABASE_URL"),
		ControlNamespace: costNamespace, AuditNamespace: costNamespace,
	}, logger)
	if err != nil {
		return fmt.Errorf("configure Model Gateway backend: %w", err)
	}
	defer backend.Close()
	st, costStore := backend.Store, backend.Cost
	callHealth, err := modelCallHealthStore(backend, costNamespace, logger)
	if err != nil {
		return err
	}
	var callRecorder cpi.CallRecorder = observability.NewCPICallMetrics(ctrlmetrics.Registry)
	var healthReporter *modelhealth.Reporter
	if callHealth != nil {
		tracker := modelhealth.NewTracker()
		callRecorder = cpi.Recorders(callRecorder, tracker)
		healthReporter = modelhealth.NewReporter(tracker, callHealth, modelhealth.NewReporterID(), logger)
	}
	usageWriter := observability.NewModelUsageMetrics(ctrlmetrics.Registry).Wrap(costStore)
	meteredAuditStore := observability.NewTrustDecisionMetrics(ctrlmetrics.Registry).Wrap(backend.Audit)
	policies := controller.NewInvocationPolicyEngine(
		controller.NewToolGrantEngine(st, logger.Named("model-grants")),
		controller.NewAgentPolicyReconciler(st, opa.NewEngine(), logger.Named("agent-policies")),
	)
	quota := controller.NewQuotaEnforcer(st, controller.NewCognitiveQuotaReconciler(st, logger.Named("quota")), logger.Named("quota"))
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
	tracePipeline.Tracer.SetFallibleCostSink(func(record observability.CostRecord) error {
		writeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var err error
		if record.OperationType == observability.CostOpModelInference {
			_, err = usageWriter.ApplyUsageEvent(writeCtx, record)
		} else {
			err = costStore.RecordCost(writeCtx, record)
		}
		if err != nil {
			logger.Warn("persist cost record", zap.Error(err), zap.String("workspace", record.Workspace), zap.String("trace_id", record.TraceID), zap.String("span_id", record.SpanID))
		}
		return err
	})
	options := []modelgateway.Option{modelgateway.WithObservability(tracePipeline.Tracer, tracePipeline.Collector), modelgateway.WithQuotaChecker(quota), modelgateway.WithAuditLog(observability.NewCognitiveAuditLog(meteredAuditStore)), modelgateway.WithCallRecorder(callRecorder)}
	// The runtime request context carries the Agent-specific deadline. Do not
	// impose a shorter process-wide proxy timeout that would truncate valid
	// long-running Model calls configured on the Agent.
	server, err := modelgateway.New(st, issuer, policies, &http.Client{}, logger, options...)
	if err != nil {
		return fmt.Errorf("create model gateway: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(ctrlmetrics.Registry, promhttp.HandlerOpts{}))
	mux.Handle("/", server)
	httpServer := &http.Server{Addr: envOrDefault("LISTEN_ADDR", ":8093"), Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return serveModelGateway(ctx, httpServer, healthReporter, runtimeauth.ServeGatewayHTTP, logger)
}

func serveModelGateway(ctx context.Context, httpServer *http.Server, healthReporter *modelhealth.Reporter, serve func(context.Context, *http.Server) error, logger *zap.Logger) error {
	if healthReporter != nil {
		// Keep publishing through request draining, then flush before closing the backend.
		defer healthReporter.RunInBackground(context.WithoutCancel(ctx))()
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("Model Gateway starting", zap.String("addr", httpServer.Addr))
		errCh <- serve(ctx, httpServer)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown Model Gateway: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// modelCallHealthStore returns where this gateway shares passive model health:
// ConfigMaps in Kubernetes mode, or the PostgreSQL store in standalone mode. It
// returns nil when the backend cannot share health between processes.
func modelCallHealthStore(backend *gatewaybackend.Backend, namespace string, logger *zap.Logger) (store.ModelCallHealthStore, error) {
	return backend.ModelCallHealthStore(namespace, "model-gateway", logger)
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
