// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"

	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/cpi"
	k8sctrl "github.com/kognition-ai/kognition/internal/k8s/controller"
	"github.com/kognition-ai/kognition/internal/k8s/dispatcher"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/plugin"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestRequiresAutomaticIdentityForKnowledgeGatewayOnly(t *testing.T) {
	agentDispatcher := dispatcher.NewK8sDispatcher(nil, nil, nil, dispatcher.Config{
		KnowledgeGatewayURL:       "http://knowledge-gateway:8095",
		KnowledgeGatewayNamespace: "kognition-system",
	})
	if !requiresAutomaticIdentity(agentDispatcher) {
		t.Fatal("knowledge-only gateway configuration must require a controller-managed identity")
	}
}

func TestRunControllerCommand(t *testing.T) {
	t.Run("parse failure", func(t *testing.T) {
		var stderr bytes.Buffer
		code := runControllerCommand([]string{"--unknown"}, &stderr, func() (*zap.Logger, error) {
			t.Fatal("logger should not be created")
			return nil, nil
		}, nil)
		if code != 2 || stderr.Len() == 0 {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
	})

	t.Run("logger failure", func(t *testing.T) {
		var stderr bytes.Buffer
		code := runControllerCommand(nil, &stderr, func() (*zap.Logger, error) {
			return nil, errors.New("logger failed")
		}, nil)
		if code != 1 || !strings.Contains(stderr.String(), "logger failed") {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
	})

	t.Run("controller failure", func(t *testing.T) {
		code := runControllerCommand(nil, &bytes.Buffer{}, func() (*zap.Logger, error) {
			return zap.NewNop(), nil
		}, func(controllerOptions, *zap.Logger) error {
			return errors.New("controller failed")
		})
		if code != 1 {
			t.Fatalf("code = %d, want 1", code)
		}
	})

	t.Run("success", func(t *testing.T) {
		code := runControllerCommand([]string{"--disable-webhooks"}, &bytes.Buffer{}, func() (*zap.Logger, error) {
			return zap.NewNop(), nil
		}, func(options controllerOptions, _ *zap.Logger) error {
			if !options.disableWebhooks {
				t.Fatal("disable-webhooks option was not propagated")
			}
			return nil
		})
		if code != 0 {
			t.Fatalf("code = %d, want 0", code)
		}
	})
}

func TestResourceWebhookStartupBoundary(t *testing.T) {
	t.Setenv("KOGNITION_CONTROLLER_NAMESPACE", "control")
	sentinel := errors.New("manager boundary")
	err := runResourceWebhooksWithManager(controllerOptions{leaderElect: true}, zap.NewNop(), func(options k8sctrl.ManagerOptions) (ctrl.Manager, error) {
		if options.LeaderElection || options.DisableWebhooks {
			t.Fatalf("resource admission must run without controller leader election: %+v", options)
		}
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("manager failure not preserved: %v", err)
	}
	err = runResourceWebhooksWithManager(controllerOptions{disableWebhooks: true}, zap.NewNop(), func(k8sctrl.ManagerOptions) (ctrl.Manager, error) {
		t.Fatal("disabled admission must fail before manager creation")
		return nil, nil
	})
	if err == nil {
		t.Fatal("resource-webhooks accepted disabled admission")
	}
}

func TestProductionConfigurationFromEnvironment(t *testing.T) {
	logger := zap.NewNop()
	options := controllerOptions{
		metricsAddr: ":9090", probeAddr: ":9091", leaderElect: false,
		leaderElectID: "custom", leaderElectNS: "system", disableWebhooks: true,
	}
	manager := managerOptions(options, logger)
	if manager.MetricsBindAddress != ":9090" || manager.HealthProbeBindAddress != ":9091" || manager.LeaderElection || manager.LeaderElectionID != "custom" || manager.LeaderElectionNamespace != "system" || !manager.DisableWebhooks || manager.Logger != logger {
		t.Fatalf("manager options = %#v", manager)
	}

	t.Setenv("KOGNITION_AGENT_IMAGE", "agent:test")
	t.Setenv("KOGNITION_RUNTIME_AUTOGEN_IMAGE", "autogen:test")
	t.Setenv("KOGNITION_RUNTIME_MCP_IMAGE", "mcp:test")
	t.Setenv("KOGNITION_AGENT_NAMESPACE", "agents")
	t.Setenv("KOGNITION_MEMORY_SERVICE_URL", "http://memory")
	t.Setenv("KOGNITION_MEMORY_POLICY_GATEWAY_URL", "http://memory-policy")
	t.Setenv("KOGNITION_MEMORY_POLICY_GATEWAY_PORT", "18091")
	t.Setenv("KOGNITION_MEMORY_CAPABILITY_HMAC_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("KOGNITION_RUNTIME_AUTH_TOKEN", "controller-token")
	t.Setenv("KOGNITION_RUNTIME_AUTH_TOKEN_SECRET_NAME", "runtime-secrets")
	t.Setenv("KOGNITION_RUNTIME_AUTH_TOKEN_SECRET_KEY", "runtime-token")
	t.Setenv("KOGNITION_RUNTIME_NETWORK_POLICY_ENABLED", "true")
	t.Setenv("KOGNITION_KUBELET_PROBE_INGRESS_CIDRS", "172.16.0.0/12,fd00::/8")
	t.Setenv("KOGNITION_CILIUM_HOST_PROBE_INGRESS_ENABLED", "true")
	t.Setenv("KOGNITION_MEMORY_POLICY_GATEWAY_NAMESPACE", "kognition-system")
	t.Setenv("KOGNITION_AGENT_COMMUNICATION_GATEWAY_URL", "http://agent-communication-gateway")
	t.Setenv("KOGNITION_AGENT_COMMUNICATION_GATEWAY_PORT", "18094")
	t.Setenv("KOGNITION_AGENT_COMMUNICATION_GATEWAY_NAMESPACE", "kognition-system")
	t.Setenv("KOGNITION_AGENT_DELEGATION_CAPABILITY_HMAC_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("KOGNITION_KNOWLEDGE_GATEWAY_URL", "http://knowledge-gateway")
	t.Setenv("KOGNITION_KNOWLEDGE_GATEWAY_PORT", "18095")
	t.Setenv("KOGNITION_KNOWLEDGE_GATEWAY_NAMESPACE", "kognition-system")
	t.Setenv("KOGNITION_KNOWLEDGE_CAPABILITY_HMAC_KEY", "0123456789abcdef0123456789abcdef")
	dispatcher, err := dispatcherConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if dispatcher.AgentImage != "agent:test" || dispatcher.RuntimeImages["autogen"] != "autogen:test" || dispatcher.RuntimeImages["mcp"] != "mcp:test" || dispatcher.AgentNamespace != "agents" || dispatcher.MemoryServiceURL != "http://memory" || dispatcher.MemoryPolicyGatewayURL != "http://memory-policy" || dispatcher.MemoryPolicyGatewayPort != 18091 || dispatcher.MemoryCapabilityIssuer == nil || dispatcher.RuntimeAuthTokenSecretName != "runtime-secrets" || dispatcher.RuntimeAuthTokenSecretKey != "runtime-token" || !dispatcher.RuntimeNetworkPolicyEnabled || !dispatcher.CiliumHostProbeIngress || len(dispatcher.KubeletProbeIngressCIDRs) != 2 || dispatcher.MemoryPolicyGatewayNamespace != "kognition-system" || dispatcher.KnowledgeGatewayURL != "http://knowledge-gateway" || dispatcher.KnowledgeGatewayPort != 18095 || dispatcher.KnowledgeGatewayNamespace != "kognition-system" || dispatcher.KnowledgeSessionCapabilityIssuer == nil || dispatcher.AgentCommunicationGatewayURL != "http://agent-communication-gateway" || dispatcher.AgentCommunicationGatewayPort != 18094 || dispatcher.AgentCommunicationGatewayNamespace != "kognition-system" || dispatcher.AgentDelegationSessionCapabilityIssuer == nil {
		t.Fatalf("dispatcher config = %#v", dispatcher)
	}
	t.Setenv("KOGNITION_KNOWLEDGE_GATEWAY_PORT", "invalid")
	if _, err := dispatcherConfigFromEnv(); err == nil {
		t.Fatal("invalid knowledge gateway port was accepted")
	}
	t.Setenv("KOGNITION_KNOWLEDGE_GATEWAY_PORT", "18095")
	t.Setenv("KOGNITION_KNOWLEDGE_CAPABILITY_HMAC_KEY", "invalid")
	if _, err := dispatcherConfigFromEnv(); err == nil {
		t.Fatal("invalid capability key configured a knowledge session issuer")
	}
	t.Setenv("KOGNITION_KNOWLEDGE_CAPABILITY_HMAC_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("KOGNITION_MEMORY_POLICY_GATEWAY_PORT", "invalid")
	if _, err := dispatcherConfigFromEnv(); err == nil {
		t.Fatal("invalid memory policy gateway port was accepted")
	}
	t.Setenv("KOGNITION_MEMORY_POLICY_GATEWAY_PORT", "18091")
	t.Setenv("KOGNITION_RUNTIME_AUTH_TOKEN_SECRET_NAME", "")
	if _, err := dispatcherConfigFromEnv(); err == nil {
		t.Fatal("runtime auth token without Secret metadata was accepted")
	}
	t.Setenv("KOGNITION_RUNTIME_AUTH_TOKEN_SECRET_NAME", "runtime-secrets")
	t.Setenv("KOGNITION_MEMORY_CAPABILITY_HMAC_KEY", "invalid")
	if _, err := dispatcherConfigFromEnv(); err == nil {
		t.Fatal("invalid capability key configured a memory policy issuer")
	}
	t.Setenv("KOGNITION_MEMORY_CAPABILITY_HMAC_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("KOGNITION_AGENT_DELEGATION_CAPABILITY_HMAC_KEY", "invalid")
	if _, err := dispatcherConfigFromEnv(); err == nil {
		t.Fatal("invalid capability key configured an agent delegation issuer")
	}
	t.Setenv("KOGNITION_AGENT_DELEGATION_CAPABILITY_HMAC_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("KOGNITION_KUBELET_PROBE_INGRESS_CIDRS", "invalid")
	if _, err := dispatcherConfigFromEnv(); err == nil {
		t.Fatal("invalid kubelet probe ingress CIDR was accepted")
	}
	t.Setenv("KOGNITION_KUBELET_PROBE_INGRESS_CIDRS", "")
	t.Setenv("KOGNITION_CILIUM_HOST_PROBE_INGRESS_ENABLED", "false")
	if _, err := dispatcherConfigFromEnv(); err == nil {
		t.Fatal("runtime NetworkPolicies without probe ingress configuration were accepted")
	}
	t.Setenv("KOGNITION_CILIUM_HOST_PROBE_INGRESS_ENABLED", "true")
	if _, err := dispatcherConfigFromEnv(); err != nil {
		t.Fatalf("Cilium host probe ingress mode was rejected: %v", err)
	}

	t.Setenv("KOGNITION_KB_INGESTER_IMAGE", "ingester:test")
	t.Setenv("KOGNITION_KB_INGESTER_SERVICE_ACCOUNT", "ingester")
	t.Setenv("KOGNITION_CONTROLLER_NAMESPACE", "system")
	ingester := ingesterOptionsFromEnv()
	if ingester.Image != "ingester:test" || ingester.ServiceAccount != "ingester" || ingester.ControllerNamespace != "system" {
		t.Fatalf("ingester options = %#v", ingester)
	}
}

func TestControllerLoggerAndLeaderElection(t *testing.T) {
	logger, err := newControllerLogger()
	if err != nil || logger == nil {
		t.Fatalf("newControllerLogger() = (%v, %v)", logger, err)
	}
	_ = logger.Sync()
	if !(&autonomyDefaultsRunnable{}).NeedLeaderElection() {
		t.Fatal("autonomy defaults must require leader election")
	}
}

type stubManagerReconciler struct {
	err   error
	calls *atomic.Int32
}

type startupManager struct {
	ctrl.Manager
	addErr         error
	readyErr       error
	adds           atomic.Int32
	readinessNames []string
	readiness      []healthz.Checker
	runnables      []ctrlmanager.Runnable
}

func (m *startupManager) GetClient() client.Client    { return nil }
func (m *startupManager) GetAPIReader() client.Reader { return nil }
func (m *startupManager) AddReadyzCheck(name string, checker healthz.Checker) error {
	m.readinessNames = append(m.readinessNames, name)
	m.readiness = append(m.readiness, checker)
	return m.readyErr
}
func (m *startupManager) Add(runnable ctrlmanager.Runnable) error {
	m.adds.Add(1)
	m.runnables = append(m.runnables, runnable)
	return m.addErr
}

type canaryOperationCapableStore struct {
	store.Store
	store.CanaryOperationStore
}

func TestConfigureCanaryOperationRetention(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		manager := &startupManager{}
		if err := configureCanaryOperationRetention(manager, &canaryOperationCapableStore{}, zap.NewNop(), "72h"); err != nil {
			t.Fatalf("configureCanaryOperationRetention() error = %v", err)
		}
		if got := manager.adds.Load(); got != 1 {
			t.Fatalf("registered runnables = %d, want 1", got)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		manager := &startupManager{}
		if err := configureCanaryOperationRetention(manager, &canaryOperationCapableStore{}, zap.NewNop(), "0"); err != nil {
			t.Fatalf("configureCanaryOperationRetention() error = %v", err)
		}
		if got := manager.adds.Load(); got != 0 {
			t.Fatalf("registered runnables = %d, want 0", got)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		if err := configureCanaryOperationRetention(&startupManager{}, &canaryOperationCapableStore{}, zap.NewNop(), "invalid"); err == nil || !strings.Contains(err.Error(), "configure canary operation retention") {
			t.Fatalf("configuration error = %v", err)
		}
	})

	t.Run("registration failure", func(t *testing.T) {
		wantErr := errors.New("registration failed")
		err := configureCanaryOperationRetention(&startupManager{addErr: wantErr}, &canaryOperationCapableStore{}, zap.NewNop(), "72h")
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "register canary operation retention") {
			t.Fatalf("registration error = %v", err)
		}
	})
}

func TestPluginHostProductionConfiguration(t *testing.T) {
	for _, key := range []string{
		"KOGNITION_PLUGIN_HOST_ENABLED",
		"KOGNITION_PLUGIN_SOCKET_DIR",
		"KOGNITION_PLUGIN_HOST_VERSION",
		"KOGNITION_PLUGIN_DISCOVERY_INTERVAL",
		"KOGNITION_PLUGIN_PROBE_INTERVAL",
		"KOGNITION_PLUGIN_PROBE_TIMEOUT",
		"KOGNITION_PLUGIN_CONNECT_TIMEOUT",
		"KOGNITION_PLUGIN_RECONNECT_BACKOFF",
		"KOGNITION_PLUGIN_MAX_RECONNECT_BACKOFF",
	} {
		t.Setenv(key, "")
	}
	cfg, enabled, err := pluginHostConfigFromEnv()
	if err != nil || enabled || cfg.SocketDir == "" {
		t.Fatalf("disabled defaults = (%#v, %v, %v)", cfg, enabled, err)
	}

	t.Setenv("KOGNITION_PLUGIN_HOST_ENABLED", "true")
	t.Setenv("KOGNITION_PLUGIN_SOCKET_DIR", t.TempDir())
	t.Setenv("KOGNITION_PLUGIN_HOST_VERSION", "test-host")
	t.Setenv("KOGNITION_PLUGIN_DISCOVERY_INTERVAL", "11ms")
	t.Setenv("KOGNITION_PLUGIN_PROBE_INTERVAL", "12ms")
	t.Setenv("KOGNITION_PLUGIN_PROBE_TIMEOUT", "13ms")
	t.Setenv("KOGNITION_PLUGIN_CONNECT_TIMEOUT", "14ms")
	t.Setenv("KOGNITION_PLUGIN_RECONNECT_BACKOFF", "15ms")
	t.Setenv("KOGNITION_PLUGIN_MAX_RECONNECT_BACKOFF", "16ms")
	cfg, enabled, err = pluginHostConfigFromEnv()
	if err != nil || !enabled {
		t.Fatalf("enabled config = (%#v, %v, %v)", cfg, enabled, err)
	}
	if cfg.HostVersion != "test-host" || cfg.DiscoveryInterval != 11*time.Millisecond || cfg.ProbeInterval != 12*time.Millisecond || cfg.ProbeTimeout != 13*time.Millisecond || cfg.ConnectTimeout != 14*time.Millisecond || cfg.ReconnectBackoff != 15*time.Millisecond || cfg.MaxReconnectBackoff != 16*time.Millisecond {
		t.Fatalf("configured host = %#v", cfg)
	}

	invalid := []struct {
		name  string
		key   string
		value string
	}{
		{name: "enabled", key: "KOGNITION_PLUGIN_HOST_ENABLED", value: "invalid"},
		{name: "duration", key: "KOGNITION_PLUGIN_PROBE_INTERVAL", value: "0s"},
		{name: "backoff range", key: "KOGNITION_PLUGIN_MAX_RECONNECT_BACKOFF", value: "1ms"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			if _, _, err := pluginHostConfigFromEnv(); err == nil {
				t.Fatal("invalid plugin host configuration was accepted")
			}
		})
	}
}

func TestConfigurePluginHostLifecycle(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		t.Setenv("KOGNITION_PLUGIN_HOST_ENABLED", "false")
		manager := &startupManager{}
		host, err := configurePluginHost(manager, zap.NewNop())
		if err != nil || host != nil || manager.adds.Load() != 0 || len(manager.readinessNames) != 0 {
			t.Fatalf("disabled registration = (%v, %v, adds %d, readiness %v)", host, err, manager.adds.Load(), manager.readinessNames)
		}
	})

	t.Run("start readiness cancel and stop", func(t *testing.T) {
		t.Setenv("KOGNITION_PLUGIN_HOST_ENABLED", "true")
		t.Setenv("KOGNITION_PLUGIN_SOCKET_DIR", t.TempDir())
		manager := &startupManager{}
		host, err := configurePluginHost(manager, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		if manager.adds.Load() != 1 || len(manager.runnables) != 1 || !reflect.DeepEqual(manager.readinessNames, []string{"plugin-host"}) {
			t.Fatalf("plugin host registration = adds %d, runnables %d, readiness %v", manager.adds.Load(), len(manager.runnables), manager.readinessNames)
		}
		runnable := manager.runnables[0].(*pluginHostRunnable)
		if host == nil || runnable.host != host {
			t.Fatal("plugin host ownership seam does not expose the manager-owned host")
		}
		if runnable.NeedLeaderElection() {
			t.Fatal("plugin host must run on every controller replica")
		}
		readiness := manager.readiness[0]
		if err := readiness(nil); err == nil {
			t.Fatal("plugin host reported ready before start")
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- runnable.Start(ctx) }()
		deadline := time.Now().Add(time.Second)
		for readiness(nil) != nil && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if err := readiness(nil); err != nil {
			t.Fatalf("plugin host did not become ready: %v", err)
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("plugin host shutdown error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("plugin host did not stop after cancellation")
		}
		if err := readiness(nil); err == nil {
			t.Fatal("plugin host reported ready after stop")
		}
	})

	t.Run("startup failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "socket-parent")
		if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KOGNITION_PLUGIN_HOST_ENABLED", "true")
		t.Setenv("KOGNITION_PLUGIN_SOCKET_DIR", filepath.Join(path, "plugins"))
		manager := &startupManager{}
		if _, err := configurePluginHost(manager, zap.NewNop()); err != nil {
			t.Fatal(err)
		}
		err := manager.runnables[0].Start(context.Background())
		if err == nil || !strings.Contains(err.Error(), "start plugin host") {
			t.Fatalf("startup error = %v", err)
		}
	})

	t.Run("registration failures", func(t *testing.T) {
		t.Setenv("KOGNITION_PLUGIN_HOST_ENABLED", "true")
		t.Setenv("KOGNITION_PLUGIN_SOCKET_DIR", t.TempDir())
		wantErr := errors.New("registration failed")
		if _, err := configurePluginHost(&startupManager{addErr: wantErr}, zap.NewNop()); !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "register plugin host") {
			t.Fatalf("runnable registration error = %v", err)
		}
		if _, err := configurePluginHost(&startupManager{readyErr: wantErr}, zap.NewNop()); !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "register plugin host readiness") {
			t.Fatalf("readiness registration error = %v", err)
		}
	})
}

func TestRunControllerStartupFailures(t *testing.T) {
	wantErr := errors.New("startup failed")
	err := runControllerWithManager(controllerOptions{}, zap.NewNop(), func(k8sctrl.ManagerOptions) (ctrl.Manager, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "create manager") {
		t.Fatalf("manager creation error = %v", err)
	}

	t.Setenv("KOGNITION_CONTROLLER_NAMESPACE", "")
	err = runControllerWithManager(controllerOptions{}, zap.NewNop(), func(k8sctrl.ManagerOptions) (ctrl.Manager, error) {
		return &startupManager{addErr: wantErr}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "KOGNITION_CONTROLLER_NAMESPACE is required") {
		t.Fatalf("missing controller namespace error = %v", err)
	}

	t.Setenv("KOGNITION_CONTROLLER_NAMESPACE", "system")
	err = runControllerWithManager(controllerOptions{}, zap.NewNop(), func(k8sctrl.ManagerOptions) (ctrl.Manager, error) {
		return &startupManager{addErr: wantErr}, nil
	})
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "register autonomy defaults") {
		t.Fatalf("runnable registration error = %v", err)
	}
}

func TestAutonomyDefaultsRunnablePolicyDefaults(t *testing.T) {
	runnable := &autonomyDefaultsRunnable{}
	if runnable.ensureFunc() == nil {
		t.Fatal("default ensure function is nil")
	}
	if got := runnable.retryInterval(); got != 2*time.Second {
		t.Fatalf("default retry interval = %v", got)
	}
	customEnsure := func(context.Context, store.Store, *zap.Logger) error { return nil }
	runnable.ensure = customEnsure
	runnable.retryPeriod = time.Second
	if runnable.ensureFunc() == nil || runnable.retryInterval() != time.Second {
		t.Fatal("custom runnable policy was not retained")
	}
}

func TestRuntimeApprovalStartupPolicyAndClockSkewConfiguration(t *testing.T) {
	if (&runtimeApprovalBindingKeysRunnable{}).NeedLeaderElection() {
		t.Fatal("RuntimeApproval key activation must run on every replica")
	}
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_MAXIMUM_CLOCK_SKEW", "45s")
	if skew, err := runtimeApprovalClockSkewFromEnv(); err != nil || skew != 45*time.Second {
		t.Fatalf("configured clock skew = %v, %v", skew, err)
	}
	for _, invalid := range []string{"29s", "invalid"} {
		t.Setenv("KOGNITION_RUNTIME_APPROVAL_MAXIMUM_CLOCK_SKEW", invalid)
		if _, err := runtimeApprovalClockSkewFromEnv(); err == nil {
			t.Fatalf("invalid clock skew %q was accepted", invalid)
		}
	}
}

func TestRuntimeApprovalQuotaConfigurationFromEnv(t *testing.T) {
	defaults := k8sctrl.DefaultRuntimeApprovalQuotaLimits()
	if got, err := runtimeApprovalQuotaLimitsFromEnv(); err != nil || got != defaults {
		t.Fatalf("default quota limits = %#v, %v", got, err)
	}
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_WORKSPACE_RECORDS", "9000")
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_WORKSPACE_BYTES", "62914560")
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_AGENT_RECORDS", "900")
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_AGENT_BYTES", "7340032")
	want := k8sctrl.RuntimeApprovalQuotaLimits{WorkspaceRecords: 9000, WorkspaceBytes: 62914560, AgentRecords: 900, AgentBytes: 7340032}
	if got, err := runtimeApprovalQuotaLimitsFromEnv(); err != nil || got != want {
		t.Fatalf("configured quota limits = %#v, %v", got, err)
	}
	for _, value := range []string{"0", "invalid", "10001"} {
		t.Setenv("KOGNITION_RUNTIME_APPROVAL_WORKSPACE_RECORDS", value)
		if _, err := runtimeApprovalQuotaLimitsFromEnv(); err == nil {
			t.Fatalf("invalid workspace record limit %q was accepted", value)
		}
	}
}

func TestRuntimeApprovalBindingKeysRunnablePropagatesActivationFailure(t *testing.T) {
	runnable := &runtimeApprovalBindingKeysRunnable{store: k8sctrl.NewStoreAdapter(nil), keys: nil}
	if err := runnable.Start(context.Background()); !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
		t.Fatalf("activation failure = %v, want binding key unavailable", err)
	}
}

func (r stubManagerReconciler) SetupWithManager(ctrl.Manager) error {
	r.calls.Add(1)
	return r.err
}

func TestParseControllerOptions(t *testing.T) {
	defaults, err := parseControllerOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.metricsAddr != ":8080" || defaults.probeAddr != ":8081" || !defaults.leaderElect || defaults.leaderElectID != "kognition-k8s-controller" || defaults.leaderElectNS != "" || defaults.disableWebhooks {
		t.Fatalf("defaults = %#v", defaults)
	}

	configured, err := parseControllerOptions([]string{
		"--metrics-bind-address=:9090",
		"--health-probe-bind-address=:9091",
		"--leader-elect=false",
		"--leader-election-id=custom",
		"--leader-election-namespace=system",
		"--disable-webhooks=true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if configured.metricsAddr != ":9090" || configured.probeAddr != ":9091" || configured.leaderElect || configured.leaderElectID != "custom" || configured.leaderElectNS != "system" || !configured.disableWebhooks {
		t.Fatalf("configured = %#v", configured)
	}
	if _, err := parseControllerOptions([]string{"--unknown"}); err == nil {
		t.Fatal("parseControllerOptions() accepted an unknown flag")
	}
}

func TestNewCPIRegistryIncludesProductionDrivers(t *testing.T) {
	want := map[string]bool{"anthropic": true, "aws-bedrock": true, "foundry": true, "ollama": true, "openai-compatible": true}
	registry := newCPIRegistry(nil)
	got := registry.List()
	if len(got) != len(want) {
		t.Fatalf("drivers = %v, want %v", got, want)
	}
	for _, name := range got {
		if !want[name] {
			t.Fatalf("unexpected driver %q in %v", name, got)
		}
	}
	if registry.Observed() {
		t.Fatal("a registry created without a recorder must not be observed")
	}
}

type callRecorder struct{ outcomes []cpi.CallOutcome }

func (r *callRecorder) RecordCall(outcome cpi.CallOutcome) { r.outcomes = append(r.outcomes, outcome) }

func TestNewCPIRegistryReportsCallOutcomes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	recorder := &callRecorder{}
	registry := newCPIRegistry(recorder)
	if !registry.Observed() {
		t.Fatal("a registry created with a recorder must be observed")
	}

	driver, ok := registry.Get("anthropic")
	if !ok {
		t.Fatal("anthropic driver is not registered")
	}
	_, err := driver.Complete(context.Background(), &cpi.CompletionRequest{
		Model: "claude", Endpoint: server.URL, Messages: []cpi.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Complete() succeeded against a rate limiting provider")
	}
	if len(recorder.outcomes) != 1 {
		t.Fatalf("recorded %d outcomes, want 1: %+v", len(recorder.outcomes), recorder.outcomes)
	}
	got := recorder.outcomes[0]
	if got.Provider != "anthropic" || got.Operation != cpi.OperationComplete || got.Model != "claude" || got.Reason != cpi.ReasonRateLimited || !got.ProviderFault {
		t.Fatalf("outcome = %+v, want a provider-fault RateLimited complete call to claude through anthropic", got)
	}
}

func TestSetupReconcilersStopsOnError(t *testing.T) {
	var firstCalls atomic.Int32
	var failingCalls atomic.Int32
	var skippedCalls atomic.Int32
	wantErr := errors.New("setup failed")
	err := setupReconcilers(nil, []managerReconciler{
		stubManagerReconciler{calls: &firstCalls},
		stubManagerReconciler{err: wantErr, calls: &failingCalls},
		stubManagerReconciler{calls: &skippedCalls},
	})
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "stubManagerReconciler") {
		t.Fatalf("setupReconcilers() error = %v", err)
	}
	if firstCalls.Load() != 1 || failingCalls.Load() != 1 || skippedCalls.Load() != 0 {
		t.Fatalf("calls = first:%d failing:%d skipped:%d", firstCalls.Load(), failingCalls.Load(), skippedCalls.Load())
	}
	if err := setupReconcilers(nil, nil); err != nil {
		t.Fatalf("empty reconcilers: %v", err)
	}
}

func TestSetupWebhooksHonorsDisabledFlag(t *testing.T) {
	calls := 0
	setup := func() error {
		calls++
		return errors.New("setup failed")
	}
	if err := setupWebhooks(true, setup); err != nil || calls != 0 {
		t.Fatalf("disabled setup = (%v, calls %d)", err, calls)
	}
	if err := setupWebhooks(false, setup); err == nil || calls != 1 {
		t.Fatalf("enabled setup = (%v, calls %d)", err, calls)
	}
}

func TestAutonomyDefaultsRunnableRetriesStartupFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var attempts atomic.Int32
	runnable := &autonomyDefaultsRunnable{
		logger:      zap.NewNop(),
		retryPeriod: time.Millisecond,
		ensure: func(context.Context, store.Store, *zap.Logger) error {
			if attempts.Add(1) == 1 {
				return errors.New("webhook not ready")
			}
			cancel()
			return nil
		},
	}

	done := make(chan error, 1)
	go func() { done <- runnable.Start(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not retry and stop after context cancellation")
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("ensure attempts = %d, want 2", got)
	}
}

func TestControllerAssemblyDisabledPluginsAllowBuiltInRuntimeDispatch(test *testing.T) {
	scheme := runtime.NewScheme()
	if err := k8sapi.AddToScheme(scheme); err != nil {
		test.Fatal(err)
	}
	agent := &api.Agent{
		ObjectMeta: api.ObjectMeta{Name: "sdk-agent", Namespace: "tenant", UID: "agent-uid"},
		Spec:       api.AgentSpec{ModelRef: api.LocalObjectReference{Name: "model"}, Runtime: &api.AgentRuntimeSpec{Class: "autogen"}},
	}
	admin := fake.NewClientBuilder().WithScheme(scheme).WithObjects(convert.AgentToK8s(agent)).Build()
	storage := k8sctrl.NewStoreAdapter(admin)
	agentDispatcher := dispatcher.NewK8sDispatcher(admin, storage, zap.NewNop(), dispatcher.Config{})
	newControllerAssembly(storage, admin, nil, agentDispatcher, nil, zap.NewNop(), cpi.NewRegistry(), opa.NewEngine(), k8sctrl.KBIngesterOptions{}, nil, nil, nil)
	instance := &api.AgentInstance{
		ObjectMeta: api.ObjectMeta{Name: "sdk-session", Namespace: "tenant", UID: "instance-uid"},
		Spec:       api.AgentInstanceSpec{AgentRef: api.LocalObjectReference{Name: agent.Name}},
	}
	_, err := agentDispatcher.Dispatch(context.Background(), instance)
	if err == nil {
		test.Fatal("unconfigured built-in runtime must fail closed without invoking a disabled plugin host")
	}
}

func TestControllerAssemblyRegistersProductionRoster(t *testing.T) {
	t.Setenv("KOGNITION_MEMORY_SERVICE_URL", "http://memory.test:8090")
	t.Setenv("KOGNITION_SCHEDULER_PLUGINS", " session-locality, cost-aware, context-capacity ")
	pluginHost := plugin.NewHost(plugin.DefaultHostConfig(), zap.NewNop())
	agentDispatcher := dispatcher.NewK8sDispatcher(nil, nil, zap.NewNop(), dispatcher.Config{})
	assembly := newControllerAssembly(
		nil,
		nil,
		nil,
		agentDispatcher,
		pluginHost,
		zap.NewNop(),
		cpi.NewRegistry(),
		opa.NewEngine(),
		k8sctrl.KBIngesterOptions{
			Image:               "ingester:test",
			ServiceAccount:      "ingester",
			ControllerNamespace: "kognition-system",
		},
		nil,
		nil,
		nil,
	)
	if assembly.pluginHost != pluginHost {
		t.Fatal("controller assembly did not retain the production plugin host")
	}
	selector := reflect.ValueOf(agentDispatcher).Elem().FieldByName("runtimePluginSelector")
	if selector.IsNil() {
		t.Fatal("controller assembly did not inject the plugin host into the runtime dispatcher")
	}

	wantReconcilers := []string{
		"AgentReconcilerAdapter",
		"AgentInstanceReconcilerAdapter",
		"AgentJobReconcilerAdapter",
		"AgentPolicyReconcilerAdapter",
		"CognitiveQuotaReconcilerAdapter",
		"AgentIdentityReconcilerAdapter",
		"KnowledgeBaseReconcilerAdapter",
		"KnowledgeBaseIndexRunReconcilerAdapter",
		"KnowledgeBaseIndexScheduleReconcilerAdapter",
		"KnowledgeBaseClaimReconcilerAdapter",
		"MemoryClaimReconcilerAdapter",
		"MemoryClassReconcilerAdapter",
		"MemoryVolumeReconcilerAdapter",
		"MemoryGrantReconcilerAdapter",
		"MemoryRecordReconcilerAdapter",
		"MemorySummarizationReconcilerAdapter",
		"MemoryRecordSummarizationReconcilerAdapter",
		"ModelClassReconcilerAdapter",
		"ModelReconcilerAdapter",
		"ModelAutoscalerReconcilerAdapter",
		"AgentAutoscalerReconcilerAdapter",
		"AutonomyReviewReconcilerAdapter",
		"TrustProfileReconcilerAdapter",
		"TrustProfileIndexReconciler",
		"AgentServiceReconcilerAdapter",
		"IntentGatewayReconcilerAdapter",
		"PromptReconcilerAdapter",
		"SolutionReconcilerAdapter",
		"EvaluationReconcilerAdapter",
		"EvaluationRunReconcilerAdapter",
		"SecretRefReconcilerAdapter",
		"ToolReconcilerAdapter",
		"ToolClassReconcilerAdapter",
		"ToolInvocationRunReconcilerAdapter",
		"WorkflowReconcilerAdapter",
		"WorkflowRunReconcilerAdapter",
		"WorkspaceReconcilerAdapter",
	}
	assertConcreteTypeNames(t, "reconcilers", assembly.reconcilers, wantReconcilers)

	for _, reconciler := range assembly.reconcilers {
		value := reflect.ValueOf(reconciler)
		if value.Elem().Type().Name() == "AgentInstanceReconcilerAdapter" {
			scheduler := value.Elem().FieldByName("internal").Elem().FieldByName("scheduler")
			if scheduler.IsNil() {
				t.Fatal("AgentInstanceReconcilerAdapter has no configured scheduler")
			}
		}
		if value.Elem().Type().Name() != "MemoryClaimReconcilerAdapter" {
			continue
		}
		provisioner := value.Elem().FieldByName("internal").Elem().FieldByName("provisioner")
		if provisioner.IsNil() {
			t.Fatal("MemoryClaimReconcilerAdapter has no configured memory service provisioner")
		}
		break
	}
	for _, reconciler := range assembly.reconcilers {
		name := reflect.ValueOf(reconciler).Elem().Type().Name()
		if name != "WorkflowReconcilerAdapter" && name != "WorkflowRunReconcilerAdapter" {
			continue
		}
		internal := reflect.ValueOf(reconciler).Elem().FieldByName("internal").Elem()
		if internal.FieldByName("communicationPolicy").IsNil() {
			t.Fatalf("%s has no configured communication policy authorizer", name)
		}
	}

	wantMutating := []string{"LimitRangeDefaulter", "AgentDefaulter"}
	assertConcreteTypeNames(t, "mutating hooks", assembly.mutating, wantMutating)

	wantValidating := []string{
		"SecretRefValidator",
		"ModelInvocationValidator",
		"ToolValidator",
		"ToolClassValidator",
		"ToolGrantValidator",
		"SignatureValidator",
		"CognitiveAdmissionPolicyHook",
		"QuotaEnforcer",
		"AutonomyClassValidator",
		"TrustProfileValidator",
		"RuntimeApprovalValidator",
		"AgentValidator",
		"AgentServiceValidator",
		"SolutionValidator",
		"WorkflowValidator",
		"KnowledgeBaseClassValidator",
		"KnowledgeBaseValidator",
		"KnowledgeBaseClaimValidator",
		"KnowledgeBaseIndexRunValidator",
		"KnowledgeBaseIndexScheduleValidator",
		"ApprovalDecisionValidator",
		"AutonomyReviewValidator",
		"IntentGatewayValidator",
		"AgentIdentityValidator",
		"CrossNamespaceHook",
	}
	assertConcreteTypeNames(t, "validating hooks", assembly.validating, wantValidating)
}

func TestSchedulerPluginNamesFromEnv(t *testing.T) {
	t.Setenv("KOGNITION_SCHEDULER_PLUGINS", " context-capacity, ,session-locality,cost-aware ")
	want := []string{"context-capacity", "session-locality", "cost-aware"}
	if got := schedulerPluginNamesFromEnv(); !reflect.DeepEqual(got, want) {
		t.Fatalf("scheduler plugin names = %v, want %v", got, want)
	}

	t.Setenv("KOGNITION_SCHEDULER_PLUGINS", "")
	if got := schedulerPluginNamesFromEnv(); got != nil {
		t.Fatalf("empty scheduler plugin names = %v, want nil", got)
	}
}

func TestControllerAssemblyKeepsProbeReconcilerWithoutDurableCanaryStore(t *testing.T) {
	assembly := newControllerAssembly(
		&canaryOperationCapableStore{},
		nil,
		nil,
		nil,
		nil,
		zap.NewNop(),
		cpi.NewRegistry(),
		opa.NewEngine(),
		k8sctrl.KBIngesterOptions{},
		nil,
		nil,
		nil,
	)

	for _, reconciler := range assembly.reconcilers {
		if reflect.ValueOf(reconciler).Elem().Type().Name() == "ProbeReconcilerAdapter" {
			return
		}
	}
	t.Fatal("controller assembly omitted ProbeReconcilerAdapter for a store without durable reasoning canaries")
}

func TestGetEnvOrDefault(t *testing.T) {
	t.Setenv("KOGNITION_TEST_SETTING", "configured")
	if got := getEnvOrDefault("KOGNITION_TEST_SETTING", "fallback"); got != "configured" {
		t.Fatalf("configured value = %q, want configured", got)
	}
	t.Setenv("KOGNITION_TEST_SETTING", "")
	if got := getEnvOrDefault("KOGNITION_TEST_SETTING", "fallback"); got != "fallback" {
		t.Fatalf("empty value = %q, want fallback", got)
	}
}

func TestRuntimeApprovalAdmissionUsernamesFromEnv(t *testing.T) {
	t.Setenv("KOGNITION_CONTROLLER_NAMESPACE", "kognition-system")
	t.Setenv("KOGNITION_CONTROLLER_SERVICE_ACCOUNT", "kognition-controller")
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_WRITER_SERVICE_ACCOUNTS", "kognition-tool-gateway, kognition-memory-policy-gateway")
	want := []string{
		"system:serviceaccount:kognition-system:kognition-controller",
		"system:serviceaccount:kognition-system:kognition-tool-gateway",
		"system:serviceaccount:kognition-system:kognition-memory-policy-gateway",
	}
	if got := runtimeApprovalAdmissionUsernamesFromEnv(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RuntimeApproval admission usernames = %#v, want %#v", got, want)
	}
	t.Setenv("KOGNITION_CONTROLLER_SERVICE_ACCOUNT", "")
	if got := runtimeApprovalAdmissionUsernamesFromEnv(); got != nil {
		t.Fatalf("RuntimeApproval admission usernames without controller service account = %#v, want nil", got)
	}
}

func TestRuntimeApprovalDecisionAuthorityFromEnv(t *testing.T) {
	t.Setenv("KOGNITION_CONTROLLER_NAMESPACE", "control")
	t.Setenv("KOGNITION_CONTROLLER_SERVICE_ACCOUNT", "controller")
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_WRITER_SERVICE_ACCOUNTS", "tool-gateway,memory-policy-gateway")
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_DECISION_SERVICE_ACCOUNT", "")
	if got := runtimeApprovalDecisionAuthorityFromEnv(); got != "" {
		t.Fatal("lifecycle identities implicitly authorized decisions", got)
	}
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_DECISION_SERVICE_ACCOUNT", "resource-webhooks")
	if got := runtimeApprovalDecisionAuthorityFromEnv(); got != "system:serviceaccount:control:resource-webhooks" {
		t.Fatal("decision authority is not explicit", got)
	}
	t.Setenv("KOGNITION_CONTROLLER_NAMESPACE", "")
	if got := runtimeApprovalDecisionAuthorityFromEnv(); got != "" {
		t.Fatal("unscoped decision authority accepted", got)
	}
}

func TestRuntimeApprovalExecutionWritersFromEnv(t *testing.T) {
	t.Setenv("KOGNITION_CONTROLLER_NAMESPACE", "control")
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_TOOL_SERVICE_ACCOUNT", "tool")
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_MEMORY_SERVICE_ACCOUNT", "memory")
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_COMMUNICATION_SERVICE_ACCOUNT", "communication")
	want := map[string]string{"system:serviceaccount:control:tool": "Tool", "system:serviceaccount:control:memory": "Memory", "system:serviceaccount:control:communication": "Communication"}
	if got := runtimeApprovalExecutionWritersFromEnv(); !reflect.DeepEqual(got, want) {
		t.Fatal("execution authority map differs", got)
	}
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_MEMORY_SERVICE_ACCOUNT", "tool")
	if got := runtimeApprovalExecutionWritersFromEnv(); got["system:serviceaccount:control:tool"] != "" {
		t.Fatal("ambiguous cross-plane identity accepted", got)
	}
	t.Setenv("KOGNITION_CONTROLLER_NAMESPACE", "")
	if got := runtimeApprovalExecutionWritersFromEnv(); len(got) != 0 {
		t.Fatal("unscoped execution identity accepted", got)
	}
}

func assertConcreteTypeNames[T any](t *testing.T, label string, values []T, want []string) {
	t.Helper()
	got := make([]string, 0, len(values))
	for _, value := range values {
		typeOf := reflect.TypeOf(value)
		if typeOf.Kind() == reflect.Pointer {
			typeOf = typeOf.Elem()
		}
		got = append(got, typeOf.Name())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}
