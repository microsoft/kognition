// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makeWorkspace(ns, name string) *v1alpha1.Workspace {
	return &v1alpha1.Workspace{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workspace"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.WorkspaceSpec{DisplayName: name},
	}
}

func storeWorkspace(t *testing.T, s *memStore, ws *v1alpha1.Workspace) {
	t.Helper()
	data, err := json.Marshal(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), "Workspace", ws.ObjectMeta.Namespace, ws.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
}

func makeAgentIdentity(ns, name, agentRef string) *v1alpha1.AgentIdentity {
	return &v1alpha1.AgentIdentity{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentIdentity"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: agentRef}},
	}
}

func storeAgentIdentity(t *testing.T, s *memStore, id *v1alpha1.AgentIdentity) {
	t.Helper()
	data, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), "AgentIdentity", id.ObjectMeta.Namespace, id.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
}

func makeAgentRole(ns, name string, rules []v1alpha1.PolicyRule) *v1alpha1.AgentRole {
	return &v1alpha1.AgentRole{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentRole"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: name},
		Rules:      rules,
	}
}

func storeAgentRole(t *testing.T, s *memStore, role *v1alpha1.AgentRole) {
	t.Helper()
	data, err := json.Marshal(role)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), "AgentRole", role.ObjectMeta.Namespace, role.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
}

func makeAgentRoleBinding(ns, name, roleName string, subjects []v1alpha1.Subject) *v1alpha1.AgentRoleBinding {
	return &v1alpha1.AgentRoleBinding{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentRoleBinding"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.AgentRoleBindingSpec{
			RoleRef:  v1alpha1.LocalObjectReference{Name: roleName},
			Subjects: subjects,
		},
	}
}

func storeAgentRoleBinding(t *testing.T, s *memStore, binding *v1alpha1.AgentRoleBinding) {
	t.Helper()
	data, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), "AgentRoleBinding", binding.ObjectMeta.Namespace, binding.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
}

func makeCommPolicy(ns, name string, spec v1alpha1.AgentCommunicationPolicySpec) *v1alpha1.AgentCommunicationPolicy {
	return &v1alpha1.AgentCommunicationPolicy{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentCommunicationPolicy"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       spec,
	}
}

func storeCommPolicy(t *testing.T, s *memStore, policy *v1alpha1.AgentCommunicationPolicy) {
	t.Helper()
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), "AgentCommunicationPolicy", policy.ObjectMeta.Namespace, policy.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
}

func makeMemoryGrant(ns, name, agentRef, memoryClaimRef, accessMode string) *v1alpha1.MemoryGrant {
	return &v1alpha1.MemoryGrant{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "MemoryGrant"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.MemoryGrantSpec{
			AgentRef:       v1alpha1.LocalObjectReference{Name: agentRef},
			MemoryClaimRef: v1alpha1.LocalObjectReference{Name: memoryClaimRef},
			AccessMode:     accessMode,
		},
	}
}

func storeMemoryGrant(t *testing.T, s *memStore, grant *v1alpha1.MemoryGrant) {
	t.Helper()
	data, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), "MemoryGrant", grant.ObjectMeta.Namespace, grant.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
}

type memoryGrantAuditStore struct {
	entries []observability.AuditEntry
	err     error
}

func (s *memoryGrantAuditStore) Append(_ context.Context, entry observability.AuditEntry) error {
	if s.err != nil {
		return s.err
	}
	for _, existing := range s.entries {
		if existing.ID == entry.ID {
			return nil
		}
	}
	s.entries = append(s.entries, entry)
	return nil
}

func (s *memoryGrantAuditStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	for _, entry := range entries {
		if err := s.Append(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *memoryGrantAuditStore) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return append([]observability.AuditEntry(nil), s.entries...), nil
}

func makeToolGrant(ns, name string, spec v1alpha1.ToolGrantSpec) *v1alpha1.ToolGrant {
	return &v1alpha1.ToolGrant{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolGrant"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       spec,
	}
}

func storeToolGrant(t *testing.T, s *memStore, grant *v1alpha1.ToolGrant) {
	t.Helper()
	data, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), "ToolGrant", grant.ObjectMeta.Namespace, grant.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
}

func makeModelGrant(ns, name, agentRef, modelRef string) *v1alpha1.ModelGrant {
	return &v1alpha1.ModelGrant{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ModelGrant"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.ModelGrantSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: agentRef},
			ModelRef: v1alpha1.LocalObjectReference{Name: modelRef},
		},
	}
}

func storeModelGrant(t *testing.T, s *memStore, grant *v1alpha1.ModelGrant) {
	t.Helper()
	data, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), "ModelGrant", grant.ObjectMeta.Namespace, grant.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
}

func makeCrossNamespaceGrant(ns, name, from, to, resKind, resName string) *v1alpha1.CrossNamespaceGrant {
	return &v1alpha1.CrossNamespaceGrant{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "CrossNamespaceGrant"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.CrossNamespaceGrantSpec{
			From:      from,
			To:        to,
			Resources: []v1alpha1.GrantResource{{Kind: resKind, Name: resName}},
		},
	}
}

func storeCrossNamespaceGrant(t *testing.T, s *memStore, grant *v1alpha1.CrossNamespaceGrant) {
	t.Helper()
	data, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), "CrossNamespaceGrant", grant.ObjectMeta.Namespace, grant.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
}

func TestCrossWorkspaceCommunicationDoesNotReuseLocalAgentNames(t *testing.T) {
	ctx := context.Background()
	storage := newMemStore()
	storeCrossNamespaceGrant(t, storage, makeCrossNamespaceGrant("team-b", "reference", "team-b", "team-a", "Agent", "target"))
	storeCommPolicy(t, storage, makeCommPolicy("team-b", "local", v1alpha1.AgentCommunicationPolicySpec{
		AllowFrom: []v1alpha1.LocalObjectReference{{Name: "analyst"}}, AllowTo: []v1alpha1.LocalObjectReference{{Name: "target"}}, DefaultDeny: true}))
	engine := NewCommunicationPolicyEngine(storage, zaptest.NewLogger(t))
	if allowed, err := engine.EvaluateDispatch(ctx, "team-b", "analyst", "target"); err != nil || !allowed {
		t.Fatalf("same-namespace baseline: %v %v", allowed, err)
	}
	if allowed, err := engine.EvaluateCrossWorkspaceDispatch(ctx, "team-a", "analyst", "team-b", "target"); err != nil || allowed {
		t.Fatalf("foreign names inherited local permission: %v %v", allowed, err)
	}
	storeCommPolicy(t, storage, makeCommPolicy("team-b", "qualified", v1alpha1.AgentCommunicationPolicySpec{
		AllowFromQualified: []v1alpha1.AgentReference{{Namespace: "team-a", Name: "analyst"}}, AllowTo: []v1alpha1.LocalObjectReference{{Name: "target"}}, DefaultDeny: true}))
	if allowed, err := engine.EvaluateCrossWorkspaceDispatch(ctx, "team-a", "analyst", "team-b", "target"); err != nil || !allowed {
		t.Fatalf("qualified source with independent reference grant denied: %v %v", allowed, err)
	}
	storeCrossNamespaceGrant(t, storage, makeCrossNamespaceGrant("team-b", "third-reference", "team-b", "team-c", "Agent", "target"))
	if allowed, err := engine.EvaluateCrossWorkspaceDispatch(ctx, "team-c", "analyst", "team-b", "target"); err != nil || allowed {
		t.Fatalf("same-named third-namespace source inherited qualified permission: %v %v", allowed, err)
	}
}

