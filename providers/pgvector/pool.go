// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PoolBounds tunes pgxpool sizing and lifetimes. Zero values mean "use the
// package defaults", which are intentionally conservative for a single-tenant
// namespace-local adapter.
type PoolBounds struct {
	MinConns          int32
	MaxConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
}

const applicationName = "kognition-pgvector"

var defaultPoolBounds = PoolBounds{
	MinConns:          1,
	MaxConns:          4,
	MaxConnLifetime:   30 * time.Minute,
	MaxConnIdleTime:   5 * time.Minute,
	HealthCheckPeriod: time.Minute,
}

// BuildPoolConfig produces a fully-specified *pgxpool.Config from the typed
// backend spec and previously-loaded connection material. It does not open any
// network connection. TLS is enforced at verify-full: the returned
// ConnConfig.TLSConfig is the exact *tls.Config from ConnectionMaterial
// (RootCAs, ServerName and MinVersion already set by LoadConnectionMaterial),
// and pgconn's own sslmode handling is bypassed by installing TLSConfig
// directly.
func BuildPoolConfig(spec BackendSpec, material *ConnectionMaterial, bounds PoolBounds) (*pgxpool.Config, error) {
	if material == nil || material.TLS == nil {
		return nil, status.Error(codes.InvalidArgument, "pgvector pool requires connection material with TLS")
	}
	if err := spec.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid pgvector pool configuration")
	}
	if material.Username == "" || material.Password == "" {
		return nil, status.Error(codes.InvalidArgument, "pgvector pool requires non-empty credentials")
	}
	// Minimal DSN — every meaningful field is set explicitly on ConnConfig
	// below so URL escaping does not affect passwords with special characters.
	config, err := pgxpool.ParseConfig("postgres://placeholder@placeholder:1/placeholder?sslmode=disable")
	if err != nil {
		return nil, status.Error(codes.Internal, "cannot construct pgvector pool config")
	}
	if config.ConnConfig == nil || config.ConnConfig.Config.RuntimeParams == nil {
		return nil, status.Error(codes.Internal, "pgvector pool config is missing connection defaults")
	}
	config.ConnConfig.Host = spec.Host
	config.ConnConfig.Port = uint16(spec.Port)
	config.ConnConfig.Database = spec.Database
	config.ConnConfig.User = material.Username
	config.ConnConfig.Password = material.Password
	// Override any TLS derived from the placeholder DSN. TLSConfig alone
	// controls the handshake once installed; ServerName, RootCAs and
	// MinVersion come from LoadConnectionMaterial.
	config.ConnConfig.TLSConfig = material.TLS.Clone()
	config.ConnConfig.RuntimeParams["application_name"] = applicationName
	// Force clean query protocol for predictable behaviour on read paths that
	// Querier uses; write paths (later) can override per-conn if needed.
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec

	resolved := resolveBounds(bounds)
	config.MinConns = resolved.MinConns
	config.MaxConns = resolved.MaxConns
	config.MaxConnLifetime = resolved.MaxConnLifetime
	config.MaxConnIdleTime = resolved.MaxConnIdleTime
	config.HealthCheckPeriod = resolved.HealthCheckPeriod
	return config, nil
}

func resolveBounds(bounds PoolBounds) PoolBounds {
	resolved := bounds
	if resolved.MinConns <= 0 {
		resolved.MinConns = defaultPoolBounds.MinConns
	}
	if resolved.MaxConns <= 0 {
		resolved.MaxConns = defaultPoolBounds.MaxConns
	}
	if resolved.MaxConns < resolved.MinConns {
		resolved.MaxConns = resolved.MinConns
	}
	if resolved.MaxConnLifetime <= 0 {
		resolved.MaxConnLifetime = defaultPoolBounds.MaxConnLifetime
	}
	if resolved.MaxConnIdleTime <= 0 {
		resolved.MaxConnIdleTime = defaultPoolBounds.MaxConnIdleTime
	}
	if resolved.HealthCheckPeriod <= 0 {
		resolved.HealthCheckPeriod = defaultPoolBounds.HealthCheckPeriod
	}
	if resolved.MaxConnIdleTime > resolved.MaxConnLifetime {
		resolved.MaxConnIdleTime = resolved.MaxConnLifetime
	}
	return resolved
}

// NewPool builds a *pgxpool.Pool bound to the typed backend spec. The context
// bounds the initial handshake and the caller retains ownership of Close.
func NewPool(ctx context.Context, spec BackendSpec, material *ConnectionMaterial, bounds PoolBounds) (*pgxpool.Pool, error) {
	config, err := BuildPoolConfig(spec, material, bounds)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, status.Error(codes.Unavailable, fmt.Sprintf("cannot open pgvector pool: %v", err))
	}
	return pool, nil
}
