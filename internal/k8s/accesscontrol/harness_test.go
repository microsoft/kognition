// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

// Package accesscontrol proves the shipped human roles against a real
// kube-apiserver with the RBAC authorizer enabled.
//
// The suite renders the chart with helm, installs the CRDs and the rendered
// human ClusterRoles, binds one authenticated principal to each role, and checks
// the outcome of real requests rather than only access reviews. A request is
// authorized when the API server answers anything other than Forbidden: an
// allowed request for an object that does not exist fails with NotFound, and an
// allowed create of an empty object fails validation with Invalid.
//
// To run:
//
//	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
//	$env:KUBEBUILDER_ASSETS = (setup-envtest use 1.30.x -p path)
//	go test -tags integration ./internal/k8s/accesscontrol
package accesscontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/kognition-ai/kognition/internal/accesscatalog"
)

const (
	nsA = "team-a"
	nsB = "team-b"

	chartPath = "../../../deploy/helm/kognition"
	crdPath   = "../../../deploy/helm/kognition/crds"

	unboundUser = "user-unbound"
)

type principal struct {
	name string
	kube kubernetes.Interface
	dyn  dynamic.Interface
}

type harness struct {
	env     *envtest.Environment
	admin   *rest.Config
	kube    kubernetes.Interface
	dyn     dynamic.Interface
	catalog accesscatalog.Catalog
	crds    []accesscatalog.ShippedCRD
	users   map[string]*principal
}

var h *harness

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set; skipping the access-control envtest suite")
		fmt.Fprintln(os.Stderr, "Hint: $env:KUBEBUILDER_ASSETS = (setup-envtest use 1.30.x -p path)")
		os.Exit(0)
	}
	code, err := run(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "access-control envtest setup failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	roles, err := renderHumanRoles()
	if err != nil {
		return 0, err
	}
	crds, err := accesscatalog.LoadShippedCRDs(crdPath)
	if err != nil {
		return 0, err
	}

	env := &envtest.Environment{CRDDirectoryPaths: []string{crdPath}, ErrorIfCRDPathMissing: true}
	// Set the authorizer explicitly so the suite never depends on a default.
	env.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	cfg, err := env.Start()
	if err != nil {
		return 0, fmt.Errorf("start envtest: %w", err)
	}
	defer func() { _ = env.Stop() }()

	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return 0, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return 0, err
	}
	h = &harness{env: env, admin: cfg, kube: kube, dyn: dyn, catalog: accesscatalog.Default(), crds: crds, users: map[string]*principal{}}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := h.bootstrap(ctx, roles); err != nil {
		return 0, err
	}
	return m.Run(), nil
}

// renderHumanRoles renders the chart exactly as an operator would and returns
// the human ClusterRoles, so the shipped artifact is what gets tested.
func renderHumanRoles() ([]rbacv1.ClusterRole, error) {
	if _, err := exec.LookPath("helm"); err != nil {
		return nil, errors.New("helm is required to render the shipped human roles")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "helm", "template", "access-test", chartPath, "--namespace", "control",
		"--kube-version", "1.30.3", "--set", "mode=kubernetes", "--set", "networkPolicy.kubeletProbeIngressCIDRs[0]=172.16.0.0/12").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("render chart: %w: %s", err, output)
	}
	var roles []rbacv1.ClusterRole
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	for {
		var document runtime.RawExtension
		if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		if len(document.Raw) == 0 {
			continue
		}
		var role rbacv1.ClusterRole
		if err := json.Unmarshal(document.Raw, &role); err != nil || role.Kind != "ClusterRole" {
			continue
		}
		if _, human := role.Labels[accesscatalog.RoleLabel]; human {
			roles = append(roles, role)
		}
	}
	if len(roles) == 0 {
		return nil, errors.New("the chart rendered no human roles")
	}
	return roles, nil
}

