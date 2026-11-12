// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
)

func TestKubernetesExecProbeRunnerCreatesRestrictedSandbox(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{batchv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	runner := NewKubernetesExecProbeRunner(client)
	var observedJob batchv1.Job
	runner.wait = func(_ context.Context, job *batchv1.Job) error {
		if err := client.Get(context.Background(), clientObjectKey(job), &observedJob); err != nil {
			return err
		}
		var policies networkingv1.NetworkPolicyList
		if err := client.List(context.Background(), &policies); err != nil {
			return err
		}
		if len(policies.Items) != 1 || len(policies.Items[0].Spec.Egress) != 0 || len(policies.Items[0].Spec.PolicyTypes) != 1 || policies.Items[0].Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
			t.Fatalf("exec probe NetworkPolicy = %#v", policies.Items)
		}
		policy := policies.Items[0]
		if policy.Labels["app.kubernetes.io/managed-by"] != "kognition" || policy.Labels[execProbeLabel] == "" {
			t.Fatalf("exec probe policy ownership = %#v", policy.Labels)
		}
		for key, value := range policy.Spec.PodSelector.MatchLabels {
			if observedJob.Spec.Template.Labels[key] != value || policy.Labels[key] != value {
				t.Fatalf("exec probe policy does not select its owned Job Pods: policy=%#v pod=%#v", policy.Labels, observedJob.Spec.Template.Labels)
			}
		}
		return nil
	}
	target := intctrl.ExecProbeTarget{Image: "registry.example/probe:1.0", CPUMilli: 250, MemoryMB: 128, Timeout: 1500 * time.Millisecond}
	tool := &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "probe", Namespace: "tenant-a"}}
	if err := runner.RunExecProbe(context.Background(), tool, target, []string{"/probe", "--ready"}); err != nil {
		t.Fatalf("RunExecProbe: %v", err)
	}
	container := observedJob.Spec.Template.Spec.Containers[0]
	if security := observedJob.Spec.Template.Spec.SecurityContext; security == nil || security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("sandbox must use RuntimeDefault seccomp: %#v", security)
	}
	if container.Image != target.Image || len(container.Command) != 2 || container.Command[0] != "/probe" || container.Command[1] != "--ready" {
		t.Fatalf("sandbox container = %#v", container)
	}
	if observedJob.Spec.Template.Spec.AutomountServiceAccountToken == nil || *observedJob.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatalf("sandbox Pod must disable service-account token: %#v", observedJob.Spec.Template.Spec.AutomountServiceAccountToken)
	}
	if container.SecurityContext == nil || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem || len(container.SecurityContext.Capabilities.Drop) != 1 || container.SecurityContext.Capabilities.Drop[0] != corev1.Capability("ALL") {
		t.Fatalf("sandbox security context = %#v", container.SecurityContext)
	}
	if observedJob.Spec.ActiveDeadlineSeconds == nil || *observedJob.Spec.ActiveDeadlineSeconds != 2 || observedJob.Spec.BackoffLimit == nil || *observedJob.Spec.BackoffLimit != 0 {
		t.Fatalf("sandbox Job deadline/backoff = %#v", observedJob.Spec)
	}
}

func TestKubernetesExecProbeRunnerPropagatesSandboxFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	runner := NewKubernetesExecProbeRunner(client)
	runner.wait = func(_ context.Context, _ *batchv1.Job) error { return errors.New("exit status 1") }
	err := runner.RunExecProbe(context.Background(), &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "probe", Namespace: "tenant-a"}}, intctrl.ExecProbeTarget{Image: "registry.example/probe:1.0"}, []string{"/probe"})
	if err == nil || err.Error() != "exit status 1" {
		t.Fatalf("RunExecProbe error = %v", err)
	}
}

func TestKubernetesExecProbeRunnerRejectsReplacementJob(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	runner := NewKubernetesExecProbeRunner(client)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "exec-probe", Namespace: "tenant-a", Labels: map[string]string{execProbeLabel: "expected"}},
	}
	if err := client.Create(context.Background(), job); err != nil {
		t.Fatalf("create Job: %v", err)
	}
	job.Labels[execProbeLabel] = "replacement"
	if err := client.Update(context.Background(), job); err != nil {
		t.Fatalf("replace Job labels: %v", err)
	}
	if err := runner.waitForJob(context.Background(), &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "exec-probe", Namespace: "tenant-a", Labels: map[string]string{execProbeLabel: "expected"}}}); err == nil || !strings.Contains(err.Error(), "sandbox labels do not match") {
		t.Fatalf("waitForJob error = %v, want sandbox label mismatch", err)
	}
}

func clientObjectKey(job *batchv1.Job) client.ObjectKey {
	return client.ObjectKey{Namespace: job.Namespace, Name: job.Name}
}
