// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/apiserver"
	"github.com/kognition-ai/kognition/internal/cpi"
)

type lifecycleServerStub struct {
	startErr    error
	shutdownErr error
	started     chan struct{}
	stop        chan struct{}
	stopOnce    sync.Once
	shutdowns   int
}

func newLifecycleServerStub() *lifecycleServerStub {
	return &lifecycleServerStub{started: make(chan struct{}), stop: make(chan struct{})}
}

func (s *lifecycleServerStub) Start() error {
	close(s.started)
	if s.startErr != nil {
		return s.startErr
	}
	<-s.stop
	return http.ErrServerClosed
}

func (s *lifecycleServerStub) Shutdown(context.Context) error {
	s.shutdowns++
	s.stopOnce.Do(func() { close(s.stop) })
	return s.shutdownErr
}

type listenerLifecycleStub struct {
	startErr error
	stopErr  error
	starts   int
	stops    int
}

func TestAPIServiceProductionDefaults(t *testing.T) {
	logger := newAPIServerLogger()
	if logger == nil {
		t.Fatal("newAPIServerLogger() returned nil")
	}
	_ = logger.Sync()

	t.Setenv("DATABASE_URL", "postgres://configured")
	if got := databaseURL(); got != "postgres://configured" {
		t.Fatalf("configured database URL = %q", got)
	}
	t.Setenv("DATABASE_URL", "")
	if got := databaseURL(); !strings.Contains(got, "localhost:5432/kognition") {
		t.Fatalf("default database URL = %q", got)
	}

	if got := len(newProductionCPIRegistry().List()); got != 5 {
		t.Fatalf("production drivers = %d, want 5", got)
	}

	pool, err := newDatabasePool(context.Background(), "postgres://user:pass@localhost/database")
	if err != nil {
		t.Fatalf("newDatabasePool: %v", err)
	}
	defer pool.Close()
	store := newPostgresStore(pool, zap.NewNop())
	if store == nil {
		t.Fatal("newPostgresStore() returned nil")
	}
	ctx, cancel := newSignalContext()
	if ctx == nil || cancel == nil {
		t.Fatal("newSignalContext() returned nil")
	}
	cancel()
	server := newAPIServer(serverConfigFromEnv(), store, zap.NewNop())
	if server == nil {
		t.Fatal("newAPIServer() returned nil")
	}
}

func (s *listenerLifecycleStub) StartNotifyListener(context.Context) error {
	s.starts++
	return s.startErr
}

func (s *listenerLifecycleStub) StopNotifyListener(context.Context) error {
	s.stops++
	return s.stopErr
}

func TestServerConfigFromEnv(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "127.0.0.1:9090")
	t.Setenv("KOGNITION_GRPC_ADDR", "127.0.0.1:9091")
	t.Setenv("KOGNITION_TLS_CERT_FILE", "cert.pem")
	t.Setenv("KOGNITION_TLS_KEY_FILE", "key.pem")
	config := serverConfigFromEnv()
	if config.Addr != "127.0.0.1:9090" || config.GRPCAddr != "127.0.0.1:9091" || config.ReadTimeout != 30*time.Second || config.WriteTimeout != 60*time.Second || config.TLSCertFile != "cert.pem" || config.TLSKeyFile != "key.pem" {
		t.Fatalf("server config = %#v", config)
	}
}

func TestNewCPIRegistryIncludesProductionDrivers(t *testing.T) {
	want := map[string]bool{"anthropic": true, "aws-bedrock": true, "foundry": true, "ollama": true, "openai-compatible": true}
	got := newCPIRegistry(http.DefaultClient).List()
	if len(got) != len(want) {
		t.Fatalf("drivers = %v", got)
	}
	for _, name := range got {
		if !want[name] {
			t.Fatalf("unexpected driver %q", name)
		}
	}
}

func TestBuildServerOptions(t *testing.T) {
	t.Setenv("KOGNITION_AGENT_SERVICE_TOKEN", "")
	registry := newCPIRegistry(http.DefaultClient)
	t.Setenv("KOGNITION_AUTH_ENABLED", "")
	options, enabled, err := buildServerOptions(context.Background(), registry)
	if err != nil || enabled || len(options) != 1 {
		t.Fatalf("disabled auth = (%d options, enabled %v, error %v)", len(options), enabled, err)
	}

	t.Setenv("KOGNITION_AUTH_ENABLED", "true")
	t.Setenv("KOGNITION_BOOTSTRAP_TOKEN", "token")
	t.Setenv("KOGNITION_OIDC_ISSUER", "")
	t.Setenv("KOGNITION_OIDC_REQUIRED_GROUPS", "api-users")
	t.Setenv("KOGNITION_CONTROL_PLANE_ADMIN_GROUPS", "policy-admins")
	options, enabled, err = buildServerOptions(context.Background(), registry)
	if err != nil || !enabled || len(options) != 3 {
		t.Fatalf("enabled auth = (%d options, enabled %v, error %v)", len(options), enabled, err)
	}

	t.Setenv("KOGNITION_BOOTSTRAP_TOKEN", "")
	if _, enabled, err = buildServerOptions(context.Background(), registry); err == nil || !enabled {
		t.Fatalf("invalid auth = (enabled %v, error %v)", enabled, err)
	}
}

