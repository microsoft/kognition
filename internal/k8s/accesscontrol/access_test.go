// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package accesscontrol

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/kognition-ai/kognition/internal/accesscatalog"
)

func gvr(crd accesscatalog.ShippedCRD) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: crd.Group, Version: "v1alpha1", Resource: crd.Resource}
}

func resourceClient(dyn dynamic.Interface, crd accesscatalog.ShippedCRD, namespace string) dynamic.ResourceInterface {
	if crd.Scope == accesscatalog.ScopeNamespaced {
		return dyn.Resource(gvr(crd)).Namespace(namespace)
	}
	return dyn.Resource(gvr(crd))
}

func probeObject(crd accesscatalog.ShippedCRD, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": crd.Group + "/v1alpha1",
		"kind":       crd.Kind,
		"metadata":   map[string]any{"name": name},
	}}
	if crd.Scope == accesscatalog.ScopeNamespaced {
		obj.SetNamespace(namespace)
	}
	return obj
}

// classify turns an API error into an authorization outcome. Anything other than
// Forbidden means the request was authorized and then failed on the object: it
// did not exist (NotFound) or an empty object was invalid (Invalid).
func classify(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsForbidden(err):
		return false, nil
	case apierrors.IsUnauthorized(err):
		return false, fmt.Errorf("the principal's credentials were rejected: %w", err)
	case apierrors.IsNotFound(err), apierrors.IsInvalid(err), apierrors.IsBadRequest(err),
		apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		return true, nil
	default:
		return false, fmt.Errorf("unexpected API error: %w", err)
	}
}

// attempt performs a real request as the principal and reports whether the API
// server authorized it, and whether it created an object.
func attempt(ctx context.Context, p *principal, crd accesscatalog.ShippedCRD, namespace, name, sub, verb string) (allowed, created bool, err error) {
	res := resourceClient(p.dyn, crd, namespace)
	var subs []string
	if sub != "" {
		subs = []string{sub}
	}
	var callErr error
	switch verb {
	case "get":
		_, callErr = res.Get(ctx, name, metav1.GetOptions{}, subs...)
	case "list":
		_, callErr = res.List(ctx, metav1.ListOptions{})
	case "watch":
		seconds := int64(1)
		watcher, watchErr := res.Watch(ctx, metav1.ListOptions{TimeoutSeconds: &seconds})
		if watchErr == nil {
			watcher.Stop()
		}
		callErr = watchErr
	case "create":
		_, callErr = res.Create(ctx, probeObject(crd, namespace, name), metav1.CreateOptions{})
		created = callErr == nil
	case "update":
		_, callErr = res.Update(ctx, probeObject(crd, namespace, name), metav1.UpdateOptions{}, subs...)
	case "patch":
		_, callErr = res.Patch(ctx, name, types.MergePatchType, []byte(`{}`), metav1.PatchOptions{}, subs...)
	case "delete":
		callErr = res.Delete(ctx, name, metav1.DeleteOptions{}, subs...)
	case "deletecollection":
		callErr = res.DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})
	default:
		return false, false, fmt.Errorf("unsupported request verb %q", verb)
	}
	allowed, err = classify(callErr)
	return allowed, created, err
}

// expected is the catalog's answer, including how the binding is scoped: a role
// bound with a RoleBinding applies only in that namespace and never to
// cluster-scoped kinds.
func (h *harness) expected(role accesscatalog.Role, crd accesscatalog.ShippedCRD, namespace, sub, verb string) bool {
	if !h.catalog.Permits(role.Name, crd.Kind, sub, verb) {
		return false
	}
	if role.Binding == accesscatalog.BindingNamespace {
		return crd.Scope == accesscatalog.ScopeNamespaced && namespace == nsA
	}
	return true
}

func probeName(crd accesscatalog.ShippedCRD, role accesscatalog.Role) string {
	return strings.ToLower(crd.Kind) + "-" + strings.TrimPrefix(role.Name, "kognition-")
}

