// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/gatewaybackend"
	k8sctrl "github.com/kognition-ai/kognition/internal/k8s/controller"
	"github.com/kognition-ai/kognition/internal/memory/policygateway"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/toolexec"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/kognition-ai/kognition/internal/store/postgres"
	"github.com/kognition-ai/kognition/internal/toolgateway"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDeployedWorkloadIdentityGateways(test *testing.T) {
	if os.Getenv("KOGNITION_IDENTITY_GATEWAY_ACCEPTANCE") != "true" {
		test.Skip("explicit isolated gateway acceptance required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	configuration, err := rest.InClusterConfig()
	if err != nil {
		test.Fatal(err)
	}
	configuration.Timeout = 10 * time.Second
	admin, err := client.New(configuration, client.Options{Scheme: k8sctrl.SetupScheme()})
	if err != nil {
		test.Fatal(err)
	}
	var root corev1.Namespace
	if err := admin.Get(ctx, client.ObjectKey{Name: "kube-system"}, &root); err != nil {
		test.Fatal(err)
	}
	if expected := os.Getenv("KOGNITION_IDENTITY_CLUSTER_UID"); expected == "" || string(root.UID) != expected {
		test.Fatal("isolated cluster UID mismatch")
	}
	var nodes corev1.NodeList
	if err := admin.List(ctx, &nodes); err != nil {
		test.Fatal(err)
	}
	runID := os.Getenv("KOGNITION_IDENTITY_RUN_ID")
	if parsed, err := uuid.Parse(runID); err != nil || parsed == uuid.Nil || parsed.String() != runID || len(nodes.Items) != 1 || nodes.Items[0].Name != "kognition-"+runID+"-control-plane" || nodes.Items[0].Labels["kognition.dev/identity-acceptance-run"] != runID {
		test.Fatal("fixture node ownership mismatch")
	}
	if err := k8sctrl.NewHostClockHealthCheck(30 * time.Second)(); err != nil {
		test.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, os.Getenv("KOGNITION_IDENTITY_POSTGRES_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		test.Fatal(err)
	}
	keys, err := runtimeapproval.BindingKeyStoreFromJSON(os.Getenv(runtimeapproval.BindingKeysEnvironmentVariable))
	if err != nil {
		test.Fatal(err)
	}
	kubernetesStore := k8sctrl.NewStoreAdapterWithControlNamespace(admin, "identity-kubernetes")
	if err := kubernetesStore.ConfigureRuntimeApprovalClock(30*time.Second, k8sctrl.NewHostClockHealthCheck(30*time.Second)); err != nil {
		test.Fatal(err)
	}
	stores := map[string]store.Store{"kubernetes": kubernetesStore, "standalone": postgres.New(pool, zap.NewNop())}
	if selected := os.Getenv("KOGNITION_IDENTITY_GATEWAY_MODE"); selected != "" {
		storage, exists := stores[selected]
		if !exists {
			test.Fatal("unknown gateway acceptance mode")
		}
		stores = map[string]store.Store{selected: storage}
	}
	for mode, storage := range stores {
		test.Run(mode, func(test *testing.T) {
			backend, ok := storage.(gatewaybackend.Store)
			if !ok {
				test.Fatal("production RuntimeApproval store required")
			}
			if err := backend.ActivateRuntimeApprovalBindingKeys(ctx, keys); err != nil {
				test.Fatal(err)
			}
			coordinator, err := runtimeapproval.NewGate3Coordinator(runtimeapproval.Gate3Config{Store: backend, Resolver: runtimeapproval.NewResolver(storage), Keys: keys, TTL: time.Minute})
			if err != nil {
				test.Fatal(err)
			}
			claimant := api.RuntimeApprovalClaimant{WorkloadUID: os.Getenv("KOGNITION_RUNTIME_APPROVAL_WORKLOAD_UID"), ProcessStartNonce: uuid.NewString()}
			if parsed, err := uuid.Parse(claimant.WorkloadUID); err != nil || parsed == uuid.Nil || parsed.String() != claimant.WorkloadUID {
				test.Fatal("actual runner Pod UID required for execution claimant")
			}
			if mode == "standalone" {
				policy := api.AutonomyClass{TypeMeta: api.TypeMeta{APIVersion: api.APIVersion, Kind: "AutonomyClass"}, ObjectMeta: api.ObjectMeta{Name: "supervised"}, Spec: api.AutonomyClassSpec{ApprovalPolicy: &api.ApprovalPolicy{ToolInvocation: "never", MemoryWrite: "always", ExternalCommunication: "always"}}}
				if _, err := storage.Get(ctx, "AutonomyClass", "system", "supervised"); errors.Is(err, store.ErrNotFound) {
					body, err := json.Marshal(policy)
					if err != nil {
						test.Fatal(err)
					}
					if err := storage.Create(ctx, "AutonomyClass", "system", "supervised", body); err != nil {
						test.Fatal(err)
					}
				} else if err != nil {
					test.Fatal(err)
				}
			}
			namespace := "identity-" + mode
			read := func(kind, name string, value any) {
				test.Helper()
				body, err := storage.Get(ctx, kind, namespace, name)
				if err != nil {
					test.Fatal(err)
				}
				if err := json.Unmarshal(body, value); err != nil {
					test.Fatal(err)
				}
			}
			create := func(kind, name string, value any) {
				test.Helper()
				body, err := json.Marshal(value)
				if err != nil {
					test.Fatal(err)
				}
				var document map[string]any
				if err := json.Unmarshal(body, &document); err != nil {
					test.Fatal(err)
				}
				document["kind"], document["apiVersion"] = kind, api.APIVersion
				body, err = json.Marshal(document)
				if err != nil {
					test.Fatal(err)
				}
				if err := storage.Create(ctx, kind, namespace, name, body); err != nil {
					test.Fatal(err)
				}
				if status, exists := document["status"]; exists {
					var stored map[string]any
					read(kind, name, &stored)
					var metadata api.ObjectMeta
					encoded, err := json.Marshal(stored["metadata"])
					if err != nil {
						test.Fatal(err)
					}
					if err := json.Unmarshal(encoded, &metadata); err != nil {
						test.Fatal(err)
					}
					stored["status"] = status
					body, err = json.Marshal(stored)
					if err != nil {
						test.Fatal(err)
					}
					if err := storage.Update(ctx, kind, namespace, name, body, metadata.ResourceVersion); err != nil {
						test.Fatal(err)
					}
				}
			}
			var agent api.Agent
			read("Agent", "worker", &agent)
			id, err := runtimeauth.AgentIncarnationSPIFFEID("kognition.dev", namespace, "worker", agent.UID)
			if err != nil {
				test.Fatal(err)
			}
			var source *runtimeauth.WorkloadIdentity
			enrollment, stopEnrollment := context.WithTimeout(ctx, 45*time.Second)
			defer stopEnrollment()
			enrollmentTicker := time.NewTicker(time.Second)
			defer enrollmentTicker.Stop()
			for {
				source, err = runtimeauth.NewWorkloadIdentity(enrollment, runtimeauth.WorkloadIdentityConfig{SocketAddress: os.Getenv("KOGNITION_SPIFFE_ENDPOINT_SOCKET"), ExpectedID: id, TrustDomain: "kognition.dev"})
				if err == nil {
					break
				}
				select {
				case <-enrollment.Done():
					test.Fatal("current Agent SVID unavailable", err)
				case <-enrollmentTicker.C:
				}
			}
			defer func() { _ = source.Close() }()
			identityName := "agent-identity-worker"
			identityReconciler := controller.NewAgentIdentityReconciler(storage, zap.NewNop())
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := identityReconciler.Reconcile(ctx, controller.Request{Namespace: namespace, Name: identityName}); err != nil {
					test.Fatal(err)
				}
			}
			suffix := uuid.NewString()[:8]
			metadata := func(name string) api.ObjectMeta {
				return api.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"kognition.dev/identity-acceptance-run": runID}}
			}
			instance := api.AgentInstance{ObjectMeta: metadata("session-" + suffix), Spec: api.AgentInstanceSpec{AgentRef: api.LocalObjectReference{Name: "worker"}}, Status: api.AgentInstanceStatus{Phase: api.PhaseRunning, SessionID: "fixture-" + suffix}}
			create("AgentInstance", instance.Name, instance)
			read("AgentInstance", instance.Name, &instance)
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				test.Fatal(err)
			}
			sessionIssuer, err := runtimeauth.NewToolSessionCapabilityIssuer(key, time.Minute)
			if err != nil {
				test.Fatal(err)
			}
			toolIssuer, err := runtimeauth.NewToolCapabilityIssuer(key, time.Minute)
			if err != nil {
				test.Fatal(err)
			}
			memoryIssuer, err := runtimeauth.NewMemoryCapabilityIssuer(key, time.Minute)
			if err != nil {
				test.Fatal(err)
			}
			var toolCalls, memoryCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/tool":
					toolCalls.Add(1)
					_, _ = io.WriteString(writer, `{"updated":true}`)
				case "/v1/memory/search":
					memoryCalls.Add(1)
					_, _ = io.WriteString(writer, `[]`)
				default:
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()
			tool := api.Tool{ObjectMeta: metadata("tool-" + suffix), Spec: api.ToolSpec{ClassName: "sandbox-openapi", Protocol: "OpenAPI", Endpoint: upstream.URL + "/tool", Metadata: map[string]string{"allowPrivateNetwork": "true"}, InputSchema: map[string]any{"type": "object"}}, Status: api.ToolStatus{Phase: api.PhaseReady}}
			create("Tool", tool.Name, tool)
			read("Tool", tool.Name, &tool)
			toolGrant := "tool-grant-" + suffix
			create("ToolGrant", toolGrant, api.ToolGrant{ObjectMeta: metadata(toolGrant), Spec: api.ToolGrantSpec{AgentRef: api.LocalObjectReference{Name: "worker"}, ToolRef: api.LocalObjectReference{Name: tool.Name}, Verbs: []api.ToolGrantVerb{"invoke"}}})
			role := "role-" + suffix
			create("AgentRole", role, api.AgentRole{ObjectMeta: metadata(role), Rules: []api.PolicyRule{{Resources: []string{"Tool"}, Verbs: []string{"get", "invoke"}}, {Resources: []string{"Memory"}, Verbs: []string{"get"}}}})
			create("AgentRoleBinding", role, api.AgentRoleBinding{ObjectMeta: metadata(role), Spec: api.AgentRoleBindingSpec{RoleRef: api.LocalObjectReference{Name: role}, Subjects: []api.Subject{{Kind: "AgentIdentity", Name: identityName}}}})
			claimName, volumeName, handle := "claim-"+suffix, "volume-"+suffix, uuid.NewString()
			create("MemoryClaim", claimName, api.MemoryClaim{ObjectMeta: metadata(claimName), Spec: api.MemoryClaimSpec{MemoryClassName: "fixture"}, Status: api.MemoryClaimStatus{Phase: api.PhaseBound, BoundVolumeName: volumeName}})
			create("MemoryVolume", volumeName, api.MemoryVolume{ObjectMeta: metadata(volumeName), Spec: api.MemoryVolumeSpec{ClaimRef: &api.LocalObjectReference{Name: claimName}, BackendHandle: handle}, Status: api.MemoryVolumeStatus{Phase: api.PhaseBound}})
			memoryGrant := "memory-grant-" + suffix
			create("MemoryGrant", memoryGrant, api.MemoryGrant{ObjectMeta: metadata(memoryGrant), Spec: api.MemoryGrantSpec{AgentRef: api.LocalObjectReference{Name: "worker"}, MemoryClaimRef: api.LocalObjectReference{Name: claimName}, AccessMode: "ReadOnly"}})
			policies := controller.NewInvocationPolicyEngine(controller.NewToolGrantEngine(storage, zap.NewNop()), controller.NewAgentPolicyReconciler(storage, opa.NewEngine(), zap.NewNop())).WithAgentRBAC(storage, controller.NewRBACEngine(storage, zap.NewNop()))
			executor := toolexec.New(nil)
			core, err := toolgateway.New(storage, controller.NewToolGatewayAuthorizer(policies), controller.NewQuotaEnforcer(storage, controller.NewCognitiveQuotaReconciler(storage, zap.NewNop()), zap.NewNop()), toolgateway.NewInvocationRunExecutor(storage, runtime.NewToolDispatcher(executor, executor)), zap.NewNop(), toolgateway.WithLedger(toolgateway.NewStoreLedger(storage)), toolgateway.WithGate3Coordinator(coordinator), toolgateway.WithGate3RequestBuilder(toolgateway.NewGate3RequestBuilder(storage)), toolgateway.WithRuntimeApprovalExecutionStore(backend), toolgateway.WithClaimant(claimant))
			if err != nil {
				test.Fatal(err)
			}
			toolServer, err := toolgateway.NewServer(storage, sessionIssuer, toolIssuer, core, zaptest.NewLogger(test))
			if err != nil {
				test.Fatal(err)
			}
			memoryServer, err := policygateway.New(storage, memoryIssuer, upstream.URL, &http.Client{Timeout: 5 * time.Second}, zap.NewNop(), policygateway.WithGate3Coordinator(coordinator), policygateway.WithGate3RequestBuilder(policygateway.NewGate3RequestBuilder()), policygateway.WithRuntimeApprovalExecutionStore(backend), policygateway.WithClaimant(claimant), policygateway.WithMemoryMutationReceiptStore(postgres.New(pool, zap.NewNop())))
			if err != nil {
				test.Fatal(err)
			}
			toolEndpoint := serveDeployedIdentityGateway(test, ctx, source, "spiffe://kognition.dev/gateway/tool", toolServer)
			memoryEndpoint := serveDeployedIdentityGateway(test, ctx, source, "spiffe://kognition.dev/gateway/memory", memoryServer)
			bindings := map[string]runtimeauth.WorkloadGatewayEndpoint{"tool": {URL: toolEndpoint, PeerID: "spiffe://kognition.dev/gateway/tool"}, "memory": {URL: memoryEndpoint, PeerID: "spiffe://kognition.dev/gateway/memory"}, "model": {URL: toolEndpoint, PeerID: "spiffe://kognition.dev/gateway/tool"}, "communication": {URL: memoryEndpoint, PeerID: "spiffe://kognition.dev/gateway/memory"}}
			proxy, err := runtimeauth.NewWorkloadProxy(source, bindings)
			if err != nil {
				test.Fatal(err)
			}
			defer proxy.Close()
			proxyServer := httptest.NewServer(proxy)
			defer proxyServer.Close()
			request := func(path, token string, payload any, want int) []byte {
				test.Helper()
				body, err := json.Marshal(payload)
				if err != nil {
					test.Fatal(err)
				}
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyServer.URL+path, bytes.NewReader(body))
				if err != nil {
					test.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("Content-Type", "application/json")
				response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
				if err != nil {
					test.Fatal(err)
				}
				defer func() { _ = response.Body.Close() }()
				output, err := io.ReadAll(response.Body)
				if err != nil {
					test.Fatal(err)
				}
				if response.StatusCode != want {
					test.Fatalf("%s status=%d want=%d body=%s", path, response.StatusCode, want, output)
				}
				return output
			}
			toolSession, err := sessionIssuer.Issue(runtimeauth.ToolSessionCapabilityBinding{Namespace: namespace, AgentName: agent.Name, AgentIdentityName: identityName, InstanceName: instance.Name, InstanceUID: instance.UID, SessionID: instance.Status.SessionID})
			if err != nil {
				test.Fatal(err)
			}
			payload := map[string]any{"tool_name": tool.Name, "tool_resource_version": tool.ResourceVersion, "invocation_id": "fixture-" + suffix, "input": map[string]any{"ticket": "42"}}
			var exchange struct {
				Capability string `json:"capability"`
			}
			if err := json.Unmarshal(request("/tool/v1/tools/capabilities", toolSession, payload, http.StatusOK), &exchange); err != nil || exchange.Capability == "" {
				test.Fatal("Tool exchange unavailable", err)
			}
			first := request("/tool/v1/tools/invoke", exchange.Capability, payload, http.StatusOK)
			second := request("/tool/v1/tools/invoke", exchange.Capability, payload, http.StatusOK)
			var initial, replay toolgateway.Response
			if err := json.Unmarshal(first, &initial); err != nil {
				test.Fatal(err)
			}
			if err := json.Unmarshal(second, &replay); err != nil {
				test.Fatal(err)
			}
			if initial.InvocationID != replay.InvocationID || !reflect.DeepEqual(initial.Output, replay.Output) || initial.ExecutionMode != replay.ExecutionMode || toolCalls.Load() != 1 {
				test.Fatalf("Tool replay mismatch or repeated effect: target-calls=%d initial=%s replay=%s", toolCalls.Load(), first, second)
			}
			memoryToken, err := memoryIssuer.Issue(runtimeauth.MemoryCapabilityBinding{Namespace: namespace, AgentName: agent.Name, AgentIdentityName: identityName, InstanceName: instance.Name, InstanceUID: instance.UID, SessionID: instance.Status.SessionID, VolumeHandle: handle})
			if err != nil {
				test.Fatal(err)
			}
			memoryPayload := map[string]any{"volume_handle": handle, "query": "fixture"}
			request("/memory/v1/memory/search", memoryToken, memoryPayload, http.StatusOK)
			wrongPeer, err := runtimeauth.NewWorkloadIdentity(ctx, runtimeauth.WorkloadIdentityConfig{SocketAddress: os.Getenv("KOGNITION_SPIFFE_ENDPOINT_SOCKET"), ExpectedID: "spiffe://kognition.dev/gateway/tool", TrustDomain: "kognition.dev"})
			if err != nil {
				test.Fatal(err)
			}
			defer func() { _ = wrongPeer.Close() }()
			for _, denial := range []struct {
				endpoint, peer, path, token string
				payload                     any
			}{{toolEndpoint, "spiffe://kognition.dev/gateway/tool", "/v1/tools/capabilities", toolSession, payload}, {memoryEndpoint, "spiffe://kognition.dev/gateway/memory", "/v1/memory/search", memoryToken, memoryPayload}} {
				client, err := wrongPeer.HTTPClient(denial.peer)
				if err != nil {
					test.Fatal(err)
				}
				client.Timeout = 5 * time.Second
				body, err := json.Marshal(denial.payload)
				if err != nil {
					test.Fatal(err)
				}
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, denial.endpoint+denial.path, bytes.NewReader(body))
				if err != nil {
					test.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+denial.token)
				request.Header.Set("Content-Type", "application/json")
				response, err := client.Do(request)
				if err != nil {
					test.Fatal(err)
				}
				_ = response.Body.Close()
				if response.StatusCode != http.StatusForbidden {
					test.Fatalf("valid non-Agent SVID authorized %s: status=%d", denial.path, response.StatusCode)
				}
			}
			if toolCalls.Load() != 1 || memoryCalls.Load() != 1 {
				test.Fatal("wrong workload peer reached a fixture target")
			}
			if err := storage.Delete(ctx, "ToolGrant", namespace, toolGrant); err != nil {
				test.Fatal(err)
			}
			if err := storage.Delete(ctx, "MemoryGrant", namespace, memoryGrant); err != nil {
				test.Fatal(err)
			}
			request("/tool/v1/tools/invoke", exchange.Capability, payload, http.StatusForbidden)
			request("/memory/v1/memory/search", memoryToken, memoryPayload, http.StatusForbidden)
			if toolCalls.Load() != 1 || memoryCalls.Load() != 1 {
				test.Fatal("revocation reached a fixture target")
			}
			test.Logf("mode=%s UID=%s real-SPIRE Tool exchange/execution/replay and Memory search passed; wrong workload peer and revoked grants denied before targets", mode, agent.UID)
		})
	}
}

