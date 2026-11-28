// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package accesscatalog

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

const rbacGroup = "rbac.authorization.k8s.io"

var (
	readVerbs  = []string{"get", "list", "watch"}
	writeVerbs = []string{"get", "list", "watch", "create", "update", "patch", "delete"}

	// requestVerbs is every verb the matrix probes, including verbs no role may hold.
	requestVerbs = []string{
		"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection",
		"use", "invoke", "approve", "reject", "reconcile", "bind", "escalate", "impersonate",
	}
	requestSubresources = []string{"", "status", "decision", "reconcile"}

	clusterScopedKinds = map[string]bool{
		"AutonomyClass": true, "KnowledgeBaseClass": true, "KnowledgeBindingRecord": true,
		"MemoryClass": true, "ModelClass": true, "ToolClass": true,
	}
)

type expectedGrant struct {
	kinds []string
	sub   string
	verbs []string
}

type expectedRole struct {
	name      string
	binding   Binding
	aggregate string
	grants    []expectedGrant
}

func kindsOf(classes ...Class) []string {
	var kinds []string
	for kind, class := range expectedClasses {
		for _, want := range classes {
			if class == want {
				kinds = append(kinds, kind)
			}
		}
	}
	sort.Strings(kinds)
	return kinds
}

func namespacedKindsOf(classes ...Class) []string {
	var kinds []string
	for _, kind := range kindsOf(classes...) {
		if !clusterScopedKinds[kind] {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// expectedRoles is plan section 6 encoded independently of the catalog.
func expectedRoles() []expectedRole {
	runViewer := []expectedGrant{{kinds: namespacedKindsOf(ClassSubmit, ClassRuntime, ClassDecision), verbs: readVerbs}}
	return []expectedRole{
		{name: "kognition-viewer", binding: BindingNamespace, aggregate: "view", grants: []expectedGrant{
			{kinds: namespacedKindsOf(ClassConfig, ClassKnowledge, ClassGovernance, ClassGrant, ClassPlatform), verbs: readVerbs},
		}},
		{name: "kognition-run-viewer", binding: BindingNamespace, grants: runViewer},
		{name: "kognition-data-reader", binding: BindingNamespace, grants: []expectedGrant{
			{kinds: []string{"MemoryRecord"}, verbs: readVerbs},
		}},
		{name: "kognition-builder", binding: BindingNamespace, aggregate: "edit", grants: []expectedGrant{
			{kinds: namespacedKindsOf(ClassConfig), verbs: writeVerbs},
			{kinds: []string{"EvaluationRun"}, verbs: []string{"get", "list", "watch", "create", "delete"}},
		}},
		{name: "kognition-curator", binding: BindingNamespace, aggregate: "edit", grants: []expectedGrant{
			{kinds: namespacedKindsOf(ClassKnowledge), verbs: writeVerbs},
		}},
		{name: "kognition-consumer", binding: BindingNamespace, grants: []expectedGrant{
			{kinds: []string{"WorkflowRun", "AgentJob"}, verbs: []string{"create"}},
			{kinds: []string{"Workflow"}, verbs: []string{"use"}},
			{kinds: []string{"Agent"}, verbs: []string{"invoke"}},
		}},
		{name: "kognition-operator", binding: BindingNamespace, grants: append(append([]expectedGrant{}, runViewer...),
			expectedGrant{kinds: []string{"RuntimeApproval"}, sub: "reconcile", verbs: []string{"reconcile"}})},
		{name: "kognition-approver", binding: BindingNamespace, grants: []expectedGrant{
			{kinds: []string{"RuntimeApproval"}, verbs: readVerbs},
			{kinds: []string{"ApprovalDecision"}, verbs: []string{"get", "list", "watch", "create"}},
			{kinds: []string{"RuntimeApproval"}, sub: "decision", verbs: []string{"approve", "reject"}},
		}},
		{name: "kognition-autonomy-reviewer", binding: BindingNamespace, grants: []expectedGrant{
			{kinds: []string{"AutonomyReview", "TrustProfile"}, verbs: readVerbs},
			{kinds: []string{"AutonomyReview"}, sub: "status", verbs: []string{"update", "patch"}},
		}},
		{name: "kognition-policy-admin", binding: BindingNamespace, grants: []expectedGrant{
			{kinds: namespacedKindsOf(ClassGovernance), verbs: writeVerbs},
		}},
		{name: "kognition-grant-admin", binding: BindingNamespace, grants: []expectedGrant{
			{kinds: namespacedKindsOf(ClassGrant, ClassCredential), verbs: writeVerbs},
		}},
		{name: "kognition-access-admin", binding: BindingNamespace},
		{name: "kognition-auditor", binding: BindingNamespace, grants: []expectedGrant{
			{kinds: namespacedKindsOf(ClassConfig, ClassKnowledge, ClassSubmit, ClassRuntime, ClassGrant, ClassGovernance, ClassDecision, ClassPlatform, ClassSystem), verbs: readVerbs},
		}},
		{name: "kognition-class-viewer", binding: BindingCluster, grants: []expectedGrant{
			{kinds: kindsOf(ClassClass), verbs: readVerbs},
		}},
		{name: "kognition-platform-admin", binding: BindingCluster, grants: []expectedGrant{
			{kinds: kindsOf(ClassClass, ClassPlatform), verbs: writeVerbs},
			{kinds: kindsOf(ClassSystem), verbs: readVerbs},
		}},
	}
}

func (r expectedRole) allows(kind, sub, verb string) bool {
	for _, grant := range r.grants {
		if grant.sub == sub && contains(grant.kinds, kind) && contains(grant.verbs, verb) {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ruleAllows is a reference RBAC evaluator: additive rules, exact matching.
func ruleAllows(rules []rbacv1.PolicyRule, group, resource, verb, name string) bool {
	for _, rule := range rules {
		if !contains(rule.APIGroups, group) || !contains(rule.Resources, resource) || !contains(rule.Verbs, verb) {
			continue
		}
		if len(rule.ResourceNames) > 0 && !contains(rule.ResourceNames, name) {
			continue
		}
		return true
	}
	return false
}

func pluralByKind(t *testing.T) map[string]string {
	t.Helper()
	plurals := map[string]string{}
	for _, crd := range loadShippedCRDs(t) {
		plurals[crd.Kind] = crd.Resource
	}
	return plurals
}

func resourceFor(plural, sub string) string {
	if sub == "" {
		return plural
	}
	return plural + "/" + sub
}

func TestRoleCatalogMatchesPlan(t *testing.T) {
	want := expectedRoles()
	got := Default().Roles
	if len(got) != len(want) {
		t.Fatalf("catalog has %d roles, plan has %d", len(got), len(want))
	}
	for i, role := range want {
		if got[i].Name != role.name {
			t.Errorf("role %d is %q, plan says %q", i, got[i].Name, role.name)
			continue
		}
		if got[i].Binding != role.binding {
			t.Errorf("%s binding = %q, plan says %q", role.name, got[i].Binding, role.binding)
		}
		if got[i].AggregateTo != role.aggregate {
			t.Errorf("%s aggregation target = %q, plan says %q", role.name, got[i].AggregateTo, role.aggregate)
		}
		if strings.TrimSpace(got[i].Summary) == "" {
			t.Errorf("%s has no summary", role.name)
		}
	}
}

func TestGeneratedRulesMatchPlanMatrix(t *testing.T) {
	plurals := pluralByKind(t)
	catalog := Default()
	for _, role := range expectedRoles() {
		t.Run(role.name, func(t *testing.T) {
			rules, err := catalog.PolicyRules(role.name)
			if err != nil {
				t.Fatalf("PolicyRules: %v", err)
			}
			for kind := range expectedClasses {
				for _, sub := range requestSubresources {
					for _, verb := range requestVerbs {
						want := role.allows(kind, sub, verb)
						got := ruleAllows(rules, APIGroup, resourceFor(plurals[kind], sub), verb, "")
						if got != want {
							t.Errorf("%s %s%s: generated rules allow=%v, plan says %v", verb, kind, subSuffix(sub), got, want)
						}
						if permits := catalog.Permits(role.name, kind, sub, verb); permits != want {
							t.Errorf("%s %s%s: catalog Permits=%v, plan says %v", verb, kind, subSuffix(sub), permits, want)
						}
					}
				}
			}
		})
	}
}

func subSuffix(sub string) string {
	if sub == "" {
		return ""
	}
	return "/" + sub
}

// requireCatalog fails loops over an empty catalog, which would pass vacuously.
func requireCatalog(t *testing.T, catalog Catalog) {
	t.Helper()
	if len(catalog.Kinds) == 0 || len(catalog.Roles) == 0 {
		t.Fatal("catalog is empty; invariant checks would pass vacuously")
	}
}

func TestGeneratedRulesSafetyInvariants(t *testing.T) {
	plurals := pluralByKind(t)
	systemResources := map[string]bool{}
	clusterResources := map[string]bool{}
	for kind, class := range expectedClasses {
		if class == ClassSystem {
			systemResources[plurals[kind]] = true
		}
		if clusterScopedKinds[kind] {
			clusterResources[plurals[kind]] = true
		}
	}
	catalog := Default()
	requireCatalog(t, catalog)
	for _, role := range catalog.Roles {
		t.Run(role.Name, func(t *testing.T) {
			rules, err := catalog.PolicyRules(role.Name)
			if err != nil {
				t.Fatalf("PolicyRules: %v", err)
			}
			if len(rules) == 0 {
				t.Fatal("role has no rules")
			}
			for _, rule := range rules {
				if len(rule.NonResourceURLs) != 0 {
					t.Errorf("non-resource URLs are not allowed: %v", rule.NonResourceURLs)
				}
				for _, group := range rule.APIGroups {
					if group != APIGroup && !(group == rbacGroup && role.Name == "kognition-access-admin") {
						t.Errorf("API group %q is not allowed for %s", group, role.Name)
					}
				}
				for _, value := range append(append(append(append([]string{}, rule.APIGroups...), rule.Resources...), rule.Verbs...), rule.ResourceNames...) {
					if strings.Contains(value, "*") {
						t.Errorf("wildcard %q in %v", value, rule)
					}
				}
				for _, verb := range rule.Verbs {
					if verb == "deletecollection" || verb == "escalate" || verb == "impersonate" {
						t.Errorf("verb %q must never be shipped (%v)", verb, rule)
					}
				}
				for _, resource := range rule.Resources {
					base, sub, _ := strings.Cut(resource, "/")
					switch base {
					case "secrets", "pods", "nodes", "configmaps", "serviceaccounts":
						t.Errorf("core resource %q must never be shipped", resource)
					}
					if sub == "status" && !(role.Name == "kognition-autonomy-reviewer" && resource == "autonomyreviews/status") {
						t.Errorf("%s must not write %s; controllers own status", role.Name, resource)
					}
					if systemResources[base] {
						for _, verb := range rule.Verbs {
							if !contains(readVerbs, verb) {
								t.Errorf("%s grants %q on system resource %s", role.Name, verb, resource)
							}
						}
					}
					if role.Binding == BindingNamespace && clusterResources[base] {
						t.Errorf("%s is bound per namespace but names cluster-scoped %s", role.Name, resource)
					}
				}
				if contains(rule.Verbs, "bind") && role.Name != "kognition-access-admin" {
					t.Errorf("only kognition-access-admin may hold bind, found in %s", role.Name)
				}
			}
		})
	}
}

func TestOnlyTheAutonomyReviewerWritesStatus(t *testing.T) {
	crds := loadShippedCRDs(t)
	catalog := Default()
	requireCatalog(t, catalog)
	for _, crd := range crds {
		if crd.Kind == "AutonomyReview" && !crd.HasStatus {
			t.Fatal("the autonomy decision exception assumes AutonomyReview has a status subresource")
		}
		for _, role := range catalog.Roles {
			for _, verb := range []string{"create", "update", "patch", "delete"} {
				allowed := catalog.Permits(role.Name, crd.Kind, "status", verb)
				exception := role.Name == "kognition-autonomy-reviewer" && crd.Kind == "AutonomyReview" && (verb == "update" || verb == "patch")
				if allowed && !exception {
					t.Errorf("%s may %s %s/status", role.Name, verb, crd.Kind)
				}
			}
		}
	}
}

// SecretRef with the internal backend stores the credential in an annotation,
// so any role that can read the object can read the credential.
func TestOnlyGrantAdminReachesCredentialKinds(t *testing.T) {
	catalog := Default()
	requireCatalog(t, catalog)
	reachable := 0
	for _, kind := range catalog.Kinds {
		if kind.Class != ClassCredential {
			continue
		}
		for _, role := range catalog.Roles {
			for _, verb := range requestVerbs {
				if catalog.Permits(role.Name, kind.Name, "", verb) {
					reachable++
					if role.Name != "kognition-grant-admin" {
						t.Errorf("%s may %s %s, which can carry credential values", role.Name, verb, kind.Name)
					}
				}
			}
		}
	}
	if reachable == 0 {
		t.Fatal("kognition-grant-admin must be able to manage the credential class")
	}
}

func TestNoRoleCombinesAuthoringDelegationAndDecisions(t *testing.T) {
	catalog := Default()
	requireCatalog(t, catalog)
	for _, role := range catalog.Roles {
		duties := 0
		if catalog.Permits(role.Name, "Agent", "", "create") {
			duties++
		}
		if catalog.Permits(role.Name, "ToolGrant", "", "create") {
			duties++
		}
		if catalog.Permits(role.Name, "RuntimeApproval", "decision", "approve") ||
			catalog.Permits(role.Name, "ApprovalDecision", "", "create") ||
			catalog.Permits(role.Name, "AutonomyReview", "status", "update") {
			duties++
		}
		if duties > 1 {
			t.Errorf("%s combines %d of authoring, delegation and decision duties", role.Name, duties)
		}
	}
}

func TestSecretsAndPortForwardAreNeverGranted(t *testing.T) {
	catalog := Default()
	requireCatalog(t, catalog)
	for _, role := range catalog.Roles {
		rules, err := catalog.PolicyRules(role.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, verb := range requestVerbs {
			for _, resource := range []string{"secrets", "pods/portforward", "pods/exec", "serviceaccounts/token"} {
				if ruleAllows(rules, "", resource, verb, "x") {
					t.Errorf("%s allows %s on %s", role.Name, verb, resource)
				}
			}
		}
	}
}

func TestAccessAdminCanBindOnlyKognitionRoles(t *testing.T) {
	catalog := Default()
	rules, err := catalog.PolicyRules("kognition-access-admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range catalog.Roles {
		if !ruleAllows(rules, rbacGroup, "clusterroles", "bind", role.Name) {
			t.Errorf("access-admin cannot bind %s", role.Name)
		}
	}
	denied := []struct{ resource, verb, name string }{
		{"clusterroles", "bind", "cluster-admin"},
		{"clusterroles", "bind", "admin"},
		{"clusterroles", "bind", "edit"},
		{"clusterroles", "bind", ""},
		{"clusterroles", "get", "kognition-viewer"},
		{"clusterroles", "escalate", "kognition-viewer"},
		{"clusterroles", "create", ""},
		{"clusterrolebindings", "create", ""},
		{"clusterrolebindings", "get", ""},
		{"roles", "create", ""},
		{"roles", "update", ""},
		{"roles", "delete", ""},
	}
	for _, request := range denied {
		if ruleAllows(rules, rbacGroup, request.resource, request.verb, request.name) {
			t.Errorf("access-admin must not %s %s %q", request.verb, request.resource, request.name)
		}
	}
	allowed := []struct{ resource, verb string }{
		{"roles", "get"}, {"roles", "list"}, {"roles", "watch"},
		{"rolebindings", "get"}, {"rolebindings", "list"}, {"rolebindings", "watch"},
		{"rolebindings", "create"}, {"rolebindings", "update"}, {"rolebindings", "patch"}, {"rolebindings", "delete"},
	}
	for _, request := range allowed {
		if !ruleAllows(rules, rbacGroup, request.resource, request.verb, "") {
			t.Errorf("access-admin should %s %s", request.verb, request.resource)
		}
	}
	for _, kind := range []string{"Agent", "ToolGrant", "WorkflowRun"} {
		for _, verb := range requestVerbs {
			if catalog.Permits("kognition-access-admin", kind, "", verb) {
				t.Errorf("access-admin must not manage Kognition kind %s (%s)", kind, verb)
			}
		}
	}
}

func TestRulesAreCanonicalAndDeterministic(t *testing.T) {
	catalog := Default()
	requireCatalog(t, catalog)
	for _, role := range catalog.Roles {
		first, err := catalog.PolicyRules(role.Name)
		if err != nil {
			t.Fatal(err)
		}
		second, err := catalog.PolicyRules(role.Name)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Errorf("%s rules differ between calls", role.Name)
		}
		for _, rule := range first {
			for _, values := range [][]string{rule.APIGroups, rule.Resources, rule.Verbs, rule.ResourceNames} {
				if !sort.StringsAreSorted(values) {
					t.Errorf("%s has an unsorted list %v", role.Name, values)
				}
				for i := 1; i < len(values); i++ {
					if values[i] == values[i-1] {
						t.Errorf("%s has a duplicate %q", role.Name, values[i])
					}
				}
			}
		}
	}
}

func TestPolicyRulesRejectsUnsafeOrInconsistentGrants(t *testing.T) {
	base := Catalog{Kinds: []Kind{
		{Name: "Agent", Resource: "agents", Scope: ScopeNamespaced, Class: ClassConfig},
		{Name: "ModelClass", Resource: "modelclasses", Scope: ScopeCluster, Class: ClassClass},
	}}
	namespaced := func(grant Grant) Role {
		return Role{Name: "r", Binding: BindingNamespace, Grants: []Grant{grant}}
	}
	cases := map[string]Role{
		"wildcard verb":                 namespaced(Grant{Kinds: []string{"Agent"}, Verbs: []string{"*"}}),
		"deletecollection":              namespaced(Grant{Kinds: []string{"Agent"}, Verbs: []string{"deletecollection"}}),
		"escalate":                      namespaced(Grant{Kinds: []string{"Agent"}, Verbs: []string{"escalate"}}),
		"impersonate":                   namespaced(Grant{Kinds: []string{"Agent"}, Verbs: []string{"impersonate"}}),
		"unknown kind":                  namespaced(Grant{Kinds: []string{"Nope"}, Verbs: []string{"get"}}),
		"cluster kind in namespaced":    namespaced(Grant{Kinds: []string{"ModelClass"}, Verbs: []string{"get"}}),
		"unknown class":                 namespaced(Grant{Classes: []Class{"mystery"}, Verbs: []string{"get"}}),
		"class with no namespaced kind": namespaced(Grant{Classes: []Class{ClassClass}, Verbs: []string{"get"}}),
		"no verbs":                      namespaced(Grant{Kinds: []string{"Agent"}}),
		"explicit and kinds mixed":      namespaced(Grant{Kinds: []string{"Agent"}, Resources: []string{"roles"}, APIGroup: rbacGroup, Verbs: []string{"get"}}),
		"explicit without group":        namespaced(Grant{Resources: []string{"roles"}, Verbs: []string{"get"}}),
		"wildcard resource":             namespaced(Grant{Resources: []string{"*"}, APIGroup: rbacGroup, Verbs: []string{"get"}}),
		"wildcard group":                namespaced(Grant{Resources: []string{"roles"}, APIGroup: "*", Verbs: []string{"get"}}),
		"wildcard resource name":        namespaced(Grant{Resources: []string{"clusterroles"}, APIGroup: rbacGroup, Verbs: []string{"bind"}, ResourceNames: []string{"*"}}),
	}
	for name, role := range cases {
		t.Run(name, func(t *testing.T) {
			catalog := base
			catalog.Roles = []Role{role}
			if rules, err := catalog.PolicyRules("r"); err == nil {
				t.Fatalf("expected an error, got rules %+v", rules)
			}
		})
	}

	t.Run("a cluster-bound role may name cluster-scoped kinds", func(t *testing.T) {
		catalog := base
		catalog.Roles = []Role{{Name: "r", Binding: BindingCluster, Grants: []Grant{{Classes: []Class{ClassClass}, Verbs: []string{"get"}}}}}
		rules, err := catalog.PolicyRules("r")
		if err != nil || len(rules) != 1 || !reflect.DeepEqual(rules[0].Resources, []string{"modelclasses"}) {
			t.Fatalf("rules = %+v, err = %v", rules, err)
		}
	})
}

func TestPolicyRulesRejectsAnUnknownRole(t *testing.T) {
	if _, err := Default().PolicyRules("kognition-nonexistent"); err == nil {
		t.Fatal("expected an error for an unknown role")
	}
	if Default().Permits("kognition-nonexistent", "Agent", "", "get") {
		t.Fatal("an unknown role must permit nothing")
	}
	if Default().Permits("kognition-viewer", "NoSuchKind", "", "get") {
		t.Fatal("an unknown kind must be denied")
	}
}

func TestClusterRolesCarryTheRoleLabelAndNoBindingOrAggregation(t *testing.T) {
	catalog := Default()
	roles, err := catalog.ClusterRoles()
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != len(catalog.Roles) {
		t.Fatalf("got %d ClusterRoles for %d roles", len(roles), len(catalog.Roles))
	}
	for i, role := range roles {
		if role.Name != catalog.Roles[i].Name {
			t.Errorf("ClusterRole %d is %q, want %q (catalog order)", i, role.Name, catalog.Roles[i].Name)
		}
		if want := strings.TrimPrefix(role.Name, "kognition-"); role.Labels[RoleLabel] != want {
			t.Errorf("%s label %s = %q, want %q", role.Name, RoleLabel, role.Labels[RoleLabel], want)
		}
		if role.AggregationRule != nil {
			t.Errorf("%s must not define an aggregation rule", role.Name)
		}
		for key := range role.Labels {
			if strings.HasPrefix(key, "rbac.authorization.k8s.io/aggregate-to-") {
				t.Errorf("%s carries aggregation label %s by default", role.Name, key)
			}
		}
		rules, _ := catalog.PolicyRules(role.Name)
		if !reflect.DeepEqual(role.Rules, rules) {
			t.Errorf("%s ClusterRole rules differ from PolicyRules", role.Name)
		}
	}
}
