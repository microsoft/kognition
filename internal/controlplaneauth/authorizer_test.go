// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controlplaneauth

import (
	"context"
	"encoding/json"
	"errors"
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
