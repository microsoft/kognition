// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// pickAdminAddress reserves an ephemeral loopback port and releases it before
// returning, giving the admin server something concrete to bind to. There is a
// small window between release and re-bind, but on local test runs it is safe
// enough to exercise the real ListenAndServe path.
func pickAdminAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release loopback port: %v", err)
	}
	return address
}

// waitForAdminReady polls /healthz until it succeeds so the test does not race
// the goroutine that calls ListenAndServe. It gives up after a short deadline
// to prevent a wedged test from hanging CI.
func waitForAdminReady(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	client := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
	for {
		resp, err := client.Get("http://" + address + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("admin server did not become ready at %s", address)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStartAdminServerServesHealthReadyAndMetrics exercises the admin listener
// end-to-end: it must respond 200 on /healthz unconditionally, 503 on /readyz
// until the informer index reports synced, 200 on /readyz afterwards, and 200
// on /metrics with a Prometheus payload from the controller-runtime registry.
func TestStartAdminServerServesHealthReadyAndMetrics(t *testing.T) {
	address := pickAdminAddress(t)
	index := newKnowledgeBaseUIDIndex(zap.NewNop())
	transport := &http.Transport{}
	client := &http.Client{Timeout: 2 * time.Second, Transport: transport}

	server, errCh := startAdminServer(address, index, zap.NewNop())
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown admin server: %v", err)
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("admin server exited with unexpected error: %v", err)
		}
	})

	waitForAdminReady(t, address)
	// /readyz must be 503 while the informer has not marked synced.
	respNotReady, err := client.Get("http://" + address + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz before sync: %v", err)
	}
	respNotReady.Body.Close()
	if respNotReady.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before sync status = %d, want 503", respNotReady.StatusCode)
	}

	index.MarkSynced()

	respReady, err := client.Get("http://" + address + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz after sync: %v", err)
	}
	respReady.Body.Close()
	if respReady.StatusCode != http.StatusOK {
		t.Fatalf("/readyz after sync status = %d, want 200", respReady.StatusCode)
	}

	// /metrics must serve the controller-runtime registry via promhttp.
	respMetrics, err := client.Get("http://" + address + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer respMetrics.Body.Close()
	if respMetrics.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", respMetrics.StatusCode)
	}
	body, err := io.ReadAll(respMetrics.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	// Prometheus exposition format always begins with '#' comment lines; the
	// promhttp handler emits at least the process/go collector defaults from
	// the controller-runtime registry.
	if !strings.Contains(string(body), "# ") {
		t.Fatalf("/metrics body does not look like Prometheus exposition: %q", string(body))
	}

	// Verify the server pinned the configured timeouts. This guards against
	// silent regressions to the ReadHeader/Read/Write timeouts.
	if server.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v, want 10s", server.ReadHeaderTimeout)
	}
	if server.ReadTimeout != 30*time.Second {
		t.Fatalf("ReadTimeout = %v, want 30s", server.ReadTimeout)
	}
	if server.WriteTimeout != 30*time.Second {
		t.Fatalf("WriteTimeout = %v, want 30s", server.WriteTimeout)
	}
	if server.Addr != address {
		t.Fatalf("server.Addr = %q, want %q", server.Addr, address)
	}
}

// TestStartAdminServerReportsBindFailure ensures the goroutine surfaces bind
// errors (not just http.ErrServerClosed) via the returned channel, so run()
// can distinguish a real failure from a graceful shutdown.
func TestStartAdminServerReportsBindFailure(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	defer blocker.Close()

	index := newKnowledgeBaseUIDIndex(zap.NewNop())
	server, errCh := startAdminServer(blocker.Addr().String(), index, zap.NewNop())
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	select {
	case err := <-errCh:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("expected bind failure, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("admin server did not report bind failure in time")
	}
}
