// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type deployedStreamOutput struct {
	buffer bytes.Buffer
	mutex  sync.Mutex
	wrote  chan struct{}
}

func (output *deployedStreamOutput) Write(data []byte) (int, error) {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	count, err := output.buffer.Write(data)
	select {
	case output.wrote <- struct{}{}:
	default:
	}
	return count, err
}

func (output *deployedStreamOutput) String() string {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	return output.buffer.String()
}

func deployedConcurrentStreams(test *testing.T, ctx context.Context, admin client.Client, config *rest.Config, template *batchv1.Job, role *rbacv1.Role, create func(client.Object), network func(string, ...*corev1.Pod)) {
	test.Helper()
	controller, err := kubernetes.NewForConfig(config)
	if err != nil {
		test.Fatal(err)
	}
	type invocation struct {
		job     *batchv1.Job
		policy  *networkingv1.NetworkPolicy
		pod     corev1.Pod
		input   *io.PipeWriter
		stdout  *deployedStreamOutput
		stderr  *deployedStreamOutput
		done    chan error
		cancel  context.CancelFunc
		message string
	}
	invocations := make([]invocation, 2)
	for index := range invocations {
		name := fmt.Sprintf("%s-stream-%d", template.Name, index)
		labels := map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/managed": "stdio-invocation", "kognition.dev/stdio-invocation": name}
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: template.Namespace, Labels: labels}, Spec: batchv1.JobSpec{BackoffLimit: ptr.To(int32(0)), Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: *template.Spec.Template.Spec.DeepCopy()}}}
		create(job)
		invocations[index].job = job
		invocations[index].policy = &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: job.Namespace, Labels: labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: name, UID: job.UID, Controller: ptr.To(true)}}}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/stdio-invocation": name}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}}}
		create(invocations[index].policy)
		if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
			pods := &corev1.PodList{}
			if err := admin.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{"kognition.dev/stdio-invocation": name}); err != nil {
				return false, err
			}
			for _, candidate := range pods.Items {
				owner := metav1.GetControllerOf(&candidate)
				if owner != nil && owner.UID == job.UID && candidate.Status.Phase == corev1.PodRunning {
					invocations[index].pod = candidate
					return true, nil
				}
			}
			return false, nil
		}); err != nil {
			test.Fatal(err)
		}
		role.Rules[0].ResourceNames = append(role.Rules[0].ResourceNames, invocations[index].pod.Name)
	}
	if err := admin.Update(ctx, role); err != nil {
		test.Fatal(err)
	}
	for index := range invocations {
		current := &invocations[index]
		stream, cancel := context.WithTimeout(ctx, 45*time.Second)
		current.cancel = cancel
		input, writer := io.Pipe()
		current.input = writer
		current.stdout = &deployedStreamOutput{wrote: make(chan struct{}, 1)}
		current.stderr = &deployedStreamOutput{wrote: make(chan struct{}, 1)}
		current.done = make(chan error, 1)
		current.message = fmt.Sprintf("%s-message-%d\n", template.Name, index)
		request := controller.CoreV1().RESTClient().Post().Namespace(current.pod.Namespace).Resource("pods").Name(current.pod.Name).SubResource("attach").VersionedParams(&corev1.PodAttachOptions{Container: "exec-probe", Stdin: true, Stdout: true, Stderr: true}, scheme.ParameterCodec)
		executor, err := remotecommand.NewSPDYExecutor(config, http.MethodPost, request.URL())
		if err != nil {
			cancel()
			_ = input.Close()
			_ = writer.Close()
			test.Fatal(err)
		}
		go func() {
			current.done <- executor.StreamWithContext(stream, remotecommand.StreamOptions{Stdin: input, Stdout: current.stdout, Stderr: current.stderr})
		}()
		defer func() {
			cancel()
			_ = writer.Close()
			_ = input.Close()
		}()
		go func() {
			_, _ = io.WriteString(writer, current.message)
		}()
	}
	awaitOutput := func(current *invocation, expected string) {
		test.Helper()
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		for _, output := range []*deployedStreamOutput{current.stdout, current.stderr} {
			for output.String() != expected {
				select {
				case <-output.wrote:
				case err := <-current.done:
					test.Fatalf("stream ended before complete echo: %v", err)
				case <-deadline.C:
					test.Fatalf("stream did not echo before deadline: got=%q expected=%q", output.String(), expected)
				}
			}
		}
	}
	for index := range invocations {
		awaitOutput(&invocations[index], invocations[index].message)
	}
	first := &invocations[0]
	originalPolicy := first.policy.DeepCopy()
	if err := admin.Delete(ctx, first.policy, client.Preconditions{UID: ptr.To(first.policy.UID)}); err != nil {
		test.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		err := admin.Get(ctx, client.ObjectKeyFromObject(first.policy), &networkingv1.NetworkPolicy{})
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}); err != nil {
		test.Fatal(err)
	}
	replacement := originalPolicy.DeepCopy()
	replacement.ResourceVersion, replacement.UID, replacement.ManagedFields = "", "", nil
	replacement.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
	create(replacement)
	*first.policy = *replacement.DeepCopy()
	request := controller.CoreV1().RESTClient().Post().Namespace(first.pod.Namespace).Resource("pods").Name(first.pod.Name).SubResource("attach").VersionedParams(&corev1.PodAttachOptions{Container: "exec-probe", Stdin: true, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	response := request.Do(ctx).Error()
	if !apierrors.IsForbidden(response) || !strings.Contains(response.Error(), "attachment requires its current Job-owned deny-egress NetworkPolicy") {
		test.Fatalf("replacement must deny new attachment while existing streams remain open: %v", response)
	}
	network("active-stream-policy-replacement", &first.pod, &invocations[1].pod)
	for index := range invocations {
		current := &invocations[index]
		message := fmt.Sprintf("%s-during-policy-replacement-%d\n", template.Name, index)
		go func() { _, _ = io.WriteString(current.input, message) }()
		current.message += message
		awaitOutput(current, current.message)
	}
	first.policy.Spec = originalPolicy.Spec
	if err := admin.Update(ctx, first.policy); err != nil {
		test.Fatal(err)
	}
	test.Logf("active stream policy replacement remained isolated: oldUID=%s replacementUID=%s", originalPolicy.UID, first.policy.UID)
	for index := range invocations {
		select {
		case err := <-invocations[index].done:
			test.Fatalf("concurrent stream already finished: %v", err)
		default:
		}
	}
	invocations[0].cancel()
	select {
	case err := <-invocations[0].done:
		if !errors.Is(err, context.Canceled) {
			test.Fatalf("expected stream cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("stream cancellation did not settle")
	}
	second := &invocations[1]
	secondMessage := template.Name + "-survived-neighbor-cancellation\n"
	go func() { _, _ = io.WriteString(second.input, secondMessage) }()
	awaitOutput(second, second.message+secondMessage)
	select {
	case err := <-second.done:
		if !errors.Is(err, context.DeadlineExceeded) {
			test.Fatalf("expected stream deadline: %v", err)
		}
	case <-time.After(50 * time.Second):
		test.Fatal("stream deadline did not settle")
	}
	for index := range invocations {
		current := &invocations[index]
		expected := current.message
		if index == 1 {
			expected += secondMessage
		}
		if current.stdout.String() != expected || current.stderr.String() != expected {
			test.Fatalf("concurrent stream %d cross-talk/loss: stdout=%q stderr=%q expected=%q", index, current.stdout.String(), current.stderr.String(), expected)
		}
		test.Logf("concurrent stream %d settled with exact isolated stdout/stderr: Pod=%s/%s UID=%s", index, current.pod.Namespace, current.pod.Name, current.pod.UID)
	}
	job := first.job
	if err := admin.Get(ctx, client.ObjectKeyFromObject(job), job); err != nil {
		test.Fatal(err)
	}
	job.Finalizers = append(job.Finalizers, "kognition.dev/sg07-cleanup-test")
	if err := admin.Update(ctx, job); err != nil {
		test.Fatal(err)
	}
	defer func() {
		recovery, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		current := &batchv1.Job{}
		if err := admin.Get(recovery, client.ObjectKeyFromObject(job), current); err != nil {
			test.Error(err)
			return
		}
		if current.UID != job.UID {
			test.Error("refusing to recover cleanup for a replacement Job")
			return
		}
		current.Finalizers = slices.DeleteFunc(current.Finalizers, func(finalizer string) bool { return finalizer == "kognition.dev/sg07-cleanup-test" })
		if err := admin.Update(recovery, current); err != nil {
			test.Error(err)
			return
		}
		if err := wait.PollUntilContextTimeout(recovery, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			err := admin.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{})
			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		}); err != nil {
			test.Error(err)
		} else {
			test.Logf("held-finalizer cleanup recovered with fresh context; Job UID=%s absent", job.UID)
		}
	}()
	if err := admin.Delete(ctx, job, client.Preconditions{UID: ptr.To(job.UID)}, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		test.Fatal(err)
	}
	cleanup, cancelCleanup := context.WithTimeout(ctx, 2*time.Second)
	defer cancelCleanup()
	observedTerminating := false
	err = wait.PollUntilContextCancel(cleanup, 200*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		if deadline, bounded := ctx.Deadline(); bounded && time.Until(deadline) < 200*time.Millisecond {
			return false, nil
		}
		current := &batchv1.Job{}
		err := admin.Get(ctx, client.ObjectKeyFromObject(job), current)
		if err == nil {
			if current.UID != job.UID || current.DeletionTimestamp == nil || !slices.Contains(current.Finalizers, "kognition.dev/sg07-cleanup-test") {
				return false, fmt.Errorf("cleanup did not retain the exact terminating Job with its held finalizer")
			}
			observedTerminating = true
		}
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	})
	if !errors.Is(err, context.DeadlineExceeded) || !observedTerminating {
		test.Fatalf("held-finalizer cleanup must observe the terminating Job then time out: observed=%t err=%v", observedTerminating, err)
	}
	test.Logf("cleanup failure observed, not treated as successful deletion: Job UID=%s err=%v", job.UID, err)
}