func namespacesFor(crd accesscatalog.ShippedCRD) []string {
	if crd.Scope == accesscatalog.ScopeNamespaced {
		return []string{nsA, nsB}
	}
	return []string{""}
}

func TestHarnessEnforcesRBAC(t *testing.T) {
	ctx := context.Background()
	agents := accesscatalog.ShippedCRD{Group: accesscatalog.APIGroup, Kind: "Agent", Resource: "agents", Scope: accesscatalog.ScopeNamespaced}

	if _, err := h.dyn.Resource(gvr(agents)).Namespace(nsA).List(ctx, metav1.ListOptions{}); err != nil {
		t.Fatalf("the cluster administrator must be able to list agents: %v", err)
	}
	unbound := h.users[unboundUser]
	for _, verb := range []string{"get", "list", "create", "update", "patch", "delete", "deletecollection", "watch"} {
		allowed, _, err := attempt(ctx, unbound, agents, nsA, "probe", "", verb)
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		if allowed {
			t.Errorf("an unbound principal must not %s agents; RBAC is not being enforced", verb)
		}
	}
}

func TestEveryRoleMatchesTheCatalogAgainstARealAPIServer(t *testing.T) {
	if len(h.catalog.Roles) == 0 || len(h.crds) == 0 {
		t.Fatal("nothing to verify; the matrix would pass vacuously")
	}
	type probe struct {
		crd       accesscatalog.ShippedCRD
		namespace string
		name      string
	}
	var mu sync.Mutex
	var deniedCreates []probe
	checked := 0

	t.Run("roles", func(t *testing.T) {
		for _, role := range h.catalog.Roles {
			t.Run(role.Name, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				p := h.users[userFor(role.Name)]
				for _, crd := range h.crds {
					name := probeName(crd, role)
					for _, namespace := range namespacesFor(crd) {
						for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"} {
							want := h.expected(role, crd, namespace, "", verb)
							got, created, err := attempt(ctx, p, crd, namespace, name, "", verb)
							if err != nil {
								t.Fatalf("%s %s/%s in %q: %v", verb, crd.Group, crd.Resource, namespace, err)
							}
							if got != want {
								t.Errorf("%s may %s %s in %q = %v, catalog says %v", role.Name, verb, crd.Resource, namespace, got, want)
							}
							mu.Lock()
							checked++
							if verb == "create" && !want {
								deniedCreates = append(deniedCreates, probe{crd, namespace, name})
							}
							mu.Unlock()
							if created && !want {
								t.Errorf("%s created %s/%s although the catalog denies it", role.Name, crd.Resource, name)
							}
						}
						if !crd.HasStatus {
							continue
						}
						for _, verb := range []string{"get", "update", "patch"} {
							want := h.expected(role, crd, namespace, "status", verb)
							got, _, err := attempt(ctx, p, crd, namespace, name, "status", verb)
							if err != nil {
								t.Fatalf("%s %s/status in %q: %v", verb, crd.Resource, namespace, err)
							}
							if got != want {
								t.Errorf("%s may %s %s/status in %q = %v, catalog says %v", role.Name, verb, crd.Resource, namespace, got, want)
							}
							mu.Lock()
							checked++
							mu.Unlock()
						}
					}
				}
			})
		}
	})

	t.Logf("verified %d real requests", checked)
	for _, p := range deniedCreates {
		_, err := resourceClient(h.dyn, p.crd, p.namespace).Get(context.Background(), p.name, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			t.Errorf("a denied create left %s/%s in %q behind (err=%v)", p.crd.Resource, p.name, p.namespace, err)
		}
	}
}

var customVerbs = []string{"use", "invoke", "approve", "reject", "reconcile", "bind", "escalate", "impersonate"}

