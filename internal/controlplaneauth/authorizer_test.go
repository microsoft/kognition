// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controlplaneauth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/store"
)

type policyStore struct {
	bindings [][]byte
	roles    map[string][]byte
	listErr  error
	getErr   error
}

func TestAuthorizeWorkflowExecutionExactScope(t *testing.T) {
	identity := &auth.Identity{Method: "mtls", Issuer: "operator-ca", Subject: "executor-1"}
	digest := strings.Repeat("a", 64)
	workflowRule := api.ControlPlaneExecutionRule{Effect: "allow", Operation: api.ControlPlaneWorkflowUse,
		Workspace: "planning", ResourceName: "investigate", ResourceUID: "workflow-uid", RevisionDigest: digest}
	agentRule := api.ControlPlaneExecutionRule{Effect: "allow", Operation: api.ControlPlaneAgentInvoke,
		Workspace: "workers", ResourceName: "analyst", ResourceUID: "agent-uid"}
	role := api.ControlPlaneRole{Spec: api.ControlPlaneRoleSpec{ExecutionRules: []api.ControlPlaneExecutionRule{workflowRule, agentRule}}}
	binding := api.ControlPlaneRoleBinding{ObjectMeta: api.ObjectMeta{Namespace: "control"}, Spec: api.ControlPlaneRoleBindingSpec{
		RoleRef: api.LocalObjectReference{Name: "executor"}, Subjects: []api.ControlPlaneRoleSubject{{Method: identity.Method, Issuer: identity.Issuer, Subject: identity.Subject}},
	}}
	bindingData, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	policy := &policyStore{bindings: [][]byte{bindingData}, roles: map[string][]byte{}}
	setRole := func() {
		t.Helper()
		data, err := json.Marshal(role)
		if err != nil {
			t.Fatal(err)
		}
		policy.roles["control/executor"] = data
	}
	setRole()
	authorizer := New(policy)
	for _, test := range []struct {
		name string
		call func() error
		want error
	}{
		{"workflow", func() error {
			return authorizer.AuthorizeWorkflowUse(context.Background(), identity, "planning", "investigate", "workflow-uid", digest)
		}, nil},
		{"agent", func() error {
			return authorizer.AuthorizeAgentInvocation(context.Background(), identity, "workers", "analyst", "agent-uid")
		}, nil},
		{"wrong workspace", func() error {
			return authorizer.AuthorizeAgentInvocation(context.Background(), identity, "planning", "analyst", "agent-uid")
		}, ErrForbidden},
		{"wrong target", func() error {
			return authorizer.AuthorizeAgentInvocation(context.Background(), identity, "workers", "other", "agent-uid")
		}, ErrForbidden},
		{"recreated agent", func() error {
			return authorizer.AuthorizeAgentInvocation(context.Background(), identity, "workers", "analyst", "new-uid")
		}, ErrForbidden},
		{"recreated workflow", func() error {
			return authorizer.AuthorizeWorkflowUse(context.Background(), identity, "planning", "investigate", "new-uid", digest)
		}, ErrForbidden},
		{"changed revision", func() error {
			return authorizer.AuthorizeWorkflowUse(context.Background(), identity, "planning", "investigate", "workflow-uid", strings.Repeat("b", 64))
		}, ErrForbidden},
		{"missing uid", func() error {
			return authorizer.AuthorizeAgentInvocation(context.Background(), identity, "workers", "analyst", "")
		}, ErrForbidden},
		{"invalid revision", func() error {
			return authorizer.AuthorizeWorkflowUse(context.Background(), identity, "planning", "investigate", "workflow-uid", "*")
		}, ErrForbidden},
		{"wrong principal", func() error {
			return authorizer.AuthorizeAgentInvocation(context.Background(), &auth.Identity{Method: identity.Method, Issuer: identity.Issuer, Subject: "author"}, "workers", "analyst", "agent-uid")
		}, ErrForbidden},
		{"wrong issuer", func() error {
			return authorizer.AuthorizeAgentInvocation(context.Background(), &auth.Identity{Method: identity.Method, Issuer: "other-ca", Subject: identity.Subject}, "workers", "analyst", "agent-uid")
		}, ErrForbidden},
		{"wrong method", func() error {
			return authorizer.AuthorizeAgentInvocation(context.Background(), &auth.Identity{Method: "oidc", Issuer: identity.Issuer, Subject: identity.Subject}, "workers", "analyst", "agent-uid")
		}, ErrForbidden},
		{"anonymous", func() error {
			return authorizer.AuthorizeAgentInvocation(context.Background(), nil, "workers", "analyst", "agent-uid")
		}, ErrForbidden},
		{"runtime token", func() error {
			return authorizer.AuthorizeAgentInvocation(context.Background(), &auth.Identity{Method: "token", Issuer: identity.Issuer, Subject: identity.Subject}, "workers", "analyst", "agent-uid")
		}, ErrForbidden},
		{"no approval authority", func() error {
			return authorizer.AuthorizeRuntimeApproval(context.Background(), identity, "workers", "approval-1", "approve")
		}, ErrForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, test.want) {
				t.Fatalf("authorization = %v, want %v", err, test.want)
			}
		})
	}
	deny := agentRule
	deny.Effect = "deny"
	for _, rules := range [][]api.ControlPlaneExecutionRule{{agentRule, deny}, {deny, agentRule}} {
		role.Spec.ExecutionRules = rules
		setRole()
		if err := authorizer.AuthorizeAgentInvocation(context.Background(), identity, "workers", "analyst", "agent-uid"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("deny precedence = %v", err)
		}
	}
	role.Spec.ExecutionRules = nil
	role.Spec.Rules = []api.ControlPlanePolicyRule{{Effect: "allow", Resources: []string{"RuntimeApproval", "*", "Agent"}, Verbs: []string{"approve", "*", "invoke"}, Workspaces: []string{"workers", "*"}, RuntimeApprovalNames: []string{"analyst", "*"}}}
	setRole()
	if err := authorizer.AuthorizeAgentInvocation(context.Background(), identity, "workers", "analyst", "agent-uid"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("legacy rules granted execution: %v", err)
	}
	role.Spec.ExecutionRules = []api.ControlPlaneExecutionRule{agentRule}
	setRole()
	policy.listErr = errors.New("policy unavailable")
	if err := authorizer.AuthorizeAgentInvocation(context.Background(), identity, "workers", "analyst", "agent-uid"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable policy = %v", err)
	}
	policy.listErr = nil
	policy.getErr = errors.New("role unavailable")
	if err := authorizer.AuthorizeAgentInvocation(context.Background(), identity, "workers", "analyst", "agent-uid"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable role = %v", err)
	}
}

