// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/k8s/activation"
	k8scontroller "github.com/kognition-ai/kognition/internal/k8s/controller"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func deployedStdioProducerAcceptance(test *testing.T, config *rest.Config, manifest activation.Manifest, roles []*unstructured.Unstructured, image, suffix string) {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	admin, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		test.Fatal(err)
	}
	var created []client.Object
	var mutex sync.Mutex
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for index := len(created) - 1; index >= 0; index-- {
			object := created[index]
			if object.GetUID() == "" {
				test.Errorf("producer cleanup missing UID: %T %s", object, object.GetName())
				continue
			}
			if err := admin.Delete(cleanup, object, client.Preconditions{UID: ptr.To(object.GetUID())}, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
				test.Error(err)
				continue
			}
			if err := wait.PollUntilContextTimeout(cleanup, 100*time.Millisecond, 40*time.Second, true, func(ctx context.Context) (bool, error) {
				current := object.DeepCopyObject().(client.Object)
				err := admin.Get(ctx, client.ObjectKeyFromObject(object), current)
				if apierrors.IsNotFound(err) {
					return true, nil
				}
				if err == nil && current.GetUID() != object.GetUID() {
					return false, fmt.Errorf("producer cleanup found replacement UID %s", current.GetUID())
				}
				return false, err
			}); err != nil {
				test.Error(err)
			}
		}
	})
	workload := manifest.Namespaces[0]
	grantDeployedProducerRoles(test, ctx, admin, manifest, roles, &created)
	controllerConfig := rest.CopyConfig(config)
	controllerConfig.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + manifest.Namespace + ":" + manifest.Controller, Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + manifest.Namespace, "system:authenticated"}}
	controller, err := client.NewWithWatch(controllerConfig, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		test.Fatal(err)
	}
	observed := interceptor.NewClient(controller, interceptor.Funcs{
		Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if err := underlying.Create(ctx, object, options...); err != nil {
				return err
			}
			mutex.Lock()
			created = append(created, object.DeepCopyObject().(client.Object))
			mutex.Unlock()
			test.Logf("production runner created %T %s UID=%s identity=%s", object, client.ObjectKeyFromObject(object), object.GetUID(), controllerConfig.Impersonate.UserName)
			return nil
		},
		List: func(ctx context.Context, underlying client.WithWatch, objects client.ObjectList, options ...client.ListOption) error {
			if err := underlying.List(ctx, objects, options...); err != nil {
				return err
			}
			if pods, ok := objects.(*corev1.PodList); ok {
				for _, pod := range pods.Items {
					for _, status := range pod.Status.ContainerStatuses {
						if pod.Status.Phase == corev1.PodRunning {
							if status.ImageID == "" {
								return fmt.Errorf("running producer Pod has no runtime image ID")
							}
							test.Logf("production Pod=%s UID=%s imageID=%s", pod.Name, pod.UID, status.ImageID)
						}
					}
				}
			}
			return nil
		},
	})
	runner := k8scontroller.NewKubernetesStdioInvocationRunner(observed, controllerConfig)
	command := `test "$(id -u)" = 65532 || exit 20
test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token || exit 21
if touch /sg08-write-probe 2>/dev/null; then exit 22; fi
IFS= read -r initialize || exit 23
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{}}'
IFS= read -r notification || exit 24
IFS= read -r invocation || exit 25
`
	for _, scenario := range []string{"success", "deadline"} {
		test.Run(scenario, func(test *testing.T) {
			toolName := "sg08-" + suffix + "-" + scenario
			script := command + `printf '{"jsonrpc":"2.0","id":2,"result":{"status":"ok","request":%s}}\n' "$invocation"`
			timeout := 30 * time.Second
			if scenario == "deadline" {
				script = command + "sleep 120"
				timeout = 8 * time.Second
			}
			started := time.Now()
			result, err := runner.RunStdioInvocation(ctx, &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: toolName, Namespace: workload}}, intctrl.StdioInvocationTarget{Image: image, Command: []string{"sh", "-c", script}, Timeout: timeout}, map[string]any{"message": suffix})
			if scenario == "success" {
				if err != nil || result.ExecutionMode != "sandbox" || result.Output["status"] != "ok" {
					test.Fatalf("production MCP result=%#v error=%v", result, err)
				}
				request, ok := result.Output["request"].(map[string]any)
				if !ok || request["method"] != "tools/call" {
					test.Fatalf("wrong MCP request: %#v", result.Output)
				}
				params, ok := request["params"].(map[string]any)
				if !ok || params["name"] != toolName {
					test.Fatalf("wrong MCP Tool: %#v", request)
				}
				arguments, ok := params["arguments"].(map[string]any)
				if !ok || arguments["message"] != suffix {
					test.Fatalf("wrong MCP arguments: %#v", params)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) || result.Output != nil {
				test.Fatalf("deadline must settle without successful output: result=%#v error=%v", result, err)
			}
			var jobs batchv1.JobList
			var pods corev1.PodList
			if err := admin.List(ctx, &jobs, client.InNamespace(workload), client.MatchingLabels{"kognition.dev/tool": toolName}); err != nil {
				test.Fatal(err)
			}
			if err := admin.List(ctx, &pods, client.InNamespace(workload), client.MatchingLabels{"kognition.dev/tool": toolName}); err != nil {
				test.Fatal(err)
			}
			if len(jobs.Items) != 0 || len(pods.Items) != 0 {
				test.Fatalf("producer returned before cleanup: jobs=%d pods=%d", len(jobs.Items), len(pods.Items))
			}
			for _, object := range created {
				if _, ok := object.(*networkingv1.NetworkPolicy); !ok {
					continue
				}
				if err := admin.Get(ctx, client.ObjectKeyFromObject(object), &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
					test.Fatalf("producer policy remains %s: %v", object.GetName(), err)
				}
			}
			test.Logf("actual production stdio %s settled in %s; Job, Pod and invocation policy absent", scenario, time.Since(started))
		})
	}
}

func grantDeployedProducerRoles(test *testing.T, ctx context.Context, admin client.Client, manifest activation.Manifest, roles []*unstructured.Unstructured, created *[]client.Object) {
	test.Helper()
	if len(roles) == 0 {
		test.Fatal("rendered production controller Roles missing")
	}
	for _, expected := range roles {
		role := expected.DeepCopy()
		if err := createAdmissionFixture(ctx, admin, role, created); err != nil {
			test.Fatal(err)
		}
		metadata := metav1.ObjectMeta{Name: role.GetName(), Namespace: role.GetNamespace()}
		reference := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: role.GetKind(), Name: role.GetName()}
		subjects := []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: manifest.Namespace, Name: manifest.Controller}}
		var binding client.Object = &rbacv1.RoleBinding{ObjectMeta: metadata, RoleRef: reference, Subjects: subjects}
		if role.GetKind() == "ClusterRole" {
			binding = &rbacv1.ClusterRoleBinding{ObjectMeta: metadata, RoleRef: reference, Subjects: subjects}
		}
		if err := createAdmissionFixture(ctx, admin, binding, created); err != nil {
			test.Fatal(err)
		}
		test.Logf("production grant %s %s/%s", role.GetKind(), role.GetNamespace(), role.GetName())
	}
}
