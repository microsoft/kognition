// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command kognition-memory-service runs the built-in memory service.
// It exposes the memory backend over HTTP/JSON for use by agent runtimes.
// By default it uses the in-memory backend (zero-config, no external deps).
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/memory/serviceapp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := runMemoryServiceMain(ctx, newProductionLogger, serviceapp.ConfigFromEnv, runMemoryService, os.Stderr)
	stop()
	os.Exit(code)
}

func newProductionLogger() (*zap.Logger, error) {
	return zap.NewProduction()
}

func runMemoryServiceMain(
	ctx context.Context,
	newLogger func() (*zap.Logger, error),
	loadConfig func() serviceapp.Config,
	serve func(context.Context, serviceapp.Config, *zap.Logger) error,
	stderr io.Writer,
) int {
	logger, err := newLogger()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "failed to create logger: %v\n", err)
		return 1
	}
	defer func() { _ = logger.Sync() }()
	if err := runMemoryServiceCommand(ctx, logger, loadConfig, serve); err != nil {
		logger.Error("memory service error", zap.Error(err))
		return 1
	}
	return 0
}

func runMemoryServiceCommand(
	ctx context.Context,
	logger *zap.Logger,
	loadConfig func() serviceapp.Config,
	serve func(context.Context, serviceapp.Config, *zap.Logger) error,
) error {
	cfg := loadConfig()
	if err := serve(ctx, cfg, logger); err != nil {
		return fmt.Errorf("backend %s: %w", cfg.BackendType, err)
	}
	return nil
}

func runMemoryService(ctx context.Context, cfg serviceapp.Config, logger *zap.Logger) error {
	return runMemoryServiceWithListen(ctx, cfg, logger, net.Listen)
}

func runMemoryServiceWithListen(
	ctx context.Context,
	cfg serviceapp.Config,
	logger *zap.Logger,
	listen func(network, address string) (net.Listener, error),
) error {
	handler, cleanup, err := serviceapp.NewHandler(ctx, cfg, logger)
	if err != nil {
		return fmt.Errorf("init memory service: %w", err)
	}
	defer cleanup()

	srv := &http.Server{
		Addr:         cfg.Addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	listener, err := listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("memory service starting", zap.String("addr", cfg.Addr), zap.String("backend", cfg.BackendType))
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down memory service")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown memory service: %w", err)
	}
	return nil
}