func TestValidateExecutionRuleRejectsUnboundedScope(t *testing.T) {
	valid := api.ControlPlaneExecutionRule{Effect: "allow", Operation: api.ControlPlaneWorkflowUse,
		Workspace: "planning", ResourceName: "investigate", ResourceUID: "workflow-uid", RevisionDigest: strings.Repeat("a", 64)}
	for name, change := range map[string]func(*api.ControlPlaneExecutionRule){
		"effect":              func(rule *api.ControlPlaneExecutionRule) { rule.Effect = "permit" },
		"operation":           func(rule *api.ControlPlaneExecutionRule) { rule.Operation = "*" },
		"workspace":           func(rule *api.ControlPlaneExecutionRule) { rule.Workspace = "*" },
		"name":                func(rule *api.ControlPlaneExecutionRule) { rule.ResourceName = "agent-*" },
		"uid":                 func(rule *api.ControlPlaneExecutionRule) { rule.ResourceUID = "" },
		"uid wildcard":        func(rule *api.ControlPlaneExecutionRule) { rule.ResourceUID = "*" },
		"uid control":         func(rule *api.ControlPlaneExecutionRule) { rule.ResourceUID = "uid\x00" },
		"uid length":          func(rule *api.ControlPlaneExecutionRule) { rule.ResourceUID = strings.Repeat("a", 129) },
		"revision":            func(rule *api.ControlPlaneExecutionRule) { rule.RevisionDigest = "*" },
		"noncanonical digest": func(rule *api.ControlPlaneExecutionRule) { rule.RevisionDigest = strings.Repeat("A", 64) },
		"agent revision":      func(rule *api.ControlPlaneExecutionRule) { rule.Operation = api.ControlPlaneAgentInvoke },
	} {
		t.Run(name, func(t *testing.T) {
			changed := valid
			change(&changed)
			if err := ValidateExecutionRule(changed); !errors.Is(err, ErrForbidden) {
				t.Fatalf("invalid rule accepted: %v", err)
			}
		})
	}
	if err := New(nil).AuthorizeWorkflowUse(context.Background(), &auth.Identity{Method: "mtls", Issuer: "ca", Subject: "operator"}, "planning", "investigate", "workflow-uid", valid.RevisionDigest); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing store = %v", err)
	}
}

