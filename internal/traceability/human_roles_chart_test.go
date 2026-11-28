// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/kognition-ai/kognition/internal/accesscatalog"
)

const humanRolesTemplate = "../../deploy/helm/kognition/templates/human-roles.yaml"

type renderedHumanRoleChart struct {
	roles    map[string]rbacv1.ClusterRole
	bindings []rbacv1.RoleRef
}

func runHelmTemplate(test *testing.T, extra ...string) ([]byte, error) {
	test.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		test.Fatal("helm is required for human role chart tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	args := append([]string{"template", "human-roles-test", "../../deploy/helm/kognition", "--namespace", "control", "--kube-version", "1.30.3", "--set", "networkPolicy.kubeletProbeIngressCIDRs[0]=172.16.0.0/12"}, extra...)
	return exec.CommandContext(ctx, "helm", args...).CombinedOutput()
}

func renderHumanRoleChart(test *testing.T, extra ...string) renderedHumanRoleChart {
	test.Helper()
	output, err := runHelmTemplate(test, extra...)
	if err != nil {
		test.Fatalf("render chart: %v\n%s", err, output)
	}
	chart := renderedHumanRoleChart{roles: map[string]rbacv1.ClusterRole{}}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	for {
		var document runtime.RawExtension
		if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			test.Fatal(err)
		}
		if len(document.Raw) == 0 {
			continue
		}
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(document.Raw, &meta); err != nil {
			test.Fatal(err)
		}
		switch meta.Kind {
		case "ClusterRole":
			var role rbacv1.ClusterRole
			if err := json.Unmarshal(document.Raw, &role); err != nil {
				test.Fatal(err)
			}
			if _, human := role.Labels[accesscatalog.RoleLabel]; human {
				chart.roles[role.Name] = role
			}
		case "RoleBinding":
			var binding rbacv1.RoleBinding
			if err := json.Unmarshal(document.Raw, &binding); err != nil {
				test.Fatal(err)
			}
			chart.bindings = append(chart.bindings, binding.RoleRef)
		case "ClusterRoleBinding":
			var binding rbacv1.ClusterRoleBinding
			if err := json.Unmarshal(document.Raw, &binding); err != nil {
				test.Fatal(err)
			}
			chart.bindings = append(chart.bindings, binding.RoleRef)
		}
	}
	return chart
}

func catalogRoleNames() []string {
	var names []string
	for _, role := range accesscatalog.Default().Roles {
		names = append(names, role.Name)
	}
	sort.Strings(names)
	return names
}

func sortedKeys(roles map[string]rbacv1.ClusterRole) []string {
	names := make([]string, 0, len(roles))
	for name := range roles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestHumanRolesRenderOnlyInKubernetesModeWhenEnabled(test *testing.T) {
	want := catalogRoleNames()
	for _, scenario := range []struct {
		name      string
		arguments []string
		rendered  bool
	}{
		{name: "kubernetes default", arguments: []string{"--set", "mode=kubernetes"}, rendered: true},
		{name: "kubernetes disabled", arguments: []string{"--set", "mode=kubernetes", "--set", "rbac.humanRoles.enabled=false"}},
		{name: "standalone default"},
		{name: "standalone explicitly enabled", arguments: []string{"--set", "rbac.humanRoles.enabled=true"}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			got := sortedKeys(renderHumanRoleChart(test, scenario.arguments...).roles)
			if !scenario.rendered {
				if len(got) != 0 {
					test.Fatalf("expected no human roles, rendered %v", got)
				}
				return
			}
			if !reflect.DeepEqual(got, want) {
				test.Fatalf("rendered roles %v, want the catalog roles %v", got, want)
			}
		})
	}
}

func TestHumanRolesAreInert(test *testing.T) {
	names := map[string]bool{}
	for _, name := range catalogRoleNames() {
		names[name] = true
	}
	for _, mode := range []string{"kubernetes", "standalone"} {
		test.Run(mode, func(test *testing.T) {
			chart := renderHumanRoleChart(test, "--set", "mode="+mode)
			if mode == "kubernetes" && len(chart.roles) == 0 {
				test.Fatal("no human roles were rendered; the inertness checks would pass vacuously")
			}
			for _, ref := range chart.bindings {
				if names[ref.Name] {
					test.Errorf("the chart must not bind human role %s by default", ref.Name)
				}
			}
			for name, role := range chart.roles {
				if role.AggregationRule != nil {
					test.Errorf("%s must not define an aggregation rule", name)
				}
				for key := range role.Labels {
					if strings.HasPrefix(key, "rbac.authorization.k8s.io/aggregate-to-") {
						test.Errorf("%s carries aggregation label %s by default", name, key)
					}
				}
				if role.Labels["app.kubernetes.io/part-of"] != "kognition" {
					test.Errorf("%s is missing the chart part-of label: %v", name, role.Labels)
				}
				if want := strings.TrimPrefix(name, "kognition-"); role.Labels[accesscatalog.RoleLabel] != want {
					test.Errorf("%s role label = %q, want %q", name, role.Labels[accesscatalog.RoleLabel], want)
				}
			}
		})
	}
}

