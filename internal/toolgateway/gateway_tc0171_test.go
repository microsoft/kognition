// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/invocation"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

// tc0171FakeGate3Store is a compact in-memory Gate3Store used by the
// TC-0171 standalone real-invocation test. It mirrors the persistence
// contract exercised by internal/runtimeapproval fakes (create-or-get is
// idempotent by invocation ID; NotRequired approvals are finalized on
// create) so the Tool Gateway drives a real Gate3Coordinator end-to-end.
type tc0171FakeGate3Store struct {
	mu        sync.Mutex
	approvals map[string]*v1alpha1.RuntimeApproval
	rv        int64
	creates   int
}

func newTC0171FakeGate3Store() *tc0171FakeGate3Store {
	return &tc0171FakeGate3Store{approvals: map[string]*v1alpha1.RuntimeApproval{}}
}

func (s *tc0171FakeGate3Store) CreateOrGetRuntimeApproval(_ context.Context, proposed v1alpha1.RuntimeApproval, _ json.RawMessage, ttl time.Duration) (bool, v1alpha1.RuntimeApproval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	if existing, ok := s.approvals[proposed.Spec.InvocationID]; ok {
		return false, *existing, nil
	}
	s.rv++
	stored := proposed
	stored.UID = "approval-" + proposed.Spec.InvocationID + "-uid"
	stored.ResourceVersion = s.rv
	stored.Spec.CreatedAt = time.Now().UTC()
	stored.Spec.ExpiresAt = stored.Spec.CreatedAt.Add(ttl)
	if stored.Status.DecisionState == v1alpha1.RuntimeApprovalDecisionNotRequired {
		runtimeapproval.FinalizeInitialStatus(&stored, time.Now())
	}
	s.approvals[proposed.Spec.InvocationID] = &stored
	return true, stored, nil
}

func (s *tc0171FakeGate3Store) AcknowledgeRuntimeApprovalDecisionAudit(_ context.Context, workspace, invocationID string, expectedRV int64, auditID string) (v1alpha1.RuntimeApproval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.approvals[invocationID]
	if !ok || stored.Spec.Workspace != workspace {
		return v1alpha1.RuntimeApproval{}, errors.New("tc0171 fake: unknown approval")
	}
	if stored.ResourceVersion != expectedRV {
		return v1alpha1.RuntimeApproval{}, errors.New("tc0171 fake: stale resource version")
	}
	if stored.Status.DecisionAudit == nil || stored.Status.DecisionAudit.ID != auditID {
		return v1alpha1.RuntimeApproval{}, errors.New("tc0171 fake: audit id mismatch")
	}
	return *stored, nil
}

// snapshot returns the durable RuntimeApproval for the given invocation ID
// so the test can assert the pending record was persisted.
func (s *tc0171FakeGate3Store) snapshot(invocationID string) (v1alpha1.RuntimeApproval, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.approvals[invocationID]
	if !ok {
		return v1alpha1.RuntimeApproval{}, false
	}
	return *stored, true
}

// tc0171ResolverStore serves the runtimeapproval.Resolver from an
// in-memory Agent + AutonomyClass fixture. TrustProfile listing is empty
// so the resolver defaults to the `supervised` class in the `system`
// namespace, which the test seeds explicitly with toolInvocation: always
// per BACKLOG.md TC-0171.
type tc0171ResolverStore struct {
	agent v1alpha1.Agent
	class v1alpha1.AutonomyClass
}

func (s *tc0171ResolverStore) Ping(context.Context) error { return nil }

func (s *tc0171ResolverStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	switch {
	case kind == "Agent" && namespace == s.agent.Namespace && name == s.agent.Name:
		return json.Marshal(s.agent)
	case kind == "AutonomyClass" && name == s.class.Name:
		return json.Marshal(s.class)
	}
	return nil, store.ErrNotFound
}

