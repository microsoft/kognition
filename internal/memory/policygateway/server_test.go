// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package policygateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/memory/inmemory"
	"github.com/kognition-ai/kognition/internal/memory/service"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestEnsureClaimantDefaultsUsesInjectedWorkloadUID(t *testing.T) {
	t.Setenv("KOGNITION_RUNTIME_APPROVAL_WORKLOAD_UID", "pod-uid-0123456789")
	server := &Server{}

	if err := server.ensureClaimantDefaults(); err != nil {
		t.Fatalf("default claimant: %v", err)
	}

	if got := server.claimant.WorkloadUID; got != "pod-uid-0123456789" {
		t.Fatalf("claim workload UID = %q, want injected Pod UID", got)
	}
	if server.claimant.ProcessStartNonce == "" {
		t.Fatal("claim process-start nonce is empty")
	}
}

type memoryStore struct {
	mu   sync.RWMutex
	data map[string][]byte
}

type unavailableMemoryStore struct {
	*memoryStore
	err error
}

type policyAuditStore struct {
	entries []observability.AuditEntry
	err     error
}

func (s *policyAuditStore) Append(_ context.Context, entry observability.AuditEntry) error {
	if s.err != nil {
		return s.err
	}
	s.entries = append(s.entries, entry)
	return nil
}

func (s *policyAuditStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	for _, entry := range entries {
		if err := s.Append(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *policyAuditStore) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return append([]observability.AuditEntry(nil), s.entries...), nil
}

func (s *unavailableMemoryStore) Ping(context.Context) error { return s.err }

func newMemoryStore() *memoryStore { return &memoryStore{data: make(map[string][]byte)} }
func (s *memoryStore) key(kind, namespace, name string) string {
	return kind + "/" + namespace + "/" + name
}
func (*memoryStore) Ping(context.Context) error { return nil }
func (s *memoryStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.data[s.key(kind, namespace, name)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), data...), nil
}
func (s *memoryStore) List(_ context.Context, kind string, options store.ListOptions) ([][]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := kind + "/" + options.Namespace + "/"
	items := make([][]byte, 0)
	for key, data := range s.data {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			items = append(items, append([]byte(nil), data...))
		}
	}
	return items, nil
}
func (s *memoryStore) Create(_ context.Context, kind, namespace, name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.key(kind, namespace, name)
	if _, exists := s.data[key]; exists {
		return store.ErrAlreadyExists
	}
	s.data[key] = append([]byte(nil), data...)
	return nil
}
func (s *memoryStore) Update(_ context.Context, kind, namespace, name string, data []byte, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.key(kind, namespace, name)
	if _, exists := s.data[key]; !exists {
		return store.ErrNotFound
	}
	s.data[key] = append([]byte(nil), data...)
	return nil
}
func (s *memoryStore) Delete(_ context.Context, kind, namespace, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.key(kind, namespace, name)
	if _, exists := s.data[key]; !exists {
		return store.ErrNotFound
	}
	delete(s.data, key)
	return nil
}
func (*memoryStore) Watch(context.Context, string, store.ListOptions) (<-chan store.WatchEvent, error) {
	return make(chan store.WatchEvent), nil
}
func (*memoryStore) CreateRevision(context.Context, string, string, string, int64, []byte, []byte) error {
	return nil
}
func (*memoryStore) ListRevisions(context.Context, string, string, string) ([]store.Revision, error) {
	return nil, nil
}
func (*memoryStore) GetRevision(context.Context, string, string, string, int64) (*store.Revision, error) {
	return nil, store.ErrNotFound
}
func (*memoryStore) AppendEvent(context.Context, string, string, string, store.InstanceEvent) error {
	return nil
}
func (*memoryStore) ListEvents(context.Context, string, string, string) ([]store.InstanceEvent, error) {
	return nil, nil
}