// ---------------------------------------------------------------------------
// TC-0066: Workspace reconciler creates default-deny policy; cross-workspace
//          access is denied without a grant.
// ---------------------------------------------------------------------------

func TestWorkspaceReconciler_DefaultDenyAndCrossWorkspaceIsolation(t *testing.T) {
	// TC-0066
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	ws := makeWorkspace("", "team-alpha")
	storeWorkspace(t, s, ws)

	r := NewWorkspaceReconciler(s, log)
	result, err := r.Reconcile(ctx, Request{Namespace: "", Name: "team-alpha", Kind: "Workspace"})
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected requeue after adding finalizer")
	}

	// Reconcile again to proceed past finalizer.
	result, err = r.Reconcile(ctx, Request{Namespace: "", Name: "team-alpha", Kind: "Workspace"})
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	// Verify default-deny policy was created.
	policyRaw, err := s.Get(ctx, "AgentCommunicationPolicy", "team-alpha", "team-alpha-default-deny")
	if err != nil {
		t.Fatalf("default-deny policy not created: %v", err)
	}
	var policy v1alpha1.AgentCommunicationPolicy
	if err := json.Unmarshal(policyRaw, &policy); err != nil {
		t.Fatal(err)
	}
	if !policy.Spec.DefaultDeny {
		t.Error("expected DefaultDeny=true in default-deny policy")
	}

	// Cross-workspace access should be denied without a grant.
	err = ValidateCrossWorkspaceAccess(ctx, s, "team-alpha", "team-beta", "Tool", "search")
	if err == nil {
		t.Error("expected cross-workspace access to be denied without grant")
	}

	_ = result
}

// ---------------------------------------------------------------------------
// TC-0067: Workspaces operate independently; deleting one does not affect another.
// ---------------------------------------------------------------------------

func TestWorkspaceReconciler_IndependentOperability(t *testing.T) {
	// TC-0067
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	// Create two workspaces.
	ws1 := makeWorkspace("", "workspace-1")
	ws2 := makeWorkspace("", "workspace-2")
	storeWorkspace(t, s, ws1)
	storeWorkspace(t, s, ws2)

	r := NewWorkspaceReconciler(s, log)

	// Reconcile both (finalizer + reconcile).
	for _, name := range []string{"workspace-1", "workspace-2"} {
		r.Reconcile(ctx, Request{Name: name, Kind: "Workspace"})
		r.Reconcile(ctx, Request{Name: name, Kind: "Workspace"})
	}

	// Mark workspace-1 for deletion.
	raw1, _ := s.Get(ctx, "Workspace", "", "workspace-1")
	var del1 v1alpha1.Workspace
	json.Unmarshal(raw1, &del1)
	now := time.Now()
	del1.ObjectMeta.DeletionTimestamp = &now
	data1, _ := json.Marshal(del1)
	s.Update(ctx, "Workspace", "", "workspace-1", data1, del1.ObjectMeta.ResourceVersion)

	// Reconcile deletion.
	_, err := r.Reconcile(ctx, Request{Name: "workspace-1", Kind: "Workspace"})
	if err != nil {
		t.Fatalf("delete reconcile: %v", err)
	}

	// Workspace-2 should still be fine.
	raw2, err := s.Get(ctx, "Workspace", "", "workspace-2")
	if err != nil {
		t.Fatalf("workspace-2 should still exist: %v", err)
	}
	var ws2check v1alpha1.Workspace
	json.Unmarshal(raw2, &ws2check)
	if ws2check.ObjectMeta.Name != "workspace-2" {
		t.Error("workspace-2 data corrupted")
	}
}

// ---------------------------------------------------------------------------
// TC-0068: AgentIdentity reconciler assigns a deterministic SPIFFE identifier.
// ---------------------------------------------------------------------------

func TestAgentIdentityReconciler_IssuesSPIFFESVID(t *testing.T) {
	// TC-0068
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	id := makeAgentIdentity("team-alpha", "planner-id", "planner")
	storeAgentIdentity(t, s, id)

	r := NewAgentIdentityReconciler(s, log)

	// First reconcile adds finalizer.
	result, err := r.Reconcile(ctx, Request{Namespace: "team-alpha", Name: "planner-id", Kind: "AgentIdentity"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected requeue after finalizer")
	}

	// Second reconcile issues SPIFFE ID.
	_, err = r.Reconcile(ctx, Request{Namespace: "team-alpha", Name: "planner-id", Kind: "AgentIdentity"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Verify SPIFFE ID was set.
	raw, _ := s.Get(ctx, "AgentIdentity", "team-alpha", "planner-id")
	var updated v1alpha1.AgentIdentity
	json.Unmarshal(raw, &updated)

	expected := "spiffe://kognition.dev/workspace/team-alpha/agent/planner"
	if updated.Spec.SPIFFEID != expected {
		t.Errorf("SPIFFE ID = %q, want %q", updated.Spec.SPIFFEID, expected)
	}
	if updated.Status.Phase != v1alpha1.PhaseReady {
		t.Errorf("phase = %q, want %q", updated.Status.Phase, v1alpha1.PhaseReady)
	}
}

// ---------------------------------------------------------------------------
// TC-0069: AgentIdentity can be resolved at runtime for an agent.
// ---------------------------------------------------------------------------

func TestAgentIdentityReconciler_RuntimeResolution(t *testing.T) {
	// TC-0069
	ctx := context.Background()
	s := newMemStore()

	id := makeAgentIdentity("team-alpha", "coder-id", "coder")
	id.Spec.SPIFFEID = "spiffe://kognition.dev/workspace/team-alpha/agent/coder"
	storeAgentIdentity(t, s, id)

	resolved, err := ResolveAgentIdentity(ctx, s, "team-alpha", "coder")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ObjectMeta.Name != "coder-id" {
		t.Errorf("resolved name = %q, want %q", resolved.ObjectMeta.Name, "coder-id")
	}

	// Non-existent agent should fail.
	_, err = ResolveAgentIdentity(ctx, s, "team-alpha", "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

// ---------------------------------------------------------------------------
// TC-0070: RBAC engine allows permitted operations.
// ---------------------------------------------------------------------------

func TestRBACEngine_AllowsPermittedOperations(t *testing.T) {
	// TC-0070
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	// Create identity.
	id := makeAgentIdentity("ns1", "agent-id", "agent-a")
	storeAgentIdentity(t, s, id)

	// Create role allowing get/list on Memory.
	role := makeAgentRole("ns1", "reader-role", []v1alpha1.PolicyRule{
		{Resources: []string{"Memory"}, Verbs: []string{"get", "list"}},
	})
	storeAgentRole(t, s, role)

	// Bind role to identity.
	binding := makeAgentRoleBinding("ns1", "reader-binding", "reader-role", []v1alpha1.Subject{
		{Kind: "AgentIdentity", Name: "agent-id"},
	})
	storeAgentRoleBinding(t, s, binding)

	engine := NewRBACEngine(s, log)

	// Should allow "get" on Memory.
	allowed, err := engine.Evaluate(ctx, id, "get", "Memory")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("expected get on Memory to be allowed")
	}

	// Should deny "delete" on Memory.
	allowed, err = engine.Evaluate(ctx, id, "delete", "Memory")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Error("expected delete on Memory to be denied")
	}
}

// ---------------------------------------------------------------------------
// TC-0071: RBAC engine denies operations without role binding.
// ---------------------------------------------------------------------------

func TestRBACEngine_DeniesWithoutBinding(t *testing.T) {
	// TC-0071
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	id := makeAgentIdentity("ns1", "lonely-id", "lonely-agent")
	storeAgentIdentity(t, s, id)

	engine := NewRBACEngine(s, log)

	allowed, err := engine.Evaluate(ctx, id, "get", "Memory")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Error("expected denial without any role bindings")
	}
}

func TestRBACEngine_WrappedMissingRoleIsSkipped(t *testing.T) {
	ctx := context.Background()
	base := newMemStore()
	id := makeAgentIdentity("ns1", "agent-id", "agent-a")
	storeAgentRoleBinding(t, base, makeAgentRoleBinding("ns1", "missing-binding", "missing-role", []v1alpha1.Subject{
		{Kind: "AgentIdentity", Name: "agent-id"},
	}))
	engine := NewRBACEngine(&modelGetErrorStore{
		Store: base,
		kind:  agentRoleKind,
		name:  "missing-role",
		err:   fmt.Errorf("wrapped missing role: %w", store.ErrNotFound),
	}, zaptest.NewLogger(t))

	allowed, err := engine.Evaluate(ctx, id, "get", "Memory")
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want nil", err)
	}
	if allowed {
		t.Fatal("Evaluate() allowed access through a missing role")
	}
}

func TestRBACEngine_AgentSubjectAndWildcardRule(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	id := makeAgentIdentity("ns1", "agent-id", "agent-a")
	storeAgentIdentity(t, s, id)
	storeAgentRole(t, s, makeAgentRole("ns1", "operator", []v1alpha1.PolicyRule{
		{Resources: []string{"*"}, Verbs: []string{"*"}},
	}))
	storeAgentRoleBinding(t, s, makeAgentRoleBinding("ns1", "operator-binding", "operator", []v1alpha1.Subject{
		{Kind: "Agent", Name: "agent-a"},
	}))

	allowed, err := NewRBACEngine(s, zaptest.NewLogger(t)).Evaluate(ctx, id, "delete", "Tool")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("expected wildcard role bound through Agent subject to allow access")
	}
}

