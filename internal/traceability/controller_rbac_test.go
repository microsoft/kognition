// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func renderControllerRBAC(test *testing.T, extra ...string) []runtime.RawExtension {
	test.Helper()
	return renderControllerChart(test, append([]string{"--set", "k8sController.activated=true"}, extra...)...)
}

func renderControllerChart(test *testing.T, extra ...string) []runtime.RawExtension {
	test.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		test.Fatal("helm is required for controller RBAC tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := append([]string{"template", "security-test", "../../deploy/helm/kognition", "--namespace", "control", "--kube-version", "1.30.3", "--set", "mode=kubernetes", "--set", "networkPolicy.kubeletProbeIngressCIDRs[0]=172.16.0.0/12"}, extra...)
	output, err := exec.CommandContext(ctx, "helm", args...).CombinedOutput()
	if err != nil {
		test.Fatalf("render controller RBAC: %v: %s", err, output)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	var documents []runtime.RawExtension
	for {
		var document runtime.RawExtension
		if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			test.Fatal(err)
		}
		if len(document.Raw) != 0 {
			documents = append(documents, document)
		}
	}
	return documents
}

func TestControllerRBACNoClusterWorkloadPrivileges(test *testing.T) {
	found := false
	for _, document := range renderControllerRBAC(test) {
		var role rbacv1.ClusterRole
		if err := json.Unmarshal(document.Raw, &role); err != nil {
			test.Fatal(err)
		}
		if role.Kind != "ClusterRole" || role.Name != "security-test-kognition-controller" {
			continue
		}
		found = true
		for _, rule := range role.Rules {
			for _, group := range rule.APIGroups {
				if group == "" || group == "apps" || group == "batch" || group == "networking.k8s.io" || group == "rbac.authorization.k8s.io" {
					test.Errorf("controller has cluster-wide workload or RBAC privileges: %+v", rule)
				}
			}
		}
	}
	if !found {
		test.Fatal("controller ClusterRole missing")
	}
}

func TestControllerDedicatedIdentity(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		arguments []string
		account   string
		created   bool
	}{
		{name: "default", account: "security-test-kognition-controller", created: true},
		{name: "custom", arguments: []string{"--set", "k8sController.serviceAccount.name=controller-only", "--set", "serviceAccount.name=platform-only"}, account: "controller-only", created: true},
		{name: "external", arguments: []string{"--set", "k8sController.serviceAccount.name=external-controller", "--set", "k8sController.serviceAccount.create=false"}, account: "external-controller"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			created, deployed, trusted := false, false, false
			bindings := 0
			for _, document := range renderControllerRBAC(test, scenario.arguments...) {
				var object corev1.ServiceAccount
				if err := json.Unmarshal(document.Raw, &object); err != nil {
					test.Fatal(err)
				}
				switch object.Kind {
				case "ServiceAccount":
					if object.Name == scenario.account {
						created = true
					}
				case "Deployment":
					var deployment appsv1.Deployment
					if err := json.Unmarshal(document.Raw, &deployment); err != nil {
						test.Fatal(err)
					}
					if object.Name != "security-test-kognition-k8s-controller" {
						if deployment.Spec.Template.Spec.ServiceAccountName == scenario.account {
							test.Fatal("non-controller Deployment uses privileged identity")
						}
						continue
					}
					deployed = deployment.Spec.Template.Spec.ServiceAccountName == scenario.account
					for _, container := range deployment.Spec.Template.Spec.Containers {
						for _, variable := range container.Env {
							if variable.Name == "KOGNITION_CONTROLLER_SERVICE_ACCOUNT" {
								trusted = variable.Value == scenario.account
							}
						}
					}
				case "RoleBinding", "ClusterRoleBinding":
					if object.Name != "security-test-kognition-controller" && object.Name != "security-test-kognition-quota-ledger" {
						continue
					}
					var binding rbacv1.RoleBinding
					if err := json.Unmarshal(document.Raw, &binding); err != nil {
						test.Fatal(err)
					}
					bindings++
					if len(binding.Subjects) != 1 || binding.Subjects[0].Name != scenario.account || binding.Subjects[0].Namespace != "control" || binding.Subjects[0].Kind != "ServiceAccount" {
						test.Fatalf("incorrect dedicated identity binding: %+v", binding)
					}
				}
			}
			if created != scenario.created || !deployed || !trusted || bindings != 0 {
				test.Fatalf("identity mismatch: created=%t deployed=%t trusted=%t bindings=%d", created, deployed, trusted, bindings)
			}
		})
	}
	for _, account := range []string{"default", "security-test-kognition"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		command := exec.CommandContext(ctx, "helm", "template", "security-test", "../../deploy/helm/kognition", "--namespace", "control", "--set", "mode=kubernetes", "--set", "networkPolicy.kubeletProbeIngressCIDRs[0]=172.16.0.0/12", "--set", "k8sController.serviceAccount.name="+account)
		output, err := command.CombinedOutput()
		cancel()
		if err == nil || !strings.Contains(string(output), "controller ServiceAccount must be distinct") {
			test.Fatalf("shared identity %s not rejected: %v %s", account, err, output)
		}
	}
}