func TestCustomVerbsAndSubresourcesAgainstTheRealAuthorizer(t *testing.T) {
	ctx := context.Background()
	for _, role := range h.catalog.Roles {
		t.Run(role.Name, func(t *testing.T) {
			t.Parallel()
			user := userFor(role.Name)
			for _, crd := range h.crds {
				namespace := ""
				if crd.Scope == accesscatalog.ScopeNamespaced {
					namespace = nsA
				}
				for _, sub := range []string{"", "status", "decision", "reconcile"} {
					for _, verb := range customVerbs {
						want := h.expected(role, crd, namespace, sub, verb)
						got, err := h.review(ctx, user, authorizationv1.ResourceAttributes{
							Namespace: namespace, Group: crd.Group, Resource: crd.Resource, Subresource: sub, Verb: verb, Name: "x",
						})
						if err != nil {
							t.Fatal(err)
						}
						if got != want {
							t.Errorf("%s may %s %s/%s = %v, catalog says %v", role.Name, verb, crd.Resource, sub, got, want)
						}
					}
				}
			}
		})
	}
}

func TestNoRoleReachesSecretsPodsOrClusterAdministration(t *testing.T) {
	ctx := context.Background()
	for _, role := range h.catalog.Roles {
		t.Run(role.Name, func(t *testing.T) {
			p := h.users[userFor(role.Name)]
			for _, namespace := range []string{nsA, nsB} {
				if _, err := p.kube.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{}); !apierrors.IsForbidden(err) {
					t.Errorf("%s must not list Secrets in %s (err=%v)", role.Name, namespace, err)
				}
				if _, err := p.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{}); !apierrors.IsForbidden(err) {
					t.Errorf("%s must not list Pods in %s (err=%v)", role.Name, namespace, err)
				}
			}
			if _, err := p.kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); !apierrors.IsForbidden(err) {
				t.Errorf("%s must not list namespaces (err=%v)", role.Name, err)
			}
			if _, err := p.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); !apierrors.IsForbidden(err) {
				t.Errorf("%s must not list nodes (err=%v)", role.Name, err)
			}
			for _, attrs := range []authorizationv1.ResourceAttributes{
				{Namespace: nsA, Resource: "pods", Subresource: "portforward", Verb: "create"},
				{Namespace: nsA, Resource: "pods", Subresource: "exec", Verb: "create"},
				{Namespace: nsA, Resource: "serviceaccounts", Subresource: "token", Verb: "create"},
				{Namespace: nsA, Resource: "secrets", Verb: "get"},
				{Group: rbacv1.GroupName, Resource: "clusterrolebindings", Verb: "create"},
				{Group: rbacv1.GroupName, Resource: "clusterroles", Verb: "create"},
			} {
				allowed, err := h.review(ctx, userFor(role.Name), attrs)
				if err != nil {
					t.Fatal(err)
				}
				if allowed {
					t.Errorf("%s must not %s %s/%s", role.Name, attrs.Verb, attrs.Resource, attrs.Subresource)
				}
			}
		})
	}
}

