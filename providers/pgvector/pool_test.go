// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kognition-ai/kognition/providers/pgvector"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// validMaterial builds a ConnectionMaterial with a non-nil TLS config, using
// the same shape LoadConnectionMaterial produces. Tests that need the real
// loader live in connection_material_test.go; the pool factory only depends on
// the material shape and never opens a network connection here.
func validMaterial() (pgvector.BackendSpec, *pgvector.ConnectionMaterial) {
	spec := pgvector.BackendSpec{
		Host: "postgres.tenant-a.svc.cluster.local", Port: 5432, Database: "knowledge", Table: "chunks",
		EmbeddingDimensions:  768,
		CredentialsSecretRef: pgvector.CredentialsReference{Name: "database", UsernameKey: "username", PasswordKey: "password"},
		TLS:                  pgvector.TLSConfig{Mode: "verify-full", CASecretRef: &pgvector.SecretKeyReference{Name: "database-ca", Key: "ca.crt"}},
	}
	roots := x509.NewCertPool()
	material := &pgvector.ConnectionMaterial{
		Username: "knowledge_reader",
		Password: "private-value ' \\ = @?#",
		TLS:      &tls.Config{MinVersion: tls.VersionTLS12, ServerName: spec.Host, RootCAs: roots},
	}
	return spec, material
}

func TestBuildPoolConfigPropagatesSpecAndMaterial(t *testing.T) {
	spec, material := validMaterial()
	config, err := pgvector.BuildPoolConfig(spec, material, pgvector.PoolBounds{})
	if err != nil {
		t.Fatal(err)
	}
	if config.ConnConfig.Host != spec.Host || config.ConnConfig.Port != uint16(spec.Port) || config.ConnConfig.Database != spec.Database {
		t.Fatal("network address or database not propagated to pgconn.Config")
	}
	if config.ConnConfig.User != material.Username || config.ConnConfig.Password != material.Password {
		t.Fatal("credentials not propagated to pgconn.Config")
	}
	if config.ConnConfig.TLSConfig == nil || config.ConnConfig.TLSConfig == material.TLS {
		t.Fatal("pool must install a clone of the caller's TLS config")
	}
	if config.ConnConfig.TLSConfig.ServerName != material.TLS.ServerName || config.ConnConfig.TLSConfig.RootCAs != material.TLS.RootCAs || config.ConnConfig.TLSConfig.MinVersion != material.TLS.MinVersion {
		t.Fatal("cloned TLS config lost verification settings")
	}
	if config.ConnConfig.RuntimeParams["application_name"] != "kognition-pgvector" {
		t.Fatal("application_name runtime param was not set")
	}
	if config.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeExec {
		t.Fatal("pool did not lock the default query exec mode")
	}
}

func TestBuildPoolConfigCloneIsIndependent(t *testing.T) {
	spec, material := validMaterial()
	config, err := pgvector.BuildPoolConfig(spec, material, pgvector.PoolBounds{})
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the cloned TLS config through the returned pool config must not
	// affect the caller's TLS config.
	config.ConnConfig.TLSConfig.ServerName = "attacker.example"
	config.ConnConfig.TLSConfig.MinVersion = tls.VersionTLS10
	if material.TLS.ServerName != spec.Host || material.TLS.MinVersion != tls.VersionTLS12 {
		t.Fatal("caller TLS config was mutated through the pool config")
	}
}

