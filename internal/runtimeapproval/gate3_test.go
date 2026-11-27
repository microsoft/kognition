// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestNewGate3CoordinatorRejectsInvalidConfig(t *testing.T) {
	store := newFakeGate3Store()
	resolver := newGate3Resolver(t, "never")
	keys := newGate3Keys(t)

	tests := []struct {
		name   string
		config Gate3Config
		want   string
	}{
		{name: "missing store", config: Gate3Config{Resolver: resolver, Keys: keys, TTL: time.Minute}, want: "store"},
		{name: "missing resolver", config: Gate3Config{Store: store, Keys: keys, TTL: time.Minute}, want: "Resolver"},
		{name: "missing keys", config: Gate3Config{Store: store, Resolver: resolver, TTL: time.Minute}, want: "BindingKeyStore"},
		{name: "zero TTL", config: Gate3Config{Store: store, Resolver: resolver, Keys: keys, TTL: 0}, want: "TTL"},
		{name: "negative TTL", config: Gate3Config{Store: store, Resolver: resolver, Keys: keys, TTL: -time.Second}, want: "TTL"},
		{name: "TTL exceeds cap", config: Gate3Config{Store: store, Resolver: resolver, Keys: keys, TTL: 25 * time.Hour}, want: "TTL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewGate3Coordinator(test.config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewGate3Coordinator() error = %v; want mention of %q", err, test.want)
			}
		})
	}
}

func TestNewGate3CoordinatorDefaultsNow(t *testing.T) {
	coordinator, err := NewGate3Coordinator(Gate3Config{
		Store: newFakeGate3Store(), Resolver: newGate3Resolver(t, "never"), Keys: newGate3Keys(t), TTL: time.Minute,
	})
	if err != nil || coordinator.now == nil {
		t.Fatalf("NewGate3Coordinator() = %v, err %v", coordinator, err)
	}
}

func TestGate3CoordinatorWorkflowContextRequiresQualifiedDispatch(t *testing.T) {
	coordinator := newGate3Coordinator(t, "never")
	request := validGate3Request()
	request.WorkflowContext = &api.WorkflowActionContext{Workspace: "origin", RunUID: "run-uid", Step: "team",
		ActivationID: "visit-1", Attempt: 1, OwnerEpoch: 1, BindingDigest: strings.Repeat("a", 64)}
	result, err := coordinator.Resolve(context.Background(), request)
	if !errors.Is(err, ErrWorkflowDispatchUnsupported) || result.State == Gate3StateReady {
		t.Fatalf("unqualified Workflow action admitted: %+v %v", result, err)
	}
	if fake := coordinator.store.(*fakeGate3Store); fake.creates != 0 {
		t.Fatal("unqualified Workflow action created an approval")
	}
}