func TestAccessAdminDelegationIsBoundedByTheAPIServer(t *testing.T) {
	ctx := context.Background()
	admin := h.users[userFor("kognition-access-admin")]
	bind := func(namespace, subject string, ref rbacv1.RoleRef) error {
		_, err := admin.kube.RbacV1().RoleBindings(namespace).Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "delegated-"},
			RoleRef:    ref,
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: subject}},
		}, metav1.CreateOptions{})
		return err
	}
	clusterRole := func(name string) rbacv1.RoleRef {
		return rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}
	}

	for _, role := range h.catalog.Roles {
		t.Run("bind "+role.Name, func(t *testing.T) {
			err := bind(nsA, "delegate-"+role.Name, clusterRole(role.Name))
			if role.Binding == accesscatalog.BindingNamespace {
				if err != nil {
					t.Errorf("access-admin must be able to bind the namespace role %s: %v", role.Name, err)
				}
				return
			}
			if !apierrors.IsForbidden(err) {
				t.Errorf("access-admin must not bind %s: it is a cluster-bound role, and a RoleBinding would still grant its namespaced rules (err=%v)", role.Name, err)
			}
		})
	}

	// Escalation prevention needs a role with real rules. cluster-admin is static.
	// The aggregated built-ins (admin, edit, view) are empty in envtest because no
	// controller-manager populates them, so binding them proves nothing here; the
	// kind tier (TC-0407) verifies them on a real cluster.
	if _, err := h.kube.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "test-privileged"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list"}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, privileged := range []string{"cluster-admin", "test-privileged"} {
		t.Run("cannot bind "+privileged, func(t *testing.T) {
			if err := bind(nsA, "delegate-"+privileged, clusterRole(privileged)); !apierrors.IsForbidden(err) {
				t.Errorf("access-admin must not bind %s (err=%v)", privileged, err)
			}
		})
	}

	// Native RBAC also lets a principal bind a role whose permissions it already
	// holds, without the bind verb. resourceNames therefore denies additional
	// permissions, not every non-Kognition role.
	if _, err := h.kube.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "test-subset"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{rbacv1.GroupName}, Resources: []string{"rolebindings"}, Verbs: []string{"get", "list"}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Run("can bind a role it already holds the permissions of", func(t *testing.T) {
		if err := bind(nsA, "delegate-subset", clusterRole("test-subset")); err != nil {
			t.Errorf("native RBAC permits binding a subset role without bind (err=%v)", err)
		}
	})

	t.Run("only in its own namespace", func(t *testing.T) {
		if err := bind(nsB, "delegate-elsewhere", clusterRole("kognition-viewer")); !apierrors.IsForbidden(err) {
			t.Errorf("access-admin bound in team-b although it was granted only in team-a (err=%v)", err)
		}
	})
	t.Run("cannot create cluster-wide bindings", func(t *testing.T) {
		_, err := admin.kube.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "delegated-"},
			RoleRef:    clusterRole("kognition-viewer"),
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: "x"}},
		}, metav1.CreateOptions{})
		if !apierrors.IsForbidden(err) {
			t.Errorf("access-admin must not create ClusterRoleBindings (err=%v)", err)
		}
	})
	t.Run("cannot edit roles", func(t *testing.T) {
		_, err := admin.kube.RbacV1().Roles(nsA).Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "forged-"},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}}},
		}, metav1.CreateOptions{})
		if !apierrors.IsForbidden(err) {
			t.Errorf("access-admin must not create Roles (err=%v)", err)
		}
	})
	t.Run("builder and consumer cannot delegate", func(t *testing.T) {
		for _, name := range []string{"kognition-builder", "kognition-consumer", "kognition-grant-admin", "kognition-platform-admin"} {
			p := h.users[userFor(name)]
			_, err := p.kube.RbacV1().RoleBindings(nsA).Create(ctx, &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "self-"},
				RoleRef:    clusterRole("kognition-viewer"),
				Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: "x"}},
			}, metav1.CreateOptions{})
			if !apierrors.IsForbidden(err) {
				t.Errorf("%s must not create RoleBindings (err=%v)", name, err)
			}
		}
	})
}

