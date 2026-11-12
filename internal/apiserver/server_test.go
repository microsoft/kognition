// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	intentgatewayv1alpha1 "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/kognition-ai/kognition/internal/auth"
)

func TestServerStartPropagatesListenErrors(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	defer listener.Close()

	server := New(Config{Addr: listener.Addr().String()}, newMemStore(), zap.NewNop())
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })

	if err := server.Start(); err == nil {
		t.Fatal("Start() error = nil, want occupied-address error")
	}
}

func TestServerStartPropagatesTLSConfigurationErrors(t *testing.T) {
	missingCert := filepath.Join(t.TempDir(), "missing-cert.pem")
	server := New(Config{
		Addr:        "127.0.0.1:0",
		TLSCertFile: missingCert,
		TLSKeyFile:  filepath.Join(t.TempDir(), "missing-key.pem"),
	}, newMemStore(), zap.NewNop())
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })

	err := server.Start()
	if err == nil || !strings.Contains(err.Error(), "missing-cert.pem") {
		t.Fatalf("Start() error = %v, want missing certificate error", err)
	}
}

func TestServerShutdownBeforeStart(t *testing.T) {
	server := New(Config{Addr: "127.0.0.1:0"}, newMemStore(), zap.NewNop())

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}
}

func TestServerPrivateMetricsListenerLifecycle(t *testing.T) {
	server := New(Config{Addr: "127.0.0.1:0", MetricsAddr: "127.0.0.1:0"}, newMemStore(), zap.NewNop(),
		WithMetricsHandler(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			_, _ = response.Write([]byte("private_metric 1\n"))
		})))
	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			if err := server.Shutdown(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(shutdown)
	address := make(chan string, 1)
	server.metricsServer.BaseContext = func(listener net.Listener) context.Context {
		address <- listener.Addr().String()
		return context.Background()
	}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Start() }()
	var metricsAddress string
	select {
	case metricsAddress = <-address:
	case err := <-serverErr:
		t.Fatalf("startup failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("metrics listener never started")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + metricsAddress + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "private_metric 1\n" {
		t.Fatalf("private scrape: status=%d body=%q err=%v", response.StatusCode, body, err)
	}
	shutdown()
	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("server exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("API server did not stop")
	}
	if response, err := client.Get("http://" + metricsAddress + "/metrics"); err == nil {
		_ = response.Body.Close()
		t.Fatal("metrics listener remained open after shutdown")
	}
}

func TestServerMetricsBindFailureStopsStartup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := New(Config{Addr: "127.0.0.1:0", MetricsAddr: listener.Addr().String()}, newMemStore(), zap.NewNop(),
		WithMetricsHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	if err := server.Start(); err == nil || !strings.Contains(err.Error(), "listen metrics server") {
		t.Fatalf("metrics bind failure must stop startup: %v", err)
	}
}

func TestServerStartAndShutdownServeGRPC(t *testing.T) {
	grpcListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve gRPC address: %v", err)
	}
	grpcAddress := grpcListener.Addr().String()
	_ = grpcListener.Close()

	server := New(Config{Addr: "127.0.0.1:0", GRPCAddr: grpcAddress}, newMemStore(), zap.NewNop())
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Start() }()

	dialCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection, err := grpc.DialContext(dialCtx, grpcAddress, grpc.WithBlock(), grpc.WithTransportCredentials(insecure.NewCredentials())) //nolint:staticcheck // Test requires blocking grpc 1.x dialing semantics.
	if err != nil {
		t.Fatalf("dial gRPC server: %v", err)
	}
	defer connection.Close()
	_, err = intentgatewayv1alpha1.NewIntentGatewayClient(connection).SubmitGoal(context.Background(), &intentgatewayv1alpha1.SubmitGoalRequest{})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("SubmitGoal status = %s, want %s; error = %v", got, codes.InvalidArgument, err)
	}

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := <-serverErr; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Start() error = %v, want %v", err, http.ErrServerClosed)
	}
}

func TestServerGRPCUsesConfiguredAuthenticator(t *testing.T) {
	server := New(Config{}, newMemStore(), zap.NewNop(), WithAuthenticator(auth.NewStaticTokenAuthenticator(map[string]string{"expected": "caller"}), nil))
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := server.newGRPCServer()
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})
	connection, err := grpc.DialContext(context.Background(), "bufnet", //nolint:staticcheck // Bufconn test relies on grpc 1.x dialing semantics.
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client := intentgatewayv1alpha1.NewIntentGatewayClient(connection)

	_, err = client.SubmitGoal(context.Background(), &intentgatewayv1alpha1.SubmitGoalRequest{})
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("missing token status = %s, want %s; error = %v", got, codes.Unauthenticated, err)
	}
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer expected"))
	_, err = client.SubmitGoal(ctx, &intentgatewayv1alpha1.SubmitGoalRequest{})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("authenticated status = %s, want %s; error = %v", got, codes.InvalidArgument, err)
	}
}