func TestGate3CoordinatorRegistersWorkflowEffectAndRejectsStoppedRetry(t *testing.T) {
	coordinator := newGate3Coordinator(t, "never")
	legacy, err := coordinator.Resolve(context.Background(), validGate3Request())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := WorkflowApprovalActorDigest(legacy.Approval)
	if err != nil {
		t.Fatal(err)
	}
	request := validGate3Request()
	request.InvocationID = "workflow-invocation"
	request.WorkflowContext = &api.WorkflowActionContext{Workspace: "origin", RunUID: "run-uid", Step: "team",
		ActivationID: "visit-1", Attempt: 1, OwnerEpoch: 1, BindingDigest: strings.Repeat("a", 64)}
	now := time.Now().UTC()
	activation, err := store.NewWorkflowActivation(store.WorkflowActivationSpec{Key: workflowContextKey(*request.WorkflowContext),
		BindingDigest: request.WorkflowContext.BindingDigest, Actors: []string{actor}, MaxEffects: 2, MaxBytes: 32768, ExpiresAt: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &workflowDispatchFixtureStore{activation: activation, now: now}
	dispatch, err := NewWorkflowDispatchGate(WorkflowDispatchConfig{Activations: fixture, Approvals: coordinator.store.(*fakeGate3Store),
		Keys: coordinator.keys, Clock: func() (time.Time, error) { return time.Now().UTC(), nil },
		Eligibility: func(context.Context, api.RuntimeApproval) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.workflowDispatch = dispatch
	result, err := coordinator.Resolve(context.Background(), request)
	if err != nil || result.State != Gate3StateReady || result.Approval.Spec.WorkflowContext == nil {
		t.Fatalf("qualified Workflow action = %+v %v", result, err)
	}
	effect, err := WorkflowApprovalEffect(result.Approval)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.activation.Effects[effect.ID].State != store.WorkflowEffectRegistered || result.Approval.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted {
		t.Fatal("Ready result skipped registration or claimed execution")
	}
	stopWorkflowDispatchFixture(t, fixture)
	result, err = coordinator.Resolve(context.Background(), request)
	if err == nil || result.State == Gate3StateReady {
		t.Fatalf("stopped Workflow retry regained readiness: %+v %v", result, err)
	}
}

func TestGate3CoordinatorValidateRequestRejectsMissingFields(t *testing.T) {
	base := validGate3Request()
	tests := []struct {
		name   string
		mutate func(*Gate3Request)
		want   string
	}{
		{name: "missing workspace", mutate: func(r *Gate3Request) { r.Workspace = "" }, want: "workspace"},
		{name: "missing invocation", mutate: func(r *Gate3Request) { r.InvocationID = "" }, want: "invocation"},
		{name: "missing plane", mutate: func(r *Gate3Request) { r.Plane = "" }, want: "plane"},
		{name: "missing operation", mutate: func(r *Gate3Request) { r.Operation = "" }, want: "operation"},
		{name: "missing action", mutate: func(r *Gate3Request) { r.Action = "" }, want: "action"},
		{name: "missing agent name", mutate: func(r *Gate3Request) { r.AgentName = "" }, want: "agent name"},
		{name: "missing session", mutate: func(r *Gate3Request) { r.SessionID = "" }, want: "session"},
		{name: "missing target kind", mutate: func(r *Gate3Request) { r.Target.Kind = "" }, want: "target"},
		{name: "missing target name", mutate: func(r *Gate3Request) { r.Target.Name = "" }, want: "target"},
		{name: "missing target uid", mutate: func(r *Gate3Request) { r.Target.UID = "" }, want: "target"},
	}
	coordinator := newGate3Coordinator(t, "never")
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			test.mutate(&request)
			_, err := coordinator.Resolve(context.Background(), request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Resolve() error = %v; want mention of %q", err, test.want)
			}
		})
	}
}

func TestGate3CoordinatorReadyWhenPolicyNotRequired(t *testing.T) {
	coordinator := newGate3Coordinator(t, "never")
	fake := coordinator.store.(*fakeGate3Store)

	result, err := coordinator.Resolve(context.Background(), validGate3Request())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Gate3StateReady {
		t.Fatalf("state = %q; want ready", result.State)
	}
	if !result.Approval.Status.DecisionEvidenceDelivered {
		t.Fatal("Ready result must have delivered decision evidence")
	}
	if result.Approval.Status.DecisionState != api.RuntimeApprovalDecisionNotRequired {
		t.Fatalf("decision state = %q; want NotRequired", result.Approval.Status.DecisionState)
	}
	if fake.ackCalls != 1 {
		t.Fatalf("ack calls = %d; want 1", fake.ackCalls)
	}
	if fake.creates != 1 {
		t.Fatalf("create calls = %d; want 1", fake.creates)
	}
}

func TestGate3CoordinatorPendingWhenPolicyRequiresApproval(t *testing.T) {
	coordinator := newGate3Coordinator(t, "always")
	fake := coordinator.store.(*fakeGate3Store)

	result, err := coordinator.Resolve(context.Background(), validGate3Request())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Gate3StatePending {
		t.Fatalf("state = %q; want pending", result.State)
	}
	if result.Approval.Status.DecisionState != api.RuntimeApprovalDecisionPending {
		t.Fatalf("decision state = %q; want Pending", result.Approval.Status.DecisionState)
	}
	if result.Approval.Status.DecisionEvidenceDelivered {
		t.Fatal("pending result must not deliver decision evidence")
	}
	if fake.ackCalls != 0 {
		t.Fatalf("ack calls = %d; want 0 on Pending", fake.ackCalls)
	}
}

func TestGate3CoordinatorPendingIsIdempotent(t *testing.T) {
	coordinator := newGate3Coordinator(t, "always")
	fake := coordinator.store.(*fakeGate3Store)
	request := validGate3Request()

	first, err := coordinator.Resolve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.Resolve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Approval.Spec.InvocationID != second.Approval.Spec.InvocationID {
		t.Fatalf("invocation ID drifted between retries: %q vs %q", first.Approval.Spec.InvocationID, second.Approval.Spec.InvocationID)
	}
	if first.Approval.UID != second.Approval.UID {
		t.Fatalf("RuntimeApproval UID drifted between retries: %q vs %q", first.Approval.UID, second.Approval.UID)
	}
	if second.State != Gate3StatePending {
		t.Fatalf("retry state = %q; want pending", second.State)
	}
	if fake.creates != 2 || fake.ackCalls != 0 {
		t.Fatalf("call counts = creates:%d acks:%d; want creates:2 acks:0", fake.creates, fake.ackCalls)
	}
}

func TestGate3CoordinatorReadyWhenApprovedAfterDecide(t *testing.T) {
	coordinator := newGate3Coordinator(t, "always")
	fake := coordinator.store.(*fakeGate3Store)
	fake.seedTerminal(t, validGate3Request(), api.RuntimeApprovalDecisionApproved)

	result, err := coordinator.Resolve(context.Background(), validGate3Request())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Gate3StateReady {
		t.Fatalf("state = %q; want ready", result.State)
	}
	if !result.Approval.Status.DecisionEvidenceDelivered {
		t.Fatal("Approved result must have delivered decision evidence")
	}
	if fake.ackCalls != 1 {
		t.Fatalf("ack calls = %d; want 1", fake.ackCalls)
	}
}

func TestGate3ApprovedActionSurvivesInstanceStatusUpdates(t *testing.T) {
	coordinator := newGate3Coordinator(t, "always")
	fake := coordinator.store.(*fakeGate3Store)
	fake.strictBinding = true
	request := validGate3Request()
	first, err := coordinator.Resolve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	approval := fake.approvals[request.InvocationID]
	approval.Status.DecisionState = api.RuntimeApprovalDecisionApproved
	now := time.Now().UTC()
	approval.Status.DecidedAt = &now
	PopulateDecisionAudit(approval)
	fake.mu.Unlock()

	request.AgentInstance.ResourceVersion++
	request.AgentInstance.Status.Phase = api.PhaseRunning
	request.AgentInstance.Status.SessionID = request.SessionID
	second, err := coordinator.Resolve(context.Background(), request)
	if err != nil {
		t.Fatalf("status-only instance update invalidated approval: %v", err)
	}
	if second.State != Gate3StateReady || second.Approval.UID != first.Approval.UID {
		t.Fatal("approved continuation must retain its original action and approval identity")
	}
	if second.Approval.Spec.AgentInstanceRef.ResourceVersion != first.Approval.Spec.AgentInstanceRef.ResourceVersion {
		t.Fatal("continuation must preserve the original observed resourceVersion")
	}
}

func TestGate3ConfigurationBindingRejectsMeaningfulChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Gate3Request)
	}{
		{"instance spec", func(r *Gate3Request) { r.AgentInstance.Spec.Revision++ }},
		{"instance generation", func(r *Gate3Request) { r.AgentInstance.Generation++ }},
		{"instance UID", func(r *Gate3Request) { r.AgentInstance.UID = "replacement" }},
		{"instance labels", func(r *Gate3Request) { r.AgentInstance.Labels = map[string]string{"role": "different"} }},
		{"instance annotations", func(r *Gate3Request) { r.AgentInstance.Annotations = map[string]string{"scope": "different"} }},
		{"instance ownership", func(r *Gate3Request) { r.AgentInstance.OwnerReferences = []api.OwnerReference{{UID: "different"}} }},
		{"instance deletion", func(r *Gate3Request) { now := time.Now(); r.AgentInstance.DeletionTimestamp = &now }},
		{"instance session", func(r *Gate3Request) { r.AgentInstance.Status.SessionID = "replacement" }},
		{"caller session", func(r *Gate3Request) { r.SessionID = "replacement" }},
		{"target version", func(r *Gate3Request) { r.Target.ResourceVersion++ }},
		{"target UID", func(r *Gate3Request) { r.Target.UID = "replacement" }},
		{"identity version", func(r *Gate3Request) { r.AgentIdentity.ResourceVersion++ }},
		{"arguments", func(r *Gate3Request) { r.CanonicalPayload = json.RawMessage(`{"input":"changed"}`) }},
		{"permission reference", func(r *Gate3Request) {
			r.AuthorizationRefs = []api.RuntimeApprovalReference{{Kind: "ToolGrant", Scope: "Namespaced",
				Namespace: "team-a", Name: "other", UID: "other", ResourceVersion: 1}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator := newGate3Coordinator(t, "always")
			coordinator.store.(*fakeGate3Store).strictBinding = true
			request := validGate3Request()
			if _, err := coordinator.Resolve(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			test.mutate(&request)
			if _, err := coordinator.Resolve(context.Background(), request); err == nil {
				t.Fatal("changed execution configuration or authorization reused an approval")
			}
		})
	}
}

func TestGate3ConfigurationDigestIsCanonicalAndContainsNoConfigurationPayload(t *testing.T) {
	coordinator := newGate3Coordinator(t, "always")
	request := validGate3Request()
	request.AgentInstance.Spec.SessionContext = map[string]string{"b": "second", "a": "sensitive prompt"}
	first, err := coordinator.Resolve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.AgentInstance.Spec.SessionContext = map[string]string{"a": "sensitive prompt", "b": "second"}
	request.AgentInstance.ResourceVersion++
	coordinator.store.(*fakeGate3Store).strictBinding = true
	second, err := coordinator.Resolve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Approval.Spec.AgentInstanceRef.ConfigurationDigest != second.Approval.Spec.AgentInstanceRef.ConfigurationDigest {
		t.Fatal("map order changed canonical configuration digest")
	}
	encoded, err := json.Marshal(first.Approval)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sensitive prompt") {
		t.Fatal("approval persisted private configuration rather than its digest")
	}
}