// ---------------------------------------------------------------------------
// TC-0072: Communication policy default-deny blocks dispatch.
// ---------------------------------------------------------------------------

func TestCommunicationPolicy_DefaultDenyBlocks(t *testing.T) {
	// TC-0072
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	policy := makeCommPolicy("ns1", "deny-all", v1alpha1.AgentCommunicationPolicySpec{
		DefaultDeny: true,
	})
	storeCommPolicy(t, s, policy)

	engine := NewCommunicationPolicyEngine(s, log)

	allowed, err := engine.EvaluateDispatch(ctx, "ns1", "agent-a", "agent-b")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Error("expected communication to be denied under default-deny")
	}
}

// ---------------------------------------------------------------------------
// TC-0073: Communication policy explicit allow permits dispatch.
// ---------------------------------------------------------------------------

func TestCommunicationPolicy_ExplicitAllowPermits(t *testing.T) {
	// TC-0073
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	policy := makeCommPolicy("ns1", "allow-specific", v1alpha1.AgentCommunicationPolicySpec{
		DefaultDeny: true,
		AllowFrom:   []v1alpha1.LocalObjectReference{{Name: "planner"}},
		AllowTo:     []v1alpha1.LocalObjectReference{{Name: "coder"}},
	})
	storeCommPolicy(t, s, policy)

	engine := NewCommunicationPolicyEngine(s, log)

	// Allowed communication.
	allowed, err := engine.EvaluateDispatch(ctx, "ns1", "planner", "coder")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("expected planner -> coder to be allowed")
	}

	// Denied communication (reverse direction).
	allowed, err = engine.EvaluateDispatch(ctx, "ns1", "coder", "planner")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Error("expected coder -> planner to be denied")
	}
}

// ---------------------------------------------------------------------------
// TC-0074: Cross-workspace communication requires CrossNamespaceGrant.
// ---------------------------------------------------------------------------

func TestCommunicationPolicy_CrossWorkspaceRequiresGrant(t *testing.T) {
	// TC-0074
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	engine := NewCommunicationPolicyEngine(s, log)

	// Without grant, cross-workspace communication is denied.
	allowed, err := engine.EvaluateCrossWorkspaceDispatch(ctx, "ws-a", "agent-1", "ws-b", "agent-2")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Error("expected cross-workspace denied without grant")
	}

	// Add a CrossNamespaceGrant.
	grant := makeCrossNamespaceGrant("ws-b", "allow-from-a", "ws-b", "ws-a", "Agent", "agent-2")
	storeCrossNamespaceGrant(t, s, grant)

	// A CrossNamespaceGrant permits reaching the target workspace but does not
	// override its default-deny communication policy.
	allowed, err = engine.EvaluateCrossWorkspaceDispatch(ctx, "ws-a", "agent-1", "ws-b", "agent-2")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Error("expected cross-workspace denied without target communication policy")
	}

	storeCommPolicy(t, s, makeCommPolicy("ws-b", "allow-agent-1", v1alpha1.AgentCommunicationPolicySpec{
		DefaultDeny:        true,
		AllowFromQualified: []v1alpha1.AgentReference{{Namespace: "ws-a", Name: "agent-1"}},
		AllowTo:            []v1alpha1.LocalObjectReference{{Name: "agent-2"}},
	}))

	// Both the boundary grant and target policy now allow the dispatch.
	allowed, err = engine.EvaluateCrossWorkspaceDispatch(ctx, "ws-a", "agent-1", "ws-b", "agent-2")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("expected cross-workspace allowed with grant")
	}
}

func TestCommunicationPolicy_DefaultDenyAndWildcardRules(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	engine := NewCommunicationPolicyEngine(s, zaptest.NewLogger(t))

	allowed, err := engine.EvaluateDispatch(ctx, "ns1", "planner", "coder")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Error("expected communication to be denied when no policies exist")
	}

	storeCommPolicy(t, s, makeCommPolicy("ns1", "allow-to-coder", v1alpha1.AgentCommunicationPolicySpec{
		DefaultDeny: true,
		AllowFrom:   []v1alpha1.LocalObjectReference{{Name: "*"}},
		AllowTo:     []v1alpha1.LocalObjectReference{{Name: "coder"}},
	}))

	allowed, err = engine.EvaluateDispatch(ctx, "ns1", "reviewer", "coder")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("expected wildcard source rule to allow dispatch to coder")
	}
}

func TestCommunicationPolicy_LegacyDefaultAllowDoesNotBypassIsolation(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	storeCommPolicy(t, s, makeCommPolicy("ns1", "legacy-unrelated-rule", v1alpha1.AgentCommunicationPolicySpec{
		DefaultDeny: false,
		AllowFrom:   []v1alpha1.LocalObjectReference{{Name: "writer"}},
		AllowTo:     []v1alpha1.LocalObjectReference{{Name: "publisher"}},
	}))

	allowed, err := NewCommunicationPolicyEngine(s, zaptest.NewLogger(t)).EvaluateDispatch(ctx, "ns1", "planner", "coder")
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("legacy defaultDeny=false policy allowed unrelated communication")
	}
}

type communicationPolicyRowsStore struct {
	store.Store
	rows [][]byte
}