func TestRenderedHumanRolesMatchTheCatalog(test *testing.T) {
	catalog := accesscatalog.Default()
	chart := renderHumanRoleChart(test, "--set", "mode=kubernetes")
	if len(chart.roles) == 0 {
		test.Fatal("no human roles were rendered")
	}
	for _, role := range catalog.Roles {
		want, err := catalog.PolicyRules(role.Name)
		if err != nil {
			test.Fatal(err)
		}
		got, ok := chart.roles[role.Name]
		if !ok {
			test.Errorf("role %s was not rendered", role.Name)
			continue
		}
		if !reflect.DeepEqual(got.Rules, want) {
			test.Errorf("%s rendered rules differ from the catalog:\n got %+v\nwant %+v", role.Name, got.Rules, want)
		}
	}
}

func TestRenderedHumanRolesReferenceOnlyInstalledKinds(test *testing.T) {
	crds, err := accesscatalog.LoadShippedCRDs("../../deploy/helm/kognition/crds")
	if err != nil {
		test.Fatal(err)
	}
	installed := map[string]bool{}
	for _, crd := range crds {
		installed[crd.Resource] = true
	}
	chart := renderHumanRoleChart(test, "--set", "mode=kubernetes")
	if len(chart.roles) == 0 {
		test.Fatal("no human roles were rendered")
	}
	for name, role := range chart.roles {
		for _, rule := range role.Rules {
			for _, group := range rule.APIGroups {
				switch group {
				case accesscatalog.APIGroup:
					for _, resource := range rule.Resources {
						base, _, _ := strings.Cut(resource, "/")
						if !installed[base] {
							test.Errorf("%s names %s, which is not an installed CRD", name, resource)
						}
					}
				case "rbac.authorization.k8s.io":
					if name != "kognition-access-admin" {
						test.Errorf("%s must not reference the RBAC API group", name)
					}
				default:
					test.Errorf("%s references unexpected API group %q", name, group)
				}
			}
		}
	}
}

func TestAccessAdminBindsExactlyTheRenderedHumanRoles(test *testing.T) {
	chart := renderHumanRoleChart(test, "--set", "mode=kubernetes")
	admin, ok := chart.roles["kognition-access-admin"]
	if !ok {
		test.Fatal("kognition-access-admin was not rendered")
	}
	var bound []string
	for _, rule := range admin.Rules {
		for _, verb := range rule.Verbs {
			if verb == "bind" {
				if len(rule.ResourceNames) == 0 {
					test.Fatal("bind must be limited with resourceNames")
				}
				bound = append(bound, rule.ResourceNames...)
			}
		}
	}
	sort.Strings(bound)
	if want := sortedKeys(chart.roles); !reflect.DeepEqual(bound, want) {
		test.Fatalf("access-admin may bind %v, want exactly the rendered human roles %v", bound, want)
	}
}

func TestCommittedHumanRoleTemplateMatchesTheCatalog(test *testing.T) {
	have, err := os.ReadFile(humanRolesTemplate)
	if err != nil {
		test.Fatalf("read the generated template: %v (run `make access-catalog`)", err)
	}
	want, err := accesscatalog.Default().RenderChartTemplate()
	if err != nil {
		test.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(have, []byte("\r\n"), []byte("\n")), want) {
		test.Fatal("deploy/helm/kognition/templates/human-roles.yaml is out of date; run `make access-catalog` and commit the result")
	}
}

func TestHumanRoleValuesAreValidated(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		arguments []string
	}{
		{name: "enabled must be boolean", arguments: []string{"--set", "mode=kubernetes", "--set", "rbac.humanRoles.enabled=maybe"}},
		{name: "unknown humanRoles field", arguments: []string{"--set", "mode=kubernetes", "--set", "rbac.humanRoles.bogus=true"}},
		{name: "unknown rbac field", arguments: []string{"--set", "mode=kubernetes", "--set", "rbac.bogus=true"}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if output, err := runHelmTemplate(test, scenario.arguments...); err == nil {
				test.Fatalf("expected the chart to reject the value, rendered %d bytes", len(output))
			}
		})
	}
}
