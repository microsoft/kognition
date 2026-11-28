// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package accesscatalog

import (
	"fmt"
	"sort"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RoleLabel marks a ClusterRole as a Kognition human role. Its value is the role
// name without the "kognition-" prefix.
const RoleLabel = "kognition.dev/access-role"

// neverShipped are verbs no human role may hold. A role that needs one must be
// designed explicitly instead of added to the catalog.
var neverShipped = map[string]bool{"deletecollection": true, "escalate": true, "impersonate": true}

func (c Catalog) role(name string) (Role, bool) {
	for _, role := range c.Roles {
		if role.Name == name {
			return role, true
		}
	}
	return Role{}, false
}

func (c Catalog) kind(name string) (Kind, bool) {
	for _, kind := range c.Kinds {
		if kind.Name == name {
			return kind, true
		}
	}
	return Kind{}, false
}

// grantKinds returns the Kognition kinds a grant covers for a role. A role bound
// per namespace never receives cluster-scoped kinds from a class, and naming one
// explicitly is an error because the binding could not grant it.
func (c Catalog) grantKinds(role Role, grant Grant) ([]Kind, error) {
	var covered []Kind
	seen := map[string]bool{}
	add := func(kind Kind) {
		if !seen[kind.Name] {
			seen[kind.Name] = true
			covered = append(covered, kind)
		}
	}
	for _, name := range grant.Kinds {
		kind, ok := c.kind(name)
		if !ok {
			return nil, fmt.Errorf("role %s grants unknown kind %q", role.Name, name)
		}
		if role.Binding == BindingNamespace && kind.Scope == ScopeCluster {
			return nil, fmt.Errorf("role %s is bound per namespace but grants cluster-scoped kind %s", role.Name, kind.Name)
		}
		add(kind)
	}
	for _, class := range grant.Classes {
		if !validClass(class) {
			return nil, fmt.Errorf("role %s grants unknown class %q", role.Name, class)
		}
		for _, kind := range c.Kinds {
			if kind.Class != class || (role.Binding == BindingNamespace && kind.Scope == ScopeCluster) {
				continue
			}
			add(kind)
		}
	}
	return covered, nil
}

func canonical(values []string) []string {
	unique := map[string]bool{}
	for _, value := range values {
		unique[value] = true
	}
	out := make([]string, 0, len(unique))
	for value := range unique {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func rejectUnsafe(role Role, grant Grant, group string, resources []string) error {
	if len(grant.Verbs) == 0 {
		return fmt.Errorf("role %s has a grant without verbs", role.Name)
	}
	for _, verb := range grant.Verbs {
		if neverShipped[verb] {
			return fmt.Errorf("role %s grants verb %q, which no human role may hold", role.Name, verb)
		}
	}
	for _, value := range append(append(append(append([]string{group}, resources...), grant.Verbs...), grant.ResourceNames...), grant.Subresource) {
		if strings.Contains(value, "*") {
			return fmt.Errorf("role %s uses wildcard %q; roles must list resources and verbs explicitly", role.Name, value)
		}
	}
	return nil
}

// PolicyRules returns the explicit RBAC rules for a role. Grants with the same
// API group, verbs and resource names are merged, and every list is sorted, so
// the output is stable.
func (c Catalog) PolicyRules(roleName string) ([]rbacv1.PolicyRule, error) {
	role, ok := c.role(roleName)
	if !ok {
		return nil, fmt.Errorf("unknown role %q", roleName)
	}
	type key struct{ group, verbs, names string }
	merged := map[key]*rbacv1.PolicyRule{}
	for _, grant := range role.Grants {
		group := APIGroup
		var resources []string
		if len(grant.Resources) > 0 {
			if len(grant.Classes) > 0 || len(grant.Kinds) > 0 || grant.Subresource != "" {
				return nil, fmt.Errorf("role %s mixes explicit resources with Kognition kinds in one grant", role.Name)
			}
			if grant.APIGroup == "" {
				return nil, fmt.Errorf("role %s grants explicit resources without an API group", role.Name)
			}
			group, resources = grant.APIGroup, grant.Resources
		} else {
			kinds, err := c.grantKinds(role, grant)
			if err != nil {
				return nil, err
			}
			if len(kinds) == 0 {
				return nil, fmt.Errorf("role %s has a grant that matches no kinds", role.Name)
			}
			for _, kind := range kinds {
				resource := kind.Resource
				if grant.Subresource != "" {
					resource += "/" + grant.Subresource
				}
				resources = append(resources, resource)
			}
		}
		if err := rejectUnsafe(role, grant, group, resources); err != nil {
			return nil, err
		}
		verbs, names := canonical(grant.Verbs), canonical(grant.ResourceNames)
		k := key{group, strings.Join(verbs, ","), strings.Join(names, ",")}
		rule, exists := merged[k]
		if !exists {
			rule = &rbacv1.PolicyRule{APIGroups: []string{group}, Verbs: verbs}
			if len(names) > 0 {
				rule.ResourceNames = names
			}
			merged[k] = rule
		}
		rule.Resources = append(rule.Resources, resources...)
	}

	rules := make([]rbacv1.PolicyRule, 0, len(merged))
	for _, rule := range merged {
		rule.Resources = canonical(rule.Resources)
		rules = append(rules, *rule)
	}
	sort.Slice(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		if a.APIGroups[0] != b.APIGroups[0] {
			return a.APIGroups[0] < b.APIGroups[0]
		}
		if a.Resources[0] != b.Resources[0] {
			return a.Resources[0] < b.Resources[0]
		}
		return strings.Join(a.Verbs, ",") < strings.Join(b.Verbs, ",")
	})
	return rules, nil
}

// Permits reports whether a role allows verb on a Kognition kind (and optional
// subresource) according to the catalog's grants, without generating rules.
// Unknown roles and kinds are denied.
func (c Catalog) Permits(roleName, kindName, subresource, verb string) bool {
	role, ok := c.role(roleName)
	if !ok {
		return false
	}
	for _, grant := range role.Grants {
		if len(grant.Resources) > 0 || grant.Subresource != subresource || !hasString(grant.Verbs, verb) {
			continue
		}
		kinds, err := c.grantKinds(role, grant)
		if err != nil {
			continue
		}
		for _, kind := range kinds {
			if kind.Name == kindName {
				return true
			}
		}
	}
	return false
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ClusterRoles returns every human role as a ClusterRole, in catalog order. The
// roles carry no bindings and no aggregation rule.
func (c Catalog) ClusterRoles() ([]rbacv1.ClusterRole, error) {
	roles := make([]rbacv1.ClusterRole, 0, len(c.Roles))
	for _, role := range c.Roles {
		rules, err := c.PolicyRules(role.Name)
		if err != nil {
			return nil, err
		}
		roles = append(roles, rbacv1.ClusterRole{
			TypeMeta: metav1.TypeMeta{APIVersion: rbacAPIGroup + "/v1", Kind: "ClusterRole"},
			ObjectMeta: metav1.ObjectMeta{
				Name:   role.Name,
				Labels: map[string]string{RoleLabel: strings.TrimPrefix(role.Name, "kognition-")},
			},
			Rules: rules,
		})
	}
	return roles, nil
}