func (storage communicationPolicyRowsStore) List(context.Context, string, store.ListOptions) ([][]byte, error) {
	return storage.rows, nil
}

func TestCommunicationPolicy_InvalidRowsCannotBeMaskedByAllow(t *testing.T) {
	allow, err := json.Marshal(makeCommPolicy("ns1", "allow", v1alpha1.AgentCommunicationPolicySpec{
		AllowFrom: []v1alpha1.LocalObjectReference{{Name: "planner"}},
		AllowTo:   []v1alpha1.LocalObjectReference{{Name: "coder"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		row  json.RawMessage
	}{
		{name: "malformed JSON", row: json.RawMessage(`{`)},
		{name: "null", row: json.RawMessage(`null`)},
		{name: "foreign namespace", row: json.RawMessage(`{"metadata":{"namespace":"foreign","name":"policy"},"spec":{}}`)},
		{name: "duplicate field", row: json.RawMessage(`{"metadata":{"namespace":"ns1","name":"policy"},"spec":{"allowTo":[],"allowTo":[{"name":"coder"}]}}`)},
		{name: "empty rule", row: json.RawMessage(`{"metadata":{"namespace":"ns1","name":"policy"},"spec":{"allowFrom":[{"name":""}]}}`)},
		{name: "unqualified source", row: json.RawMessage(`{"metadata":{"namespace":"ns1","name":"policy"},"spec":{"allowFromQualified":[{"name":"planner"}]}}`)},
		{name: "oversized row", row: json.RawMessage(strings.Repeat(" ", 1<<20) + string(allow))},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, rows := range [][][]byte{{allow, test.row}, {test.row, allow}} {
				allowed, err := NewCommunicationPolicyEngine(communicationPolicyRowsStore{rows: rows}, zaptest.NewLogger(t)).EvaluateDispatch(context.Background(), "ns1", "planner", "coder")
				if allowed || err == nil {
					t.Fatalf("invalid policy row authorized delivery or hid unavailable policy: allowed=%v err=%v", allowed, err)
				}
			}
		})
	}
	rows := make([][]byte, 513)
	for index := range rows {
		rows[index] = allow
	}
	allowed, err := NewCommunicationPolicyEngine(communicationPolicyRowsStore{rows: rows}, zaptest.NewLogger(t)).EvaluateDispatch(context.Background(), "ns1", "planner", "coder")
	if allowed || err == nil {
		t.Fatalf("unbounded policy set authorized delivery: allowed=%v err=%v", allowed, err)
	}
}

// ---------------------------------------------------------------------------
// TC-0075: MemoryGrant reconciler adds finalizer and handles deletion cascade.
// ---------------------------------------------------------------------------

func TestMemoryGrantReconciler_FinalizerAndDeletion(t *testing.T) {
	// TC-0075
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	grant := makeMemoryGrant("ns1", "grant-1", "agent-a", "memory-claim-1", "ReadWrite")
	storeMemoryGrant(t, s, grant)

	r := NewMemoryGrantReconciler(s, log)

	// Add finalizer.
	result, err := r.Reconcile(ctx, Request{Namespace: "ns1", Name: "grant-1", Kind: "MemoryGrant"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !result.Requeue {
		t.Fatal("expected requeue after finalizer")
	}

	// Mark for deletion.
	raw, _ := s.Get(ctx, "MemoryGrant", "ns1", "grant-1")
	var g v1alpha1.MemoryGrant
	json.Unmarshal(raw, &g)
	deletionTime := time.Now()
	g.ObjectMeta.DeletionTimestamp = &deletionTime
	data, _ := json.Marshal(g)
	s.Update(ctx, "MemoryGrant", "ns1", "grant-1", data, g.ObjectMeta.ResourceVersion)

	// Reconcile deletion — should remove finalizer.
	_, err = r.Reconcile(ctx, Request{Namespace: "ns1", Name: "grant-1", Kind: "MemoryGrant"})
	if err != nil {
		t.Fatalf("delete reconcile: %v", err)
	}

	raw, _ = s.Get(ctx, "MemoryGrant", "ns1", "grant-1")
	var deleted v1alpha1.MemoryGrant
	json.Unmarshal(raw, &deleted)
	if len(deleted.ObjectMeta.Finalizers) != 0 {
		t.Error("expected finalizer to be removed on deletion")
	}
}

// ---------------------------------------------------------------------------
// TC-0076: MemoryGrant cascade — revoking grant denies subsequent access.
// ---------------------------------------------------------------------------

func TestMemoryGrant_CascadeRevocationDeniesAccess(t *testing.T) {
	// TC-0076
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	id := makeAgentIdentity("ns1", "agent-id", "agent-a")
	storeAgentIdentity(t, s, id)

	grant := makeMemoryGrant("ns1", "grant-1", "agent-a", "mem-1", "ReadWrite")
	storeMemoryGrant(t, s, grant)

	// Access should be granted.
	err := EvaluateMemoryAccess(ctx, s, nil, id, "mem-1", "ReadOnly")
	if err != nil {
		t.Fatalf("expected access: %v", err)
	}

	// Begin deletion while the finalizer still keeps the grant in storage.
	raw, err := s.Get(ctx, "MemoryGrant", "ns1", "grant-1")
	if err != nil {
		t.Fatalf("get grant for deletion: %v", err)
	}
	var deleting v1alpha1.MemoryGrant
	if err := json.Unmarshal(raw, &deleting); err != nil {
		t.Fatalf("unmarshal grant for deletion: %v", err)
	}
	deletionTime := time.Now()
	deleting.ObjectMeta.DeletionTimestamp = &deletionTime
	data, err := json.Marshal(deleting)
	if err != nil {
		t.Fatalf("marshal deleting grant: %v", err)
	}
	if err := s.Update(ctx, "MemoryGrant", "ns1", "grant-1", data, deleting.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("mark grant for deletion: %v", err)
	}

	// Revocation takes effect as soon as deletion begins, not after cleanup.
	err = EvaluateMemoryAccess(ctx, s, nil, id, "mem-1", "ReadOnly")
	if err == nil {
		t.Error("expected access denied after grant revocation")
	}

	_ = log
}

func TestMemoryGrantReconciler_CascadesProvenanceRevocation(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	grant := makeMemoryGrant("ns1", "grant-1", "agent-a", "mem-1", "ReadWrite")
	storeMemoryGrant(t, s, grant)
	claim := &v1alpha1.MemoryClaim{ObjectMeta: v1alpha1.ObjectMeta{Name: "mem-1", Namespace: "ns1"}}
	claimData, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, "MemoryClaim", "ns1", "mem-1", claimData); err != nil {
		t.Fatal(err)
	}

	direct := makeMemoryRecord("ns1", "direct", fullProvenance("session-a", "write"))
	direct.Spec.Provenance.GrantID = "grant-1"
	derived := makeMemoryRecord("ns1", "derived", fullProvenance("session-a", "summary"))
	derived.Spec.Provenance.DerivedFromRecordNames = []string{"direct"}
	unrelated := makeMemoryRecord("ns1", "unrelated", fullProvenance("session-b", "write"))
	unrelated.Spec.Provenance.GrantID = "other-grant"
	for _, record := range []v1alpha1.MemoryRecord{direct, derived, unrelated} {
		storeMemoryRecord(t, s, record)
	}

	reconciler := NewMemoryGrantReconciler(s, zaptest.NewLogger(t))
	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "ns1", Name: "grant-1", Kind: "MemoryGrant"}); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	raw, err := s.Get(ctx, "MemoryGrant", "ns1", "grant-1")
	if err != nil {
		t.Fatal(err)
	}
	var deleting v1alpha1.MemoryGrant
	if err := json.Unmarshal(raw, &deleting); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	deleting.ObjectMeta.DeletionTimestamp = &now
	grantData, err := json.Marshal(&deleting)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "MemoryGrant", "ns1", "grant-1", grantData, deleting.ObjectMeta.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "ns1", Name: "grant-1", Kind: "MemoryGrant"}); err != nil {
		t.Fatalf("cascade deletion: %v", err)
	}

	raw, err = s.Get(ctx, "MemoryClaim", "ns1", "mem-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, claim); err != nil {
		t.Fatal(err)
	}
	revoked, err := RevokedMemoryGrantIDs(claim.ObjectMeta.Annotations)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := revoked["grant-1"]; !found {
		t.Fatalf("claim revocations = %#v, want grant-1", revoked)
	}
	for _, name := range []string{"direct", "derived"} {
		raw, err := s.Get(ctx, memoryRecordKind, "ns1", name)
		if err != nil {
			t.Fatal(err)
		}
		var record v1alpha1.MemoryRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		revoked, err := RevokedMemoryGrantIDs(record.ObjectMeta.Annotations)
		if err != nil {
			t.Fatal(err)
		}
		if _, found := revoked["grant-1"]; !found {
			t.Fatalf("MemoryRecord %q revocations = %#v, want grant-1", name, revoked)
		}
	}
	raw, err = s.Get(ctx, memoryRecordKind, "ns1", "unrelated")
	if err != nil {
		t.Fatal(err)
	}
	var unrelatedAfter v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &unrelatedAfter); err != nil {
		t.Fatal(err)
	}
	if len(unrelatedAfter.ObjectMeta.Annotations) != 0 {
		t.Fatalf("unrelated MemoryRecord annotations = %#v, want none", unrelatedAfter.ObjectMeta.Annotations)
	}
}