func TestGate3CoordinatorRejectedDeliversEvidence(t *testing.T) {
	coordinator := newGate3Coordinator(t, "always")
	fake := coordinator.store.(*fakeGate3Store)
	fake.seedTerminal(t, validGate3Request(), api.RuntimeApprovalDecisionRejected)

	result, err := coordinator.Resolve(context.Background(), validGate3Request())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Gate3StateRejected {
		t.Fatalf("state = %q; want rejected", result.State)
	}
	if !result.Approval.Status.DecisionEvidenceDelivered {
		t.Fatal("Rejected result must have delivered decision evidence")
	}
}

func TestGate3CoordinatorExpiredDeliversEvidence(t *testing.T) {
	coordinator := newGate3Coordinator(t, "always")
	fake := coordinator.store.(*fakeGate3Store)
	fake.seedTerminal(t, validGate3Request(), api.RuntimeApprovalDecisionExpired)

	result, err := coordinator.Resolve(context.Background(), validGate3Request())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Gate3StateExpired {
		t.Fatalf("state = %q; want expired", result.State)
	}
	if !result.Approval.Status.DecisionEvidenceDelivered {
		t.Fatal("Expired result must have delivered decision evidence")
	}
}

func TestGate3CoordinatorSkipsAckWhenEvidenceAlreadyDelivered(t *testing.T) {
	coordinator := newGate3Coordinator(t, "always")
	fake := coordinator.store.(*fakeGate3Store)
	fake.seedTerminal(t, validGate3Request(), api.RuntimeApprovalDecisionApproved)
	// Simulate a prior successful acknowledgement.
	stored := fake.approvals[validGate3Request().InvocationID]
	stored.Status.DecisionEvidenceDelivered = true

	result, err := coordinator.Resolve(context.Background(), validGate3Request())
	if err != nil {
		t.Fatal(err)
	}
	if result.State != Gate3StateReady {
		t.Fatalf("state = %q; want ready", result.State)
	}
	if fake.ackCalls != 0 {
		t.Fatalf("ack calls = %d; want 0 when evidence already delivered", fake.ackCalls)
	}
}