func storeObject(t *testing.T, s *memoryStore, kind, namespace, name string, object any) {
	t.Helper()
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), kind, namespace, name, data); err != nil {
		t.Fatal(err)
	}
}

func gatewayFixture(t *testing.T) (*Server, *memoryStore, *runtimeauth.MemoryCapabilityIssuer, runtimeauth.MemoryCapabilityBinding, *httptest.Server, *int) {
	t.Helper()
	memoryStore := newMemoryStore()
	namespace := "team-a"
	agent := v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: namespace, UID: "agent-uid"}}
	identity := v1alpha1.AgentIdentity{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-identity-planner", Namespace: namespace, Labels: map[string]string{"kognition.dev/auto-created-for-agent": "planner"}, OwnerReferences: []v1alpha1.OwnerReference{{Kind: "Agent", Name: "planner", UID: agent.ObjectMeta.UID}}},
		Spec:       v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}, SPIFFEID: "spiffe://kognition.local/workspace/team-a/agent/planner"},
		Status:     v1alpha1.AgentIdentityStatus{Phase: v1alpha1.PhaseReady},
	}
	instance := v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-r1", Namespace: namespace, UID: "instance-uid"}, Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}}, Status: v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhaseRunning, SessionID: "session-1"}}
	claim := v1alpha1.MemoryClaim{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-memory", Namespace: namespace}, Status: v1alpha1.MemoryClaimStatus{Phase: v1alpha1.PhaseBound, BoundVolumeName: "volume-a"}}
	volume := v1alpha1.MemoryVolume{ObjectMeta: v1alpha1.ObjectMeta{Name: "volume-a", Namespace: namespace}, Spec: v1alpha1.MemoryVolumeSpec{ClaimRef: &v1alpha1.LocalObjectReference{Name: claim.ObjectMeta.Name}, BackendHandle: "backend-a"}, Status: v1alpha1.MemoryVolumeStatus{Phase: v1alpha1.PhaseBound}}
	grant := v1alpha1.MemoryGrant{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-memory", Namespace: namespace}, Spec: v1alpha1.MemoryGrantSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}, MemoryClaimRef: v1alpha1.LocalObjectReference{Name: claim.ObjectMeta.Name}, AccessMode: "ReadWrite"}}
	role := v1alpha1.AgentRole{ObjectMeta: v1alpha1.ObjectMeta{Name: "memory-writer", Namespace: namespace}, Rules: []v1alpha1.PolicyRule{{Resources: []string{"Memory"}, Verbs: []string{"get", "update"}}}}
	binding := v1alpha1.AgentRoleBinding{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-memory", Namespace: namespace}, Spec: v1alpha1.AgentRoleBindingSpec{RoleRef: v1alpha1.LocalObjectReference{Name: role.ObjectMeta.Name}, Subjects: []v1alpha1.Subject{{Kind: "AgentIdentity", Name: identity.ObjectMeta.Name}}}}
	storeObject(t, memoryStore, "Agent", namespace, agent.ObjectMeta.Name, agent)
	storeObject(t, memoryStore, "AgentIdentity", namespace, identity.ObjectMeta.Name, identity)
	storeObject(t, memoryStore, "AgentInstance", namespace, instance.ObjectMeta.Name, instance)
	storeObject(t, memoryStore, "MemoryClaim", namespace, claim.ObjectMeta.Name, claim)
	storeObject(t, memoryStore, "MemoryVolume", namespace, volume.ObjectMeta.Name, volume)
	storeObject(t, memoryStore, "MemoryGrant", namespace, grant.ObjectMeta.Name, grant)
	storeObject(t, memoryStore, "AgentRole", namespace, role.ObjectMeta.Name, role)
	storeObject(t, memoryStore, "AgentRoleBinding", namespace, binding.ObjectMeta.Name, binding)
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/memory/health" {
			if r.Header.Get("Authorization") != "Bearer internal-memory-token" {
				t.Fatalf("upstream health authorization = %q, want internal token", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		upstreamCalls++
		if r.Header.Get("Authorization") != "Bearer internal-memory-token" {
			t.Fatalf("upstream authorization = %q, want internal token only", r.Header.Get("Authorization"))
		}
		if r.Header.Get(memory.TrustedAgentIdentityHeader) != "spiffe://kognition.local/workspace/team-a/agent/planner" {
			t.Fatalf("upstream AgentIdentity = %q", r.Header.Get(memory.TrustedAgentIdentityHeader))
		}
		if r.Header.Get(memory.TrustedWorkspaceHeader) != namespace {
			t.Fatalf("upstream workspace = %q, want %q", r.Header.Get(memory.TrustedWorkspaceHeader), namespace)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"record-1"}`))
	}))
	issuer, err := runtimeauth.NewMemoryCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(memoryStore, issuer, upstream.URL, nil, zaptest.NewLogger(t), WithUpstreamToken("internal-memory-token"))
	if err != nil {
		t.Fatal(err)
	}
	capabilityBinding := runtimeauth.MemoryCapabilityBinding{Namespace: namespace, AgentName: agent.ObjectMeta.Name, AgentIdentityName: identity.ObjectMeta.Name, InstanceName: instance.ObjectMeta.Name, InstanceUID: instance.ObjectMeta.UID, SessionID: "session-1", VolumeHandle: volume.Spec.BackendHandle}
	return server, memoryStore, issuer, capabilityBinding, upstream, &upstreamCalls
}

func TestServerEnforcesCapabilityAndLiveMemoryPolicy(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	newRequest := func(token, volume string) *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"`+volume+`","content":"remember"}`))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		return request
	}
	for _, test := range []struct {
		name       string
		request    *http.Request
		wantStatus int
	}{
		{name: "missing capability", request: newRequest("", binding.VolumeHandle), wantStatus: http.StatusUnauthorized},
		{name: "invalid capability", request: newRequest("invalid", binding.VolumeHandle), wantStatus: http.StatusUnauthorized},
		{name: "wrong volume", request: newRequest(capability, "other-volume"), wantStatus: http.StatusForbidden},
		{name: "authorized write", request: newRequest(capability, binding.VolumeHandle), wantStatus: http.StatusCreated},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.ServeHTTP(response, test.request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
	if *upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want only authorized request", *upstreamCalls)
	}
}

func TestServerHealthDoesNotRequireCapability(t *testing.T) {
	server, _, _, _, upstream, _ := gatewayFixture(t)
	defer upstream.Close()
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/memory/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestServerHealthReflectsStoreAndUpstreamDependencies(t *testing.T) {
	server, memoryStore, _, _, upstream, _ := gatewayFixture(t)
	defer upstream.Close()
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/memory/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("healthy gateway status = %d, want %d", response.Code, http.StatusOK)
	}
	upstream.Close()
	response = httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/memory/health", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable upstream status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	_ = memoryStore
}

func TestServerHealthReturnsUnavailableWhenPolicyStoreFails(t *testing.T) {
	server, memoryStore, _, _, upstream, _ := gatewayFixture(t)
	defer upstream.Close()
	server.store = &unavailableMemoryStore{memoryStore: memoryStore, err: errors.New("database unavailable")}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/memory/health", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable policy store status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

// TC-0306: Revoked Memory operations are denied before reaching their target.
func TestServerHonorsGrantRevocationBeforeForwarding(t *testing.T) {
	server, memoryStore, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"backend-a","content":"policy"}`))
	request.Header.Set("Authorization", "Bearer "+capability)

	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("initial status = %d, want %d", response.Code, http.StatusCreated)
	}
	if *upstreamCalls != 1 {
		t.Fatalf("initial upstream calls = %d, want 1", *upstreamCalls)
	}
	if err := memoryStore.Delete(context.Background(), "MemoryGrant", binding.Namespace, "planner-memory"); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"backend-a","content":"policy"}`))
	request.Header.Set("Authorization", "Bearer "+capability)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("revoked grant status = %d, want %d: %s", response.Code, http.StatusForbidden, response.Body.String())
	}
	if *upstreamCalls != 1 {
		t.Fatalf("upstream calls after revocation = %d, want 1", *upstreamCalls)
	}
}

