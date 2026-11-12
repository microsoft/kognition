// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	strictjson "sigs.k8s.io/json"
)

func TestKnowledgeProviderCleanupHasScopedControllerEgress(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			settings := map[string]any{
				"networkPolicy": map[string]any{"enabled": true},
				"knowledgeGateway": map[string]any{
					"enabled": enabled,
					"provider": map[string]any{
						"address": "pgvector.provider.svc:9555", "serverName": "pgvector.provider.svc",
						"peerURI": "spiffe://kognition.dev/knowledge/pgvector", "port": 9555,
						"kubernetesApiEgressCIDRs": []string{"10.96.0.1/32"},
						"networkPolicy": map[string]any{
							"namespaceSelector": map[string]string{"kubernetes.io/metadata.name": "provider"},
							"podSelector":       map[string]string{"app": "pgvector"},
						},
					},
					"tls":        map[string]string{"secretName": "provider-client-tls"},
					"runtimeTLS": map[string]string{"secretName": "knowledge-runtime-tls"},
				},
			}
			content, err := json.Marshal(settings)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "knowledge-values.json")
			if err := os.WriteFile(path, content, 0600); err != nil {
				t.Fatal(err)
			}
			found, routes := false, 0
			for _, document := range renderControllerChart(t, "--values", path) {
				var policy networkingv1.NetworkPolicy
				if err := json.Unmarshal(document.Raw, &policy); err != nil {
					t.Fatal(err)
				}
				if policy.Kind == "NetworkPolicy" {
					problems, err := strictjson.UnmarshalStrict(document.Raw, &policy)
					if err != nil || len(problems) != 0 {
						t.Fatalf("invalid rendered NetworkPolicy %s: %v, %v", policy.Name, err, problems)
					}
				}
				if policy.Kind != "NetworkPolicy" || policy.Name != "security-test-kognition-k8s-controller" {
					continue
				}
				found = true
				expectedSelector := metav1.LabelSelector{MatchLabels: map[string]string{
					"app.kubernetes.io/component": "k8s-controller", "app.kubernetes.io/instance": "security-test",
				}}
				if policy.Namespace != "control" || !reflect.DeepEqual(policy.Spec.PodSelector, expectedSelector) {
					t.Fatal("provider cleanup route must select only the release controller")
				}
				expected := networkingv1.NetworkPolicyEgressRule{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "provider"}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "pgvector"}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: ptr.To(corev1.ProtocolTCP), Port: ptr.To(intstr.FromInt(9555))}},
				}
				for _, rule := range policy.Spec.Egress {
					for _, port := range rule.Ports {
						if port.Port != nil && port.Port.IntValue() == 9555 {
							routes++
							if !reflect.DeepEqual(rule, expected) {
								t.Fatal("provider cleanup route must use the exact registered peer selectors and port")
							}
						}
					}
				}
			}
			expectedRoutes := 0
			if enabled {
				expectedRoutes = 1
			}
			if !found || routes != expectedRoutes {
				t.Fatalf("controller policy found=%v, provider cleanup routes=%d, want %d", found, routes, expectedRoutes)
			}
		})
	}
}

