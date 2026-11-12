// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/google/uuid"
	"github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
)

const stdioInvocationLabel = "kognition.dev/stdio-invocation"

type podAttachExecutor interface {
	StreamWithContext(context.Context, remotecommand.StreamOptions) error
}

type KubernetesStdioInvocationRunner struct {
	client         client.Client
	config         *rest.Config
	poll           time.Duration
	cleanupTimeout time.Duration
	waitForPod     func(context.Context, *batchv1.Job) (*corev1.Pod, error)
	newExecutor    func(*rest.Config, string, *url.URL) (podAttachExecutor, error)
}

func NewKubernetesStdioInvocationRunner(c client.Client, config *rest.Config) *KubernetesStdioInvocationRunner {
	return &KubernetesStdioInvocationRunner{client: c, config: config, poll: execProbePollInterval, newExecutor: func(config *rest.Config, method string, url *url.URL) (podAttachExecutor, error) {
		return remotecommand.NewSPDYExecutor(config, method, url)
	}}
}

func (r *KubernetesStdioInvocationRunner) RunStdioInvocation(ctx context.Context, tool *v1alpha1.Tool, target intctrl.StdioInvocationTarget, input map[string]any) (result intctrl.StdioInvocationResult, invocationErr error) {
	if r == nil || r.client == nil || r.config == nil {
		return intctrl.StdioInvocationResult{}, fmt.Errorf("Kubernetes stdio invocation runner is not configured")
	}
	if tool == nil || tool.Namespace == "" || strings.TrimSpace(target.Image) == "" || len(target.Command) == 0 {
		return intctrl.StdioInvocationResult{}, fmt.Errorf("stdio invocation Tool namespace, target image, and command are required")
	}
	timeout := target.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	sandboxCtx, cancelSandbox := context.WithTimeout(ctx, timeout)
	defer cancelSandbox()
	invocationID := uuid.NewString()
	labels := map[string]string{"app.kubernetes.io/managed-by": "kognition", stdioInvocationLabel: invocationID, "kognition.dev/tool": tool.Name, "kognition.dev/managed": "stdio-invocation"}
	job := newExecProbeJob(tool.Namespace, labels, intctrl.ExecProbeTarget{Image: target.Image, CPUMilli: target.CPUMilli, MemoryMB: target.MemoryMB, Timeout: timeout}, target.Command)
	job.GenerateName = "kognition-stdio-invocation-"
	job.Spec.Suspend = ptrTo(true)
	job.Spec.Template.Spec.Containers[0].Stdin = true
	job.Spec.Template.Spec.Containers[0].StdinOnce = true
	if err := r.client.Create(sandboxCtx, job); err != nil {
		return intctrl.StdioInvocationResult{}, fmt.Errorf("create stdio sandbox Job: %w", err)
	}
	cleanupJob := job.DeepCopy()
	var cleanupPolicy *networkingv1.NetworkPolicy
	defer func() {
		cleanupTimeout := r.cleanupTimeout
		if cleanupTimeout <= 0 {
			cleanupTimeout = 45 * time.Second
		}
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancelCleanup()
		cleanupErr := r.deleteInvocationObject(cleanupCtx, cleanupJob)
		if cleanupErr == nil && cleanupPolicy != nil {
			cleanupErr = r.deleteInvocationObject(cleanupCtx, cleanupPolicy)
		}
		if cleanupErr != nil {
			result = intctrl.StdioInvocationResult{}
			invocationErr = errors.Join(invocationErr, fmt.Errorf("cleanup stdio sandbox: %w", cleanupErr))
		}
	}()
	policy := newStdioInvocationNetworkPolicy(tool.Namespace, invocationID)
	policy.Name, policy.GenerateName = job.Name, ""
	policy.OwnerReferences = []metav1.OwnerReference{{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptrTo(true)}}
	if err := r.client.Create(sandboxCtx, policy); err != nil {
		return intctrl.StdioInvocationResult{}, fmt.Errorf("create stdio NetworkPolicy: %w", err)
	}
	cleanupPolicy = policy.DeepCopy()
	if err := r.unsuspendJob(sandboxCtx, job); err != nil {
		return intctrl.StdioInvocationResult{}, fmt.Errorf("start stdio sandbox Job: %w", err)
	}
	waitForPod := r.waitForPod
	if waitForPod == nil {
		waitForPod = r.waitForInvocationPod
	}
	pod, err := waitForPod(sandboxCtx, job)
	if err != nil {
		return intctrl.StdioInvocationResult{}, err
	}
	if !isStdioInvocationPod(pod, job) {
		return intctrl.StdioInvocationResult{}, fmt.Errorf("stdio Pod does not belong to the current invocation Job")
	}
	executor, err := r.newExecutor(r.config, "POST", r.attachURL(pod))
	if err != nil {
		return intctrl.StdioInvocationResult{}, fmt.Errorf("create stdio Pod attach executor: %w", err)
	}
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	closePipes := func(err error) {
		_ = stdinReader.CloseWithError(err)
		_ = stdinWriter.CloseWithError(err)
		_ = stdoutReader.CloseWithError(err)
		_ = stdoutWriter.CloseWithError(err)
	}
	stream := &stdioInvocationStream{Reader: stdoutReader, Writer: stdinWriter, close: func() { closePipes(nil) }}
	attachCtx, cancelAttach := context.WithCancel(sandboxCtx)
	defer cancelAttach()
	stopClose := context.AfterFunc(attachCtx, func() { closePipes(attachCtx.Err()) })
	defer stopClose()
	streamErr := make(chan error, 1)
	go func() {
		err := executor.StreamWithContext(attachCtx, remotecommand.StreamOptions{Stdin: stdinReader, Stdout: stdoutWriter, Stderr: io.Discard, Tty: false})
		closePipes(err)
		streamErr <- err
	}()
	output, invokeErr := intctrl.InvokeMCPStdio(attachCtx, stream, tool, input)
	contextErr := attachCtx.Err()
	cancelAttach()
	attachErr := <-streamErr
	if invokeErr != nil {
		return intctrl.StdioInvocationResult{}, fmt.Errorf("invoke stdio sandbox: %w", errors.Join(invokeErr, attachErr, contextErr))
	}
	if contextErr != nil {
		return intctrl.StdioInvocationResult{}, fmt.Errorf("invoke stdio sandbox: %w", contextErr)
	}
	if attachErr != nil && !errors.Is(attachErr, context.Canceled) && !errors.Is(attachErr, io.ErrClosedPipe) {
		return intctrl.StdioInvocationResult{}, fmt.Errorf("attach stdio sandbox: %w", attachErr)
	}
	return intctrl.StdioInvocationResult{Output: output, ExecutionMode: "sandbox"}, nil
}