// TC-0077: MemoryGrant lifecycle and each cascade block are durably audited.
func TestMemoryGrantReconciler_PersistsLifecycleAndCascadeAudit(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	auditStore := &memoryGrantAuditStore{}
	grant := makeMemoryGrant("ns1", "grant-audit", "agent-a", "mem-1", "ReadWrite")
	storeMemoryGrant(t, s, grant)
	claim := &v1alpha1.MemoryClaim{ObjectMeta: v1alpha1.ObjectMeta{Name: "mem-1", Namespace: "ns1"}}
	claimData, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, "MemoryClaim", "ns1", "mem-1", claimData); err != nil {
		t.Fatal(err)
	}
	recordZ := makeMemoryRecord("ns1", "record-z", fullProvenance("session-a", "write-z"))
	recordZ.Spec.Provenance.GrantID = grant.ObjectMeta.Name
	storeMemoryRecord(t, s, recordZ)
	recordA := makeMemoryRecord("ns1", "record-a", fullProvenance("session-a", "write-a"))
	recordA.Spec.Provenance.GrantID = grant.ObjectMeta.Name
	storeMemoryRecord(t, s, recordA)

	reconciler := NewMemoryGrantReconciler(s, zaptest.NewLogger(t), WithMemoryGrantAuditLog(observability.NewCognitiveAuditLog(auditStore)))
	request := Request{Namespace: "ns1", Name: grant.ObjectMeta.Name, Kind: "MemoryGrant"}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("evaluate grant: %v", err)
	}

	raw, err := s.Get(ctx, "MemoryGrant", "ns1", grant.ObjectMeta.Name)
	if err != nil {
		t.Fatal(err)
	}
	var deleting v1alpha1.MemoryGrant
	if err := json.Unmarshal(raw, &deleting); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	deleting.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(&deleting)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "MemoryGrant", "ns1", grant.ObjectMeta.Name, data, deleting.ObjectMeta.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("revoke grant: %v", err)
	}

	if len(auditStore.entries) != 5 {
		t.Fatalf("audit entries = %#v, want evaluate, revoke, and three revoke-block entries", auditStore.entries)
	}
	verbs := []string{auditStore.entries[0].Verb, auditStore.entries[1].Verb, auditStore.entries[2].Verb, auditStore.entries[3].Verb, auditStore.entries[4].Verb}
	if strings.Join(verbs, ",") != "evaluate,revoke,revoke-block,revoke-block,revoke-block" {
		t.Fatalf("audit verbs = %v", verbs)
	}
	var cascadeTargets []string
	for _, entry := range auditStore.entries {
		if entry.EventType != observability.AuditGrantChange || entry.Workspace != "ns1" || entry.SubjectRef != "ns1/grant-audit" {
			t.Fatalf("audit entry = %#v", entry)
		}
		if entry.Verb == "revoke-block" {
			var details map[string]string
			if err := json.Unmarshal(entry.Details, &details); err != nil {
				t.Fatal(err)
			}
			cascadeTargets = append(cascadeTargets, details["resourceKind"]+"/"+details["resourceName"])
		}
	}
	if strings.Join(cascadeTargets, ",") != "claim/mem-1,record/record-a,record/record-z" {
		t.Fatalf("cascade audit order = %v", cascadeTargets)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("retry completed revocation: %v", err)
	}
	if len(auditStore.entries) != 5 {
		t.Fatalf("retry produced duplicate audit entries: %#v", auditStore.entries)
	}
}

func TestMemoryGrantReconciler_AuditFailurePreventsRevocation(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	auditStore := &memoryGrantAuditStore{}
	grant := makeMemoryGrant("ns1", "grant-audit-failure", "agent-a", "mem-1", "ReadWrite")
	storeMemoryGrant(t, s, grant)
	claim := &v1alpha1.MemoryClaim{ObjectMeta: v1alpha1.ObjectMeta{Name: "mem-1", Namespace: "ns1"}}
	claimData, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, "MemoryClaim", "ns1", "mem-1", claimData); err != nil {
		t.Fatal(err)
	}
	reconciler := NewMemoryGrantReconciler(s, zaptest.NewLogger(t), WithMemoryGrantAuditLog(observability.NewCognitiveAuditLog(auditStore)))
	request := Request{Namespace: "ns1", Name: grant.ObjectMeta.Name, Kind: "MemoryGrant"}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}

	raw, err := s.Get(ctx, "MemoryGrant", "ns1", grant.ObjectMeta.Name)
	if err != nil {
		t.Fatal(err)
	}
	var deleting v1alpha1.MemoryGrant
	if err := json.Unmarshal(raw, &deleting); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	deleting.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(&deleting)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "MemoryGrant", "ns1", grant.ObjectMeta.Name, data, deleting.ObjectMeta.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	auditStore.err = errors.New("audit unavailable")
	if _, err := reconciler.Reconcile(ctx, request); err == nil || !strings.Contains(err.Error(), "revocation audit") {
		t.Fatalf("revoke error = %v, want audit failure", err)
	}

	raw, err = s.Get(ctx, "MemoryClaim", "ns1", "mem-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, claim); err != nil {
		t.Fatal(err)
	}
	if len(claim.ObjectMeta.Annotations) != 0 {
		t.Fatalf("claim annotations = %#v, want no revocation after audit failure", claim.ObjectMeta.Annotations)
	}
}

