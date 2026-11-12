// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
)

type attachExecutorFunc func(context.Context, remotecommand.StreamOptions) error

func (fn attachExecutorFunc) StreamWithContext(ctx context.Context, options remotecommand.StreamOptions) error {
	return fn(ctx, options)
}

func TestKubernetesStdioInvocationRunnerCreatesSuspendedRestrictedJobAndStreamsMCP(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{batchv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
			object.SetUID("test-object-uid")
			return underlying.Create(ctx, object, options...)
		},
	}).Build()
	runner := NewKubernetesStdioInvocationRunner(client, &rest.Config{Host: "https://kubernetes.example"})
	runner.waitForPod = func(_ context.Context, job *batchv1.Job) (*corev1.Pod, error) {
		var policies networkingv1.NetworkPolicyList
		if err := client.List(context.Background(), &policies); err != nil {
			return nil, err
		}
		if len(policies.Items) != 1 {
			t.Fatalf("stdio policies = %#v", policies.Items)
		}
		policy := policies.Items[0]
		owner := metav1.GetControllerOf(&policy)
		if policy.Name != job.Name || policy.GenerateName != "" || owner == nil || owner.Name != job.Name || owner.UID != job.UID {
			t.Fatalf("stdio policy must be addressable by its current Job: %#v", policy.ObjectMeta)
		}
		if policy.Labels["app.kubernetes.io/managed-by"] != "kognition" || policy.Labels[stdioInvocationLabel] == "" {
			t.Fatalf("stdio policy ownership = %#v", policy.Labels)
		}
		for key, value := range policy.Spec.PodSelector.MatchLabels {
			if job.Spec.Template.Labels[key] != value || policy.Labels[key] != value {
				t.Fatalf("stdio policy does not select its owned Job Pods: policy=%#v pod=%#v", policy.Labels, job.Spec.Template.Labels)
			}
		}
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "stdio-pod", Namespace: job.Namespace, Labels: job.Labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}, nil
	}
	runner.newExecutor = func(_ *rest.Config, method string, target *url.URL) (podAttachExecutor, error) {
		if method != "POST" || target.Path != "/api/v1/namespaces/tenant-a/pods/stdio-pod/attach" {
			t.Fatalf("attach target = %s %s", method, target)
		}
		return attachExecutorFunc(func(_ context.Context, options remotecommand.StreamOptions) error {
			reader := bufio.NewScanner(options.Stdin)
			encoder := json.NewEncoder(options.Stdout)
			if !reader.Scan() {
				return errors.New("missing initialize")
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{}})
			if !reader.Scan() {
				return errors.New("missing MCP invocation")
			}
			if !reader.Scan() {
				return errors.New("missing MCP invocation")
			}
			return encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "result": map[string]any{"content": []any{}, "status": "ok"}})
		}), nil
	}
	result, err := runner.RunStdioInvocation(context.Background(), &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "tenant-a"}}, intctrl.StdioInvocationTarget{Image: "registry.example/shell:1", Command: []string{"/srv/mcp", "--stdio"}, CPUMilli: 100, MemoryMB: 64, Timeout: 5 * time.Second}, map[string]any{"command": "status"})
	if err != nil || result.Output["status"] != "ok" || result.ExecutionMode != "sandbox" {
		t.Fatalf("RunStdioInvocation result=%#v err=%v", result, err)
	}
	var jobs batchv1.JobList
	if err := client.List(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("expected Job cleanup, got %#v", jobs.Items)
	}
	var policies networkingv1.NetworkPolicyList
	if err := client.List(context.Background(), &policies); err != nil {
		t.Fatal(err)
	}
	if len(policies.Items) != 0 {
		t.Fatalf("expected invocation NetworkPolicy cleanup, got %#v", policies.Items)
	}
}

func TestKubernetesStdioInvocationRunnerNormalizesJobDeadline(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := batchv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			stop := errors.New("deadline observed")
			observed := false
			storage := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, _ client.WithWatch, object client.Object, _ ...client.CreateOption) error {
					job := object.(*batchv1.Job)
					observed = true
					if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 5 {
						t.Errorf("Job deadline=%v, want normalized five seconds", job.Spec.ActiveDeadlineSeconds)
					}
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
						t.Errorf("unexpected invocation context deadline: %v", deadline)
					}
					return stop
				},
			}).Build()
			runner := NewKubernetesStdioInvocationRunner(storage, &rest.Config{Host: "https://kubernetes.example"})
			_, err := runner.RunStdioInvocation(context.Background(), &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "tenant-a"}}, intctrl.StdioInvocationTarget{Image: "shell:test", Command: []string{"mcp"}, Timeout: timeout}, nil)
			if !observed || !errors.Is(err, stop) {
				t.Fatalf("deadline observed=%t, error=%v", observed, err)
			}
		})
	}
}

func TestKubernetesStdioInvocationRunnerFailsWhenJobFailsBeforeAttach(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{batchv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	runner := NewKubernetesStdioInvocationRunner(client, &rest.Config{Host: "https://kubernetes.example"})
	runner.poll = time.Millisecond
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "failed", Namespace: "tenant-a"}, Status: batchv1.JobStatus{Failed: 1}}
	if err := client.Create(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	_, err := runner.waitForInvocationPod(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "Job failed") {
		t.Fatalf("waitForInvocationPod error = %v", err)
	}
}

