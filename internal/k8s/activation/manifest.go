// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (manifest Manifest) isCiliumAPIEgress(object *unstructured.Unstructured) bool {
	return object.GetKind() == "CiliumNetworkPolicy" &&
		object.GetNamespace() == manifest.Namespace &&
		object.GetName() == strings.TrimSuffix(manifest.Guard, "-admission-guard")+"-kubernetes-api"
}

func (manifest Manifest) validateCiliumAPIEgress(object *unstructured.Unstructured) error {
	if manifest.StdioIsolationProfile != "cilium" {
		return errors.New("Cilium API egress requires the cilium isolation profile")
	}
	release := ""
	for _, guard := range manifest.Objects {
		if guard == nil {
			return errors.New("nil bootstrap object")
		}
		if guard.GetKind() == "Deployment" && guard.GetName() == manifest.Guard {
			var err error
			release, _, err = unstructured.NestedString(guard.Object, "spec", "template", "metadata", "labels", "app.kubernetes.io/instance")
			if err != nil {
				return err
			}
		}
	}
	if release == "" || object.GetLabels()["app.kubernetes.io/instance"] != release {
		return errors.New("Cilium API egress must select the reviewed guard release")
	}
	components := []interface{}{"k8s-controller", "memory-service", "memory-policy-gateway", "tool-gateway", "model-gateway", "agent-communication-gateway"}
	if manifest.KnowledgeGateway {
		components = append(components, "knowledge-gateway")
	}
	expected := map[string]interface{}{
		"endpointSelector": map[string]interface{}{
			"matchLabels": map[string]interface{}{"k8s:app.kubernetes.io/instance": release},
			"matchExpressions": []interface{}{map[string]interface{}{
				"key": "k8s:app.kubernetes.io/component", "operator": "In",
				"values": components,
			}},
		},
		"egress": []interface{}{map[string]interface{}{
			"toEntities": []interface{}{"host", "kube-apiserver"},
			"toPorts": []interface{}{map[string]interface{}{
				"ports": []interface{}{
					map[string]interface{}{"port": "443", "protocol": "TCP"},
					map[string]interface{}{"port": "6443", "protocol": "TCP"},
				},
			}},
		}},
	}
	if !reflect.DeepEqual(object.Object["spec"], expected) || object.Object["specs"] != nil {
		return errors.New("invalid scoped Cilium API egress policy")
	}
	return nil
}