func TestResourceNamesScopeACustomActionToOneObject(t *testing.T) {
	ctx := context.Background()
	const user = "user-scoped-use"
	if err := h.addUser(user); err != nil {
		t.Fatal(err)
	}
	role, err := h.kube.RbacV1().Roles(nsA).Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "use-one-workflow"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{accesscatalog.APIGroup}, Resources: []string{"workflows"},
			Verbs: []string{"use"}, ResourceNames: []string{"approved-workflow"},
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.kube.RbacV1().RoleBindings(nsA).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "use-one-workflow"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: user}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name, namespace, workflow string
		want                      bool
	}{
		{"the named workflow", nsA, "approved-workflow", true},
		{"another workflow", nsA, "other-workflow", false},
		{"the named workflow in another namespace", nsB, "approved-workflow", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			deadline := time.Now().Add(20 * time.Second)
			for {
				got, err := h.review(ctx, user, authorizationv1.ResourceAttributes{
					Namespace: scenario.namespace, Group: accesscatalog.APIGroup, Resource: "workflows", Verb: "use", Name: scenario.workflow,
				})
				if err != nil {
					t.Fatal(err)
				}
				if got == scenario.want {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("use workflows/%s in %s = %v, want %v", scenario.workflow, scenario.namespace, got, scenario.want)
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
	}
}

func TestApproveAndRejectAreIndependentlyGrantable(t *testing.T) {
	ctx := context.Background()
	for _, grant := range []struct {
		user   string
		verb   string
		denied string
	}{
		{"user-approve-only", "approve", "reject"},
		{"user-reject-only", "reject", "approve"},
	} {
		t.Run(grant.user, func(t *testing.T) {
			if err := h.addUser(grant.user); err != nil {
				t.Fatal(err)
			}
			role, err := h.kube.RbacV1().Roles(nsA).Create(ctx, &rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: grant.user},
				Rules: []rbacv1.PolicyRule{{
					APIGroups: []string{accesscatalog.APIGroup}, Resources: []string{"runtimeapprovals/decision"}, Verbs: []string{grant.verb},
				}},
			}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.kube.RbacV1().RoleBindings(nsA).Create(ctx, &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: grant.user},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
				Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: grant.user}},
			}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			attrs := func(verb string) authorizationv1.ResourceAttributes {
				return authorizationv1.ResourceAttributes{Namespace: nsA, Group: accesscatalog.APIGroup, Resource: "runtimeapprovals", Subresource: "decision", Verb: verb, Name: "approval-1"}
			}
			deadline := time.Now().Add(20 * time.Second)
			for {
				allowed, err := h.review(ctx, grant.user, attrs(grant.verb))
				if err != nil {
					t.Fatal(err)
				}
				if allowed {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s was never allowed to %s", grant.user, grant.verb)
				}
				time.Sleep(100 * time.Millisecond)
			}
			if denied, err := h.review(ctx, grant.user, attrs(grant.denied)); err != nil || denied {
				t.Errorf("%s must not %s (allowed=%v err=%v)", grant.user, grant.denied, denied, err)
			}
		})
	}
}

func TestRevokingABindingTakesEffectOnTheNextRequests(t *testing.T) {
	ctx := context.Background()
	const user = "user-revocable"
	if err := h.addUser(user); err != nil {
		t.Fatal(err)
	}
	agents := accesscatalog.ShippedCRD{Group: accesscatalog.APIGroup, Kind: "Agent", Resource: "agents", Scope: accesscatalog.ScopeNamespaced}
	binding, err := h.kube.RbacV1().RoleBindings(nsA).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "revocable"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "kognition-viewer"},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: user}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor := func(wantAllowed bool, limit time.Duration) time.Duration {
		start := time.Now()
		for {
			allowed, _, err := attempt(ctx, h.users[user], agents, nsA, "x", "", "list")
			if err != nil {
				t.Fatal(err)
			}
			if allowed == wantAllowed {
				return time.Since(start)
			}
			if time.Since(start) > limit {
				t.Fatalf("list agents allowed=%v, want %v after %s", allowed, wantAllowed, limit)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitFor(true, 20*time.Second)
	if err := h.kube.RbacV1().RoleBindings(nsA).Delete(ctx, binding.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	elapsed := waitFor(false, 10*time.Second)
	t.Logf("revocation was observed after %s", elapsed)
}

func TestEveryCatalogRoleIsInstalledAndLabelled(t *testing.T) {
	list, err := h.kube.RbacV1().ClusterRoles().List(context.Background(), metav1.ListOptions{LabelSelector: accesscatalog.RoleLabel})
	if err != nil {
		t.Fatal(err)
	}
	var installed []string
	for _, role := range list.Items {
		installed = append(installed, role.Name)
	}
	want := sortedRoleNames(h.catalog.Roles)
	if got := strings.Join(sortedStrings(installed), ","); got != strings.Join(want, ",") {
		t.Fatalf("installed human roles %s, want %s", got, strings.Join(want, ","))
	}
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