func TestBuildPoolConfigDefaultsAndClampsBounds(t *testing.T) {
	spec, material := validMaterial()
	for name, test := range map[string]struct {
		bounds pgvector.PoolBounds
		want   pgvector.PoolBounds
	}{
		"defaults on zero": {
			bounds: pgvector.PoolBounds{},
			want:   pgvector.PoolBounds{MinConns: 1, MaxConns: 4, MaxConnLifetime: 30 * time.Minute, MaxConnIdleTime: 5 * time.Minute, HealthCheckPeriod: time.Minute},
		},
		"max clamps to min when smaller": {
			bounds: pgvector.PoolBounds{MinConns: 3, MaxConns: 1, MaxConnLifetime: time.Hour, MaxConnIdleTime: 10 * time.Minute, HealthCheckPeriod: 30 * time.Second},
			want:   pgvector.PoolBounds{MinConns: 3, MaxConns: 3, MaxConnLifetime: time.Hour, MaxConnIdleTime: 10 * time.Minute, HealthCheckPeriod: 30 * time.Second},
		},
		"idle clamps to lifetime when larger": {
			bounds: pgvector.PoolBounds{MinConns: 1, MaxConns: 2, MaxConnLifetime: 5 * time.Minute, MaxConnIdleTime: time.Hour, HealthCheckPeriod: time.Second},
			want:   pgvector.PoolBounds{MinConns: 1, MaxConns: 2, MaxConnLifetime: 5 * time.Minute, MaxConnIdleTime: 5 * time.Minute, HealthCheckPeriod: time.Second},
		},
	} {
		t.Run(name, func(t *testing.T) {
			config, err := pgvector.BuildPoolConfig(spec, material, test.bounds)
			if err != nil {
				t.Fatal(err)
			}
			if config.MinConns != test.want.MinConns || config.MaxConns != test.want.MaxConns ||
				config.MaxConnLifetime != test.want.MaxConnLifetime || config.MaxConnIdleTime != test.want.MaxConnIdleTime ||
				config.HealthCheckPeriod != test.want.HealthCheckPeriod {
				t.Fatalf("bounds = {%d,%d,%v,%v,%v}, want {%d,%d,%v,%v,%v}",
					config.MinConns, config.MaxConns, config.MaxConnLifetime, config.MaxConnIdleTime, config.HealthCheckPeriod,
					test.want.MinConns, test.want.MaxConns, test.want.MaxConnLifetime, test.want.MaxConnIdleTime, test.want.HealthCheckPeriod)
			}
		})
	}
}

func TestBuildPoolConfigRejectsInvalidInputs(t *testing.T) {
	valid, validMat := validMaterial()
	invalidSpec := valid
	invalidSpec.Host = "" // fails Validate
	nilTLS := *validMat
	nilTLS.TLS = nil
	emptyUser := *validMat
	emptyUser.Username = ""
	emptyPassword := *validMat
	emptyPassword.Password = ""

	for name, test := range map[string]struct {
		spec     pgvector.BackendSpec
		material *pgvector.ConnectionMaterial
	}{
		"nil material":   {spec: valid, material: nil},
		"nil TLS":        {spec: valid, material: &nilTLS},
		"invalid spec":   {spec: invalidSpec, material: validMat},
		"empty username": {spec: valid, material: &emptyUser},
		"empty password": {spec: valid, material: &emptyPassword},
	} {
		t.Run(name, func(t *testing.T) {
			config, err := pgvector.BuildPoolConfig(test.spec, test.material, pgvector.PoolBounds{})
			if err == nil {
				t.Fatal("expected InvalidArgument, got nil")
			}
			if config != nil {
				t.Fatal("expected nil config on error")
			}
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
		})
	}
}

func TestNewPoolIsLazyButFailsToPingUnreachableHost(t *testing.T) {
	// pgxpool constructs lazily: NewPool must succeed even when the host is
	// unreachable so that transient DNS blips do not crash the adapter, but
	// the first Ping must surface a real error rather than silently succeed
	// against a stale cached connection.
	spec, material := validMaterial()
	spec.Host = "pgvector-unreachable.invalid" // guaranteed non-resolvable per RFC 6761
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool, err := pgvector.NewPool(ctx, spec, material, pgvector.PoolBounds{MinConns: 0, MaxConns: 1})
	if err != nil {
		t.Fatalf("NewPool must be lazy: %v", err)
	}
	if pool == nil {
		t.Fatal("expected a non-nil lazy pool")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err == nil {
		t.Fatal("Ping must fail against an unreachable host")
	}
}