func serveDeployedIdentityGateway(test *testing.T, ctx context.Context, source *runtimeauth.WorkloadIdentity, peer string, handler http.Handler) string {
	test.Helper()
	enrollment, stop := context.WithTimeout(ctx, 45*time.Second)
	defer stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		verified, err := runtimeauth.NewWorkloadIdentity(ctx, runtimeauth.WorkloadIdentityConfig{SocketAddress: os.Getenv("KOGNITION_SPIFFE_ENDPOINT_SOCKET"), ExpectedID: peer, TrustDomain: "kognition.dev"})
		if err == nil {
			_ = verified.Close()
			break
		}
		select {
		case <-enrollment.Done():
			test.Fatal("gateway SVID enrollment unavailable", err)
		case <-ticker.C:
		}
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		test.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	test.Setenv("KOGNITION_WORKLOAD_IDENTITY_MODE", "spire")
	test.Setenv("KOGNITION_SPIFFE_ID", peer)
	test.Setenv("KOGNITION_SPIFFE_TRUST_DOMAIN", "kognition.dev")
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- runtimeauth.ServeGatewayHTTP(ctx, server) }()
	test.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			test.Error(err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				test.Error(err)
			}
		case <-shutdown.Done():
			test.Error("gateway did not stop")
		}
	})
	client, err := source.HTTPClient(peer)
	if err != nil {
		test.Fatal(err)
	}
	client.Timeout = time.Second
	endpoint := "https://" + address
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/healthz", nil)
		if err != nil {
			test.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			break
		}
		select {
		case err := <-done:
			done <- err
			test.Fatal("SPIRE gateway startup failed", err)
		case <-ctx.Done():
			test.Fatal("SPIRE gateway did not start")
		case <-ticker.C:
		}
	}
	return endpoint
}
