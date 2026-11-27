// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/callhealth"
	"github.com/kognition-ai/kognition/internal/store"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func modelCallHealthAdmissionClient(test *testing.T, config *rest.Config, scheme *runtime.Scheme, component string) client.Client {
	test.Helper()
	credential := rest.CopyConfig(config)
	credential.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:control:" + authorityProductionRelease + "-" + component,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"},
	}
	writer, err := client.New(credential, client.Options{Scheme: scheme})
	if err != nil {
		test.Fatal(err)
	}
	return writer
}

func modelCallHealthAdmissionObject(component string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kognition-mch-" + component + "-" + strings.Repeat("a", 32),
			Namespace: "control",
			Labels:    map[string]string{callhealth.HealthLabel: component},
		},
		Data: map[string]string{callhealth.DataKey: "{}"},
	}
}

func seedModelCallHealthNameFixtures(test *testing.T, ctx context.Context, admin client.Client) []corev1.ConfigMap {
	test.Helper()
	var fixtures []corev1.ConfigMap
	for _, component := range []string{"controller", "model-gateway", "knowledge-gateway"} {
		prefix := "kognition-mch-" + component + "-"
		for _, name := range []string{
			prefix + "arbitrary",
			prefix + strings.Repeat("b", 31),
			prefix + strings.Repeat("b", 33),
			prefix + strings.Repeat("g", 32),
			"kognition-mch-unknown-" + component + "-" + strings.Repeat("b", 32),
			"unrelated-" + component,
		} {
			value := modelCallHealthAdmissionObject(component)
			value.Name = name
			if err := admin.Create(ctx, value); err != nil {
				test.Fatal(err)
			}
			fixtures = append(fixtures, *value)
		}
	}
	return fixtures
}