func TestServerPersistsAuthorizedAndRevokedOperationAudit(t *testing.T) {
	server, memoryStore, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	auditStore := &policyAuditStore{}
	server.auditLog = observability.NewCognitiveAuditLog(auditStore)
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"backend-a","content":"memory-secret-value"}`))
		r.Header.Set("Authorization", "Bearer "+capability)
		r.Header.Set("X-Kognition-Run-ID", "demo-run")
		r.Header.Set(MemoryInvocationIDHeader, "invocation-1")
		return r
	}

	response := httptest.NewRecorder()
	server.ServeHTTP(response, request())
	if response.Code != http.StatusCreated || len(auditStore.entries) != 2 {
		t.Fatalf("authorized status=%d audit=%#v", response.Code, auditStore.entries)
	}
	if auditStore.entries[0].EventType != observability.AuditMemoryOp || auditStore.entries[0].Verb != "store" || auditStore.entries[0].AgentIdentity != "spiffe://kognition.local/workspace/team-a/agent/planner" {
		t.Fatalf("authorized audit = %#v", auditStore.entries[0])
	}
	schema := loadMemoryTrustDecisionSchema(t)
	var allowed map[string]any
	if err := json.Unmarshal(auditStore.entries[0].Details, &allowed); err != nil {
		t.Fatalf("unmarshal authorized trust decision: %v", err)
	} else if err := schema.Validate(allowed); err != nil {
		t.Fatalf("authorized trust decision does not match schema: %v", err)
	}
	if allowed["runId"] != "demo-run" || allowed["outcome"] != "allowed" || allowed["reasonCode"] != "authorized" || bytes.Contains(auditStore.entries[0].Details, []byte("memory-secret-value")) {
		t.Fatalf("authorized trust decision = %s", auditStore.entries[0].Details)
	}
	var completed observability.TrustDecisionV1
	if err := json.Unmarshal(auditStore.entries[1].Details, &completed); err != nil {
		t.Fatalf("unmarshal completed trust decision: %v", err)
	}
	if completed.Outcome != "completed" || completed.ReasonCode != "target_completed" || completed.Request.InvocationID != "invocation-1" || completed.Execution.Status != "completed" || completed.Execution.TargetReached == nil || !*completed.Execution.TargetReached {
		t.Fatalf("completed trust decision = %s", auditStore.entries[1].Details)
	}

	if err := memoryStore.Delete(context.Background(), "MemoryGrant", binding.Namespace, "planner-memory"); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request())
	if response.Code != http.StatusForbidden || len(auditStore.entries) != 3 || *upstreamCalls != 1 {
		t.Fatalf("revoked status=%d audit=%#v upstream=%d", response.Code, auditStore.entries, *upstreamCalls)
	}
	denied := auditStore.entries[2]
	var decision map[string]any
	if err := json.Unmarshal(denied.Details, &decision); err != nil {
		t.Fatalf("unmarshal denied trust decision: %v", err)
	} else if err := schema.Validate(decision); err != nil {
		t.Fatalf("denied trust decision does not match schema: %v", err)
	}
	if denied.Verb != "store" || denied.EventType != observability.AuditMemoryOp || denied.AgentIdentity != "spiffe://kognition.local/workspace/team-a/agent/planner" || decision["runId"] != "demo-run" || decision["outcome"] != "denied" || decision["reasonCode"] != "grant_missing" || bytes.Contains(denied.Details, []byte("memory-secret-value")) {
		t.Fatalf("denied audit = %#v", denied)
	}
	approval := decision["approval"].(map[string]any)
	if approval["status"] != "not_evaluated" || approval["decisionRef"] != nil {
		t.Fatalf("approval must be unevaluated after authorization denial: %s", denied.Details)
	}
}

func TestServerDoesNotPromoteUnverifiedIdentityNameIntoDenialAudit(t *testing.T) {
	server, memoryStore, issuer, binding, upstream, _ := gatewayFixture(t)
	defer upstream.Close()
	auditStore := &policyAuditStore{}
	server.auditLog = observability.NewCognitiveAuditLog(auditStore)
	if err := memoryStore.Delete(context.Background(), "AgentIdentity", binding.Namespace, binding.AgentIdentityName); err != nil {
		t.Fatal(err)
	}
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"backend-a","content":"denied"}`))
	request.Header.Set("Authorization", "Bearer "+capability)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || len(auditStore.entries) != 1 {
		t.Fatalf("status=%d audit=%#v", response.Code, auditStore.entries)
	}
	entry := auditStore.entries[0]
	var decision map[string]any
	if err := json.Unmarshal(entry.Details, &decision); err != nil || loadMemoryTrustDecisionSchema(t).Validate(decision) != nil {
		t.Fatalf("unverified trust decision = %s, err=%v", entry.Details, err)
	}
	claimed, _ := decision["claimedIdentityRef"].(map[string]any)
	if entry.AgentIdentity != "" || claimed["kind"] != "AgentIdentity" || claimed["name"] != "agent-identity-planner" || bytes.Contains(entry.Details, []byte("claimedAgentIdentityName")) {
		t.Fatalf("unverified denial audit = %#v", entry)
	}
}

