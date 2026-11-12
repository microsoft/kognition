//go:build integration

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	k8scontroller "github.com/kognition-ai/kognition/internal/k8s/controller"
	k8swebhook "github.com/kognition-ai/kognition/internal/k8s/webhook"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	pgstore "github.com/kognition-ai/kognition/internal/store/postgres"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const authorityMemoryHandle = "authority-memory"

func TestGatewayAuthorityProductionExecution(test *testing.T) {
	fixture := newGatewayAuthorityFixtureWithTimeout(test, 12*time.Minute)
	deployment := fixture.installProductionAssembly(test, true)
	if !test.Run("webhook-audit-boundary", func(test *testing.T) {
		verifyResourceWebhookAuditConfigMapBoundary(test, fixture.context, fixture.objects, fixture.config, fixture.objects.Scheme())
	}) {
		return
	}
	if !test.Run("gateway-state-boundary", func(test *testing.T) {
		verifyGatewayConfigMapBoundary(test, fixture.context, fixture.objects, fixture.config, fixture.objects.Scheme(), deployment.migrations)
	}) {
		return
	}
	started := map[string][]corev1.Pod{}
	for _, component := range []string{"tool-gateway", "memory-policy-gateway", "agent-communication-gateway"} {
		started[component] = fixture.productionPods(test, component, 2)
		for _, pod := range started[component] {
			test.Logf("Actual %s process Pod=%s UID=%s containerID=%s imageID=%s", component, pod.Name, pod.UID, pod.Status.ContainerStatuses[0].ContainerID, pod.Status.ContainerStatuses[0].ImageID)
		}
	}
	if !test.Run("tool", func(test *testing.T) { runGatewayAuthorityProductionTool(test, fixture, deployment) }) {
		return
	}
	fixture.seedProductionMemory(test)
	for _, operation := range []string{"store", "delete"} {
		if !test.Run("memory-"+operation, func(test *testing.T) {
			fixture.runProductionMemory(test, deployment, operation)
		}) {
			return
		}
	}
	if !test.Run("communication", func(test *testing.T) { fixture.runProductionCommunication(test, deployment) }) {
		return
	}
	objects, err := client.New(fixture.config, client.Options{Scheme: k8scontroller.SetupScheme()})
	if err != nil {
		test.Fatal(err)
	}
	var approvals k8sapi.RuntimeApprovalList
	if err := objects.List(fixture.context, &approvals, client.InNamespace("tenant")); err != nil {
		test.Fatal(err)
	}
	if len(approvals.Items) != 4 {
		test.Fatalf("expected four terminal production approvals, got %d", len(approvals.Items))
	}
	components := map[string]string{"Tool": "tool-gateway", "Memory": "memory-policy-gateway", "Communication": "agent-communication-gateway"}
	counts := map[string]int{}
	for _, approval := range approvals.Items {
		plane := string(approval.Spec.Plane)
		component, found := components[plane]
		if !found || approval.Status.ExecutionState != "Completed" || approval.Status.FencingToken < runtimeapproval.MinFencingToken || approval.Status.Claimant == nil {
			test.Fatalf("production approval has no exact terminal owner/fence: %s", approval.Name)
		}
		matched := false
		for _, pod := range fixture.productionPods(test, component, 2) {
			for _, initial := range started[component] {
				if pod.UID == initial.UID && pod.Status.ContainerStatuses[0].ContainerID == initial.Status.ContainerStatuses[0].ContainerID &&
					approval.Status.Claimant.WorkloadUID == string(pod.UID) && approval.Status.Claimant.ContainerID == pod.Status.ContainerStatuses[0].ContainerID {
					matched = true
				}
			}
		}
		if !matched {
			test.Fatal("production claim has no unchanged actual gateway owner", approval.Name)
		}
		counts[plane]++
		test.Logf("Production %s receipt approval=%s UID=%s claimantPodUID=%s fence=%d state=%s", plane, approval.Name, approval.UID, approval.Status.Claimant.WorkloadUID, approval.Status.FencingToken, approval.Status.ExecutionState)
	}
	if counts["Tool"] != 1 || counts["Memory"] != 2 || counts["Communication"] != 1 || deployment.effects() != (gatewayAuthorityEffects{Tool: 1, MemoryStore: 1, MemoryDelete: 1, Communication: 1}) {
		test.Fatalf("production approval/effect inventory mismatch: approvals=%v effects=%+v", counts, deployment.effects())
	}
	test.Logf("All deployed workflow and replica races passed; exact non-idempotent effects=%+v", deployment.effects())
}