func (r *KubernetesStdioInvocationRunner) deleteInvocationObject(ctx context.Context, object client.Object) error {
	uid := object.GetUID()
	if uid == "" {
		return fmt.Errorf("missing cleanup UID for %T %s", object, client.ObjectKeyFromObject(object))
	}
	if err := r.client.Delete(ctx, object, client.PropagationPolicy(metav1.DeletePropagationForeground), client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %T %s UID %s: %w", object, client.ObjectKeyFromObject(object), uid, err)
	}
	poll := r.poll
	if poll <= 0 {
		poll = execProbePollInterval
	}
	err := wait.PollUntilContextCancel(ctx, poll, true, func(ctx context.Context) (bool, error) {
		current, ok := object.DeepCopyObject().(client.Object)
		if !ok {
			return false, fmt.Errorf("cleanup copy of %T does not implement client.Object", object)
		}
		err := r.client.Get(ctx, client.ObjectKeyFromObject(object), current)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if current.GetUID() != uid {
			return false, fmt.Errorf("object replaced by UID %s", current.GetUID())
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("confirm deletion of %T %s UID %s: %w", object, client.ObjectKeyFromObject(object), uid, err)
	}
	return nil
}

func (r *KubernetesStdioInvocationRunner) waitForInvocationPod(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
	poll := r.poll
	if poll <= 0 {
		poll = execProbePollInterval
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		var current batchv1.Job
		if err := r.client.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, &current); err != nil {
			return nil, fmt.Errorf("read stdio sandbox Job: %w", err)
		}
		if current.UID != job.UID {
			return nil, fmt.Errorf("stdio sandbox Job was replaced")
		}
		if current.Status.Failed > 0 {
			return nil, fmt.Errorf("stdio sandbox Job failed")
		}
		var pods corev1.PodList
		if err := r.client.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(job.Labels)}); err != nil {
			return nil, fmt.Errorf("list stdio sandbox Pods: %w", err)
		}
		for index := range pods.Items {
			if pods.Items[index].Status.Phase == corev1.PodRunning && isStdioInvocationPod(&pods.Items[index], job) {
				return &pods.Items[index], nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for stdio sandbox Pod: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func isStdioInvocationPod(pod *corev1.Pod, job *batchv1.Job) bool {
	if pod == nil || job == nil || job.UID == "" || pod.Namespace != job.Namespace ||
		job.Labels[stdioInvocationLabel] == "" || pod.Labels[stdioInvocationLabel] != job.Labels[stdioInvocationLabel] {
		return false
	}
	owner := metav1.GetControllerOf(pod)
	return owner != nil && owner.APIVersion == batchv1.SchemeGroupVersion.String() &&
		owner.Kind == "Job" && owner.Name == job.Name && owner.UID == job.UID
}

func (r *KubernetesStdioInvocationRunner) unsuspendJob(ctx context.Context, job *batchv1.Job) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current batchv1.Job
		if err := r.client.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Name}, &current); err != nil {
			return err
		}
		if current.UID != job.UID {
			return fmt.Errorf("stdio sandbox Job was replaced")
		}
		current.Spec.Suspend = ptrTo(false)
		if err := r.client.Update(ctx, &current); err != nil {
			return err
		}
		*job = current
		return nil
	})
}

func newStdioInvocationNetworkPolicy(namespace, invocationID string) *networkingv1.NetworkPolicy {
	labels := map[string]string{"app.kubernetes.io/managed-by": "kognition", stdioInvocationLabel: invocationID}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "kognition-stdio-invocation-", Namespace: namespace, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{},
		},
	}
}

func (r *KubernetesStdioInvocationRunner) attachURL(pod *corev1.Pod) *url.URL {
	base, _ := url.Parse(r.config.Host)
	return base.ResolveReference(&url.URL{Path: fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/attach", pod.Namespace, pod.Name), RawQuery: "container=exec-probe&stdin=true&stdout=true&stderr=true&tty=false"})
}

type stdioInvocationStream struct {
	io.Reader
	io.Writer
	close func()
}

func (s *stdioInvocationStream) Close() error { s.close(); return nil }

func ptrTo[T any](value T) *T { return &value }