func TestServerEmitsRevokedTrustDecisionForMemoryGrantTombstone(t *testing.T) {
	server, memoryStore, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	auditStore := &policyAuditStore{}
	server.auditLog = observability.NewCognitiveAuditLog(auditStore)

	claimData, err := memoryStore.Get(context.Background(), "MemoryClaim", binding.Namespace, "planner-memory")
	if err != nil {
		t.Fatal(err)
	}
	var claim v1alpha1.MemoryClaim
	if err := json.Unmarshal(claimData, &claim); err != nil {
		t.Fatal(err)
	}
	claim.ObjectMeta.Annotations = map[string]string{controller.MemoryGrantRevocationsAnnotation: `["planner-memory"]`}
	updatedClaim, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Update(context.Background(), "MemoryClaim", binding.Namespace, claim.ObjectMeta.Name, updatedClaim, 0); err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Delete(context.Background(), "MemoryGrant", binding.Namespace, "planner-memory"); err != nil {
		t.Fatal(err)
	}

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"backend-a","content":"memory-secret-value"}`))
	request.Header.Set("Authorization", "Bearer "+capability)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || *upstreamCalls != 0 || len(auditStore.entries) != 1 {
		t.Fatalf("status=%d upstream=%d audit=%#v", response.Code, *upstreamCalls, auditStore.entries)
	}
	var decision map[string]any
	if err := json.Unmarshal(auditStore.entries[0].Details, &decision); err != nil {
		t.Fatal(err)
	}
	if err := loadMemoryTrustDecisionSchema(t).Validate(decision); err != nil {
		t.Fatalf("revoked trust decision does not match schema: %v", err)
	}
	authorization, _ := decision["authorization"].(map[string]any)
	grantRef, _ := authorization["grantRef"].(map[string]any)
	if decision["outcome"] != "revoked" || decision["reasonCode"] != "grant_revoked" || authorization["grant"] != "revoked" || grantRef["name"] != "planner-memory" || bytes.Contains(auditStore.entries[0].Details, []byte("memory-secret-value")) {
		t.Fatalf("revoked trust decision = %s", auditStore.entries[0].Details)
	}
}

func loadMemoryTrustDecisionSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	schema, err := compiler.Compile(filepath.Join("..", "..", "..", "api", "schemas", "trust-decision-v1.schema.json"))
	if err != nil {
		t.Fatalf("compile trust decision schema: %v", err)
	}
	return schema
}

func TestServerAuditFailureBlocksAuthorizedForwarding(t *testing.T) {
	server, _, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	server.auditLog = observability.NewCognitiveAuditLog(&policyAuditStore{err: errors.New("audit unavailable")})
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"backend-a","content":"must not forward"}`))
	request.Header.Set("Authorization", "Bearer "+capability)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || *upstreamCalls != 0 {
		t.Fatalf("status=%d upstream=%d, want audit failure before forwarding", response.Code, *upstreamCalls)
	}
}