func TestKubernetesStdioInvocationRunnerReportsCleanupFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{batchv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	invocationErr := errors.New("invocation failed")
	cleanupErr := errors.New("delete forbidden")
	cleanupCalled := false
	storage := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
			object.SetUID("original-job")
			return underlying.Create(ctx, object, options...)
		},
		Delete: func(ctx context.Context, _ client.WithWatch, object client.Object, options ...client.DeleteOption) error {
			cleanupCalled = true
			if ctx.Err() != nil {
				t.Errorf("cleanup inherited cancelled context: %v", ctx.Err())
			}
			if _, bounded := ctx.Deadline(); !bounded {
				t.Error("cleanup context has no deadline")
			}
			deleteOptions := (&client.DeleteOptions{}).ApplyOptions(options)
			if deleteOptions.Preconditions == nil || deleteOptions.Preconditions.UID == nil || *deleteOptions.Preconditions.UID != "original-job" {
				t.Errorf("cleanup lacks original UID precondition: %#v", deleteOptions.Preconditions)
			}
			if _, ok := object.(*batchv1.Job); !ok {
				t.Errorf("unexpected cleanup target: %T", object)
			}
			return cleanupErr
		},
	}).Build()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := NewKubernetesStdioInvocationRunner(storage, &rest.Config{Host: "https://kubernetes.example"})
	runner.waitForPod = func(context.Context, *batchv1.Job) (*corev1.Pod, error) {
		cancel()
		return nil, invocationErr
	}
	_, err := runner.RunStdioInvocation(ctx, &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "tenant-a"}}, intctrl.StdioInvocationTarget{Image: "shell:test", Command: []string{"mcp"}}, nil)
	if !cleanupCalled || !errors.Is(err, invocationErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup called=%t, want invocation and cleanup errors, got %v", cleanupCalled, err)
	}
}

func TestKubernetesStdioInvocationRunnerReportsIncompleteCleanup(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{batchv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	storage := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
			object.SetUID("held-object")
			if _, ok := object.(*batchv1.Job); ok {
				object.SetFinalizers([]string{"test.kognition.dev/hold"})
			}
			return underlying.Create(ctx, object, options...)
		},
	}).Build()
	invocationErr := errors.New("invocation failed")
	runner := NewKubernetesStdioInvocationRunner(storage, &rest.Config{Host: "https://kubernetes.example"})
	runner.poll = time.Millisecond
	runner.cleanupTimeout = 20 * time.Millisecond
	runner.waitForPod = func(context.Context, *batchv1.Job) (*corev1.Pod, error) {
		return nil, invocationErr
	}
	_, err := runner.RunStdioInvocation(context.Background(), &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "tenant-a"}}, intctrl.StdioInvocationTarget{Image: "shell:test", Command: []string{"mcp"}}, nil)
	if !errors.Is(err, invocationErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want invocation error and bounded cleanup failure, got %v", err)
	}
	var jobs batchv1.JobList
	if err := storage.List(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 || jobs.Items[0].DeletionTimestamp == nil || len(jobs.Items[0].Finalizers) != 1 {
		t.Fatalf("cleanup did not preserve the terminating Job and finalizer: %#v", jobs.Items)
	}
	var policies networkingv1.NetworkPolicyList
	if err := storage.List(context.Background(), &policies); err != nil {
		t.Fatal(err)
	}
	if len(policies.Items) != 1 {
		t.Fatalf("policy removed before Job deletion completed: %#v", policies.Items)
	}
}

func TestKubernetesStdioInvocationRunnerSettlesInterruptedAttach(t *testing.T) {
	transportErr := errors.New("attach transport failed")
	for name, expected := range map[string]error{"transport": transportErr, "deadline": context.DeadlineExceeded, "cancelled": context.Canceled} {
		t.Run(name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			for _, add := range []func(*runtime.Scheme) error{batchv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
				if err := add(scheme); err != nil {
					t.Fatal(err)
				}
			}
			storage := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
					object.SetUID("interrupted-object")
					return underlying.Create(ctx, object, options...)
				},
			}).Build()
			runner := NewKubernetesStdioInvocationRunner(storage, &rest.Config{Host: "https://kubernetes.example"})
			runner.waitForPod = func(_ context.Context, job *batchv1.Job) (*corev1.Pod, error) {
				return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "stdio-pod", Namespace: job.Namespace, Labels: job.Labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}}}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			streamOptions := make(chan remotecommand.StreamOptions, 1)
			runner.newExecutor = func(*rest.Config, string, *url.URL) (podAttachExecutor, error) {
				return attachExecutorFunc(func(ctx context.Context, options remotecommand.StreamOptions) error {
					streamOptions <- options
					if name == "transport" {
						return transportErr
					}
					if name == "cancelled" {
						cancel()
					}
					<-ctx.Done()
					return ctx.Err()
				}), nil
			}
			done := make(chan error, 1)
			go func() {
				_, err := runner.RunStdioInvocation(ctx, &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "tenant-a"}}, intctrl.StdioInvocationTarget{Image: "shell:test", Command: []string{"mcp"}, Timeout: 100 * time.Millisecond}, nil)
				done <- err
			}()
			options := <-streamOptions
			select {
			case err := <-done:
				if !errors.Is(err, expected) {
					t.Errorf("want %v, got %v", expected, err)
				}
			case <-time.After(time.Second):
				_ = options.Stdin.(io.Closer).Close()
				_ = options.Stdout.(io.Closer).Close()
				<-done
				t.Fatal("invocation blocked after the attach executor stopped")
			}
			var jobs batchv1.JobList
			var policies networkingv1.NetworkPolicyList
			if err := storage.List(context.Background(), &jobs); err != nil {
				t.Fatal(err)
			}
			if err := storage.List(context.Background(), &policies); err != nil {
				t.Fatal(err)
			}
			if len(jobs.Items) != 0 || len(policies.Items) != 0 {
				t.Fatalf("interrupted invocation leaked resources: jobs=%d policies=%d", len(jobs.Items), len(policies.Items))
			}
		})
	}
}