func seedGatewayAuthorityResource(test *testing.T, ctx context.Context, storage store.Store, kind, namespace, name string, value any) {
	test.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		test.Fatal(err)
	}
	object["kind"], object["apiVersion"] = kind, api.APIVersion
	body, err = json.Marshal(object)
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.Create(ctx, kind, namespace, name, body); err != nil {
		test.Fatal(err)
	}
	if status, exists := object["status"]; exists {
		current, err := storage.Get(ctx, kind, namespace, name)
		if err != nil {
			test.Fatal(err)
		}
		var stored map[string]any
		if err := json.Unmarshal(current, &stored); err != nil {
			test.Fatal(err)
		}
		encoded, err := json.Marshal(stored["metadata"])
		if err != nil {
			test.Fatal(err)
		}
		var metadata api.ObjectMeta
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

func (fixture *gatewayAuthorityFixture) productionStore(test *testing.T) *k8scontroller.StoreAdapter {
	test.Helper()
	objects, err := client.New(fixture.config, client.Options{Scheme: k8scontroller.SetupScheme()})
	if err != nil {
		test.Fatal(err)
	}
	return k8scontroller.NewStoreAdapterWithControlNamespace(objects, "control")
}

func (fixture *gatewayAuthorityFixture) seedProductionMemory(test *testing.T) {
	test.Helper()
	storage := fixture.productionStore(test)
	seed := func(kind, name string, value any) {
		seedGatewayAuthorityResource(test, fixture.context, storage, kind, "tenant", name, value)
	}
	seedGatewayAuthorityResource(test, fixture.context, storage, "MemoryClass", "", "authority-memory-class", api.MemoryClass{
		ObjectMeta: api.ObjectMeta{Name: "authority-memory-class"}, Spec: api.MemoryClassSpec{BackendType: "in-memory"},
	})
	metadata := func(name string) api.ObjectMeta { return api.ObjectMeta{Name: name, Namespace: "tenant"} }
	seed("MemoryClaim", "authority-memory-claim", api.MemoryClaim{ObjectMeta: metadata("authority-memory-claim"),
		Spec:   api.MemoryClaimSpec{MemoryClassName: "authority-memory-class", Type: "semantic", AccessModes: []string{"ReadWrite"}},
		Status: api.MemoryClaimStatus{Phase: api.PhaseBound, BoundVolumeName: "authority-memory-volume"},
	})
	seed("MemoryVolume", "authority-memory-volume", api.MemoryVolume{ObjectMeta: metadata("authority-memory-volume"),
		Spec:   api.MemoryVolumeSpec{MemoryClassName: "authority-memory-class", ClaimRef: &api.LocalObjectReference{Name: "authority-memory-claim"}, BackendHandle: authorityMemoryHandle},
		Status: api.MemoryVolumeStatus{Phase: api.PhaseBound},
	})
	seed("MemoryGrant", "authority-memory-grant", api.MemoryGrant{ObjectMeta: metadata("authority-memory-grant"),
		Spec: api.MemoryGrantSpec{AgentRef: api.LocalObjectReference{Name: "authority-http-agent"}, MemoryClaimRef: api.LocalObjectReference{Name: "authority-memory-claim"}, AccessMode: "ReadWrite"},
	})
	seed("AgentRole", "authority-memory-role", api.AgentRole{ObjectMeta: metadata("authority-memory-role"), Rules: []api.PolicyRule{{Resources: []string{"Memory"}, Verbs: []string{"get", "update"}}}})
	seed("AgentRoleBinding", "authority-memory-role", api.AgentRoleBinding{ObjectMeta: metadata("authority-memory-role"),
		Spec: api.AgentRoleBindingSpec{RoleRef: api.LocalObjectReference{Name: "authority-memory-role"}, Subjects: []api.Subject{{Kind: "AgentIdentity", Name: "agent-identity-authority-http-agent"}}},
	})
}

type gatewayAuthorityHTTPResult struct {
	status int
	body   []byte
	err    error
}

func gatewayAuthorityRequest(ctx context.Context, httpClient *http.Client, method, endpoint, path, token, invocation string, payload []byte) gatewayAuthorityHTTPResult {
	request, err := http.NewRequestWithContext(ctx, method, endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return gatewayAuthorityHTTPResult{err: err}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	if invocation != "" {
		request.Header.Set("X-Kognition-Memory-Invocation-Id", invocation)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return gatewayAuthorityHTTPResult{err: err}
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	return gatewayAuthorityHTTPResult{status: response.StatusCode, body: body, err: err}
}

func requireAuthorityResponse(test *testing.T, result gatewayAuthorityHTTPResult, status int, fields map[string]any) {
	test.Helper()
	if result.err != nil || result.status != status {
		test.Fatalf("production HTTP status=%d want=%d body=%s err=%v", result.status, status, result.body, result.err)
	}
	if len(fields) == 0 {
		return
	}
	var body map[string]any
	if err := json.Unmarshal(result.body, &body); err != nil {
		test.Fatal("invalid production response body", err)
	}
	for name, expected := range fields {
		if body[name] != expected {
			test.Fatalf("production response field %s=%v want=%v", name, body[name], expected)
		}
	}
}

func raceAuthorityRequests(test *testing.T, endpoints []string, invoke func(string) gatewayAuthorityHTTPResult, status int, fields map[string]any) {
	test.Helper()
	if len(endpoints) != 2 || endpoints[0] == endpoints[1] {
		test.Fatal("two distinct deployed replica endpoints are required")
	}
	start := make(chan struct{})
	results := make(chan gatewayAuthorityHTTPResult, len(endpoints))
	var group sync.WaitGroup
	for _, endpoint := range endpoints {
		group.Add(1)
		go func(endpoint string) {
			defer group.Done()
			<-start
			results <- invoke(endpoint)
		}(endpoint)
	}
	close(start)
	group.Wait()
	close(results)
	successes := 0
	for result := range results {
		if result.err != nil {
			test.Fatal(result.err)
		}
		if result.status == status {
			successes++
			requireAuthorityResponse(test, result, status, fields)
		} else if result.status != http.StatusConflict && result.status != http.StatusServiceUnavailable {
			test.Fatalf("unexpected production race status=%d body=%s", result.status, result.body)
		}
	}
	if successes == 0 {
		test.Fatal("neither deployed replica completed the approved workflow")
	}
	for _, endpoint := range endpoints {
		requireAuthorityResponse(test, invoke(endpoint), status, fields)
	}
}

func (fixture *gatewayAuthorityFixture) approveProductionInvocation(test *testing.T, deployment *gatewayAuthorityDeployment, invocation, component string) {
	test.Helper()
	body, err := fixture.productionStore(test).Get(fixture.context, "RuntimeApproval", "tenant", runtimeapproval.CanonicalName("tenant", invocation))
	if err != nil {
		test.Fatal(err)
	}
	var approval api.RuntimeApproval
	if err := json.Unmarshal(body, &approval); err != nil {
		test.Fatal(err)
	}
	if approval.Status.DecisionState != api.RuntimeApprovalDecisionPending || approval.Status.Claimant != nil {
		test.Fatal("production workflow did not park without an execution owner")
	}
	payload, err := json.Marshal(map[string]any{"namespace": "tenant", "name": approval.Name, "invocationID": invocation, "decision": "approve", "expectedResourceVersion": approval.ResourceVersion})
	if err != nil {
		test.Fatal(err)
	}
	pods := fixture.productionPods(test, component, 2)
	credential := fixture.boundToken(test, &pods[0])
	decide := func(token string) gatewayAuthorityHTTPResult {
		return gatewayAuthorityRequest(fixture.context, deployment.decisionClient, http.MethodPost, deployment.decisionURL, k8swebhook.RuntimeApprovalDecisionPath, token, "", payload)
	}
	requireAuthorityResponse(test, decide(credential.BearerToken), http.StatusForbidden, nil)
	requireAuthorityResponse(test, decide(fixture.credentials["authority-operator"].BearerToken), http.StatusOK, nil)
}

func (fixture *gatewayAuthorityFixture) runProductionMemory(test *testing.T, deployment *gatewayAuthorityDeployment, operation string) {
	test.Helper()
	var instance api.AgentInstance
	body, err := fixture.productionStore(test).Get(fixture.context, "AgentInstance", "tenant", "authority-http-instance")
	if err != nil {
		test.Fatal(err)
	}
	if err := json.Unmarshal(body, &instance); err != nil {
		test.Fatal(err)
	}
	issuer, err := runtimeauth.NewMemoryCapabilityIssuer(deployment.memoryKey, time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	token, err := issuer.Issue(runtimeauth.MemoryCapabilityBinding{Namespace: "tenant", AgentName: "authority-http-agent", AgentIdentityName: "agent-identity-authority-http-agent", InstanceName: instance.Name, InstanceUID: instance.UID, SessionID: instance.Status.SessionID, VolumeHandle: authorityMemoryHandle})
	if err != nil {
		test.Fatal(err)
	}
	endpoints := []string{}
	for _, pod := range fixture.productionPods(test, "memory-policy-gateway", 2) {
		endpoints = append(endpoints, "http://"+net.JoinHostPort(pod.Status.PodIP, "8091"))
	}
	invocation := "authority-memory-" + operation + "-" + uuid.NewString()
	method, path, status := http.MethodPost, "/v1/memory/store", http.StatusCreated
	payload := []byte(`{"volume_handle":"` + authorityMemoryHandle + `","content":"owned acceptance memory"}`)
	fields := map[string]any{"id": "authority-record-1", "content": "owned acceptance memory"}
	before := deployment.effects()
	if operation == "delete" {
		method, path, status = http.MethodDelete, "/v1/memory/delete/"+authorityMemoryHandle+"/authority-record-1", http.StatusOK
		payload, fields = nil, map[string]any{"deleted": true}
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	invoke := func(endpoint string) gatewayAuthorityHTTPResult {
		return gatewayAuthorityRequest(fixture.context, httpClient, method, endpoint, path, token, invocation, payload)
	}
	requireAuthorityResponse(test, invoke(endpoints[0]), http.StatusAccepted, nil)
	if deployment.effects() != before {
		test.Fatal("pending Memory workflow reached the provider")
	}
	fixture.approveProductionInvocation(test, deployment, invocation, "memory-policy-gateway")
	raceAuthorityRequests(test, endpoints, invoke, status, fields)
	expected := before
	if operation == "store" {
		expected.MemoryStore++
	} else {
		expected.MemoryDelete++
	}
	if deployment.effects() != expected {
		test.Fatalf("duplicate or missing Memory %s effect: %+v", operation, deployment.effects())
	}
	denied := gatewayAuthorityRequest(fixture.context, httpClient, http.MethodPost, endpoints[1], "/v1/memory/store", token, invocation, []byte(`{"volume_handle":"ungranted-volume","content":"owned acceptance memory"}`))
	requireAuthorityResponse(test, denied, http.StatusForbidden, nil)
	if deployment.effects() != expected {
		test.Fatal("unauthorized Memory volume reached provider")
	}
	pool, err := pgxpool.New(fixture.context, deployment.databaseURL)
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	receipt, found, err := pgstore.New(pool, zap.NewNop()).GetMemoryMutation(fixture.context, "tenant", invocation)
	if err != nil || !found || receipt.Status != store.MemoryMutationSucceeded || receipt.Operation != operation || receipt.CompletedAt == nil || receipt.PreparationToken != "" || receipt.PreparationDeadline != nil {
		test.Fatalf("Memory %s has no durable terminal PostgreSQL receipt: found=%v err=%v", operation, found, err)
	}
	test.Logf("Retained PostgreSQL Memory receipt namespace=%s invocation=%s binding=%s operation=%s status=%s completed=%s resultSHA256=%x", receipt.Namespace, receipt.InvocationID, receipt.Binding, receipt.Operation, receipt.Status, receipt.CompletedAt.UTC().Format(time.RFC3339Nano), sha256.Sum256(receipt.Result))
	test.Logf("Production Memory %s invocation=%s: pending denial, human decision, two replica race, both durable replays, wrong-volume denial and PostgreSQL receipt passed; effects=%+v", operation, invocation, deployment.effects())
}

func (fixture *gatewayAuthorityFixture) runProductionCommunication(test *testing.T, deployment *gatewayAuthorityDeployment) {
	test.Helper()
	storage := fixture.productionStore(test)
	seed := func(kind, name string, value any) {
		seedGatewayAuthorityResource(test, fixture.context, storage, kind, "outside", name, value)
	}
	metadata := func(name string) api.ObjectMeta { return api.ObjectMeta{Name: name, Namespace: "outside"} }
	seed("Agent", "authority-communication-target", api.Agent{ObjectMeta: metadata("authority-communication-target"), Spec: api.AgentSpec{ModelRef: api.LocalObjectReference{Name: "no-inference-fixture"}, Instructions: api.AgentInstructions{Inline: "synthetic communication target"}}})
	seed("AgentService", "authority-communication", api.AgentService{ObjectMeta: metadata("authority-communication"),
		Spec:   api.AgentServiceSpec{AgentRef: api.LocalObjectReference{Name: "authority-communication-target"}, Port: 8080},
		Status: api.AgentServiceStatus{Phase: api.PhaseReady, ReadyInstances: 1, Endpoint: "http://untrusted.invalid/invoke"},
	})
	digest := sha256.Sum256([]byte("outside\x00authority-communication"))
	serviceName := fmt.Sprintf("outside-authority-communication-%x", digest[:4])
	fixture.create(test, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: "tenant"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "authority-effect-provider.control.svc.cluster.local", Ports: []corev1.ServicePort{{Port: 8080}}}})
	body, err := storage.Get(fixture.context, "AgentInstance", "tenant", "authority-http-instance")
	if err != nil {
		test.Fatal(err)
	}
	var instance api.AgentInstance
	if err := json.Unmarshal(body, &instance); err != nil {
		test.Fatal(err)
	}
	issuer, err := runtimeauth.NewAgentDelegationSessionCapabilityIssuer(deployment.communicationKey, time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	session, err := issuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{Namespace: "tenant", AgentName: "authority-http-agent", AgentIdentityName: "agent-identity-authority-http-agent", InstanceName: instance.Name, InstanceUID: instance.UID, SessionID: instance.Status.SessionID})
	if err != nil {
		test.Fatal(err)
	}
	invocation := "authority-communication-" + uuid.NewString()
	payload, err := json.Marshal(map[string]any{"target_namespace": "outside", "target_agent_service": "authority-communication", "invocation_id": invocation, "message": "owned acceptance communication"})
	if err != nil {
		test.Fatal(err)
	}
	endpoints := []string{}
	for _, pod := range fixture.productionPods(test, "agent-communication-gateway", 2) {
		endpoints = append(endpoints, "http://"+net.JoinHostPort(pod.Status.PodIP, "8094"))
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	before := deployment.effects()
	exchange := func(endpoint string) gatewayAuthorityHTTPResult {
		return gatewayAuthorityRequest(fixture.context, httpClient, http.MethodPost, endpoint, "/v1/agent-services/capabilities", session, "", payload)
	}
	requireAuthorityResponse(test, exchange(endpoints[0]), http.StatusForbidden, nil)
	seed("CrossNamespaceGrant", "authority-communication", api.CrossNamespaceGrant{ObjectMeta: metadata("authority-communication"),
		Spec: api.CrossNamespaceGrantSpec{From: "outside", To: "tenant", Resources: []api.GrantResource{{Kind: "Agent", Name: "authority-communication-target"}}},
	})
	seed("AgentCommunicationPolicy", "authority-communication", api.AgentCommunicationPolicy{ObjectMeta: metadata("authority-communication"),
		Spec: api.AgentCommunicationPolicySpec{DefaultDeny: true, AllowFrom: []api.LocalObjectReference{{Name: "authority-http-agent"}}, AllowTo: []api.LocalObjectReference{{Name: "authority-communication-target"}}},
	})
	requireAuthorityResponse(test, exchange(endpoints[0]), http.StatusAccepted, map[string]any{"invocation_id": invocation, "runtime_approval_state": "pending"})
	if deployment.effects() != before {
		test.Fatal("ungranted or pending capability exchange reached provider")
	}
	fixture.approveProductionInvocation(test, deployment, invocation, "agent-communication-gateway")
	start := make(chan struct{})
	results := make(chan gatewayAuthorityHTTPResult, len(endpoints))
	var group sync.WaitGroup
	for _, endpoint := range endpoints {
		group.Add(1)
		go func(endpoint string) {
			defer group.Done()
			<-start
			results <- exchange(endpoint)
		}(endpoint)
	}
	close(start)
	group.Wait()
	close(results)
	capability := ""
	issued := 0
	for result := range results {
		if result.err != nil {
			test.Fatal(result.err)
		}
		switch result.status {
		case http.StatusOK:
			var body struct {
				Capability string `json:"capability"`
			}
			if err := json.Unmarshal(result.body, &body); err != nil || body.Capability == "" {
				test.Fatal("Communication exact capability unavailable", err)
			}
			capability = body.Capability
			issued++
		case http.StatusConflict, http.StatusServiceUnavailable:
		default:
			test.Fatalf("unexpected capability issuance race status=%d", result.status)
		}
	}
	if issued != 1 {
		test.Fatalf("expected exactly one protected capability issuance, got %d", issued)
	}
	exactIssuer, err := runtimeauth.NewAgentDelegationCapabilityIssuer(deployment.communicationKey, time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	claims, err := exactIssuer.VerifyClaims(capability)
	if err != nil || claims.Namespace != "tenant" || claims.TargetNamespace != "outside" || claims.TargetAgentService != "authority-communication" || claims.InvocationID != invocation {
		test.Fatal("issued capability lost its exact source/target/invocation binding", err)
	}
	for _, endpoint := range endpoints {
		result := exchange(endpoint)
		if result.err != nil || result.status != http.StatusConflict && result.status != http.StatusServiceUnavailable {
			test.Fatalf("completed capability issuance replay status=%d err=%v", result.status, result.err)
		}
	}
	retained, err := storage.Get(fixture.context, "RuntimeApproval", "tenant", runtimeapproval.CanonicalName("tenant", invocation))
	if err != nil {
		test.Fatal(err)
	}
	var approval api.RuntimeApproval
	if err := json.Unmarshal(retained, &approval); err != nil {
		test.Fatal(err)
	}
	if approval.Spec.Operation != "issue-capability" || approval.Spec.TargetRef.Namespace != "outside" || approval.Status.ExecutionState != api.RuntimeApprovalExecutionCompleted || approval.Status.Claimant == nil || deployment.effects() != before {
		test.Fatal("capability issuance did not complete its exact claim before target contact")
	}
	invoke := func(endpoint string) gatewayAuthorityHTTPResult {
		return gatewayAuthorityRequest(fixture.context, httpClient, http.MethodPost, endpoint, "/v1/agent-services/invoke", capability, "", payload)
	}
	raceAuthorityRequests(test, endpoints, invoke, http.StatusAccepted, map[string]any{"session_id": invocation, "target_agent": "authority-communication-target"})
	expected := before
	expected.Communication++
	changed := []byte(strings.ReplaceAll(string(payload), "owned acceptance communication", "changed communication"))
	requireAuthorityResponse(test, gatewayAuthorityRequest(fixture.context, httpClient, http.MethodPost, endpoints[1], "/v1/agent-services/invoke", capability, "", changed), http.StatusForbidden, nil)
	if deployment.effects() != expected {
		test.Fatal("duplicate or substituted Communication reached provider", deployment.effects())
	}
	test.Logf("Production Communication invocation=%s: cross-workspace denial/grant, pending capability issuance, human decision, exactly one replica-owned capability, reissuance denial, concurrent dispatch, both durable replays and substituted-message denial passed; effects=%+v", invocation, deployment.effects())
}

func (fixture *gatewayAuthorityFixture) installProductionDatabase(test *testing.T) string {
	test.Helper()
	password := uuid.NewString() + uuid.NewString()
	dsn := fmt.Sprintf("postgres://authority:%s@authority-database.control.svc:5432/authority?sslmode=disable", password)
	fixture.create(test, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: authorityProductionRelease + "-db", Namespace: "control"}, Data: map[string][]byte{"DATABASE_URL": []byte(dsn), "password": []byte(password)}})
	labels := map[string]string{"app.kubernetes.io/component": "authority-database", "kognition.dev/authority-acceptance-run": fixture.runID}
	fixture.create(test, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "authority-database", Namespace: "control"}, Spec: appsv1.DeploymentSpec{
		Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
				AutomountServiceAccountToken: ptr.To(false),
				SecurityContext:              &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(999)), RunAsGroup: ptr.To(int64(999)), RunAsNonRoot: ptr.To(true), FSGroup: ptr.To(int64(999))},
				Volumes: []corev1.Volume{
					{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
				Containers: []corev1.Container{{
					Name: "postgres", Image: "local/kognition-authority-postgres:" + fixture.runID, ImagePullPolicy: corev1.PullNever,
					SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(true), AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
					Env: []corev1.EnvVar{
						{Name: "POSTGRES_USER", Value: "authority"}, {Name: "POSTGRES_DB", Value: "authority"},
						{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
						{Name: "POSTGRES_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: authorityProductionRelease + "-db"}, Key: "password"}}},
					},
					Ports:          []corev1.ContainerPort{{Name: "postgres", ContainerPort: 5432}},
					VolumeMounts:   []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/postgresql/data"}, {Name: "run", MountPath: "/var/run/postgresql"}, {Name: "tmp", MountPath: "/tmp"}},
					ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"pg_isready", "-h", "127.0.0.1", "-U", "authority", "-d", "authority"}}}, PeriodSeconds: 1},
				}},
			},
		},
	}})
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "authority-database", Namespace: "control"}, Spec: corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{{Port: 5432, TargetPort: intstr.FromInt32(5432)}}}}
	fixture.create(test, service)
	pods := fixture.productionPods(test, "authority-database", 1)
	if net.ParseIP(service.Spec.ClusterIP) == nil {
		test.Fatal("explicit database Service IP required for the node-side acceptance runner")
	}
	runnerDSN := fmt.Sprintf("postgres://authority:%s@%s/authority?sslmode=disable", password, net.JoinHostPort(service.Spec.ClusterIP, "5432"))
	pool, err := pgxpool.New(fixture.context, runnerDSN)
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	var connectionErr error
	if err := wait.PollUntilContextTimeout(fixture.context, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		connectionErr = pool.Ping(ctx)
		return connectionErr == nil, nil
	}); err != nil {
		test.Fatalf("production database Service not ready: %v; last connection: %v", err, connectionErr)
	}
	if err := pgstore.Migrate(fixture.context, pool, ""); err != nil {
		test.Fatal("production Memory receipt migrations", err)
	}
	test.Logf("Chart-pinned PostgreSQL PodUID=%s containerID=%s imageID=%s; complete embedded migrations applied", pods[0].UID, pods[0].Status.ContainerStatuses[0].ContainerID, pods[0].Status.ContainerStatuses[0].ImageID)
	return runnerDSN
}