func TestGate3CoordinatorTerminalMissingAuditIsSurfaced(t *testing.T) {
	coordinator := newGate3Coordinator(t, "always")
	fake := coordinator.store.(*fakeGate3Store)
	fake.seedTerminal(t, validGate3Request(), api.RuntimeApprovalDecisionRejected)
	// Corrupt the seeded audit so the coordinator must refuse to ack blindly.
	fake.approvals[validGate3Request().InvocationID].Status.DecisionAudit = nil

	_, err := coordinator.Resolve(context.Background(), validGate3Request())
	if err == nil || !strings.Contains(err.Error(), "audit evidence") {
		t.Fatalf("Resolve() error = %v; want missing audit evidence", err)
	}
}

func TestGate3CoordinatorSurfacesResolverFailure(t *testing.T) {
	coordinator, err := NewGate3Coordinator(Gate3Config{
		Store: newFakeGate3Store(),
		// Resolver whose store cannot find the agent.
		Resolver: NewResolver(&resolverStore{}),
		Keys:     newGate3Keys(t),
		TTL:      time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Resolve(context.Background(), validGate3Request())
	if !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("Resolve() error = %v; want ErrPolicyUnavailable", err)
	}
}

func TestGate3CoordinatorSurfacesBindFailure(t *testing.T) {
	coordinator, err := NewGate3Coordinator(Gate3Config{
		Store:    newFakeGate3Store(),
		Resolver: newGate3Resolver(t, "never"),
		Keys:     brokenKeys{},
		TTL:      time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Resolve(context.Background(), validGate3Request())
	if !errors.Is(err, ErrBindingKeyUnavailable) {
		t.Fatalf("Resolve() error = %v; want ErrBindingKeyUnavailable", err)
	}
}

func TestGate3CoordinatorSurfacesStoreFailure(t *testing.T) {
	coordinator := newGate3Coordinator(t, "never")
	fake := coordinator.store.(*fakeGate3Store)
	fake.createErr = errors.New("simulated backend failure")

	_, err := coordinator.Resolve(context.Background(), validGate3Request())
	if err == nil || !strings.Contains(err.Error(), "simulated backend failure") {
		t.Fatalf("Resolve() error = %v; want to surface store failure", err)
	}
}

func TestGate3CoordinatorReadyPopulatesBinding(t *testing.T) {
	coordinator := newGate3Coordinator(t, "never")

	result, err := coordinator.Resolve(context.Background(), validGate3Request())
	if err != nil {
		t.Fatal(err)
	}
	binding := result.Approval.Spec.Binding
	if binding.Format != BindingFormatV1 || binding.Algorithm != BindingAlgorithmV1 || binding.Domain != BindingDomainV1 {
		t.Fatalf("binding = %#v; want kognition v1 binding", binding)
	}
	if binding.KeyVersion <= 0 || binding.Epoch <= 0 || binding.Tag == "" {
		t.Fatalf("binding = %#v; want complete key metadata", binding)
	}
}

// Helpers

func newGate3Coordinator(t *testing.T, toolPolicy string) *Gate3Coordinator {
	t.Helper()
	coordinator, err := NewGate3Coordinator(Gate3Config{
		Store:    newFakeGate3Store(),
		Resolver: newGate3Resolver(t, toolPolicy),
		Keys:     newGate3Keys(t),
		TTL:      time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func newGate3Resolver(t *testing.T, toolPolicy string) *Resolver {
	t.Helper()
	return NewResolver(&resolverStore{
		agent: resolverAgent(),
		class: api.AutonomyClass{
			TypeMeta:   api.TypeMeta{Kind: "AutonomyClass", APIVersion: api.APIVersion},
			ObjectMeta: api.ObjectMeta{Name: "assisted", UID: "class-uid", ResourceVersion: 9},
			Spec: api.AutonomyClassSpec{
				ApprovalPolicy: &api.ApprovalPolicy{ToolInvocation: toolPolicy, MemoryWrite: toolPolicy, ExternalCommunication: toolPolicy},
			},
		},
		profiles: []api.TrustProfile{resolverProfile("profile-a", "agent-uid", "assisted")},
	})
}

func newGate3Keys(t *testing.T) BindingKeyStore {
	t.Helper()
	keys, err := NewStaticBindingKeyStore([]BindingKey{{
		Version: 1, Epoch: 1, Algorithm: BindingAlgorithmV1, Domain: BindingDomainV1, Status: BindingKeyActive,
		Material: []byte("0123456789abcdef0123456789abcdef"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func validGate3Request() Gate3Request {
	return Gate3Request{
		Workspace:    "team-a",
		Namespace:    "team-a",
		InvocationID: "invocation-a",
		Plane:        api.RuntimeApprovalPlaneTool,
		Operation:    "invoke",
		Action:       ActionToolInvocation,
		AgentName:    "agent-a",
		AgentIdentity: api.AgentIdentity{
			TypeMeta:   api.TypeMeta{Kind: "AgentIdentity", APIVersion: api.APIVersion},
			ObjectMeta: api.ObjectMeta{Name: "agent-a", Namespace: "team-a", UID: "identity-uid", ResourceVersion: 2},
		},
		AgentInstance: api.AgentInstance{
			TypeMeta:   api.TypeMeta{Kind: "AgentInstance", APIVersion: api.APIVersion},
			ObjectMeta: api.ObjectMeta{Name: "agent-a-1", Namespace: "team-a", UID: "instance-uid", ResourceVersion: 3},
		},
		SessionID: "session-a",
		Target: api.RuntimeApprovalReference{
			Kind: "Tool", Scope: "Namespaced", Namespace: "team-a", Name: "tool-a", UID: "tool-uid", ResourceVersion: 4,
		},
		CanonicalPayload: json.RawMessage(`{"input":"x"}`),
	}
}

type fakeGate3Store struct {
	mu            sync.Mutex
	approvals     map[string]*api.RuntimeApproval
	rv            int64
	creates       int
	ackCalls      int
	createErr     error
	strictBinding bool
	now           func() time.Time
}

func newFakeGate3Store() *fakeGate3Store {
	return &fakeGate3Store{approvals: map[string]*api.RuntimeApproval{}, now: time.Now}
}

func (s *fakeGate3Store) GetRuntimeApprovalByInvocation(_ context.Context, workspace, invocationID string) (api.RuntimeApproval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	approval, found := s.approvals[invocationID]
	if !found || approval.Spec.Workspace != workspace {
		return api.RuntimeApproval{}, store.ErrNotFound
	}
	return *approval, nil
}

func (s *fakeGate3Store) CreateOrGetRuntimeApproval(_ context.Context, proposed api.RuntimeApproval, _ json.RawMessage, ttl time.Duration) (bool, api.RuntimeApproval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	if s.createErr != nil {
		return false, api.RuntimeApproval{}, s.createErr
	}
	if existing, ok := s.approvals[proposed.Spec.InvocationID]; ok {
		if s.strictBinding && !SameImmutableBinding(*existing, proposed) {
			return false, api.RuntimeApproval{}, errors.New("resource version conflict")
		}
		return false, *existing, nil
	}
	s.rv++
	stored := proposed
	stored.UID = "approval-" + proposed.Spec.InvocationID + "-uid"
	stored.ResourceVersion = s.rv
	stored.Spec.CreatedAt = s.now().UTC()
	stored.Spec.ExpiresAt = stored.Spec.CreatedAt.Add(ttl)
	if stored.Status.DecisionState == api.RuntimeApprovalDecisionNotRequired {
		FinalizeInitialStatus(&stored, s.now())
	}
	s.approvals[proposed.Spec.InvocationID] = &stored
	return true, stored, nil
}

func (s *fakeGate3Store) AcknowledgeRuntimeApprovalDecisionAudit(_ context.Context, workspace, invocationID string, expectedRV int64, auditID string) (api.RuntimeApproval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ackCalls++
	stored, ok := s.approvals[invocationID]
	if !ok || stored.Spec.Workspace != workspace {
		return api.RuntimeApproval{}, errors.New("fake: unknown approval")
	}
	if stored.ResourceVersion != expectedRV {
		return api.RuntimeApproval{}, errors.New("fake: stale resource version")
	}
	if stored.Status.DecisionAudit == nil || stored.Status.DecisionAudit.ID != auditID {
		return api.RuntimeApproval{}, errors.New("fake: audit id mismatch")
	}
	s.rv++
	stored.ResourceVersion = s.rv
	stored.Status.DecisionEvidenceDelivered = true
	return *stored, nil
}

// seedTerminal pre-populates the store with an approval that has already
// reached the given terminal decision state and carries populated audit
// evidence. This mirrors the state a real store presents after a prior
// DecideRuntimeApproval or ExpireRuntimeApprovalDecision call.
func (s *fakeGate3Store) seedTerminal(t *testing.T, request Gate3Request, decision string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rv++
	decidedAt := s.now().UTC()
	approval := api.RuntimeApproval{
		ObjectMeta: api.ObjectMeta{
			Name:            CanonicalName(request.Workspace, request.InvocationID),
			Namespace:       request.Namespace,
			UID:             "approval-" + request.InvocationID + "-uid",
			ResourceVersion: s.rv,
		},
		Spec: api.RuntimeApprovalSpec{
			Workspace: request.Workspace, InvocationID: request.InvocationID,
			InitialDecision: api.RuntimeApprovalDecisionPending, Plane: request.Plane, Operation: request.Operation,
			SessionRef: request.SessionID, CreatedAt: decidedAt, ExpiresAt: decidedAt.Add(time.Minute),
		},
		Status: api.RuntimeApprovalStatus{
			DecisionState:  decision,
			ExecutionState: api.RuntimeApprovalExecutionNotStarted,
			DecidedAt:      &decidedAt,
		},
	}
	PopulateDecisionAudit(&approval)
	s.approvals[request.InvocationID] = &approval
}

type brokenKeys struct{}

func (brokenKeys) Active(context.Context) (BindingKey, error) {
	return BindingKey{}, ErrBindingKeyUnavailable
}
func (brokenKeys) ByVersion(context.Context, int64) (BindingKey, error) {
	return BindingKey{}, ErrBindingKeyUnavailable
}
func (brokenKeys) All(context.Context) ([]BindingKey, error) {
	return nil, ErrBindingKeyUnavailable
}