func TestKubernetesStdioInvocationRunnerRejectsUnownedAttachTargets(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Pod){
		"no-owner":         func(pod *corev1.Pod) { pod.OwnerReferences = nil },
		"stale-owner":      func(pod *corev1.Pod) { pod.OwnerReferences[0].UID = "previous-job" },
		"wrong-name":       func(pod *corev1.Pod) { pod.OwnerReferences[0].Name = "other-job" },
		"wrong-kind":       func(pod *corev1.Pod) { pod.OwnerReferences[0].Kind = "Deployment" },
		"wrong-api":        func(pod *corev1.Pod) { pod.OwnerReferences[0].APIVersion = "example.com/v1" },
		"not-controller":   func(pod *corev1.Pod) { pod.OwnerReferences[0].Controller = ptrTo(false) },
		"wrong-namespace":  func(pod *corev1.Pod) { pod.Namespace = "other" },
		"wrong-invocation": func(pod *corev1.Pod) { pod.Labels = map[string]string{stdioInvocationLabel: "other"} },
	} {
		t.Run(name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			for _, add := range []func(*runtime.Scheme) error{batchv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
				if err := add(scheme); err != nil {
					t.Fatal(err)
				}
			}
			runner := NewKubernetesStdioInvocationRunner(fake.NewClientBuilder().WithScheme(scheme).Build(), &rest.Config{Host: "https://kubernetes.example"})
			runner.waitForPod = func(_ context.Context, job *batchv1.Job) (*corev1.Pod, error) {
				job.UID = "current-job"
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "forged", Namespace: job.Namespace, Labels: job.Labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}}}
				mutate(pod)
				return pod, nil
			}
			runner.newExecutor = func(*rest.Config, string, *url.URL) (podAttachExecutor, error) {
				t.Error("attempted to attach to an unowned Pod")
				return nil, errors.New("unexpected attach")
			}
			_, err := runner.RunStdioInvocation(context.Background(), &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "tenant-a"}}, intctrl.StdioInvocationTarget{Image: "shell:test", Command: []string{"mcp"}}, nil)
			if err == nil || !strings.Contains(err.Error(), "does not belong") {
				t.Fatalf("unowned target error = %v", err)
			}
		})
	}
}

func TestKubernetesStdioInvocationRunnerDoesNotStartReplacementJob(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{batchv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	var cleanupUID *types.UID
	storage := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
			object.SetUID("original-job")
			if err := underlying.Create(ctx, object, options...); err != nil {
				return err
			}
			if job, ok := object.(*batchv1.Job); ok {
				if err := underlying.Delete(ctx, job); err != nil {
					return err
				}
				replacement := job.DeepCopy()
				replacement.UID = "replacement-job"
				replacement.ResourceVersion = ""
				return underlying.Create(ctx, replacement)
			}
			return nil
		},
		Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, options ...client.DeleteOption) error {
			deleteOptions := &client.DeleteOptions{}
			deleteOptions.ApplyOptions(options)
			if deleteOptions.Preconditions != nil {
				cleanupUID = deleteOptions.Preconditions.UID
			}
			return nil
		},
	}).Build()
	runner := NewKubernetesStdioInvocationRunner(storage, &rest.Config{Host: "https://kubernetes.example"})
	_, err := runner.RunStdioInvocation(context.Background(), &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "tenant-a"}}, intctrl.StdioInvocationTarget{Image: "shell:test", Command: []string{"mcp"}, Timeout: time.Second}, nil)
	if err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("replacement error = %v", err)
	}
	var jobs batchv1.JobList
	if err := storage.List(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 || jobs.Items[0].Spec.Suspend == nil || !*jobs.Items[0].Spec.Suspend {
		t.Fatalf("replacement Job was started: %#v", jobs.Items)
	}
	if cleanupUID == nil || *cleanupUID != "original-job" {
		t.Fatalf("cleanup UID precondition = %v", cleanupUID)
	}
}
