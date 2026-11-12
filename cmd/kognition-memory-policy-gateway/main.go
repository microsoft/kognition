// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command kognition-memory-policy-gateway authorizes runtime memory requests
// before proxying them to the internal memory service.
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

	"github.com/kognition-ai/kognition/internal/gatewaybackend"
	"github.com/kognition-ai/kognition/internal/memory/policygateway"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	pgstore "github.com/kognition-ai/kognition/internal/store/postgres"
)

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = logger.Sync() }()
	if err := run(logger); err != nil {
		logger.Error("memory policy gateway exited", zap.Error(err))
		os.Exit(1)
	}
}

func run(logger *zap.Logger) error {
	keys, err := runtimeauth.NewConfiguredCapabilityKeyRing(os.Getenv("MEMORY_CAPABILITY_HMAC_KEY_ID"), []byte(os.Getenv("MEMORY_CAPABILITY_HMAC_KEY")), os.Getenv("MEMORY_CAPABILITY_HMAC_PREVIOUS_KEY_ID"), []byte(os.Getenv("MEMORY_CAPABILITY_HMAC_PREVIOUS_KEY")))
	if err != nil {
		return fmt.Errorf("configure memory capability key ring: %w", err)
	}
	issuer, err := runtimeauth.NewMemoryCapabilityIssuerWithKeyRing(keys, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("configure memory capability verification: %w", err)
	}
	upstream := strings.TrimSpace(os.Getenv("KOGNITION_MEMORY_SERVICE_URL"))
	if upstream == "" {
		return errors.New("KOGNITION_MEMORY_SERVICE_URL is required")
	}
	auditNamespace := strings.TrimSpace(os.Getenv("KOGNITION_AUDIT_NAMESPACE"))
	metricsRegistry := prometheus.NewRegistry()
	controlNamespace := strings.TrimSpace(os.Getenv("KOGNITION_MEMORY_POLICY_GATEWAY_NAMESPACE"))
	maximumClockSkew, err := runtimeApprovalClockSkewFromEnv()
	if err != nil {
		return err
	}
	backendContext, cancelBackend := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBackend()
	backend, err := gatewaybackend.Open(backendContext, gatewaybackend.Config{
		Mode: os.Getenv("KOGNITION_MODE"), DatabaseURL: os.Getenv("DATABASE_URL"),
		ControlNamespace: controlNamespace, AuditNamespace: auditNamespace,
		Hostname: os.Getenv("HOSTNAME"), Component: "memory-policy-gateway",
		WorkloadUID: os.Getenv("KOGNITION_RUNTIME_APPROVAL_WORKLOAD_UID"), MaximumClockSkew: maximumClockSkew,
	}, logger)
	if err != nil {
		return fmt.Errorf("configure Memory Policy Gateway backend: %w", err)
	}
	defer backend.Close()
	st := backend.Store
	meteredAuditStore := observability.NewTrustDecisionMetrics(metricsRegistry).Wrap(backend.Audit)
	bindingKeys, err := runtimeapproval.BindingKeyStoreFromJSON(os.Getenv(runtimeapproval.BindingKeysEnvironmentVariable))
	if err != nil {
		return fmt.Errorf("configure RuntimeApproval binding keys: %w", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	databasePool, err := pgxpool.New(ctx, envOrDefault("DATABASE_URL", "postgres://kognition:kognition@localhost:5432/kognition?sslmode=disable"))
	if err != nil {
		return fmt.Errorf("configure Memory mutation receipt store: %w", err)
	}
	defer databasePool.Close()
	memoryReceipts := pgstore.New(databasePool, logger.Named("memory-receipts"))
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
	server, err := policygateway.New(
		st,
		issuer,
		upstream,
		&http.Client{Timeout: 30 * time.Second},
		logger,
		policygateway.WithUpstreamToken(os.Getenv("MEMORY_SERVICE_INTERNAL_TOKEN")),
		policygateway.WithAuditLog(observability.NewCognitiveAuditLog(meteredAuditStore)),
		policygateway.WithGate3Coordinator(gate3Coordinator),
		policygateway.WithGate3RequestBuilder(policygateway.NewGate3RequestBuilder()),
		policygateway.WithRuntimeApprovalExecutionStore(st),
		policygateway.WithClaimant(backend.Claimant),
		policygateway.WithMemoryMutationReceiptStore(memoryReceipts),
	)
	if err != nil {
		return fmt.Errorf("create memory policy gateway: %w", err)
	}
	preparationReaper := policygateway.NewMemoryPreparationReaper(
		st,
		memoryReceipts,
		logger.Named("memory-preparation-reaper"),
		policygateway.DefaultMemoryPreparationReapInterval,
		policygateway.DefaultMemoryPreparationReapLimit,
	)
	go preparationReaper.Start(ctx)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{}))
	mux.Handle("/", server)
	httpServer := &http.Server{
		Addr:              envOrDefault("LISTEN_ADDR", ":8091"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("memory policy gateway starting", zap.String("addr", httpServer.Addr))
		errCh <- runtimeauth.ServeGatewayHTTP(ctx, httpServer)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown memory policy gateway: %w", err)
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
