// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command kognition-agent-communication-gateway serves capability-bound AgentService delegation.
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

	"github.com/kognition-ai/kognition/internal/agentgateway"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/gatewaybackend"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = logger.Sync() }()
	if err := run(logger); err != nil {
		logger.Error("Agent Communication Gateway exited", zap.Error(err))
		os.Exit(1)
	}
}

func run(logger *zap.Logger) error {
	keys, err := runtimeauth.NewConfiguredCapabilityKeyRing(
		os.Getenv("AGENT_DELEGATION_CAPABILITY_HMAC_KEY_ID"),
		[]byte(os.Getenv("AGENT_DELEGATION_CAPABILITY_HMAC_KEY")),
		os.Getenv("AGENT_DELEGATION_CAPABILITY_HMAC_PREVIOUS_KEY_ID"),
		[]byte(os.Getenv("AGENT_DELEGATION_CAPABILITY_HMAC_PREVIOUS_KEY")),
	)
	if err != nil {
		return fmt.Errorf("configure agent delegation capability key ring: %w", err)
	}
	sessionIssuer, err := runtimeauth.NewAgentDelegationSessionCapabilityIssuerWithKeyRing(keys, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("configure agent delegation session issuer: %w", err)
	}
	exactIssuer, err := runtimeauth.NewAgentDelegationCapabilityIssuerWithKeyRing(keys, time.Minute)
	if err != nil {
		return fmt.Errorf("configure agent delegation issuer: %w", err)
	}
	controlNamespace := os.Getenv("KOGNITION_AGENT_COMMUNICATION_GATEWAY_NAMESPACE")
	runtimeToken := os.Getenv("KOGNITION_RUNTIME_AUTH_TOKEN")
	if runtimeToken == "" {
		return fmt.Errorf("KOGNITION_RUNTIME_AUTH_TOKEN is required")
	}
	maximumClockSkew, err := runtimeApprovalClockSkewFromEnv()
	if err != nil {
		return err
	}
	backendContext, cancelBackend := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBackend()
	backend, err := gatewaybackend.Open(backendContext, gatewaybackend.Config{
		Mode: os.Getenv("KOGNITION_MODE"), DatabaseURL: os.Getenv("DATABASE_URL"),
		ControlNamespace: controlNamespace, AuditNamespace: controlNamespace,
		Hostname: os.Getenv("HOSTNAME"), Component: "agent-communication-gateway",
		WorkloadUID: os.Getenv("KOGNITION_RUNTIME_APPROVAL_WORKLOAD_UID"), MaximumClockSkew: maximumClockSkew,
	}, logger)
	if err != nil {
		return fmt.Errorf("configure Agent Communication Gateway backend: %w", err)
	}
	defer backend.Close()
	st := backend.Store
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
	meteredAuditStore := observability.NewTrustDecisionMetrics(ctrlmetrics.Registry).Wrap(backend.Audit)
	options := []agentgateway.Option{
		agentgateway.WithAuditLog(observability.NewCognitiveAuditLog(meteredAuditStore)),
		agentgateway.WithGate3Coordinator(gate3Coordinator),
		agentgateway.WithGate3RequestBuilder(agentgateway.NewGate3RequestBuilder(st)),
		agentgateway.WithRuntimeApprovalExecutionStore(st),
		agentgateway.WithClaimant(backend.Claimant),
	}
	if os.Getenv("KOGNITION_MODE") == "standalone" {
		endpoint, token := os.Getenv("KOGNITION_API_SERVER_URL"), os.Getenv("KOGNITION_API_SERVER_TOKEN")
		if endpoint == "" || token == "" {
			return fmt.Errorf("KOGNITION_API_SERVER_URL and KOGNITION_API_SERVER_TOKEN are required in standalone mode")
		}
		options = append(options, agentgateway.WithStandaloneAPIServer(endpoint, token))
	}
	server, err := agentgateway.NewServer(
		st,
		sessionIssuer,
		exactIssuer,
		controller.NewCommunicationPolicyEngine(st, logger.Named("communication-policy")),
		runtimeToken,
		os.Getenv("KOGNITION_AGENT_NAMESPACE"),
		nil,
		logger,
		options...,
	)
	if err != nil {
		return fmt.Errorf("create Agent Communication Gateway: %w", err)
	}
	agentgateway.InitMetrics()
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(ctrlmetrics.Registry, promhttp.HandlerOpts{}))
	mux.Handle("/", server)
	httpServer := &http.Server{
		Addr:              envOrDefault("LISTEN_ADDR", ":8094"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("Agent Communication Gateway starting", zap.String("addr", httpServer.Addr))
		errCh <- runtimeauth.ServeGatewayHTTP(ctx, httpServer)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown Agent Communication Gateway: %w", err)
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