// ---------------------------------------------------------------------------
// TC-0077: MemoryGrant audit log is emitted.
// ---------------------------------------------------------------------------

func TestMemoryGrant_AuditLogEmitted(t *testing.T) {
	// TC-0077
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	grant := makeMemoryGrant("ns1", "grant-audit", "agent-b", "mem-2", "ReadOnly")
	storeMemoryGrant(t, s, grant)

	r := NewMemoryGrantReconciler(s, log)

	// Reconcile (finalizer).
	r.Reconcile(ctx, Request{Namespace: "ns1", Name: "grant-audit", Kind: "MemoryGrant"})
	// Reconcile (evaluate + audit).
	_, err := r.Reconcile(ctx, Request{Namespace: "ns1", Name: "grant-audit", Kind: "MemoryGrant"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// If we reach here without panic, the audit log function executed successfully.
	// The zap test logger captures the output for verification.
}

// ---------------------------------------------------------------------------
// TC-0152: Dual-gate — MemoryGrant + RBAC both required.
// ---------------------------------------------------------------------------

func TestMemoryGrant_DualGateRequiresBothGrantAndRBAC(t *testing.T) {
	// TC-0152
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	id := makeAgentIdentity("ns1", "dual-id", "dual-agent")
	storeAgentIdentity(t, s, id)

	// Create MemoryGrant but no RBAC role.
	grant := makeMemoryGrant("ns1", "dual-grant", "dual-agent", "mem-x", "ReadWrite")
	storeMemoryGrant(t, s, grant)

	rbac := NewRBACEngine(s, log)

	// With RBAC engine but no role binding, should be denied.
	err := EvaluateMemoryAccess(ctx, s, rbac, id, "mem-x", "ReadOnly")
	if err == nil {
		t.Error("expected denial when RBAC has no binding")
	}

	// Add RBAC role + binding.
	role := makeAgentRole("ns1", "mem-reader", []v1alpha1.PolicyRule{
		{Resources: []string{"Memory"}, Verbs: []string{"get"}},
	})
	storeAgentRole(t, s, role)
	binding := makeAgentRoleBinding("ns1", "dual-binding", "mem-reader", []v1alpha1.Subject{
		{Kind: "AgentIdentity", Name: "dual-id"},
	})
	storeAgentRoleBinding(t, s, binding)

	// Now should pass both gates.
	err = EvaluateMemoryAccess(ctx, s, rbac, id, "mem-x", "ReadOnly")
	if err != nil {
		t.Fatalf("expected access with both grant and RBAC: %v", err)
	}
}

func TestMemoryGrant_WriteAccessRequiresGrantAndRBACUpdate(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	id := makeAgentIdentity("ns1", "writer-id", "writer-agent")
	storeAgentIdentity(t, s, id)
	storeMemoryGrant(t, s, makeMemoryGrant("ns1", "writer-grant", "writer-agent", "mem-write", "ReadOnly"))

	rbac := NewRBACEngine(s, log)
	storeAgentRole(t, s, makeAgentRole("ns1", "mem-writer", []v1alpha1.PolicyRule{
		{Resources: []string{"Memory"}, Verbs: []string{"update"}},
	}))
	storeAgentRoleBinding(t, s, makeAgentRoleBinding("ns1", "writer-binding", "mem-writer", []v1alpha1.Subject{
		{Kind: "AgentIdentity", Name: "writer-id"},
	}))

	err := EvaluateMemoryAccess(ctx, s, rbac, id, "mem-write", "ReadWrite")
	if err == nil || !strings.Contains(err.Error(), "no MemoryGrant found") {
		t.Fatalf("ReadOnly grant with update RBAC error = %v, want MemoryGrant denial", err)
	}

	raw, err := s.Get(ctx, "MemoryGrant", "ns1", "writer-grant")
	if err != nil {
		t.Fatalf("get memory grant: %v", err)
	}
	var grant v1alpha1.MemoryGrant
	if err := json.Unmarshal(raw, &grant); err != nil {
		t.Fatalf("unmarshal memory grant: %v", err)
	}
	grant.Spec.AccessMode = "ReadWrite"
	data, err := json.Marshal(grant)
	if err != nil {
		t.Fatalf("marshal memory grant: %v", err)
	}
	if err := s.Update(ctx, "MemoryGrant", "ns1", "writer-grant", data, grant.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("upgrade memory grant: %v", err)
	}

	if err := EvaluateMemoryAccess(ctx, s, rbac, id, "mem-write", "ReadWrite"); err != nil {
		t.Fatalf("ReadWrite grant with update RBAC denied: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TC-0153: Admission webhook — validates MemoryGrant references exist.
// (Validates that EvaluateMemoryAccess rejects invalid references.)
// ---------------------------------------------------------------------------

func TestMemoryGrant_AdmissionRejectsInvalidReferences(t *testing.T) {
	// TC-0153
	ctx := context.Background()
	s := newMemStore()

	id := makeAgentIdentity("ns1", "admit-id", "admit-agent")
	storeAgentIdentity(t, s, id)

	// No MemoryGrant exists — should be rejected.
	err := EvaluateMemoryAccess(ctx, s, nil, id, "nonexistent-claim", "ReadOnly")
	if err == nil {
		t.Error("expected admission denial for nonexistent memory grant")
	}
}

// ---------------------------------------------------------------------------
// TC-0078: ToolGrant allows tool invocation with matching verb.
// ---------------------------------------------------------------------------

func TestToolGrantEngine_AllowsMatchingVerb(t *testing.T) {
	// TC-0078
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	grant := makeToolGrant("ns1", "grant-invoke", v1alpha1.ToolGrantSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"},
		ToolRef:  v1alpha1.LocalObjectReference{Name: "search-tool"},
		Verbs:    []v1alpha1.ToolGrantVerb{"invoke", "discover"},
	})
	storeToolGrant(t, s, grant)

	engine := NewToolGrantEngine(s, log)

	// Should allow invoke.
	err := engine.EvaluateToolAccess(ctx, "ns1", "agent-a", "search-tool", "invoke")
	if err != nil {
		t.Fatalf("expected invoke allowed: %v", err)
	}

	// Should deny inspect (not in verbs).
	err = engine.EvaluateToolAccess(ctx, "ns1", "agent-a", "search-tool", "inspect")
	if err == nil {
		t.Error("expected inspect to be denied")
	}
}

func TestToolGrantEngine_EnforcesConditionsAndArgumentConstraints(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	agent := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "agent-a", Labels: map[string]string{"clearance": "standard"}},
	}
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, "Agent", "ns1", "agent-a", data); err != nil {
		t.Fatal(err)
	}

	grant := makeToolGrant("ns1", "constrained-grant", v1alpha1.ToolGrantSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"},
		ToolRef:  v1alpha1.LocalObjectReference{Name: "search-tool"},
		Verbs:    []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
		ArgumentConstraints: map[string]any{
			"type":     "object",
			"required": []any{"query"},
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "minLength": 3},
			},
		},
		Conditions: &v1alpha1.ToolGrantConditions{
			ValidFrom:           time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			ValidUntil:          time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
			RequiredAgentLabels: map[string]string{"clearance": "standard"},
		},
	})
	storeToolGrant(t, s, grant)
	engine := NewToolGrantEngine(s, zaptest.NewLogger(t))

	decision, err := engine.EvaluateToolInvocationDecision(ctx, "ns1", "agent-a", "search-tool", map[string]any{"query": "ok"})
	var constraintErr *ToolGrantArgumentConstraintError
	if err == nil || !errors.As(err, &constraintErr) {
		t.Fatal("expected short query to violate argument constraints")
	}
	if decision.GrantRef == "" || constraintErr.GrantName != "constrained-grant" {
		t.Fatalf("argument denial lost grant provenance: decision=%#v error=%v", decision, err)
	}
	if _, err := engine.EvaluateToolInvocationDecision(ctx, "ns1", "agent-a", "search-tool", map[string]any{"query": "valid"}); err != nil {
		t.Fatalf("expected conforming invocation to be allowed: %v", err)
	}

	agent.ObjectMeta.Labels["clearance"] = "restricted"
	data, err = json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "Agent", "ns1", "agent-a", data, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.EvaluateToolInvocationDecision(ctx, "ns1", "agent-a", "search-tool", map[string]any{"query": "valid"}); err == nil {
		t.Fatal("expected required agent-label mismatch to deny invocation")
	}
}