func (s *tc0171ResolverStore) List(context.Context, string, store.ListOptions) ([][]byte, error) {
	return nil, nil
}
func (*tc0171ResolverStore) Create(context.Context, string, string, string, []byte) error {
	return nil
}
func (*tc0171ResolverStore) Update(context.Context, string, string, string, []byte, int64) error {
	return nil
}
func (*tc0171ResolverStore) Delete(context.Context, string, string, string) error { return nil }
func (*tc0171ResolverStore) Watch(context.Context, string, store.ListOptions) (<-chan store.WatchEvent, error) {
	return nil, nil
}
func (*tc0171ResolverStore) CreateRevision(context.Context, string, string, string, int64, []byte, []byte) error {
	return nil
}
func (*tc0171ResolverStore) ListRevisions(context.Context, string, string, string) ([]store.Revision, error) {
	return nil, nil
}
func (*tc0171ResolverStore) GetRevision(context.Context, string, string, string, int64) (*store.Revision, error) {
	return nil, store.ErrNotFound
}
func (*tc0171ResolverStore) AppendEvent(context.Context, string, string, string, store.InstanceEvent) error {
	return nil
}
func (*tc0171ResolverStore) ListEvents(context.Context, string, string, string) ([]store.InstanceEvent, error) {
	return nil, nil
}

