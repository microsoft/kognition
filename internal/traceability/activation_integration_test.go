// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestActivationAPIDefaultingAndDrift(test *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		test.Skip("KUBEBUILDER_ASSETS is not set")
	}
	environment := &envtest.Environment{}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	config, err := environment.Start()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := stopControllerEnvtest(environment); err != nil {
			test.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	admin, err := client.NewWithWatch(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		test.Fatal(err)
	}
	for _, name := range []string{"system:discovery", "system:basic-user", "system:public-info-viewer", "system:service-account-issuer-discovery"} {
		role := &rbacv1.ClusterRole{}
		if err := admin.Get(ctx, client.ObjectKey{Name: name}, role); err != nil {
			test.Fatal(err)
		}
		if len(role.Rules) == 0 {
			test.Fatalf("default role %s has no rules", name)
		}
		test.Logf("Default role %s rules: %+v", name, role.Rules)
	}
	for _, namespace := range []string{"control", "tenant"} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			test.Fatal(err)
		}
	}
	var rendered strings.Builder
	var controller *unstructured.Unstructured
	for _, document := range renderControllerChart(test, "--set", "k8sController.managedNamespaces[0]=tenant") {
		rendered.WriteString("---\n")
		rendered.Write(document.Raw)
		rendered.WriteString("\n")
		object := &unstructured.Unstructured{}
		if err := object.UnmarshalJSON(document.Raw); err != nil {
			test.Fatal(err)
		}
		component, _, _ := unstructured.NestedString(object.Object, "spec", "template", "metadata", "labels", "app.kubernetes.io/component")
		if object.GetKind() == "Deployment" && component == "k8s-controller" {
			controller = object
		}
	}
	manifest, err := activation.Parse(strings.NewReader(rendered.String()))
	if err != nil {
		test.Fatal(err)
	}
	if controller == nil {
		test.Fatal("controller missing")
	}
	test.Run("fresh-drain-with-default-grants", func(test *testing.T) {
		if err := activation.VerifyDrained(ctx, admin, manifest); err != nil {
			test.Fatal(err)
		}
	})
	certificateServer := httptest.NewTLSServer(nil)
	certificate := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateServer.Certificate().Raw}))
	certificateServer.Close()
	for _, expected := range append([]*unstructured.Unstructured{controller}, manifest.Objects...) {
		live := expected.DeepCopy()
		if live.GetKind() == "ValidatingWebhookConfiguration" {
			webhooks, _, _ := unstructured.NestedSlice(live.Object, "webhooks")
			for _, webhook := range webhooks {
				if err := unstructured.SetNestedField(webhook.(map[string]interface{}), certificate, "clientConfig", "caBundle"); err != nil {
					test.Fatal(err)
				}
			}
			if err := unstructured.SetNestedSlice(live.Object, webhooks, "webhooks"); err != nil {
				test.Fatal(err)
			}
		}
		if err := admin.Create(ctx, live); err != nil {
			test.Fatalf("create %s %s: %v", live.GetKind(), live.GetName(), err)
		}
		switch live.GetKind() {
		case "Deployment":
			replicas, _, _ := unstructured.NestedInt64(live.Object, "spec", "replicas")
			live.Object["status"] = map[string]interface{}{"observedGeneration": live.GetGeneration(), "replicas": replicas, "updatedReplicas": replicas, "readyReplicas": replicas, "availableReplicas": replicas}
		case "ValidatingAdmissionPolicy":
			live.Object["status"] = map[string]interface{}{"observedGeneration": live.GetGeneration(), "typeChecking": map[string]interface{}{}}
		default:
			continue
		}
		if err := admin.Status().Update(ctx, live); err != nil {
			test.Fatal(err)
		}
	}
	test.Log("Rollout and type-check status are simulated; API defaulting and persistence are real.")
	snapshot := func() []*unstructured.Unstructured {
		test.Helper()
		var objects []*unstructured.Unstructured
		for _, expected := range append([]*unstructured.Unstructured{controller}, manifest.Objects...) {
			object := &unstructured.Unstructured{}
			object.SetGroupVersionKind(expected.GroupVersionKind())
			if err := admin.Get(ctx, client.ObjectKeyFromObject(expected), object); err != nil {
				test.Fatal(err)
			}
			objects = append(objects, object)
		}
		return objects
	}
	before := snapshot()
	if err := activation.Verify(ctx, admin, manifest); err != nil {
		test.Fatalf("defaulted bootstrap: %v", err)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		test.Fatal("successful verification changed bootstrap objects or resource versions")
	}
	test.Run("six-policy-behavior-and-cleanup", func(test *testing.T) {
		results, err := activation.VerifyPolicyBehavior(ctx, config, manifest)
		if err != nil {
			test.Fatalf("behavioral preflight: %v; results=%+v", err, results)
		}
		if len(results) != 6 {
			test.Fatalf("expected six paired probes: %+v", results)
		}
		for _, result := range results {
			if !result.Allowed || !result.Denied || result.UID == "" || !result.CleanupConfirmed {
				test.Fatalf("incomplete probe: %+v", result)
			}
		}
		if !reflect.DeepEqual(before, snapshot()) {
			test.Fatal("behavioral preflight changed bootstrap objects")
		}
	})
	test.Run("lost-create-response-cleanup", func(test *testing.T) {
		lost := false
		fault := rest.CopyConfig(config)
		fault.Wrap(func(transport http.RoundTripper) http.RoundTripper {
			return activationRoundTripper(func(request *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(request)
				if !lost && err == nil && response.StatusCode == http.StatusCreated && request.Method == http.MethodPost && request.URL.Query().Get("dryRun") == "" && strings.HasSuffix(request.URL.Path, "/configmaps") {
					lost = true
					response.Body.Close()
					return nil, errors.New("injected lost Create response")
				}
				return response, err
			})
		})
		if _, err := activation.VerifyPolicyBehavior(ctx, fault, manifest); !lost || err == nil || !strings.Contains(err.Error(), "recovered owned UID") {
			test.Fatalf("lost response must fail with owned cleanup: lost=%t error=%v", lost, err)
		}
		for _, resource := range []struct{ apiVersion, kind string }{{"v1", "ConfigMapList"}, {"rbac.authorization.k8s.io/v1", "RoleList"}, {"rbac.authorization.k8s.io/v1", "RoleBindingList"}, {"admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicyList"}, {"admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicyBindingList"}} {
			objects := &unstructured.UnstructuredList{}
			objects.SetAPIVersion(resource.apiVersion)
			objects.SetKind(resource.kind)
			if err := admin.List(ctx, objects); err != nil {
				test.Fatal(err)
			}
			for _, object := range objects.Items {
				if object.GetAnnotations()["kognition.dev/admission-probe-owner"] != "" {
					test.Fatalf("probe leaked after lost response: %s %s", object.GetKind(), object.GetName())
				}
			}
		}
		if !reflect.DeepEqual(before, snapshot()) {
			test.Fatal("lost response changed bootstrap objects")
		}
	})
	test.Run("missing-live-binding", func(test *testing.T) {
		for _, object := range before {
			if object.GetKind() != "ValidatingAdmissionPolicyBinding" {
				continue
			}
			if err := admin.Delete(ctx, object); err != nil {
				test.Fatal(err)
			}
			defer func() {
				restored := object.DeepCopy()
				restored.SetResourceVersion("")
				restored.SetUID("")
				if err := admin.Create(ctx, restored); err != nil {
					test.Error(err)
				}
			}()
			if _, err := activation.VerifyPolicyBehavior(ctx, config, manifest); !apierrors.IsNotFound(err) {
				test.Fatalf("missing binding accepted: %v", err)
			}
			return
		}
		test.Fatal("binding fixture missing")
	})
	test.Run("drained-upgrade-replicaset", func(test *testing.T) {
		current := &appsv1.Deployment{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(controller), current); err != nil {
			test.Fatal(err)
		}
		labels := map[string]string{"test": "drained-upgrade"}
		replicas := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "drain-replicas", Namespace: manifest.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: current.Name, UID: current.UID, Controller: ptr.To(true)}}},
			Spec:       appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{DeprecatedServiceAccount: manifest.Controller, Containers: []corev1.Container{{Name: "fixture", Image: "fixture:local"}}}}},
		}
		if err := admin.Create(ctx, replicas); err != nil {
			test.Fatal(err)
		}
		test.Cleanup(func() {
			if err := admin.Delete(ctx, replicas); client.IgnoreNotFound(err) != nil {
				test.Error(err)
			}
		})
		if replicas.Spec.Replicas == nil || *replicas.Spec.Replicas != 1 || replicas.Spec.Template.Spec.ServiceAccountName != manifest.Controller {
			test.Fatal("expected API replica and service-account alias defaulting")
		}
		if err := activation.VerifyDrained(ctx, admin, manifest); err == nil {
			test.Fatal("accepted defaulted active ReplicaSet")
		}
		replicas.Spec.Replicas = ptr.To(int32(0))
		if err := admin.Update(ctx, replicas); err != nil {
			test.Fatal(err)
		}
		if err := activation.VerifyDrained(ctx, admin, manifest); err == nil {
			test.Fatal("accepted unobserved stopped ReplicaSet")
		}
		replicas.Status = appsv1.ReplicaSetStatus{ObservedGeneration: replicas.Generation}
		if err := admin.Status().Update(ctx, replicas); err != nil {
			test.Fatal(err)
		}
		before := replicas.DeepCopy()
		if err := activation.VerifyDrained(ctx, admin, manifest); err != nil {
			test.Fatalf("stopped current ReplicaSet: %v", err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(replicas), replicas); err != nil {
			test.Fatal(err)
		}
		if !reflect.DeepEqual(before, replicas) {
			test.Fatal("drain check mutated ReplicaSet")
		}
		replicas.OwnerReferences[0].UID = "foreign-uid"
		if err := admin.Update(ctx, replicas); err != nil {
			test.Fatal(err)
		}
		if err := activation.VerifyDrained(ctx, admin, manifest); err == nil {
			test.Fatal("accepted foreign ReplicaSet owner UID")
		}
	})
	for _, scenario := range []struct {
		name    string
		kind    string
		message string
		status  bool
		mutate  func(*unstructured.Unstructured)
	}{
		{name: "ignore-live-policy", kind: "ValidatingAdmissionPolicy", message: "(spec)", mutate: func(object *unstructured.Unstructured) {
			object.Object["spec"].(map[string]interface{})["failurePolicy"] = "Ignore"
		}},
		{name: "weakened-live-binding", kind: "ValidatingAdmissionPolicyBinding", message: "(spec)", mutate: func(object *unstructured.Unstructured) {
			object.Object["spec"].(map[string]interface{})["validationActions"] = []interface{}{"Warn"}
		}},
		{name: "wrong-live-controller", kind: "ValidatingAdmissionPolicy", message: "(spec)", mutate: func(object *unstructured.Unstructured) {
			object.Object["spec"].(map[string]interface{})["matchConditions"] = []interface{}{map[string]interface{}{"name": "wrong-controller", "expression": "request.userInfo.username == 'system:serviceaccount:control:other'"}}
		}},
		{name: "missing-type-check", kind: "ValidatingAdmissionPolicy", message: "missing or malformed type checking", status: true, mutate: func(object *unstructured.Unstructured) {
			unstructured.RemoveNestedField(object.Object, "status", "typeChecking")
		}},
		{name: "stale-type-check", kind: "ValidatingAdmissionPolicy", message: "current-generation type checking", status: true, mutate: func(object *unstructured.Unstructured) {
			object.Object["status"].(map[string]interface{})["observedGeneration"] = int64(0)
		}},
		{name: "expression-warning", kind: "ValidatingAdmissionPolicy", message: "expression warnings", status: true, mutate: func(object *unstructured.Unstructured) {
			object.Object["status"].(map[string]interface{})["typeChecking"] = map[string]interface{}{"expressionWarnings": []interface{}{map[string]interface{}{"fieldRef": "spec.validations[0].expression", "warning": "test warning"}}}
		}},
		{name: "overprivileged-guard", kind: "Role", message: "(rules)", mutate: func(object *unstructured.Unstructured) {
			object.Object["rules"] = []interface{}{map[string]interface{}{"apiGroups": []interface{}{""}, "resources": []interface{}{"secrets"}, "verbs": []interface{}{"get"}}}
		}},
		{name: "baseline-owner", kind: "NetworkPolicy", message: "ownership drift", mutate: func(object *unstructured.Unstructured) {
			object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "untrusted", UID: "untrusted"}})
		}},
		{name: "baseline-egress", kind: "NetworkPolicy", message: "(spec)", mutate: func(object *unstructured.Unstructured) {
			object.Object["spec"].(map[string]interface{})["egress"] = []interface{}{map[string]interface{}{}}
		}},
		{name: "guard-not-ready", kind: "Deployment", message: "rollout is not ready", status: true, mutate: func(object *unstructured.Unstructured) {
			object.Object["status"].(map[string]interface{})["availableReplicas"] = int64(0)
		}},
		{name: "guard-image-drift", kind: "Deployment", message: "(spec)", mutate: func(object *unstructured.Unstructured) {
			containers, _, _ := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
			containers[0].(map[string]interface{})["image"] = "untrusted:changed"
			if err := unstructured.SetNestedSlice(object.Object, containers, "spec", "template", "spec", "containers"); err != nil {
				test.Fatal(err)
			}
		}},
		{name: "service-selector-drift", kind: "Service", message: "(spec)", mutate: func(object *unstructured.Unstructured) {
			if err := unstructured.SetNestedStringMap(object.Object, map[string]string{"untrusted": "true"}, "spec", "selector"); err != nil {
				test.Fatal(err)
			}
		}},
		{name: "wrong-guard-subject", kind: "RoleBinding", message: "(subjects)", mutate: func(object *unstructured.Unstructured) {
			object.Object["subjects"] = []interface{}{map[string]interface{}{"kind": "ServiceAccount", "name": "untrusted", "namespace": "control"}}
		}},
		{name: "missing-ca", kind: "ValidatingWebhookConfiguration", message: "no valid serving CA", mutate: func(object *unstructured.Unstructured) {
			webhooks := object.Object["webhooks"].([]interface{})
			unstructured.RemoveNestedField(webhooks[0].(map[string]interface{}), "clientConfig", "caBundle")
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if err := activation.Verify(ctx, admin, manifest); err != nil {
				test.Fatalf("dirty starting fixture: %v", err)
			}
			var original *unstructured.Unstructured
			for _, expected := range manifest.Objects {
				if expected.GetKind() != scenario.kind {
					continue
				}
				original = &unstructured.Unstructured{}
				original.SetGroupVersionKind(expected.GroupVersionKind())
				if err := admin.Get(ctx, client.ObjectKeyFromObject(expected), original); err != nil {
					test.Fatal(err)
				}
				break
			}
			if original == nil {
				test.Fatal("fixture object missing")
			}
			changed := original.DeepCopy()
			scenario.mutate(changed)
			update := func(object *unstructured.Unstructured) error {
				if scenario.status {
					return admin.Status().Update(ctx, object)
				}
				return admin.Update(ctx, object)
			}
			if err := update(changed); err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() {
				original.SetResourceVersion(changed.GetResourceVersion())
				if err := update(original); err != nil {
					test.Error(err)
					return
				}
				if !scenario.status && (scenario.kind == "Deployment" || scenario.kind == "ValidatingAdmissionPolicy") {
					if err := unstructured.SetNestedField(original.Object, original.GetGeneration(), "status", "observedGeneration"); err != nil {
						test.Error(err)
						return
					}
					if err := admin.Status().Update(ctx, original); err != nil {
						test.Error(err)
						return
					}
				}
				if err := activation.Verify(ctx, admin, manifest); err != nil {
					test.Errorf("restored fixture rejected: %v", err)
				}
			})
			if err := activation.Verify(ctx, admin, manifest); err == nil || !strings.Contains(err.Error(), scenario.message) {
				test.Fatalf("expected %q, got %v", scenario.message, err)
			}
			persisted := &unstructured.Unstructured{}
			persisted.SetGroupVersionKind(changed.GroupVersionKind())
			if err := admin.Get(ctx, client.ObjectKeyFromObject(changed), persisted); err != nil {
				test.Fatal(err)
			}
			if !reflect.DeepEqual(changed.Object, persisted.Object) {
				test.Fatal("verification persisted a repair")
			}
		})
	}
	for _, scenario := range []struct {
		name    string
		failure error
	}{
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, manifest.Guard, errors.New("fixture denied"))},
		{"missing", apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, manifest.Guard)},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			failing := interceptor.NewClient(admin, interceptor.Funcs{Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
				if key.Name == manifest.Guard {
					return scenario.failure
				}
				return underlying.Get(ctx, key, object, options...)
			}})
			if err := activation.Verify(ctx, failing, manifest); !errors.Is(err, scenario.failure) {
				test.Fatalf("expected read failure, got %v", err)
			}
		})
	}
	for _, deadline := range []bool{false, true} {
		name := "cancellation"
		cancelled, stop := context.WithCancel(ctx)
		expected := context.Canceled
		if deadline {
			stop()
			cancelled, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
			expected = context.DeadlineExceeded
			name = "timeout"
		}
		stop()
		test.Run(name, func(test *testing.T) {
			if err := activation.Verify(cancelled, admin, manifest); !errors.Is(err, expected) {
				test.Fatalf("expected %v, got %v", expected, err)
			}
		})
	}
	test.Run("normalization-preserves-explicit-values", func(test *testing.T) {
		objects := copyActivationDocuments(append([]*unstructured.Unstructured{controller}, manifest.Objects...))
		service := activationDocument(test, objects, "Service")
		currentService := service.DeepCopy()
		if err := admin.Get(ctx, client.ObjectKeyFromObject(service), currentService); err != nil {
			test.Fatal(err)
		}
		for _, field := range []string{"clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy"} {
			value, found, err := unstructured.NestedFieldCopy(currentService.Object, "spec", field)
			if err != nil || !found {
				test.Fatalf("missing allocated %s: %v", field, err)
			}
			if err := unstructured.SetNestedField(service.Object, value, "spec", field); err != nil {
				test.Fatal(err)
			}
		}
		configuration := activationDocument(test, objects, "ValidatingWebhookConfiguration")
		for _, webhook := range configuration.Object["webhooks"].([]interface{}) {
			webhook.(map[string]interface{})["clientConfig"].(map[string]interface{})["caBundle"] = certificate
		}
		explicit, err := activation.Parse(strings.NewReader(encodeActivationDocuments(test, objects)))
		if err != nil {
			test.Fatal(err)
		}
		if err := activation.Verify(ctx, admin, explicit); err != nil {
			test.Fatalf("matching explicit allocations and CA: %v", err)
		}
		for _, scenario := range []struct {
			field string
			value interface{}
		}{
			{"clusterIP", "10.96.0.254"},
			{"clusterIPs", []interface{}{"10.96.0.254"}},
			{"ipFamilies", []interface{}{"IPv6"}},
			{"ipFamilyPolicy", "RequireDualStack"},
			{"caBundle", certificate + certificate},
		} {
			test.Run(scenario.field, func(test *testing.T) {
				changed := copyActivationDocuments(objects)
				if scenario.field == "caBundle" {
					decoded, err := base64.StdEncoding.DecodeString(certificate)
					if err != nil {
						test.Fatal(err)
					}
					scenario.value = base64.StdEncoding.EncodeToString(append(decoded, decoded...))
					webhook := activationDocument(test, changed, "ValidatingWebhookConfiguration").Object["webhooks"].([]interface{})[0].(map[string]interface{})
					webhook["clientConfig"].(map[string]interface{})["caBundle"] = scenario.value
				} else if err := unstructured.SetNestedField(activationDocument(test, changed, "Service").Object, scenario.value, "spec", scenario.field); err != nil {
					test.Fatal(err)
				}
				wanted, err := activation.Parse(strings.NewReader(encodeActivationDocuments(test, changed)))
				if err != nil {
					test.Fatal(err)
				}
				checked := false
				observing := interceptor.NewClient(admin, interceptor.Funcs{Update: func(ctx context.Context, underlying client.WithWatch, target client.Object, options ...client.UpdateOption) error {
					object := target.(*unstructured.Unstructured)
					var value interface{}
					if scenario.field == "caBundle" && object.GetKind() == "ValidatingWebhookConfiguration" {
						value = object.Object["webhooks"].([]interface{})[0].(map[string]interface{})["clientConfig"].(map[string]interface{})["caBundle"]
						checked = true
					} else if scenario.field != "caBundle" && object.GetKind() == "Service" {
						value, _, _ = unstructured.NestedFieldNoCopy(object.Object, "spec", scenario.field)
						checked = true
					}
					if checked && value != nil && !reflect.DeepEqual(value, scenario.value) {
						test.Fatal("explicit desired value was overwritten")
					}
					return underlying.Update(ctx, target, options...)
				}})
				before := snapshot()
				err = activation.Verify(ctx, observing, wanted)
				if !checked || err == nil || !(strings.Contains(err.Error(), "normalize Service") || strings.Contains(err.Error(), "(spec)") || strings.Contains(err.Error(), "(webhooks)")) {
					test.Fatalf("explicit %s bypassed: checked=%t, err=%v", scenario.field, checked, err)
				}
				if !reflect.DeepEqual(before, snapshot()) {
					test.Fatal("failed verification persisted changes")
				}
				if err := activation.Verify(ctx, admin, manifest); err != nil {
					test.Fatalf("unchanged bootstrap rejected: %v", err)
				}
			})
		}
	})
	test.Run("baseline-deleting-and-deleted", func(test *testing.T) {
		expected := activationDocument(test, manifest.Objects, "NetworkPolicy")
		live := expected.DeepCopy()
		if err := admin.Get(ctx, client.ObjectKeyFromObject(expected), live); err != nil {
			test.Fatal(err)
		}
		live.SetFinalizers([]string{"kognition.dev/test-retain"})
		if err := admin.Update(ctx, live); err != nil {
			test.Fatal(err)
		}
		test.Cleanup(func() {
			current := expected.DeepCopy()
			if err := admin.Get(ctx, client.ObjectKeyFromObject(expected), current); err == nil {
				current.SetFinalizers(nil)
				if err := admin.Update(ctx, current); err != nil {
					test.Error(err)
					return
				}
			} else if !apierrors.IsNotFound(err) {
				test.Error(err)
				return
			}
			if err := admin.Create(ctx, expected.DeepCopy()); err != nil {
				test.Error(err)
				return
			}
			if err := activation.Verify(ctx, admin, manifest); err != nil {
				test.Errorf("restored deleted baseline: %v", err)
			}
		})
		if err := admin.Delete(ctx, live); err != nil {
			test.Fatal(err)
		}
		if err := activation.Verify(ctx, admin, manifest); err == nil || !strings.Contains(err.Error(), "is deleting") {
			test.Fatalf("expected deleting rejection, got %v", err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(expected), live); err != nil {
			test.Fatal(err)
		}
		live.SetFinalizers(nil)
		if err := admin.Update(ctx, live); err != nil {
			test.Fatal(err)
		}
		if err := activation.Verify(ctx, admin, manifest); !apierrors.IsNotFound(err) {
			test.Fatalf("expected missing baseline, got %v", err)
		}
	})
	test.Run("dry-run-failure", func(test *testing.T) {
		failure := errors.New("fixture dry-run failed")
		failing := interceptor.NewClient(admin, interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error { return failure }})
		if err := activation.Verify(ctx, failing, manifest); !errors.Is(err, failure) {
			test.Fatalf("expected dry-run failure, got %v", err)
		}
	})
	before = snapshot()
	if err := activation.Verify(ctx, admin, manifest); err != nil {
		test.Fatalf("final bootstrap: %v", err)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		test.Fatal("final verification changed bootstrap objects or resource versions")
	}
	current := &appsv1.Deployment{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: manifest.Namespace, Name: manifest.Deployment}, current); err != nil {
		test.Fatal(err)
	}
	if current.Spec.Replicas == nil || *current.Spec.Replicas != 0 {
		test.Fatal("verification activated controller")
	}
}

type activationRoundTripper func(*http.Request) (*http.Response, error)

func (transport activationRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
