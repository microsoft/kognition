// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	agentRoleKind        = "AgentRole"
	agentRoleBindingKind = "AgentRoleBinding"
)

// RBACEngine evaluates whether an agent identity is allowed to perform
// a given verb on a given resource kind.
type RBACEngine struct {
	store  store.Store
	logger *zap.Logger
}

// NewRBACEngine creates a new RBAC evaluation engine.
func NewRBACEngine(s store.Store, logger *zap.Logger) *RBACEngine {
	return &RBACEngine{
		store:  s,
		logger: logger.With(zap.String("component", "rbac-engine")),
	}
}

// Evaluate checks if the given identity is permitted to perform verb on resourceKind
// within the identity's namespace.
func (e *RBACEngine) Evaluate(ctx context.Context, identity *v1alpha1.AgentIdentity, verb, resourceKind string) (bool, error) {
	roles, err := e.EvaluateRoles(ctx, identity, verb, resourceKind)
	return len(roles) > 0, err
}

// EvaluateRoles returns the persisted roles that permit this action. It keeps
// the existing RBAC decision while exposing provenance for gateway audit.
func (e *RBACEngine) EvaluateRoles(ctx context.Context, identity *v1alpha1.AgentIdentity, verb, resourceKind string) ([]string, error) {
	namespace := identity.ObjectMeta.Namespace

	// List all role bindings in this namespace.
	rawBindings, err := e.store.List(ctx, agentRoleBindingKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return nil, fmt.Errorf("list role bindings: %w", err)
	}

	// Find bindings that reference this identity.
	var boundRoleNames []string
	for _, raw := range rawBindings {
		var binding v1alpha1.AgentRoleBinding
		if err := json.Unmarshal(raw, &binding); err != nil {
			continue
		}
		if subjectsMatch(binding.Spec.Subjects, identity) {
			boundRoleNames = append(boundRoleNames, binding.Spec.RoleRef.Name)
		}
	}

	if len(boundRoleNames) == 0 {
		e.logger.Debug("no role bindings found for identity",
			zap.String("identity", identity.ObjectMeta.Name),
			zap.String("namespace", namespace))
		return nil, nil
	}

	// Evaluate each bound role.
	for _, roleName := range boundRoleNames {
		rawRole, err := e.store.Get(ctx, agentRoleKind, namespace, roleName)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("get role %q: %w", roleName, err)
		}

		var role v1alpha1.AgentRole
		if err := json.Unmarshal(rawRole, &role); err != nil {
			continue
		}

		if rolePermits(&role, verb, resourceKind) {
			return []string{fmt.Sprintf("%s/%s@%d", namespace, role.ObjectMeta.Name, role.ObjectMeta.ResourceVersion)}, nil
		}
	}

	return nil, nil
}

// subjectsMatch returns true if the identity matches any subject in the list.
func subjectsMatch(subjects []v1alpha1.Subject, identity *v1alpha1.AgentIdentity) bool {
	for _, s := range subjects {
		switch s.Kind {
		case "AgentIdentity":
			if s.Name == identity.ObjectMeta.Name {
				return true
			}
		case "Agent":
			if s.Name == identity.Spec.AgentRef.Name {
				return true
			}
		}
	}
	return false
}

// rolePermits checks if the role allows the given verb on the resource kind.
func rolePermits(role *v1alpha1.AgentRole, verb, resourceKind string) bool {
	for _, rule := range role.Rules {
		if matchesResource(rule.Resources, resourceKind) && matchesVerb(rule.Verbs, verb) {
			return true
		}
	}
	return false
}

// matchesResource checks if resourceKind matches any entry in the resources list.
func matchesResource(resources []string, kind string) bool {
	for _, r := range resources {
		if r == "*" || r == kind {
			return true
		}
	}
	return false
}

// matchesVerb checks if verb matches any entry in the verbs list.
func matchesVerb(verbs []string, verb string) bool {
	for _, v := range verbs {
		if v == "*" || v == verb {
			return true
		}
	}
	return false
}
