// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

func TestAgentCommunicationGatewayChartPreservesImageSelection(test *testing.T) {
	digest := "sha256:6ef4229c952281c96a824e7a70af39a13646872b4ec2e3a09ba76fca9896111e"
	for _, scenario := range []struct {
		name, digest, expected string
	}{
		{name: "digest-overrides-tag", digest: digest, expected: "fixture/gateway@" + digest},
		{name: "tag-without-digest", expected: "fixture/gateway:legacy"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			for _, document := range renderControllerChart(test,
				"--set", "agentCommunicationGateway.enabled=true",
				"--set-string", "agentCommunicationGateway.image.repository=fixture/gateway",
				"--set-string", "agentCommunicationGateway.image.tag=legacy",
				"--set-string", "agentCommunicationGateway.image.digest="+scenario.digest,
			) {
				var deployment appsv1.Deployment
				if err := json.Unmarshal(document.Raw, &deployment); err != nil {
					test.Fatal(err)
				}
				if deployment.Kind != "Deployment" || deployment.Name != "security-test-kognition-agent-communication-gateway" {
					continue
				}
				if len(deployment.Spec.Template.Spec.Containers) != 1 || deployment.Spec.Template.Spec.Containers[0].Image != scenario.expected {
					test.Fatalf("gateway image selection: %+v, want %s", deployment.Spec.Template.Spec.Containers, scenario.expected)
				}
				return
			}
			test.Fatal("Agent Communication Gateway Deployment missing")
		})
	}
}

func TestGatewayCoordinationLeaseAdmission(test *testing.T) {
	var policy admissionv1.ValidatingAdmissionPolicy
	for _, document := range renderControllerChart(test, "--set", "toolGateway.enabled=true", "--set", "agentCommunicationGateway.enabled=true", "--set", "k8sController.managedNamespaces[0]=tenant") {
		var candidate admissionv1.ValidatingAdmissionPolicy
		if err := json.Unmarshal(document.Raw, &candidate); err != nil {
			test.Fatal(err)
		}
		if candidate.Kind == "ValidatingAdmissionPolicy" && candidate.Name == "security-test-kognition-control-gateway-leases" {
			policy = candidate
		}
	}
	if policy.Name == "" {
		test.Fatal("gateway Lease writes have no independent admission boundary")
	}
	environment, err := cel.NewEnv(cel.Variable("request", cel.DynType), cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType))
	if err != nil {
		test.Fatal(err)
	}
	evaluate := func(expression string, activation map[string]any) bool {
		test.Helper()
		ast, issues := environment.Compile(expression)
		if issues.Err() != nil {
			test.Fatal(issues.Err())
		}
		program, err := environment.Program(ast)
		if err != nil {
			test.Fatal(err)
		}
		result, _, err := program.Eval(activation)
		if err != nil {
			test.Fatal(err)
		}
		return result == types.True
	}
	for _, component := range []string{"tool-gateway", "memory-policy-gateway", "agent-communication-gateway"} {
		for _, scenario := range []struct {
			name, namespace, lease, operation string
			oldUnowned, helmOwned             bool
			allow                             bool
		}{
			{name: "workspace-record", namespace: "tenant", lease: "kognition-runtime-approval-records", operation: "CREATE", allow: true},
			{name: "binding-control", namespace: "control", lease: "kognition-runtime-approval-binding-keys", operation: "CREATE", allow: true},
			{name: "rate-control", namespace: "control", lease: "kognition-tool-grant-rate-test", operation: "UPDATE", allow: true},
			{name: "usage-control", namespace: "control", lease: "kognition-usage-lock-test", operation: "UPDATE", allow: true},
			{name: "foreign-namespace", namespace: "foreign", lease: "kognition-runtime-approval-records", operation: "CREATE"},
			{name: "binding-tenant", namespace: "tenant", lease: "kognition-runtime-approval-binding-keys", operation: "CREATE"},
			{name: "arbitrary", namespace: "tenant", lease: "foreign-leader", operation: "UPDATE"},
			{name: "takeover", namespace: "tenant", lease: "kognition-runtime-approval-records", operation: "UPDATE", oldUnowned: true},
			{name: "helm-owned", namespace: "control", lease: "kognition-runtime-approval-binding-keys", operation: "UPDATE", helmOwned: true},
			{name: "delete", namespace: "tenant", lease: "kognition-runtime-approval-records", operation: "DELETE"},
		} {
			test.Run(component+"/"+scenario.name, func(test *testing.T) {
				metadata := map[string]any{"namespace": scenario.namespace, "name": scenario.lease, "labels": map[string]any{"app.kubernetes.io/managed-by": "kognition"}}
				if scenario.helmOwned {
					metadata["annotations"] = map[string]any{"meta.helm.sh/release-name": "other"}
				}
				object := map[string]any{"metadata": metadata}
				var oldObject any
				if scenario.operation != "CREATE" {
					oldObject = object
				}
				if scenario.oldUnowned {
					oldObject = map[string]any{"metadata": map[string]any{"namespace": scenario.namespace, "name": scenario.lease}}
				}
				activation := map[string]any{"request": map[string]any{"namespace": scenario.namespace, "operation": scenario.operation, "userInfo": map[string]any{"username": "system:serviceaccount:control:security-test-kognition-" + component}}, "object": object, "oldObject": oldObject}
				for _, condition := range policy.Spec.MatchConditions {
					if !evaluate(condition.Expression, activation) {
						test.Fatal("gateway escaped policy matching")
					}
				}
				accepted := true
				for _, validation := range policy.Spec.Validations {
					accepted = accepted && evaluate(validation.Expression, activation)
				}
				if accepted != scenario.allow {
					test.Fatalf("gateway Lease acceptance=%t want %t", accepted, scenario.allow)
				}
			})
		}
	}
}

