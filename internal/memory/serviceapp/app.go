// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package serviceapp composes the memory service from its runtime configuration.
package serviceapp

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	k8saudit "github.com/kognition-ai/kognition/internal/k8s/audit"
	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/memory/embed"
	"github.com/kognition-ai/kognition/internal/memory/inmemory"
	"github.com/kognition-ai/kognition/internal/memory/mem0"
	"github.com/kognition-ai/kognition/internal/memory/pgvector"
	"github.com/kognition-ai/kognition/internal/memory/service"
	"github.com/kognition-ai/kognition/internal/observability"
)

// Config contains the process-level settings used to compose the memory service.
type Config struct {
	Addr           string
	BackendType    string
	DatabaseURL    string
	Mem0BaseURL    string
	Mem0APIKey     string
	Mem0OrgID      string
	Mem0ProjectID  string
	InternalToken  string
	AuditNamespace string
	Embedding      embed.Config
}

type auditStoreFactory func(string) (observability.AuditStore, error)

// ConfigFromEnv reads the memory service's runtime environment.
func ConfigFromEnv() Config {
	return Config{
		Addr:           envOrDefault("MEMORY_SERVICE_ADDR", ":8090"),
		BackendType:    envOrDefault("MEMORY_BACKEND_TYPE", "in-memory"),
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		Mem0BaseURL:    envOrDefault("MEM0_API_URL", "https://api.mem0.ai"),
		Mem0APIKey:     os.Getenv("MEM0_API_KEY"),
		Mem0OrgID:      os.Getenv("MEM0_ORG_ID"),
		Mem0ProjectID:  os.Getenv("MEM0_PROJECT_ID"),
		InternalToken:  os.Getenv("MEMORY_SERVICE_INTERNAL_TOKEN"),
		AuditNamespace: os.Getenv("KOGNITION_AUDIT_NAMESPACE"),
		Embedding: embed.Config{
			Provider:   os.Getenv("MEMORY_EMBEDDING_PROVIDER"),
			Endpoint:   os.Getenv("MEMORY_EMBEDDING_ENDPOINT"),
			Model:      os.Getenv("MEMORY_EMBEDDING_MODEL"),
			APIKey:     os.Getenv("MEMORY_EMBEDDING_API_KEY"),
			APIVersion: os.Getenv("MEMORY_EMBEDDING_API_VERSION"),
		},
	}
}

// NewHandler constructs the configured backend and HTTP handler. The returned
// cleanup function releases resources owned by the selected backend.
func NewHandler(ctx context.Context, cfg Config, logger *zap.Logger) (http.Handler, func(), error) {
	return newHandler(ctx, cfg, logger, func(namespace string) (observability.AuditStore, error) {
		return k8saudit.NewInClusterConfigMapStore(namespace)
	})
}

func newHandler(ctx context.Context, cfg Config, logger *zap.Logger, newAuditStore auditStoreFactory) (http.Handler, func(), error) {
	backend, cleanup, err := selectBackend(ctx, cfg, logger)
	if err != nil {
		return nil, cleanup, err
	}

	var handlerOpts []service.Option
	if cfg.AuditNamespace != "" {
		auditStore, err := newAuditStore(cfg.AuditNamespace)
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("init Kubernetes audit store: %w", err)
		}
		handlerOpts = append(handlerOpts, service.WithAuditLog(observability.NewCognitiveAuditLog(auditStore)))
	}
	if cfg.InternalToken != "" {
		handlerOpts = append(handlerOpts, service.WithInternalToken(cfg.InternalToken))
	}
	if cfg.Embedding.Requested() {
		embedder, err := embed.New(cfg.Embedding)
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("init embedder: %w", err)
		}
		handlerOpts = append(handlerOpts, service.WithEmbedder(embedder))
		logger.Info("server-side embedding enabled",
			zap.String("provider", cfg.Embedding.Provider), zap.String("model", cfg.Embedding.Model))
	} else {
		logger.Info("server-side embedding disabled; text search uses BM25 fallback")
	}

	handler := service.NewHandler(backend, logger, handlerOpts...)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return mux, cleanup, nil
}

func selectBackend(ctx context.Context, cfg Config, logger *zap.Logger) (memory.Backend, func(), error) {
	noop := func() {}
	switch cfg.BackendType {
	case "in-memory":
		logger.Info("using in-memory backend (ephemeral, zero-config)")
		return inmemory.New(), noop, nil
	case "mem0":
		mem0Config := mem0.Config{
			BaseURL:   cfg.Mem0BaseURL,
			APIKey:    cfg.Mem0APIKey,
			OrgID:     cfg.Mem0OrgID,
			ProjectID: cfg.Mem0ProjectID,
		}
		logger.Info("using mem0 backend", zap.String("url", mem0Config.BaseURL))
		return mem0.New(mem0Config, logger), noop, nil
	case "pgvector":
		if cfg.DatabaseURL == "" {
			return nil, noop, fmt.Errorf("pgvector backend requires DATABASE_URL")
		}
		pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
		if err != nil {
			return nil, noop, fmt.Errorf("connect pgvector pool: %w", err)
		}
		logger.Info("using pgvector backend")
		return pgvector.New(pool, logger), pool.Close, nil
	default:
		return nil, noop, fmt.Errorf("unsupported backend type %q", cfg.BackendType)
	}
}

func envOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
