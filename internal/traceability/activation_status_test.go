// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestActivationPolicyStatus(test *testing.T) {
	documents := activationDocuments(test)
	manifest, err := activation.Parse(strings.NewReader(encodeActivationDocuments(test, documents)))
	if err != nil {
		test.Fatal(err)
	}
	certificateServer := httptest.NewTLSServer(nil)
	certificate := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateServer.Certificate().Raw}))
	certificateServer.Close()
	var stored []client.Object
	for _, expected := range append([]*unstructured.Unstructured{activationDocument(test, documents, "Controller")}, manifest.Objects...) {
		object := expected.DeepCopy()
		object.SetGeneration(1)
		switch object.GetKind() {
		case "Deployment":
			replicas, _, _ := unstructured.NestedInt64(object.Object, "spec", "replicas")
			object.Object["status"] = map[string]interface{}{"observedGeneration": int64(1), "replicas": replicas, "updatedReplicas": replicas, "availableReplicas": replicas}
		case "ValidatingAdmissionPolicy":
			object.Object["status"] = map[string]interface{}{"observedGeneration": int64(1), "typeChecking": map[string]interface{}{}}
		case "ValidatingWebhookConfiguration":
			for _, webhook := range object.Object["webhooks"].([]interface{}) {
				webhook.(map[string]interface{})["clientConfig"].(map[string]interface{})["caBundle"] = certificate
			}
		}
		stored = append(stored, object)
	}
	base := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(stored...).Build()
	validCondition := map[string]interface{}{"type": "Accepted", "status": "True", "observedGeneration": int64(1), "lastTransitionTime": "2026-09-24T00:00:00Z", "reason": "Checked", "message": "checked"}
	type statusScenario struct {
		name    string
		path    []string
		value   interface{}
		remove  bool
		message string
	}
	scenarios := []statusScenario{
		{"absent-conditions", nil, nil, false, ""},
		{"empty-conditions", []string{"conditions"}, []interface{}{}, false, ""},
		{"reported-success", []string{"conditions"}, []interface{}{validCondition}, false, ""},
		{"empty-warnings", []string{"typeChecking", "expressionWarnings"}, []interface{}{}, false, ""},
		{"missing-generation", []string{"observedGeneration"}, nil, true, "current-generation"},
		{"null-generation", []string{"observedGeneration"}, nil, false, "current-generation"},
		{"string-generation", []string{"observedGeneration"}, "1", false, "current-generation"},
		{"future-generation", []string{"observedGeneration"}, int64(2), false, "current-generation"},
		{"stale-generation", []string{"observedGeneration"}, int64(0), false, "current-generation"},
		{"missing-typechecking", []string{"typeChecking"}, nil, true, "missing or malformed type checking"},
		{"null-typechecking", []string{"typeChecking"}, nil, false, "missing or malformed type checking"},
		{"string-typechecking", []string{"typeChecking"}, "checked", false, "missing or malformed type checking"},
		{"array-typechecking", []string{"typeChecking"}, []interface{}{}, false, "missing or malformed type checking"},
		{"null-warnings", []string{"typeChecking", "expressionWarnings"}, nil, false, "expression warnings"},
		{"string-warnings", []string{"typeChecking", "expressionWarnings"}, "none", false, "expression warnings"},
		{"warning", []string{"typeChecking", "expressionWarnings"}, []interface{}{map[string]interface{}{"warning": "bad CEL"}}, false, "expression warnings"},
		{"null-conditions", []string{"conditions"}, nil, false, "malformed conditions"},
		{"map-conditions", []string{"conditions"}, map[string]interface{}{}, false, "malformed conditions"},
		{"null-condition", []string{"conditions"}, []interface{}{nil}, false, "unsuccessful condition"},
		{"string-condition", []string{"conditions"}, []interface{}{"ready"}, false, "cannot unmarshal"},
		{"duplicate-condition", []string{"conditions"}, []interface{}{validCondition, validCondition}, false, "unsuccessful condition"},
	}
	for _, field := range []string{"type", "status", "observedGeneration", "lastTransitionTime", "reason"} {
		condition := (&unstructured.Unstructured{Object: validCondition}).DeepCopy().Object
		delete(condition, field)
		scenarios = append(scenarios, statusScenario{"missing-condition-" + field, []string{"conditions"}, []interface{}{condition}, false, "unsuccessful condition"})
	}
	for _, status := range []string{"False", "Unknown"} {
		condition := (&unstructured.Unstructured{Object: validCondition}).DeepCopy().Object
		condition["status"] = status
		scenarios = append(scenarios, statusScenario{"condition-" + status, []string{"conditions"}, []interface{}{condition}, false, "unsuccessful condition"})
	}
	for _, mutation := range []struct {
		name, field, message string
		value                interface{}
	}{
		{"stale", "observedGeneration", "stale or unsuccessful", int64(0)},
		{"future", "observedGeneration", "stale or unsuccessful", int64(2)},
		{"generation-type", "observedGeneration", "cannot unmarshal", "1"},
		{"status-type", "status", "cannot unmarshal", true},
		{"invalid-time", "lastTransitionTime", "parsing time", "yesterday"},
		{"reason-type", "reason", "cannot unmarshal", int64(1)},
	} {
		condition := (&unstructured.Unstructured{Object: validCondition}).DeepCopy().Object
		condition[mutation.field] = mutation.value
		scenarios = append(scenarios, statusScenario{"condition-" + mutation.name, []string{"conditions"}, []interface{}{condition}, false, mutation.message})
	}
	for _, scenario := range scenarios {
		test.Run(scenario.name, func(test *testing.T) {
			updates := 0
			admin := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, target client.Object, options ...client.GetOption) error {
					object, unstructuredTarget := target.(*unstructured.Unstructured)
					if !unstructuredTarget {
						return underlying.Get(ctx, key, target, options...)
					}
					for _, item := range stored {
						original := item.(*unstructured.Unstructured)
						if original.GroupVersionKind() == object.GroupVersionKind() && client.ObjectKeyFromObject(original) == key {
							object.Object = original.DeepCopy().Object
							if object.GetKind() == "ValidatingAdmissionPolicy" && scenario.path != nil {
								path := append([]string{"status"}, scenario.path...)
								if scenario.remove {
									unstructured.RemoveNestedField(object.Object, path...)
								} else if err := unstructured.SetNestedField(object.Object, scenario.value, path...); err != nil {
									return err
								}
							}
							return nil
						}
					}
					test.Fatalf("unexpected object read: %s %v", object.GetKind(), key)
					return nil
				},
				Update: func(_ context.Context, _ client.WithWatch, _ client.Object, options ...client.UpdateOption) error {
					settings := &client.UpdateOptions{}
					for _, option := range options {
						option.ApplyToUpdate(settings)
					}
					if !reflect.DeepEqual(settings.DryRun, []string{"All"}) {
						test.Fatal("non-dry-run update requested")
					}
					updates++
					return nil
				},
			})
			err := activation.Verify(context.Background(), admin, manifest)
			if scenario.message == "" {
				if err != nil {
					test.Fatal(err)
				}
				if updates != len(manifest.Objects) {
					test.Fatalf("verified %d objects, expected %d", updates, len(manifest.Objects))
				}
			} else if err == nil || !strings.Contains(err.Error(), scenario.message) {
				test.Fatalf("expected %q, got %v", scenario.message, err)
			}
		})
	}
}
