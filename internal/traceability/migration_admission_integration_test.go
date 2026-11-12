// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestMigrationPodsPassRestrictedAdmission(test *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		test.Skip("KUBEBUILDER_ASSETS is not set")
	}
	environment := &envtest.Environment{}
	config, err := environment.Start()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := stopControllerEnvtest(environment); err != nil {
			test.Errorf("stop envtest: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		test.Fatal(err)
	}
	admin, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		test.Fatal(err)
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "migration-admission", Labels: map[string]string{
		"pod-security.kubernetes.io/enforce":         "restricted",
		"pod-security.kubernetes.io/enforce-version": "v1.30",
	}}}
	if err := admin.Create(ctx, namespace); err != nil {
		test.Fatal(err)
	}
	for _, mode := range []string{"standalone", "kubernetes"} {
		test.Run(mode, func(test *testing.T) {
			found := false
			for _, document := range renderControllerRBAC(test, "--set", "mode="+mode) {
				var job batchv1.Job
				if err := json.Unmarshal(document.Raw, &job); err != nil {
					test.Fatal(err)
				}
				if job.Kind != "Job" || job.Labels["app.kubernetes.io/component"] != "migrations" {
					continue
				}
				found = true
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "migrations-" + mode, Namespace: namespace.Name}, Spec: job.Spec.Template.Spec}
				if err := admin.Create(ctx, pod, client.DryRunAll); err != nil {
					test.Fatalf("restricted migration admission: %v", err)
				}
				unsafe := pod.DeepCopy()
				allowEscalation := true
				unsafe.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation = &allowEscalation
				if err := admin.Create(ctx, unsafe, client.DryRunAll); !apierrors.IsForbidden(err) {
					test.Fatalf("privilege escalation must be rejected by admission, got %v", err)
				}
			}
			if !found {
				test.Fatal("migration Job missing")
			}
		})
	}
}