func TestControllerNamespaceEnrollment(test *testing.T) {
	for _, scenario := range []struct {
		name       string
		arguments  []string
		namespaces []string
	}{
		{name: "default", namespaces: []string{"control"}},
		{name: "explicit", arguments: []string{"--set", "k8sController.managedNamespaces[0]=tenant", "--set", "k8sController.agentNamespace=runtime"}, namespaces: []string{"control", "tenant", "runtime"}},
		{name: "duplicates", arguments: []string{"--set", "k8sController.managedNamespaces[0]=control", "--set", "k8sController.managedNamespaces[1]=runtime", "--set", "k8sController.agentNamespace=runtime"}, namespaces: []string{"control", "runtime"}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			roles := map[string]bool{}
			bindings := map[string]bool{}
			var configured []string
			for _, document := range renderControllerRBAC(test, scenario.arguments...) {
				var role rbacv1.Role
				if err := json.Unmarshal(document.Raw, &role); err != nil {
					test.Fatal(err)
				}
				if role.Name == "security-test-kognition-controller" {
					switch role.Kind {
					case "Role":
						if roles[role.Namespace] {
							test.Fatalf("duplicate Role in %s", role.Namespace)
						}
						roles[role.Namespace] = true
					case "RoleBinding":
						var binding rbacv1.RoleBinding
						if err := json.Unmarshal(document.Raw, &binding); err != nil {
							test.Fatal(err)
						}
						if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != role.Name || len(binding.Subjects) != 1 || binding.Subjects[0].Namespace != "control" || binding.Subjects[0].Kind != "ServiceAccount" || binding.Subjects[0].Name != "security-test-kognition-controller" {
							test.Fatalf("incorrect controller binding: %+v", binding)
						}
						if bindings[role.Namespace] {
							test.Fatalf("duplicate RoleBinding in %s", role.Namespace)
						}
						bindings[role.Namespace] = true
					}
				}
				if role.Kind == "Deployment" && role.Name == "security-test-kognition-k8s-controller" {
					var deployment appsv1.Deployment
					if err := json.Unmarshal(document.Raw, &deployment); err != nil {
						test.Fatal(err)
					}
					for _, container := range deployment.Spec.Template.Spec.Containers {
						for _, variable := range container.Env {
							if variable.Name == "KOGNITION_MANAGED_NAMESPACES" {
								configured = strings.Split(variable.Value, ",")
							}
						}
					}
				}
			}
			expected := map[string]bool{}
			for _, namespace := range scenario.namespaces {
				expected[namespace] = true
			}
			if !reflect.DeepEqual(roles, expected) || len(bindings) != 0 || !reflect.DeepEqual(configured, scenario.namespaces) {
				test.Fatalf("namespace mismatch: roles=%v bindings=%v env=%v want=%v", roles, bindings, configured, scenario.namespaces)
			}
		})
	}
}

func TestControllerNamespaceEnrollmentRejectsInvalidValues(test *testing.T) {
	test.Run("installation placement", func(test *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "helm", "template", "security-test", "../../deploy/helm/kognition", "--namespace", "control", "--set", "mode=kubernetes", "--set", "networkPolicy.kubeletProbeIngressCIDRs[0]=172.16.0.0/12", "--set", "k8sController.agentNamespace=control")
		output, err := command.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "must differ from the installation namespace") {
			test.Fatalf("installation namespace placement not rejected: %v %s", err, output)
		}
	})
	for _, setting := range []string{
		`[""]`,
		`["*"]`,
		`["Uppercase"]`,
		`[123]`,
		`"tenant"`,
		`["` + strings.Repeat("a", 64) + `"]`,
	} {
		test.Run(setting, func(test *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "helm", "template", "security-test", "../../deploy/helm/kognition", "--namespace", "control", "--set", "mode=kubernetes", "--set", "networkPolicy.kubeletProbeIngressCIDRs[0]=172.16.0.0/12", "--values", "-")
			command.Stdin = strings.NewReader("k8sController:\n  managedNamespaces: " + setting + "\n")
			output, err := command.CombinedOutput()
			if err == nil || !(strings.Contains(string(output), "controllerNamespaces") || strings.Contains(string(output), "controller namespaces must be explicit")) {
				test.Fatalf("expected namespace validation failure: err=%v output=%s", err, output)
			}
		})
	}
}

func TestControllerRBACMatchesCustomResourceScopes(test *testing.T) {
	paths, err := filepath.Glob("../../deploy/helm/kognition/crds/*.yaml")
	if err != nil || len(paths) == 0 {
		test.Fatalf("read chart CRDs: %v", err)
	}
	scopes := map[string]apiextensionsv1.ResourceScope{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			test.Fatal(err)
		}
		decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(data)), 4096)
		for {
			var definition apiextensionsv1.CustomResourceDefinition
			if err := decoder.Decode(&definition); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				test.Fatal(err)
			}
			if definition.Spec.Group == "kognition.dev" {
				scopes[definition.Spec.Names.Plural] = definition.Spec.Scope
			}
		}
	}
	checked := map[string]bool{}
	for _, document := range renderControllerRBAC(test) {
		var role rbacv1.ClusterRole
		if err := json.Unmarshal(document.Raw, &role); err != nil {
			test.Fatal(err)
		}
		if role.Name != "security-test-kognition-controller" || (role.Kind != "Role" && role.Kind != "ClusterRole") {
			continue
		}
		for _, rule := range role.Rules {
			for _, group := range rule.APIGroups {
				if group != "kognition.dev" {
					continue
				}
				for _, resource := range rule.Resources {
					name, _, _ := strings.Cut(resource, "/")
					scope, exists := scopes[name]
					if !exists {
						test.Errorf("RBAC references unknown core resource %s", resource)
						continue
					}
					checked[name] = true
					if (scope == apiextensionsv1.ClusterScoped) != (role.Kind == "ClusterRole") {
						test.Errorf("%s is %s but permissions appear in %s", resource, scope, role.Kind)
					}
				}
			}
		}
	}
	if len(checked) == 0 {
		test.Fatal("no core custom-resource permissions checked")
	}
}
