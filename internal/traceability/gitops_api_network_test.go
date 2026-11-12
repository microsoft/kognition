// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"encoding/json"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestGitOpsControlPlaneAPIEgressUsesExplicitEndpoints(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		settings  []string
		wantCount int
	}{
		{"enabled", []string{"networkPolicy.enabled=true", "networkPolicy.kubernetesApiEgressCIDRs[0]=10.96.0.1/32", "networkPolicy.kubernetesApiEgressCIDRs[1]=172.20.0.4/32"}, 1},
		{"no-endpoints", []string{"networkPolicy.enabled=true"}, 0},
		{"disabled", []string{"networkPolicy.enabled=false", "networkPolicy.kubernetesApiEgressCIDRs[0]=10.96.0.1/32"}, 0},
		{"standalone", []string{"mode=standalone", "networkPolicy.enabled=true", "networkPolicy.kubernetesApiEgressCIDRs[0]=10.96.0.1/32"}, 0},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			arguments := []string{}
			for _, setting := range scenario.settings {
				arguments = append(arguments, "--set", setting)
			}
			found := 0
			for _, document := range renderControllerChart(test, arguments...) {
				var policy networkingv1.NetworkPolicy
				if err := json.Unmarshal(document.Raw, &policy); err != nil {
					test.Fatal(err)
				}
				if policy.Kind != "NetworkPolicy" || policy.Name != "security-test-kognition-kubernetes-api" {
					continue
				}
				found++
				if policy.Namespace != "control" || !reflect.DeepEqual(policy.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}) || len(policy.Spec.Ingress) != 0 {
					test.Fatal("API egress policy must be installation-scoped and must not grant ingress")
				}
				expectedSelector := metav1.LabelSelector{
					MatchLabels: map[string]string{"app.kubernetes.io/instance": "security-test"},
					MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key: "app.kubernetes.io/component", Operator: metav1.LabelSelectorOpIn,
						Values: []string{"k8s-controller", "memory-service", "memory-policy-gateway", "tool-gateway", "model-gateway", "agent-communication-gateway"},
					}},
				}
				if !reflect.DeepEqual(policy.Spec.PodSelector, expectedSelector) {
					test.Fatalf("API policy selects unrelated workloads: %+v", policy.Spec.PodSelector)
				}
				expectedEgress := []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{
						{IPBlock: &networkingv1.IPBlock{CIDR: "10.96.0.1/32"}},
						{IPBlock: &networkingv1.IPBlock{CIDR: "172.20.0.4/32"}},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt(443))},
						{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt(6443))},
					},
				}}
				if !reflect.DeepEqual(policy.Spec.Egress, expectedEgress) {
					test.Fatalf("API egress broadened beyond the reviewed endpoints and HTTPS ports: %+v", policy.Spec.Egress)
				}
			}
			if found != scenario.wantCount {
				test.Fatalf("explicit API egress policies = %d, want %d", found, scenario.wantCount)
			}
		})
	}
}