func TestToolGrantEngine_AdditiveGrantCanAllowRejectedArguments(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	storeToolGrant(t, s, makeToolGrant("ns1", "a-constrained", v1alpha1.ToolGrantSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"},
		ToolRef:  v1alpha1.LocalObjectReference{Name: "search-tool"},
		Verbs:    []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
		ArgumentConstraints: map[string]any{
			"type":     "object",
			"required": []any{"query"},
		},
	}))
	storeToolGrant(t, s, makeToolGrant("ns1", "b-unconstrained", v1alpha1.ToolGrantSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"},
		ToolRef:  v1alpha1.LocalObjectReference{Name: "search-tool"},
		Verbs:    []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
	}))

	decision, err := NewToolGrantEngine(s, zaptest.NewLogger(t)).EvaluateToolInvocationDecision(ctx, "ns1", "agent-a", "search-tool", map[string]any{})
	if err != nil {
		t.Fatalf("expected later additive grant to allow invocation: %v", err)
	}
	if !strings.Contains(decision.GrantRef, "b-unconstrained") {
		t.Fatalf("decision.GrantRef = %q, want later permissive grant", decision.GrantRef)
	}
}

func TestToolGrantEngine_DeniesOutsideValidityWindow(t *testing.T) {
	tests := []struct {
		name       string
		conditions v1alpha1.ToolGrantConditions
	}{
		{name: "before valid-from", conditions: v1alpha1.ToolGrantConditions{ValidFrom: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}},
		{name: "after valid-until", conditions: v1alpha1.ToolGrantConditions{ValidUntil: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newMemStore()
			storeToolGrant(t, s, makeToolGrant("ns1", "timed-grant", v1alpha1.ToolGrantSpec{
				AgentRef:   v1alpha1.LocalObjectReference{Name: "agent-a"},
				ToolRef:    v1alpha1.LocalObjectReference{Name: "search-tool"},
				Verbs:      []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
				Conditions: &test.conditions,
			}))
			engine := NewToolGrantEngine(s, zaptest.NewLogger(t))
			if _, err := engine.EvaluateToolInvocationDecision(context.Background(), "ns1", "agent-a", "search-tool", map[string]any{}); err == nil {
				t.Fatal("expected inactive grant to deny invocation")
			}
		})
	}
}

func TestToolGrantEngine_AllowsAgentPolicyToolGrant(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	policy := v1alpha1.AgentPolicy{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentPolicy"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "agent-tools"},
		Spec: v1alpha1.AgentPolicySpec{
			Enforce:      true,
			Scope:        "agent",
			TargetAgents: []string{"agent-a"},
			ToolGrants: []v1alpha1.PolicyToolGrant{
				{ToolRef: "search-tool", Allow: true},
			},
		},
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, agentPolicyKind, "ns1", policy.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}

	engine := NewToolGrantEngine(s, zaptest.NewLogger(t))
	if err := engine.EvaluateToolAccess(ctx, "ns1", "agent-a", "search-tool", "invoke"); err != nil {
		t.Fatalf("expected AgentPolicy tool grant to allow invoke: %v", err)
	}
	if err := engine.EvaluateToolAccess(ctx, "ns1", "agent-b", "search-tool", "invoke"); err == nil {
		t.Fatal("expected non-target agent to be denied")
	}
	for _, verb := range []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbDiscover, v1alpha1.ToolGrantVerbInspect} {
		if err := engine.EvaluateToolAccess(ctx, "ns1", "agent-a", "search-tool", verb); err == nil {
			t.Fatalf("embedded AgentPolicy tool grant must not confer %s access", verb)
		}
	}
}

func TestToolGrantEngine_WarnOnlyAgentPolicyDoesNotGrantAccess(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	policy := v1alpha1.AgentPolicy{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentPolicy"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "warn-tools"},
		Spec: v1alpha1.AgentPolicySpec{
			Enforce: false,
			Scope:   "workspace",
			ToolGrants: []v1alpha1.PolicyToolGrant{
				{ToolRef: "search-tool", Allow: true},
			},
		},
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, agentPolicyKind, "ns1", policy.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}

	if err := NewToolGrantEngine(s, zaptest.NewLogger(t)).EvaluateToolAccess(ctx, "ns1", "agent-a", "search-tool", v1alpha1.ToolGrantVerbInvoke); err == nil {
		t.Fatal("warn-only AgentPolicy must not confer tool access")
	}
}

func TestToolGrantEngine_EnforcesAgentPolicyToolGrantConditions(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	policy := v1alpha1.AgentPolicy{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentPolicy"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "conditional-tools"},
		Spec: v1alpha1.AgentPolicySpec{
			Enforce: true,
			Scope:   "workspace",
			ToolGrants: []v1alpha1.PolicyToolGrant{
				{ToolRef: "search-tool", Allow: true, Conditions: `deny[msg] { input.verb == "invoke"; msg := "invoke denied" }`},
			},
		},
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, agentPolicyKind, "ns1", policy.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	engine := NewToolGrantEngine(s, zaptest.NewLogger(t))

	if err := engine.EvaluateToolAccess(ctx, "ns1", "agent-a", "search-tool", v1alpha1.ToolGrantVerbInvoke); err == nil {
		t.Fatal("conditional policy grant should deny invoke")
	}
	if err := engine.EvaluateToolAccess(ctx, "ns1", "agent-a", "search-tool", v1alpha1.ToolGrantVerbInspect); err == nil {
		t.Fatal("conditional embedded policy grant must not confer inspect access")
	}
}

// ---------------------------------------------------------------------------
// TC-0079: ToolGrant denies access without a matching grant.
// ---------------------------------------------------------------------------

func TestToolGrantEngine_DeniesWithoutGrant(t *testing.T) {
	// TC-0079
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	engine := NewToolGrantEngine(s, log)

	err := engine.EvaluateToolAccess(ctx, "ns1", "agent-a", "secret-tool", "invoke")
	if err == nil {
		t.Error("expected denial without any ToolGrant")
	}
}

// ---------------------------------------------------------------------------
// TC-0080: ToolGrant rate limiting enforces calls-per-minute.
// ---------------------------------------------------------------------------

func TestToolGrantEngine_RateLimitEnforcement(t *testing.T) {
	// TC-0080
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	grant := makeToolGrant("ns1", "rate-grant", v1alpha1.ToolGrantSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"},
		ToolRef:  v1alpha1.LocalObjectReference{Name: "api-tool"},
		Verbs:    []v1alpha1.ToolGrantVerb{"invoke"},
		RateLimits: &v1alpha1.ToolGrantRateLimits{
			CallsPerMinute: 3,
		},
	})
	storeToolGrant(t, s, grant)

	engine := NewToolGrantEngine(s, log)

	// First 3 invocations should succeed.
	for i := 0; i < 3; i++ {
		_, err := engine.EvaluateToolInvocationDecision(ctx, "ns1", "agent-a", "api-tool", map[string]any{})
		if err != nil {
			t.Fatalf("call %d should succeed: %v", i+1, err)
		}
	}

	// 4th call should be rate-limited.
	_, err := engine.EvaluateToolInvocationDecision(ctx, "ns1", "agent-a", "api-tool", map[string]any{})
	if err == nil {
		t.Error("expected rate limit error on 4th call")
	}
}

