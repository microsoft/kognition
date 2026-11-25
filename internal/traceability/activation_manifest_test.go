// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func activationDocuments(test *testing.T, extra ...string) []*unstructured.Unstructured {
	test.Helper()
	var objects []*unstructured.Unstructured
	arguments := append([]string{"--set", "k8sController.managedNamespaces[0]=tenant", "--set", "k8sController.managedNamespaces[1]=second"}, extra...)
	for _, document := range renderControllerChart(test, arguments...) {
		object := &unstructured.Unstructured{}
		if err := object.UnmarshalJSON(document.Raw); err != nil {
			test.Fatal(err)
		}
		objects = append(objects, object)
	}
	return objects
}

func encodeActivationDocuments(test *testing.T, objects []*unstructured.Unstructured) string {
	test.Helper()
	var encoded strings.Builder
	for _, object := range objects {
		data, err := json.Marshal(object)
		if err != nil {
			test.Fatal(err)
		}
		encoded.WriteString("---\n")
		encoded.Write(data)
		encoded.WriteByte('\n')
	}
	return encoded.String()
}

func TestActivationRejectsUnsafeManifest(test *testing.T) {
	original := activationDocuments(test)
	for _, protection := range []string{"model-dispatch", "model-call-health"} {
		for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
			test.Run(protection+"/"+kind, func(test *testing.T) {
				objects := copyActivationDocuments(original)
				removed := false
				for index, object := range objects {
					if object.GetKind() == kind && strings.HasSuffix(object.GetName(), "-"+protection) {
						objects = append(objects[:index], objects[index+1:]...)
						removed = true
						break
					}
				}
				if !removed {
					test.Fatalf("%s protection missing from rendered chart", protection)
				}
				if _, err := activation.Parse(strings.NewReader(encodeActivationDocuments(test, objects))); err == nil {
					test.Fatalf("activation accepted omitted %s protection", protection)
				}
			})
		}
	}
	for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
		test.Run("gateway-leases/"+kind, func(test *testing.T) {
			objects := copyActivationDocuments(original)
			removed := false
			for index, object := range objects {
				if object.GetKind() == kind && strings.HasSuffix(object.GetName(), "-gateway-leases") {
					objects = append(objects[:index], objects[index+1:]...)
					removed = true
					break
				}
			}
			if !removed {
				test.Fatal("gateway Lease protection missing from rendered chart")
			}
			if _, err := activation.Parse(strings.NewReader(encodeActivationDocuments(test, objects))); err == nil {
				test.Fatal("activation accepted omitted gateway Lease protection")
			}
		})
	}
	test.Run("gateway-leases/renamed-pair", func(test *testing.T) {
		objects := copyActivationDocuments(original)
		for _, object := range objects {
			if strings.HasSuffix(object.GetName(), "-gateway-leases") && (object.GetKind() == "ValidatingAdmissionPolicy" || object.GetKind() == "ValidatingAdmissionPolicyBinding") {
				object.SetName("replacement-policy")
				if object.GetKind() == "ValidatingAdmissionPolicyBinding" {
					if err := unstructured.SetNestedField(object.Object, "replacement-policy", "spec", "policyName"); err != nil {
						test.Fatal(err)
					}
				}
			}
		}
		if _, err := activation.Parse(strings.NewReader(encodeActivationDocuments(test, objects))); err == nil {
			test.Fatal("activation accepted renamed gateway enforcement")
		}
	})
	for _, mutation := range []string{"missing-policy", "missing-binding", "renamed-pair"} {
		test.Run("webhook-audit-state/"+mutation, func(test *testing.T) {
			objects := copyActivationDocuments(original)
			for index := len(objects) - 1; index >= 0; index-- {
				object := objects[index]
				if object.GetName() != "security-test-kognition-control-state" || (object.GetKind() != "ValidatingAdmissionPolicy" && object.GetKind() != "ValidatingAdmissionPolicyBinding") {
					continue
				}
				if mutation == "missing-policy" && object.GetKind() == "ValidatingAdmissionPolicy" || mutation == "missing-binding" && object.GetKind() == "ValidatingAdmissionPolicyBinding" {
					objects = append(objects[:index], objects[index+1:]...)
				} else if mutation == "renamed-pair" {
					object.SetName("replacement-state")
					if object.GetKind() == "ValidatingAdmissionPolicyBinding" {
						if err := unstructured.SetNestedField(object.Object, "replacement-state", "spec", "policyName"); err != nil {
							test.Fatal(err)
						}
					}
				}
			}
			if _, err := activation.Parse(strings.NewReader(encodeActivationDocuments(test, objects))); err == nil {
				test.Fatal("activation accepted missing or replaced audit/state enforcement")
			}
		})
	}
	for _, scenario := range []struct {
		name, kind, message string
		path                []string
		value               interface{}
	}{
		{"controller-replicas-type", "Controller", "zero controller replicas", []string{"spec", "replicas"}, "zero"},
		{"controller-version", "Controller", "controller Deployment version", []string{"apiVersion"}, "apps/v1beta1"},
		{"guard-account", "Deployment", "guard service account", []string{"spec", "template", "spec", "serviceAccountName"}, "untrusted"},
		{"guard-replicas", "Deployment", "replicas", []string{"spec", "replicas"}, int64(0)},
		{"guard-subject", "RoleBinding", "RoleBinding identity", []string{"subjects"}, []interface{}{map[string]interface{}{"kind": "ServiceAccount", "name": "untrusted", "namespace": "control"}}},
		{"guard-role-reference", "RoleBinding", "RoleBinding identity", []string{"roleRef", "name"}, "untrusted"},
		{"guard-role-permissions", "Role", "read-only permissions", []string{"rules"}, []interface{}{}},
		{"mispaired-binding", "ValidatingAdmissionPolicyBinding", "policy-to-binding pairing", []string{"spec", "policyName"}, "missing"},
		{"warn-binding", "ValidatingAdmissionPolicyBinding", "must deny", []string{"spec", "validationActions"}, []interface{}{"Warn"}},
		{"narrow-binding", "ValidatingAdmissionPolicyBinding", "narrowed scope", []string{"spec", "matchResources"}, map[string]interface{}{}},
		{"ignore-policy", "ValidatingAdmissionPolicy", "fail-closed", []string{"spec", "failurePolicy"}, "Ignore"},
		{"empty-validations", "ValidatingAdmissionPolicy", "validations", []string{"spec", "validations"}, []interface{}{}},
		{"open-baseline", "NetworkPolicy", "deny-egress baseline", []string{"spec", "egress"}, []interface{}{map[string]interface{}{}}},
		{"webhook-service", "ValidatingWebhookConfiguration", "webhook identity", nil, "service"},
		{"webhook-controller", "ValidatingWebhookConfiguration", "webhook conditions", nil, "controller"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			objects := copyActivationDocuments(original)
			object := activationDocument(test, objects, scenario.kind)
			if scenario.kind == "ValidatingWebhookConfiguration" {
				webhooks := object.Object["webhooks"].([]interface{})
				object = &unstructured.Unstructured{Object: webhooks[0].(map[string]interface{})}
				if scenario.value == "service" {
					scenario.path, scenario.value = []string{"clientConfig", "service", "namespace"}, "untrusted"
				} else {
					scenario.path, scenario.value = []string{"matchConditions"}, []interface{}{map[string]interface{}{"name": "controller-identity", "expression": "true"}}
				}
			}
			if err := unstructured.SetNestedField(object.Object, scenario.value, scenario.path...); err != nil {
				test.Fatal(err)
			}
			_, err := activation.Parse(strings.NewReader(encodeActivationDocuments(test, objects)))
			if err == nil || !strings.Contains(err.Error(), scenario.message) {
				test.Fatalf("expected %q, got %v", scenario.message, err)
			}
		})
	}
	for _, kind := range []string{"Deployment", "Service", "ServiceAccount", "Role", "RoleBinding", "NetworkPolicy", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "ValidatingWebhookConfiguration"} {
		for _, mutation := range []string{"version", "missing", "duplicate"} {
			test.Run(kind+"/"+mutation, func(test *testing.T) {
				objects := copyActivationDocuments(original)
				object := activationDocument(test, objects, kind)
				message := ""
				switch mutation {
				case "version":
					object.SetAPIVersion("unsupported/v9")
					message = "unsupported bootstrap GVK"
				case "missing":
					message = "manifest must contain"
					if kind == "ServiceAccount" || kind == "Role" || kind == "RoleBinding" {
						message = "incomplete bootstrap policy and guard inventory"
					}
					if kind == "NetworkPolicy" {
						message = "unexpected workload namespace"
					}
					for index, candidate := range objects {
						if candidate == object {
							objects = append(objects[:index], objects[index+1:]...)
							break
						}
					}
				case "duplicate":
					message = "manifest must contain"
					if kind == "ServiceAccount" || kind == "Role" || kind == "RoleBinding" {
						message = "duplicate bootstrap object"
					}
					if kind == "NetworkPolicy" {
						message = "duplicate stdio baseline namespace"
					}
					objects = append(objects, object.DeepCopy())
				}
				_, err := activation.Parse(strings.NewReader(encodeActivationDocuments(test, objects)))
				if err == nil || !strings.Contains(err.Error(), message) {
					test.Fatalf("accepted %s %s: %v", kind, mutation, err)
				}
			})
		}
	}
	for _, mutation := range []string{"object", "identity", "namespace", "constructed", "duplicate-baseline"} {
		test.Run("immutable/"+mutation, func(test *testing.T) {
			manifest, err := activation.Parse(strings.NewReader(encodeActivationDocuments(test, original)))
			if err != nil {
				test.Fatal(err)
			}
			message := "unmodified output from Parse"
			switch mutation {
			case "object":
				manifest.Objects[0].SetLabels(map[string]string{"changed": "true"})
			case "identity":
				manifest.Controller = "other-controller"
				message = "webhook conditions"
			case "namespace":
				manifest.Namespaces[0], manifest.Namespaces[1] = manifest.Namespaces[1], manifest.Namespaces[0]
				message = "webhook conditions"
			case "constructed":
				manifest = activation.Manifest{Namespace: manifest.Namespace, Controller: manifest.Controller, Deployment: manifest.Deployment, Guard: manifest.Guard, Namespaces: manifest.Namespaces, Objects: manifest.Objects}
			case "duplicate-baseline":
				var namespace string
				for _, object := range manifest.Objects {
					if object.GetKind() == "NetworkPolicy" {
						if namespace == "" {
							namespace = object.GetNamespace()
						} else {
							object.SetNamespace(namespace)
							object.SetName("other-stdio-baseline")
						}
					}
				}
				message = "duplicate stdio baseline namespace"
			}
			if err := activation.Verify(context.Background(), nil, manifest); err == nil || !strings.Contains(err.Error(), message) {
				test.Fatalf("expected %q, got %v", message, err)
			}
		})
	}
}

func copyActivationDocuments(original []*unstructured.Unstructured) []*unstructured.Unstructured {
	objects := make([]*unstructured.Unstructured, len(original))
	for index, object := range original {
		objects[index] = object.DeepCopy()
	}
	return objects
}

func activationDocument(test *testing.T, objects []*unstructured.Unstructured, kind string) *unstructured.Unstructured {
	test.Helper()
	for _, object := range objects {
		if kind == "Controller" && object.GetKind() == "Deployment" && strings.HasSuffix(object.GetName(), "-k8s-controller") {
			return object
		}
		if object.GetKind() == kind && (strings.HasSuffix(object.GetName(), "-admission-guard") || strings.HasSuffix(object.GetName(), "-stdio-baseline") || strings.HasPrefix(kind, "ValidatingAdmissionPolicy")) {
			return object
		}
	}
	test.Fatalf("missing fixture kind %s", kind)
	return nil
}
