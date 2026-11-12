// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProxyOnlyListensOnLiteralLoopback(test *testing.T) {
	for _, address := range []string{"0.0.0.0:8096", "localhost:8096", "[::]:8096", "remote:8096", "invalid"} {
		test.Setenv("KOGNITION_WORKLOAD_PROXY_LISTEN_ADDR", address)
		if _, err := proxyListenAddress(); err == nil {
			test.Fatalf("unsafe listener accepted: %s", address)
		}
	}
	for _, address := range []string{"127.0.0.1:8096", "[::1]:8096", ""} {
		test.Setenv("KOGNITION_WORKLOAD_PROXY_LISTEN_ADDR", address)
		if _, err := proxyListenAddress(); err != nil {
			test.Fatal(err)
		}
	}
}

func TestProxyReadinessTracksIdentityAvailability(test *testing.T) {
	ready := false
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/readyz" || request.Method != http.MethodGet {
			test.Error("wrong readiness request")
		}
		if !ready {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	address := strings.TrimPrefix(server.URL, "http://")
	if err := checkProxyReadiness(address, 5*time.Second); err == nil {
		test.Fatal("unavailable identity is ready")
	}
	ready = true
	if err := checkProxyReadiness(address, 5*time.Second); err != nil {
		test.Fatal(err)
	}
	ready = false
	if err := checkProxyReadiness(address, 5*time.Second); err == nil {
		test.Fatal("cached readiness survived identity loss")
	}
}

func TestProxyListenerFailureReleasesEarlierBindings(test *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		test.Fatal(err)
	}
	address := reservation.Addr().String()
	_ = reservation.Close()
	if err := serveProxyServers(context.Background(), []*http.Server{{Addr: address}, {Addr: "invalid"}}); err == nil {
		test.Fatal("invalid listener accepted")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		test.Fatal("earlier listener leaked:", err)
	}
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := serveProxyServers(ctx, []*http.Server{{Addr: "127.0.0.1:0"}}); err != nil {
		test.Fatal(err)
	}
}

func TestStatusListenerRequiresCompletePinnedConfiguration(test *testing.T) {
	test.Setenv("KOGNITION_WORKLOAD_STATUS_LISTEN_ADDR", "")
	test.Setenv("KOGNITION_WORKLOAD_STATUS_CONTROLLER_ID", "")
	address, peer, err := statusListenConfiguration(os.Getenv, "kognition.dev", "spiffe://kognition.dev/workload/worker")
	if err != nil || address != "" || peer != "" {
		test.Fatal("legacy proxy unexpectedly enabled a status listener")
	}
	for _, scenario := range []struct{ address, peer string }{
		{"0.0.0.0:8097", ""},
		{"", "spiffe://kognition.dev/controller/identity"},
		{"localhost:8097", "spiffe://kognition.dev/controller/identity"},
		{"0.0.0.0:0", "spiffe://kognition.dev/controller/identity"},
		{"0.0.0.0:8097", "spiffe://untrusted.dev/controller/identity"},
		{"0.0.0.0:8097", "spiffe://kognition.dev/workload/worker"},
	} {
		test.Setenv("KOGNITION_WORKLOAD_STATUS_LISTEN_ADDR", scenario.address)
		test.Setenv("KOGNITION_WORKLOAD_STATUS_CONTROLLER_ID", scenario.peer)
		if _, _, err := statusListenConfiguration(os.Getenv, "kognition.dev", "spiffe://kognition.dev/workload/worker"); err == nil {
			test.Fatalf("incomplete or unsafe status listener accepted: %+v", scenario)
		}
	}
	test.Setenv("KOGNITION_WORKLOAD_STATUS_LISTEN_ADDR", "0.0.0.0:8097")
	test.Setenv("KOGNITION_WORKLOAD_STATUS_CONTROLLER_ID", "spiffe://kognition.dev/controller/identity")
	if _, _, err := statusListenConfiguration(os.Getenv, "kognition.dev", "spiffe://kognition.dev/workload/worker"); err != nil {
		test.Fatal(err)
	}
}