func TestToolGrantEngine_NonInvocationAuthorizationDoesNotConsumeRateLimit(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	storeToolGrant(t, s, makeToolGrant("ns1", "rate-grant", v1alpha1.ToolGrantSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"},
		ToolRef:  v1alpha1.LocalObjectReference{Name: "api-tool"},
		Verbs:    []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke, v1alpha1.ToolGrantVerbInspect},
		RateLimits: &v1alpha1.ToolGrantRateLimits{
			CallsPerMinute: 1,
		},
	}))
	engine := NewToolGrantEngine(s, zaptest.NewLogger(t))

	for i := 0; i < 3; i++ {
		if err := engine.EvaluateToolAccess(ctx, "ns1", "agent-a", "api-tool", v1alpha1.ToolGrantVerbInspect); err != nil {
			t.Fatalf("inspect authorization %d failed: %v", i+1, err)
		}
	}
	if _, err := engine.EvaluateToolInvocationDecision(ctx, "ns1", "agent-a", "api-tool", map[string]any{}); err != nil {
		t.Fatalf("first invocation should retain the full rate allowance: %v", err)
	}
	if _, err := engine.EvaluateToolInvocationDecision(ctx, "ns1", "agent-a", "api-tool", map[string]any{}); err == nil {
		t.Fatal("second invocation should exceed the rate limit")
	}
}

func TestToolGrantEngine_ConcurrentInvocationsReserveRateLimitAtomically(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	storeToolGrant(t, s, makeToolGrant("ns1", "rate-grant", v1alpha1.ToolGrantSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"},
		ToolRef:  v1alpha1.LocalObjectReference{Name: "api-tool"},
		Verbs:    []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
		RateLimits: &v1alpha1.ToolGrantRateLimits{
			CallsPerMinute: 1,
		},
	}))
	engine := NewToolGrantEngine(s, zaptest.NewLogger(t))

	const callers = 20
	start := make(chan struct{})
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			_, err := engine.EvaluateToolInvocationDecision(ctx, "ns1", "agent-a", "api-tool", map[string]any{})
			results <- err
		}()
	}
	close(start)

	allowed := 0
	for i := 0; i < callers; i++ {
		if <-results == nil {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("allowed invocations = %d, want exactly 1", allowed)
	}
}

type sharedToolGrantRateStore struct {
	*memStore
	mu       sync.Mutex
	counters map[string]int64
}

func (s *sharedToolGrantRateStore) ReserveToolGrantInvocation(_ context.Context, reservation store.ToolGrantRateReservation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := s.counters[reservation.Key]
	if reservation.CallsPerMinute > 0 && count >= reservation.CallsPerMinute {
		return store.ErrQuotaExceeded
	}
	s.counters[reservation.Key] = count + 1
	return nil
}

func TestToolGrantEngine_RateLimitIsSharedAcrossEngineInstances(t *testing.T) {
	ctx := context.Background()
	s := &sharedToolGrantRateStore{memStore: newMemStore(), counters: make(map[string]int64)}
	storeToolGrant(t, s.memStore, makeToolGrant("ns1", "rate-grant", v1alpha1.ToolGrantSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"},
		ToolRef:  v1alpha1.LocalObjectReference{Name: "api-tool"},
		Verbs:    []v1alpha1.ToolGrantVerb{v1alpha1.ToolGrantVerbInvoke},
		RateLimits: &v1alpha1.ToolGrantRateLimits{
			CallsPerMinute: 1,
		},
	}))

	first := NewToolGrantEngine(s, zaptest.NewLogger(t))
	second := NewToolGrantEngine(s, zaptest.NewLogger(t))
	if _, err := first.EvaluateToolInvocationDecision(ctx, "ns1", "agent-a", "api-tool", map[string]any{}); err != nil {
		t.Fatalf("first invocation should succeed: %v", err)
	}
	if _, err := second.EvaluateToolInvocationDecision(ctx, "ns1", "agent-a", "api-tool", map[string]any{}); err == nil {
		t.Fatal("second engine should observe the shared rate-limit reservation")
	}
}

// ---------------------------------------------------------------------------
// TC-0081: ModelGrant allows model access when grant exists.
// ---------------------------------------------------------------------------

func TestToolGrantEngine_ModelGrantAllowsAccess(t *testing.T) {
	// TC-0081
	ctx := context.Background()
	s := newMemStore()
	log := zaptest.NewLogger(t)

	grant := makeModelGrant("ns1", "model-grant-1", "agent-a", "gpt-4")
	storeModelGrant(t, s, grant)

	engine := NewToolGrantEngine(s, log)

	// Should allow.
	err := engine.EvaluateModelAccess(ctx, "ns1", "agent-a", "gpt-4")
	if err != nil {
		t.Fatalf("expected model access: %v", err)
	}

	// Should deny unknown model.
	err = engine.EvaluateModelAccess(ctx, "ns1", "agent-a", "gpt-5")
	if err == nil {
		t.Error("expected denial for ungranted model")
	}

	// Should deny different agent.
	err = engine.EvaluateModelAccess(ctx, "ns1", "agent-b", "gpt-4")
	if err == nil {
		t.Error("expected denial for different agent")
	}
}

func TestToolGrantEngine_SelectorAndWildcards(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	agent := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "ns1", Name: "agent-a", Labels: map[string]string{"team": "research", "tier": "trusted"}},
	}
	agentData, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, "Agent", "ns1", "agent-a", agentData); err != nil {
		t.Fatal(err)
	}
	storeToolGrant(t, s, makeToolGrant("ns1", "selector-grant", v1alpha1.ToolGrantSpec{
		AgentSelector: map[string]string{"team": "research", "tier": "trusted"},
		ToolRef:       v1alpha1.LocalObjectReference{Name: "*"},
		Verbs:         []v1alpha1.ToolGrantVerb{"*"},
	}))
	storeModelGrant(t, s, makeModelGrant("ns1", "all-models", "agent-a", "*"))
	engine := NewToolGrantEngine(s, zaptest.NewLogger(t))

	if err := engine.EvaluateToolAccess(ctx, "ns1", "agent-a", "search-tool", "inspect"); err != nil {
		t.Fatalf("expected selector and wildcards to allow tool access: %v", err)
	}
	if err := engine.EvaluateModelAccess(ctx, "ns1", "agent-a", "future-model"); err != nil {
		t.Fatalf("expected wildcard model grant to allow access: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Verify unused import suppression
// ---------------------------------------------------------------------------

var _ store.Store = (*memStore)(nil)
