// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/providers/pgvector"
)

func TestPoolPostgresRequiresRealVerifiedTLS(t *testing.T) {
	portText, caFile := os.Getenv("KOGNITION_PGVECTOR_TLS_TEST_PORT"), os.Getenv("KOGNITION_PGVECTOR_TLS_TEST_CA")
	if portText == "" || caFile == "" {
		t.Skip("explicit isolated TLS database fixture required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		t.Fatal("invalid isolated TLS fixture port")
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal("cannot read isolated fixture trust root")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatal("invalid isolated fixture root")
	}
	spec := pgvector.BackendSpec{
		Host: "localhost", Port: int32(port), Database: "postgres", Table: "knowledge_chunks", EmbeddingDimensions: 3,
		CredentialsSecretRef: pgvector.CredentialsReference{Name: "database", UsernameKey: "username", PasswordKey: "password"},
		TLS:                  pgvector.TLSConfig{Mode: "verify-full"},
	}
	material := &pgvector.ConnectionMaterial{
		Username: "postgres", Password: "local-database-qualification-only",
		TLS: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "localhost"},
	}
	for _, scenario := range []string{"verified", "wrong hostname", "untrusted root"} {
		t.Run(scenario, func(t *testing.T) {
			config := *material
			config.TLS = material.TLS.Clone()
			if scenario == "wrong hostname" {
				config.TLS.ServerName = "wrong.invalid"
			}
			if scenario == "untrusted root" {
				config.TLS.RootCAs = x509.NewCertPool()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pool, err := pgvector.NewPool(ctx, spec, &config, pgvector.PoolBounds{MinConns: 1, MaxConns: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			var encrypted bool
			err = pool.QueryRow(ctx, "SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()").Scan(&encrypted)
			if scenario == "verified" && (err != nil || !encrypted) {
				t.Fatalf("verified TLS was not negotiated: encrypted=%v err=%v", encrypted, err)
			}
			if scenario != "verified" && err == nil {
				t.Fatal("unverified PostgreSQL TLS connection was accepted")
			}
		})
	}
}