func TestBuildServerOptionsAgentServiceCredential(t *testing.T) {
	serviceToken := strings.Repeat("s", 32)
	for _, scenario := range []struct {
		name, enabled, service, bootstrap, runtime string
		valid                                      bool
	}{
		{"authentication disabled", "", serviceToken, "operator", "runtime", false},
		{"short credential", "true", "short", "operator", "runtime", false},
		{"bootstrap reused", "true", serviceToken, serviceToken, "runtime", false},
		{"runtime reused", "true", serviceToken, "operator", serviceToken, false},
		{"dedicated credential", "true", serviceToken, "operator", "runtime", true},
		{"service credential alone", "true", serviceToken, "", "runtime", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Setenv("KOGNITION_AUTH_ENABLED", scenario.enabled)
			t.Setenv("KOGNITION_AGENT_SERVICE_TOKEN", scenario.service)
			t.Setenv("KOGNITION_BOOTSTRAP_TOKEN", scenario.bootstrap)
			t.Setenv("KOGNITION_RUNTIME_AUTH_TOKEN", scenario.runtime)
			t.Setenv("KOGNITION_OIDC_ISSUER", "")
			options, enabled, err := buildServerOptions(context.Background(), newCPIRegistry(http.DefaultClient))
			if scenario.valid {
				if err != nil || !enabled || len(options) != 4 {
					t.Fatalf("valid configuration: options=%d enabled=%v error=%v", len(options), enabled, err)
				}
			} else if err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestRunAPIServerCommand(t *testing.T) {
	t.Setenv("KOGNITION_AGENT_SERVICE_TOKEN", "")
	registry := newCPIRegistry(http.DefaultClient)

	t.Run("authentication failure", func(t *testing.T) {
		t.Setenv("KOGNITION_AUTH_ENABLED", "true")
		t.Setenv("KOGNITION_BOOTSTRAP_TOKEN", "")
		t.Setenv("KOGNITION_OIDC_ISSUER", "")
		err := runAPIServerCommand(context.Background(), zap.NewNop(), registry, func(context.Context, *zap.Logger, *cpi.Registry, []apiserver.Option) (*apiServerRuntime, error) {
			t.Fatal("runtime factory should not be called")
			return nil, nil
		})
		if err == nil || !strings.Contains(err.Error(), "configure authentication") {
			t.Fatalf("authentication error = %v", err)
		}
	})

	t.Run("runtime failure", func(t *testing.T) {
		t.Setenv("KOGNITION_AUTH_ENABLED", "")
		wantErr := errors.New("runtime failed")
		err := runAPIServerCommand(context.Background(), zap.NewNop(), registry, func(context.Context, *zap.Logger, *cpi.Registry, []apiserver.Option) (*apiServerRuntime, error) {
			return nil, wantErr
		})
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "create API server runtime") {
			t.Fatalf("runtime error = %v", err)
		}
	})

	t.Run("authenticated lifecycle", func(t *testing.T) {
		t.Setenv("KOGNITION_AUTH_ENABLED", "true")
		t.Setenv("KOGNITION_BOOTSTRAP_TOKEN", "token")
		t.Setenv("KOGNITION_OIDC_ISSUER", "")
		server := newLifecycleServerStub()
		listener := &listenerLifecycleStub{}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-server.started
			cancel()
		}()
		err := runAPIServerCommand(ctx, zap.NewNop(), registry, func(_ context.Context, _ *zap.Logger, got *cpi.Registry, options []apiserver.Option) (*apiServerRuntime, error) {
			if got != registry {
				t.Fatal("the runtime factory must receive the command's CPI registry")
			}
			if len(options) != 3 {
				t.Fatalf("options = %d, want 3", len(options))
			}
			return &apiServerRuntime{server: server, listener: listener, closePool: func() {}}, nil
		})
		if err != nil {
			t.Fatalf("runAPIServerCommand: %v", err)
		}
	})
}

func TestNewProductionRuntime(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost/database")
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_BINDING_KEYS_JSON", `[{"version":1,"epoch":1,"algorithm":"HMAC-SHA-256","domain":"kognition.dev/runtime-approval/action/v1","status":"Active","material":"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}]`)
	registry := newProductionCPIRegistry()
	if registry.Observed() {
		t.Fatal("the production registry must not be observed before the runtime is built")
	}
	runtime, err := newProductionRuntime(context.Background(), zap.NewNop(), registry, nil)
	if err != nil {
		t.Fatalf("newProductionRuntime: %v", err)
	}
	if !registry.Observed() {
		t.Fatal("newProductionRuntime must report CPI call outcomes to the runtime's metrics registry")
	}
	if runtime.server == nil || runtime.listener == nil || runtime.closePool == nil || runtime.initialize == nil {
		t.Fatalf("runtime = %#v", runtime)
	}
	if err := runtime.initialize(context.Background()); err == nil {
		runtime.closePool()
		t.Fatal("production RuntimeApproval activation unexpectedly succeeded against fake database credentials")
	}
	runtime.closePool()
}

func TestRunServerLifecycleGracefulShutdown(t *testing.T) {
	server := newLifecycleServerStub()
	listener := &listenerLifecycleStub{startErr: errors.New("notifications unavailable")}
	ctx, cancel := context.WithCancel(context.Background())
	closed := 0
	done := make(chan error, 1)
	go func() {
		done <- runServerLifecycle(ctx, server, listener, func() { closed++ }, zap.NewNop())
	}()
	<-server.started
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runServerLifecycle: %v", err)
	}
	if server.shutdowns != 1 || listener.starts != 1 || listener.stops != 1 || closed != 1 {
		t.Fatalf("shutdowns=%d listener=(%d,%d) closed=%d", server.shutdowns, listener.starts, listener.stops, closed)
	}
}

func TestRunServerLifecycleReturnsJoinedErrorsAndCleansUp(t *testing.T) {
	startErr := errors.New("bind failed")
	shutdownErr := errors.New("shutdown failed")
	stopErr := errors.New("listener failed")
	server := newLifecycleServerStub()
	server.startErr = startErr
	server.shutdownErr = shutdownErr
	listener := &listenerLifecycleStub{stopErr: stopErr}
	closed := false
	err := runServerLifecycle(context.Background(), server, listener, func() { closed = true }, zap.NewNop())
	for _, want := range []error{startErr, shutdownErr, stopErr} {
		if !errors.Is(err, want) {
			t.Fatalf("error %v does not include %v", err, want)
		}
	}
	if !closed || server.shutdowns != 1 || listener.stops != 1 {
		t.Fatalf("cleanup = closed:%v shutdowns:%d listener stops:%d", closed, server.shutdowns, listener.stops)
	}
}

func TestEnvOrDefault(t *testing.T) {
	t.Setenv("KOGNITION_TEST_SETTING", "")
	if got := envOrDefault("KOGNITION_TEST_SETTING", "fallback"); got != "fallback" {
		t.Fatalf("envOrDefault() = %q, want fallback", got)
	}
	t.Setenv("KOGNITION_TEST_SETTING", "configured")
	if got := envOrDefault("KOGNITION_TEST_SETTING", "fallback"); got != "configured" {
		t.Fatalf("envOrDefault() = %q, want configured", got)
	}
}

func TestSplitCSV(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty"},
		{name: "whitespace", in: "  "},
		{name: "trimmed and empty entries", in: " operators, , platform-admins ", want: []string{"operators", "platform-admins"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := splitCSV(test.in)
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("splitCSV(%q) = %#v, want %#v", test.in, got, test.want)
			}
		})
	}
}

func TestBuildAuthenticator_ConfiguresBootstrapIdentityAndGroups(t *testing.T) {
	t.Setenv("KOGNITION_BOOTSTRAP_TOKEN", "secret-token")
	t.Setenv("KOGNITION_BOOTSTRAP_SUBJECT", "deployment-bootstrap")
	t.Setenv("KOGNITION_OIDC_ISSUER", "")
	t.Setenv("KOGNITION_OIDC_REQUIRED_GROUPS", " operators, platform-admins ")

	authenticator, groups, err := buildAuthenticator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := authenticator.Authenticate(context.Background(), "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Subject != "deployment-bootstrap" || identity.Username != "deployment-bootstrap" || identity.Method != "token" {
		t.Fatalf("identity = %#v", identity)
	}
	if strings.Join(groups, ",") != "operators,platform-admins" {
		t.Fatalf("groups = %#v", groups)
	}
}

func TestBuildAuthenticator_DefaultsBootstrapSubject(t *testing.T) {
	t.Setenv("KOGNITION_BOOTSTRAP_TOKEN", "secret-token")
	t.Setenv("KOGNITION_BOOTSTRAP_SUBJECT", "")
	t.Setenv("KOGNITION_OIDC_ISSUER", "")

	authenticator, _, err := buildAuthenticator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := authenticator.Authenticate(context.Background(), "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Subject != "bootstrap" {
		t.Fatalf("subject = %q, want bootstrap", identity.Subject)
	}
}

func TestBuildAuthenticator_RejectsMissingAndInvalidConfiguration(t *testing.T) {
	t.Run("no providers", func(t *testing.T) {
		t.Setenv("KOGNITION_BOOTSTRAP_TOKEN", "")
		t.Setenv("KOGNITION_OIDC_ISSUER", "")
		if _, _, err := buildAuthenticator(context.Background()); err == nil || !strings.Contains(err.Error(), "neither") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("invalid OIDC issuer", func(t *testing.T) {
		t.Setenv("KOGNITION_BOOTSTRAP_TOKEN", "")
		t.Setenv("KOGNITION_OIDC_ISSUER", "://invalid")
		if _, _, err := buildAuthenticator(context.Background()); err == nil {
			t.Fatal("buildAuthenticator() succeeded with invalid issuer")
		}
	})
}