// TestGatewayTC0171SupervisedAlwaysPersistsPendingApprovalBeforeExecution
// discharges backlog test case TC-0171 (BACKLOG.md:1268) and ADR-0014
// completion evidence (docs/adr/0014-...:415): submit a real Tool
// invocation against an AutonomyClass with level:10 and toolInvocation:
// always, and prove a durable pending RuntimeApproval is persisted before
// the Tool target is contacted. Unlike the gate3 stub-resolver tests, this
// test wires the real runtimeapproval.Gate3Coordinator, real Resolver, and
// a durable Gate3Store fake so the persistence side effect is observable.
func TestGatewayTC0171SupervisedAlwaysPersistsPendingApprovalBeforeExecution(t *testing.T) {
	// Real Agent + AutonomyClass fixture per TC-0171 spec.
	agent := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "Agent", APIVersion: v1alpha1.APIVersion},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-r1", Namespace: "team-a", UID: "agent-uid-1", ResourceVersion: 2},
	}
	class := v1alpha1.AutonomyClass{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "AutonomyClass", APIVersion: v1alpha1.APIVersion},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "supervised", UID: "class-uid-supervised", ResourceVersion: 10},
		Spec: v1alpha1.AutonomyClassSpec{
			Level:            10,
			RequiresApproval: false,
			ApprovalPolicy: &v1alpha1.ApprovalPolicy{
				ToolInvocation:        "always",
				MemoryWrite:           "always",
				ExternalCommunication: "always",
			},
		},
	}

	// Real Gate3Coordinator over the fake persistence store.
	gate3Store := newTC0171FakeGate3Store()
	resolver := runtimeapproval.NewResolver(&tc0171ResolverStore{agent: agent, class: class})
	keys, err := runtimeapproval.NewStaticBindingKeyStore([]runtimeapproval.BindingKey{{
		Version:   1,
		Epoch:     1,
		Algorithm: runtimeapproval.BindingAlgorithmV1,
		Domain:    runtimeapproval.BindingDomainV1,
		Status:    runtimeapproval.BindingKeyActive,
		Material:  []byte("0123456789abcdef0123456789abcdef"),
	}})
	if err != nil {
		t.Fatalf("NewStaticBindingKeyStore: %v", err)
	}
	coordinator, err := runtimeapproval.NewGate3Coordinator(runtimeapproval.Gate3Config{
		Store:    gate3Store,
		Resolver: resolver,
		Keys:     keys,
		TTL:      time.Minute,
	})
	if err != nil {
		t.Fatalf("NewGate3Coordinator: %v", err)
	}

	// Real Gateway wiring, with an executor that fails the test if invoked.
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{}, before: func() {
		t.Fatalf("TC-0171: Tool target must not be contacted while approval is pending")
	}}
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: "team-a", ResourceVersion: 7}}
	toolData, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := New(
		&gatewayStore{data: map[string][]byte{"Tool/team-a/crm-update": toolData}},
		authorizer,
		quota,
		executor,
		zaptest.NewLogger(t),
		WithLedger(ledger),
		WithInvocationTracer(invocation.NewRecorder()),
	)
	if err != nil {
		t.Fatal(err)
	}
	gateway.newID = func() string { return "invocation-1" }
	WithGate3Coordinator(coordinator)(gateway)
	WithGate3RequestBuilder(func(_ context.Context, req Request, tool *v1alpha1.Tool) (runtimeapproval.Gate3Request, bool) {
		return runtimeapproval.Gate3Request{
			Workspace:    tool.Namespace,
			Namespace:    tool.Namespace,
			InvocationID: req.InvocationID,
			Plane:        v1alpha1.RuntimeApprovalPlaneTool,
			Operation:    "invoke",
			Action:       runtimeapproval.ActionToolInvocation,
			AgentName:    agent.Name,
			AgentIdentity: v1alpha1.AgentIdentity{
				TypeMeta:   v1alpha1.TypeMeta{Kind: "AgentIdentity", APIVersion: v1alpha1.APIVersion},
				ObjectMeta: v1alpha1.ObjectMeta{Name: agent.Name, Namespace: agent.Namespace, UID: "identity-uid", ResourceVersion: 3},
			},
			AgentInstance: v1alpha1.AgentInstance{
				TypeMeta:   v1alpha1.TypeMeta{Kind: "AgentInstance", APIVersion: v1alpha1.APIVersion},
				ObjectMeta: v1alpha1.ObjectMeta{Name: agent.Name + "-1", Namespace: agent.Namespace, UID: "instance-uid", ResourceVersion: 4},
			},
			SessionID: "session-tc0171",
			Target: v1alpha1.RuntimeApprovalReference{
				Kind: "Tool", Scope: "Namespaced", Namespace: tool.Namespace, Name: tool.Name, UID: "tool-uid", ResourceVersion: tool.ResourceVersion,
			},
			CanonicalPayload: json.RawMessage(`{"ticket":"42"}`),
		}, true
	})(gateway)

	// Submit the real Tool invocation.
	response, err := gateway.Invoke(context.Background(), testRequest())

	// Contract: ADR-0014 tagged pending transport, Tool executor unreached.
	if !errors.Is(err, ErrRuntimeApprovalPending) {
		t.Fatalf("expected ErrRuntimeApprovalPending, got %v", err)
	}
	if response.RuntimeApprovalState != RuntimeApprovalStatePending {
		t.Fatalf("expected pending runtime approval state, got %q", response.RuntimeApprovalState)
	}
	if response.RuntimeApproval == nil {
		t.Fatalf("expected tagged runtime approval reference on pending response")
	}
	if executor.calls != 0 {
		t.Fatalf("Tool target contacted before approval: executor.calls=%d", executor.calls)
	}
	if quota.calls != 0 {
		t.Fatalf("quota reserved before approval: quota.calls=%d", quota.calls)
	}

	// Contract: durable RuntimeApproval persisted with DecisionState=Pending
	// and the tagged reference points at that exact durable object.
	stored, ok := gate3Store.snapshot("invocation-1")
	if !ok {
		t.Fatalf("expected durable RuntimeApproval for invocation-1 to be persisted")
	}
	if stored.Status.DecisionState != v1alpha1.RuntimeApprovalDecisionPending {
		t.Fatalf("expected persisted DecisionState=Pending, got %q", stored.Status.DecisionState)
	}
	if stored.Spec.Workspace != "team-a" || stored.Spec.InvocationID != "invocation-1" {
		t.Fatalf("unexpected persisted approval binding: %#v", stored.Spec)
	}
	if response.RuntimeApproval.UID != string(stored.UID) || response.RuntimeApproval.Name != stored.Name || response.RuntimeApproval.Namespace != stored.Namespace {
		t.Fatalf("tagged reference %#v does not match persisted approval %s/%s uid=%s", response.RuntimeApproval, stored.Namespace, stored.Name, stored.UID)
	}
	if gate3Store.creates != 1 {
		t.Fatalf("expected exactly 1 CreateOrGetRuntimeApproval call, got %d", gate3Store.creates)
	}

	// Contract: pending decision recorded in the invocation ledger.
	ledger.mu.Lock()
	decisions := append([]LedgerEntry(nil), ledger.decisions...)
	ledger.mu.Unlock()
	if len(decisions) != 1 || decisions[0].Status != "runtime_approval_pending" {
		t.Fatalf("unexpected pending decision entry: %#v", decisions)
	}
}