func verifyModelCallHealthConfigMapBoundary(test *testing.T, ctx context.Context, admin client.Client, config *rest.Config, scheme *runtime.Scheme, nameFixtures []corev1.ConfigMap) {
	test.Helper()
	controller := modelCallHealthAdmissionClient(test, config, scheme, "controller")
	gateway := modelCallHealthAdmissionClient(test, config, scheme, "model-gateway")
	knowledgeGateway := modelCallHealthAdmissionClient(test, config, scheme, "knowledge-gateway")
	toolGateway := modelCallHealthAdmissionClient(test, config, scheme, "tool-gateway")
	for _, component := range []string{"controller", "model-gateway", "knowledge-gateway", "tool-gateway"} {
		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "model-call-health-fixture-" + component, Namespace: "control"},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "create", "update", "delete"}}},
		}
		binding := &rbacv1.RoleBinding{
			ObjectMeta: role.ObjectMeta,
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: authorityProductionRelease + "-" + component, Namespace: "control"}},
		}
		for _, object := range []client.Object{role, binding} {
			if err := admin.Create(ctx, object); err != nil {
				test.Fatal(err)
			}
			uid := object.GetUID()
			test.Cleanup(func() {
				if err := admin.Delete(context.Background(), object, client.Preconditions{UID: &uid}); err != nil {
					test.Errorf("remove exact health admission grant: %v", err)
				}
			})
		}
		for _, verb := range []string{"get", "list", "create", "update", "delete"} {
			if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
				review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
					User:   "system:serviceaccount:control:" + authorityProductionRelease + "-" + component,
					Groups: []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"},
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Namespace: "control", Verb: verb, Resource: "configmaps",
					},
				}}
				if err := admin.Create(ctx, review); err != nil {
					return false, err
				}
				return review.Status.Allowed, nil
			}); err != nil {
				test.Fatalf("fixture %s %s permission not ready: %v", component, verb, err)
			}
		}
	}
	requireAdmissionDenial := func(test *testing.T, err error) {
		test.Helper()
		if !isControllerPolicyDenial(err) {
			test.Fatalf("want chart admission denial, got %v", err)
		}
	}
	requireHealthDenial := func(test *testing.T, err error) {
		test.Helper()
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "ValidatingAdmissionPolicy '"+authorityProductionRelease+"-control-model-call-health'") {
			test.Fatalf("want dedicated model call health admission denial, got %v", err)
		}
	}
	for _, component := range []string{"controller", "model-gateway", "knowledge-gateway"} {
		value := modelCallHealthAdmissionObject(component)
		if err := controller.Create(ctx, value); err != nil {
			test.Fatal(err)
		}
		test.Cleanup(func() {
			if err := controller.Delete(context.Background(), value, client.Preconditions{UID: &value.UID}); err != nil {
				test.Errorf("remove exact health fixture: %v", err)
			}
		})
		for _, writer := range []struct {
			name   string
			client client.Client
		}{{"controller", controller}, {"model-gateway", gateway}, {"knowledge-gateway", knowledgeGateway}} {
			if writer.name != "controller" && writer.name != component {
				test.Run(writer.name+"/cannot-write/"+component, func(test *testing.T) {
					created := value.DeepCopy()
					created.ResourceVersion, created.UID = "", ""
					requireAdmissionDenial(test, writer.client.Create(ctx, created, client.DryRunAll))
					requireAdmissionDenial(test, writer.client.Update(ctx, value.DeepCopy(), client.DryRunAll))
				})
				continue
			}
			test.Run(writer.name+"/"+component, func(test *testing.T) {
				for _, size := range []int{2, 921600} {
					created := value.DeepCopy()
					created.ResourceVersion, created.UID = "", ""
					created.Data[callhealth.DataKey] = strings.Repeat("x", size)
					created.Name = strings.Replace(created.Name, strings.Repeat("a", 32), strings.Repeat("c", 32), 1)
					if err := writer.client.Create(ctx, created, client.DryRunAll); err != nil {
						test.Fatalf("CREATE %d characters: %v", size, err)
					}
					updated := value.DeepCopy()
					updated.Data[callhealth.DataKey] = created.Data[callhealth.DataKey]
					if err := writer.client.Update(ctx, updated, client.DryRunAll); err != nil {
						test.Fatalf("UPDATE %d characters: %v", size, err)
					}
				}
				for name, mutate := range map[string]func(*corev1.ConfigMap){
					"missing-labels": func(value *corev1.ConfigMap) { value.Labels = nil },
					"wrong-label": func(value *corev1.ConfigMap) {
						value.Labels = map[string]string{"unrelated": component}
					},
					"wrong-component": func(value *corev1.ConfigMap) { value.Labels[callhealth.HealthLabel] = "unknown" },
					"component-mismatch": func(value *corev1.ConfigMap) {
						if component == "controller" {
							value.Labels[callhealth.HealthLabel] = "model-gateway"
						} else {
							value.Labels[callhealth.HealthLabel] = "controller"
						}
					},
					"extra-label": func(value *corev1.ConfigMap) { value.Labels["unrelated"] = "true" },
					"annotation":  func(value *corev1.ConfigMap) { value.Annotations = map[string]string{"unrelated": "true"} },
					"owner": func(value *corev1.ConfigMap) {
						value.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "unrelated", UID: "unrelated"}}
					},
					"finalizer":    func(value *corev1.ConfigMap) { value.Finalizers = []string{"unrelated.example/retain"} },
					"binary-data":  func(value *corev1.ConfigMap) { value.BinaryData = map[string][]byte{"unrelated": {1}} },
					"missing-data": func(value *corev1.ConfigMap) { value.Data = nil },
					"empty-data":   func(value *corev1.ConfigMap) { value.Data[callhealth.DataKey] = "" },
					"extra-data":   func(value *corev1.ConfigMap) { value.Data["unrelated"] = "{}" },
					"wrong-data":   func(value *corev1.ConfigMap) { value.Data = map[string]string{"unrelated": "{}"} },
					"immutable":    func(value *corev1.ConfigMap) { value.Immutable = ptr.To(true) },
					"oversized":    func(value *corev1.ConfigMap) { value.Data[callhealth.DataKey] = strings.Repeat("x", 921601) },
				} {
					test.Run(name, func(test *testing.T) {
						requireDenial := requireHealthDenial
						// The state policy rejects missing ownership labels before the shape policy.
						if name == "missing-labels" || name == "wrong-label" {
							requireDenial = requireAdmissionDenial
						}
						updated := value.DeepCopy()
						mutate(updated)
						requireDenial(test, writer.client.Update(ctx, updated, client.DryRunAll))
						created := updated.DeepCopy()
						created.ResourceVersion, created.UID = "", ""
						requireDenial(test, writer.client.Create(ctx, created, client.DryRunAll))
					})
				}
				for _, malformed := range nameFixtures {
					if malformed.Labels[callhealth.HealthLabel] != component {
						continue
					}
					test.Run(malformed.Name, func(test *testing.T) {
						requireAdmissionDenial(test, writer.client.Update(ctx, malformed.DeepCopy(), client.DryRunAll))
						created := malformed.DeepCopy()
						created.ResourceVersion, created.UID = "", ""
						requireAdmissionDenial(test, writer.client.Create(ctx, created, client.DryRunAll))
						repaired := malformed.DeepCopy()
						repaired.Labels = nil
						requireAdmissionDenial(test, writer.client.Update(ctx, repaired, client.DryRunAll))
					})
				}
			})
		}
		test.Run("delete/"+component, func(test *testing.T) {
			if err := controller.Delete(ctx, value.DeepCopy(), client.DryRunAll); err != nil {
				test.Fatalf("controller DELETE: %v", err)
			}
			requireAdmissionDenial(test, gateway.Delete(ctx, value.DeepCopy(), client.DryRunAll))
			requireAdmissionDenial(test, knowledgeGateway.Delete(ctx, value.DeepCopy(), client.DryRunAll))
			requireAdmissionDenial(test, toolGateway.Delete(ctx, value.DeepCopy(), client.DryRunAll))
		})
		test.Run("foreign-writer/"+component, func(test *testing.T) {
			created := value.DeepCopy()
			created.ResourceVersion, created.UID = "", ""
			requireAdmissionDenial(test, toolGateway.Create(ctx, created, client.DryRunAll))
			requireAdmissionDenial(test, toolGateway.Update(ctx, value.DeepCopy(), client.DryRunAll))
		})
	}
	test.Run("knowledge-reporter-store", func(test *testing.T) {
		sink, err := callhealth.NewConfigMapStore(knowledgeGateway, "control", "knowledge-gateway")
		if err != nil {
			test.Fatal(err)
		}
		const reporter = "knowledge-admission-reporter"
		if err := sink.ReplaceModelCallHealth(ctx, reporter, []store.ModelCallHealth{{Target: "t", Calls: 1}}); err != nil {
			test.Fatalf("create reporter publication: %v", err)
		}
		test.Cleanup(func() {
			var objects corev1.ConfigMapList
			if err := controller.List(context.Background(), &objects, client.InNamespace("control"), client.MatchingLabels{callhealth.HealthLabel: "knowledge-gateway"}); err != nil {
				test.Errorf("list exact reporter for cleanup: %v", err)
				return
			}
			for i := range objects.Items {
				object := &objects.Items[i]
				if !strings.Contains(object.Data[callhealth.DataKey], reporter) {
					continue
				}
				if err := controller.Delete(context.Background(), object, client.Preconditions{UID: &object.UID}); err != nil {
					test.Errorf("remove exact reporter: %v", err)
				}
			}
		})
		records := make([]store.ModelCallHealth, store.MaxModelCallHealthTargets)
		for i := range records {
			records[i] = store.ModelCallHealth{Target: fmt.Sprintf("%064x", i), Calls: 1, LastSuccessAt: time.Now().UTC()}
		}
		if err := sink.ReplaceModelCallHealth(ctx, reporter, records); err != nil {
			test.Fatalf("update maximum-size reporter publication: %v", err)
		}
		got, err := sink.ListModelCallHealth(ctx, time.Minute)
		if err != nil || len(got) != len(records) {
			test.Fatalf("read reporter publication: records=%d error=%v, want %d", len(got), err, len(records))
		}
	})
}