func (h *harness) bootstrap(ctx context.Context, roles []rbacv1.ClusterRole) error {
	for _, namespace := range []string{nsA, nsB} {
		if _, err := h.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create namespace %s: %w", namespace, err)
		}
	}
	for i := range roles {
		role := roles[i]
		role.ResourceVersion = ""
		if _, err := h.kube.RbacV1().ClusterRoles().Create(ctx, &role, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("install %s: %w", role.Name, err)
		}
	}
	for _, crd := range h.crds {
		if _, err := h.dyn.Resource(gvr(crd)).List(ctx, metav1.ListOptions{}); err != nil {
			return fmt.Errorf("the API server does not serve %s: %w", crd.Resource, err)
		}
	}

	for _, role := range h.catalog.Roles {
		user := userFor(role.Name)
		if err := h.addUser(user); err != nil {
			return err
		}
		subject := rbacv1.Subject{APIGroup: rbacv1.GroupName, Kind: "User", Name: user}
		ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}
		var err error
		if role.Binding == accesscatalog.BindingCluster {
			_, err = h.kube.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "bind-" + role.Name}, RoleRef: ref, Subjects: []rbacv1.Subject{subject}}, metav1.CreateOptions{})
		} else {
			_, err = h.kube.RbacV1().RoleBindings(nsA).Create(ctx, &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "bind-" + role.Name}, RoleRef: ref, Subjects: []rbacv1.Subject{subject}}, metav1.CreateOptions{})
		}
		if err != nil {
			return fmt.Errorf("bind %s: %w", role.Name, err)
		}
	}
	if err := h.addUser(unboundUser); err != nil {
		return err
	}
	return h.waitForBindings(ctx)
}

func userFor(role string) string { return "user-" + strings.TrimPrefix(role, "kognition-") }

func (h *harness) addUser(name string) error {
	user, err := h.env.AddUser(envtest.User{Name: name}, h.admin)
	if err != nil {
		return fmt.Errorf("create principal %s: %w", name, err)
	}
	cfg := rest.CopyConfig(user.Config())
	cfg.QPS, cfg.Burst = 2000, 4000
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	h.users[name] = &principal{name: name, kube: kube, dyn: dyn}
	return nil
}

// waitForBindings blocks until the authorizer has observed every binding, so a
// test never races the RBAC informers.
func (h *harness) waitForBindings(ctx context.Context) error {
	for _, role := range h.catalog.Roles {
		attrs, ok := h.probeAttributes(role)
		if !ok {
			continue
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			allowed, err := h.review(ctx, userFor(role.Name), attrs)
			if err != nil {
				return err
			}
			if allowed {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the authorizer never allowed %s to %s %s", role.Name, attrs.Verb, attrs.Resource)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

// probeAttributes finds one action the role is documented to allow.
func (h *harness) probeAttributes(role accesscatalog.Role) (authorizationv1.ResourceAttributes, bool) {
	if role.Name == "kognition-access-admin" {
		return authorizationv1.ResourceAttributes{Namespace: nsA, Group: rbacv1.GroupName, Resource: "rolebindings", Verb: "create"}, true
	}
	for _, kind := range h.catalog.Kinds {
		for _, sub := range []string{"", "status", "decision", "reconcile"} {
			for _, verb := range []string{"get", "create", "use", "invoke", "approve", "reconcile", "update"} {
				if h.catalog.Permits(role.Name, kind.Name, sub, verb) {
					namespace := ""
					if role.Binding == accesscatalog.BindingNamespace && kind.Scope == accesscatalog.ScopeNamespaced {
						namespace = nsA
					}
					return authorizationv1.ResourceAttributes{Namespace: namespace, Group: accesscatalog.APIGroup, Resource: kind.Resource, Subresource: sub, Verb: verb}, true
				}
			}
		}
	}
	return authorizationv1.ResourceAttributes{}, false
}

// review asks the real authorizer whether user may perform the action. It is
// created as the cluster administrator, so it reports the decision for user.
func (h *harness) review(ctx context.Context, user string, attrs authorizationv1.ResourceAttributes, groups ...string) (bool, error) {
	result, err := h.kube.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{User: user, Groups: groups, ResourceAttributes: &attrs},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("subject access review: %w", err)
	}
	if result.Status.EvaluationError != "" {
		return false, fmt.Errorf("authorizer evaluation error: %s", result.Status.EvaluationError)
	}
	return result.Status.Allowed && !result.Status.Denied, nil
}

func sortedRoleNames(roles []accesscatalog.Role) []string {
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		names = append(names, role.Name)
	}
	sort.Strings(names)
	return names
}
