// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/memory/serviceapp"
)

func TestRunMemoryServiceMain(t *testing.T) {
	t.Run("logger failure", func(t *testing.T) {
		var stderr bytes.Buffer
		code := runMemoryServiceMain(context.Background(), func() (*zap.Logger, error) {
			return nil, errors.New("logger failed")
		}, nil, nil, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "logger failed") {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
	})

	t.Run("service failure", func(t *testing.T) {
		code := runMemoryServiceMain(context.Background(), func() (*zap.Logger, error) {
			return zap.NewNop(), nil
		}, func() serviceapp.Config {
			return serviceapp.Config{BackendType: "test"}
		}, func(context.Context, serviceapp.Config, *zap.Logger) error {
			return errors.New("service failed")
		}, &bytes.Buffer{})
		if code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
	})

	t.Run("success", func(t *testing.T) {
		code := runMemoryServiceMain(context.Background(), func() (*zap.Logger, error) {
			return zap.NewNop(), nil
		}, func() serviceapp.Config {
			return serviceapp.Config{BackendType: "test"}
		}, func(context.Context, serviceapp.Config, *zap.Logger) error { return nil }, &bytes.Buffer{})
		if code != 0 {
			t.Fatalf("code = %d, want 0", code)
		}
	})
}

func TestRunMemoryServiceCommand(t *testing.T) {
	wantErr := errors.New("serve failed")
	loaded := false
	err := runMemoryServiceCommand(context.Background(), zap.NewNop(), func() serviceapp.Config {
		loaded = true
		return serviceapp.Config{BackendType: "test"}
	}, func(_ context.Context, cfg serviceapp.Config, _ *zap.Logger) error {
		if cfg.BackendType != "test" {
			t.Fatalf("backend = %q", cfg.BackendType)
		}
		return wantErr
	})
	if !loaded || !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "backend test") {
		t.Fatalf("runMemoryServiceCommand() = %v, loaded=%v", err, loaded)
	}

	err = runMemoryServiceCommand(context.Background(), zap.NewNop(), func() serviceapp.Config {
		return serviceapp.Config{BackendType: "test"}
	}, func(context.Context, serviceapp.Config, *zap.Logger) error { return nil })
	if err != nil {
		t.Fatalf("successful command = %v", err)
	}
}

func TestRunMemoryServiceRejectsUnsupportedBackend(t *testing.T) {
	err := runMemoryService(context.Background(), serviceapp.Config{
		Addr:        "127.0.0.1:0",
		BackendType: "unknown",
	}, zap.NewNop())
	if err == nil || !strings.Contains(err.Error(), `unsupported backend type "unknown"`) {
		t.Fatalf("error = %v, want unsupported backend", err)
	}
}

func TestRunMemoryServicePropagatesListenFailure(t *testing.T) {
	err := runMemoryService(context.Background(), serviceapp.Config{
		Addr:        "127.0.0.1:-1",
		BackendType: "in-memory",
	}, zap.NewNop())
	if err == nil {
		t.Fatal("expected listen failure")
	}
}

func TestRunMemoryServiceShutsDownWhenContextIsCanceled(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- runMemoryServiceWithListen(ctx, serviceapp.Config{
			Addr:        addr,
			BackendType: "in-memory",
		}, zap.NewNop(), func(network, address string) (net.Listener, error) {
			if network != "tcp" || address != addr {
				t.Errorf("listen = (%q, %q), want (tcp, %q)", network, address, addr)
			}
			return listener, nil
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		response, requestErr := http.Get("http://" + addr + "/v1/memory/health")
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				cancel()
				t.Fatalf("health status = %d, want 200", response.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("memory service did not become ready: %v", requestErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runMemoryService() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("memory service did not shut down")
	}
}
