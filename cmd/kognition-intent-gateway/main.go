// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command kognition-intent-gateway serves HTTP goal submissions in Kubernetes mode.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kognition-ai/kognition/internal/auth"
	k8sctrl "github.com/kognition-ai/kognition/internal/k8s/controller"
	k8sgateway "github.com/kognition-ai/kognition/internal/k8s/gateway"
)

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = logger.Sync() }()

	if err := run(logger); err != nil {
		logger.Error("intent gateway exited", zap.Error(err))
		os.Exit(1)
	}
}

func run(logger *zap.Logger) error {
	config, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	k8sClient, err := client.New(config, client.Options{Scheme: k8sctrl.SetupScheme()})
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	store := k8sctrl.NewStoreAdapter(k8sClient)
	serverConfig := k8sgateway.Config{
		Addr:         envOrDefault("LISTEN_ADDR", ":8080"),
		GRPCAddr:     os.Getenv("KOGNITION_GRPC_ADDR"),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	if os.Getenv("KOGNITION_AUTH_ENABLED") == "true" {
		authenticator, groups, err := auth.NewEnvironmentAuthenticator(context.Background())
		if err != nil {
			return fmt.Errorf("configure authentication: %w", err)
		}
		serverConfig.Authenticator = authenticator
		serverConfig.AuthRequiredGroups = groups
	}
	server := k8sgateway.New(serverConfig, store, logger.Named("intent-gateway"))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- server.Start() }()

	select {
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, context.Canceled) {
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
