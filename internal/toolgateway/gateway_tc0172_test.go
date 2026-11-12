// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/invocation"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

// tc0172ResolverStore serves the runtimeapproval.Resolver from an in-memory
// Agent + TrustProfile + AutonomyClass fixture. The TrustProfile pins the
// Agent to the `assisted` class so the resolver returns the assisted-tier
// AutonomyClass with `toolInvocation: high-risk-only`. TC-0172 covers the
// unclassified-Tool conservative-pending path per BACKLOG.md:1269.
type tc0172ResolverStore struct {
	agent    v1alpha1.Agent
	class    v1alpha1.AutonomyClass
	profiles []v1alpha1.TrustProfile
}

func (s *tc0172ResolverStore) Ping(context.Context) error { return nil }

func (s *tc0172ResolverStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	switch {
	case kind == "Agent" && namespace == s.agent.Namespace && name == s.agent.Name:
		return json.Marshal(s.agent)
	case kind == "AutonomyClass" && name == s.class.Name:
		return json.Marshal(s.class)
	}
	return nil, store.ErrNotFound
}

func (s *tc0172ResolverStore) List(_ context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if kind != "TrustProfile" {
		return nil, nil
	}
	out := make([][]byte, 0, len(s.profiles))
	for _, p := range s.profiles {
		if opts.Namespace != "" && p.Namespace != opts.Namespace {
			continue
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

func (*tc0172ResolverStore) Create(context.Context, string, string, string, []byte) error {
	return nil
}
func (*tc0172ResolverStore) Update(context.Context, string, string, string, []byte, int64) error {
	return nil
}
func (*tc0172ResolverStore) Delete(context.Context, string, string, string) error { return nil }
func (*tc0172ResolverStore) Watch(context.Context, string, store.ListOptions) (<-chan store.WatchEvent, error) {
	return nil, nil
}
func (*tc0172ResolverStore) CreateRevision(context.Context, string, string, string, int64, []byte, []byte) error {
	return nil
}
func (*tc0172ResolverStore) ListRevisions(context.Context, string, string, string) ([]store.Revision, error) {
	return nil, nil
}
func (*tc0172ResolverStore) GetRevision(context.Context, string, string, string, int64) (*store.Revision, error) {
	return nil, store.ErrNotFound
}
func (*tc0172ResolverStore) AppendEvent(context.Context, string, string, string, store.InstanceEvent) error {
	return nil
}
func (*tc0172ResolverStore) ListEvents(context.Context, string, string, string) ([]store.InstanceEvent, error) {
	return nil, nil
}

// TestGatewayTC0172AssistedHighRiskOnlyPersistsPendingApprovalBeforeExecution
// discharges backlog test case TC-0172 (BACKLOG.md:1269) and ADR-0014
// completion evidence (docs/adr/0014-...:415): submit a real Tool invocation
// for an Agent at the `assisted` tier where the Tool carries no trusted risk
// classification metadata. Under `high-risk-only`, the Runtime Autonomy Gate
// must fail closed to Pending and persist a durable RuntimeApproval BEFORE
// the Tool target is contacted. This test wires the real
// runtimeapproval.Gate3Coordinator, real Resolver, and a durable Gate3Store
// fake so the persistence side effect is observable.
func TestGatewayTC0172AssistedHighRiskOnlyPersistsPendingApprovalBeforeExecution(t *testing.T) {
	// Real Agent + TrustProfile + AutonomyClass fixture per TC-0172 spec.
	agent := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "Agent", APIVersion: v1alpha1.APIVersion},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-r1", Namespace: "team-a", UID: "agent-uid-1", ResourceVersion: 2},
	}
	profile := v1alpha1.TrustProfile{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "TrustProfile", APIVersion: v1alpha1.APIVersion},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-r1-profile", Namespace: "team-a", UID: "profile-uid-1", ResourceVersion: 5},
		Spec: v1alpha1.TrustProfileSpec{
			AgentRef:            v1alpha1.LocalObjectReference{Name: agent.Name},
			AgentIncarnationUID: agent.UID,
			InitialScore:        50,
			InitialClass:        "supervised",
		},
		Status: v1alpha1.TrustProfileStatus{CurrentClass: "assisted"},
	}
	class := v1alpha1.AutonomyClass{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "AutonomyClass", APIVersion: v1alpha1.APIVersion},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "assisted", UID: "class-uid-assisted", ResourceVersion: 11},
		Spec: v1alpha1.AutonomyClassSpec{
			Level:            50,
			RequiresApproval: false,
			ApprovalPolicy: &v1alpha1.ApprovalPolicy{
				ToolInvocation:        "high-risk-only",
				MemoryWrite:           "high-risk-only",
				ExternalCommunication: "high-risk-only",
			},
		},
	}

	gate3Store := newTC0171FakeGate3Store()
	resolver := runtimeapproval.NewResolver(&tc0172ResolverStore{
		agent:    agent,
		class:    class,
		profiles: []v1alpha1.TrustProfile{profile},
	})
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

	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm"}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{}, before: func() {
		t.Fatalf("TC-0172: Tool target must not be contacted while approval is pending")
	}}
	// Tool has NO trusted risk classification metadata - the conservative
	// path under high-risk-only must still fail closed to Pending.
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
	gateway.newID = func() string { return "invocation-2" }
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
			SessionID: "session-tc0172",
			Target: v1alpha1.RuntimeApprovalReference{
				Kind: "Tool", Scope: "Namespaced", Namespace: tool.Namespace, Name: tool.Name, UID: "tool-uid", ResourceVersion: tool.ResourceVersion,
			},
			CanonicalPayload: json.RawMessage(`{"ticket":"42"}`),
		}, true
	})(gateway)

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
	// against the assisted-class incarnation binding.
	stored, ok := gate3Store.snapshot("invocation-2")
	if !ok {
		t.Fatalf("expected durable RuntimeApproval for invocation-2 to be persisted")
	}
	if stored.Status.DecisionState != v1alpha1.RuntimeApprovalDecisionPending {
		t.Fatalf("expected persisted DecisionState=Pending, got %q", stored.Status.DecisionState)
	}
	if stored.Spec.Workspace != "team-a" || stored.Spec.InvocationID != "invocation-2" {
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
