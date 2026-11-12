// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func deployedAttachmentAcceptance(test *testing.T, config *rest.Config, manifest activation.Manifest, image, suffix string, installationObjects *[]client.Object) {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	admin, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		test.Fatal(err)
	}
	var created []client.Object
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for index := len(created) - 1; index >= 0; index-- {
			object := created[index]
			if object.GetUID() == "" {
				test.Errorf("missing cleanup UID for %T %s", object, object.GetName())
				continue
			}
			err := admin.Delete(cleanup, object, client.Preconditions{UID: ptr.To(object.GetUID())}, client.PropagationPolicy(metav1.DeletePropagationForeground))
			if err != nil && !apierrors.IsNotFound(err) {
				test.Errorf("cleanup %T %s: %v", object, object.GetName(), err)
				continue
			}
			if err := wait.PollUntilContextTimeout(cleanup, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
				current := object.DeepCopyObject().(client.Object)
				err := admin.Get(ctx, client.ObjectKeyFromObject(object), current)
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				if err == nil && current.GetUID() != object.GetUID() {
					return false, fmt.Errorf("replacement UID for %T %s", object, object.GetName())
				}
				return false, err
			}); err != nil {
				test.Error(err)
			} else {
				test.Logf("attach cleanup confirmed %T %s/%s UID=%s", object, object.GetNamespace(), object.GetName(), object.GetUID())
			}
		}
	})
	create := func(object client.Object) {
		test.Helper()
		if err := createAdmissionFixture(ctx, admin, object, &created); err != nil {
			test.Fatal(err)
		}
	}
	workload := manifest.Namespaces[0]
	labels := map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/managed": "stdio-invocation", "kognition.dev/stdio-invocation": suffix}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "kognition-stdio-invocation-" + suffix, Namespace: workload, Labels: labels}, Spec: batchv1.JobSpec{BackoffLimit: ptr.To(int32(0)), Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false), TerminationGracePeriodSeconds: ptr.To(int64(0)),
		SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Containers:      []corev1.Container{{Name: "exec-probe", Image: image, ImagePullPolicy: deployedFixturePullPolicy(), Command: []string{"sh", "-c", `while IFS= read -r line; do printf '%s\n' "$line"; printf '%s\n' "$line" >&2; done`}, Stdin: true, StdinOnce: true, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}},
	}}}}
	create(job)
	var pod corev1.Pod
	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		pods := &corev1.PodList{}
		if err := admin.List(ctx, pods, client.InNamespace(workload), client.MatchingLabels{"kognition.dev/stdio-invocation": suffix}); err != nil {
			return false, err
		}
		for _, candidate := range pods.Items {
			owner := metav1.GetControllerOf(&candidate)
			if owner != nil && owner.UID == job.UID && candidate.Status.Phase == corev1.PodRunning {
				pod = candidate
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		test.Fatalf("credential-free attach Pod: %v", err)
	}
	checkNetwork := deployedNetworkControls(test, ctx, config, admin, create, &pod, manifest, suffix)
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "sg03-attach-" + suffix, Namespace: workload}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods/attach"}, ResourceNames: []string{pod.Name}, Verbs: []string{"create"}}}}
	create(role)
	create(&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: workload}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: manifest.Namespace, Name: manifest.Controller}}})
	controllerConfig := rest.CopyConfig(config)
	controllerConfig.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + manifest.Namespace + ":" + manifest.Controller, Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + manifest.Namespace, "system:authenticated"}}
	controller, err := kubernetes.NewForConfig(controllerConfig)
	if err != nil {
		test.Fatal(err)
	}
	options := &corev1.PodAttachOptions{Container: "exec-probe", Stdin: true, Stdout: true, Stderr: true}
	request := func(options *corev1.PodAttachOptions) *rest.Request {
		return controller.CoreV1().RESTClient().Post().Namespace(workload).Resource("pods").Name(pod.Name).SubResource("attach").VersionedParams(options, scheme.ParameterCodec)
	}
	deny := func(message string) {
		test.Helper()
		var response error
		if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
			response = request(options).Do(ctx).Error()
			return apierrors.IsForbidden(response) && strings.Contains(response.Error(), "attach.guard.kognition.dev") && strings.Contains(response.Error(), message), nil
		}); err != nil {
			test.Fatalf("attach denial %q: %v; response=%v", message, err, response)
		}
		test.Logf("deployed attach denied: %v", response)
	}
	deny("cannot verify attachment NetworkPolicy")
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: job.Name, Namespace: workload, Labels: labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true)}}}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/stdio-invocation": suffix}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}}}
	create(policy)
	checkNetwork("after-invocation-policy")
	for _, invalid := range []corev1.PodAttachOptions{
		{Container: "exec-probe", Stdin: true, Stdout: true, TTY: true},
		{Container: "other", Stdin: true, Stdout: true, Stderr: true},
		{Container: "exec-probe", Stdout: true, Stderr: true},
		{Container: "exec-probe", Stdin: true, Stderr: true},
		{Container: "exec-probe", Stdin: true, Stdout: true},
	} {
		original := options
		options = &invalid
		deny("non-interactive stdio sandbox streams")
		options = original
	}
	deployedPolicyLifecycle(test, ctx, admin, manifest, policy, installationObjects, &created, deny, func(phase string) { checkNetwork(phase) })
	guardRole := &rbacv1.Role{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: workload, Name: manifest.Guard}, guardRole); err != nil {
		test.Fatal(err)
	}
	originalRole := guardRole.DeepCopy()
	guardRole.Rules = nil
	if err := admin.Update(ctx, guardRole); err != nil {
		test.Fatal(err)
	}
	func() {
		defer func() {
			restored := guardRole.DeepCopy()
			restored.Rules = originalRole.Rules
			if err := admin.Patch(ctx, restored, client.MergeFrom(guardRole)); err != nil {
				test.Error(err)
			}
		}()
		deny("cannot verify attachment Pod")
	}()
	if test.Failed() {
		return
	}
	blackhole := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-timeout-" + suffix, Namespace: manifest.Namespace, Labels: map[string]string{"sg03-api-timeout": suffix}}, Spec: *job.Spec.Template.Spec.DeepCopy()}
	blackhole.Spec.Containers[0].Command = []string{"nc", "-ll", "-p", "6443", "-e", "sleep", "30"}
	blackhole.Spec.Containers[0].Stdin, blackhole.Spec.Containers[0].StdinOnce = false, false
	create(blackhole)
	blackholeService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: blackhole.Name, Namespace: manifest.Namespace}, Spec: corev1.ServiceSpec{Selector: blackhole.Labels, Ports: []corev1.ServicePort{{Port: 6443, TargetPort: intstr.FromInt32(6443)}}}}
	create(blackholeService)
	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := admin.Get(ctx, client.ObjectKeyFromObject(blackhole), blackhole); err != nil {
			return false, err
		}
		return blackhole.Status.Phase == corev1.PodRunning, nil
	}); err != nil {
		test.Fatalf("API timeout fixture: %v", err)
	}
	guardDeployment := &appsv1.Deployment{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: manifest.Namespace, Name: manifest.Guard}, guardDeployment); err != nil {
		test.Fatal(err)
	}
	originalGuard := guardDeployment.DeepCopy()
	waitForGuard := func() {
		test.Helper()
		if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
			if err := admin.Get(ctx, client.ObjectKeyFromObject(guardDeployment), guardDeployment); err != nil {
				return false, err
			}
			if guardDeployment.Status.ObservedGeneration != guardDeployment.Generation || guardDeployment.Status.UpdatedReplicas != *guardDeployment.Spec.Replicas || guardDeployment.Status.ReadyReplicas != *guardDeployment.Spec.Replicas || guardDeployment.Status.Replicas != *guardDeployment.Spec.Replicas {
				return false, nil
			}
			ready := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-ready", Namespace: workload, Labels: labels}, Spec: *job.Spec.Template.Spec.DeepCopy()}
			return admin.Create(ctx, ready, client.DryRunAll) == nil, nil
		}); err != nil {
			test.Fatalf("guard rollout: %v", err)
		}
	}
	func() {
		defer func() {
			recovery, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := admin.Get(recovery, client.ObjectKeyFromObject(guardDeployment), guardDeployment); err != nil {
				test.Error(err)
				return
			}
			restored := guardDeployment.DeepCopy()
			restored.Spec.Template = originalGuard.Spec.Template
			if err := admin.Patch(recovery, restored, client.MergeFrom(guardDeployment)); err != nil {
				test.Error(err)
			}
		}()
		guardDeployment.Spec.Template.Spec.Containers[0].Env = append(guardDeployment.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "KUBERNETES_SERVICE_HOST", Value: blackholeService.Spec.ClusterIP}, corev1.EnvVar{Name: "KUBERNETES_SERVICE_PORT", Value: "6443"})
		if err := admin.Update(ctx, guardDeployment); err != nil {
			test.Fatal(err)
		}
		waitForGuard()
		started := time.Now()
		response := request(options).Do(ctx).Error()
		if response == nil || !strings.Contains(response.Error(), "attach.guard.kognition.dev") || (!strings.Contains(response.Error(), "context deadline exceeded") && !strings.Contains(response.Error(), "TLS handshake timeout")) {
			test.Fatalf("guard API timeout must fail closed: %v", response)
		}
		if elapsed := time.Since(started); elapsed > 12*time.Second {
			test.Fatalf("unbounded guard timeout: %v", elapsed)
		}
		test.Logf("guard API TLS handshake stalled; attachment failed closed: %v", response)
	}()
	if test.Failed() {
		return
	}
	waitForGuard()
	deployedConcurrentStreams(test, ctx, admin, controllerConfig, job, role, create, checkNetwork)
	deployment := &appsv1.Deployment{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: manifest.Namespace, Name: manifest.Deployment}, deployment); err != nil {
		test.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
		test.Fatal("controller activated during attach acceptance")
	}
	executor, err := remotecommand.NewSPDYExecutor(controllerConfig, http.MethodPost, request(options).URL())
	if err != nil {
		test.Fatal(err)
	}
	stream, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	message := "sg03-" + suffix + "\n"
	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(stream, remotecommand.StreamOptions{Stdin: strings.NewReader(message), Stdout: &stdout, Stderr: &stderr}); err != nil {
		test.Fatalf("deployed guard/kubelet attach: %v", err)
	}
	if stdout.String() != message || stderr.String() != message {
		test.Fatalf("stream mismatch: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	test.Logf("actual kubelet stdin/stdout/stderr exchange confirmed: Pod=%s/%s UID=%s JobUID=%s policyUID=%s; isolation profile=%s", workload, pod.Name, pod.UID, job.UID, policy.UID, manifest.StdioIsolationProfile)
	for _, status := range pod.Status.ContainerStatuses {
		if status.ImageID == "" {
			test.Fatal("missing stream runtime image ID")
		}
		test.Logf("stream container=%s image=%s imageID=%s", status.Name, status.Image, status.ImageID)
	}
}
