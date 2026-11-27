// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controlplaneauth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/store"
	"k8s.io/apimachinery/pkg/util/validation"
)

var (
	ErrForbidden   = errors.New("control-plane authorization denied")
	ErrUnavailable = errors.New("control-plane authorization unavailable")
)

const (
	runtimeApprovalResource = "RuntimeApproval"
	MaxExecutionRules       = 128
)

// Authorizer evaluates persisted standalone control-plane roles and bindings.
type Authorizer struct {
	store store.Store
}

func New(st store.Store) *Authorizer {
	return &Authorizer{store: st}
}

// AuthorizeRuntimeApproval applies default deny and order-independent deny precedence.
func (a *Authorizer) AuthorizeRuntimeApproval(ctx context.Context, identity *auth.Identity, workspace, name, verb string) error {
	if !canonicalIdentity(identity) || workspace == "" || name == "" || !validVerb(verb) {
		return ErrForbidden
	}
	return a.authorize(ctx, identity, func(role api.ControlPlaneRole) (bool, bool, error) {
		allowed, denied := false, false
		for _, rule := range role.Spec.Rules {
			if !matchesRule(rule, workspace, name, verb) {
				continue
			}
			switch rule.Effect {
			case "deny":
				denied = true
			case "allow":
				allowed = true
			default:
				return false, false, ErrUnavailable
			}
		}
		return allowed, denied, nil
	})
}

func (a *Authorizer) AuthorizeWorkflowUse(ctx context.Context, identity *auth.Identity, workspace, name, uid, revisionDigest string) error {
	return a.authorizeExecution(ctx, identity, api.ControlPlaneExecutionRule{
		Effect: "allow", Operation: api.ControlPlaneWorkflowUse, Workspace: workspace,
		ResourceName: name, ResourceUID: uid, RevisionDigest: revisionDigest,
	})
}

func (a *Authorizer) AuthorizeAgentInvocation(ctx context.Context, identity *auth.Identity, workspace, name, uid string) error {
	return a.authorizeExecution(ctx, identity, api.ControlPlaneExecutionRule{
		Effect: "allow", Operation: api.ControlPlaneAgentInvoke, Workspace: workspace,
		ResourceName: name, ResourceUID: uid,
	})
}

func ValidateExecutionRule(rule api.ControlPlaneExecutionRule) error {
	if (rule.Effect != "allow" && rule.Effect != "deny") ||
		len(validation.IsDNS1123Label(rule.Workspace)) != 0 ||
		len(validation.IsDNS1123Subdomain(rule.ResourceName)) != 0 ||
		rule.ResourceUID == "" || len(rule.ResourceUID) > 128 || strings.TrimSpace(rule.ResourceUID) != rule.ResourceUID ||
		strings.ContainsAny(rule.ResourceUID, "*/\\") ||
		strings.ContainsFunc(rule.ResourceUID, func(character rune) bool { return character <= ' ' || character > '~' }) {
		return ErrForbidden
	}
	switch rule.Operation {
	case api.ControlPlaneWorkflowUse:
		digest, err := hex.DecodeString(rule.RevisionDigest)
		if err != nil || len(digest) != 32 || strings.ToLower(rule.RevisionDigest) != rule.RevisionDigest {
			return ErrForbidden
		}
	case api.ControlPlaneAgentInvoke:
		if rule.RevisionDigest != "" {
			return ErrForbidden
		}
	default:
		return ErrForbidden
	}
	return nil
}

func (a *Authorizer) authorizeExecution(ctx context.Context, identity *auth.Identity, requested api.ControlPlaneExecutionRule) error {
	if !canonicalIdentity(identity) || ValidateExecutionRule(requested) != nil {
		return ErrForbidden
	}
	return a.authorize(ctx, identity, func(role api.ControlPlaneRole) (bool, bool, error) {
		if len(role.Spec.ExecutionRules) > MaxExecutionRules {
			return false, false, ErrUnavailable
		}
		allowed, denied := false, false
		for _, rule := range role.Spec.ExecutionRules {
			if ValidateExecutionRule(rule) != nil {
				return false, false, ErrUnavailable
			}
			if rule.Operation != requested.Operation || rule.Workspace != requested.Workspace ||
				rule.ResourceName != requested.ResourceName || rule.ResourceUID != requested.ResourceUID ||
				rule.RevisionDigest != requested.RevisionDigest {
				continue
			}
			allowed = allowed || rule.Effect == "allow"
			denied = denied || rule.Effect == "deny"
		}
		return allowed, denied, nil
	})
}

func (a *Authorizer) authorize(ctx context.Context, identity *auth.Identity, evaluate func(api.ControlPlaneRole) (bool, bool, error)) error {
	if a == nil || a.store == nil {
		return ErrUnavailable
	}
	bindings, err := a.store.List(ctx, "ControlPlaneRoleBinding", store.ListOptions{})
	if err != nil {
		return fmt.Errorf("%w: list role bindings: %w", ErrUnavailable, err)
	}

	allowed := false
	for _, data := range bindings {
		var binding api.ControlPlaneRoleBinding
		if err := json.Unmarshal(data, &binding); err != nil {
			return fmt.Errorf("%w: decode role binding: %w", ErrUnavailable, err)
		}
		if !matchesIdentity(binding.Spec.Subjects, identity) {
			continue
		}
		if binding.Spec.RoleRef.Name == "" {
			return fmt.Errorf("%w: role binding %q has no role reference", ErrUnavailable, binding.Name)
		}

		roleData, err := a.store.Get(ctx, "ControlPlaneRole", binding.Namespace, binding.Spec.RoleRef.Name)
		if err != nil {
			return fmt.Errorf("%w: get role %q: %w", ErrUnavailable, binding.Spec.RoleRef.Name, err)
		}
		var role api.ControlPlaneRole
		if err := json.Unmarshal(roleData, &role); err != nil {
			return fmt.Errorf("%w: decode role %q: %w", ErrUnavailable, binding.Spec.RoleRef.Name, err)
		}
		roleAllowed, roleDenied, err := evaluate(role)
		if err != nil {
			return fmt.Errorf("%w: role %q: %w", ErrUnavailable, role.Name, err)
		}
		if roleDenied {
			return ErrForbidden
		}
		allowed = allowed || roleAllowed
	}

	if !allowed {
		return ErrForbidden
	}
	return nil
}

func canonicalIdentity(identity *auth.Identity) bool {
	return identity != nil && identity.Method != "" && identity.Method != "anonymous" && identity.Method != "token" && identity.Issuer != "" && identity.Subject != ""
}

func validVerb(verb string) bool {
	return verb == "approve" || verb == "reject" || verb == "reconcile"
}

func matchesIdentity(subjects []api.ControlPlaneRoleSubject, identity *auth.Identity) bool {
	for _, subject := range subjects {
		if subject.Method == identity.Method && subject.Issuer == identity.Issuer && subject.Subject == identity.Subject {
			return true
		}
	}
	return false
}

func matchesRule(rule api.ControlPlanePolicyRule, workspace, name, verb string) bool {
	return contains(rule.Resources, runtimeApprovalResource) &&
		contains(rule.Verbs, verb) &&
		contains(rule.Workspaces, workspace) &&
		matchesName(rule, name)
}

func matchesName(rule api.ControlPlanePolicyRule, name string) bool {
	if contains(rule.RuntimeApprovalNames, name) {
		return true
	}
	selector := rule.RuntimeApprovalNameSelector
	return selector != nil && selector.Prefix != "" && strings.HasPrefix(name, selector.Prefix)
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
