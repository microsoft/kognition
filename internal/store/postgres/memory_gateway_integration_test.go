// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/gatewaybackend"
	"github.com/kognition-ai/kognition/internal/memory/pgvector"
	"github.com/kognition-ai/kognition/internal/memory/policygateway"
	"github.com/kognition-ai/kognition/internal/memory/service"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestStandaloneMemoryMutationReplayAcrossRestart(t *testing.T) {
	databaseURL := os.Getenv("KOGNITION_TEST_MEMORY_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("requires the disposable pgvector database from tests/standalone-migrations.ps1 -VerifyMemory")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	logger := zaptest.NewLogger(t)
	config := gatewaybackend.Config{Mode: "standalone", DatabaseURL: databaseURL, Component: "memory-test"}
	backend, err := gatewaybackend.Open(ctx, config, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if backend != nil {
			backend.Close()
		}
	}()
	var databaseName string
	if err := backend.PostgresPool.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); err != nil || !strings.HasPrefix(databaseName, "migration_check_") {
		t.Fatal("Memory integration requires an isolated migration-check database")
	}
	workspace := "memory-" + uuid.NewString()
	create := func(kind, namespace, name string, resource any) {
		t.Helper()
		data, err := json.Marshal(resource)
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.Store.Create(ctx, kind, namespace, name, data); err != nil {
			t.Fatal(err)
		}
	}
	read := func(kind, name string, resource any) {
		t.Helper()
		data, err := backend.Store.Get(ctx, kind, workspace, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, resource); err != nil {
			t.Fatal(err)
		}
	}
	reconcile := func(reconciler controller.Reconciler, name string) {
		t.Helper()
		if _, err := reconciler.Reconcile(ctx, controller.Request{Namespace: workspace, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	className := "class-" + uuid.NewString()
	create("AutonomyClass", "system", "supervised", v1alpha1.AutonomyClass{ObjectMeta: v1alpha1.ObjectMeta{Name: "supervised"}, Spec: v1alpha1.AutonomyClassSpec{ApprovalPolicy: &v1alpha1.ApprovalPolicy{MemoryWrite: "never", ToolInvocation: "always", ExternalCommunication: "always"}}})
	create("ModelClass", "", className, v1alpha1.ModelClass{ObjectMeta: v1alpha1.ObjectMeta{Name: className}, Spec: v1alpha1.ModelClassSpec{Provisioner: "warm-session-test"}})
	create("Model", workspace, "model", v1alpha1.Model{ObjectMeta: v1alpha1.ObjectMeta{Name: "model", Namespace: workspace}, Spec: v1alpha1.ModelSpec{ClassName: className, ModelID: "controlled-model"}})
	create("Agent", workspace, "source", v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "source", Namespace: workspace}, Spec: v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "model"}}})
	agents := controller.NewAgentReconciler(backend.Store, logger)
	reconcile(agents, "source")
	reconcile(agents, "source")
	instances, err := backend.Store.List(ctx, "AgentInstance", store.ListOptions{Namespace: workspace})
	if err != nil || len(instances) != 1 {
		t.Fatalf("controller instances: count=%d error=%v", len(instances), err)
	}
	var instance v1alpha1.AgentInstance
	if err := json.Unmarshal(instances[0], &instance); err != nil {
		t.Fatal(err)
	}
	registry := cpi.NewRegistry()
	driver := &warmSessionDriver{}
	registry.MustRegister(driver)
	instanceController := controller.NewAgentInstanceReconcilerWithDispatcher(backend.Store, controller.NewCPIDispatcher(backend.Store, registry, logger), logger)
	instanceController.SetRequireAutomaticIdentity(true)
	for attempt := 0; attempt < 3; attempt++ {
		reconcile(instanceController, instance.ObjectMeta.Name)
	}
	identities := controller.NewAgentIdentityReconciler(backend.Store, logger)
	reconcile(identities, "agent-identity-source")
	reconcile(identities, "agent-identity-source")
	reconcile(instanceController, instance.ObjectMeta.Name)
	read("AgentInstance", instance.ObjectMeta.Name, &instance)
	if instance.Status.Phase != v1alpha1.PhaseRunning || instance.ObjectMeta.UID == "" || instance.Status.SessionID == "" || driver.calls != 0 {
		t.Fatal("controller did not establish a real warm Running session")
	}
	memoryClassName := "memory-class-" + uuid.NewString()
	create("MemoryClass", "", memoryClassName, v1alpha1.MemoryClass{
		ObjectMeta: v1alpha1.ObjectMeta{Name: memoryClassName},
		Spec:       v1alpha1.MemoryClassSpec{BackendType: "pgvector"},
	})
	create("MemoryClaim", workspace, "memory", v1alpha1.MemoryClaim{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "memory", Namespace: workspace},
		Spec:       v1alpha1.MemoryClaimSpec{MemoryClassName: memoryClassName, Type: v1alpha1.MemoryTypeEpisodic},
	})
	claims := controller.NewMemoryClaimReconciler(backend.Store, logger,
		controller.WithMemoryBackendProvisioner(pgvector.New(backend.PostgresPool, logger)))
	var claim v1alpha1.MemoryClaim
	for range 4 {
		reconcile(claims, "memory")
		read("MemoryClaim", "memory", &claim)
		if claim.Status.Phase == v1alpha1.PhaseBound {
			break
		}
	}
	if claim.Status.Phase != v1alpha1.PhaseBound || claim.Status.BoundVolumeName == "" {
		t.Fatal("controller did not bind the actual MemoryClaim")
	}
	var volume v1alpha1.MemoryVolume
	read("MemoryVolume", claim.Status.BoundVolumeName, &volume)
	volumeHandle := volume.Spec.BackendHandle
	if !strings.HasPrefix(volumeHandle, "pgvector-") {
		t.Fatal("memory verification did not exercise a controller-generated opaque backend handle")
	}
	create("MemoryGrant", workspace, "writer", v1alpha1.MemoryGrant{ObjectMeta: v1alpha1.ObjectMeta{Name: "writer", Namespace: workspace}, Spec: v1alpha1.MemoryGrantSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "source"}, MemoryClaimRef: v1alpha1.LocalObjectReference{Name: "memory"}, AccessMode: "ReadWrite"}})
	create("AgentRole", workspace, "writer", v1alpha1.AgentRole{ObjectMeta: v1alpha1.ObjectMeta{Name: "writer", Namespace: workspace}, Rules: []v1alpha1.PolicyRule{{Resources: []string{"Memory"}, Verbs: []string{"get", "update"}}}})
	create("AgentRoleBinding", workspace, "writer", v1alpha1.AgentRoleBinding{ObjectMeta: v1alpha1.ObjectMeta{Name: "writer", Namespace: workspace}, Spec: v1alpha1.AgentRoleBindingSpec{RoleRef: v1alpha1.LocalObjectReference{Name: "writer"}, Subjects: []v1alpha1.Subject{{Kind: "AgentIdentity", Name: "agent-identity-source"}}}})
	capabilityKey := make([]byte, 32)
	bindingKey := make([]byte, 32)
	if _, err := rand.Read(capabilityKey); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(bindingKey); err != nil {
		t.Fatal(err)
	}
	issuer, err := runtimeauth.NewMemoryCapabilityIssuer(capabilityKey, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := issuer.Issue(runtimeauth.MemoryCapabilityBinding{Namespace: workspace, AgentName: "source", AgentIdentityName: "agent-identity-source", InstanceName: instance.ObjectMeta.Name, InstanceUID: instance.ObjectMeta.UID, SessionID: instance.Status.SessionID, VolumeHandle: volumeHandle})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := runtimeapproval.NewStaticBindingKeyStore([]runtimeapproval.BindingKey{{Version: 1, Epoch: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: bindingKey}})
	if err != nil {
		t.Fatal(err)
	}
	var mutations atomic.Int32
	var rawServer *httptest.Server
	defer func() {
		if rawServer != nil {
			rawServer.Close()
		}
	}()
	internalToken := uuid.NewString()
	newGateway := func() *policygateway.Server {
		t.Helper()
		if err := backend.Store.ActivateRuntimeApprovalBindingKeys(ctx, keys); err != nil {
			t.Fatal(err)
		}
		audit := observability.NewCognitiveAuditLog(backend.Audit)
		handler := service.NewHandler(pgvector.New(backend.PostgresPool, logger), logger, service.WithInternalToken(internalToken), service.WithAuditLog(audit))
		mux := http.NewServeMux()
		handler.RegisterRoutes(mux)
		rawServer = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodPost && request.URL.Path == "/v1/memory/store" {
				mutations.Add(1)
			}
			mux.ServeHTTP(writer, request)
		}))
		coordinator, err := runtimeapproval.NewGate3Coordinator(runtimeapproval.Gate3Config{Store: backend.Store, Resolver: runtimeapproval.NewResolver(backend.Store), Keys: keys, TTL: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		receipts, ok := backend.Store.(store.MemoryMutationReceiptStore)
		if !ok {
			t.Fatal("standalone backend has no durable Memory receipts")
		}
		gateway, err := policygateway.New(backend.Store, issuer, rawServer.URL, rawServer.Client(), logger,
			policygateway.WithUpstreamToken(internalToken), policygateway.WithAuditLog(audit),
			policygateway.WithGate3Coordinator(coordinator), policygateway.WithGate3RequestBuilder(policygateway.NewGate3RequestBuilder()),
			policygateway.WithRuntimeApprovalExecutionStore(backend.Store), policygateway.WithClaimant(backend.Claimant), policygateway.WithMemoryMutationReceiptStore(receipts))
		if err != nil {
			t.Fatal(err)
		}
		return gateway
	}
	gateway := newGateway()
	invoke := func(invocationID, handle, content string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(service.StoreMemoryRequest{VolumeHandle: handle, Content: content})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewReader(body)).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+capability)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(policygateway.MemoryInvocationIDHeader, invocationID)
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, request)
		return response
	}
	first := invoke("write-1", volumeHandle, "retained Memory")
	if first.Code != http.StatusCreated {
		t.Fatalf("authorized mutation: status=%d body=%s", first.Code, first.Body.String())
	}
	var original service.StoreMemoryResponse
	if err := json.Unmarshal(first.Body.Bytes(), &original); err != nil || original.ID == "" {
		t.Fatal("mutation returned no record ID")
	}
	rawServer.Close()
	backend.Close()
	backend, err = gatewaybackend.Open(ctx, config, logger)
	if err != nil {
		t.Fatal(err)
	}
	gateway = newGateway()
	replayed := invoke("write-1", volumeHandle, "retained Memory")
	if replayed.Code != http.StatusCreated || !bytes.Equal(first.Body.Bytes(), replayed.Body.Bytes()) || first.Header().Get("Content-Type") != replayed.Header().Get("Content-Type") || mutations.Load() != 1 {
		t.Fatalf("restart replay: status=%d mutations=%d body=%s", replayed.Code, mutations.Load(), replayed.Body.String())
	}
	if response := invoke("write-1", volumeHandle, "different content"); response.Code < 400 || mutations.Load() != 1 {
		t.Fatalf("changed-input replay: status=%d mutations=%d", response.Code, mutations.Load())
	}
	if response := invoke("other-volume", uuid.NewString(), "denied"); response.Code != http.StatusForbidden || mutations.Load() != 1 {
		t.Fatalf("cross-volume mutation: status=%d mutations=%d", response.Code, mutations.Load())
	}
	if err := backend.Store.Delete(ctx, "MemoryGrant", workspace, "writer"); err != nil {
		t.Fatal(err)
	}
	if response := invoke("revoked", volumeHandle, "denied"); response.Code != http.StatusForbidden || mutations.Load() != 1 {
		t.Fatalf("revoked mutation: status=%d mutations=%d", response.Code, mutations.Load())
	}
	if response := invoke("write-1", volumeHandle, "retained Memory"); response.Code != http.StatusForbidden || mutations.Load() != 1 {
		t.Fatalf("revoked replay: status=%d mutations=%d", response.Code, mutations.Load())
	}
	var count int
	var storedContent string
	if err := backend.PostgresPool.QueryRow(ctx, "SELECT count(*), min(content) FROM memory_vectors WHERE volume_id = $1", volumeHandle).Scan(&count, &storedContent); err != nil || count != 1 || storedContent != "retained Memory" {
		t.Fatalf("durable records: count=%d error=%v", count, err)
	}
}