func TestAuthorizeExecutionLivePolicyAndSeparateRoleDeny(t *testing.T) {
	identity := &auth.Identity{Method: "mtls", Issuer: "ca", Subject: "executor"}
	rule := api.ControlPlaneExecutionRule{Effect: "allow", Operation: api.ControlPlaneAgentInvoke,
		Workspace: "workers", ResourceName: "analyst", ResourceUID: "agent-uid"}
	policy := &policyStore{roles: map[string][]byte{}}
	addRole := func(name string, rules []api.ControlPlaneExecutionRule) []byte {
		t.Helper()
		roleData, err := json.Marshal(api.ControlPlaneRole{Spec: api.ControlPlaneRoleSpec{ExecutionRules: rules}})
		if err != nil {
			t.Fatal(err)
		}
		policy.roles["control/"+name] = roleData
		bindingData, err := json.Marshal(api.ControlPlaneRoleBinding{ObjectMeta: api.ObjectMeta{Namespace: "control"}, Spec: api.ControlPlaneRoleBindingSpec{
			RoleRef: api.LocalObjectReference{Name: name}, Subjects: []api.ControlPlaneRoleSubject{{Method: identity.Method, Issuer: identity.Issuer, Subject: identity.Subject}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return bindingData
	}
	allowBinding := addRole("allow", []api.ControlPlaneExecutionRule{rule})
	policy.bindings = [][]byte{allowBinding}
	authorizer := New(policy)
	check := func(want error) {
		t.Helper()
		if err := authorizer.AuthorizeAgentInvocation(context.Background(), identity, "workers", "analyst", "agent-uid"); !errors.Is(err, want) {
			t.Fatalf("authorization = %v, want %v", err, want)
		}
	}
	check(nil)
	addRole("allow", nil)
	check(ErrForbidden)
	addRole("allow", []api.ControlPlaneExecutionRule{rule})
	check(nil)
	policy.bindings = nil
	check(ErrForbidden)
	deny := rule
	deny.Effect = "deny"
	denyBinding := addRole("deny", []api.ControlPlaneExecutionRule{deny})
	for _, bindings := range [][][]byte{{allowBinding, denyBinding}, {denyBinding, allowBinding}} {
		policy.bindings = bindings
		check(ErrForbidden)
	}
	policy.bindings = [][]byte{allowBinding}
	for _, invalid := range []api.ControlPlaneExecutionRule{
		{Effect: "allow", Operation: api.ControlPlaneAgentInvoke, Workspace: "*", ResourceName: "analyst", ResourceUID: "agent-uid"},
		{Effect: "allow", Operation: "Agent/*", Workspace: "workers", ResourceName: "analyst", ResourceUID: "agent-uid"},
	} {
		addRole("allow", []api.ControlPlaneExecutionRule{rule, invalid})
		check(ErrUnavailable)
	}
	rules := make([]api.ControlPlaneExecutionRule, MaxExecutionRules+1)
	for index := range rules {
		rules[index] = rule
	}
	addRole("allow", rules)
	check(ErrUnavailable)
	addRole("allow", rules[:MaxExecutionRules])
	check(nil)
}

func (s *policyStore) List(context.Context, string, store.ListOptions) ([][]byte, error) {
	return s.bindings, s.listErr
}

func (s *policyStore) Get(_ context.Context, _, namespace, name string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	data, ok := s.roles[namespace+"/"+name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return data, nil
}

func (*policyStore) Ping(context.Context) error                                          { return nil }
func (*policyStore) Create(context.Context, string, string, string, []byte) error        { return nil }
func (*policyStore) Update(context.Context, string, string, string, []byte, int64) error { return nil }
func (*policyStore) Delete(context.Context, string, string, string) error                { return nil }
func (*policyStore) Watch(context.Context, string, store.ListOptions) (<-chan store.WatchEvent, error) {
	return nil, nil
}
func (*policyStore) CreateRevision(context.Context, string, string, string, int64, []byte, []byte) error {
	return nil
}
func (*policyStore) ListRevisions(context.Context, string, string, string) ([]store.Revision, error) {
	return nil, nil
}
func (*policyStore) GetRevision(context.Context, string, string, string, int64) (*store.Revision, error) {
	return nil, nil
}
func (*policyStore) AppendEvent(context.Context, string, string, string, store.InstanceEvent) error {
	return nil
}
func (*policyStore) ListEvents(context.Context, string, string, string) ([]store.InstanceEvent, error) {
	return nil, nil
}

func TestAuthorizeRuntimeApprovalDenyPrecedesAllow(t *testing.T) {
	identity := &auth.Identity{Method: "oidc", Issuer: "https://issuer.example", Subject: "operator-1"}
	role := api.ControlPlaneRole{ObjectMeta: api.ObjectMeta{Name: "decision", Namespace: "control"}, Spec: api.ControlPlaneRoleSpec{Rules: []api.ControlPlanePolicyRule{
		{Effect: "allow", Resources: []string{"RuntimeApproval"}, Verbs: []string{"approve"}, Workspaces: []string{"team-a"}, RuntimeApprovalNames: []string{"approval-a"}},
		{Effect: "deny", Resources: []string{"RuntimeApproval"}, Verbs: []string{"approve"}, Workspaces: []string{"team-a"}, RuntimeApprovalNames: []string{"approval-a"}},
	}}}
	binding := api.ControlPlaneRoleBinding{ObjectMeta: api.ObjectMeta{Name: "operator", Namespace: "control"}, Spec: api.ControlPlaneRoleBindingSpec{
		RoleRef:  api.LocalObjectReference{Name: "decision"},
		Subjects: []api.ControlPlaneRoleSubject{{Method: identity.Method, Issuer: identity.Issuer, Subject: identity.Subject}},
	}}
	roleData, _ := json.Marshal(role)
	bindingData, _ := json.Marshal(binding)
	authorizer := New(&policyStore{bindings: [][]byte{bindingData}, roles: map[string][]byte{"control/decision": roleData}})

	if err := authorizer.AuthorizeRuntimeApproval(context.Background(), identity, "team-a", "approval-a", "approve"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("AuthorizeRuntimeApproval() error = %v, want %v", err, ErrForbidden)
	}
	role.Spec.Rules = role.Spec.Rules[:1]
	roleData, _ = json.Marshal(role)
	authorizer = New(&policyStore{bindings: [][]byte{bindingData}, roles: map[string][]byte{"control/decision": roleData}})
	if err := authorizer.AuthorizeRuntimeApproval(context.Background(), identity, "team-a", "approval-a", "approve"); err != nil {
		t.Fatalf("AuthorizeRuntimeApproval() error = %v", err)
	}
}

func TestAuthorizeRuntimeApprovalRequiresExactCanonicalScope(t *testing.T) {
	identity := &auth.Identity{Method: "oidc", Issuer: "https://issuer.example", Subject: "operator-1"}
	role := api.ControlPlaneRole{ObjectMeta: api.ObjectMeta{Name: "decision", Namespace: "control"}, Spec: api.ControlPlaneRoleSpec{Rules: []api.ControlPlanePolicyRule{{
		Effect: "allow", Resources: []string{"RuntimeApproval"}, Verbs: []string{"approve"}, Workspaces: []string{"team-a"}, RuntimeApprovalNameSelector: &api.RuntimeApprovalBoundedNameSelector{Prefix: "approval-"},
	}}}}
	binding := api.ControlPlaneRoleBinding{ObjectMeta: api.ObjectMeta{Name: "operator", Namespace: "control"}, Spec: api.ControlPlaneRoleBindingSpec{
		RoleRef: api.LocalObjectReference{Name: "decision"}, Subjects: []api.ControlPlaneRoleSubject{{Method: identity.Method, Issuer: identity.Issuer, Subject: identity.Subject}},
	}}
	roleData, _ := json.Marshal(role)
	bindingData, _ := json.Marshal(binding)
	policy := &policyStore{bindings: [][]byte{bindingData}, roles: map[string][]byte{"control/decision": roleData}}
	authorizer := New(policy)

	for _, test := range []struct {
		name      string
		identity  *auth.Identity
		workspace string
		approval  string
		verb      string
		want      error
	}{
		{name: "exact scope", identity: identity, workspace: "team-a", approval: "approval-123", verb: "approve"},
		{name: "wrong method", identity: &auth.Identity{Method: "mtls", Issuer: identity.Issuer, Subject: identity.Subject}, workspace: "team-a", approval: "approval-123", verb: "approve", want: ErrForbidden},
		{name: "wrong issuer", identity: &auth.Identity{Method: identity.Method, Issuer: "https://forged.example", Subject: identity.Subject}, workspace: "team-a", approval: "approval-123", verb: "approve", want: ErrForbidden},
		{name: "wrong subject", identity: &auth.Identity{Method: identity.Method, Issuer: identity.Issuer, Subject: "operator-2"}, workspace: "team-a", approval: "approval-123", verb: "approve", want: ErrForbidden},
		{name: "wrong workspace", identity: identity, workspace: "team-b", approval: "approval-123", verb: "approve", want: ErrForbidden},
		{name: "wrong name", identity: identity, workspace: "team-a", approval: "other-123", verb: "approve", want: ErrForbidden},
		{name: "wrong verb", identity: identity, workspace: "team-a", approval: "approval-123", verb: "reject", want: ErrForbidden},
		{name: "bootstrap token", identity: &auth.Identity{Method: "token", Subject: identity.Subject}, workspace: "team-a", approval: "approval-123", verb: "approve", want: ErrForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := authorizer.AuthorizeRuntimeApproval(context.Background(), test.identity, test.workspace, test.approval, test.verb); !errors.Is(err, test.want) || (test.want == nil && err != nil) {
				t.Fatalf("AuthorizeRuntimeApproval() error = %v, want %v", err, test.want)
			}
		})
	}

	policy.listErr = errors.New("database unavailable")
	if err := authorizer.AuthorizeRuntimeApproval(context.Background(), identity, "team-a", "approval-123", "approve"); !errors.Is(err, ErrUnavailable) || !errors.Is(err, policy.listErr) {
		t.Fatalf("unavailable policy store error = %v, want %v", err, ErrUnavailable)
	}
	policy.listErr = nil
	policy.getErr = errors.New("database unavailable")
	if err := authorizer.AuthorizeRuntimeApproval(context.Background(), identity, "team-a", "approval-123", "approve"); !errors.Is(err, ErrUnavailable) || !errors.Is(err, policy.getErr) {
		t.Fatalf("unavailable role lookup error = %v, want %v", err, ErrUnavailable)
	}
}

func TestAuthorizeRuntimeApprovalPreservesMalformedPolicyErrors(t *testing.T) {
	identity := &auth.Identity{Method: "oidc", Issuer: "https://issuer.example", Subject: "operator-1"}
	binding := api.ControlPlaneRoleBinding{ObjectMeta: api.ObjectMeta{Name: "operator", Namespace: "control"}, Spec: api.ControlPlaneRoleBindingSpec{
		RoleRef: api.LocalObjectReference{Name: "decision"}, Subjects: []api.ControlPlaneRoleSubject{{Method: identity.Method, Issuer: identity.Issuer, Subject: identity.Subject}},
	}}
	bindingData, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		policy *policyStore
	}{
		{name: "binding", policy: &policyStore{bindings: [][]byte{[]byte(`{"metadata":`)}}},
		{name: "role", policy: &policyStore{bindings: [][]byte{bindingData}, roles: map[string][]byte{"control/decision": []byte(`{"metadata":`)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := New(test.policy).AuthorizeRuntimeApproval(context.Background(), identity, "team-a", "approval-123", "approve")
			var syntaxErr *json.SyntaxError
			if !errors.Is(err, ErrUnavailable) || !errors.As(err, &syntaxErr) {
				t.Fatalf("malformed policy error = %v, want unavailable and JSON syntax error", err)
			}
		})
	}
}