func TestServerBlocksRecordsWrittenUnderRevokedGrant(t *testing.T) {
	server, memoryStore, issuer, binding, fixtureUpstream, _ := gatewayFixture(t)
	defer fixtureUpstream.Close()

	rawMux := http.NewServeMux()
	rawHandler := service.NewHandler(inmemory.New(), zaptest.NewLogger(t), service.WithInternalToken("internal-memory-token"))
	rawHandler.RegisterRoutes(rawMux)
	rawService := httptest.NewServer(rawMux)
	defer rawService.Close()
	upstream, err := url.Parse(rawService.URL)
	if err != nil {
		t.Fatal(err)
	}
	server.upstream = upstream

	provision := httptest.NewRequest(http.MethodPost, "/v1/memory/provision", bytes.NewBufferString(`{"volume_handle":"backend-a"}`))
	provision.Header.Set("Authorization", "Bearer internal-memory-token")
	provisionResponse := httptest.NewRecorder()
	rawMux.ServeHTTP(provisionResponse, provision)
	if provisionResponse.Code != http.StatusCreated {
		t.Fatalf("provision status = %d", provisionResponse.Code)
	}

	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	storeRequest := httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"backend-a","id":"record-1","content":"revocable","metadata":{"kognition.dev/memory-grant":"forged"}}`))
	storeRequest.Header.Set("Authorization", "Bearer "+capability)
	storeResponse := httptest.NewRecorder()
	server.ServeHTTP(storeResponse, storeRequest)
	if storeResponse.Code != http.StatusCreated {
		t.Fatalf("store status = %d: %s", storeResponse.Code, storeResponse.Body.String())
	}

	internalGet := httptest.NewRequest(http.MethodGet, "/v1/memory/get/backend-a/record-1", nil)
	internalGet.Header.Set("Authorization", "Bearer internal-memory-token")
	internalGetResponse := httptest.NewRecorder()
	rawMux.ServeHTTP(internalGetResponse, internalGet)
	if internalGetResponse.Code != http.StatusOK {
		t.Fatalf("internal get status = %d", internalGetResponse.Code)
	}
	var stored service.RecordDTO
	if err := json.NewDecoder(internalGetResponse.Body).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Metadata[controller.MemoryGrantProvenanceMetadataKey] != "planner-memory" {
		t.Fatalf("stored grant provenance = %#v, want planner-memory", stored.Metadata)
	}
	authorizedGet := httptest.NewRequest(http.MethodGet, "/v1/memory/get/backend-a/record-1", nil)
	authorizedGet.Header.Set("Authorization", "Bearer "+capability)
	authorizedGetResponse := httptest.NewRecorder()
	server.ServeHTTP(authorizedGetResponse, authorizedGet)
	if authorizedGetResponse.Code != http.StatusOK {
		t.Fatalf("authorized get status = %d: %s", authorizedGetResponse.Code, authorizedGetResponse.Body.String())
	}
	var visible service.RecordDTO
	if err := json.NewDecoder(authorizedGetResponse.Body).Decode(&visible); err != nil {
		t.Fatal(err)
	}
	if _, exposed := visible.Metadata[controller.MemoryGrantProvenanceMetadataKey]; exposed {
		t.Fatalf("authorized response exposed internal grant provenance: %#v", visible.Metadata)
	}

	readerGrant := v1alpha1.MemoryGrant{ObjectMeta: v1alpha1.ObjectMeta{Name: "reader-memory", Namespace: binding.Namespace}, Spec: v1alpha1.MemoryGrantSpec{AgentRef: v1alpha1.LocalObjectReference{Name: binding.AgentName}, MemoryClaimRef: v1alpha1.LocalObjectReference{Name: "planner-memory"}, AccessMode: "ReadWrite"}}
	storeObject(t, memoryStore, "MemoryGrant", binding.Namespace, readerGrant.ObjectMeta.Name, readerGrant)
	rawGrant, err := memoryStore.Get(context.Background(), "MemoryGrant", binding.Namespace, "planner-memory")
	if err != nil {
		t.Fatal(err)
	}
	var deletingGrant v1alpha1.MemoryGrant
	if err := json.Unmarshal(rawGrant, &deletingGrant); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	deletingGrant.ObjectMeta.DeletionTimestamp = &now
	grantData, err := json.Marshal(&deletingGrant)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Update(context.Background(), "MemoryGrant", binding.Namespace, "planner-memory", grantData, deletingGrant.ObjectMeta.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.NewMemoryGrantReconciler(memoryStore, zaptest.NewLogger(t)).Reconcile(context.Background(), controller.Request{Kind: "MemoryGrant", Namespace: binding.Namespace, Name: "planner-memory"}); err != nil {
		t.Fatalf("reconcile revoked grant: %v", err)
	}

	request := func(method, path, body string) *http.Request {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+capability)
		return req
	}
	for _, test := range []struct {
		name string
		req  *http.Request
	}{
		{name: "get", req: request(http.MethodGet, "/v1/memory/get/backend-a/record-1", "")},
		{name: "list", req: request(http.MethodPost, "/v1/memory/list", `{"volume_handle":"backend-a"}`)},
		{name: "search", req: request(http.MethodPost, "/v1/memory/search", `{"volume_handle":"backend-a","query":"revocable","top_k":5}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.ServeHTTP(response, test.req)
			if test.name == "get" {
				if response.Code != http.StatusNotFound {
					t.Fatalf("get status = %d, want 404: %s", response.Code, response.Body.String())
				}
				return
			}
			if response.Code != http.StatusOK {
				t.Fatalf("%s response status = %d, want 200: %s", test.name, response.Code, response.Body.String())
			}
			var results []json.RawMessage
			if err := json.NewDecoder(response.Body).Decode(&results); err != nil || len(results) != 0 {
				t.Fatalf("%s response results = %#v err=%v, want empty", test.name, results, err)
			}
		})
	}
	missing := httptest.NewRequest(http.MethodGet, "/v1/memory/get/backend-a/missing", nil)
	missing.Header.Set("Authorization", "Bearer "+capability)
	missingResponse := httptest.NewRecorder()
	server.ServeHTTP(missingResponse, missing)
	if missingResponse.Code != http.StatusNotFound {
		t.Fatalf("missing record status = %d, want upstream 404: %s", missingResponse.Code, missingResponse.Body.String())
	}
}