func TestKnowledgeGatewaySupportsPreprovisionedCapabilitySecret(t *testing.T) {
	for _, create := range []string{"default", "false"} {
		t.Run(create, func(t *testing.T) {
			arguments := []string{
				"--set", "knowledgeGateway.enabled=true",
				"--set", "knowledgeGateway.provider.address=pgvector.control.svc:9443",
				"--set", "knowledgeGateway.provider.serverName=pgvector.control.svc",
				"--set", "knowledgeGateway.provider.peerURI=spiffe://kognition.dev/knowledge/pgvector",
				"--set", "knowledgeGateway.tls.secretName=provider-client-tls",
				"--set", "knowledgeGateway.runtimeTLS.secretName=knowledge-runtime-tls",
			}
			if create != "default" {
				arguments = append(arguments, "--set", "knowledgeGateway.secret.create="+create)
			}
			secrets, consumers := 0, 0
			for _, document := range renderControllerChart(t, arguments...) {
				var object struct {
					Kind string `json:"kind"`
				}
				if err := json.Unmarshal(document.Raw, &object); err != nil {
					t.Fatal(err)
				}
				switch object.Kind {
				case "Secret":
					var secret corev1.Secret
					if err := json.Unmarshal(document.Raw, &secret); err != nil {
						t.Fatal(err)
					}
					if secret.Name == "security-test-kognition-knowledge-gateway" {
						secrets++
						if len(secret.Data["capability-hmac-key"]) == 0 || len(secret.Data["capability-hmac-key-id"]) == 0 {
							t.Fatal("chart-created capability Secret is incomplete")
						}
					}
				case "Deployment":
					var deployment appsv1.Deployment
					if err := json.Unmarshal(document.Raw, &deployment); err != nil {
						t.Fatal(err)
					}
					for _, container := range deployment.Spec.Template.Spec.Containers {
						for _, environment := range container.Env {
							if environment.ValueFrom == nil || environment.ValueFrom.SecretKeyRef == nil {
								continue
							}
							reference := environment.ValueFrom.SecretKeyRef
							if reference.Name == "security-test-kognition-knowledge-gateway" && reference.Key == "capability-hmac-key" {
								consumers++
							}
						}
					}
				}
			}
			expectedSecrets := 1
			if create == "false" {
				expectedSecrets = 0
			}
			if secrets != expectedSecrets || consumers != 2 {
				t.Fatalf("capability Secret create=%s: rendered=%d, consumers=%d", create, secrets, consumers)
			}
		})
	}
}

func TestKnowledgeGatewayCiliumAPIRouteIsExactlyBoundToReviewedGateway(t *testing.T) {
	values := map[string]any{"knowledgeGateway": map[string]any{
		"enabled": true,
		"provider": map[string]any{
			"name": "pgvector", "address": "pgvector.control.svc:9443", "serverName": "pgvector.control.svc", "peerURI": "spiffe://kognition.dev/knowledge/pgvector",
			"configuration":            map[string]any{"apiGroup": "pgvector.knowledge.kognition.dev", "apiVersion": "v1alpha1", "kind": "PgvectorBackend", "resource": "pgvectorbackends"},
			"kubernetesApiEgressCIDRs": []string{"172.20.0.4/32"},
			"networkPolicy":            map[string]any{"namespaceSelector": map[string]string{"kubernetes.io/metadata.name": "control"}, "podSelector": map[string]string{"app": "pgvector"}},
		},
		"tls":        map[string]string{"secretName": "provider-client-tls"},
		"runtimeTLS": map[string]string{"secretName": "knowledge-runtime-tls"},
	}}
	content, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "knowledge-values.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	original := ciliumAPITarget(t, "--values", path)
	if _, err := activation.ParseCandidate(encodeCiliumAPITarget(t, original), "security-test"); err != nil {
		t.Fatalf("reviewed gateway profile rejected: %v", err)
	}
	for _, test := range []string{"gateway removed", "foreign namespace", "wrong identity", "foreign component"} {
		t.Run(test, func(t *testing.T) {
			objects := copyActivationDocuments(original)
			for index, object := range objects {
				if object.GetKind() == "Deployment" && object.GetName() == "security-test-kognition-knowledge-gateway" {
					switch test {
					case "gateway removed":
						objects = append(objects[:index], objects[index+1:]...)
					case "foreign namespace":
						object.SetNamespace("foreign")
					case "wrong identity":
						if err := unstructured.SetNestedField(object.Object, "foreign", "spec", "template", "spec", "serviceAccountName"); err != nil {
							t.Fatal(err)
						}
					}
					break
				}
			}
			if test == "foreign component" {
				for _, object := range objects {
					if object.GetKind() == "CiliumNetworkPolicy" && object.GetName() == "security-test-kognition-kubernetes-api" {
						expressions, _, err := unstructured.NestedSlice(object.Object, "spec", "endpointSelector", "matchExpressions")
						if err != nil {
							t.Fatal(err)
						}
						expression, ok := expressions[0].(map[string]any)
						if !ok {
							t.Fatal("unexpected selector expression")
						}
						expression["values"] = []any{"k8s-controller", "memory-service", "memory-policy-gateway", "tool-gateway", "model-gateway", "agent-communication-gateway", "foreign"}
						if err := unstructured.SetNestedSlice(object.Object, expressions, "spec", "endpointSelector", "matchExpressions"); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if _, err := activation.ParseCandidate(encodeCiliumAPITarget(t, objects), "security-test"); err == nil {
				t.Fatal("unsafe gateway API route accepted")
			}
		})
	}
}
