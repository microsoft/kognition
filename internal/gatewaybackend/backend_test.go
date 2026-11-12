// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gatewaybackend

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

func TestOpenRejectsInvalidConfiguration(t *testing.T) {
	for _, test := range []struct {
		name   string
		config Config
		want   string
	}{
		{name: "unknown mode", config: Config{Mode: "standalon"}, want: "unsupported KOGNITION_MODE"},
		{name: "standalone requires database", config: Config{Mode: "standalone"}, want: "DATABASE_URL is required"},
		{name: "default remains Kubernetes", config: Config{}, want: "namespaces are required"},
		{name: "Kubernetes requires namespace", config: Config{Mode: "kubernetes"}, want: "namespaces are required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, err := Open(context.Background(), test.config, zaptest.NewLogger(t))
			if backend != nil || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Open() = %v, %v; want %q", backend, err, test.want)
			}
		})
	}
}

func TestStandaloneDoesNotLoadKubernetesConfiguration(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing-kubeconfig"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	backend, err := Open(ctx, Config{Mode: "standalone", DatabaseURL: "postgres://127.0.0.1:1/kognition?sslmode=disable&connect_timeout=1"}, zaptest.NewLogger(t))
	if backend != nil || err == nil || !strings.Contains(err.Error(), "connect standalone database") {
		t.Fatalf("Open() = %v, %v; want database connection error without Kubernetes dependency", backend, err)
	}
}

func TestStandaloneDatabaseErrorsDoNotExposeCredentials(t *testing.T) {
	const secret = "synthetic-database-secret"
	for _, databaseURL := range []string{
		"user=user password='" + secret,
		"postgres://user:" + secret + "@127.0.0.1:invalid/database",
		"postgres://user:" + secret + "@127.0.0.1:1/database?sslmode=disable&connect_timeout=1",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		backend, err := Open(ctx, Config{Mode: "standalone", DatabaseURL: databaseURL}, zaptest.NewLogger(t))
		cancel()
		if backend != nil {
			backend.Close()
			t.Fatal("unexpected successful database connection")
		}
		if err == nil {
			t.Fatal("expected a database configuration or connection error")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatal("database startup error exposed the connection credential")
		}
	}
}