func (manifest Manifest) validateObject(object *unstructured.Unstructured) error {
	encoded, err := object.MarshalJSON()
	if err != nil {
		return err
	}
	switch object.GetKind() {
	case "CiliumNetworkPolicy", "CiliumClusterwideNetworkPolicy":
		if object.GetLabels()["app.kubernetes.io/managed-by"] != "Helm" || object.GetAnnotations()["meta.helm.sh/release-name"] == "" || object.GetAnnotations()["meta.helm.sh/release-name"] != object.GetLabels()["app.kubernetes.io/instance"] || object.GetAnnotations()["meta.helm.sh/release-namespace"] != manifest.Namespace {
			return errors.New("invalid Cilium baseline release ownership")
		}
		if manifest.isCiliumAPIEgress(object) {
			return manifest.validateCiliumAPIEgress(object)
		}
		if manifest.probeComponent(object) != "" || manifest.isRuntimeProbe(object) {
			return manifest.validateProbeIngress(object)
		}
		expected := map[string]interface{}{
			"endpointSelector": map[string]interface{}{
				"matchLabels":      map[string]interface{}{"k8s:app.kubernetes.io/managed-by": "kognition"},
				"matchExpressions": []interface{}{map[string]interface{}{"key": "k8s:kognition.dev/stdio-invocation", "operator": "Exists"}},
			},
			"egressDeny": []interface{}{map[string]interface{}{"toEntities": []interface{}{"all"}}},
		}
		if !reflect.DeepEqual(object.Object["spec"], expected) || object.Object["specs"] != nil {
			return errors.New("invalid Cilium deny-egress baseline")
		}
	case "Deployment":
		var deployment appsv1.Deployment
		if err := json.Unmarshal(encoded, &deployment); err != nil {
			return err
		}
		if deployment.Spec.Template.Spec.ServiceAccountName != manifest.Guard || deployment.Spec.Replicas == nil || *deployment.Spec.Replicas < 1 {
			return errors.New("invalid independent guard service account or replicas")
		}
	case "Role":
		var role rbacv1.Role
		if err := json.Unmarshal(encoded, &role); err != nil {
			return err
		}
		expected := []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}},
			{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get"}},
			{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"}, Verbs: []string{"get"}},
		}
		if !reflect.DeepEqual(role.Rules, expected) {
			return errors.New("invalid guard read-only permissions")
		}
	case "RoleBinding":
		var binding rbacv1.RoleBinding
		if err := json.Unmarshal(encoded, &binding); err != nil {
			return err
		}
		if binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: manifest.Guard}) || !reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Name: manifest.Guard, Namespace: manifest.Namespace}}) {
			return errors.New("invalid guard RoleBinding identity")
		}
	case "NetworkPolicy":
		var policy networkingv1.NetworkPolicy
		if err := json.Unmarshal(encoded, &policy); err != nil {
			return err
		}
		selector := metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "kognition"}, MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "kognition.dev/stdio-invocation", Operator: metav1.LabelSelectorOpExists}}}
		if !reflect.DeepEqual(policy.Spec.PodSelector, selector) || !reflect.DeepEqual(policy.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}) || len(policy.Spec.Egress) != 0 || len(policy.Spec.Ingress) != 0 {
			return errors.New("invalid deny-egress baseline")
		}
	case "ValidatingAdmissionPolicy":
		var policy admissionv1.ValidatingAdmissionPolicy
		if err := json.Unmarshal(encoded, &policy); err != nil {
			return err
		}
		if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionv1.Fail || policy.Spec.MatchConstraints == nil || len(policy.Spec.MatchConstraints.ResourceRules) == 0 || len(policy.Spec.MatchConditions) == 0 || len(policy.Spec.Validations) == 0 {
			return errors.New("policy must contain fail-closed rules, conditions and validations")
		}
		for _, validation := range policy.Spec.Validations {
			if validation.Expression == "" || validation.Reason == nil || *validation.Reason != metav1.StatusReasonForbidden {
				return errors.New("policy requires Forbidden validations")
			}
		}
	case "ValidatingAdmissionPolicyBinding":
		var binding admissionv1.ValidatingAdmissionPolicyBinding
		if err := json.Unmarshal(encoded, &binding); err != nil {
			return err
		}
		if !reflect.DeepEqual(binding.Spec.ValidationActions, []admissionv1.ValidationAction{admissionv1.Deny}) || binding.Spec.MatchResources != nil || binding.Spec.ParamRef != nil {
			return errors.New("policy binding must deny without narrowed scope or parameters")
		}
	case "ValidatingWebhookConfiguration":
		var configuration admissionv1.ValidatingWebhookConfiguration
		if err := json.Unmarshal(encoded, &configuration); err != nil {
			return err
		}
		if configuration.Name != manifest.Guard || len(configuration.Webhooks) != 2 {
			return errors.New("invalid guard webhook identity")
		}
		for index, name := range []string{"attach.guard.kognition.dev", "sandbox.guard.kognition.dev"} {
			webhook := configuration.Webhooks[index]
			service := webhook.ClientConfig.Service
			if webhook.Name != name || webhook.FailurePolicy == nil || *webhook.FailurePolicy != admissionv1.Fail || service == nil || service.Name != manifest.Guard || service.Namespace != manifest.Namespace || service.Path == nil || *service.Path != "/guard" || service.Port == nil || *service.Port != 443 || webhook.ClientConfig.URL != nil {
				return errors.New("invalid fail-closed guard webhook identity")
			}
			conditions := []admissionv1.MatchCondition{{Name: "controller-identity", Expression: fmt.Sprintf("request.userInfo.username == 'system:serviceaccount:%s:%s'", manifest.Namespace, manifest.Controller)}}
			scope := admissionv1.NamespacedScope
			rules := []admissionv1.RuleWithOperations{{Operations: []admissionv1.OperationType{admissionv1.Connect}, Rule: admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods/attach"}, Scope: &scope}}}
			if index == 1 {
				namespaces, err := json.Marshal(manifest.Namespaces)
				if err != nil {
					return err
				}
				conditions = []admissionv1.MatchCondition{
					{Name: "enrolled-workload-namespace", Expression: "request.namespace in " + string(namespaces)},
					{Name: "reserved-sandbox-name", Expression: "(has(object.metadata.name) && object.metadata.name.startsWith('kognition-stdio-invocation-')) || (has(object.metadata.generateName) && object.metadata.generateName.startsWith('kognition-stdio-invocation-')) || (oldObject != null && has(oldObject.metadata.name) && oldObject.metadata.name.startsWith('kognition-stdio-invocation-'))"},
				}
				rules[0].Operations = []admissionv1.OperationType{admissionv1.Create, admissionv1.Update}
				rules[0].Resources = []string{"pods", "pods/ephemeralcontainers"}
			}
			if !reflect.DeepEqual(webhook.MatchConditions, conditions) {
				return errors.New("invalid guard webhook conditions")
			}
			if !reflect.DeepEqual(webhook.Rules, rules) || webhook.NamespaceSelector != nil || webhook.ObjectSelector != nil || !reflect.DeepEqual(webhook.AdmissionReviewVersions, []string{"v1"}) || webhook.SideEffects == nil || *webhook.SideEffects != admissionv1.SideEffectClassNone || webhook.MatchPolicy == nil || *webhook.MatchPolicy != admissionv1.Equivalent || webhook.TimeoutSeconds == nil || *webhook.TimeoutSeconds != 5 {
				return errors.New("invalid guard webhook rules or delivery settings")
			}
		}
	}
	return nil
}
