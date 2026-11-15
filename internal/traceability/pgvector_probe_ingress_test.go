// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/installation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPgvectorProviderProbeIngress(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required for chart rendering")
	}
	for _, scenario := range []struct {
		name     string
		cilium   bool
		nodeCIDR bool
		disable  bool
		reject   bool
	}{
		{name: "legacy exact node route", nodeCIDR: true},
		{name: "AKS node entities", cilium: true},
		{name: "Cilium replaces redundant node CIDRs", cilium: true, nodeCIDR: true},
		{name: "Cilium requires enforcement", cilium: true, disable: true, reject: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			arguments := []string{
				"template", "sample-provider", "../../deploy/helm/kognition-knowledge-pgvector",
				"--namespace", "kognition-providers",
				"--set", "adapter.enabled=true",
				"--set", "adapter.image=example.invalid/provider@sha256:" + strings.Repeat("a", 64),
				"--set", "adapter.watchNamespace=kognition-work",
				"--set", "adapter.tlsSecretName=provider-tls",
				"--set", "adapter.clientPeerURI=spiffe://kognition/knowledge-gateway",
				"--set", "adapter.databaseAddress=postgres.kognition-system.svc:5432",
				"--set", "adapter.networkPolicy.databasePodSelector.app=postgres",
				"--set-json", `adapter.networkPolicy.clients=[{"namespace":"kognition-system","podSelector":{"app":"knowledge-gateway"}}]`,
			}
			if scenario.disable {
				arguments = append(arguments, "--set", "adapter.networkPolicy.enabled=false")
			} else {
				arguments = append(arguments, "--set", "adapter.networkPolicy.ciliumApiServerEgress=true")
			}
			if scenario.cilium {
				arguments = append(arguments, "--set", "adapter.networkPolicy.ciliumHostProbeIngress=true")
				arguments = append(arguments, "--set-json", `adapter.secretNames=["sample-pgvector-credentials","sample-pgvector-ca"]`)
			}
			if scenario.nodeCIDR {
				arguments = append(arguments, "--set", "adapter.networkPolicy.kubeletProbeIngressCIDRs[0]=10.224.0.4/32")
			}
			output, err := exec.CommandContext(ctx, helm, arguments...).CombinedOutput()
			if scenario.reject {
				if err == nil || !strings.Contains(string(output), "Cilium probe ingress requires enforced NetworkPolicy") {
					t.Fatalf("unsafe probe configuration was not explicitly rejected: %v\n%s", err, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("provider render failed: %v\n%s", err, output)
			}
			objects, err := installation.Documents(output)
			if err != nil {
				t.Fatal(err)
			}
			probes := 0
			for _, object := range objects {
				if object.GetKind() == "Role" && object.GetNamespace() == "kognition-work" {
					rules, _, _ := unstructured.NestedSlice(object.Object, "rules")
					for _, value := range rules {
						rule, ok := value.(map[string]interface{})
						if !ok || !reflect.DeepEqual(rule["resources"], []interface{}{"secrets"}) {
							continue
						}
						if scenario.cilium && !reflect.DeepEqual(rule["resourceNames"], []interface{}{"sample-pgvector-credentials", "sample-pgvector-ca"}) {
							t.Fatal("reviewed provider must read only its declared credential and trust bundle")
						}
					}
				}
				if object.GetKind() != "CiliumNetworkPolicy" || object.GetName() != "sample-provider-pgvector-probes" {
					continue
				}
				probes++
				if object.GetNamespace() != "kognition-providers" {
					t.Fatal("probe policy escaped the provider namespace")
				}
				defaultDeny, _, _ := unstructured.NestedBool(object.Object, "spec", "enableDefaultDeny", "ingress")
				policyMode, found, _ := unstructured.NestedMap(object.Object, "spec", "enableDefaultDeny")
				if defaultDeny || !found || !reflect.DeepEqual(policyMode, map[string]interface{}{"ingress": false, "egress": false}) {
					t.Fatal("probe allowance must not introduce new default-deny policy")
				}
				selector, _, _ := unstructured.NestedStringMap(object.Object, "spec", "endpointSelector", "matchLabels")
				if !reflect.DeepEqual(selector, map[string]string{"k8s:app.kubernetes.io/name": "sample-provider-pgvector"}) {
					t.Fatalf("probe selector is broader than the provider: %v", selector)
				}

				ingress, _, _ := unstructured.NestedSlice(object.Object, "spec", "ingress")
				expected := []any{map[string]any{
					"fromEntities": []any{"host", "remote-node", "health"},
					"toPorts":      []any{map[string]any{"ports": []any{map[string]any{"port": "9092", "protocol": "TCP"}}}},
				}}
				if !reflect.DeepEqual(ingress, expected) {
					t.Fatalf("unexpected probe authority: %v", ingress)
				}
				if _, exists, _ := unstructured.NestedFieldNoCopy(object.Object, "spec", "egress"); exists {
					t.Fatal("health policy must not grant egress")
				}
			}
			if (probes == 1) != scenario.cilium {
				t.Fatalf("probe policy count = %d, Cilium enabled = %v", probes, scenario.cilium)
			}
			hasNodeCIDR := strings.Contains(string(output), "cidr: \"10.224.0.4/32\"")
			if hasNodeCIDR != (scenario.nodeCIDR && !scenario.cilium) {
				t.Fatal("Cilium must replace, not combine with unsupported native node ipBlock selection")
			}
		})
	}
}