func TestServerProxiesToAuthenticatedMemoryService(t *testing.T) {
	_, memoryStore, issuer, binding, fixtureUpstream, _ := gatewayFixture(t)
	defer fixtureUpstream.Close()

	rawMux := http.NewServeMux()
	rawHandler := service.NewHandler(inmemory.New(), zaptest.NewLogger(t), service.WithInternalToken("internal-memory-token"))
	rawHandler.RegisterRoutes(rawMux)
	rawService := httptest.NewServer(rawMux)
	defer rawService.Close()

	request := func(method, target string, payload string, token string) *http.Response {
		t.Helper()
		httpRequest, err := http.NewRequest(method, target, bytes.NewBufferString(payload))
		if err != nil {
			t.Fatal(err)
		}
		httpRequest.Header.Set("Content-Type", "application/json")
		if token != "" {
			httpRequest.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := rawService.Client().Do(httpRequest)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	response := request(http.MethodPost, rawService.URL+"/v1/memory/provision", `{"volume_handle":"backend-a"}`, "internal-memory-token")
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("raw provision status = %d, want %d", response.StatusCode, http.StatusCreated)
	}

	direct := request(http.MethodPost, rawService.URL+"/v1/memory/store", `{"volume_handle":"backend-a","id":"record-1","content":"private"}`, "")
	direct.Body.Close()
	if direct.StatusCode != http.StatusUnauthorized {
		t.Fatalf("direct raw memory request status = %d, want %d", direct.StatusCode, http.StatusUnauthorized)
	}

	gateway, err := New(memoryStore, issuer, rawService.URL, nil, zaptest.NewLogger(t), WithUpstreamToken("internal-memory-token"))
	if err != nil {
		t.Fatal(err)
	}
	gatewayService := httptest.NewServer(gateway)
	defer gatewayService.Close()
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}

	gatewayRequest, err := http.NewRequest(http.MethodPost, gatewayService.URL+"/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"backend-a","id":"record-1","content":"authorized"}`))
	if err != nil {
		t.Fatal(err)
	}
	gatewayRequest.Header.Set("Content-Type", "application/json")
	gatewayRequest.Header.Set("Authorization", "Bearer "+capability)
	stored, err := gatewayService.Client().Do(gatewayRequest)
	if err != nil {
		t.Fatal(err)
	}
	stored.Body.Close()
	if stored.StatusCode != http.StatusCreated {
		t.Fatalf("gateway store status = %d, want %d", stored.StatusCode, http.StatusCreated)
	}

	verified := request(http.MethodGet, rawService.URL+"/v1/memory/get/backend-a/record-1", "", "internal-memory-token")
	defer verified.Body.Close()
	if verified.StatusCode != http.StatusOK {
		t.Fatalf("internal raw read status = %d, want %d", verified.StatusCode, http.StatusOK)
	}
	var record struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(verified.Body).Decode(&record); err != nil {
		t.Fatal(err)
	}
	if record.Content != "authorized" {
		t.Fatalf("stored record content = %q, want authorized", record.Content)
	}
}

func TestServerRejectsCapabilityAfterSessionOrInstanceReplacement(t *testing.T) {
	server, memoryStore, issuer, binding, upstream, upstreamCalls := gatewayFixture(t)
	defer upstream.Close()
	capability, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/memory/search", bytes.NewBufferString(`{"volume_handle":"backend-a","query":"policy"}`))
		r.Header.Set("Authorization", "Bearer "+capability)
		return r
	}

	raw, err := memoryStore.Get(context.Background(), "AgentInstance", binding.Namespace, binding.InstanceName)
	if err != nil {
		t.Fatal(err)
	}
	var instance v1alpha1.AgentInstance
	if err := json.Unmarshal(raw, &instance); err != nil {
		t.Fatal(err)
	}
	instance.Status.SessionID = "replacement-session"
	data, err := json.Marshal(instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Update(context.Background(), "AgentInstance", binding.Namespace, binding.InstanceName, data, 0); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request())
	if response.Code != http.StatusForbidden || *upstreamCalls != 0 {
		t.Fatalf("stale session response = %d, upstream calls = %d", response.Code, *upstreamCalls)
	}

	instance.Status.SessionID = binding.SessionID
	instance.ObjectMeta.UID = "replacement-instance-uid"
	data, err = json.Marshal(instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Update(context.Background(), "AgentInstance", binding.Namespace, binding.InstanceName, data, 0); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request())
	if response.Code != http.StatusForbidden || *upstreamCalls != 0 {
		t.Fatalf("replacement instance response = %d, upstream calls = %d", response.Code, *upstreamCalls)
	}
}
