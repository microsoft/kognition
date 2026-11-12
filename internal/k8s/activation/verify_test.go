// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestVerifyRejectsUndrainedController(test *testing.T) {
	for name, subject := range map[string]rbacv1.Subject{
		"service-account":      {Kind: "ServiceAccount", Name: "controller", Namespace: "control"},
		"user":                 {Kind: "User", Name: "system:serviceaccount:control:controller"},
		"namespace-group":      {Kind: "Group", Name: "system:serviceaccounts:control"},
		"all-service-accounts": {Kind: "Group", Name: "system:serviceaccounts"},
		"all-authenticated":    {Kind: "Group", Name: "system:authenticated"},
	} {
		test.Run(name, func(test *testing.T) {
			manifest, admin := fixture(test, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "outside"}, Subjects: []rbacv1.Subject{subject}})
			if err := VerifyDrained(context.Background(), admin, manifest); err == nil || !strings.Contains(err.Error(), "revoke controller RoleBinding outside/legacy") {
				test.Fatalf("unexpected result: %v", err)
			}
		})
	}
	for name, object := range map[string]client.Object{
		"unlabeled-workload":     &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "tenant"}},
		"deployment":             &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "tenant"}},
		"replicaset":             &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "tenant"}},
		"statefulset":            &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "tenant"}},
		"daemonset":              &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "tenant"}},
		"job":                    &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "tenant"}},
		"cronjob":                &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "tenant"}},
		"replication-controller": &corev1.ReplicationController{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "tenant"}},
		"controller-pod":         &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "control"}, Spec: corev1.PodSpec{ServiceAccountName: "controller"}},
		"legacy-workload":        &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "tenant", Labels: map[string]string{"app.kubernetes.io/managed-by": "kognition"}}},
		"legacy-reserved-name":   &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "kognition-stdio-invocation-old", Namespace: "tenant"}},
		"legacy-cluster-grant":   &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "old"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "controller", Namespace: "control"}}},
	} {
		test.Run(name, func(test *testing.T) {
			manifest, admin := fixture(test, object)
			if err := VerifyDrained(context.Background(), admin, manifest); err == nil || !strings.Contains(err.Error(), "before") && !strings.Contains(err.Error(), "still exists") {
				test.Fatal("accepted undrained installation")
			}
		})
	}
}

func TestVerifyDrainedInstallation(test *testing.T) {
	manifest, admin := fixture(test)
	if err := VerifyDrained(context.Background(), admin, manifest); err != nil {
		test.Fatal(err)
	}
	if err := Verify(context.Background(), admin, manifest); err == nil {
		test.Fatal("full verification accepted missing policies")
	}
}

func TestVerifyDrainedRejectsControllerProducer(test *testing.T) {
	manifest, admin := fixture(test, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-controller", Namespace: "control"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "controller"}}},
	})
	if err := VerifyDrained(context.Background(), admin, manifest); err == nil || !strings.Contains(err.Error(), "legacy-controller") {
		test.Fatalf("expected legacy controller producer rejection, got %v", err)
	}
}

func TestVerifyDrainedSharedRBAC(test *testing.T) {
	for _, scenario := range []struct {
		name        string
		rules       []rbacv1.PolicyRule
		aggregation *rbacv1.AggregationRule
		allowed     bool
	}{
		{name: "discovery", allowed: true, rules: []rbacv1.PolicyRule{{Verbs: []string{"get"}, NonResourceURLs: []string{"/api", "/api/*", "/apis", "/apis/*", "/openapi/*", "/version"}}}},
		{name: "self-review", allowed: true, rules: []rbacv1.PolicyRule{{Verbs: []string{"create"}, APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"}}}},
		{name: "self-identity", allowed: true, rules: []rbacv1.PolicyRule{{Verbs: []string{"create"}, APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"selfsubjectreviews"}}}},
		{name: "secret-read", rules: []rbacv1.PolicyRule{{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"secrets"}}}},
		{name: "wildcard-url", rules: []rbacv1.PolicyRule{{Verbs: []string{"get"}, NonResourceURLs: []string{"/*"}}}},
		{name: "metrics", rules: []rbacv1.PolicyRule{{Verbs: []string{"get"}, NonResourceURLs: []string{"/metrics"}}}},
		{name: "wildcard-verb", rules: []rbacv1.PolicyRule{{Verbs: []string{"*"}, NonResourceURLs: []string{"/api"}}}},
		{name: "other-review", rules: []rbacv1.PolicyRule{{Verbs: []string{"create"}, APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"subjectaccessreviews"}}}},
		{name: "aggregation", aggregation: &rbacv1.AggregationRule{ClusterRoleSelectors: []metav1.LabelSelector{{}}}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			manifest, admin := fixture(test,
				&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "system:discovery"}, Rules: scenario.rules, AggregationRule: scenario.aggregation},
				&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "system:discovery"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:discovery"}, Subjects: []rbacv1.Subject{{Kind: "Group", Name: "system:authenticated"}}},
			)
			err := VerifyDrained(context.Background(), admin, manifest)
			if scenario.allowed && err != nil || !scenario.allowed && (err == nil || !strings.Contains(err.Error(), "role rules exceed")) {
				test.Fatalf("allowed=%t, got %v", scenario.allowed, err)
			}
		})
	}
}

func TestParseRejectsIncompleteManifest(test *testing.T) {
	for _, input := range []string{"", "not: [valid", "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  labels:\n    app.kubernetes.io/component: k8s-controller\nspec:\n  replicas: 1\n"} {
		if _, err := Parse(strings.NewReader(input)); err == nil {
			test.Fatal("accepted incomplete bootstrap")
		}
	}
}

func fixture(test *testing.T, objects ...client.Object) (Manifest, client.Client) {
	test.Helper()
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{appsv1.AddToScheme, batchv1.AddToScheme, corev1.AddToScheme, rbacv1.AddToScheme} {
		if err := register(scheme); err != nil {
			test.Fatal(err)
		}
	}
	objects = append(objects,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "control"}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(0))}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "guard", Namespace: "control"}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(2))}, Status: appsv1.DeploymentStatus{UpdatedReplicas: 2, AvailableReplicas: 2}},
	)
	return Manifest{Namespace: "control", Controller: "controller", Deployment: "controller", Guard: "guard", Namespaces: []string{"tenant"}}, fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}
