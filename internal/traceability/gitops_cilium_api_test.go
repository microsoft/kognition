// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func ciliumAPITarget(test *testing.T, additional ...string) []*unstructured.Unstructured {
	test.Helper()
	var objects []*unstructured.Unstructured
	arguments := []string{
		"--set", "networkPolicy.enabled=true",
		"--set", "networkPolicy.ciliumApiServerEgress=true",
		"--set", "admissionGuard.stdioIsolationProfile=cilium",
		"--set", "k8sController.managedNamespaces[0]=tenant",
	}
	arguments = append(arguments, additional...)
	for _, document := range renderControllerChart(test, arguments...) {
		object := &unstructured.Unstructured{}
		if err := json.Unmarshal(document.Raw, object); err != nil {
			test.Fatal(err)
		}
		objects = append(objects, object)
	}
	return objects
}

func encodeCiliumAPITarget(test *testing.T, objects []*unstructured.Unstructured) []byte {
	test.Helper()
	var output strings.Builder
	for _, object := range objects {
		content, err := object.MarshalJSON()
		if err != nil {
			test.Fatal(err)
		}
		output.Write(content)
		output.WriteString("\n---\n")
	}
	return []byte(output.String())
}

func TestGitOpsCiliumAPIEgressRetainsStrictActivationInventory(test *testing.T) {
	objects := ciliumAPITarget(test)
	apiPolicies, denies := 0, 0
	for _, object := range objects {
		if object.GetKind() != "CiliumNetworkPolicy" {
			continue
		}
		if object.GetNamespace() == "control" && object.GetName() == "security-test-kognition-kubernetes-api" {
			apiPolicies++
		}
		if object.GetNamespace() == "tenant" && strings.HasSuffix(object.GetName(), "-stdio-deny") {
			denies++
		}
	}
	if apiPolicies != 1 || denies != 1 {
		test.Fatalf("require one installation API policy and one unchanged workload deny: api=%d denies=%d", apiPolicies, denies)
	}
	if _, err := activation.ParseCandidate(encodeCiliumAPITarget(test, objects), "security-test"); err != nil {
		test.Fatalf("exact scoped Cilium API target rejected: %v", err)
	}
}

func TestGitOpsCiliumAPIEgressRejectsUnsafeProfile(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "helm", "template", "security-test", "../../deploy/helm/kognition",
		"--namespace", "control", "--kube-version", "1.31.1", "--set", "mode=kubernetes",
		"--set", "networkPolicy.enabled=true", "--set", "networkPolicy.kubeletProbeIngressCIDRs[0]=172.20.0.4/32",
		"--set", "networkPolicy.ciliumApiServerEgress=true").CombinedOutput()
	if err == nil {
		test.Fatal("Cilium API egress accepted without the explicitly reviewed Cilium isolation profile")
	}
	if !strings.Contains(string(output), "ciliumApiServerEgress requires Kubernetes mode, enforced NetworkPolicies and the cilium isolation profile") {
		test.Fatalf("unexpected unsafe-profile rejection: %v %s", err, output)
	}
}

func TestActivationRejectsBroadenedCiliumAPIEgress(test *testing.T) {
	base := ciliumAPITarget(test)
	for _, scenario := range []string{
		"workload-namespace", "foreign-namespace", "foreign-name", "foreign-release",
		"guard-release-missing", "guard-release-type", "matching-foreign-release", "non-cilium-profile",
		"all-endpoints", "foreign-instance", "agent-component", "all-entities",
		"additional-peer", "no-port-restriction", "additional-port", "udp",
		"ingress", "multiple-specs", "duplicate", "missing-stdio-deny",
	} {
		test.Run(scenario, func(test *testing.T) {
			objects := make([]*unstructured.Unstructured, len(base))
			var policy *unstructured.Unstructured
			for index, object := range base {
				objects[index] = object.DeepCopy()
				if object.GetKind() == "CiliumNetworkPolicy" && object.GetNamespace() == "control" {
					policy = objects[index]
				}
			}
			if policy == nil {
				test.Fatal("scoped API policy missing from the rendered target")
			}
			spec := policy.Object["spec"].(map[string]interface{})
			selector := spec["endpointSelector"].(map[string]interface{})
			egress := spec["egress"].([]interface{})[0].(map[string]interface{})
			portRule := egress["toPorts"].([]interface{})[0].(map[string]interface{})
			switch scenario {
			case "workload-namespace":
				policy.SetNamespace("tenant")
			case "foreign-namespace":
				policy.SetNamespace("outside")
			case "foreign-name":
				policy.SetName("unreviewed-api-policy")
			case "foreign-release":
				annotations := policy.GetAnnotations()
				annotations["meta.helm.sh/release-name"] = "other"
				policy.SetAnnotations(annotations)
			case "guard-release-missing", "guard-release-type", "non-cilium-profile":
				for _, object := range objects {
					if object.GetKind() != "Deployment" || object.GetName() != "security-test-kognition-admission-guard" {
						continue
					}
					if scenario == "guard-release-missing" {
						unstructured.RemoveNestedField(object.Object, "spec", "template", "metadata", "labels", "app.kubernetes.io/instance")
					} else if scenario == "guard-release-type" {
						if err := unstructured.SetNestedField(object.Object, int64(1), "spec", "template", "metadata", "labels", "app.kubernetes.io/instance"); err != nil {
							test.Fatal(err)
						}
					} else {
						unstructured.RemoveNestedField(object.Object, "spec", "template", "metadata", "annotations", "kognition.dev/stdio-isolation-profile")
					}
				}
			case "matching-foreign-release":
				labels := policy.GetLabels()
				labels["app.kubernetes.io/instance"] = "other"
				policy.SetLabels(labels)
				annotations := policy.GetAnnotations()
				annotations["meta.helm.sh/release-name"] = "other"
				policy.SetAnnotations(annotations)
				selector["matchLabels"] = map[string]interface{}{"k8s:app.kubernetes.io/instance": "other"}
			case "all-endpoints":
				spec["endpointSelector"] = map[string]interface{}{}
			case "foreign-instance":
				selector["matchLabels"] = map[string]interface{}{"k8s:app.kubernetes.io/instance": "other"}
			case "agent-component":
				expression := selector["matchExpressions"].([]interface{})[0].(map[string]interface{})
				expression["values"] = append(expression["values"].([]interface{}), "agent")
			case "all-entities":
				egress["toEntities"] = []interface{}{"all"}
			case "additional-peer":
				egress["toCIDR"] = []interface{}{"0.0.0.0/0"}
			case "no-port-restriction":
				delete(egress, "toPorts")
			case "additional-port":
				portRule["ports"] = append(portRule["ports"].([]interface{}), map[string]interface{}{"port": "80", "protocol": "TCP"})
			case "udp":
				portRule["ports"].([]interface{})[0].(map[string]interface{})["protocol"] = "UDP"
			case "ingress":
				spec["ingress"] = []interface{}{map[string]interface{}{}}
			case "multiple-specs":
				policy.Object["specs"] = []interface{}{spec}
			case "duplicate":
				objects = append(objects, policy.DeepCopy())
			case "missing-stdio-deny":
				var retained []*unstructured.Unstructured
				for _, object := range objects {
					if object.GetKind() != "CiliumNetworkPolicy" || object.GetNamespace() != "tenant" {
						retained = append(retained, object)
					}
				}
				objects = retained
			}
			if _, err := activation.ParseCandidate(encodeCiliumAPITarget(test, objects), "security-test"); err == nil {
				test.Fatal("activation accepted an unreviewed Cilium API policy or missing workload isolation")
			}
		})
	}
}