func TestAgentCommunicationGatewayCoordinationIsNamespaceScoped(test *testing.T) {
	name := "security-test-kognition-agent-communication-gateway"
	foundCluster, foundRole, foundBinding := false, false, false
	foundWorkspaceLease := false
	for _, document := range renderControllerChart(test, "--set", "agentCommunicationGateway.enabled=true") {
		var role rbacv1.Role
		if err := json.Unmarshal(document.Raw, &role); err != nil {
			test.Fatal(err)
		}
		if role.Kind == "ClusterRole" && role.Name == name {
			foundCluster = true
			for _, rule := range role.Rules {
				foundWorkspaceLease = foundWorkspaceLease || reflect.DeepEqual(rule, rbacv1.PolicyRule{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{"kognition-runtime-approval-records"}, Verbs: []string{"get", "update"}})
				if reflect.DeepEqual(rule.APIGroups, []string{"coordination.k8s.io"}) && len(rule.ResourceNames) == 0 && !reflect.DeepEqual(rule.Verbs, []string{"create"}) {
					test.Error("unbounded cluster Lease read/update authority retained")
				}
				for _, group := range rule.APIGroups {
					if group == "" {
						test.Errorf("gateway core-resource coordination must not be cluster-wide: %+v", rule)
					}
				}
			}
		}
		if role.Kind == "Role" && role.Name == name+"-runtime-identity" {
			foundRole = true
			if role.Namespace != "control" {
				test.Errorf("coordination Role namespace = %q, want control", role.Namespace)
			}
			for _, expected := range []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "create", "update"}},
				{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "create", "update"}},
			} {
				found := false
				for _, rule := range role.Rules {
					found = found || reflect.DeepEqual(rule, expected)
				}
				if !found {
					test.Errorf("missing namespace-scoped coordination rule: %+v", expected)
				}
			}
		}
		if role.Kind == "RoleBinding" && role.Name == name+"-runtime-identity" {
			foundBinding = true
			var binding rbacv1.RoleBinding
			if err := json.Unmarshal(document.Raw, &binding); err != nil {
				test.Fatal(err)
			}
			if binding.Namespace != "control" || binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != role.Name || len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" || binding.Subjects[0].Name != name || binding.Subjects[0].Namespace != "control" {
				test.Errorf("invalid namespace-scoped coordination binding: %+v", binding)
			}
		}
	}
	if !foundCluster || !foundRole || !foundBinding {
		test.Fatalf("missing gateway RBAC: cluster=%t role=%t binding=%t", foundCluster, foundRole, foundBinding)
	}
	if !foundWorkspaceLease {
		test.Fatal("gateway must retain Lease access for approval creation in tenant workspaces")
	}
}
